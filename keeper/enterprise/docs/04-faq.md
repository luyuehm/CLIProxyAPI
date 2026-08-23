# CPA Usage Keeper Enterprise — 常见问题 (FAQ)

## 部署

### Q: 部署失败，服务启动后立即退出？

最常见的原因：

1. **`.env` 配置不完整** — 确认 `CPA_BASE_URL`、`CPA_MANAGEMENT_KEY`、`LOGIN_PASSWORD` 均已填写
2. **CPA 无法访问** — 确认 `CPA_BASE_URL` 在容器内可访问。Docker 中宿主机上的 CPA 需使用 `http://host.docker.internal:8317`
3. **端口被占用** — 修改 `APP_PORT` 为其他端口

### Q: 如何确认服务是否正常？

```bash
cd enterprise
bash scripts/healthcheck.sh
# 输出 HEALTHY 表示正常
```

### Q: 如何查看日志？

```bash
docker compose logs -f cpa-usage-keeper
```

### Q: 需要单独安装 Redis 吗？

**不需要。** Keeper 通过 TCP RESP 协议直接连接 CPA 的 8317 端口拉取用量数据，无需额外部署 Redis。

## CPA 集成

### Q: 管理面板看不到用量数据？

依次排查：

```bash
# 1. 确认 CPA 已开启用量统计
# 查看 CPA 的 config.yaml 中以下配置：
#   usage-statistics-enabled: true

# 2. 确认 CPA 管理密钥一致
# .env 的 CPA_MANAGEMENT_KEY 必须与 CPA config.yaml 中 remote-management.secret-key 一致

# 3. 确认 CPA 允许远程管理
# CPA config.yaml 中：
#   remote-management:
#     allow-remote: true

# 4. 修改 CPA 配置后重启 CPA 服务

# 5. 观察 Keeper 日志
docker compose logs cpa-usage-keeper | tail -50
```

### Q: CPA 和 Keeper 不在同一台机器上？

将 `.env` 中的 `CPA_BASE_URL` 设置为 CPA 的实际可达地址（IP 或域名），并确保 `REDIS_QUEUE_ADDR` 也指向 CPA 的 RESP 端口。

## 访问

### Q: 登录管理面板时提示密码错误？

确认 `.env` 中的 `LOGIN_PASSWORD` 已设置，然后重启：

```bash
docker compose up -d --force-recreate cpa-usage-keeper
```

### Q: 忘记登录密码怎么办？

修改 `.env` 中的 `LOGIN_PASSWORD`，然后重建容器：

```bash
docker compose up -d --force-recreate cpa-usage-keeper
```

### Q: 如何通过反向代理部署在子路径下？

在 `.env` 中设置 `APP_BASE_PATH=/keeper`，然后在反向代理中配置：

```nginx
location /keeper/ {
    proxy_pass http://127.0.0.1:4320;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
}
```

### Q: 如何启用 HTTPS？

方法一：在前面加 Nginx/Caddy 反向代理并配置 HTTPS（推荐）。
方法二：在 `.env` 中设置：

```env
TLS_ENABLED=true
TLS_CERT_FILE=/path/to/cert.pem
TLS_KEY_FILE=/path/to/key.pem
```

## 升级与数据

### Q: 如何升级到新版本？

```bash
cd enterprise
bash scripts/update.sh
```

脚本会拉取最新代码、重新构建并重启，最后自检健康状态。

### Q: 数据和配置会丢失吗？

不会。Keeper 数据存储在 `WORK_DIR`（默认 `./data`）中，包括 SQLite 数据库、日志和备份。即使删除容器，宿主机上的数据依然保留。备份功能默认启用，每 24h 自动备份一次，保留 7 天。

### Q: 如何备份数据？

```bash
# 停止服务后直接复制数据目录
cp -r ./data ./data-backup-$(date +%Y%m%d)
```

或使用内置备份功能（默认启用），备份文件位于 `./data/backups/`。

## 安全

### Q: 生产环境有何安全建议？

- 使用强密码：`LOGIN_PASSWORD` 建议 16 位以上随机字符
- 使用强管理密钥：`CPA_MANAGEMENT_KEY` 建议 32 位以上随机字符
- 前面加 Nginx / Caddy 反向代理并配置 HTTPS
- 限制暴露范围：Docker 端口映射使用 `127.0.0.1:4320:4320` 仅本地访问
- 定期备份 `./data` 目录

### Q: 为什么 `.env` 不能被提交到 git？

`.env` 包含 `CPA_MANAGEMENT_KEY`、`LOGIN_PASSWORD` 等敏感信息。提交到 git 会导致凭据泄露。只提交 `.env.example` 模板。