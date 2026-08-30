// Package contentfilter implements realtime content filtering for the CPA gateway.
// It pulls content filter rules from the KEEPER container's SQLite database
// (enterprise-keeper-cpa-usage-keeper-1, read-only), caches them in memory with
// 30s hot reload, and applies them via a Gin middleware: inbound requests have
// sensitive words replaced, outbound responses have PII partially masked
// (e.g. 138****1234), and SSE streams are masked chunk-by-chunk in realtime.
//
// The package is a pure addition: it never writes to KEEPER data and does not
// modify upstream core files (only mounts through the pre-reserved
// api.WithMiddleware() extension point).
package contentfilter

import (
	"strings"
)

// Action values supported by KEEPER content_filter_rules.
const (
	ActionMask = "mask"
)

// PIIType identifies a category of personally identifiable information that a
// rule wants masked. KEEPER stores pii_types as a comma-separated list.
type PIIType string

// Supported PII types. The regexes live in engine.go so that all matching
// logic stays in one place.
const (
	PIIPhone        PIIType = "phone"
	PIIIDCard       PIIType = "id_card"
	PIIEmail        PIIType = "email"
	PIIBankCard     PIIType = "bank_card"
	PIIPassport     PIIType = "passport"
	PIIMedicalRecord PIIType = "medical_record"
)

// Rule mirrors one row of KEEPER's content_filter_rules table. Sensitive
// words and PII types are stored as comma-separated text and parsed into
// slices when the rule is loaded.
type Rule struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Enabled       bool     `json:"enabled"`
	Scenario      string   `json:"scenario"`
	Action        string   `json:"action"`
	SensitiveWords []string `json:"sensitive_words"`
	PIITypes      []PIIType `json:"pii_types"`
	WhiteList     []string `json:"white_list"`
	Models        []string `json:"models"`
	Priority      int64    `json:"priority"`
}

// enabled reports whether the rule is active. A rule must be enabled and carry
// either sensitive words or PII types to be applied.
func (r *Rule) enabled() bool {
	if r == nil || !r.Enabled {
		return false
	}
	return len(r.SensitiveWords) > 0 || len(r.PIITypes) > 0
}

// appliesToModel reports whether the rule targets the given upstream model.
// A "*" or empty model list, or an unknown request model, means the rule
// applies (compliance-first: prefer over-masking to leaking).
func (r *Rule) appliesToModel(model string) bool {
	if r == nil || len(r.Models) == 0 {
		return true
	}
	if strings.TrimSpace(model) == "" {
		return true
	}
	for _, m := range r.Models {
		if m == "*" || strings.EqualFold(m, model) {
			return true
		}
	}
	return false
}

// isWhitelisted reports whether the given value is explicitly excluded from
// masking by the rule's whitelist.
func (r *Rule) isWhitelisted(value string) bool {
	if r == nil || len(r.WhiteList) == 0 {
		return false
	}
	trimmed := strings.TrimSpace(value)
	for _, w := range r.WhiteList {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		if strings.EqualFold(w, trimmed) {
			return true
		}
	}
	return false
}

// parseCSV splits a KEEPER-stored list value into a trimmed, deduplicated
// slice, preserving order. KEEPER stores lists as text; the production data
// uses newline separators (one value per line), while older configs may use
// commas. Both are handled, so the parser is robust to either encoding.
// Empty input yields nil.
func parseCSV(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	// Split on commas OR newlines (KEEPER uses newline in production).
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// parsePIITypes parses a comma-separated PII type list into PIIType values,
// ignoring unknown types so that forward-compatible KEEPER configs degrade
// gracefully instead of erroring.
func parsePIITypes(raw string) []PIIType {
	values := parseCSV(raw)
	if len(values) == 0 {
		return nil
	}
	out := make([]PIIType, 0, len(values))
	for _, v := range values {
		switch PIIType(v) {
		case PIIPhone, PIIIDCard, PIIEmail, PIIBankCard, PIIPassport, PIIMedicalRecord:
			out = append(out, PIIType(v))
		}
	}
	return out
}
