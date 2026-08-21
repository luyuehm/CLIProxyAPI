package shadow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// Engine is the shadow traffic mirroring engine. It runs a bounded worker pool
// that dispatches mirrored requests to candidate model endpoints asynchronously.
// The primary request path is never blocked: Enqueue returns immediately, dropping
// when the queue is full.
type Engine struct {
	cfg     Config
	queue   chan *MirrorTarget
	workers sync.WaitGroup
	stop    chan struct{}
	store   *EvalStore
	client  *http.Client
	mu      sync.RWMutex
	active  bool
}

// NewEngine creates an engine with the given config.
// Call Start() before use.
func NewEngine(cfg Config) *Engine {
	cfg.Defaults()
	e := &Engine{
		cfg:   cfg,
		store: NewEvalStore(),
		client: &http.Client{
			Timeout: cfg.Timeout,
			Transport: &http.Transport{
				MaxIdleConns:        8,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
	return e
}

// Start launches the worker pool. It is safe to call multiple times — only the
// first call spins up goroutines.
func (e *Engine) Start() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active {
		return
	}
	e.active = true
	e.queue = make(chan *MirrorTarget, e.cfg.QueueSize)
	e.stop = make(chan struct{})

	for i := 0; i < e.cfg.WorkerCount; i++ {
		e.workers.Add(1)
		go e.worker(i)
	}
	log.Infof("shadow: engine started with %d workers, queue=%d", e.cfg.WorkerCount, e.cfg.QueueSize)
}

// Stop gracefully stops the worker pool. In-flight requests are waited on.
func (e *Engine) Stop() {
	e.mu.Lock()
	if !e.active {
		e.mu.Unlock()
		return
	}
	e.active = false
	close(e.stop)
	close(e.queue)
	e.mu.Unlock()
	e.workers.Wait()
	log.Info("shadow: engine stopped")
}

// Enqueue dispatches a mirrored request to a candidate model. Returns false when
// the queue is full or the engine is not started — the caller should NOT retry
// and MUST NOT block on the result.
//
// Enqueue must be called from the primary request goroutine. It never blocks
// for more than a single channel send.
func (e *Engine) Enqueue(target *MirrorTarget) bool {
	e.mu.RLock()
	q := e.queue
	active := e.active
	e.mu.RUnlock()
	if !active || q == nil {
		return false
	}
	select {
	case q <- target:
		return true
	default:
		log.Warn("shadow: mirror queue full, dropping request")
		return false
	}
}

// Store returns the evaluation ledger.
func (e *Engine) Store() *EvalStore { return e.store }

// Config returns a copy of the current engine config.
func (e *Engine) Config() Config {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg
}

// UpdateConfig hot-swaps the engine config atomically.
func (e *Engine) UpdateConfig(cfg Config) {
	e.mu.Lock()
	e.cfg = cfg
	e.mu.Unlock()
}

// worker processes mirror targets from the queue.
func (e *Engine) worker(id int) {
	defer e.workers.Done()
	log.Debugf("shadow: worker %d started", id)
	for {
		select {
		case <-e.stop:
			log.Debugf("shadow: worker %d stopped", id)
			return
		case target, ok := <-e.queue:
			if !ok {
				return
			}
			e.evaluateMirror(target)
		}
	}
}

// evaluateMirror sends the mirrored request to the candidate endpoint and
// records the evaluation result.
func (e *Engine) evaluateMirror(target *MirrorTarget) {
	start := time.Now()

	url := normalizeEndpoint(target.Rule.Endpoint) + target.Path
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(target.Body))
	if err != nil {
		log.Errorf("shadow: mirror request creation error: %v", err)
		return
	}

	// Copy headers
	for k, vs := range target.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if target.Rule.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+target.Rule.APIKey)
	}
	for k, v := range target.Rule.Headers {
		req.Header.Set(k, v)
	}

	// Read the primary body to extract the prompt for hashing.
	// We use a truncated JSON keys-only fingerprint for the similarity comparison.
	primaryBody := target.Body

	resp, err := e.client.Do(req)
	if err != nil {
		rec := &Record{
			Kind:        EvalKindMirror,
			Model:       target.Rule.Model,
			Candidate:   target.Rule.Candidate,
			Timestamp:   start,
			PromptHash:  shortHash(primaryBody),
			Error:       fmt.Sprintf("candidate request failed: %v", err),
			PrimaryTTFT: 0,
		}
		e.store.Push(rec)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB cap
	elapsed := time.Since(start)

	rec := &Record{
		Kind:           EvalKindMirror,
		Model:          target.Rule.Model,
		Candidate:      target.Rule.Candidate,
		Timestamp:      start,
		PromptHash:     shortHash(primaryBody),
		PrimaryTTFT:    0, // filled by caller
		CandidateTTFT:  float64(elapsed.Milliseconds()),
		LatencyDeltaMs: float64(elapsed.Milliseconds()),
		PrimaryTokens:  0, // filled by caller when available
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		rec.Similarity = computeSimilarity(primaryBody, respBody)
		rec.TokenRatio = computeTokenRatio(respBody, primaryBody)
		rec.CandidateTokens = extractTokenCount(respBody)
	} else {
		rec.Error = fmt.Sprintf("candidate responded %d: %s", resp.StatusCode, truncateString(string(respBody), 200))
	}

	e.store.Push(rec)
}

// FormatGuessConfig returns a minimal config for the shadow API handler.
// It strips the APIKey from the returned config for safe serialization.
func (c Config) FormatGuessConfig() Config {
	out := c
	for i := range out.Mirrors {
		out.Mirrors[i].APIKey = ""
	}
	return out
}

