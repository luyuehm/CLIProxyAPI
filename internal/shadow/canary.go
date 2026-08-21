package shadow

import (
	"crypto/rand"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
)

// CanaryRouter manages weighted canary release routing for model pairs.
// It is safe for concurrent use.
type CanaryRouter struct {
	rules     []CanaryConfig
	rollbacks atomic.Value // stored as []CanaryConfig
}

// NewCanaryRouter creates a router with the given rules.
func NewCanaryRouter(canaries []CanaryConfig) *CanaryRouter {
	r := &CanaryRouter{}
	r.Update(canaries)
	return r
}

// Update hot-swaps the canary rules atomically.
func (r *CanaryRouter) Update(canaries []CanaryConfig) {
	if canaries == nil {
		canaries = []CanaryConfig{}
	}
	r.rollbacks.Store(canaries)
}

// Rules returns a copy of the current canary rules.
func (r *CanaryRouter) Rules() []CanaryConfig {
	val := r.rollbacks.Load()
	if val == nil {
		return nil
	}
	rules := val.([]CanaryConfig)
	out := make([]CanaryConfig, len(rules))
	copy(out, rules)
	return out
}

// Decide checks whether a request should be routed to a canary candidate.
// It returns the matching canary config and true when the request should be
// redirected to the candidate model.
//
// The decision is based on, in order:
//  1. Forcing headers (e.g. X-Canary: true) — always route to candidate.
//  2. User/Department header match — check UserIDHeader + UserIDs.
//  3. Weighted random — probability = weight / 100.
func (r *CanaryRouter) Decide(model string, headers http.Header) (CanaryConfig, bool, string) {
	val := r.rollbacks.Load()
	if val == nil {
		return CanaryConfig{}, false, ""
	}
	rules := val.([]CanaryConfig)

	for _, rule := range rules {
		if rule.Model != model {
			continue
		}

		// 1. Forcing headers
		if rule.Headers != nil {
			match := true
			for k, v := range rule.Headers {
				if strings.EqualFold(headers.Get(k), v) {
					match = true
					continue
				}
				match = false
				break
			}
			if match && len(rule.Headers) > 0 {
				return rule, true, "header-force"
			}
		}

		// 2. User/Department header match
		if rule.UserIDHeader != "" && len(rule.UserIDs) > 0 {
			userVal := headers.Get(rule.UserIDHeader)
			if userVal != "" {
				for _, uid := range rule.UserIDs {
					if strings.EqualFold(userVal, uid) {
						return rule, true, "user-force"
					}
				}
			}
		}

		// 3. Weighted random
		if rule.Weight > 0 {
			n, _ := rand.Int(rand.Reader, big.NewInt(100))
			if n.Int64() < int64(rule.Weight) {
				return rule, true, "weighted"
			}
		}
	}

	return CanaryConfig{}, false, ""
}

// Rollback immediately sets the canary weight for the given model to 0.
func (r *CanaryRouter) Rollback(model string) bool {
	val := r.rollbacks.Load()
	if val == nil {
		return false
	}
	rules := val.([]CanaryConfig)
	updated := false
	newRules := make([]CanaryConfig, 0, len(rules))
	for _, rule := range rules {
		if rule.Model == model {
			rule.Weight = 0
			updated = true
		}
		newRules = append(newRules, rule)
	}
	if !updated {
		return false
	}
	r.rollbacks.Store(newRules)
	return true
}

// CanaryAction is a decoupled return value the HTTP handler checks at the
// model-selection layer. It avoids the middleware having to rewrite the body.
type CanaryAction struct {
	Model     string
	Candidate string
	Reason    string
	Weight    int
}

// EvaluateCanary returns a CanaryAction when the request should be
// canary-routed, without mutating anything. The caller applies the routing.
func EvaluateCanary(rules []CanaryConfig, model string, headers http.Header) *CanaryAction {
	r := NewCanaryRouter(rules)
	rule, ok, reason := r.Decide(model, headers)
	if !ok {
		return nil
	}
	return &CanaryAction{
		Model:     model,
		Candidate: rule.Candidate,
		Reason:    reason,
		Weight:    rule.Weight,
	}
}

// MirrorSelector is a per-model sampling decision.
type MirrorSelector struct {
	enabled bool
	mirrors []MirrorConfig
}

// NewMirrorSelector creates a selector from config.
func NewMirrorSelector(cfg Config) *MirrorSelector {
	return &MirrorSelector{
		enabled: cfg.Enabled,
		mirrors: cfg.Mirrors,
	}
}

// ShouldSample returns the matching MirrorConfig and true when the request
// should be mirrored to the candidate.
func (s *MirrorSelector) ShouldSample(model string, headers http.Header) (MirrorConfig, bool) {
	if !s.enabled {
		return MirrorConfig{}, false
	}
	for _, m := range s.mirrors {
		if m.Model != model {
			continue
		}
		// User header filter
		if m.UserHeader != "" {
			userVal := headers.Get(m.UserHeader)
			if userVal == "" {
				continue
			}
		}
		// Ratio sampling
		if m.Ratio <= 0 {
			continue
		}
		if m.Ratio >= 1.0 {
			return m, true
		}
		// Random sampling within ratio
		n, _ := rand.Int(rand.Reader, big.NewInt(10000))
		threshold := int64(m.Ratio * 10000)
		if n.Int64() < threshold {
			return m, true
		}
	}
	return MirrorConfig{}, false
}

// AtomicBool is a simple atomic boolean.
type AtomicBool struct {
	v int32
}

func (b *AtomicBool) Set(v bool) {
	var i int32
	if v {
		i = 1
	}
	atomic.StoreInt32(&b.v, i)
}

func (b *AtomicBool) Get() bool {
	return atomic.LoadInt32(&b.v) != 0
}
