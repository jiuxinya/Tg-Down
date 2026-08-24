// 桌面客户端入口：以内嵌引擎（internal/telegram + internal/web）为后端，
// Wails WebView 为界面，壳层服务（internal/desktop）负责实例反代与系统集成。
//
// 构建：make build-desktop（三平台各自原生编译，与服务器版同一 TDLib 链接要求）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/gen2brain/beeep"

	"tg-down/internal/config"
	"tg-down/internal/desktop"
	"tg-down/internal/logger"
	"tg-down/internal/store"
	"tg-down/internal/telegram"
	"tg-down/internal/web"
)

// version 由构建时 -ldflags "-X main.version=..." 注入
var version = "dev"

// trayDisableEnv 设为 0/1 之外的任意非空值时禁用托盘（部分 Wayland 会话的兜底开关）
const trayDisableEnv = "TG_DOWN_DESKTOP_NO_TRAY"

func main() {
	noTray := flag.Bool("no-tray", false, "禁用系统托盘（调试用）")
	flag.Parse()

	if err := run(!*noTray); err != nil {
		fmt.Fprintf(os.Stderr, "[桌面端] %v\n", err)
		os.Exit(1)
	}
}

// engine 抽象内嵌引擎的生命周期，便于在没有 TDLib 与 GUI 的测试里替换实现
type engine interface {
	Base() string
	Log() *logger.Logger
	Wait()
	Close()
}

// startEngineFn 供测试替换
var startEngineFn = func(ctx context.Context) (engine, error) { return startEngine(ctx) }

