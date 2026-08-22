package queue

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"tg-down/internal/downloader"
	"tg-down/internal/logger"
	"tg-down/internal/store"
)

// TestHistoryTask_AllFilesFailed_NotCompleted 是 v2.0 最严重缺陷的回归测试。
//
// 缺陷：单个媒体的下载错误在 dispatch goroutine 里被吞掉，只打一行日志；任务终态只看
// "扫描是否出错"。结果是——一个 500 个文件全部下载失败的任务，最终显示绿色的"已完成"。
func TestHistoryTask_AllFilesFailed_NotCompleted(t *testing.T) {
	fc := newFakeClient()
	// 关闭自动重试，直接观察终态
	m := NewManager(fc, newTestStore(t), logger.New(logger.LevelError), 1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(ctx)

	dto, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1")
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	// 扫描成功（任务级 err = nil），但 500 个文件全部下载失败
	fc.setFailedMedia(dto.ID, 500)
	fc.release(dto.ID)

	got := waitForStatus(t, m, dto.ID, StatusPartial, testWaitTimeout)
	if got.Status != string(StatusPartial) {
		t.Fatalf("任务状态 = %q，want %q（全部文件失败绝不能报告为已完成）", got.Status, StatusPartial)
	}
	if got.Error == "" {
		t.Error("部分失败的任务应携带失败说明")
	}
}

func addPartialTaskForRetryTest(t *testing.T, m *Manager, id string) *task {
	t.Helper()
	task := newTask(KindHistory, &downloader.HistorySpec{ChatID: 99}, "partial")
	task.id = id
	now := time.Now()
	task.status = StatusPartial
	task.errMsg = "1 个文件下载失败"
	task.finishedAt = &now
	task.stats.Total = 1
	task.stats.Failed = 1
	task.markDone()
	if err := m.store.CreateTask(context.Background(), &store.TaskRow{
		ID: id, Kind: string(KindHistory), ChatID: 99, ChatTitle: "partial",
		Status: string(StatusPartial), CreatedAt: task.createdAt, FinishedAt: &now,
		Error: task.errMsg, Total: 1, Failed: 1,
	}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.tasks[id] = task
	m.order = append(m.order, task)
	m.mu.Unlock()
	return task
}

func TestRetryPartialDoesNotDeadlock(t *testing.T) {
	m := NewManager(newFakeClient(), newTestStore(t), logger.New(logger.LevelError), 1, 0)
	addPartialTaskForRetryTest(t, m, "partial-retry")
	type result struct {
		dto TaskDTO
		err error
	}
	done := make(chan result, 1)
	go func() {
		dto, err := m.Retry("partial-retry")
		done <- result{dto: dto, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil || Status(got.dto.Status) != StatusQueued {
			t.Fatalf("Retry(partial) = (%+v, %v)", got.dto, got.err)
		}
	case <-time.After(testWaitTimeout):
		t.Fatal("Retry(partial) deadlocked while taking the task snapshot")
	}
	if len(m.historyCh) != 1 {
		t.Fatalf("retried task was not enqueued, queue len=%d", len(m.historyCh))
	}
	row, err := m.store.GetTask(context.Background(), "partial-retry")
	if err != nil || row == nil || row.Status != string(StatusQueued) || row.FinishedAt != nil {
		t.Fatalf("persisted retry state = %+v, err=%v", row, err)
	}
}

func TestRetryPartialWaitsForPreviousRunBeforeReplacingDone(t *testing.T) {
	m := NewManager(newFakeClient(), newTestStore(t), logger.New(logger.LevelError), 1, 0)
	target := addPartialTaskForRetryTest(t, m, "partial-finishing")
	target.mu.Lock()
	target.done = make(chan struct{})
	target.doneClosed = false
	previousDone := target.done
	target.mu.Unlock()

	retryDone := make(chan error, 1)
	go func() {
		_, err := m.Retry("partial-finishing")
		retryDone <- err
	}()
	select {
	case err := <-retryDone:
		t.Fatalf("Retry returned before previous run finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	target.markDone()
	select {
	case err := <-retryDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testWaitTimeout):
		t.Fatal("Retry did not continue after previous run finished")
	}
	select {
	case <-previousDone:
	default:
		t.Fatal("previous done channel was not closed")
	}
	target.mu.Lock()
	currentDone := target.done
	target.mu.Unlock()
	select {
	case <-currentDone:
		t.Fatal("previous run closed the replacement done channel")
	default:
	}
}

func TestRetryPartialQueueFullRollsBackState(t *testing.T) {
	m := NewManager(newFakeClient(), newTestStore(t), logger.New(logger.LevelError), 1, 0)
	target := addPartialTaskForRetryTest(t, m, "partial-full")
	for i := 0; i < cap(m.historyCh); i++ {
		m.historyCh <- &task{id: fmt.Sprintf("filler-%d", i)}
	}
	if _, err := m.Retry("partial-full"); err == nil {
		t.Fatal("Retry(partial) with full queue should fail")
	}
	dto, _ := m.Get("partial-full")
	if Status(dto.Status) != StatusPartial || dto.Error == "" || dto.Stats.Failed != 1 || dto.FinishedAt == nil {
		t.Fatalf("partial task was not rolled back: %+v", dto)
	}
	select {
	case <-target.done:
	default:
		t.Fatal("rollback replaced the original closed done channel")
	}
	row, err := m.store.GetTask(context.Background(), "partial-full")
	if err != nil || row == nil || row.Status != string(StatusPartial) || row.Failed != 1 || row.FinishedAt == nil {
		t.Fatalf("store row changed despite failed enqueue: row=%+v err=%v", row, err)
	}
}

func TestRetryPartialRejectedAfterManagerStops(t *testing.T) {
	m := NewManager(newFakeClient(), newTestStore(t), logger.New(logger.LevelError), 1, 0)
	addPartialTaskForRetryTest(t, m, "partial-stopped")
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(runDone)
	}()
	deadline := time.Now().Add(testWaitTimeout)
	for {
		m.mu.Lock()
		started := m.runCtx != nil
		m.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Manager.Run did not start")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(testWaitTimeout):
		t.Fatal("Manager.Run did not stop")
	}

	if _, err := m.Retry("partial-stopped"); err == nil || !strings.Contains(err.Error(), "队列已停止") {
		t.Fatalf("Retry(partial after stop) error = %v", err)
	}
	dto, _ := m.Get("partial-stopped")
	if Status(dto.Status) != StatusPartial || dto.Error == "" || dto.FinishedAt == nil {
		t.Fatalf("stopped retry changed the original task: %+v", dto)
	}
}

func TestRetryOnlySuccessKeepsStatsInvariantAndClearsFailureState(t *testing.T) {
	m := NewManager(newFakeClient(), newTestStore(t), logger.New(logger.LevelError), 1, 1)
	task := newTask(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat")
	task.status = StatusRunning
	task.stats.Total = 1
	task.stats.Failed = 1

	retryScheduled, _ := m.settleHistoryTask(task, nil, 1, false, false)
	if !retryScheduled {
		t.Fatal("failed media did not arm RetryOnly")
	}
	media := &downloader.MediaInfo{ChatID: 1, MessageID: 1, FileSize: 10}
	task.applyRecordEvent(&downloader.RecordEvent{Media: media, Status: downloader.RecordQueued})
	task.applyRecordEvent(&downloader.RecordEvent{
		Media: media, Status: downloader.RecordCompleted, DownloadedSize: 10,
	})
	m.settleHistoryTask(task, nil, 0, false, false)
	dto := task.ToDTO()
	if Status(dto.Status) != StatusCompleted || dto.Error != "" || dto.FinishedAt == nil {
		t.Fatalf("successful RetryOnly terminal state = %+v", dto)
	}
	terminal := dto.Stats.Downloaded + dto.Stats.Failed + dto.Stats.Skipped
	if dto.Stats.Total != 1 || terminal != dto.Stats.Total || dto.Stats.Downloaded != 1 {
		t.Fatalf("RetryOnly stats invariant broken: %+v", dto.Stats)
	}
}

// TestHistoryTask_PartialKeepsScanCursor 校验部分失败不清空扫描游标。
// completed 才清游标；partial 清了的话，失败文件的续扫定位就丢了。
func TestHistoryTask_PartialKeepsScanCursor(t *testing.T) {
	fc := newFakeClient()
	m := NewManager(fc, newTestStore(t), logger.New(logger.LevelError), 1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(ctx)

	dto, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1")
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	// 模拟扫描过程中推进了游标
	m.handleScanProgress(dto.ID, 100, 10, 8888)

	fc.setFailedMedia(dto.ID, 3)
	fc.release(dto.ID)

	got := waitForStatus(t, m, dto.ID, StatusPartial, testWaitTimeout)
	if got.ScanCursor != 8888 {
		t.Errorf("部分失败后 ScanCursor = %d, want 8888（不应清零）", got.ScanCursor)
	}
}

// TestHistoryTask_NoFailures_Completed 校验没有失败时仍正常报告完成并清游标
func TestHistoryTask_NoFailures_Completed(t *testing.T) {
	fc := newFakeClient()
	m := NewManager(fc, newTestStore(t), logger.New(logger.LevelError), 1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(ctx)

	dto, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1")
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	m.handleScanProgress(dto.ID, 100, 10, 8888)
	fc.release(dto.ID)

	got := waitForStatus(t, m, dto.ID, StatusCompleted, testWaitTimeout)
	if got.ScanCursor != 0 {
		t.Errorf("完成后 ScanCursor = %d, want 0（完整扫完应清游标）", got.ScanCursor)
	}
}

// TestHistoryTask_AutoRetryOnFailedMedia 校验有文件失败时自动重试被触发，
// 且重试只补失败文件（RetryOnly），不重扫整条历史。
func TestHistoryTask_AutoRetryOnFailedMedia(t *testing.T) {
	fc := newFakeClient()
	m := NewManager(fc, newTestStore(t), logger.New(logger.LevelError), 1, 1) // autoRetry=1
	m.retryBackoff = func(int) time.Duration { return time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(ctx)

	dto, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1")
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	// 首轮 2 个文件失败（一次性消费）→ 触发自动重试；重试轮为干净运行 → 应完成
	fc.setFailedMedia(dto.ID, 2)
	fc.release(dto.ID)

	got := waitForStatus(t, m, dto.ID, StatusCompleted, 2*testWaitTimeout)
	if got.Status != string(StatusCompleted) {
		t.Fatalf("重试补下成功后应为 completed，得到 %q", got.Status)
	}

	specs := fc.specsFor(dto.ID)
	if len(specs) < 2 {
		t.Fatalf("应执行 2 轮（首轮 + 重试），实际 %d 轮", len(specs))
	}
	if !specs[1].RetryOnly {
		t.Error("重试轮应设置 RetryOnly=true（历史已扫完，重扫无意义）")
	}
}

func TestRunCancelsPendingAutoRetryBackoff(t *testing.T) {
	fc := newFakeClient()
	m := NewManager(fc, newTestStore(t), logger.New(logger.LevelError), 1, 1)
	m.retryBackoff = func(int) time.Duration { return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(runDone)
	}()

	dto, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m, dto.ID, StatusRunning, testWaitTimeout)
	fc.setFailedMedia(dto.ID, 1)
	fc.release(dto.ID)
	deadline := time.Now().Add(testWaitTimeout)
	for {
		got, _ := m.Get(dto.ID)
		if Status(got.Status) == StatusQueued && got.Attempts == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("automatic retry did not enter backoff: %+v", got)
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(testWaitTimeout):
		t.Fatal("Manager.Run waited for the full retry backoff after cancellation")
	}
	if err := m.Wait(context.Background(), dto.ID); err != nil {
		t.Fatalf("Wait(backoff task after shutdown) error = %v", err)
	}
	row, err := m.store.GetTask(context.Background(), dto.ID)
	if err != nil || row == nil || row.Status != string(StatusQueued) {
		t.Fatalf("backoff task was not left recoverable: row=%+v err=%v", row, err)
	}
}
