package queue

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tg-down/internal/downloader"
	"tg-down/internal/logger"
	"tg-down/internal/store"
)

const testWaitTimeout = time.Second

// fakeClient 是 ChatDownloader 的测试替身：DownloadHistoryMedia 按 taskID 阻塞在一个延迟创建的
// gate 上，直至测试调用 release(taskID) 或 ctx 被取消，便于精确控制任务的执行时序。
type fakeClient struct {
	mu             sync.Mutex
	gates          map[string]chan struct{}
	errs           map[string]error
	calls          map[string]int
	counts         map[int64]int64
	countErrs      map[int64]error
	countCalls     map[int64]int
	cancelGates    map[string]chan struct{}
	recordFn       func(context.Context, *downloader.RecordEvent)
	scanProgressFn func(taskID string, scannedMessages, foundMedia, scanCursor int64)
	dupLookupFn    func(ctx context.Context, uniqueID string) (string, bool)
	specs          map[string][]downloader.HistorySpec
	failedMedia    map[string]int64
	maxMessageIDs  map[string]int64
	monitorTaskID  string
	monitorChatID  int64
	monitorTitle   string
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		gates:         make(map[string]chan struct{}),
		errs:          make(map[string]error),
		calls:         make(map[string]int),
		counts:        make(map[int64]int64),
		countErrs:     make(map[int64]error),
		countCalls:    make(map[int64]int),
		cancelGates:   make(map[string]chan struct{}),
		specs:         make(map[string][]downloader.HistorySpec),
		failedMedia:   make(map[string]int64),
		maxMessageIDs: make(map[string]int64),
	}
}

func (f *fakeClient) gate(id string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.gates[id]
	if !ok {
		ch = make(chan struct{})
		f.gates[id] = ch
	}
	return ch
}

// release 放行指定任务的 DownloadHistoryMedia 调用；err 非空时该调用将以此错误返回
func (f *fakeClient) release(id string) { close(f.gate(id)) }

func (f *fakeClient) setErr(id string, err error) {
	f.mu.Lock()
	f.errs[id] = err
	f.mu.Unlock()
}

func (f *fakeClient) callCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[id]
}

func (f *fakeClient) monitor() (taskID string, chatID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.monitorTaskID, f.monitorChatID
}

func (f *fakeClient) setCount(chatID, total int64) {
	f.mu.Lock()
	f.counts[chatID] = total
	f.mu.Unlock()
}

func (f *fakeClient) setCountErr(chatID int64, err error) {
	f.mu.Lock()
	f.countErrs[chatID] = err
	f.mu.Unlock()
}

func (f *fakeClient) CountHistoryMedia(_ context.Context, chatID int64, _ []string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.countCalls[chatID]++
	if err := f.countErrs[chatID]; err != nil {
		return 0, err
	}
	return f.counts[chatID], nil
}

func (f *fakeClient) DownloadHistoryMedia(
	ctx context.Context, spec *downloader.HistorySpec,
) (*downloader.HistoryResult, error) {
	taskID := spec.TaskID
	f.mu.Lock()
	f.calls[taskID]++
	f.specs[taskID] = append(f.specs[taskID], *spec)
	f.mu.Unlock()

	select {
	case <-f.gate(taskID):
	case <-ctx.Done():
		f.mu.Lock()
		cancelGate := f.cancelGates[taskID]
		f.mu.Unlock()
		if cancelGate != nil {
			<-cancelGate
		}
		return &downloader.HistoryResult{}, ctx.Err()
	}

	f.mu.Lock()
	err := f.errs[taskID]
	// 失败计数一次性消费：本轮报告 N 个文件失败，后续重试轮即为干净运行
	result := &downloader.HistoryResult{Failed: f.failedMedia[taskID], MaxMessageID: f.maxMessageIDs[taskID]}
	delete(f.failedMedia, taskID)
	f.mu.Unlock()
	return result, err
}

func (f *fakeClient) blockCancel(id string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	gate := make(chan struct{})
	f.cancelGates[id] = gate
	return gate
}

func (f *fakeClient) setMaxMessageID(id string, messageID int64) {
	f.mu.Lock()
	f.maxMessageIDs[id] = messageID
	f.mu.Unlock()
}

// setFailedMedia 让指定任务的下一次下载运行返回 n 个单文件失败（任务级 err 仍为 nil）
func (f *fakeClient) setFailedMedia(id string, n int64) {
	f.mu.Lock()
	f.failedMedia[id] = n
	f.mu.Unlock()
}

// specsFor 返回指定任务各轮运行收到的 spec（按顺序）
func (f *fakeClient) specsFor(id string) []downloader.HistorySpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]downloader.HistorySpec, len(f.specs[id]))
	copy(out, f.specs[id])
	return out
}

func (f *fakeClient) SetMonitorTask(taskID string, chatID int64, chatTitle string) {
	f.mu.Lock()
	f.monitorTaskID = taskID
	f.monitorChatID = chatID
	f.monitorTitle = chatTitle
	f.mu.Unlock()
}

func (f *fakeClient) SetRecordFunc(fn func(context.Context, *downloader.RecordEvent)) {
	f.mu.Lock()
	f.recordFn = fn
	f.mu.Unlock()
}

func (f *fakeClient) SetScanProgressFunc(fn func(taskID string, scannedMessages, foundMedia, scanCursor int64)) {
	f.mu.Lock()
	f.scanProgressFn = fn
	f.mu.Unlock()
}

func (f *fakeClient) SetDuplicateLookupFunc(fn func(ctx context.Context, uniqueID string) (string, bool)) {
	f.mu.Lock()
	f.dupLookupFn = fn
	f.mu.Unlock()
}

// newTestStore 创建一个基于临时文件的测试用 Store
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newTestManager 创建已启动 Run(ctx) 的 Manager
func newTestManager(t *testing.T) (*Manager, *fakeClient) {
	t.Helper()
	fc := newFakeClient()
	m := NewManager(fc, newTestStore(t), logger.New(logger.LevelError), 1, 0)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(ctx)

	return m, fc
}

