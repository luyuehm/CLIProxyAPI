package arbitrage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/sirupsen/logrus"
)

// CredentialLookup resolves an AuthRef to the local credential material a
// Provider needs. It is supplied by the deployment (see CredentialResolver):
// the arbitration contract only ever carries a *reference name*, never a key.
//
// found=false means the gateway holds no credential for that reference; the
// reconciler then leaves that decision unrouted so requests keep using default
// routing instead of failing against an unusable Provider.
type CredentialLookup func(authRef string) (Credential, bool)

// Credential is the resolved material for one AuthRef.
type Credential struct {
	// APIKey is the upstream bearer key. Never logged, never returned over HTTP.
	APIKey string
	// ExtraHeaders are optional static headers the Provider requires.
	ExtraHeaders map[string]string
}

// RuntimeIDPrefix namespaces every auth record this package owns, so a
// reconciler pass can identify and remove its own rows without touching
// operator-configured credentials or plugin virtual auths.
const RuntimeIDPrefix = "arbitrage-provider-"

// Reconciler keeps the gateway's runtime credentials and model registrations in
// step with the delivered decision snapshot.
//
// For each actionable decision it makes sure a runtime-only OpenAI-compatible
// credential exists whose base_url is the decision's Endpoint and whose key came
// from AuthRef resolution, registers that credential's models so the gateway can
// route to it, and removes rows whose decision disappeared or changed target.
//
// Credential material is kept in an auth record that never touches disk: the
// record carries `runtime_only=true` (so Manager.persist skips it) and no
// Metadata (a second guard in the same code path).
type Reconciler struct {
	manager *coreauth.Manager
	store   *Store
	lookup  CredentialLookup
	// config is passed to the runtime executor so it honours the gateway's proxy
	// and payload rules. May be nil.
	config *config.Config
	// endpoints validates a delivered Endpoint before any credential is attached
	// to it.
	endpoints endpointPolicy

	mu sync.Mutex
	// applied maps our runtime auth ID to the fingerprint of the endpoint+key it
	// currently holds, so an unchanged decision costs no Manager churn.
	applied map[string]string
	// executors tracks Provider keys this reconciler registered an executor for,
	// so a retargeted Provider's executor can be released with its credential.
	executors map[string]struct{}
}

// NewReconciler builds a reconciler. A nil manager or store disables it.
func NewReconciler(manager *coreauth.Manager, store *Store, lookup CredentialLookup) *Reconciler {
	return NewReconcilerWithConfig(manager, store, lookup, nil)
}

// NewReconcilerWithConfig is NewReconciler with an explicit gateway config for
// the runtime executor (proxy settings, payload rules).
func NewReconcilerWithConfig(manager *coreauth.Manager, store *Store, lookup CredentialLookup, cfg *config.Config) *Reconciler {
	return &Reconciler{
		manager:   manager,
		store:     store,
		lookup:    lookup,
		config:    cfg,
		endpoints: defaultEndpointPolicy(),
		applied:   make(map[string]string),
		executors: make(map[string]struct{}),
	}
}

// SetEndpointPolicy overrides the endpoint validation policy. It must be called
// before the first Reconcile.
func (r *Reconciler) SetEndpointPolicy(policy EndpointPolicy) {
	if r == nil {
		return
	}
	r.endpoints = policy.policy()
}

// runtimeAuthID derives a stable auth ID from (model, provider) so repeated
// reconciles update the same row instead of accumulating duplicates.
func runtimeAuthID(model, provider string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(model) + "\x00" + strings.ToLower(strings.TrimSpace(provider))))
	return RuntimeIDPrefix + hex.EncodeToString(sum[:8])
}

// providerKeyFor is the mapping the RouterHost uses: it answers with the
// gateway's executable Provider key for a decision, or "" when no local
// credential is available (so the host declines the route).
//
// The key is the arbitration Provider name itself (not an openai-compatible-*
// alias): the runtime executor is registered under that same name and reports
// it as its Identifier, which is what makes usage_events carry the Provider
// name KEEPER's health aggregation expects.
func (r *Reconciler) providerKeyFor(decision Decision) string {
	if r == nil || r.lookup == nil || r.manager == nil {
		return ""
	}
	if _, ok := r.lookup(decision.AuthRef); !ok {
		return ""
	}
	providerKey := providerKeyForDecision(decision)
	// The pin is gated on a LIVE credential, not on the auth_ref pool alone.
	// A decision can sit in the pool while the reconciler dropped it (refused
	// endpoint, empty key) — and KEEPER's upsert does not validate endpoint
	// shape, so a refused endpoint is reachable in normal operation. Pinning a
	// provider whose credential was never registered would make
	// Manager.Execute return auth_not_found on a single-provider shard,
	// failing the request instead of falling back to default routing. An
	// operator-configured static credential for the same provider still
	// counts: falling back to it is exactly the availability-first behaviour
	// this channel must preserve.
	if !r.manager.HasProviderAuth(providerKey) {
		return ""
	}
	return providerKey
}

