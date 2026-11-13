# Gateway Auto-Tracing (RIC-2508)

The gateway traces every model call it executes, with no tracing setup in
your code. Applications that call models through CLIProxyAPI get a trace
span per call in KEEPER's trace list automatically — no SDK import, no
explicit instrumentation, no response-latency impact.

## How it works

```
 client ──▶ CLIProxyAPI (gateway) ──▶ upstream provider
                │
                │ 1. W3C traceparent adopted from the request, or generated
                │ 2. every model call publishes a usage record
                │ 3. the tracer channel converts it to one trace span
                ▼
        KEEPER  POST /api/v1/traces/ingest
                │
                ▼
        KEEPER trace list / decision graph / eval dataset
```

- **Adopt or generate.** If the request carries a valid W3C `traceparent`
  header (anything instrumented with OpenTelemetry does this for you), the
  gateway joins that trace: the span is parented under the incoming span
  and the trace identity is echoed back on the response. Without one, the
  gateway generates a fresh trace, so a plain OpenAI/Anthropic SDK call
  through the gateway still produces a span.
- **Zero business code changes.** The conversion from a finished model call
  to a trace span happens inside the gateway from data it already records
  (model, provider, tokens, latency, status, session hierarchy). Your
  application code is untouched.
- **Async, off-path reporting.** Spans are delivered to KEEPER by a
  background worker on a bounded queue. A slow or down KEEPER never blocks,
  delays or fails the model response; failed reports enter a local retry
  buffer and replay when KEEPER is reachable again.

## Enabling

All switches are environment variables on the gateway process; nothing is
turned on by default.

| Variable | Meaning | Default |
|---|---|---|
| `CPA_TRACE_ENABLED` | Master switch (`true`/`1`/`on` …) | off |
| `LANGSMITH_TRACING` | Alias: `true` enables the channel too | off |
| `CPA_TRACE_KEEPER_URL` | KEEPER base URL for the ingest endpoint | derived from `CPA_BASE_URL` (port → 4320) |
| `CPA_TRACE_KEEPER_KEY` | Shared `X-CPA-Management-Key` value | falls back to `CPA_CONTENT_FILTER_AUDIT_KEEPER_KEY` |
| `CPA_TRACE_INGEST_TOKEN` | Dedicated KEEPER ingest token (`X-Trace-Ingest-Token`), when KEEPER runs ingest behind `TRACE_INGEST_TOKEN` | unset |
| `CPA_TRACE_QUEUE_SIZE` | In-flight report queue depth | 1024 |
| `CPA_TRACE_RETRY_BUFFER` | Retry-buffer depth for failed reports | 256 |

The channel only becomes active when **both** an enable switch and a KEEPER
source are configured; otherwise the gateway runs exactly as an unconfigured
build (no reporting goroutine, no added headers on the reporting path).

## What gets reported

One span per model call, KEEPER ingest contract (`POST /api/v1/traces/ingest`):

- trace head: `trace_id` (W3C trace-id), `request_id`, `session_id`,
  started-at
- span: deterministic id, name `llm:<model>`, kind `llm`, status
  `ok`/`error`, model, provider, duration, input/output/cache tokens,
  error message (capped at 512 chars)
- attributes: provider, executor, auth index/type, source, service tier,
  reasoning effort, session hierarchy, endpoint, stream flag

No request/response bodies and no credentials are ever sent — the payload is
call metadata only, and every attribute key is checked against KEEPER's
sensitive-key filter before it leaves the gateway.

## Framework integration

Anything that already produces W3C trace context joins the gateway's traces
automatically; anything that just calls the gateway gets a trace without
context.

| Integration | What you do | Result |
|---|---|---|
| Plain OpenAI / Anthropic / OpenAI-compatible SDK, through the gateway | nothing | one trace span per call, auto-generated trace id |
| OpenTelemetry-instrumented apps (Python/JS OTel, LangChain/LangGraph, LangSmith SDK, Langfuse, CrewAI, Vercel AI SDK, Pydantic AI) | point the SDK base URL at the gateway (and set the OTel/SDK tracing vars you already use) | your app's trace id and parent span are adopted; the gateway's call is a child span in your existing trace |
| KEEPER eval dataset pipeline | nothing extra | auto-traces flow into `traces → 评估数据集` (RIC-2479) sampling/export without manual collection |

## Known boundaries

- **Reported, not billed.** The trace channel is best-effort observability;
  the authoritative usage/billing record is KEEPER's usage events. A lost
  trace never affects what you are billed for.
- **Process-local retry buffer.** A gateway restart loses pending (not
  yet delivered) reports. Delivered traces are durable in KEEPER.
- **Span granularity is one span per model call.** Multi-step agent runs
  compose into a trace via the session hierarchy and W3C parent linkage;
  tool-call sub-spans are out of scope for this channel.
