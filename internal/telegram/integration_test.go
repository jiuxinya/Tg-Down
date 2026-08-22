//go:build integration

// 真账号回归测试。默认不参与编译（需 -tags integration），跑法见 `make test-integration`。
//
// 安全约定（曾经真的踩过：一个拼错的命令行参数让程序读了仓库根目录的 config.yaml，
// 用真实凭据连上了真实账号）：
//
//   - 凭据只从 TG_DOWN_IT_* 环境变量取，绝不读仓库里的 config.yaml；
//   - 缺任一变量就整体 Skip，绝不"尽力而为"地退化到默认配置；
//   - 会话目录必须是已登录态。本测试不做交互式登录（也没有输入验证码的地方），
//     未授权即 Fatal，不会把测试挂在 TDLib 的验证码等待上；
//   - 下载目录一律用 t.TempDir()，测试产物不落进用户的 downloads/。
//
// 准备一个专用测试账号与一个只含少量媒体的测试频道，不要拿主账号跑。
package telegram

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"tg-down/internal/config"
	"tg-down/internal/downloader"
	"tg-down/internal/logger"
	mediapkg "tg-down/internal/media"
)

const (
	envAPIID      = "TG_DOWN_IT_API_ID"
	envAPIHash    = "TG_DOWN_IT_API_HASH"
	envPhone      = "TG_DOWN_IT_PHONE"
	envSessionDir = "TG_DOWN_IT_SESSION_DIR"
	envChatID     = "TG_DOWN_IT_CHAT_ID"
	envMessageID  = "TG_DOWN_IT_MESSAGE_ID"

	itConnectTimeout  = 60 * time.Second
	itScanTimeout     = 5 * time.Minute
	itDownloadTimeout = 10 * time.Minute
)

// itEnv 是一次集成测试运行所需的全部外部输入
type itEnv struct {
	apiID      int
	apiHash    string
	phone      string
	sessionDir string
	chatID     int64
	messageID  int64
}

// requireEnv 读取环境变量；缺任何一个都直接 Skip 整个测试。
func requireEnv(t *testing.T) itEnv {
	t.Helper()

	get := func(k string) string {
		v := os.Getenv(k)
		if v == "" {
			t.Skipf("集成测试需要环境变量 %s，已跳过（见 internal/telegram/integration_test.go 顶部说明）", k)
		}
		return v
	}

	rawID := get(envAPIID)
	apiID, err := strconv.Atoi(rawID)
	if err != nil {
		t.Fatalf("%s 不是合法整数: %v", envAPIID, err)
	}
	rawChat := get(envChatID)
	chatID, err := strconv.ParseInt(rawChat, 10, 64)
	if err != nil {
		t.Fatalf("%s 不是合法整数: %v", envChatID, err)
	}
	rawMessage := get(envMessageID)
	messageID, err := strconv.ParseInt(rawMessage, 10, 64)
	if err != nil || messageID <= 0 {
		t.Fatalf("%s 不是合法的正整数: %v", envMessageID, err)
	}

	return itEnv{
		apiID:      apiID,
		apiHash:    get(envAPIHash),
		phone:      get(envPhone),
		sessionDir: get(envSessionDir),
		chatID:     chatID,
		messageID:  messageID,
	}
}

// newITClient 用临时下载目录 + 外部提供的已登录会话目录建客户端。
// 返回的 client 已连接且已授权，t.Cleanup 负责关闭。
func newITClient(t *testing.T, env itEnv) (*Client, *config.Config) {
	t.Helper()

	cfg := &config.Config{}
	cfg.API.ID = env.apiID
	cfg.API.Hash = env.apiHash
	cfg.API.Phone = env.phone
	cfg.Session.Dir = env.sessionDir
	cfg.Download.Path = t.TempDir() // 绝不写进用户的 downloads/
	cfg.Download.MaxConcurrent = config.DefaultMaxConcurrent
	cfg.Download.BatchSize = config.DefaultBatchSize
	cfg.Download.PartitionSize = config.DefaultPartitionSize
	cfg.Download.SaveMetadata = true
	cfg.Retry.MaxRetries = config.DefaultMaxRetries
	cfg.Retry.BaseDelay = config.DefaultBaseDelay
	cfg.Retry.MaxDelay = config.DefaultMaxDelay
	cfg.Log.Level = "debug"

	log := logger.New(cfg.Log.Level)
	c := New(cfg, log)
	t.Cleanup(c.Close)

	ctx, cancel := context.WithTimeout(context.Background(), itConnectTimeout)
	defer cancel()

	// 不提供验证码/密码回调：会话必须已是登录态。否则宁可失败，也不要把测试
	// 挂在一个永远等不到输入的验证码提示上。
	deny := func(context.Context) (string, error) {
		t.Fatalf("会话未登录：%s 指向的目录里没有可用会话。请先用该账号正常登录一次。", envSessionDir)
		return "", nil
	}
	if err := c.Connect(ctx, deny, deny); err != nil {
		t.Fatalf("连接失败: %v", err)
	}

	ok, err := c.IsAuthorized(ctx)
	if err != nil {
		t.Fatalf("查询授权状态失败: %v", err)
	}
	if !ok {
		t.Fatalf("会话未授权，请先登录 %s", env.sessionDir)
	}
	return c, cfg
}

