# RIC-367: 多节点负载均衡与集群管理 — 方案设计

## 概述

cpa-usage-keeper 当前为单实例 SQLite 架构，受限于单进程写锁和单点故障风险。本方案设计一种**领导者-跟随者（Leader-Follower）**集群拓扑，在最小化代码改动的约束下实现多节点部署。

## 架构约束

| 约束 | 现状 | 集群方案 |
|------|------|----------|
| 数据库 | SQLite WAL，单 writer (MaxOpenConns=1) + 8 reader | **保持 SQLite，Leader 独占写** |
| 数据总线 | CPA → Redis stream → poller → inbox | **复用**，Follower 跳过 ingest |
| 后台任务 | 单进程 runner (ingest/process/aggregation/ranking/sync/backup) | **仅 Leader 运行** |
| 会话 | SQLite GormSessionStore | 迁移到 **Redis 共享会话** |
| 部署 | Docker Compose，单实例 | **Docker Compose 集群编排** |
| 健康检查 | 单节点 healthz | **集群健康检查 + 成员状态** |

---

## 1. 集群拓扑

```
┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│  Load Balancer (HAProxy / Nginx)                     │
│  /healthz → 所有节点  /api/* → Leader 写，Follower 读 │
└──────┬───────────────────────┬──────────────────┬────┘
       │                       │                  │
       ▼                       ▼                  ▼
┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│  Leader       │     │  Follower 1   │     │  Follower 2   │
│  ───────────  │     │  ───────────  │     │  ───────────  │
│  RW SQLite    │◄────│  RO SQLite    │◄────│  RO SQLite    │
│  Ingest/Proc  │     │  (NFS 挂载)   │     │  (NFS 挂载)   │
│  Aggregation  │     │  API 只读     │     │  API 只读     │
│  Ranking/Sync │     │  无后台任务   │     │  无后台任务   │
│  Backup/Cron  │     │              │     │              │
└──────────────┘     └──────────────┘     └──────────────┘
       │                       │                  │
       └───────────┬───────────┴──────────┬───────┘
                   │                      │
           ┌───────▼────────┐    ┌────────▼──────┐
           │  Shared Redis  │    │  Shared NFS   │
           │  (会话/锁)      │    │  (SQLite RO)  │
           └────────────────┘    └───────────────┘
```

### 角色定义

- **Leader**（1 节点）：持有 SQLite 写锁，运行全部后台任务（ingest/process/aggregation/ranking/sync/backup），接受写 API。
- **Follower**（N 节点）：只读挂载 SQLite，仅运行 HTTP API 服务（读请求），不运行后台任务。
- **Load Balancer**: 前置 Nginx/HAProxy，按路由规则分发请求。

### 适用规模

- 2-5 节点，中大型企业部署（日 1000 万+ 请求，高可用需求）
- 超过 5 节点建议升级到 PostgreSQL（见 5. 未来演进）

---

## 2. 存储与数据一致性

### 2.1 SQLite 共享方案

**Leader 本地写，NFS 只读分发** — SQLite WAL 模式天然支持 1 writer + N reader。

```
Leader 数据目录:
  /data/app.db       ← 主写
  /data/app.db-wal   ← WAL（实时同步到 NFS）
  /data/app.db-shm   ← 共享内存

Follower 挂载:
  mount -t nfs leader:/data /data (ro)
```

**关键约束**:
- Follower 以 `mode=ro` 打开 SQLite，确保不会被意外写入
- NFS 延迟容忍 ≤ 100ms，超过则 Follower 降级返回 stale 数据 + 警告头
- WAL checkpoint 由 Leader 的 `PRAGMA wal_autocheckpoint=1000` 控制
- 备份由 Leader 的 `VACUUM INTO` 定时执行，Follower 不参与备份

### 2.2 会话一致性

当前会话存储在 SQLite，集群下必须迁移到 Redis 共享会话：

```go
// 新增: RedisSessionStore (替代 GormSessionStore)
type RedisSessionStore struct {
    client *redis.Client
    ttl    time.Duration
}

// 存储: SESSION:{token_hash} → {user_id, role, last_seen_ip, last_seen_at}
// TTL: 匹配 AUTH_SESSION_TTL (默认 168h)，每次请求刷新
```

**会话读取路径**：LB 做 session affinity（cookie `keeper_session`）→ 首选节点 → 回退 Redis 查询。

### 2.3 数据一致性级别

