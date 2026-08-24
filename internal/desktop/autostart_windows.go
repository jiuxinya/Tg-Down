//go:build windows

package desktop

import (
	"fmt"

	"golang.org/x/sys/windows/registry"
)

// Windows 实现：HKCU\...\Run 注册表键，值为带引号的完整可执行路径

const winRunKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const winValueName = "Tg-Down"

type windowsAutostart struct{}

// NewAutostart 返回当前平台的实现
func NewAutostart() Autostart { return windowsAutostart{} }

// Enabled 除了检查键值存在，还要确认记录的路径仍是当前可执行文件：
// 应用被移动或原地更新后旧键值还在，只看存在性会报告"已启用"而实际登录时拉不起来。
// samePath 比较两个可执行文件路径；Windows 路径不区分大小写。
func samePath(a, b string) bool { return strings.EqualFold(a, b) }

func (windowsAutostart) Enabled() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, winRunKey, registry.QUERY_VALUE)
	if err != nil {
		return false, fmt.Errorf("打开注册表 Run 键失败: %w", err)
	}
	defer func() { _ = k.Close() }()
	v, _, err := k.GetStringValue(winValueName)
	if err == registry.ErrNotExist {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	exe, err := currentExecutable()
	if err != nil {
		return false, err
	}
	return samePath(v, winRunValue(exe)), nil
}

// winRunValue 是写入 Run 键的值：整体加引号，带空格的路径才不会被拆成命令+参数
func winRunValue(exe string) string { return `"` + exe + `"` }

func (windowsAutostart) Enable() error {
	exe, err := currentExecutable()
	if err != nil {
		return err
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, winRunKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表 Run 键失败: %w", err)
	}
	defer func() { _ = k.Close() }()
	return k.SetStringValue(winValueName, winRunValue(exe))
}

func (windowsAutostart) Disable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, winRunKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表 Run 键失败: %w", err)
	}
	defer func() { _ = k.Close() }()
	err = k.DeleteValue(winValueName)
	if err == registry.ErrNotExist {
		return nil
	}
	return err
}
