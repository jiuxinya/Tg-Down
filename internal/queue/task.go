package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"tg-down/internal/downloader"
	"tg-down/internal/store"
)

// idSeq 为同一纳秒内的并发 Enqueue 调用提供唯一性兜底
var idSeq atomic.Int64

// generateID 生成任务 ID，纯标准库实现，无需引入新依赖
func generateID() string {
	return fmt.Sprintf("t%d%d", time.Now().UnixNano(), idSeq.Add(1))
}

// task 是队列内部的任务状态容器，自带锁保护可变字段，
// 使执行任务的 goroutine（写者）与 List/Get（读者）之间不发生数据竞争。
type task struct {
	id        string
	kind      Kind
	chatID    int64
	chatTitle string
	createdAt time.Time
	done      chan struct{} // 任务终结时关闭（经 markDone），monitor 切换时用于等待旧任务停止

	// persistMu 串行化同一任务的落盘：Manager.persist 先取快照再发两条 UPDATE，
	// 若两次 persist 交错，先取到的旧快照可能后写入而覆盖新状态。它只保护"取快照 +
	// 写库"这段过程，与保护字段本身的 mu 是两把锁，不可合并（persist 期间不持有 mu）。
	persistMu sync.Mutex

	mu            sync.Mutex
	doneClosed    bool // 与 done 一起受 mu 保护；部分重试会安装一个全新的完成通道
	status        Status
	errMsg        string
	startedAt     *time.Time
	finishedAt    *time.Time
	stats         downloader.Stats
	cancel        context.CancelFunc
	phase         string // 运行阶段（counting/downloading），仅内存态
	expectedTotal int64  // 下载前统计出的媒体总数（近似值），0 表示未知

	scannedMessages int64                     // 历史扫描已翻阅的消息数（仅内存态，运行中有值）
	foundMedia      int64                     // 历史扫描累计发现的媒体数（仅内存态，运行中有值）
	scanCursor      int64                     // 历史扫描游标（持久化，重启恢复续扫起点）
	attempts        int                       // 自动重试已消耗次数（持久化）
	resumed         bool                      // 本任务是否为进程重启后恢复
	retryFailedOnly bool                      // 本次运行只补下失败文件，跳过历史扫描（历史已完整扫过）
	stopAtMessageID int64                     // 增量扫描下界（定时任务：只扫比上次水位更新的消息）
	scheduleID      string                    // 触发本任务的定时计划（非空时完成后回写增量水位）（需补下中断行）
	filters         downloader.HistoryFilters // 任务级过滤条件（持久化，零值 = 不过滤）
	messageID       int64                     // 单消息任务的目标消息 id（持久化，0 = 整聊天）
	lastScanNotify  time.Time                 // 上次扫描进度对外推送时刻，用于限频

	lastRecordNotify      time.Time // 上次下载记录对外推送时刻，用于限频
	recordTrailingPending bool      // 是否已排定一次尾随推送（限频窗口内多次更新只排一次）
	restoreErr            string    // 持久化运行参数损坏时阻止自动恢复和无过滤重试

	// rateSamples 是任务级速率的滑动窗口采样，按文件完成时的累计字节数记点。
	// downloader 的速度是全局值（所有任务共用一个分子），任务卡片需要自己这一份。
	rateSamples []taskRateSample
}

// taskRateSample 是某一时刻该任务累计已下载字节数的采样点
type taskRateSample struct {
	at    time.Time
	bytes int64
}

const (
	// taskSpeedWindow 是任务速率的滑动窗口长度。取值明显大于 downloader 的 5s：
	// 任务级采样点只在文件完成时产生，大文件之间可以间隔很久，窗口太短会让速度频繁归零。
	taskSpeedWindow = 30 * time.Second
	// taskRateMaxSamples 是窗口内保留的采样点上限，防止大量小文件瞬间完成时无限增长
	taskRateMaxSamples = 256
)

// scanNotifyMinGap 是扫描进度对外推送（SSE）的最小间隔：
// 本地缓存命中时历史页可毫秒级连续返回，不限频会造成广播风暴
const scanNotifyMinGap = 500 * time.Millisecond

// recordNotifyMinGap 是下载记录对外推送（SSE）的最小间隔：
// 重试时的跳过风暴或大量小文件快速完成会产生每文件一次的 task 事件，
// 不限频会向浏览器灌入成百上千条消息造成前端卡顿
const recordNotifyMinGap = 250 * time.Millisecond

// newTask 创建一个初始状态为 queued 的任务；spec 携带 ChatID/Filters/MessageID 与定时增量信息
func newTask(kind Kind, spec *downloader.HistorySpec, chatTitle string) *task {
	return &task{
		id:              generateID(),
		kind:            kind,
		chatID:          spec.ChatID,
		chatTitle:       chatTitle,
		createdAt:       time.Now(),
		done:            make(chan struct{}),
		status:          StatusQueued,
		filters:         cloneHistoryFilters(spec.Filters),
		messageID:       spec.MessageID,
		stopAtMessageID: spec.StopAtMessageID,
		scheduleID:      spec.ScheduleID,
	}
}

