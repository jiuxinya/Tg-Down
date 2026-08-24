package queue

import (
	"testing"
	"time"

	"tg-down/internal/downloader"
)

// newSpeedTask 造一个运行中的任务，采样点由测试直接注入，避免依赖真实时钟推进。
func newSpeedTask(expectedTotal int64, stats downloader.Stats) *task {
	t := newTask(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat")
	t.status = StatusRunning
	t.expectedTotal = expectedTotal
	t.stats = stats
	return t
}

// TestTaskSpeed_WindowRate 校验任务级速度取窗口内累计字节的增量除以时间跨度。
func TestTaskSpeed_WindowRate(t *testing.T) {
	now := time.Now()
	task := newSpeedTask(0, downloader.Stats{Total: 10, Downloaded: 2, DownloadedSize: 3000})
	task.rateSamples = []taskRateSample{
		{at: now.Add(-4 * time.Second), bytes: 1000},
		{at: now, bytes: 3000},
	}
	speed, _ := task.speedAndETALocked(now)
	if speed != 500 { // (3000-1000)/4s
		t.Errorf("速度 = %d B/s，期望 500", speed)
	}
}

// TestTaskSpeed_OnlyWhileRunning 断言终态任务不报速度：窗口里可能还留着最后几个采样点，
// 继续报出去会让已完成的任务卡片显示一个虚假的速率。
func TestTaskSpeed_OnlyWhileRunning(t *testing.T) {
	now := time.Now()
	task := newSpeedTask(0, downloader.Stats{Total: 2, Downloaded: 2, DownloadedSize: 3000})
	task.rateSamples = []taskRateSample{
		{at: now.Add(-2 * time.Second), bytes: 1000},
		{at: now, bytes: 3000},
	}
	task.status = StatusCompleted
	if speed, eta := task.speedAndETALocked(now); speed != 0 || eta != 0 {
		t.Errorf("终态任务报出了速度/ETA = %d/%d，期望 0/0", speed, eta)
	}
}

// TestTaskSpeed_NoETAWithoutTrustworthyTotal 锁定 ETA 的前提：总数未知时只报速度不报 ETA。
// 带日期/大小/关键词过滤的任务 expectedTotal 恒为 0，拿错的分母算剩余时间比不算更糟。
func TestTaskSpeed_NoETAWithoutTrustworthyTotal(t *testing.T) {
	now := time.Now()
	samples := []taskRateSample{
		{at: now.Add(-2 * time.Second), bytes: 1000},
		{at: now, bytes: 3000},
	}

	// 总数未知（expectedTotal=0 且 stats.Total=0）
	unknown := newSpeedTask(0, downloader.Stats{Downloaded: 2, DownloadedSize: 3000})
	unknown.rateSamples = samples
	speed, eta := unknown.speedAndETALocked(now)
	if speed <= 0 {
		t.Fatalf("速度应有值，得到 %d", speed)
	}
	if eta != 0 {
		t.Errorf("总数未知时 ETA = %d，期望 0（不可估算）", eta)
	}

	// 总数已知：4 个文件共 2 个已完成，平均 1500 B/个，剩余 2 个 = 3000 B，速度 1000 B/s → 3s
	known := newSpeedTask(4, downloader.Stats{Total: 4, Downloaded: 2, DownloadedSize: 3000})
	known.rateSamples = samples
	speed, eta = known.speedAndETALocked(now)
	if speed != 1000 {
		t.Fatalf("速度 = %d，期望 1000", speed)
	}
	if eta != 3 {
		t.Errorf("ETA = %d 秒，期望 3", eta)
	}
}

// TestTaskSpeed_PrunesOutdatedSamples 校验窗口外的采样点被剔除，长时间无完成时速度归零。
func TestTaskSpeed_PrunesOutdatedSamples(t *testing.T) {
	now := time.Now()
	task := newSpeedTask(0, downloader.Stats{Downloaded: 1, DownloadedSize: 1000})
	task.rateSamples = []taskRateSample{
		{at: now.Add(-2 * taskSpeedWindow), bytes: 500},
		{at: now.Add(-taskSpeedWindow - time.Second), bytes: 1000},
	}
	if speed, _ := task.speedAndETALocked(now); speed != 0 {
		t.Errorf("窗口外采样仍在计速，speed = %d，期望 0", speed)
	}
	if len(task.rateSamples) != 0 {
		t.Errorf("过期采样未被剔除，剩余 %d 个", len(task.rateSamples))
	}
}
