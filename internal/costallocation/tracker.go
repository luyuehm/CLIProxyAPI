package costallocation

import (
	"context"
	"sort"
	"sync"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type recordEntry struct {
	Timestamp  time.Time
	Dimensions DimensionScope
	Metric     UsageMetric
	Provider   string
	Model      string
	APIKey     string
}

// Tracker aggregates usage records across departments, teams, projects, API keys, and time periods.
// It implements coreusage.Plugin.
type Tracker struct {
	mu  sync.RWMutex
	cfg AllocationConfig

	entries []recordEntry

	total       UsageMetric
	departments map[string]*DepartmentStat
	monthly     map[string]*PeriodStat
	quarterly   map[string]*PeriodStat
	annual      map[string]*PeriodStat
}

// NewTracker constructs a new Tracker instance.
func NewTracker(cfg AllocationConfig) *Tracker {
	if cfg.Currency == "" {
		cfg.Currency = "USD"
	}
	t := &Tracker{
		cfg:         cfg,
		departments: make(map[string]*DepartmentStat),
		monthly:     make(map[string]*PeriodStat),
		quarterly:   make(map[string]*PeriodStat),
		annual:      make(map[string]*PeriodStat),
	}
	return t
}

// Enabled returns whether the tracker is active.
func (t *Tracker) Enabled() bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.cfg.Enabled
}

// HandleUsage processes an incoming usage record from the usage manager.
func (t *Tracker) HandleUsage(_ context.Context, record coreusage.Record) {
	if t == nil || !t.Enabled() {
		return
	}

	dims := ResolveDimensions(t.cfg, record)
	breakdown := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType).TokenBreakdown

	var tokenStat TokenStat
	if breakdown.Valid() {
		tokenStat = TokenStat{
			InputTokens:      int64(breakdown.Input.UncachedTokens),
			OutputTokens:     int64(breakdown.Output.NonReasoningTokens + breakdown.Output.ReasoningTokens),
			CacheReadTokens:  int64(breakdown.Input.CacheReadTokens),
			CacheWriteTokens: int64(breakdown.Input.CacheWriteTokens),
			ReasoningTokens:  int64(breakdown.Output.ReasoningTokens),
			TotalTokens:      int64(breakdown.TotalTokens),
		}
	} else if record.Detail.TotalTokens > 0 {
		tokenStat = TokenStat{
			InputTokens:      record.Detail.InputTokens,
			OutputTokens:     record.Detail.OutputTokens,
			CacheReadTokens:  record.Detail.CacheReadTokens,
			CacheWriteTokens: record.Detail.CacheCreationTokens,
			ReasoningTokens:  record.Detail.ReasoningTokens,
			TotalTokens:      record.Detail.TotalTokens,
		}
	}

	var cost float64
	if breakdown.Valid() {
		rate := priceFor(t.cfg.Prices, dims.Provider, dims.Model)
		cost = costForTokens(breakdown, rate)
	}

	metric := UsageMetric{
		Requests: 1,
		Tokens:   tokenStat,
		Cost:     cost,
	}
	if record.Failed {
		metric.FailedRequests = 1
	}

	reqTime := record.RequestedAt
	if reqTime.IsZero() {
		reqTime = time.Now()
	}

	entry := recordEntry{
		Timestamp:  reqTime,
		Dimensions: dims,
		Metric:     metric,
		Provider:   dims.Provider,
		Model:      dims.Model,
		APIKey:     dims.APIKey,
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.entries = append(t.entries, entry)
	t.total.Add(metric)

	// Update department / team / project aggregates
	t.accumulateDepartmentLocked(t.departments, dims, metric)

	// Update period aggregates
	t.accumulatePeriodLocked(t.monthly, PeriodMonthly, reqTime, dims, metric)
	t.accumulatePeriodLocked(t.quarterly, PeriodQuarterly, reqTime, dims, metric)
	t.accumulatePeriodLocked(t.annual, PeriodAnnual, reqTime, dims, metric)
}

func (t *Tracker) accumulateDepartmentLocked(depts map[string]*DepartmentStat, dims DimensionScope, metric UsageMetric) {
	dStat, ok := depts[dims.Department]
	if !ok {
		dStat = &DepartmentStat{
			Department: dims.Department,
			Teams:      make(map[string]TeamStat),
		}
		depts[dims.Department] = dStat
	}
	dStat.Add(metric)

	teamStat, ok := dStat.Teams[dims.Team]
	if !ok {
		teamStat = TeamStat{
			Team:     dims.Team,
			Projects: make(map[string]ProjectStat),
		}
	}
	teamStat.Add(metric)

	projStat, ok := teamStat.Projects[dims.Project]
	if !ok {
		projStat = ProjectStat{
			Project:  dims.Project,
			ByModel:  make(map[string]UsageMetric),
			ByAPIKey: make(map[string]UsageMetric),
		}
	}
	projStat.Add(metric)

	// Model breakdown
	modelKey := dims.Provider + "/" + dims.Model
	mMetric := projStat.ByModel[modelKey]
	mMetric.Add(metric)
	projStat.ByModel[modelKey] = mMetric

	// APIKey breakdown
	apiKeyKey := dims.APIKey
	if apiKeyKey == "" {
		apiKeyKey = "none"
	}
	kMetric := projStat.ByAPIKey[apiKeyKey]
	kMetric.Add(metric)
	projStat.ByAPIKey[apiKeyKey] = kMetric

	teamStat.Projects[dims.Project] = projStat
	dStat.Teams[dims.Team] = teamStat
}

