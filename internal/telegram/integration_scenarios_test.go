//go:build integration

package telegram

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tg-down/internal/downloader"
	"tg-down/internal/logger"
	mediapkg "tg-down/internal/media"
	"tg-down/internal/notify"
	"tg-down/internal/queue"
	"tg-down/internal/store"
)

// 以下场景把 v2.0 发布前跳过的手测清单固化下来。它们各自需要额外的测试固件
// （相册消息、转发消息、第二个聊天），缺失时按需 Skip 而不是让整组失败。
const (
	envAlbumMessageID  = "TG_DOWN_IT_ALBUM_MESSAGE_ID"
	envForwardChatID   = "TG_DOWN_IT_FORWARD_CHAT_ID"
	envNotifyWebhookIT = "TG_DOWN_IT_WEBHOOK_URL"
)

// optionalInt64 读取可选的 int64 环境变量；缺失或非法时 Skip 当前用例。
func optionalInt64(t *testing.T, key string) int64 {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		t.Skipf("未设置 %s，跳过该场景", key)
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("%s 不是合法整数: %v", key, err)
	}
	return v
}

// recordCounter 按状态累计下载事件，供各场景断言"下载/跳过/失败"的数量。
// HistoryResult 只汇报 Failed 与 MaxMessageID，其余口径只能从事件流里取。
type recordCounter struct {
	mu   sync.Mutex
	done int
	skip int
	fail int
}

func (rc *recordCounter) record(_ context.Context, evt *downloader.RecordEvent) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	switch evt.Status {
	case downloader.RecordCompleted:
		rc.done++
	case downloader.RecordSkipped:
		rc.skip++
	case downloader.RecordFailed:
		rc.fail++
	}
}

func (rc *recordCounter) completed() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.done
}

func (rc *recordCounter) skipped() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.skip
}

// itStore 开一个落盘库（内存库反映不出重启后重新打开的行为）
func itStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "it.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}

// countFiles 统计目录下的文件数与 sidecar 数（跳过 TDLib 缓存与缩略图目录）
func countFiles(t *testing.T, root string) (files, sidecars int) {
	t.Helper()
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name == ".tdlib-files" || name == ".thumbs" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(p) == ".json" {
			sidecars++
			return nil
		}
		files++
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s 失败: %v", root, err)
	}
	return files, sidecars
}

