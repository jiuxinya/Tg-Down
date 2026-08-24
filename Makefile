# Makefile for Telegram Media Downloader (TDLib engine)

.PHONY: all build build-desktop package-desktop run test test-fast test-integration clean deps help install config tdlib dev web web-dev

# TDLib 安装前缀（scripts/install-tdlib.sh 默认装到这里）
TDLIB_PREFIX ?= $(HOME)/.tdlib
# OpenSSL 前缀（macOS Homebrew: openssl@3；Linux 通常留空）
OPENSSL_PREFIX ?= $(shell command -v brew >/dev/null 2>&1 && brew --prefix openssl@3 2>/dev/null)

# TDLib 静态库列表来自 scripts/tdlib-libs.sh（唯一来源，Makefile/CI/Dockerfile 共用）
TDLIB_STATIC_LIBS := $(shell bash scripts/tdlib-libs.sh --print)
UNAME_S := $(shell uname -s)

IS_WINDOWS := $(filter MINGW% MSYS% CYGWIN%,$(UNAME_S))

# Windows(MinGW) 的 PE 格式没有 rpath 概念，加了会直接链接失败
RPATH_FLAG = $(if $(IS_WINDOWS),,-Wl,-rpath,$(TDLIB_PREFIX)/lib)

# go-tdlib 的 linux 链接列表缺 tde2e，darwin/windows 则没有链接指令。
# 三个平台都追加完整列表；linux 上与上游重复的 -l 项无害，且保证静态库依赖顺序正确。

# Windows 上 go build -o 不会自动补 .exe
BIN := tg-down$(if $(IS_WINDOWS),.exe,)

export CGO_ENABLED = 1
export CGO_CFLAGS = -I$(TDLIB_PREFIX)/include $(if $(OPENSSL_PREFIX),-I$(OPENSSL_PREFIX)/include,)
export CGO_LDFLAGS = -L$(TDLIB_PREFIX)/lib $(RPATH_FLAG) $(if $(OPENSSL_PREFIX),-L$(OPENSSL_PREFIX)/lib,) $(TDLIB_STATIC_LIBS)

# 默认目标
all: build

# 构建 TDLib（首次需要，约 30-60 分钟）
tdlib:
	@echo "正在构建 TDLib 到 $(TDLIB_PREFIX) ..."
	@bash scripts/install-tdlib.sh

# 版本号：优先取 git describe，无仓库时为 dev
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# 构建前端（Vite + Preact），产物落在 internal/web/static/dist 并由 go:embed 打进二进制
web:
	@echo "正在构建前端 ..."
	@cd web && npm ci --no-audit --no-fund && npm run build

# 前端开发服务器（热更新，API 代理到本地 8080 的 Go 服务）
web-dev:
	@cd web && npm run dev

# 构建程序（需先 make tdlib 安装 libtdjson）；先出前端产物再编译，否则 embed 进去的是空目录
build: web
	@echo "正在编译Telegram媒体下载器 (CGo + TDLib, $(VERSION))..."
	@go build -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd
	@echo "编译完成！"

# 桌面客户端（Wails 壳 + 内嵌引擎）。macOS 需补 UniformTypeIdentifiers 框架（拖放 API 的链接依赖）。
# 必须带 desktop,production 标签，否则 wails.Run 运行时报 "will not build without the correct build tags"。
DESKTOP_BIN := tg-down-desktop$(if $(IS_WINDOWS),.exe,)
DESKTOP_TAGS := desktop,production

build-desktop: web
	@echo "正在编译桌面客户端 (CGo + TDLib + Wails, $(VERSION))..."
	@CGO_LDFLAGS="$(CGO_LDFLAGS)$(if $(filter Darwin,$(UNAME_S)), -framework UniformTypeIdentifiers,)" \
		go build -tags "$(DESKTOP_TAGS)" \
		-ldflags "-s -w -X main.version=$(VERSION)" \
		-o $(DESKTOP_BIN) ./cmd/desktop
	@echo "桌面客户端编译完成：$(DESKTOP_BIN)"

