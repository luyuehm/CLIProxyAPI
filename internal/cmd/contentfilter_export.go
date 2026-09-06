// DoContentFilterExport dumps the KEEPER content_filter_logs table to a
// local file. RIC-440 (2026-08-31) adds the operator-facing CLI:
// `cli-proxy-api contentfilter-export -format csv -since 2026-08-01
//
//	-until 2026-08-31 -out ./audit.csv -limit 100000`.
//
// The function honours the same env vars as the live filter
// (CPA_CONTENT_FILTER_KEEPER_HOST_DB, *_CONTAINER, *_DB_PATH) so an
// operator can run the export against the same KEEPER instance the
// gateway writes to without re-specifying paths.
package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/contentfilter"
)

// ContentFilterExportOptions configures DoContentFilterExport.
type ContentFilterExportOptions struct {
	Format     string    // csv | json | jsonl (default csv)
	Since      time.Time // inclusive; zero means unbounded
	Until      time.Time // exclusive; zero means unbounded
	Out        string    // output file path; required
	Limit      int       // 0 means unlimited
	Model      string    // exact-match filter
	UserID     string    // exact-match filter
	ClientIP   string    // exact-match filter
	FilterType string    // inbound|outbound
}

// DoContentFilterExport reads content_filter_logs from KEEPER and writes
// the matching rows to options.Out. The function exits the process on
// fatal errors because it is invoked as a top-level CLI command.
func DoContentFilterExport(options ContentFilterExportOptions) {
	if strings.TrimSpace(options.Out) == "" {
		fmt.Fprintln(os.Stderr, "contentfilter-export: -out is required")
		os.Exit(2)
	}
	format := contentfilter.ExportFormat(strings.ToLower(strings.TrimSpace(options.Format)))
	if format == "" {
		format = contentfilter.ExportCSV
	}
	if format != contentfilter.ExportCSV &&
		format != contentfilter.ExportJSON &&
		format != contentfilter.ExportJSONL {
		fmt.Fprintf(os.Stderr, "contentfilter-export: unsupported format %q (csv|json|jsonl)\n", options.Format)
		os.Exit(2)
	}

	src := contentfilter.ExportSource{
		HostDBPath:      strings.TrimSpace(os.Getenv("CPA_CONTENT_FILTER_KEEPER_HOST_DB")),
		ContainerName:   strings.TrimSpace(os.Getenv("CPA_CONTENT_FILTER_KEEPER_CONTAINER")),
		ContainerDBPath: strings.TrimSpace(os.Getenv("CPA_CONTENT_FILTER_KEEPER_DB_PATH")),
		SidecarPath:     os.Getenv("CPA_CONTENT_FILTER_AUDIT_SIDECAR"),
	}
	if src.HostDBPath == "" {
		src.HostDBPath = contentfilter.DefaultHostVolumeDBPath
	}
	if src.ContainerName == "" {
		src.ContainerName = contentfilter.DefaultContainerName
	}
	if src.ContainerDBPath == "" {
		src.ContainerDBPath = contentfilter.DefaultContainerDBPath
	}

	filter := contentfilter.ExportFilter{
		Since:      options.Since,
		Until:      options.Until,
		Limit:      options.Limit,
		Model:      options.Model,
		UserID:     options.UserID,
		ClientIP:   options.ClientIP,
		FilterType: options.FilterType,
	}

	res, err := contentfilter.ExportLogs(src, filter, format, options.Out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "contentfilter-export: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("contentfilter-export: wrote %d row(s) to %s (format=%s)\n",
		res.Rows, res.Path, res.Format)
}
