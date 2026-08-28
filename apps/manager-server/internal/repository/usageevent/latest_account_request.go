package usageevent

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

const (
	latestRequestAuthFileIndex = "idx_usage_events_latest_request_auth_file"
	latestRequestSourceIndex   = "idx_usage_events_latest_request_source"
)

// LatestAccountRequestQuery identifies one credential using the immutable
// snapshot captured with a request. AuthFileSnapshot is the primary identity;
// Source is used only for records created before auth-file snapshots existed.
type LatestAccountRequestQuery struct {
	RequestIndex     int
	AuthFileSnapshot string
	AuthIndex        string
}

// LatestAccountRequest contains the safe diagnostics needed by the credential
// list. Sensitive database-only fields such as fail_body and raw_json are
// deliberately not represented here.
type LatestAccountRequest struct {
	RequestIndex    int
	TimestampMS     int64
	Failed          bool
	FailStatusCode  sql.NullInt64
	FailSummary     string
	HeaderErrorKind string
	HeaderErrorCode string
	HeaderTraceID   string
}

type rankedAccountRequest struct {
	LatestAccountRequest
	id int64
}

type latestRequestPredicate struct {
	index string
	sql   string
	args  []any
}

func (r *repository) RecentAccountRequests(
	ctx context.Context,
	targets []LatestAccountRequestQuery,
	limit int,
) ([]LatestAccountRequest, error) {
	if len(targets) == 0 || limit <= 0 {
		return []LatestAccountRequest{}, nil
	}
	ready, err := r.latestRequestIndexesReady(ctx)
	if err != nil {
		return nil, err
	}
	if ready {
		return r.recentAccountRequestsIndexed(ctx, targets, limit)
	}
	return r.recentAccountRequestsBatched(ctx, targets, limit)
}

func (r *repository) latestRequestIndexesReady(ctx context.Context) (bool, error) {
	var count int
	err := r.db.QueryRowContext(ctx, `select count(*) from sqlite_master
		where type = 'index'
			and tbl_name = 'usage_events'
			and name in (?, ?)`,
		latestRequestAuthFileIndex,
		latestRequestSourceIndex,
	).Scan(&count)
	if err == nil {
		return count == 2, nil
	}

	// MySQL does not expose sqlite_master. Its schema migration creates the
	// same canonical index names, so inspect information_schema instead.
	err = r.db.QueryRowContext(ctx, `select count(distinct index_name) from information_schema.statistics
		where table_schema = database()
			and table_name = 'usage_events'
			and index_name in (?, ?)`,
		latestRequestAuthFileIndex,
		latestRequestSourceIndex,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count == 2, nil
}

func (r *repository) recentAccountRequestsIndexed(
	ctx context.Context,
	targets []LatestAccountRequestQuery,
	limit int,
) ([]LatestAccountRequest, error) {
	requests := make([]LatestAccountRequest, 0, len(targets)*limit)
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		authFileSnapshot := strings.TrimSpace(target.AuthFileSnapshot)
		if authFileSnapshot == "" {
			continue
		}
		authIndex := strings.TrimSpace(target.AuthIndex)
		snapshot, err := r.recentAccountRequestsByPredicates(
			ctx,
			target.RequestIndex,
			limit,
			snapshotLatestRequestPredicates(authFileSnapshot, authIndex),
		)
		if err != nil {
			return nil, err
		}
		legacy, err := r.recentAccountRequestsByPredicates(
			ctx,
			target.RequestIndex,
			limit,
			legacyLatestRequestPredicates(authFileSnapshot, authIndex),
		)
		if err != nil {
			return nil, err
		}
		requests = append(requests, mergeRecentAccountRequests(limit, snapshot, legacy)...)
	}
	return requests, nil
}

func snapshotLatestRequestPredicates(authFileSnapshot, authIndex string) []latestRequestPredicate {
	return latestRequestPredicates(
		latestRequestAuthFileIndex,
		`e.auth_file_snapshot collate nocase = ?`,
		[]any{authFileSnapshot},
		authIndex,
	)
}

