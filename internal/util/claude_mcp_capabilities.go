package util

import (
	"fmt"
	"net/http"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Structured capability error codes. They are intentionally stable so
// monitoring, alerting, and client fallback logic can match on the contract
// instead of parsing human-readable messages.
const (
	// MCPCapabilityErrorNotDeclared means the manifest did not prove support
	// for a requested optional protocol capability.
	MCPCapabilityErrorNotDeclared = "MCP_CAPABILITY_NOT_DECLARED"
	// MCPCapabilityErrorManifestInvalid means the manifest existed but its
	// known fields had the wrong JSON type.
	MCPCapabilityErrorManifestInvalid = "MCP_CAPABILITY_MANIFEST_INVALID"
)

// claudeMCPRequestCapabilities describes which optional MCP protocol features
// the current /v1/messages request is asking the gateway to use.
type claudeMCPRequestCapabilities struct {
	ParallelToolUse bool
	StrictToolUse   bool
	AutoDegrade     bool
}

// parseClaudeMCPRequestCapabilities reads the client-side request signals.
// Anthropic-style requests may carry these under a nested tool_use object, so
// both spellings are accepted.
func parseClaudeMCPRequestCapabilities(body []byte) claudeMCPRequestCapabilities {
	return claudeMCPRequestCapabilities{
		ParallelToolUse: jsonBoolAt(body, "parallel_tool_calls") ||
			jsonBoolAt(body, "tool_use.parallel_tool_calls") ||
			jsonBoolAt(body, "tool_use.parallel") ||
			jsonDisableParallelToolUse(body),
		StrictToolUse: jsonBoolAt(body, "strict_tool_use") ||
			jsonBoolAt(body, "tool_use.strict") ||
			jsonBoolAt(body, "tool_choice.strict"),
		AutoDegrade: jsonBoolAt(body, "mcp_capability_auto_degrade") ||
			jsonBoolAt(body, "allow_mcp_capability_degrade") ||
			jsonBoolAt(body, "tool_use.auto_degrade"),
	}
}

// validateClaudeMCPCapabilityManifest validates the capability contract of one
// MCP tool definition against the requested request modes.
//
// A missing manifest, a manifest that omits a requested feature, and an
// explicit false all have the same meaning under the fail-closed contract:
// capability unknown. Only a structurally invalid manifest gets the manifest
// error; every unknown case gets MCP_CAPABILITY_NOT_DECLARED.
func validateClaudeMCPCapabilityManifest(body []byte, tool gjson.Result, index int) *MCPToolSchemaError {
	source := claudeMCPManifestSource(tool)
	capabilities := source.Get("capabilities")
	if !capabilities.Exists() {
		return validateClaudeMCPDeclaredCapabilities(body, tool, index, false, false)
	}
	if !capabilities.IsObject() {
		return claudeMCPCapabilityErr(index, "capabilities", MCPCapabilityErrorManifestInvalid,
			"capabilities must be a JSON object", http.StatusUnprocessableEntity)
	}

	parallelPresent, parallelDeclared, parallelSupported := claudeMCPManifestCapability(capabilities, "parallel_tool_use")
	if parallelPresent {
		if err := claudeMCPCapabilityTypeErr(capabilities, "parallel_tool_use", index); err != nil {
			return err
		}
	}
	strictPresent, strictDeclared, strictSupported := claudeMCPManifestCapability(capabilities, "strict_tool_use")
	if strictPresent {
		if err := claudeMCPCapabilityTypeErr(capabilities, "strict_tool_use", index); err != nil {
			return err
		}
	}
	if err := validateClaudeMCPCapabilityList(capabilities, "required", index); err != nil {
		return err
	}
	if err := validateClaudeMCPCapabilityList(capabilities, "optional", index); err != nil {
		return err
	}

	return validateClaudeMCPDeclaredCapabilities(body, tool, index, parallelDeclared && parallelSupported, strictDeclared && strictSupported)
}

// validateClaudeMCPDeclaredCapabilities compares the resolved manifest state
// against the request. It returns nil when the request needs nothing or when
// every requested feature is proven supported.
func validateClaudeMCPDeclaredCapabilities(body []byte, tool gjson.Result, index int, parallelSupported, strictSupported bool) *MCPToolSchemaError {
	desired := parseClaudeMCPRequestCapabilities(body)
	if desired.ParallelToolUse && !parallelSupported {
		return claudeMCPCapabilityNotDeclaredErr(tool, index, "parallel_tool_use",
			"set parallel_tool_calls=false to degrade to serial tool use")
	}
	if desired.StrictToolUse && !strictSupported {
		return claudeMCPCapabilityNotDeclaredErr(tool, index, "strict_tool_use",
			"set strict_tool_use=false to degrade to permissive tool use")
	}
	return nil
}

// claudeMCPManifestSource returns the object that carries the manifest. A
// nested mcp object is preferred, while type:"mcp" entries may also carry the
// capability block directly on the tool entry.
func claudeMCPManifestSource(tool gjson.Result) gjson.Result {
	if mcp := tool.Get("mcp"); mcp.Exists() && mcp.IsObject() {
		return mcp
	}
	return tool
}

// claudeMCPManifestCapability resolves one capability. The first return value
// reports whether a boolean flag is present (and therefore subject to type
// validation). A boolean flag is authoritative. A required list entry is
// treated as proof of support. An optional list entry alone does not prove
// support, matching the "unknown must fail closed" contract.
func claudeMCPManifestCapability(capabilities gjson.Result, name string) (present, declared, supported bool) {
	if value := capabilities.Get(name); value.Exists() {
		if value.Type == gjson.True || value.Type == gjson.False {
			return true, true, value.Bool()
		}
		return true, true, false
	}
	if containsString(capabilities.Get("required"), name) {
		return false, true, true
	}
	if containsString(capabilities.Get("optional"), name) {
		// Optional declaration does not establish support; it only documents
		// that support may vary by deployment.
		return false, true, false
	}
	return false, false, false
}

// claudeMCPCapabilityTypeErr rejects a known capability field with a non-boolean
// JSON value.
func claudeMCPCapabilityTypeErr(capabilities gjson.Result, name string, index int) *MCPToolSchemaError {
	value := capabilities.Get(name)
	if value.Type == gjson.True || value.Type == gjson.False {
		return nil
	}
	field := "capabilities." + name
	return claudeMCPCapabilityErr(index, field, MCPCapabilityErrorManifestInvalid,
		fmt.Sprintf("%s must be a JSON boolean, got %s", name, gjsonTypeName(value)), http.StatusUnprocessableEntity)
}

// validateClaudeMCPCapabilityList checks that required/optional lists are
// string arrays when present.
func validateClaudeMCPCapabilityList(capabilities gjson.Result, name string, index int) *MCPToolSchemaError {
	value := capabilities.Get(name)
	if !value.Exists() {
		return nil
	}
	if !value.IsArray() {
		return claudeMCPCapabilityErr(index, "capabilities."+name, MCPCapabilityErrorManifestInvalid,
			name+" must be a JSON array of strings", http.StatusUnprocessableEntity)
	}
	for _, entry := range value.Array() {
		if entry.Type != gjson.String {
			return claudeMCPCapabilityErr(index, "capabilities."+name, MCPCapabilityErrorManifestInvalid,
				name+" entries must be strings", http.StatusUnprocessableEntity)
		}
	}
	return nil
}

// claudeMCPCapabilityNotDeclaredErr builds the fail-closed error for a missing
// or unknown capability.
func claudeMCPCapabilityNotDeclaredErr(tool gjson.Result, index int, capability, fallback string) *MCPToolSchemaError {
	source := claudeMCPManifestSource(tool)
	toolName := strings.TrimSpace(source.Get("name").String())
	if toolName == "" {
		toolName = strings.TrimSpace(tool.Get("name").String())
	}
	server := strings.TrimSpace(source.Get("server_url").String())
	if server == "" {
		server = strings.TrimSpace(tool.Get("server_url").String())
	}
	message := fmt.Sprintf("server %q tool %q does not declare %s; %s", server, toolName, capability, fallback)
	err := claudeMCPCapabilityErr(index, "capabilities", MCPCapabilityErrorNotDeclared, message, http.StatusUnprocessableEntity)
	err.Capability = capability
	err.Fallback = fallback
	return err
}

// claudeMCPCapabilityErr builds a capability error with the requested field and
// stable structured code.
func claudeMCPCapabilityErr(index int, suffix, code, message string, status int) *MCPToolSchemaError {
	field := fmt.Sprintf("tools[%d]", index)
	if suffix != "" {
		field += ".mcp." + suffix
	}
	return &MCPToolSchemaError{
		Status:  status,
		Field:   field,
		Message: fmt.Sprintf("unsupported MCP capability contract at %s: %s", field, message),
		Code:    code,
	}
}

// degradeClaudeMCPRequest rewrites only the fields that already requested the
// unsupported capability, so upstream translators see an explicit serial /
// permissive request instead of a silently unsupported one.
func degradeClaudeMCPRequest(body []byte, capability string) []byte {
	switch capability {
	case "parallel_tool_use":
		for _, path := range []string{"parallel_tool_calls", "tool_use.parallel_tool_calls", "tool_use.parallel"} {
			if gjson.GetBytes(body, path).Exists() {
				body, _ = sjson.SetBytes(body, path, false)
			}
		}
		if gjson.GetBytes(body, "tool_choice.disable_parallel_tool_use").Exists() {
			body, _ = sjson.SetBytes(body, "tool_choice.disable_parallel_tool_use", true)
		}
	case "strict_tool_use":
		for _, path := range []string{"strict_tool_use", "tool_use.strict", "tool_choice.strict"} {
			if gjson.GetBytes(body, path).Exists() {
				body, _ = sjson.SetBytes(body, path, false)
			}
		}
	}
	return body
}

// logMCPCapabilityCheck emits one per-request capability result. The boolean
// field is deliberately explicit so operators can filter capability_ok=false.
func logMCPCapabilityCheck(tools, degradedCount int, degraded []string) {
	fields := log.Fields{
		"component":      "mcp_capability",
		"mcp_tools":      tools,
		"capability_ok":  true,
		"degraded_count": 0,
	}
	if degradedCount > 0 {
		fields["capability_ok"] = false
		fields["capability_degraded"] = true
		fields["degraded_count"] = degradedCount
		fields["degraded_capabilities"] = degraded
		log.WithFields(fields).Warn("mcp capability contract required explicit degradation")
		return
	}
	log.WithFields(fields).Info("mcp capability contract ok")
}

// jsonBoolAt reports whether a JSON path exists and has a boolean true value.
func jsonBoolAt(body []byte, path string) bool {
	value := gjson.GetBytes(body, path)
	return value.Type == gjson.True
}

// jsonDisableParallelToolUse reports an explicit request for parallel tool use
// expressed as tool_choice.disable_parallel_tool_use=false. When that field is
// true the request is already serial, so no capability is needed.
func jsonDisableParallelToolUse(body []byte) bool {
	value := gjson.GetBytes(body, "tool_choice.disable_parallel_tool_use")
	if !value.Exists() || value.Type == gjson.Null {
		return false
	}
	return value.Type == gjson.False
}

// containsString reports whether a JSON string array contains value.
func containsString(result gjson.Result, value string) bool {
	if !result.IsArray() {
		return false
	}
	for _, item := range result.Array() {
		if item.Type == gjson.String && item.String() == value {
			return true
		}
	}
	return false
}

// appendUniqueString appends value when it is not already present.
func appendUniqueString(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// gjsonTypeName returns a stable name for a gjson value.
func gjsonTypeName(value gjson.Result) string {
	switch value.Type {
	case gjson.Null:
		return "null"
	case gjson.False:
		return "false"
	case gjson.Number:
		return "number"
	case gjson.String:
		return "string"
	case gjson.True:
		return "true"
	case gjson.JSON:
		if value.IsArray() {
			return "array"
		}
		return "object"
	default:
		return "unknown"
	}
}
