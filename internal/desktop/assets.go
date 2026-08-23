package desktop

import _ "embed"

//go:embed assets/tray.png
var trayPNG []byte

//go:embed assets/tray.ico
var trayICO []byte

// TrayIconPNG 返回托盘图标（macOS / Linux）
func TrayIconPNG() []byte { return trayPNG }

// TrayIconICO 返回托盘图标（Windows 要求 ICO 格式）
func TrayIconICO() []byte { return trayICO }
