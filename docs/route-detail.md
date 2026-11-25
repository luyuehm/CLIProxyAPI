# Routing Decision Visibility

## On-by-default fallback marker (`X-OpenRouter-Fallback`)

Every API response — streaming and non-streaming — now carries an always-on
`X-OpenRouter-Fallback` response header **when a fallback actually occurred**
during the request: one model/provider attempt failed and a different model or
provider served the request.

```
X-OpenRouter-Fallback: modelA->modelB (reason: upstream_503)
```

The value is the compact `failedModel->servingModel` pair plus the sanitized
failure reason of the first failed attempt. The reason is a bounded
machine-readable code (e.g. `upstream_503`) when one exists, otherwise a
capped, client-safe description. Both models are stripped of any non-printable
ASCII so a client-supplied name can never inject a header line.

The header is **absent** when:

- **No fallback happened** — a direct success on the first attempt (no
  fallback configuration to report).
- **Every candidate failed** — the request returned an error; the error body
  carries the final error, so the header would not add information.
- **An internal credential retry** — a failure followed by a success on the
  *same* model and provider is an internal availability retry, not a routing
  fallback. Only a move to a different model or provider emits the marker.

Streaming responses carry the header too: the routing outcome is known before
the first chunk is committed (bootstrap retries resolve before headers are
sent), so the marker reaches the client through the response header of the
stream.

The header is exposed via CORS so browser clients can read it.

## Structured fallback log

Every request that fell back also emits a structured log line carrying the
failed chain and the model that served the request:

```
route: fallback event served  served_model=modelB fallback_chain=[{model:modelA status:503 code:upstream_503 reason:upstream_503}]
```

When the whole chain failed, the line is emitted at WARN level with no
`served_model`, so a production incident has an auditable trail of every
failed candidate and the final reason.

## On-by-default response body contract (`metadata.routing_trace`)

The headers above answer "which provider served this request" for clients that
can read headers. Non-streaming chat-completion responses additionally carry
the same decision **in the body**, so an SDK or log pipeline can attribute a
completion without header parsing and without the `?debug=true` switch:

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

`routing-trace.mode` selects `off` / `summary` (default) / `full`, and
`routing-trace.full-trace-api-keys` narrows the detailed variant to listed
client keys (every other key receives the summary variant, never an error).
The field is scoped to client-facing chat-completion protocols: the Responses
API, the Interactions API, video endpoints, internal model callbacks, and
streaming responses are byte-for-byte unchanged and keep the headers. The full
schema, the bounded `switch_reason` vocabulary, and the safety guards are in
[routing-trace.md](routing-trace.md).

## Debug routing audit (`?debug=true`)

Automatic routing is a black box by default: a response names only the model,
never the provider, the credential, or why a fallback happened. This makes it
hard to attribute a cost spike or a latency shift to a concrete routing
decision. Non-streaming responses now carry a compact always-on
`X-OpenRouter-Route` header, and the `?debug=true` routing audit exposes the
full decision trail on demand without changing the default body.

## Always-on non-streaming header

Every non-streaming API response, including error responses, carries an
`X-OpenRouter-Route` response header. Its value is a JSON object:

```json
{
  "provider": "openai",
  "model": "gpt-5.1",
  "fallback_chain": ["anthropic", "openai"],
  "failure_code": "upstream_503",
  "failure_reason": "upstream overloaded"
}
```

| Field | Type | Meaning |
|---|---|---|
| `provider` | string | The provider whose credential served the request. On a terminal failure it is the last provider attempted. |
| `model` | string | The upstream model identifier used for the final attempt. |
| `fallback_chain` | string[] | Ordered unique provider attempts when a fallback occurred. `[]` when nothing failed. |
| `failure_code` | string | A short machine-readable failure code, e.g. `upstream_503`. Present only on error responses. |
| `failure_reason` | string | A sanitized, client-safe one-line description of the terminal failure. Present only on error responses. |

The header is exposed via CORS so browser clients can read it. It does not
change the response body and it is not emitted on streaming responses.

## Enabling

