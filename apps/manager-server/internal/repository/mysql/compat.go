package mysql

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var (
	insertOrIgnorePattern  = regexp.MustCompile(`(?i)\binsert\s+or\s+ignore\s+into\b`)
	insertOrReplacePattern = regexp.MustCompile(`(?i)\binsert\s+or\s+replace\s+into\b`)
	conflictNothingPattern = regexp.MustCompile(`(?is)\s+on\s+conflict\s*\([^)]*\)\s+do\s+nothing\s*`)
	conflictUpdatePattern  = regexp.MustCompile(`(?is)\s+on\s+conflict\s*\([^)]*\)\s+do\s+update\s+set\s+`)
	excludedColumnPattern  = regexp.MustCompile(`(?i)\bexcluded\.([a-zA-Z_][a-zA-Z0-9_]*)`)
	returningIDPattern     = regexp.MustCompile(`(?is)\s+returning\s+id\s*;?\s*$`)
	noCasePattern          = regexp.MustCompile(`(?i)\s+collate\s+nocase\b`)
	jsonEachPattern        = regexp.MustCompile(`(?is)select\s+value\s+from\s+json_each\s*\(\s*\?\s*\)`)
	ftsLikePattern         = regexp.MustCompile(`(?is)([a-zA-Z_][a-zA-Z0-9_]*\.)?event_id\s+in\s*\(\s*select\s+rowid\s+from\s+usage_monitoring_event_search_v1\s+where\s+search_text\s+like\s+\?\s*\)`)
	indexedByPattern       = regexp.MustCompile(`(?i)\s+indexed\s+by\s+[a-zA-Z_][a-zA-Z0-9_]*`)
	notIndexedPattern      = regexp.MustCompile(`(?i)\s+not\s+indexed\b`)
	jsonTypePattern        = regexp.MustCompile(`(?i)\bjson_type\s*\(\s*([^,()]+(?:\([^)]*\))?)\s*,\s*([^()]+?)\s*\)`)
	jsonGroupFilterPattern = regexp.MustCompile(`(?i)json_group_array\s*\(\s*distinct\s+([a-zA-Z_][a-zA-Z0-9_.]*)\s*\)\s*filter\s*\(\s*where\s+[a-zA-Z_][a-zA-Z0-9_.]*\s*<>\s*''\s*\)`)
	integerBucketPattern   = regexp.MustCompile(`(?i)\(\s*([a-zA-Z_][a-zA-Z0-9_.]*)\s*/\s*(\?|[0-9]+)\s*\)\s*\*`)
	castIntegerPattern     = regexp.MustCompile(`(?i)\bas\s+integer\b`)
	cteMaterializedPattern = regexp.MustCompile(`(?i)\bas\s+(?:not\s+)?materialized\s*\(`)
)

type compatConnector struct {
	inner driver.Connector
}

func (c *compatConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &compatConn{Conn: conn}, nil
}

func (c *compatConnector) Driver() driver.Driver { return c.inner.Driver() }

type compatConn struct {
	driver.Conn
}

func (c *compatConn) Prepare(query string) (driver.Stmt, error) {
	translated, _ := translateQuery(query)
	stmt, err := c.Conn.Prepare(translated)
	if err != nil {
		return nil, fmt.Errorf("%w [sql=%s]", err, compactQuery(translated))
	}
	return stmt, nil
}

func (c *compatConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	translated, _ := translateQuery(query)
	if conn, ok := c.Conn.(driver.ConnPrepareContext); ok {
		stmt, err := conn.PrepareContext(ctx, translated)
		if err != nil {
			return nil, fmt.Errorf("%w [sql=%s]", err, compactQuery(translated))
		}
		return stmt, nil
	}
	return c.Prepare(translated)
}

func (c *compatConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if conn, ok := c.Conn.(driver.ConnBeginTx); ok {
		return conn.BeginTx(ctx, opts)
	}
	return c.Conn.Begin()
}

func (c *compatConn) Ping(ctx context.Context) error {
	if conn, ok := c.Conn.(driver.Pinger); ok {
		return conn.Ping(ctx)
	}
	return nil
}

func (c *compatConn) ResetSession(ctx context.Context) error {
	if conn, ok := c.Conn.(driver.SessionResetter); ok {
		return conn.ResetSession(ctx)
	}
	return nil
}

func (c *compatConn) IsValid() bool {
	if conn, ok := c.Conn.(driver.Validator); ok {
		return conn.IsValid()
	}
	return true
}

