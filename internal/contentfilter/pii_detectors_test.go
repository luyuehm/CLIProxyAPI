package contentfilter

import (
	"strings"
	"testing"
)

func TestEmailDetector(t *testing.T) {
	d := emailDetector{}
	out, n := d.Redact("reach me at john.doe+spam@example.co.uk today", "[X]")
	if n != 1 {
		t.Fatalf("count=%d want 1", n)
	}
	if strings.Contains(out, "john.doe") || strings.Contains(out, "@example") {
		t.Fatalf("email not fully redacted: %s", out)
	}
}

func TestEmailDetectorNoMatch(t *testing.T) {
	d := emailDetector{}
	out, n := d.Redact("no email here", "[X]")
	if n != 0 {
		t.Fatalf("count=%d want 0", n)
	}
	if out != "no email here" {
		t.Fatalf("text changed: %s", out)
	}
}

func TestPhoneDetectorCNMobile(t *testing.T) {
	d := phoneDetector{}
	out, n := d.Redact("call 13912345678", "[X]")
	if n != 1 {
		t.Fatalf("count=%d want 1", n)
	}
	if strings.Contains(out, "13912345678") {
		t.Fatalf("phone not redacted: %s", out)
	}
}

func TestPhoneDetectorIntlFormat(t *testing.T) {
	d := phoneDetector{}
	out, n := d.Redact("call +1-415-5551234 today", "[X]")
	if n != 1 {
		t.Fatalf("count=%d want 1 (intl)", n)
	}
	if strings.Contains(out, "415-5551234") {
		t.Fatalf("intl phone not redacted: %s", out)
	}
}

func TestPhoneDetectorIgnoresEmbeddedDigits(t *testing.T) {
	d := phoneDetector{}
	// A 30-digit run should not be matched as a phone number.
	out, n := d.Redact("token=123456789012345678901234567890", "[X]")
	if n != 0 {
		t.Fatalf("expected 0 matches for embedded digit run, got %d (%s)", n, out)
	}
}

func TestIdCardDetector(t *testing.T) {
	d := idCardDetector{}
	out, n := d.Redact("my id 110101199003071234 end", "[X]")
	if n != 1 {
		t.Fatalf("count=%d want 1", n)
	}
	if strings.Contains(out, "110101199003071234") {
		t.Fatalf("id not redacted: %s", out)
	}
}

func TestIdCardDetectorX(t *testing.T) {
	d := idCardDetector{}
	out, n := d.Redact("id 11010119900307123X", "[X]")
	if n != 1 {
		t.Fatalf("count=%d want 1", n)
	}
	if strings.Contains(out, "11010119900307123") {
		t.Fatalf("id not redacted: %s", out)
	}
}

func TestBankCardDetector(t *testing.T) {
	d := bankCardDetector{}
	out, n := d.Redact("card 6222020200011111113", "[X]")
	if n != 1 {
		t.Fatalf("count=%d want 1", n)
	}
	if strings.Contains(out, "6222020200011111113") {
		t.Fatalf("bank card not redacted: %s", out)
	}
}

func TestBankCardDetectorIgnoresShortDigits(t *testing.T) {
	d := bankCardDetector{}
	out, n := d.Redact("count 1234567", "[X]")
	if n != 0 {
		t.Fatalf("expected 0 for short digits, got %d (%s)", n, out)
	}
}

func TestCompileDetectorsFiltersUnknown(t *testing.T) {
	d := compileDetectors([]string{"email", "bogus", "phone"})
	if len(d) != 2 {
		t.Fatalf("expected 2 detectors, got %d", len(d))
	}
}

func TestCompileDetectorsDedup(t *testing.T) {
	d := compileDetectors([]string{"email", "email", "email"})
	if len(d) != 1 {
		t.Fatalf("expected 1 detector after dedup, got %d", len(d))
	}
}

func TestAllPIITypesIsCopy(t *testing.T) {
	a := AllPIITypes()
	a[0] = "mutated"
	b := AllPIITypes()
	if b[0] == "mutated" {
		t.Fatal("AllPIITypes should return a copy")
	}
}

func TestBuildSensitiveWordMatcherEmpty(t *testing.T) {
	if m := BuildSensitiveWordMatcher(nil, false); m != nil {
		t.Fatal("nil words should give nil matcher")
	}
	if m := BuildSensitiveWordMatcher([]string{"a"}, false); m != nil {
		t.Fatal("single-rune words ignored, want nil")
	}
}

func TestBuildSensitiveWordMatcherMatches(t *testing.T) {
	m := BuildSensitiveWordMatcher([]string{"alpha", "beta"}, false)
	if m == nil {
		t.Fatal("expected matcher")
	}
	if !m.Matches("ALPHA here") {
		t.Fatal("expected case-insensitive match")
	}
	if !m.Matches("contains beta") {
		t.Fatal("expected match")
	}
	if m.Matches("gamma") {
		t.Fatal("unexpected match")
	}
	if c := m.Count("alpha beta alpha"); c != 3 {
		t.Fatalf("count=%d want 3", c)
	}
}

func TestBuildSensitiveWordMatcherReplaceAll(t *testing.T) {
	m := BuildSensitiveWordMatcher([]string{"secret"}, false)
	out, n := m.ReplaceAll("a secret secret thing", "[X]")
	if n != 2 {
		t.Fatalf("count=%d want 2", n)
	}
	if strings.Contains(out, "secret") {
		t.Fatalf("not replaced: %s", out)
	}
}

func TestMaskString(t *testing.T) {
	cases := map[string]string{
		"a":      "*",
		"ab":     "a*",
		"abc":    "a*c",
		"abcdef": "a****f",
		"中文测试":   "中**试",
	}
	for in, want := range cases {
		if got := maskString(in); got != want {
			t.Errorf("maskString(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatchesAnyPattern(t *testing.T) {
	if !matchesAnyPattern("gpt-4", []string{"gpt-*"}) {
		t.Error("gpt-4 should match gpt-*")
	}
	if matchesAnyPattern("claude-3", []string{"gpt-*"}) {
		t.Error("claude-3 should not match gpt-*")
	}
	if matchesAnyPattern("anything", []string{"gpt-*", "claude-*"}) {
		// No exact match, no wildcard match -> false
		t.Error("anything should not match gpt-*/claude-*")
	}
}

func TestMatchesAny(t *testing.T) {
	if !matchesAny("openai", []string{"OpenAI", "gemini"}) {
		t.Error("openai should match OpenAI")
	}
	if matchesAny("codex", []string{"openai", "gemini"}) {
		t.Error("codex should not match")
	}
}
