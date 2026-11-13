package fallback

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
)

// ExecuteFunc runs one attempt against a provider and reports whether the
// attempt failed. Provider/protocol level errors (503, rate limit...) must
// be returned as err; a nil err means the attempt succeeded.
type ExecuteFunc func(ctx context.Context, provider, model string) error

// Outcome is the structured result of one orchestrator run.
type Outcome struct {
	// Success is true when a provider returned nil.
	Success bool
	// Provider is the provider that produced the final result, or "" when
	// no attempt ran.
	Provider string
	// Fallbacks records every switch that happened, in order.
	Fallbacks []FallbackRecord
	// Err is the final error when Success is false: a sentinel (policy
	// violation), a wrapped attempt error, or the raw failure of the last
	// eligible attempt.
	Err error
}

// FallbackRecord is one observability entry: why the orchestrator switched
// providers and what it cost.
type FallbackRecord struct {
	// From and To are the provider switch endpoints; From is "" when the
	// record describes the very first attempt.
	From, To string
	// Reason is a stable machine-readable code, e.g.
	// "provider_a_503", "provider_a_cooled_down", "timeout_budget_exhausted".
	Reason string
	// Detail carries a human-readable explanation; it never contains
	// credential material.
	Detail string
	// ExtraLatency is the wall time spent on the failed attempt that this
	// record follows, i.e. the cost of the fallback so far.
	ExtraLatency time.Duration
	// At is the event timestamp from the orchestrator's clock.
	At time.Time
}

// Orchestrator is the automatic fallback engine. It is safe for concurrent
// use; state lives in the optional HealthWindow.
type Orchestrator struct {
	// Policy is the declarative chain and constraints.
	Policy Policy
	// Health is the optional per-provider backoff tracker. A nil value
	// disables cooldowns entirely.
	Health *HealthWindow
	// Now is the clock; it defaults to time.Now.
	Now func() time.Time
	// Log is the structured logger. nil defaults to a no-op logger so the
	// orchestrator is safe to embed in value contexts.
	Log *logrus.Entry
	// dryRun, when set, evaluates the policy and the chain without running
	// any attempt: it plans which provider would be chosen and which
	// constraints would fire, and records the plan as Fallbacks. It is the
	// issue's "dry-run" observability mode.
	dryRun bool
}

// NewOrchestrator builds an orchestrator over the policy. Health may be nil
// to disable backoff tracking; the clock defaults to time.Now and the
// structured logger to a no-op entry.
func NewOrchestrator(policy Policy, health *HealthWindow) *Orchestrator {
	o := &Orchestrator{
		Policy: policy,
		Health: health,
		Log:    logrus.NewEntry(logrus.New()),
		Now:    time.Now,
	}
	return o
}

// WithDryRun returns a copy of the orchestrator in dry-run mode: policy
// evaluation and planning happen, but no attempt executes and no health
// state is written.
func (o *Orchestrator) WithDryRun() *Orchestrator {
	clone := *o
	clone.dryRun = true
	return &clone
}

// SetLogger replaces the structured logger.
func (o *Orchestrator) SetLogger(e *logrus.Entry) {
	if e != nil {
		o.Log = e
	}
}

// SetHealth installs the backoff tracker.
func (o *Orchestrator) SetHealth(h *HealthWindow) {
	o.Health = h
}

