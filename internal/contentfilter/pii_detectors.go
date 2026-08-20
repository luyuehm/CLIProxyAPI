package contentfilter

import (
	"regexp"
	"strings"
)

// PIIDetector identifies and redacts a single category of personally
// identifiable information from a text fragment.
type PIIDetector interface {
	// Name returns the stable identifier for this detector (e.g. "email").
	Name() string
	// Redact returns text with every match replaced by replacement and the
	// number of replacements performed.
	Redact(text, replacement string) (string, int)
}

// compileDetectors builds the requested PII detectors. Unknown categories are
// ignored so a typo in configuration cannot disable the whole filter.
func compileDetectors(types []string) []PIIDetector {
	if len(types) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(types))
	var out []PIIDetector
	for _, t := range types {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		switch t {
		case PIITypeEmail:
			out = append(out, emailDetector{})
		case PIITypePhone:
			out = append(out, phoneDetector{})
		case PIITypeIDCard:
			out = append(out, idCardDetector{})
		case PIITypeBankCard:
			out = append(out, bankCardDetector{})
		}
	}
	return out
}

// PII category constants exposed for configuration validation and logging.
const (
	PIITypeEmail    = "email"
	PIITypePhone    = "phone"
	PIITypeIDCard   = "id-card"
	PIITypeBankCard = "bank-card"
)

// allPIITypes lists every supported PII category.
var allPIITypes = []string{PIITypeEmail, PIITypePhone, PIITypeIDCard, PIITypeBankCard}

// AllPIITypes returns a copy of the supported PII category list.
func AllPIITypes() []string {
	out := make([]string, len(allPIITypes))
	copy(out, allPIITypes)
	return out
}

// emailDetector matches RFC-ish email addresses.
type emailDetector struct{}

func (emailDetector) Name() string { return PIITypeEmail }

var emailRegex = regexp.MustCompile(`(?i)[A-Z0-9._%+\-]{1,64}@[A-Z0-9.\-]{1,253}\.[A-Z]{2,63}`)

func (d emailDetector) Redact(text, replacement string) (string, int) {
	if !strings.ContainsRune(text, '@') {
		return text, 0
	}
	count := 0
	out := emailRegex.ReplaceAllStringFunc(text, func(match string) string {
		count++
		return replacement
	})
	return out, count
}

// phoneDetector matches Chinese mobile numbers and E.164 international numbers.
// It intentionally matches digit runs in isolation (surrounded by non-digit
// boundaries) to avoid mangling numeric fields such as token counts.
type phoneDetector struct{}

func (phoneDetector) Name() string { return PIITypePhone }

var (
	// cnMobileRegex matches a Chinese mobile number with an optional +86/86 prefix.
	cnMobileRegex = regexp.MustCompile(`(?:\+?86[-\s]?)?1[3-9]\d{9}`)
	// intlPhoneRegex matches an international number written with a leading "+",
	// an optional 1-3 digit country code, and a subscriber number of 6-12 digits
	// that may use spaces or dashes as separators (e.g. "+1-415-555-1234").
	intlPhoneRegex = regexp.MustCompile(`\+\d{1,3}(?:[\s-]?\d){6,12}`)
)

func (d phoneDetector) Redact(text, replacement string) (string, int) {
	count := 0
	redact := func(re *regexp.Regexp) {
		out := re.ReplaceAllStringFunc(text, func(match string) string {
			// Guard against redacting substrings of longer digit runs; phone
			// numbers must be bounded by non-digit neighbours.
			if isEmbeddedInDigits(text, match) {
				return match
			}
			count++
			return replacement
		})
		text = out
	}
	redact(cnMobileRegex)
	redact(intlPhoneRegex)
	return text, count
}

// isEmbeddedInDigits reports whether the match sits inside a longer run of
// digits, in which case it is almost certainly not a phone number.
func isEmbeddedInDigits(text, match string) bool {
	idx := strings.Index(text, match)
	if idx < 0 {
		return false
	}
	if idx > 0 && isASCIIDigit(text[idx-1]) {
		return true
	}
	end := idx + len(match)
	if end < len(text) && isASCIIDigit(text[end]) {
		return true
	}
	return false
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

// idCardDetector matches Chinese resident identity card numbers (18 digits,
// last digit may be X). A checksum is not enforced so truncated copies are still
// caught; the digit-length guard limits false positives.
type idCardDetector struct{}

func (idCardDetector) Name() string { return PIITypeIDCard }

var idCardRegex = regexp.MustCompile(`(?i)\b\d{17}[\dx]\b`)

func (d idCardDetector) Redact(text, replacement string) (string, int) {
	count := 0
	out := idCardRegex.ReplaceAllStringFunc(text, func(match string) string {
		count++
		return replacement
	})
	return out, count
}

// bankCardDetector matches 13- to 19-digit bank card numbers isolated by word
// boundaries. Continuous digit runs shorter than 13 digits are left alone.
type bankCardDetector struct{}

func (bankCardDetector) Name() string { return PIITypeBankCard }

var bankCardRegex = regexp.MustCompile(`\b\d{13,19}\b`)

func (d bankCardDetector) Redact(text, replacement string) (string, int) {
	count := 0
	out := bankCardRegex.ReplaceAllStringFunc(text, func(match string) string {
		count++
		return replacement
	})
	return out, count
}
