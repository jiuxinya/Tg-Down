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
    for spec in "16 16" "32 32" "64 64x32@2x" "128 128" "256 256" "512 512" "1024 512x2x"; do :; done
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

    # dmg：未签名版本；用户首次打开需右键 → 打开（Gatekeeper）
    hdiutil create -volname "Tg-Down" -srcfolder "$DIST/$APP" -ov -format udzo "$DIST/$NAME.dmg" >/dev/null
    echo ">>> $DIST/$NAME.dmg"
    ;;

  windows)
    rm -rf "$DIST/pkg"; mkdir -p "$DIST/pkg"
    cp "$DIST/tg-down-desktop.exe" "$DIST/pkg/tg-down-desktop.exe"
    cp LICENSE "$DIST/pkg/" 2>/dev/null || true
    cp README.md "$DIST/pkg/" 2>/dev/null || true
    (cd "$DIST/pkg" && powershell.exe -NoProfile -Command "Compress-Archive -Path * -DestinationPath ..\\\\$NAME.zip -Force") \
      || (cd "$DIST" && zip -q "$NAME.zip" pkg/*)
    echo ">>> $DIST/$NAME.zip"
    # NSIS 安装包仅在提供 makensis 时生成（可选）
    if command -v makensis >/dev/null 2>&1; then
      cat > "$DIST/installer.nsi" <<NSI
OutFile "$NAME-setup.exe"
InstallDir "\$PROGRAMFILES64\\Tg-Down"
Name "Tg-Down"
Section
  SetOutPath \$INSTDIR
  File "$DIST/pkg/tg-down-desktop.exe"
  WriteUninstaller "\$INSTDIR\\uninstall.exe"
SectionEnd
Section "uninstall"
  Delete "\$INSTDIR\\tg-down-desktop.exe"
  Delete "\$INSTDIR\\uninstall.exe"
  RmDir "\$INSTDIR"
SectionEnd
NSI
      makensis -V2 "$DIST/installer.nsi"
      echo ">>> $DIST/$NAME-setup.exe"
    fi
    ;;

  linux)
    rm -rf "$DIST/pkg"; mkdir -p "$DIST/pkg/usr/local/bin" "$DIST/pkg/opt/tg-down"
    install -m 755 "$DIST/tg-down-desktop" "$DIST/pkg/usr/local/bin/tg-down-desktop"
    cp LICENSE "$DIST/pkg/opt/tg-down/" 2>/dev/null || true
    tar -czf "$DIST/$NAME.tar.gz" -C "$DIST/pkg" .
    echo ">>> $DIST/$NAME.tar.gz"

    if command -v nfpm >/dev/null 2>&1; then
      # .desktop 先落盘再经 src/dst 打包：nfpm 的 files 段不支持 content 内联字段
      mkdir -p "$DIST/pkg/usr/share/applications"
      cat > "$DIST/pkg/usr/share/applications/tg-down-desktop.desktop" <<'DESKTOP'
[Desktop Entry]
Type=Application
Name=Tg-Down
Exec=tg-down-desktop
Terminal=false
Categories=Network;
DESKTOP
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
  - src: $ROOT/internal/desktop/assets/tray.png
    dst: /opt/tg-down/appicon.png
  - src: $DIST/pkg/usr/share/applications/tg-down-desktop.desktop
    dst: /usr/share/applications/tg-down-desktop.desktop
depends:
  - libgtk-3-0
  - libwebkit2gtk-4.1-0
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