| 操作 | 一致性级别 | 说明 |
|------|-----------|------|
| Usage 写入 (ingest) | 强一致 | 仅 Leader 写入 inbox |
| 聚合 (rollup) | 最终一致 | 仅 Leader 运行，5s debounce 窗口 |
| API 读 (realtime) | 最终一致（~1s）| Follower 可能读到延迟 |
| 告警/预算写入 | 强一致 | 仅 Leader 处理 |
| 用户配置写入 | 强一致 | 仅 Leader 处理 |
| 前端的实时概览 | 宽松 | 同节点读取，无跨节点保证 |

---

## 3. 集群协调与领导者选举

### 3.1 选主机制（基于 Redis）

```go
// 新增: internal/cluster/elector.go
type Elector struct {
    redisClient *redis.Client
    key         string  // "keeper:leader"
    ttl         time.Duration  // 15s
    nodeID      string  // 唯一节点标识
}

// 选主流程:
// 1. 启动时所有节点尝试 SETNX keeper:leader <nodeID> NX EX 15
// 2. 成功 → 成为 Leader，后台刷新 TTL (每 10s)
// 3. 失败 → 成为 Follower，监听 key 过期
// 4. Leader 下线 → 15s 后 key 过期 → 剩余节点重新选举
// 5. 网络分区 → 旧 Leader 降级为 Follower（无法续约）
```

### 3.2 角色切换

```go
// 新增: internal/cluster/role.go
type Role int
const (
    RoleFollower Role = iota
    RoleLeader
)

// Leader 降级流程:
// 1. 续约失败 → 停止后台 runner (graceful, 30s timeout)
// 2. 关闭 SQLite 写连接 → 以 RO 模式重新打开
// 3. 切换 gin 路由为只读模式
// 4. 报告状态: role=follower

// Follower 升级流程:
// 1. 选举胜出 → 以 RW 模式打开 SQLite
// 2. 启动全部后台 runner
// 3. 切换 gin 路由为完整模式
// 4. 追赶 lag（从 Redis 回放未处理消息）
```

### 3.3 成员发现

```go
// 新增: internal/cluster/membership.go
// 每个节点启动时在 Redis 注册:
//   HSET keeper:nodes <nodeID> '{"host":"10.0.0.1","port":4320,"role":"follower","started_at":"..."}'
// 心跳:
//   EXPIRE keeper:nodes 30  (每 20s 刷新)
// 成员查询:
//   HGETALL keeper:nodes → 所有活跃节点
```

---

## 4. 后台任务协调

### 4.1 任务清单

| 后台任务 | 运行节点 | 冲突风险 | 协调方式 |
|---------|---------|---------|---------|
| Redis Ingest | 仅 Leader | 低，Redis 队列消费 | 角色判断 |
| Redis Process | 仅 Leader | 低，inbox 处理 | 角色判断 |
| Usage Aggregation | 仅 Leader | 中，rollup 重复 | 角色判断 + 幂等 key |
| Ranking Sync | 仅 Leader | 低 | 角色判断 |
| Metadata Sync | 仅 Leader | 低 | 角色判断 |
| Quota Refresh | 仅 Leader | 中，API 限流 | 角色判断 + 分布式限流 |
| Database Backup | 仅 Leader | 低 | 角色判断 |

### 4.2 分布式锁辅助（可选）

对于非关键但可能出现竞争的后台操作（如版本升级时的迁移），使用 Redis 分布式锁：

```go
// 新增: internal/cluster/lock.go
// SETNX keeper:lock:migration <nodeID> NX EX 300
// 成功 → 执行迁移 → DEL
// 失败 → 跳过（另一个节点已执行）
```

### 4.3 幂等性保障

Usage 事件在 Leader 切换时可能重复处理：

- `redis_usage_inboxes` 的 `message_hash` 唯一约束 + `ON CONFLICT IGNORE` 已保证幂等
- 聚合 rollup 使用 `UsageAggregationRunner` 的 debounce 窗口 + 事件 ID 上界，切换后从 `MAX(id)` 重新开始

---

## 5. 负载均衡策略

### 5.1 路由规则

```
Nginx upstream 配置:

upstream keeper {
    # 会话亲和性（基于 cookie）
    hash $cookie_keeper_session consistent;
    
    server 10.0.0.1:4320 weight=10;  # Leader (写 + 读)
    server 10.0.0.2:4320 weight=5;   # Follower (只读)
    server 10.0.0.3:4320 weight=5;   # Follower (只读)
}

# 写 API → Leader（通过 header 转发）
# GET /api/usage, GET /api/overview → 任意节点
# POST /api/alert, POST /api/user, POST /api/quota/refresh → Leader
# 所有 /api/admin/* → Leader
```

