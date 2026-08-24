package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"time"

	"github.com/energye/systray"
	"github.com/gen2brain/beeep"

	"tg-down/internal/desktop"
)

// startTrayAsync 注册系统托盘。systray.Register 非阻塞，专为内嵌进已有事件循环
// （这里是 Wails）的应用设计；菜单点击经回调分发。
func (a *uiApp) startTrayAsync(autostart desktop.Autostart) {
	systray.Register(func() { a.onTrayReady(autostart) }, func() {})
}

func (a *uiApp) onTrayReady(autostart desktop.Autostart) {
	if runtime.GOOS == "windows" {
		systray.SetIcon(desktop.TrayIconICO())
	} else {
		systray.SetIcon(desktop.TrayIconPNG())
	}
	systray.SetTooltip(desktop.AppName + " " + version)

	mShow := systray.AddMenuItem("显示主窗口", "打开管理窗口")
	systray.AddSeparator()
	mPause := systray.AddMenuItem("暂停全部下载", "暂停本地引擎的全部媒体下载")
	mResume := systray.AddMenuItem("恢复全部下载", "恢复本地引擎的媒体下载")
	systray.AddSeparator()
	enabled, err := autostart.Enabled()
	if err != nil {
		a.log.Warn("查询开机自启状态失败，菜单按未启用显示: %v", err)
	}
	mAuto := systray.AddMenuItemCheckbox("开机自启", "登录系统时自动启动 Tg-Down", enabled)
	mUpdate := systray.AddMenuItem("检查更新…", "查询 GitHub 最新发布")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "结束引擎并退出")

	mShow.Click(a.show)
	mPause.Click(func() { a.enginePost("/api/media/pause-all") })
	mResume.Click(func() { a.enginePost("/api/media/resume-all") })
	mUpdate.Click(a.checkUpdateInteractive)
	mQuit.Click(a.quit)
	mAuto.Click(func() { a.toggleAutostart(autostart, mAuto) })
}

// trayCheckbox 是 systray 复选菜单项中本文件用到的部分，便于测试替身
type trayCheckbox interface {
	Check()
	Uncheck()
}

// toggleAutostart 切换开机自启，并让复选框严格跟随系统的实际状态。
// 此前 Disable 失败被完全吞掉：勾选框保持勾上，用户以为关掉了，下次登录照样自启。
func (a *uiApp) toggleAutostart(autostart desktop.Autostart, item trayCheckbox) {
	on, err := autostart.Enabled()
	if err != nil {
		a.notifyErr("读取开机自启状态失败", err)
		return
	}
	action, verb := autostart.Enable, "开启"
	if on {
		action, verb = autostart.Disable, "关闭"
	}
	if err := action(); err != nil {
		a.notifyErr(verb+"开机自启失败", err)
	}
	// 无论成败都回读一次：写入部分成功（如 plist 已落盘但 launchctl 报错）时，
	// 勾选框应显示系统里真正的状态，而不是这次点击的意图。
	now, err := autostart.Enabled()
	if err != nil {
		a.log.Warn("回读开机自启状态失败: %v", err)
		return
	}
	if now {
		item.Check()
	} else {
		item.Uncheck()
	}
}

func (a *uiApp) notifyErr(what string, err error) {
	a.log.Error("%s: %v", what, err)
	if nerr := beeep.Notify(desktop.AppName, what+": "+err.Error(), ""); nerr != nil {
		a.log.Debug("系统通知发送失败: %v", nerr)
	}
}

// enginePost 调用本地引擎的控制端点（回环无鉴权）
func (a *uiApp) enginePost(path string) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(a.engineBase+path, "application/json", bytes.NewReader(nil)) //nolint:noctx // 固定拼接的回环地址
	if err != nil {
		a.notifyErr("调用本地引擎失败", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// 静默丢弃状态码时，托盘的暂停/恢复点了没反应也查不出原因
		body, _ := io.ReadAll(io.LimitReader(resp.Body, engineErrBodyLimit))
		a.notifyErr("本地引擎拒绝了请求", fmt.Errorf("%s HTTP %d: %s", path, resp.StatusCode, bytes.TrimSpace(body)))
	}
}

// engineErrBodyLimit 限制回显的错误正文长度
const engineErrBodyLimit = 512

func (a *uiApp) checkUpdateInteractive() {
	go func() {
		rel, err := desktop.CheckUpdate(version)
		switch {
		case err != nil:
			_ = beeep.Notify(desktop.AppName, fmt.Sprintf("更新检查失败: %v", err), "")
		case rel == nil:
			_ = beeep.Notify(desktop.AppName, "当前已是最新版本 "+version, "")
		default:
			_ = beeep.Notify(desktop.AppName, "发现新版本 "+rel.Tag+"，正在打开发布页…", "")
			a.openExternal(rel.URL)
		}
	}()
}
