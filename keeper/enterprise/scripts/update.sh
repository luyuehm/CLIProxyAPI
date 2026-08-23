#!/bin/bash
#
# CPA Usage Keeper Enterprise — 更新脚本
# 用法: bash scripts/update.sh
# 功能: 拉取最新镜像、重新构建并重启服务
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/.."

if [ ! -f ../docker-compose.yml ]; then
  echo "[ERROR] docker-compose.yml 不存在，请确保在 enterprise/ 目录下运行"
  exit 1
fi

echo "[INFO] 拉取最新构建..."
cd ..

# 重新构建镜像（确保使用最新代码）
docker compose build --pull

echo "[INFO] 重启服务..."
docker compose up -d --remove-orphans

echo "[INFO] 等待服务启动..."
sleep 10

# 健康检查
bash enterprise/scripts/healthcheck.sh
if [ $? -eq 0 ]; then
  echo "[INFO] 更新完成，服务运行正常 ✅"
else
  echo "[WARN] 部分服务可能未正常启动，请查看日志: docker compose logs"
fi