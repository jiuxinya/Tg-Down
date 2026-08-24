package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/wailsapp/wails/v2/pkg/options"

	"tg-down/internal/desktop"
	"tg-down/internal/logger"
)

// fakeEngine 记录收尾动作发生的顺序与当时的上下文状态。
// 不碰 TDLib、不开 SQLite，因此可以在没有 GUI 的测试里跑完整条启动/收尾链路。
type fakeEngine struct {
	base string
	log  *logger.Logger
	ctx  context.Context

	mu       sync.Mutex
	order    []string
	waited   bool
	closedAt struct {
		afterWait   bool
		ctxCanceled bool
	}
}

func (e *fakeEngine) Base() string        { return e.base }
func (e *fakeEngine) Log() *logger.Logger { return e.log }

func (e *fakeEngine) Wait() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.waited = true
	e.order = append(e.order, "wait")
}

func (e *fakeEngine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closedAt.afterWait = e.waited
	e.closedAt.ctxCanceled = e.ctx.Err() != nil
	e.order = append(e.order, "close")
}

// setupRun 把 run() 的外部依赖换成测试替身，返回被创建的引擎
func setupRun(t *testing.T, wails func(*options.App) error) *fakeEngine {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(desktop.DirOverrideEnv, dir)
	t.Setenv(desktop.UpdateCheckDisabledEnv, "1") // 测试不外联
	t.Setenv(trayDisableEnv, "")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) }) // AppDir 会切工作目录

	// 引擎地址指向一个真实但只会 404 的服务：事件流监视立刻失败重试，不挂在拨号上
	stub := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(stub.Close)

	eng := &fakeEngine{base: stub.URL, log: logger.New("error")}
	realStart, realWails := startEngineFn, wailsRun
	t.Cleanup(func() { startEngineFn, wailsRun = realStart, realWails })
	startEngineFn = func(ctx context.Context) (engine, error) {
		eng.ctx = ctx
		return eng, nil
	}
	wailsRun = wails
	return eng
}

// TestShutdownOrderOnWailsError 回归测试：错误返回路径此前走的是 defer 的 LIFO 顺序，
// 先 shell.Stop 再 eng.Close 最后 cancel——store 在引擎上下文还活着时就被关掉，
// eng.Wait 从不执行，web.Server 的协程继续对着已关闭的库提供服务。
func TestShutdownOrderOnWailsError(t *testing.T) {
	wantErr := errors.New("窗口起不来")
	eng := setupRun(t, func(*options.App) error { return wantErr })

	err := run(false)
	if !errors.Is(err, wantErr) {
		t.Fatalf("run() = %v, want %v", err, wantErr)
	}
	assertOrderlyShutdown(t, eng)
}

func TestShutdownOrderOnHappyPath(t *testing.T) {
	eng := setupRun(t, func(*options.App) error { return nil })

	if err := run(false); err != nil {
		t.Fatalf("run() = %v", err)
	}
	assertOrderlyShutdown(t, eng)
}

func assertOrderlyShutdown(t *testing.T, eng *fakeEngine) {
	t.Helper()
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if !eng.waited {
		t.Error("必须等引擎 HTTP 服务退出（eng.Wait）")
	}
	if !eng.closedAt.ctxCanceled {
		t.Error("关闭 store 前必须先取消引擎上下文")
	}
	if !eng.closedAt.afterWait {
		t.Error("关闭 store 必须晚于 eng.Wait")
	}
	if len(eng.order) != 2 || eng.order[0] != "wait" || eng.order[1] != "close" {
		t.Errorf("收尾顺序 = %v, want [wait close]", eng.order)
	}
}

// TestSecondInstanceExitsBeforeOpeningEngine 单实例互斥此前落在 wails.Run 里，
// 第二个进程要先切目录、开库、拉 TDLib、绑两个回环端口之后才被拦下。
func TestSecondInstanceExitsBeforeOpeningEngine(t *testing.T) {
	started := false
	eng := setupRun(t, func(*options.App) error { return nil })
	inner := startEngineFn
	startEngineFn = func(ctx context.Context) (engine, error) {
		started = true
		return inner(ctx)
	}

	// 模拟"已有实例正在运行"
	lock, err := desktop.AcquireInstanceLock(os.Getenv(desktop.DirOverrideEnv))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	if err := run(false); err != nil {
		t.Fatalf("第二个实例应安静退出，got %v", err)
	}
	if started {
		t.Error("抢锁失败时不应启动引擎（开库/拉 TDLib/绑端口）")
	}
	if eng.waited {
		t.Error("引擎根本不该被创建")
	}
}