Append the `debug` query parameter to the request. Only explicit truthy values
(`1`, `true`, `yes`, `on`, case-insensitive) enable reporting; a stray
`?debug=` is a no-op.

```
POST /api/v1/chat/completions?debug=true
```

When the flag is off, no routing metadata is added to the response body and
the body is byte-for-byte identical to a request without the flag. The
`X-OpenRouter-Route` header is still present on non-streaming responses. The
full audit is strictly opt-in: the route_info reporter is created only for
debug-flagged requests, and the conductor's reporting helper returns
immediately when no reporter is registered, so non-debug requests pay no
bookkeeping cost beyond the always-on header.

## Where the payload lands

The same `route_info` object reaches the client through two channels, chosen
by response type:

- **Non-streaming** responses carry `route_info` as a top-level field in the
  JSON response body.
- **Streaming** responses cannot mutate the body after the first chunk is
  committed, so the same payload is delivered in the `X-Route-Detail` response
  header. The header is exposed via CORS so browser clients can read it.

## `route_info` fields

| Field | Type | Meaning |
|---|---|---|
| `request_id` | string | The inbound request ID, so the trail can be correlated to logs. |
| `final_provider` | string | The provider whose credential actually served the request. |
| `selection_reason` | string | A human-readable, always-non-empty explanation of how the final provider was chosen (primary, single-candidate, model-router routing, or a fallback after N failed attempts). |
| `candidates` | string[] | The ordered provider list resolved for the request. |
| `fallback_events` | object[] | The failed credential attempts, in time order. Empty when nothing failed. |

Each entry in `fallback_events` is:

| Field | Type | Meaning |
|---|---|---|
| `failed_provider` | string | The provider of the failed attempt. |
| `auth_index` | string | The credential index that was attempted. |
| `model` | string | The upstream model used for the attempt. |
| `status` | int | The upstream HTTP status observed for the failure (`0` when unknown). |
| `code` | string | A short machine-readable failure code, e.g. `upstream_503`. |
| `error` | string | A sanitized, one-line, client-safe description of the failure. |
| `timestamp` | string | The wall-clock time the outcome was observed (UTC, millisecond precision). |

## Semantics

- **Credential-level, not cross-provider, fallback.** Within one provider the
  conductor retries with a different credential; that shows up as a
  `fallback_events` entry whose `auth_index` differs while `failed_provider`
  stays the same. Cross-provider routing is handled by a separate model-router
  channel and is reported through `candidates` plus a `selection_reason` that
  names the router.
- **Fallback events are time-ordered.** They are emitted by the conductor as
  attempts occur and rendered in chronological order, so a retry trail reads
  top-to-bottom.
- **The list is bounded.** Up to 16 fallback events are reported per request so
  a pathological retry loop cannot bloat the response.
- **Billing is unaffected.** Routing metadata is an audit of which credential
  served a request; it does not change how that request is priced or billed.
  The cost of a fallback is still whatever the serving credential's pricing is
  — the metadata just makes that attribution possible.

## Safety

Failure text is sanitized the same way the conductor sanitizes its own log
diagnostics: credential material is redacted and the result is capped, so an
upstream body or a secret never reaches the payload. Upstream response bodies
are not included.

## Implementation pointers

- `sdk/cliproxy/executor/route_info.go` — the reporter interface and event type.
- `sdk/cliproxy/auth/route_reporting.go` — the conductor's reporting helper.
- `sdk/api/handlers/route_info.go` — the collector, debug-flag parsing,
  `X-OpenRouter-Route` and `X-OpenRouter-Fallback` rendering, JSON/header
  injection, and the structured fallback log.
- `sdk/api/handlers/handlers_execution.go` — always-on collector wiring and
  route/fallback header attachment on the non-streaming paths.
- `sdk/api/handlers/handlers_stream.go` — fallback header attachment and
  fallback logging on the streaming paths.
- `internal/api/server_middleware.go` — CORS exposure of `X-Route-Detail`,
  `X-OpenRouter-Route`, and `X-OpenRouter-Fallback`.

## Automatic provider fallback (`X-Fallback-Reason` + `attempted_models`)

