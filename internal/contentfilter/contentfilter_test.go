package contentfilter

import (
	"bytes"
	"strings"
	"testing"
)

// newTestRule builds a Rule with the given sensitive words and PII types.
func newTestRule(id int64, name string, words []string, pii []PIIType) *Rule {
	return &Rule{
		ID:             id,
		Name:           name,
		Enabled:        true,
		Scenario:       "general",
		Action:         ActionMask,
		SensitiveWords: words,
		PIITypes:       pii,
		Models:         nil, // applies to all models
	}
}

func TestParseCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"绝密文件", []string{"绝密文件"}},
		{"phone, id_card ,email", []string{"phone", "id_card", "email"}},
		{"a,,b, a", []string{"a", "b"}}, // dedup + trim
		{"绝密文件\n商业机密\n内部机密", []string{"绝密文件", "商业机密", "内部机密"}}, // newline sep (KEEPER prod)
	}
	for _, c := range cases {
		got := parseCSV(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("parseCSV(%q) = %v, want %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("parseCSV(%q) = %v, want %v", c.in, got, c.want)
			}
		}
	}
}

func TestParsePIITypes(t *testing.T) {
	got := parsePIITypes("phone,id_card,unknown_type,email")
	want := []PIIType{PIIPhone, PIIIDCard, PIIEmail}
	if len(got) != len(want) {
		t.Fatalf("parsePIITypes = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("parsePIITypes = %v, want %v", got, want)
		}
	}
}

func TestEngineSensitiveWordsInbound(t *testing.T) {
	e := NewEngine(true)
	rules := []*Rule{newTestRule(1, "r1", []string{"绝密文件", "商业机密"}, nil)}
	res := e.Apply(rules, "此文件为绝密文件，涉及商业机密。", true, "")
	if !res.Changed {
		t.Fatalf("expected changed result")
	}
	// 绝密文件 and 商业机密 are both 4 runes.
	want := "此文件为****，涉及****。"
	if res.Text != want {
		t.Fatalf("Apply = %q, want %q", res.Text, want)
	}
	if len(res.Matches) != 2 {
		t.Fatalf("Matches = %v, want 2", res.Matches)
	}
}

func TestEnginePhonePartialMaskOutbound(t *testing.T) {
	e := NewEngine(true)
	rules := []*Rule{newTestRule(1, "r1", nil, []PIIType{PIIPhone})}
	res := e.Apply(rules, "联系电话 13800138000 请查收", false, "")
	want := "联系电话 138****8000 请查收"
	if res.Text != want {
		t.Fatalf("Apply = %q, want %q", res.Text, want)
	}
	if len(res.Matches) != 1 {
		t.Fatalf("Matches = %v, want 1", res.Matches)
	}
}

func TestEnginePhoneFullMaskInbound(t *testing.T) {
	e := NewEngine(true)
	rules := []*Rule{newTestRule(1, "r1", nil, []PIIType{PIIPhone})}
	res := e.Apply(rules, "手机13800138000", true, "")
	want := "手机***********"
	if res.Text != want {
		t.Fatalf("Apply = %q, want %q", res.Text, want)
	}
}

func TestEngineIDCardPartialMaskOutbound(t *testing.T) {
	e := NewEngine(true)
	rules := []*Rule{newTestRule(1, "r1", nil, []PIIType{PIIIDCard})}
	res := e.Apply(rules, "身份证号 11010519491231002X", false, "")
	want := "身份证号 1****************X"
	if res.Text != want {
		t.Fatalf("Apply = %q, want %q", res.Text, want)
	}
}

func TestEngineEmailPartialMaskOutbound(t *testing.T) {
	e := NewEngine(true)
	rules := []*Rule{newTestRule(1, "r1", nil, []PIIType{PIIEmail})}
	res := e.Apply(rules, "联系 test@example.com 获取", false, "")
	want := "联系 t***@*******.com 获取"
	if res.Text != want {
		t.Fatalf("Apply = %q, want %q", res.Text, want)
	}
}

func TestEngineBankCardPartialMaskOutbound(t *testing.T) {
	e := NewEngine(true)
	rules := []*Rule{newTestRule(1, "r1", nil, []PIIType{PIIBankCard})}
	res := e.Apply(rules, "卡号 6222021234567890123 已绑定", false, "")
	// 19 digits -> keep 4 head (6222), 4 tail (0123)
	want := "卡号 6222***********0123 已绑定"
	if res.Text != want {
		t.Fatalf("Apply = %q, want %q", res.Text, want)
	}
}

func TestEnginePassportPartialMaskOutbound(t *testing.T) {
	e := NewEngine(true)
	rules := []*Rule{newTestRule(1, "r1", nil, []PIIType{PIIPassport})}
	res := e.Apply(rules, "护照 E1234567", false, "")
	want := "护照 E******7"
	if res.Text != want {
		t.Fatalf("Apply = %q, want %q", res.Text, want)
	}
}

func TestEngineWhitelist(t *testing.T) {
	e := NewEngine(true)
	r := newTestRule(1, "r1", nil, []PIIType{PIIPhone})
	r.WhiteList = []string{"13800138000"}
	rules := []*Rule{r}
	res := e.Apply(rules, "白名单 13800138000 保留", false, "")
	if res.Changed {
		t.Fatalf("expected no change, got %q", res.Text)
	}
	if res.Text != "白名单 13800138000 保留" {
		t.Fatalf("Apply = %q, want unchanged", res.Text)
	}
}

