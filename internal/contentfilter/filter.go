// Package contentfilter provides sensitive-word and PII redaction for inbound
// chat-style request payloads. It operates on raw request JSON (OpenAI chat,
// Claude messages, and Gemini generateContent shapes) and returns a mutated
// body plus a decision describing whether the request must be blocked.
//
// The package is intentionally self-contained: it depends only on the config
// type and gjson/sjson, never on internal/translator or internal/runtime, so
// the "canonical representation -> per-provider translation" architecture is
// not disturbed.
package contentfilter

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Action constants.
const (
	ActionRedact = "redact"
	ActionBlock  = "block"
	ActionMask   = "mask"
)

// Decision summarizes a single ApplyRequest outcome.
type Decision struct {
	// Blocked is true when the request must be rejected upstream.
	Blocked bool
	// Reason is a human-readable explanation, populated when Blocked.
	Reason string
	// Redactions is the total number of matches replaced or masked.
	Redactions int
	// Changed is true when the payload was mutated.
	Changed bool
}

// Filter applies configured sensitive-word and PII rules to request payloads.
// A zero-value Filter (or one built from a disabled config) is a no-op.
type Filter struct {
	cfg         config.ContentFilterConfig
	words       *SensitiveWordMatcher
	detectors   []PIIDetector
	placeholder string
}

// New builds a Filter from the config. Returns nil when the filter is
// effectively disabled, so callers can skip allocation entirely.
func New(cfg config.ContentFilterConfig) *Filter {
	if !cfg.Enabled {
		return nil
	}
	words := BuildSensitiveWordMatcher(cfg.SensitiveWords, cfg.WholeWord)
	detectors := compileDetectors(normalizePIITypes(cfg.PIITypes))
	if words == nil && len(detectors) == 0 {
		return nil
	}

	placeholder := strings.TrimSpace(cfg.Placeholder)
	if placeholder == "" {
		placeholder = "[REDACTED]"
	}

	action := strings.ToLower(strings.TrimSpace(cfg.Action))
	if action == "" {
		action = ActionRedact
	}

	f := &Filter{
		cfg:         cfg,
		words:       words,
		detectors:   detectors,
		placeholder: placeholder,
	}
	if f.cfg.Action == "" {
		f.cfg.Action = action
	}
	if f.cfg.MinRedactionsToBlock <= 0 {
		f.cfg.MinRedactionsToBlock = 1
	}
	return f
}

func normalizePIITypes(types []string) []string {
	if len(types) == 0 {
		return nil
	}
	for _, t := range types {
		if strings.EqualFold(strings.TrimSpace(t), "all") {
			return AllPIITypes()
		}
	}
	return types
}

// AppliesToModel reports whether the filter should run for the given model and
// protocol. Empty Models/Protocols lists mean "match everything".
func (f *Filter) AppliesToModel(model, protocol string) bool {
	if f == nil {
		return false
	}
	if len(f.cfg.Protocols) > 0 && !matchesAny(strings.ToLower(protocol), f.cfg.Protocols) {
		return false
	}
	if len(f.cfg.Models) > 0 && !matchesAnyPattern(model, f.cfg.Models) {
		return false
	}
	return true
}

// ApplyRequest runs the configured rules against the raw request JSON. It
// mutates message content (system prompts and user/assistant messages) in
// place for OpenAI chat, Claude messages, and Gemini generateContent shapes.
//
// Under the "block" action the returned Decision.Blocked is true once the
// configured threshold is reached; the caller is responsible for emitting the
// HTTP error. Under "redact" and "mask" the payload is rewritten and the
// decision reports the replacement count.
func (f *Filter) ApplyRequest(payload []byte, model, protocol string) ([]byte, *Decision) {
	dec := &Decision{}
	if f == nil {
		return payload, dec
	}
	if !f.AppliesToModel(model, protocol) {
		return payload, dec
	}
	if !jsonLooksLikeChat(payload) {
		return payload, dec
	}

	payload = f.processTextFields(payload, dec)

	if f.cfg.Action == ActionBlock && dec.Redactions >= f.cfg.MinRedactionsToBlock {
		dec.Blocked = true
		dec.Reason = fmt.Sprintf("content filter policy: %d match(es) for model %q", dec.Redactions, model)
	}
	return payload, dec
}

