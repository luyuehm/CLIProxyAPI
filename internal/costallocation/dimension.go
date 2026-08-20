package costallocation

import (
	"strings"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// DimensionScope represents resolved metadata dimensions for an allocated request.
type DimensionScope struct {
	Department string            `json:"department"`
	Team       string            `json:"team"`
	Project    string            `json:"project"`
	APIKey     string            `json:"api_key"`
	Provider   string            `json:"provider"`
	Model      string            `json:"model"`
	Tags       map[string]string `json:"tags,omitempty"`
}

func normalizeTagKey(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	k = strings.TrimPrefix(k, "x-")
	k = strings.ReplaceAll(k, "_", "-")
	return k
}

// ResolveDimensions maps a usage record to its department, team, project, and tags.
func ResolveDimensions(cfg AllocationConfig, record coreusage.Record) DimensionScope {
	apiKey := strings.TrimSpace(record.APIKey)
	provider := strings.TrimSpace(record.Provider)
	model := strings.TrimSpace(record.Model)
	if provider == "" {
		provider = "unknown"
	}
	if model == "" {
		model = "unknown"
	}

	tags := make(map[string]string)

	// Extract tags from configured TagKeys in response headers or standard tag headers
	if record.ResponseHeaders != nil {
		for _, key := range cfg.TagKeys {
			val := record.ResponseHeaders.Get(key)
			if val != "" {
				tags[normalizeTagKey(key)] = strings.TrimSpace(val)
			}
		}
		// Also inspect standard headers if present
		for _, stdKey := range []string{"X-Department", "X-Team", "X-Project", "X-Cost-Center", "X-Tag"} {
			val := record.ResponseHeaders.Get(stdKey)
			if val != "" {
				tags[normalizeTagKey(stdKey)] = strings.TrimSpace(val)
			}
		}
	}

	// Default fallback values
	dept := strings.TrimSpace(cfg.DefaultDepartment)
	if dept == "" {
		dept = "unallocated"
	}
	team := strings.TrimSpace(cfg.DefaultTeam)
	if team == "" {
		team = "default"
	}
	proj := strings.TrimSpace(cfg.DefaultProject)
	if proj == "" {
		proj = "default"
	}

	// If explicit headers provided department/team/project, they can seed or override
	if hDept := tags["department"]; hDept != "" {
		dept = hDept
	}
	if hTeam := tags["team"]; hTeam != "" {
		team = hTeam
	}
	if hProj := tags["project"]; hProj != "" {
		proj = hProj
	}

	// Match configured AllocationRules
	for _, rule := range cfg.Rules {
		matched := matchRule(rule, record, tags)
		if matched {
			if strings.TrimSpace(rule.Department) != "" {
				dept = strings.TrimSpace(rule.Department)
			}
			if strings.TrimSpace(rule.Team) != "" {
				team = strings.TrimSpace(rule.Team)
			}
			if strings.TrimSpace(rule.Project) != "" {
				proj = strings.TrimSpace(rule.Project)
			}
			break
		}
	}

	return DimensionScope{
		Department: dept,
		Team:       team,
		Project:    proj,
		APIKey:     apiKey,
		Provider:   provider,
		Model:      model,
		Tags:       tags,
	}
}

func matchRule(rule AllocationRule, record coreusage.Record, tags map[string]string) bool {
	apiKey := strings.TrimSpace(record.APIKey)

	// Check exact API key matches
	if len(rule.APIKeys) > 0 {
		for _, k := range rule.APIKeys {
			if strings.TrimSpace(k) == apiKey {
				return true
			}
		}
	}

	// Check API key prefix matches
	if len(rule.APIKeyPrefixes) > 0 && apiKey != "" {
		for _, prefix := range rule.APIKeyPrefixes {
			if strings.HasPrefix(apiKey, strings.TrimSpace(prefix)) {
				return true
			}
		}
	}

	// Check tag matches
	if len(rule.Tags) > 0 {
		allMatch := true
		for k, expectedVal := range rule.Tags {
			actualVal, exists := tags[normalizeTagKey(k)]
			if !exists || !strings.EqualFold(actualVal, expectedVal) {
				allMatch = false
				break
			}
		}
		if allMatch {
			return true
		}
	}

	// Check header matches
	if len(rule.Headers) > 0 && record.ResponseHeaders != nil {
		allMatch := true
		for k, expectedVal := range rule.Headers {
			actualVal := record.ResponseHeaders.Get(k)
			if !strings.EqualFold(actualVal, expectedVal) {
				allMatch = false
				break
			}
		}
		if allMatch {
			return true
		}
	}

	return false
}
