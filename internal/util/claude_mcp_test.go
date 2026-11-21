package util

import (
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

func TestRewriteClaudeMCPToolDefinitions_NoTools(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)
	out, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP != nil {
		t.Fatalf("unexpected error: %v", errMCP)
	}
	if string(out) != string(body) {
		t.Fatalf("body changed when no tools present: %s", out)
	}
}

func TestRewriteClaudeMCPToolDefinitions_NoMCPEntry(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","tools":[{"name":"web_search","description":"search","input_schema":{"type":"object"}}]}`)
	out, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP != nil {
		t.Fatalf("unexpected error: %v", errMCP)
	}
	if !gjson.ValidBytes(out) {
		t.Fatalf("output is not valid JSON: %s", out)
	}
	// Native tools are untouched.
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "web_search" {
		t.Fatalf("tools[0].name = %q, want web_search", got)
	}
	if gjson.GetBytes(out, "tools.0.mcp").Exists() {
		t.Fatalf("unexpected mcp field introduced")
	}
}

func TestRewriteClaudeMCPToolDefinitions_ValidEntry(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","tools":[{"type":"mcp","mcp":{"server_url":"https://example.com/tools","name":"getWeather","description":"Get current weather","inputSchema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]}`)
	out, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP != nil {
		t.Fatalf("unexpected error: %v", errMCP)
	}
	if !gjson.ValidBytes(out) {
		t.Fatalf("output is not valid JSON: %s", out)
	}
	tool := gjson.GetBytes(out, "tools.0")
	if v := tool.Get("name").String(); v != "getWeather" {
		t.Fatalf("tools[0].name = %q, want getWeather", v)
	}
	if v := tool.Get("description").String(); v != "Get current weather" {
		t.Fatalf("tools[0].description = %q", v)
	}
	if !tool.Get("input_schema").IsObject() {
		t.Fatalf("input_schema not an object: %s", tool.Get("input_schema").Raw)
	}
	if v := tool.Get("input_schema.properties.city.type").String(); v != "string" {
		t.Fatalf("input_schema.properties.city.type = %q, want string", v)
	}
	// The mcp envelope and type must be gone.
	if tool.Get("mcp").Exists() {
		t.Fatalf("mcp envelope still present: %s", out)
	}
	if v := tool.Get("type").String(); v != "" {
		t.Fatalf("type field still present: %q", v)
	}
}

func TestRewriteClaudeMCPToolDefinitions_EntryWithoutTypeUsesMCP(t *testing.T) {
	body := []byte(`{"tools":[{"mcp":{"name":"calc","description":"Calculator","inputSchema":{"type":"object","properties":{"expr":{"type":"string"}}}}}]}`)
	out, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP != nil {
		t.Fatalf("unexpected error: %v", errMCP)
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "calc" {
		t.Fatalf("tools[0].name = %q, want calc", got)
	}
	if gjson.GetBytes(out, "tools.0.mcp").Exists() {
		t.Fatalf("mcp envelope still present: %s", out)
	}
}

func TestRewriteClaudeMCPToolDefinitions_DescriptionTruncatedUTF8Safe(t *testing.T) {
	// 2-byte rune (é = U+00E9) repeated so the 1024-byte limit lands mid-rune.
	longDesc := strings.Repeat("é", 700)
	body := []byte(`{"tools":[{"type":"mcp","mcp":{"name":"big","description":"` + longDesc + `","inputSchema":{"type":"object"}}}]}`)
	out, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP != nil {
		t.Fatalf("unexpected error: %v", errMCP)
	}
	got := gjson.GetBytes(out, "tools.0.description").String()
	if len(got) > maxClaudeToolDescriptionLen {
		t.Fatalf("description length = %d, want <= %d", len(got), maxClaudeToolDescriptionLen)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncated description is not valid UTF-8")
	}
}

func TestRewriteClaudeMCPToolDefinitions_DescriptionTruncated(t *testing.T) {
	longDesc := strings.Repeat("x", 2000)
	body := []byte(`{"tools":[{"type":"mcp","mcp":{"name":"big","description":"` + longDesc + `","inputSchema":{"type":"object"}}}]}`)
	out, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP != nil {
		t.Fatalf("unexpected error: %v", errMCP)
	}
	if got := len(gjson.GetBytes(out, "tools.0.description").String()); got != maxClaudeToolDescriptionLen {
		t.Fatalf("description length = %d, want %d", got, maxClaudeToolDescriptionLen)
	}
}

func TestRewriteClaudeMCPToolDefinitions_RejectsUnion(t *testing.T) {
	body := []byte(`{"tools":[{"type":"mcp","mcp":{"name":"bad","inputSchema":{"anyOf":[{"type":"object"},{"type":"string"}]}}}]}`)
	_, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP == nil {
		t.Fatal("expected union rejection")
	}
	if errMCP.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", errMCP.Status, http.StatusUnprocessableEntity)
	}
	if !strings.Contains(errMCP.Field, "inputSchema.anyOf") {
		t.Fatalf("field = %q, want inputSchema.anyOf", errMCP.Field)
	}
}