# 打包桌面发布产物（dmg / zip+NSIS / tar.gz+deb+rpm），见 scripts/package-desktop.sh
package-desktop: build-desktop
	@mkdir -p dist/desktop && cp $(DESKTOP_BIN) dist/desktop/
	@bash scripts/package-desktop.sh \
		$(shell go env GOOS) $(shell go env GOARCH) "$(VERSION)"

# 运行程序
run: build
	@echo "正在运行程序..."
	@./$(BIN)

# 运行全部测试（继承上方导出的 CGO 环境；裸 go test 因缺少 TDLib 头文件会编译失败）
test:
	@go test -race -cover ./...

# 快速单测车道：除 cmd 与 internal/telegram 外的所有包都不碰 CGo/TDLib，几秒出结果，
# 不必先花 30-60 分钟编 TDLib。
#
# 包清单用 go list 求补集而不是手写：新加的包自动进车道；而一旦某个包意外 import 了
# internal/telegram（把 CGo 依赖传染出去），这条车道会立刻编译失败——正是要守的边界。
# cmd/desktop 同样被排除：它经 Wails 引擎链 import internal/telegram，属合法例外。
FAST_PKGS = $(shell go list ./... | grep -v -e '/cmd$$' -e '/cmd/desktop$$' -e '/internal/telegram$$')

test-fast: export CGO_CFLAGS =
test-fast: export CGO_LDFLAGS =
test-fast:
	@go test -race -cover $(FAST_PKGS)

# 真账号回归测试：需要 config.yaml 里的真实凭据与一个测试频道，见 internal/telegram/integration_test.go
test-integration:
	@go test -tags integration -count=1 -v -timeout 30m ./internal/telegram/...

# 下载依赖
deps:
	@echo "正在下载依赖..."
	@go mod download
	@go mod tidy

# 清理构建文件
clean:
	@echo "正在清理构建文件..."
	@rm -f tg-down tg-down.exe tg-down-desktop tg-down-desktop.exe desktop
	@rm -rf dist
	@echo "清理完成！"

# 安装到系统路径
install: build
	@echo "正在安装到系统路径..."
	@sudo cp tg-down /usr/local/bin/
	@echo "安装完成！可以在任何位置使用 'tg-down' 命令"

# 创建配置文件
config:
	@if [ ! -f config.yaml ]; then \
		echo "创建配置文件..."; \
		install -m 600 config.yaml.example config.yaml; \
		echo "请编辑 config.yaml 文件设置您的API信息"; \
	else \
		echo "配置文件已存在"; \
	fi

# 显示帮助信息
help:
	@echo "可用的命令:"
	@echo "  make tdlib     - 构建并安装 TDLib (首次必需)"
	@echo "  make build     - 编译程序"
	@echo "  make run       - 编译并运行程序"
	@echo "  make test      - 运行全部测试（需先 make tdlib）"
	@echo "  make test-fast - 快速单测（不需要 TDLib）"
	@echo "  make test-integration - 真实账号集成测试（需 TG_DOWN_IT_* 环境变量）"
	@echo "  make web       - 构建前端（Vite）"
	@echo "  make web-dev   - 前端开发服务器（热更新）"
	@echo "  make build-desktop   - 编译桌面客户端（Wails）"
	@echo "  make package-desktop - 打包桌面发布产物"
	@echo "  make deps    - 下载依赖"
	@echo "  make clean   - 清理构建文件"
	@echo "  make install - 安装到系统路径"
	@echo "  make config  - 创建配置文件"
	@echo "  make help    - 显示此帮助信息"

# 开发模式 - 监听文件变化并自动重新编译
dev:
	@echo "开发模式 - 监听文件变化..."
	@if command -v air > /dev/null; then \
		air; \
	else \
		echo "请安装 air 工具: go install github.com/air-verse/air@latest"; \
	fi
