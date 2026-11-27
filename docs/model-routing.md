# Request-level model routing (RIC-3351)

Wires the `internal/modelrouting` strategy engine into the gateway request path
and exposes the strategy objects over the management API. The engine itself —
the frozen `Strategy` schema, the immutable per-scope version history, the daily
KEEPER price cache and the decision engine — was delivered by RIC-2522 /
RIC-2524 / RIC-2525. This channel is the adapter that makes it reachable.

Before this channel, `grep -rn modelrouting sdk/ cmd/ internal/api/ internal/config/`
returned nothing: the engine was built, tested, on trunk, and **unreachable**.
A client could write `optimization: min_cost` into a strategy and nothing would
ever consult it.

## Config block

```yaml
model-routing:
  enabled: false                  # default: off, zero change to routing
  keeper-url: ""                  # falls back to CPA_MODEL_ROUTING_KEEPER_URL
  # management-key: ""            # prefer CPA_MODEL_ROUTING_KEEPER_KEY
  sync-interval: 24h              # price sync period
  state-path: ""                  # durable version history; empty = memory only
  default-strategy:               # omit to keep the engine's built-in default
    optimization: min_cost        # min_cost | balanced | high_quality
    quality-floor: 0.6            # [0,1]
    # max-budget: 0.01            # optional hard USD cap per request
    preferred-model: ""           # optional cost-tie breaker
    only-allow-model: ""          # optional hard pin
```

An empty block or `enabled: false` builds nothing:

- no gate is installed on `BaseAPIHandler`,
- no price-sync goroutine starts,
- `/v0/management/routing/policies` is not mounted (404),
- every request routes exactly as an unconfigured build.

An invalid `default-strategy` or `sync-interval` disables the channel and logs
the reason, so a typo never becomes a half-applied policy.

## Request path

The gate runs on the plain model-to-provider path of the three execution
entries (chat completions, streaming, count). It is **not** applied when:

- a plugin or arbitrage router already resolved the request (that router
  outranks it),
- the execution is a nested plugin execution excluded from routing,
- the caller pinned one provider (`ForcedProvider`) — substituting another
  provider's model would send a foreign model name upstream,
- the request arrives on a **media endpoint** — image or video. Three signals
  are checked because no single one covers every media route:
  - the **request path** (`/v1/images*`, `/v1/videos*`) — the only signal that
    catches `/v1/images/generations` and `/v1/images/edits` when they run
    through the Responses-backed branch, where the handler type is
    `openai-response` (the same as the genuine text Responses API);
  - the **handler type** (`openai-image`, `openai-video`) — because the video
    handlers use the plain execution path and never set the image flag;
  - the `allowImageModel` flag — defence in depth.

  Those endpoints have their own model validation, and a text price table has
  nothing to say about them,
- the requested model carries a thinking-budget suffix (`model(8192)`) —
  routing to another model would silently drop the budget the client declared.

For each routed request the gate:

1. resolves the caller scope — see *Scope* below,
2. resolves the strategy in force (per-scope newest version → configured
   `default-strategy` → engine built-in default) **and its version number in one
   lookup**, so a concurrent POST can never attribute a vN decision to v(N+1),
3. narrows the price snapshot to models the gateway can actually serve for this
   request — see *Candidate pool* below,
4. runs the decision engine,
5. sets `x-or-model` / `x-or-reason` on the response,
6. emits one structured decision log line (`request_id`, `strategy_version`,
   `model`, `est_cost`, `quality`, `reason`, `outcome`),
7. either swaps the model to execute, or returns the terminal rejection before
   any upstream call.

Terminal outcomes:

| Outcome | Status | Body code | Who asked for it |
|---|---|---|---|
| budget cap hit | 402 | `budget_exceeded` | the operator (`max-budget`) |
| forced model unavailable | 503 | `model_unavailable` | the operator (`only-allow-model`) |
| no candidate meets `required_capabilities` | 400 | `capability_constraint_violation` | the client |

A **general-path** `model_unavailable` — no priced model above the requested
quality tier — is *not* in that table. It is a statement about the price table,
not a violation of anything anyone asked for, so it passes through to the
requested model (`route_passthrough:no_model_meets_quality_floor`). Rejecting
there would 503 a complex request on a gateway whose registered models are all
cheap — and the shipped example config sets a 0.6 floor, so that is reachable
out of the box. Only the pinned case (`only-allow-model`) keeps its 503, because
there the operator named the model and failing is the contract.

The rule: routing rejects when a policy someone explicitly set is violated; it
declines (passes through) when the engine merely cannot improve on the request.

### Candidate pool: only models the gateway can serve

The decision engine knows nothing about endpoints or credentials: given a price
table it returns the cheapest row that meets the floor. Applying that
unfiltered would hand back

- an image-only model on a chat request → 503 from the image-only guard,
- a model with no credential → 400 `model_not_found`,
- a text model on an image request → an image request dispatched to a chat
  model.

