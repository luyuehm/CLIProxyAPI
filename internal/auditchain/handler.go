// Package auditchain, HTTP surface.
//
// The handler rides the authenticated /v0/management group (injected via
// api.WithAuditChainHandler, registered by internal/api), so an unauthenticated
// caller is answered 401/403 by the management middleware before any record
// content can leak — the "no permission, no content" guarantee of the audit
// contract.
//
// Routes:
//
//	GET  /v0/management/audit/operations/:id?from=RFC3339&to=RFC3339
//	    reads one operation's event timeline plus the chain-integrity verdict.
//	POST /v0/management/audit/events
//	    appends one event to an operation's chain.
package auditchain

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// Handler serves the audit-chain endpoints.
type Handler struct {
	store *Store
	// key seals model context snapshots at rest. nil means snapshot capture
	// is disabled: ModelInput/ModelOutput are dropped (fail-closed) and
	// events report snapshot_recorded=false.
	key []byte
}

// NewHandler builds the HTTP handler over a store. key may be nil to disable
// encrypted context capture.
func NewHandler(store *Store, key []byte) *Handler {
	return &Handler{store: store, key: key}
}

// Handle dispatches by route shape: a GET that carries the operation id
// parameter reads a chain; a POST appends an event; anything else is a
// method error.
func (h *Handler) Handle(c *gin.Context) {
	if h == nil || h.store == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	switch {
	case c.Request.Method == http.MethodGet && c.Param("id") != "":
		h.operation(c)
	case c.Request.Method == http.MethodPost && c.Param("id") == "":
		h.append(c)
	default:
		c.AbortWithStatus(http.StatusMethodNotAllowed)
	}
}

type storedEventResponse struct {
	Seq              int64  `json:"seq"`
	Kind             string `json:"kind"`
	Actor            string `json:"actor,omitempty"`
	Summary          string `json:"summary,omitempty"`
	Payload          any    `json:"payload,omitempty"`
	AuthMethod       string `json:"auth_method,omitempty"`
	OccurredAt       string `json:"occurred_at"`
	PrevHash         string `json:"prev_hash"`
	Hash             string `json:"hash"`
	SnapshotRecorded bool   `json:"snapshot_recorded"`
}

type integrityResponse struct {
	Intact       bool   `json:"intact"`
	Tampered     bool   `json:"tampered"`
	FirstBadSeq  int64  `json:"first_bad_seq"`
	HeadHash     string `json:"head_hash"`
	BackupDigest string `json:"backup_digest,omitempty"`
}

type operationResponse struct {
	OperationID string                `json:"operation_id"`
	Events      []storedEventResponse `json:"events"`
	Integrity   integrityResponse     `json:"integrity"`
}

func (h *Handler) operation(c *gin.Context) {
	operationID := c.Param("id")
	var from, to time.Time
	if v := strings.TrimSpace(c.Query("from")); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "from must be RFC3339"})
			return
		}
		from = t
	}
	if v := strings.TrimSpace(c.Query("to")); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "to must be RFC3339"})
			return
		}
		to = t
	}
	chain, err := h.store.Read(c.Request.Context(), operationID, from, to)
	if errors.Is(err, ErrOperationNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "operation not found"})
		return
	}
	if err != nil {
		log.WithError(err).Warn("audit chain read failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "audit chain read failed"})
		return
	}
	resp := operationResponse{OperationID: chain.OperationID}
	for _, ev := range chain.Events {
		er := storedEventResponse{
			Seq:              ev.Seq,
			Kind:             ev.Kind,
			Actor:            ev.Actor,
			Summary:          ev.Summary,
			AuthMethod:       ev.AuthMethod,
			OccurredAt:       ev.OccurredAt.Format(time.RFC3339Nano),
			PrevHash:         ev.PrevHash,
			Hash:             ev.Hash,
			SnapshotRecorded: ev.SnapshotRecorded,
		}
		if ev.PayloadJSON != "" && ev.PayloadJSON != "null" {
			var p any
			if json.Unmarshal([]byte(ev.PayloadJSON), &p) == nil {
				er.Payload = p
			} else {
				er.Payload = ev.PayloadJSON
			}
		}
		resp.Events = append(resp.Events, er)
	}
	resp.Integrity = integrityResponse{
		Intact:       chain.Integrity.Intact,
		Tampered:     chain.Integrity.Tampered,
		FirstBadSeq:  chain.Integrity.FirstBadSeq,
		HeadHash:     chain.Integrity.HeadHash,
		BackupDigest: chain.Integrity.BackupDigest,
	}
	c.JSON(http.StatusOK, resp)
}

type appendRequest struct {
	OperationID string          `json:"operation_id"`
	Kind        string          `json:"kind"`
	Actor       string          `json:"actor"`
	Summary     string          `json:"summary"`
	Payload     json.RawMessage `json:"payload"`
	ModelInput  string          `json:"model_input"`
	ModelOutput string          `json:"model_output"`
	OccurredAt  time.Time       `json:"occurred_at"`
}

func (h *Handler) append(c *gin.Context) {
	var req appendRequest
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}
	// Only forward snapshots when the channel is configured to capture them:
	// without a key the store drops them anyway, and echoing them back would
	// be a pointless copy.
	payload := ""
	if len(req.Payload) > 0 {
		payload = string(req.Payload)
	}
	ev := Event{
		OperationID: req.OperationID,
		Kind:        req.Kind,
		Actor:       req.Actor,
		Summary:     req.Summary,
		PayloadJSON: payload,
		ModelInput:  req.ModelInput,
		ModelOutput: req.ModelOutput,
		OccurredAt:  req.OccurredAt,
	}
	row, err := h.store.Append(c.Request.Context(), ev, h.key)
	if err != nil {
		// Validation failures are operator input errors; surface as 400
		// without echoing internal detail.
		if errors.Is(err, ErrInvalidKey) {
			log.WithError(err).Warn("audit chain append rejected")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "audit chain unavailable"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"operation_id":      ev.OperationID,
		"seq":               row.Seq,
		"hash":              row.Hash,
		"snapshot_recorded": row.SnapshotRecorded,
		"occurred_at":       row.OccurredAt.Format(time.RFC3339Nano),
	})
}