// providerKeyForDecision is the single spelling of "which provider key does this
// decision execute under", shared by the reconciler and the router host.
func providerKeyForDecision(decision Decision) string {
	return strings.ToLower(strings.TrimSpace(decision.Provider))
}

// ProviderKeyFor exposes the mapping for the RouterHost.
func (r *Reconciler) ProviderKeyFor(decision Decision) string {
	return r.providerKeyFor(decision)
}

// Reconcile applies the current decision snapshot to the gateway.
//
// It is safe to call repeatedly and from the pull loop. Rows whose decision
// disappeared (or moved to another Provider) are removed, and so are executors
// for Providers no longer targeted. When the snapshot expires the decisions stay
// applied but the RouterHost stops routing through them, so requests fall back to
// default routing either way.
func (r *Reconciler) Reconcile(ctx context.Context) {
	if r == nil || r.manager == nil || r.store == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	decisions := r.store.All()
	desired := make(map[string]Decision, len(decisions))
	for _, decision := range decisions {
		desired[decision.Model] = decision
	}

	// One alias rebuild for the whole batch instead of one per Register/Update.
	// Without this a large snapshot drives N full rebuilds under the manager
	// write lock, which stalls the request path for the duration.
	if ctx == nil {
		ctx = context.Background()
	}
	batchCtx := coreauth.WithDeferredAPIKeyModelAliasRebuild(ctx)
	seen := make(map[string]struct{}, len(desired))
	liveProviders := make(map[string]struct{}, len(desired))
	for model, decision := range desired {
		authID := runtimeAuthID(model, decision.Provider)
		seen[authID] = struct{}{}
		liveProviders[providerKeyForDecision(decision)] = struct{}{}
		r.applyDecision(batchCtx, authID, model, decision)
	}
	r.manager.RefreshAPIKeyModelAlias()

	// Remove rows this package owns whose decision is gone or retargeted.
	for authID := range r.applied {
		if _, keep := seen[authID]; keep {
			continue
		}
		r.manager.Remove(coreauth.WithSkipPersist(context.Background()), authID)
		registry.GetGlobalRegistry().UnregisterClient(authID)
		delete(r.applied, authID)
	}

	// Release executors for Providers no longer targeted, so a retargeted
	// Provider cannot be selected through a stale executor.
	for providerKey := range r.executors {
		if _, live := liveProviders[providerKey]; live {
			continue
		}
		r.manager.UnregisterExecutor(providerKey)
		delete(r.executors, providerKey)
	}
}

// ensureExecutor guarantees a Provider key can actually execute. A Provider that
// is only ever seen in a decision (not in the static config) has no executor, and
// auth selection would refuse to pick its credential. Registering the wrapper
// here makes "decision applied" and "decision executable" the same event.
func (r *Reconciler) ensureExecutor(providerKey string) {
	if providerKey == "" {
		return
	}
	if _, live := r.executors[providerKey]; live {
		return
	}
	if _, exists := r.manager.Executor(providerKey); exists {
		// An operator-configured executor already owns this Provider; do not
		// replace it, only remember that we must not release it on retarget.
		return
	}
	r.manager.RegisterExecutor(WrapProviderExecutor(providerKey, r.config))
	r.executors[providerKey] = struct{}{}
	logger.WithField("provider", providerKey).Info("arbitrage: registered runtime executor for arbitrated provider")
}

// customHeaderAttribute converts a header name into the auth attribute the
// gateway's executor reads. The convention is "header:" + name (see
// internal/util/header_helpers.go); a bare "header_" prefix is inert.
//
// Names that would let a credential override the request's identity or routing
// are refused: Authorization/Host, anything with whitespace or CR/LF (header
// injection), and empty names.
func customHeaderAttribute(name string) (string, bool) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || strings.ContainsAny(trimmed, " \t\r\n:") {
		return "", false
	}
	switch strings.ToLower(trimmed) {
	case "authorization", "host", "content-length", "connection":
		return "", false
	}
	return "header:" + trimmed, true
}

// dropRuntimeAuth removes a runtime credential this reconciler owns, if any.
func (r *Reconciler) dropRuntimeAuth(authID string) {
	if r == nil || r.manager == nil {
		return
	}
	if _, existed := r.applied[authID]; !existed {
		return
	}
	r.manager.Remove(coreauth.WithSkipPersist(context.Background()), authID)
	registry.GetGlobalRegistry().UnregisterClient(authID)
	delete(r.applied, authID)
}

