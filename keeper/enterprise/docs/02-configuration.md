# CPA Usage Keeper Enterprise — 配置说明

## 环境变量（.env）

Keeper 的所有配置通过 `.env` 文件管理。以下是完整的配置项说明。

### 1. 最小必填

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `CPA_BASE_URL` | ✅ | — | Keeper 服务端访问 CPA 的地址。Docker Compose 内通常是 `http://cli-proxy-api:8317` |
| `CPA_MANAGEMENT_KEY` | ✅ | — | CPA 管理密钥，用于拉取管理接口数据 |

### 2. Web 访问与反代

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `APP_HOST` | — | 所有接口 | HTTP 监听主机；原生部署仅本机访问可设为 `127.0.0.1` |
| `APP_PORT` | — | `4320` | HTTP 监听端口 |
| `APP_BASE_PATH` | — | 根路径 `/` | 部署子路径，如 `/keeper` |
| `CPA_PUBLIC_URL` | — | 当前 origin | 公开 CPA 地址，用于"返回 CPA"跳转 |
| `TRUSTED_PROXY_CIDRS` | — | 仅 loopback | 允许提供 X-Forwarded-For 的代理 CIDR |

### 3. 登录保护

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `AUTH_ENABLED` | — | `true` | 是否启用登录保护 |
| `LOGIN_PASSWORD` | 启用时必填 | — | 登录密码 |
| `AUTH_SESSION_TTL` | — | `168h` | Session 有效时长 |

### 4. 时区与请求行为

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `TZ` | — | `Asia/Shanghai` | 业务时区，影响 Today、按天聚合、页面时间 |
| `REQUEST_TIMEOUT` | — | `30s` | CPA HTTP 请求和 Redis 队列超时 |
| `TLS_SKIP_VERIFY` | — | `false` | 跳过 TLS 证书验证 |

### 5. 配额刷新

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `QUOTA_REFRESH_WORKER_LIMIT` | — | `10` | Auth Files 配额刷新最大并发数（最大 100） |

### 6. Redis 队列高级配置

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `REDIS_QUEUE_ADDR` | — | CPA_BASE_URL 主机名:8317 | Redis/RESP TCP 地址 |
| `REDIS_QUEUE_TLS` | — | `false` | 是否使用 TLS 连接 Redis 队列 |
| `REDIS_QUEUE_BATCH_SIZE` | — | `10000` | 每次 LPOP 最多拉取记录数 |
| `REDIS_QUEUE_IDLE_INTERVAL` | — | `1s` | 队列空闲检查间隔 |

### 7. 存储、日志与备份

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `WORK_DIR` | — | `./data` | 工作目录，SQLite 数据库、日志、备份默认写入此处 |
| `LOG_LEVEL` | — | `info` | 日志级别 |
| `LOG_FILE_ENABLED` | — | `true` | 是否写入持久化日志文件 |
| `LOG_RETENTION_DAYS` | — | `7` | 综合日志保留天数；0 表示不自动清理 |
| `BACKUP_ENABLED` | — | `true` | 是否启用 SQLite 数据库备份 |
| `BACKUP_INTERVAL` | — | `24h` | 备份间隔 |
| `BACKUP_RETENTION_DAYS` | — | `7` | 备份文件保留天数 |

### 8. 内置 HTTPS

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `TLS_ENABLED` | — | `false` | 是否启用内置 HTTPS |
| `TLS_CERT_FILE` | 启用 TLS 时必填 | — | 证书文件路径 |
| `TLS_KEY_FILE` | 启用 TLS 时必填 | — | 私钥文件路径 |

## CPA 配置要求

Keeper 正常运行需要 CPA 开启以下配置（修改 CPA 的 `config.yaml`）：

```yaml
# 管理 API 开关
remote-management:
  allow-remote: true
  secret-key: "your-management-key"   # 必须与 .env 的 CPA_MANAGEMENT_KEY 一致

# 用量统计开关
usage-statistics-enabled: true
redis-usage-queue-retention-seconds: 60
```

## 端口映射

| 端口 | 服务 | 说明 |
|------|------|------|
| `4320` | Keeper | 管理面板 Web 端口 |
| `8317` | CPA | OpenAI 兼容 API 代理端口 + 管理 RESP 协议端口 |

## Docker Compose 配置

### 服务架构

```
┌──────────────┐     HTTP/RESP     ┌──────────────────┐
│   CPA 核心引擎 │◄──────────────────│ KEEPER 用量监控面板│
│  (port 8317)  │    TCP 8317      │  (port 4320)     │
│               │  (共享端口)       │                  │
│  redisqueue   │                  │  RedisQueueClient │
│  (内存队列)    │                  │  (RESP 协议拉取)   │
└──────────────┘                   └──────────────────┘
```

### 数据持久化

Keeper 数据存储在 `./data` 目录（由 `WORK_DIR` 控制），包括：

| 目录/文件 | 说明 |
|-----------|------|
| `data/app.db` | SQLite 数据库（用量数据、配置快照） |
| `data/logs/` | 运行日志 |
| `data/backups/` | 数据库备份 |

### 资源限制（可选）

在 `docker-compose.yml` 中添加：

```yaml
services:
  cpa-usage-keeper:
    deploy:
      resources:
        limits:
          cpus: '1'
          memory: 256M
```

## 安全配置

- 修改 `LOGIN_PASSWORD` 为强密码（建议 16 位以上随机字符）
- 修改 `CPA_MANAGEMENT_KEY` 为强随机字符串（32 位以上）
- 生产环境建议使用反向代理（如 Nginx）并配置 HTTPS
- 不要将 `.env` 提交到 git（已配置 `.gitignore`）