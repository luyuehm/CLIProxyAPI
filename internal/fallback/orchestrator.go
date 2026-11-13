package fallback

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
)

// ExecuteFunc runs one attempt against a provider and reports whether the
// attempt failed. Provider/protocol level errors (503, rate limit...)
// should be wrapped in *ProviderError so the orchestrator can emit a stable
// machine-readable reason code (e.g. "provider_A_503") in logs and
// FallbackRecords; an untyped error degrades to a generic bounded code
// ("provider_A_error"), never to the raw error text. A nil err means the
// attempt succeeded.
type ExecuteFunc func(ctx context.Context, provider, model string) error

// Outcome is the structured result of one orchestrator run.
type Outcome struct {
	// Success is true when a provider returned nil.
	Success bool
	// Planned is true when the outcome is a dry-run plan: no attempt
	// executed and Success must not be read as "served".
	Planned bool
	// Provider is the provider that produced the final result (or the
	// planned provider in dry-run mode), or "" when no attempt ran.
	Provider string
	// Fallbacks records the decision trail: one record per committed
	// provider switch, plus the terminal record (constraint violation,
	// refused start, or final failure/health-skip).
	Fallbacks []FallbackRecord
	// Err is the final error when Success is false: a sentinel (policy
	// violation), a wrapped attempt error, or the raw failure of the last
	// eligible attempt.
	Err error
}

// FallbackRecord is one observability entry: which provider was attempted
// (or why a switch was refused) and what it cost.
type FallbackRecord struct {
	// From and To are the provider switch endpoints; From is "" when the
	// record describes the very first attempt.
	From, To string
	// Reason is a bounded machine-readable code built from the provider
	// key and status only (never raw error text), e.g.
	// "provider_a_503", "provider_a_cooled_down", "timeout_budget_exhausted".
	Reason string
	// Detail carries a human-readable explanation. For attempts it echoes
	// the integrator's error text, which the orchestrator does not
	// sanitize: treat Detail as log-only, never as a metric label or API
	// field.
	Detail string
	// ExtraLatency is the wall time from run start to this record, i.e.
	// the cost the fallback has so far.
	ExtraLatency time.Duration
	// At is the event timestamp from the orchestrator's clock.
	At time.Time
}