// applyDecision upserts one runtime credential + model registration.
func (r *Reconciler) applyDecision(ctx context.Context, authID, model string, decision Decision) {
	credential, ok := r.lookup(decision.AuthRef)
	if !ok || strings.TrimSpace(credential.APIKey) == "" {
		r.dropRuntimeAuth(authID)
		return
	}
	endpoint := strings.TrimSpace(decision.Endpoint)
	if endpoint == "" {
		r.dropRuntimeAuth(authID)
		return
	}
	// The endpoint decides where the operator's real upstream key is sent, so it
	// is validated before a credential exists for it.
	if err := r.endpoints.validate(endpoint); err != nil {
		logrus.WithError(err).
			WithFields(logrus.Fields{"model": model, "provider": decision.Provider}).
			Warn("arbitrage: refusing delivered endpoint")
		r.dropRuntimeAuth(authID)
		return
	}

	fingerprint := fingerprintFor(endpoint, credential.APIKey, credential.ExtraHeaders)
	if r.applied[authID] == fingerprint {
		// Unchanged target: keep the existing row and its registrations.
		if _, live := r.manager.GetByID(authID); live {
			return
		}
	}

	providerKey := providerKeyForDecision(decision)
	r.ensureExecutor(providerKey)
	attrs := map[string]string{
		"base_url":     endpoint,
		"api_key":      credential.APIKey,
		"provider_key": providerKey,
		// runtime_only keeps Manager.persist from writing this record to disk.
		coreauth.AttributeRuntimeOnly: "true",
		coreauth.AttributeSource:      "arbitrage:" + decision.Provider,
		// NOTE: compat_name is deliberately NOT set. executorKeyFromAuth() turns a
		// non-empty compat_name into a synthetic "openai-compatible-<name>" key,
		// which would break the scheduler/provider-key alignment that makes the
		// driven Provider selectable, and would key usage_events under that
		// synthetic name instead of the raw Provider KEEPER prices and aggregates
		// health against.
	}
	for name, value := range credential.ExtraHeaders {
		attrName, ok := customHeaderAttribute(name)
		if !ok {
			continue
		}
		attrs[attrName] = value
	}

	auth := &coreauth.Auth{
		ID:         authID,
		Provider:   providerKey,
		Label:      decision.Provider,
		Status:     coreauth.StatusActive,
		Attributes: attrs,
		// Metadata stays nil: a second persistence guard, and this credential
		// has no refreshable token state.
	}
	// Publish the models this credential serves BEFORE registering it. Auth
	// selection builds each credential's supported-model set from the registry at
	// Register time; a model registered afterwards leaves the scheduler shard
	// empty and the credential unselectable until some later refresh.
	registry.GetGlobalRegistry().RegisterClient(authID, providerKey, []*registry.ModelInfo{{
		ID:      model,
		Object:  "model",
		OwnedBy: decision.Provider,
	}})

	// SkipPersist is belt-and-braces with runtime_only; both are enforced.
	regCtx := coreauth.WithSkipPersist(ctx)
	if _, live := r.manager.GetByID(authID); live {
		if _, err := r.manager.Update(regCtx, auth); err != nil {
			logrus.WithError(err).WithField("auth_id", authID).Warn("arbitrage: update runtime credential failed")
			return
		}
	} else {
		if _, err := r.manager.Register(regCtx, auth); err != nil {
			logrus.WithError(err).WithField("auth_id", authID).Warn("arbitrage: register runtime credential failed")
			return
		}
	}

	// Refresh once more so an in-place model change (same auth, new model set) is
	// picked up even when the credential row already existed.
	r.manager.RefreshSchedulerEntry(authID)
	r.applied[authID] = fingerprint
}

// Release removes every runtime credential (and executor) this package owns. It
// runs on shutdown so a restart never inherits stale runtime rows.
func (r *Reconciler) Release() {
	if r == nil || r.manager == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.applied))
	for authID := range r.applied {
		ids = append(ids, authID)
	}
	sort.Strings(ids)
	skipCtx := coreauth.WithSkipPersist(context.Background())
	for _, authID := range ids {
		r.manager.Remove(skipCtx, authID)
		registry.GetGlobalRegistry().UnregisterClient(authID)
		delete(r.applied, authID)
	}
	providers := make([]string, 0, len(r.executors))
	for providerKey := range r.executors {
		providers = append(providers, providerKey)
	}
	sort.Strings(providers)
	for _, providerKey := range providers {
		r.manager.UnregisterExecutor(providerKey)
		delete(r.executors, providerKey)
	}
}

// AppliedCount reports how many runtime credentials the reconciler owns.
func (r *Reconciler) AppliedCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.applied)
}

// fingerprintFor hashes the routing-relevant inputs so an unchanged decision
// avoids Manager churn while any endpoint/key/header change forces a rewrite.
func fingerprintFor(endpoint, apiKey string, headers map[string]string) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(strings.TrimSpace(endpoint)))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write([]byte(apiKey))
	_, _ = hasher.Write([]byte{0})
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		_, _ = hasher.Write([]byte(strings.ToLower(name)))
		_, _ = hasher.Write([]byte{'='})
		_, _ = hasher.Write([]byte(headers[name]))
		_, _ = hasher.Write([]byte{0})
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// String renders the reconciler state for diagnostics without leaking keys.
func (r *Reconciler) String() string {
	if r == nil {
		return "arbitrage.Reconciler(nil)"
	}
	return fmt.Sprintf("arbitrage.Reconciler(applied=%d)", r.AppliedCount())
}
