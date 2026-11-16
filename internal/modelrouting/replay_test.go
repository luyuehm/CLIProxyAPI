package modelrouting

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"
)

// testReplaySnapshot is the deterministic price table the replay tests run
// against: five models spanning every capability tier (same tier layout as
// the S2 engine tests), so the cheap / balanced / high_quality presets have
// distinct, assertable winners. SyncedAt is a fixed instant so the stale
// flag is computable without wall-clock.
func testReplaySnapshot() PriceSnapshot {
	return PriceSnapshot{
		Prices: map[string]Price{
			"claude-haiku":      {Model: "claude-haiku", PromptPricePer1M: 1, CompletionPricePer1M: 2, Multiplier: 1},
			"gpt-4o":            {Model: "gpt-4o", PromptPricePer1M: 5, CompletionPricePer1M: 15, Multiplier: 1},
			"claude-sonnet-4-6": {Model: "claude-sonnet-4-6", PromptPricePer1M: 3, CompletionPricePer1M: 12, Multiplier: 1},
			"claude-opus-4-5":   {Model: "claude-opus-4-5", PromptPricePer1M: 15, CompletionPricePer1M: 75, Multiplier: 1},
			"claude-opus-4-7":   {Model: "claude-opus-4-7", PromptPricePer1M: 15, CompletionPricePer1M: 75, Multiplier: 1},
		},
		SyncedAt: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC),
	}
}

// sampleLines is the mixed replay input set the tests share: feature-only
// records (the contract path), a body-only record (the scorer path), one
// malformed line, and one blank line.
var sampleLines = []string{
	`{"request_id":"req-001","task_type":"simple","length_bucket":"short","est_tokens":64}`,
	`{"request_id":"req-002","task_type":"code","length_bucket":"short","est_tokens":128}`,
	`{"request_id":"req-003","task_type":"analysis","length_bucket":"long","est_tokens":4096}`,
	`{"request_id":"req-004","body":"{\"messages\":[{\"role\":\"user\",\"content\":\"Hello, write me a haiku about springs\"}]}","requested_model":"gpt-4o"}`,
	`{"request_id":"req-005",`,
	``,
}

// sampleJSONL joins the shared sample set into one JSONL stream.
func sampleJSONL() string {
	return strings.Join(sampleLines, "\n") + "\n"
}

// namedPresets is the three built-in presets in the report's row order.
func namedPresets() []NamedStrategy {
	out := make([]NamedStrategy, 0, len(PresetStrategies))
	for _, p := range PresetStrategies {
		out = append(out, NamedStrategy{Name: p.Name, Strategy: p.Strategy})
	}
	return out
}

// presetOrDie resolves a preset by name, failing the test on a caller bug.
func presetOrDie(t *testing.T, name string) Strategy {
	t.Helper()
	p, ok := PresetStrategy(name)
	if !ok {
		t.Fatalf("preset %q must be one of the built-ins", name)
	}
	return p.Strategy
}

// replayAll re-runs one strategy over the shared sample set with full detail
// capture, the shape the determinism test compares.
func replayAll(strategy Strategy, snap PriceSnapshot, name string) ReplayResult {
	return Replay(strings.NewReader(sampleJSONL()), strategy, snap, ReplayOptions{StrategyName: name, Details: -1})
}

// TestReplayDeterministic is acceptance scenario 1: the same sample set and
// price snapshot, replayed twice back to back, produce byte-identical
// output (per-row model, cost, quality, reason — the audit property).
func TestReplayDeterministic(t *testing.T) {
	snap := testReplaySnapshot()
	for _, preset := range PresetStrategies {
		first := replayAll(preset.Strategy, snap, preset.Name)
		second := replayAll(preset.Strategy, snap, preset.Name)
		if first.Summary != second.Summary {
			t.Errorf("%s: summary drifted between replays:\nfirst  %+v\nsecond %+v", preset.Name, first.Summary, second.Summary)
		}
		if len(first.Details) != len(second.Details) {
			t.Fatalf("%s: detail length drifted: %d vs %d", preset.Name, len(first.Details), len(second.Details))
		}
		for i := range first.Details {
			a, b := first.Details[i], second.Details[i]
			if a.Model != b.Model || a.EstCostUSD != b.EstCostUSD || a.Quality != b.Quality || a.Reason != b.Reason || a.Outcome != b.Outcome {
				t.Fatalf("%s: row %d drifted between replays:\nfirst  %+v\nsecond %+v", preset.Name, i, a, b)
			}
		}
		// The JSON encoding must be stable too: that is the form the
		// report ships in.
		ja, errA := json.Marshal(first)
		jb, errB := json.Marshal(second)
		if errA != nil || errB != nil {
			t.Fatalf("%s: result must marshal: %v / %v", preset.Name, errA, errB)
		}
		if !bytes.Equal(ja, jb) {
			t.Fatalf("%s: JSON encoding drifted between replays", preset.Name)
		}
	}
}

