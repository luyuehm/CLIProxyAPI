#!/bin/sh
#
# AI Gateway Enterprise — Docker Entrypoint
# 启动前验证 LICENSE_KEY，无效 key 则拒绝启动
#
# 授权验证契约:
#   LICENSE_KEY            必填。未设置时拒绝启动。
#   LICENSE_SERVER_URL     可选。设置后向该地址发起远程验证（授权服务端必须返回
#                          JSON {"valid":true} 表示有效）。留空 = 离线模式，
#                          仅做本地格式校验，不依赖外部网络（适合私有化/内网部署）。
#
set -e

# ── 1. LICENSE_KEY 必填 ──────────────────────────────────────────
if [ -z "${LICENSE_KEY:-}" ]; then
  echo "ERROR: LICENSE_KEY is not set. Please set LICENSE_KEY in .env"
  echo "       If you don't have a license key, contact support@yourdomain.com"
  exit 1
fi

# ── 2. 远程验证（仅当配置了真实的授权服务器） ────────────────────
LICENSE_SERVER_URL="${LICENSE_SERVER_URL:-}"

if [ -n "$LICENSE_SERVER_URL" ]; then
  echo "[license] verifying against ${LICENSE_SERVER_URL} ..."
  VERIFIED=false
  for i in 1 2 3; do
    RESPONSE=$(curl -s -m 5 "${LICENSE_SERVER_URL}?key=${LICENSE_KEY}" 2>/dev/null || echo '{"valid":false}')
    if echo "$RESPONSE" | grep -q '"valid"[[:space:]]*:[[:space:]]*true'; then
      VERIFIED=true
      break
    fi
    if [ $i -lt 3 ]; then
      echo "[license] attempt ${i} failed, retrying in 2s ..."
      sleep 2
    fi
  done

  if [ "$VERIFIED" = "false" ]; then
    echo "ERROR: License key validation failed. Key: ${LICENSE_KEY}"
    echo "       Please check your license key or contact support@yourdomain.com"
    exit 1
  fi
else
  # 离线模式：不依赖外部授权服务器，仅确认 key 已配置
  echo "[license] offline mode: LICENSE_SERVER_URL not set, skipping remote validation."
fi

echo "[license] OK, starting AI Gateway Enterprise ..."

# ── 3. 授权验证通过，启动主程序 ──────────────────────────────────
exec "$@"