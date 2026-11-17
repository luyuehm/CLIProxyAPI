package modelrouting

// The offline replay layer (RIC-2525 / OPENROUT-002-S3). Re-running recorded
// traffic under arbitrary strategies and a fixed price snapshot, fully
// offline: no upstream calls, no price-source pulls. The S1-A snapshot and
// the S1-B scorer are the entire input, so two replays of the same records,
// strategy, and snapshot are byte-identical (the audit property the parent
// ticket freezes as uncuttable).

import (
	"bufio"
	"encoding/json"
	"io"
	"time"
)

// ReplayInputContractVersion is the version of the ReplayRecord shape.
// Records are operator-produced (decision-log export or a JSONL sample
// import), so the field set is frozen once shipped: replaying an older
// export must keep working, and a shape change requires a version bump plus
// a note here.
const ReplayInputContractVersion = 1

// OutcomeSkipped is the replay-side outcome kind for records with no
// replayable input (the engine's own outcome kinds are re-used verbatim).
const OutcomeSkipped = "skipped"

// ReplayRecord is one input sample of a replay run. It is the replay input
// contract the decision-log export targets (the S2 rabbit-hole ruling in
// RIC-2525): the S1-B feature fields plus the token estimate, so the
// original request body is NOT required. Replayed S2 decision fields
// (request_id, strategy_version, model, est_cost, quality, reason,
// outcome) use the engine's frozen log field names, verbatim.
//
// A record is replayable when it carries EITHER the scorer features
// (task_type + length_bucket) OR a raw body; carrying neither is a skip,
// never a guess. The est_tokens field, when present, is the recorded body
// length / 4 estimate — the engine's frozen cost factor.
type ReplayRecord struct {
	// RequestID is the recorded lifecycle id; echoed verbatim.
	RequestID string `json:"request_id"`
	// StrategyVersion is the S1-A record number in force when the request
	// was served (0 = built-in default applied). Echoed verbatim.
	StrategyVersion int64 `json:"strategy_version,omitempty"`
	// Model is the S2-chosen model, as recorded at serve time.
	Model string `json:"model,omitempty"`
	// EstCost is the S2-recorded cost estimate (USD).
	EstCost float64 `json:"est_cost,omitempty"`
	// Quality is the S2-recorded quality score.
	Quality float64 `json:"quality,omitempty"`
	// Reason is the S2-recorded decision reason text.
	Reason string `json:"reason,omitempty"`
	// Outcome is the S2 outcome kind (ok / budget_exceeded /
	// model_unavailable).
	Outcome string `json:"outcome,omitempty"`
	// TaskType and LengthBucket are the S1-B features the recorded request
	// scored into; with both present the scorer is bypassed and the frozen
	// quality table maps them straight to the quality value.
	TaskType     TaskType     `json:"task_type,omitempty"`
	LengthBucket LengthBucket `json:"length_bucket,omitempty"`
	// EstTokens is the recorded body-length / 4 token estimate. When zero
	// and a body is present it is recomputed from the body; a
	// feature-only record with no body then estimates zero tokens, i.e. a
	// zero cost basis — the recorded feature fields stay the contract's
	// source of truth, nothing is invented.
	EstTokens int `json:"est_tokens,omitempty"`
	// Body, when present, is the raw request payload: the only replayable
	// input left when the features are absent. It is consumed by the local
	// S1-B scorer only — never sent anywhere.
	Body string `json:"body,omitempty"`
	// RequestedModel is the client-requested model at serve time, replayed
	// as the engine's tie-break input.
	RequestedModel string `json:"requested_model,omitempty"`
}

