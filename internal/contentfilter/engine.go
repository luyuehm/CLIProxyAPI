package contentfilter

import (
	"regexp"
	"strings"
)

// maskRune is the masking character used for both sensitive words (full mask)
// and PII masking. KEEPER uses the same "*" convention.
const maskRune = '*'

// partialMasker keeps a fixed prefix/suffix of a PII match visible and masks
// the middle. For emails the "visible" local part is the first rune.
type partialMasker struct {
	keepHead int
	keepTail int
}

// outboundPartialMaskers defines how each PII type is partially masked in
// outbound responses (keep prefix/suffix for readability). Inbound requests
// always mask PII fully so a secret travelling into the gateway is never
// echoed upstream.
var outboundPartialMaskers = map[PIIType]partialMasker{
	PIIPhone:    {keepHead: 3, keepTail: 4}, // 138****1234
	PIIIDCard:   {keepHead: 1, keepTail: 1}, // 1***********2
	PIIEmail:    {keepHead: 1, keepTail: 1}, // u***@***.com
	PIIBankCard: {keepHead: 4, keepTail: 4}, // 6222****1234
	PIIPassport: {keepHead: 1, keepTail: 1}, // E****123
}

// piiPattern describes one PII type's compiled regex. The regex may wrap the
// actual value in capturing groups to enforce digit boundaries (RE2 supports
// neither lookahead nor lookbehind). group is the submatch index holding the
// value to mask (0 = whole match, 1 = first capture group).
type piiPattern struct {
	typ   PIIType
	re    *regexp.Regexp
	group int
}

// compiledPIIPatterns holds the standard PII pattern set, ordered so that
// longer/more specific patterns run first (ID card before bank card before
// phone) so a long digit run is classified as the most specific type.
var compiledPIIPatterns = []piiPattern{
	{PIIIDCard, idCardRe, 1},
	{PIIBankCard, bankCardRe, 1},
	{PIIPhone, phoneRe, 1},
	{PIIEmail, emailRe, 0},
	{PIIPassport, passportRe, 0},
}

var (
	// phoneRe matches Chinese mobile numbers: 11 digits starting with 1[3-9],
	// on digit boundaries. The non-digit delimiters are non-capturing; the
	// digits are capture group 1 so replacement keeps the delimiters.
	phoneRe = regexp.MustCompile(`(?:^|[^0-9])(1[3-9][0-9]{9})(?:[^0-9]|$)`)

	// idCardRe matches 18-digit Chinese ID card numbers (17 digits + digit/X)
	// on digit boundaries.
	idCardRe = regexp.MustCompile(`(?:^|[^0-9])([0-9]{17}[0-9Xx])(?:[^0-9]|$)`)

	// emailRe matches common email addresses. Self-delimiting, no boundaries
	// needed.
	emailRe = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)

	// bankCardRe matches bank card numbers: 13-19 digits with optional
	// space/dash separators between groups, on digit boundaries.
	bankCardRe = regexp.MustCompile(`(?:^|[^0-9])((?:[0-9][\s-]?){12,18}[0-9])(?:[^0-9]|$)`)

	// passportRe matches passports: 1-2 uppercase letters followed by 7 digits
	// on word boundaries (RE2 supports \b; only lookarounds are unsupported).
	passportRe = regexp.MustCompile(`\b[A-Z]{1,2}[0-9]{7}\b`)
)

// Engine applies content filter rules to text. It is safe for concurrent use
// once created; the rule set is swapped atomically by the syncer.
type Engine struct {
	outboundPartial bool
}

// NewEngine builds an engine. When outboundPartial is true, outbound PII
// masking keeps a prefix/suffix for readability (e.g. 138****1234); otherwise
// outbound PII is fully masked like inbound.
func NewEngine(outboundPartial bool) *Engine {
	return &Engine{outboundPartial: outboundPartial}
}

// runeLen returns the number of runes in s.
func runeLen(s string) int {
	return len([]rune(s))
}

// fullMask returns a run of maskRune the same length (in runes) as s.
func fullMask(s string) string {
	return strings.Repeat(string(maskRune), runeLen(s))
}

// maskPII masks a single PII match. Inbound masks fully; outbound uses the
// partial masker (keep prefix/suffix) when configured. Emails keep their
// @-structure visible (u***@***.com) so the address stays recognizable.
func (e *Engine) maskPII(typ PIIType, matched string, inbound bool) string {
	if inbound {
		return fullMask(matched)
	}
	if typ == PIIEmail {
		return maskEmail(matched)
	}
	var pm partialMasker
	if p, ok := outboundPartialMaskers[typ]; ok {
		pm = p
	} else {
		pm = partialMasker{keepHead: 1, keepTail: 1}
	}
	runes := []rune(matched)
	ln := len(runes)
	if ln <= pm.keepHead+pm.keepTail {
		return fullMask(matched)
	}
	head := string(runes[:pm.keepHead])
	tail := string(runes[ln-pm.keepTail:])
	middle := strings.Repeat(string(maskRune), ln-pm.keepHead-pm.keepTail)
	return head + middle + tail
}

