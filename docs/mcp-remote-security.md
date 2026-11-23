# MCP Remote Server Connection Security Baseline (RIC-3138 / MCP-003)

This document is the enterprise security baseline for exposing a Model Context
Protocol (MCP) server remotely — that is, running it on a network-accessible
host rather than as a local subprocess launched by the client.

It answers the question a team faces when it moves an MCP server off a
developer's laptop and in front of production AI applications: **what must be
true about transport, authentication, and authorization before it is safe to
open the endpoint to remote clients?**

The baseline is derived from the normative MCP specification
(`2026-07-28`), its `authorization` and `transports` pages, and the MCP
`Connect to remote MCP Servers` / `Connect to local MCP servers` guides. Where
the protocol is silent (e.g. TLS version), this document applies the
requirements of the standards MCP itself conforms to (OAuth 2.1 / RFC 6750)
and states the enterprise baseline explicitly.

## TL;DR — the mandatory controls

Exposing an MCP server remotely is **not** the same as pointing the same
config at a URL. Local transport trusts the machine; remote transport must
prove identity and permission. Three controls are mandatory and non-negotiable:

| # | Control | Why | Spec basis |
|---|---------|-----|-----------|
| 1 | **Transport encryption: TLS 1.2+ (HTTPS)** | Tokens and tool arguments cross the network; plaintext HTTP leaks both. OAuth 2.1 (which MCP HTTP transport conforms to) mandates TLS for the authorization/token endpoints, and MCP's normative *Security Considerations* require **communication security**. | `authorization` → Security Considerations |
| 2 | **Identity authentication: OAuth 2.1 / Bearer token** | The server must know *who* is calling. STDIO gets credentials from the environment because the process is local; HTTP MUST authenticate every request. | `authorization` → Protocol Requirements, Access Token Usage |
| 3 | **Authorization: least-privilege scopes + per-tool consent** | The server must know *what* the caller may do. MCP's scope-selection strategy and step-up flow exist precisely to prevent excessive permission grants. | `authorization` → Scope Selection Strategy, Step-Up |

> The protocol calls authorization **optional** for MCP. It is not optional for
> a remote deployment. If a server is reachable from a network, all three
> controls are required. This is the single most common mistake when teams
> "just change the path to a URL".

---

## 1. Transport encryption (TLS 1.2+)

### Requirement

- A remote MCP server MUST be served over **HTTPS** (HTTP over TLS) using
  **TLS 1.2 or later**. TLS 1.0 / 1.1 and plaintext HTTP are prohibited.
- The server certificate MUST be issued by a CA the client trusts, for the
  exact hostname the client connects to. Disable certificate verification
  only for short-lived local development, never for production.
- Redirects (if any) MUST stay on HTTPS; a client MUST NOT follow an
  HTTPS→HTTP downgrade.

### Why

MCP uses OAuth 2.1 (draft-ietf-oauth-v2-1-13) for HTTP transports. OAuth 2.1
requires TLS for the authorization server's endpoints, and MCP's normative
security requirements cover "communication security" — meaning the token and
every JSON-RPC message must be protected in transit. Without TLS, a
man-in-the-middle can capture the bearer token and replay it, and can read or
mutate tool arguments.

### Config example — TLS on the server

For a Go/CLIProxyAPI-style server (`config.example.yaml`):

```yaml
tls:
  enabled: true
  cert-file: /etc/mcp/certs/mcp.example.com.crt
  key-file: /etc/mcp/certs/mcp.example.com.key
  min-version: "TLS1.2"   # baseline; prefer TLS1.3 where supported
```

For a reverse-proxy termination (nginx):

```nginx
server {
    listen 443 ssl;
    server_name mcp.example.com;
    ssl_protocols TLSv1.2 TLSv1.3;          # no TLSv1 / TLSv1.1
    ssl_certificate     /etc/ssl/mcp.example.com.crt;
    ssl_certificate_key /etc/ssl/mcp.example.com.key;
    location /mcp { proxy_pass https://127.0.0.1:8317/mcp; }
}
```

### Config example — client side

The client connects with an `https://` URL. There is no local `command` /
`args` / file-path entry at all:

```json
{
  "mcpServers": {
    "enterprise-tools": {
      "url": "https://mcp.example.com/mcp",
      "headers": { "Authorization": "Bearer <access-token>" }
    }
  }
}
```

The gateway's MCP tool definition (`docs/mcp-tool-definitions.md`) accepts the
same HTTPS URL and carries the token out-of-band via the client's own auth:

```json
{
  "type": "mcp",
  "mcp": {
    "server_url": "https://mcp.example.com/mcp",
    "name": "get_weather",
    "inputSchema": { "type": "object" }
  }
}
```

---

## 2. Identity authentication (OAuth 2.1 / Bearer)

### Requirement

- A remote MCP server MUST authenticate every HTTP request using an
  **OAuth 2.1 / RFC 6750 Bearer** access token in the `Authorization` header.
- The token MUST be sent on **every** request. It MUST NOT appear in the URL
  query string.
