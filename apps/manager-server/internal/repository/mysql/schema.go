package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"

	sqliterepo "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/sqlite"
)

const searchIndexTable = "usage_monitoring_event_search_v1"

// literalDefaultPattern matches a quoted literal default such as `default ''`
// or `default 'codex'`, which MySQL rejects on TEXT columns.
var literalDefaultPattern = regexp.MustCompile(`(?i)\bdefault\s+'`)

type schemaObject struct {
	Type      string
	Name      string
	TableName string
	SQL       string
}

type tablePlan struct {
	Name        string
	CreateSQL   string
	Columns     []columnPlan
	ForeignKeys []foreignKeyPlan
}

type columnPlan struct {
	Name       string
	Definition string
	Generated  bool
}

type foreignKeyPlan struct {
	Name       string
	Table      string
	Columns    []string
	RefTable   string
	RefColumns []string
	OnDelete   string
}

func Migrate(ctx context.Context, db *sql.DB) error {
	objects, err := currentSQLiteSchema(ctx)
	if err != nil {
		return err
	}
	plans := make([]tablePlan, 0)
	indexes := make([]schemaObject, 0)
	for _, object := range objects {
		switch object.Type {
		case "table":
			plan, ok, err := convertTable(object)
			if err != nil {
				return fmt.Errorf("convert table %s: %w", object.Name, err)
			}
			if ok {
				plans = append(plans, plan)
			}
		case "index":
			if strings.TrimSpace(object.SQL) != "" {
				indexes = append(indexes, object)
			}
		}
	}
	sort.Slice(plans, func(left, right int) bool { return plans[left].Name < plans[right].Name })
	for _, plan := range plans {
		if _, err := db.ExecContext(ctx, plan.CreateSQL); err != nil {
			return fmt.Errorf("create mysql table %s: %w", plan.Name, err)
		}
		if err := ensureColumns(ctx, db, plan); err != nil {
			return err
		}
	}
	if err := seedInitialState(ctx, db); err != nil {
		return err
	}
	for _, object := range indexes {
		if err := ensureIndex(ctx, db, object); err != nil {
			return err
		}
	}
	for _, plan := range plans {
		for _, foreignKey := range plan.ForeignKeys {
			if err := ensureForeignKey(ctx, db, foreignKey); err != nil {
				// Foreign keys are an integrity enhancement here, not an application
				// dependency. Existing installations may have legacy column widths or
				// engines that make one constraint ineligible; the repository already
				// enforces the same parent/child lifecycle explicitly.
				continue
			}
		}
	}
	// Some managed MySQL installations keep binary logging enabled while the
	// application user intentionally lacks SUPER. Trigger creation is best
	// effort; the corresponding diagnostic row is also replaced on every write
	// and does not participate in primary request processing.
	_ = ensureTriggers(ctx, db)
	return nil
}

func seedInitialState(ctx context.Context, db *sql.DB) error {
	statements := []string{
		`insert ignore into usage_hourly_aggregate_state (
			aggregate_name, schema_version, status, backfill_last_event_id,
			coverage_event_id, target_event_id, processed_events, updated_at_ms
		) values ('hourly_core', 1, 'ready', 0, 0, 0, 0, 0)`,
		`insert ignore into usage_pricing_rollup_state (
			rollup_name, schema_version, structure_revision, status,
			backfill_last_event_id, coverage_event_id, target_event_id,
			processed_events, updated_at_ms
		) values ('pricing_v1', 1, '', 'pending', 0, 0, 0, 0, 0)`,
		`insert ignore into usage_monitoring_rollup_state (
			rollup_name, schema_version, structure_revision, status,
			backfill_last_event_id, coverage_event_id, target_event_id,
			processed_events, updated_at_ms
		) values ('stats_v1', 1, '', 'ready', 0, 0, 0, 0, 0)`,
		`insert ignore into usage_monitoring_rollup_state (
			rollup_name, schema_version, structure_revision, status,
			backfill_last_event_id, coverage_event_id, target_event_id,
			processed_events, updated_at_ms
		) values ('metadata_v1', 1, '', 'ready', 0, 0, 0, 0, 0)`,
		`insert ignore into usage_monitoring_rollup_state (
			rollup_name, schema_version, structure_revision, status,
			backfill_last_event_id, coverage_event_id, target_event_id,
			processed_events, updated_at_ms
		) values ('projection_v1', 1, '', 'ready', 0, 0, 0, 0, 0)`,
		`insert ignore into usage_data_migrations (
			name, status, last_event_id, target_event_id, processed_rows,
			changed_rows, updated_at_ms
		) values ('usage_cache_accounting_v1', 'completed', 0, 0, 0, 0, 0)`,
		`insert ignore into usage_data_migrations (
			name, status, last_event_id, target_event_id, processed_rows,
			changed_rows, updated_at_ms
		) values ('usage_cache_accounting_v2', 'completed', 0, 0, 0, 0, 0)`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("seed mysql state: %w", err)
		}
	}
	return nil
}

