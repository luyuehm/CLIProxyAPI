# OpenRouter Quickstart: Model Choice & Fallback Routing

Most new users who start with OpenRouter have a working API call but no
answer to the two questions that actually decide whether the integration
survives production: **which model do I send a given task to**, and **what
happens when that model is rate-limited, down, or over budget**.

This page is that answer. It is a decision matrix, not a model catalog:
six high-frequency scenarios, each with a primary model, a backup, a
fallback, and a ready-to-run `curl` chain plus a Node.js fetch example with
error handling. The full model list (hundreds of entries) stays at
[openrouter.ai/models](https://openrouter.ai/models) — this matrix covers
only what a first integration needs.

## Data currency

- **Data as of: 2026-11-14**, sourced from the live
  [`/models` catalog](https://openrouter.ai/api/v1/models)
  (471 listings). Prices below are in USD per 1M tokens, read from the
  catalog's `pricing` fields.
- Every model id in this matrix is machine-checked against the live catalog
  by `.github/scripts/check-openrouter-matrix.sh` (run in CI). If a model is
  delisted, that check fails and blocks the release — the matrix cannot
  silently rot into dead links.
- Prices and lineups change. Before trusting a number, re-check the model's
  entry at <https://openrouter.ai/models>.

## The matrix

| Scenario | Primary | Backup | Fallback | Why this ordering |
|---|---|---|---|---|
| **Low-cost chat** | `qwen/qwen3.7-flash` | `openai/gpt-5-nano` | `google/gemini-2.5-flash-lite` | Cheapest full-featured tier in the catalog ($0.03/$0.13 per 1M, 1M context). Backup adds a second vendor; fallback stays cheap ($0.10/$0.40) and long-context. |
| **Code generation** | `openai/gpt-5.1-codex` | `qwen/qwen3-coder` | `deepseek/deepseek-chat` | Coding-tuned primary; backup and fallback are code-capable at ~¼ of the primary's output price, so a fallback still ships working code cheaply. |
| **Long context** | `moonshotai/kimi-k3` | `openai/gpt-5.4` | `google/gemini-2.5-pro` | All three expose ~1M-token windows. Kimi K3 leads on input price ($0.19/1M) for corpus-style work; fallbacks keep the chain alive if the primary is delisted. |
| **Low-latency interaction** | `google/gemini-3.6-flash` | `openai/gpt-5-mini` | `anthropic/claude-haiku-5.5` | Flash / mini / haiku are the vendors' latency-optimized lines. Keep `max_tokens` low; all support 400k+ context. |
| **Image input (vision)** | `qwen/qwen3-vl-235b-a22b-instruct` | `google/gemini-3.6-flash` | `openai/gpt-5` | All three declare image input modality in the catalog; primary is a dedicated VL line at the lowest per-1M price. |
| **Multi-turn conversation** | `qwen/qwen-plus` | `moonshotai/kimi-k2` | `deepseek/deepseek-chat` | Long, cheap context for conversation history ($0.26/$0.78 per 1M, 1M window); fallbacks diversify vendors so one outage doesn't end the session. |

Machine-readable copy of this table: [`test/openrouter_matrix.json`](test/openrouter_matrix.json).

### No match? General fallback

If your combination is not on the table (for example "multimodal **plus**
ultra-low-latency"), nothing in this matrix has been benchmarked for it.
Do **not** improvise from this page: compare candidates yourself at
<https://openrouter.ai/models> using the price, context length, and
modality columns, then apply the same fallback-chain pattern below. That
combination is intentionally out of scope for this quickstart — covering
every combination would make the matrix unmaintainable.

## How the fallback chain works

A chain is just an ordered list of model ids plus a loop with error
handling. Two failure classes matter:

1. **Transient** — 429 rate limits and 5xx provider errors: retry the
   current model once or twice with backoff, then move to the next.
2. **Structural** — 404 model-not-found, 400 model-not-available, or a
   response whose `error.message` names the model as missing: skip
   straight to the next model; retrying the same id cannot fix it.

OpenRouter already does per-provider failover *within* one model id
(a `deepseek/deepseek-chat` request rides across DeepSeek's upstream
providers). The chain above adds the next layer: **cross-model** fallback
when the whole model id stops working.

## Examples

### Low-cost chat (primary + fallback, with error handling)

`curl` — the same two-request shape works for every scenario; just swap
the `model` values:

```bash
# Primary model; on 429/5xx or a missing-model error, the fallback runs.
set -u
OPENROUTER_KEY="${OPENROUTER_KEY:?export your OpenRouter API key first}"

call_model() {
  curl -sS -w '\nHTTP_STATUS:%{http_code}' \
    -H "Authorization: Bearer $OPENROUTER_KEY" \
    -H 'Content-Type: application/json' \
    -d '{
      "model": "$1",
      "messages": [
        {"role": "user", "content": "Say hello in one short sentence."}
      ],
      "max_tokens": 128
    }' \
    https://openrouter.ai/api/v1/chat/completions
}

if ! BODY="$(call_model qwen/qwen3.7-flash)"; then
  echo "request failed, falling back to openai/gpt-5-nano" >&2
  BODY="$(call_model openai/gpt-5-nano)"
fi
echo "$BODY"
```

Confirm two things from the response:

- **HTTP 200** — the request succeeded on the primary or a fallback.
- **`model` field in the JSON body** is one of the matrix ids
  (`qwen/qwen3.7-flash`, `openai/gpt-5-nano`, `google/gemini-2.5-flash-lite`
  for this scenario) — OpenRouter echoes the id that actually served the
  request, which is how you verify the fallback fired.

`Node.js` (fetch, same chain logic, structured error handling):

```javascript
const API = "https://openrouter.ai/api/v1/chat/completions";
const CHAIN = ["qwen/qwen3.7-flash", "openai/gpt-5-nano", "google/gemini-2.5-flash-lite"];

async function chat(messages, model, attempt = 0) {
  const res = await fetch(API, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${process.env.OPENROUTER_KEY}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ model, messages, max_tokens: 128 }),
  });

  if (res.ok) return res.json();

  const errBody = await res.json().catch(() => ({}));
  const msg = String(errBody?.error?.message ?? res.statusText);

  // Structural failure: this model id no longer works here. Skip it.
  const structural = res.status === 404 || res.status === 400;
  // Transient failure: retry once, then fall through to the next model.
  if (!structural && res.status === 429 && attempt === 0) {
    await new Promise((r) => setTimeout(r, 1000));
    return chat(messages, model, attempt + 1);
  }

  if (attempt >= 0 && structural && CHAIN[attempt + 1]) {
    console.error(`model ${model} unavailable (${msg}); falling back`);
    return chat(messages, CHAIN[attempt + 1], attempt + 1);
  }
  throw new Error(`all models in chain failed, last error: ${msg}`);
}

const out = await chat(
  [{ role: "user", content: "Say hello in one short sentence." }],
  CHAIN[0],
);
// out.model tells you which model actually served the request.
```

### Other scenarios

Swap the `CHAIN` array (or the `curl` model args) for the scenario's row:

| Scenario | Chain |
|---|---|
| Code generation | `openai/gpt-5.1-codex` → `qwen/qwen3-coder` → `deepseek/deepseek-chat` |
| Long context | `moonshotai/kimi-k3` → `openai/gpt-5.4` → `google/gemini-2.5-pro` |
| Low-latency | `google/gemini-3.6-flash` → `openai/gpt-5-mini` → `anthropic/claude-haiku-5.5` |
| Vision | `qwen/qwen3-vl-235b-a22b-instruct` → `google/gemini-3.6-flash` → `openai/gpt-5` |
| Multi-turn | `qwen/qwen-plus` → `moonshotai/kimi-k2` → `deepseek/deepseek-chat` |

Keep the message history in the `messages` array for multi-turn; the chain
is about *which model serves the next turn*, not about conversation state.

## Where this meets CLIProxyAPI

If you run CLIProxyAPI in front of your upstreams instead of (or alongside)
OpenRouter, the same decisions apply one layer down: its
`openai-compatibility` providers accept OpenRouter's endpoint, and the
round-robin / weighted strategies in `config.example.yaml` spread traffic
across credentials while the model chain above spreads it across models.
Routing visibility — which credential actually served a request and which
fallbacks fired — is documented in [docs/route-detail.md](route-detail.md).

## What this page deliberately does not cover

- An interactive model-comparison dashboard (a BI feature, not a quickstart).
- Live benchmark runs — the matrix is data-driven but static; re-verify
  prices at <https://openrouter.ai/models> before committing budget.
- Persisted user routing-policy management — this is the 0-to-1 starting
  point; strategy lifecycle management is a separate project.
