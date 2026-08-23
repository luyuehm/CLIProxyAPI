package api

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"cpa-usage-keeper/internal/service"
	servicedto "cpa-usage-keeper/internal/service/dto"
	"cpa-usage-keeper/internal/timeutil"
)

// requestLogDetailProvider fetches a single CPA request log by request ID.
type requestLogDetailProvider interface {
	FetchRequestLogByID(ctx context.Context, requestID string) ([]byte, int, error)
}

type auditLogTokenPayload struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	ReasoningTokens     int64 `json:"reasoning_tokens"`
	CachedTokens        int64 `json:"cached_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
}

type auditLogPayload struct {
	ID          string               `json:"id,omitempty"`
	Timestamp   string               `json:"timestamp"`
	APIKey      string               `json:"api_key,omitempty"`
	Provider    string               `json:"provider,omitempty"`
	Source      string               `json:"source"`
	AuthIndex   string               `json:"auth_index,omitempty"`
	Model       string               `json:"model"`
	StatusCode  int                  `json:"status_code"`
	Failed      bool                 `json:"failed"`
	LatencyMS   int64                `json:"latency_ms"`
	TTFTMS      *int64               `json:"ttft_ms,omitempty"`
	Tokens      auditLogTokenPayload `json:"tokens"`
	CostUSD     float64              `json:"cost_usd"`
	CostAllowed bool                 `json:"cost_allowed"`
}

func buildAuditLogPayload(row servicedto.UsageEventRecord) auditLogPayload {
	id := ""
	if row.ID != 0 {
		id = strconv.FormatInt(row.ID, 10)
	}
	statusCode := row.StatusCode
	if statusCode <= 0 {
		statusCode = 200
	}
	return auditLogPayload{
		ID:         id,
		Timestamp:  timeutil.FormatStorageTime(row.Timestamp),
		APIKey:     row.APIGroupKey,
		Provider:   row.Provider,
		Source:     row.Source,
		AuthIndex:  row.AuthIndex,
		Model:      row.Model,
		StatusCode: statusCode,
		Failed:     row.Failed,
		LatencyMS:  row.LatencyMS,
		TTFTMS:     row.TTFTMS,
		Tokens: auditLogTokenPayload{
			InputTokens:         row.InputTokens,
			OutputTokens:        row.OutputTokens,
			ReasoningTokens:     row.ReasoningTokens,
			CachedTokens:        row.CachedTokens,
			CacheReadTokens:     row.CacheReadTokens,
			CacheCreationTokens: row.CacheCreationTokens,
			TotalTokens:         row.TotalTokens,
		},
		CostUSD:     row.CostUSD,
		CostAllowed: row.CostAvailable,
	}
}

func registerAuditLogsRoute(
	router gin.IRoutes,
	usageProvider service.UsageProvider,
	cpaClient requestLogDetailProvider,
) {
	router.GET("/audit/logs", func(c *gin.Context) {
		filter, err := parseAuditLogFilterQuery(c.Request)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		exportFormat := strings.TrimSpace(c.Query("export"))
		if exportFormat != "" && exportFormat != "csv" && exportFormat != "json" {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("unsupported export format %q", exportFormat)})
			return
		}
		if exportFormat != "" {
			// 导出不走分页，一次命中筛选范围内的结果。
			filter.Page = 1
			filter.Offset = 0
			filter.PageSize = 1000
			filter.Limit = 1000
		}

		rows, err := usageProvider.ListUsageEvents(c.Request.Context(), filter)
		if err != nil {
			writeInternalError(c, "list audit logs failed", err)
			return
		}

		payloads := make([]auditLogPayload, 0, len(rows.Events))
		for _, row := range rows.Events {
			payloads = append(payloads, buildAuditLogPayload(row))
		}

		if exportFormat != "" {
			writeAuditLogExport(c, payloads, exportFormat)
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"events":      payloads,
			"total_count": rows.TotalCount,
			"page":        rows.Page,
			"page_size":   rows.PageSize,
			"total_pages": rows.TotalPages,
		})
	})

	router.GET("/audit/logs/:id/request-log", func(c *gin.Context) {
		if cpaClient == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "cpa request log access unavailable"})
			return
		}
		requestID := strings.TrimSpace(c.Param("id"))
		if requestID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "missing request id"})
			return
		}
		body, statusCode, err := cpaClient.FetchRequestLogByID(c.Request.Context(), requestID)
		if err != nil {
			if statusCode == http.StatusNotFound {
				c.JSON(http.StatusNotFound, gin.H{"error": "request log not found"})
				return
			}
			writeInternalError(c, "fetch request log failed", err)
			return
		}
		c.Data(http.StatusOK, "text/plain; charset=utf-8", body)
	})
}

func parseAuditLogFilterQuery(req *http.Request) (servicedto.UsageFilter, error) {
	if req == nil {
		return servicedto.UsageFilter{}, nil
	}
	filter, err := parseUsageFilterQuery(req, timeutil.NormalizeStorageTime(time.Now()))
	if err != nil {
		return servicedto.UsageFilter{}, err
	}
	if filter.PageSize == 0 {
		filter.PageSize = servicedto.DefaultUsageEventsLimit
	}
	// 审计日志状态筛选支持 2xx/3xx/4xx/5xx 分组，也支持精确 status_code。
	statusGroup := strings.TrimSpace(req.URL.Query().Get("status_group"))
	if statusGroup != "" {
		switch statusGroup {
		case "2xx", "3xx", "4xx", "5xx":
			filter.StatusGroup = statusGroup
		case "1xx":
			filter.StatusGroup = "1xx"
		default:
			return filter, fmt.Errorf("invalid status_group %q", statusGroup)
		}
	}
	return filter, nil
}

func writeAuditLogExport(c *gin.Context, events []auditLogPayload, format string) {
	switch strings.ToLower(format) {
	case "csv":
		writeAuditLogCSV(c, events)
	case "json":
		writeAuditLogJSON(c, events)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("unsupported export format %q", format)})
	}
}

func writeAuditLogCSV(c *gin.Context, events []auditLogPayload) {
	if events == nil {
		events = []auditLogPayload{}
	}
	c.Header("Content-Disposition", `attachment; filename="audit-logs.csv"`)
	c.Header("Content-Type", "text/csv; charset=utf-8")
	writer := csv.NewWriter(c.Writer)
	_ = writer.Write([]string{
		"timestamp", "api_key", "provider", "source", "auth_index", "model",
		"status_code", "failed", "latency_ms", "ttft_ms",
		"input_tokens", "output_tokens", "reasoning_tokens",
		"cached_tokens", "cache_read_tokens", "cache_creation_tokens",
		"total_tokens", "cost_usd",
	})
	for _, event := range events {
		ttft := ""
		if event.TTFTMS != nil {
			ttft = strconv.FormatInt(*event.TTFTMS, 10)
		}
		_ = writer.Write([]string{
			event.Timestamp,
			event.APIKey,
			event.Provider,
			event.Source,
			event.AuthIndex,
			event.Model,
			strconv.Itoa(event.StatusCode),
			strconv.FormatBool(event.Failed),
			strconv.FormatInt(event.LatencyMS, 10),
			ttft,
			strconv.FormatInt(event.Tokens.InputTokens, 10),
			strconv.FormatInt(event.Tokens.OutputTokens, 10),
			strconv.FormatInt(event.Tokens.ReasoningTokens, 10),
			strconv.FormatInt(event.Tokens.CachedTokens, 10),
			strconv.FormatInt(event.Tokens.CacheReadTokens, 10),
			strconv.FormatInt(event.Tokens.CacheCreationTokens, 10),
			strconv.FormatInt(event.Tokens.TotalTokens, 10),
			strconv.FormatFloat(event.CostUSD, 'f', 6, 64),
		})
	}
	writer.Flush()
}

func writeAuditLogJSON(c *gin.Context, events []auditLogPayload) {
	if events == nil {
		events = []auditLogPayload{}
	}
	c.Header("Content-Disposition", `attachment; filename="audit-logs.json"`)
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.JSON(http.StatusOK, events)
}