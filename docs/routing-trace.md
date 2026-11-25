# Routing Trace Response Contract (`metadata.routing_trace`)

Automatic routing is a black box by default: when the primary provider times out
or errors, the gateway transparently switches to the next candidate, and the
response body names only the model — never which provider actually served the
request, whether a switch happened, or why. That makes a provider-dependent
output difference indistinguishable from a model-quality difference, and it
makes an incident unreproducible.

`routing_trace` closes that gap in the **response payload** itself, so an SDK or
a log pipeline can attribute every completion without parsing headers and
without enabling a debug switch (RIC-3275, OPENROUT-001).

## What it is

A non-streaming chat-completion response carries one additional object:

```json
{
  "id": "chatcmpl-abc123",
  "object": "chat.completion",
  "model": "gpt-5.1",
  "choices": [ ... ],
  "metadata": {
    "routing_trace": {
      "schema_version": "1.0",
      "request_id": "req_01J8Z8Y6Q4",
      "provider_id": "anthropic",
      "model": "gpt-5.1",
      "fallback_used": true,
      "switch_reason": "timeout"
    }
  }
}
```

## Scope

| Path | Carries `routing_trace` |
|---|---|
| `POST /v1/chat/completions` (OpenAI chat) | yes |
| `POST /v1/messages` (Claude messages) | yes |
| Gemini content generation | yes |
| `POST /v1/responses` (Responses API) | no |
| Interactions API | no |
| Video / image generation endpoints | no |
| Streaming responses (`stream: true`) | no — headers only |
| Internal host model callbacks | no |

Streaming clients keep the existing always-on headers (`X-OpenRouter-Route`,
`X-OpenRouter-Fallback`, `X-Fallback-Reason`): the trace is a body contract and
the body cannot be mutated after the first chunk is committed. The trace is
rendered from the same per-request collector that feeds those headers and the
structured fallback log, so all three channels report the same decision — they
cannot disagree.

The trace is additive: it never changes a status code, an error body, or a
streaming frame. On a terminal failure the existing error contract is unchanged
(`attempted_models` on a 503 from the fallback engine).

## Fields

### `summary` (default)

| Field | Type | Meaning |
|---|---|---|
| `schema_version` | string | Payload shape version. A breaking change bumps it. |
| `request_id` | string | Inbound request ID (absent when the caller did not supply one). |
| `provider_id` | string | The provider whose credential actually served the request. On the error path unreachable (see Scope). |
| `model` | string | The upstream model identifier used by the serving attempt. |
| `fallback_used` | bool | `true` when a failed attempt preceded the serving attempt **and** the target moved. |
| `switch_reason` | string | `none` when no switch happened; otherwise a bounded code (see below). Always present. |

### `full`

Adds the detailed decision trail:

| Field | Type | Meaning |
|---|---|---|
| `candidates` | string[] | The ordered provider list resolved for the request. |
| `decision_path` | string[] | How the provider was chosen: `["primary"]`, `["model_router"]`, `["primary","fallback"]`, … |
| `attempts` | object[] | The failed attempts, in time order (bounded to 16). |
| `latency_ms` | int | Wall-clock time from request admission to the response, in milliseconds. |

Each `attempts` entry:

| Field | Type | Meaning |
|---|---|---|
| `provider_id` | string | The provider of the failed attempt. |
| `model` | string | The upstream model used for the attempt. |
| `status` | int | The upstream HTTP status (`0` when unknown). |
| `reason` | string | The bounded switch reason for this attempt. |

## `switch_reason` vocabulary

Consumers branch on these values, so the vocabulary is deliberately small and
stable:

| Value | Meaning |
|---|---|
| `none` | No switch: a direct success on the preferred provider. |
| `timeout` | A deadline / 504-class failure (context deadline, upstream timeout). |
| `http_<code>` | An upstream HTTP status in 400–599, e.g. `http_500`, `http_429`. |
| `<engine code>` | The fallback engine's bounded reason when no status applies, e.g. `provider_openai_cooled_down`. |
| `provider_error` | A transport-level failure with no status and no engine code. |

A failure followed by a success on the **same** provider and model is an
internal availability retry, not a routing switch: `fallback_used` stays
`false` and `switch_reason` stays `none`. This matches `X-OpenRouter-Fallback`,
so a monitor cannot see the header and the body disagree.

## Disclosure levels

Configured under `routing-trace.mode`:

| Mode | Payload |
|---|---|
| `off` | No `routing_trace` field at all. |
| `summary` (default) | The summary fields above — small enough that the trace cannot meaningfully change network cost for a high-volume caller (well under 256 bytes). |
| `full` | Summary plus the detailed trail. |

`routing-trace.full-trace-api-keys` narrows `full` to a listed set of client
API keys:

- a key in the list receives the `full` variant;
- every other key receives the `summary` variant — a valid response, never an
  error and never the attempt trail, so a free tier degrades gracefully;
- when the list is empty, every authenticated key follows `mode`.

Keys are compared verbatim against the inbound `Authorization: Bearer`,
`X-Api-Key` or `X-Goog-Api-Key` credential (already authenticated by the access
layer) and are never logged, echoed, or included in the response.

```yaml
routing-trace:
  mode: full
  full-trace-api-keys:
    - sk-enterprise-tenant-a
```

## Safety

- **Existing metadata is preserved.** The trace merges into a sibling
  `metadata` object; when an upstream already returned a non-object `metadata`
  value, the body is left untouched rather than overwritten.
- **Non-JSON bodies are untouched.** A non-object payload (a plain-text or
  array response produced by an interceptor) passes through byte-for-byte.
- **Bounded payload.** The attempt list is capped at 16 entries and reason
  codes are sanitized (control characters folded to `_`, length capped), so a
  pathological retry loop or a hostile upstream code cannot bloat or corrupt
  the response.
- **No credential material.** `provider_id` is the provider registration name;
  `auth_index` (a credential identifier) is never included.

## Where the numbers come from

The trace is rendered from the same credential-level route events that drive
`route_info` (`?debug=true`) and the headers, so it inherits their semantics:

- `provider_id` is the provider whose credential completed the request;
- `attempts` are cross-credential attempts within the resolved provider list,
  not a re-derivation from logs;
- cost and billing are unaffected — the trace is attribution metadata only.

## Implementation pointers

- `sdk/api/handlers/routing_trace.go` — the payload builder, disclosure-level
  resolution, `switch_reason` vocabulary, and injection with its guards.
- `sdk/api/handlers/handlers_execution.go` — wiring on the two non-streaming
  chat-completion paths, gated by `routingTraceRequested` (protocol scope).
- `internal/config/sdk_config.go` — `RoutingTraceConfig`, mode validation, and
  the `routing-trace` YAML block.
- `sdk/api/handlers/route_info.go` — the per-request collector that also feeds
  `route_info`, `X-OpenRouter-Route`, `X-OpenRouter-Fallback`, and the
  structured fallback log.
