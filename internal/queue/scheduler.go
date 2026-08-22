package queue

import (
	"context"
	"encoding/json"
	"time"

	"tg-down/internal/downloader"
)

const (
	// scheduleTickInterval 是定时计划的巡检周期
	scheduleTickInterval = time.Minute
	// MinScheduleIntervalMin 是定时计划允许的最小间隔（分钟），供 API 校验复用
	MinScheduleIntervalMin = 10
	// MaxScheduleIntervalMin 是 time.Duration 能安全表达的最大分钟数，供 API 校验复用。
	MaxScheduleIntervalMin = int64((1<<63 - 1) / time.Minute)
)

// runScheduler 周期性巡检定时计划，到期的计划触发一次历史下载任务；
// 随 Manager.Run 的 ctx 退出
func (m *Manager) runScheduler(ctx context.Context) {
	ticker := time.NewTicker(scheduleTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.fireDueSchedules(ctx)
		}
	}
}

// fireDueSchedules 触发所有到期的计划。先记录触发时间再入队：
// 若因同聊天已有任务被去重拒绝，也不会在下个 tick 立即热重试
func (m *Manager) fireDueSchedules(ctx context.Context) {
	rows, err := m.store.ListSchedules(ctx)
	if err != nil {
		m.logger.Warn("查询定时计划失败: %v", err)
		return
	}
	now := time.Now()
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		intervalMin := int64(r.IntervalMin)
		if intervalMin < MinScheduleIntervalMin || intervalMin > MaxScheduleIntervalMin {
			m.logger.Warn("定时计划 %s 的间隔无效（%d 分钟），跳过触发", r.ID, r.IntervalMin)
			continue
		}
		interval := time.Duration(intervalMin) * time.Minute
		if r.LastRun != nil && now.Sub(*r.LastRun) < interval {
			continue
		}
		m.mu.Lock()
		accepting := m.acceptingLocked()
		m.mu.Unlock()
		if !accepting {
			continue
		}
		var filters downloader.HistoryFilters
		if r.Filters != "" {
			if err := json.Unmarshal([]byte(r.Filters), &filters); err != nil {
				m.logger.Warn("定时计划 %s 的过滤器 JSON 损坏，跳过触发: %v", r.ID, err)
				continue
			}
			if problem := filters.Validate(); problem != "" {
				m.logger.Warn("定时计划 %s 的过滤器无效，跳过触发: %s", r.ID, problem)
				continue
			}
		}
		if err := m.store.TouchScheduleLastRun(ctx, r.ID, now); err != nil {
			m.logger.Warn("更新定时计划触发时间失败: %v", err)
			continue
		}
		// 增量扫描：只扫比上次水位更新的消息。首次触发（LastMaxID=0）仍为全量扫描。
		spec := &downloader.HistorySpec{
			ChatID:          r.ChatID,
			ChatTitle:       r.ChatTitle,
			Filters:         filters,
			StopAtMessageID: r.LastMaxID,
			ScheduleID:      r.ID,
		}
		if _, err := m.Enqueue(KindHistory, spec, r.ChatTitle); err != nil {
			// 常见于同聊天已有排队/运行中的任务，跳过本次触发
			m.logger.Info("定时计划 %s（聊天 %d）本次触发跳过: %v", r.ID, r.ChatID, err)
			continue
		}
		m.logger.Info("定时计划 %s 已触发聊天 %d 的历史下载", r.ID, r.ChatID)
	}
}
