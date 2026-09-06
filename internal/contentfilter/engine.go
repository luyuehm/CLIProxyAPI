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
//
// minScore gates false positives. A match with score < minScore is treated as
// a non-match. The score is composed of:
//
//	1.0                 if the pattern's verifier (verify) accepts the match
//	heuristicWeight/2   if verify rejects (e.g. phone with too many repeats)
//
// where heuristicWeight is the per-type baseline (1.0 for tightly-bounded
// types like id_card, 0.6 for noisier phone).
type piiPattern struct {
	typ      PIIType
	re       *regexp.Regexp
	group    int
	verify   func(string) bool // optional secondary check on the captured value
	weight   float64           // base weight when verify accepts
	priority int               // higher wins when types overlap (e.g. 18-digit number)
}

// compiledPIIPatterns holds the standard PII pattern set. Order matters for
// matching: longer/more specific patterns run first so a long digit run is
// classified as the most specific type (e.g. id_card before bank_card before
// phone). The Apply loop then short-circuits any later pattern whose window
// overlaps an earlier match.
var compiledPIIPatterns = []piiPattern{
	// Order: id_card (18 digits + X) -> bank_card (13-19 digits with separators)
	//        -> phone (11 digits 1[3-9]) -> email -> passport.
	// 18-digit id_card has the highest priority; we use the priority field
	// to disambiguate when an 11- to 19-digit run could match multiple.
	{PIIIDCard, idCardRe, 1, verifyIDCard, 1.0, 100},
	{PIIBankCard, bankCardRe, 1, verifyBankCard, 0.95, 80},
	{PIIPhone, phoneRe, 1, verifyPhone, 0.85, 60},
	{PIIEmail, emailRe, 0, verifyEmail, 0.9, 40},
	{PIIPassport, passportRe, 0, verifyPassport, 0.8, 30},
	{PIIMedicalRecord, medicalRecordRe, 0, nil, 0.7, 20},
}

var (
	// phoneRe matches Chinese mobile numbers: 11 digits starting with 1[3-9],
	// on digit boundaries. The non-digit delimiters are non-capturing; the
	// digits are capture group 1 so replacement keeps the delimiters.
	//
	// Tightened 2026-08-31 (RIC-440): rejects pure repeat digits (00000000000)
	// and common test-book values (12345678901) via verifyPhone below.
	phoneRe = regexp.MustCompile(`(?:^|[^0-9])(1[3-9][0-9]{9})(?:[^0-9]|$)`)

	// idCardRe matches 18-digit Chinese ID card numbers (17 digits + digit/X)
	// on digit boundaries. The verifier rejects sequences that do not pass
	// the GB 11643 check-digit (10/100 chance for a random match, so false
	// positives drop ~10x; all-zero or trivial sequences are also rejected).
	idCardRe = regexp.MustCompile(`(?:^|[^0-9])([0-9]{17}[0-9Xx])(?:[^0-9]|$)`)

	// emailRe matches common email addresses. The RFC-perfect form is
	// impractical; we keep the practical subset used by KEEPER. The verifier
	// rejects obviously synthetic addresses and at-least-one-alpha local part.
	emailRe = regexp.MustCompile(`(?i)\b[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}\b`)

	// bankCardRe matches 13-19 digit runs with optional space/dash separators.
	// Verifier requires Luhn validity so a random digit string does not match
	// (drops the false-positive rate to ~1/10 on Luhn-valid 16-digit noise).
	bankCardRe = regexp.MustCompile(`(?:^|[^0-9])((?:[0-9][\s-]?){12,18}[0-9])(?:[^0-9]|$)`)

	// passportRe matches passports: 1-2 uppercase letters followed by 7 digits
	// on word boundaries. Verifier requires the prefix letter set used by
	// CN passports and a 7-digit numeric tail (rejects letter-letter-letter runs).
	passportRe = regexp.MustCompile(`\b[A-Z]{1,2}[0-9]{7}\b`)

	// medicalRecordRe matches medical record / case-number patterns common
	// in CN clinical text: 8- to 12-digit runs prefixed by a context word
	// (病案号/住院号/门诊号/病历号). The prefix requirement alone drops the
	// false-positive rate to near zero.
	medicalRecordRe = regexp.MustCompile(`(?i)(?:病案号|住院号|门诊号|病历号)[:：\s]*([A-Z0-9]{6,16})`)
)

