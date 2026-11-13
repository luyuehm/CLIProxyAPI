# Quality Score Contract (frozen) — OPENROUT-002-S1B

Contract version: **1** (`modelrouting.ContractVersion`)
Package: `internal/modelrouting/` (files: `quality.go`, `quality_test.go`)
Owner issue: RIC-2523. Consumers: S2 decision engine (RIC-2524), S3 replay (RIC-2525).

This file is the **frozen interface contract**. S2 must consume exactly the
signatures, value tables, and boundary behaviors below. Any value change
requires bumping `ContractVersion` and a coordinated S2 review — S2's dependency
is this document, not the implementation.

## 1. Output types

```go
type TaskType string
// "code" | "analysis" | "simple" | "default"

type LengthBucket string
// "short" | "medium" | "long"

type CapabilityLevel int
// 1=LevelLite, 2=LevelStandard, 3=LevelAdvanced, 4=LevelFrontier

type QualityScore struct {
    TaskType     TaskType       `json:"task_type"`
    LengthBucket LengthBucket   `json:"length_bucket"`
    Quality      float64        `json:"quality"` // required capability floor, in [0,1]
}
```

Semantics of `Quality`: **the requested capability floor, not a predicted
answer quality.** `Quality = 0.70` means "this request needs at least a
model whose capability satisfies the floor band for 0.70" — S2 maps it to a
`CapabilityLevel` (section 3) and filters candidate models with
`MeetsLevel(name, RequiredLevel)`. Higher Quality → higher minimum tier.
It is a deterministic function of (TaskType, LengthBucket) only; it carries
no per-request randomness.

## 2. Entry points (Go API)

```go
func Score(req pluginapi.ModelRouteRequest) QualityScore
func ScoreBody(body []byte) QualityScore          // same, raw payload

func (s QualityScore) RequiredLevel() CapabilityLevel
func LevelForModel(name string) (CapabilityLevel, bool)
func MeetsLevel(name string, required CapabilityLevel) bool
```

- `Score` is the S2 entry point: it takes the existing
  `sdk/pluginapi.ModelRouteRequest` struct, so no new request plumbing is
  needed. Contract v1 scores **only from `req.Body`** (the raw client
  payload). `SourceFormat`, `RequestedModel`, `Stream`, `Headers`, `Query`,
  and `Metadata` are intentionally not consumed in v1; a v2 extension that
  uses them must be additive (new optional inputs, unchanged outputs for
  existing inputs).
- `ScoreBody` exists so S3 offline replay can score stored payloads without
  reconstructing a full `ModelRouteRequest`.
- `Score` never panics and never returns a `Quality` of 0.

### Token/length estimation

Estimated tokens = `extractedChars / 4` (frozen factor, KISS). No network,
no tokenizer. `extractedChars` is the concatenated length of all extracted
text fields (section 2.1).

### 2.1 Feature extraction (Body only)

Payload shapes recognized, in order of precedence:

1. **OpenAI/Anthropic chat**: `messages[]` — string `content` or array of
   content blocks (`text` fields; image/audio blocks ignored); `role:"user"`
   count = user turns; `system` (string) appended.
2. **Gemini**: `contents[]` — `parts[].text` appended; `role:"user"` count =
   user turns.
3. **Legacy**: top-level `prompt` (string or string array) appended.

Extraction is best-effort with `gjson` (already a repo dependency, no new
one added). A valid JSON body that matches none of the three shapes, a
truncated/invalid JSON body, an empty body, or whitespace-only body all
degrade to the **default tier** (section 4). Parsing failures are never
retried.

## 3. Capability levels and threshold bands (frozen)

The registry `ModelInfo` has **no capability field** (verified at baseline
`4a7b18eb`), so tiers are derived from a minimal model-name table
(`modelLevelRules` in `quality.go`). Levels:

| Level | Constant | Meaning |
|---|---|---|
| 1 | `LevelLite` | haiku / flash-lite / mini / fast / lite |
| 2 | `LevelStandard` | sonnet / flash / gpt base / codex |
| 3 | `LevelAdvanced` | opus (pre-4.6) / 2.5-pro / 5.x-pro / k2 / grok-4 |
| 4 | `LevelFrontier` | latest opus (4.6+/5) / fable / gpt-6 / gemini-3.x-pro / k3 |

Quality → minimum level bands (inclusive upper edges):

| Quality band | Minimum level |
|---|---|
| ≤ 0.35 | LevelLite |
| 0.35 < q ≤ 0.55 | LevelStandard |
| 0.55 < q ≤ 0.75 | LevelAdvanced |
| > 0.75 | LevelFrontier |