func (c *compatConn) CheckNamedValue(value *driver.NamedValue) error {
	switch typed := value.Value.(type) {
	case int:
		value.Value = int64(typed)
	case int8:
		value.Value = int64(typed)
	case int16:
		value.Value = int64(typed)
	case int32:
		value.Value = int64(typed)
	case uint:
		value.Value = int64(typed)
	case uint8:
		value.Value = int64(typed)
	case uint16:
		value.Value = int64(typed)
	case uint32:
		value.Value = int64(typed)
	}
	return nil
}

func (c *compatConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	translated, _ := translateQuery(query)
	result, err := execContext(ctx, c.Conn, translated, args)
	if errors.Is(err, driver.ErrSkip) {
		return nil, driver.ErrSkip
	}
	if err != nil {
		return nil, fmt.Errorf("%w [sql=%s]", err, compactQuery(translated))
	}
	return result, nil
}

func (c *compatConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	translated, returnsID := translateQuery(query)
	if returnsID {
		result, err := execContext(ctx, c.Conn, translated, args)
		if errors.Is(err, driver.ErrSkip) {
			stmt, prepareErr := c.PrepareContext(ctx, translated)
			if prepareErr != nil {
				return nil, prepareErr
			}
			defer stmt.Close()
			values := namedValuesToValues(args)
			result, err = stmt.Exec(values)
		}
		if err != nil {
			return nil, err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return nil, err
		}
		return &singleIDRows{id: id}, nil
	}
	if conn, ok := c.Conn.(driver.QueryerContext); ok {
		rows, err := conn.QueryContext(ctx, translated, args)
		if errors.Is(err, driver.ErrSkip) {
			return nil, driver.ErrSkip
		}
		if err != nil {
			return nil, fmt.Errorf("%w [sql=%s]", err, compactQuery(translated))
		}
		return rows, nil
	}
	return nil, driver.ErrSkip
}

func namedValuesToValues(values []driver.NamedValue) []driver.Value {
	result := make([]driver.Value, len(values))
	for index := range values {
		result[index] = values[index].Value
	}
	return result
}

func compactQuery(query string) string {
	query = strings.Join(strings.Fields(query), " ")
	const limit = 800
	if len(query) > limit {
		return query[:limit] + "..."
	}
	return query
}

