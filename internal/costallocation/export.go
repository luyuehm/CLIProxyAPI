package costallocation

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"sort"
)

// ExportCSV generates a CSV-formatted cost allocation report with rows per
// department per period. The columns are:
//
//	department, period, spend, request_count, input_tokens, output_tokens, total_tokens
func ExportCSV(summaries []DepartmentPeriodSummary) ([]byte, error) {
	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)

	// Header row
	if err := writer.Write([]string{
		"department", "period", "spend", "request_count",
		"input_tokens", "output_tokens", "total_tokens",
	}); err != nil {
		return nil, fmt.Errorf("write csv header: %w", err)
	}

	// Sort by department name then period key for deterministic output.
	type row struct {
		department string
		periodKey  string
		summary    PeriodSummary
	}

	var rows []row
	for _, dp := range summaries {
		for _, ps := range dp.Periods {
			rows = append(rows, row{department: dp.DepartmentName, periodKey: ps.PeriodKey, summary: ps})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].department != rows[j].department {
			return rows[i].department < rows[j].department
		}
		return rows[i].periodKey < rows[j].periodKey
	})

	for _, r := range rows {
		record := []string{
			r.department,
			r.periodKey,
			formatFloat(r.summary.Spend, 4),
			formatInt64(r.summary.RequestCount),
			formatInt64(r.summary.InputTokens),
			formatInt64(r.summary.OutputTokens),
			formatInt64(r.summary.TotalTokens),
		}
		if err := writer.Write(record); err != nil {
			return nil, fmt.Errorf("write csv row: %w", err)
		}
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, fmt.Errorf("csv flush: %w", err)
	}
	return buf.Bytes(), nil
}

// formatFloat formats a float64 with up to n decimal places, stripping
// trailing zeros.
func formatFloat(v float64, precision int) string {
	if v == 0 {
		return "0"
	}
	format := fmt.Sprintf("%%.%df", precision)
	s := fmt.Sprintf(format, v)
	// Strip trailing zeros (and trailing decimal point).
	for len(s) > 0 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}
	return s
}

func formatInt64(n int64) string {
	if n == 0 {
		return "0"
	}
	return fmt.Sprintf("%d", n)
}
