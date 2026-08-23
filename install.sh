#!/bin/bash
#
# AI Gateway Enterprise — 一键安装脚本
# 用法: bash install.sh
#
set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

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

# ── 配置检查 ──────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# 检查 .env
if [ ! -f .env ]; then
  if [ -f .env.example ]; then
    cp .env.example .env
    log_warn "已从 .env.example 生成 .env 文件"
    log_warn "请编辑 .env 文件填入 LICENSE_KEY、CPA_MANAGEMENT_KEY 和 LOGIN_PASSWORD 后重新运行"
    exit 0
  else
    log_error ".env.example 文件不存在，请确保 enterprise/ 目录完整"
    exit 1
  fi
fi

# 检查 config.yaml（CPA 管理密钥配置）
if [ ! -f config.yaml ]; then
  if [ -f config.yaml.example ]; then
    cp config.yaml.example config.yaml
    log_warn "已从 config.yaml.example 生成 config.yaml 文件"
    log_warn "请编辑 config.yaml 填入 remote-management.secret-key（必须与 .env 中 CPA_MANAGEMENT_KEY 一致）后重新运行"
    exit 0
  else
    log_error "config.yaml.example 文件不存在，请确保 enterprise/ 目录完整"
    exit 1
  fi
fi

# 校验必填配置
source .env 2>/dev/null || true

if [ -z "${LICENSE_KEY:-}" ]; then
  log_error ".env 中 LICENSE_KEY 未设置，请填写授权密钥"
  exit 1
fi

if [ -z "${LOGIN_PASSWORD:-}" ]; then
  log_error ".env 中 LOGIN_PASSWORD 未设置，请填写管理面板登录密码"
  exit 1
fi

if [ -z "${CPA_MANAGEMENT_KEY:-}" ]; then
  log_error ".env 中 CPA_MANAGEMENT_KEY 未设置，请填写 CPA 管理密钥"
  exit 1
fi

# 校验 config.yaml 中的管理密钥与 .env 一致（仅检查明文场景）
# 若 config.yaml 中已是 bcrypt 哈希（CPA 在可写挂载下会自动持久化哈希），
# 无法仅凭文件校验，跳过对比——运行时由 CPA 的 bcrypt 验证兜底。
CFG_KEY=$(grep -E '^\s+secret-key:' config.yaml 2>/dev/null | head -1 | sed 's/.*secret-key:[[:space:]]*"\(.*\)".*/\1/' | sed "s/.*secret-key:[[:space:]]*'\(.*\)'.*/\1/" | sed 's/.*secret-key:[[:space:]]*//')
case "$CFG_KEY" in
  \$2a\$*|\$2b\$*|\$2y\$*) CFG_KEY="" ;;
esac
if [ -n "$CFG_KEY" ] && [ "$CFG_KEY" != "$CPA_MANAGEMENT_KEY" ]; then
  log_error "config.yaml 中 remote-management.secret-key ($CFG_KEY) 与 .env 中 CPA_MANAGEMENT_KEY ($CPA_MANAGEMENT_KEY) 不一致"
  exit 1
fi
log_info "正在启动 AI Gateway Enterprise..."

COMPOSE_FILE="$SCRIPT_DIR/docker-compose.yml"
if [ ! -f "$COMPOSE_FILE" ]; then
  log_error "docker-compose.yml 不存在，请确保在 enterprise/ 目录下运行"
  exit 1
fi

docker compose -f "$COMPOSE_FILE" up -d

# ── 等待启动 ──────────────────────────────────────────────────────
log_info "等待服务启动（约 10 秒）..."
sleep 10

# ── 验证 ──────────────────────────────────────────────────────────
CPA_PORT="${CPA_PORT:-8317}"
KEEPER_PORT="${KEEPER_PORT:-4320}"
ALL_OK=true

# 检查 CPA
CPA_STATUS=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:${CPA_PORT}/healthz" 2>/dev/null || echo "000")
if [ "$CPA_STATUS" = "200" ]; then
  log_info "CPA 核心引擎  ✅ (端口 ${CPA_PORT})"
else
  log_error "CPA 核心引擎  ❌ (HTTP ${CPA_STATUS})"
  ALL_OK=false
fi

# 检查 KEEPER
KEEPER_STATUS=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:${KEEPER_PORT}/healthz" 2>/dev/null || echo "000")
if [ "$KEEPER_STATUS" = "200" ]; then
  log_info "KEEPER 管理面板 ✅ (端口 ${KEEPER_PORT})"
else
  log_error "KEEPER 管理面板 ❌ (HTTP ${KEEPER_STATUS})"
  ALL_OK=false
fi

# ── 输出结果 ──────────────────────────────────────────────────────
echo ""
echo "============================================"
if [ "$ALL_OK" = true ]; then
  echo -e "  ${GREEN}AI Gateway Enterprise 部署成功!${NC}"
  echo ""
  echo -e "  管理面板: ${GREEN}http://<host-ip>:${KEEPER_PORT}${NC} (默认 0.0.0.0 绑定，同一局域网可访问)"
  echo -e "  API 代理: ${GREEN}http://<host-ip>:${CPA_PORT}${NC} (默认 0.0.0.0 绑定，同一局域网可访问)"
  echo ""
  echo "  请使用 .env 中设置的 LOGIN_PASSWORD 登录管理面板"
  echo "  API 请求请发送到 :${CPA_PORT}，使用 OpenAI 兼容协议"
else
  echo -e "  ${YELLOW}部分服务未正常启动，请查看日志:${NC}"
  echo "  docker compose -f $COMPOSE_FILE logs"
  echo "============================================"
  exit 1
fi
echo "============================================"