// ReplayOutcome is one replayed request's recomputed decision.
type ReplayOutcome struct {
	// RequestID echoes the record's id; empty for synthesized samples.
	RequestID string `json:"request_id,omitempty"`
	// Model is the replay-chosen model; empty for skipped and
	// unavailable outcomes.
	Model string `json:"model,omitempty"`
	// EstCostUSD is the replay-computed cost estimate (USD); 0 for skipped
	// and unavailable outcomes.
	EstCostUSD float64 `json:"est_cost"`
	// Quality is the request's heuristic quality score, in [0,1]; 0 for
	// skipped records.
	Quality float64 `json:"quality"`
	// Reason is the replay decision text; for ok rows it carries the
	// S2-frozen "chose_" segment, for skipped rows the skip reason.
	Reason string `json:"reason"`
	// Outcome is the engine outcome kind, or OutcomeSkipped for records
	// with no replayable input.
	Outcome string `json:"outcome"`
	// Skipped is true when the record carried neither the scorer's
	// features nor a body, so nothing could be decided.
	Skipped bool `json:"skipped,omitempty"`
}

// ReplaySummary aggregates one strategy's replay over its sample set. The
// JSON field names are the frozen report contract the ticket pins
// (total_cost, avg_cost, avg_quality, below_floor_ratio): a rename is a
// contract break for downstream report consumers.
type ReplaySummary struct {
	// Requests is every record seen in the input (skipped rows included).
	Requests int `json:"requests"`
	// Skipped is the number of records with no replayable input.
	Skipped int `json:"skipped"`
	// TotalCostUSD is the sum of the replay-computed est_cost (USD) over
	// every decided row.
	TotalCostUSD float64 `json:"total_cost"`
	// AvgCostUSD is TotalCostUSD divided by the decided row count; 0 when
	// nothing was decided.
	AvgCostUSD float64 `json:"avg_cost"`
	// AvgQuality is the mean quality over every decided row; 0 when
	// nothing was decided.
	AvgQuality float64 `json:"avg_quality"`
	// BelowFloorCount is the number of decided rows whose quality fell
	// below the strategy's floor — the strategy tryout's core criterion.
	BelowFloorCount int `json:"below_floor_count"`
	// BelowFloorRatio is BelowFloorCount over the decided rows; 0 when
	// nothing was decided.
	BelowFloorRatio float64 `json:"below_floor_ratio"`
}

// ReplayResult is one strategy's full replay output: the summary plus,
// optionally, the per-request detail rows.
type ReplayResult struct {
	// StrategyName is the label the report groups rows under.
	StrategyName string `json:"strategy_name"`
	// Strategy is the strategy the replay ran under.
	Strategy Strategy `json:"strategy"`
	// Summary aggregates the run.
	Summary ReplaySummary `json:"summary"`
	// Details carries one row per request, input order, when captured;
	// nil when the caller did not request detail (streaming runs cap it).
	Details []ReplayOutcome `json:"details,omitempty"`
	// DetailsTruncated is true when Details stopped at the capture cap
	// before the input ended; Summary always covers the full input.
	DetailsTruncated bool `json:"details_truncated,omitempty"`
}

// ReplayOptions configures one replay run.
type ReplayOptions struct {
	// StrategyName labels the result rows.
	StrategyName string
	// Details caps the per-request rows captured: 0 uses the default cap,
	// >0 captures at most that many, -1 captures every row. The summary is
	// always full-input; only the detail slice is capped, which is what
	// keeps a multi-million-line replay bounded in memory.
	Details int
}

// defaultDetailCap bounds per-request detail capture on streaming runs
// (1000 lines per batch, per the ticket's memory-pressure ruling): the
// summary still covers every line, so capping the detail keeps a 100k-line
// replay bounded instead of holding ~50 MB of outcome structs.
const defaultDetailCap = 1000

