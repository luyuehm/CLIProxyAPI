package modelrouting

// The multi-strategy comparison report (RIC-2525 / OPENROUT-002-S3). Re-runs
// the same traffic sample set under N strategies (the cheap / balanced /
// high_quality presets and any user copy) and emits a JSON-serializable
// report: a per-strategy summary row (total cost, avg cost/request, mean
// quality, below-floor ratio) plus a per-request diff detail. The report is
// produced offline — the price snapshot is a fixed input, and a stale
// snapshot is flagged in the header, never silently re-resolved.

import (
	"bufio"
	"encoding/json"
	"io"
	"time"
)

// NamedStrategy pairs a report row label with the strategy it replays under.
type NamedStrategy struct {
	// Name is the frozen report row key (a preset name, or a user label for
	// a copied custom strategy).
	Name string
	// Strategy is the strategy the row replays with.
	Strategy Strategy
}

// ReportOptions configures a BuildReport run.
type ReportOptions struct {
	// Now is the report generation instant used for the stale flag;
	// time.Now when unset (tests inject a fixed one so the flag is
	// assertable without wall-clock).
	Now time.Time
	// StaleHorizon bounds how old a price snapshot may be before the header
	// marks it stale; DailyInterval (24h) when unset.
	StaleHorizon time.Duration
	// Details caps each strategy's per-request detail capture (0 = the
	// default cap). The summary rows are always full-input; only the
	// detail and the request-diff sections are capped.
	Details int
}

// ComparisonReport is one full multi-strategy comparison: a header carrying
// the price-snapshot provenance (including the stale flag) and, per
// strategy, a summary row plus per-request detail, and a cross-strategy
// per-request diff.
type ComparisonReport struct {
	// Header carries the report instant and the price-snapshot provenance.
	Header ComparisonHeader `json:"header"`
	// Strategies is one summary + detail row per named strategy, input
	// order (deterministic: the caller supplies the order).
	Strategies []StrategyReport `json:"strategies"`
	// RequestDiff is the cross-strategy per-request detail: for each
	// request_id, which model each strategy chose. Capped like the
	// per-strategy detail; requests without a request_id (synthesized
	// samples) have no join key and are omitted.
	RequestDiff []RequestDiffRow `json:"request_diff,omitempty"`
	// DiffTruncated is true when RequestDiff stopped at the capture cap
	// before the input ended.
	DiffTruncated bool `json:"diff_truncated,omitempty"`
}

// ComparisonHeader is the report's provenance block: when it was generated
// and the price snapshot it ran on, including the stale flag.
type ComparisonHeader struct {
	// GeneratedAt is when the report was produced.
	GeneratedAt time.Time `json:"generated_at"`
	// PriceSnapshot is the provenance of the price table the run used.
	PriceSnapshot PriceSnapshotHeader `json:"price_snapshot"`
}

// PriceSnapshotHeader records the price table's sync time and staleness.
// Stale is computed from the snapshot's own SyncedAt at generation — it is
// NEVER a live re-pull, which is the "no silent recompute" guarantee.
type PriceSnapshotHeader struct {
	// SyncedAt is when the price table was last synced; zero = never.
	SyncedAt time.Time `json:"synced_at"`
	// Stale is true when the snapshot is older than the horizon.
	Stale bool `json:"stale"`
	// StaleHorizon is the horizon the stale flag was computed against.
	StaleHorizon time.Duration `json:"stale_horizon"`
}

// StrategyReport is one strategy's row: its summary plus, optionally, the
// per-request detail.
type StrategyReport struct {
	// Name is the report row key.
	Name string `json:"name"`
	// Strategy is the strategy the row ran under.
	Strategy Strategy `json:"strategy"`
	// Summary aggregates the strategy's run over the full sample set.
	Summary ReplaySummary `json:"summary"`
	// Details carries one row per request, input order, when captured.
	Details []ReplayOutcome `json:"details,omitempty"`
	// DetailsTruncated marks a capped detail that stopped before the input
	// ended (Summary still covers the full input).
	DetailsTruncated bool `json:"details_truncated,omitempty"`
}

// RequestDiffRow is one request's cross-strategy choice: the request_id
// and, per strategy name, the model that strategy's replay picked.
type RequestDiffRow struct {
	// RequestID is the join key.
	RequestID string `json:"request_id"`
	// Chosen maps strategy name -> chosen model (empty model = that
	// strategy's replay did not pick a runnable model for this request).
	Chosen map[string]string `json:"chosen"`
}