func legacyLatestRequestPredicates(authFileSnapshot, authIndex string) []latestRequestPredicate {
	filePredicates := []latestRequestPredicate{
		{index: latestRequestSourceIndex, sql: `e.auth_file_snapshot is null and e.source collate nocase = ?`, args: []any{authFileSnapshot}},
		{index: latestRequestSourceIndex, sql: `e.auth_file_snapshot = '' and e.source collate nocase = ?`, args: []any{authFileSnapshot}},
	}
	predicates := make([]latestRequestPredicate, 0, 4)
	for _, filePredicate := range filePredicates {
		predicates = append(predicates, latestRequestPredicates(
			filePredicate.index,
			filePredicate.sql,
			filePredicate.args,
			authIndex,
		)...)
	}
	return predicates
}

func latestRequestPredicates(index, baseSQL string, baseArgs []any, authIndex string) []latestRequestPredicate {
	if authIndex != "" {
		return []latestRequestPredicate{{
			index: index,
			sql:   baseSQL + ` and e.auth_index collate nocase = ?`,
			args:  append(append([]any{}, baseArgs...), authIndex),
		}}
	}
	return []latestRequestPredicate{
		{index: index, sql: baseSQL + ` and e.auth_index is null`, args: append([]any{}, baseArgs...)},
		{index: index, sql: baseSQL + ` and e.auth_index collate nocase = ''`, args: append([]any{}, baseArgs...)},
	}
}

func (r *repository) recentAccountRequestsByPredicates(
	ctx context.Context,
	requestIndex int,
	limit int,
	predicates []latestRequestPredicate,
) ([]rankedAccountRequest, error) {
	parts := make([][]rankedAccountRequest, 0, len(predicates))
	for _, predicate := range predicates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows, err := r.recentAccountRequestsByPredicate(ctx, requestIndex, limit, predicate)
		if err != nil {
			return nil, err
		}
		parts = append(parts, rows)
	}
	return mergeRankedAccountRequests(limit, parts...), nil
}

func (r *repository) recentAccountRequestsByPredicate(
	ctx context.Context,
	requestIndex int,
	limit int,
	predicate latestRequestPredicate,
) ([]rankedAccountRequest, error) {
	args := append(append([]any{}, predicate.args...), limit)
	query := latestAccountRequestQuery(predicate)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	requests := make([]rankedAccountRequest, 0, limit)
	for rows.Next() {
		var request rankedAccountRequest
		var failed int
		if err := rows.Scan(
			&request.id,
			&request.TimestampMS,
			&failed,
			&request.FailStatusCode,
			&request.FailSummary,
			&request.HeaderErrorKind,
			&request.HeaderErrorCode,
			&request.HeaderTraceID,
		); err != nil {
			return nil, err
		}
		request.RequestIndex = requestIndex
		request.Failed = failed != 0
		requests = append(requests, request)
	}
	return requests, rows.Err()
}

func latestAccountRequestQuery(predicate latestRequestPredicate) string {
	return fmt.Sprintf(`select /*+ INDEX(e %s) */
	e.id,
	e.timestamp_ms,
	e.failed,
	e.fail_status_code,
	coalesce(e.fail_summary, ''),
	coalesce(e.header_error_kind, ''),
	coalesce(e.header_error_code, ''),
	coalesce(e.header_trace_id, '')
from usage_events e indexed by %s
where %s
order by e.timestamp_ms desc, e.id desc
limit ?`, predicate.index, predicate.index, predicate.sql)
}