func TestEngineModelScoping(t *testing.T) {
	e := NewEngine(true)
	r := newTestRule(1, "r1", []string{"绝密文件"}, nil)
	r.Models = []string{"claude-sonnet-4"}
	rules := []*Rule{r}

	// Different model: no change.
	res := e.Apply(rules, "绝密文件", true, "gpt-5")
	if res.Changed {
		t.Fatalf("expected no change for other model, got %q", res.Text)
	}
	// Matching model: masked.
	res = e.Apply(rules, "绝密文件", true, "claude-sonnet-4")
	if !res.Changed {
		t.Fatalf("expected change for matching model")
	}
	// Empty model (all rules apply).
	res = e.Apply(rules, "绝密文件", true, "")
	if !res.Changed {
		t.Fatalf("expected change for empty model")
	}
}

func TestEngineDisabledRule(t *testing.T) {
	e := NewEngine(true)
	r := newTestRule(1, "r1", []string{"绝密文件"}, nil)
	r.Enabled = false
	res := e.Apply([]*Rule{r}, "绝密文件", true, "")
	if res.Changed {
		t.Fatalf("disabled rule must not apply")
	}
}

func TestFilterStream(t *testing.T) {
	e := NewEngine(true)
	rules := []*Rule{newTestRule(1, "r1", nil, []PIIType{PIIPhone})}
	// SSE with one event carrying a phone number, split across chunks.
	// Simulate the stream arriving in awkward chunks to prove the buffer
	// holds multi-line events until their blank line.
	chunks := []string{
		"data: {\"ph",
		"one\":\"1380013800",
		"0\"}\n\nda",
		"ta: [DONE]\n\n",
	}
	var out bytes.Buffer
	sm := &streamMasker{
		engine: e,
		rules:  rules,
		model:  "",
		out:    &out,
	}
	for _, c := range chunks {
		if _, err := sm.Write([]byte(c)); err != nil {
			t.Fatalf("Write(%q): %v", c, err)
		}
	}
	if err := sm.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := "data: {\"phone\":\"138****8000\"}\n\ndata: [DONE]\n\n"
	if out.String() != want {
		t.Fatalf("streamed = %q, want %q", out.String(), want)
	}
}

func TestFilterStreamSensitiveWord(t *testing.T) {
	e := NewEngine(true)
	rules := []*Rule{newTestRule(1, "r1", []string{"绝密文件"}, nil)}
	var out bytes.Buffer
	sm := &streamMasker{engine: e, rules: rules, out: &out}
	if _, err := sm.Write([]byte("data: 绝密文件内容\n\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sm.Close()
	want := "data: ****内容\n\n"
	if out.String() != want {
		t.Fatalf("streamed = %q, want %q", out.String(), want)
	}
}

func TestEngineNoRules(t *testing.T) {
	e := NewEngine(true)
	res := e.Apply(nil, "绝密文件 13800138000", true, "")
	if res.Changed {
		t.Fatalf("no rules should not change text")
	}
	if res.Text != "绝密文件 13800138000" {
		t.Fatalf("Apply = %q", res.Text)
	}
}

func TestEngineEmptyText(t *testing.T) {
	e := NewEngine(true)
	rules := []*Rule{newTestRule(1, "r1", []string{"绝密文件"}, nil)}
	res := e.Apply(rules, "", true, "")
	if res.Changed {
		t.Fatalf("empty text should not change")
	}
}

// TestRuleIsWhitelisted exercises the whitelist helper directly.
func TestRuleIsWhitelisted(t *testing.T) {
	r := newTestRule(1, "r1", nil, nil)
	r.WhiteList = []string{"admin@example.com", "127.0.0.1"}
	if !r.isWhitelisted("admin@example.com") {
		t.Fatalf("expected whitelisted")
	}
	if !r.isWhitelisted(" 127.0.0.1 ") {
		t.Fatalf("expected whitelisted (trimmed)")
	}
	if r.isWhitelisted("other@example.com") {
		t.Fatalf("unexpected whitelist match")
	}
}

// TestAppliesToModel exercises model scoping on the Rule itself.
func TestAppliesToModel(t *testing.T) {
	r := newTestRule(1, "r1", nil, nil)
	r.Models = []string{"*"}
	if !r.appliesToModel("anything") {
		t.Fatalf("wildcard model should apply")
	}
	r2 := newTestRule(2, "r2", nil, nil)
	r2.Models = []string{"claude-sonnet-4"}
	if r2.appliesToModel("gpt-5") {
		t.Fatalf("different model should not apply")
	}
	if !r2.appliesToModel("CLAUDE-SONNET-4") {
		t.Fatalf("case-insensitive model match expected")
	}
}

// TestMaskHelper sanity-checks the partial masker helper.
func TestMaskHelper(t *testing.T) {
	e := NewEngine(true)
	got := e.maskPII(PIIPhone, "13800138000", false)
	if got != "138****8000" {
		t.Fatalf("maskPII = %q, want 138****8000", got)
	}
	gotFull := e.maskPII(PIIPhone, "13800138000", true)
	if !strings.Contains(gotFull, "***") {
		t.Fatalf("inbound mask should contain asterisks: %q", gotFull)
	}
}
