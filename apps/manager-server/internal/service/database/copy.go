package database

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

var migrationTableExcludes = map[string]bool{
	"sqlite_sequence":                  true,
	"usage_monitoring_event_search_v1": true,
}

func listTables(ctx context.Context, db *sql.DB, driver string) ([]string, error) {
	var query string
	if driver == DriverMySQL {
		query = `select table_name from information_schema.tables
			where table_schema=database() and table_type='BASE TABLE' order by table_name`
	} else {
		query = `select name from sqlite_schema where type='table' and name not like 'sqlite_%' order by name`
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if migrationTableExcludes[name] || strings.HasPrefix(name, "usage_monitoring_event_search_v1_") {
			continue
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func targetIsEmpty(ctx context.Context, db *sql.DB, driver string, tables []string) (bool, []string, error) {
	occupied := make([]string, 0)
	for _, table := range tables {
		var count int64
		query := "select count(*) from " + quoteIdentifier(driver, table)
		var arguments []any
		if table == "settings" {
			query += " where " + quoteIdentifier(driver, "key") + " not in (?, ?)"
			arguments = []any{"usage_account_history_identity_format_version", "usage_dashboard_hourly_format_version"}
		}
		if err := db.QueryRowContext(ctx, query, arguments...).Scan(&count); err != nil {
			return false, nil, err
		}
		if count > 0 && !ignorableSeedTable(table) {
			occupied = append(occupied, table)
		}
	}
	return len(occupied) == 0, occupied, nil
}

func ignorableSeedTable(name string) bool {
	switch name {
	case "usage_hourly_aggregate_state", "usage_pricing_rollup_state", "usage_monitoring_rollup_state", "usage_monitoring_search_index_state", "usage_data_migrations":
		return true
	default:
		return false
	}
}

func estimateRows(ctx context.Context, db *sql.DB, driver string) int64 {
	if driver == DriverMySQL {
		var total sql.NullInt64
		_ = db.QueryRowContext(ctx, `select coalesce(sum(table_rows), 0) from information_schema.tables
			where table_schema=database() and table_type='BASE TABLE'`).Scan(&total)
		return total.Int64
	}
	rows, err := db.QueryContext(ctx, `select stat from sqlite_stat1`)
	if err != nil {
		return 0
	}
	defer rows.Close()
	var total int64
	for rows.Next() {
		var stat string
		if rows.Scan(&stat) == nil {
			fields := strings.Fields(stat)
			if len(fields) == 0 {
				continue
			}
			var estimate int64
			if _, err := fmt.Sscan(fields[0], &estimate); err == nil && estimate > 0 {
				total += estimate
			}
		}
	}
	return total
}

func difference(left, right []string) []string {
	available := make(map[string]bool, len(right))
	for _, item := range right {
		available[item] = true
	}
	out := make([]string, 0)
	for _, item := range left {
		if !available[item] {
			out = append(out, item)
		}
	}
	sort.Strings(out)
	return out
}

func intersect(left, right []string) []string {
	available := make(map[string]bool, len(right))
	for _, item := range right {
		available[item] = true
	}
	out := make([]string, 0)
	for _, item := range left {
		if available[item] {
			out = append(out, item)
		}
	}
	return out
}

func quoteIdentifier(driver, value string) string {
	if driver == DriverMySQL {
		return "`" + strings.ReplaceAll(value, "`", "``") + "`"
	}
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

type queryContext interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func tableColumns(ctx context.Context, queryer queryContext, driver, table string) ([]string, error) {
	var rows *sql.Rows
	var err error
	if driver == DriverMySQL {
		rows, err = queryer.QueryContext(ctx, `select column_name from information_schema.columns
			where table_schema=database() and table_name=? and extra not like '%GENERATED%' order by ordinal_position`, table)
	} else {
		rows, err = queryer.QueryContext(ctx, "pragma table_xinfo("+quoteIdentifier(driver, table)+")")
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := make([]string, 0)
	if driver == DriverMySQL {
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return nil, err
			}
			columns = append(columns, name)
		}
	} else {
		for rows.Next() {
			var cid int
			var name, dataType string
			var notNull, pk, hidden int
			var defaultValue any
			if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &pk, &hidden); err != nil {
				return nil, err
			}
			if hidden == 0 {
				columns = append(columns, name)
			}
		}
	}
	return columns, rows.Err()
}

func copyTable(
	ctx context.Context,
	source *sql.Tx,
	sourceDriver string,
	target *sql.Conn,
	targetDriver string,
	table string,
	progress func(int64),
) (int64, error) {
	sourceColumns, err := tableColumns(ctx, source, sourceDriver, table)
	if err != nil {
		return 0, fmt.Errorf("read source columns for %s: %w", table, err)
	}
	targetColumns, err := tableColumns(ctx, target, targetDriver, table)
	if err != nil {
		return 0, fmt.Errorf("read target columns for %s: %w", table, err)
	}
	available := make(map[string]bool, len(targetColumns))
	for _, column := range targetColumns {
		available[column] = true
	}
	columns := make([]string, 0, len(sourceColumns))
	for _, column := range sourceColumns {
		if available[column] {
			columns = append(columns, column)
		}
	}
	if len(columns) == 0 {
		return 0, fmt.Errorf("table %s has no shared columns", table)
	}
	quotedSource := make([]string, len(columns))
	quotedTarget := make([]string, len(columns))
	for index, column := range columns {
		quotedSource[index] = quoteIdentifier(sourceDriver, column)
		quotedTarget[index] = quoteIdentifier(targetDriver, column)
	}
	query := "select " + strings.Join(quotedSource, ",") + " from " + quoteIdentifier(sourceDriver, table)
	rows, err := source.QueryContext(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("read source table %s: %w", table, err)
	}
	defer rows.Close()
	columnTypes, err := rows.ColumnTypes()
	if err != nil {
		return 0, fmt.Errorf("read source column types for %s: %w", table, err)
	}

	var copied int64
	batch := make([][]any, 0, migrationBatchSize)
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			return copied, fmt.Errorf("scan source table %s: %w", table, err)
		}
		for index := range values {
			values[index] = normalizeCopiedValue(values[index], columnTypes[index].DatabaseTypeName())
		}
		batch = append(batch, values)
		if len(batch) >= migrationBatchSize {
			if err := insertBatch(ctx, target, targetDriver, table, quotedTarget, batch); err != nil {
				return copied, err
			}
			copied += int64(len(batch))
			progress(int64(len(batch)))
			batch = batch[:0]
		}
	}
	if err := rows.Err(); err != nil {
		return copied, err
	}
	if len(batch) > 0 {
		if err := insertBatch(ctx, target, targetDriver, table, quotedTarget, batch); err != nil {
			return copied, err
		}
		copied += int64(len(batch))
		progress(int64(len(batch)))
	}
	return copied, nil
}