func (t *Tracker) accumulatePeriodLocked(periods map[string]*PeriodStat, pType Period, ts time.Time, dims DimensionScope, metric UsageMetric) {
	key := PeriodKey(pType, ts)
	pStat, ok := periods[key]
	if !ok {
		start, resetAt := PeriodWindow(pType, ts)
		pStat = &PeriodStat{
			PeriodKey:   key,
			PeriodType:  pType,
			PeriodStart: start,
			PeriodEnd:   resetAt,
			Departments: make(map[string]DepartmentStat),
		}
		periods[key] = pStat
	}
	pStat.Add(metric)

	dStat, ok := pStat.Departments[dims.Department]
	if !ok {
		dStat = DepartmentStat{
			Department: dims.Department,
			Teams:      make(map[string]TeamStat),
		}
	}
	dStat.Add(metric)

	teamStat, ok := dStat.Teams[dims.Team]
	if !ok {
		teamStat = TeamStat{
			Team:     dims.Team,
			Projects: make(map[string]ProjectStat),
		}
	}
	teamStat.Add(metric)

	projStat, ok := teamStat.Projects[dims.Project]
	if !ok {
		projStat = ProjectStat{
			Project:  dims.Project,
			ByModel:  make(map[string]UsageMetric),
			ByAPIKey: make(map[string]UsageMetric),
		}
	}
	projStat.Add(metric)

	modelKey := dims.Provider + "/" + dims.Model
	mMetric := projStat.ByModel[modelKey]
	mMetric.Add(metric)
	projStat.ByModel[modelKey] = mMetric

	apiKeyKey := dims.APIKey
	if apiKeyKey == "" {
		apiKeyKey = "none"
	}
	kMetric := projStat.ByAPIKey[apiKeyKey]
	kMetric.Add(metric)
	projStat.ByAPIKey[apiKeyKey] = kMetric

	teamStat.Projects[dims.Project] = projStat
	dStat.Teams[dims.Team] = teamStat
	pStat.Departments[dims.Department] = dStat
}

// Report returns the full current allocation report.
func (t *Tracker) Report() AllocationReport {
	if t == nil {
		return AllocationReport{GeneratedAt: time.Now()}
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	rep := AllocationReport{
		Enabled:     t.cfg.Enabled,
		Currency:    t.cfg.Currency,
		GeneratedAt: time.Now(),
		Total:       t.total,
		Departments: make(map[string]DepartmentStat, len(t.departments)),
		Monthly:     make(map[string]PeriodStat, len(t.monthly)),
		Quarterly:   make(map[string]PeriodStat, len(t.quarterly)),
		Annual:      make(map[string]PeriodStat, len(t.annual)),
	}

	for k, v := range t.departments {
		rep.Departments[k] = *v
	}
	for k, v := range t.monthly {
		rep.Monthly[k] = *v
	}
	for k, v := range t.quarterly {
		rep.Quarterly[k] = *v
	}
	for k, v := range t.annual {
		rep.Annual[k] = *v
	}

	return rep
}

// Summary returns a consolidated summary with rankings.
func (t *Tracker) Summary() SummaryReport {
	if t == nil {
		return SummaryReport{GeneratedAt: time.Now()}
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	sum := SummaryReport{
		Currency:           t.cfg.Currency,
		GeneratedAt:        time.Now(),
		Total:              t.total,
		DepartmentRankings: make([]DepartmentSummaryItem, 0, len(t.departments)),
		ProjectRankings:    make([]ProjectSummaryItem, 0),
	}

	totalSpend := t.total.Cost

	for deptName, dStat := range t.departments {
		pct := 0.0
		if totalSpend > 0 {
			pct = (dStat.Cost / totalSpend) * 100.0
		}
		sum.DepartmentRankings = append(sum.DepartmentRankings, DepartmentSummaryItem{
			Department: deptName,
			Metric:     dStat.UsageMetric,
			Percentage: pct,
		})

		for teamName, teamStat := range dStat.Teams {
			for projName, projStat := range teamStat.Projects {
				projPct := 0.0
				if totalSpend > 0 {
					projPct = (projStat.Cost / totalSpend) * 100.0
				}
				sum.ProjectRankings = append(sum.ProjectRankings, ProjectSummaryItem{
					Department: deptName,
					Team:       teamName,
					Project:    projName,
					Metric:     projStat.UsageMetric,
					Percentage: projPct,
				})
			}
		}
	}

	// Sort rankings descending by cost
	sort.Slice(sum.DepartmentRankings, func(i, j int) bool {
		return sum.DepartmentRankings[i].Metric.Cost > sum.DepartmentRankings[j].Metric.Cost
	})
	sort.Slice(sum.ProjectRankings, func(i, j int) bool {
		return sum.ProjectRankings[i].Metric.Cost > sum.ProjectRankings[j].Metric.Cost
	})

	return sum
}

// Departments returns list of department names with current metrics.
func (t *Tracker) Departments() []DepartmentSummaryItem {
	return t.Summary().DepartmentRankings
}

// Reset clears recorded in-memory data.
func (t *Tracker) Reset() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = nil
	t.total = UsageMetric{}
	t.departments = make(map[string]*DepartmentStat)
	t.monthly = make(map[string]*PeriodStat)
	t.quarterly = make(map[string]*PeriodStat)
	t.annual = make(map[string]*PeriodStat)
}