func TestRewriteClaudeMCPToolDefinitions_RejectsUnionTypeArray(t *testing.T) {
	body := []byte(`{"tools":[{"type":"mcp","mcp":{"name":"bad","inputSchema":{"type":["object","null"]}}}]}`)
	_, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP == nil {
		t.Fatal("expected union type array rejection")
	}
	if errMCP.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", errMCP.Status, http.StatusUnprocessableEntity)
	}
}

func TestRewriteClaudeMCPToolDefinitions_RejectsOneOf(t *testing.T) {
	body := []byte(`{"tools":[{"type":"mcp","mcp":{"name":"bad","inputSchema":{"oneOf":[{"type":"object"},{"type":"array"}]}}}]}`)
	_, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP == nil {
		t.Fatal("expected oneOf rejection")
	}
	if errMCP.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", errMCP.Status, http.StatusUnprocessableEntity)
	}
}

func TestRewriteClaudeMCPToolDefinitions_RejectsNonObjectRootType(t *testing.T) {
	body := []byte(`{"tools":[{"type":"mcp","mcp":{"name":"bad","inputSchema":{"type":"string"}}}]}`)
	_, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP == nil {
		t.Fatal("expected non-object root type rejection")
	}
	if errMCP.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", errMCP.Status, http.StatusUnprocessableEntity)
	}
}

func TestRewriteClaudeMCPToolDefinitions_RejectsMissingName(t *testing.T) {
	body := []byte(`{"tools":[{"type":"mcp","mcp":{"description":"no name here","inputSchema":{"type":"object"}}}]}`)
	_, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP == nil {
		t.Fatal("expected missing name rejection")
	}
	if errMCP.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", errMCP.Status, http.StatusBadRequest)
	}
}

func TestRewriteClaudeMCPToolDefinitions_RejectsMissingInputSchema(t *testing.T) {
	body := []byte(`{"tools":[{"type":"mcp","mcp":{"name":"lonely"}}]}`)
	_, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP == nil {
		t.Fatal("expected missing inputSchema rejection")
	}
	if errMCP.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", errMCP.Status, http.StatusBadRequest)
	}
}

func TestRewriteClaudeMCPToolDefinitions_MixedToolArray(t *testing.T) {
	body := []byte(`{"tools":[{"name":"web_search","description":"search","input_schema":{"type":"object"}},{"type":"mcp","mcp":{"name":"calc","description":"Calculator","inputSchema":{"type":"object","properties":{"expr":{"type":"string"}}}}}]}`)
	out, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP != nil {
		t.Fatalf("unexpected error: %v", errMCP)
	}
	if got := gjson.GetBytes(out, "tools").Array(); len(got) != 2 {
		t.Fatalf("tools len = %d, want 2", len(got))
	}
	if v := gjson.GetBytes(out, "tools.0.name").String(); v != "web_search" {
		t.Fatalf("tools[0].name = %q, want web_search", v)
	}
	if v := gjson.GetBytes(out, "tools.1.name").String(); v != "calc" {
		t.Fatalf("tools[1].name = %q, want calc", v)
	}
}

func TestRewriteClaudeMCPToolDefinitions_ToolsTooLarge(t *testing.T) {
	var tools strings.Builder
	tools.WriteByte('[')
	for i := 0; i < 20000; i++ {
		if i > 0 {
			tools.WriteByte(',')
		}
		tools.WriteString(`{"name":"web_search","input_schema":{"type":"object"}}`)
	}
	// Each entry is ~55 bytes, 20000 entries is well over 1 MiB.
	tools.WriteByte(']')
	body := []byte(`{"tools":` + tools.String() + `}`)
	rawTools := gjson.GetBytes(body, "tools").Raw
	if len(rawTools) <= MaxClaudeMCPToolsBytes {
		t.Fatalf("test fixture too small: %d bytes", len(rawTools))
	}
	_, errMCP := RewriteClaudeMCPToolDefinitions(body)
	if errMCP == nil {
		t.Fatal("expected oversized tools rejection")
	}
	if errMCP.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", errMCP.Status, http.StatusRequestEntityTooLarge)
	}
	if !strings.Contains(errMCP.Message, "limit") {
		t.Fatalf("message = %q, want size limit mention", errMCP.Message)
	}
}