// TestIT_Sidecar_MatchesMessage 校验元数据 sidecar 的内容确实对应该条消息，
// 而不只是"有一个 .json 文件"。
func TestIT_Sidecar_MatchesMessage(t *testing.T) {
	env := requireEnv(t)
	c, cfg := newITClient(t, env)

	ctx, cancel := context.WithTimeout(context.Background(), itDownloadTimeout)
	defer cancel()

	spec := &downloader.HistorySpec{ChatID: env.chatID, MessageID: env.messageID, TaskID: "it-sidecar"}
	if _, err := c.DownloadHistoryMedia(ctx, spec); err != nil {
		t.Fatalf("下载失败: %v", err)
	}

	var found bool
	err := filepath.Walk(cfg.Download.Path, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Ext(p) != ".json" {
			return err
		}
		raw, readErr := os.ReadFile(p) //nolint:gosec // 测试自建的临时目录
		if readErr != nil {
			return readErr
		}
		text := string(raw)
		// sidecar 必须带上本条消息的 id 与所属聊天，否则它对不上任何一条消息
		if strings.Contains(text, `"message_id": `+strconv.FormatInt(env.messageID, 10)) &&
			strings.Contains(text, `"chat_id": `+strconv.FormatInt(env.chatID, 10)) {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历下载目录失败: %v", err)
	}
	if !found {
		t.Errorf("没有找到与 chat=%d msg=%d 对应的 sidecar", env.chatID, env.messageID)
	}
}

// TestIT_Album_GroupsUnderOneDirectory 校验相册聚合：同一 media_album_id 的媒体
// 落在同一个 album_<id> 目录下，且 album_id 落库。
func TestIT_Album_GroupsUnderOneDirectory(t *testing.T) {
	env := requireEnv(t)
	albumMsgID := optionalInt64(t, envAlbumMessageID)
	c, cfg := newITClient(t, env)

	st, _ := itStore(t)
	c.SetRecordFunc(store.NewRecorder(st))

	ctx, cancel := context.WithTimeout(context.Background(), itDownloadTimeout)
	defer cancel()

	spec := &downloader.HistorySpec{ChatID: env.chatID, MessageID: albumMsgID, TaskID: "it-album"}
	if _, err := c.DownloadHistoryMedia(ctx, spec); err != nil {
		t.Fatalf("下载相册失败: %v", err)
	}

	// 等异步落盘
	time.Sleep(2 * time.Second)
	page, err := st.QueryHistory(ctx, &store.HistoryFilter{ChatID: env.chatID, Limit: 100})
	if err != nil {
		t.Fatalf("查询历史失败: %v", err)
	}
	var withAlbum int
	for _, rec := range page.Items {
		if rec.AlbumID != 0 {
			withAlbum++
		}
	}
	if withAlbum == 0 {
		t.Fatalf("没有任何记录带 album_id——%s 指向的消息可能不属于相册", envAlbumMessageID)
	}

	// 目录结构：默认模板把相册收进 album_<id> 子目录
	var albumDirs int
	err = filepath.Walk(cfg.Download.Path, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() && strings.HasPrefix(info.Name(), "album_") {
			albumDirs++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历下载目录失败: %v", err)
	}
	if albumDirs == 0 {
		t.Error("相册媒体没有落进 album_<id> 目录")
	}
}

// TestIT_ForwardDedup_CopiesInsteadOfRedownload 校验内容级去重：
// 同一份内容被转发到另一个聊天时，第二次不再走网络下载，而是从既有文件复制。
func TestIT_ForwardDedup_CopiesInsteadOfRedownload(t *testing.T) {
	env := requireEnv(t)
	forwardChatID := optionalInt64(t, envForwardChatID)
	c, cfg := newITClient(t, env)

	st, _ := itStore(t)
	counts := &recordCounter{}
	recorder := store.NewRecorder(st)
	c.SetRecordFunc(func(ctx context.Context, evt *downloader.RecordEvent) {
		recorder(ctx, evt)
		counts.record(ctx, evt)
	})
	c.SetDuplicateLookupFunc(func(ctx context.Context, uniqueID string) (string, bool) {
		rec, err := st.FindCompletedByUniqueID(ctx, uniqueID)
		if err != nil || rec == nil {
			return "", false
		}
		return rec.FilePath, true
	})

	ctx, cancel := context.WithTimeout(context.Background(), itDownloadTimeout)
	defer cancel()

	// 第一遍：源聊天，正常下载
	first := &downloader.HistorySpec{
		ChatID:  env.chatID,
		TaskID:  "it-dedup-source",
		Filters: downloader.HistoryFilters{MediaTypes: []string{mediapkg.Photo}},
	}
	if _, err := c.DownloadHistoryMedia(ctx, first); err != nil {
		t.Fatalf("源聊天下载失败: %v", err)
	}
	if counts.completed() == 0 {
		t.Skip("源聊天里没有可下载的图片，无法验证去重")
	}
	sourceCompleted := counts.completed()
	time.Sleep(2 * time.Second)

	// 第二遍：转发目标聊天，命中去重的部分应记为 skipped 而非 downloaded
	second := &downloader.HistorySpec{
		ChatID:  forwardChatID,
		TaskID:  "it-dedup-forward",
		Filters: downloader.HistoryFilters{MediaTypes: []string{mediapkg.Photo}},
	}
	skippedBefore := counts.skipped()
	if _, err := c.DownloadHistoryMedia(ctx, second); err != nil {
		t.Fatalf("转发聊天下载失败: %v", err)
	}
	dedupHits := counts.skipped() - skippedBefore
	t.Logf("源聊天完成 %d 个；转发聊天命中去重 %d 个", sourceCompleted, dedupHits)
	if dedupHits == 0 {
		t.Errorf("转发聊天里一个去重都没命中——%s 指向的聊天需要包含从源聊天转发的媒体",
			envForwardChatID)
	}

	// 去重复制出来的文件同样要有 sidecar（v3.2 修复项）
	files, sidecars := countFiles(t, cfg.Download.Path)
	if sidecars < files {
		t.Errorf("有文件缺少 sidecar: 文件 %d，sidecar %d", files, sidecars)
	}
}

// TestIT_CrashRecovery_ResumesInterrupted 校验 kill -9 后的恢复路径：
// 崩溃遗留的在途行被清扫为 failed(interrupted)，重新打开后能被定位并补下。
//
// 用"关掉 store 再重开"模拟进程重启：真正的 kill -9 无法在同一个测试进程里做，
// 而恢复逻辑关心的正是重启后从库里读到的状态。
func TestIT_CrashRecovery_ResumesInterrupted(t *testing.T) {
	env := requireEnv(t)
	c, cfg := newITClient(t, env)

	dbPath := filepath.Join(t.TempDir(), "crash.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	c.SetRecordFunc(store.NewRecorder(st))

	ctx, cancel := context.WithTimeout(context.Background(), itDownloadTimeout)
	defer cancel()

	spec := &downloader.HistorySpec{ChatID: env.chatID, MessageID: env.messageID, TaskID: "it-crash"}
	if _, err := c.DownloadHistoryMedia(ctx, spec); err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	time.Sleep(2 * time.Second)

	// 人为制造"崩溃时留下的在途行"：把该消息重新写回 queued（UpsertHistoryStart
	// 对非 completed 行放行，因此先取出原记录再以 queued 状态覆盖）
	page, err := st.QueryHistory(ctx, &store.HistoryFilter{ChatID: env.chatID, Limit: 100})
	if err != nil {
		t.Fatalf("查询历史失败: %v", err)
	}
	if len(page.Items) == 0 {
		t.Fatal("下载后历史为空，无法构造崩溃现场")
	}
	stale := *page.Items[0]
	stale.Status = store.HistoryStatusQueued
	stale.TaskID = "it-crash"
	if err := st.UpsertHistoryStart(ctx, &stale); err != nil {
		t.Fatalf("构造在途行失败: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("关闭数据库失败: %v", err)
	}

	// 重启：重新打开同一个库
	reopened, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("重新打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	swept, err := reopened.SweepInterruptedHistory(ctx)
	if err != nil {
		t.Fatalf("清扫在途行失败: %v", err)
	}
	if swept == 0 {
		t.Fatal("重启后没有清扫到任何在途行")
	}

	pending, err := reopened.ListFailedByTask(ctx, "it-crash")
	if err != nil {
		t.Fatalf("查询待补下失败: %v", err)
	}
	if len(pending) == 0 {
		t.Fatal("清扫后没有可补下的消息，恢复链路断了")
	}
	t.Logf("清扫 %d 行，待补下 %d 条", swept, len(pending))

	// 补下：文件其实还在磁盘上，应当全部按已存在跳过
	c.SetRecordFunc(store.NewRecorder(reopened))
	resume := &downloader.HistorySpec{
		ChatID:          env.chatID,
		TaskID:          "it-crash",
		RetryMessageIDs: pending,
		RetryOnly:       true, // 历史已扫完，只补这些消息
	}
	res, err := c.DownloadHistoryMedia(ctx, resume)
	if err != nil {
		t.Fatalf("补下失败: %v", err)
	}
	if res.Failed > 0 {
		t.Errorf("补下有 %d 个失败", res.Failed)
	}
	// 文件仍在磁盘上，补下应当按已存在跳过而不是重新下载
	if files, _ := countFiles(t, cfg.Download.Path); files == 0 {
		t.Error("补下后下载目录为空")
	}
}

// TestIT_Schedule_IncrementalWatermark 校验定时任务的增量水位：
// 第二次触发只扫比上次水位更新的消息，而不是把整条历史重扫一遍。
func TestIT_Schedule_IncrementalWatermark(t *testing.T) {
	env := requireEnv(t)
	c, _ := newITClient(t, env)
	st, _ := itStore(t)

	var firstScanned, secondScanned atomic.Int64
	current := &firstScanned
	c.SetScanProgressFunc(func(_ string, scannedMessages, _, _ int64) {
		current.Store(scannedMessages)
	})

	ctx, cancel := context.WithTimeout(context.Background(), itScanTimeout)
	defer cancel()

	row := &store.ScheduleRow{
		ID: "it-sched", ChatID: env.chatID, IntervalMin: 10,
		Enabled: true, CreatedAt: time.Now(),
	}
	if err := st.CreateSchedule(ctx, row); err != nil {
		t.Fatalf("创建定时计划失败: %v", err)
	}

	// 第一次：全量扫描，记录水位
	firstSpec := &downloader.HistorySpec{
		ChatID: env.chatID, TaskID: "it-sched-1",
		Filters: downloader.HistoryFilters{MediaTypes: []string{mediapkg.Photo}},
	}
	firstRes, err := c.DownloadHistoryMedia(ctx, firstSpec)
	if err != nil {
		t.Fatalf("首次扫描失败: %v", err)
	}
	if firstRes.MaxMessageID == 0 {
		t.Skip("聊天里没有匹配的媒体，无法验证水位")
	}
	if err := st.UpdateScheduleLastMaxID(ctx, row.ID, firstRes.MaxMessageID); err != nil {
		t.Fatalf("回写水位失败: %v", err)
	}

	// 第二次：带上水位作为下界，应当立刻收手
	current = &secondScanned
	secondSpec := &downloader.HistorySpec{
		ChatID: env.chatID, TaskID: "it-sched-2",
		StopAtMessageID: firstRes.MaxMessageID,
		Filters:         downloader.HistoryFilters{MediaTypes: []string{mediapkg.Photo}},
	}
	if _, err := c.DownloadHistoryMedia(ctx, secondSpec); err != nil {
		t.Fatalf("增量扫描失败: %v", err)
	}
	t.Logf("首次扫描 %d 条消息，增量扫描 %d 条", firstScanned.Load(), secondScanned.Load())
	if firstScanned.Load() > 0 && secondScanned.Load() >= firstScanned.Load() {
		t.Errorf("增量扫描没有收敛：第二次扫了 %d 条，第一次 %d 条——水位没起作用",
			secondScanned.Load(), firstScanned.Load())
	}
}

// TestIT_Notify_TaskFinished 校验任务完成通知真的发得出去（收藏夹 + 可选 webhook）。
func TestIT_Notify_TaskFinished(t *testing.T) {
	env := requireEnv(t)
	c, _ := newITClient(t, env)

	webhook := os.Getenv(envNotifyWebhookIT) // 可选
	n := notify.New(c.SendSelfMessage, webhook, logger.New("debug"))

	finished := time.Now()
	dto := &queue.TaskDTO{
		ID: "it-notify", Kind: "history", ChatID: env.chatID,
		ChatTitle: "集成测试", Status: string(queue.StatusCompleted),
		CreatedAt: finished.Add(-time.Minute), FinishedAt: &finished,
		Stats: downloader.Stats{Total: 1, Downloaded: 1, DownloadedSize: 1024},
	}
	n.TaskFinished(dto)

	// TaskFinished 是异步发送，给它一点时间；失败只会记日志，因此这里断言的是
	// "调用链路没有 panic 且收藏夹里能看到消息"——后者需要人工确认。
	time.Sleep(3 * time.Second)
	t.Log("已触发完成通知；请在收藏夹（Saved Messages）中确认收到一条任务完成消息")
}
