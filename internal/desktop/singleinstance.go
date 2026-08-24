package desktop

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 单实例互斥。Wails 自带的 SingleInstanceLock 在 wails.Run 里才生效，那时第二个进程
// 已经切过工作目录、打开了 SQLite、拉起了 TDLib 并占了两个回环端口——两个进程同时
// 持有同一个库文件与同一份 TDLib 会话目录，损坏就发生在这段窗口里。
// 因此把互斥前移到打开任何资源之前，用应用数据目录里的文件锁完成（不依赖 D-Bus，
// Linux 上会话总线不可用时同样成立）。

const (
	// instanceLockFile 是文件锁的载体，锁随进程退出（含崩溃）由内核自动释放
	instanceLockFile = "instance.lock"
	// shellURLFile 记录当前实例的壳服务地址，供后来者唤起已运行的窗口
	shellURLFile = "shell.url"

	showRequestTimeout = 2 * time.Second
)

// ErrAlreadyRunning 表示另一个实例已持有锁
var ErrAlreadyRunning = errors.New("另一个 Tg-Down 实例正在运行")

// InstanceLock 是持有中的单实例锁
type InstanceLock struct {
	f *os.File
}

// AcquireInstanceLock 在 appDir 下取独占锁。已有实例持锁时返回 ErrAlreadyRunning。
func AcquireInstanceLock(appDir string) (*InstanceLock, error) {
	p := filepath.Join(appDir, instanceLockFile)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // 应用数据目录内的固定文件名
	if err != nil {
		return nil, fmt.Errorf("打开实例锁失败: %w", err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		if errors.Is(err, ErrAlreadyRunning) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("获取实例锁失败: %w", err)
	}
	return &InstanceLock{f: f}, nil
}

// Release 释放锁。进程异常退出时内核也会释放，这里只是让正常退出更快让位。
func (l *InstanceLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = unlockFile(l.f)
	_ = l.f.Close()
	l.f = nil
}

// PublishShellURL 记录本实例的壳地址，供第二个实例唤起窗口
func PublishShellURL(appDir, url string) error {
	return os.WriteFile(filepath.Join(appDir, shellURLFile), []byte(url), 0o600)
}

// ClearShellURL 退出时清掉地址记录，避免下次启动对着死端口发请求
func ClearShellURL(appDir string) {
	_ = os.Remove(filepath.Join(appDir, shellURLFile))
}

// ShowRunningInstance 让已在运行的实例把窗口拉到前台。
// 抢锁失败的进程调用它后即可退出，用户看到的仍是"点了图标窗口就出来"。
func ShowRunningInstance(appDir string) error {
	data, err := os.ReadFile(filepath.Join(appDir, shellURLFile)) //nolint:gosec // 应用数据目录内的固定文件名
	if err != nil {
		return fmt.Errorf("读取运行中实例地址失败: %w", err)
	}
	base := strings.TrimSpace(string(data))
	if base == "" {
		return errors.New("运行中实例地址为空")
	}
	client := &http.Client{Timeout: showRequestTimeout}
	//nolint:noctx // 已由 Client.Timeout 兜底，且调用点随即退出进程
	resp, err := client.Post(strings.TrimRight(base, "/")+"/desktop/api/show", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
