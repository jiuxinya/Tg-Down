#!/usr/bin/env bash
# 打包桌面客户端发布产物。前置条件：
#   - 已用 make build-desktop（或等效 go build）产出二进制到 dist/desktop/
#   - darwin 需要 sips/iconutil（macOS 自带）；linux 打 deb/rpm 需要系统装有 nfpm
# 用法：scripts/package-desktop.sh <darwin|windows|linux> <amd64|arm64> <version>
set -euo pipefail

OS="${1:?os required}"
ARCH="${2:?arch required}"
VERSION="${3:-dev}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$ROOT/dist/desktop"
NAME="tg-down-desktop-$OS-$ARCH"

cd "$ROOT"

case "$OS" in
  darwin)
    APP="Tg-Down.app"
    rm -rf "$DIST/$APP" "$DIST/$NAME.dmg"
    mkdir -p "$DIST/$APP/Contents/MacOS" "$DIST/$APP/Contents/Resources"

    # icns：从 appicon.png 派生各尺寸（macOS runner 自带 sips/iconutil）
    ICONSET="$DIST/Tg-Down.iconset"
    rm -rf "$ICONSET"; mkdir -p "$ICONSET"
    sips -z 16 16     build/appicon.png --out "$ICONSET/icon_16x16.png"      >/dev/null
    sips -z 32 32     build/appicon.png --out "$ICONSET/icon_16x16@2x.png"   >/dev/null
    sips -z 32 32     build/appicon.png --out "$ICONSET/icon_32x32.png"      >/dev/null
    sips -z 64 64     build/appicon.png --out "$ICONSET/icon_32x32@2x.png"   >/dev/null
    sips -z 128 128   build/appicon.png --out "$ICONSET/icon_128x128.png"    >/dev/null
    sips -z 256 256   build/appicon.png --out "$ICONSET/icon_128x128@2x.png" >/dev/null
    sips -z 256 256   build/appicon.png --out "$ICONSET/icon_256x256.png"    >/dev/null
    sips -z 512 512   build/appicon.png --out "$ICONSET/icon_256x256@2x.png" >/dev/null
    sips -z 512 512   build/appicon.png --out "$ICONSET/icon_512x512.png"    >/dev/null
    sips -z 1024 1024 build/appicon.png --out "$ICONSET/icon_512x512@2x.png" >/dev/null
    iconutil -c icns "$ICONSET" -o "$DIST/$APP/Contents/Resources/app.icns"
    rm -rf "$ICONSET"

    cp "$DIST/tg-down-desktop" "$DIST/$APP/Contents/MacOS/Tg-Down"
    chmod +x "$DIST/$APP/Contents/MacOS/Tg-Down"
    cat > "$DIST/$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleName</key><string>Tg-Down</string>
  <key>CFBundleDisplayName</key><string>Tg-Down</string>
  <key>CFBundleIdentifier</key><string>app.tg-down.desktop</string>
  <key>CFBundleExecutable</key><string>Tg-Down</string>
  <key>CFBundleIconFile</key><string>app</string>
  <key>CFBundleShortVersionString</key><string>${VERSION#v}</string>
  <key>CFBundleVersion</key><string>${VERSION#v}</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <key>NSHighResolutionCapable</key><true/>
</dict></plist>
PLIST

    # dmg：未签名版本；用户首次打开需右键 → 打开（Gatekeeper）。
    # 注意：macOS 26 起 hdiutil create 的 UDZO(zlib) 格式报 Function not implemented，
    # 改用全系统可用的 UDBZ；CI 虚拟机 runner 上 hdiutil 本身偶发失败
    # （actions/runner-images#12323、electron-builder#9155），保留重试兜底，
    # 每次重试前清理可能残留的挂载点，避免 resource busy。
    #
    # 内容目录里除 .app 外再放一个 /Applications 软链接作为拖拽目标。
    STAGE="$DIST/dmg-stage"
    rm -rf "$STAGE"; mkdir -p "$STAGE"
    cp -R "$DIST/$APP" "$STAGE/$APP"
    ln -s /Applications "$STAGE/Applications"

    dmg_retry() {
      local log="$DIST/hdiutil-retry.log"
      : >"$log"
      local attempt
      for attempt in 1 2 3 4 5; do
        hdiutil detach "/Volumes/Tg-Down" -force >/dev/null 2>&1 || true
        if hdiutil create -volname "Tg-Down" -srcfolder "$STAGE" -ov -format UDBZ "$DIST/$NAME.dmg" >>"$log" 2>&1; then
          return 0
        fi
        sleep "$attempt"
      done
      cat "$log" >&2
      echo "hdiutil create 重试 5 次仍失败" >&2
      return 1
    }
    dmg_retry
    echo ">>> $DIST/$NAME.dmg"
    ;;

  windows)
    rm -rf "$DIST/pkg"; mkdir -p "$DIST/pkg"
    cp "$DIST/tg-down-desktop.exe" "$DIST/pkg/tg-down-desktop.exe"
    # 无 "|| true"：随包分发的许可证与说明缺失属于打包错误，必须当场暴露
    cp LICENSE "$DIST/pkg/"
    cp README.md "$DIST/pkg/"
    # 两条分支产出同一种布局（归档根目录即文件本身，无 pkg/ 前缀）
    (cd "$DIST/pkg" && powershell.exe -NoProfile -Command "Compress-Archive -Path * -DestinationPath ..\\\\$NAME.zip -Force") \
      || (cd "$DIST/pkg" && zip -qr "../$NAME.zip" .)
    echo ">>> $DIST/$NAME.zip"

    # NSIS 安装包。makensis 是原生 Windows 程序，不认 MSYS2 的 POSIX 路径（/d/a/...），
    # 因此脚本内引用的路径一律先经 cygpath 转成 Windows 形态。
    #
    # 缺 makensis 时默认失败而非跳过：发布流水线的上传清单与发布说明都列着 setup.exe，
    # 静默跳过会让缺件一路走到发布页。本机调试可用 SKIP_NSIS=1 显式跳过。
    if [ "${SKIP_NSIS:-}" = "1" ]; then
      echo ">>> SKIP_NSIS=1，跳过 setup.exe"
    elif command -v makensis >/dev/null 2>&1; then
      win_path() {
        if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else echo "$1"; fi
      }
      NSI_EXE="$(win_path "$DIST/pkg/tg-down-desktop.exe")"
      NSI_LICENSE="$(win_path "$DIST/pkg/LICENSE")"
      # OutFile 按 makensis 的工作目录解析，必须给绝对路径，否则产物落在仓库根目录
      NSI_OUT="$(win_path "$DIST/$NAME-setup.exe")"
      cat > "$DIST/installer.nsi" <<NSI
Unicode true
Name "Tg-Down"
OutFile "$NSI_OUT"
InstallDir "\$PROGRAMFILES64\\Tg-Down"
InstallDirRegKey HKLM "Software\\Tg-Down" "InstallDir"
; 安装到 Program Files 需要管理员权限，否则 WriteRegStr/File 会静默失败
RequestExecutionLevel admin
ShowInstDetails show

!include "MUI2.nsh"
!define MUI_ABORTWARNING
!insertmacro MUI_PAGE_LICENSE "$NSI_LICENSE"
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "SimpChinese"
!insertmacro MUI_LANGUAGE "English"

Section "Tg-Down" SecMain
  SetOutPath "\$INSTDIR"
  File "$NSI_EXE"
  File "$NSI_LICENSE"
  WriteUninstaller "\$INSTDIR\\uninstall.exe"

  CreateDirectory "\$SMPROGRAMS\\Tg-Down"
  CreateShortcut "\$SMPROGRAMS\\Tg-Down\\Tg-Down.lnk" "\$INSTDIR\\tg-down-desktop.exe"
  CreateShortcut "\$SMPROGRAMS\\Tg-Down\\卸载 Tg-Down.lnk" "\$INSTDIR\\uninstall.exe"

  WriteRegStr HKLM "Software\\Tg-Down" "InstallDir" "\$INSTDIR"
  WriteRegStr HKLM "Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\Tg-Down" "DisplayName" "Tg-Down"
  WriteRegStr HKLM "Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\Tg-Down" "DisplayVersion" "${VERSION#v}"
  WriteRegStr HKLM "Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\Tg-Down" "Publisher" "Tg-Down contributors"
  WriteRegStr HKLM "Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\Tg-Down" "DisplayIcon" "\$INSTDIR\\tg-down-desktop.exe"
  WriteRegStr HKLM "Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\Tg-Down" "UninstallString" "\$INSTDIR\\uninstall.exe"
  WriteRegDWORD HKLM "Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\Tg-Down" "NoModify" 1
  WriteRegDWORD HKLM "Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\Tg-Down" "NoRepair" 1
SectionEnd

; WebView2 引导：Win11 与打过补丁的 Win10 已自带 Evergreen Runtime，仅在缺失时下载官方引导器。
; 检测键在 32/64 位视图下各有一份，两处都查不到才认定缺失。
Section "WebView2 运行时" SecWebView2
  SetRegView 64
  ReadRegStr \$0 HKLM "SOFTWARE\\WOW6432Node\\Microsoft\\EdgeUpdate\\Clients\\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
  StrCmp \$0 "" 0 wv2_done
  ReadRegStr \$0 HKCU "Software\\Microsoft\\EdgeUpdate\\Clients\\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
  StrCmp \$0 "" 0 wv2_done
  DetailPrint "正在安装 WebView2 运行时 ..."
  NSISdl::download "https://go.microsoft.com/fwlink/p/?LinkId=2124703" "\$TEMP\\MicrosoftEdgeWebview2Setup.exe"
  Pop \$1
  StrCmp \$1 "success" 0 wv2_fail
  ExecWait '"\$TEMP\\MicrosoftEdgeWebview2Setup.exe" /silent /install'
  Delete "\$TEMP\\MicrosoftEdgeWebview2Setup.exe"
  Goto wv2_done
  wv2_fail:
    DetailPrint "WebView2 下载失败（\$1）；若界面无法显示，请手动安装 WebView2 运行时。"
  wv2_done:
SectionEnd

Section "un.卸载"
  Delete "\$INSTDIR\\tg-down-desktop.exe"
  Delete "\$INSTDIR\\LICENSE"
  Delete "\$INSTDIR\\uninstall.exe"
  RmDir "\$INSTDIR"
  Delete "\$SMPROGRAMS\\Tg-Down\\Tg-Down.lnk"
  Delete "\$SMPROGRAMS\\Tg-Down\\卸载 Tg-Down.lnk"
  RmDir "\$SMPROGRAMS\\Tg-Down"
  DeleteRegKey HKLM "Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\Tg-Down"
  DeleteRegKey HKLM "Software\\Tg-Down"
SectionEnd
NSI
      makensis -V2 "$DIST/installer.nsi"
      test -f "$DIST/$NAME-setup.exe" || { echo "makensis 未产出 $NAME-setup.exe" >&2; exit 1; }
      echo ">>> $DIST/$NAME-setup.exe"
    else
      echo "makensis 不在 PATH，无法生成 setup.exe" >&2
      exit 1
    fi
    ;;

  linux)
    rm -rf "$DIST/pkg"; mkdir -p "$DIST/pkg/usr/local/bin" "$DIST/pkg/opt/tg-down" \
      "$DIST/pkg/usr/share/applications"
    install -m 755 "$DIST/tg-down-desktop" "$DIST/pkg/usr/local/bin/tg-down-desktop"
    # 无 "|| true"：许可证缺失属于打包错误
    cp LICENSE "$DIST/pkg/opt/tg-down/"
    install -m 644 "$ROOT/internal/desktop/assets/tray.png" "$DIST/pkg/opt/tg-down/appicon.png"

    # .desktop 必须在打 tar 之前写好，否则便携包里没有桌面入口。
    # Icon 指向随包安装的 appicon.png（此前只打包了图标却没有 Icon= 行，图标是孤儿文件）。
    cat > "$DIST/pkg/usr/share/applications/tg-down-desktop.desktop" <<'DESKTOP'
