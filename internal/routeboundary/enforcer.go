package routeboundary

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Error code and response headers carried by every boundary verdict, pinned
// by the RIC-2532 shared contract.
const (
	// ErrorType is the OpenAI-style error code and type for boundary
	// violations.
	ErrorType = "route_boundary_violation"

	// HeaderBoundary is the single X-OpenRouter-Route-Boundary wire value,
	// which serves both directions of the contract: as the per-request
	// override header (admission) and as the per-request outcome header
	// stamped on the response ("allowed" / "override"). Absent for
	// unconstrained keys and when the channel is off.
	HeaderBoundary = "X-OpenRouter-Route-Boundary"
	// HeaderPolicyApplied marks whether the request was constrained by a
	// delivered boundary ("true"/"false"). Absent when the channel is off
	// or the snapshot is unverified.
	HeaderPolicyApplied = "X-OpenRouter-Policy-Applied"
	// HeaderExplicitProvider is the client's explicit provider selection; an
	// out-of-boundary value is rejected with 403 route_boundary_violation.
	HeaderExplicitProvider = "X-OpenRouter-Provider"

	// outcomeAllowed / outcomeOverride are the HeaderBoundary outcome values.
	outcomeAllowed  = "allowed"
	outcomeOverride = "override"

	// overrideCtxKey is the gin-context key the admission middleware sets
	// when it grants an override. The execution gate only pins the pool on
	// this value — never on the raw request header — so a nested
	// plugin-originated execution that bypassed the admission gate cannot
	// self-grant an override.
	overrideCtxKey = "routeBoundaryOverride"

	// constrainedMarkerKey is stamped on the request's gin context when the
	// gate admits a boundary-constrained execution (the allowed path). It
	// tells the post-execution translation which failures are "the
	// boundary's pool could not be served" rather than an unrelated
	// gateway condition.
	constrainedMarkerKey = "routeBoundaryConstrained"
)

// AvailabilityProbe decides whether a provider can serve a boundary-narrowed
// request. It is the seam between the enforcer and the credential store: the
// default implementation probes the auth manager's live credentials.
type AvailabilityProbe interface {
	// HasManagedCredential reports whether the provider has at least one
	// managed (non-BYOK) usable credential. A boundary constrains managed
	// credentials only: when no provider inside the boundary has a managed
	// credential, the request fails instead of silently escaping the
	// boundary through a user-supplied key (the BYOK rabbit hole).
	HasManagedCredential(provider string) bool
}

// managerProbe implements AvailabilityProbe over a live auth manager.
type managerProbe struct{ m *coreauth.Manager }

// NewManagerProbe builds the default probe. A nil manager yields a nil probe:
// without credential introspection the enforcer narrows pools by the
// boundary alone and leaves availability to the executor's natural failures
// (fail-open).
func NewManagerProbe(m *coreauth.Manager) AvailabilityProbe {
	if m == nil {
		return nil
	}
	return managerProbe{m: m}
}

// HasManagedCredential reports whether the provider has at least one managed
// usable credential.
func (p managerProbe) HasManagedCredential(provider string) bool {
	if p.m == nil {
		return false
	}
	key := normalizeProvider(provider)
	if key == "" {
		return false
	}
	for _, auth := range p.m.List() {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		if normalizeProvider(auth.Provider) != key {
			continue
		}
		if isManagedCredential(auth) {
			return true
		}
	}
	return false
}

// isManagedCredential classifies a credential as managed (KEEPER-provisioned)
// or BYOK (operator/user-supplied). Managed = delivered through a KEEPER
// storage backend (objectstore/postgres/git) or the runtime-only arbitrage
// channel (memory source with the runtime_only marker). Local config keys and
// auth-dir files are BYOK: they may target arbitrary non-compliant
// endpoints, which is exactly what the boundary must never route to.
func isManagedCredential(auth *coreauth.Auth) bool {
	switch auth.AuthSourceKind() {
	case coreauth.AuthSourceObjectStore, coreauth.AuthSourcePostgres, coreauth.AuthSourceGit, coreauth.AuthSourceMemory:
		return true
	default:
		return false
	}
}

