package contentfilter

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func cfgOf(vals ...func(*config.ContentFilterConfig)) config.ContentFilterConfig {
	var c config.ContentFilterConfig
	for _, fn := range vals {
		fn(&c)
	}
	return c
}

func withWords(words ...string) func(*config.ContentFilterConfig) {
	return func(c *config.ContentFilterConfig) { c.SensitiveWords = words }
}
func withEnabled() func(*config.ContentFilterConfig) {
	return func(c *config.ContentFilterConfig) { c.Enabled = true }
}
func withAction(a string) func(*config.ContentFilterConfig) {
	return func(c *config.ContentFilterConfig) { c.Action = a }
}
func withPII(types ...string) func(*config.ContentFilterConfig) {
	return func(c *config.ContentFilterConfig) { c.PIITypes = types }
}
func withPlaceholder(p string) func(*config.ContentFilterConfig) {
	return func(c *config.ContentFilterConfig) { c.Placeholder = p }
}
func withWholeWord() func(*config.ContentFilterConfig) {
	return func(c *config.ContentFilterConfig) { c.WholeWord = true }
}
func withModels(patterns ...string) func(*config.ContentFilterConfig) {
	return func(c *config.ContentFilterConfig) { c.Models = patterns }
}
func withProtocols(p ...string) func(*config.ContentFilterConfig) {
	return func(c *config.ContentFilterConfig) { c.Protocols = p }
}
func withMin(n int) func(*config.ContentFilterConfig) {
	return func(c *config.ContentFilterConfig) { c.MinRedactionsToBlock = n }
}

func TestNewDisabledReturnsNil(t *testing.T) {
	if f := New(config.ContentFilterConfig{}); f != nil {
		t.Fatalf("expected nil for disabled config, got %v", f)
	}
	if f := New(cfgOf(withEnabled())); f != nil {
		t.Fatalf("enabled but no words/pii should be nil, got %v", f)
	}
}

func TestNewDefaultsActionAndPlaceholder(t *testing.T) {
	f := New(cfgOf(withEnabled(), withWords("secret")))
	if f == nil {
		t.Fatal("expected non-nil filter")
	}
	if f.cfg.Action != ActionRedact {
		t.Fatalf("default action = %q, want %q", f.cfg.Action, ActionRedact)
	}
	if f.placeholder != "[REDACTED]" {
		t.Fatalf("default placeholder = %q, want %q", f.placeholder, "[REDACTED]")
	}
	if f.cfg.MinRedactionsToBlock != 1 {
		t.Fatalf("default min = %d, want 1", f.cfg.MinRedactionsToBlock)
	}
}

func TestApplyRequestRedactsSensitiveWordInMessages(t *testing.T) {
	f := New(cfgOf(withEnabled(), withWords("secret")))
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"this is a secret plan"}]}`)
	out, dec := f.ApplyRequest(body, "gpt-4", "openai")
	if dec.Redactions != 1 {
		t.Fatalf("redactions = %d, want 1", dec.Redactions)
	}
	if strings.Contains(string(out), "secret") {
		t.Fatalf("body still contains 'secret': %s", out)
	}
	if !dec.Changed {
		t.Fatal("expected Changed=true")
	}
}

func TestApplyRequestHandlesContentBlocks(t *testing.T) {
	f := New(cfgOf(withEnabled(), withWords("forbidden")))
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"forbidden word"}]}]}`)
	out, dec := f.ApplyRequest(body, "m", "openai")
	if dec.Redactions != 1 {
		t.Fatalf("redactions=%d want 1", dec.Redactions)
	}
	if strings.Contains(string(out), "forbidden") {
		t.Fatalf("still contains forbidden: %s", out)
	}
}

func TestApplyRequestClaudeSystemField(t *testing.T) {
	f := New(cfgOf(withEnabled(), withWords("cloaked")))
	body := []byte(`{"system":"you are cloaked","messages":[{"role":"user","content":"hi"}]}`)
	out, dec := f.ApplyRequest(body, "claude-3", "claude")
	if dec.Redactions != 1 {
		t.Fatalf("redactions=%d want 1", dec.Redactions)
	}
	if strings.Contains(string(out), "cloaked") {
		t.Fatalf("still contains cloaked: %s", out)
	}
}

