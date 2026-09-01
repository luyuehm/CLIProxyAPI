// Package contentfilter: export_http.go implements RIC-443, the KEEPER-facing
// HTTP download endpoint for the audit log export built in RIC-440.
//
// The handler lives in this package (not internal/api/handlers/management)
// because internal/contentfilter already imports internal/api for ServerOption
// — a handler in management that imported contentfilter would be an import
// cycle. It is injected into the /v0/management group via
// api.WithContentFilterExportHandler (wired in sdk/cliproxy/builder.go), so
// it rides the same management-key auth as every other management route.
//
// Privacy: the audit table stores the original unmasked body in
// raw_preview (see middleware.enqueueAudit — raw is the unredacted input).
// Exporting that verbatim would leak the very text the filter is protecting.
// RIC-443 therefore re-masks raw_preview / matches with the live rule engine
// at export time (CONTEXT_RULE: 导出数据保持脱敏，不泄露敏感原文).
package contentfilter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ExportHTTPHandler builds the RIC-443 export handler. The handler opens the
// KEEPER database read-only once per request, loads the latest rules from the
// same handle (readRulesFromOpenDB) so previews are re-masked with the current
// rule set, then streams the matching rows to the response.
func ExportHTTPHandler(src ExportSource) gin.HandlerFunc {
	engine := NewEngine(true) // outbound partial masking keeps previews readable
	return func(c *gin.Context) {
		filter, err := exportFilterFromQuery(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		format := ExportFormat(strings.ToLower(strings.TrimSpace(c.Query("format"))))
		if format == "" {
			format = ExportCSV
		}
		if format != ExportCSV && format != ExportJSON && format != ExportJSONL {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("unsupported export format %q (csv|json|jsonl)", c.Query("format"))})
			return
		}

		db, closer, err := openExportSource(src)
		if err != nil {
			logger.WithError(err).Warn("content filter export: open source failed")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "export source unavailable: " + err.Error()})
			return
		}
		defer closer()

		rules, err := readRulesFromOpenDB(db)
		if err != nil {
			logger.WithError(err).Warn("content filter export: load rules failed")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "failed to load content filter rules: " + err.Error()})
			return
		}
		filter.Masker = exportMasker(engine, rules)

		c.Header("Content-Type", exportContentType(format))
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"content-filter-audit-%s.%s\"",
			time.Now().UTC().Format("20060102-150405"), string(format)))

		written, err := StreamExportLogs(c.Writer, db, filter, format)
		if err != nil {
			logger.WithError(err).Warn("content filter export: stream failed")
			// The response may already be partially written; report the error
			// in the body where possible.
			return
		}
		logger.WithField("rows", written).Info("content filter export completed")
	}
}

// exportFilterFromQuery maps HTTP query params to an ExportFilter. All params
// are optional; an empty query exports the full table (bounded by limit).
func exportFilterFromQuery(c *gin.Context) (ExportFilter, error) {
	var f ExportFilter
	var err error
	if v := strings.TrimSpace(c.Query("since")); v != "" {
		f.Since, err = time.Parse(time.RFC3339, v)
		if err != nil {
			return f, fmt.Errorf("invalid since (want RFC3339, e.g. 2026-08-01T00:00:00Z): %v", err)
		}
	}
	if v := strings.TrimSpace(c.Query("until")); v != "" {
		f.Until, err = time.Parse(time.RFC3339, v)
		if err != nil {
			return f, fmt.Errorf("invalid until (want RFC3339): %v", err)
		}
	}
	if v := strings.TrimSpace(c.Query("limit")); v != "" {
		f.Limit, err = strconv.Atoi(v)
		if err != nil || f.Limit < 0 {
			return f, fmt.Errorf("invalid limit %q", v)
		}
	}
	if v := strings.TrimSpace(c.Query("rule_id")); v != "" {
		f.RuleID, err = strconv.ParseInt(v, 10, 64)
		if err != nil || f.RuleID <= 0 {
			return f, fmt.Errorf("invalid rule_id %q", v)
		}
	}
	f.Model = strings.TrimSpace(c.Query("model"))
	f.UserID = strings.TrimSpace(c.Query("user_id"))
	f.ClientIP = strings.TrimSpace(c.Query("client_ip"))
	f.FilterType = strings.TrimSpace(c.Query("filter_type"))
	return f, nil
}

// exportMasker returns a Masker that re-masks the sensitive export columns
// with the live rule engine. raw_preview holds the original (unmasked) body,
// so it is re-masked outbound-style — the exported file shows the same masked
// preview the UI shows. matches is a JSON array of the raw detected values
// (phone numbers, id cards, …); each value is re-masked individually so the
// download never contains a sensitive original value verbatim.
func exportMasker(engine *Engine, rules []*Rule) func(*ExportRecord) {
	return func(rec *ExportRecord) {
		if rec == nil {
			return
		}
		if len(rules) == 0 {
			// No rules loaded — keep the stored (already truncated) preview
			// rather than failing the whole export.
			return
		}
		// Re-mask the raw preview so sensitive text is not exported verbatim.
		// The filtered preview is already the masked text.
		res := engine.Apply(rules, rec.RawPreview, false, rec.Model)
		if res.Changed {
			rec.RawPreview = res.Text
		}
		// Mask each stored matched value. The RIC-440+ format stores a JSON
		// array; a parse failure (e.g. legacy CSV from RIC-438) leaves the
		// column as-is rather than failing the whole export.
		var stored []string
		if err := json.Unmarshal([]byte(rec.Matches), &stored); err == nil && len(stored) > 0 {
			masked := make([]string, 0, len(stored))
			for _, v := range stored {
				mr := engine.Apply(rules, v, false, rec.Model)
				if mr.Changed {
					masked = append(masked, mr.Text)
				} else {
					masked = append(masked, v)
				}
			}
			rec.Matches = MarshalMatchesJSON(masked)
		}
	}
}

// exportContentType maps an export format to its HTTP Content-Type.
func exportContentType(format ExportFormat) string {
	switch format {
	case ExportCSV:
		return "text/csv; charset=utf-8"
	case ExportJSONL:
		return "application/x-ndjson; charset=utf-8"
	default:
		return "application/json; charset=utf-8"
	}
}
