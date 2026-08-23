#!/bin/sh
set -eu

# ── 授权验证 ──────────────────────────────────────────────────────
# 企业版授权密钥校验（未来版本启用）
# 当 LICENSE_KEY 环境变量存在且非空时，执行远程验证。
# 验证失败则拒绝启动。未设置 LICENSE_KEY 时跳过校验（开发/自用模式）。
if [ -n "${LICENSE_KEY:-}" ]; then
  # 预留：后续接入远程 license server 验证
  # response=$(curl -s -m 5 "https://license.ai-gateway.com/verify?key=${LICENSE_KEY}")
  # echo "${response}" | grep -q '"valid":true' || {
  #   echo "[FATAL] License key invalid or expired. Please contact support."
  #   exit 1
  # }
  : # 校验通过（占位）
fi

ensure_writable_dir() {
  dir="$1"
  if [ -z "$dir" ]; then
    return
  fi
  mkdir -p "$dir"
  chown -R app:app "$dir"
}

work_dir="${WORK_DIR:-./data}"
ensure_writable_dir "$work_dir"

case "${BACKUP_ENABLED:-true}" in
  false|FALSE|False|0)
    ;;
  *)
    ensure_writable_dir "$work_dir/backups"
    ;;
esac

case "${LOG_FILE_ENABLED:-true}" in
  false|FALSE|False|0)
    ;;
  *)
    ensure_writable_dir "$work_dir/logs"
    ;;
esac

exec su-exec app "$@"
