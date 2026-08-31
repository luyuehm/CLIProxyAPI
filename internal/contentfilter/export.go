// Package contentfilter: export.go implements RIC-440 audit log export.
// Operators run `cli-proxy-api contentfilter-export -format csv -since
// 2026-08-01 -until 2026-08-31 -out ./audit.csv` to dump a date-bounded
// slice of content_filter_logs to a local file. The same ExportLogs API is
// available programmatically so the KEEPER admin UI can offer a "Download
// audit" link that streams the result.
package contentfilter

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ExportFormat selects the on-disk format for ExportLogs. JSONL and CSV
// are streaming-friendly; JSON is a single document that loads the entire
// result into memory and is best for small windows.
type ExportFormat string

const (
	ExportCSV   ExportFormat = "csv"
	ExportJSON  ExportFormat = "json"
	ExportJSONL ExportFormat = "jsonl"
)

// ExportFilter is the WHERE clause criteria for ExportLogs. Zero values
// mean "no bound on that field".
type ExportFilter struct {
	Since time.Time
	Until time.Time
	// RuleID, Model, UserID, ClientIP, FilterType are exact-match filters
	// applied to the corresponding column. Empty string means no filter.
	RuleID     int64
	Model      string
	UserID     string
	ClientIP   string
	FilterType string
	// Limit caps the number of rows returned; 0 means unlimited. The
	// underlying query is paginated with OFFSET in chunks of ChunkSize so
	// a small Limit + large DB does not blow up memory.
	Limit int
	// ChunkSize is the row chunk used for streaming. Default 1000 when 0.
	ChunkSize int
}

// ExportResult summarises a single export call. Rows is the number of rows
// actually written; Format is the requested format; Path is the file the
// rows were written to.
type ExportResult struct {
	Rows   int
	Format ExportFormat
	Path   string
}

// ExportSource identifies where ExportLogs should read rows from. Both
// fields are optional; the sidecar is used when HostDBPath is empty or
// fails to open.
type ExportSource struct {
	HostDBPath      string
	ContainerName   string
	ContainerDBPath string
	DockerCmd       string
	// SidecarPath is the local audit sidecar. Used as a fallback when the
	// KEEPER db is unavailable.
	SidecarPath string
	// DockerCopyTimeout bounds the cp-out operation when reading from
	// a container. Default 10s.
	DockerCopyTimeout time.Duration
}

// ExportLogs reads content_filter_logs from the KEEPER database (or sidecar
// fallback) and writes the matching rows to outPath in the chosen format.
func ExportLogs(src ExportSource, filter ExportFilter, format ExportFormat, outPath string) (ExportResult, error) {
	if format == "" {
		format = ExportCSV
	}
	format = ExportFormat(strings.ToLower(string(format)))
	if format != ExportCSV && format != ExportJSON && format != ExportJSONL {
		return ExportResult{}, fmt.Errorf("contentfilter: unsupported export format %q", format)
	}

	f, err := os.Create(outPath)
	if err != nil {
		return ExportResult{}, fmt.Errorf("contentfilter: create export file: %w", err)
	}
	defer f.Close()

	db, closer, err := openExportSource(src)
	if err != nil {
		return ExportResult{}, err
	}
	defer closer()

	rows, err := streamExportRows(f, db, filter, format)
	if err != nil {
		return ExportResult{}, err
	}
	return ExportResult{Rows: rows, Format: format, Path: outPath}, nil
}

func openExportSource(src ExportSource) (*sql.DB, func(), error) {
	timeout := src.DockerCopyTimeout
	if timeout <= 0 {
		timeout = DefaultDockerCopyTimeout
	}
	if src.HostDBPath != "" {
		if _, err := os.Stat(src.HostDBPath); err == nil {
			db, err := openExportKEEPER(src.HostDBPath)
			if err == nil {
				return db, func() { _ = db.Close() }, nil
			}
		}
	}
	if src.ContainerName != "" {
		tmp, err := copyExportDBViaDocker(src, timeout)
		if err == nil {
			db, err := openExportKEEPER(tmp)
			if err == nil {
				return db, func() { _ = db.Close(); _ = os.Remove(tmp) }, nil
			}
			_ = os.Remove(tmp)
		}
	}
	if src.SidecarPath != "" {
		if _, err := os.Stat(src.SidecarPath); err == nil {
			db, err := openExportKEEPER(src.SidecarPath)
			if err == nil {
				return db, func() { _ = db.Close() }, nil
			}
		}
	}
	return nil, func() {}, fmt.Errorf("contentfilter: no export source available (set HostDBPath or SidecarPath)")
}

