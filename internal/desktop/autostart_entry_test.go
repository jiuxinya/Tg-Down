package desktop

import (
	"encoding/xml"
	"strings"
	"testing"
)

// TestPlistEscapesExecutablePath 路径里带 & 或 < 时原样插入会让 plist 变成非法 XML，
// launchd 直接拒绝加载，界面却报告"已启用"。
func TestPlistEscapesExecutablePath(t *testing.T) {
	exe := `/Applications/Tg & Down/tg-down<beta>`
	content := plistContent(darwinLaunchLabel, exe)

	if strings.Contains(content, "Down/tg-down<beta>") {
		t.Error("可执行路径未做 XML 转义")
	}
	if !strings.Contains(content, "&amp;") || !strings.Contains(content, "&lt;beta&gt;") {
		t.Errorf("转义结果不符:\n%s", content)
	}
	// 必须仍是合法 XML
	if err := xml.Unmarshal([]byte(content), new(struct {
		XMLName xml.Name `xml:"plist"`
	})); err != nil {
		t.Fatalf("生成的 plist 不是合法 XML: %v\n%s", err, content)
	}
}

// TestPlistStalenessDetection 应用被移动或原地更新后旧 plist 还在，
// 只看文件存在性会报告"已启用"而登录时其实拉不起来。
func TestPlistStalenessDetection(t *testing.T) {
	content := plistContent(darwinLaunchLabel, "/Applications/Tg-Down.app/Contents/MacOS/tg-down")
	if !strings.Contains(content, plistExecFragment("/Applications/Tg-Down.app/Contents/MacOS/tg-down")) {
		t.Error("同一路径应判定为仍然有效")
	}
	if strings.Contains(content, plistExecFragment("/Users/me/Downloads/tg-down")) {
		t.Error("路径已变更应判定为陈旧")
	}
}

// TestEscapeDesktopExec 未转义时带空格的路径会被拆成两个参数，带 $ 的会被当变量展开
func TestEscapeDesktopExec(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/usr/bin/tg-down", `"/usr/bin/tg-down"`},
		{"/opt/Tg Down/tg-down", `"/opt/Tg Down/tg-down"`},
		{"/opt/a$b/tg-down", `"/opt/a\\$b/tg-down"`},
		{"/opt/a`b/tg-down", "\"/opt/a\\\\`b/tg-down\""},
		{`/opt/a"b/tg-down`, `"/opt/a\\"b/tg-down"`},
		{`/opt/a\b/tg-down`, `"/opt/a\\\\b/tg-down"`},
	}
	for _, c := range cases {
		if got := escapeDesktopExec(c.in); got != c.want {
			t.Errorf("escapeDesktopExec(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDesktopEntryStalenessAndHidden(t *testing.T) {
	exe := "/opt/Tg Down/tg-down"
	enabled := desktopEntryContent(exe, false)

	if !desktopEntryEnabled(enabled) {
		t.Error("新写的条目应为启用状态")
	}
	if !hasLine(enabled, desktopEntryExecLine(exe)) {
		t.Errorf("Exec 行比对失败:\n%s", enabled)
	}
	if hasLine(enabled, desktopEntryExecLine("/opt/other/tg-down")) {
		t.Error("路径已变更应判定为陈旧")
	}

	// Disable 写出的条目必须能被 Enabled 认成"已关闭"：
	// 此前 Disable 直接删文件，而 Enabled 读的是 Hidden=true，两边各说各话。
	disabled := desktopEntryContent(exe, true)
	if desktopEntryEnabled(disabled) {
		t.Errorf("Hidden 条目应判定为未启用:\n%s", disabled)
	}
	// 桌面环境自身关掉自启时写的是这个键，同样要认
	if desktopEntryEnabled("[Desktop Entry]\nX-GNOME-Autostart-enabled=false\n") {
		t.Error("X-GNOME-Autostart-enabled=false 应判定为未启用")
	}
}

func TestHasLineIsWholeLineMatch(t *testing.T) {
	content := "Exec=\"/opt/tg-down\"\nName=Tg-Down\n"
	if hasLine(content, `Exec="/opt/tg`) {
		t.Error("hasLine 不应做前缀匹配")
	}
	if !hasLine(content, `Exec="/opt/tg-down"`) {
		t.Error("整行相同应匹配")
	}
}
