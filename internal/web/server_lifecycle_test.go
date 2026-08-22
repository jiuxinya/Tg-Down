package web

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tg-down/internal/config"
	"tg-down/internal/downloader"
	"tg-down/internal/logger"
	"tg-down/internal/store"
	"tg-down/internal/tgapi"
)

type blockingCloseClient struct {
	tgapi.Client
	closeStarted chan struct{}
	releaseClose chan struct{}
	closeOnce    sync.Once
}

func (c *blockingCloseClient) SetRecordFunc(func(context.Context, *downloader.RecordEvent)) {}

func (c *blockingCloseClient) SetScanProgressFunc(func(string, int64, int64, int64)) {}

func (c *blockingCloseClient) SetDuplicateLookupFunc(func(context.Context, string) (string, bool)) {
}

func (c *blockingCloseClient) HasCredentials() bool { return false }

func (c *blockingCloseClient) Close() {
	c.closeOnce.Do(func() { close(c.closeStarted) })
	<-c.releaseClose
}

func (c *blockingCloseClient) Phone() string { return "" }

func (c *blockingCloseClient) TargetChat() int64 { return 0 }

func (c *blockingCloseClient) Stats() downloader.Stats { return downloader.Stats{} }

func (c *blockingCloseClient) ActiveMedia() []downloader.MediaProgress { return nil }

func (c *blockingCloseClient) DownloadConcurrency() int { return 1 }

func (c *blockingCloseClient) ActiveDownloadCount() int { return 0 }

func (c *blockingCloseClient) AllMediaPaused() bool { return false }

func (c *blockingCloseClient) DownloadSpeed() int64 { return 0 }

func (c *blockingCloseClient) ConnectionState() string { return "" }

func TestRunWaitsForBackgroundOnListenFailure(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	client := &blockingCloseClient{
		closeStarted: make(chan struct{}),
		releaseClose: make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(client.releaseClose) }) }
	t.Cleanup(release)

	cfg := &config.Config{Download: config.DownloadConfig{Path: t.TempDir()}}
	s := New(client, st, logger.New(logger.LevelError), "127.0.0.1:-1", cfg)
	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(context.Background()) }()

	select {
	case <-client.closeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Telegram background loop did not observe HTTP listen failure")
	}

	select {
	case err := <-runDone:
		t.Fatalf("Run returned before Telegram background loop exited: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "HTTP") {
			t.Fatalf("Run() error = %v, want HTTP listen failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after all background loops exited")
	}
}

func TestRunWaitsForBackgroundOnContextCancellation(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	client := &blockingCloseClient{
		closeStarted: make(chan struct{}),
		releaseClose: make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(client.releaseClose) }) }
	t.Cleanup(release)

	ctx, cancel := context.WithCancel(context.Background())
	cfg := &config.Config{Download: config.DownloadConfig{Path: t.TempDir()}}
	s := New(client, st, logger.New(logger.LevelError), DefaultAddr, cfg)
	serveStarted := make(chan struct{})
	s.serveHTTP = func(*http.Server) error {
		close(serveStarted)
		<-ctx.Done()
		return http.ErrServerClosed
	}

	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(ctx) }()
	select {
	case <-serveStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP serving did not start")
	}
	cancel()
	select {
	case <-client.closeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Telegram background loop did not observe context cancellation")
	}
	select {
	case err := <-runDone:
		t.Fatalf("Run returned before Telegram background loop exited: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil on context cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after all background loops exited")
	}
}