func TestApplyRequestClaudeSystemArray(t *testing.T) {
	f := New(cfgOf(withEnabled(), withWords("secret")))
	body := []byte(`{"system":[{"type":"text","text":"top secret"}],"messages":[]}`)
	out, dec := f.ApplyRequest(body, "claude-3", "claude")
	if dec.Redactions != 1 {
		t.Fatalf("redactions=%d want 1", dec.Redactions)
	}
	if strings.Contains(string(out), "secret") {
		t.Fatalf("still contains secret: %s", out)
	}
}

func TestApplyRequestGeminiContents(t *testing.T) {
	f := New(cfgOf(withEnabled(), withWords("hidden")))
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"this is hidden"}]}]}`)
	out, dec := f.ApplyRequest(body, "gemini-pro", "gemini")
	if dec.Redactions != 1 {
		t.Fatalf("redactions=%d want 1", dec.Redactions)
	}
	if strings.Contains(string(out), "hidden") {
		t.Fatalf("still contains hidden: %s", out)
	}
}

func TestApplyRequestGeminiSystemInstruction(t *testing.T) {
	f := New(cfgOf(withEnabled(), withWords("secret")))
	body := []byte(`{"systemInstruction":{"parts":[{"text":"secret prompt"}]},"contents":[]}`)
	out, dec := f.ApplyRequest(body, "gemini-pro", "gemini")
	if dec.Redactions != 1 {
		t.Fatalf("redactions=%d want 1", dec.Redactions)
	}
	if strings.Contains(string(out), "secret") {
		t.Fatalf("still contains secret: %s", out)
	}
}

func TestApplyRequestResponsesInputArray(t *testing.T) {
	f := New(cfgOf(withEnabled(), withWords("secret")))
	body := []byte(`{"model":"gpt-4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"secret here"}]}]}`)
	out, dec := f.ApplyRequest(body, "gpt-4", "responses")
	if dec.Redactions != 1 {
		t.Fatalf("redactions=%d want 1", dec.Redactions)
	}
	if strings.Contains(string(out), "secret") {
		t.Fatalf("still contains secret: %s", out)
	}
}

func TestApplyRequestCustomPlaceholder(t *testing.T) {
	f := New(cfgOf(withEnabled(), withWords("secret"), withPlaceholder("[BLOCKED]")))
	body := []byte(`{"messages":[{"role":"user","content":"secret"}]}`)
	out, _ := f.ApplyRequest(body, "m", "openai")
	if !strings.Contains(string(out), "[BLOCKED]") {
		t.Fatalf("expected custom placeholder, got %s", out)
	}
}

func TestApplyRequestMaskAction(t *testing.T) {
	f := New(cfgOf(withEnabled(), withWords("confidential"), withAction(ActionMask)))
	body := []byte(`{"messages":[{"role":"user","content":"confidential report"}]}`)
	out, dec := f.ApplyRequest(body, "m", "openai")
	if dec.Redactions != 1 {
		t.Fatalf("redactions=%d want 1", dec.Redactions)
	}
	if strings.Contains(string(out), "confidential") {
		t.Fatalf("still contains original word: %s", out)
	}
	if !strings.Contains(string(out), "c") || !strings.Contains(string(out), "l") {
		t.Fatalf("expected first/last rune preserved: %s", out)
	}
	if !strings.Contains(string(out), "*") {
		t.Fatalf("expected mask stars: %s", out)
	}
}

func TestApplyRequestBlockActionRejects(t *testing.T) {
	f := New(cfgOf(withEnabled(), withWords("banned"), withAction(ActionBlock)))
	body := []byte(`{"messages":[{"role":"user","content":"banned topic"}]}`)
	_, dec := f.ApplyRequest(body, "gpt-4", "openai")
	if !dec.Blocked {
		t.Fatal("expected Blocked=true")
	}
	if !strings.Contains(dec.Reason, "1 match") {
		t.Fatalf("unexpected reason: %s", dec.Reason)
	}
}