- The server MUST validate the token's **audience**: it must have been issued
  specifically for this MCP server (RFC 8707 resource indicator / RFC 9728
  protected-resource metadata). A token minted for another resource MUST be
  rejected.
- The client MUST NOT send a token it did not receive from this server's own
  authorization server, and the server MUST NOT accept or forward any other
  token.

### Why

Local stdio MCP "defaults to trust": the client launches a subprocess on your
machine with your credentials in its environment. Remote HTTP has no such
trust — anyone on the network can reach the endpoint, so the server must prove
**who** the caller is before doing anything. OAuth 2.1 with audience binding
is the protocol-defined mechanism; it also gives you revocability and audit.

### Config example — server

The MCP server advertises its authorization server via OAuth 2.0 Protected
Resource Metadata (RFC 9728), discovered by clients:

```http
GET /.well-known/oauth-protected-resource HTTP/1.1
Host: mcp.example.com
```

```json
{
  "resource": "https://mcp.example.com/mcp",
  "authorization_servers": ["https://auth.example.com"],
  "scopes_supported": ["files:read", "files:write"]
}
```

### Config example — client request (every request carries the header)

```http
POST /mcp HTTP/1.1
Host: mcp.example.com
Authorization: Bearer eyJhbGciOiJIUzI1NiIs...
Content-Type: application/json
Accept: application/json, text/event-stream
MCP-Protocol-Version: 2026-07-28

{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}
```

> **Anti-pattern:** placing the token in the query string
> (`https://mcp.example.com/mcp?access_token=...`). The MCP spec explicitly
> forbids it — tokens leak into proxy logs, history, and referrers. Always use
> the `Authorization` header.

---

## 3. Authorization (least-privilege scope + per-tool consent)

### Requirement

- The client MUST request only the **scopes necessary** for its intended
  operations (least privilege). When the server challenges with a `scope`
  parameter in `WWW-Authenticate`, the client MUST treat that set as
  authoritative for the current operation.
- The server SHOULD advertise the **minimal** scope set in `scopes_supported`;
  additional scopes are obtained incrementally through the step-up flow, never
  granted wholesale up front.
- Access to individual tools MUST be controllable by the client (per-tool
  enable/disable) and reviewed against the least-privilege principle.
- On scope insufficiency, the server responds `403 Forbidden` with
  `error="insufficient_scope"` and the required scopes; the client SHOULD
  re-authorize with the union of granted + required scopes, and MUST bound its
  retries.

### Why

MCP's whole scope model exists to stop a client from asking for everything. A
bearer token with broad scopes is a standing credential: if it leaks, the
attacker inherits every permission. Least-privilege scoping plus per-tool
consent keeps the blast radius of any single token to the minimum.

### Config example — scope challenge & step-up

Server responds to an under-scoped call:

```http
HTTP/1.1 403 Forbidden
WWW-Authenticate: Bearer error="insufficient_scope", scope="files:write", \
  resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource", \
  error_description="File write permission required for this operation"
```

Client re-authorizes with the union of its current scopes and the challenged
set (`files:read files:write`), then retries — bounded, never in a loop.

### Config example — per-tool consent

In the client connector settings, or in the gateway tool declaration, restrict
to the tools actually needed:

```json
{
  "mcp": {
    "server_url": "https://mcp.example.com/mcp",
    "capabilities": {
      "required": ["strict_tool_use"],
      "optional": ["resource_templates"]
    }
  }
}
```

---

## Local vs remote — configuration difference table

This is the table a developer needs when they are tempted to "reuse" a local
config for a remote server. The two modes are **not** interchangeable.

| Dimension | Local (stdio) | Remote (Streamable HTTP) |
|-----------|---------------|--------------------------|
| **Transport** | Client-launched subprocess; newline-delimited JSON-RPC over stdin/stdout | HTTP POST to a single MCP endpoint; reply is JSON or a request-scoped SSE stream |
| **URL / config key** | `command` + `args` (e.g. `npx -y @modelcontextprotocol/server-filesystem /Users/me/Desktop`); **no URL** | `url` — MUST be `https://` (TLS). Example: `https://mcp.example.com/mcp` |
| **Credentials** | From the **environment** (`env` block / process env). No auth round-trip. | **OAuth 2.1 Bearer** token in `Authorization` header on **every** request; obtained via authorization-server discovery + PKCE. |
| **Scope / permission** | OS user permissions of the launching process; path allowlist in `args`. | Least-privilege **OAuth scopes** + per-tool consent; scope challenge / step-up flow. |
| **Timeout defaults** | Bounded by process lifetime; the client does not own a network hop. | The client MUST set a **request timeout**; streaming/SSE responses are long-lived, so the timeout must apply to the initial response, not the stream. Reverse proxies need matching `proxy_read_timeout`. |
| **Default trust** | **Trusted** — runs locally with your credentials. | **Untrusted** — must prove identity (auth) and permission (scope). |
| **Boundary on no-auth** | N/A — the process either starts or fails to start. | `401 Unauthorized` with a `WWW-Authenticate: Bearer` challenge. See below. |
| **Security posture** | Localhost-only by convention; bind to `127.0.0.1`. | MUST validate `Origin` (DNS-rebinding defense), SHOULD authenticate, SHOULD bind localhost when running locally. |