// waitForStatus 轮询直至任务到达指定状态，超时则 Fatal
func waitForStatus(t *testing.T, m *Manager, id string, want Status, timeout time.Duration) TaskDTO {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		dto, ok := m.Get(id)
		if ok && Status(dto.Status) == want {
			return dto
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待任务 %s 状态变为 %s 超时，当前: %+v (ok=%v)", id, want, dto, ok)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestEnqueueLifecycle_QueuedRunningCompleted 验证任务依次经历 queued -> running -> completed
func TestEnqueueLifecycle_QueuedRunningCompleted(t *testing.T) {
	fc := newFakeClient()
	m := NewManager(fc, newTestStore(t), logger.New(logger.LevelError), 1, 0)

	dto, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1")
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if got, ok := m.Get(dto.ID); !ok || got.Status != string(StatusQueued) {
		t.Fatalf("Get() after Enqueue = %+v, ok=%v, want status=queued", got, ok)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	waitForStatus(t, m, dto.ID, StatusRunning, testWaitTimeout)
	fc.release(dto.ID)
	waitForStatus(t, m, dto.ID, StatusCompleted, testWaitTimeout)
}

// TestRunHistoryTask_CountsBeforeDownload 验证计数阶段先于下载完成：
// 下载仍阻塞在 gate 上时，ExpectedTotal 已可见且已落库，phase 已进入 downloading
func TestRunHistoryTask_CountsBeforeDownload(t *testing.T) {
	fc := newFakeClient()
	fc.setCount(1, 42)
	st := newTestStore(t)
	m := NewManager(fc, st, logger.New(logger.LevelError), 1, 0)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(ctx)

	dto, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1")
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitForStatus(t, m, dto.ID, StatusRunning, testWaitTimeout)

	// 下载被 gate 阻塞期间，总数与阶段应已就绪
	deadline := time.Now().Add(testWaitTimeout)
	for {
		got, ok := m.Get(dto.ID)
		if ok && got.ExpectedTotal == 42 && got.Phase == phaseDownloading {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待计数完成超时，当前: %+v", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	row, err := st.GetTask(context.Background(), dto.ID)
	if err != nil || row == nil || row.ExpectedTotal != 42 {
		t.Fatalf("store row = %+v, err = %v, want ExpectedTotal 42（下载前已落库）", row, err)
	}

	fc.release(dto.ID)
	final := waitForStatus(t, m, dto.ID, StatusCompleted, testWaitTimeout)
	if final.Phase != "" {
		t.Fatalf("终态 Phase = %q, want 空", final.Phase)
	}
	if final.ExpectedTotal != 42 {
		t.Fatalf("终态 ExpectedTotal = %d, want 42", final.ExpectedTotal)
	}
}

// TestRunHistoryTask_CountFailureFallsBack 验证计数失败仅回退为未知总数，任务照常执行
func TestRunHistoryTask_CountFailureFallsBack(t *testing.T) {
	m, fc := newTestManager(t)
	fc.setCountErr(1, errors.New("count boom"))

	dto, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1")
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitForStatus(t, m, dto.ID, StatusRunning, testWaitTimeout)
	fc.release(dto.ID)
	final := waitForStatus(t, m, dto.ID, StatusCompleted, testWaitTimeout)
	if final.ExpectedTotal != 0 {
		t.Fatalf("ExpectedTotal = %d, want 0（计数失败保持未知）", final.ExpectedTotal)
	}
}

// TestCancel_QueuedTask 验证取消排队中的任务不会触发其实际执行
func TestCancel_QueuedTask(t *testing.T) {
	m, fc := newTestManager(t)

	dto1, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1")
	if err != nil {
		t.Fatalf("Enqueue(task1) error = %v", err)
	}
	waitForStatus(t, m, dto1.ID, StatusRunning, testWaitTimeout)

	dto2, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 2}, "chat-2")
	if err != nil {
		t.Fatalf("Enqueue(task2) error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got, ok := m.Get(dto2.ID); !ok || got.Status != string(StatusQueued) {
		t.Fatalf("task2 should still be queued while task1 occupies the only worker, got %+v ok=%v", got, ok)
	}

	if err := m.Cancel(dto2.ID); err != nil {
		t.Fatalf("Cancel(task2) error = %v", err)
	}
	waitForStatus(t, m, dto2.ID, StatusCanceled, testWaitTimeout)

	fc.release(dto1.ID)
	waitForStatus(t, m, dto1.ID, StatusCompleted, testWaitTimeout)

	if calls := fc.callCount(dto2.ID); calls != 0 {
		t.Fatalf("canceled queued task should never call DownloadHistoryMedia, got %d calls", calls)
	}
}

// TestCancel_QueuedTask_DoneClosedOnce 验证排队中任务被取消时 done 立即关闭（cancelTask 的
// StatusQueued 分支），且该任务后续被 worker 从 historyCh 取出触发 runHistoryTask 的早退路径时，
// 对同一 done 的第二次 markDone 不会 panic（doneClosed 保证幂等）
func TestCancel_QueuedTask_DoneClosedOnce(t *testing.T) {
	m, fc := newTestManager(t)

	dto1, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1")
	if err != nil {
		t.Fatalf("Enqueue(task1) error = %v", err)
	}
	waitForStatus(t, m, dto1.ID, StatusRunning, testWaitTimeout)

	dto2, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 2}, "chat-2")
	if err != nil {
		t.Fatalf("Enqueue(task2) error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	if err := m.Cancel(dto2.ID); err != nil {
		t.Fatalf("Cancel(task2) error = %v", err)
	}
	waitForStatus(t, m, dto2.ID, StatusCanceled, testWaitTimeout)

	m.mu.Lock()
	t2 := m.tasks[dto2.ID]
	m.mu.Unlock()
	select {
	case <-t2.done:
	case <-time.After(testWaitTimeout):
		t.Fatal("done 通道未在 cancelTask 的 StatusQueued 分支关闭")
	}

	// 释放 task1，使 worker 空出后从 historyCh 取出已取消的 task2，
	// 命中 runHistoryTask 的早退路径，对同一 task2 再次 markDone
	fc.release(dto1.ID)
	waitForStatus(t, m, dto1.ID, StatusCompleted, testWaitTimeout)
	time.Sleep(50 * time.Millisecond)

	if calls := fc.callCount(dto2.ID); calls != 0 {
		t.Fatalf("canceled queued task should never call DownloadHistoryMedia, got %d calls", calls)
	}
}

func TestWaitForQueuedCancelIncludesFinalNotification(t *testing.T) {
	m, fc := newTestManager(t)
	first, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "first")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m, first.ID, StatusRunning, testWaitTimeout)
	second, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 2}, "second")
	if err != nil {
		t.Fatal(err)
	}

	notifyStarted := make(chan struct{})
	releaseNotify := make(chan struct{})
	m.SetOnChange(func(dto *TaskDTO) {
		if dto.ID == second.ID && Status(dto.Status) == StatusCanceled {
			close(notifyStarted)
			<-releaseNotify
		}
	})
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- m.Cancel(second.ID) }()
	select {
	case <-notifyStarted:
	case <-time.After(testWaitTimeout):
		t.Fatal("queued cancellation did not reach final notification")
	}
	row, err := m.store.GetTask(context.Background(), second.ID)
	if err != nil || row == nil || row.Status != string(StatusCanceled) || row.FinishedAt == nil {
		t.Fatalf("queued cancellation was not persisted before notification: row=%+v err=%v", row, err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- m.Wait(context.Background(), second.ID) }()
	fc.release(first.ID)
	waitForStatus(t, m, first.ID, StatusCompleted, testWaitTimeout)
	deadline := time.Now().Add(testWaitTimeout)
	for len(m.historyCh) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("worker did not consume the canceled queued task")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-waitDone:
		t.Fatalf("Wait returned before final notification completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseNotify)
	if err := <-cancelDone; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testWaitTimeout):
		t.Fatal("Wait did not return after queued cancellation finished")
	}
	if calls := fc.callCount(second.ID); calls != 0 {
		t.Fatalf("canceled queued task ran %d times", calls)
	}
}

