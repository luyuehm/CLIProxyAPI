# Stream Resume

Long generations break. A network flap, a client-side timeout, or a proxy
restart tears down the SSE connection mid-answer, and the client is left with a
truncated response and no way to ask for the rest. This feature lets a client
continue a streaming response from the last event it actually processed,
without restarting the request or paying for the tokens again.

It is opt-in and transparent: requests that do not ask for it behave exactly as
before.

## Protocol

### Opening a resumable stream

Send the normal streaming request with one extra header:

```http
POST /v1/chat/completions
Authorization: Bearer <key>
X-Stream-Resume: 1
Content-Type: application/json

{"model": "gpt-4o", "stream": true, "messages": [...]}
```

The gateway then:

- returns an unguessable stream identifier in `X-Stream-Id`;
- tags every SSE event with a monotonically increasing `id:` field;
- keeps generating into a detached session if the client's connection drops.

```
X-Stream-Id: 3vXr...9Q

id: 1
data: {"choices":[{"delta":{"content":"Hel"}}]}

id: 2
data: {"choices":[{"delta":{"content":"lo"}}]}
```

### Resuming

Re-send the same request, replacing the opt-in header with the identity of the
stream and the last event you processed:

```http
POST /v1/chat/completions
Authorization: Bearer <key>
X-Stream-Id: 3vXr...9Q
Last-Event-ID: 500
```

The response is an SSE stream carrying events `501` onward, then continuing
live if the generation is still running. Events already received are never sent
again, and no event is skipped, so the concatenation of both halves is exactly
the response the client would have received without the interruption.

`X-Last-Event-Id` is accepted as an alias for `Last-Event-ID` for clients that
cannot set the latter.

### Responses on a resume attempt

| Status | Meaning | Client action |
| --- | --- | --- |
| `200` | Resuming; body carries the missing events | Read the stream |
| `403` | `stream_forbidden` — the stream belongs to a different API key | Restart the request |
| `409` | `stream_resume_gap` — the breakpoint was evicted from the retention window | Restart the request |
| `410` | `stream_not_found` — unknown or expired stream | Restart the request |
| `429` | Too many concurrent streams for this key | Retry, or resume an existing one |

A `403` is returned before any buffered byte is written, and its body is
identical for every mismatch, so a resume attempt reveals neither another key's
content nor whether it existed.

## Guarantees

- **No gap, no duplicate.** The gateway assigns event ids itself. Any `id:` field
  an upstream provider already emitted is replaced, so the sequence a client
  sees is always the gateway's.
- **Data consistency.** If the requested breakpoint has fallen out of the
  retention window the gateway refuses (`409`) rather than silently skipping
  frames.
- **Ownership.** A session is bound to the API key that opened it. Resuming
  requires presenting a key with the same fingerprint; the plaintext key is
  never stored.
- **Cancellation.** A resumable request runs on a context detached from the
  client connection, so a disconnect does not abort the upstream generation.
  Non-resumable requests keep the original behaviour: a disconnect cancels the
  upstream.

## Retention and limits

| Knob | Default | Meaning |
| --- | --- | --- |
| idle TTL | 2 min | How long a session with no reader is kept after its last event |
| per-session window | 1 MiB | Retained events per stream; older ones are dropped |
| sessions per key | 16 | Concurrent retained streams per API key |
| total sessions | 1024 | Across the whole instance |

The TTL is an **idle** window, not an absolute deadline. A generation that keeps
producing events is never reclaimed, so a long task stays resumable for as long
as it is running; the window closes only after production stops and no client
has reattached. This is what keeps long tasks from falling off the retention
cliff: nothing expires while it is still moving, and the byte window keeps
memory bounded at the same time.

When a key reaches its per-key cap the oldest session with no attached reader is
evicted; if every session is still in flight the request is rejected with `429`.
Sessions are never evicted while a producer still holds them or a reader is
attached.

## Scope

Supported on the streaming endpoints the gateway serves:

- `POST /v1/chat/completions`
- `POST /v1/completions`
- `POST /v1/messages`
- `POST /v1/responses`

Out of scope in this revision:

- **Resume across gateway instances.** Sessions live in the instance that
  produced them. A restart or a load-balanced hop to another instance loses
  them, which surfaces as `410`. Sharing sessions across instances needs a
  distributed store and is tracked separately.
- **Provider-side resume.** The continuation always re-runs the upstream request
  from the gateway's retained events; it does not ask the provider to resume from
  an offset. Providers that stream deterministically are unaffected; a provider
  whose output is non-deterministic makes the *replayed* portion the gateway's
  retained copy rather than a second model call for those events.
- **Cross-device resume.** A resume is only meaningful with the same API key.
- **WebSocket transports** (`/v1/realtime`, Codex websocket sessions). They
  already carry their own session semantics.

## When auth is disabled

Binding a stream to its API key only has meaning when the gateway is configured
with an authentication provider. With no provider configured every principal is
empty, so all clients share one fingerprint and the confidentiality boundary
rests on the stream identifier's 256 bits of entropy instead. Enable
authentication before exposing resume to untrusted clients.

## Implementation

| Path | Role |
| --- | --- |
| `internal/api/middleware/stream_resume.go` | Route middleware: tee output, serve resumes, status codes |
| `internal/proxy/session.go` | Per-stream retained frames, subscriptions, ownership |
| `internal/proxy/registry.go` | Session index, key fingerprinting, caps, idle sweep |
| `internal/proxy/sse.go` | Frame splitting, event id assignment, `Last-Event-ID` parsing |

The middleware decides frame boundaries rather than relying on the handler,
so it works identically whether the handler writes one frame per call or many.
`internal/proxy` also contains an earlier upstream-stall recovery engine
(`handler.go`, `internal/stream`); it is a separate mechanism and is not part of
this feature.