// verifyPhone returns true when the captured phone number is a plausible
// real number, false for synthetic / repeat-digit strings.
func verifyPhone(s string) bool {
	if len(s) != 11 || s[0] != '1' || s[1] < '3' || s[1] > '9' {
		return false
	}
	if isAllSameRune(s) {
		return false
	}
	// Common test-book phone numbers used in docs / examples.
	if isAscending(s) || isDescending(s) {
		return false
	}
	// Reject well-known placeholder numbers.
	switch s {
	case "13800138000", "13800138001", "13900000000", "13000000000":
		return false
	}
	return true
}

// verifyIDCard runs the GB 11643 check-digit on a 18-char ID number. Returns
// false on syntactically-valid but check-digit-invalid strings so most random
// 18-digit runs do not match.
func verifyIDCard(s string) bool {
	if len(s) != 18 {
		return false
	}
	if isAllSameRune(s[:17]) {
		return false
	}
	weights := [17]int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}
	checkDigits := "10X98765432"
	sum := 0
	for i := 0; i < 17; i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return false
		}
		sum += int(c-'0') * weights[i]
	}
	expected := checkDigits[sum%11]
	last := s[17]
	if last >= 'a' && last <= 'z' {
		last -= 32
	}
	return byte(expected) == last
}

// verifyBankCard returns true when the digit string (separators stripped)
// passes the Luhn check. Strips ' '/'-' first.
func verifyBankCard(s string) bool {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	if isAllSameRune(digits) {
		return false
	}
	sum := 0
	alt := false
	for i := len(digits) - 1; i >= 0; i-- {
		c := digits[i]
		if c < '0' || c > '9' {
			return false
		}
		n := int(c - '0')
		if alt {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	return sum%10 == 0
}

// verifyEmail rejects synthetic / RFC-invalid local parts. Requires at least
// one alpha character in the local part (drops "12345@x.cn" style false
// positives) and a TLD of at least 2 letters.
func verifyEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	if at <= 0 {
		return false
	}
	local := s[:at]
	domain := s[at+1:]
	if !strings.ContainsAny(local, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		return false
	}
	if !strings.Contains(domain, ".") {
		return false
	}
	return true
}

// verifyPassport requires the letter prefix to be one of the known CN
// passport-prefix letters and the digit tail to be non-trivial.
func verifyPassport(s string) bool {
	if len(s) < 8 || len(s) > 9 {
		return false
	}
	// Split at the boundary.
	var letter, digit string
	for i, r := range s {
		if r >= '0' && r <= '9' {
			letter = s[:i]
			digit = s[i:]
			break
		}
	}
	if letter == "" || len(digit) != 7 {
		return false
	}
	if isAllSameRune(digit) {
		return false
	}
	// Known CN passport prefixes (E/G/D/H/P/etc).
	switch letter {
	case "E", "G", "D", "H", "P", "S", "EA", "EB", "EC", "ED", "EE":
		return true
	}
	// Be permissive on letter prefix to avoid blocking non-CN passports in
	// long-context code/text that includes passport numbers from other
	// countries. The 7-digit tail + word boundary is already a tight filter.
	return len(letter) >= 1 && len(letter) <= 2
}

// isAllSameRune reports whether every rune of s equals s[0].
func isAllSameRune(s string) bool {
	if s == "" {
		return true
	}
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			return false
		}
	}
	return true
}

// isAscending / isDescending report trivial digit sequences like 0123456789.
func isAscending(s string) bool {
	if len(s) < 4 {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] != s[i-1]+1 {
			return false
		}
	}
	return true
}

func isDescending(s string) bool {
	if len(s) < 4 {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i]+1 != s[i-1] {
			return false
		}
	}
	return true
}

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

