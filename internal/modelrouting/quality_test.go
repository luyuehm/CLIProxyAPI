package modelrouting

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// openaiBody mirrors the OpenAI/Anthropic chat-completions payload shape the
// gateway receives; tests build these instead of hand-writing JSON.
type openaiBody struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

func marshalOpenAI(t *testing.T, b openaiBody) []byte {
	t.Helper()
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal test payload: %v", err)
	}
	return out
}

// TestScoreDeterministic is acceptance scenario 1: the same request scored 10
// times yields identical results, all fields included.
func TestScoreDeterministic(t *testing.T) {
	body := marshalOpenAI(t, openaiBody{
		Model: "gpt-5.5",
		Messages: []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{
			{Role: "user", Content: "Refactor ```\nfunc Add(a, b int) int { return a + b }\n``` in ./pkg/math/add.go"},
			{Role: "assistant", Content: "Sure."},
			{Role: "user", Content: "Now add tests for it."},
		},
	})
	first := ScoreBody(body)
	for i := 0; i < 9; i++ {
		if again := ScoreBody(body); !reflect.DeepEqual(again, first) {
			t.Fatalf("run %d = %+v, first run = %+v; scoring is not deterministic", i+1, again, first)
		}
	}
	if first.Quality <= 0 || first.Quality > 1 {
		t.Fatalf("Quality = %v, want within (0, 1]", first.Quality)
	}
}

// TestScoreCodeTask is acceptance scenario 2: a fenced code block with
// short context scores as code at the frozen contract value.
func TestScoreCodeTask(t *testing.T) {
	body := marshalOpenAI(t, openaiBody{
		Model: "claude-sonnet-4-6",
		Messages: []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{
			{Role: "user", Content: "Fix this:\n```\nfunc main() { println(hello) }\n```"},
		},
	})
	got := ScoreBody(body)
	if got.TaskType != TaskTypeCode {
		t.Fatalf("TaskType = %q, want %q", got.TaskType, TaskTypeCode)
	}
	if got.LengthBucket != LengthBucketShort {
		t.Fatalf("LengthBucket = %q, want %q", got.LengthBucket, LengthBucketShort)
	}
	// Frozen contract values: code/short = 0.45, simple/short = 0.30.
	if got.Quality != qualityTable[TaskTypeCode][LengthBucketShort] {
		t.Fatalf("Quality = %v, want frozen contract value %v", got.Quality, qualityTable[TaskTypeCode][LengthBucketShort])
	}
	if got.Quality < qualityTable[TaskTypeSimple][LengthBucketShort] {
		t.Fatalf("code Quality %v below simple floor %v", got.Quality, qualityTable[TaskTypeSimple][LengthBucketShort])
	}
}

// TestScoreTaskRules covers the individual heuristic rules R1-R5.
func TestScoreTaskRules(t *testing.T) {
	cases := []struct {
		name string
		msgs []string
		want TaskType
	}{
		{"fence", []string{"Here ```python\nprint(1)\n```"}, TaskTypeCode},
		{"tilde fence", []string{"~~~sql\nSELECT 1\n~~~"}, TaskTypeCode},
		{"file path", []string{"Update ./internal/proxy/server.go to log errors"}, TaskTypeCode},
		{"code construct", []string{"Write: public static void main(String[] args) {}"}, TaskTypeCode},
		{"multi-turn", []string{"What is X?", "It is Y", "Now explain Z in depth"}, TaskTypeAnalysis},
		{"short single turn", []string{"What is 2+2?"}, TaskTypeSimple},
		{"medium prose no code", []string{strings.Repeat("Discuss the history of networking protocols. ", 20)}, TaskTypeDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := openaiBody{Model: "m"}
			for _, m := range tc.msgs {
				body.Messages = append(body.Messages, struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				}{Role: "user", Content: m})
			}
			if got := ScoreBody(marshalOpenAI(t, body)); got.TaskType != tc.want {
				t.Fatalf("TaskType = %q, want %q", got.TaskType, tc.want)
			}
		})
	}
}

