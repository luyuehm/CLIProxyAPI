package claude

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
)

func TestClaudeMessages_RejectsUnsupportedMCPUnion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := `{"model":"claude-sonnet-4-6","stream":false,"tools":[{"type":"mcp","mcp":{"name":"bad","inputSchema":{"anyOf":[{"type":"object"},{"type":"string"}]}}}]}`

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))

	handler := &ClaudeCodeAPIHandler{}
	handler.ClaudeMessages(c)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusUnprocessableEntity, recorder.Body.String())
	}
	respBody := recorder.Body.Bytes()
	if got := gjson.GetBytes(respBody, "error.type").String(); got != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error; body=%s", got, respBody)
	}
	if msg := gjson.GetBytes(respBody, "error.message").String(); !strings.Contains(msg, "inputSchema.anyOf") {
		t.Fatalf("error.message = %q, want inputSchema.anyOf mention; body=%s", msg, respBody)
	}
}

func TestClaudeMessages_RejectsOversizedTools(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var tools strings.Builder
	tools.WriteByte('[')
	for i := 0; i < 20000; i++ {
		if i > 0 {
			tools.WriteByte(',')
		}
		tools.WriteString(`{"name":"web_search","input_schema":{"type":"object"}}`)
	}
	tools.WriteByte(']')
	body := `{"model":"claude-sonnet-4-6","stream":false,"tools":` + tools.String() + `}`

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))

	handler := &ClaudeCodeAPIHandler{}
	handler.ClaudeMessages(c)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusRequestEntityTooLarge, recorder.Body.String())
	}
	respBody := recorder.Body.Bytes()
	if got := gjson.GetBytes(respBody, "error.type").String(); got != "request_too_large" {
		t.Fatalf("error.type = %q, want request_too_large; body=%s", got, respBody)
	}
	if msg := gjson.GetBytes(respBody, "error.message").String(); !strings.Contains(msg, "limit") {
		t.Fatalf("error.message = %q, want size limit mention; body=%s", msg, respBody)
	}
}

func TestClaudeMessages_MissingMCPCapabilityReturnsStructuredCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := `{"model":"claude-sonnet-4-6","stream":false,"parallel_tool_calls":true,"tools":[{"type":"mcp","mcp":{"server_url":"https://example.com/mcp","name":"calc","inputSchema":{"type":"object"}}}]}`

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))

	handler := &ClaudeCodeAPIHandler{}
	handler.ClaudeMessages(c)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusUnprocessableEntity, recorder.Body.String())
	}
	respBody := recorder.Body.Bytes()
	if got := gjson.GetBytes(respBody, "error.code").String(); got != util.MCPCapabilityErrorNotDeclared {
		t.Fatalf("error.code = %q, want %q; body=%s", got, util.MCPCapabilityErrorNotDeclared, respBody)
	}
	if msg := gjson.GetBytes(respBody, "error.message").String(); !strings.Contains(msg, "degrade to serial") {
		t.Fatalf("error.message = %q, want serial degradation hint; body=%s", msg, respBody)
	}
}

func TestClaudeMessages_InvalidMCPCapabilityReturnsStructuredCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := `{"model":"claude-sonnet-4-6","stream":false,"tools":[{"type":"mcp","mcp":{"server_url":"https://example.com/mcp","name":"calc","inputSchema":{"type":"object"},"capabilities":{"parallel_tool_use":"yes"}}}]}`

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))

	handler := &ClaudeCodeAPIHandler{}
	handler.ClaudeMessages(c)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusUnprocessableEntity, recorder.Body.String())
	}
	respBody := recorder.Body.Bytes()
	if got := gjson.GetBytes(respBody, "error.code").String(); got != util.MCPCapabilityErrorManifestInvalid {
		t.Fatalf("error.code = %q, want %q; body=%s", got, util.MCPCapabilityErrorManifestInvalid, respBody)
	}
}

func TestWriteMCPSchemaErrorResponse_PreservesStatusAndField(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	handler := &ClaudeCodeAPIHandler{}

	handler.WriteMCPSchemaErrorResponse(c, &util.MCPToolSchemaError{
		Status:  http.StatusUnprocessableEntity,
		Field:   "tools[0].inputSchema.anyOf",
		Message: "unsupported MCP tool definition at tools[0].inputSchema.anyOf: union not supported",
	})

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnprocessableEntity)
	}
	respBody := recorder.Body.Bytes()
	if got := gjson.GetBytes(respBody, "error.type").String(); got != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error; body=%s", got, respBody)
	}
	if msg := gjson.GetBytes(respBody, "error.message").String(); !strings.Contains(msg, "tools[0].inputSchema.anyOf") {
		t.Fatalf("error.message = %q, want field mention", msg)
	}
}

func TestWriteMCPSchemaErrorResponse_TooLarge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	handler := &ClaudeCodeAPIHandler{}

	handler.WriteMCPSchemaErrorResponse(c, &util.MCPToolSchemaError{
		Status:  http.StatusRequestEntityTooLarge,
		Field:   "tools",
		Message: "tools payload exceeds the 1048576-byte (1 MiB) limit",
	})

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusRequestEntityTooLarge)
	}
	respBody := recorder.Body.Bytes()
	if got := gjson.GetBytes(respBody, "error.type").String(); got != "request_too_large" {
		t.Fatalf("error.type = %q, want request_too_large; body=%s", got, respBody)
	}
}