// Replay runs the decision engine over a stream of records with a fixed
// strategy and price snapshot. It is the offline core: every input is local
// (the record, the snapshot, the strategy) and no upstream or price source
// is ever contacted. Streaming: a large input is read line by line, one
// record at a time, and the per-request detail is capped, so memory stays
// bounded regardless of sample-set size.
//
// The record format is one ReplayRecord per JSONL line; blank lines count
// as skipped, malformed lines count as skipped with the reason recorded —
// one bad line must not kill a multi-million-line stream.
//
// The strategy must pass Validate; strategies in force (presets, ScopeTable
// versions, the built-in default) all do by construction. An invalid
// strategy yields an empty result rather than a silent downgrade to the
// default, which would hide the misconfiguration in a cost comparison.
func Replay(records io.Reader, strategy Strategy, snapshot PriceSnapshot, opts ReplayOptions) ReplayResult {
	result := ReplayResult{StrategyName: opts.StrategyName, Strategy: strategy}
	if err := Validate(strategy); err != nil {
		return result
	}
	detailCap := opts.effectiveDetailCap()
	// Details is always a usable slice: a capped run preallocates up to
	// the cap (64 is the small-input hint), an unlimited run (cap 0)
	// grows as needed.
	result.Details = make([]ReplayOutcome, 0, min(detailCap, 64))

	scanner := newLineScanner(records)

	// The streaming loop reads one record per iteration. ErrTooLong is
	// special: the scanner keeps reporting it until the NEXT Scan call
	// succeeds, so the oversize-skip path must be reached on a fresh
	// Scan, not by re-looping on the stale error.
	for {
		advanced := scanner.Scan()
		if !advanced && scanner.Err() == bufio.ErrTooLong {
			// One record over the line cap: the scanner has already
			// consumed the rest of that line, so count it skipped; the
			// next iteration's Scan resumes at the following line and
			// clears the error.
			row := ReplayOutcome{
				Reason:  "record_exceeds_line_cap",
				Outcome: OutcomeSkipped,
				Skipped: true,
			}
			accumulate(&result, row)
			appendDetail(&result, row, detailCap)
			continue
		}
		if !advanced {
			// i/o error or clean end of input: stop.
			break
		}
		rec, err := decodeReplayRecord(scanner.Bytes())
		var row ReplayOutcome
		if err != nil {
			// One malformed line must not kill a multi-million-line
			// stream: count it as skipped and keep the input order
			// stable.
			row = ReplayOutcome{
				Reason:  "malformed_record",
				Outcome: OutcomeSkipped,
				Skipped: true,
			}
		} else {
			row = replayRecord(rec, strategy, snapshot)
		}
		accumulate(&result, row)
		appendDetail(&result, row, detailCap)
	}
	if detailCap > 0 && result.Summary.Requests > detailCap {
		result.DetailsTruncated = true
	}
	return result
}

// effectiveDetailCap resolves the options' cap to the applied value:
// unset (0) -> default cap, negative -> unlimited, otherwise the value.
func (o ReplayOptions) effectiveDetailCap() int {
	switch o.Details {
	case 0:
		return defaultDetailCap
	default:
		if o.Details < 0 {
			return 0
		}
		return o.Details
	}
}

// accumulate folds one outcome into the running summary.
func accumulate(result *ReplayResult, row ReplayOutcome) {
	s := &result.Summary
	s.Requests++
	if row.Skipped {
		s.Skipped++
		return
	}
	decided := s.Requests - s.Skipped
	s.TotalCostUSD += row.EstCostUSD
	s.AvgQuality += (row.Quality - s.AvgQuality) / float64(decided)
	s.AvgCostUSD = s.TotalCostUSD / float64(decided)
	if row.Quality < result.Strategy.QualityFloor {
		s.BelowFloorCount++
	}
	// The ratio is recomputed on EVERY decided row, not only below-floor
	// ones: a row at/above the floor lowers it (the stale-ratio bug a
	// self-comparing test would miss).
	s.BelowFloorRatio = float64(s.BelowFloorCount) / float64(decided)
}

// appendDetail appends the just-accumulated row to the detail slice while
// the cap allows; detailCap 0 means unlimited (the caller opted into
// full capture). The summary is never affected: it covers the full input
// regardless of the cap.
func appendDetail(result *ReplayResult, row ReplayOutcome, detailCap int) {
	if detailCap > 0 && len(result.Details) >= detailCap {
		// Capped and the cap is full: the row is counted in the summary
		// only; the slice stops growing (DetailsTruncated is set by the
		// caller when input remains).
		return
	}
	result.Details = append(result.Details, row)
}

// decodeReplayRecord parses one JSONL line. Blank lines are not records:
// they yield an empty record, which replayRecord reports as skipped.
func decodeReplayRecord(line []byte) (ReplayRecord, error) {
	var rec ReplayRecord
	if len(line) == 0 {
		return rec, nil
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		return ReplayRecord{}, err
	}
	return rec, nil
}

