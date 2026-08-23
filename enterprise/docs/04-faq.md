# AI Gateway Enterprise — 常见问题 (FAQ)

## 部署

### Q: 部署失败，CPA 服务启动后立即退出？

最常见的三个原因：

1. **`.env` 中 `LICENSE_KEY` 为空** — 务必填写，否则 `docker-entrypoint.sh` 会拒绝启动。
2. **缓存了旧的 `.env.example`** — 旧模板使用 `ADMIN_PASSWORD`，新版本改用 `LOGIN_PASSWORD`。请确认 `.env` 中的变量名与新模板一致。
3. **CPU 架构不兼容** — Apple Silicon (M 系列) 或 ARM 服务器上构建时，Dockerfile 需要原生 build，首次启动较慢，请耐心等待或查看 `docker compose logs cpa`。

### Q: 如何确认服务是否全部启动？

```bash
cd enterprise
bash scripts/healthcheck.sh
# 输出 HEALTHY 表示正常
```

### Q: 端口被占用怎么办？

```bash
# 在 .env 中修改端口
CPA_PORT=8318
KEEPER_PORT=4321
```

### Q: `docker compose up -d` 报 `LICENSE_KEY is required`？

`.env` 文件不存在或 `LICENSE_KEY` 为空。请确认：

```bash
ls -la .env        # 确认存在
grep LICENSE_KEY .env  # 确认已填写非空值
```

### Q: 还需要安装 Redis 吗？

**不需要。** 本架构不包含独立 Redis 容器。CPA 的用量队列是进程内内存队列，
KEEPER 直接通过 TCP RESP 协议（复用 CPA 的 8317 端口）拉取用量数据。
无需额外部署维护 Redis。

## 授权

### Q: `LICENSE_KEY` 从哪获取？

购买企业版后由销售团队发放。未购买时也可用于试用流程。

### Q: `LICENSE_SERVER_URL` 留空会不会不安全？

留空时进入**离线模式**：容器只校验 key 非空，会跳过远程验证。这对于完全内网 / 无法访问外网的私有化部署是必要的（它们本就不依赖外部授权服务器）。

### Q: 远程验证失败会怎样？

容器会重试 3 次（每次间隔 2 秒），全部失败则拒绝启动：

```
ERROR: License key validation failed.
```

请检查 `LICENSE_SERVER_URL` 地址可达、返回的 JSON 包含 `{"valid":true}`。

## KEEPER 管理面板

### Q: 登录管理面板时提示密码错误？

确认 `.env` 中的 `LOGIN_PASSWORD` 已设置，并重新执行：

```bash
cd enterprise
docker compose up -d      # 重新创建 keeper 容器使新密码生效
```

### Q: 管理面板看不到用量数据？

依次排查：

```bash
# 1. 确认 CPA 已开启用量统计（config.yaml 中 usage-statistics-enabled: true）
grep usage-statistics-enabled config.yaml

# 2. 确认 CPA 管理密钥一致（config.yaml 的 remote-management.secret-key == .env 的 CPA_MANAGEMENT_KEY）
grep secret-key config.yaml
grep CPA_MANAGEMENT_KEY .env

# 3. 修改后重启
docker compose up -d --force-recreate cpa keeper

# 4. 观察 KEEPER 日志是否有认证/连接错误
docker compose logs keeper | tail -50
```

### Q: 忘记登录密码怎么办？

修改 `.env` 中的 `LOGIN_PASSWORD`，然后重建容器：

```bash
docker compose up -d --force-recreate keeper
```

## 升级与数据

### Q: 如何升级到新版本？

```bash
cd enterprise
bash scripts/update.sh
```

脚本会拉取最新镜像并重启，最后自检健康状态。

### Q: 数据和配置会丢失吗？

不会。`docker-compose.yml` 使用命名卷持久化所有数据（CPA 认证、CPA 日志、KEEPER 数据库）。即使删除容器，卷中的数据依然保留。

### Q: 如何备份 / 迁移？

```bash
# 停止服务后，导出命名卷：
docker run --rm -v enterprise_cpa-auth:/data -v $(pwd)/backup:/backup alpine tar czf /backup/cpa-auth.tar.gz -C /data .
docker run --rm -v enterprise_cpa-logs:/data -v $(pwd)/backup:/backup alpine tar czf /backup/cpa-logs.tar.gz -C /data .
docker run --rm -v enterprise_keeper-data:/data -v $(pwd)/backup:/backup alpine tar czf /backup/keeper-data.tar.gz -C /data .
```

在新环境 `docker compose up -d` 后，用同样方式反向导入即可。

## 安全

### Q: 生产环境有何安全建议？

- 使用强密码：`LOGIN_PASSWORD` 建议 16 位以上随机字符
- 使用强管理密钥：`CPA_MANAGEMENT_KEY` 建议 32 位以上随机字符
- 前面加 Nginx / Caddy 反向代理并配置 HTTPS
- 定期备份命名卷

### Q: 为什么 `.env` 和 `config.yaml` 不能被提交到 git？

`.env` 包含 `LICENSE_KEY`、`LOGIN_PASSWORD`、`CPA_MANAGEMENT_KEY` 等敏感信息。
`config.yaml` 包含 `remote-management.secret-key` 管理密钥。
提交到 git 会导致凭据泄露。只提交 `.env.example` 和 `config.yaml.example` 模板。