func execContext(ctx context.Context, conn driver.Conn, query string, args []driver.NamedValue) (driver.Result, error) {
	if executor, ok := conn.(driver.ExecerContext); ok {
		return executor.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

type singleIDRows struct {
	id   int64
	done bool
}

func (r *singleIDRows) Columns() []string { return []string{"id"} }
func (r *singleIDRows) Close() error      { return nil }
func (r *singleIDRows) Next(values []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	values[0] = r.id
	return nil
}

func translateQuery(query string) (string, bool) {
	translated := strings.TrimSpace(query)
	returnsID := returningIDPattern.MatchString(translated)
	translated = returningIDPattern.ReplaceAllString(translated, "")

	translated = insertOrIgnorePattern.ReplaceAllString(translated, "insert ignore into")
	translated = insertOrReplacePattern.ReplaceAllString(translated, "replace into")
	if conflictNothingPattern.MatchString(translated) {
		translated = conflictNothingPattern.ReplaceAllString(translated, "")
		translated = replaceFirstInsertInto(translated, "insert ignore into")
	}
	translated = conflictUpdatePattern.ReplaceAllString(translated, " on duplicate key update ")
	translated = excludedColumnPattern.ReplaceAllString(translated, "values($1)")
	if returnsID && strings.Contains(strings.ToLower(translated), "on duplicate key update") {
		translated = regexp.MustCompile(`(?is)on\s+duplicate\s+key\s+update\s+`).ReplaceAllString(translated, "on duplicate key update id = last_insert_id(id), ")
	}
	translated = rewriteConditionalDuplicateUpdate(translated)

	translated = noCasePattern.ReplaceAllString(translated, "")
	translated = indexedByPattern.ReplaceAllString(translated, "")
	translated = notIndexedPattern.ReplaceAllString(translated, "")
	translated = jsonEachPattern.ReplaceAllString(translated, "select jt.value from json_table(?, '$[*]' columns (value varchar(1024) path '$')) as jt")
	translated = jsonTypePattern.ReplaceAllString(translated, "json_type(json_extract($1, $2))")
	translated = jsonGroupFilterPattern.ReplaceAllString(translated,
		"coalesce(concat('[', group_concat(distinct case when $1 <> '' then json_quote($1) end), ']'), '[]')")
	translated = strings.ReplaceAll(translated, "json_type(json_extract(metadata_json, '$.provider_usage.actual'))", "json_type(json_extract(metadata_json, '$.provider_usage.actual'))")
	translated = ftsLikePattern.ReplaceAllStringFunc(translated, func(match string) string {
		prefix := ""
		if dot := strings.Index(match, ".event_id"); dot > 0 {
			prefix = match[:dot+1]
		}
		return prefix + "search_text like ?"
	})
	translated = strings.ReplaceAll(translated, "cast(unixepoch('subsec') * 1000 as integer)", "cast(unix_timestamp(current_timestamp(3)) * 1000 as signed)")
	translated = strings.ReplaceAll(translated, "CAST(unixepoch('subsec') * 1000 AS INTEGER)", "cast(unix_timestamp(current_timestamp(3)) * 1000 as signed)")
	translated = integerBucketPattern.ReplaceAllString(translated, "($1 div $2) *")
	translated = castIntegerPattern.ReplaceAllString(translated, "as signed")
	translated = cteMaterializedPattern.ReplaceAllString(translated, "as (")
	translated = replaceValuesCTEs(translated)
	translated = relocateLeadingCTEInsert(translated)
	translated = rewriteUpdateFrom(translated)
	translated = replaceScalarMinMax(translated)
	return translated, returnsID
}

// replaceValuesCTEs converts SQLite's compact CTE row constructor:
//
//	with targets(a, b) as (values (?, ?), (?, ?))
//
// into the MySQL 8 compatible equivalent:
//
//	with targets(a, b) as (select ?, ? union all select ?, ?)
func replaceValuesCTEs(query string) string {
	lower := strings.ToLower(query)
	for offset := 0; offset < len(query); {
		valuesAt := strings.Index(lower[offset:], "values")
		if valuesAt < 0 {
			break
		}
		valuesAt += offset
		if valuesAt > 0 && isIdentifierByte(query[valuesAt-1]) {
			offset = valuesAt + len("values")
			continue
		}
		after := valuesAt + len("values")
		if after < len(query) && isIdentifierByte(query[after]) {
			offset = after
			continue
		}
		previous := previousNonSpace(query, valuesAt-1)
		if previous < 0 || query[previous] != '(' || !isCTEAsOpen(query, previous) {
			offset = after
			continue
		}
		close := matchingParen(query, previous)
		if close < 0 {
			break
		}
		body := strings.TrimSpace(query[after:close])
		rows := splitTopLevelSQL(body, ',')
		if len(rows) == 0 {
			offset = close + 1
			continue
		}
		selects := make([]string, 0, len(rows))
		valid := true
		for _, row := range rows {
			row = strings.TrimSpace(row)
			if len(row) < 2 || row[0] != '(' || row[len(row)-1] != ')' {
				valid = false
				break
			}
			selects = append(selects, "select "+strings.TrimSpace(row[1:len(row)-1]))
		}
		if !valid {
			offset = close + 1
			continue
		}
		replacement := strings.Join(selects, " union all ")
		query = query[:valuesAt] + replacement + query[close:]
		lower = strings.ToLower(query)
		offset = valuesAt + len(replacement)
	}
	return query
}

func isCTEAsOpen(query string, open int) bool {
	index := previousNonSpace(query, open-1)
	if index < 1 || (query[index] != 's' && query[index] != 'S') {
		return false
	}
	start := index - 1
	return start >= 0 && strings.EqualFold(query[start:index+1], "as") &&
		(start == 0 || !isIdentifierByte(query[start-1]))
}

// MySQL accepts a CTE used by INSERT between the target column list and the
// SELECT source (INSERT INTO t (...) WITH ... SELECT ...), rather than SQLite's
// leading WITH ... INSERT form.
func relocateLeadingCTEInsert(query string) string {
	trimmedStart := len(query) - len(strings.TrimLeft(query, " \t\r\n"))
	if !hasKeywordAt(query, trimmedStart, "with") {
		return query
	}
	insertAt := findTopLevelKeyword(query, "insert", trimmedStart+len("with"))
	if insertAt < 0 || !hasKeywordAt(query, insertAt, "insert") {
		return query
	}
	selectAt := findTopLevelKeyword(query, "select", insertAt+len("insert"))
	if selectAt < 0 {
		return query
	}
	leading := strings.TrimSpace(query[trimmedStart:insertAt])
	insertHead := strings.TrimSpace(query[insertAt:selectAt])
	selectTail := strings.TrimSpace(query[selectAt:])
	return query[:trimmedStart] + insertHead + " " + leading + " " + selectTail
}

func findTopLevelKeyword(query, keyword string, start int) int {
	depth := 0
	var quote byte
	for index := start; index < len(query); index++ {
		char := query[index]
		if quote != 0 {
			if char == quote {
				if index+1 < len(query) && query[index+1] == quote {
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
			if depth == 0 && hasKeywordAt(query, index, keyword) {
				return index
			}
		}
	}
	return -1
}

func hasKeywordAt(query string, at int, keyword string) bool {
	if at < 0 || at+len(keyword) > len(query) || !strings.EqualFold(query[at:at+len(keyword)], keyword) {
		return false
	}
	return (at == 0 || !isIdentifierByte(query[at-1])) &&
		(at+len(keyword) == len(query) || !isIdentifierByte(query[at+len(keyword)]))
}

func previousNonSpace(value string, start int) int {
	for index := start; index >= 0; index-- {
		switch value[index] {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return index
		}
	}
	return -1
}

func matchingParen(query string, open int) int {
	if open < 0 || open >= len(query) || query[open] != '(' {
		return -1
	}
	depth := 0
	var quote byte
	for index := open; index < len(query); index++ {
		char := query[index]
		if quote != 0 {
			if char == quote {
				if index+1 < len(query) && query[index+1] == quote {
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
			depth--
			if depth == 0 {
				return index
			}
		}
	}
	return -1
}

func splitTopLevelSQL(value string, separator byte) []string {
	parts := make([]string, 0)
	start, depth := 0, 0
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
			if char == separator && depth == 0 {
				parts = append(parts, value[start:index])
				start = index + 1
			}
		}
	}
	parts = append(parts, value[start:])
	return parts
}

func replaceFirstInsertInto(query, replacement string) string {
	pattern := regexp.MustCompile(`(?i)\binsert\s+into\b`)
	location := pattern.FindStringIndex(query)
	if location == nil {
		return query
	}
	return query[:location[0]] + replacement + query[location[1]:]
}

func replaceScalarMinMax(query string) string {
	type replacement struct {
		start int
		end   int
		name  string
	}
	results := make([]replacement, 0)
	lower := strings.ToLower(query)
	for offset := 0; offset < len(query); {
		minAt := strings.Index(lower[offset:], "min(")
		maxAt := strings.Index(lower[offset:], "max(")
		at, name := -1, ""
		switch {
		case minAt >= 0 && (maxAt < 0 || minAt < maxAt):
			at, name = offset+minAt, "least"
		case maxAt >= 0:
			at, name = offset+maxAt, "greatest"
		default:
			at = -1
		}
		if at < 0 {
			break
		}
		if at > 0 && isIdentifierByte(query[at-1]) {
			offset = at + 4
			continue
		}
		end, comma := matchingCall(query, at+3)
		if end > 0 && comma {
			results = append(results, replacement{start: at, end: at + 3, name: name})
		}
		// Continue inside the current call as scalar max/min expressions are
		// commonly nested (max(max(a,b)-max(c,0),0)).
		offset = at + 4
	}
	for index := len(results) - 1; index >= 0; index-- {
		item := results[index]
		query = query[:item.start] + item.name + query[item.end:]
	}
	return query
}

// SQLite permits a WHERE predicate after an upsert's DO UPDATE assignments.
// MySQL has no equivalent syntax, so guard every assignment with the same
// predicate. This preserves the important "only replace a newer snapshot"
// semantics used by usage_monitoring_header_latest_v1.
func rewriteConditionalDuplicateUpdate(query string) string {
	const marker = "on duplicate key update"
	lower := strings.ToLower(query)
	updateAt := strings.Index(lower, marker)
	if updateAt < 0 {
		return query
	}
	assignmentsAt := updateAt + len(marker)
	whereAt := findTopLevelKeyword(query, "where", assignmentsAt)
	if whereAt < 0 {
		return query
	}
	assignments := splitTopLevelSQL(strings.TrimSpace(query[assignmentsAt:whereAt]), ',')
	condition := strings.TrimSpace(query[whereAt+len("where"):])
	targetTable := insertTargetTable(query)
	if len(assignments) == 0 || condition == "" {
		return query
	}
	rewritten := make([]string, 0, len(assignments))
	for _, assignment := range assignments {
		equals := topLevelAssignmentEquals(assignment)
		if equals < 0 {
			return query
		}
		left := strings.TrimSpace(assignment[:equals])
		right := strings.TrimSpace(assignment[equals+1:])
		if left == "" || right == "" {
			return query
		}
		fallback := left
		if targetTable != "" && isSimpleSQLIdentifier(left) {
			fallback = targetTable + "." + left
		}
		rewritten = append(rewritten, left+" = if(("+condition+"), "+right+", "+fallback+")")
	}
	return query[:assignmentsAt] + " " + strings.Join(rewritten, ", ")
}

func insertTargetTable(query string) string {
	pattern := regexp.MustCompile(`(?i)\binsert\s+into\s+([a-zA-Z_][a-zA-Z0-9_]*)`)
	match := pattern.FindStringSubmatch(query)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func isSimpleSQLIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if !isIdentifierByte(value[index]) || index == 0 && value[index] >= '0' && value[index] <= '9' {
			return false
		}
	}
	return true
}

// SQLite/PostgreSQL-style UPDATE ... SET ... FROM ... WHERE ... becomes a
// MySQL joined update. Keeping the original predicate in WHERE gives exactly
// the same row selection while the 1=1 join only exposes the source alias.
func rewriteUpdateFrom(query string) string {
	trimmedStart := len(query) - len(strings.TrimLeft(query, " \t\r\n"))
	if !hasKeywordAt(query, trimmedStart, "update") {
		return query
	}
	setAt := findTopLevelKeyword(query, "set", trimmedStart+len("update"))
	if setAt < 0 {
		return query
	}
	fromAt := findTopLevelKeyword(query, "from", setAt+len("set"))
	if fromAt < 0 {
		return query
	}
	whereAt := findTopLevelKeyword(query, "where", fromAt+len("from"))
	if whereAt < 0 {
		return query
	}
	target := strings.TrimSpace(query[trimmedStart+len("update") : setAt])
	assignments := strings.TrimSpace(query[setAt+len("set") : fromAt])
	source := strings.TrimSpace(query[fromAt+len("from") : whereAt])
	predicate := strings.TrimSpace(query[whereAt+len("where"):])
	if target == "" || assignments == "" || source == "" || predicate == "" {
		return query
	}
	targetQualifier := updateTargetQualifier(target)
	if targetQualifier != "" {
		parts := splitTopLevelSQL(assignments, ',')
		for index, assignment := range parts {
			equals := topLevelAssignmentEquals(assignment)
			if equals < 0 {
				return query
			}
			left := strings.TrimSpace(assignment[:equals])
			if isSimpleSQLIdentifier(left) {
				parts[index] = targetQualifier + "." + left + " " + strings.TrimSpace(assignment[equals:])
			}
		}
		assignments = strings.Join(parts, ", ")
	}
	return query[:trimmedStart] + "update " + target + " join " + source +
		" on 1 = 1 set " + assignments + " where " + predicate
}

func updateTargetQualifier(target string) string {
	fields := strings.Fields(target)
	if len(fields) == 1 && isSimpleSQLIdentifier(fields[0]) {
		return fields[0]
	}
	if len(fields) == 2 && isSimpleSQLIdentifier(fields[0]) && isSimpleSQLIdentifier(fields[1]) {
		return fields[1]
	}
	if len(fields) == 3 && strings.EqualFold(fields[1], "as") && isSimpleSQLIdentifier(fields[0]) && isSimpleSQLIdentifier(fields[2]) {
		return fields[2]
	}
	return ""
}

func topLevelAssignmentEquals(value string) int {
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
		case '=':
			if depth == 0 {
				return index
			}
		}
	}
	return -1
}

func matchingCall(query string, open int) (int, bool) {
	if open >= len(query) || query[open] != '(' {
		return -1, false
	}
	depth := 0
	hasTopLevelComma := false
	var quote byte
	for index := open; index < len(query); index++ {
		char := query[index]
		if quote != 0 {
			if char == quote {
				if index+1 < len(query) && query[index+1] == quote {
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
			depth--
			if depth == 0 {
				return index, hasTopLevelComma
			}
		case ',':
			if depth == 1 {
				hasTopLevelComma = true
			}
		}
	}
	return -1, false
}

func isIdentifierByte(char byte) bool {
	return char == '_' || char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9'
}

var _ driver.Connector = (*compatConnector)(nil)
var _ driver.ConnPrepareContext = (*compatConn)(nil)
var _ driver.ConnBeginTx = (*compatConn)(nil)
var _ driver.ExecerContext = (*compatConn)(nil)
var _ driver.QueryerContext = (*compatConn)(nil)
var _ driver.Pinger = (*compatConn)(nil)
var _ driver.SessionResetter = (*compatConn)(nil)
var _ driver.Validator = (*compatConn)(nil)
var _ driver.NamedValueChecker = (*compatConn)(nil)

var errUnsupportedReturning = errors.New("mysql compatibility returning query is unsupported")