// TestReplayStreamingBound is the memory-pressure rabbit-hole ruling: a
// 100k-line sample set streams through the default detail cap, the summary
// still covers every line, the detail slice truncates at the cap, and the
// live set after the run stays bounded (one struct per capped row, not per
// input line).
func TestReplayStreamingBound(t *testing.T) {
	const n = 100_000
	snap := testReplaySnapshot()
	strategy := presetOrDie(t, PresetCheap)

	r := &jsonlGenReader{count: n}
	runtime.GC()
	base := liveHeapInuse()

	res := Replay(r, strategy, snap, ReplayOptions{StrategyName: PresetCheap})

	if res.Summary.Requests != n {
		t.Fatalf("summary must cover every streamed line, got %d of %d", res.Summary.Requests, n)
	}
	if !res.DetailsTruncated {
		t.Error("a capped detail must be marked truncated on a 100k-line input")
	}
	if len(res.Details) != defaultDetailCap {
		t.Errorf("detail slice must stop at the %d cap, got %d", defaultDetailCap, len(res.Details))
	}
	// Every row is the same shape: simple/short under cheap -> the cheapest
	// model (claude-haiku) at the recorded 128-token estimate.
	wantRow := res.Details[0]
	if wantRow.Model != "claude-haiku" {
		t.Fatalf("cheap preset must pick the cheapest eligible model, got %q", wantRow.Model)
	}
	wantPerRequest := estimateCost(Price{PromptPricePer1M: 1, CompletionPricePer1M: 2, Multiplier: 1}, 128)
	if res.Summary.TotalCostUSD != float64(n)*wantPerRequest {
		// Sequential float accumulation is deterministic; compare against
		// the same accumulation to avoid a tolerance band.
		acc := 0.0
		for i := 0; i < n; i++ {
			acc += wantPerRequest
		}
		if res.Summary.TotalCostUSD != acc {
			t.Errorf("total cost mismatch: got %v, want %v", res.Summary.TotalCostUSD, acc)
		}
	}

	// Peak-memory bound via a deterministic live-object metric: after the
	// run the live set is the capped detail slice plus O(1) summary state —
	// never one struct per input line. An 8 MiB budget isolates the
	// capped slice from GC noise.
	after := liveHeapInuse()
	if delta := after - base; delta > 8<<20 {
		t.Errorf("post-run live heap grew %d bytes; the 100k-line replay must stay bounded (8 MiB budget)", delta)
	}
}

// liveHeapInuse is the deterministic live-object metric for the memory
// bound: total bytes currently allocated and in use (HeapAlloc). It
// isolates what the replay RETAINED from the GC churn of the 100k-line
// stream.
func liveHeapInuse() int64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

// TestReplaySkips is the contract boundary: a record carrying neither the
// scorer features nor a body is skipped with the reason recorded; a
// malformed line and a blank line are counted, never fatal to the stream.
func TestReplaySkips(t *testing.T) {
	snap := testReplaySnapshot()
	res := replayAll(presetOrDie(t, PresetCheap), snap, PresetCheap)
	// Skipped rows: the malformed req-005 line and the blank line.
	if res.Summary.Skipped != 2 {
		t.Fatalf("want 2 skipped rows, got %d", res.Summary.Skipped)
	}
	if res.Summary.Requests != 6 {
		t.Fatalf("want 6 rows seen, got %d", res.Summary.Requests)
	}
	byID := map[string]ReplayOutcome{}
	skipReasons := map[string]int{}
	for _, d := range res.Details {
		if d.RequestID != "" {
			byID[d.RequestID] = d
		} else {
			skipReasons[d.Reason]++
		}
	}
	if n := skipReasons["malformed_record"]; n != 1 {
		t.Errorf("the malformed line must be a recorded skip, got %+v", skipReasons)
	}
	// req-004 (body-only) is decided by the local scorer, not skipped.
	if got := byID["req-004"]; got.Outcome != OutcomeOK || got.Model == "" {
		t.Errorf("req-004 must decide ok via the body scorer, got %+v", got)
	}
	// req-001 (simple/short 0.30) sits below the cheap floor: the
	// below-floor counter must catch it.
	if got := byID["req-001"]; got.Outcome != OutcomeOK {
		t.Errorf("req-001 must decide ok, got %+v", got)
	}
	if res.Summary.BelowFloorCount == 0 {
		t.Error("the simple-tier rows sit below the cheap floor; the below-floor counter must be nonzero")
	}
}