### 5.2 写路由判断

```go
// 新增: internal/api/cluster.go
// 在 gin middleware 中判断:
// 1. 请求方法为 POST/PUT/DELETE 且路由为管理类 → 检查是否 Leader
// 2. 非 Leader → 307 重定向到 Leader 节点（从 Redis 成员表获取 Leader 地址）
// 3. 或返回 503 + Retry-After header（客户端自行重试）
```

### 5.3 健康检查增强

```go
// 扩展: internal/api/health.go
// GET /healthz →
//   {
//     "status": "ok",
//     "role": "leader|follower",
//     "leader_id": "node-xxx",
//     "members": ["node-xxx", "node-yyy", "node-zzz"],
//     "sqlite_path": "/data/app.db",
//     "uptime": "72h",
//     "last_election": "2026-08-22T10:00:00Z"
//   }

// 集群健康端点:
// GET /api/cluster/health →
//   {
//     "leader": { "node_id": "...", "last_seen": "..." },
//     "members": [
//       { "node_id": "...", "role": "follower", "status": "healthy|lost", "lag_ms": 50 }
//     ],
//     "redis": "connected",
//     "nfs": "mounted"
//   }
```

---

## 6. 配置同步

### 6.1 机制

当前配置通过 `.env` 文件管理。集群模式下有两种策略：

**策略 A: 文件分发（推荐，Phase 3 初期）**
- `.env` 通过 Ansible/Salt 等配置管理工具分发到所有节点
- `enterprise/config.yaml` 同理
- 变更后重启节点即可

**策略 B: 运行时热同步（Phase 3 后期）**
- 新增 `internal/cluster/configsync.go`
- Leader 的配置变更 → 发布到 Redis channel `keeper:config:update`
- Follower 订阅 → 热更新内存配置（不重启）
- 配置版本号校验，防止回滚

### 6.2 环境变量差异

| 变量 | Leader | Follower |
|------|--------|----------|
| `APP_HOST` | `0.0.0.0` | `0.0.0.0` |
| `APP_PORT` | `4320` | `4320` |
| `SQLITE_PATH` | `/data/app.db` | `/data/app.db (ro)` |
| `CLUSTER_NODE_ID` | `node-1` | `node-2` |
| `CLUSTER_ENABLED` | `true` | `true` |
| `CLUSTER_REDIS_ADDR` | `redis:6379` | `redis:6379` |
| `NFS_DATA_DIR` | `/data` | `/data (ro)` |

---

## 7. 运维告警与故障转移

### 7.1 故障检测

| 检测项 | 检测方式 | 告警级别 | 处理动作 |
|--------|---------|---------|---------|
| Leader 失联 | Redis key 过期 (15s) | CRITICAL | 自动重新选举 |
| Follower 失联 | 心跳超时 (30s) | WARNING | 从 LB 移除 |
| NFS 挂载断开 | 文件 stat 失败 | CRITICAL | Follower 降级为 503 |
| Redis 断连 | Redis ping 失败 | CRITICAL | 所有节点降级为单机模式 |
| SQLite 写锁等待 | `PRAGMA busy_timeout` 超时 | WARNING | 等待重试 |
| 数据同步延迟 | 比较 `MAX(event_id)` | INFO | 日志记录 |

### 7.2 自动故障转移

```
Leader 故障时序:
T+0     Leader 续约失败 (Redis SETNX 过期)
T+0     Follower 检测到 key 过期
T+0~1s  所有 Follower 竞争 SETNX
T+1s    胜出者成为新 Leader
T+1~5s  新 Leader 启动后台 runner
T+5~10s 新 Leader 追赶 lag（从 Redis 回放）
T+10s   集群恢复服务
```

**预期 RTO: ~15s 以内**（Redis key TTL + 选举 + 追赶）

### 7.3 运维命令

```bash
# 新增 CLI 子命令
keeper cluster status          # 查看集群状态
keeper cluster leave            # 优雅离开集群
keeper cluster elect            # 手动触发选举
keeper cluster node list        # 列出所有节点
keeper cluster node remove <id> # 移除失联节点
```

### 7.4 监控告警集成

复用现有 `internal/alert` 模块，新增集群告警规则：

```go
// 新增告警类型
AlertRuleClusterLeaderLost      // Leader 丢失
AlertRuleClusterMemberLost      // 成员节点丢失
AlertRuleClusterSplitBrain      // 脑裂风险（同一 key 出现两个 Leader）
AlertRuleClusterReplicationLag  // 数据同步延迟超阈值
```

---