// jsonLooksLikeChat heuristically detects the three supported request shapes so
// non-chat payloads (e.g. image generation, token counts) are left alone.
func jsonLooksLikeChat(payload []byte) bool {
	if !gjson.ValidBytes(payload) {
		return false
	}
	// OpenAI chat / Claude messages: messages array. Gemini: contents array
	// or requests wrapped under request.contents.
	if gjson.GetBytes(payload, "messages").IsArray() {
		return true
	}
	if gjson.GetBytes(payload, "contents").IsArray() {
		return true
	}
	if gjson.GetBytes(payload, "request.contents").IsArray() {
		return true
	}
	if gjson.GetBytes(payload, "request.system_instruction").Exists() || gjson.GetBytes(payload, "request.systemInstruction").Exists() {
		return true
	}
	if gjson.GetBytes(payload, "input").IsArray() {
		return true // OpenAI Responses
	}
	if gjson.GetBytes(payload, "system").Exists() {
		return true // Claude system
	}
	return false
}

// processTextFields walks every text-bearing field in the supported request
// shapes and applies the redaction strategy. The mutated payload is returned.
func (f *Filter) processTextFields(payload []byte, dec *Decision) []byte {
	original := payload

	// OpenAI chat completions: messages[].content (string or array of {type:text,text}).
	payload = f.walkMessagesArray(payload, "messages", dec)

	// OpenAI Responses: input items (string or array of message items).
	payload = f.walkInputArray(payload, "input", dec)

	// Claude: top-level "system" (string or array of text blocks).
	payload = f.walkSystemField(payload, "system", dec)

	// Gemini generateContent: contents[] with parts[].text.
	payload = f.walkGeminiContents(payload, "contents", dec)
	payload = f.walkGeminiContents(payload, "request.contents", dec)

	// Gemini systemInstruction (object with parts[]).
	for _, path := range []string{"systemInstruction", "system_instruction", "request.systemInstruction", "request.system_instruction"} {
		payload = f.walkGeminiSystemInstruction(payload, path, dec)
	}

	dec.Changed = !equalBytes(payload, original)
	return payload
}

func (f *Filter) walkMessagesArray(payload []byte, path string, dec *Decision) []byte {
	messages := gjson.GetBytes(payload, path)
	if !messages.IsArray() {
		return payload
	}
	messages.ForEach(func(key, msg gjson.Result) bool {
		base := path + "." + key.String()
		content := msg.Get("content")
		if !content.Exists() {
			return true
		}
		if content.Type == gjson.String {
			payload = f.redactStringField(payload, base+".content", content.String(), dec)
		} else if content.IsArray() {
			content.ForEach(func(blockKey, block gjson.Result) bool {
				if block.Get("type").String() != "text" {
					return true
				}
				textPath := base + ".content." + blockKey.String() + ".text"
				if t := block.Get("text"); t.Type == gjson.String {
					payload = f.redactStringField(payload, textPath, t.String(), dec)
				}
				return true
			})
		}
		return true
	})
	return payload
}

func (f *Filter) walkInputArray(payload []byte, path string, dec *Decision) []byte {
	input := gjson.GetBytes(payload, path)
	if !input.IsArray() {
		return payload
	}
	input.ForEach(func(key, item gjson.Result) bool {
		base := path + "." + key.String()
		// OpenAI Responses input items may carry role + content (string or array).
		content := item.Get("content")
		if content.Exists() {
			if content.Type == gjson.String {
				payload = f.redactStringField(payload, base+".content", content.String(), dec)
			} else if content.IsArray() {
				content.ForEach(func(blockKey, block gjson.Result) bool {
					if block.Get("type").String() != "input_text" && block.Get("type").String() != "text" {
						return true
					}
					textPath := base + ".content." + blockKey.String() + ".text"
					if t := block.Get("text"); t.Type == gjson.String {
						payload = f.redactStringField(payload, textPath, t.String(), dec)
					}
					return true
				})
			}
		} else if item.Type == gjson.String {
			payload = f.redactStringField(payload, base, item.String(), dec)
		}
		return true
	})
	return payload
}

func (f *Filter) walkSystemField(payload []byte, path string, dec *Decision) []byte {
	system := gjson.GetBytes(payload, path)
	if !system.Exists() {
		return payload
	}
	if system.Type == gjson.String {
		payload = f.redactStringField(payload, path, system.String(), dec)
		return payload
	}
	if system.IsArray() {
		system.ForEach(func(key, block gjson.Result) bool {
			if block.Get("type").String() != "text" {
				return true
			}
			textPath := path + "." + key.String() + ".text"
			if t := block.Get("text"); t.Type == gjson.String {
				payload = f.redactStringField(payload, textPath, t.String(), dec)
			}
			return true
		})
	}
	return payload
}