// TestReplayOffline is acceptance scenario 3: a full multi-strategy replay
// runs to completion in an environment with no provider auth and no network
// access (this suite runs without upstream credentials). The pipeline
// touches only the in-memory snapshot and the frozen scorer — the Price
// source (the package's only network-facing type) is never constructed
// here.
func TestReplayOffline(t *testing.T) {
	snap := testReplaySnapshot()
	// A clean import set (no malformed lines) drives the full
	// parse -> multi-strategy report -> encode pipeline offline.
	clean := strings.Join(sampleLines[:4], "\n") + "\n"
	recs := MustParseRecords(strings.NewReader(clean))
	report := BuildReport(recs, namedPresets(), snap, ReportOptions{Now: snap.SyncedAt, Details: -1})
	if len(report.Strategies) != len(PresetStrategies) {
		t.Fatalf("want %d strategy rows, got %d", len(PresetStrategies), len(report.Strategies))
	}
	for _, row := range report.Strategies {
		if row.Summary.Requests != 4 {
			t.Errorf("%s: an offline replay must decide every replayable row", row.Name)
		}
	}
	if out, err := EncodeReport(report); err != nil || len(out) == 0 {
		t.Fatalf("report encoding failed offline: %v", err)
	}
}

// TestReplayEstTokenPath pins the cost-factor contract: a record that names
// the token estimate replays at exactly the engine's cost for that
// estimate, and a body-carrying record derives the estimate from its own
// length (the frozen chars/4 factor).
func TestReplayEstTokenPath(t *testing.T) {
	snap := testReplaySnapshot()
	strategy := presetOrDie(t, PresetCheap)

	// Body-only: cost must equal the engine's estimate for len(body)/4.
	body := `{"messages":[{"role":"user","content":"hi"}]}`
	rec := ReplayRecord{RequestID: "body-rec", Body: body}
	got := replayRecord(rec, strategy, snap)
	wantCost := estimateCost(snap.Prices["claude-haiku"], len(body)/estimateTokensPerChar)
	if got.Outcome != OutcomeOK || got.EstCostUSD != wantCost {
		t.Fatalf("body-only record must cost the engine's estimate: got %+v, want cost %v", got, wantCost)
	}

	// est_tokens wins over any recomputation when both are present.
	rec.EstTokens = 1
	got = replayRecord(rec, strategy, snap)
	if got.EstCostUSD != estimateCost(snap.Prices["claude-haiku"], 1) {
		t.Errorf("recorded est_tokens must be the cost input, got %v", got.EstCostUSD)
	}
}

// TestReplayInvalidStrategy pins the misconfiguration guard: an invalid
// strategy (the engine would reject it) yields an empty row, never a
// silent downgrade to the default.
func TestReplayInvalidStrategy(t *testing.T) {
	snap := testReplaySnapshot()
	res := Replay(strings.NewReader(sampleJSONL()), Strategy{Optimization: "bogus", QualityFloor: 0.5}, snap, ReplayOptions{Details: -1})
	if res.Summary.Requests != 0 || len(res.Details) != 0 {
		t.Fatalf("an invalid strategy must produce an empty row, got %+v", res.Summary)
	}
}

// jsonlGenReader streams n lines of the 100k-row sample shape one at a time
// without retaining the set: the memory-bound test's input source.
type jsonlGenReader struct {
	count int
	i     int
	buf   []byte
	pos   int
}

// Read implements io.Reader: one record line per internal fill; when the
// count is exhausted it returns (0, io.EOF), which is what bufio.Scanner
// reads as a clean end of input.
func (r *jsonlGenReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.buf) {
		if r.i >= r.count {
			return 0, io.EOF
		}
		r.buf = []byte(fmt.Sprintf(`{"request_id":"r-%06d","task_type":"simple","length_bucket":"short","est_tokens":128}
`, r.i))
		r.i++
		r.pos = 0
	}
	copied := copy(p, r.buf[r.pos:])
	r.pos += copied
	return copied, nil
}
