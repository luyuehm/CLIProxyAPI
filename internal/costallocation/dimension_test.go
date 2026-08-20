package costallocation

import (
	"net/http"
	"testing"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestDimensionResolution(t *testing.T) {
	cfg := AllocationConfig{
		Enabled:           true,
		DefaultDepartment: "General",
		DefaultTeam:       "Core",
		DefaultProject:    "Default",
		Rules: []AllocationRule{
			{
				Department:     "Engineering",
				Team:           "AI-Platform",
				Project:        "Copilot",
				APIKeyPrefixes: []string{"sk-eng-"},
			},
			{
				Department: "Marketing",
				Team:       "Growth",
				Project:    "SEO-Bot",
				APIKeys:    []string{"sk-mkt-123"},
			},
			{
				Department: "Finance",
				Team:       "Audit",
				Project:    "InvoiceAI",
				Tags: map[string]string{
					"cost_center": "fin-001",
				},
			},
		},
		TagKeys: []string{"X-Cost-Center"},
	}

	// 1. API key prefix match
	rec1 := coreusage.Record{
		APIKey:   "sk-eng-abc-xyz",
		Provider: "openai",
		Model:    "gpt-4o",
	}
	d1 := ResolveDimensions(cfg, rec1)
	if d1.Department != "Engineering" || d1.Team != "AI-Platform" || d1.Project != "Copilot" {
		t.Fatalf("expected Engineering/AI-Platform/Copilot, got %+v", d1)
	}

	// 2. Exact API key match
	rec2 := coreusage.Record{
		APIKey:   "sk-mkt-123",
		Provider: "anthropic",
		Model:    "claude-3-5-sonnet",
	}
	d2 := ResolveDimensions(cfg, rec2)
	if d2.Department != "Marketing" || d2.Team != "Growth" || d2.Project != "SEO-Bot" {
		t.Fatalf("expected Marketing/Growth/SEO-Bot, got %+v", d2)
	}

	// 3. Header tag match
	headers := http.Header{}
	headers.Set("X-Cost-Center", "fin-001")
	rec3 := coreusage.Record{
		APIKey:          "sk-random-999",
		Provider:        "openai",
		Model:           "gpt-4o-mini",
		ResponseHeaders: headers,
	}
	d3 := ResolveDimensions(cfg, rec3)
	if d3.Department != "Finance" || d3.Team != "Audit" || d3.Project != "InvoiceAI" {
		t.Fatalf("expected Finance/Audit/InvoiceAI, got %+v", d3)
	}

	// 4. Fallback default
	rec4 := coreusage.Record{
		APIKey:   "sk-unknown",
		Provider: "custom",
		Model:    "m1",
	}
	d4 := ResolveDimensions(cfg, rec4)
	if d4.Department != "General" || d4.Team != "Core" || d4.Project != "Default" {
		t.Fatalf("expected fallback default, got %+v", d4)
	}
}