func currentSQLiteSchema(ctx context.Context) ([]schemaObject, error) {
	source, err := sql.Open("sqlite", "file:cpamp-mysql-schema?mode=memory&cache=shared")
	if err != nil {
		return nil, err
	}
	source.SetMaxOpenConns(1)
	defer source.Close()
	if err := sqliterepo.Migrate(source); err != nil {
		return nil, fmt.Errorf("build canonical sqlite schema: %w", err)
	}
	rows, err := source.QueryContext(ctx, `select type, name, tbl_name, coalesce(sql, '')
		from sqlite_master where type in ('table', 'index') order by type, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	objects := make([]schemaObject, 0)
	for rows.Next() {
		var object schemaObject
		if err := rows.Scan(&object.Type, &object.Name, &object.TableName, &object.SQL); err != nil {
			return nil, err
		}
		objects = append(objects, object)
	}
	return objects, rows.Err()
}

func convertTable(object schemaObject) (tablePlan, bool, error) {
	lowerSQL := strings.ToLower(strings.TrimSpace(object.SQL))
	if object.Name == "sqlite_sequence" || object.Name == searchIndexTable || strings.HasPrefix(object.Name, searchIndexTable+"_") {
		return tablePlan{}, false, nil
	}
	if strings.HasPrefix(lowerSQL, "create virtual table") {
		return tablePlan{}, false, nil
	}
	open := strings.Index(object.SQL, "(")
	close := strings.LastIndex(object.SQL, ")")
	if open < 0 || close <= open {
		return tablePlan{}, false, fmt.Errorf("unrecognized create table statement")
	}
	parts := splitTopLevel(object.SQL[open+1:close], ',')
	if len(parts) == 0 {
		return tablePlan{}, false, fmt.Errorf("table has no definitions")
	}

	singleKeyColumns := make(map[string]bool)
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "primary key") || strings.HasPrefix(lower, "unique") {
			columns := parseConstraintColumns(trimmed)
			if len(columns) == 1 && isSimpleIdentifier(columns[0]) {
				singleKeyColumns[normalizeIdentifier(columns[0])] = true
			}
			continue
		}
		name, remainder, ok := splitColumnDefinition(trimmed)
		if ok {
			lowerRemainder := strings.ToLower(remainder)
			if strings.Contains(lowerRemainder, "primary key") || strings.Contains(lowerRemainder, " unique") {
				singleKeyColumns[name] = true
			}
		}
	}

	definitions := make([]string, 0, len(parts)+8)
	columns := make([]columnPlan, 0, len(parts)+8)
	foreignKeys := make([]foreignKeyPlan, 0)
	constraintIndex := 0
	foreignIndex := 0
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		switch {
		case strings.HasPrefix(lower, "foreign key"):
			foreignIndex++
			foreignKey, ok := parseForeignKey(object.Name, foreignIndex, trimmed)
			if ok {
				foreignKeys = append(foreignKeys, foreignKey)
			}
			continue
		case strings.HasPrefix(lower, "check"):
			continue
		case strings.HasPrefix(lower, "primary key"), strings.HasPrefix(lower, "unique"):
			keyColumns := parseConstraintColumns(trimmed)
			if len(keyColumns) == 0 {
				continue
			}
			if len(keyColumns) == 1 && isSimpleIdentifier(keyColumns[0]) {
				kind := "unique key"
				if strings.HasPrefix(lower, "primary key") {
					kind = "primary key"
				}
				definitions = append(definitions, fmt.Sprintf("%s (%s)", kind, quoteIdentifier(normalizeIdentifier(keyColumns[0]))))
				continue
			}
			constraintIndex++
			generatedName := fmt.Sprintf("_cpamp_uq_%d", constraintIndex)
			expressionColumns := make([]string, 0, len(keyColumns))
			valid := true
			for _, column := range keyColumns {
				if !isSimpleIdentifier(column) {
					valid = false
					break
				}
				expressionColumns = append(expressionColumns, quoteIdentifier(normalizeIdentifier(column)))
			}
			if !valid {
				continue
			}
			definition := fmt.Sprintf("%s binary(32) generated always as (unhex(sha2(cast(json_array(%s) as char), 256))) stored",
				quoteIdentifier(generatedName), strings.Join(expressionColumns, ", "))
			definitions = append(definitions, definition)
			definitions = append(definitions, fmt.Sprintf("unique key %s (%s)", quoteIdentifier("uq_"+object.Name+fmt.Sprintf("_%d", constraintIndex)), quoteIdentifier(generatedName)))
			columns = append(columns, columnPlan{Name: generatedName, Definition: definition, Generated: true})
			continue
		}

		name, remainder, ok := splitColumnDefinition(trimmed)
		if !ok {
			continue
		}
		definition := convertColumnDefinition(name, remainder, singleKeyColumns[name])
		definitions = append(definitions, definition)
		columns = append(columns, columnPlan{Name: name, Definition: definition})
	}
	if len(definitions) == 0 {
		return tablePlan{}, false, fmt.Errorf("no compatible definitions")
	}
	createSQL := fmt.Sprintf("create table if not exists %s (\n  %s\n) engine=InnoDB default charset=utf8mb4 collate=utf8mb4_unicode_ci",
		quoteIdentifier(object.Name), strings.Join(definitions, ",\n  "))
	return tablePlan{Name: object.Name, CreateSQL: createSQL, Columns: columns, ForeignKeys: foreignKeys}, true, nil
}

func convertColumnDefinition(name, remainder string, keyColumn bool) string {
	lower := strings.ToLower(strings.TrimSpace(remainder))
	typeName := "longtext"
	tail := remainder
	switch {
	case strings.HasPrefix(lower, "integer"):
		typeName = "bigint"
		tail = strings.TrimSpace(remainder[len("integer"):])
	case strings.HasPrefix(lower, "real"):
		typeName = "double"
		tail = strings.TrimSpace(remainder[len("real"):])
	case strings.HasPrefix(lower, "text"):
		tail = strings.TrimSpace(remainder[len("text"):])
		// MySQL never accepts a literal default on a TEXT column, and before
		// 8.0.13 it accepts no default at all. Every SQLite text column that
		// carries a default holds a short identifier, so a bounded VARCHAR
		// preserves the canonical semantics on both 5.7 and 8.x.
		if keyColumn || literalDefaultPattern.MatchString(tail) {
			typeName = "varchar(512)"
		}
	case strings.HasPrefix(lower, "blob"):
		typeName = "longblob"
		tail = strings.TrimSpace(remainder[len("blob"):])
	}
	tail = regexp.MustCompile(`(?i)\bautoincrement\b`).ReplaceAllString(tail, "auto_increment")
	tail = regexp.MustCompile(`(?i)\binteger\b`).ReplaceAllString(tail, "signed")
	return strings.TrimSpace(fmt.Sprintf("%s %s %s", quoteIdentifier(name), typeName, tail))
}

func ensureColumns(ctx context.Context, db *sql.DB, plan tablePlan) error {
	existing, err := tableColumns(ctx, db, plan.Name)
	if err != nil {
		return err
	}
	for _, column := range plan.Columns {
		if existing[column.Name] {
			continue
		}
		definition := column.Definition
		if !column.Generated && strings.Contains(strings.ToLower(definition), " not null") && !strings.Contains(strings.ToLower(definition), " default") && !strings.Contains(strings.ToLower(definition), "auto_increment") {
			definition = regexp.MustCompile(`(?i)\s+not\s+null`).ReplaceAllString(definition, " null")
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("alter table %s add column %s", quoteIdentifier(plan.Name), definition)); err != nil {
			return fmt.Errorf("add mysql column %s.%s: %w", plan.Name, column.Name, err)
		}
	}
	return nil
}

func tableColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `select column_name from information_schema.columns
		where table_schema = database() and table_name = ?`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

func ensureIndex(ctx context.Context, db *sql.DB, object schemaObject) error {
	if object.TableName == searchIndexTable || strings.HasPrefix(object.TableName, searchIndexTable+"_") {
		return nil
	}
	lower := strings.ToLower(object.SQL)
	if strings.Contains(lower, " where ") {
		return nil
	}
	open := strings.Index(object.SQL, "(")
	close := strings.LastIndex(object.SQL, ")")
	if open < 0 || close <= open {
		return nil
	}
	var exists int
	if err := db.QueryRowContext(ctx, `select count(*) from information_schema.statistics
		where table_schema = database() and table_name = ? and index_name = ?`, object.TableName, object.Name).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return nil
	}
	columnTypes, err := indexedColumnTypes(ctx, db, object.TableName)
	if err != nil {
		return err
	}
	parts := splitTopLevel(object.SQL[open+1:close], ',')
	converted := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		fields := strings.Fields(trimmed)
		if len(fields) == 0 || !isSimpleIdentifier(fields[0]) {
			return nil
		}
		name := normalizeIdentifier(fields[0])
		column := quoteIdentifier(name)
		if isLongColumnType(columnTypes[name]) {
			column += "(128)"
		}
		if len(fields) > 1 && strings.EqualFold(fields[len(fields)-1], "desc") {
			column += " desc"
		}
		converted = append(converted, column)
	}
	if len(converted) == 0 {
		return nil
	}
	unique := ""
	if strings.HasPrefix(strings.TrimSpace(lower), "create unique index") {
		unique = "unique "
	}
	statement := fmt.Sprintf("create %sindex %s on %s (%s)", unique, quoteIdentifier(object.Name), quoteIdentifier(object.TableName), strings.Join(converted, ", "))
	if _, err := db.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("create mysql index %s on %s: %w", object.Name, object.TableName, err)
	}
	return nil
}

func indexedColumnTypes(ctx context.Context, db *sql.DB, table string) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `select column_name, data_type from information_schema.columns
		where table_schema = database() and table_name = ?`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]string)
	for rows.Next() {
		var name, dataType string
		if err := rows.Scan(&name, &dataType); err != nil {
			return nil, err
		}
		result[name] = strings.ToLower(dataType)
	}
	return result, rows.Err()
}

func isLongColumnType(dataType string) bool {
	return strings.Contains(dataType, "text") || strings.Contains(dataType, "blob")
}

func ensureForeignKey(ctx context.Context, db *sql.DB, plan foreignKeyPlan) error {
	var exists int
	if err := db.QueryRowContext(ctx, `select count(*) from information_schema.table_constraints
		where constraint_schema = database() and table_name = ? and constraint_name = ?`, plan.Table, plan.Name).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return nil
	}
	columns := quoteIdentifiers(plan.Columns)
	refColumns := quoteIdentifiers(plan.RefColumns)
	statement := fmt.Sprintf("alter table %s add constraint %s foreign key (%s) references %s (%s)",
		quoteIdentifier(plan.Table), quoteIdentifier(plan.Name), strings.Join(columns, ", "), quoteIdentifier(plan.RefTable), strings.Join(refColumns, ", "))
	if plan.OnDelete != "" {
		statement += " on delete " + plan.OnDelete
	}
	if _, err := db.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("create mysql foreign key %s: %w", plan.Name, err)
	}
	return nil
}

func ensureTriggers(ctx context.Context, db *sql.DB) error {
	const name = "trg_usage_events_delete_routing_diagnostics"
	var exists int
	if err := db.QueryRowContext(ctx, `select count(*) from information_schema.triggers
		where trigger_schema = database() and trigger_name = ?`, name).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, `create trigger trg_usage_events_delete_routing_diagnostics
		after delete on usage_events for each row
		delete from usage_routing_diagnostics where event_hash = old.event_hash`)
	if err != nil {
		return fmt.Errorf("create mysql usage cleanup trigger: %w", err)
	}
	return nil
}

func parseForeignKey(table string, index int, definition string) (foreignKeyPlan, bool) {
	pattern := regexp.MustCompile(`(?is)^foreign\s+key\s*\(([^)]*)\)\s+references\s+([^\s(]+)\s*\(([^)]*)\)(?:\s+on\s+delete\s+([a-z ]+))?`)
	match := pattern.FindStringSubmatch(strings.TrimSpace(definition))
	if len(match) == 0 {
		return foreignKeyPlan{}, false
	}
	columns := normalizeIdentifiers(splitTopLevel(match[1], ','))
	refColumns := normalizeIdentifiers(splitTopLevel(match[3], ','))
	onDelete := strings.ToLower(strings.TrimSpace(match[4]))
	switch onDelete {
	case "cascade", "set null", "restrict", "no action":
	default:
		onDelete = ""
	}
	return foreignKeyPlan{
		Name:       fmt.Sprintf("fk_%s_%d", table, index),
		Table:      table,
		Columns:    columns,
		RefTable:   normalizeIdentifier(match[2]),
		RefColumns: refColumns,
		OnDelete:   onDelete,
	}, true
}

func parseConstraintColumns(definition string) []string {
	open := strings.Index(definition, "(")
	close := strings.LastIndex(definition, ")")
	if open < 0 || close <= open {
		return nil
	}
	return splitTopLevel(definition[open+1:close], ',')
}

func splitColumnDefinition(definition string) (string, string, bool) {
	definition = strings.TrimSpace(definition)
	if definition == "" {
		return "", "", false
	}
	if definition[0] == '"' || definition[0] == '`' {
		quote := definition[0]
		end := strings.IndexByte(definition[1:], quote)
		if end < 0 {
			return "", "", false
		}
		end++
		return definition[1:end], strings.TrimSpace(definition[end+1:]), true
	}
	fields := strings.Fields(definition)
	if len(fields) < 2 {
		return "", "", false
	}
	return normalizeIdentifier(fields[0]), strings.TrimSpace(definition[len(fields[0]):]), true
}