func run(enableTray bool) error {
	appDir, err := desktop.AppDir()
	if err != nil {
		return err
	}

	// 单实例互斥必须在打开数据库、拉起 TDLib、绑定端口之前完成：Wails 自带的锁
	// 要等到 wails.Run 才生效，那时两个进程已经同时握着同一个库和同一份会话目录。
	lock, err := desktop.AcquireInstanceLock(appDir)
	if errors.Is(err, desktop.ErrAlreadyRunning) {
		if serr := desktop.ShowRunningInstance(appDir); serr != nil {
			fmt.Fprintf(os.Stderr, "[桌面端] %v（唤起已有窗口失败: %v）\n", desktop.ErrAlreadyRunning, serr)
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Release()

	// 引擎生命周期上下文：随窗口退出取消，带动 Telegram 连接、队列、HTTP 全部收尾
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	eng, err := startEngineFn(ctx)
	if err != nil {
		return fmt.Errorf("启动本地下载引擎失败: %w", err)
	}
	log := eng.Log()

	// 收尾顺序对所有返回路径一致：先取消上下文让引擎自行停服，等它完全退出，
	// 再关壳、最后关库。顺序颠倒（LIFO 默认顺序）会在引擎仍在服务时关掉 store，
	// web.Server 的协程随即对着已关闭的库继续跑。
	var shell *desktop.Shell
	shutdown := sync.OnceFunc(func() {
		cancel()
		eng.Wait()
		if shell != nil {
			shell.Stop()
		}
		desktop.ClearShellURL(appDir)
		eng.Close()
	})
	defer shutdown()

	reg, err := desktop.OpenRegistry(appDir + "/" + desktop.InstancesFile)
	if err != nil {
		return fmt.Errorf("加载远程实例注册表失败: %w", err)
	}

	autostart := desktop.NewAutostart()
	shell = desktop.NewShell(reg, eng.Base(), appDir, version, autostart, log)
	ui := &uiApp{engineBase: eng.Base(), log: log}
	shell.SetOnShow(ui.show)
	shellURL, err := shell.Start(ctx)
	if err != nil {
		return fmt.Errorf("启动壳服务失败: %w", err)
	}
	ui.shellURL = shellURL
	if err := desktop.PublishShellURL(appDir, shellURL); err != nil {
		// 只影响"第二次启动唤起已有窗口"，主流程照常
		log.Warn("记录壳服务地址失败: %v", err)
	}

	// 本地任务失败的系统通知（远程实例由其自身 notify 配置负责）
	go desktop.WatchTaskFailures(ctx, eng.Base(), log)

	go checkUpdateOnce(appDir, log)

	if enableTray && os.Getenv(trayDisableEnv) == "" {
		ui.startTrayAsync(autostart)
	}

	if err := wailsRun(buildOptions(ui)); err != nil {
		return fmt.Errorf("窗口运行失败: %w", err)
	}
	shutdown()
	log.Info("桌面客户端已退出")
	return nil
}

// --- 引擎托管 ---

type engineHandle struct {
	base string
	srv  *web.Server
	// client 持引用防止 GC；连接关闭由 web.Server 在 ctx 路径完成
	client *telegram.Client
	st     *store.Store
	done   chan struct{}
	log    *logger.Logger
}

// Base 返回引擎根地址（形如 http://127.0.0.1:<port>）
func (e *engineHandle) Base() string { return e.base }

// Log 返回引擎日志器（壳层与托盘共用同一个 sink）
func (e *engineHandle) Log() *logger.Logger { return e.log }

// Wait 阻塞直到引擎 HTTP 服务完全退出
func (e *engineHandle) Wait() { <-e.done }

// Close 幂等清理存储
func (e *engineHandle) Close() {
	if e.st != nil {
		_ = e.st.Close()
		e.st = nil
	}
}

// startEngine 复刻 --web 模式的装配链，但监听回环随机端口且不注册信号处理：
// 生命周期完全由窗口/托盘驱动。
func startEngine(ctx context.Context) (*engineHandle, error) {
	cfg, err := config.LoadConfigForWeb()
	if err != nil {
		return nil, fmt.Errorf("加载配置失败: %w", err)
	}
	log := logger.New(cfg.Log.Level)
	// 桌面端默认开启文件日志（应用数据目录 logs/tg-down.log），便于排查崩溃；
	// 用户显式配置过 log.file 则尊重原值
	logFile := cfg.Log.File
	if logFile == "" {
		if logsDir, lerr := desktop.LogsDir(); lerr == nil {
			logFile = logsDir + "/tg-down.log"
		}
	}
	if logFile != "" {
		if serr := log.SetFileSink(logFile, logger.DefaultFileLogMaxBytes, logger.DefaultFileLogKeep); serr != nil {
			log.Warn("日志文件不可用，仅输出到控制台: %v", serr)
		}
	}

	clearWebToken(log)

	log.Info("Tg-Down 桌面客户端 %s 启动", version)
	client := telegram.NewWithUpdates(cfg, log, 0)
	if client == nil {
		return nil, fmt.Errorf("创建客户端失败")
	}
	st, err := store.Open(cfg.Store.Path)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	srv := web.New(client, st, log, "127.0.0.1:0", cfg)

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("绑定引擎端口失败: %w", err)
	}
	h := &engineHandle{
		base: "http://" + ln.Addr().String(),
		srv:  srv,
		log:  log,
		done: make(chan struct{}),
	}
	h.client = client
	h.st = st
	go func() {
		defer close(h.done)
		if err := h.srv.Serve(ctx, ln); err != nil {
			// 端口冲突、监听器失效等：不报出来的话界面只会一直停在"正在连接"
			log.Error("本地下载引擎异常退出: %v", err)
		}
	}()
	return h, nil
}

// webTokenEnv 与 internal/web 读取的变量同名。AppDir() 会把工作目录切到应用数据目录，
// LoadConfigForWeb 随后从那里加载 .env，服务器版留下的 TG_DOWN_WEB_TOKEN 会一并生效。
const webTokenEnv = "TG_DOWN_WEB_TOKEN" // #nosec G101 -- 环境变量名，非硬编码凭据

// clearWebToken 在桌面模式下清掉引擎的访问令牌。
//
// 选这条而不是"把令牌透传给内部调用方"：内嵌引擎只监听 127.0.0.1 的随机端口，
// 它前面的壳服务同样无鉴权，能访问引擎端口的一方必然也能访问壳端口再经反代过去，
// 令牌因此不提供任何额外隔离，却会让壳反代、托盘控制、事件流监视三处内部调用
// 全部静默 401（界面停在"正在连接"，且日志里看不出原因）。
// 远程实例的鉴权与此无关：那是 instances.json 里的 per-instance 令牌，走 /api/remote/ 注入。
func clearWebToken(log *logger.Logger) {
	if os.Getenv(webTokenEnv) == "" {
		return
	}
	log.Warn("检测到 %s，桌面端内嵌引擎仅监听回环端口，已忽略该令牌；远程实例令牌不受影响", webTokenEnv)
	if err := os.Unsetenv(webTokenEnv); err != nil {
		log.Error("清除 %s 失败，内部调用可能被拒绝: %v", webTokenEnv, err)
	}
}

// checkUpdateOnce 单次更新检查并弹系统通知（同一版本只提示一次）
func checkUpdateOnce(appDir string, log *logger.Logger) {
	rel, err := desktop.CheckUpdate(version)
	if err != nil {
		log.Debug("更新检查跳过: %v", err)
		return
	}
	if rel == nil {
		return
	}
	msg := "发现新版本 " + rel.Tag + "，请到发布页下载"
	log.Info("%s：%s", msg, rel.URL)
	if !desktop.ClaimUpdateNotice(appDir, rel.Version) {
		return // 同一版本已提示过，不再每次启动打扰
	}
	if err := beeep.Notify(desktop.AppName, msg, ""); err != nil {
		log.Debug("系统通知发送失败: %v", err)
	}
}