// maskEmail keeps the first rune of the local part and the top-level domain
// visible, masking the rest. e.g. "test@example.com" -> "t***@*******.com".
func maskEmail(matched string) string {
	at := strings.IndexByte(matched, '@')
	if at < 0 {
		return fullMask(matched)
	}
	local := matched[:at]
	domain := matched[at+1:]

	maskedLocal := string([]rune(local)[:minInt(1, len([]rune(local)))])
	if runeLen(local) > 1 {
		maskedLocal += strings.Repeat(string(maskRune), runeLen(local)-1)
	}

	labels := strings.Split(domain, ".")
	for i := 0; i < len(labels)-1; i++ {
		if labels[i] != "" {
			labels[i] = fullMask(labels[i])
		}
	}
	return maskedLocal + "@" + strings.Join(labels, ".")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Result carries the outcome of applying rules to a piece of text. The
// RuleHits slice lists per-rule matches (rule id, name, matched values) so
// callers can write one audit row per hit.
type Result struct {
	Changed  bool
	Text     string
	Matches  []string
	RuleHits []RuleHit
}

// RuleHit is one rule's matches. It is the basis for audit rows.
type RuleHit struct {
	RuleID   int64
	RuleName string
	Matches  []string
}

// ruleApplicable reports whether a rule applies to the given upstream model.
func ruleApplicable(r *Rule, model string) bool {
	if r == nil || !r.enabled() {
		return false
	}
	if !r.appliesToModel(model) {
		return false
	}
	return len(r.SensitiveWords) > 0 || len(r.PIITypes) > 0
}

// applyRule masks one rule's sensitive words and PII types over text.
// It returns the masked text and any matched values. Matches equal to an
// entry in the rule's whitelist are left untouched.
func (e *Engine) applyRule(r *Rule, text string, inbound bool) (string, []string) {
	var matches []string
	out := text

	// Sensitive words: exact match, rune-length mask.
	for _, w := range r.SensitiveWords {
		w = strings.TrimSpace(w)
		if w == "" || r.isWhitelisted(w) || !strings.Contains(out, w) {
			continue
		}
		out = strings.ReplaceAll(out, w, fullMask(w))
		matches = append(matches, w)
	}

	// PII types: regex match, partial mask on outbound.
	for _, t := range r.PIITypes {
		for _, p := range compiledPIIPatterns {
			if p.typ != t {
				continue
			}
			locs := p.re.FindAllStringSubmatchIndex(out, -1)
			if len(locs) == 0 {
				continue
			}
			// Walk matches from the end so earlier offsets stay valid. When
			// the pattern wraps the value in a capture group, only that group
			// is replaced so boundary delimiters are preserved.
			var sb strings.Builder
			last := 0
			for _, loc := range locs {
				var start, end int
				if p.group > 0 && p.group*2+1 < len(loc) {
					start, end = loc[p.group*2], loc[p.group*2+1]
				} else {
					start, end = loc[0], loc[1]
				}
				if start < 0 || end <= last {
					continue
				}
				matched := out[start:end]
				if r.isWhitelisted(matched) {
					continue
				}
				sb.WriteString(out[last:start])
				sb.WriteString(e.maskPII(t, matched, inbound))
				matches = append(matches, matched)
				last = end
			}
			sb.WriteString(out[last:])
			out = sb.String()
		}
	}
	return out, matches
}

// Apply runs all enabled rules (scoped to the given upstream model) over text.
// inbound controls masking style: inbound fully masks sensitive words and PII;
// outbound fully masks sensitive words and partially masks PII.
func (e *Engine) Apply(rules []*Rule, text string, inbound bool, model string) Result {
	if len(rules) == 0 || text == "" {
		return Result{Text: text}
	}

	out := text
	var matches []string
	var hits []RuleHit
	changed := false

	for _, r := range rules {
		if !ruleApplicable(r, model) {
			continue
		}
		filtered, m := e.applyRule(r, out, inbound)
		if len(m) > 0 {
			changed = true
			matches = append(matches, m...)
			hits = append(hits, RuleHit{RuleID: r.ID, RuleName: r.Name, Matches: m})
			out = filtered
		}
	}

	return Result{Changed: changed, Text: out, Matches: matches, RuleHits: hits}
}
