# MCP Tool Definitions for `/v1/messages` (RIC-2950)

Anthropic's native Messages API accepts a tool declarations array where each
entry is an Anthropic-shaped tool (`name`, `description`, `input_schema`).
This gateway extends `/v1/messages` (and `/v1/messages/count_tokens`) so a tool
declaration may instead carry an **MCP tool definition** that is mapped onto the
native shape before the request is routed upstream. The MCP convention is
standardized across MCP servers, so a tool authored once can be shared with any
Agent that speaks the Messages protocol without a hand-written adapter.

## Request format

Add an entry to the `tools` array with `type: "mcp"` and an `mcp` object that
carries the MCP tool definition:

```json
{
  "model": "claude-sonnet-4-6",
  "tools": [
    {
      "type": "mcp",
      "mcp": {
        "server_url": "https://tools.example.com/mcp",
        "name": "get_weather",
        "description": "Get the current weather for a city. Provide the city name.",
        "inputSchema": {
          "type": "object",
          "properties": {
            "city": { "type": "string", "description": "City name" }
          },
          "required": ["city"]
        }
      }
    }
  ]
}
```

Field semantics:

| Field | Required | Meaning |
|-------|----------|---------|
| `tools[].type` | yes | `"mcp"` for an MCP tool definition. |
| `tools[].mcp.name` | yes | Tool name (1–64 chars). Mapped to `tools[].name`. |
| `tools[].mcp.description` | no | Tool description. If longer than 1024 bytes it is truncated with a warning log. |
| `tools[].mcp.inputSchema` | yes | JSON Schema (object) describing the tool input. Mapped to `tools[].input_schema`. |
| `tools[].mcp.server_url` | no | Optional MCP server provenance; currently informational only. |
| `tools[].mcp.capabilities` | no | Optional capability manifest (object). Declares optional MCP protocol features this server supports. |

A `tools[].mcp` entry may also omit `type`; the presence of the `mcp` object is
sufficient to trigger mapping. The `tools[].mcp.input_schema` snake_case spelling
is accepted as well. Non-MCP entries in the same `tools` array are left
untouched, so native Anthropic tools and MCP tools can be mixed.

## Capability manifest

The capability manifest is a machine-readable declaration that lets the client
pre-check optional MCP protocol behavior before dispatch. It is validated in
the same request rewrite that maps `tools[].mcp` to native Anthropic tools.

```json
{
  "mcp": {
    "server_url": "https://tools.example.com/mcp",
    "name": "get_weather",
    "inputSchema": { "type": "object" },
    "capabilities": {
      "required": ["strict_tool_use"],
      "optional": ["resource_templates"],
      "parallel_tool_use": true,
      "strict_tool_use": true
    }
  }
}
```

| Field | Meaning |
|-------|---------|
| `mcp.capabilities.parallel_tool_use` | Boolean. `true` proves parallel tool use support. |
| `mcp.capabilities.strict_tool_use` | Boolean. `true` proves strict tool use support. |
| `mcp.capabilities.required` | String array. A listed capability is treated as proven support when no boolean is present. |
| `mcp.capabilities.optional` | String array. Documentation only; does **not** prove support. |

Request mode is derived from the same body:

| Request signal | Meaning |
|----------------|---------|
| `parallel_tool_calls: true` | Asks the gateway to use parallel tool calls. |
| `strict_tool_use: true` | Asks the gateway to use strict tool use. |
| `tool_choice.disable_parallel_tool_use: false` | Equivalent parallel request signal. |
| `tool_choice.strict: true` | Equivalent strict request signal. |
| `tool_use.parallel` / `tool_use.strict` | Additional nested spellings accepted for SDK compatibility. |

The default is fail-closed: a requested capability that is absent, explicitly
`false`, or listed only under `optional` returns `422` with
`error.code: "MCP_CAPABILITY_NOT_DECLARED"` and a serial / permissive repair
hint. The gateway never silently degrades.

