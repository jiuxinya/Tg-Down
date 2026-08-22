package downloader

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"tg-down/internal/logger"
)

// TestDuplicateCopy_RespectsConcurrencyLimit 校验内容级去重的查库与复制受下载并发限制约束。
//
// 回归点：此前去重发生在抢占下载槽之前，于是在途上限（partition_size，默认 100）会让上百个
// goroutine 同时做整文件复制并争抢 4 条 SQLite 连接，而 max_concurrent（默认 5）完全管不到——
// 磁盘 IO 直接被打满，用户设置的并发数形同虚设。
func TestDuplicateCopy_RespectsConcurrencyLimit(t *testing.T) {
	dir := t.TempDir()
	const limit = 2
	d := New(dir, limit, logger.New(logger.LevelError))

	src := filepath.Join(dir, "source.bin")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatalf("WriteFile(src) error = %v", err)
	}

	const total = 8
	var inFlight, maxInFlight atomic.Int64
	arrived := make(chan struct{}, total)
	release := make(chan struct{})

	d.SetDownloadFunc(func(_ context.Context, _ *MediaInfo, _ string) error {
		t.Error("去重命中时不应触发真实下载")
		return nil
	})
	d.SetDuplicateLookupFunc(func(_ context.Context, _ string) (string, bool) {
		cur := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
				break
			}
		}
		arrived <- struct{}{}
		<-release // 占住槽位，直到测试观察完并发峰值
		inFlight.Add(-1)
		return src, true
	})

	var runWG sync.WaitGroup
	for i := range total {
		runWG.Add(1)
		go func() {
			defer runWG.Done()
			media := &MediaInfo{
				MessageID: int64(i), TDFileID: int32(i), UniqueID: "dup", //nolint:gosec // 测试用小整数
				MediaType: "document", FileName: "f" + string(rune('a'+i)) + ".bin", FileSize: 7, ChatID: 100,
			}
			_ = d.DownloadMedia(context.Background(), media)
		}()
	}

	// 等到并发上限个查询同时在途；若去重不受限流约束，会有远多于 limit 个同时到达
	for range limit {
		<-arrived
	}
	close(release)
	runWG.Wait()

	if got := maxInFlight.Load(); got > limit {
		t.Errorf("去重查询并发峰值 = %d，超过下载并发上限 %d（去重未受限流约束）", got, limit)
	}
}