func openExportKEEPER(path string) (*sql.DB, error) {
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=busy_timeout%3d5000"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func copyExportDBViaDocker(src ExportSource, timeout time.Duration) (string, error) {
	tmp, err := os.CreateTemp("", "keeper-export-*.db")
	if err != nil {
		return "", fmt.Errorf("contentfilter: create temp db copy: %v", err)
	}
	_ = tmp.Close()
	_ = os.Remove(tmp.Name())
	cmd := src.DockerCmd
	if cmd == "" {
		cmd = "docker"
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c := exec.CommandContext(ctx, cmd, "cp",
		fmt.Sprintf("%s:%s", src.ContainerName, src.ContainerDBPath), tmp.Name())
	if out, err := c.CombinedOutput(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("contentfilter: docker cp export: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return tmp.Name(), nil
}

// streamExportRows is split out for testability: the production path calls
// it with an os.File, tests can pass a bytes.Buffer.
func streamExportRows(w io.Writer, db *sql.DB, filter ExportFilter, format ExportFormat) (int, error) {
	chunkSize := filter.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 1000
	}
	args := []any{}
	where := []string{}
	if !filter.Since.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, filter.Since.UTC().Format("2006-01-02 15:04:05"))
	}
	if !filter.Until.IsZero() {
		where = append(where, "created_at < ?")
		args = append(args, filter.Until.UTC().Format("2006-01-02 15:04:05"))
	}
	if filter.RuleID > 0 {
		where = append(where, "rule_id = ?")
		args = append(args, filter.RuleID)
	}
	if filter.Model != "" {
		where = append(where, "model = ?")
		args = append(args, filter.Model)
	}
	if filter.UserID != "" {
		where = append(where, "user_id = ?")
		args = append(args, filter.UserID)
	}
	if filter.ClientIP != "" {
		where = append(where, "client_ip = ?")
		args = append(args, filter.ClientIP)
	}
	if filter.FilterType != "" {
		where = append(where, "filter_type = ?")
		args = append(args, filter.FilterType)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	csvW := newCSVWriter(w)
	jsonW := newJSONWriter(w, format)
	first := true
	written := 0
	remaining := filter.Limit
	offset := 0
	for {
		pageSize := chunkSize
		if filter.Limit > 0 && remaining < pageSize {
			pageSize = remaining
		}
		rows, err := db.QueryContext(context.Background(),
			"SELECT id, rule_id, IFNULL(rule_name,''), IFNULL(filter_type,''), match_count, IFNULL(matches,''), action, IFNULL(model,''), IFNULL(client_ip,''), IFNULL(user_id,''), IFNULL(raw_preview,''), IFNULL(filtered_preview,''), IFNULL(created_at,'') FROM content_filter_logs"+clause+" ORDER BY id ASC LIMIT ? OFFSET ?",
			append(args, pageSize, offset)...)
		if err != nil {
			return written, fmt.Errorf("contentfilter: export query: %w", err)
		}
		pageRows := 0
		for rows.Next() {
			rec, err := scanExportRow(rows)
			if err != nil {
				rows.Close()
				return written, err
			}
			switch format {
			case ExportCSV:
				if err := csvW.Write(rec); err != nil {
					rows.Close()
					return written, err
				}
			case ExportJSON:
				if err := jsonW.WriteObject(rec, &first); err != nil {
					rows.Close()
					return written, err
				}
			case ExportJSONL:
				if err := jsonW.WriteLine(rec); err != nil {
					rows.Close()
					return written, err
				}
			}
			pageRows++
			written++
			if filter.Limit > 0 {
				remaining--
				if remaining <= 0 {
					break
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return written, err
		}
		if pageRows < pageSize {
			break
		}
		offset += pageRows
		if filter.Limit > 0 && remaining <= 0 {
			break
		}
	}
	switch format {
	case ExportCSV:
		csvW.Flush()
		if err := csvW.Error(); err != nil {
			return written, err
		}
	case ExportJSON:
		if err := jsonW.Close(); err != nil {
			return written, err
		}
	}
	return written, nil
}

// ExportRecord is the row layout written by ExportLogs.
type ExportRecord struct {
	ID              int64  `json:"id" csv:"id"`
	RuleID          int64  `json:"rule_id" csv:"rule_id"`
	RuleName        string `json:"rule_name" csv:"rule_name"`
	FilterType      string `json:"filter_type" csv:"filter_type"`
	MatchCount      int    `json:"match_count" csv:"match_count"`
	Matches         string `json:"matches" csv:"matches"`
	Action          string `json:"action" csv:"action"`
	Model           string `json:"model" csv:"model"`
	ClientIP        string `json:"client_ip" csv:"client_ip"`
	UserID          string `json:"user_id" csv:"user_id"`
	RawPreview      string `json:"raw_preview" csv:"raw_preview"`
	FilteredPreview string `json:"filtered_preview" csv:"filtered_preview"`
	CreatedAt       string `json:"created_at" csv:"created_at"`
}

var exportRecordCSVHeader = []string{
	"id", "rule_id", "rule_name", "filter_type", "match_count",
	"matches", "action", "model", "client_ip", "user_id",
	"raw_preview", "filtered_preview", "created_at",
}

func scanExportRow(rows *sql.Rows) (ExportRecord, error) {
	var r ExportRecord
	if err := rows.Scan(&r.ID, &r.RuleID, &r.RuleName, &r.FilterType,
		&r.MatchCount, &r.Matches, &r.Action, &r.Model, &r.ClientIP,
		&r.UserID, &r.RawPreview, &r.FilteredPreview, &r.CreatedAt); err != nil {
		return r, fmt.Errorf("contentfilter: scan export row: %w", err)
	}
	return r, nil
}

type csvWriter struct {
	w      *csv.Writer
	once   bool
	header []string
}

func newCSVWriter(out io.Writer) *csvWriter {
	return &csvWriter{
		w:      csv.NewWriter(out),
		header: exportRecordCSVHeader,
	}
}

func (c *csvWriter) Write(r ExportRecord) error {
	if !c.once {
		if err := c.w.Write(c.header); err != nil {
			return err
		}
		c.once = true
	}
	row := []string{
		fmtInt(r.ID), fmtInt(r.RuleID), r.RuleName, r.FilterType,
		fmtInt(int64(r.MatchCount)), r.Matches, r.Action, r.Model, r.ClientIP,
		r.UserID, r.RawPreview, r.FilteredPreview, r.CreatedAt,
	}
	return c.w.Write(row)
}

func (c *csvWriter) Flush()       { c.w.Flush() }
func (c *csvWriter) Error() error { return c.w.Error() }

type jsonWriter struct {
	w        io.Writer
	format   ExportFormat
	wroteAny bool
}

func newJSONWriter(out io.Writer, format ExportFormat) *jsonWriter {
	w := &jsonWriter{w: out, format: format}
	if format == ExportJSON {
		_, _ = io.WriteString(out, "[\n")
	}
	return w
}

func (j *jsonWriter) WriteObject(r ExportRecord, first *bool) error {
	j.wroteAny = true
	if !*first {
		if _, err := io.WriteString(j.w, ",\n"); err != nil {
			return err
		}
	}
	*first = false
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = j.w.Write(b)
	return err
}

func (j *jsonWriter) WriteLine(r ExportRecord) error {
	j.wroteAny = true
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := j.w.Write(b); err != nil {
		return err
	}
	_, err = io.WriteString(j.w, "\n")
	return err
}

func (j *jsonWriter) Close() error {
	if j.format == ExportJSON {
		if j.wroteAny {
			_, _ = io.WriteString(j.w, "\n]\n")
		} else {
			_, _ = io.WriteString(j.w, "]\n")
		}
	}
	return nil
}

func fmtInt(v int64) string {
	if v == 0 {
		return ""
	}
	return fmt.Sprintf("%d", v)
}
