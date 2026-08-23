package contentfilter

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// SensitiveWordMatcher matches configured sensitive words in text.
// Words are compiled longest-first so multi-word phrases win over substrings.
type SensitiveWordMatcher struct {
	regex     *regexp.Regexp
	whole     bool
	wordList  []string
	category  string
}

// BuildSensitiveWordMatcher compiles a case-insensitive sensitive word matcher.
func BuildSensitiveWordMatcher(words []string, wholeWord bool) *SensitiveWordMatcher {
	var valid []string
	for _, w := range words {
		w = strings.TrimSpace(w)
		if utf8.RuneCountInString(w) < 2 {
			continue
		}
		valid = append(valid, w)
	}
	if len(valid) == 0 {
		return nil
	}

	sort.Slice(valid, func(i, j int) bool {
		return len(valid[i]) > len(valid[j])
	})

	escaped := make([]string, len(valid))
	for i, w := range valid {
		escaped[i] = regexp.QuoteMeta(w)
	}

	pattern := "(?i)"
	if wholeWord {
		pattern += `(?:^|\W)` + strings.Join(escaped, `|`) + `(?:\W|$)`
	} else {
		pattern += strings.Join(escaped, "|")
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	return &SensitiveWordMatcher{
		regex:    re,
		whole:    wholeWord,
		wordList: append([]string(nil), valid...),
	}
}

// Matches returns true if text contains any sensitive word.
func (m *SensitiveWordMatcher) Matches(text string) bool {
	if m == nil || m.regex == nil {
		return false
	}
	return m.regex.MatchString(text)
}

// FindAll returns all matching sensitive words found in text.
func (m *SensitiveWordMatcher) FindAll(text string) []string {
	if m == nil || m.regex == nil {
		return nil
	}
	matches := m.regex.FindAllString(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	var result []string
	for _, match := range matches {
		trimmed := strings.TrimSpace(match)
		if trimmed != "" && !seen[strings.ToLower(trimmed)] {
			seen[strings.ToLower(trimmed)] = true
			result = append(result, trimmed)
		}
	}
	return result
}

// ReplaceAll replaces every sensitive word occurrence with replacement string.
func (m *SensitiveWordMatcher) ReplaceAll(text, replacement string) (string, int) {
	if m == nil || m.regex == nil {
		return text, 0
	}
	count := 0
	out := m.regex.ReplaceAllStringFunc(text, func(match string) string {
		count++
		return replacement
	})
	return out, count
}

// Mask replaces sensitive words keeping the first and last rune visible (or masked).
func (m *SensitiveWordMatcher) Mask(text string) (string, int) {
	if m == nil || m.regex == nil {
		return text, 0
	}
	count := 0
	out := m.regex.ReplaceAllStringFunc(text, func(match string) string {
		count++
		runes := []rune(match)
		n := len(runes)
		switch {
		case n <= 1:
			return "*"
		case n == 2:
			return string(runes[0]) + "*"
		default:
			return string(runes[0]) + strings.Repeat("*", n-2) + string(runes[n-1])
		}
	})
	return out, count
}

// Words returns a copy of configured words.
func (m *SensitiveWordMatcher) Words() []string {
	if m == nil {
		return nil
	}
	out := make([]string, len(m.wordList))
	copy(out, m.wordList)
	return out
}
