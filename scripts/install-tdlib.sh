#!/usr/bin/env bash
# Build & install TDLib (libtdjson) at the commit pinned by zelenin/go-tdlib master (0dd3ea6, 2026-05-09).
# Installs to a writable custom prefix (no sudo). Used by local dev and CI.
#
#   TDLIB_PREFIX  install prefix            (default: $HOME/.tdlib)
#   TDLIB_COMMIT  tdlib/td commit to build  (default: pinned below)
#   TDLIB_SRC     source checkout dir       (default: $HOME/.cache/tdlib-src)
#   JOBS          parallel build jobs       (default: 4; raise if you have >16GB RAM)
#
# After install, build the Go project with:
#   export CGO_CFLAGS="-I$TDLIB_PREFIX/include"
#   export CGO_LDFLAGS="-Wl,-rpath,$TDLIB_PREFIX/lib -L$TDLIB_PREFIX/lib -ltdjson"
set -euo pipefail

PREFIX="${TDLIB_PREFIX:-$HOME/.tdlib}"
# Pinned to match github.com/zelenin/go-tdlib master 0dd3ea6 (TDLib 2026-05-08).
TDLIB_COMMIT="${TDLIB_COMMIT:-49b3bcbb6bfebf2ed44dd9f25102d2e1a94a58c4}"
SRC="${TDLIB_SRC:-$HOME/.cache/tdlib-src}"
JOBS="${JOBS:-4}"

echo ">>> TDLib build: prefix=$PREFIX commit=${TDLIB_COMMIT:0:10} jobs=$JOBS"

OPENSSL_ROOT=""
CMAKE_GENERATOR_ARGS=()
case "$(uname -s)" in
  Darwin)
    if command -v brew >/dev/null 2>&1; then
      # No '|| true': a failed dep install must surface here, not as a confusing cmake error later.
      brew install gperf cmake openssl@3 >/dev/null
      OPENSSL_ROOT="$(brew --prefix openssl@3)"
    fi
    ;;
  Linux)
    if command -v apt-get >/dev/null 2>&1; then
      # 容器/root 环境无 sudo，直接调用 apt-get
      SUDO="sudo"
      if [ "$(id -u)" = "0" ] || ! command -v sudo >/dev/null 2>&1; then
        SUDO=""
      fi
      $SUDO apt-get update -y || true # transient mirror failures are non-fatal; the install below still gates
      $SUDO apt-get install -y make git zlib1g-dev libssl-dev gperf cmake g++
    fi
    ;;
  MINGW* | MSYS*)
    # Windows：必须走 MSYS2 + MinGW-w64。CGo 只认 gcc 系工具链，MSVC 这条路走不通。
    if command -v pacman >/dev/null 2>&1; then
      pacman -S --needed --noconfirm \
        git make \
        mingw-w64-x86_64-toolchain \
        mingw-w64-x86_64-cmake \
        mingw-w64-x86_64-gperf \
        mingw-w64-x86_64-openssl \
        mingw-w64-x86_64-zlib
    else
      echo "!!! 未找到 pacman：Windows 上请在 MSYS2 MINGW64 shell 里运行本脚本" >&2
      exit 1
    fi
    # MSYS2 默认挑 "MSYS Makefiles" 生成器，产出的是 MSYS(POSIX 模拟) 二进制，
    # CGo 的 mingw gcc 链不上；必须强制 MinGW 生成器。
    CMAKE_GENERATOR_ARGS=(-G "MinGW Makefiles")
    OPENSSL_ROOT="${MINGW_PREFIX:-/mingw64}"
    ;;
  *)
    # 此前这里没有 default 分支：不认识的系统会静默跳过依赖安装，
    # 然后在 cmake 阶段抛出一个与真正原因毫无关系的错误。
    echo "!!! 未知系统 $(uname -s)：请自行安装 cmake / gperf / openssl / zlib / g++ 后重试" >&2
    ;;
esac

mkdir -p "$SRC"
if [ ! -d "$SRC/.git" ]; then
  git clone https://github.com/tdlib/td.git "$SRC"
fi
cd "$SRC"
git fetch origin "$TDLIB_COMMIT" 2>/dev/null || git fetch --all --tags || true
git checkout -f "$TDLIB_COMMIT"

case "$(uname -s)" in
  MINGW* | MSYS*)
    # MinGW 下的 tl-parser 编译缺陷：tlc.c 在 _WIN32 下不包含 <unistd.h>，改经
    # tl-parser.h → wgetopt.h 拿 getopt 声明；而 wgetopt.h 只认 glibc 的
    # __GNU_LIBRARY__，MinGW 未定义它，落到旧式无参原型 `extern int getopt ();`，
    # tlc.c:115 的三参调用直接报 "too many arguments to function 'getopt'"。
    # MinGW 同样提供带完整原型的 <unistd.h>（两种声明在 C 里兼容），补上包含即可。
    sed -i 's/^#ifndef _WIN32$/#if !defined(_WIN32) || defined(__MINGW32__)/' \
      td/generate/tl-parser/tlc.c
    ;;
esac

rm -rf build
mkdir build
cd build
# ${arr[@]+"${arr[@]}"}：set -u 下空数组展开为零个词（而不是一个空字符串参数）
cmake ${CMAKE_GENERATOR_ARGS[@]+"${CMAKE_GENERATOR_ARGS[@]}"} \
  -DCMAKE_BUILD_TYPE=Release \
  ${OPENSSL_ROOT:+-DOPENSSL_ROOT_DIR="$OPENSSL_ROOT"} \
  -DCMAKE_INSTALL_PREFIX="$PREFIX" ..
cmake --build . --target install -j"$JOBS"

echo ">>> TDLib installed to $PREFIX"
ls -la "$PREFIX/lib/"libtdjson* 2>/dev/null || true

# PE 没有 rpath 的概念，MinGW 上带 -Wl,-rpath 只会让链接器报警
RPATH_HINT=" -Wl,-rpath,$PREFIX/lib"
case "$(uname -s)" in
  MINGW* | MSYS* | CYGWIN*) RPATH_HINT="" ;;
esac

cat <<EOF

Done. Build the project with (statically linked against TDLib):
  export CGO_CFLAGS="-I$PREFIX/include${OPENSSL_ROOT:+ -I$OPENSSL_ROOT/include}"
  export CGO_LDFLAGS="-L$PREFIX/lib${RPATH_HINT}${OPENSSL_ROOT:+ -L$OPENSSL_ROOT/lib}"
  go build ./...

Or simply: make build   (the Makefile sets these automatically)
EOF
