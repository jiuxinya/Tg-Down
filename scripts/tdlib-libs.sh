#!/usr/bin/env bash
# TDLib 静态库链接列表的唯一来源。
#
# 此前这份列表在四个地方各抄了一遍（Makefile、setup-tdlib action、release 工作流、Dockerfile）。
# TDLib 升级后新增/更名一个内部库时，漏改任何一处都会在链接阶段炸出一堆
# "undefined symbol"，而且四处的报错各不相同。
#
# 用法：
#   eval "$(bash scripts/tdlib-libs.sh)"     # 导出 TDLIB_STATIC_LIBS / TDLIB_SYS_LIBS
#   bash scripts/tdlib-libs.sh --print       # 只打印链接串
#
# 顺序有意义：链接器按从左到右解析符号，被依赖的库必须排在依赖它的库之后。

set -euo pipefail

# TDLib 自身的静态库（顺序即依赖顺序）
TDLIB_LIBS="-ltdjson_static -ltdjson_private -ltdclient -ltdcore -ltde2e -ltdmtproto -ltdactor -ltdapi -ltddb -ltdsqlite -ltdnet -ltdutils"

# 平台相关的系统库。
# Windows(MinGW) 没有 libdl，且需要 winsock/crypt32/加密相关的系统库；
# 其余平台用 -ldl。
case "$(uname -s)" in
  MINGW* | MSYS* | CYGWIN*)
    # -lpsapi：TDLib 的 Stat.cpp 用 GetProcessMemoryInfo 做内存统计（MinGW 下
    # 该符号在 psapi 库，不在默认链接集里，缺了报 undefined reference）。
    SYS_LIBS="-lstdc++ -lssl -lcrypto -lz -lm -lws2_32 -lcrypt32 -lgdi32 -ladvapi32 -luser32 -lbcrypt -lpsapi"
    ;;
  *)
    SYS_LIBS="-lstdc++ -lssl -lcrypto -ldl -lz -lm"
    ;;
esac

TDLIB_STATIC_LIBS="$TDLIB_LIBS $SYS_LIBS"

if [ "${1:-}" = "--print" ]; then
  printf '%s\n' "$TDLIB_STATIC_LIBS"
else
  printf 'export TDLIB_STATIC_LIBS="%s"\n' "$TDLIB_STATIC_LIBS"
fi
