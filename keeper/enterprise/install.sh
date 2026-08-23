#!/bin/bash
#
# CPA Usage Keeper Enterprise — 一键安装脚本
# 用法: bash install.sh
#
set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# ── 环境检测 ──────────────────────────────────────────────────────
log_info "检测运行环境..."

if ! command -v docker &>/dev/null; then
  log_error "Docker 未安装。请先安装 Docker: https://docs.docker.com/get-docker/"
  exit 1
fi

if ! docker info &>/dev/null; then
  log_error "Docker 守护进程未运行。请先启动 Docker。"
  exit 1
fi

if ! docker compose version &>/dev/null; then
  log_error "Docker Compose 插件未安装。请升级 Docker 到最新版本。"
  exit 1
fi

log_info "Docker $(docker --version | cut -d' ' -f3 | tr -d ',')"
log_info "Docker Compose $(docker compose version | awk '{print $NF}')"

# ── 配置准备 ──────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# 检查 .env
if [ ! -f .env ]; then
  if [ -f ../.env.example ]; then
    cp ../.env.example .env
    log_warn "已从 .env.example 生成 .env 文件"
    log_warn "请编辑 .env 填入 CPA_MANAGEMENT_KEY 和 LOGIN_PASSWORD 后重新运行"
    exit 0
  else
    log_error ".env.example 文件不存在，请确保仓库完整"
    exit 1
  fi
fi

# 校验必填配置
source .env 2>/dev/null || true

if [ -z "${CPA_BASE_URL:-}" ]; then
  log_error ".env 中 CPA_BASE_URL 未设置，请填写 CPA 服务地址"
  exit 1
fi

if [ -z "${CPA_MANAGEMENT_KEY:-}" ]; then
  log_error ".env 中 CPA_MANAGEMENT_KEY 未设置，请填写 CPA 管理密钥"
  exit 1
fi

if [ -z "${LOGIN_PASSWORD:-}" ]; then
  log_error ".env 中 LOGIN_PASSWORD 未设置，请填写管理面板登录密码"
  exit 1
fi

# ── 构建并启动 ────────────────────────────────────────────────────
log_info "正在部署 CPA Usage Keeper Enterprise..."

cd ..
COMPOSE_FILE="docker-compose.yml"
if [ ! -f "$COMPOSE_FILE" ]; then
  if [ -f docker-compose.example.yml ]; then
    cp docker-compose.example.yml docker-compose.yml
    log_warn "已从 docker-compose.example.yml 生成 docker-compose.yml"
  else
    log_error "docker-compose.yml 不存在"
    exit 1
  fi
fi

docker compose -f "$COMPOSE_FILE" --env-file enterprise/.env up -d

# ── 等待启动 ──────────────────────────────────────────────────────
log_info "等待服务启动（约 10 秒）..."
sleep 10

# ── 验证 ──────────────────────────────────────────────────────────
APP_PORT="${APP_PORT:-4320}"
HEALTH_STATUS=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:${APP_PORT}/healthz" 2>/dev/null || echo "000")

echo ""
echo "============================================"
if [ "$HEALTH_STATUS" = "200" ]; then
  echo -e "  ${GREEN}CPA Usage Keeper Enterprise 部署成功!${NC}"
  echo ""
  echo -e "  管理面板: ${GREEN}http://<host-ip>:${APP_PORT}${NC}"
  echo ""
  echo "  请使用 .env 中设置的 LOGIN_PASSWORD 登录管理面板"
else
  echo -e "  ${YELLOW}服务未正常启动 (HTTP ${HEALTH_STATUS})${NC}"
  echo "  请查看日志: docker compose logs"
  echo "============================================"
  exit 1
fi
echo "============================================"