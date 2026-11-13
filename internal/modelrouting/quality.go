// Package modelrouting implements request-level model routing for the gateway.
//
// The package is built in stages; each stage owns its own files to avoid
// conflicts between parallel work:
//   - strategy/version/price_cache (S1-A): routing policy schema, immutable
//     versioning, daily price cache.
//   - quality* (S1-B): request-feature heuristic quality scoring and the
//     frozen quality_score.md contract consumed by the S2 decision engine.
//   - engine/replay/report (S2/S3): decision engine and offline replay.
//
// All scoring in this package is deterministic: identical inputs always
// produce identical outputs. No randomness, no clock reads, no I/O.
package modelrouting

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ContractVersion is the frozen quality_score.md contract version. S2
// consumes exactly this contract shape.
const ContractVersion = 1

// TaskType is the heuristic task classification of a request.
type TaskType string

const (
	// TaskTypeCode marks requests containing code (fenced blocks, source
	// file paths, source-language constructs).
	TaskTypeCode TaskType = "code"
	// TaskTypeAnalysis marks multi-turn or long-document analysis requests.
	TaskTypeAnalysis TaskType = "analysis"
	// TaskTypeSimple marks short single-turn requests.
	TaskTypeSimple TaskType = "simple"
	// TaskTypeDefault is the fallback for empty, malformed, or otherwise
	// unrecognized payloads. Scoring never panics and never returns 0.
	TaskTypeDefault TaskType = "default"
)

// LengthBucket is the context-length tier, estimated from extracted text
// length (estimated tokens = total chars / 4).
type LengthBucket string

const (
	// LengthBucketShort: estimated tokens <= 4000.
	LengthBucketShort LengthBucket = "short"
	// LengthBucketMedium: estimated tokens 4001-32000.
	LengthBucketMedium LengthBucket = "medium"
	// LengthBucketLong: estimated tokens > 32000.
	LengthBucketLong LengthBucket = "long"
)

// CapabilityLevel is a model capability tier; higher is more capable.
// The registry ModelInfo carries no capability field, so tiers are derived
// from a model-name table (modelLevelRules).
type CapabilityLevel int

const (
	// LevelLite covers small/fast models (haiku, flash-lite, mini).
	LevelLite CapabilityLevel = 1
	// LevelStandard covers mainstream models (sonnet, flash, gpt base).
	LevelStandard CapabilityLevel = 2
	// LevelAdvanced covers strong models (opus, 2.5/5.x pro-class, k2, grok-4).
	LevelAdvanced CapabilityLevel = 3
	// LevelFrontier covers top-tier models (latest opus/fable, gpt-6, k3).
	LevelFrontier CapabilityLevel = 4
)

// QualityScore is the frozen S1-B output consumed by the S2 decision engine.
type QualityScore struct {
	// TaskType is the heuristic task class.
	TaskType TaskType `json:"task_type"`
	// LengthBucket is the context-length tier.
	LengthBucket LengthBucket `json:"length_bucket"`
	// Quality is the requested capability floor in [0,1]. It is a pure
	// deterministic function of (TaskType, LengthBucket) — see
	// qualityTable. S2 filters candidate models by RequiredLevel().
	Quality float64 `json:"quality"`
}

// Frozen scoring constants (see quality_score.md, do not tune ad hoc).
const (
	// qualityThresholdStandard: upper edge of the LevelLite band — Quality
	// at/below 0.35 is satisfied by LevelLite.
	qualityThresholdStandard = 0.35
	// qualityThresholdAdvanced: upper edge of the LevelStandard band — Quality
	// above 0.35 and at/below 0.55 needs LevelStandard.
	qualityThresholdAdvanced = 0.55
	// qualityThresholdFrontier: upper edge of the LevelAdvanced band — Quality
	// above 0.55 and at/below 0.75 needs LevelAdvanced; above 0.75 needs
	// LevelFrontier.
	qualityThresholdFrontier = 0.75

	// bucketShortMaxTokens / bucketMediumMaxTokens are the length-tier cuts.
	bucketShortMaxTokens  = 4000
	bucketMediumMaxTokens = 32000

	// tokensPerChar is the token estimate factor (chars / 4).
	tokensPerChar = 4

	// simpleMaxChars: at/below this extracted text length a request counts
	// as short single-turn "simple".
	simpleMaxChars = 400
	// analysisMultiTurnMinUserTurns: >= this many user turns is multi-turn.
	analysisMultiTurnMinUserTurns = 3
	// analysisLongDocTokens: estimated tokens at/above this are long-document.
	analysisLongDocTokens = 8000
)

