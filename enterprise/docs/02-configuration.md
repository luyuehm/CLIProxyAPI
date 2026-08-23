# AI Gateway Enterprise — 配置说明

## 环境变量（.env）

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `LICENSE_KEY` | ✅ | — | 企业版授权密钥，购买后获取 |
| `CPA_MANAGEMENT_KEY` | ✅ | — | CPA 管理密钥，与 `config.yaml` 中 `remote-management.secret-key` 一致 |
| `LOGIN_PASSWORD` | ✅ | — | 管理面板登录密码 |
| `CPA_PORT` | — | `8317` | CPA 路由引擎端口 |
| `KEEPER_PORT` | — | `4320` | KEEPER 管理面板端口 |
| `CPA_BASE_URL` | — | `http://cpa:8317` | CPA 服务地址（容器内通信） |
| `LICENSE_SERVER_URL` | — | 空（离线模式） | 授权验证服务器，留空不验证 |

## 端口映射

| 端口 | 服务 | 说明 |
|------|------|------|
| `8317` | CPA | OpenAI 兼容 API 代理端口 + 管理 RESP 协议端口 |
| `4320` | KEEPER | 管理面板 Web 端口 |

## CPA 管理密钥（secret-key）机制

CPA 的 `remote-management.secret-key` 支持两种格式：

- **明文**：启动时 CPA 自动检测并 bcrypt 哈希到内存中，同时在磁盘上尝试持久化哈希值
- **bcrypt 哈希**：直接使用，不做二次哈希

KEEPER 通过 `CPA_MANAGEMENT_KEY` 环境变量传递**明文**密钥，通过 TCP RESP 协议
认证到 CPA 的 8317 端口（与 HTTP 共享同一端口）。`AuthenticateManagementKey` 使用
`bcrypt.CompareHashAndPassword` 验证，因此明文密钥能正确匹配内存中的哈希值。

**重要**：`config.yaml` 中的 `secret-key` 与 `.env` 中的 `CPA_MANAGEMENT_KEY`
**必须使用同一个明文值**——CPA 的 bcrypt 哈希在内存中与明文 RESP 登录兼容。

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

注意：本架构**不包含独立 Redis 容器**。CPA 的用量队列是进程内内存队列，
KEEPER 通过 RESP 协议从 CPA 的 8317 端口直接拉取数据，无需额外维护 Redis 服务。

### 数据持久化

`docker-compose.yml` 使用三个命名卷持久化数据：

| 卷 | 挂载点 | 存储内容 |
|----|--------|---------|
| `cpa-auth` | `/root/.cli-proxy-api` | CPA 认证信息（OAuth 登录凭据） |
| `cpa-logs` | `/var/log/cpa` | CPA 运行日志 |
| `keeper-data` | `/data` | KEEPER 数据库、日志、备份 |

### 资源限制（可选）

在 `docker-compose.yml` 中为生产环境添加资源限制：

```yaml
services:
  cpa:
    deploy:
      resources:
        limits:
          cpus: '2'
          memory: 512M
  keeper:
    deploy:
      resources:
        limits:
          cpus: '1'
          memory: 256M
```

## 配置管理

### CPA 配置

CPA 的详细配置通过 `config.yaml` 文件管理，挂载到容器内 `/etc/cpa/config.yaml`。

KEEPER 集成需要 CPA 开启以下配置：

```yaml
# 管理 API（KEEPER 通过 RESP 协议在 8317 端口认证）
remote-management:
  # 允许非 localhost 访问管理 API（容器内需要）
  allow-remote: true
  # 管理密钥，与 .env 中 CPA_MANAGEMENT_KEY 一致
  secret-key: "your-management-key"

# 用量统计（KEEPER 通过 RESP 协议拉取用量数据）
usage-statistics-enabled: true
```

### 安全配置

- 修改 `LOGIN_PASSWORD` 为强密码（建议 16 位以上随机字符）
- 修改 `CPA_MANAGEMENT_KEY` 为强随机字符串（32 位以上）
- 生产环境建议使用反向代理（如 Nginx）并配置 HTTPS
- 不要将 `config.yaml` 和 `.env` 提交到 git（已配置 `.gitignore`）