func TestApplyRequestBlockBelowThreshold(t *testing.T) {
	f := New(cfgOf(withEnabled(), withWords("banned"), withAction(ActionBlock), withMin(2)))
	body := []byte(`{"messages":[{"role":"user","content":"banned once"}]}`)
	_, dec := f.ApplyRequest(body, "gpt-4", "openai")
	if dec.Blocked {
		t.Fatal("should not block below threshold")
	}
	if dec.Redactions != 1 {
		t.Fatalf("redactions=%d want 1", dec.Redactions)
	}
}

func TestApplyRequestProtocolScope(t *testing.T) {
	f := New(cfgOf(withEnabled(), withWords("secret"), withProtocols("claude")))
	body := []byte(`{"messages":[{"role":"user","content":"secret"}]}`)
	out, dec := f.ApplyRequest(body, "gpt-4", "openai")
	if dec.Changed {
		t.Fatal("should not apply to non-matching protocol")
	}
	if string(out) != string(body) {
		t.Fatal("body should be unchanged for non-matching protocol")
	}
	out2, dec2 := f.ApplyRequest(body, "claude-3", "claude")
	if !dec2.Changed {
		t.Fatal("should apply to matching protocol")
	}
	if strings.Contains(string(out2), "secret") {
		t.Fatal("should have redacted")
	}
}

func TestApplyRequestModelScope(t *testing.T) {
	f := New(cfgOf(withEnabled(), withWords("secret"), withModels("gpt-*")))
	body := []byte(`{"model":"claude-3","messages":[{"role":"user","content":"secret"}]}`)
	out, dec := f.ApplyRequest(body, "claude-3", "openai")
	if dec.Changed {
		t.Fatal("should not apply to non-matching model")
	}
	out2, dec2 := f.ApplyRequest([]byte(`{"model":"gpt-4","messages":[{"role":"user","content":"secret"}]}`), "gpt-4", "openai")
	if !dec2.Changed {
		t.Fatal("should apply to matching model")
	}
	if strings.Contains(string(out2), "secret") {
		t.Fatalf("should have redacted: %s", out2)
	}
	_ = out
}

func TestApplyRequestNonChatPayloadUntouched(t *testing.T) {
	f := New(cfgOf(withEnabled(), withWords("secret")))
	body := []byte(`{"prompt":"secret","max_tokens":10}`) // completions, no messages array
	out, dec := f.ApplyRequest(body, "gpt-4", "openai")
	if dec.Changed {
		t.Fatal("non-chat payload should not be modified")
	}
	if string(out) != string(body) {
		t.Fatal("non-chat payload body should be identical")
	}
}

func TestApplyRequestInvalidJSON(t *testing.T) {
	f := New(cfgOf(withEnabled(), withWords("secret")))
	body := []byte(`{not json`)
	out, dec := f.ApplyRequest(body, "gpt-4", "openai")
	if dec.Changed || dec.Redactions != 0 {
		t.Fatal("invalid json should be a no-op")
	}
	if string(out) != string(body) {
		t.Fatal("invalid json body should be unchanged")
	}
}

func TestApplyRequestNilFilterIsNoOp(t *testing.T) {
	var f *Filter
	body := []byte(`{"messages":[{"role":"user","content":"secret"}]}`)
	out, dec := f.ApplyRequest(body, "gpt-4", "openai")
	if dec.Redactions != 0 || dec.Changed {
		t.Fatal("nil filter should be a no-op")
	}
	if string(out) != string(body) {
		t.Fatal("nil filter should not change body")
	}
}

func TestApplyRequestPIIEmail(t *testing.T) {
	f := New(cfgOf(withEnabled(), withPII("email")))
	body := []byte(`{"messages":[{"role":"user","content":"contact me at john.doe@example.com please"}]}`)
	out, dec := f.ApplyRequest(body, "gpt-4", "openai")
	if dec.Redactions != 1 {
		t.Fatalf("redactions=%d want 1", dec.Redactions)
	}
	if strings.Contains(string(out), "john.doe@example.com") {
		t.Fatalf("email not redacted: %s", out)
	}
	if !strings.Contains(string(out), "[REDACTED]") {
		t.Fatalf("expected placeholder: %s", out)
	}
}

