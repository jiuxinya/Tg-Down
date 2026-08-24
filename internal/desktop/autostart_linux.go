//go:build linux

package desktop

import (
	"fmt"
	"os"
	"path/filepath"
)

// Linux 实现：XDG 自启目录下的 .desktop 文件（GNOME/KDE/多数桌面环境通用）

const linuxEntryName = "tg-down.desktop"

type linuxAutostart struct{}

// NewAutostart 返回当前平台的实现
func NewAutostart() Autostart { return linuxAutostart{} }

func (linuxAutostart) entryPath() (string, error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("解析用户配置目录失败: %w", err)
	}
	return filepath.Join(cfg, "autostart", linuxEntryName), nil
}

// Enabled 要求条目未被标记 Hidden，且 Exec 仍指向当前可执行文件：
// 应用被移动或原地更新后旧条目还在，只看存在性会报告"已启用"而实际登录时拉不起来。
func (linuxAutostart) Enabled() (bool, error) {
	p, err := (linuxAutostart{}).entryPath()
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(p) //nolint:gosec // p 由 entryPath 从用户配置目录拼出，非外部输入
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("读取自启条目失败: %w", err)
	}
	content := string(data)
	if !desktopEntryEnabled(content) {
		return false, nil
	}
	exe, err := currentExecutable()
	if err != nil {
		return false, err
	}
	return hasLine(content, desktopEntryExecLine(exe)), nil
}

func (linuxAutostart) Enable() error {
	exe, err := currentExecutable()
	if err != nil {
		return err
	}
	p, err := (linuxAutostart{}).entryPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return fmt.Errorf("创建 autostart 目录失败: %w", err)
	}
	return os.WriteFile(p, []byte(desktopEntryContent(exe, false)), 0o600)
}

// Disable 改写条目为 Hidden=true 而不是删除文件：Enabled 判定的就是这个标记，
// 桌面环境自身的自启开关也是这么标记的，两边保持同一套语义。
func (linuxAutostart) Disable() error {
	p, err := (linuxAutostart{}).entryPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return nil // 本来就没有条目，无需写一个"已禁用"的出来
	}
	exe, err := currentExecutable()
	if err != nil {
		return err
	}
	return os.WriteFile(p, []byte(desktopEntryContent(exe, true)), 0o600)
}