// Enforcer applies the delivered per-key route boundaries to one request's
// provider pool. It is installed on the data-plane handlers through the
// pre-reserved router-configurator hook (the SetModelRouterHost precedent):
// no upstream handler, translator or route file is modified.
//
// Semantics (contract fixed in RIC-2532):
//   - channel off / store unverified / caller key unresolvable / no
//     delivered boundary → pass through, no verdict (identical to an
//     unconfigured build);
//   - delivered boundary → pool := model pool ∩ allowedProviders; when no
//     provider inside the boundary has a usable managed credential the
//     request is 503 route_boundary_violation — never a degrade to a
//     provider outside the list;
//   - an explicit out-of-boundary provider (?alt=<p>, X-OpenRouter-Provider,
//     forced provider, router-selected provider) is 403, naming <p>;
//   - strict=true pins execution to the top-priority allowed provider: when
//     it is unavailable the request fails instead of walking the list;
//   - a granted override (admission middleware set overrideCtxKey after
//     checking overrideAllowed) pins execution to exactly that provider,
//     out of the boundary;
//   - on the allowed path the gateway stamps X-OpenRouter-Route-Boundary:
//     allowed on the response writer.
type Enforcer struct {
	store *Store
	probe AvailabilityProbe
}

// EnforcerOptions configures an Enforcer.
type EnforcerOptions struct {
	// Store supplies the delivered per-key boundaries. Nil disables the
	// enforcer entirely (ApplyBoundary is a pass-through).
	Store *Store
	// Probe supplies credential-availability decisions (strict + BYOK).
	// Nil degrades to pure boundary narrowing: the pool is still narrowed
	// and explicit out-of-boundary providers are still rejected.
	Probe AvailabilityProbe
}

// NewEnforcer builds an enforcer. A nil store leaves ApplyBoundary inert.
func NewEnforcer(opts EnforcerOptions) *Enforcer {
	if opts.Store == nil {
		return nil
	}
	return &Enforcer{store: opts.Store, probe: opts.Probe}
}

