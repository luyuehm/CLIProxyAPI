package modelroutingruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/modelrouting"
	"golang.org/x/net/context"
)

// scopeIDPrefix marks a scope id derived from an API key, so an id read off
// GET /routing/policies is recognizable as "the scope of this key" rather
// than an operator-chosen organization name.
const scopeIDPrefix = "key_"

// scopeIDHashChars is how many hex characters of the SHA-256 digest identify
// the key. 32 hex chars = 128 bits: collision-free for any realistic key
// population, and short enough for an operator to pass as a query parameter.
const scopeIDHashChars = 32

// ScopeIDForKey derives the opaque user-scope id for an authenticated API key.
//
// The raw key is never used as a scope id: scope ids surface in the
// management API responses and in decision logs, and a gateway API key is a
// bearer credential. The digest is deterministic, so the same key always
// resolves to the same scope, and an operator can compute it locally:
//
//	printf %s "$KEY" | shasum -a 256 | cut -c1-32
//
// An empty key yields an empty id (the caller keeps the built-in default).
func ScopeIDForKey(apiKey string) string {
	key := strings.TrimSpace(apiKey)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return scopeIDPrefix + hex.EncodeToString(sum[:])[:scopeIDHashChars]
}

// scopeFromContext resolves the routing scope for the request carried by ctx.
//
// The gateway authenticates at the API-key level only: the access manager
// records the principal (the key) under "userApiKey", and there is no
// organization or tenant identifier anywhere on the request path. A non-empty
// principal therefore maps to the user scope keyed by ScopeIDForKey; a request
// without a principal (auth disabled) maps to the zero scope, which resolves
// to the built-in / configured default strategy.
//
// Not an isolation boundary: the bearer fallback in apiKeyFromGin exists only
// for the auth-disabled / nested-execution cases. Every data-plane route group
// mounts AuthMiddleware, so with auth on, "userApiKey" is always the validated
// principal. A routing scope selects an operator-managed cost policy — it is
// never the thing that decides who may call what (that is
// internal/routeboundary, keyed from the same principal).
func scopeFromContext(ctx context.Context) modelrouting.Scope {
	ginCtx := ginContext(ctx)
	if ginCtx == nil {
		return modelrouting.Scope{}
	}
	return userScopeForAPIKey(apiKeyFromGin(ginCtx))
}

// userScopeForAPIKey builds the user scope for a key, or the zero scope when
// the key is empty.
func userScopeForAPIKey(apiKey string) modelrouting.Scope {
	id := ScopeIDForKey(apiKey)
	if id == "" {
		return modelrouting.Scope{}
	}
	return modelrouting.Scope{Kind: modelrouting.ScopeUser, ID: id}
}

// ginContext recovers the gin context the handlers embed under the "gin" key.
func ginContext(ctx context.Context) *gin.Context {
	if ctx == nil {
		return nil
	}
	ginCtx, _ := ctx.Value("gin").(*gin.Context)
	return ginCtx
}

// apiKeyFromGin resolves the request's API key the way the KEEPER channels
// key their snapshots: the access-manager principal when the auth middleware
// ran, falling back to the bearer material on the request.
//
// The fallback covers the auth-disabled deployment and nested executions that
// lost the principal. It must not be relied on for isolation: when it applies,
// the caller chose the key material, and therefore its own scope.
func apiKeyFromGin(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if value, exists := c.Get("userApiKey"); exists {
		if key, ok := value.(string); ok && strings.TrimSpace(key) != "" {
			return strings.TrimSpace(key)
		}
	}
	return apiKeyFromRequest(c.Request)
}

// apiKeyFromRequest resolves the API key from the standard auth headers the
// access manager checks.
func apiKeyFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	for _, candidate := range []string{
		extractBearer(r.Header.Get("Authorization")),
		r.Header.Get("X-Goog-Api-Key"),
		r.Header.Get("X-Api-Key"),
	} {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			return candidate
		}
	}
	if r.URL != nil {
		if key := strings.TrimSpace(r.URL.Query().Get("key")); key != "" {
			return key
		}
	}
	return ""
}

// extractBearer pulls the token out of an "Authorization: Bearer <token>"
// header, or returns the raw value when there is no Bearer prefix.
func extractBearer(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	const prefix = "bearer "
	if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return header[len(prefix):]
	}
	return header
}