// Orchestrator is the automatic fallback engine. Concurrent Run invocations
// are safe; state lives in the optional HealthWindow. Configure
// Policy/Health/Now/Log before the first Run and do not reconfigure while a
// Run is in flight.
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
// to the preferred candidate only: a preferred candidate excluded by a hard
// constraint is the terminal result, never a silent promotion to the next
// candidate.
//
// Concurrent Run invocations are safe: Run only reads the orchestrator's
// fields and resolves nil Log/Now into local defaults without writing them
// back. Configure Policy/Health/Now/Log before the first Run.
func (o *Orchestrator) Run(ctx context.Context, requestedModel string, opts RequestOptions, execute ExecuteFunc) Outcome {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	logEntry := o.Log
	if logEntry == nil {
		logEntry = logrus.NewEntry(logrus.New())
	}
	log := logEntry.WithField("component", "fallback")
	log = log.WithField("model", requestedModel)

	// Eligibility split by mode: a full chain when fallback is enabled,
	// the preferred candidate alone when it is disabled (B1: no promotion).
	chain, hardErr := o.Policy.eligible()
	if !opts.FallbackEnabled() {
		chain, hardErr = o.Policy.preferredEligible()
	}
	if hardErr != nil {
		log.WithError(hardErr).Info("fallback: policy hard constraint")
		out := Outcome{Err: hardErr}
		out.Fallbacks = append(out.Fallbacks, FallbackRecord{
			Reason: reasonCode(hardErr, ""),
			Detail: hardErr.Error(),
			At:     now(),
		})
		return out
	}

	var (
		outcome      Outcome
		pendingFrom  Candidate // candidate the next switch will move FROM
		pendingEvent error     // the failure / health-skip that justifies it
		lastAttempt  Candidate // last candidate actually executed
		lastEvent    error     // the executed failure on lastAttempt
		executed     bool      // at least one attempt actually ran
		start        = now()
	)

	deadline := time.Time{}
	if o.Policy.TimeoutBudget > 0 {
		deadline = start.Add(o.Policy.TimeoutBudget)
	}

	for i, c := range chain {
		// Health gate: a cooled-down provider is skipped without a call. The
		// skip becomes the pending event that justifies the next switch.
		if o.Health != nil {
			if cooled, until := o.Health.CooledDown(c.Provider, now()); cooled {
				pendingFrom, pendingEvent = c, &coolingError{provider: c.Provider, until: until}
				log.WithField("cooldown_until", until).Debug("fallback: provider cooling")
				if !opts.FallbackEnabled() {
					// single-candidate mode: a cooled preferred provider is terminal
					outcome.Err = pendingEvent
					outcome.Fallbacks = append(outcome.Fallbacks, FallbackRecord{
						From:   c.Provider,
						To:     "",
						Reason: reasonCode(pendingEvent, c.Provider),
						Detail: pendingEvent.Error(),
						At:     now(),
					})
					return outcome
				}
				continue
			}
		}

		// Budget gate: no attempt may start past the deadline. A refused
		// start is a terminal record, not a switch — the pending switch was
		// never committed.
		if !deadline.IsZero() && !now().Before(deadline) {
			outcome.Err = ErrTimeoutBudgetExhausted
			outcome.Fallbacks = append(outcome.Fallbacks, FallbackRecord{
				From:         c.Provider,
				To:           "",
				Reason:       "timeout_budget_exhausted",
				Detail:       fmt.Sprintf("budget %s reached before attempting %s", o.Policy.TimeoutBudget, c.Provider),
				ExtraLatency: now().Sub(start),
				At:           now(),
			})
			log.Warn("fallback: timeout budget exhausted")
			return outcome
		}

		// Commit to c: flush the switch that moved us here, then (in dry-run)
		// stop before executing.
		if i > 0 {
			o.flushSwitch(&outcome, pendingFrom, c, pendingEvent, start, now, log)
		}
		if o.dryRun {
			outcome.Planned = true
			outcome.Success = true
			outcome.Provider = c.Provider
			outcome.Fallbacks = append(outcome.Fallbacks, FallbackRecord{
				From:   c.Provider,
				To:     c.Provider,
				Reason: "dry_run_plan",
				Detail: fmt.Sprintf("would attempt %s as step %d", c.Provider, i+1),
				At:     now(),
			})
			log.WithField("planned_provider", c.Provider).Debug("fallback: dry-run plan")
			return outcome
		}

		model := requestedModel
		if c.Model != "" {
			model = c.Model
		}

		executed = true
		lastAttempt = c
		attemptStart := now()
		err := execute(ctx, c.Provider, model)
		elapsed := now().Sub(attemptStart)

		if err == nil {
			if o.Health != nil {
				o.Health.ObserveSuccess(c.Provider, now())
			}
			outcome.Success = true
			outcome.Provider = c.Provider
			return outcome
		}

		if o.Health != nil {
			o.Health.ObserveFailure(c.Provider, now())
		}
		lastEvent = err
		// This failure now justifies the next switch out of c.
		pendingFrom, pendingEvent = c, err
		log.WithFields(logrus.Fields{
			"provider":   c.Provider,
			"reason":     reasonCode(err, c.Provider),
			"attempt_ms": elapsed.Milliseconds(),
		}).Warn("fallback: provider attempt failed")
		if !opts.FallbackEnabled() {
			outcome.Err = err
			outcome.Fallbacks = append(outcome.Fallbacks, FallbackRecord{
				From:   c.Provider,
				To:     "",
				Reason: reasonCode(err, c.Provider),
				Detail: err.Error(),
				At:     now(),
			})
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
	// Termination record for the final consideration.
	outcome.Fallbacks = append(outcome.Fallbacks, FallbackRecord{
		From:   providerName(lastAttempt),
		To:     "",
		Reason: reasonCode(lastEvent, providerName(lastAttempt)),
		Detail: lastEvent.Error(),
		At:     now(),
	})
	log.WithError(outcome.Err).Warn("fallback: chain exhausted")
	return outcome
}

// flushSwitch records the move from `from` to `to`, justified by event (the
// failure or health skip on `from`). It takes the resolved clock and logger
// so a zero-value orchestrator stays race-free under concurrent Run.
func (o *Orchestrator) flushSwitch(outcome *Outcome, from, to Candidate, event error, start time.Time, now func() time.Time, log *logrus.Entry) {
	reason, detail := "", "switch"
	if event != nil {
		reason = reasonCode(event, from.Provider)
		detail = event.Error()
	}
	record := FallbackRecord{
		From:         from.Provider,
		To:           to.Provider,
		Reason:       reason,
		Detail:       detail,
		ExtraLatency: now().Sub(start),
		At:           now(),
	}
	outcome.Fallbacks = append(outcome.Fallbacks, record)
	log.WithFields(logrus.Fields{
		"fallback_from": record.From,
		"fallback_to":   record.To,
		"reason":        record.Reason,
	}).Info("fallback: switching provider")
}

// providerName returns the candidate's provider key, or "" for the zero
// candidate (nothing was attempted yet).
func providerName(c Candidate) string { return c.Provider }