// TestCancel_RunningTask 验证取消运行中的任务会取消其 ctx 并最终落定为 canceled
func TestCancel_RunningTask(t *testing.T) {
	m, fc := newTestManager(t)

	dto, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1")
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitForStatus(t, m, dto.ID, StatusRunning, testWaitTimeout)

	if err := m.Cancel(dto.ID); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	waitForStatus(t, m, dto.ID, StatusCanceled, testWaitTimeout)
	_ = fc // gate intentionally never released; cancellation must unblock via ctx
}

func TestWaitBlocksUntilCanceledTaskCleanupCompletes(t *testing.T) {
	m, fc := newTestManager(t)
	dto, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m, dto.ID, StatusRunning, testWaitTimeout)
	cleanupGate := fc.blockCancel(dto.ID)
	if err := m.Cancel(dto.ID); err != nil {
		t.Fatal(err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- m.Wait(context.Background(), dto.ID) }()
	select {
	case err := <-waitDone:
		t.Fatalf("Wait returned before downloader cleanup completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(cleanupGate)
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testWaitTimeout):
		t.Fatal("Wait did not return after downloader cleanup completed")
	}
	got, _ := m.Get(dto.ID)
	if Status(got.Status) != StatusCanceled {
		t.Fatalf("task status after Wait = %s", got.Status)
	}
}

func TestWaitHonorsContextAndRejectsUnknownTask(t *testing.T) {
	m, fc := newTestManager(t)
	dto, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m, dto.ID, StatusRunning, testWaitTimeout)
	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Wait(waitCtx, dto.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait(canceled context) error = %v", err)
	}
	if err := m.Wait(context.Background(), "missing"); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("Wait(missing) error = %v", err)
	}
	if err := m.Cancel(dto.ID); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m, dto.ID, StatusCanceled, testWaitTimeout)
	_ = fc
}

// TestRetry_FailedTask 验证重试失败任务会以新 ID 重新入队并真正再次执行
func TestRetry_FailedTask(t *testing.T) {
	m, fc := newTestManager(t)

	dto, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1")
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitForStatus(t, m, dto.ID, StatusRunning, testWaitTimeout)
	fc.setErr(dto.ID, errors.New("boom"))
	fc.release(dto.ID)
	waitForStatus(t, m, dto.ID, StatusFailed, testWaitTimeout)

	retried, err := m.Retry(dto.ID)
	if err != nil {
		t.Fatalf("Retry() error = %v", err)
	}
	if retried.ID == dto.ID {
		t.Fatal("Retry() should create a new task ID, got the same ID")
	}
	if retried.ChatID != dto.ChatID || retried.Kind != dto.Kind || retried.ChatTitle != dto.ChatTitle {
		t.Fatalf("Retry() = %+v, want same kind/chatID/chatTitle as %+v", retried, dto)
	}

	waitForStatus(t, m, retried.ID, StatusRunning, testWaitTimeout)
	fc.setErr(retried.ID, errors.New("boom again"))
	fc.release(retried.ID)
	waitForStatus(t, m, retried.ID, StatusFailed, testWaitTimeout)

	if calls := fc.callCount(retried.ID); calls != 1 {
		t.Fatalf("retried task should have actually run once, got %d calls", calls)
	}

	t.Run("disallowed on non-terminal status", func(t *testing.T) {
		dto2, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 2}, "chat-2")
		if err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}
		waitForStatus(t, m, dto2.ID, StatusRunning, testWaitTimeout)
		if _, err := m.Retry(dto2.ID); err == nil {
			t.Fatal("Retry() on a running task should return an error")
		}
		fc.release(dto2.ID)
		waitForStatus(t, m, dto2.ID, StatusCompleted, testWaitTimeout)
	})
}

// TestMaxConcurrentTasks_Serializes 验证 maxConcurrentTasks=1 时两个 history 任务严格串行执行
func TestMaxConcurrentTasks_Serializes(t *testing.T) {
	m, fc := newTestManager(t)

	dto1, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1")
	if err != nil {
		t.Fatalf("Enqueue(task1) error = %v", err)
	}
	waitForStatus(t, m, dto1.ID, StatusRunning, testWaitTimeout)

	dto2, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 2}, "chat-2")
	if err != nil {
		t.Fatalf("Enqueue(task2) error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got, ok := m.Get(dto2.ID); !ok || got.Status != string(StatusQueued) {
		t.Fatalf("task2 should not start running until task1 finishes, got %+v ok=%v", got, ok)
	}

	fc.release(dto1.ID)
	waitForStatus(t, m, dto1.ID, StatusCompleted, testWaitTimeout)

	waitForStatus(t, m, dto2.ID, StatusRunning, testWaitTimeout)
	fc.release(dto2.ID)
	waitForStatus(t, m, dto2.ID, StatusCompleted, testWaitTimeout)
}

