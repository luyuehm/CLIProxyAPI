package contentfilter

import (
	"testing"
)

func TestPIIDetectors(t *testing.T) {
	detectors := CompileDetectors(AllPIITypes())
	if len(detectors) < 4 {
		t.Fatalf("expected at least 4 detectors, got %d", len(detectors))
	}

	// 1. Phone
	phoneText := "请联系我的电话 13812345678 或者 +86 13987654321 进行沟通"
	var phoneDet PIIDetector
	for _, d := range detectors {
		if d.Name() == PIITypePhone {
			phoneDet = d
		}
	}
	if phoneDet == nil {
		t.Fatal("phone detector not found")
	}
	redactedPhone, count := phoneDet.Redact(phoneText, "[PHONE]")
	if count < 2 {
		t.Errorf("expected at least 2 phone matches, got %d: %s", count, redactedPhone)
	}
	maskedPhone, _ := phoneDet.Mask(phoneText)
	if maskedPhone == phoneText || !containsSubstr(maskedPhone, "138****5678") {
		t.Errorf("unexpected masked phone result: %s", maskedPhone)
	}

	// 2. ID Card
	idText := "身份证号码为 110101199003072345 谢谢"
	var idDet PIIDetector
	for _, d := range detectors {
		if d.Name() == PIITypeIDCard {
			idDet = d
		}
	}
	if idDet == nil {
		t.Fatal("id card detector not found")
	}
	maskedID, count := idDet.Mask(idText)
	if count != 1 || !containsSubstr(maskedID, "110101********2345") {
		t.Errorf("unexpected id card mask result: %s (count: %d)", maskedID, count)
	}

	// 3. Email
	emailText := "我的邮箱是 support@example.com 和 test.user@company.cn"
	var emailDet PIIDetector
	for _, d := range detectors {
		if d.Name() == PIITypeEmail {
			emailDet = d
		}
	}
	if emailDet == nil {
		t.Fatal("email detector not found")
	}
	maskedEmail, count := emailDet.Mask(emailText)
	if count != 2 || !containsSubstr(maskedEmail, "@example.com") {
		t.Errorf("unexpected email mask result: %s (count: %d)", maskedEmail, count)
	}

	// 4. Bank Card
	bankText := "转账到银行卡 6222021234567890123 进行结算"
	var bankDet PIIDetector
	for _, d := range detectors {
		if d.Name() == PIITypeBankCard {
			bankDet = d
		}
	}
	if bankDet == nil {
		t.Fatal("bank card detector not found")
	}
	maskedBank, count := bankDet.Mask(bankText)
	if count != 1 || !containsSubstr(maskedBank, "622202") {
		t.Errorf("unexpected bank card mask result: %s (count: %d)", maskedBank, count)
	}

	// 5. Medical Record
	medText := "就诊卡号 MZ20230819001，请出示医保卡 YB123456789"
	var medDet PIIDetector
	for _, d := range detectors {
		if d.Name() == PIITypeMedicalRecord {
			medDet = d
		}
	}
	if medDet == nil {
		t.Fatal("medical record detector not found")
	}
	maskedMed, count := medDet.Mask(medText)
	if count < 2 {
		t.Errorf("expected 2 medical matches, got %d: %s", count, maskedMed)
	}
}

func TestSensitiveWordMatcher(t *testing.T) {
	words := []string{"商业机密", "绝密文件", "银行卡号"}
	matcher := BuildSensitiveWordMatcher(words, false)
	if matcher == nil {
		t.Fatal("failed to build sensitive word matcher")
	}

	text := "这是一份包含商业机密和绝密文件的报告"
	matches := matcher.FindAll(text)
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %v", matches)
	}

	masked, count := matcher.Mask(text)
	if count != 2 {
		t.Errorf("expected 2 mask counts, got %d", count)
	}
	if masked == text {
		t.Errorf("text was not masked")
	}
}

func TestFilterEngine(t *testing.T) {
	rules := []RuleConfig{
		{
			ID:             1,
			Name:           "金融与PII脱敏规则",
			Enabled:        true,
			Scenario:       ScenarioFinance,
			Action:         ActionMask,
			SensitiveWords: []string{"支付密码", "内幕交易"},
			PIITypes:       []string{PIITypePhone, PIITypeEmail, PIITypeBankCard},
			WhiteList:      []string{"allowed@example.com"},
			Models:         []string{"*"},
		},
		{
			ID:             2,
			Name:           "医疗合规阻断规则",
			Enabled:        true,
			Scenario:       ScenarioMedical,
			Action:         ActionBlock,
			SensitiveWords: []string{"精神分裂症病历"},
			PIITypes:       []string{PIITypeMedicalRecord},
			Models:         []string{"gpt-*"},
		},
	}

	engine := NewFilter(rules)

	// Test 1: Masking finance & PII
	text1 := "用户手机 13812345678, 邮箱 user@test.com, 请勿泄漏支付密码"
	res1 := engine.ProcessText(text1, ProcessOptions{Model: "claude-3-opus"})
	if res1.Blocked {
		t.Errorf("expected text1 not to be blocked")
	}
	if !res1.Changed {
		t.Errorf("expected text1 to be changed")
	}
	if res1.MatchCount < 3 {
		t.Errorf("expected at least 3 matches in text1, got %d", res1.MatchCount)
	}

	// Test 2: Blocking medical sensitive word on gpt model
	text2 := "患者包含精神分裂症病历需要审查"
	res2 := engine.ProcessText(text2, ProcessOptions{Model: "gpt-4o"})
	if !res2.Blocked {
		t.Errorf("expected text2 to be blocked")
	}
	if res2.Action != ActionBlock {
		t.Errorf("expected action block, got %s", res2.Action)
	}

	// Test 3: Model pattern filtering
	// rule 2 specifies gpt-*, should not apply to claude
	text3 := "患者包含精神分裂症病历需要审查"
	res3 := engine.ProcessText(text3, ProcessOptions{Model: "claude-3-5-sonnet"})
	if res3.Blocked {
		t.Errorf("expected text3 not to be blocked under claude model")
	}
}

func containsSubstr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(s) > 0 && len(sub) > 0 && indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