// TestIT_Connect_And_ListChats 验证会话复用：不做交互登录也能连上并拉到聊天列表。
func TestIT_Connect_And_ListChats(t *testing.T) {
	env := requireEnv(t)
	c, _ := newITClient(t, env)

	ctx, cancel := context.WithTimeout(context.Background(), itConnectTimeout)
	defer cancel()

	chats, err := c.GetChats(ctx)
	if err != nil {
		t.Fatalf("GetChats 失败: %v", err)
	}
	if len(chats) == 0 {
		t.Fatal("聊天列表为空——测试账号至少要有一个会话")
	}

	var found bool
	for _, ch := range chats {
		if ch.ID == env.chatID {
			found = true
			t.Logf("测试频道: %d %q (%s)", ch.ID, ch.Title, ch.Type)
		}
	}
	if !found {
		t.Errorf("聊天列表里没有 %s=%d；该账号可能不在这个频道里", envChatID, env.chatID)
	}
}

// TestIT_CountHistoryMedia 验证服务端计数（M2 的 SearchChatMessages 路径）。
func TestIT_CountHistoryMedia(t *testing.T) {
	env := requireEnv(t)
	c, _ := newITClient(t, env)

	ctx, cancel := context.WithTimeout(context.Background(), itScanTimeout)
	defer cancel()

	filterable := []string{
		mediapkg.Photo, mediapkg.Video, mediapkg.Document, mediapkg.Animation,
		mediapkg.Audio, mediapkg.Voice, mediapkg.VideoNote,
	}
	total, err := c.CountHistoryMedia(ctx, env.chatID, filterable)
	if err != nil {
		t.Fatalf("CountHistoryMedia 失败: %v", err)
	}
	if total <= 0 {
		t.Fatalf("测试频道里数不出媒体（total=%d）；请往频道里放几张图/几个文件", total)
	}
	t.Logf("默认类型媒体总数: %d", total)

	// 贴纸没有服务端 filter，选中它时总数应当是"未知"(0) 而不是一个错的分母——
	// 错的分母会让前端进度条冲破 100%。
	n, err := c.CountHistoryMedia(ctx, env.chatID, nil)
	if err != nil {
		t.Fatalf("CountHistoryMedia(含贴纸) 失败: %v", err)
	}
	if n != 0 {
		t.Errorf("含贴纸时总数应为 0（未知），实得 %d", n)
	}
}

