# 桌面客户端

Windows / macOS / Linux 三平台的桌面客户端，内嵌完整下载引擎（TDLib + 任务队列 + Web 管理台），
无需单独安装服务端。Docker 部署不受影响，仍使用网页端；桌面客户端可把远程 Docker 实例
一并接入统一管理。

## 功能

- **本地引擎**：与 `tg-down --web` 完全同一套后端，数据落在应用数据目录
  - macOS: `~/Library/Application Support/Tg-Down`
  - Windows: `%APPDATA%\Tg-Down`
  - Linux: `$XDG_CONFIG_HOME/tg-down`（或 `~/.config/tg-down`）
- **远程实例**：「实例…」面板添加多台 Docker 实例（地址 + 访问令牌），同一窗口切换管理；
  凭据仅保存在本机 `instances.json`（0600），经壳层反代注入，不暴露给界面层
- **统一设置**：设置页覆盖代理、并发、重试、元数据、通知、日志级别等完整配置项；
  能热应用的立即生效，需重启的会给出提示
- **报错信息**：日志页按级别过滤（错误/警告快捷视图）；日志默认落盘
  `logs/tg-down.log`（10MB 轮转 ×3）；任务失败弹系统通知
- **系统集成**：托盘菜单（显示/暂停全部/恢复全部/开机自启/检查更新/退出）、
  关闭即隐藏到托盘、单实例锁（重复启动唤起已有窗口）、启动时检查新版本

## 安装

| 平台 | 产物 | 说明 |
|------|------|------|
| macOS Apple Silicon | `tg-down-desktop-darwin-arm64.dmg` | 未签名；首次打开右键 → 打开 |
| Windows x86_64 | `tg-down-desktop-windows-amd64-setup.exe` 或便携 zip | 安装包含 WebView2 引导（Win10 缺运行时时自动装） |
| Linux x86_64 | deb / rpm / tar.gz | 依赖 webkit2gtk-4.1：Ubuntu 22.04+/Debian 12+ |

Linux 依赖安装：

```bash
# Debian/Ubuntu
sudo apt install libgtk-3-0 libwebkit2gtk-4.1-0 libayatana-appindicator3-1
# Fedora
sudo dnf install gtk3 webkit2gtk4.1 libayatana-appindicator-gtk3
```

deb / rpm 已声明上述依赖，由包管理器自动装；tar.gz 便携包需手动安装。
`libayatana-appindicator` 缺失时程序仍可运行，但托盘图标不显示。

## 从源码构建

依赖与服务器版一致（Go 1.25+、TDLib、Node），另加各平台 WebView：

```bash
make tdlib          # 首次必需
make build-desktop  # 产出 tg-down-desktop（macOS 自动补 UniformTypeIdentifiers 链接框架）
```

必须带 `desktop,production` 构建标签。Linux 需要
`libgtk-3-dev libwebkit2gtk-4.1-dev` 并追加 `webkit2_41` 标签。

打包发布产物：

```bash
make package-desktop            # 当前平台
bash scripts/package-desktop.sh <darwin|windows|linux> <amd64|arm64> v版本号
```

## 架构速览

```
WebView (系统 WebView2/WKWebView/webkit2gtk)
   │  http://127.0.0.1:<壳端口>
   ▼
Shell 壳服务（internal/desktop）
   ├── /                     内嵌 SPA（与 Web 管理台同一份构建产物）
   ├── /desktop/api/*        实例注册表 / 自启开关 / 应用信息
   ├── /api/*                反代本地引擎（同源相对路径落在壳端口，须转发；
   │                         剥离 Origin/Referer/Cookie 以通过引擎 CSRF 校验）
   └── /api/remote/{id}/*    反代远程实例（注入令牌，SSE 流式透传）
        │
本机引擎 = internal/web.Server.Serve() @ 127.0.0.1:<随机端口>
托盘/自启/通知/更新检查 = cmd/desktop + internal/desktop
```

设计约束：`internal/desktop` 不 import `internal/telegram`（CGo 边界守卫），
引擎装配在 `cmd/desktop` 完成；快速测试车道因此保持免 TDLib。

## 故障排除

| 问题 | 处理 |
|------|------|
| 启动即退出，提示 build tags | 必须用 `-tags "desktop,production"` 构建，见上文 |
| 托盘图标缺失（Linux） | 多数情况是缺 `libayatana-appindicator`（托盘经 DBus StatusNotifierItem 实现）；deb/rpm 已声明该依赖，tar.gz 需自行安装。仍不显示时设 `TG_DOWN_DESKTOP_NO_TRAY=1` 或加 `--no-tray` 参数 |
| 远程实例一直转圈 | 「实例… → 测试」看具体错误；确认地址可达且令牌 ≥16 字符 |
| 日志在哪 | 应用数据目录 `logs/tg-down.log`；设置页可调级别 |