func TestApplyRequestPIIPhone(t *testing.T) {
	f := New(cfgOf(withEnabled(), withPII("phone")))
	body := []byte(`{"messages":[{"role":"user","content":"call 13812345678 now"}]}`)
	out, dec := f.ApplyRequest(body, "gpt-4", "openai")
	if dec.Redactions == 0 {
		t.Fatalf("expected phone redaction, got %s", out)
	}
	if strings.Contains(string(out), "13812345678") {
		t.Fatalf("phone not redacted: %s", out)
	}
}

func TestApplyRequestPIIIdCard(t *testing.T) {
	f := New(cfgOf(withEnabled(), withPII("id-card")))
	body := []byte(`{"messages":[{"role":"user","content":"id is 110101199003071234"}]}`)
	out, dec := f.ApplyRequest(body, "gpt-4", "openai")
	if dec.Redactions != 1 {
		t.Fatalf("redactions=%d want 1", dec.Redactions)
	}
	if strings.Contains(string(out), "110101199003071234") {
		t.Fatalf("id card not redacted: %s", out)
	}
}

func TestApplyRequestPIIBankCard(t *testing.T) {
	f := New(cfgOf(withEnabled(), withPII("bank-card")))
	body := []byte(`{"messages":[{"role":"user","content":"card 6222020200011111113"}]}`)
	out, dec := f.ApplyRequest(body, "gpt-4", "openai")
	if dec.Redactions != 1 {
		t.Fatalf("redactions=%d want 1", dec.Redactions)
	}
	if strings.Contains(string(out), "6222020200011111113") {
		t.Fatalf("bank card not redacted: %s", out)
	}
}

func TestApplyRequestPIIAllKeyword(t *testing.T) {
	f := New(cfgOf(withEnabled(), withPII("all")))
	if len(f.detectors) != len(allPIITypes) {
		t.Fatalf("detectors=%d want %d", len(f.detectors), len(allPIITypes))
	}
}

func TestApplyRequestCombinesWordsAndPII(t *testing.T) {
	f := New(cfgOf(withEnabled(), withWords("secret"), withPII("email")))
	body := []byte(`{"messages":[{"role":"user","content":"secret email: jane@acme.com"}]}`)
	out, dec := f.ApplyRequest(body, "gpt-4", "openai")
	if dec.Redactions < 2 {
		t.Fatalf("expected >=2 redactions, got %d", dec.Redactions)
	}
	if strings.Contains(string(out), "secret") || strings.Contains(string(out), "jane@acme.com") {
		t.Fatalf("not fully redacted: %s", out)
	}
}

func TestApplyRequestLongerWordWinsOverSubstring(t *testing.T) {
	f := New(cfgOf(withEnabled(), withWords("secret", "secret sauce")))
	body := []byte(`{"messages":[{"role":"user","content":"this is a secret sauce recipe"}]}`)
	out, dec := f.ApplyRequest(body, "gpt-4", "openai")
	if dec.Redactions != 1 {
		t.Fatalf("expected 1 match (longer phrase), got %d", dec.Redactions)
	}
	if strings.Contains(string(out), "secret") {
		t.Fatalf("longer phrase not redacted: %s", out)
	}
}

func TestApplyRequestCaseInsensitive(t *testing.T) {
	f := New(cfgOf(withEnabled(), withWords("Secret")))
	body := []byte(`{"messages":[{"role":"user","content":"a SECRET and a secret"}]}`)
	out, dec := f.ApplyRequest(body, "gpt-4", "openai")
	if dec.Redactions != 2 {
		t.Fatalf("expected 2 case-insensitive matches, got %d", dec.Redactions)
	}
	if strings.Contains(strings.ToLower(string(out)), "secret") {
		t.Fatalf("not all redacted: %s", out)
	}
}
