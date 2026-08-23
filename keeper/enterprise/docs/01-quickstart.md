# CPA Usage Keeper Enterprise — 快速部署指南

## 系统要求

- Docker 24.0+
- Docker Compose 插件（已随 Docker Desktop 安装）
- 操作系统：Linux / macOS / Windows
- 一个正在运行的 [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 实例（或准备同时部署）

## 1 分钟快速部署

```bash
# 1. 进入企业版目录
cd enterprise

# 2. 配置环境变量
cp ../.env.example .env
# 编辑 .env，至少填入 CPA_BASE_URL、CPA_MANAGEMENT_KEY 和 LOGIN_PASSWORD

# 3. 一键部署
bash install.sh
```

## 手动部署

```bash
# 1. 配置环境变量
cp .env.example .env
vi .env   # 填入 CPA_BASE_URL、CPA_MANAGEMENT_KEY、LOGIN_PASSWORD 等

# 2. 构建并启动
cd ..
docker compose up -d --build

# 3. 验证
curl http://localhost:4320/healthz
# → 返回 {"status":"ok"}
```

## 访问管理面板

打开浏览器访问 `http://<host-ip>:4320`（默认 0.0.0.0 绑定，可从同一局域网的其他机器访问），使用 `.env` 中设置的 `LOGIN_PASSWORD` 登录。

## 配置 CPA 集成

Keeper 需要 CPA 开启以下配置（修改 CPA 的 `config.yaml`）：

```yaml
remote-management:
  allow-remote: true
  secret-key: "<与 .env 中 CPA_MANAGEMENT_KEY 一致>"

usage-statistics-enabled: true
redis-usage-queue-retention-seconds: 60
```

修改后重启 CPA 服务。

## 升级

```bash
cd enterprise
bash scripts/update.sh
```

## 查看日志

```bash
cd ..
docker compose logs -f            # 查看所有服务日志
docker compose logs -f cpa-usage-keeper  # 仅查看 Keeper 日志
```

## 停止服务

```bash
cd ..
docker compose down
```

## 下一步

- 查看 [配置说明](02-configuration.md) 了解所有配置项
- 查看 [企业功能说明](03-enterprise.md) 了解企业版特性
- 查看 [常见问题](04-faq.md) 了解常见问题和排错方法