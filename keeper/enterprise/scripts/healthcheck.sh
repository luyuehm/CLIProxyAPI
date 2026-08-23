#!/bin/bash
#
# CPA Usage Keeper Enterprise — 健康检查脚本
# 用法: bash scripts/healthcheck.sh
# 输出: 健康时返回 0，输出 HEALTHY；异常时返回 1，输出 UNHEALTHY: 详情
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/.."

# 加载配置
if [ -f .env ]; then
  source .env
fi

APP_PORT="${APP_PORT:-4320}"

# 检查 Keeper 服务健康
KEEPER_OK=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:${APP_PORT}/healthz" 2>/dev/null || echo "000")

# 检查 Login 页面可达
LOGIN_OK=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:${APP_PORT}/login" 2>/dev/null || echo "000")

# 检查数据同步状态（API 接口）
SYNC_OK=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:${APP_PORT}/api/health" 2>/dev/null || echo "000")

if [ "$KEEPER_OK" = "200" ] && [ "$LOGIN_OK" = "200" ]; then
  echo "HEALTHY"
  exit 0
else
  echo "UNHEALTHY: healthz=HTTP_${KEEPER_OK}, login=HTTP_${LOGIN_OK}, api=HTTP_${SYNC_OK}"
  exit 1
fi