> **The trap this table exists to prevent:** copying the local
> `command`/`args` entry and adding a URL, or pasting a bearer token into a
> query string, or assuming "it's a server, it'll just work". Every row above
> is a place where local semantics silently differ from remote semantics.

---

## Boundary case — unauthenticated request to a remote server

A remote MCP server that requires authentication and receives a request
without a valid bearer token **MUST** respond `401 Unauthorized` (invalid or
expired token), per OAuth 2.1 / RFC 6750 and MCP's error handling. This is
expected behavior, not a misconfiguration.

### Response you will see

```http
HTTP/1.1 401 Unauthorized
WWW-Authenticate: Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource", \
  scope="files:read"
Content-Type: application/json

{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"Unauthorized: bearer token required"}}
```

### How to read the error

1. **Status `401`** — authentication failed. `WWW-Authenticate: Bearer`
   tells the client the token is missing, invalid, expired, or not issued for
   this resource (audience mismatch). A `403` instead means authentication
   succeeded but the scope is insufficient.
2. **`resource_metadata`** — the URL of the server's protected-resource
   metadata document. Fetch it to discover the authorization server and the
   `scopes_supported` set.
3. **`scope`** — the scopes the server requires for this operation. The client
   SHOULD use this set to seed its authorization request (least privilege), not
   request everything.
4. **JSON-RPC body** — the `error` object carries the machine-readable
   `code` and a human-readable `message`. Log the code, not just the message.

### Client-side reading (Node SDK example)

```js
try {
  await client.request({ method: "tools/list" });
} catch (err) {
  // err.code === -32001 for the gateway's auth error, or the HTTP status
  // surfaces on the transport error. Distinguish:
  //   401  -> re-authenticate (missing/expired/invalid/audience token)
  //   403  -> scope upgrade via step-up flow (insufficient_scope)
  if (err.status === 401) await client.refreshToken();
  else if (err.status === 403) await client.requestStepUp(err.scope);
  else throw err;
}
```

### Checklist — reading a 401 correctly

- [ ] Did you get `401` (not `403`)? 401 = identity problem; 403 = permission problem.
- [ ] Is the token present in the `Authorization` header (not the query string)?
- [ ] Is the token unexpired and **not revoked**?
- [ ] Is the token's `aud`/resource the exact MCP server URL you are calling?
- [ ] Did you refresh from the **same** authorization server the MCP server advertises?

---

## Verification (how to prove the baseline holds)

Run these checks before declaring a remote MCP deployment production-ready.

| Check | Command / probe | Pass condition |
|-------|-----------------|----------------|
| TLS on | `openssl s_client -connect mcp.example.com:443 -tls1_2` | Handshake succeeds; cert hostname matches; no `-tls1`/`-tls1_1` fallback |
| TLS 1.1 blocked | `openssl s_client -connect mcp.example.com:443 -tls1_1` | **Fails** (server refuses) |
| Auth enforced | `curl -s -o /dev/null -w '%{http_code}' https://mcp.example.com/mcp` | `401` (no token → rejected) |
| Auth accepted | `curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -X POST https://mcp.example.com/mcp -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'` | `200`/`202` with a `result` (not an auth error) |
| Audience binding | Present a token minted for `https://other.example.com/mcp` | `401` (rejected for wrong resource) |
| Scope minimized | Call a tool requiring `files:write` with a `files:read`-only token | `403` + `error="insufficient_scope"` |
| Origin validation | `curl -s -o /dev/null -w '%{http_code}' -H 'Origin: https://evil.example.com' https://mcp.example.com/mcp` | `403` (invalid Origin rejected) |

---

## References

- MCP spec `2026-07-28`, `authorization` — OAuth 2.1 flow, Bearer usage, scope
  selection strategy, step-up flow, error handling, security considerations.
- MCP spec `2026-07-28`, `transports` / `transports/streamable-http` — Origin
  validation, localhost binding, authentication requirement, TLS/HTTPS
  endpoint.
- MCP `Connect to remote MCP Servers` — remote connection flow, Custom
  Connectors, per-tool permission configuration.
- MCP `Connect to local MCP servers` — local stdio config (`command`/`args`,
  `env`, OS-user permissions) for the comparison table.
- RFC 6750 (Bearer), RFC 8707 (Resource Indicators), RFC 9728 (Protected
  Resource Metadata), OAuth 2.1 draft (draft-ietf-oauth-v2-1-13).

---

## Out of scope

- Implementing a full identity-proxy service (that is server functionality,
  not a documentation baseline).
- Regulatory certification flows (SOC 2, etc.).
- OAuth 2.0 / identity-provider deep detail — the baseline only recommends the
  pattern; concrete provider wiring links to the provider's official docs.
