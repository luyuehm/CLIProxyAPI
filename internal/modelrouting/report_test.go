package modelrouting

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The comparison-report tests (RIC-2525 contract VERIFY: TestReport*).
// They pin the report's frozen JSON field names, the stale-snapshot header
// guarantee, and the cheap-≤-high_quality ordering under a shared
// snapshot, all against a deterministic input set.

// compareSet is the report test's input set: ids on every row (so the
// per-request diff has a join key), a mixed quality profile, and recorded
// token estimates so costs are deterministic.
func compareSet() []ReplayRecord {
	return []ReplayRecord{
		{RequestID: "c-1", TaskType: TaskTypeSimple, LengthBucket: LengthBucketShort, EstTokens: 64},
		{RequestID: "c-2", TaskType: TaskTypeCode, LengthBucket: LengthBucketShort, EstTokens: 128},
		{RequestID: "c-3", TaskType: TaskTypeAnalysis, LengthBucket: LengthBucketLong, EstTokens: 4096},
	}
}

// TestReportComparison is acceptance scenario 2: the sample set under the
// three presets produces three summary rows with the frozen fields
// (total_cost, avg_cost, avg_quality, below_floor_ratio) plus the
// per-request diff, and cheap's total cost is ≤ high_quality's under the
// shared snapshot (asserted on report fields, not absolute dollars).
func TestReportComparison(t *testing.T) {
	snap := testReplaySnapshot()
	report := BuildReport(compareSet(), namedPresets(), snap, ReportOptions{Now: snap.SyncedAt, Details: -1})

	if len(report.Strategies) != 3 {
		t.Fatalf("want 3 strategy rows, got %d", len(report.Strategies))
	}
	byName := map[string]StrategyReport{}
	for _, row := range report.Strategies {
		byName[row.Name] = row
		// Frozen JSON field names: marshal and look the fields up — a
		// tag drift would break downstream report consumers, and a
		// self-compare against the struct cannot catch it.
		raw, err := json.Marshal(row.Summary)
		if err != nil {
			t.Fatalf("summary must marshal: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("summary must be a JSON object: %v", err)
		}
		for _, key := range []string{"total_cost", "avg_cost", "avg_quality", "below_floor_ratio"} {
			if _, ok := fields[key]; !ok {
				t.Errorf("summary JSON must carry the frozen field %q, got %s", key, string(raw))
			}
		}
	}

	cheap, okCheap := byName[PresetCheap]
	hq, okHQ := byName[PresetHighQuality]
	if !okCheap || !okHQ {
		t.Fatalf("the report must carry cheap and high_quality rows, got %v", byName)
	}
	if cheap.Summary.TotalCostUSD > hq.Summary.TotalCostUSD {
		t.Errorf("cheap's total_cost %v must be ≤ high_quality's %v under one shared snapshot",
			cheap.Summary.TotalCostUSD, hq.Summary.TotalCostUSD)
	}
	// avg_cost is total / decided; with the same row count the ordering
	// carries over.
	if cheap.Summary.AvgCostUSD > hq.Summary.AvgCostUSD {
		t.Errorf("cheap's avg_cost %v must be ≤ high_quality's %v", cheap.Summary.AvgCostUSD, hq.Summary.AvgCostUSD)
	}
	// balanced's floor (0.55) leaves the simple (0.30) and code (0.45)
	// rows below it: the uncuttable "below-floor ratio" must count both.
	bal, okBal := byName[PresetBalanced]
	if !okBal {
		t.Fatalf("the report must carry a balanced row")
	}
	if bal.Summary.BelowFloorCount != 2 || bal.Summary.BelowFloorRatio != 2.0/3.0 {
		t.Errorf("balanced must count the two rows below its 0.55 floor, got %+v", bal.Summary)
	}
	// The per-request diff joins all three rows on the three ids.
	if len(report.RequestDiff) != 3 {
		t.Fatalf("the diff must cover the 3 joined requests, got %d", len(report.RequestDiff))
	}
	diffByID := map[string]RequestDiffRow{}
	for _, d := range report.RequestDiff {
		diffByID[d.RequestID] = d
	}
	// c-3 (analysis/long 0.80) needs the frontier tier: the only priced
	// frontier model is claude-opus-4-7, so cheap converges on it too —
	// the quality requirement outranks the cost goal.
	if got := diffByID["c-3"].Chosen[PresetCheap]; got != "claude-opus-4-7" {
		t.Errorf("c-3 must converge on the frontier model under cheap, got %q", got)
	}
	// c-1 (simple/short 0.30): cheap picks the lite-tier winner;
	// high_quality's floor pushes it to the frontier tier — the spread a
	// user is trying to see.
	if got := diffByID["c-1"].Chosen[PresetCheap]; got != "claude-haiku" {
		t.Errorf("c-1 under cheap must pick the lite winner, got %q", got)
	}
	if got := diffByID["c-1"].Chosen[PresetHighQuality]; got == "claude-haiku" {
		t.Errorf("c-1 under high_quality must not stay on the lite tier, got %q", got)
	}
}

// TestReportStaleHeader is acceptance scenario 4: a price snapshot older
// than the 24h horizon is flagged stale in the report header with its
// timestamp — and the header is computed from the snapshot, never a live
// re-pull (the offline guarantee).
func TestReportStaleHeader(t *testing.T) {
	snap := testReplaySnapshot()
	recs := compareSet()

	// Fresh: generated at the sync instant -> not stale.
	fresh := BuildReport(recs, namedPresets(), snap, ReportOptions{Now: snap.SyncedAt})
	if fresh.Header.PriceSnapshot.Stale {
		t.Errorf("a snapshot generated at its own sync time must not be stale, got %+v", fresh.Header.PriceSnapshot)
	}
	if fresh.Header.PriceSnapshot.SyncedAt != snap.SyncedAt {
		t.Errorf("the header must carry the snapshot's sync timestamp, got %v", fresh.Header.PriceSnapshot.SyncedAt)
	}

	// Stale: generated 25h after the sync -> flagged, with the timestamp
	// preserved (the report never recomputes prices, only the flag).
	late := snap.SyncedAt.Add(25 * time.Hour)
	staleReport := BuildReport(recs, namedPresets(), snap, ReportOptions{Now: late})
	if !staleReport.Header.PriceSnapshot.Stale {
		t.Errorf("a 25h-old snapshot must be flagged stale, got %+v", staleReport.Header.PriceSnapshot)
	}
	if staleReport.Header.PriceSnapshot.SyncedAt != snap.SyncedAt {
		t.Errorf("the stale header must keep the original sync timestamp, got %v", staleReport.Header.PriceSnapshot.SyncedAt)
	}
	// The decisions themselves are unchanged by the flag: the stale report
	// must be byte-identical to the fresh one in its rows.
	if len(staleReport.Strategies) != len(fresh.Strategies) {
		t.Fatalf("stale must flag the header, not the rows: %d vs %d rows", len(staleReport.Strategies), len(fresh.Strategies))
	}
	for i := range fresh.Strategies {
		if staleReport.Strategies[i].Summary != fresh.Strategies[i].Summary {
			t.Fatalf("a stale flag must not change the decided rows (no silent recompute): %+v vs %+v",
				staleReport.Strategies[i].Summary, fresh.Strategies[i].Summary)
		}
	}
}

// TestParseRecords pins the JSONL import path: a clean file parses
// row-for-row in order; a malformed line is a hard error with the
// successfully parsed prefix returned (an imported file is trusted input,
// unlike the streaming path where one bad line is a skip).
func TestParseRecords(t *testing.T) {
	recs, err := ParseRecords(strings.NewReader(sampleJSONL()))
	if err == nil {
		t.Fatalf("the shared sample set carries a malformed line; ParseRecords must report it, got %d records", len(recs))
	}
	if len(recs) != 4 {
		t.Errorf("the 4 well-formed lines must parse before the error, got %d", len(recs))
	}

	clean := strings.Join(sampleLines[:4], "\n") + "\n"
	got, err := ParseRecords(strings.NewReader(clean))
	if err != nil {
		t.Fatalf("a clean file must parse: %v", err)
	}
	if len(got) != 4 || got[0].RequestID != "req-001" || got[3].Body == "" {
		t.Errorf("parse must be row-for-row in order, got %+v", got)
	}
	if got[0].EstTokens != 64 || got[0].TaskType != TaskTypeSimple {
		t.Errorf("feature fields must decode verbatim, got %+v", got[0])
	}

	// MustParseRecords is the programmer-error path: a bad file panics.
	defer func() {
		if recover() == nil {
			t.Error("MustParseRecords must panic on a bad file (programmer-error path)")
		}
	}()
	MustParseRecords(strings.NewReader(`{not json`))
}

// TestEncodeReportStable pins the report's JSON output shape: two encodings
// of the same report are byte-identical, and the document carries the
// header (with the stale flag) plus one row per strategy.
func TestEncodeReportStable(t *testing.T) {
	snap := testReplaySnapshot()
	report := BuildReport(compareSet(), namedPresets(), snap, ReportOptions{Now: snap.SyncedAt, Details: -1})
	a, errA := EncodeReport(report)
	b, errB := EncodeReport(report)
	if errA != nil || errB != nil {
		t.Fatalf("encode failed: %v / %v", errA, errB)
	}
	if string(a) != string(b) {
		t.Fatalf("encoding must be deterministic")
	}
	for _, want := range []string{`"stale"`, `"generated_at"`, `"request_diff"`, `"below_floor_ratio"`} {
		if !strings.Contains(string(a), want) {
			t.Errorf("the encoded report must carry %s", want)
		}
	}
}