// TestMonitor_DoesNotBlockHistoryQueue 是双通道设计的关键回归测试：
// monitor 任务必须独立于 history worker 池运行，即使 maxConcurrentTasks=1 且唯一的 worker
// 正被一个长期阻塞的 history 任务占用，Enqueue(KindMonitor, ...) 也不能被卡住。
func TestMonitor_DoesNotBlockHistoryQueue(t *testing.T) {
	m, fc := newTestManager(t)

	dto1, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1")
	if err != nil {
		t.Fatalf("Enqueue(history) error = %v", err)
	}
	waitForStatus(t, m, dto1.ID, StatusRunning, testWaitTimeout)

	done := make(chan struct{})
	var monDTO TaskDTO
	var monErr error
	go func() {
		monDTO, monErr = m.Enqueue(KindMonitor, &downloader.HistorySpec{ChatID: 999}, "monitor-chat")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(testWaitTimeout):
		t.Fatal("Enqueue(KindMonitor) blocked behind the history worker pool")
	}
	if monErr != nil {
		t.Fatalf("Enqueue(monitor) error = %v", monErr)
	}
	if monDTO.Status != string(StatusRunning) {
		t.Fatalf("monitor task status = %s, want running", monDTO.Status)
	}
	if mid, mchat := fc.monitor(); mid != monDTO.ID || mchat != 999 {
		t.Fatalf("client monitor association = (%s,%d), want (%s,999)", mid, mchat, monDTO.ID)
	}

	stopped, err := m.Enqueue(KindMonitor, &downloader.HistorySpec{ChatID: 0}, "")
	if err != nil {
		t.Fatalf("Enqueue(stop monitor) error = %v", err)
	}
	if stopped.ID != monDTO.ID {
		t.Fatalf("stopping monitor returned %+v, want canceled snapshot of %s", stopped, monDTO.ID)
	}
	waitForStatus(t, m, monDTO.ID, StatusCanceled, testWaitTimeout)
	if mid, mchat := fc.monitor(); mid != "" || mchat != 0 {
		t.Fatalf("client monitor association not cleared after stop: (%s,%d)", mid, mchat)
	}

	fc.release(dto1.ID)
	waitForStatus(t, m, dto1.ID, StatusCompleted, testWaitTimeout)
}

// TestMonitor_SwitchCancelsPrevious 验证切换监控目标会先取消旧的 monitor 任务再启动新的
func TestMonitor_SwitchCancelsPrevious(t *testing.T) {
	m, fc := newTestManager(t)

	first, err := m.Enqueue(KindMonitor, &downloader.HistorySpec{ChatID: 111}, "first")
	if err != nil {
		t.Fatalf("Enqueue(first monitor) error = %v", err)
	}
	waitForStatus(t, m, first.ID, StatusRunning, testWaitTimeout)

	second, err := m.Enqueue(KindMonitor, &downloader.HistorySpec{ChatID: 222}, "second")
	if err != nil {
		t.Fatalf("Enqueue(second monitor) error = %v", err)
	}
	waitForStatus(t, m, first.ID, StatusCanceled, testWaitTimeout)
	waitForStatus(t, m, second.ID, StatusRunning, testWaitTimeout)

	if mid, mchat := fc.monitor(); mid != second.ID || mchat != 222 {
		t.Fatalf("client monitor association = (%s,%d), want (%s,222)", mid, mchat, second.ID)
	}
}

// TestEnqueueHistory_DuplicateChatRejected 验证同一 chat_id 已存在排队中/运行中的 history 任务时，
// 重复 Enqueue 被拒绝，且既不创建新的内存任务也不写入新的 store 行；任务终结后允许重新入队。
func TestEnqueueHistory_DuplicateChatRejected(t *testing.T) {
	m, fc := newTestManager(t)

	dto1, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1")
	if err != nil {
		t.Fatalf("Enqueue(task1) error = %v", err)
	}
	waitForStatus(t, m, dto1.ID, StatusRunning, testWaitTimeout)

	if _, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1"); err == nil {
		t.Fatal("对处于 running 状态的同一 chat_id 重复 Enqueue 应返回错误")
	}

	dto2, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 2}, "chat-2")
	if err != nil {
		t.Fatalf("Enqueue(task2) error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got, ok := m.Get(dto2.ID); !ok || got.Status != string(StatusQueued) {
		t.Fatalf("task2 should still be queued while task1 occupies the only worker, got %+v ok=%v", got, ok)
	}

	if _, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 2}, "chat-2"); err == nil {
		t.Fatal("对处于 queued 状态的同一 chat_id 重复 Enqueue 应返回错误")
	}

	if got := len(m.List()); got != 2 {
		t.Fatalf("重复入队不应创建新任务，当前任务数 = %d, want 2", got)
	}
	rows, err := m.store.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("ListTasks() error = %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("重复入队不应写入新的 store 行，当前行数 = %d, want 2", len(rows))
	}

	fc.release(dto1.ID)
	waitForStatus(t, m, dto1.ID, StatusCompleted, testWaitTimeout)
	waitForStatus(t, m, dto2.ID, StatusRunning, testWaitTimeout)

	// task1 已终结，chat-1 应允许重新入队
	dto3, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1")
	if err != nil {
		t.Fatalf("任务终结后重新 Enqueue(chat-1) error = %v", err)
	}
	waitForStatus(t, m, dto3.ID, StatusQueued, testWaitTimeout)

	fc.release(dto2.ID)
	waitForStatus(t, m, dto2.ID, StatusCompleted, testWaitTimeout)
	waitForStatus(t, m, dto3.ID, StatusRunning, testWaitTimeout)
	fc.release(dto3.ID)
	waitForStatus(t, m, dto3.ID, StatusCompleted, testWaitTimeout)
}

