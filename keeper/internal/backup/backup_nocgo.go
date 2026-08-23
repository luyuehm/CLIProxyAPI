//go:build !cgo

package backup

import (
	"context"
	"database/sql"
	"fmt"
)

// copySQLiteDatabase copies the source database to the destination path by
// recreating the source's tables in the destination and copying rows over.
// It is a fallback for builds without cgo, where go-sqlite3's online backup
// API (SQLiteConn.Backup) is unavailable because the driver is compiled as a
// non-functional stub.
func copySQLiteDatabase(ctx context.Context, sourceDB *sql.DB, destPath string) error {
	sourceConn, err := sourceDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open source database connection: %w", err)
	}
	defer sourceConn.Close()

	destDB, err := sql.Open("sqlite3", destPath)
	if err != nil {
		return fmt.Errorf("open backup database: %w", err)
	}
	defer destDB.Close()
	destConn, err := destDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open backup database connection: %w", err)
	}
	defer destConn.Close()

	if err := copySQLiteConn(ctx, destConn, sourceConn); err != nil {
		return err
	}
	return nil
}

func copySQLiteConn(ctx context.Context, destConn, sourceConn *sql.Conn) error {
	type table struct {
		name, schema string
	}
	tables, err := queryTables(ctx, sourceConn, destConn)
	if err != nil {
		return err
	}
	for _, t := range tables {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := destConn.ExecContext(ctx, "DROP TABLE IF EXISTS \""+t.name+"\""); err != nil {
			return fmt.Errorf("drop backup table %s: %w", t.name, err)
		}
		if _, err := destConn.ExecContext(ctx, t.schema); err != nil {
			return fmt.Errorf("recreate backup table %s: %w", t.name, err)
		}
	}
	for _, t := range tables {
		if err := copyTable(ctx, sourceConn, destConn, t.name); err != nil {
			return err
		}
	}
	return nil
}

func queryTables(ctx context.Context, sourceConn, destConn *sql.Conn) ([]struct{ name, schema string }, error) {
	rows, err := sourceConn.QueryContext(ctx,
		"SELECT name, sql FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("list backup tables: %w", err)
	}
	defer rows.Close()

	var tables []struct{ name, schema string }
	tableNames := make(map[string]bool)
	for rows.Next() {
		var name, schema string
		if err := rows.Scan(&name, &schema); err != nil {
			return nil, fmt.Errorf("scan backup table listing: %w", err)
		}
		if tableNames[name] {
			continue
		}
		tableNames[name] = true
		tables = append(tables, struct{ name, schema string }{name: name, schema: schema})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("enumerate backup tables: %w", err)
	}
	return tables, nil
}

func copyTable(ctx context.Context, sourceConn, destConn *sql.Conn, table string) error {
	rows, err := sourceConn.QueryContext(ctx, "SELECT * FROM \""+table+"\"")
	if err != nil {
		return fmt.Errorf("read backup table %s: %w", table, err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return fmt.Errorf("read backup columns for %s: %w", table, err)
	}
	colNames := "(" + quoteColumns(cols) + ")"
	placeholders := "(" + placeholders(len(cols)) + ")"

	insert, err := destConn.PrepareContext(ctx, "INSERT INTO \""+table+"\" "+colNames+" VALUES "+placeholders)
	if err != nil {
		return fmt.Errorf("prepare backup write for %s: %w", table, err)
	}
	defer insert.Close()

	values := make([]any, len(cols))
	pointers := make([]any, len(cols))
	for i := range pointers {
		pointers[i] = &values[i]
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := rows.Scan(pointers...); err != nil {
			return fmt.Errorf("scan backup row for %s: %w", table, err)
		}
		copied := make([]any, len(values))
		for i, v := range values {
			switch bytes := v.(type) {
			case []byte:
				buf := make([]byte, len(bytes))
				copy(buf, bytes)
				copied[i] = buf
			default:
				copied[i] = v
			}
		}
		if _, err := insert.ExecContext(ctx, copied...); err != nil {
			return fmt.Errorf("write backup row for %s: %w", table, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read backup rows for %s: %w", table, err)
	}
	return nil
}

func quoteColumns(cols []string) string {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = "\"" + c + "\""
	}
	return joinStrings(quoted, ", ")
}

func placeholders(n int) string {
	values := make([]string, n)
	for i := range values {
		values[i] = "?"
	}
	return joinStrings(values, ", ")
}

func joinStrings(values []string, sep string) string {
	out := ""
	for i, v := range values {
		if i > 0 {
			out += sep
		}
		out += v
	}
	return out
}