// qualityTable is the frozen (TaskType, LengthBucket) -> Quality contract
// table. Values are audited in TestQualityTableInvariants; changing them
// requires bumping ContractVersion.
var qualityTable = map[TaskType]map[LengthBucket]float64{
	TaskTypeDefault:  {LengthBucketShort: 0.30, LengthBucketMedium: 0.40, LengthBucketLong: 0.55},
	TaskTypeSimple:   {LengthBucketShort: 0.30, LengthBucketMedium: 0.35, LengthBucketLong: 0.50},
	TaskTypeCode:     {LengthBucketShort: 0.45, LengthBucketMedium: 0.55, LengthBucketLong: 0.70},
	TaskTypeAnalysis: {LengthBucketShort: 0.50, LengthBucketMedium: 0.65, LengthBucketLong: 0.80},
}

// modelLevelRules is the ordered name -> capability-level table. First match
// wins; entries are ordered specific (high tiers) before general. Unknown
// models match nothing and are treated conservatively (LevelLite only).
type modelLevelRule struct {
	kind  string // "prefix" or "contains"
	match string
	level CapabilityLevel
}

const (
	rulePrefix   = "prefix"
	ruleContains = "contains"
)

var modelLevelRules = []modelLevelRule{
	// Frontier (L4) — most specific first.
	{rulePrefix, "claude-fable", LevelFrontier},
	{rulePrefix, "claude-opus-4-6", LevelFrontier},
	{rulePrefix, "claude-opus-4-7", LevelFrontier},
	{rulePrefix, "claude-opus-4-8", LevelFrontier},
	{rulePrefix, "claude-opus-5", LevelFrontier},
	{rulePrefix, "gpt-6", LevelFrontier},
	{rulePrefix, "gemini-3.1-pro-low", LevelAdvanced}, // low variant of a frontier family
	{rulePrefix, "gemini-3.1-pro", LevelFrontier},
	{rulePrefix, "gemini-3-pro", LevelFrontier},
	{rulePrefix, "kimi-k3", LevelFrontier},
	// Advanced (L3).
	{rulePrefix, "claude-opus", LevelAdvanced}, // opus revisions 4-1..4-5
	{ruleContains, "opus", LevelAdvanced},
	{rulePrefix, "gemini-2.5-pro", LevelAdvanced},
	{rulePrefix, "gpt-5.5", LevelAdvanced},
	{rulePrefix, "gpt-5.6", LevelAdvanced},
	{rulePrefix, "gpt-oss-120b", LevelAdvanced},
	{rulePrefix, "kimi-k2", LevelAdvanced},
	{rulePrefix, "grok-4", LevelAdvanced},
	{ruleContains, "pro", LevelAdvanced}, // other *-pro variants
	// Lite (L1) substring fallbacks must precede the "flash"/family catch-alls.
	// "-mini" (not "mini") so "gemini" does not accidentally match.
	{ruleContains, "flash-lite", LevelLite},
	{ruleContains, "haiku", LevelLite},
	{ruleContains, "-mini", LevelLite},
	{ruleContains, "-fast", LevelLite},
	{ruleContains, "lite", LevelLite},
	// Standard (L2) family catch-alls.
	{ruleContains, "sonnet", LevelStandard},
	{ruleContains, "flash", LevelStandard},
	{ruleContains, "claude", LevelStandard},
	{ruleContains, "gemini", LevelStandard},
	{ruleContains, "gpt", LevelStandard},
	{ruleContains, "kimi", LevelStandard},
	{ruleContains, "grok", LevelStandard},
	{ruleContains, "codex", LevelStandard},
}