// TestNewManager_ResumesInterruptedTasksFromStore 验证 v2.0 断点续跑：重启前运行中的
// history 任务以同一 id 重置为 queued 并在 Run 启动后从持久化游标续扫；
// 运行中的 monitor 任务自动恢复；终态任务原样载入；List() 保持最新优先的顺序。
func TestNewManager_ResumesInterruptedTasksFromStore(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	interrupted := &store.TaskRow{
		ID: "old-1", Kind: string(KindHistory), ChatID: 1, ChatTitle: "chat-1",
		Status: string(StatusRunning), CreatedAt: time.Now().Add(-2 * time.Minute),
		ScanCursor: 777, StopAtMessageID: 555, ScheduleID: "schedule-old",
	}
	if err := st.CreateTask(ctx, interrupted); err != nil {
		t.Fatalf("CreateTask(interrupted) error = %v", err)
	}
	monitor := &store.TaskRow{
		ID: "mon-1", Kind: string(KindMonitor), ChatID: 9, ChatTitle: "chat-9",
		Status: string(StatusRunning), CreatedAt: time.Now().Add(-time.Minute),
	}
	if err := st.CreateTask(ctx, monitor); err != nil {
		t.Fatalf("CreateTask(monitor) error = %v", err)
	}
	done := &store.TaskRow{
		ID: "new-1", Kind: string(KindHistory), ChatID: 2, ChatTitle: "chat-2",
		Status: string(StatusCompleted), CreatedAt: time.Now(),
	}
	if err := st.CreateTask(ctx, done); err != nil {
		t.Fatalf("CreateTask(done) error = %v", err)
	}

	fc := newFakeClient()
	m := NewManager(fc, st, logger.New(logger.LevelError), 1, 0)

	list := m.List()
	if len(list) != 3 {
		t.Fatalf("List() len = %d, want 3", len(list))
	}
	if list[0].ID != "new-1" || list[1].ID != "mon-1" || list[2].ID != "old-1" {
		t.Fatalf("List() order = [%s,%s,%s], want [new-1,mon-1,old-1] (最新优先)", list[0].ID, list[1].ID, list[2].ID)
	}
	if list[0].Status != string(StatusCompleted) {
		t.Fatalf("终态任务不应被改动，got status = %s", list[0].Status)
	}
	if list[2].Status != string(StatusQueued) {
		t.Fatalf("中断的 history 任务应重置为 queued 待恢复，got status=%s", list[2].Status)
	}
	waitCtx, stopWait := context.WithTimeout(context.Background(), testWaitTimeout)
	defer stopWait()
	if err := m.Wait(waitCtx, "new-1"); err != nil {
		t.Fatalf("Wait(restored terminal task) error = %v", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(runCtx)

	// history 任务恢复执行：同一 id、从持久化游标续扫
	waitForStatus(t, m, "old-1", StatusRunning, testWaitTimeout)
	fc.release("old-1")
	waitForStatus(t, m, "old-1", StatusCompleted, testWaitTimeout)
	fc.mu.Lock()
	specs := fc.specs["old-1"]
	monitorTaskID, monitorChatID := fc.monitorTaskID, fc.monitorChatID
	fc.mu.Unlock()
	if len(specs) != 1 || specs[0].FromMessageID != 777 ||
		specs[0].StopAtMessageID != 555 || specs[0].ScheduleID != "schedule-old" {
		t.Fatalf("恢复任务未携带完整的持久化运行参数: %+v", specs)
	}

	// monitor 任务自动恢复：同一 id 重新关联 client
	if monitorTaskID != "mon-1" || monitorChatID != 9 {
		t.Fatalf("monitor 任务未恢复: SetMonitorTask(%q, %d), want (mon-1, 9)", monitorTaskID, monitorChatID)
	}

	row2, err := st.GetTask(ctx, "new-1")
	if err != nil {
		t.Fatalf("GetTask(new-1) error = %v", err)
	}
	if row2 == nil || row2.Status != string(StatusCompleted) {
		t.Fatalf("终态任务的 store 行不应被改动: %+v", row2)
	}
}

// TestHandleRecordEvent_AsyncPersistPreservesOrder 验证下载记录持久化异步化后：
// 事件最终仍会到达 store（轮询等待，而非同步断言），且同一媒体项的 Started 先于 Completed 落盘
// ——若顺序颠倒，该记录会停留在 downloading 而非到达 completed。
func TestHandleRecordEvent_AsyncPersistPreservesOrder(t *testing.T) {
	m, fc := newTestManager(t)

	dto, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1")
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitForStatus(t, m, dto.ID, StatusRunning, testWaitTimeout)

	fc.mu.Lock()
	recordFn := fc.recordFn
	fc.mu.Unlock()
	if recordFn == nil {
		t.Fatal("Manager 未通过 SetRecordFunc 注册记录回调")
	}

	media := &downloader.MediaInfo{
		TaskID: dto.ID, ChatID: 1, MessageID: 42,
		MediaType: "photo", FileName: "a.jpg", FileSize: 100,
	}
	ctx := context.Background()
	recordFn(ctx, &downloader.RecordEvent{Media: media, Status: downloader.RecordQueued, FilePath: "/tmp/a.jpg"})
	recordFn(ctx, &downloader.RecordEvent{Media: media, Status: downloader.RecordCompleted, FilePath: "/tmp/a.jpg"})

	deadline := time.Now().Add(testWaitTimeout)
	var recs []*store.HistoryRecord
	for {
		page, qErr := m.store.QueryHistory(ctx, &store.HistoryFilter{ChatID: 1})
		if qErr != nil {
			t.Fatalf("QueryHistory() error = %v", qErr)
		}
		recs = page.Items
		if len(recs) == 1 && recs[0].Status == store.HistoryStatusCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待记录异步落盘为 completed 超时，当前记录: %+v", recs)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if recs[0].FileSize != media.FileSize {
		t.Fatalf("记录 FileSize = %d, want %d", recs[0].FileSize, media.FileSize)
	}

	fc.release(dto.ID)
	waitForStatus(t, m, dto.ID, StatusCompleted, testWaitTimeout)
}

// TestRunHistoryTask_AutoRetry 验证任务级自动重试：失败后同一 id 退避重入队并续扫，
// 重试耗尽后落定为 failed（最终失败点）
func TestRunHistoryTask_AutoRetry(t *testing.T) {
	fc := newFakeClient()
	m := NewManager(fc, newTestStore(t), logger.New(logger.LevelError), 1, 2)
	m.retryBackoff = func(int) time.Duration { return time.Millisecond }

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(ctx)

	dto, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "chat-1")
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitForStatus(t, m, dto.ID, StatusRunning, testWaitTimeout)

	// 三次执行（首跑 + 2 次自动重试）全部失败
	fc.mu.Lock()
	fc.errs[dto.ID] = errors.New("network down")
	fc.mu.Unlock()
	fc.release(dto.ID)

	waitForStatus(t, m, dto.ID, StatusFailed, testWaitTimeout)
	got, _ := m.Get(dto.ID)
	if got.Attempts != 2 {
		t.Fatalf("Attempts = %d, want 2", got.Attempts)
	}
	fc.mu.Lock()
	calls := fc.calls[dto.ID]
	fc.mu.Unlock()
	if calls != 3 {
		t.Fatalf("DownloadHistoryMedia 调用次数 = %d, want 3（首跑+2次重试）", calls)
	}
}

// TestEnqueue_FiltersFlowThroughSpecAndRetry 验证任务过滤器与单消息参数：
// 传入 Enqueue 的过滤器随 HistorySpec 抵达 client、落库持久化，且手动 Retry 后仍然携带
func TestEnqueue_FiltersFlowThroughSpecAndRetry(t *testing.T) {
	fc := newFakeClient()
	st := newTestStore(t)
	m := NewManager(fc, st, logger.New(logger.LevelError), 1, 0)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(ctx)

	filters := downloader.HistoryFilters{MediaTypes: []string{"photo"}, DateFrom: 1700000000, MaxFileSize: 1 << 20}
	dto, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1, Filters: filters}, "chat-1")
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if dto.Filters == nil || dto.Filters.MediaTypes[0] != "photo" {
		t.Fatalf("DTO 未携带过滤器: %+v", dto.Filters)
	}

	waitForStatus(t, m, dto.ID, StatusRunning, testWaitTimeout)
	fc.setErr(dto.ID, errors.New("boom"))
	fc.release(dto.ID)
	waitForStatus(t, m, dto.ID, StatusFailed, testWaitTimeout)

	fc.mu.Lock()
	specs := fc.specs[dto.ID]
	fc.mu.Unlock()
	if len(specs) != 1 || specs[0].Filters.MaxFileSize != 1<<20 || len(specs[0].Filters.MediaTypes) != 1 {
		t.Fatalf("spec 未携带过滤器: %+v", specs)
	}

	row, err := st.GetTask(context.Background(), dto.ID)
	if err != nil || row == nil || row.Filters == "" {
		t.Fatalf("过滤器未落库: row=%+v err=%v", row, err)
	}

	// 手动重试产生新任务，过滤器随行
	retryDTO, err := m.Retry(dto.ID)
	if err != nil {
		t.Fatalf("Retry() error = %v", err)
	}
	waitForStatus(t, m, retryDTO.ID, StatusRunning, testWaitTimeout)
	fc.release(retryDTO.ID)
	waitForStatus(t, m, retryDTO.ID, StatusCompleted, testWaitTimeout)
	fc.mu.Lock()
	retrySpecs := fc.specs[retryDTO.ID]
	fc.mu.Unlock()
	if len(retrySpecs) != 1 || retrySpecs[0].Filters.DateFrom != 1700000000 {
		t.Fatalf("Retry 未携带过滤器: %+v", retrySpecs)
	}
}