// normalizeEndpoint ensures the endpoint has a scheme and no trailing slash.
func normalizeEndpoint(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "https://" + endpoint
	}
	return strings.TrimRight(endpoint, "/")
}

// truncateString truncates a string to n runes.
func truncateString(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

// shortHash returns a compact content hash of the input.
func shortHash(body []byte) string {
	// Use a simple 64-bit FNV-1a hash for speed; this is a content fingerprint,
	// not a security boundary.
	var h uint64 = 14695981039346656037
	for _, b := range body {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return fmt.Sprintf("%016x", h)
}

// computeSimilarity returns a crude text overlap coefficient between the
// production and candidate response bodies. It is a fast heuristic, not a
// semantic similarity measure.
func computeSimilarity(primary, candidate []byte) float64 {
	pp := extractResponseText(primary)
	cp := extractResponseText(candidate)
	if len(pp) == 0 && len(cp) == 0 {
		return 1.0
	}
	if len(pp) == 0 || len(cp) == 0 {
		return 0.0
	}

	// Token overlap (word-level Jaccard-like)
	pTokens := tokenize(pp)
	cTokens := tokenize(cp)
	if len(pTokens) == 0 && len(cTokens) == 0 {
		return 1.0
	}

	intersection := 0
	set := make(map[string]int, len(pTokens))
	for _, t := range pTokens {
		set[t]++
	}
	for _, t := range cTokens {
		if set[t] > 0 {
			intersection++
			set[t]--
		}
	}

	union := make(map[string]int)
	for _, t := range pTokens {
		union[t]++
	}
	for _, t := range cTokens {
		union[t]++
	}
	unionSize := 0
	for _, v := range union {
		unionSize += v
	}
	if unionSize == 0 {
		return 1.0
	}
	return float64(intersection) / float64(unionSize)
}

// computeTokenRatio estimates output token usage ratio between candidate and primary.
func computeTokenRatio(candidate, primary []byte) float64 {
	pTokens := extractTokenCount(primary)
	cTokens := extractTokenCount(candidate)
	if pTokens == 0 {
		return 0
	}
	return float64(cTokens) / float64(pTokens)
}

// extractResponseText attempts to pull the content/response text
// from either an OpenAI /v1/chat/completions or a Claude /v1/messages response.
func extractResponseText(body []byte) string {
	if len(body) == 0 {
		return ""
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return ""
	}

	// OpenAI format: choices[0].message.content
	if choicesRaw, ok := top["choices"]; ok {
		var choices []map[string]json.RawMessage
		if err := json.Unmarshal(choicesRaw, &choices); err == nil && len(choices) > 0 {
			if msgRaw, ok := choices[0]["message"]; ok {
				var msg map[string]json.RawMessage
				if err := json.Unmarshal(msgRaw, &msg); err == nil {
					if contentRaw, ok := msg["content"]; ok {
						var content string
						if err := json.Unmarshal(contentRaw, &content); err == nil {
							return content
						}
						// content might be an array of content parts
						var parts []map[string]json.RawMessage
						if err := json.Unmarshal(contentRaw, &parts); err == nil {
							var sb strings.Builder
							for _, part := range parts {
								if t, ok := part["type"]; ok {
									var typeStr string
									if err := json.Unmarshal(t, &typeStr); err == nil && typeStr == "text" {
										if textRaw, ok := part["text"]; ok {
											var text string
											if err := json.Unmarshal(textRaw, &text); err == nil {
												sb.WriteString(text)
												sb.WriteString(" ")
											}
										}
									}
								}
							}
							return strings.TrimSpace(sb.String())
						}
					}
				}
			}
			// Delta (streaming)
			if deltaRaw, ok := choices[0]["delta"]; ok {
				var delta map[string]json.RawMessage
				if err := json.Unmarshal(deltaRaw, &delta); err == nil {
					if contentRaw, ok := delta["content"]; ok {
						var content string
						if err := json.Unmarshal(contentRaw, &content); err == nil {
							return content
						}
					}
				}
			}
		}
	}

	// Claude format: content[0].text
	if contentRaw, ok := top["content"]; ok {
		var content []map[string]json.RawMessage
		if err := json.Unmarshal(contentRaw, &content); err == nil {
			for _, block := range content {
				if t, ok := block["type"]; ok {
					var typeStr string
					if err := json.Unmarshal(t, &typeStr); err == nil && typeStr == "text" {
						if textRaw, ok := block["text"]; ok {
							var text string
							if err := json.Unmarshal(textRaw, &text); err == nil {
								return text
							}
						}
					}
				}
			}
		}
	}

	return ""
}

// extractTokenCount attempts to extract usage token count from the response.
// Tries both OpenAI and Claude response formats.
func extractTokenCount(body []byte) int {
	if len(body) == 0 {
		return 0
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return 0
	}

	// OpenAI usage
	if usageRaw, ok := top["usage"]; ok {
		var usage struct {
			CompletionTokens int `json:"completion_tokens"`
		}
		if err := json.Unmarshal(usageRaw, &usage); err == nil {
			return usage.CompletionTokens
		}
	}

	// Claude usage
	if usageRaw, ok := top["usage"]; ok {
		var usage struct {
			OutputTokens int `json:"output_tokens"`
		}
		if err := json.Unmarshal(usageRaw, &usage); err == nil {
			return usage.OutputTokens
		}
	}

	return 0
}

// tokenize splits text into whitespace-delimited tokens.
func tokenize(text string) []string {
	words := strings.Fields(text)
	lower := make([]string, len(words))
	for i, w := range words {
		lower[i] = strings.ToLower(w)
	}
	return lower
}