// replayRecord recomputes one record's decision: the frozen feature table
// first (the cheap, body-free path), the local scorer when only the body is
// present. A record with neither is skipped with the reason recorded.
func replayRecord(rec ReplayRecord, strategy Strategy, snapshot PriceSnapshot) ReplayOutcome {
	score, estTokens, ok := resolveReplayInputs(rec)
	if !ok {
		return ReplayOutcome{
			RequestID: rec.RequestID,
			Reason:    "no_replay_input: record carries neither scorer features nor a body",
			Outcome:   OutcomeSkipped,
			Skipped:   true,
		}
	}
	return decisionOutcome(decideReplay(strategy, snapshot, score, estTokens, rec.RequestedModel), rec.RequestID)
}

// resolveReplayInputs maps the record to a deterministic QualityScore plus
// the token estimate the cost computation consumes:
//
//   - both features present -> the frozen quality table value, no scorer;
//     the token estimate is the recorded est_tokens, or the body's length /
//     4 when a body is present (the engine's frozen factor, so a record
//     with both a body and est_tokens still costs exactly what the engine
//     would have costed live);
//   - body only -> the local S1-B scorer; ScoreBody degrades unrecognized
//     payloads to the default tier (0.30) rather than failing — recorded
//     traffic is trusted input, not a network peer.
//
// (ok=false) is returned only when the record has neither, the skip case.
func resolveReplayInputs(rec ReplayRecord) (QualityScore, int, bool) {
	body := rec.Body
	if rec.TaskType != "" && rec.LengthBucket != "" {
		quality, ok := qualityForFeatures(rec.TaskType, rec.LengthBucket)
		if !ok {
			// A features pair that names no table cell: skip rather than
			// fabricate a score (the scorer's "never returns 0" rule is
			// about its own outputs, and a made-up value would break it).
			return QualityScore{}, 0, false
		}
		return QualityScore{TaskType: rec.TaskType, LengthBucket: rec.LengthBucket, Quality: quality},
			replayEstTokens(rec, body), true
	}
	if body != "" {
		return ScoreBody([]byte(body)), replayEstTokens(rec, body), true
	}
	return QualityScore{}, 0, false
}

// replayEstTokens is the replay's single cost-factor input: the recorded
// est_tokens when present, else the recorded body's length / 4 — the same
// frozen factor the engine applies, so a replay of the same sample costs
// exactly what the live decision would have.
func replayEstTokens(rec ReplayRecord, body string) int {
	if rec.EstTokens > 0 {
		return rec.EstTokens
	}
	return estimatedTokens(len(body))
}

// qualityForFeatures looks a (task, bucket) pair up in the frozen quality
// table. An unknown combination yields ok=false so the record is skipped
// instead of silently scoring 0.
func qualityForFeatures(task TaskType, bucket LengthBucket) (float64, bool) {
	byBucket, ok := qualityTable[task]
	if !ok {
		return 0, false
	}
	quality, ok := byBucket[bucket]
	return quality, ok
}

// replayNote is the audit segment appended to every ok replay reason; it
// marks the decision as an offline replay so audit logs never confuse a
// replayed decision with a live one.
const replayNote = "offline_replay"

