# Replay Input Contract (frozen) — OPENROUT-002-S3

Contract version: **1** (`modelrouting.ReplayInputContractVersion`)
Package: `internal/modelrouting/` (files: `replay.go`, `report.go`, `presets.go`)
Owner issue: RIC-2525. Consumers: decision-log export producers, JSONL
sample import, and any future offline replay tooling.

## 1. What a replay record is

One JSONL line = one `modelrouting.ReplayRecord`. It carries the S2
decision-log fields (echoed verbatim for audit) plus the S1-B request
features that make the request re-decidable **without the original body**:

| field | JSON key | source | replay role |
|---|---|---|---|
| recorded lifecycle id | `request_id` | S2 log | echoed; diff join key |
| in-force S1-A record number (0 = default) | `strategy_version` | S2 log | echoed |
| S2-chosen model | `model` | S2 log | echoed |
| S2-recorded cost estimate (USD) | `est_cost` | S2 log | echoed |
| S2-recorded quality score | `quality` | S2 log | echoed |
| S2-recorded reason text | `reason` | S2 log | echoed |
| S2 outcome kind | `outcome` | S2 log | echoed |
| S1-B task class (`code`/`analysis`/`simple`/`default`) | `task_type` | S1-B scorer | re-scoring input |
| S1-B length tier (`short`/`medium`/`long`) | `length_bucket` | S1-B scorer | re-scoring input |
| recorded body length / 4 | `est_tokens` | S2 log / export | cost factor |
| raw payload, optional | `body` | user import | scorer fallback |
| client-requested model | `requested_model` | S2 log | tie-break input |

## 2. Replay input resolution (frozen, v1)

1. **Both features present** (`task_type` + `length_bucket`): the frozen
   S1-B quality table maps the pair straight to the quality value — no
   body needed. `est_tokens` is the cost factor; when absent and a body is
   present it is recomputed as `len(body) / 4` (the engine's frozen
   factor); feature-only records with no body estimate 0 tokens (zero
   cost basis — the recorded features stay the source of truth, nothing
   is invented).
2. **Body only**: the local S1-B scorer (`ScoreBody`) resolves the
   features; unrecognized payloads degrade to the default tier (0.30)
   per the S1-B contract, never fail.
3. **Neither**: the record is skipped with the reason
   `no_replay_input` recorded — never guessed.

Malformed lines and over-long lines are counted as skipped
(`malformed_record` / `record_exceeds_line_cap`); they never end the
stream. An unknown `task_type`/`length_bucket` pair that names no quality
table cell is skipped, never scored 0 (the scorer's "never returns 0"
rule).

## 3. Replay output (frozen)

One `ReplayOutcome` per record: `request_id`, `model`, `est_cost` (USD,
recomputed — not the recorded value), `quality` (recomputed), `reason`
(for `ok` rows the S2-frozen `chose_<model>: quality_score=<f>
cost=$<f>` segment plus the `[offline_replay]` audit note), `outcome`
(engine kinds verbatim, or `skipped`). The S2 reason segment is a
**frozen parse contract** shared with `internal/modelrouting/engine.go`;
replay appends the note only — it never reformats the segment.

## 4. Invariants

- **Determinism**: identical records + strategy + price snapshot ⇒
  byte-identical rows (asserted by `TestReplayDeterministic`). No
  randomness, no clock reads, no I/O inside the decision path.
- **Offline**: the replay touches only the in-memory `PriceSnapshot` and
  the frozen scorer; the `PriceSource` (the package's only
  network-facing type) is never constructed. `TestReplayOffline` runs
  the full pipeline in an environment with no provider credentials.
- **Memory bound**: streaming input is read line by line; the detail
  slice is capped (default 1000 lines, per the ticket's batch ruling),
  the summary always covers the full input
  (`TestReplayStreamingBound` asserts the bound at 100k lines).
- **No silent default**: an invalid strategy yields an empty row, never
  a downgrade to the built-in default (`TestReplayInvalidStrategy`).

## 5. Change policy

Any field or rule change requires a `ReplayInputContractVersion` bump
and a coordinated review of S2's decision-log export, because the echo
fields are S2's frozen log field names and the engine's reason segment
must stay parseable.