func (r *repository) recentAccountRequestsBatched(
	ctx context.Context,
	targets []LatestAccountRequestQuery,
	limit int,
) ([]LatestAccountRequest, error) {

	values := make([]string, 0, len(targets))
	args := make([]any, 0, len(targets)*3+1)
	for _, target := range targets {
		authFileSnapshot := strings.TrimSpace(target.AuthFileSnapshot)
		if authFileSnapshot == "" {
			continue
		}
		values = append(values, "(?, ?, ?)")
		args = append(
			args,
			target.RequestIndex,
			authFileSnapshot,
			strings.TrimSpace(target.AuthIndex),
		)
	}
	if len(values) == 0 {
		return []LatestAccountRequest{}, nil
	}
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, `with credential_targets(
	request_index, auth_file_snapshot, auth_index
) as (
	values `+strings.Join(values, ",")+`
), snapshot_candidates as (
	select
		t.request_index,
		e.id,
		e.timestamp_ms,
		e.failed,
		e.fail_status_code,
		coalesce(e.fail_summary, '') as fail_summary,
		coalesce(e.header_error_kind, '') as header_error_kind,
		coalesce(e.header_error_code, '') as header_error_code,
		coalesce(e.header_trace_id, '') as header_trace_id
	from credential_targets t
	join usage_events e
		on e.auth_file_snapshot collate nocase = t.auth_file_snapshot
		and coalesce(e.auth_index, '') collate nocase = t.auth_index
), legacy_source_candidates as (
	select
		t.request_index,
		e.id,
		e.timestamp_ms,
		e.failed,
		e.fail_status_code,
		coalesce(e.fail_summary, '') as fail_summary,
		coalesce(e.header_error_kind, '') as header_error_kind,
		coalesce(e.header_error_code, '') as header_error_code,
		coalesce(e.header_trace_id, '') as header_trace_id
	from credential_targets t
	join usage_events e
		on coalesce(e.auth_file_snapshot, '') = ''
		and e.source collate nocase = t.auth_file_snapshot
		and coalesce(e.auth_index, '') collate nocase = t.auth_index
), candidates as (
	select * from snapshot_candidates
	union all
	select * from legacy_source_candidates
), ranked as (
	select
		request_index,
		timestamp_ms,
		failed,
		fail_status_code,
		fail_summary,
		header_error_kind,
		header_error_code,
		header_trace_id,
		row_number() over (
			partition by request_index
			order by timestamp_ms desc, id desc
		) as row_rank
	from candidates
)
select
	request_index,
	timestamp_ms,
	failed,
	fail_status_code,
	fail_summary,
	header_error_kind,
	header_error_code,
	header_trace_id
from ranked
where row_rank <= ?
order by request_index, row_rank`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	requests := make([]LatestAccountRequest, 0, len(values)*limit)
	for rows.Next() {
		var request LatestAccountRequest
		var failed int
		if err := rows.Scan(
			&request.RequestIndex,
			&request.TimestampMS,
			&failed,
			&request.FailStatusCode,
			&request.FailSummary,
			&request.HeaderErrorKind,
			&request.HeaderErrorCode,
			&request.HeaderTraceID,
		); err != nil {
			return nil, err
		}
		request.Failed = failed != 0
		requests = append(requests, request)
	}
	return requests, rows.Err()
}

func mergeRecentAccountRequests(limit int, parts ...[]rankedAccountRequest) []LatestAccountRequest {
	merged := mergeRankedAccountRequests(limit, parts...)
	requests := make([]LatestAccountRequest, len(merged))
	for index, request := range merged {
		requests[index] = request.LatestAccountRequest
	}
	return requests
}

func mergeRankedAccountRequests(limit int, parts ...[]rankedAccountRequest) []rankedAccountRequest {
	total := 0
	for _, part := range parts {
		total += len(part)
	}
	merged := make([]rankedAccountRequest, 0, total)
	for _, part := range parts {
		merged = append(merged, part...)
	}
	sort.SliceStable(merged, func(left, right int) bool {
		if merged[left].TimestampMS != merged[right].TimestampMS {
			return merged[left].TimestampMS > merged[right].TimestampMS
		}
		return merged[left].id > merged[right].id
	})
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}