func TestTaskFiltersDoNotAliasCallerOrDTO(t *testing.T) {
	m := NewManager(newFakeClient(), newTestStore(t), logger.New(logger.LevelError), 1, 0)
	filters := downloader.HistoryFilters{MediaTypes: []string{"photo"}}
	dto, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1, Filters: filters}, "chat")
	if err != nil {
		t.Fatal(err)
	}
	filters.MediaTypes[0] = "video"
	dto.Filters.MediaTypes[0] = "document"

	got, ok := m.Get(dto.ID)
	if !ok || got.Filters == nil || len(got.Filters.MediaTypes) != 1 || got.Filters.MediaTypes[0] != "photo" {
		t.Fatalf("internal filters were mutated through an external snapshot: %+v", got.Filters)
	}
}

// TestFireDueSchedules 验证定时计划：到期触发入队并更新 last_run；
// 未到期/运行中重叠时不重复触发
func TestFireDueSchedules(t *testing.T) {
	fc := newFakeClient()
	st := newTestStore(t)
	m := NewManager(fc, st, logger.New(logger.LevelError), 1, 0)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(ctx)

	if err := st.CreateSchedule(ctx, &store.ScheduleRow{
		ID: "s1", ChatID: 7, ChatTitle: "chat-7", IntervalMin: 10, Enabled: true,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateSchedule() error = %v", err)
	}

	// last_run 为空 → 立即到期触发
	m.fireDueSchedules(ctx)
	deadline := time.Now().Add(testWaitTimeout)
	var taskID string
	for taskID == "" {
		for _, dto := range m.List() {
			if dto.ChatID == 7 && dto.Kind == string(KindHistory) {
				taskID = dto.ID
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("定时计划未触发任务")
		}
		time.Sleep(5 * time.Millisecond)
	}
	rows, _ := st.ListSchedules(ctx)
	if len(rows) != 1 || rows[0].LastRun == nil {
		t.Fatalf("last_run 未更新: %+v", rows[0])
	}

	// 任务仍在运行且未到期 → 不重复触发
	m.fireDueSchedules(ctx)
	count := 0
	for _, dto := range m.List() {
		if dto.ChatID == 7 && dto.Kind == string(KindHistory) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("到期前重复触发: %d 个任务", count)
	}

	fc.release(taskID)
	waitForStatus(t, m, taskID, StatusCompleted, testWaitTimeout)
}

func TestHistoryCountSkippedForUnsupportedFilters(t *testing.T) {
	fc := newFakeClient()
	fc.setCount(1, 500)
	m := NewManager(fc, newTestStore(t), logger.New(logger.LevelError), 1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(ctx)

	dto, err := m.Enqueue(KindHistory, &downloader.HistorySpec{
		ChatID: 1, Filters: downloader.HistoryFilters{DateFrom: 1700000000, Query: "report"},
	}, "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(testWaitTimeout)
	for fc.callCount(dto.ID) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("filtered task did not reach the download phase")
		}
		time.Sleep(time.Millisecond)
	}
	got, _ := m.Get(dto.ID)
	if got.ExpectedTotal != 0 {
		t.Fatalf("filtered task ExpectedTotal = %d, want unknown (0)", got.ExpectedTotal)
	}
	fc.mu.Lock()
	countCalls := fc.countCalls[1]
	fc.mu.Unlock()
	if countCalls != 0 {
		t.Fatalf("inexact CountHistoryMedia called %d times", countCalls)
	}
	fc.release(dto.ID)
	waitForStatus(t, m, dto.ID, StatusCompleted, testWaitTimeout)
}

func TestEnqueueHistoryQueueFullReturnsAndRollsBack(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(newFakeClient(), st, logger.New(logger.LevelError), 1, 0)
	for i := 0; i < historyQueueBuffer; i++ {
		if _, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: int64(i + 1)}, "queued"); err != nil {
			t.Fatalf("fill queue at %d: %v", i, err)
		}
	}

	type result struct {
		dto TaskDTO
		err error
	}
	done := make(chan result, 1)
	go func() {
		dto, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: historyQueueBuffer + 1}, "overflow")
		done <- result{dto: dto, err: err}
	}()
	select {
	case got := <-done:
		if got.err == nil || !strings.Contains(got.err.Error(), "队列已满") {
			t.Fatalf("overflow enqueue = (%+v, %v)", got.dto, got.err)
		}
	case <-time.After(testWaitTimeout):
		t.Fatal("overflow enqueue blocked instead of returning an error")
	}

	if got := len(m.List()); got != historyQueueBuffer {
		t.Fatalf("in-memory task count = %d, want %d", got, historyQueueBuffer)
	}
	rows, err := st.ListTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(rows); got != historyQueueBuffer {
		t.Fatalf("persisted task count = %d, want %d", got, historyQueueBuffer)
	}
}

