# 私有端点强制路由（OPENROUT-002）

本页说明 CLIProxyAPI（CPA 数据面）如何在 Provider 路由策略中强制执行
**数据边界**：企业使用 Private Endpoints（自有云账号或专有部署）时，可让
路由层**只在已批准的私有端点集合内选路**，即便公共 Provider 可用也绝不
跨网络发送请求。

> 该约束是**请求级硬过滤**。不传 `require_private` 时，选路行为与未改动
> 版本完全一致，不引入任何额外限制。

## 为什么需要它

自动路由按可用性/成本/延迟在 Provider 间做选择，私有端点不可用时会
fallback 到公共 Provider。对受合规约束的企业（数据不得离开 VPC/专有
网络），"宁可失败也不能外泄"必须优先于请求成功率。此前路由引擎没有
"必须私有"的硬约束，客户无法把合规边界置于可用性之上。

## 契约

### 1. 请求参数 `require_private`

请求级开关（对应 `fallback.RequestOptions.RequirePrivate`）。设为 `true`
时：

- 路由评分**之前**先做硬过滤：仅 `private: true` 的候选端点可被放行，
  其余 allow/ignore/order 等排序参数只作用于剩余端点；
- 过滤后没有任何可用私有端点 → 请求直接失败，**不发起任何公共
  Provider 调用**，错误体 `code: NO_PRIVATE_ENDPOINT`（HTTP 503）；
- 响应头 `x-openrouter-private-only: true` 标记该请求处于数据边界模式。

### 2. Provider 候选标记 `private`

策略配置（`fallback.Config` / `fallback.Policy`）中的候选端点可声明
私有属性：

```yaml
providers:
  - provider: acme-vpc-ep      # 自有云账号的私有端点
    private: true
  - provider: public-llm       # 公共 Provider
```

### 3. 行为矩阵（验收场景）

| 场景 | require_private | 私有端点可用 | 结果 |
|------|:---:|:---:|------|
| 无可用私有端点 | true | 否 | HTTP 503，`code: NO_PRIVATE_ENDPOINT`，零公共调用 |
| 私有端点正常 | true | 是 | 成功，`x-openrouter-provider` 为该私有端点 ID |
| 私有端点超时 + 配置了公共 fallback | true | 是→超时 | 失败，**不进入公共 fallback**，`x-openrouter-private-only: true` |
| 未传该字段 | 未设置 | — | 保持原有路由行为，不引入额外限制 |

### 4. 优先级规则（已知兔子洞的处置）

- `require_private=true` 是**硬约束**：先于 quality floor、cost limit、
  健康退避与任何排序参数生效；其他参数只在存活的私有端点集合内起
  作用。
- 私有端点命名与多可用区冲突：当前使用平台内部 endpoint ID，字段语义
  后续可能标准化，以本节声明为准。
- 错误优先级：候选集合中**存在**私有端点但被 cost/quality 约束排除
  时，报告对应的约束错误（cost_limit_exceeded 等）；**不存在**私有
  端点时报告 `NO_PRIVATE_ENDPOINT`。

## 实现位置

- `internal/fallback/policy.go` — `Candidate.Private`、
  `RequestOptions.RequirePrivate`、`ErrNoPrivateEndpoint`
  （stable code `no_private_endpoint`，映射 `NO_PRIVATE_ENDPOINT`）
- `internal/fallback/orchestrator.go` — 硬过滤接入选路循环，
  `Outcome.PrivateOnly` 驱动响应头
- `internal/fallback/config.go` — `ProviderRef.Private` 声明式配置

网关请求路径的接线（HTTP 参数解析 → RequestOptions、503/响应头映射）
见本单的 NEXT_STEP。
