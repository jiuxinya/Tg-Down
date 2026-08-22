// Package main implements the entry point for the Telegram media downloader application.
// It provides functionality to download media files from Telegram chats and monitor new messages.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"tg-down/internal/config"
	"tg-down/internal/downloader"
	"tg-down/internal/logger"
	"tg-down/internal/store"
	"tg-down/internal/telegram"
	"tg-down/internal/web"
)

const (
	// ModeDownloadHistory is the mode for downloading historical media.
	ModeDownloadHistory = 1
	// ModeMonitorNewMessages is the mode for monitoring new messages.
	ModeMonitorNewMessages = 2
	// ModeDownloadAndMonitor is the mode for both downloading history and monitoring new messages.
	ModeDownloadAndMonitor = 3

	// ExitCodeConfigError is the exit code for configuration errors.
	ExitCodeConfigError = 1
	// ExitCodeRunError is the exit code for runtime errors.
	ExitCodeRunError = 1
	// ExitCodeSessionError is the exit code for session errors.
	ExitCodeSessionError = 1
	// ExitCodeUsage is the exit code for unrecognized command-line arguments.
	ExitCodeUsage = 2

	// SignalBufferSize is the buffer size for signal channel.
	SignalBufferSize = 2

	// MinChatChoice is the minimum valid chat choice.
	MinChatChoice = 1
	// MaxModeChoice is the maximum valid mode choice.
	MaxModeChoice = 3
)

// version 由构建时 -ldflags "-X main.version=..." 注入
var version = "dev"

func main() {
	telegram.SetAppVersion(version)
	web.SetVersion(version)

	// 版本信息: tg-down --version
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("tg-down %s\n", version)
		return
	}

	// 清除会话: tg-down --clear-session
	if len(os.Args) > 1 && os.Args[1] == "--clear-session" {
		clearSessionAndExit()
		return
	}

	// Web 管理端模式: tg-down --web [监听地址]
	if len(os.Args) > 1 && os.Args[1] == "--web" {
		addr := web.DefaultAddr
		if len(os.Args) > 2 {
			addr = os.Args[2]
		}
		runWebMode(addr)
		return
	}

	if len(os.Args) > 1 && (os.Args[1] == "--help" || os.Args[1] == "-h") {
		printUsage(os.Stdout)
		return
	}

	// 无法识别的参数一律拒绝，不再"当作没传参数"直接进入交互模式。
	// 那个行为很危险：一个拼错的 flag（`--config`、`--help`）会静默地用真实凭据登录、
	// 连上真实账号并开始下载，而用户以为自己只是打了个错字。
	if len(os.Args) > 1 {
		_, _ = fmt.Fprintf(os.Stderr, "无法识别的参数: %s\n\n", os.Args[1])
		printUsage(os.Stderr)
		os.Exit(ExitCodeUsage)
	}

	cfg, log := initializeApplication()
	if err := runApp(cfg, log); err != nil {
		log.Error("%v", err)
		os.Exit(ExitCodeRunError)
	}
	log.Info("程序退出")
}

// printUsage 输出命令行用法
func printUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `tg-down %s — Telegram 媒体下载器

用法:
  tg-down                    交互式 CLI（读取当前目录的 config.yaml）
  tg-down --web [监听地址]   Web 管理台（默认 %s）
  tg-down --clear-session    清除本地 TDLib 会话
  tg-down --version, -v      输出版本
  tg-down --help, -h         输出本帮助
`, version, web.DefaultAddr)
}

