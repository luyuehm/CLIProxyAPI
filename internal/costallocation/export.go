package costallocation

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strconv"
	"strings"
)

// ExportCSV generates a CSV representation of the allocation statistics.
func (t *Tracker) ExportCSV(opts QueryOptions) ([]byte, error) {
	if t == nil {
		return nil, fmt.Errorf("tracker is nil")
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)

	currency := t.cfg.Currency
	if currency == "" {
		currency = "USD"
	}

	// Write CSV Header
	header := []string{
		"Period",
		"Department",
		"Team",
		"Project",
		"API Key",
		"Provider",
		"Model",
		"Requests",
		"Failed Requests",
		"Input Tokens",
		"Output Tokens",
		"Cache Read Tokens",
		"Cache Write Tokens",
		"Reasoning Tokens",
		"Total Tokens",
		fmt.Sprintf("Cost (%s)", currency),
	}
	if err := w.Write(header); err != nil {
		return nil, err
	}

	// Iterate over recorded entries (or aggregated items) matching query options
	for _, entry := range t.entries {
		if opts.Department != "" && !strings.EqualFold(entry.Dimensions.Department, opts.Department) {
			continue
		}
		if opts.Team != "" && !strings.EqualFold(entry.Dimensions.Team, opts.Team) {
			continue
		}
		if opts.Project != "" && !strings.EqualFold(entry.Dimensions.Project, opts.Project) {
			continue
		}
		if opts.APIKey != "" && !strings.EqualFold(entry.Dimensions.APIKey, opts.APIKey) {
			continue
		}
		if opts.Provider != "" && !strings.EqualFold(entry.Provider, opts.Provider) {
			continue
		}
		if opts.Model != "" && !strings.EqualFold(entry.Model, opts.Model) {
			continue
		}

		periodStr := PeriodKey(opts.PeriodType, entry.Timestamp)
		if opts.PeriodKey != "" && !strings.EqualFold(periodStr, opts.PeriodKey) {
			continue
		}

		apiKeyStr := entry.APIKey
		if apiKeyStr == "" {
			apiKeyStr = "none"
		}

		row := []string{
			periodStr,
			entry.Dimensions.Department,
			entry.Dimensions.Team,
			entry.Dimensions.Project,
			apiKeyStr,
			entry.Provider,
			entry.Model,
			strconv.FormatInt(entry.Metric.Requests, 10),
			strconv.FormatInt(entry.Metric.FailedRequests, 10),
			strconv.FormatInt(entry.Metric.Tokens.InputTokens, 10),
			strconv.FormatInt(entry.Metric.Tokens.OutputTokens, 10),
			strconv.FormatInt(entry.Metric.Tokens.CacheReadTokens, 10),
			strconv.FormatInt(entry.Metric.Tokens.CacheWriteTokens, 10),
			strconv.FormatInt(entry.Metric.Tokens.ReasoningTokens, 10),
			strconv.FormatInt(entry.Metric.Tokens.TotalTokens, 10),
			fmt.Sprintf("%.6f", entry.Metric.Cost),
		}
		if err := w.Write(row); err != nil {
			return nil, err
		}
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
