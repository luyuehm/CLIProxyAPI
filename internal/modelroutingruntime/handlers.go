package modelroutingruntime

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/modelrouting"
)

// Errors returned by the strategy-object API. They are management-plane
// codes, distinct from the data-plane route_rejected codes.
const (
	// errCodeInvalidScope is a malformed or missing scope selector.
	errCodeInvalidScope = "invalid_scope"
	// errCodeInvalidStrategy is a strategy document that failed validation.
	errCodeInvalidStrategy = "invalid_strategy"
	// errCodeNoteRequired is a POST without the audit change note.
	errCodeNoteRequired = "note_required"
	// errCodeInvalidBody is a body that is not a JSON object.
	errCodeInvalidBody = "invalid_body"
	// errCodeWriteFailed is a version append that failed for any other reason.
	errCodeWriteFailed = "write_failed"
	// errCodePersistenceFailed is a version that is in force in memory but
	// could not be written to the durable state file.
	errCodePersistenceFailed = "persistence_failed"
)

// Request-size bounds for POST /routing/policies. A strategy plus a change
// note is small; the caps stop one management call from bloating the response,
// the authoritative state file, and every later read.
const (
	// maxPolicyRequestBody is the largest accepted POST body.
	maxPolicyRequestBody = 64 << 10 // 64 KiB
	// maxPolicyNoteLength is the largest accepted change note, in bytes.
	maxPolicyNoteLength = 4 << 10 // 4 KiB
)

// scopeResponse is the wire form of a routing scope. The zero scope (the
// built-in / configured default view) is reported as kind "default".
type scopeResponse struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
}

// effectiveResponse is the wire form of the strategy in force.
type effectiveResponse struct {
	Strategy modelrouting.Strategy `json:"strategy"`
	Found    bool                  `json:"found"`
	Reason   string                `json:"reason"`
	Version  int64                 `json:"version"`
}

// priceStatusResponse is the wire form of the price cache status.
type priceStatusResponse struct {
	Models   int    `json:"models"`
	SyncedAt string `json:"synced_at,omitempty"`
	Stale    bool   `json:"stale"`
}

// policyUpdateRequest is the POST /routing/policies body. The strategy is kept
// as raw JSON so it goes through modelrouting.StrategyFromJSON, which reports
// the offending field and rejects an empty document.
type policyUpdateRequest struct {
	Scope    *scopeRequest   `json:"scope,omitempty"`
	Strategy json.RawMessage `json:"strategy"`
	Note     string          `json:"note"`
}

type scopeRequest struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// GetPolicies serves GET /v0/management/routing/policies: the effective view,
// the immutable version history and the price snapshot status for one scope.
//
// Without scope query parameters it reports the default view — the strategy a
// caller with no configured version gets — which is what an operator checks
// after setting model-routing.default-strategy.
func (r *Runtime) GetPolicies(c *gin.Context) {
	if r == nil || r.core == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":   "model_routing_disabled",
			"message": "the model routing channel is not enabled",
		})
		return
	}

	scope, kind, ok := scopeFromQuery(c)
	if !ok {
		return
	}
	r.writePolicies(c, http.StatusOK, scope, kind, 0)
}

// PostPolicies serves POST /v0/management/routing/policies: appends one
// immutable strategy version to a scope and returns the new version together
// with the resulting effective view.
//
// The history is append-only. A rollback is a new version carrying the old
// field values, never an edit of a stored record.
func (r *Runtime) PostPolicies(c *gin.Context) {
	if r == nil || r.core == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":   "model_routing_disabled",
			"message": "the model routing channel is not enabled",
		})
		return
	}

	var body policyUpdateRequest
	// Bound the body: the request is small by nature (a strategy plus a note)
	// and every accepted version is re-serialized into the response and the
	// state file, so an unbounded note grows both without limit.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPolicyRequestBody)
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   errCodeInvalidBody,
			"message": "request body must be a JSON object of at most " + strconv.Itoa(maxPolicyRequestBody) + " bytes",
		})
		return
	}

	// Audit requirement: every strategy change carries a change note.
	note := strings.TrimSpace(body.Note)
	if note == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   errCodeNoteRequired,
			"message": "note is required: every strategy change records why it was made",
		})
		return
	}
	if len(note) > maxPolicyNoteLength {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   errCodeInvalidBody,
			"message": "note must be at most " + strconv.Itoa(maxPolicyNoteLength) + " bytes",
		})
		return
	}

	scope, kind, ok := scopeFromRequest(body.Scope, c)
	if !ok {
		return
	}
	if scope.Kind == "" || scope.ID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": errCodeInvalidScope,
			"message": "scope is required: pass scope.kind (user|org) and scope.id, " +
				"or scope_kind/scope_id query parameters",
		})
		return
	}

	strategy, err := modelrouting.StrategyFromJSON(body.Strategy)
	if err != nil {
		// Validation errors name the offending field; the version history is
		// untouched.
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   errCodeInvalidStrategy,
			"message": err.Error(),
		})
		return
	}

	version, err := r.core.Versions.Update(scope, modelrouting.Record{Strategy: strategy, Note: note})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   errCodeWriteFailed,
			"message": err.Error(),
		})
		return
	}
	r.trackScope(scope)

	// Durability: the in-memory append already happened (the history is
	// append-only, so it cannot be undone). A failed write is reported as
	// 202 Accepted with persisted:false rather than 5xx: the version IS live,
	// and a retrying client would otherwise append a duplicate. A state file
	// that failed to load is written to a .recovered sibling instead of being
	// clobbered; the response says where it went.
	persistedPath, persistErr := r.persist()
	if persistErr != nil {
		logger.WithError(persistErr).WithField("scope_id", scope.ID).
			Error("modelrouting: strategy version applied in memory but NOT persisted")
		c.JSON(http.StatusAccepted, gin.H{
			"error":     errCodePersistenceFailed,
			"message":   "version " + formatVersion(version.Version) + " is in force but was not persisted: " + persistErr.Error(),
			"version":   version.Version,
			"persisted": false,
		})
		return
	}
	if persistedPath != r.statePath && persistedPath != "" {
		logger.WithField("path", persistedPath).WithField("scope_id", scope.ID).
			Warn("modelrouting: the state file did not load at boot; this version was written to a .recovered file, not the original")
	}

	r.writePolicies(c, http.StatusOK, scope, kind, version.Version)
}