func cloneHistoryFilters(filters downloader.HistoryFilters) downloader.HistoryFilters {
	filters.MediaTypes = append([]string(nil), filters.MediaTypes...)
	return filters
}

// markDone 关闭 done 通道，多次调用安全；由任务终结的唯一执行路径调用
func (t *task) markDone() {
	t.mu.Lock()
	if !t.doneClosed {
		close(t.done)
		t.doneClosed = true
	}
	t.mu.Unlock()
}

// markDoneIfQueued 关闭关停后仍保持 queued 的本轮完成信号。返回 true 表示本次完成了关闭。
func (t *task) markDoneIfQueued() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status != StatusQueued || t.doneClosed {
		return false
	}
	close(t.done)
	t.doneClosed = true
	return true
}

// taskFromRow 是 ToDTO 的逆操作，从持久化行重建进程重启后的内存任务；
// done/doneClosed 属于进程本地状态，无法持久化，此处总是全新初始化
func taskFromRow(row *store.TaskRow) (*task, error) {
	var filters downloader.HistoryFilters
	if row.Filters != "" {
		if err := json.Unmarshal([]byte(row.Filters), &filters); err != nil {
			t := taskFromRowValues(row, filters)
			t.restoreErr = fmt.Sprintf("任务过滤器 JSON 损坏: %v", err)
			return t, fmt.Errorf("%s", t.restoreErr)
		}
		if problem := filters.Validate(); problem != "" {
			t := taskFromRowValues(row, filters)
			t.restoreErr = "任务过滤器无效: " + problem
			return t, fmt.Errorf("%s", t.restoreErr)
		}
	}
	return taskFromRowValues(row, filters), nil
}

func taskFromRowValues(row *store.TaskRow, filters downloader.HistoryFilters) *task {
	return &task{
		id:              row.ID,
		kind:            Kind(row.Kind),
		chatID:          row.ChatID,
		chatTitle:       row.ChatTitle,
		createdAt:       row.CreatedAt,
		done:            make(chan struct{}),
		status:          Status(row.Status),
		errMsg:          row.Error,
		startedAt:       row.StartedAt,
		finishedAt:      row.FinishedAt,
		expectedTotal:   row.ExpectedTotal,
		scanCursor:      row.ScanCursor,
		attempts:        row.Attempts,
		filters:         filters,
		messageID:       row.MessageID,
		stopAtMessageID: row.StopAtMessageID,
		scheduleID:      row.ScheduleID,
		retryFailedOnly: row.RetryFailedOnly,
		stats: downloader.Stats{
			Total:          row.Total,
			Downloaded:     row.Downloaded,
			Failed:         row.Failed,
			Skipped:        row.Skipped,
			TotalSize:      row.TotalSize,
			DownloadedSize: row.DownloadedSize,
		},
	}
}

// filtersJSON 返回过滤器的 JSON 序列化（零值返回空串，落库为 NULL）
func (t *task) filtersJSON() string {
	if t.filters.IsZero() {
		return ""
	}
	data, err := json.Marshal(t.filters)
	if err != nil {
		return ""
	}
	return string(data)
}

// ToDTO 加锁返回任务状态的值拷贝快照
func (t *task) ToDTO() TaskDTO {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.toDTOLocked()
}

// toDTOLocked 返回任务快照；调用方必须已持有 t.mu。
func (t *task) toDTOLocked() TaskDTO {
	var filters *downloader.HistoryFilters
	if !t.filters.IsZero() {
		f := cloneHistoryFilters(t.filters)
		filters = &f
	}
	speedBps, etaSeconds := t.speedAndETALocked(time.Now())
	return TaskDTO{
		SpeedBps:        speedBps,
		ETASeconds:      etaSeconds,
		Filters:         filters,
		MessageID:       t.messageID,
		ID:              t.id,
		Kind:            string(t.kind),
		ChatID:          t.chatID,
		ChatTitle:       t.chatTitle,
		Status:          string(t.status),
		Error:           t.errMsg,
		CreatedAt:       t.createdAt,
		StartedAt:       t.startedAt,
		FinishedAt:      t.finishedAt,
		Stats:           t.stats,
		Phase:           t.phase,
		ExpectedTotal:   t.expectedTotal,
		ScannedMessages: t.scannedMessages,
		FoundMedia:      t.foundMedia,
		ScanCursor:      t.scanCursor,
		Attempts:        t.attempts,
		StopAtMessageID: t.stopAtMessageID,
		ScheduleID:      t.scheduleID,
		RetryFailedOnly: t.retryFailedOnly,
	}
}

// applyScanProgress 更新扫描进度与游标，返回本次是否应对外推送（按 scanNotifyMinGap 限频）
func (t *task) applyScanProgress(scannedMessages, foundMedia, scanCursor int64) (notify bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.scannedMessages = scannedMessages
	t.foundMedia = foundMedia
	if scanCursor != 0 {
		t.scanCursor = scanCursor
	}
	if time.Since(t.lastScanNotify) < scanNotifyMinGap {
		return false
	}
	t.lastScanNotify = time.Now()
	return true
}

