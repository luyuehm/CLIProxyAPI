package contentfilter

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestExportMaskerRemasksSensitiveColumns is the RIC-443 CONTEXT_RULE guard:
// exported audit data must stay masked and must not leak sensitive original
// text (raw_preview / matches) verbatim.
func TestExportMaskerRemasksSensitiveColumns(t *testing.T) {
	e := NewEngine(true)
	rules := []*Rule{
		newTestRule(1, "phone", nil, []PIIType{PIIPhone}),
		newTestRule(2, "email", nil, []PIIType{PIIEmail}),
	}

	masker := exportMasker(e, rules)

	// MATCHES: stored as JSON array of the raw detected values; each value
	// must come out masked.
	rec := &ExportRecord{
		RuleID:     1,
		RuleName:   "phone",
		RawPreview: "联系电话 13812345678 请查收",
		Matches:    `["13812345678"]`,
		Model:      "gpt-5",
	}
	masker(rec)
	if rec.RawPreview == "联系电话 13812345678 请查收" {
		t.Fatalf("raw_preview was not re-masked: %q", rec.RawPreview)
	}
	if strings.Contains(rec.RawPreview, "13812345678") {
		t.Fatalf("raw_preview leaked the phone: %q", rec.RawPreview)
	}
	var maskedMatches []string
	if err := json.Unmarshal([]byte(rec.Matches), &maskedMatches); err != nil {
		t.Fatalf("matches no longer a JSON array: %q (%v)", rec.Matches, err)
	}
	if len(maskedMatches) != 1 {
		t.Fatalf("matches len = %d, want 1: %q", len(maskedMatches), rec.Matches)
	}
	if maskedMatches[0] == "13812345678" || strings.Contains(maskedMatches[0], "13812345678") {
		t.Fatalf("matches leaked the raw phone: %q", maskedMatches[0])
	}

	// EMAIL
	rec2 := &ExportRecord{
		RuleID:     2,
		RuleName:   "email",
		RawPreview: "请发送 alice@example.com",
		Matches:    `["alice@example.com"]`,
		Model:      "gpt-5",
	}
	masker(rec2)
	if strings.Contains(rec2.RawPreview, "alice@example.com") {
		t.Fatalf("raw_preview leaked the email: %q", rec2.RawPreview)
	}
	var maskedMatches2 []string
	_ = json.Unmarshal([]byte(rec2.Matches), &maskedMatches2)
	if len(maskedMatches2) == 1 && maskedMatches2[0] == "alice@example.com" {
		t.Fatalf("matches leaked the raw email: %q", maskedMatches2[0])
	}
}

// TestExportMaskerLegacyCSVMatches ensures a legacy RIC-438 comma-joined
// matches column (not a JSON array) is left intact rather than failing the
// whole export.
func TestExportMaskerLegacyCSVMatches(t *testing.T) {
	e := NewEngine(true)
	rules := []*Rule{newTestRule(1, "phone", nil, []PIIType{PIIPhone})}
	masker := exportMasker(e, rules)

	rec := &ExportRecord{
		RuleID:     1,
		RuleName:   "phone",
		RawPreview: "no sensitive text here",
		Matches:    "13812345678",
		Model:      "gpt-5",
	}
	masker(rec)
	if rec.Matches != "13812345678" {
		t.Fatalf("legacy CSV matches mutated: %q", rec.Matches)
	}
}

// TestExportMaskerNoRulesIsNoop ensures nil/empty rule sets keep the export
// working rather than failing (the stored preview is already truncated).
func TestExportMaskerNoRulesIsNoop(t *testing.T) {
	e := NewEngine(true)
	masker := exportMasker(e, nil)
	rec := &ExportRecord{RawPreview: "anything", Matches: `["x"]`}
	masker(rec)
	if rec.RawPreview != "anything" || rec.Matches != `["x"]` {
		t.Fatalf("no-rules masker mutated record: %+v", rec)
	}
}