func TestScheduleWatermarkAdvancesOnlyAfterCompleteSuccess(t *testing.T) {
	fc := newFakeClient()
	st := newTestStore(t)
	m := NewManager(fc, st, logger.New(logger.LevelError), 1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(ctx)
	if err := st.CreateSchedule(ctx, &store.ScheduleRow{
		ID: "s-water", ChatID: 7, IntervalMin: 10, Enabled: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateScheduleLastMaxID(ctx, "s-water", 100); err != nil {
		t.Fatal(err)
	}

	run := func(chatID int64, resultMax, failed int64, runErr error, want Status) {
		t.Helper()
		dto, err := m.Enqueue(KindHistory, &downloader.HistorySpec{
			ChatID: chatID, StopAtMessageID: 100, ScheduleID: "s-water",
		}, "scheduled")
		if err != nil {
			t.Fatal(err)
		}
		fc.setMaxMessageID(dto.ID, resultMax)
		fc.setFailedMedia(dto.ID, failed)
		if runErr != nil {
			fc.setErr(dto.ID, runErr)
		}
		fc.release(dto.ID)
		waitForStatus(t, m, dto.ID, want, testWaitTimeout)
	}

	run(7, 200, 0, errors.New("scan failed"), StatusFailed)
	rows, _ := st.ListSchedules(ctx)
	if rows[0].LastMaxID != 100 {
		t.Fatalf("watermark advanced after task error: %d", rows[0].LastMaxID)
	}
	run(8, 250, 1, nil, StatusPartial)
	rows, _ = st.ListSchedules(ctx)
	if rows[0].LastMaxID != 100 {
		t.Fatalf("watermark advanced after media failure: %d", rows[0].LastMaxID)
	}
	run(9, 300, 0, nil, StatusCompleted)
	rows, _ = st.ListSchedules(ctx)
	if rows[0].LastMaxID != 300 {
		t.Fatalf("watermark after complete success = %d, want 300", rows[0].LastMaxID)
	}
}

func TestRunShutdownKeepsHistoryAndMonitorRecoverable(t *testing.T) {
	fc := newFakeClient()
	st := newTestStore(t)
	m := NewManager(fc, st, logger.New(logger.LevelError), 1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(runDone)
	}()

	history, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "history")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m, history.ID, StatusRunning, testWaitTimeout)
	monitor, err := m.Enqueue(KindMonitor, &downloader.HistorySpec{ChatID: 2}, "monitor")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m, monitor.ID, StatusRunning, testWaitTimeout)

	cancel()
	select {
	case <-runDone:
	case <-time.After(testWaitTimeout):
		t.Fatal("Manager.Run did not wait for task shutdown")
	}
	historyRow, err := st.GetTask(context.Background(), history.ID)
	if err != nil || historyRow == nil || historyRow.Status != string(StatusQueued) {
		t.Fatalf("history task not recoverable after shutdown: row=%+v err=%v", historyRow, err)
	}
	monitorRow, err := st.GetTask(context.Background(), monitor.ID)
	if err != nil || monitorRow == nil || monitorRow.Status != string(StatusRunning) {
		t.Fatalf("monitor task not recoverable after shutdown: row=%+v err=%v", monitorRow, err)
	}
}

func TestShutdownRacingEnqueueNeverLeavesSuccessfulTaskUnwaitable(t *testing.T) {
	fc := newFakeClient()
	st := newTestStore(t)
	m := NewManager(fc, st, logger.New(logger.LevelError), 4, 0)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(runDone)
	}()
	seed, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "seed")
	if err != nil {
		t.Fatal(err)
	}

	const contenders = 64
	start := make(chan struct{})
	type enqueueResult struct {
		dto TaskDTO
		err error
	}
	results := make(chan enqueueResult, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(chatID int64) {
			defer wg.Done()
			<-start
			dto, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: chatID}, "race")
			results <- enqueueResult{dto: dto, err: err}
		}(int64(i + 2))
	}
	close(start)
	cancel()
	wg.Wait()
	close(results)
	select {
	case <-runDone:
	case <-time.After(testWaitTimeout):
		t.Fatal("Manager.Run did not finish during enqueue race")
	}

	succeeded := []string{seed.ID}
	for result := range results {
		if result.err == nil {
			succeeded = append(succeeded, result.dto.ID)
		}
	}
	for _, id := range succeeded {
		waitCtx, stopWait := context.WithTimeout(context.Background(), testWaitTimeout)
		err := m.Wait(waitCtx, id)
		stopWait()
		if err != nil {
			t.Fatalf("successful enqueue %s remained unwaitable after shutdown: %v", id, err)
		}
		row, err := st.GetTask(context.Background(), id)
		if err != nil || row == nil {
			t.Fatalf("successful enqueue %s is not recoverable: row=%+v err=%v", id, row, err)
		}
	}
	if _, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 999}, "stopped"); err == nil {
		t.Fatal("enqueue after shutdown unexpectedly succeeded")
	}
}

