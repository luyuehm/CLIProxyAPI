package contentfilter

import (
	"strings"
)

const (
	ActionMask   = "mask"
	ActionRedact = "redact"
	ActionBlock  = "block"

	ScenarioGeneral = "general"
	ScenarioFinance = "finance"
	ScenarioMedical = "medical"
	ScenarioCustom  = "custom"
)

// Default preset sensitive words for compliance scenarios
var DefaultFinanceSensitiveWords = []string{
	"银行卡号", "信用卡CVV", "支付密码", "交易密码", "证券账号",
	"资金密码", "授信额度", "客户洗钱", "内幕交易", "转账限额",
	"账户余额", "私钥助记词", "钱包私钥",
}

var DefaultMedicalSensitiveWords = []string{
	"艾滋病确诊", "恶性肿瘤晚期", "精神分裂症病历", "传染病隔离",
	"阳性诊断书", "处方用药剂量", "家族遗传病史", "个人病史隐私",
	"乙肝大三阳", "流产记录", "基因检测缺陷",
}

var DefaultGeneralSensitiveWords = []string{
	"绝密文件", "商业机密", "内部机密", "root密码", "私钥证书",
}

// RuleConfig defines the runtime configuration for a filter rule.
type RuleConfig struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Enabled        bool     `json:"enabled"`
	Scenario       string   `json:"scenario"`
	Action         string   `json:"action"` // mask, redact, block
	SensitiveWords []string `json:"sensitive_words"`
	PIITypes       []string `json:"pii_types"`
	WhiteList      []string `json:"white_list"`
	Models         []string `json:"models"`
	Priority       int      `json:"priority"`
}

// ProcessOptions provides context for a filtering execution.
type ProcessOptions struct {
	Model    string
	ClientIP string
	UserID   string
}

// MatchedDetail describes a match occurrence.
type MatchedDetail struct {
	Type     string `json:"type"`     // sensitive_word, pii
	Category string `json:"category"` // detector name or "sensitive_word"
	Value    string `json:"value"`    // matched string
	Count    int    `json:"count"`
}

// ProcessResult is the outcome of filtering text against rules.
type ProcessResult struct {
	FilteredText string          `json:"filtered_text"`
	OriginalText string          `json:"original_text"`
	Changed      bool            `json:"changed"`
	Blocked      bool            `json:"blocked"`
	BlockReason  string          `json:"block_reason,omitempty"`
	Action       string          `json:"action"`
	MatchCount   int             `json:"match_count"`
	MatchedWords []string        `json:"matched_words"`
	MatchedPII   []string        `json:"matched_pii"`
	Details      []MatchedDetail `json:"details"`
	MatchedRules []string        `json:"matched_rules"`
}

// Filter is the content filtering engine.
type Filter struct {
	rules []compiledRule
}

type compiledRule struct {
	config    RuleConfig
	words     *SensitiveWordMatcher
	detectors []PIIDetector
	whitelist map[string]bool
}

// NewFilter builds a Filter engine from rule configurations.
func NewFilter(rules []RuleConfig) *Filter {
	f := &Filter{}
	for _, rc := range rules {
		if !rc.Enabled {
			continue
		}
		cr := compileRule(rc)
		f.rules = append(f.rules, cr)
	}
	return f
}

func compileRule(rc RuleConfig) compiledRule {
	cr := compiledRule{
		config:    rc,
		whitelist: make(map[string]bool, len(rc.WhiteList)),
	}

	for _, w := range rc.WhiteList {
		trimmed := strings.ToLower(strings.TrimSpace(w))
		if trimmed != "" {
			cr.whitelist[trimmed] = true
		}
	}

	if len(rc.SensitiveWords) > 0 {
		cr.words = BuildSensitiveWordMatcher(rc.SensitiveWords, false)
	}

	if len(rc.PIITypes) > 0 {
		cr.detectors = CompileDetectors(rc.PIITypes)
	}

	return cr
}

// ProcessText filters text through all matching active rules.
func (f *Filter) ProcessText(text string, opts ProcessOptions) *ProcessResult {
	res := &ProcessResult{
		FilteredText: text,
		OriginalText: text,
		Action:       ActionMask,
		MatchedWords: make([]string, 0),
		MatchedPII:   make([]string, 0),
		Details:      make([]MatchedDetail, 0),
		MatchedRules: make([]string, 0),
	}

	if strings.TrimSpace(text) == "" || len(f.rules) == 0 {
		return res
	}

	currentText := text

	for _, rule := range f.rules {
		if !modelMatches(opts.Model, rule.config.Models) {
			continue
		}

		ruleHit := false
		action := rule.config.Action
		if action == "" {
			action = ActionMask
		}

		// 1. Sensitive words detection
		if rule.words != nil {
			matched := rule.words.FindAll(currentText)
			// Filter out whitelisted words
			var nonWhitelisted []string
			for _, m := range matched {
				if !rule.whitelist[strings.ToLower(m)] {
					nonWhitelisted = append(nonWhitelisted, m)
				}
			}

			if len(nonWhitelisted) > 0 {
				ruleHit = true
				res.MatchedWords = append(res.MatchedWords, nonWhitelisted...)
				res.Details = append(res.Details, MatchedDetail{
					Type:     "sensitive_word",
					Category: rule.config.Scenario,
					Value:    strings.Join(nonWhitelisted, ", "),
					Count:    len(nonWhitelisted),
				})
				res.MatchCount += len(nonWhitelisted)

				if action == ActionBlock {
					res.Blocked = true
					res.BlockReason = "Blocked by sensitive word rule: " + rule.config.Name
					res.Action = ActionBlock
					res.MatchedRules = append(res.MatchedRules, rule.config.Name)
					return res
				} else if action == ActionRedact {
					currentText, _ = rule.words.ReplaceAll(currentText, "[SENSITIVE]")
				} else {
					currentText, _ = rule.words.Mask(currentText)
				}
			}
		}

		// 2. PII Detection & Redaction
		for _, det := range rule.detectors {
			var updated string
			var count int

			if action == ActionRedact {
				repl := "[REDACTED_" + strings.ToUpper(det.Name()) + "]"
				updated, count = det.Redact(currentText, repl)
			} else {
				updated, count = det.Mask(currentText)
			}

			if count > 0 && updated != currentText {
				ruleHit = true
				res.MatchedPII = append(res.MatchedPII, det.Name())
				res.Details = append(res.Details, MatchedDetail{
					Type:     "pii",
					Category: det.Name(),
					Value:    det.Name(),
					Count:    count,
				})
				res.MatchCount += count

				if action == ActionBlock {
					res.Blocked = true
					res.BlockReason = "Blocked by PII rule: " + rule.config.Name + " (" + det.Name() + ")"
					res.Action = ActionBlock
					res.MatchedRules = append(res.MatchedRules, rule.config.Name)
					return res
				}

				currentText = updated
			}
		}

		if ruleHit {
			res.MatchedRules = append(res.MatchedRules, rule.config.Name)
			res.Action = action
		}
	}

	res.FilteredText = currentText
	res.Changed = currentText != text
	return res
}

func modelMatches(model string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return true
	}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" || p == "*" {
			return true
		}
		if strings.HasSuffix(p, "*") {
			prefix := strings.TrimSuffix(p, "*")
			if strings.HasPrefix(strings.ToLower(model), strings.ToLower(prefix)) {
				return true
			}
		} else if strings.EqualFold(p, model) {
			return true
		}
	}
	return false
}