// fenceMarkers detect fenced code blocks.
var fenceMarkers = []string{"```", "~~~"}

// codePathRe matches file-path-like tokens with source-code extensions
// (e.g. "./main.go", "/usr/lib/x.c", "file://repo/src/a.ts").
var codePathRe = regexp.MustCompile(`(?i)(?:[/\\.]|file://)(?:[\w.-]+/)*[\w.-]*\.(?:go|py|js|jsx|ts|tsx|java|kt|rs|c|cpp|cc|h|hpp|cs|rb|php|swift|sh|sql)\b`)

// codeKeywordRe matches common source-language constructs (e.g. `func Main(`,
// `def run(`, `class Foo:`, `#include <stdio.h>`, `import {x} from 'y'`).
var codeKeywordRe = regexp.MustCompile(`\bfunc\s+\w+\s*\(|\bdef\s+\w+\s*\(|\bclass\s+\w+\s*:|#[\s]*include\s+<|\bpublic\s+static\s|\bimport\s+(?:\{|\w+\s+from\s+['"]|['"])`)

// Score scores a routing request deterministically from its payload.
// Only Body drives scoring in contract v1; the other ModelRouteRequest
// fields are documented as not consumed (see quality_score.md).
func Score(req pluginapi.ModelRouteRequest) QualityScore {
	return ScoreBody(req.Body)
}

// ScoreBody scores a raw request payload (OpenAI/Anthropic/Gemini chat JSON).
// Empty, truncated, or unrecognized payloads degrade to the default tier —
// they never panic and never return a 0 quality.
func ScoreBody(body []byte) QualityScore {
	if len(bytes.TrimSpace(body)) == 0 || !json.Valid(body) {
		return defaultScore()
	}
	text, userTurns, hasContent := extractFeatures(body)
	if !hasContent {
		// Valid JSON but no recognizable chat payload: unrecognized.
		return defaultScore()
	}
	task := classifyTask(text, userTurns, len(text))
	bucket := lengthBucket(len(text))
	return QualityScore{
		TaskType:     task,
		LengthBucket: bucket,
		Quality:      qualityTable[task][bucket],
	}
}

// defaultScore is the contract boundary value for empty or unrecognized
// payloads: TaskTypeDefault + short bucket + 0.30.
func defaultScore() QualityScore {
	return QualityScore{
		TaskType:     TaskTypeDefault,
		LengthBucket: LengthBucketShort,
		Quality:      qualityTable[TaskTypeDefault][LengthBucketShort],
	}
}

// RequiredLevel returns the minimum model capability tier satisfying this
// score, per the frozen threshold bands in quality_score.md.
func (s QualityScore) RequiredLevel() CapabilityLevel {
	switch {
	case s.Quality <= qualityThresholdStandard:
		return LevelLite
	case s.Quality <= qualityThresholdAdvanced:
		return LevelStandard
	case s.Quality <= qualityThresholdFrontier:
		return LevelAdvanced
	default:
		return LevelFrontier
	}
}

// LevelForModel resolves a model's capability tier from its name. ok is
// false when no table rule matches (unknown model).
func LevelForModel(name string) (CapabilityLevel, bool) {
	return modelLevel(normalizeModelName(name))
}

// MeetsLevel reports whether a model satisfies the required capability tier.
// Unknown models are conservative: they serve LevelLite requests only.
func MeetsLevel(name string, required CapabilityLevel) bool {
	lvl, ok := modelLevel(normalizeModelName(name))
	if !ok {
		return required == LevelLite
	}
	return lvl >= required
}

