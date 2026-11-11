# 套利决策转发（RIC-2458）

本页说明 CLIProxyAPI（CPA 数据面）如何消费 KEEPER 的跨云竞价决策，把真实请求
切换到当期最便宜的健康 Provider。

> 该通道**默认关闭**。不配置任何环境变量时，网关的选路行为与未改动版本完全一致。

## 为什么需要它

KEEPER 侧的套利引擎（RIC-1921 / RIC-2228）只负责**算**：它按跨云单价与真实健康
观测挑出每个模型当期最优的 Provider，并落审计事件。但用户请求走的是 CPA，KEEPER
不承接流量，因此决策本身不产生任何降本——月度账单里的 `SavingsPct` 只是反事实价差
估算。

本通道补上最后一环：CPA 周期拉取决策，让**实际上游目标**跟着决策走，账单口径才从
「估算」变成「已发生」。

## 数据流

```
                   ┌──────────────── KEEPER ────────────────┐
provider_prices ──►│ 竞价引擎 (internal/router)              │
usage_events    ──►│   ├─ 健康观测（延迟/失败率）            │
                   │   └─ 决策快照                           │
                   └──────────────┬─────────────────────────┘
                                  │ GET /api/v1/arbitrage/decisions/delivery
                                  │ X-CPA-Management-Key
                   ┌──────────────▼─────────────────────────┐
                   │ CPA · internal/arbitrage                │
                   │   Puller   ──► Store（不可变快照，30s） │
                   │   Reconciler ─► 运行时凭据 + 模型注册   │
                   │   RouterHost ─► 请求选路（<1µs，无 I/O）│
                   └──────────────┬─────────────────────────┘
                                  │ 真实请求
                                  ▼
                       选中 Provider 的 Endpoint
                                  │
                                  └─► usage_events（provider = 原始厂商名）
                                        └─► KEEPER 健康观测 / 漂移重竞价
```

健康数据回流是闭环的关键：运行时凭据与模型都登记在**原始厂商名**（例如 `aliyun`）
之下，因此 `usage_events.provider` 与 KEEPER 的 `provider_prices.provider` 同键，
KEEPER 才能按真实延迟/失败率重新竞价。

## 配置

两个仓库各需要一处配置。

### 1. KEEPER（决策来源）

```bash
COST_ARBITRAGE_ENABLED=true          # 开启竞价引擎（已有开关）
COST_ARBITRAGE_INTERVAL=5m           # 重评估周期，决定快照新鲜度窗口
```

并登记参与竞价的 Provider 与单价（管理面）：

```bash
curl -X PUT http://keeper:4320/api/v1/arbitrage/providers \
  -H 'X-CPA-Usage-Keeper-Request: fetch' \
  -H 'Cookie: <admin session>' \
  -d '{"provider":"aliyun","model":"deepseek-v3","price_per_1m":20,
       "endpoint":"https://aliyun.example.com/v1","auth_ref":"aliyun-prod"}'
```

`auth_ref` 只是**凭据引用名**，不是密钥本身。KEEPER 从不保存明文密钥，报表与下发
响应里也只出现这个名字。

### 2. CPA（决策消费方）

| 变量 | 必填 | 说明 |
| --- | --- | --- |
| `CPA_ARBITRAGE_ENABLED` | ✅ | `true` 开启通道 |
| `CPA_ARBITRAGE_KEEPER_URL` | ✅ | KEEPER 基地址；留空时由 `CPA_BASE_URL` 推导（端口改 4320） |
| `CPA_ARBITRAGE_KEEPER_KEY` | ✅ | 共享管理密钥 `X-CPA-Management-Key`；留空时回退 `CPA_CONTENT_FILTER_AUDIT_KEEPER_KEY` |
| `CPA_ARBITRAGE_AUTH_REFS` | ✅ | `auth_ref=apiKey` 逗号分隔表，把引用名解析为本机凭据 |
| `CPA_ARBITRAGE_INTERVAL_SECONDS` | — | 拉取周期，默认 30 |
| `CPA_ARBITRAGE_STALENESS_FACTOR` | — | 快照可用 = KEEPER 周期 × 本系数，默认 3 |
| `CPA_ARBITRAGE_ALLOWED_ENDPOINT_HOSTS` | — | 可选：下发 Endpoint 的主机名白名单（逗号分隔） |
| `CPA_ARBITRAGE_ALLOW_INSECURE_ENDPOINTS` | — | **仅开发环境**：允许 `http://` 与内网地址 |