// markRecordNotify 采用前沿限频 + 尾随补发：距上次推送超过 recordNotifyMinGap 立即推送；
// 否则在窗口内首次触发时请求排定一次尾随推送（返回 scheduleTrailing），确保突发结束后的最终态不丢失。
func (t *task) markRecordNotify() (now, scheduleTrailing bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if time.Since(t.lastRecordNotify) >= recordNotifyMinGap {
		t.lastRecordNotify = time.Now()
		t.recordTrailingPending = false
		return true, false
	}
	if t.recordTrailingPending {
		return false, false
	}
	t.recordTrailingPending = true
	return false, true
}

// clearRecordTrailing 在尾随推送真正发出前复位限频状态，使后续更新能重新触发推送
func (t *task) clearRecordTrailing() {
	t.mu.Lock()
	t.recordTrailingPending = false
	t.lastRecordNotify = time.Now()
	t.mu.Unlock()
}

// applyRecordEvent 按下载事件更新任务统计。
//
// 事件契约（见 downloader.DownloadMedia）：每个媒体恰好一次 RecordQueued，随后恰好一次终态
// 事件。因此 Total 只在入队时累加，终态事件各自累加自己的计数，恒有
// Total = Downloaded + Failed + Skipped（全部终结后）。
func (t *task) applyRecordEvent(evt *downloader.RecordEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch evt.Status {
	case downloader.RecordQueued:
		// RetryOnly 补的是本任务已经计入 Total 的失败文件，不能重复增加总数。
		if !t.retryFailedOnly {
			t.stats.Total++
		}
	case downloader.RecordSkipped:
		t.stats.Skipped++
	case downloader.RecordCompleted:
		t.stats.Downloaded++
		if evt.DownloadedSize > 0 {
			t.stats.DownloadedSize += evt.DownloadedSize
		} else {
			t.stats.DownloadedSize += evt.Media.FileSize
		}
		t.noteRateLocked(time.Now())
	case downloader.RecordFailed:
		t.stats.Failed++
	}
}

// noteRateLocked 记录一个任务级速率采样点；调用方必须已持有 t.mu。
func (t *task) noteRateLocked(now time.Time) {
	t.rateSamples = append(t.rateSamples, taskRateSample{at: now, bytes: t.stats.DownloadedSize})
	t.pruneRateLocked(now)
}

func (t *task) pruneRateLocked(now time.Time) {
	cut := 0
	for cut < len(t.rateSamples) && now.Sub(t.rateSamples[cut].at) > taskSpeedWindow {
		cut++
	}
	t.rateSamples = t.rateSamples[cut:]
	if over := len(t.rateSamples) - taskRateMaxSamples; over > 0 {
		t.rateSamples = t.rateSamples[over:]
	}
}

// speedAndETALocked 返回任务级速度（字节/秒）与预计剩余秒数；调用方必须已持有 t.mu。
//
// 速度取滑动窗口内累计字节的增量除以时间跨度。只有运行中的任务才报速度：终态任务的
// 窗口里可能还留着最后几个采样点，继续报出去会让已完成的任务卡片显示一个虚假的速率。
//
// ETA 需要一个可信的分母。expectedTotal 在带日期/大小/关键词/发送者过滤时恒为 0，
// 贴纸任务与计数失败时同样为 0；此时只报速度不报 ETA，而不是拿错的总数算出一个错的
// 剩余时间——与进度条"宁可没有分母，也不要错的分母"是同一条原则。
func (t *task) speedAndETALocked(now time.Time) (speedBps, etaSeconds int64) {
	if t.status != StatusRunning {
		return 0, 0
	}
	t.pruneRateLocked(now)
	if len(t.rateSamples) < 2 {
		return 0, 0
	}
	oldest := t.rateSamples[0]
	newest := t.rateSamples[len(t.rateSamples)-1]
	elapsed := newest.at.Sub(oldest.at).Seconds()
	if elapsed <= 0 {
		return 0, 0
	}
	speedBps = int64(float64(newest.bytes-oldest.bytes) / elapsed)
	if speedBps <= 0 {
		return 0, 0
	}

	// 剩余文件数按"总数 - 已终结"估算，再乘以已完成文件的平均大小换成字节。
	total := t.expectedTotal
	if total <= 0 {
		total = int64(t.stats.Total)
	}
	done := int64(t.stats.Downloaded + t.stats.Skipped + t.stats.Failed)
	if total <= 0 || done <= 0 || done >= total {
		return speedBps, 0
	}
	avgSize := t.stats.DownloadedSize / int64(max(t.stats.Downloaded, 1))
	if avgSize <= 0 {
		return speedBps, 0
	}
	remainBytes := (total - done) * avgSize
	return speedBps, remainBytes / speedBps
}