// ApplyBoundary enforces the caller's delivered route boundary on the
// execution provider pool. providers is the pool after entry-protocol
// adjustment; modelName the normalized upstream model; forced and routed the
// internal explicit provider selections (executor option and model router).
//
// It returns the pool to execute and the gateway violation error to surface
// before any upstream request is made (nil on success).
func (e *Enforcer) ApplyBoundary(ctx context.Context, providers []string, modelName, forced, routed string) ([]string, *interfaces.ErrorMessage) {
	if e == nil || e.store == nil {
		return providers, nil
	}
	// Unverified store: a KEEPER that has never delivered a snapshot must
	// not constrain traffic at boot — pass through, stamp nothing.
	if !e.store.Verified() {
		return providers, nil
	}
	ginCtx := ginContext(ctx)
	key := apiKeyFromGin(ginCtx)
	if key == "" {
		// Nested plugin-originated execution without caller material: the
		// boundary cannot be looked up, so the request passes through.
		return providers, nil
	}
	boundary, ok := e.store.Get(key)
	if !ok || !boundary.Constrains() {
		return providers, nil
	}

	// Granted out-of-boundary pass (overrideAllowed=true, decided and
	// audited at admission). Pins execution to exactly this provider.
	if override := grantedOverride(ginCtx); override != "" {
		if ginCtx != nil && ginCtx.Writer != nil {
			ginCtx.Writer.Header().Set(HeaderBoundary, outcomeOverride)
		}
		return []string{normalizeProvider(override)}, nil
	}

	// Explicit provider selections must stay inside the boundary (403,
	// naming the provider). The admission middleware already rejects the
	// client-visible ones (alt/header) on the plain HTTP path; this
	// re-checks every source so nested executions stay safe.
	alt := altProviderFromContext(ctx)
	explicit := explicitHeaderFromContext(ctx)
	for _, candidate := range []string{forced, routed, alt, explicit} {
		if candidate == "" {
			continue
		}
		if !boundary.Allows(candidate) {
			logger.WithFields(logrus.Fields{
				"provider": candidate,
				"model":    modelName,
			}).Debug("routeboundary: explicit provider outside boundary rejected")
			return nil, e.violation(http.StatusForbidden,
				candidate+" is not within the configured route boundary")
		}
	}
	// The first in-boundary explicit selection pins the pool
	// deterministically: the client asked for that provider, so it is
	// executed (and, when it lacks a managed credential, failed) instead
	// of degrading to the rest of the list.
	if candidate := firstNonEmpty(forced, routed, alt, explicit); candidate != "" {
		if !e.providerUsable(candidate) {
			return nil, e.noProviderViolation(modelName)
		}
		e.stampAllowed(ginCtx)
		return []string{normalizeProvider(candidate)}, nil
	}

	// Automatic path: narrow the pool to the boundary.
	pool := narrowPool(providers, boundary)
	if boundary.Strict {
		// Strict: pin to the top-priority allowed provider. When it
		// cannot serve the request the request fails — the rest of the
		// list is deliberately not tried.
		preferred := boundary.AllowedProviders[0]
		if !containsNormalized(pool, preferred) || !e.providerUsable(preferred) {
			return nil, e.noProviderViolation(modelName)
		}
		pool = []string{preferred}
	}
	// Managed-availability filter: scenario 2 and the BYOK rabbit hole
	// land here. When no provider inside the boundary has a usable managed
	// credential, fail with route_boundary_violation instead of degrading
	// — no upstream request is ever sent outside the pool.
	usable := make([]string, 0, len(pool))
	for _, provider := range pool {
		if e.providerUsable(provider) {
			usable = append(usable, provider)
		}
	}
	if len(usable) == 0 {
		logger.WithField("model", modelName).Debug("routeboundary: no provider within boundary is usable, failing request")
		return nil, e.noProviderViolation(modelName)
	}
	e.stampAllowed(ginCtx)
	return usable, nil
}

// providerUsable reports whether the provider can serve inside a boundary:
// a usable managed credential exists for it. Without a probe the question
// cannot be answered — pass (fail-open) and let the executor's natural
// credential failures surface instead.
func (e *Enforcer) providerUsable(provider string) bool {
	if e.probe == nil {
		return true
	}
	return e.probe.HasManagedCredential(provider)
}

// stampAllowed marks a successful in-boundary execution on the response
// writer and records the constraint on the gin context so the post-execution
// translation knows this request ran inside a boundary. Streaming responses
// commit headers after the first chunk, so the value is set before the
// handler delivers anything and rides along.
func (e *Enforcer) stampAllowed(ginCtx *gin.Context) {
	if ginCtx == nil {
		return
	}
	if ginCtx.Writer != nil {
		ginCtx.Writer.Header().Set(HeaderBoundary, outcomeAllowed)
	}
	ginCtx.Set(constrainedMarkerKey, true)
}

// TranslateExecutionError rewrites a post-execution credential-availability
// failure into the contract's 503 route_boundary_violation when the request
// was boundary-constrained and its pool could not be served (scenario 2:
// every in-boundary provider unavailable, e.g. all cooling down). It
// returns nil for unconstrained requests or for failures that are not
// credential-availability failures, in which case the caller keeps the
// original error.
func (e *Enforcer) TranslateExecutionError(ctx context.Context, execErr error, modelName string) *interfaces.ErrorMessage {
	if e == nil || execErr == nil {
		return nil
	}
	if !constrainedMarkerSet(ginContext(ctx)) {
		return nil
	}
	if !isCredentialAvailabilityError(execErr) {
		return nil
	}
	logger.WithField("model", modelName).Debug("routeboundary: in-boundary pool exhausted at execution, failing with route_boundary_violation")
	return e.noProviderViolation(modelName)
}

