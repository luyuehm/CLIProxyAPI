package modelrouting

// Request-level capability constraints (OPENROUT-003 / RIC-3142).
//
// A client may declare required_capabilities on a routing request. The
// decision engine filters every candidate model BEFORE selection, so a model
// that cannot meet a required capability is never chosen — including on any
// future fallback, because the filtered candidate set is the only universe
// the engine (and any downstream executor) may consider.
//
// The model catalog does not yet carry structured_outputs / function_calling
// labels, so this file is the pinned static capability source. It is derived
// from the frozen capability-tier table (modelLevelRules), which is the
// package's existing proxy for model capability: a higher tier is credited
// with at least the labels of the tier below it. The mapping is deliberately
// conservative — a model is only credited with a capability the proxy is
// confident it has — because the hard contract is "constraints can never be
// bypassed": over-crediting a model would route a structured-output task to a
// model that cannot honor the schema, while under-crediting can only produce
// a 400 (the known-rabbit-hole note in the issue: record a warning, never
// silently degrade).

import (
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

// Capability label constants. These are the request-level capability names a
// client may require. A capability not in this set is never claimed by any
// model, so requiring it filters every candidate (fail-closed).
const (
	// CapabilityStructuredOutputs requires a model that can emit output
	// matching a declared JSON schema (response_format / json_schema).
	CapabilityStructuredOutputs = "structured_outputs"
	// CapabilityFunctionCalling requires a model that can emit native tool /
	// function calls (tools / function_call).
	CapabilityFunctionCalling = "function_calling"
)

// capabilitiesForModel returns the static capability labels a model is known
// to support, ordered and deduplicated. Unknown and LevelLite models resolve
// to the conservative empty-or-tool-only set; a model is never credited with
// a capability the tier proxy cannot vouch for.
func capabilitiesForModel(model string) []string {
	switch levelFor(model) {
	case LevelFrontier, LevelAdvanced, LevelStandard:
		return []string{CapabilityStructuredOutputs, CapabilityFunctionCalling}
	case LevelLite:
		return []string{CapabilityFunctionCalling}
	default:
		return nil
	}
}

// missingCapabilities returns the required capabilities the model does not
// support, in the order the caller required them. An empty result means the
// model satisfies every required capability.
func missingCapabilities(model string, required []string) []string {
	supported := capabilitiesForModel(model)
	missing := make([]string, 0, len(required))
	for _, capability := range required {
		if !containsString(supported, capability) {
			missing = append(missing, capability)
		}
	}
	return missing
}

// containsString reports whether the slice contains the value.
func containsString(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

// requiredCapabilitiesFromBody parses the request-level required_capabilities
// parameter from a routing request body. It accepts either a JSON array of
// strings or a comma-separated string; values are lowercased, trimmed, and
// deduplicated. An absent or empty parameter returns nil (no constraint).
func requiredCapabilitiesFromBody(body []byte) []string {
	if len(body) == 0 {
		return nil
	}
	result := gjson.GetBytes(body, "required_capabilities")
	if !result.Exists() {
		return nil
	}
	var raw []string
	if result.IsArray() {
		for _, item := range result.Array() {
			raw = append(raw, item.String())
		}
	} else {
		value := strings.TrimSpace(result.String())
		if value == "" {
			return nil
		}
		raw = strings.Split(value, ",")
	}
	normalized := normalizeCapabilities(raw)
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

// normalizeCapabilities lowercases, trims, deduplicates, and sorts the raw
// capability names so a decision is deterministic for any input order.
func normalizeCapabilities(raw []string) []string {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		capability := strings.ToLower(strings.TrimSpace(item))
		if capability == "" {
			continue
		}
		if _, exists := seen[capability]; exists {
			continue
		}
		seen[capability] = struct{}{}
		out = append(out, capability)
	}
	sort.Strings(out)
	return out
}

// filterByCapabilities partitions candidates into those that satisfy every
// required capability and the blocked ones (with each one's missing
// capabilities). An empty required set keeps every candidate and reports no
// blocked model, so an unconstrained request behaves exactly as before.
func filterByCapabilities(cands []candidate, required []string) ([]candidate, []BlockedModel) {
	if len(required) == 0 {
		return cands, nil
	}
	kept := make([]candidate, 0, len(cands))
	blocked := make([]BlockedModel, 0)
	for _, c := range cands {
		if missing := missingCapabilities(c.name, required); len(missing) > 0 {
			blocked = append(blocked, BlockedModel{
				Model:               c.name,
				MissingCapabilities: missing,
			})
			continue
		}
		kept = append(kept, c)
	}
	return kept, blocked
}