// classifyTask applies the frozen rule list (5 rules, first match wins):
//
//   - R1 fenced code block present            -> code
//   - R2 source file path or code construct   -> code
//   - R3 multi-turn (>=3 user turns) or long
//     document (>= 8000 est. tokens)          -> analysis
//   - R4 short single/double turn (<= 400 chars, <=2 user turns) -> simple
//   - R5 fallback                             -> default
func classifyTask(text string, userTurns, totalChars int) TaskType {
	if containsFence(text) {
		return TaskTypeCode
	}
	if codePathRe.MatchString(text) || codeKeywordRe.MatchString(text) {
		return TaskTypeCode
	}
	estTokens := totalChars / tokensPerChar
	if userTurns >= analysisMultiTurnMinUserTurns || estTokens >= analysisLongDocTokens {
		return TaskTypeAnalysis
	}
	if totalChars <= simpleMaxChars && userTurns <= 2 {
		return TaskTypeSimple
	}
	return TaskTypeDefault
}

// lengthBucket maps total extracted chars to the frozen tier cuts.
func lengthBucket(totalChars int) LengthBucket {
	tokens := totalChars / tokensPerChar
	switch {
	case tokens <= bucketShortMaxTokens:
		return LengthBucketShort
	case tokens <= bucketMediumMaxTokens:
		return LengthBucketMedium
	default:
		return LengthBucketLong
	}
}

// extractFeatures pulls text and user-turn counts out of the three supported
// chat payload shapes (OpenAI/Anthropic messages, Gemini contents, legacy
// prompt). hasContent is false when none of those fields is present.
func extractFeatures(body []byte) (text string, userTurns int, hasContent bool) {
	var sb strings.Builder

	// OpenAI / Anthropic: messages[].content (string or content-block array).
	msgs := gjson.GetBytes(body, "messages")
	if msgs.Exists() {
		hasContent = true
		for _, m := range msgs.Array() {
			if m.Get("role").String() == "user" {
				userTurns++
			}
			appendContent(&sb, m.Get("content"))
		}
		if sys := gjson.GetBytes(body, "system"); sys.Exists() && sys.Type == gjson.String {
			sb.WriteString(sys.String())
		}
	}

	// Gemini: contents[].parts[].text, roles "user"/"model".
	contents := gjson.GetBytes(body, "contents")
	if contents.Exists() {
		hasContent = true
		for _, c := range contents.Array() {
			if c.Get("role").String() == "user" {
				userTurns++
			}
			for _, p := range c.Get("parts").Array() {
				if t := p.Get("text"); t.Exists() {
					sb.WriteString(t.String())
				}
			}
		}
	}

	// Legacy: prompt (string or array of strings).
	if p := gjson.GetBytes(body, "prompt"); p.Exists() {
		hasContent = true
		sb.WriteString(p.String())
	}

	return sb.String(), userTurns, hasContent
}

// appendContent writes a messages[] content value: a plain string, or the
// text blocks of an array of content objects (image blocks are ignored).
func appendContent(sb *strings.Builder, content gjson.Result) {
	switch content.Type {
	case gjson.String:
		sb.WriteString(content.String())
	default: // array of blocks or null: walk block objects for text fields.
		for _, block := range content.Array() {
			if t := block.Get("text"); t.Exists() {
				sb.WriteString(t.String())
			}
		}
	}
}

// containsFence reports whether text contains a fenced code block marker.
func containsFence(text string) bool {
	for _, marker := range fenceMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// normalizeModelName canonicalizes a model name for table lookup.
func normalizeModelName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// modelLevel applies the ordered name rules; first match wins.
func modelLevel(name string) (CapabilityLevel, bool) {
	for _, rule := range modelLevelRules {
		switch rule.kind {
		case rulePrefix:
			if strings.HasPrefix(name, rule.match) {
				return rule.level, true
			}
		default: // ruleContains
			if strings.Contains(name, rule.match) {
				return rule.level, true
			}
		}
	}
	return 0, false
}
