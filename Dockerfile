# syntax=docker/dockerfile:1
# 多阶段构建：webbuild 出前端产物，build 编译 TDLib 与 Go 二进制，runtime 仅带运行时依赖。
# TDLib 层只依赖 scripts/install-tdlib.sh，源码变更不会触发耗时的 TDLib 重建。

# 前端层：只依赖 web/，改 Go 代码不会触发前端重建
FROM node:22-bookworm-slim AS webbuild
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.25.12-bookworm AS build

RUN apt-get update && apt-get install -y --no-install-recommends \
      cmake gperf g++ make git zlib1g-dev libssl-dev \
    && rm -rf /var/lib/apt/lists/*

# TDLib 层（可缓存，约 30-60 分钟）
COPY scripts/install-tdlib.sh /tmp/install-tdlib.sh
RUN TDLIB_PREFIX=/opt/tdlib TDLIB_SRC=/tmp/tdlib-src JOBS=$(nproc) \
      bash /tmp/install-tdlib.sh \
    && rm -rf /tmp/tdlib-src

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# 前端产物（webbuild 阶段的 vite outDir 是 ../internal/web/static/dist，即 /internal/...）
COPY --from=webbuild /internal/web/static/dist ./internal/web/static/dist
ARG VERSION=dev
# linux 下 go-tdlib 绑定默认静态链接（tdjson_static），但其列表缺 tde2e 且 -L 指向
# /usr/local/lib，这里统一补全
ENV CGO_ENABLED=1 \
    CGO_CFLAGS="-I/opt/tdlib/include"
# 静态库列表取自 scripts/tdlib-libs.sh（唯一来源）
RUN CGO_LDFLAGS="-L/opt/tdlib/lib $(bash scripts/tdlib-libs.sh --print)" \
      go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/tg-down ./cmd

FROM debian:12-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      libssl3 zlib1g ca-certificates curl gosu tzdata \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/tg-down /usr/local/bin/tg-down
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# 运行目录位于持久卷：网页填写的 API 凭据会以 0600 写入 /data/config.yaml，容器重建后仍可读取。
# API_ID/API_HASH/PHONE 环境变量仍按既有优先级覆盖文件值。
WORKDIR /data
ENV STORE_PATH=/data/tg-down.db \
    SESSION_DIR=/sessions \
    DOWNLOAD_PATH=/downloads \
    PUID=1000 \
    PGID=1000

VOLUME ["/downloads", "/sessions", "/data"]
EXPOSE 8080

# 非回环监听强制 TG_DOWN_WEB_TOKEN（缺失时进程拒绝启动），健康检查带同一 token
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s \
  CMD curl -fsS "http://127.0.0.1:8080/api/state?token=${TG_DOWN_WEB_TOKEN}" || exit 1

ENTRYPOINT ["/entrypoint.sh"]
CMD ["--web", "0.0.0.0:8080"]
