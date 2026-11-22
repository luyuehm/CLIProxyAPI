package util

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestClaudeMCPCapabilityManifest_DeclaredParallelPasses(t *testing.T) {
	body := []byte(`{"parallel_tool_calls":true,"tools":[{"type":"mcp","mcp":{"server_url":"https://example.com/mcp","name":"calc","inputSchema":{"type":"object"},"capabilities":{"parallel_tool_use":true,"strict_tool_use":true}}}]}`)
	out, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP != nil {
		t.Fatalf("unexpected error: %v", errMCP)
	}
	if got := gjson.GetBytes(out, "parallel_tool_calls").Bool(); !got {
		t.Fatalf("parallel_tool_calls = false, want true; body=%s", out)
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "calc" {
		t.Fatalf("tools[0].name = %q, want calc", got)
	}
	if gjson.GetBytes(out, "tools.0.mcp").Exists() {
		t.Fatalf("mcp envelope still present: %s", out)
	}
}

func TestClaudeMCPCapabilityManifest_MissingParallelFailsClosed(t *testing.T) {
	body := []byte(`{"parallel_tool_calls":true,"tools":[{"type":"mcp","mcp":{"server_url":"https://example.com/mcp","name":"calc","inputSchema":{"type":"object"}}}]}`)
	_, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP == nil {
		t.Fatal("expected missing capability rejection")
	}
	if errMCP.Code != MCPCapabilityErrorNotDeclared {
		t.Fatalf("code = %q, want %q", errMCP.Code, MCPCapabilityErrorNotDeclared)
	}
	if errMCP.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", errMCP.Status, http.StatusUnprocessableEntity)
	}
	if errMCP.Capability != "parallel_tool_use" {
		t.Fatalf("capability = %q, want parallel_tool_use", errMCP.Capability)
	}
	if !strings.Contains(errMCP.Message, "degrade to serial") {
		t.Fatalf("message = %q, want serial degradation hint", errMCP.Message)
	}
	if !strings.Contains(errMCP.Field, "tools[0]") {
		t.Fatalf("field = %q, want tools[0]", errMCP.Field)
	}
}

func TestClaudeMCPCapabilityManifest_ExplicitFalseFailsClosed(t *testing.T) {
	body := []byte(`{"parallel_tool_calls":true,"tools":[{"type":"mcp","mcp":{"server_url":"https://example.com/mcp","name":"calc","inputSchema":{"type":"object"},"capabilities":{"parallel_tool_use":false}}}]}`)
	_, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP == nil || errMCP.Code != MCPCapabilityErrorNotDeclared {
		t.Fatalf("expected not-declared rejection, got %v", errMCP)
	}
}

func TestClaudeMCPCapabilityManifest_InvalidBooleanType(t *testing.T) {
	body := []byte(`{"parallel_tool_calls":true,"tools":[{"type":"mcp","mcp":{"server_url":"https://example.com/mcp","name":"calc","inputSchema":{"type":"object"},"capabilities":{"parallel_tool_use":"yes"}}}]}`)
	_, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP == nil {
		t.Fatal("expected invalid manifest rejection")
	}
	if errMCP.Code != MCPCapabilityErrorManifestInvalid {
		t.Fatalf("code = %q, want %q", errMCP.Code, MCPCapabilityErrorManifestInvalid)
	}
	if errMCP.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", errMCP.Status, http.StatusUnprocessableEntity)
	}
	if !strings.Contains(errMCP.Message, "boolean") {
		t.Fatalf("message = %q, want boolean repair hint", errMCP.Message)
	}
}

func TestClaudeMCPCapabilityManifest_InvalidManifestRejectedWithoutRequest(t *testing.T) {
	body := []byte(`{"tools":[{"type":"mcp","mcp":{"server_url":"https://example.com/mcp","name":"calc","inputSchema":{"type":"object"},"capabilities":{"parallel_tool_use":["yes"]}}}]}`)
	_, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP == nil || errMCP.Code != MCPCapabilityErrorManifestInvalid {
		t.Fatalf("expected invalid manifest rejection, got %v", errMCP)
	}
}

func TestClaudeMCPCapabilityManifest_NonObjectManifestRejected(t *testing.T) {
	body := []byte(`{"tools":[{"type":"mcp","mcp":{"server_url":"https://example.com/mcp","name":"calc","inputSchema":{"type":"object"},"capabilities":true}}]}`)
	_, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP == nil || errMCP.Code != MCPCapabilityErrorManifestInvalid {
		t.Fatalf("expected invalid manifest rejection, got %v", errMCP)
	}
}

