package util

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// MaxClaudeMCPToolsBytes is the largest accepted serialized tools payload for
// the /v1/messages tools parameter. It mirrors the 1 MiB body cap used by the
// audit-chain handler so callers get the same size protection.
const MaxClaudeMCPToolsBytes = 1 << 20 // 1 MiB

// maxClaudeToolDescriptionLen is Claude's tool description length limit.
// MCP descriptions longer than this are truncated with a warning.
const maxClaudeToolDescriptionLen = 1024

// MCPToolSchemaError records a rejected MCP tool definition: the HTTP status
// to return, the concrete field that failed, and a human-readable message.
type MCPToolSchemaError struct {
	Status     int
	Field      string
	Message    string
	Code       string
	Capability string
	Fallback   string
}

// Error implements the error interface.
func (e *MCPToolSchemaError) Error() string { return e.Message }

// RewriteClaudeMCPToolDefinitions validates the optional MCP capability
// manifest for every tools[].mcp entry, then converts those entries into
// native Anthropic tool shape {"name","description","input_schema"}. Entries
// without an MCP definition are left untouched and the original body is
// returned unchanged when no MCP entry exists.
//
// Two rejection classes are reported as *MCPToolSchemaError:
//   - the serialized tools payload exceeds MaxClaudeMCPToolsBytes (413);
//   - an MCP input schema is structurally invalid or uses an unsupported
//     union type at the root (400/422) with the precise field path.
//   - an MCP capability manifest is missing a requested optional capability or
//     has an invalid field type (422) with the code MCP_CAPABILITY_NOT_DECLARED
//     or MCP_CAPABILITY_MANIFEST_INVALID.
//
// Native Claude -> Anthropic requests pass the body verbatim to upstream, so
// this rewrite must run before dispatch or the unknown mcp field would be
// forwarded.
func RewriteClaudeMCPToolDefinitions(body []byte) ([]byte, *MCPToolSchemaError) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return body, nil
	}
	if len(tools.Raw) > MaxClaudeMCPToolsBytes {
		return nil, &MCPToolSchemaError{
			Status:  http.StatusRequestEntityTooLarge,
			Field:   "tools",
			Message: fmt.Sprintf("tools payload exceeds the %d-byte (%d MiB) limit", MaxClaudeMCPToolsBytes, MaxClaudeMCPToolsBytes>>20),
		}
	}

	converted := body
	convertedAny := false
	desired := parseClaudeMCPRequestCapabilities(converted)
	requestedCapabilities := desired.ParallelToolUse || desired.StrictToolUse
	degraded := false
	var degradedCapabilities []string
	checkedTools := 0
	index := 0
	for _, tool := range tools.Array() {
		path := fmt.Sprintf("tools.%d", index)
		if !isClaudeMCPToolEntry(tool) {
			index++
			continue
		}
		if capErr := validateClaudeMCPCapabilityManifest(converted, tool, index); capErr != nil {
			if desired.AutoDegrade && capErr.Code == MCPCapabilityErrorNotDeclared {
				// A single manifest can be missing several capabilities. Keep
				// degrading and re-validating until the request is compatible or
				// an invalid manifest must still fail closed.
				degradeAttempts := 0
				for capErr != nil && desired.AutoDegrade &&
					capErr.Code == MCPCapabilityErrorNotDeclared && degradeAttempts < 2 {
					converted = degradeClaudeMCPRequest(converted, capErr.Capability)
					desired = parseClaudeMCPRequestCapabilities(converted)
					degraded = true
					degradedCapabilities = appendUniqueString(degradedCapabilities, capErr.Capability)
					degradeAttempts++
					capErr = validateClaudeMCPCapabilityManifest(converted, tool, index)
				}
				if capErr != nil {
					return nil, capErr
				}
				checkedTools++
			} else {
				return nil, capErr
			}
		} else {
			checkedTools++
		}
		native, errMCP := buildClaudeToolFromMCP(tool, index)
		if errMCP != nil {
			return nil, errMCP
		}
		updated, errSet := sjson.SetRawBytes(converted, path, native)
		if errSet != nil {
			return nil, &MCPToolSchemaError{
				Status:  http.StatusBadRequest,
				Field:   path,
				Message: fmt.Sprintf("unable to rewrite MCP tool definition at %s: %v", path, errSet),
			}
		}
		converted = updated
		convertedAny = true
		index++
	}
	if !convertedAny {
		return body, nil
	}
	if checkedTools > 0 && (requestedCapabilities || degraded) {
		degradedCount := 0
		if degraded {
			degradedCount = len(degradedCapabilities)
		}
		logMCPCapabilityCheck(checkedTools, degradedCount, degradedCapabilities)
	}
	return converted, nil
}

// isClaudeMCPToolEntry reports whether a tools[] entry declares an MCP tool:
// either the entry's type is "mcp", or it carries a nested mcp definition
// object.
func isClaudeMCPToolEntry(tool gjson.Result) bool {
	if tool.Get("mcp").Exists() {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(tool.Get("type").String()), "mcp")
}