// constrainedMarkerSet reports whether this request was admitted to a
// boundary-constrained execution by ApplyBoundary.
func constrainedMarkerSet(ginCtx *gin.Context) bool {
	if ginCtx == nil {
		return false
	}
	value, exists := ginCtx.Get(constrainedMarkerKey)
	if !exists {
		return false
	}
	return value == true
}

// isCredentialAvailabilityError reports whether a post-execution error means
// "no usable credential for the pool" (auth manager selection failures and
// model-level cooldowns) rather than a real upstream response.
func isCredentialAvailabilityError(err error) bool {
	if err == nil {
		return false
	}
	// The scheduler's model-cooldown error (all credentials for the model
	// cooling down) is an unexported type detected through its marker, the
	// same pattern the handlers' own error enrichment uses.
	type modelCooldownMarker interface{ IsModelCooldown() bool }
	var marker modelCooldownMarker
	if errors.As(err, &marker) && marker != nil {
		return true
	}
	var authErr *coreauth.Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	code := strings.TrimSpace(authErr.Code)
	return code == "auth_not_found" || code == "auth_unavailable"
}

// apiKeyFromGin resolves the request's API key the way the KEEPER channel
// keys its snapshots: the access-manager principal when the route auth
// middleware ran, falling back to the bearer material on the request (the
// admission position, before auth, and nested executions that lost the
// principal). Because the admission middleware is mounted globally before
// the /v1 route group's AuthMiddleware, the "userApiKey" value is not yet
// populated on the request path — the bearer fallback matches the
// access-manager's key extraction so the delivered KEEPER keys line up with
// what the credential provider authenticates.
func apiKeyFromGin(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if value, exists := c.Get("userApiKey"); exists {
		if key, ok := value.(string); ok && key != "" {
			return key
		}
	}
	return bearerFromRequest(c.Request)
}

// ginContext recovers the embedded gin context from an execution context
// (handlers.GetContextWithCancel embeds it under the "gin" key).
func ginContext(ctx context.Context) *gin.Context {
	if ctx == nil {
		return nil
	}
	ginCtx, _ := ctx.Value("gin").(*gin.Context)
	return ginCtx
}

// requestFromContext recovers the embedded gin request from an execution
// context.
func requestFromContext(ctx context.Context) *http.Request {
	if ctx == nil {
		return nil
	}
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil {
		return ginCtx.Request
	}
	return nil
}

// bearerFromRequest resolves the API key from the standard auth headers the
// access manager checks: Authorization Bearer, X-Goog-Api-Key, X-Api-Key,
// and the ?key= query parameter. Mirrors the policies channel so the KEEPER
// boundary keys line up with what the access manager authenticates.
func bearerFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	for _, candidate := range []string{
		extractBearer(r.Header.Get("Authorization")),
		r.Header.Get("X-Goog-Api-Key"),
		r.Header.Get("X-Api-Key"),
	} {
		if candidate != "" {
			return candidate
		}
	}
	if r.URL != nil {
		if key := r.URL.Query().Get("key"); key != "" {
			return key
		}
	}
	return ""
}

// extractBearer pulls the token out of an "Authorization: Bearer <token>"
// header, or returns the raw value when there is no Bearer prefix (matching
// the access-manager behaviour for non-standard auth material).
func extractBearer(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 {
		return header
	}
	if !strings.EqualFold(parts[0], "bearer") {
		return header
	}
	return strings.TrimSpace(parts[1])
}

// grantedOverride returns the admission-granted out-of-boundary provider,
// set on the gin context by the middleware after it verified
// overrideAllowed.
func grantedOverride(ginCtx *gin.Context) string {
	if ginCtx == nil {
		return ""
	}
	value, exists := ginCtx.Get(overrideCtxKey)
	if !exists {
		return ""
	}
	provider, _ := value.(string)
	return strings.TrimSpace(provider)
}