// BuildReport re-runs the sample set under every named strategy and
// assembles the comparison. It is the offline comparator: the price
// snapshot is a fixed input (never re-pulled), so the same records +
// strategies + snapshot yield the same report across runs. Each strategy's
// row is computed once; the per-request diff joins those rows' details on
// request_id, so no decision is computed more than once per strategy.
func BuildReport(records []ReplayRecord, strategies []NamedStrategy, snapshot PriceSnapshot, opts ReportOptions) ComparisonReport {
	horizon := opts.StaleHorizon
	if horizon <= 0 {
		horizon = DailyInterval
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	detailCap := opts.effectiveReportDetailCap()

	report := ComparisonReport{
		Header: ComparisonHeader{
			GeneratedAt: now,
			PriceSnapshot: PriceSnapshotHeader{
				SyncedAt:     snapshot.SyncedAt,
				Stale:        snapshotStaleAsOf(snapshot, now, horizon),
				StaleHorizon: horizon,
			},
		},
	}
	for _, ns := range strategies {
		res := replayRecords(records, ns.Strategy, snapshot, ns.Name, detailCap)
		report.Strategies = append(report.Strategies, StrategyReport{
			Name:             ns.Name,
			Strategy:         ns.Strategy,
			Summary:          res.Summary,
			Details:          res.Details,
			DetailsTruncated: res.DetailsTruncated,
		})
	}
	buildRequestDiff(&report, detailCap)
	return report
}

// effectiveReportDetailCap mirrors ReplayOptions' cap for the report path:
// 0 -> default cap, negative -> unlimited, otherwise the value.
func (o ReportOptions) effectiveReportDetailCap() int {
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

// replayRecords runs one strategy over an in-memory record set (the
// multi-strategy comparison shape) with a capped detail slice. The summary
// always covers the full input; only Details is bounded.
func replayRecords(records []ReplayRecord, strategy Strategy, snapshot PriceSnapshot, name string, detailCap int) ReplayResult {
	if err := Validate(strategy); err != nil {
		// Invalid strategy: an empty row, not a silent default — the
		// comparison must not hide a misconfiguration.
		return ReplayResult{StrategyName: name, Strategy: strategy}
	}
	result := ReplayResult{StrategyName: name, Strategy: strategy}
	if detailCap >= 0 {
		result.Details = make([]ReplayOutcome, 0, min(detailCap, 64))
	}
	for i := range records {
		row := replayRecord(records[i], strategy, snapshot)
		accumulate(&result, row)
		appendDetail(&result, row, detailCap)
	}
	if detailCap > 0 && result.Summary.Requests > detailCap {
		result.DetailsTruncated = true
	}
	return result
}

// buildRequestDiff joins the already-computed strategy details on
// request_id — one row per distinct id in first-appearance order across the
// shared record set, so the report is a pure function of the inputs. Rows
// whose requests lack an id have no join key and are left out of the diff
// (the per-strategy details still carry their decisions).
func buildRequestDiff(report *ComparisonReport, detailCap int) {
	if len(report.Strategies) < 2 {
		// A diff across fewer than two strategies is the single-strategy
		// detail already in Strategies; no cross-join to build.
		return
	}
	byID := make([]map[string]string, len(report.Strategies))
	for i, row := range report.Strategies {
		chosen := make(map[string]string, len(row.Details))
		for _, d := range row.Details {
			if !d.Skipped && d.RequestID != "" {
				chosen[d.RequestID] = d.Model
			}
		}
		byID[i] = chosen
	}
	// First-appearance order across the strategy details (identical for
	// every strategy: they all ran the same records in the same order).
	var order []string
	seen := make(map[string]bool)
	for _, row := range report.Strategies {
		for _, d := range row.Details {
			if d.Skipped || d.RequestID == "" || seen[d.RequestID] {
				continue
			}
			seen[d.RequestID] = true
			order = append(order, d.RequestID)
		}
	}
	for _, id := range order {
		chosen := make(map[string]string, len(byID))
		for i, m := range byID {
			// A missing key means this strategy's row for the id was
			// either skipped or fell outside its detail cap: record the
			// absence explicitly.
			if v, ok := m[id]; ok {
				chosen[report.Strategies[i].Name] = v
			} else {
				chosen[report.Strategies[i].Name] = ""
			}
		}
		report.RequestDiff = append(report.RequestDiff, RequestDiffRow{RequestID: id, Chosen: chosen})
		if detailCap > 0 && len(report.RequestDiff) >= detailCap {
			if len(order) > detailCap {
				report.DiffTruncated = true
			}
			break
		}
	}
}

// ParseRecords reads a JSONL stream of ReplayRecords into memory, the
// multi-strategy comparison's input shape. It is the in-memory counterpart
// to Replay's streaming single-strategy path: a comparison applies N
// strategies to one shared set, so the set is held; the per-strategy detail
// is what stays bounded.
func ParseRecords(reader io.Reader) ([]ReplayRecord, error) {
	var out []ReplayRecord
	scanner := newLineScanner(reader)
	for scanner.Scan() {
		// In the parse path a malformed line is a hard error (the caller
		// is importing their own file, not a live stream): surface it
		// rather than silently counting it skipped.
		rec, err := decodeReplayRecord(scanner.Bytes())
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}
	if err := scanner.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// MustParseRecords is ParseRecords with a panic on error, for tests and
// small in-process imports where a bad sample file is a programmer error.
func MustParseRecords(reader io.Reader) []ReplayRecord {
	recs, err := ParseRecords(reader)
	if err != nil {
		panic("modelrouting: parse replay records: " + err.Error())
	}
	return recs
}

// newLineScanner builds the shared JSONL line reader: one record per line,
// a 10 MB ceiling per line (a record may inline a full request body).
func newLineScanner(reader io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	return scanner
}

// EncodeReport renders a report to a JSON document, the deliverable "JSON
// 输出（stdout 或管理 API）" of the ticket. It is stable across runs for
// identical inputs: struct fields marshal in declaration order and map keys
// are sorted by encoding/json, so the output is deterministic.
func EncodeReport(report ComparisonReport) ([]byte, error) {
	return json.MarshalIndent(report, "", "  ")
}