[Desktop Entry]
Type=Application
Name=Tg-Down
Comment=Telegram media downloader
Exec=tg-down-desktop
Icon=/opt/tg-down/appicon.png
Terminal=false
Categories=Network;FileTransfer;
DESKTOP

    tar -czf "$DIST/$NAME.tar.gz" -C "$DIST/pkg" .
    echo ">>> $DIST/$NAME.tar.gz"

    if command -v nfpm >/dev/null 2>&1; then
      cat > "$DIST/nfpm.yaml" <<YAML
name: tg-down-desktop
arch: $ARCH
platform: linux
version: ${VERSION#v}
section: net
priority: optional
maintainer: "Tg-Down contributors"
description: |
  Telegram media downloader desktop client.
license: MIT
contents:
  - src: $DIST/pkg/usr/local/bin/tg-down-desktop
    dst: /usr/local/bin/tg-down-desktop
  - src: $DIST/pkg/opt/tg-down/appicon.png
    dst: /opt/tg-down/appicon.png
  - src: $DIST/pkg/opt/tg-down/LICENSE
    dst: /opt/tg-down/LICENSE
  - src: $DIST/pkg/usr/share/applications/tg-down-desktop.desktop
    dst: /usr/share/applications/tg-down-desktop.desktop
# deb 与 rpm 的包名词汇不同，必须分开声明：此前两者共用 Debian 名，
# 产出的 rpm 依赖在 Fedora/openSUSE 上不可满足，装不上。
# libayatana-appindicator 是托盘（StatusNotifierItem）的运行时依赖，缺它托盘不出现。
overrides:
  deb:
    depends:
      - libgtk-3-0
      - libwebkit2gtk-4.1-0
      - libayatana-appindicator3-1
  rpm:
    depends:
      - gtk3
      - webkit2gtk4.1
      - libayatana-appindicator-gtk3
YAML
      nfpm package -f "$DIST/nfpm.yaml" -p deb -t "$DIST/$NAME.deb"
      nfpm package -f "$DIST/nfpm.yaml" -p rpm -t "$DIST/$NAME.rpm"
      echo ">>> $DIST/$NAME.deb / $DIST/$NAME.rpm"
    else
      echo ">>> nfpm 不在 PATH，跳过 deb/rpm"
    fi
    ;;

  *)
    echo "未知平台: $OS" >&2; exit 1 ;;
esac
