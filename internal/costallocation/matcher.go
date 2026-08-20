package costallocation

import (
	"strings"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// AllocateDepartment returns the department name for a usage record, based on
// the configured rules. The first matching rule wins. When no rule matches, the
// record falls to the default rule or the configured unallocated department name.
func AllocateDepartment(rules []DepartmentRule, unallocatedName string, record coreusage.Record) string {
	for _, rule := range rules {
		if rule.Default {
			continue // skip default rules in the main scan
		}
		if matchRule(rule, record) {
			return rule.Name
		}
	}
	// Fallback: first default rule wins.
	for _, rule := range rules {
		if rule.Default {
			return rule.Name
		}
	}
	return unallocatedName
}

// matchRule reports whether a usage record matches a department rule's criteria.
func matchRule(rule DepartmentRule, record coreusage.Record) bool {
	apiKey := strings.TrimSpace(record.APIKey)
	authID := strings.TrimSpace(record.AuthID)

	// Exact API key match
	for _, key := range rule.APIKeys {
		if strings.TrimSpace(key) != "" && apiKey == strings.TrimSpace(key) {
			return true
		}
	}

	// API key prefix match
	for _, prefix := range rule.APIKeyPrefixes {
		p := strings.TrimSpace(prefix)
		if p != "" && strings.HasPrefix(apiKey, p) {
			return true
		}
	}

	// API key suffix match
	for _, suffix := range rule.APIKeySuffixes {
		s := strings.TrimSpace(suffix)
		if s != "" && strings.HasSuffix(apiKey, s) {
			return true
		}
	}

	// Exact auth ID match
	for _, id := range rule.AuthIDs {
		if strings.TrimSpace(id) != "" && authID == strings.TrimSpace(id) {
			return true
		}
	}

	// Auth ID prefix match
	for _, prefix := range rule.AuthIDPrefixes {
		p := strings.TrimSpace(prefix)
		if p != "" && strings.HasPrefix(authID, p) {
			return true
		}
	}

	return false
}