// runApp 承载 CLI 主流程：以 defer 打开/清理资源，出错时返回错误交由 main 决定退出码，
// 从而让 store.Close/client.Close/cancel 等 defer 在进程退出前正常执行（不再被内联 os.Exit 跳过）。
func runApp(cfg *config.Config, log *logger.Logger) error {
	st, err := store.Open(cfg.Store.Path)
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}
	defer func() { _ = st.Close() }()

	ctx, cancel := setupSignalHandling(log)
	defer cancel()

	mode, err := selectMode(os.Stdin, os.Stdout)
	if err != nil {
		return fmt.Errorf("选择操作模式失败: %w", err)
	}

	// TDLib 客户端始终带更新监听；是否触发实时下载由 targetChatID 控制
	client := telegram.NewWithUpdates(cfg, log, 0)
	client.SetRecordFunc(store.NewRecorder(st, func(err error) {
		log.Warn("持久化下载历史失败: %v", err)
	}))
	client.SetDuplicateLookupFunc(func(ctx context.Context, uniqueID string) (string, bool) {
		rec, err := st.FindCompletedByUniqueID(ctx, uniqueID)
		if err != nil {
			log.Warn("查询内容去重记录失败: %v", err)
			return "", false
		}
		if rec == nil {
			return "", false
		}
		return rec.FilePath, true
	})
	defer client.Close() // Close 在未连接(td==nil)时为无操作，认证失败也可安全调用

	log.Info("正在连接到Telegram...")
	if err := client.Authenticate(ctx); err != nil {
		return fmt.Errorf("连接/认证失败: %w", err)
	}
	log.Info("成功连接到Telegram")

	targetChatID, err := resolveTargetChat(ctx, cfg, client, log)
	if err != nil {
		return err
	}
	chatTitle := client.ChatTitle(ctx, targetChatID)
	if mode == ModeMonitorNewMessages || mode == ModeDownloadAndMonitor {
		client.SetMonitorTask(fmt.Sprintf("cli-monitor-%d", time.Now().UnixNano()), targetChatID, chatTitle)
	}

	if err := executeMode(ctx, cancel, client, log, mode, targetChatID, chatTitle); err != nil {
		return fmt.Errorf("运行失败: %w", err)
	}
	return nil
}

// runWebMode 启动 Web 管理端（允许无凭据启动，登录信息可在网页内填写）
func runWebMode(addr string) {
	cfg, err := config.LoadConfigForWeb()
	if err != nil {
		fmt.Printf("加载配置失败: %v\n", err)
		os.Exit(ExitCodeConfigError)
	}
	log := logger.New(cfg.Log.Level)
	log.Info("Telegram群聊媒体下载器启动 (Web 模式)")
	if err := runWeb(cfg, log, addr); err != nil {
		log.Error("%v", err)
		os.Exit(ExitCodeRunError)
	}
	log.Info("程序退出")
}

// runWeb 承载 Web 模式主流程，以 defer 保证 store 关闭，出错时返回错误交由 runWebMode 决定退出码。
func runWeb(cfg *config.Config, log *logger.Logger, addr string) error {
	client := telegram.NewWithUpdates(cfg, log, 0)
	if client == nil {
		return fmt.Errorf("创建客户端失败")
	}

	st, err := store.Open(cfg.Store.Path)
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}
	defer func() { _ = st.Close() }()

	ctx, cancel := setupSignalHandling(log)
	defer cancel()

	if err := web.New(client, st, log, addr, cfg).Run(ctx); err != nil {
		return fmt.Errorf("web 服务运行失败: %w", err)
	}
	return nil
}

// initializeApplication 初始化应用程序配置和日志
func initializeApplication() (*config.Config, *logger.Logger) {
	cfg, err := config.LoadConfig()
	if err != nil {
		fmt.Printf("加载配置失败: %v\n", err)
		fmt.Println("请确保已正确配置 config.yaml 或环境变量")
		fmt.Println("可以参考 config.yaml.example 和 .env.example 文件")
		os.Exit(ExitCodeConfigError)
	}

	log := logger.New(cfg.Log.Level)
	log.Info("Telegram群聊媒体下载器启动")
	return cfg, log
}

// setupSignalHandling 设置信号处理。
//
// 这里不用 signal.NotifyContext：它在首个信号取消 ctx 后，内部 goroutine 就退出了，
// 但信号仍注册在它那条容量 1 的通道上——第二次 Ctrl+C 会被吞掉，进程反而杀不掉。
// 而下面这个「二次信号强制退出」正是卡在 fmt.Scanln（无视 ctx）时唯一的逃生口。
//
// Windows 上这两个信号都真实投递：runtime 的 ctrlHandler 把 CTRL_C/CTRL_BREAK 映射为 SIGINT，
// 把窗口关闭/注销/关机映射为 SIGTERM。os.Interrupt 即 syscall.SIGINT（含 windows 构建标签），
// 用它只是为了名字可移植。
func setupSignalHandling(log *logger.Logger) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())

	sigChan := make(chan os.Signal, SignalBufferSize)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Info("收到中断信号，正在退出...")
		cancel()
		// 首个信号取消 ctx 触发优雅退出；若此时正阻塞在 stdin 提示（Scanln 无视 ctx），
		// 再次收到信号则强制退出，避免进程无法用 Ctrl+C/SIGTERM 结束。
		<-sigChan
		log.Warn("再次收到中断信号，强制退出")
		os.Exit(ExitCodeRunError)
	}()

	return ctx, cancel
}

