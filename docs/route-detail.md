# Routing Decision Visibility (`?debug=true`)

Automatic routing is a black box by default: a response names only the model,
never the provider, the credential, or why a fallback happened. This makes it
hard to attribute a cost spike or a latency shift to a concrete routing
decision. The routing audit exposes that decision trail on demand, without
changing the default response.

## Enabling

Append the `debug` query parameter to the request. Only explicit truthy values
(`1`, `true`, `yes`, `on`, case-insensitive) enable reporting; a stray
`?debug=` is a no-op.

```
POST /api/v1/chat/completions?debug=true
```

When the flag is off, no routing metadata is produced and the response body is
byte-for-byte identical to a request without the flag. The audit is strictly
opt-in: a reporter is created only for debug-flagged requests, and the
conductor's reporting helper returns immediately when no reporter is
registered, so non-debug requests pay no bookkeeping cost.

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
- `sdk/api/handlers/route_info.go` — the collector, debug-flag parsing, and
  JSON/header injection.
- `internal/api/server_middleware.go` — CORS exposure of `X-Route-Detail`.