// writePolicies renders the shared GET/POST response. newVersion is 0 for GET.
func (r *Runtime) writePolicies(c *gin.Context, status int, scope modelrouting.Scope, kind string, newVersion int64) {
	view := r.effective(scope)
	versions := r.core.Management.ListVersions(scope)
	if versions == nil {
		versions = []modelrouting.Version{}
	}
	prices := r.core.Management.PriceStatus()

	payload := gin.H{
		"enabled":   true,
		"scope":     renderScope(scope, kind),
		"effective": renderEffective(view),
		"versions":  versions,
		"price_status": priceStatusResponse{
			Models:   prices.Models,
			SyncedAt: formatTime(prices.SyncedAt),
			Stale:    prices.Stale,
		},
	}
	if newVersion > 0 {
		payload["version"] = newVersion
	}
	c.JSON(status, payload)
}

func renderScope(scope modelrouting.Scope, kind string) scopeResponse {
	if scope.Kind == "" || scope.ID == "" {
		return scopeResponse{Kind: "default"}
	}
	if kind == "" {
		kind = scope.Kind
	}
	return scopeResponse{Kind: kind, ID: scope.ID}
}

func renderEffective(view effectiveView) effectiveResponse {
	return effectiveResponse{
		Strategy: view.Strategy,
		Found:    view.Found,
		Reason:   view.Reason,
		Version:  view.Version,
	}
}

// scopeFromQuery resolves the scope selector from the GET query parameters.
// It writes the 400 response and returns ok=false on a malformed selector.
func scopeFromQuery(c *gin.Context) (modelrouting.Scope, string, bool) {
	kind := strings.TrimSpace(c.Query("scope_kind"))
	id := strings.TrimSpace(c.Query("scope_id"))
	if kind == "" && id == "" {
		return modelrouting.Scope{}, "", true
	}
	scope, kind, errMessage, ok := buildScope(kind, id)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   errCodeInvalidScope,
			"message": errMessage,
		})
		return modelrouting.Scope{}, "", false
	}
	return scope, kind, true
}

// scopeFromRequest resolves the scope selector from the POST body, falling
// back to the query parameters (so a caller can address a scope either way).
func scopeFromRequest(scope *scopeRequest, c *gin.Context) (modelrouting.Scope, string, bool) {
	kind := strings.TrimSpace(c.Query("scope_kind"))
	id := strings.TrimSpace(c.Query("scope_id"))
	if scope != nil {
		if value := strings.TrimSpace(scope.Kind); value != "" {
			kind = value
		}
		if value := strings.TrimSpace(scope.ID); value != "" {
			id = value
		}
	}
	if kind == "" && id == "" {
		return modelrouting.Scope{}, "", true
	}
	resolved, kind, errMessage, ok := buildScope(kind, id)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   errCodeInvalidScope,
			"message": errMessage,
		})
		return modelrouting.Scope{}, "", false
	}
	return resolved, kind, true
}

// buildScope validates the selector: kind must be user or org, and both parts
// must be present.
func buildScope(kind, id string) (modelrouting.Scope, string, string, bool) {
	switch kind {
	case modelrouting.ScopeUser, modelrouting.ScopeOrg:
	case "":
		return modelrouting.Scope{}, "", "scope_kind is required when scope_id is set (user|org)", false
	default:
		return modelrouting.Scope{}, "", "scope_kind must be user or org", false
	}
	if id == "" {
		return modelrouting.Scope{}, "", "scope_id is required when scope_kind is set", false
	}
	return modelrouting.Scope{Kind: kind, ID: id}, kind, "", true
}

// formatTime renders a snapshot instant as RFC3339, or "" when it is unset.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// formatVersion renders a version number for an operator-facing message.
func formatVersion(version int64) string {
	return "v" + strconv.FormatInt(version, 10)
}