// resolveTargetChat 决定目标聊天ID：优先用配置，否则交互式选择
func resolveTargetChat(ctx context.Context, cfg *config.Config, client *telegram.Client, log *logger.Logger) (int64, error) {
	if cfg.Chat.TargetID != 0 {
		log.Info("使用配置的聊天ID: %d", cfg.Chat.TargetID)
		return cfg.Chat.TargetID, nil
	}

	chatID, err := selectChat(ctx, client, log)
	if err != nil {
		return 0, fmt.Errorf("选择聊天失败: %w", err)
	}
	return chatID, nil
}

// executeMode 执行指定的操作模式
func executeMode(
	ctx context.Context,
	cancel context.CancelFunc,
	client *telegram.Client,
	log *logger.Logger,
	mode int,
	targetChatID int64,
	chatTitle string,
) error {
	switch mode {
	case ModeDownloadHistory:
		return executeDownloadHistory(ctx, client, log, targetChatID, chatTitle)
	case ModeMonitorNewMessages:
		return executeMonitorNewMessages(ctx, cancel, log, targetChatID)
	case ModeDownloadAndMonitor:
		return executeDownloadAndMonitor(ctx, client, log, targetChatID, chatTitle)
	default:
		return fmt.Errorf("未知的操作模式: %d", mode)
	}
}

// logHistoryMediaCount 在下载前统计并打印聊天媒体总数（近似值）；
// 统计失败仅告警不阻断，返回非 nil 仅表示 ctx 已取消
func logHistoryMediaCount(ctx context.Context, client *telegram.Client, log *logger.Logger, chatID int64) error {
	total, err := client.CountHistoryMedia(ctx, chatID, nil)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		log.Warn("统计媒体总数失败（继续下载）: %v", err)
		return nil
	}
	log.Info("该聊天共约 %d 个媒体文件", total)
	return nil
}

// executeDownloadHistory 执行下载历史媒体模式
func executeDownloadHistory(
	ctx context.Context, client *telegram.Client, log *logger.Logger, targetChatID int64, chatTitle string,
) error {
	log.Info("开始下载历史媒体文件...")
	if err := logHistoryMediaCount(ctx, client, log, targetChatID); err != nil {
		return nil
	}
	taskID := fmt.Sprintf("cli-history-%d", time.Now().UnixNano())
	spec := &downloader.HistorySpec{ChatID: targetChatID, ChatTitle: chatTitle, TaskID: taskID}
	result, err := client.DownloadHistoryMedia(ctx, spec)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("下载历史媒体失败: %w", err)
	}
	reportFailedMedia(log, result)
	return nil
}

// reportFailedMedia 在有文件下载失败时明确告知用户，而不是静默地宣告成功
func reportFailedMedia(log *logger.Logger, result *downloader.HistoryResult) {
	if result != nil && result.Failed > 0 {
		log.Warn("有 %d 个文件下载失败，可重新运行以补下（已下载的文件会被跳过）", result.Failed)
	}
}

// executeMonitorNewMessages 执行监控新消息模式
func executeMonitorNewMessages(
	ctx context.Context,
	cancel context.CancelFunc,
	log *logger.Logger,
	targetChatID int64,
) error {
	log.Info("开始实时监控新消息...")
	log.Info("实时监控已启动，目标聊天ID: %d", targetChatID)

	startInteractiveMonitoring(ctx, cancel, log, targetChatID)
	<-ctx.Done()
	return nil
}

// executeDownloadAndMonitor 执行下载历史并监控新消息模式
func executeDownloadAndMonitor(
	ctx context.Context, client *telegram.Client, log *logger.Logger, targetChatID int64, chatTitle string,
) error {
	log.Info("开始下载历史媒体文件...")
	if err := logHistoryMediaCount(ctx, client, log, targetChatID); err != nil {
		return nil
	}
	taskID := fmt.Sprintf("cli-history-%d", time.Now().UnixNano())
	spec := &downloader.HistorySpec{ChatID: targetChatID, ChatTitle: chatTitle, TaskID: taskID}
	result, err := client.DownloadHistoryMedia(ctx, spec)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("下载历史媒体失败: %w", err)
	}
	reportFailedMedia(log, result)
	log.Info("历史媒体下载完成，实时监控已自动启动")
	log.Info("实时监控已启动，目标聊天ID: %d", targetChatID)
	<-ctx.Done()
	return nil
}