func TestBeginDrainBlocksNewWorkAtTaskSnapshotBoundary(t *testing.T) {
	m := NewManager(newFakeClient(), newTestStore(t), logger.New(logger.LevelError), 1, 0)
	queued, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 1}, "queued")
	if err != nil {
		t.Fatal(err)
	}
	addPartialTaskForRetryTest(t, m, "partial-drain")
	ids, err := m.BeginDrain()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != queued.ID {
		t.Fatalf("BeginDrain() ids = %v, want [%s]", ids, queued.ID)
	}
	if _, err := m.BeginDrain(); err == nil {
		t.Fatal("second BeginDrain unexpectedly succeeded")
	}
	if _, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 2}, "blocked"); err == nil {
		t.Fatal("Enqueue succeeded while draining")
	}
	if _, err := m.Retry("partial-drain"); err == nil {
		t.Fatal("Retry succeeded while draining")
	}
	beforeSchedule := len(m.List())
	if err := m.store.CreateSchedule(context.Background(), &store.ScheduleRow{
		ID: "drain-schedule", ChatID: 3, IntervalMin: MinScheduleIntervalMin,
		Enabled: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	m.fireDueSchedules(context.Background())
	if got := len(m.List()); got != beforeSchedule {
		t.Fatalf("scheduler added work while draining: before=%d after=%d", beforeSchedule, got)
	}
	schedules, err := m.store.ListSchedules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(schedules) != 1 || schedules[0].LastRun != nil {
		t.Fatalf("scheduler advanced last_run while draining: %+v", schedules)
	}
	if err := m.Cancel(queued.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Wait(context.Background(), queued.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.EndDrain(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Enqueue(KindHistory, &downloader.HistorySpec{ChatID: 2}, "resumed"); err != nil {
		t.Fatalf("Enqueue after EndDrain: %v", err)
	}
}

func TestRunReturnRejectsLateRecordWithoutPersistence(t *testing.T) {
	m := NewManager(newFakeClient(), newTestStore(t), logger.New(logger.LevelError), 1, 0)
	var recorderCalls int
	m.recorder = func(context.Context, *downloader.RecordEvent) { recorderCalls++ }
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

	eventDone := make(chan struct{})
	go func() {
		m.handleRecordEvent(context.Background(), &downloader.RecordEvent{
			Media:  &downloader.MediaInfo{TaskID: "late", ChatID: 1, MessageID: 1},
			Status: downloader.RecordQueued,
		})
		close(eventDone)
	}()
	select {
	case <-eventDone:
	case <-time.After(testWaitTimeout):
		t.Fatal("late record event blocked after Manager.Run returned")
	}
	if recorderCalls != 0 || len(m.recordCh) != 0 {
		t.Fatalf("late event persisted or entered a dead queue: calls=%d queued=%d", recorderCalls, len(m.recordCh))
	}
}

func TestCorruptPersistedFiltersAreNotRecoveredOrRetried(t *testing.T) {
	st := newTestStore(t)
	if err := st.CreateTask(context.Background(), &store.TaskRow{
		ID: "bad-filter", Kind: string(KindHistory), ChatID: 1, Status: string(StatusRunning),
		CreatedAt: time.Now(), Filters: `{broken`,
	}); err != nil {
		t.Fatal(err)
	}
	fc := newFakeClient()
	m := NewManager(fc, st, logger.New(logger.LevelError), 1, 0)
	dto, ok := m.Get("bad-filter")
	if !ok || Status(dto.Status) != StatusFailed || !strings.Contains(dto.Error, "过滤器") {
		t.Fatalf("corrupt task was not failed safely: %+v", dto)
	}
	row, _ := st.GetTask(context.Background(), "bad-filter")
	if row.Status != string(StatusFailed) {
		t.Fatalf("corrupt task store status = %s", row.Status)
	}
	if _, err := m.Retry("bad-filter"); err == nil || !strings.Contains(err.Error(), "参数损坏") {
		t.Fatalf("Retry(corrupt task) error = %v", err)
	}
	if calls := fc.callCount("bad-filter"); calls != 0 {
		t.Fatalf("corrupt task unexpectedly ran %d times", calls)
	}
}

func TestFireDueSchedulesSkipsInvalidPersistedValues(t *testing.T) {
	fc := newFakeClient()
	st := newTestStore(t)
	m := NewManager(fc, st, logger.New(logger.LevelError), 1, 0)
	ctx := context.Background()
	rows := []*store.ScheduleRow{
		{ID: "zero", ChatID: 1, IntervalMin: 0, Filters: "", Enabled: true, CreatedAt: time.Now()},
		{ID: "huge", ChatID: 2, IntervalMin: int(MaxScheduleIntervalMin + 1), Enabled: true, CreatedAt: time.Now()},
		{ID: "json", ChatID: 3, IntervalMin: 10, Filters: `{broken`, Enabled: true, CreatedAt: time.Now()},
		{ID: "semantic", ChatID: 4, IntervalMin: 10, Filters: `{"media_types":["bogus"]}`, Enabled: true, CreatedAt: time.Now()},
	}
	for _, row := range rows {
		if err := st.CreateSchedule(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	m.fireDueSchedules(ctx)
	if got := len(m.List()); got != 0 {
		t.Fatalf("invalid schedules created %d tasks", got)
	}
	stored, err := st.ListSchedules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range stored {
		if row.LastRun != nil {
			t.Errorf("invalid schedule %s unexpectedly updated last_run", row.ID)
		}
	}
}