// buildClaudeToolFromMCP maps one MCP tool entry to the native Anthropic tool
// shape, validating the input schema along the way.
func buildClaudeToolFromMCP(tool gjson.Result, index int) ([]byte, *MCPToolSchemaError) {
	mcp := tool.Get("mcp")
	if mcp.Exists() && !mcp.IsObject() {
		return nil, claudeMCPFieldErr(index, "mcp", "must be a JSON object", http.StatusBadRequest)
	}
	source := tool
	if mcp.Exists() {
		// Prefer the nested MCP definition; also permit type:"mcp" entries that
		// carry the MCP tool shape directly on the entry itself.
		source = mcp
	}

	name := strings.TrimSpace(source.Get("name").String())
	if name == "" {
		name = strings.TrimSpace(tool.Get("name").String())
	}
	if name == "" {
		return nil, claudeMCPFieldErr(index, "name", "missing tool name", http.StatusBadRequest)
	}
	if len(name) > 64 {
		return nil, claudeMCPFieldErr(index, "name", fmt.Sprintf("tool name %q exceeds 64 characters", name), http.StatusBadRequest)
	}

	description := strings.TrimSpace(source.Get("description").String())
	if description == "" {
		description = strings.TrimSpace(source.Get("title").String())
	}
	if len(description) > maxClaudeToolDescriptionLen {
		log.Warnf("claude mcp tool %q description truncated from %d to %d bytes", name, len(description), maxClaudeToolDescriptionLen)
		description = truncateClaudeMCPDescription(description)
	}

	inputSchema := source.Get("inputSchema")
	if !inputSchema.Exists() {
		// Be lenient with the native snake_case spelling too.
		inputSchema = source.Get("input_schema")
	}
	if !inputSchema.Exists() {
		return nil, claudeMCPFieldErr(index, "inputSchema", fmt.Sprintf("tool %q is missing inputSchema", name), http.StatusBadRequest)
	}
	if errSchema := rejectClaudeUnsupportedInputSchema(inputSchema, index, name); errSchema != nil {
		return nil, errSchema
	}

	native := []byte(`{"name":"","description":"","input_schema":{}}`)
	native, _ = sjson.SetRawBytes(native, "name", []byte(marshalClaudeMCPString(name)))
	if description != "" {
		native, _ = sjson.SetRawBytes(native, "description", []byte(marshalClaudeMCPString(description)))
	}
	native, errSet := sjson.SetRawBytes(native, "input_schema", []byte(inputSchema.Raw))
	if errSet != nil {
		return nil, claudeMCPFieldErr(index, "inputSchema", fmt.Sprintf("tool %q has an invalid inputSchema: %v", name, errSet), http.StatusUnprocessableEntity)
	}
	return native, nil
}

// truncateClaudeMCPDescription truncates a tool description to the description
// length limit without splitting a UTF-8 rune, so the serialized JSON stays
// valid regardless of the description's character set.
func truncateClaudeMCPDescription(description string) string {
	if len(description) <= maxClaudeToolDescriptionLen {
		return description
	}
	limit := maxClaudeToolDescriptionLen
	for limit > 0 && !utf8.RuneStart(description[limit]) {
		limit--
	}
	if limit == 0 {
		return ""
	}
	return description[:limit]
}

// rejectClaudeUnsupportedInputSchema returns a field-error for input schemas
// Claude cannot consume as a tool input schema: root-level unions (anyOf /
// oneOf / allOf), a union type array, or a non-object root type.
func rejectClaudeUnsupportedInputSchema(schema gjson.Result, index int, name string) *MCPToolSchemaError {
	if !schema.IsObject() {
		return claudeMCPFieldErr(index, "inputSchema", fmt.Sprintf("tool %q inputSchema must be a JSON object", name), http.StatusUnprocessableEntity)
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if schema.Get(key).Exists() {
			return claudeMCPFieldErr(index, "inputSchema."+key, fmt.Sprintf("tool %q inputSchema uses unsupported union type %q", name, key), http.StatusUnprocessableEntity)
		}
	}
	typeVal := schema.Get("type")
	if typeVal.Exists() {
		if typeVal.IsArray() {
			if len(typeVal.Array()) > 1 {
				return claudeMCPFieldErr(index, "inputSchema.type", fmt.Sprintf("tool %q inputSchema uses an unsupported union type array", name), http.StatusUnprocessableEntity)
			}
			if len(typeVal.Array()) == 1 && typeVal.Array()[0].String() != "object" {
				return claudeMCPFieldErr(index, "inputSchema.type", fmt.Sprintf("tool %q inputSchema root type must be %q, got %q", name, "object", typeVal.Array()[0].String()), http.StatusUnprocessableEntity)
			}
		} else if t := typeVal.String(); t != "" && !strings.EqualFold(t, "object") {
			return claudeMCPFieldErr(index, "inputSchema.type", fmt.Sprintf("tool %q inputSchema root type must be %q, got %q", name, "object", t), http.StatusUnprocessableEntity)
		}
	}
	return nil
}

func claudeMCPFieldErr(index int, suffix, message string, status int) *MCPToolSchemaError {
	field := fmt.Sprintf("tools[%d]", index)
	if suffix != "" {
		field += "." + suffix
	}
	return &MCPToolSchemaError{
		Status:  status,
		Field:   field,
		Message: fmt.Sprintf("unsupported MCP tool definition at %s: %s", field, message),
	}
}

// marshalClaudeMCPString encodes a string value as JSON.
func marshalClaudeMCPString(value string) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(raw)
}