// GrantOverride records an admission-granted out-of-boundary provider on
// the request's gin context. Only the admission middleware grants through
// this path (after checking the key's override permission); the execution
// gate pins the pool to the granted provider and to nothing else.
func GrantOverride(c *gin.Context, provider string) {
	if c == nil {
		return
	}
	c.Set(overrideCtxKey, strings.TrimSpace(provider))
}

// explicitHeaderFromContext returns the X-OpenRouter-Provider selection when
// present.
func explicitHeaderFromContext(ctx context.Context) string {
	r := requestFromContext(ctx)
	if r == nil {
		return ""
	}
	return strings.TrimSpace(r.Header.Get(HeaderExplicitProvider))
}

// altProviderFromContext returns the ?alt= value when it names an explicit
// provider selection. Transport format markers the gateway itself passes
// ("sse", "responses/compact", and the Gemini alt= values) are not provider
// selections and are ignored.
func altProviderFromContext(ctx context.Context) string {
	r := requestFromContext(ctx)
	if r == nil || r.URL == nil {
		return ""
	}
	alt := strings.TrimSpace(r.URL.Query().Get("alt"))
	if alt == "" {
		alt = strings.TrimSpace(r.URL.Query().Get("$alt"))
	}
	if alt == "" {
		return ""
	}
	if isTransportAlt(alt) {
		return ""
	}
	return alt
}

// isTransportAlt reports whether alt is a transport/format marker the
// gateway itself passes to executors rather than a provider selection.
func isTransportAlt(alt string) bool {
	if alt == "" {
		return true
	}
	_, ok := transportAltValues[strings.ToLower(alt)]
	return ok
}

// narrowPool intersects the execution pool with the boundary, preserving
// the pool's preference order.
func narrowPool(providers []string, boundary Boundary) []string {
	out := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		key := normalizeProvider(provider)
		if key == "" || !boundary.Allows(key) {
			continue
		}
		if _, done := seen[key]; done {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

func containsNormalized(providers []string, provider string) bool {
	key := normalizeProvider(provider)
	for _, candidate := range providers {
		if normalizeProvider(candidate) == key {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// violation builds the contract-mandated boundary error. The JSON body is a
// DirectResponse so WriteErrorResponse passes it verbatim, keeping code and
// type exactly as the shared contract pins them; the headers ride the same
// path and mark the request as boundary-constrained.
func (e *Enforcer) violation(status int, message string) *interfaces.ErrorMessage {
	headers := make(http.Header)
	headers.Set(HeaderPolicyApplied, "true")
	return &interfaces.ErrorMessage{
		StatusCode:     status,
		DirectResponse: true,
		Body:           violationBody(message),
		Headers:        headers,
		Error:          errors.New("route boundary violation: " + message),
	}
}

// noProviderViolation is the scenario-2 verdict: nothing inside the
// boundary can serve the model (strict miss, empty intersection, or no
// managed credential anywhere in the pool).
func (e *Enforcer) noProviderViolation(modelName string) *interfaces.ErrorMessage {
	return e.violation(http.StatusServiceUnavailable,
		"no provider within route boundary is available for model "+modelName)
}

// violationErrorMap renders the contract-pinned violation body
// {"error":{"code","type","message"}} as the map gin.AbortWithStatusJSON
// and the enforcer's DirectResponse path both render: one source for the
// shared contract, so the admission 403 and the execution 503 can never
// drift apart.
func violationErrorMap(message string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"code":    ErrorType,
			"type":    ErrorType,
			"message": message,
		},
	}
}

// violationBody renders the violation body as a DirectResponse payload. The
// message is gateway-built (model/provider names are client-supplied but
// the encoder escapes them), so this cannot realistically fail; keep a safe
// literal fallback regardless.
func violationBody(message string) []byte {
	out, err := json.Marshal(violationErrorMap(message))
	if err != nil {
		out = []byte(`{"error":{"code":"` + ErrorType + `","type":"` + ErrorType + `","message":""}}`)
	}
	return out
}