Every one of those turns a working request into a failure. The handler therefore
passes an `Allowed` predicate with the request; the gate filters the snapshot
through it before deciding. A model is servable when:

- its base name (thinking suffix stripped) is neither an image-only nor a video
  model, and
- a provider resolves for it in the model registry (or the home channel is
  enabled, which serves every model through one provider).

Known gap: modality is detected by known model names (`isOpenAIImageOnlyModel`,
`isVideoOnlyModel`), not by capability. An *image-producing chat model* such as
`gemini-2.5-flash-image-preview` can therefore still be re-routed to a cheaper
text model, losing the image output. Closing that generally needs a capability
tier on the requested model rather than a name list; it is a follow-up, and the
media endpoints above are not affected because they never reach the gate.

### Degradation: decline instead of failing

Three states make the gate decline to route. In all three the request keeps its
own model, the response carries only `x-or-reason`, and nothing is rejected:

| State | `x-or-reason` |
|---|---|
| no price snapshot (KEEPER down, not yet synced, no `keeper-url`) | `route_passthrough:no_price_snapshot` |
| snapshot present but no priced model is servable for this request | `route_passthrough:no_servable_candidate` |
| servable models exist but none meets the requested quality tier | `route_passthrough:no_model_meets_quality_floor` |

A fourth case is **not** a pass-through: a snapshot older than the staleness
horizon still produces a valid servable model, so the request is routed, but the
decision reason carries `[stale_price_snapshot]`. That matters for `max_budget`:
a 402 taken on a months-old table is distinguishable from one taken on fresh
prices. Passing through instead would discard a valid choice, so this is the
right side of the trade-off — but the degradation must be visible.

These three paths are why routing does not fail a request the engine merely
cannot improve on: the engine's own outcome for an empty candidate set is a 503,
and returning it would turn "enabled but KEEPER down" into a full request
outage. Routing **does** reject in the three cases in the table above (402 / 400
/ 503-on-a-pin), where a policy someone explicitly set is violated. The rule is
narrow, and it is the whole of it: reject only what was asked for, decline
otherwise.

The declined state is logged **once** per reason at warn, then at debug: a
permanently enabled channel with a down KEEPER would otherwise write one
warning per request for the whole deployment, burying real failures. The
per-request signal is the header, which every request still carries.

The per-request cost cap and the version management API keep working while
routed selection waits for a snapshot.

## Scope

The gateway authenticates at the API-key level only. There is no organization
or tenant identifier anywhere on the request path — the access manager records
the principal (the key) under `userApiKey`, and nothing above it.

The scope is therefore `user:<hash>`:

```
scope id = "key_" + first 32 hex chars of SHA-256(api key)
```

- The raw key is never used as a scope id: ids surface in management responses
  and decision logs, and a gateway key is a bearer credential.
- The digest is deterministic, so a key always resolves to the same scope, and
  an operator can compute it locally:
  `printf %s "$KEY" | shasum -a 256 | cut -c1-32`.
- A request with no principal (auth disabled) maps to the zero scope, which
  always resolves to `default-strategy` / the built-in default.

`org` scopes are supported by the table and the API (`scope.kind: org`) but are
not derived from the request path, because no org identity exists there yet.

## Strategy-object API

Mounted only when the channel is enabled, inside the authenticated
`/v0/management` group, next to the existing credential-level
`GET/PUT/PATCH /routing/strategy`. They are different controls: that one picks
round-robin / weighted / fill-first credential selection; this one is the
per-scope cost/quality policy the decision engine enforces.

### `GET /v0/management/routing/policies`

Query (all optional): `scope_kind=user|org`, `scope_id=<id>`. Omitted ⇒ the
default view.

```json
{
  "enabled": true,
  "scope": { "kind": "user", "id": "key_67f1b3dc…" },
  "effective": {
    "strategy": { "optimization": "min_cost", "quality_floor": 0.5 },
    "found": true,
    "reason": "strategy v3 in force",
    "version": 3
  },
  "versions": [
    { "version": 1, "timestamp": "…", "strategy": {…}, "note": "initial" },
    { "version": 2, "timestamp": "…", "strategy": {…}, "note": "escalate" }
  ],
  "price_status": { "models": 3, "synced_at": "2026-11-27T00:43:27Z", "stale": false }
}
```

### `POST /v0/management/routing/policies`

Appends one immutable version and returns the new version with the resulting
effective view.

```json
{
  "scope": { "kind": "user", "id": "key_67f1b3dc…" },
  "strategy": { "optimization": "high_quality", "quality_floor": 0.9 },
  "note": "escalate for the launch week"
}
```

Scope may also come from the query string (`scope_kind` / `scope_id`); the body
wins when both are present.

Rejections (400, no version written): missing `note` (`note_required`, audit
requirement), invalid strategy (`invalid_strategy` — the message names the
field, e.g. `optimization "bogus" must be one of …`), missing/malformed scope
(`invalid_scope`), body over 64 KiB or a note over 4 KiB (`invalid_body`). The
size caps stop one management call from bloating the response, the state file
and every later read.

