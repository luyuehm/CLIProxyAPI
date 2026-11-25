## 目标

落地 RIC-3275（OPENROUT-001）「路由决策可解释性」：当主 Provider 超时/报错触发 Fallback 时，非流式 Chat Completion 响应体自带 `metadata.routing_trace`，明确「实际服务的是哪个 Provider、是否发生切换、为什么切换、耗时多少」，让 SDK / 日志管道无需解析响应头、无需开 `?debug=true` 即可做归因。

## 契约

```json
"metadata": {
  "routing_trace": {
    "schema_version": "1.0",
    "provider_id": "anthropic",
    "model": "gpt-5.1",
    "fallback_used": true,
    "switch_reason": "timeout"
  }
}
```

- **`summary`（默认）**：`schema_version` / `request_id` / `provider_id` / `model` / `fallback_used` / `switch_reason`（实测 < 256B）。
- **`full`**：追加 `candidates` / `decision_path` / `attempts`（失败尝试链，上限 16 条）/ `latency_ms`。
- **`off`**：不附加任何字段，响应体逐字节不变。
- **`switch_reason` 词汇表**（消费方按值分支，故钉死）：`none` / `timeout` / `http_<4xx-5xx>` / 引擎 reason code（如 `provider_openai_cooled_down`）/ `provider_error`。

## 关键设计

1. **单一数据源**：trace 由既有的 per-request `RouteInfoCollector` 渲染 —— 与 `X-OpenRouter-Route`、`X-OpenRouter-Fallback`、`X-Fallback-Reason` 以及结构化 fallback 日志同源，三条通道不可能互相矛盾（不可砍项「数据一致性」）。
2. **同目标重试不算 fallback**：失败后同一 provider+model 成功属内部可用性重试，`fallback_used=false`、`switch_reason=none`，与 `X-OpenRouter-Fallback` 判定口径一致。
3. **免费档优雅降级**：`routing-trace.full-trace-api-keys` 非空时，未列名的 key 收到 `summary` 变体（合法响应、不报错、不含尝试链），不是 403/空响应。
4. **作用域收窄**：仅客户端 chat-completion 协议（openai chat / claude messages / gemini content）。Responses API、Interactions API、视频端点、内部 model callback、流式响应均逐字节不变（流式继续只走既有响应头）。
5. **载荷安全**：已有 `metadata` 为非对象时不覆盖；非 JSON 体原样透传；尝试链上限 16；reason code 折控制字符并截断；不含 `auth_index` 等凭据标识。

## 文件

- 新增 `sdk/api/handlers/routing_trace.go`（渲染 / 披露级别解析 / 注入守卫 / switch_reason 词汇表）
- 新增 `sdk/api/handlers/routing_trace_test.go`（18 用例，含 AC1/AC2/AC3）
- 修改 `sdk/api/handlers/handlers_execution.go`（两条非流式路径接线，`routingTraceRequested` 协议门控）
- 修改 `sdk/api/handlers/route_info.go`（collector 记录 `startedAt` 供 `latency_ms`）
- 修改 `internal/config/sdk_config.go` + `config_load.go` + `parse.go`（`routing-trace` 配置块、mode 校验）
- 新增 `internal/config/routing_trace_test.go`（5 用例）
- 新增 `docs/routing-trace.md`（Schema + 词汇表 + 安全约束）；`docs/route-detail.md` 交叉引用；`config.example.yaml` 配置块

## VERIFY（实测）

```
$ go build ./...                                        # BUILD_ALL_OK
$ go test ./sdk/api/handlers/... ./internal/config/ -count=1
ok  sdk/api/handlers            0.46s
ok  sdk/api/handlers/claude     0.27s
ok  sdk/api/handlers/gemini     0.37s
ok  sdk/api/handlers/openai     9.87s
ok  internal/config             0.20s
```

**范围回归**：全量 handlers 套件（含 Responses compact / Interactions / model_execution）在一次改宽协议门控后确实翻红 5 例，收窄为 chat-completion 协议后全绿 —— 证明「其它端点逐字节不变」这条不变式是被测试钉住的，不是口头承诺。

**变异证明 ×11**（sed 翻转后跑门禁，均见红，`git checkout --` 还原后全绿）：

| 变异 | 翻红用例数 |
|---|---|
| `fallbackUsed := false`（切换检测失效） | 3 |
| `switch_reason` 恒为 `none` | 2 |
| full 白名单被绕过（所有 key 都得 full） | 1 |
| `off` 模式失效 | 1 |
| 协议门控放宽（Responses/Interactions 也被注入） | 3 |
| 未知 mode 在加载期放行 | 3 |
| `timeout` 词汇表降级为 `provider_error` | 2 |
| 尝试链上限移除 | 1 |
| reason code 控制字符折换移除 | 2 |

> 其中两项首轮变异表现为 0 翻红，原因是删除分支导致 `fmt` / `unicode` 变成未使用导入、包编译失败（无 `--- FAIL` 行）。改为保留导入引用的等效变异后分别翻红 3 / 2 例，确认不变式真实被钉。

## NEXT_STEP

- 人工评审并合并。`done` 验收以 `git ls-tree origin/main sdk/api/handlers/routing_trace.go` 命中 + 本 PR `mergedAt` 非空为准。
- 默认即 `summary`（开箱可得 provider 归因）；需要尝试链的付费租户在 `routing-trace.full-trace-api-keys` 配置其 key。
- 后续可选（本期 Out of Scope）：控制台 Routing Event 视图、把 trace 写入 KEEPER 台账做跨请求审计。

Closes RIC-3275

🤖 Generated with [Claude Code](https://claude.com/claude-code)