// TestIT_DownloadSingleMessage 端到端下载一个受控消息：取消息 → 下载 → 落盘 → sidecar。
// 消息 ID 必须由测试环境显式提供，测试绝不扫描并下载整个聊天的全部照片。
func TestIT_DownloadSingleMessage(t *testing.T) {
	env := requireEnv(t)
	c, cfg := newITClient(t, env)

	var (
		queued int
		done   int
		failed int
	)
	c.SetRecordFunc(func(_ context.Context, evt *downloader.RecordEvent) {
		switch evt.Status {
		case downloader.RecordQueued:
			queued++
		case downloader.RecordCompleted, downloader.RecordSkipped:
			done++
		case downloader.RecordFailed:
			failed++
			t.Logf("失败: msg=%d reason=%s", evt.Media.MessageID, evt.Reason)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), itDownloadTimeout)
	defer cancel()

	spec := &downloader.HistorySpec{
		ChatID:    env.chatID,
		MessageID: env.messageID,
		TaskID:    "it-single-message",
	}
	res, err := c.DownloadHistoryMedia(ctx, spec)
	if err != nil {
		t.Fatalf("DownloadHistoryMedia 失败: %v", err)
	}
	if res.Failed > 0 {
		t.Errorf("有 %d 个文件下载失败", res.Failed)
	}

	// 事件契约（M1.7 定的）：每个媒体恰好一次 queued + 恰好一次终态
	if queued != done+failed {
		t.Errorf("事件不守恒: queued=%d, 终态=%d(done)+%d(failed)", queued, done, failed)
	}
	if done == 0 {
		t.Fatal("一个文件都没下到——测试频道里需要有照片")
	}

	// 落盘检查：文件真实存在且非空，metadata sidecar 也在
	var files, sidecars int
	walkErr := filepath.Walk(cfg.Download.Path, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		switch {
		case filepath.Ext(p) == ".json":
			sidecars++
		default:
			files++
			if info.Size() == 0 {
				t.Errorf("落盘了 0 字节文件: %s", p)
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("遍历下载目录失败: %v", walkErr)
	}
	t.Logf("下载 %d 个文件，%d 个 sidecar（含 .tdlib-files 缓存与 .thumbs）", files, sidecars)
	if files == 0 {
		t.Error("下载目录里没有文件")
	}
}

// TestIT_DownloadHistory_Rerun_SkipsExisting 第二次跑同一任务应当全部跳过（M1.8 的按大小比对）。
func TestIT_DownloadHistory_Rerun_SkipsExisting(t *testing.T) {
	env := requireEnv(t)
	c, _ := newITClient(t, env)

	run := func(taskID string) (skipped, downloaded int) {
		c.SetRecordFunc(func(_ context.Context, evt *downloader.RecordEvent) {
			switch evt.Status {
			case downloader.RecordSkipped:
				skipped++
			case downloader.RecordCompleted:
				downloaded++
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), itDownloadTimeout)
		defer cancel()

		spec := &downloader.HistorySpec{
			ChatID:    env.chatID,
			MessageID: env.messageID,
			TaskID:    taskID,
		}
		if _, err := c.DownloadHistoryMedia(ctx, spec); err != nil {
			t.Fatalf("DownloadHistoryMedia(%s) 失败: %v", taskID, err)
		}
		return skipped, downloaded
	}

	// 两次都用同一个 client（同一个临时下载目录），第二次应当因文件已存在且大小相符而全跳过
	_, firstDownloaded := run("it-rerun-1")
	if firstDownloaded == 0 {
		t.Fatal("首轮一个文件都没下到")
	}
	secondSkipped, secondDownloaded := run("it-rerun-2")
	if secondDownloaded != 0 {
		t.Errorf("重跑不该重下任何文件，实际重下了 %d 个", secondDownloaded)
	}
	if secondSkipped != firstDownloaded {
		t.Errorf("重跑应跳过 %d 个，实际跳过 %d 个", firstDownloaded, secondSkipped)
	}
}

// TestIT_ExportChat 导出必须包含纯文本消息；无过滤搜索应枚举完整历史，而不只返回媒体。
func TestIT_ExportChat(t *testing.T) {
	env := requireEnv(t)
	c, _ := newITClient(t, env)

	ctx, cancel := context.WithTimeout(context.Background(), itScanTimeout)
	defer cancel()

	res, err := c.ExportChat(ctx, ExportSpec{
		ChatID: env.chatID,
		Limit:  200,
	})
	if err != nil {
		t.Fatalf("ExportChat 失败: %v", err)
	}
	if res.Messages == 0 {
		t.Fatal("导出了 0 条消息")
	}
	for _, p := range []string{res.JSONPath, res.HTMLPath} {
		st, statErr := os.Stat(p)
		if statErr != nil {
			t.Errorf("导出产物不存在: %s (%v)", p, statErr)
			continue
		}
		if st.Size() == 0 {
			t.Errorf("导出产物是空文件: %s", p)
		}
	}
	t.Logf("导出 %d 条消息，%d 个媒体引用（其中 %d 个已在本地）",
		res.Messages, res.MediaCount, res.MediaOnDisk)
}