`CPA_ARBITRAGE_AUTH_REFS` 只存在于 CPA 进程环境，不落库、不进响应。示例：

```bash
CPA_ARBITRAGE_AUTH_REFS="aliyun-prod=sk-aliyun-xxx,tencent-prod=sk-tencent-yyy"
```

## 安全边界

下发通道决定了**运营方真实 Provider 密钥被发往哪里**，因此当作不可信输入处理：

| 检查 | 默认行为 |
| --- | --- |
| Endpoint 协议 | 仅 `https`（`CPA_ARBITRAGE_ALLOW_INSECURE_ENDPOINTS` 可放开，仅限开发） |
| Endpoint 地址 | 拒绝 loopback / 私有网段 / 链路本地 / `169.254.169.254` 云元数据 |
| Endpoint 内嵌凭据 | 拒绝（`user:pass@host`） |
| 主机白名单 | 配置后只允许名单内主机 |
| 重定向 | 拉取客户端拒绝跟随（Go 会把自定义头带到重定向目标，等于泄露管理密钥） |
| 快照大小 | 8 MiB + 5000 条上限，超出截断并告警 |
| 新鲜度窗口 | 上限 1 小时，KEEPER 声明的 `interval_seconds` 先被钳制到 [5s, 1h] |

未通过校验的 Endpoint **不会**获得凭据、不会注册执行器，该决策静默失效并回落默认选路。

## 可用性语义（重要）

失败一律回落到网关原有选路，**不会让请求失败**：

| 情况 | 行为 |
| --- | --- |
| 通道未配置 / 未启用 | 完全无副作用，路由与上游版本一致 |
| KEEPER 短时不可达 | 保留上一份快照（latch），按新鲜度窗口继续用 |
| 快照过期 | 该模型的决策失效，请求走默认选路 |
| 模型没有决策 | 走默认选路 |
| `auth_ref` 解析不到凭据 | 该 Provider 不参与选路（绝不下发会失败的目标） |
| Endpoint 未通过校验 | 该决策失效，回落默认选路 |
| 价格回退 / 重竞价 | 下一轮 reconcile 移除旧 Provider 的运行时凭据与执行器 |

## 观测

该端点挂在**已鉴权的** `/v0/management` 组内，与其他管理路由同一套管理密钥鉴权
（不会在裸引擎上暴露给任何能访问端口的人）。

```bash
curl http://127.0.0.1:8317/v0/management/arbitrage/status \
  -H "Authorization: Bearer $MANAGEMENT_KEY"
```

返回快照新鲜度、各模型当前决策与运行时凭据数量。响应**不含** `auth_ref` 与任何密钥。

关键字段：`snapshot.fresh` 为 `false` 表示已回落默认选路；`snapshot.failures`
持续增长说明 KEEPER 侧不可达或密钥不匹配。

## 验收口径

| 验收项 | 证据 |
| --- | --- |
| 决策变更 → 上游目标切换 | 状态端点 `decisions[].provider` 变化；上游日志出现新 Endpoint |
| 决策不可用时平滑回落 | 关闭 KEEPER 后请求成功率不变 |
| 缓存路径开销 < 5ms | `go test -bench BenchmarkRouterHost ./internal/arbitrage/`（实测 ~90ns） |
| 账单口径转为实付对比 | KEEPER `usage_events` 出现真实选中 Provider 的用量 |

## 已知边界

- 仅覆盖 OpenAI 兼容协议的 Provider（`base_url` + Bearer key）。OAuth 类
  Provider（Claude / Codex / Gemini 订阅）不参与本通道。
- 快照新鲜度依赖两端时钟大致同步：`generated_at` 超前本地超过 5 分钟会被钳制，
  避免未来时间戳把快照永久钉死为"新鲜"。
- 运行时凭据标记 `runtime_only` 且不带 `Metadata`，因此永不写入磁盘；进程重启
  后由下一轮拉取重新建立。
- `CPA_ARBITRAGE_AUTH_REFS` 里的密钥位于进程环境：同 UID 进程与 `docker inspect`
  可见。生产环境请优先使用 secret 注入而非明文环境变量。
- KEEPER 是「凭据目的地权威」：谁能写决策下发内容，谁就能决定上游密钥发往哪个
  主机。必须把 KEEPER 与其管理密钥视作同等敏感资产。
