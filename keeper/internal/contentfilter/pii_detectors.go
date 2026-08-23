package contentfilter

import (
	"regexp"
	"strings"
)

// PIIDetector defines the interface for detecting and redacting specific PII types.
type PIIDetector interface {
	Name() string
	Redact(text, replacement string) (string, int)
	Mask(text string) (string, int)
}

const (
	PIITypePhone         = "phone"
	PIITypeIDCard        = "id_card"
	PIITypeEmail         = "email"
	PIITypeBankCard      = "bank_card"
	PIITypeMedicalRecord = "medical_record"
	PIITypePassport      = "passport"
)

// AllPIITypes returns all supported PII types.
func AllPIITypes() []string {
	return []string{
		PIITypePhone,
		PIITypeIDCard,
		PIITypeEmail,
		PIITypeBankCard,
		PIITypeMedicalRecord,
		PIITypePassport,
	}
}

// CompileDetectors instantiates detector instances for the given PII type names.
func CompileDetectors(types []string) []PIIDetector {
	if len(types) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(types))
	var detectors []PIIDetector
	for _, t := range types {
		normalized := strings.ToLower(strings.TrimSpace(t))
		normalized = strings.ReplaceAll(normalized, "-", "_")
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		switch normalized {
		case PIITypePhone:
			detectors = append(detectors, phoneDetector{})
		case PIITypeIDCard:
			detectors = append(detectors, idCardDetector{})
		case PIITypeEmail:
			detectors = append(detectors, emailDetector{})
		case PIITypeBankCard:
			detectors = append(detectors, bankCardDetector{})
		case PIITypeMedicalRecord:
			detectors = append(detectors, medicalRecordDetector{})
		case PIITypePassport:
			detectors = append(detectors, passportDetector{})
		}
	}
	return detectors
}

// --- Email Detector ---

type emailDetector struct{}

func (emailDetector) Name() string { return PIITypeEmail }

var emailRegex = regexp.MustCompile(`(?i)[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}`)

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

func (d emailDetector) Mask(text string) (string, int) {
	if !strings.ContainsRune(text, '@') {
		return text, 0
	}
	count := 0
	out := emailRegex.ReplaceAllStringFunc(text, func(match string) string {
		count++
		parts := strings.SplitN(match, "@", 2)
		if len(parts) != 2 {
			return maskGeneric(match)
		}
		user, domain := parts[0], parts[1]
		userRunes := []rune(user)
		var maskedUser string
		if len(userRunes) <= 2 {
			maskedUser = string(userRunes[0]) + "*"
		} else {
			maskedUser = string(userRunes[0]) + strings.Repeat("*", len(userRunes)-2) + string(userRunes[len(userRunes)-1])
		}
		return maskedUser + "@" + domain
	})
	return out, count
}

// --- Phone Detector (CN Mobile + International) ---

type phoneDetector struct{}

func (phoneDetector) Name() string { return PIITypePhone }

var (
	cnMobileRegex  = regexp.MustCompile(`(?:\+?86[-\s]?)?1[3-9]\d{9}`)
	intlPhoneRegex = regexp.MustCompile(`\+\d{1,3}(?:[\s-]?\d){6,12}`)
)

