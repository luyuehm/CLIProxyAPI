#!/bin/bash
#
# AI Gateway Enterprise — 健康检查脚本
# 用法: bash scripts/healthcheck.sh
# 输出: 健康时返回 0，输出 HEALTHY；异常时返回 1，输出 UNHEALTHY: 详情
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# 加载配置
if [ -f ../.env ]; then
  source ../.env
fi

CPA_PORT="${CPA_PORT:-8317}"
KEEPER_PORT="${KEEPER_PORT:-4320}"

CPA_OK=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:${CPA_PORT}/healthz" 2>/dev/null || echo "000")
KEEPER_OK=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:${KEEPER_PORT}/healthz" 2>/dev/null || echo "000")

if [ "$CPA_OK" = "200" ] && [ "$KEEPER_OK" = "200" ]; then
  echo "HEALTHY"
  exit 0
else
  echo "UNHEALTHY: CPA=HTTP_${CPA_OK}, KEEPER=HTTP_${KEEPER_OK}"
  exit 1
fi