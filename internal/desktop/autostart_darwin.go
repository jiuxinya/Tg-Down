package desktop

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// macOS 实现：LaunchAgent plist + launchctl。KeepAlive 不设——自启只负责登录时拉起，
// 退出后不自动复活（用户可从托盘手动再开）。

const (
	darwinPlistName = darwinLaunchLabel + ".plist"
	// launchctlTimeout 给 launchctl 一个上限：它偶发挂起时不应拖住调用它的界面线程
	launchctlTimeout = 10 * time.Second
)

type darwinAutostart struct{}

func (d darwinAutostart) plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("解析用户主目录失败: %w", err)
	}
	dir := filepath.Join(home, "Library", "LaunchAgents")
	return filepath.Join(dir, darwinPlistName), nil
}

// NewAutostart 返回当前平台的实现
func NewAutostart() Autostart { return darwinAutostart{} }

// Enabled 除了检查 plist 是否存在，还要确认里面记录的路径仍是当前可执行文件：
// 应用被移动或原地更新后旧 plist 还在，只看存在性会报告"已启用"而实际登录时拉不起来。
func (darwinAutostart) Enabled() (bool, error) {
	p, err := (darwinAutostart{}).plistPath()
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(p) //nolint:gosec // p 由 plistPath 从用户主目录拼出，非外部输入
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("读取 LaunchAgent 失败: %w", err)
	}
	exe, err := currentExecutable()
	if err != nil {
		return false, err
	}
	return strings.Contains(string(data), plistExecFragment(exe)), nil
}

func (darwinAutostart) Enable() error {
	exe, err := currentExecutable()
	if err != nil {
		return err
	}
	p, err := (darwinAutostart{}).plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return fmt.Errorf("创建 LaunchAgents 目录失败: %w", err)
	}
	// 路径可能变过，先卸掉旧的再写新的，否则 launchctl load 会因已加载而不生效。
	// 未加载时 unload 报错属预期，不用管。
	_ = runLaunchctl("unload", p)
	if err := os.WriteFile(p, []byte(plistContent(darwinLaunchLabel, exe)), 0o600); err != nil {
		return fmt.Errorf("写入 LaunchAgent 失败: %w", err)
	}
	if err := runLaunchctl("load", p); err != nil {
		// plist 已写入，登录时仍会生效；只有本次会话没被立即注册
		return fmt.Errorf("注册 LaunchAgent 失败（重新登录后仍会生效）: %w", err)
	}
	return nil
}

func (darwinAutostart) Disable() error {
	p, err := (darwinAutostart{}).plistPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return nil
	}
	// agent 未加载时 unload 必然报错，这里无从区分，只作最大努力尝试；
	// 真正决定下次登录是否自启的是 plist 文件本身，删除失败才是要报出来的错。
	_ = runLaunchctl("unload", p)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除 LaunchAgent 失败: %w", err)
	}
	return nil
}

// runLaunchctl 执行 launchctl 子命令并回报结果。此前一律 `_ =` 丢弃，
// Enable 因此永远返回 nil：注册失败在界面上看不出任何区别。
func runLaunchctl(action, plist string) error {
	ctx, cancel := context.WithTimeout(context.Background(), launchctlTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "launchctl", action, "-w", plist).CombinedOutput() //nolint:gosec // 固定命令，plist 为本函数生成的路径
	if err == nil {
		return nil
	}
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return fmt.Errorf("launchctl %s: %w: %s", action, err, msg)
	}
	return fmt.Errorf("launchctl %s: %w", action, err)
}