func splitTopLevel(value string, delimiter byte) []string {
	parts := make([]string, 0)
	start := 0
	depth := 0
	var quote byte
	for index := 0; index < len(value); index++ {
		char := value[index]
		if quote != 0 {
			if char == quote {
				if index+1 < len(value) && value[index+1] == quote {
					index++
					continue
				}
				quote = 0
			}
			continue
		}
		if char == '\'' || char == '"' || char == '`' {
			quote = char
			continue
		}
		switch char {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if char == delimiter && depth == 0 {
				parts = append(parts, strings.TrimSpace(value[start:index]))
				start = index + 1
			}
		}
	}
	parts = append(parts, strings.TrimSpace(value[start:]))
	return parts
}

func quoteIdentifier(value string) string {
	return "`" + strings.ReplaceAll(normalizeIdentifier(value), "`", "``") + "`"
}

func quoteIdentifiers(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, quoteIdentifier(value))
	}
	return result
}

func normalizeIdentifiers(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, normalizeIdentifier(value))
	}
	return result
}

func normalizeIdentifier(value string) string {
	return strings.Trim(strings.TrimSpace(value), "`\"[]")
}

func isSimpleIdentifier(value string) bool {
	value = normalizeIdentifier(value)
	if value == "" {
		return false
	}
	for index, char := range value {
		if !(char == '_' || char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || index > 0 && char >= '0' && char <= '9') {
			return false
		}
	}
	return true
}