// decideReplay runs the engine's decision core over pre-resolved inputs —
// the same candidate pool, budget cap, and pick rules as Decide, with the
// token estimate injected instead of recomputed from a body.
func decideReplay(strategy Strategy, snapshot PriceSnapshot, score QualityScore, estTokens int, requestedModel string) Decision {
	required := levelForFloor(strategy.QualityFloor)
	if rl := score.RequiredLevel(); rl > required {
		required = rl
	}

	// Forced-model pin: the same hard semantics as the engine — honor it
	// or fail, never substitute.
	if forced := strategy.OnlyAllowModel; forced != "" {
		price, found := snapshot.PriceFor(forced)
		if !found {
			return Decision{
				Outcome:        OutcomeModelUnavailable,
				Model:          forced,
				Quality:        score.Quality,
				Reason:         "forced_model_unavailable:" + headerSafe(forced),
				RequestedModel: requestedModel,
			}
		}
		return forcedReplayDecision(candidate{name: forced, level: levelFor(forced), estCost: estimateCost(price, estTokens)}, strategy, score, requestedModel)
	}

	cands := eligibleReplayCandidates(snapshot, required, estTokens)
	if len(cands) == 0 {
		return Decision{
			Outcome:        OutcomeModelUnavailable,
			Quality:        score.Quality,
			Reason:         "no_eligible_candidate_meets_quality_floor",
			RequestedModel: requestedModel,
		}
	}

	// Cost hard cap: restrict the pool to candidates within budget; an
	// emptied pool is the 402 semantics with no upstream call.
	if strategy.MaxBudget != nil {
		cap := *strategy.MaxBudget
		kept := cands[:0]
		for _, c := range cands {
			if c.estCost <= cap {
				kept = append(kept, c)
			}
		}
		if len(kept) == 0 {
			return Decision{
				Outcome:        OutcomeBudgetExceeded,
				Quality:        score.Quality,
				Reason:         "budget_exceeded:max_budget=" + formatUSD(cap),
				RequestedModel: requestedModel,
			}
		}
		cands = kept
	}

	// Delegate to the engine's own picker so the two can never drift:
	// same rules, same tie-breaks, one implementation.
	return okDecisionWithNote(pick(cands, strategy, requestedModel), score, requestedModel, replayNote)
}

// eligibleReplayCandidates is the engine's general-path pool: priced models
// whose tier meets the required level, costed at the replay token estimate.
func eligibleReplayCandidates(snapshot PriceSnapshot, required CapabilityLevel, estTokens int) []candidate {
	seen := make(map[string]struct{}, len(snapshot.Prices))
	cands := make([]candidate, 0, len(snapshot.Prices))
	for name, p := range snapshot.Prices {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if !MeetsLevel(name, required) {
			continue
		}
		cands = append(cands, candidate{name: name, level: levelFor(name), estCost: estimateCost(p, estTokens)})
	}
	return cands
}

// forcedReplayDecision mirrors the engine's forced-model branch, annotated
// as a replay.
func forcedReplayDecision(c candidate, strategy Strategy, score QualityScore, requestedModel string) Decision {
	if strategy.MaxBudget != nil && c.estCost > *strategy.MaxBudget {
		return Decision{
			Outcome:        OutcomeBudgetExceeded,
			Model:          c.name,
			Quality:        score.Quality,
			Reason:         "budget_exceeded:forced_model_over_cap",
			RequestedModel: requestedModel,
		}
	}
	return okDecisionWithNote(c, score, requestedModel, replayNote)
}

// okDecisionWithNote builds a success decision whose reason carries the
// S2-frozen "chose_" segment plus the replay audit note, keeping downstream
// reason parsing intact.
func okDecisionWithNote(c candidate, score QualityScore, requestedModel, note string) Decision {
	return Decision{
		Outcome:        OutcomeOK,
		Model:          c.name,
		EstCostUSD:     c.estCost,
		Quality:        score.Quality,
		Reason:         reasonFor(c.name, score.Quality, c.estCost) + " [" + note + "]",
		RequestedModel: requestedModel,
	}
}

// decisionOutcome maps the engine's Decision onto a replay row.
func decisionOutcome(d Decision, requestID string) ReplayOutcome {
	return ReplayOutcome{
		RequestID:  requestID,
		Model:      d.Model,
		EstCostUSD: d.EstCostUSD,
		Quality:    d.Quality,
		Reason:     d.Reason,
		Outcome:    d.Outcome,
	}
}

// snapshotStaleAsOf marks a price snapshot stale as of now under the given
// horizon — the report header's stale flag is computed from the snapshot's
// own SyncedAt at report generation time, never re-resolved against a live
// source. (The staleness rule lives on Snapshot; the decision-facing
// PriceSnapshot shares its SyncedAt.)
func snapshotStaleAsOf(snapshot PriceSnapshot, now time.Time, horizon time.Duration) bool {
	return Snapshot{Prices: snapshot.Prices, SyncedAt: snapshot.SyncedAt}.Stale(now, horizon)
}