func (f *Filter) walkGeminiContents(payload []byte, path string, dec *Decision) []byte {
	contents := gjson.GetBytes(payload, path)
	if !contents.IsArray() {
		return payload
	}
	contents.ForEach(func(cKey, content gjson.Result) bool {
		parts := content.Get("parts")
		if !parts.IsArray() {
			return true
		}
		parts.ForEach(func(pKey, part gjson.Result) bool {
			if t := part.Get("text"); t.Type == gjson.String {
				textPath := path + "." + cKey.String() + ".parts." + pKey.String() + ".text"
				payload = f.redactStringField(payload, textPath, t.String(), dec)
			}
			return true
		})
		return true
	})
	return payload
}

func (f *Filter) walkGeminiSystemInstruction(payload []byte, path string, dec *Decision) []byte {
	instruction := gjson.GetBytes(payload, path)
	if !instruction.Exists() {
		return payload
	}
	if instruction.Type == gjson.String {
		payload = f.redactStringField(payload, path, instruction.String(), dec)
		return payload
	}
	parts := instruction.Get("parts")
	if !parts.IsArray() {
		return payload
	}
	parts.ForEach(func(pKey, part gjson.Result) bool {
		if t := part.Get("text"); t.Type == gjson.String {
			textPath := path + ".parts." + pKey.String() + ".text"
			payload = f.redactStringField(payload, textPath, t.String(), dec)
		}
		return true
	})
	return payload
}

// redactStringField applies the configured strategy to a single string value at
// the given JSON path and writes it back when changed.
func (f *Filter) redactStringField(payload []byte, path, text string, dec *Decision) []byte {
	updated, count := f.redactText(text)
	if count == 0 || updated == text {
		return payload
	}
	dec.Redactions += count
	out, errSet := sjson.SetBytes(payload, path, updated)
	if errSet != nil {
		return payload
	}
	return out
}

// redactText runs every configured detector and the sensitive-word matcher
// against the text, returning the redacted result and the match count.
func (f *Filter) redactText(text string) (string, int) {
	total := 0
	if f.words != nil {
		repl := f.placeholder
		if f.cfg.Action == ActionMask {
			repl = "" // mask is applied per-match below
		}
		if f.cfg.Action == ActionMask {
			out := f.maskWords(text, f.words, &total)
			text = out
		} else {
			out, count := f.words.ReplaceAll(text, repl)
			text = out
			total += count
		}
	}
	for _, d := range f.detectors {
		repl := f.placeholder
		if f.cfg.Action == ActionMask {
			repl = "" // masking handled per-detector
		}
		out, count := d.Redact(text, repl)
		if count > 0 {
			text = out
			total += count
		}
	}
	return text, total
}

// maskWords replaces each sensitive-word match with a masked variant keeping
// the first and last rune visible. Single-rune matches become a single mask.
func (f *Filter) maskWords(text string, m *SensitiveWordMatcher, total *int) string {
	if m == nil || m.regex == nil {
		return text
	}
	out := m.regex.ReplaceAllStringFunc(text, func(match string) string {
		*total++
		return maskString(match)
	})
	return out
}

func maskString(s string) string {
	runes := []rune(s)
	n := len(runes)
	switch {
	case n <= 1:
		return "*"
	case n == 2:
		return string(runes[0]) + "*"
	default:
		return string(runes[0]) + strings.Repeat("*", n-2) + string(runes[n-1])
	}
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func matchesAny(lower string, candidates []string) bool {
	for _, c := range candidates {
		if strings.EqualFold(strings.TrimSpace(c), lower) {
			return true
		}
	}
	return false
}

// matchesAnyPattern reports whether value matches any wildcard pattern.
// Patterns support "*" as a trailing-only or full-segment wildcard, matching
// the convention used elsewhere in the config (e.g. "gpt-*", "claude-*").
func matchesAnyPattern(value string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" || p == "*" {
			return true
		}
		if patternMatches(p, value) {
			return true
		}
	}
	return false
}

func patternMatches(pattern, value string) bool {
	if !strings.HasSuffix(pattern, "*") {
		return strings.EqualFold(pattern, value)
	}
	prefix := strings.TrimSuffix(pattern, "*")
	return len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix)
}
