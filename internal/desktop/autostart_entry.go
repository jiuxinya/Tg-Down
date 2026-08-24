package desktop

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 自启项的内容生成与解析。三个平台的实现各自只负责落盘/注册表，
// 转义与陈旧判定放在这里，才能在任意平台上跑同一份测试。

// darwinLaunchLabel 是 LaunchAgent 的 Label，同时用作 plist 文件名前缀
const darwinLaunchLabel = "app.tg-down.desktop"

// currentExecutable 返回当前可执行文件解析软链后的绝对路径
func currentExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("定位可执行文件失败: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("解析可执行文件路径失败: %w", err)
	}
	return resolved, nil
}

// --- macOS: LaunchAgent plist ---

// plistContent 生成 LaunchAgent。可执行路径必须 XML 转义：
// 路径里出现 & 或 < 时原样插入会让 plist 变成非法 XML，launchd 直接拒绝加载。
func plistContent(label, exe string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>Label</key><string>` + xmlEscape(label) + `</string>
	<key>ProgramArguments</key><array>` + plistExecFragment(exe) + `</array>
	<key>RunAtLoad</key><true/>
</dict></plist>
`
}

// plistExecFragment 是 ProgramArguments 里代表可执行路径的那一段，
// Enabled 用它判断 plist 记录的路径是否仍是当前可执行文件。
func plistExecFragment(exe string) string {
	return "<string>" + xmlEscape(exe) + "</string>"
}

func xmlEscape(s string) string {
	var buf bytes.Buffer
	if err := xml.EscapeText(&buf, []byte(s)); err != nil {
		return s
	}
	return buf.String()
}

// --- Linux: XDG .desktop ---

// desktopEntryContent 生成 XDG 自启项；hidden 为真时写出被禁用的条目
// （桌面环境自身的自启开关也是这么标记的，Enabled 两种来源一起认）。
func desktopEntryContent(exe string, hidden bool) string {
	gnome, extra := "true", ""
	if hidden {
		gnome, extra = "false", "Hidden=true\n"
	}
	return `[Desktop Entry]
Type=Application
Name=` + AppName + `
` + desktopEntryExecLine(exe) + `
Terminal=false
X-GNOME-Autostart-enabled=` + gnome + `
Categories=Network;
` + extra
}

func desktopEntryExecLine(exe string) string {
	return "Exec=" + escapeDesktopExec(exe)
}

// escapeDesktopExec 按 Desktop Entry 规范转义可执行路径：整体加双引号，
// 其中的 " ` $ \ 先按 Exec 规则加一个反斜杠，再按 string 值规则把每个反斜杠写成两个。
// 不转义时，带空格的路径会被拆成两个参数，带 $ 的路径会被当成变量展开。
func escapeDesktopExec(exe string) string {
	var b strings.Builder
	// 预留转义字符与引号的余量，避免逐字符追加时反复扩容
	const escapeHeadroom = 8
	b.Grow(len(exe) + escapeHeadroom)
	b.WriteByte('"')
	for _, r := range exe {
		switch r {
		case '\\':
			b.WriteString(`\\\\`)
		case '"', '`', '$':
			b.WriteString(`\\`)
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// desktopEntryEnabled 判断条目是否处于启用状态：Hidden=true 或
// X-GNOME-Autostart-enabled=false 都表示已被关闭。
func desktopEntryEnabled(content string) bool {
	for line := range strings.SplitSeq(content, "\n") {
		switch strings.TrimSpace(line) {
		case "Hidden=true", "X-GNOME-Autostart-enabled=false":
			return false
		}
	}
	return true
}

// hasLine 判断内容里是否存在与 want 完全相同的一行（用于比对 Exec= 行）
func hasLine(content, want string) bool {
	for line := range strings.SplitSeq(content, "\n") {
		if strings.TrimRight(line, "\r") == want {
			return true
		}
	}
	return false
}