// TestScoreLongDocument is acceptance scenario 3: >32k-token context lands in
// the long tier and demands a higher capability level than the same task type
// at short context.
func TestScoreLongDocument(t *testing.T) {
	long := marshalOpenAI(t, openaiBody{
		Model: "gpt-5.5",
		Messages: []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{
			{Role: "user", Content: strings.Repeat("a", 128004)}, // 32001 estimated tokens
		},
	})
	got := ScoreBody(long)
	if got.LengthBucket != LengthBucketLong {
		t.Fatalf("LengthBucket = %q, want %q", got.LengthBucket, LengthBucketLong)
	}
	if got.TaskType != TaskTypeAnalysis {
		t.Fatalf("TaskType = %q, want %q (long document)", got.TaskType, TaskTypeAnalysis)
	}
	short := QualityScore{TaskType: TaskTypeAnalysis, Quality: qualityTable[TaskTypeAnalysis][LengthBucketShort]}
	if got.RequiredLevel() <= short.RequiredLevel() {
		t.Fatalf("long required level %v not above short required level %v", got.RequiredLevel(), short.RequiredLevel())
	}
}

// TestScoreLengthBucketBoundaries pins the exact tier cuts.
func TestScoreLengthBucketBoundaries(t *testing.T) {
	// Estimated tokens = chars/4. Cuts: <=4000 short, 4001-32000 medium, >32000 long.
	cases := []struct {
		chars int
		want  LengthBucket
	}{
		{400, LengthBucketShort},
		{16000, LengthBucketShort},   // 4000 tokens: last short value
		{16004, LengthBucketMedium},  // 4001 tokens: first medium value
		{128000, LengthBucketMedium}, // 32000 tokens: last medium value
		{128004, LengthBucketLong},   // 32001 tokens: first long value
	}
	for _, tc := range cases {
		body := openaiBody{Model: "m"}
		body.Messages = append(body.Messages, struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{Role: "user", Content: strings.Repeat("a", tc.chars)})
		if got := ScoreBody(marshalOpenAI(t, body)); got.LengthBucket != tc.want {
			t.Fatalf("chars=%d: LengthBucket = %q, want %q", tc.chars, got.LengthBucket, tc.want)
		}
	}
}

// TestScoreBoundaryInputs is acceptance scenario 4: empty, truncated, and
// unrecognized payloads degrade to the default tier without panicking or
// returning 0.
func TestScoreBoundaryInputs(t *testing.T) {
	complete := marshalOpenAI(t, openaiBody{Model: "m", Messages: []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{{Role: "user", Content: "hello"}}})
	truncated := complete[:len(complete)-3] // valid JSON no longer parses

	inputs := [][]byte{
		nil,
		{},
		[]byte("   \t\n"),
		truncated,
		[]byte(`{"foo":"bar"}`), // valid JSON, no chat fields
		[]byte(`[1,2,3]`),       // valid JSON, not an object
		[]byte(`not json`),
	}
	for i, in := range inputs {
		got := ScoreBody(in) // must not panic
		if got.TaskType != TaskTypeDefault {
			t.Fatalf("input %d: TaskType = %q, want %q", i, got.TaskType, TaskTypeDefault)
		}
		if got.LengthBucket != LengthBucketShort {
			t.Fatalf("input %d: LengthBucket = %q, want %q", i, got.LengthBucket, LengthBucketShort)
		}
		if got.Quality <= 0 {
			t.Fatalf("input %d: Quality = %v, want > 0", i, got.Quality)
		}
		if got.Quality != qualityTable[TaskTypeDefault][LengthBucketShort] {
			t.Fatalf("input %d: Quality = %v, want frozen default %v", i, got.Quality, qualityTable[TaskTypeDefault][LengthBucketShort])
		}
	}
}

// TestScoreGeminiContents covers the Gemini contents/parts payload shape.
func TestScoreGeminiContents(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[{"text":"hello"},{"text":"FENCE\nx()\nFENCE"}]}],"systemInstruction":{"parts":[{"text":"be brief"}]}}`
	// Swap the placeholder for a real fence marker to exercise R1 via Gemini shape.
	body = strings.Replace(body, "FENCE", "```", 2)
	got := ScoreBody([]byte(body))
	if got.TaskType != TaskTypeCode {
		t.Fatalf("TaskType = %q, want %q", got.TaskType, TaskTypeCode)
	}
}

