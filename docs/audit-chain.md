# Audit Chain of Responsibility (RIC-2686)

High-risk automated actions — outbound messages, deletions, anything an agent
does on a human's behalf — used to be approved with only a yes/no: the
approver never saw *why* the model recommended the action, and after the
fact nobody could reconstruct the decision chain for an audit. This channel
fixes that with a tamper-evident evidence log.

For each business operation (a caller-chosen operation ID), the gateway
records the events that matter in one hash-linked timeline:

```
 op-del-001
   │
   │  model_decision   the model's recommendation + summary
   │  approval         who approved, when, and their decision note
   │  tool_call        the actual tool name and its arguments
   │  result           what the execution produced
   ▼
 hash chain: every record binds prev_hash → hash, so rewriting any
 record (including the approver's identity) is detected on the next read
```

- **Query by operation ID.** `GET /v0/management/audit/operations/:id`
  returns the full event timeline in chronological order together with the
  chain-integrity verdict. A `from`/`to` RFC3339 window filters the display
  events; integrity is always verified over the whole sequence, so a
  tampered middle record cannot be hidden behind a time range.
- **Append-only, hash-chained.** Each record's digest binds the previous
  digest, the event kind, the actor identity, the summary, the structured
  payload, the event time and the encrypted snapshot. Changing any field —
  including the approver's name or the model's recommendation — breaks the
  chain.
- **Tamper detection.** A read that finds a broken record reports
  `tampered: true`, the first broken sequence number, and the digest of the
  intact prefix (`backup_digest`) so the un-tampered portion stays
  provable.
- **Encrypted model context.** When a snapshot key is configured, the
  model's input/output captured for the decision is encrypted at rest with
  AES-GCM and bound into the record hash. Without a key the fields are
  dropped fail-closed — the chain still records the decision, just without
  the raw model context. Plaintext model context is never written.

## Enabling

All switches are environment variables on the gateway process; an
unconfigured build mounts no routes and opens no database.

| Variable | Meaning |
| --- | --- |
| `CPA_AUDIT_CHAIN_ENABLED` | `true` enables the channel (default off) |
| `CPA_AUDIT_CHAIN_DB_PATH` | SQLite path (default: `<tmp>/cpa-audit-chain/audit.db`) |
| `CPA_AUDIT_CHAIN_KEY_HEX` | 32-byte hex key enabling encrypted model context capture |

## Endpoints

Both routes live inside the authenticated `/v0/management` group, so a
caller without the management key is answered `401`/`403` by the
management middleware before any record content is served — a denied
request never learns which records exist.

```
GET  /v0/management/audit/operations/:id?from=RFC3339&to=RFC3339
POST /v0/management/audit/events
```

`POST /v0/management/audit/events` body:

```json
{
  "operation_id": "op-del-001",
  "kind": "approval",
  "actor": "alice@corp",
  "summary": "approved after risk review",
  "payload": { "risk_level": "high" },
  "model_input": "...", "model_output": "...",
  "occurred_at": "2026-11-15T12:00:00Z"
}
```

`kind` values: `model_decision`, `approval`, `tool_call`, `result` (custom
kinds are accepted with the same validation rules). `approval` events
require a non-empty `actor` — the approver identity is a hash-bound
contract field and cannot be omitted or forged after the fact.

## Out of scope (deliberate)

- **PDF compliance reports.** Export is structured JSON; report formats
  are industry-specific and belong to the consumer.
- **Distributed storage.** The chain writes to local SQLite in the
  self-hosted form factor; the append-only, hash-linked design is what
  carries into a later distributed deployment.

## Implementation pointers

- `internal/auditchain/` — store, hash chain, HTTP handler, env-gated
  `ServerOption()` (returns nil when disabled, mirroring the
  contentfilter / policies / arbitrage channels).
- `internal/api/server_management.go` — route registration inside the
  authenticated group; `serveAuditChain` reports 404 when the channel is
  off.
- `sdk/cliproxy/builder.go` — wiring of `auditchain.ServerOption()`.
