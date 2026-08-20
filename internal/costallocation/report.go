package costallocation

import "time"

// TokenStat holds token count metrics.
type TokenStat struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// Add adds another TokenStat.
func (s *TokenStat) Add(other TokenStat) {
	s.InputTokens += other.InputTokens
	s.OutputTokens += other.OutputTokens
	s.CacheReadTokens += other.CacheReadTokens
	s.CacheWriteTokens += other.CacheWriteTokens
	s.ReasoningTokens += other.ReasoningTokens
	s.TotalTokens += other.TotalTokens
}

// UsageMetric holds aggregated request count, tokens, and calculated cost.
type UsageMetric struct {
	Requests       int64     `json:"requests"`
	FailedRequests int64     `json:"failed_requests"`
	Tokens         TokenStat `json:"tokens"`
	Cost           float64   `json:"cost"`
}

// Add adds another UsageMetric.
func (m *UsageMetric) Add(other UsageMetric) {
	m.Requests += other.Requests
	m.FailedRequests += other.FailedRequests
	m.Tokens.Add(other.Tokens)
	m.Cost += other.Cost
}

// ProjectStat holds statistics for a project under a team and department.
type ProjectStat struct {
	Project        string                 `json:"project"`
	UsageMetric                           `json:",inline"`
	ByModel        map[string]UsageMetric `json:"by_model,omitempty"`
	ByAPIKey       map[string]UsageMetric `json:"by_api_key,omitempty"`
}

// TeamStat holds statistics for a team under a department.
type TeamStat struct {
	Team        string                 `json:"team"`
	UsageMetric                        `json:",inline"`
	Projects    map[string]ProjectStat `json:"projects,omitempty"`
}

// DepartmentStat holds statistics for a department.
type DepartmentStat struct {
	Department  string              `json:"department"`
	UsageMetric                     `json:",inline"`
	Teams       map[string]TeamStat `json:"teams,omitempty"`
}

// PeriodStat holds aggregated statistics for a specific time period.
type PeriodStat struct {
	PeriodKey   string                    `json:"period_key"`
	PeriodType  Period                    `json:"period_type"`
	PeriodStart time.Time                 `json:"period_start"`
	PeriodEnd   time.Time                 `json:"period_end"`
	UsageMetric                           `json:",inline"`
	Departments map[string]DepartmentStat `json:"departments,omitempty"`
}

// AllocationReport is the root report snapshot.
type AllocationReport struct {
	Enabled     bool                      `json:"enabled"`
	Currency    string                    `json:"currency"`
	GeneratedAt time.Time                 `json:"generated_at"`
	Total       UsageMetric               `json:"total"`
	Departments map[string]DepartmentStat `json:"departments"`
	Monthly     map[string]PeriodStat     `json:"monthly,omitempty"`
	Quarterly   map[string]PeriodStat     `json:"quarterly,omitempty"`
	Annual      map[string]PeriodStat     `json:"annual,omitempty"`
}

// DepartmentSummaryItem represents ranking entry in summary view.
type DepartmentSummaryItem struct {
	Department string      `json:"department"`
	Metric     UsageMetric `json:"metric"`
	Percentage float64     `json:"percentage"`
}

// ProjectSummaryItem represents ranking entry for projects in summary view.
type ProjectSummaryItem struct {
	Department string      `json:"department"`
	Team       string      `json:"team"`
	Project    string      `json:"project"`
	Metric     UsageMetric `json:"metric"`
	Percentage float64     `json:"percentage"`
}

// SummaryReport provides a consolidated high-level overview.
type SummaryReport struct {
	Currency           string                  `json:"currency"`
	GeneratedAt        time.Time               `json:"generated_at"`
	Total              UsageMetric             `json:"total"`
	DepartmentRankings []DepartmentSummaryItem `json:"department_rankings"`
	ProjectRankings    []ProjectSummaryItem    `json:"project_rankings"`
}

// QueryOptions filters and scopes a query.
type QueryOptions struct {
	PeriodType Period
	PeriodKey  string
	Department string
	Team       string
	Project    string
	APIKey     string
	Provider   string
	Model      string
}
