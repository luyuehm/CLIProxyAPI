package costallocation

import (
	"context"
	"strings"
	"sync"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// MaxHistoryBuckets is the maximum number of period buckets retained per
// department. Older buckets are evicted from the front.
const MaxHistoryBuckets = 24

// periodBucket holds aggregated spend and token counts for one period window
// (a calendar month or quarter).
type periodBucket struct {
	PeriodKey   string    `json:"period_key"`
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`

	Spend        float64 `json:"spend"`
	RequestCount int64   `json:"request_count"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
}

// departmentState tracks aggregated cost data for one department.
type departmentState struct {
	// Current window tracking.
	CurrentSpend        float64   `json:"current_spend"`
	CurrentRequestCount int64     `json:"current_request_count"`
	CurrentInputTokens  int64     `json:"current_input_tokens"`
	CurrentOutputTokens int64     `json:"current_output_tokens"`
	CurrentTotalTokens  int64     `json:"current_total_tokens"`
	CurrentPeriodStart  time.Time `json:"current_period_start"`
	CurrentPeriodEnd    time.Time `json:"current_period_end"`

	// History of completed period buckets (most recent last).
	Buckets []*periodBucket `json:"buckets,omitempty"`
}

// Tracker aggregates usage records by department, tracking spend, token usage,
// and request counts per period window. It implements coreusage.Plugin.
type Tracker struct {
	mu sync.Mutex

	cfg CostAllocationConfig

	// departments is keyed by department name.
	departments map[string]*departmentState
}

// NewTracker constructs a tracker from config.
func NewTracker(cfg CostAllocationConfig) *Tracker {
	return &Tracker{
		cfg:         cfg,
		departments: make(map[string]*departmentState),
	}
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
	if t == nil || !t.cfg.Enabled || record.Failed {
		return
	}

	breakdown := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType).TokenBreakdown
	if !breakdown.Valid() {
		// Inconsistent accounting: nothing reliable to charge. Skip silently
		// rather than double-counting from a malformed breakdown.
		return
	}

	department := AllocateDepartment(t.cfg.Rules, t.cfg.effectiveUnallocatedName(), record)

	provider := strings.TrimSpace(record.Provider)
	model := strings.TrimSpace(record.Model)
	rate := priceFor(t.cfg.Prices, provider, model)
	cost := costForTokens(breakdown, rate)

	t.mu.Lock()
	defer t.mu.Unlock()

	now := record.RequestedAt
	if now.IsZero() {
		now = time.Now()
	}

	state, exists := t.departments[department]
	if !exists {
		state = &departmentState{}
		t.departments[department] = state
	}

	// Check if the record falls within the current window; if not, finalize the
	// current bucket into history and open a new window.
	period := t.cfg.effectivePeriod()
	if state.CurrentPeriodEnd.IsZero() || !now.Before(state.CurrentPeriodEnd) {
		if !state.CurrentPeriodStart.IsZero() {
			state.Buckets = append(state.Buckets, &periodBucket{
				PeriodKey:    PeriodKey(period, state.CurrentPeriodStart),
				PeriodStart:  state.CurrentPeriodStart,
				PeriodEnd:    state.CurrentPeriodEnd,
				Spend:        state.CurrentSpend,
				RequestCount: state.CurrentRequestCount,
				InputTokens:  state.CurrentInputTokens,
				OutputTokens: state.CurrentOutputTokens,
				TotalTokens:  state.CurrentTotalTokens,
			})
			if len(state.Buckets) > MaxHistoryBuckets {
				state.Buckets = state.Buckets[len(state.Buckets)-MaxHistoryBuckets:]
			}
		}
		start, reset := PeriodWindow(period, now)
		state.CurrentPeriodStart = start
		state.CurrentPeriodEnd = reset
		state.CurrentSpend = 0
		state.CurrentRequestCount = 0
		state.CurrentInputTokens = 0
		state.CurrentOutputTokens = 0
		state.CurrentTotalTokens = 0
	}

	state.CurrentSpend += cost
	state.CurrentRequestCount++
	state.CurrentInputTokens += breakdown.Input.TotalTokens
	state.CurrentOutputTokens += breakdown.Output.TotalTokens
	state.CurrentTotalTokens += breakdown.TotalTokens
}