When the gateway's automatic provider fallback engine (RIC-3143, OPENROUT-001)
is enabled via the `fallback` config block, a request whose preferred provider
fails is transparently retried on the next provider in the configured chain.
The degraded response carries a bounded, machine-readable reason header that is
complementary to `X-OpenRouter-Fallback`:

```
X-Fallback-Reason: provider_openai_503
```

| Header | Meaning |
|---|---|
| `X-Fallback-Reason` | The engine's bounded reason code for the switch that served the request, e.g. `provider_openai_503`, `provider_openai_cooled_down`, `timeout_budget_exhausted`, `provider_openai_error`. Present only on a success that was served by a provider other than the preferred one. |
| `X-OpenRouter-Fallback` | The model pair that moved (`failedModel->servingModel`) plus a sanitized reason, from the credential-level routing trail. |

The two headers are complementary, never merged: `X-Fallback-Reason` names
**why** the request degraded (the engine reason), `X-OpenRouter-Fallback` names
**what** moved (the model pair). A direct success on the preferred provider
carries **neither** header, so a monitor can alert on the presence of
`X-Fallback-Reason` without parsing its value.

### Terminal failure (`attempted_models`)

When every provider in the chain fails (or the chain is exhausted), the request
returns `503` and the error body carries the providers actually attempted, in
attempt order, deduplicated:

```json
{
  "error": {
    "message": "all providers unavailable after automatic fallback (reason: provider_c_503)",
    "type": "server_error",
    "code": "provider_unavailable",
    "attempted_models": ["openai", "anthropic", "gemini"]
  }
}
```

`attempted_models` is the AC3 contract: it names the chain the request walked so
a client or monitor knows which providers were tried before the failure. It is
absent on a request that failed without falling back (a single-provider chain
keeps its native status and error body).

### Health signal and recovery

The engine's health signal is the `HealthWindow` (5-minute sliding window,
exponential backoff cooldown, success clears the state). A provider is skipped
while cooling; when it recovers (a success clears the cooldown, or the cooldown
expires) the next request is routed to it directly and `X-Fallback-Reason`
disappears. The gateway does **not** gate on an error-rate threshold: any
provider-level failure (5xx, timeout) during a request triggers the switch —
this is more aggressive than the "50% error rate" wording in the parent issue
and is the intended behavior.

### Concurrency

100 concurrent requests hitting the same failing provider produce at most 2
distinct switch decisions — the health window's cooldown/saturation reset bounds
the target set, so the engine does not flap under concurrent load. The engine is
safe under the `-race` detector.

### Configuration

The engine is enabled with a `fallback` block in the config:

```yaml
fallback:
  enabled: true
  providers:
    - provider: openai
    - provider: anthropic
      model: claude-3-5-sonnet   # optional per-candidate model override
    - provider: gemini
  cost-limit-usd: 0.01          # optional hard per-request cost ceiling
  quality-floor: 1.0            # optional minimum quality index
  timeout-budget: 30s           # optional total wall-time budget across attempts
  backoff-window: 5m            # health window (default 5m)
  backoff-base: 5s              # first backoff cooldown (default 5s)
  backoff-max: 5m               # cooldown cap (default 5m)
```

The first provider is the preferred one. An empty or disabled block leaves the
gateway's existing credential-level failover as the only behavior. The chain is
intersected with any KEEPER route boundary so fallback never leaves a key's
allowed provider set.

## Implementation pointers (RIC-3143)

- `internal/fallback/` — the pure engine (`Orchestrator`, `HealthWindow`,
  `Policy`/`Config`), unchanged.
- `sdk/api/handlers/fallback_bridge.go` — the adapter that runs one request
  through the engine, renders `X-Fallback-Reason` and `attempted_models`, and
  threads the per-request outcome to the headers/error body.
- `sdk/api/handlers/handlers_execution.go` — engine wiring on the non-streaming
  paths.
- `sdk/api/handlers/handlers_stream.go` — engine wiring on the streaming paths.
- `internal/fallbackruntime/setup.go` — production installation from the
  config block via the pre-reserved `WithRouterConfigurator` hook.
- `internal/config/config_types.go` — the `FallbackConfig` declarative block.