To opt in to an explicit compatibility fallback, set
`mcp_capability_auto_degrade: true`. When enabled, a missing optional
capability rewrites only the corresponding request signal to `false` and logs
`capability_ok=false, capability_degraded=true`. An invalid manifest still
fails; the switch does not hide a broken contract.

```bash
curl -s 'https://<host>/v1/messages' \
  -H 'x-api-key: <key>' -H 'content-type: application/json' \
  -d '{
    "model": "claude-sonnet-4-6",
    "parallel_tool_calls": true,
    "tools": [{
      "type": "mcp",
      "mcp": {
        "server_url": "https://tools.example.com/mcp",
        "name": "get_weather",
        "inputSchema": { "type": "object" }
      }
    }]
  }'
```

Response:

```json
{
  "type": "error",
  "error": {
    "type": "invalid_request_error",
    "code": "MCP_CAPABILITY_NOT_DECLARED",
    "message": "unsupported MCP capability contract at tools[0].mcp.capabilities: server \"https://tools.example.com/mcp\" tool \"get_weather\" does not declare parallel_tool_use; set parallel_tool_calls=false to degrade to serial tool use (field: tools[0].mcp.capabilities)"
  }
}
```

An invalid manifest field type returns `422` with
`error.code: "MCP_CAPABILITY_MANIFEST_INVALID"` and a field-specific repair
hint, so the client never enters the running state with a malformed contract.

## Mapping behavior

For each `tools[].mcp` entry the gateway rewrites the declaration to the native
Anthropic shape before dispatch:

```json
{
  "type": "",                                      /* dropped */
  "mcp": { ... },                                  /* dropped */
  "name": "get_weather",
  "description": "Get the current weather for a city. ...",
  "input_schema": {
    "type": "object",
    "properties": { "city": { "type": "string", "description": "City name" } },
    "required": ["city"]
  }
}
```

This keeps the payload identical to what an Anthropic-native client would send,
so downstream translation (Claude → Gemini, Claude → OpenAI, and so on) needs no
MCP-specific logic.

## Input schema validation

The input schema must be a JSON object. The following constructs are rejected
with HTTP `422` and an `invalid_request_error` that names the offending field:

- Root-level unions: `anyOf`, `oneOf`, `allOf`.
- A `type` array with more than one element (a union type).
- A root `type` other than `object`.

```bash
curl -s 'https://<host>/v1/messages' \
  -H 'x-api-key: <key>' -H 'content-type: application/json' \
  -d '{
    "model": "claude-sonnet-4-6",
    "tools": [{
      "type": "mcp",
      "mcp": {
        "name": "bad",
        "inputSchema": { "anyOf": [{ "type": "object" }, { "type": "string" }] }
      }
    }]
  }'
```

Response:

```json
{
  "type": "error",
  "error": {
    "type": "invalid_request_error",
    "message": "unsupported MCP tool definition at tools[0].inputSchema.anyOf: ... (field: tools[0].inputSchema.anyOf)"
  }
}
```

A missing `name` or missing `inputSchema` is rejected with HTTP `400`.

## Payload size limit

The serialized `tools` array is capped at **1 MiB** (`1048576` bytes). A larger
payload is rejected with HTTP `413` and error type `request_too_large`:

```json
{
  "type": "error",
  "error": {
    "type": "request_too_large",
    "message": "tools payload exceeds the 1048576-byte (1 MiB) limit (field: tools)"
  }
}
```

## Behavior summary

| Scenario | Result |
|----------|--------|
| Valid MCP definition (`tools[].mcp`) | 200; tool mapped to native `name`/`description`/`input_schema`. |
| Unsupported `input_schema` (union / non-object) | 422 `invalid_request_error` naming the field. |
| `tools` payload > 1 MiB | 413 `request_too_large`. |
| Non-MCP tools only | Passed through unchanged. |
