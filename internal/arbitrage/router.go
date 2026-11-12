package arbitrage

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// sinkModelRouteTargetProvider is pluginapi.ModelRouteTargetProvider. It is
// kept as a package-local constant so the intent ("route through the built-in
// auth/executor path") stays readable at the call site.
const sinkModelRouteTargetProvider = pluginapi.ModelRouteTargetProvider

// RouterHost implements handlers.PluginModelRouterHost on top of the delivered
// decision snapshot: before model-to-provider resolution it answers "which
// Provider serves this model", so execution targets the driven Provider instead
// of the gateway's default pick.
//
// It is deliberately declarative — it never mutates auth state. The reconciler
// owns registering the runtime credentials/executors each decision needs; the
// host only maps a model to an already-prepared Provider key. That separation
// keeps the request path a pure map lookup.
type RouterHost struct {
	store *Store
	now   func() time.Time
	// providerKeyFor maps a decision to the Provider key this gateway can
	// execute. Returning "" means "no usable local credential", which makes the
	// host decline the route and lets default routing proceed.
	providerKeyFor func(Decision) string
}

// NewRouterHost builds a model-router host over a decision store.
func NewRouterHost(store *Store, now func() time.Time, providerKeyFor func(Decision) string) *RouterHost {
	if now == nil {
		now = time.Now
	}
	return &RouterHost{store: store, now: now, providerKeyFor: providerKeyFor}
}

// HasModelRouters reports whether any fresh decision is currently actionable.
// Returning false is the conservative default: with no usable snapshot the
// gateway behaves exactly as an unconfigured build.
func (h *RouterHost) HasModelRouters() bool {
	if h == nil || h.store == nil {
		return false
	}
	return h.store.Usable(h.now())
}

// HasModelRoutersExcept reports the same when a caller excludes a plugin id.
// Decisions are host-side, not plugin routes, so the exclusion does not apply.
func (h *RouterHost) HasModelRoutersExcept(string) bool {
	return h.HasModelRouters()
}

// RouteModel resolves a requested model to a Provider route decision.
func (h *RouterHost) RouteModel(ctx context.Context, req pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
	return h.RouteModelExcept(ctx, req, "")
}

// RouteModelExcept is the plugin-aware variant; the skip id is irrelevant here.
func (h *RouterHost) RouteModelExcept(_ context.Context, req pluginapi.ModelRouteRequest, _ string) (pluginapi.ModelRouteResponse, bool) {
	if h == nil || h.store == nil {
		return pluginapi.ModelRouteResponse{}, false
	}
	model := strings.TrimSpace(req.RequestedModel)
	if model == "" {
		return pluginapi.ModelRouteResponse{}, false
	}
	decision, ok := h.store.Decision(model, h.now())
	if !ok {
		return pluginapi.ModelRouteResponse{}, false
	}
	providerKey := ""
	if h.providerKeyFor != nil {
		providerKey = strings.TrimSpace(h.providerKeyFor(decision))
	}
	if providerKey == "" {
		// The snapshot names a Provider this gateway has no credential for.
		// Decline the route: availability first, default routing resumes.
		return pluginapi.ModelRouteResponse{}, false
	}
	return pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: sinkModelRouteTargetProvider,
		Target:     providerKey,
		Reason:     "arbitrage_" + strings.TrimSpace(decision.Reason),
	}, true
}
