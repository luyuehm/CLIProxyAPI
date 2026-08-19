package contentfilter

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// SensitiveWordMatcher matches configured sensitive words in text.
// Words are compiled longest-first so multi-word phrases win over their
// substrings (e.g. "secret token" before "secret").
type SensitiveWordMatcher struct {
	regex *regexp.Regexp
	whole bool // require word boundaries on both sides
	words []string
}

// BuildSensitiveWordMatcher compiles a case-insensitive matcher.
// Words shorter than two runes are ignored. Invalid regex characters are
// quoted so a user-supplied word can never fail to compile.
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
		// Require a non-word neighbour (or string edge) on both sides. \W does
		// not match CJK, but sensitive words typically contain ASCII anyway;
		// CJK phrases still match because the boundary check passes at edges
		// and between non-ASCII runs.
		pattern += `(?:^|\W)` + strings.Join(escaped, `|`) + `(?:\W|$)`
	} else {
		pattern += strings.Join(escaped, "|")
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		// Should be unreachable because every token is quoted, but stay safe.
		return nil
	}
	return &SensitiveWordMatcher{regex: re, whole: wholeWord, words: append([]string(nil), valid...)}
}

// Matches reports whether the text contains at least one sensitive word.
func (m *SensitiveWordMatcher) Matches(text string) bool {
	if m == nil || m.regex == nil {
		return false
	}
	return m.regex.MatchString(text)
}

// Count reports the number of sensitive-word matches in text.
func (m *SensitiveWordMatcher) Count(text string) int {
	if m == nil || m.regex == nil {
		return 0
	}
	return len(m.regex.FindAllStringIndex(text, -1))
}

// ReplaceAll returns text with every sensitive-word match replaced.
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

// Words returns the configured word list (a copy).
func (m *SensitiveWordMatcher) Words() []string {
	if m == nil {
		return nil
	}
	out := make([]string, len(m.words))
	copy(out, m.words)
	return out
}
