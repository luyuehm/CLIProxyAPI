# AI Gateway Enterprise — 快速部署指南

## 系统要求

- Docker 24.0+
- Docker Compose 插件（已随 Docker Desktop 安装）
- 操作系统：Linux / macOS / Windows

## 1 分钟快速部署

```bash
# 1. 下载部署包（示例）
curl -sSL https://get.ai-gateway.com | bash

# 或手动操作：
cd enterprise

# 2. 配置环境变量
cp .env.example .env
# 编辑 .env，填入 LICENSE_KEY、CPA_MANAGEMENT_KEY 和 LOGIN_PASSWORD
# CPA_MANAGEMENT_KEY 必须与 enterprise/config.yaml 中 remote-management.secret-key 一致

# 3. 配置 CPA 管理密钥
cp config.yaml.example config.yaml
# 编辑 config.yaml，设置 remote-management.secret-key（与 .env 的 CPA_MANAGEMENT_KEY 相同）

# 4. 启动
bash install.sh
```

如果你使用本仓库自带微调过的 `config.yaml`（已内嵌 `change-me-enterprise-management-key`），
步骤 3 可跳过，但生产环境务必改掉默认密钥。

## 手动部署

```bash
# 1. 进入企业版目录
cd ~/CLIProxyAPI/enterprise

# 2. 配置环境变量
cp .env.example .env
vi .env   # 填入 LICENSE_KEY、CPA_MANAGEMENT_KEY 和 LOGIN_PASSWORD

# 3. 确保 CPA config.yaml 已配置管理密钥
vi config.yaml
#   remote-management:
#     secret-key: "<与 .env 中 CPA_MANAGEMENT_KEY 一致>"
#     allow-remote: true
#   usage-statistics-enabled: true
#
#   提示：CPA 启动时会把明文 secret-key 自动 bcrypt 哈希进内存，
#   KEEPER 用明文密钥通过 RESP 协议登录 CPA 的 8317 端口工作正常。

# 4. 启动服务
docker compose up -d

# 5. 验证
curl http://localhost:8317/healthz
# → 返回 {"status":"ok"}

curl http://localhost:4320/healthz
# → 返回 {"status":"ok"}
```

## 访问管理面板

打开浏览器访问 `http://<host-ip>:4320`（默认 0.0.0.0 绑定，可从同一局域网的其他机器访问），使用 `.env` 中设置的 `LOGIN_PASSWORD` 登录。

## 发送 API 请求

与 OpenAI 完全兼容的协议，只需修改 `base_url`：

```bash
curl http://localhost:8317/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-api-key" \
  -d '{
    "model": "gpt-4o",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

## 升级

```bash
cd enterprise
bash scripts/update.sh
```

## 查看日志

```bash
cd enterprise
docker compose logs -f      # 查看所有服务日志
docker compose logs -f cpa  # 仅查看 CPA 日志
```

## 停止服务

```bash
cd enterprise
docker compose down
```

> 注意：架构不再包含独立 Redis 容器。CPA 内置内存用量队列，KEEPER 直接通过
> TCP RESP 协议（CPA 8317 端口的 management 通道）拉取用量数据，因此无需
> 额外部署维护 Redis。