// startInteractiveMonitoring 启动交互式监控
func startInteractiveMonitoring(
	ctx context.Context,
	cancel context.CancelFunc,
	log *logger.Logger,
	targetChatID int64,
) {
	go func() {
		fmt.Println("\n监控已启动！")
		fmt.Println("输入命令:")
		fmt.Println("  'status' - 查看监控状态")
		fmt.Println("  'quit' - 退出程序")
		fmt.Print("> ")

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			var input string
			if _, scanErr := fmt.Scanln(&input); scanErr != nil {
				// stdin 关闭（EOF，如 `< /dev/null`、管道耗尽、Ctrl-D）时停止读取，
				// 否则 Scanln 会立即返回 EOF 形成 100% CPU 忙循环；监控本身由 ctx 继续驱动。
				if errors.Is(scanErr, io.EOF) {
					log.Warn("标准输入已关闭，停止交互式命令读取（监控继续运行）")
					return
				}
				continue
			}

			if !handleInteractiveCommand(cancel, log, targetChatID, input) {
				return
			}
			fmt.Print("> ")
		}
	}()
}

// handleInteractiveCommand 处理交互式命令
func handleInteractiveCommand(
	cancel context.CancelFunc,
	log *logger.Logger,
	targetChatID int64,
	input string,
) bool {
	switch input {
	case "status":
		log.Info("监控状态: 正在运行，目标聊天ID: %d", targetChatID)
	case "quit":
		log.Info("用户请求退出")
		cancel()
		return false
	default:
		fmt.Println("未知命令，请输入 'status' 或 'quit'")
	}
	return true
}

// selectChat 选择目标聊天
func selectChat(ctx context.Context, client *telegram.Client, log *logger.Logger) (int64, error) {
	log.Info("获取聊天列表...")
	chats, err := client.GetChats(ctx)
	if err != nil {
		return 0, fmt.Errorf("获取聊天列表失败: %w", err)
	}

	if len(chats) == 0 {
		return 0, fmt.Errorf("没有找到任何聊天")
	}

	displayChatList(chats)
	choice, err := getUserChatChoice(len(chats))
	if err != nil {
		return 0, err
	}

	selectedChat := chats[choice-1]
	log.Info("选择了聊天: %s (ID: %d)", selectedChat.Title, selectedChat.ID)
	return selectedChat.ID, nil
}

// displayChatList 显示聊天列表
func displayChatList(chats []telegram.ChatInfo) {
	fmt.Println("\n可用的聊天:")
	for i, chat := range chats {
		fmt.Printf("%d. %s (%s) - ID: %d\n", i+1, chat.Title, chat.Type, chat.ID)
	}
}

// getUserChatChoice 获取用户的聊天选择
func getUserChatChoice(maxChoice int) (int, error) {
	fmt.Print("\n请选择聊天 (输入序号): ")
	var choice int
	if _, err := fmt.Scanln(&choice); err != nil {
		return 0, fmt.Errorf("读取输入失败: %w", err)
	}

	if choice < MinChatChoice || choice > maxChoice {
		return 0, fmt.Errorf("选择无效: %d", choice)
	}

	return choice, nil
}

// selectMode 选择操作模式。输入失败或非法时返回错误，不能替用户默认执行真实下载。
func selectMode(input io.Reader, output io.Writer) (int, error) {
	_, _ = fmt.Fprintln(output, "\n请选择操作模式:")
	_, _ = fmt.Fprintln(output, "1. 只下载历史媒体文件")
	_, _ = fmt.Fprintln(output, "2. 只监控新消息")
	_, _ = fmt.Fprintln(output, "3. 下载历史媒体文件 + 监控新消息")

	_, _ = fmt.Fprint(output, "\n请选择模式 (1-3): ")
	var choice string
	if _, err := fmt.Fscanln(input, &choice); err != nil {
		return 0, fmt.Errorf("读取模式输入失败: %w", err)
	}

	mode, err := strconv.Atoi(choice)
	if err != nil || mode < ModeDownloadHistory || mode > MaxModeChoice {
		return 0, fmt.Errorf("无效模式 %q，必须输入 1、2 或 3", choice)
	}

	return mode, nil
}

// clearSessionAndExit 清除会话并退出
func clearSessionAndExit() {
	fmt.Println("正在清除会话...")
	if err := clearSession(); err != nil {
		fmt.Printf("清除会话失败: %v\n", err)
		os.Exit(ExitCodeSessionError)
	}

	fmt.Println("会话已清除，下次启动将需要重新登录")
}

// clearSession 只需要会话目录，不要求 API 凭据。凭据缺失或填写错误正是用户最常需要
// 清理本地登录状态的场景，不能让严格的 CLI 配置校验挡在恢复入口之前。
func clearSession() error {
	sessionDir, err := config.LoadSessionDir()
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}
	if err := telegram.RemoveSessionDatabase(sessionDir); err != nil {
		return fmt.Errorf("删除 TDLib 会话目录失败: %w", err)
	}
	return nil
}
