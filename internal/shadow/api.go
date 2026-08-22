package shadow

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// APIHandler serves the shadow evaluation and canary control endpoints.
// These are exposed through the management API layer.
type APIHandler struct {
	engine       *Engine
	router       *CanaryRouter
	cfg          Config
	mu           sync.RWMutex
	primaryTTFT  *RingBuffer
	primaryToken *RingBuffer
}

// RingBuffer is a simple fixed-size ring buffer for float64 values.
type RingBuffer struct {
	items []float64
	next  int
	full  bool
	cap   int
	mu    sync.RWMutex
}

// NewRingBuffer creates a ring buffer.
func NewRingBuffer(cap int) *RingBuffer {
	if cap <= 0 {
		cap = 100
	}
	return &RingBuffer{items: make([]float64, cap), cap: cap}
}

// Push adds a value.
func (rb *RingBuffer) Push(v float64) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.items[rb.next] = v
	rb.next = (rb.next + 1) % rb.cap
	if rb.next == 0 {
		rb.full = true
	}
}

// Avg returns the average of stored values.
func (rb *RingBuffer) Avg() float64 {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	count := 0
	var sum float64
	max := rb.cap
	if !rb.full {
		max = rb.next
	}
	for i := 0; i < max; i++ {
		sum += rb.items[i]
		count++
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

// NewAPIHandler creates a handler for the shadow management API.
func NewAPIHandler(cfg Config) *APIHandler {
	cfg.Defaults()
	engine := NewEngine(cfg)
	router := NewCanaryRouter(cfg.Canaries)
	return &APIHandler{
		engine:       engine,
		router:       router,
		cfg:          cfg,
		primaryTTFT:  NewRingBuffer(100),
		primaryToken: NewRingBuffer(100),
	}
}

// Start launches the background engine.
func (h *APIHandler) Start() {
	h.engine.Start()
}

// Stop shuts down the background engine.
func (h *APIHandler) Stop() {
	h.engine.Stop()
}

// Engine returns the underlying engine.
func (h *APIHandler) Engine() *Engine { return h.engine }

// CanaryRouter returns the canary router.
func (h *APIHandler) CanaryRouter() *CanaryRouter { return h.router }

// RecordPrimaryMetrics records primary request metrics for comparison.
func (h *APIHandler) RecordPrimaryMetrics(ttftMs float64, tokens int) {
	h.primaryTTFT.Push(ttftMs)
	h.primaryToken.Push(float64(tokens))
}

// HandleConfig returns the current config as JSON.
func (h *APIHandler) HandleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut || r.Method == http.MethodPost {
		var newCfg Config
		if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
			http.Error(w, `{"error":"invalid config: `+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		newCfg.Defaults()
		h.mu.Lock()
		h.cfg = newCfg
		h.mu.Unlock()
		h.engine.UpdateConfig(newCfg)
		h.router.Update(newCfg.Canaries)
		writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
		return
	}

	h.mu.RLock()
	cfg := h.cfg
	h.mu.RUnlock()
	writeJSON(w, http.StatusOK, cfg.FormatGuessConfig())
}

// HandleStats returns aggregate evaluation stats.
func (h *APIHandler) HandleStats(w http.ResponseWriter, r *http.Request) {
	model := r.URL.Query().Get("model")
	candidate := r.URL.Query().Get("candidate")

	stats := h.engine.store.Stats(model, candidate)
	if stats == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"total_evals": 0, "error_count": 0, "avg_similarity": 0,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total_evals":         stats.TotalEvals,
		"error_count":         stats.ErrorCount,
		"avg_similarity":      stats.Similarity,
		"avg_ttft_ms":         stats.AvgTTFTMs,
		"avg_primary_ttft_ms": h.primaryTTFT.Avg(),
		"avg_primary_tokens":  h.primaryToken.Avg(),
	})
}

// HandleRecords returns recent evaluation records.
func (h *APIHandler) HandleRecords(w http.ResponseWriter, r *http.Request) {
	model := r.URL.Query().Get("model")
	candidate := r.URL.Query().Get("candidate")
	limit := 100

	records := h.engine.store.Query(model, candidate, limit)
	if records == nil {
		records = []*Record{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"records": records,
		"count":   len(records),
		"total":   h.engine.store.Len(),
	})
}

// HandleCanary returns the current canary rules.
func (h *APIHandler) HandleCanary(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"canaries": h.router.Rules(),
		})
	case http.MethodPost, http.MethodPut:
		var body struct {
			Model  string `json:"model"`
			Weight int    `json:"weight"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
			return
		}
		rules := h.router.Rules()
		updated := false
		for i, rule := range rules {
			if rule.Model == body.Model {
				rules[i].Weight = body.Weight
				updated = true
				break
			}
		}
		if !updated {
			http.Error(w, `{"error":"model not found in canary rules"}`, http.StatusNotFound)
			return
		}
		h.router.Update(rules)
		writeJSON(w, http.StatusOK, map[string]any{"status": "updated", "model": body.Model, "weight": body.Weight})
	case http.MethodDelete:
		model := r.URL.Query().Get("model")
		if model == "" {
			http.Error(w, `{"error":"model query param required"}`, http.StatusBadRequest)
			return
		}
		if h.router.Rollback(model) {
			writeJSON(w, http.StatusOK, map[string]any{"status": "rolled_back", "model": model})
		} else {
			http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
		}
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// HandleHealth returns engine health.
func (h *APIHandler) HandleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":      h.cfg.Enabled,
		"queue_size":   h.cfg.QueueSize,
		"workers":      h.cfg.WorkerCount,
		"stored_evals": h.engine.store.Len(),
		"time":         time.Now().UTC().Format(time.RFC3339),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