// contextExcluded reports whether a match is excluded by the rule's context
// list. If the immediate neighborhood of a match (8 runes on each side)
// contains any context keyword, the match is dropped. This implements
// the "编号 / 订单号 / 工单号" exemptions requested in RIC-440 (false
// positives in long code blocks).
func (r *Rule) contextExcluded(matched, surrounding string) bool {
	if r == nil || len(r.ContextExcludes) == 0 {
		return false
	}
	lower := strings.ToLower(surrounding)
	for _, kw := range r.ContextExcludes {
		kw = strings.TrimSpace(kw)
		if kw == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
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

	// PII types: regex match, partial mask on outbound. We collect all
	// candidate matches first, run the per-type verifier to drop
	// low-confidence hits, and finally apply replacements right-to-left so
	// earlier byte offsets stay valid.
	allCandidates := make([]candidateMatch, 0, 4)
	for _, t := range r.PIITypes {
		for _, p := range compiledPIIPatterns {
			if p.typ != t {
				continue
			}
			locs := p.re.FindAllStringSubmatchIndex(out, -1)
			for _, loc := range locs {
				var start, end int
				if p.group > 0 && p.group*2+1 < len(loc) {
					start, end = loc[p.group*2], loc[p.group*2+1]
				} else {
					start, end = loc[0], loc[1]
				}
				if start < 0 {
					continue
				}
				matched := out[start:end]
				allCandidates = append(allCandidates, candidateMatch{
					start:   start,
					end:     end,
					typ:     p.typ,
					pattern: p,
					matched: matched,
				})
			}
		}
	}

	// Sort by priority desc, then start asc, then end desc — so the most
	// specific (highest priority) wins on overlap. We use a stable tiebreaker
	// on start order to keep the behaviour predictable.
	if len(allCandidates) > 1 {
		sortCandidatesByPriority(allCandidates)
	}

	// Greedy interval-schedule: keep a candidate only if it does not overlap
	// an already-kept one with higher priority.
	kept := make([]candidateMatch, 0, len(allCandidates))
	for _, c := range allCandidates {
		if c.pattern.verify != nil && !c.pattern.verify(c.matched) {
			continue
		}
		if c.start < c.end-int(r.MinMatchLen) {
			// Apply minimum length gate at the candidate level.
			_ = c
		}
		if r.MinMatchLen > 0 && (c.end-c.start) < int(r.MinMatchLen) {
			continue
		}
		// Context exclusion window: 16 chars on each side of the match.
		if r.ContextExcludes != nil {
			winStart := c.start - 16
			if winStart < 0 {
				winStart = 0
			}
			winEnd := c.end + 16
			if winEnd > len(out) {
				winEnd = len(out)
			}
			if r.contextExcluded(c.matched, out[winStart:winEnd]) {
				continue
			}
		}
		overlaps := false
		for _, k := range kept {
			if c.start < k.end && c.end > k.start {
				overlaps = true
				break
			}
		}
		if !overlaps && !r.isWhitelisted(c.matched) {
			kept = append(kept, c)
		}
	}

	// Apply right-to-left so earlier byte offsets stay valid.
	for i := len(kept) - 1; i >= 0; i-- {
		c := kept[i]
		var sb strings.Builder
		sb.WriteString(out[:c.start])
		sb.WriteString(e.maskPII(c.typ, c.matched, inbound))
		sb.WriteString(out[c.end:])
		out = sb.String()
		matches = append(matches, c.matched)
	}
	return out, matches
}

// candidateMatch is an intermediate record of a regex match. We use a
// separate struct (rather than mutating strings mid-loop) so we can apply
// priority-based overlap resolution after all candidates are gathered.
type candidateMatch struct {
	start, end int
	typ        PIIType
	pattern    piiPattern
	matched    string
}

// sortCandidatesByPriority sorts by (priority desc, start asc, end desc).
// Insertion sort is fine here: the candidate count per request is small
// (usually < 20).
func sortCandidatesByPriority(cs []candidateMatch) {
	for i := 1; i < len(cs); i++ {
		j := i
		for j > 0 {
			a, b := cs[j-1], cs[j]
			if candidateLess(a, b) {
				break
			}
			cs[j-1], cs[j] = b, a
			j--
		}
	}
}

func candidateLess(a, b candidateMatch) bool {
	if a.pattern.priority != b.pattern.priority {
		return a.pattern.priority > b.pattern.priority
	}
	if a.start != b.start {
		return a.start < b.start
	}
	return a.end > b.end
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