### Append-only, and rollback

History is never rewritten. A change appends one record carrying the full new
snapshot plus the note; the "before" state is the previous record, still intact.

**Rollback is a new version**: `POST` the old field values again. It becomes
`v(N+1)` with v1's semantics and an audit note saying so. `v1` itself is
untouched.

### Durability

`state-path` writes the whole history to one JSON document (mode 0600) after
every successful append, atomically (temp file + rename), so a crash mid-write
cannot leave a truncated file that a restart reads as "no policies". Every
record is restored verbatim — version numbers, timestamps, notes, snapshots —
no clock is consulted, so the history survives a restart rather than being
re-derived.

- Empty `state-path` = memory only; policies do not survive a restart. Set it
  in any deployment that uses the management API.
- A corrupt or unsupported state file is logged loudly (with how many scopes did
  restore) and the channel keeps running: a bad management-state file must not
  take the data plane down. **The damaged file is never overwritten** — later
  appends go to a sibling `<path>.recovered-<unix>` file and the log says so.
  Without that, the first policy edit after a damaged read would erase every
  other scope's history, which is exactly the data the error told the operator
  to reconcile.
- The restore path refuses to overwrite a scope that already holds records, and
  refuses records whose version or timestamp does not strictly increase.
- A failed write returns **202** with `persisted: false` and the version number:
  the version is live in memory (the history is append-only, it cannot be
  rolled back), so a retrying client would otherwise append a duplicate.

### Known limitation: one writer per `state-path`

The file is a whole-document read-modify-write with no cross-process lock. Two
replicas sharing one `state-path` would each rewrite the other's scopes and lose
them. Run this channel with a single writer per state file — give each replica
its own `state-path`, or point them all at the same KEEPER-fed prices and manage
policies through one instance. Per-scope sharding or file locking is deliberately
not built: it is a deployment-shape decision (KISS), not something the adapter
should guess at.

### Integrity scope

The persisted file is a restart store, not a tamper-evident ledger.

What it does detect: accidental corruption — parse errors, non-contiguous or
non-monotonic version records, and a bad load is never overwritten.

What it does not detect: a deliberate edit by someone with write access to the
volume (a record's strategy or note rewritten in place, or a whole scope block
deleted). Contiguity catches a *dropped* record; it cannot catch an *edited* one
without a key.

This is a deliberate scope decision, not an oversight. Policy changes are
**not currently appended to `internal/auditchain`** — that channel is disabled by
default (`CPA_AUDIT_CHAIN_ENABLED`) and covers high-risk actions, not routing
policy edits. Wiring routing-policy changes into it is a separate piece of work;
until then, treat `state-path` with the same access control as the config file
(0600, one writer). Anyone who can rewrite that file can already rewrite
`config.yaml` and the gateway's credential store.

### Known cost: the decision engine, not the adapter

The per-request cost of an enabled channel is dominated by
`internal/modelrouting.Decide`: it scores every priced model against the
capability-tier table. Measured at ~80µs for a 200-model price table; the
adapter around it (scope resolution, servable-set lookup, headers, one decision
log line) adds ~10µs.

That is the delivered engine's design (RIC-2524), not something this wiring
introduced, and it only runs when the operator enables the channel. The
servable-set filter — the one piece this adapter could have made per-request —
is memoised per (credential generation, price snapshot), so it is paid once per
sync, not once per request (`BenchmarkRouteRequestEndToEnd`).

## Lifecycle

`modelroutingruntime.Runtime.Start/Stop` are wired to the service's `Run` /
`Shutdown`, like the arbitrage and route-boundary channels.

`Start` runs the engine's initial price pull **off the boot path**: a
black-holed KEEPER URL must not delay the gateway accepting requests (the pull
is bounded by 10s). Until the first snapshot lands the gate passes requests
through, so starting late costs nothing.

`Stop` waits for that off-path start to finish before draining, so it can never
race a syncer that has not registered its cancel func yet — which would leak the
goroutine. `Stop` is a no-op when no KEEPER source is configured.

**Shutdown can take up to the price-pull timeout** (10s) if a pull is in flight
when the process stops: `Stop` joins the in-flight request rather than abandoning
it. `Service.Shutdown` allows 30s, so this is fine there, but a Kubernetes
`terminationGracePeriodSeconds` below ~11s would kill the process mid-drain.
Raise the grace period to at least 15s, or accept that the last pull is
interrupted (the history is durable either way; only the price snapshot is
re-fetched on next boot).

## Environment

| Variable | Purpose |
|---|---|
| `CPA_MODEL_ROUTING_KEEPER_URL` | price source, when `keeper-url` is empty |
| `CPA_MODEL_ROUTING_KEEPER_KEY` | `X-CPA-Management-Key` for the pull |
| `CPA_CONTENT_FILTER_AUDIT_KEEPER_KEY` | shared fallback key |

Prefer the environment over `management-key` in the config file. The config
field is `json:"-"`, so it is never serialized back out by the management API.
