#!/bin/bash
# 容器入口：按 PUID/PGID 修正数据目录属主后降权运行 tg-down。
# 标记文件记录上次完成迁移的 UID:GID；首次启动、旧版空标记或身份变化时递归修正一次。
set -euo pipefail

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"

if [[ ! "$PUID" =~ ^[0-9]+$ || ! "$PGID" =~ ^[0-9]+$ ]]; then
  echo "!!! PUID and PGID must be numeric" >&2
  exit 1
fi

if ! getent group "$PGID" >/dev/null 2>&1; then
  groupadd -g "$PGID" tgdown
fi
if ! getent passwd "$PUID" >/dev/null 2>&1; then
  useradd -u "$PUID" -g "$PGID" -M -s /usr/sbin/nologin tgdown
fi

for dir in /downloads /sessions /data; do
  if [ -L "$dir" ]; then
    echo "!!! $dir must not be a symbolic link" >&2
    exit 1
  fi
  mkdir -p "$dir"
  if [ "$(stat -c '%u:%g' "$dir")" != "$PUID:$PGID" ]; then
    chown -h "$PUID:$PGID" "$dir"
  fi
done

marker=/data/.ownership-initialized
owner="$PUID:$PGID"
previous_owner=""
if [ -f "$marker" ] && [ ! -L "$marker" ]; then
  IFS= read -r previous_owner < "$marker" || true
fi
if [ "$previous_owner" != "$owner" ]; then
  echo ">>> Migrating volume ownership to $owner (first run or PUID/PGID changed; large volumes may take time)"
  # -P 禁止遍历目录树里的符号链接，-h 只修改链接自身，不能借卷内链接改到卷外目标。
  # set -e 保证任一目录失败时不会更新 marker，下次启动会重新尝试。
  chown -hR -P -- "$owner" /downloads /sessions /data

  marker_tmp="$(mktemp /data/.ownership-initialized.tmp.XXXXXX)"
  trap 'rm -f -- "${marker_tmp:-}"' EXIT
  printf '%s\n' "$owner" > "$marker_tmp"
  chmod 600 "$marker_tmp"
  chown -h "$owner" "$marker_tmp"
  mv -f -- "$marker_tmp" "$marker"
  marker_tmp=""
  trap - EXIT
fi

if [ -L /data/config.yaml ]; then
  echo "!!! /data/config.yaml must not be a symbolic link" >&2
  exit 1
fi
if [ -f /data/config.yaml ]; then
  chmod 600 /data/config.yaml
  chown -h "$owner" /data/config.yaml
fi

exec gosu "$PUID:$PGID" tg-down "$@"