## 8. 实现路线图

### Phase 3.1: 基础集群（第 1 周）

| 子任务 | 文件 | 工作量 |
|--------|------|--------|
| Redis 会话存储 | `internal/auth/redis_session.go` | 1d |
| 集群选举器 | `internal/cluster/elector.go` | 1d |
| 成员管理 | `internal/cluster/membership.go` | 0.5d |
| 角色路由中间件 | `internal/api/cluster.go` | 0.5d |
| 健康检查增强 | `internal/api/health.go` | 0.5d |
| 环境变量新增 | `internal/config/config.go` | 0.5d |
| Docker Compose 集群编排 | `deploy/docker-compose.cluster.yml` | 0.5d |
| 测试 | 各模块 | 1d |

### Phase 3.2: 运维完善（第 2 周）

| 子任务 | 文件 | 工作量 |
|--------|------|--------|
| 配置同步 | `internal/cluster/configsync.go` | 1d |
| 分布式锁 | `internal/cluster/lock.go` | 0.5d |
| 后台任务调度隔离 | `internal/app/app.go` | 1d |
| CLI 集群命令 | `cmd/server/cluster_cmd.go` | 1d |
| 告警规则扩展 | `internal/alert/` | 0.5d |
| 故障转移测试 | 集成测试 | 1d |
| 文档 | `enterprise/docs/05-clustering.md` | 0.5d |

---

## 9. 风险与替代方案

### 风险

| 风险 | 影响 | 缓解 |
|------|------|------|
| NFS 单点故障 | 所有 Follower 不可读 | 使用 HA NFS (GlusterFS/EFS) |
| 脑裂（网络分区） | 双 Leader 写冲突 | 设置 strict quorum（多数派），Redis key 属主仲裁 |
| SQLite 不适用于 >5 节点 | 性能瓶颈 | 5. 未来演进 → PostgreSQL |
| Agent 切换期间数据丢失 | 重复/丢失 event | message_hash 幂等 + 回放机制 |

### 替代方案对比

| 方案 | 复杂度 | 一致性 | 性能 | 适用场景 |
|------|--------|--------|------|---------|
| **Leader-Follower (本方案)** | 中 | 最终一致 | 高 | 中大型企业，2-5 节点 |
| Active-Standby (冷备) | 低 | 最终一致 | 中 | 小型企业，仅 HA 需求 |
| PostgreSQL (替代 SQLite) | 高 | 强一致 | 高 | 大型企业，5+ 节点 |
| 多主 CRDT | 极高 | 最终一致 | 低 | 边缘部署，不推荐 |

---

## 10. 未来演进

### 5+ 节点 → PostgreSQL 迁移路径

```
1. 新增 PostgreSQL 驱动 (internal/repository/postgres.go)
2. 抽象 Repository 接口（现有 GORM 操作已部分抽象）
3. 配置切换: DB_DRIVER=sqlite|postgres
4. 数据迁移脚本 (enterprise/scripts/migrate-to-postgres.sh)
5. 移除 NFS 依赖，所有节点直连 PostgreSQL
```

### 多数据中心 (备选)

```
- 主集群 (Leader + Follower) → 主 Redis + SQLite
- 备集群 (只读) → 订阅主 Redis + 异步复制
- 跨数据中心 DNS 负载均衡
```

---

## 文件清单

```
internal/cluster/
  ├── elector.go        # 基于 Redis 的领导者选举
  ├── membership.go     # 成员发现与心跳
  ├── role.go           # 角色类型与切换
  ├── lock.go           # 分布式锁辅助
  └── configsync.go     # 配置运行时同步

internal/auth/
  └── redis_session.go  # Redis 共享会话存储

internal/api/
  └── cluster.go        # 集群路由中间件 + 健康端点

deploy/
  └── docker-compose.cluster.yml  # 集群编排

enterprise/docs/
  └── 05-clustering.md  # 集群部署文档
```

---

## 评审要点

1. **Leader-Follower vs Active-Standby** — 是否接受 NFS 引入的依赖？Follower 只读能否满足业务需求？
2. **会话 Redis 化** — 是否可以作为独立子任务先行实施（不依赖集群功能）？
3. **PostgreSQL 准备度** — 是否需要在 Phase 3 预留 PostgreSQL 的 Repository 抽象层，还是等 Phase 4 再重构？
4. **负载均衡** — 由基础设施层（Nginx/HAProxy）处理，还是 Keeper 内建写路由重定向？
5. **RTO 15s** — 是否满足企业 SLA？需要加速可以缩短 Redis key TTL 到 5s。