// TestQualityTableInvariants guards the frozen contract table: values stay in
// [0,1], never 0, monotone across length tiers, and code never below simple.
func TestQualityTableInvariants(t *testing.T) {
	for task, tiers := range qualityTable {
		for _, want := range []LengthBucket{LengthBucketShort, LengthBucketMedium, LengthBucketLong} {
			q, ok := tiers[want]
			if !ok {
				t.Fatalf("task %q missing tier %q", task, want)
			}
			if q <= 0 || q > 1 {
				t.Fatalf("task %q tier %q: Quality = %v, want within (0, 1]", task, want, q)
			}
		}
		// Required capability must not decrease as context grows.
		l, m, x := tiers[LengthBucketShort], tiers[LengthBucketMedium], tiers[LengthBucketLong]
		if m < l || x < m {
			t.Fatalf("task %q: tiers not monotone: short=%v medium=%v long=%v", task, l, m, x)
		}
	}
	if qualityTable[TaskTypeCode][LengthBucketShort] < qualityTable[TaskTypeSimple][LengthBucketShort] {
		t.Fatal("code floor below simple floor")
	}
}

// TestQualityThresholds pins the Quality -> capability band cuts.
func TestQualityThresholds(t *testing.T) {
	cases := []struct {
		quality float64
		want    CapabilityLevel
	}{
		{0.30, LevelLite},
		{0.35, LevelLite},     // upper band edge inclusive
		{0.36, LevelStandard}, // first value above the L1 band
		{0.55, LevelStandard},
		{0.56, LevelAdvanced},
		{0.75, LevelAdvanced},
		{0.76, LevelFrontier},
		{1.00, LevelFrontier},
	}
	for _, tc := range cases {
		got := QualityScore{Quality: tc.quality}.RequiredLevel()
		if got != tc.want {
			t.Fatalf("Quality %v: RequiredLevel = %v, want %v", tc.quality, got, tc.want)
		}
	}
}

// TestModelLevelMapping pins the model-name -> capability tier table for the
// registry families, plus conservative handling of unknown models.
func TestModelLevelMapping(t *testing.T) {
	cases := []struct {
		name    string
		wantLvl CapabilityLevel
		wantOK  bool
	}{
		{"claude-haiku-4-5-20251001", LevelLite, true},
		{"gemini-2.5-flash-lite", LevelLite, true},
		{"gemini-3.1-flash-lite", LevelLite, true},
		{"grok-3-mini", LevelLite, true},
		{"claude-sonnet-4-6", LevelStandard, true},
		{"gemini-2.5-flash", LevelStandard, true},
		{"gpt-5.3-codex-spark", LevelStandard, true},
		{"codex-auto-review", LevelStandard, true},
		{"claude-opus-4-5-20251101", LevelAdvanced, true},
		{"gpt-5.5", LevelAdvanced, true},
		{"kimi-k2.7-code", LevelAdvanced, true},
		{"grok-4.5", LevelAdvanced, true},
		{"gemini-3.1-pro-low", LevelAdvanced, true},
		{"claude-opus-4-8", LevelFrontier, true},
		{"claude-fable-5-1", LevelFrontier, true},
		{"gpt-6-astra", LevelFrontier, true},
		{"gemini-3.1-pro", LevelFrontier, true},
		{"kimi-k3-256k", LevelFrontier, true},
		{"my-mystery-model", 0, false},
	}
	for _, tc := range cases {
		lvl, ok := LevelForModel(tc.name)
		if ok != tc.wantOK || lvl != tc.wantLvl {
			t.Fatalf("LevelForModel(%q) = (%v, %v), want (%v, %v)", tc.name, lvl, ok, tc.wantLvl, tc.wantOK)
		}
	}

	// Unknown models serve only the lite floor.
	for _, lvl := range []CapabilityLevel{LevelLite, LevelStandard, LevelAdvanced, LevelFrontier} {
		want := lvl == LevelLite
		if got := MeetsLevel("my-mystery-model", lvl); got != want {
			t.Fatalf("MeetsLevel(unknown, %v) = %v, want %v", lvl, got, want)
		}
	}
	if !MeetsLevel("claude-opus-4-8", LevelStandard) {
		t.Fatal("frontier model must satisfy standard tier")
	}
	if MeetsLevel("claude-haiku-4-5-20251001", LevelStandard) {
		t.Fatal("lite model must not satisfy standard tier")
	}
}