Model name resolution: rules are evaluated **in table order, first match
wins** (specific high-tier prefixes before family catch-alls). Unknown model
names match no rule; `MeetsLevel` for them returns `true` only for
`LevelLite` requests (conservative: unknown models may serve easy requests,
never demanding ones).

## 4. Frozen rule list and value tables

### 4.1 Task classification rules (5 rules, first match wins)

| # | Rule | TaskType |
|---|---|---|
| R1 | Text contains a fence marker (`` ``` `` or `~~~`) | code |
| R2 | Text matches a source file path with code extension, or a source-language construct (`func X(`, `def X(`, `class X:`, `#include <`, `public static`, `import {...}/from '...'`) | code |
| R3 | user turns ≥ 3 **or** estimated tokens ≥ 8000 | analysis |
| R4 | extracted chars ≤ 400 **and** user turns ≤ 2 | simple |
| R5 | nothing else matched | default |

### 4.2 Length buckets

| Estimated tokens | Bucket |
|---|---|
| ≤ 4000 | short |
| 4001 – 32000 | medium |
| > 32000 | long |

### 4.3 Quality table (TaskType × LengthBucket → Quality)

| TaskType \ Bucket | short | medium | long |
|---|---|---|---|
| default | 0.30 | 0.40 | 0.55 |
| simple | 0.30 | 0.35 | 0.50 |
| code | 0.45 | 0.55 | 0.70 |
| analysis | 0.50 | 0.65 | 0.80 |

Invariants (guarded by `TestQualityTableInvariants`): every value in (0,1];
values are monotone non-decreasing short → medium → long; code ≥ simple at
the same bucket.

### 4.4 Boundary behavior (frozen)

| Input | Result |
|---|---|
| nil / empty / whitespace-only body | default tier: `{default, short, 0.30}` |
| Truncated or invalid JSON | default tier: `{default, short, 0.30}` |
| Valid JSON, no recognizable chat shape (`{"foo":"bar"}`, arrays, …) | default tier: `{default, short, 0.30}` |
| Any input | never panics, never returns Quality 0 |

## 5. Determinism (non-negotiable)

`Score` is a pure function of its input: no random source, no clock, no I/O,
no package-level mutable state read by the scorer. Concurrent calls on the
same input always agree (verified by the double-run assertion in
`TestScoreDeterministic`; safe under `-race`). Audit logs recording
(TaskType, LengthBucket, Quality) are therefore reproducible.

## 6. Out of scope (frozen negative list)

- No LLM-as-judge / subjective answer-quality prediction (parent PRD
  rabbit-hole decision; extra LLM calls would add latency and cost).
- No historical-feedback calibration / RL / auto-tuning (parent out-of-scope;
  interpretability first).
- No multi-language code recognition dictionaries (KISS; misclassification
  of e.g. Chinese-embedded code samples is an accepted limitation).
- No per-model latency or price inputs into the score (price belongs to the
  S1-A cache; S2 combines both at decision time).

## 7. Verification (contract guard)

The gate command below covers every current guard in the package; if a new
guard is added, extend the first line's regex so it stays in scope.

```
go test ./internal/modelrouting/... -run 'TestQuality|TestScore|TestModelLevel'   # all guards
go test -race ./internal/modelrouting/...                                          # determinism under concurrency
go build ./...                                                                     # compile
gofmt -l internal/modelrouting/                                                    # no diff
```

Key guards: `TestScoreDeterministic` (acceptance 1), `TestScoreCodeTask`
(acceptance 2, asserts frozen contract value), `TestScoreLongDocument`
(acceptance 3), `TestScoreBoundaryInputs` (acceptance 4),
`TestQualityTableValues` (pins the twelve frozen Quality literals AND the
capability tier each implies, so silent table or threshold drift fails hard),
`TestQualityTableInvariants` + `TestQualityThresholds` (range/monotonicity +
threshold bands), `TestModelLevelMapping` (sampled name → tier mapping),
`TestModelLevelRulesFrozen` (pins every entry of the `modelLevelRules` tier
table structurally, so a tier flip or reordering hard-fails even when no
sampled name exercises the rule).

## 8. Versioning

Contract v1 is frozen as of baseline `4a7b18eb`. S2 and S3 pin to
`ContractVersion`. Additive changes (new optional inputs, new task types
with their own rows) may ship as v2 without breaking v1 consumers; changes to
existing table values or threshold bands are breaking and require a
coordinated release across S1B/S2/S3.