func TestClaudeMCPCapabilityManifest_AutoDegradeFallsBackExplicitly(t *testing.T) {
	body := []byte(`{"parallel_tool_calls":true,"mcp_capability_auto_degrade":true,"tools":[{"type":"mcp","mcp":{"server_url":"https://example.com/mcp","name":"calc","inputSchema":{"type":"object"}}}]}`)
	out, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP != nil {
		t.Fatalf("unexpected error: %v", errMCP)
	}
	if got := gjson.GetBytes(out, "parallel_tool_calls"); got.Exists() && got.Bool() {
		t.Fatalf("parallel_tool_calls = true, want explicit false after degradation; body=%s", out)
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "calc" {
		t.Fatalf("tools[0].name = %q, want calc", got)
	}
}

func TestClaudeMCPCapabilityManifest_AutoDegradeHandlesMultipleMissingCapabilities(t *testing.T) {
	body := []byte(`{"parallel_tool_calls":true,"strict_tool_use":true,"mcp_capability_auto_degrade":true,"tools":[{"type":"mcp","mcp":{"server_url":"https://example.com/mcp","name":"calc","inputSchema":{"type":"object"}}}]}`)
	out, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP != nil {
		t.Fatalf("unexpected error: %v", errMCP)
	}
	if got := gjson.GetBytes(out, "parallel_tool_calls"); got.Exists() && got.Bool() {
		t.Fatalf("parallel_tool_calls = true after degradation; body=%s", out)
	}
	if got := gjson.GetBytes(out, "strict_tool_use"); got.Exists() && got.Bool() {
		t.Fatalf("strict_tool_use = true after degradation; body=%s", out)
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "calc" {
		t.Fatalf("tools[0].name = %q, want calc", got)
	}
}

func TestClaudeMCPCapabilityManifest_StrictMissingFailsClosed(t *testing.T) {
	body := []byte(`{"strict_tool_use":true,"tools":[{"type":"mcp","mcp":{"server_url":"https://example.com/mcp","name":"calc","inputSchema":{"type":"object"}}}]}`)
	_, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP == nil || errMCP.Code != MCPCapabilityErrorNotDeclared {
		t.Fatalf("expected strict capability rejection, got %v", errMCP)
	}
	if errMCP.Capability != "strict_tool_use" {
		t.Fatalf("capability = %q, want strict_tool_use", errMCP.Capability)
	}
}

func TestClaudeMCPCapabilityManifest_RequiredListCountsAsSupport(t *testing.T) {
	body := []byte(`{"parallel_tool_calls":true,"tools":[{"type":"mcp","mcp":{"server_url":"https://example.com/mcp","name":"calc","inputSchema":{"type":"object"},"capabilities":{"required":["parallel_tool_use"]}}}]}`)
	if _, errMCP := RewriteClaudeMCPToolDefinitions(body); errMCP != nil {
		t.Fatalf("required capability should pass, got %v", errMCP)
	}
}

func TestClaudeMCPCapabilityManifest_ToolChoiceParallelSignalFailsClosed(t *testing.T) {
	body := []byte(`{"tool_choice":{"type":"auto","disable_parallel_tool_use":false},"tools":[{"type":"mcp","mcp":{"server_url":"https://example.com/mcp","name":"calc","inputSchema":{"type":"object"}}}]}`)
	_, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP == nil || errMCP.Code != MCPCapabilityErrorNotDeclared {
		t.Fatalf("expected parallel capability rejection, got %v", errMCP)
	}
	if errMCP.Capability != "parallel_tool_use" {
		t.Fatalf("capability = %q, want parallel_tool_use", errMCP.Capability)
	}
}

func TestClaudeMCPCapabilityManifest_ToolChoiceStrictSignalFailsClosed(t *testing.T) {
	body := []byte(`{"tool_choice":{"type":"auto","strict":true},"tools":[{"type":"mcp","mcp":{"server_url":"https://example.com/mcp","name":"calc","inputSchema":{"type":"object"}}}]}`)
	_, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP == nil || errMCP.Code != MCPCapabilityErrorNotDeclared {
		t.Fatalf("expected strict capability rejection, got %v", errMCP)
	}
	if errMCP.Capability != "strict_tool_use" {
		t.Fatalf("capability = %q, want strict_tool_use", errMCP.Capability)
	}
}

func TestClaudeMCPCapabilityManifest_OptionalListDoesNotProveSupport(t *testing.T) {
	body := []byte(`{"parallel_tool_calls":true,"tools":[{"type":"mcp","mcp":{"server_url":"https://example.com/mcp","name":"calc","inputSchema":{"type":"object"},"capabilities":{"optional":["parallel_tool_use"]}}}]}`)
	_, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP == nil || errMCP.Code != MCPCapabilityErrorNotDeclared {
		t.Fatalf("optional capability should fail closed, got %v", errMCP)
	}
}

func TestClaudeMCPCapabilityManifest_NoManifestAndNoRequestedModePasses(t *testing.T) {
	body := []byte(`{"tools":[{"type":"mcp","mcp":{"server_url":"https://example.com/mcp","name":"calc","inputSchema":{"type":"object"}}}]}`)
	out, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP != nil {
		t.Fatalf("unexpected error: %v", errMCP)
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "calc" {
		t.Fatalf("tools[0].name = %q, want calc", got)
	}
}

func TestClaudeMCPCapabilityManifest_InvalidRequiredListRejected(t *testing.T) {
	body := []byte(`{"tools":[{"type":"mcp","mcp":{"server_url":"https://example.com/mcp","name":"calc","inputSchema":{"type":"object"},"capabilities":{"required":["parallel_tool_use",42]}}}]}`)
	_, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP == nil || errMCP.Code != MCPCapabilityErrorManifestInvalid {
		t.Fatalf("expected invalid required list rejection, got %v", errMCP)
	}
}
