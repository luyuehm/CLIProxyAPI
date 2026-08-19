package costbudget

import (
	"context"
	"strings"
	"sync"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// spendKey identifies a price/budget scope for a request.
func spendKey(provider, model string) (string, string) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" {
		provider = "unknown"
	}
	if model == "" {
		model = "unknown"
	}
	return provider, model
}

// periodSpend holds the aggregated spend for one budget period window.
type periodSpend struct {
	start      time.Time
	resetAt    time.Time
	totalSpend float64
	// lastAlertLevel tracks the highest level already notified for this window
	// so each threshold fires at most once per period.
	lastAlertLevel AlertLevel
}

// Tracker aggregates usage records against one or more budgets.
//
// It implements coreusage.Plugin and is registered with the default usage
// manager. Because the manager calls HandleUsage from a single dispatcher
// goroutine, Tracker still uses its own mutex to protect against HTTP report
// reads from a different goroutine.
type Tracker struct {
	mu sync.Mutex

	cfg BudgetConfig

	// per-period spend, keyed by the budget slice index so multiple budgets
	// of the same period are independent.
	spend []periodSpend

	alerter Alerter
}

// NewTracker constructs a tracker from config. The alerter is optional; when
// nil, alerts are only logged.
//
// Spend windows are not seeded eagerly: the first record that arrives
// establishes each budget's window. This avoids pinning the window to the
// process start time (which may be far from the record's actual time) and
// keeps the report consistent with the records that have been seen.
func NewTracker(cfg BudgetConfig, alerter Alerter) *Tracker {
	t := &Tracker{cfg: cfg, alerter: alerter}
	t.spend = make([]periodSpend, len(cfg.Budgets))
	return t
}

// Enabled reports whether the tracker is active.
func (t *Tracker) Enabled() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cfg.Enabled
}

// HandleUsage implements coreusage.Plugin. It is invoked by the usage manager
// dispatcher for every upstream request record.
func (t *Tracker) HandleUsage(_ context.Context, record coreusage.Record) {
	if t == nil || !t.Enabled() || record.Failed {
		return
	}

	provider, model := spendKey(record.Provider, record.Model)
	breakdown := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType).TokenBreakdown
	if !breakdown.Valid() {
		// Inconsistent accounting: nothing reliable to charge. Skip silently
		// rather than double-counting from a malformed breakdown.
		return
	}

	rate := priceFor(t.cfg.Prices, provider, model)
	spend := costForTokens(breakdown, rate)

	t.mu.Lock()
	defer t.mu.Unlock()

	now := record.RequestedAt
	if now.IsZero() {
		now = time.Now()
	}
	for i := range t.spend {
		t.applySpendLocked(i, spend, now)
	}
}

// applySpendLocked adds spend to a period window, rolling the window over when
// the current time has moved past its reset boundary, then evaluates alerts.
// Caller must hold t.mu.
func (t *Tracker) applySpendLocked(i int, spend float64, now time.Time) {
	ps := &t.spend[i]
	budget := t.cfg.Budgets[i]
	if budget.Amount <= 0 {
		return
	}
	// Establish the window on first use, or roll it over when now has moved
	// past the current window's reset boundary. On (re)establishment, spend
	// and the last-alert level reset so the new window starts clean.
	needsWindow := ps.resetAt.IsZero()
	if !needsWindow && !now.Before(ps.resetAt) {
		needsWindow = true
	}
	if needsWindow {
		start, reset := PeriodWindow(budget.Period, now)
		ps.start = start
		ps.resetAt = reset
		ps.totalSpend = 0
		ps.lastAlertLevel = AlertNone
	}
	ps.totalSpend += spend

	utilization := ps.totalSpend / budget.Amount
	level := classify(utilization, budget)
	if level == AlertNone {
		return
	}
	// Fire only when crossing to a higher level than already notified.
	if levelRank(level) <= levelRank(ps.lastAlertLevel) {
		return
	}
	ps.lastAlertLevel = level
	alert := Alert{
		Period:            budget.Period,
		Level:             level,
		BudgetAmount:      budget.Amount,
		CurrentSpend:      ps.totalSpend,
		Utilization:       utilization,
		PeriodStart:       ps.start,
		PeriodResetAt:     ps.resetAt,
		TriggeredAt:       now,
		ThresholdFraction: fractionFor(level, budget),
	}
	t.notifyLocked(alert)
}

// notifyLocked delivers an alert. Caller must hold t.mu.
func (t *Tracker) notifyLocked(alert Alert) {
	log.WithFields(log.Fields{
		"costbudget_period":      string(alert.Period),
		"costbudget_level":       string(alert.Level),
		"costbudget_spend":       alert.CurrentSpend,
		"costbudget_budget":      alert.BudgetAmount,
		"costbudget_utilization": alert.Utilization,
	}).Warn("cost budget threshold crossed")
	if t.alerter != nil {
		t.alerter.Notify(alert)
	}
}

func levelRank(level AlertLevel) int {
	switch level {
	case AlertCritical:
		return 2
	case AlertWarning:
		return 1
	default:
		return 0
	}
}
