package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tg-down/internal/config"
	"tg-down/internal/downloader"
	"tg-down/internal/logger"
	"tg-down/internal/queue"
	"tg-down/internal/store"
	"tg-down/internal/tgapi"
)

type slowLogoutClient struct {
	tgapi.Client
	downloadStarted chan struct{}
	releaseDownload chan struct{}
	logoutCalled    chan struct{}
	logoutOnce      sync.Once
	logoutCalls     atomic.Int32
}

func (c *slowLogoutClient) CountHistoryMedia(context.Context, int64, []string) (int64, error) {
	return 1, nil
}

func (c *slowLogoutClient) DownloadHistoryMedia(ctx context.Context, _ *downloader.HistorySpec) (*downloader.HistoryResult, error) {
	close(c.downloadStarted)
	<-c.releaseDownload
	return nil, ctx.Err()
}

func (c *slowLogoutClient) SetMonitorTask(string, int64, string) {}

func (c *slowLogoutClient) SetRecordFunc(func(context.Context, *downloader.RecordEvent)) {}

func (c *slowLogoutClient) SetScanProgressFunc(func(string, int64, int64, int64)) {}

func (c *slowLogoutClient) SetDuplicateLookupFunc(func(context.Context, string) (string, bool)) {}

func (c *slowLogoutClient) Logout(context.Context) error {
	c.logoutCalls.Add(1)
	c.logoutOnce.Do(func() { close(c.logoutCalled) })
	return nil
}

func TestHandleAuthLogoutWaitsForCanceledDownloads(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "logout.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	client := &slowLogoutClient{
		downloadStarted: make(chan struct{}),
		releaseDownload: make(chan struct{}),
		logoutCalled:    make(chan struct{}),
	}
	cfg := &config.Config{
		Download: config.DownloadConfig{Path: t.TempDir()},
		Queue:    config.QueueConfig{MaxConcurrentTasks: 1},
	}
	s := New(client, st, logger.New("error"), DefaultAddr, cfg)
	s.setState(StateReady)

	runCtx, stop := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		s.queue.Run(runCtx)
		close(runDone)
	}()
	t.Cleanup(func() {
		stop()
		<-runDone
	})

	if _, err := s.queue.Enqueue(
		queue.KindHistory,
		&downloader.HistorySpec{ChatID: 1},
		"test",
	); err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.downloadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("download did not start")
	}

	res := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		s.handleAuthLogout(res, httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil))
		close(done)
	}()

	select {
	case <-client.logoutCalled:
		t.Fatal("client.Logout ran before canceled download cleanup finished")
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-done:
		t.Fatal("logout handler returned before canceled download cleanup finished")
	case <-time.After(50 * time.Millisecond):
	}
	secondRes := httptest.NewRecorder()
	s.handleAuthLogout(secondRes, httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil))
	if secondRes.Code != http.StatusConflict {
		t.Fatalf("concurrent logout status = %d, want 409", secondRes.Code)
	}

	close(client.releaseDownload)
	select {
	case <-client.logoutCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("client.Logout was not called after download cleanup")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("logout handler did not return")
	}
	if res.Code != http.StatusOK {
		t.Fatalf("logout status = %d, want 200 (%s)", res.Code, res.Body.String())
	}
	if calls := client.logoutCalls.Load(); calls != 1 {
		t.Fatalf("client.Logout calls = %d, want 1", calls)
	}
}

func TestHandleTasksCreateRejectedAcrossQueueDrainBoundary(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "drain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	client := &slowLogoutClient{
		downloadStarted: make(chan struct{}),
		releaseDownload: make(chan struct{}),
		logoutCalled:    make(chan struct{}),
	}
	s := New(client, st, logger.New("error"), DefaultAddr, &config.Config{
		Download: config.DownloadConfig{Path: t.TempDir()},
		Queue:    config.QueueConfig{MaxConcurrentTasks: 1},
	})
	s.setState(StateReady)
	if _, err := s.queue.BeginDrain(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.queue.EndDrain() })

	res := httptest.NewRecorder()
	s.handleTasksCreate(res, httptest.NewRequest(
		http.MethodPost,
		"/api/tasks",
		strings.NewReader(`{"kind":"history","chat_id":1}`),
	))
	if res.Code != http.StatusConflict {
		t.Fatalf("task create status = %d, want 409 after drain boundary (%s)", res.Code, res.Body.String())
	}
	if tasks := s.queue.List(); len(tasks) != 0 {
		t.Fatalf("task crossed drain boundary: %#v", tasks)
	}
}