// Run executes the request across the candidate chain until one succeeds or
// all constraints are exhausted. opts.FallbackEnabled=false restricts the run
// to the preferred candidate and returns its error as-is.
func (o *Orchestrator) Run(ctx context.Context, requestedModel string, opts RequestOptions, execute ExecuteFunc) Outcome {
	log := o.Log.WithField("component", "fallback")
	log = log.WithField("model", requestedModel)

	chain, hardErr := o.Policy.eligible()
	if hardErr != nil {
		log.WithError(hardErr).Info("fallback: policy hard constraint")
		out := Outcome{Err: hardErr}
		if hardErr == ErrNoCandidates || hardErr == ErrCostLimitExceeded || hardErr == ErrQualityFloorUnsat {
			out.Fallbacks = append(out.Fallbacks, FallbackRecord{
				Reason: hardErr.Error(),
				Detail: hardErr.Error(),
				At:     o.Now(),
			})
		}
		return out
	}

	// Request-level disable: only the preferred candidate may run.
	if !opts.FallbackEnabled() {
		chain = chain[:1]
	}

	var (
		outcome   Outcome
		lastEvent error // last executed failure, or a health-skip description
		executed  bool  // at least one attempt actually ran
		start     = o.Now()
		attempt   int
	)

	deadline := time.Time{}
	if o.Policy.TimeoutBudget > 0 {
		deadline = start.Add(o.Policy.TimeoutBudget)
	}

	for i, c := range chain {
		attempt++
		if i > 0 {
			// The switch itself is the observability record: the event on
			// From (its failure, or a health skip) is what triggered the
			// move to To.
			reason := ""
			detail := "switch"
			if lastEvent != nil {
				reason = reasonCode(lastEvent)
				detail = lastEvent.Error()
			}
			record := FallbackRecord{
				From:         chain[i-1].Provider,
				To:           c.Provider,
				Reason:       reason,
				Detail:       detail,
				ExtraLatency: o.Now().Sub(start),
				At:           o.Now(),
			}
			outcome.Fallbacks = append(outcome.Fallbacks, record)
			log.WithFields(logrus.Fields{
				"fallback_from": record.From,
				"fallback_to":   record.To,
				"reason":        record.Reason,
			}).Info("fallback: switching provider")
		}

		// Health gate: a cooled-down provider is skipped without a call.
		if o.Health != nil {
			if cooled, until := o.Health.CooledDown(c.Provider, o.Now()); cooled {
				lastEvent = &coolingError{provider: c.Provider, until: until}
				log.WithField("cooldown_until", until).Debug("fallback: provider cooling")
				if !opts.FallbackEnabled() {
					// single-candidate mode: a cooled preferred provider is terminal
					outcome.Err = lastEvent
					return outcome
				}
				continue
			}
		}

		// Budget gate: no attempt may start past the deadline.
		if !deadline.IsZero() && !o.Now().Before(deadline) {
			outcome.Err = ErrTimeoutBudgetExhausted
			outcome.Fallbacks = append(outcome.Fallbacks, FallbackRecord{
				From:         providerAt(chain, i-1),
				To:           c.Provider,
				Reason:       "timeout_budget_exhausted",
				Detail:       fmt.Sprintf("budget %s reached before attempt %d", o.Policy.TimeoutBudget, attempt),
				ExtraLatency: o.Now().Sub(start),
				At:           o.Now(),
			})
			log.Warn("fallback: timeout budget exhausted")
			return outcome
		}

		// Dry run stops planning here: no attempt executes.
		if o.dryRun {
			outcome.Success = true
			outcome.Provider = c.Provider
			outcome.Fallbacks = append(outcome.Fallbacks, FallbackRecord{
				From:   providerAt(chain, i-1),
				To:     c.Provider,
				Reason: "dry_run_plan",
				Detail: fmt.Sprintf("would attempt %s as step %d", c.Provider, attempt),
				At:     o.Now(),
			})
			log.WithField("planned_provider", c.Provider).Debug("fallback: dry-run plan")
			return outcome
		}

		model := requestedModel
		if c.Model != "" {
			model = c.Model
		}

		executed = true
		attemptStart := o.Now()
		err := execute(ctx, c.Provider, model)
		elapsed := o.Now().Sub(attemptStart)

		if err == nil {
			if o.Health != nil {
				o.Health.ObserveSuccess(c.Provider, o.Now())
			}
			outcome.Success = true
			outcome.Provider = c.Provider
			return outcome
		}

		if o.Health != nil {
			o.Health.ObserveFailure(c.Provider, o.Now())
		}
		lastEvent = err
		log.WithFields(logrus.Fields{
			"provider":   c.Provider,
			"reason":     reasonCode(err),
			"attempt_ms": elapsed.Milliseconds(),
			"attempts":   attempt,
		}).Warn("fallback: provider attempt failed")
		if !opts.FallbackEnabled() {
			outcome.Err = err
			log.WithError(err).Info("fallback: request-level disable, returning error")
			return outcome
		}
	}

	// Chain exhausted: either the last executed attempt failed, or every
	// candidate was health-skipped (no call was made at all).
	if !executed {
		lastEvent = ErrAllProvidersCooling
	}
	outcome.Err = lastEvent
	// Termination record: the last candidate was attempted (or health-
	// skipped) and no further candidate remains in the chain.
	outcome.Fallbacks = append(outcome.Fallbacks, FallbackRecord{
		From:   providerAt(chain, len(chain)-1),
		To:     "",
		Reason: reasonCode(lastEvent),
		Detail: lastEvent.Error(),
		At:     o.Now(),
	})
	log.WithError(outcome.Err).Warn("fallback: chain exhausted")
	return outcome
}

// providerAt returns chain[i].Provider with a safe "" for i == -1 (first
// attempt, which has no "from").
func providerAt(chain []Candidate, i int) string {
	if i < 0 || i >= len(chain) {
		return ""
	}
	return chain[i].Provider
}