func (d phoneDetector) Redact(text, replacement string) (string, int) {
	count := 0
	redact := func(re *regexp.Regexp) {
		out := re.ReplaceAllStringFunc(text, func(match string) string {
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

func (d phoneDetector) Mask(text string) (string, int) {
	count := 0
	maskFunc := func(re *regexp.Regexp) {
		out := re.ReplaceAllStringFunc(text, func(match string) string {
			if isEmbeddedInDigits(text, match) {
				return match
			}
			count++
			raw := strings.TrimPrefix(strings.TrimPrefix(match, "+86"), "86")
			raw = strings.TrimLeft(raw, " -")
			if len(raw) == 11 && strings.HasPrefix(raw, "1") {
				return raw[:3] + "****" + raw[7:]
			}
			return maskGeneric(match)
		})
		text = out
	}
	maskFunc(cnMobileRegex)
	maskFunc(intlPhoneRegex)
	return text, count
}

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

// --- ID Card Detector (Chinese 18-digit Resident ID) ---

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

func (d idCardDetector) Mask(text string) (string, int) {
	count := 0
	out := idCardRegex.ReplaceAllStringFunc(text, func(match string) string {
		count++
		if len(match) == 18 {
			return match[:6] + "********" + match[14:]
		}
		return maskGeneric(match)
	})
	return out, count
}

// --- Bank Card Detector (13 to 19 digits) ---

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

func (d bankCardDetector) Mask(text string) (string, int) {
	count := 0
	out := bankCardRegex.ReplaceAllStringFunc(text, func(match string) string {
		count++
		if len(match) >= 12 {
			return match[:6] + strings.Repeat("*", len(match)-10) + match[len(match)-4:]
		}
		return maskGeneric(match)
	})
	return out, count
}

// --- Medical Record / Prescription / Health Card ID Detector ---

type medicalRecordDetector struct{}

func (medicalRecordDetector) Name() string { return PIITypeMedicalRecord }

var (
	medicalRecordRegex    = regexp.MustCompile(`(?i)\b(?:MZ|ZY|BL|MED|CF|HIS|EMR)[-_]?[0-9]{6,14}\b`)
	medicalInsuranceRegex = regexp.MustCompile(`(?i)\b(?:YB|SI)[-_]?[0-9A-Z]{9,16}\b`)
)

func (d medicalRecordDetector) Redact(text, replacement string) (string, int) {
	count := 0
	redact := func(re *regexp.Regexp) {
		out := re.ReplaceAllStringFunc(text, func(match string) string {
			count++
			return replacement
		})
		text = out
	}
	redact(medicalRecordRegex)
	redact(medicalInsuranceRegex)
	return text, count
}

func (d medicalRecordDetector) Mask(text string) (string, int) {
	count := 0
	maskFunc := func(re *regexp.Regexp) {
		out := re.ReplaceAllStringFunc(text, func(match string) string {
			count++
			if len(match) > 6 {
				return match[:3] + strings.Repeat("*", len(match)-5) + match[len(match)-2:]
			}
			return maskGeneric(match)
		})
		text = out
	}
	maskFunc(medicalRecordRegex)
	maskFunc(medicalInsuranceRegex)
	return text, count
}

// --- Passport Detector ---

type passportDetector struct{}

func (passportDetector) Name() string { return PIITypePassport }

var passportRegex = regexp.MustCompile(`(?i)\b[EG]\d{8}\b|\b[SE]\d{7,8}\b|\bPASSPORT[-_]?[0-9A-Z]{6,10}\b`)

func (d passportDetector) Redact(text, replacement string) (string, int) {
	count := 0
	out := passportRegex.ReplaceAllStringFunc(text, func(match string) string {
		count++
		return replacement
	})
	return out, count
}

func (d passportDetector) Mask(text string) (string, int) {
	count := 0
	out := passportRegex.ReplaceAllStringFunc(text, func(match string) string {
		count++
		if len(match) > 4 {
			return match[:2] + strings.Repeat("*", len(match)-4) + match[len(match)-2:]
		}
		return maskGeneric(match)
	})
	return out, count
}

// Helper generic masker
func maskGeneric(s string) string {
	runes := []rune(s)
	n := len(runes)
	switch {
	case n <= 1:
		return "*"
	case n == 2:
		return string(runes[0]) + "*"
	case n <= 4:
		return string(runes[0]) + strings.Repeat("*", n-2) + string(runes[n-1])
	default:
		prefixLen := n / 4
		if prefixLen < 1 {
			prefixLen = 1
		}
		suffixLen := prefixLen
		return string(runes[:prefixLen]) + strings.Repeat("*", n-prefixLen-suffixLen) + string(runes[n-suffixLen:])
	}
}