func normalizeCopiedValue(value any, databaseType string) any {
	bytes, ok := value.([]byte)
	if !ok {
		return value
	}
	typeName := strings.ToUpper(strings.TrimSpace(databaseType))
	if strings.Contains(typeName, "CHAR") || strings.Contains(typeName, "TEXT") ||
		strings.Contains(typeName, "JSON") || strings.Contains(typeName, "ENUM") ||
		strings.Contains(typeName, "SET") {
		return string(bytes)
	}
	return value
}

func insertBatch(ctx context.Context, target *sql.Conn, driver, table string, quotedColumns []string, batch [][]any) error {
	if len(batch) == 0 {
		return nil
	}
	rowPlaceholder := "(" + strings.TrimRight(strings.Repeat("?,", len(quotedColumns)), ",") + ")"
	placeholders := make([]string, len(batch))
	arguments := make([]any, 0, len(batch)*len(quotedColumns))
	for index, row := range batch {
		placeholders[index] = rowPlaceholder
		arguments = append(arguments, row...)
	}
	query := "insert into " + quoteIdentifier(driver, table) + " (" + strings.Join(quotedColumns, ",") + ") values " + strings.Join(placeholders, ",")
	if _, err := target.ExecContext(ctx, query, arguments...); err != nil {
		return fmt.Errorf("insert target table %s: %w", table, err)
	}
	return nil
}

func setForeignKeyChecks(ctx context.Context, target *sql.Conn, driver string, enabled bool) error {
	if driver == DriverMySQL {
		value := 0
		if enabled {
			value = 1
		}
		_, err := target.ExecContext(ctx, fmt.Sprintf("set foreign_key_checks=%d", value))
		return err
	}
	value := "OFF"
	if enabled {
		value = "ON"
	}
	_, err := target.ExecContext(ctx, "pragma foreign_keys="+value)
	return err
}
