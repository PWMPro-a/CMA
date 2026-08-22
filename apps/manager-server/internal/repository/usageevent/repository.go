package usageevent

import (
	"context"
	"database/sql"
	"io"
	"strings"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/usage"
)

type Repository interface {
	InsertBatch(ctx context.Context, events []model.UsageEvent) (model.InsertResult, error)
	ListRecent(ctx context.Context, limit int) ([]model.UsageEvent, error)
	ListSupplyUsageMinutes(ctx context.Context, sinceMS int64) ([]SupplyUsageMinute, error)
	ListSupplyQuotaCalibrationEvents(ctx context.Context, sinceMS int64, limit int) ([]usage.Event, error)
	ListSupplyQuotaWindowUsage(ctx context.Context, targets []SupplyQuotaWindowUsageQuery) ([]SupplyQuotaWindowUsage, error)
	ModelUsageSummary(ctx context.Context, limit int) (model.ModelUsageSummary, error)
	BackfillResponseMetadata(ctx context.Context, batchLimit int) (int, error)
	BackfillRoutingDiagnostics(ctx context.Context, batchLimit int) (int, error)
	Count(ctx context.Context) (int64, error)
	ExportJSONL(ctx context.Context) ([]byte, error)
	WriteCompatibleUsage(ctx context.Context, writer io.Writer, limit int) error
	WriteExportJSONL(ctx context.Context, writer io.Writer, limit int) error
	AggregateBetween(ctx context.Context, fromMs, toMs int64) (Aggregate, error)
	TopModelsBetween(ctx context.Context, fromMs, toMs int64, limit int) ([]ModelStat, error)
	ModelStatsBetween(ctx context.Context, fromMs, toMs int64) ([]ModelStat, error)
	RecentFailuresBetween(ctx context.Context, fromMs, toMs int64, limit int) ([]RecentFailure, error)
	HourlyTimelineBetween(ctx context.Context, fromMs, toMs int64) ([]TimelinePoint, error)
	BucketTimelineBetween(ctx context.Context, fromMs, toMs int64, bucketMs int64) ([]TimelinePoint, error)
	AggregateWithFilter(ctx context.Context, filter AnalyticsFilter) (Aggregate, error)
	ModelStatsWithFilter(ctx context.Context, filter AnalyticsFilter, limit int) ([]ModelStat, error)
	TimelineWithFilter(ctx context.Context, filter AnalyticsFilter, granularity string, location *time.Location) ([]TimelinePoint, error)
	LatencyAnalyticsWithFilter(ctx context.Context, filter AnalyticsFilter, granularity string, location *time.Location) ([]LatencyPercentiles, LatencySummary, error)
	LatencyPercentilesWithFilter(ctx context.Context, filter AnalyticsFilter, granularity string, location *time.Location) ([]LatencyPercentiles, error)
	LatencySummaryWithFilter(ctx context.Context, filter AnalyticsFilter) (LatencySummary, error)
	HourlyDistributionWithFilter(ctx context.Context, filter AnalyticsFilter, location *time.Location) ([]HourlyPoint, error)
	FilterOptionValuesWithFilter(ctx context.Context, filter AnalyticsFilter) (FilterOptionValues, error)
	FilterSelectorValuesWithFilter(ctx context.Context, filter AnalyticsFilter) (FilterSelectorValues, error)
	HeatmapWithFilter(ctx context.Context, filter AnalyticsFilter, location *time.Location) ([]HeatmapPoint, error)
	ChannelModelStatsWithFilter(ctx context.Context, filter AnalyticsFilter) ([]ChannelModelStat, error)
	FailureSourcesWithFilter(ctx context.Context, filter AnalyticsFilter) ([]FailureSourceStat, error)
	AccountModelStatsWithFilter(ctx context.Context, filter AnalyticsFilter) ([]AccountModelStat, error)
	AccountWindowModelStats(ctx context.Context, windows []AccountWindowUsageQuery) ([]AccountWindowModelStat, error)
	RecentAccountRequests(ctx context.Context, targets []LatestAccountRequestQuery, limit int) ([]LatestAccountRequest, error)
	CredentialModelStatsWithFilter(ctx context.Context, filter AnalyticsFilter) ([]CredentialModelStat, error)
	CredentialTimelineWithFilter(ctx context.Context, filter AnalyticsFilter, granularity string, location *time.Location) ([]CredentialTimelinePoint, error)
	APIKeyTimelineWithFilter(ctx context.Context, filter AnalyticsFilter, granularity string, location *time.Location) ([]APIKeyTimelinePoint, error)
	APIKeyModelStatsWithFilter(ctx context.Context, filter AnalyticsFilter) ([]APIKeyModelStat, error)
	TaskBucketsWithFilter(ctx context.Context, filter AnalyticsFilter) ([]TaskBucket, error)
	RecentFailuresWithFilter(ctx context.Context, filter AnalyticsFilter, limit int) ([]RecentFailure, error)
	EventsPageWithFilter(ctx context.Context, filter AnalyticsFilter, beforeMS int64, beforeID int64, limit int) (EventsPage, error)
	EventsCountWithFilter(ctx context.Context, filter AnalyticsFilter) (int64, error)
	LatestHeaderSnapshots(ctx context.Context, sinceMS int64, limit int) ([]HeaderSnapshot, error)
	ActiveDaysWithFilter(ctx context.Context, filter AnalyticsFilter, location *time.Location) (int64, error)
	ZeroTokenModelsWithFilter(ctx context.Context, filter AnalyticsFilter) ([]string, error)
	RoutingDiagnosticsWithFilter(ctx context.Context, filter AnalyticsFilter) (RoutingDiagnostics, error)
	DeleteCredentialHistory(ctx context.Context, authFileSnapshot, authIndex string) (int64, error)
	DeleteCredentialIdentityHistory(ctx context.Context, identity model.CredentialIdentity) (int64, error)
}

// SupplyUsageMinute is the small restart-recovery snapshot consumed by smart
// replenishment. It intentionally contains only aggregate values so warming
// the in-memory planner never loads individual request records.
type SupplyUsageMinute struct {
	MinuteMS       int64
	Requests       int64
	Successful     int64
	Failed         int64
	TotalTokens    int64
	LatencySumMS   int64
	LatencySamples int64
}

// SupplyQuotaWindowUsageQuery identifies one credential's current provider
// quota window. RequestIndex correlates results without exposing credential
// details outside the repository call.
type SupplyQuotaWindowUsageQuery struct {
	RequestIndex     int
	AuthFileSnapshot string
	AuthIndex        string
	FromMS           int64
	ToMS             int64
}

type SupplyQuotaWindowUsage struct {
	RequestIndex int
	TotalTokens  int64
	FirstSeenMS  int64
	LastSeenMS   int64
}

type repository struct {
	db *sql.DB
}

func New(db *sql.DB) Repository {
	return &repository{db: db}
}

func (r *repository) InsertBatch(ctx context.Context, events []model.UsageEvent) (model.InsertResult, error) {
	if len(events) == 0 {
		return model.InsertResult{}, nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return model.InsertResult{}, err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	ledgerStmt, err := tx.PrepareContext(ctx, `insert or ignore into usage_event_identity_ledger (
		event_hash,
		raw_event_id,
		timestamp_ms,
		bucket_ms,
		aggregate_schema_version,
		first_seen_at_ms,
		updated_at_ms
	) values (?, null, ?, ?, 0, ?, ?)`)
	if err != nil {
		return model.InsertResult{}, err
	}
	defer ledgerStmt.Close()

	attachLedgerStmt, err := tx.PrepareContext(ctx, `update usage_event_identity_ledger set
		raw_event_id = ?,
		timestamp_ms = ?,
		bucket_ms = ?,
		updated_at_ms = ?
	where event_hash = ?`)
	if err != nil {
		return model.InsertResult{}, err
	}
	defer attachLedgerStmt.Close()

	attachExistingLedgerStmt, err := tx.PrepareContext(ctx, `update usage_event_identity_ledger set
		raw_event_id = (select id from usage_events where event_hash = ?),
		timestamp_ms = (select timestamp_ms from usage_events where event_hash = ?),
		bucket_ms = (select timestamp_ms - (timestamp_ms % 3600000) from usage_events where event_hash = ?),
		first_seen_at_ms = coalesce((select case when created_at_ms > 0 then created_at_ms end from usage_events where event_hash = ?), first_seen_at_ms),
		updated_at_ms = ?
	where event_hash = ?`)
	if err != nil {
		return model.InsertResult{}, err
	}
	defer attachExistingLedgerStmt.Close()

	stmt, err := tx.PrepareContext(ctx, `insert or ignore into usage_events (
		request_id, event_hash, timestamp_ms, timestamp, provider, executor_type, model, endpoint, method, path,
		auth_type, auth_index, source, source_hash, api_key_hash,
		account_snapshot, auth_label_snapshot, auth_file_snapshot, auth_provider_snapshot, auth_project_id_snapshot, auth_snapshot_at_ms,
		requested_model, resolved_model, reasoning_effort, service_tier, request_service_tier, response_service_tier, cache_input_mode,
		input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_tokens, cache_read_tokens, cache_creation_tokens,
		normalized_uncached_input_tokens, normalized_total_input_tokens, normalized_cache_read_tokens, normalized_cache_creation_tokens, total_tokens,
		latency_ms, ttft_ms, failed, fail_status_code, fail_summary,
		response_metadata_json, header_quota_recover_at_ms, header_quota_used_percent, header_quota_plan_type, header_error_kind, header_error_code, header_trace_id,
		fail_body, raw_json, created_at_ms
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return model.InsertResult{}, err
	}
	defer stmt.Close()

	routingStmt, err := tx.PrepareContext(ctx, `insert or replace into usage_routing_diagnostics (
		event_hash, timestamp_ms, affinity_outcome, session_source, binding_generation,
		quota_used_percent, pck_shadow_sampled, pck_original_hash, pck_context_root_hash, pck_prefix_generation
	) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return model.InsertResult{}, err
	}
	defer routingStmt.Close()

	result := model.InsertResult{}
	for _, event := range events {
		ledgerNowMS := event.CreatedAtMS
		if ledgerNowMS <= 0 {
			ledgerNowMS = time.Now().UnixMilli()
		}
		bucketMS := event.TimestampMS - event.TimestampMS%(60*60*1000)
		ledgerResult, err := ledgerStmt.ExecContext(
			ctx,
			event.EventHash,
			event.TimestampMS,
			bucketMS,
			ledgerNowMS,
			ledgerNowMS,
		)
		if err != nil {
			return model.InsertResult{}, err
		}
		claimed, _ := ledgerResult.RowsAffected()
		if claimed == 0 {
			result.Skipped++
			continue
		}

		accounting := usage.NormalizeCacheAccounting(usage.CacheInputContext{
			ExplicitMode:     event.CacheInputMode,
			ExecutorType:     event.ExecutorType,
			Provider:         event.Provider,
			ProviderSnapshot: event.AuthProviderSnapshot,
			ResolvedModel:    event.ResolvedModel,
			RequestedModel:   event.RequestedModel,
			DisplayModel:     event.Model,
		}, event.InputTokens, event.CachedTokens, event.CacheTokens, event.CacheReadTokens, event.CacheCreationTokens)
		event.CacheInputMode = accounting.Mode
		event.NormalizedUncachedInputTokens = accounting.UncachedInputTokens
		event.NormalizedTotalInputTokens = accounting.TotalInputTokens
		event.NormalizedCacheReadTokens = accounting.CacheReadTokens
		event.NormalizedCacheCreationTokens = accounting.CacheCreationTokens
		if event.TotalTokens <= 0 {
			event.TotalTokens = accounting.TotalInputTokens + max(event.OutputTokens, int64(0)) + max(event.ReasoningTokens, int64(0))
		}
		if event.RequestServiceTier == "" {
			event.RequestServiceTier = event.ServiceTier
		}
		event.ServiceTier = usage.EffectiveServiceTier(usage.CacheInputContext{
			ExecutorType:     event.ExecutorType,
			Provider:         event.Provider,
			ProviderSnapshot: event.AuthProviderSnapshot,
			AuthType:         event.AuthType,
		}, event.RequestServiceTier, event.ServiceTier, event.ResponseServiceTier)
		failed := 0
		if event.Failed {
			failed = 1
		}
		metadataJSON, quotaRecoverAtMS, quotaUsedPercent, quotaPlanType, errorKind, errorCode, traceID := responseHeaderDerivedForInsert(event)
		failSummarySource := event.FailSummary
		if failSummarySource == "" {
			failSummarySource = event.FailBody
		}
		failSummary := usage.FailSummaryFromBody(failSummarySource)
		rawJSON := usage.SafeRawJSON(event.RawJSON)
		res, err := stmt.ExecContext(
			ctx,
			nullString(event.RequestID),
			event.EventHash,
			event.TimestampMS,
			event.Timestamp,
			nullString(event.Provider),
			nullString(event.ExecutorType),
			event.Model,
			nullString(event.Endpoint),
			nullString(event.Method),
			nullString(event.Path),
			nullString(event.AuthType),
			nullString(event.AuthIndex),
			nullString(event.Source),
			nullString(event.SourceHash),
			nullString(event.APIKeyHash),
			nullString(event.AccountSnapshot),
			nullString(event.AuthLabelSnapshot),
			nullString(event.AuthFileSnapshot),
			nullString(event.AuthProviderSnapshot),
			nullString(event.AuthProjectIDSnapshot),
			nullPositiveInt64(event.AuthSnapshotAtMS),
			nullString(event.RequestedModel),
			nullString(event.ResolvedModel),
			nullString(event.ReasoningEffort),
			nullString(event.ServiceTier),
			nullString(event.RequestServiceTier),
			nullString(event.ResponseServiceTier),
			nullString(event.CacheInputMode),
			event.InputTokens,
			event.OutputTokens,
			event.ReasoningTokens,
			event.CachedTokens,
			event.CacheTokens,
			event.CacheReadTokens,
			event.CacheCreationTokens,
			event.NormalizedUncachedInputTokens,
			event.NormalizedTotalInputTokens,
			event.NormalizedCacheReadTokens,
			event.NormalizedCacheCreationTokens,
			event.TotalTokens,
			nullInt(event.LatencyMS),
			nullInt(event.TTFTMS),
			failed,
			nullPositiveInt64(int64(event.FailStatusCode)),
			nullString(failSummary),
			nullString(metadataJSON),
			nullPositiveInt64(quotaRecoverAtMS),
			nullFloat(quotaUsedPercent),
			nullString(quotaPlanType),
			nullString(errorKind),
			nullString(errorCode),
			nullString(traceID),
			nullString(event.FailBody),
			nullString(rawJSON),
			event.CreatedAtMS,
		)
		if err != nil {
			return model.InsertResult{}, err
		}
		affected, _ := res.RowsAffected()
		if affected > 0 {
			rawEventID, err := res.LastInsertId()
			if err != nil {
				return model.InsertResult{}, err
			}
			if _, err := attachLedgerStmt.ExecContext(
				ctx,
				rawEventID,
				event.TimestampMS,
				bucketMS,
				ledgerNowMS,
				event.EventHash,
			); err != nil {
				return model.InsertResult{}, err
			}
			result.Inserted++
			result.InsertedEventHashes = append(result.InsertedEventHashes, event.EventHash)
			if routing := usageRoutingDiagnostics(event.ResponseMetadata); routing != nil {
				if _, err := routingStmt.ExecContext(
					ctx,
					event.EventHash,
					event.TimestampMS,
					nullString(routing.AffinityOutcome),
					nullString(routing.SessionSource),
					routing.BindingGeneration,
					nullFloat(routing.QuotaUsedPercent),
					boolInt(routing.PCKShadowSampled != nil && *routing.PCKShadowSampled),
					nullString(routing.PCKOriginalHash),
					nullString(routing.PCKContextRootHash),
					nullString(routing.PCKPrefixGeneration),
				); err != nil {
					return model.InsertResult{}, err
				}
			}
		} else {
			if _, err := attachExistingLedgerStmt.ExecContext(
				ctx,
				event.EventHash,
				event.EventHash,
				event.EventHash,
				event.EventHash,
				ledgerNowMS,
				event.EventHash,
			); err != nil {
				return model.InsertResult{}, err
			}
			result.Skipped++
		}
	}
	if err := tx.Commit(); err != nil {
		return model.InsertResult{}, err
	}
	return result, nil
}

func usageRoutingDiagnostics(metadata *usage.ResponseHeaderMetadata) *usage.HeaderRoutingMetadata {
	if metadata == nil || metadata.Routing == nil {
		return nil
	}
	routing := metadata.Routing
	if routing.AffinityOutcome == "" && routing.SessionSource == "" && routing.BindingGeneration == 0 &&
		routing.QuotaUsedPercent == nil && routing.PCKShadowSampled == nil && routing.PCKOriginalHash == "" &&
		routing.PCKContextRootHash == "" && routing.PCKPrefixGeneration == "" {
		return nil
	}
	return routing
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (r *repository) ListSupplyUsageMinutes(ctx context.Context, sinceMS int64) ([]SupplyUsageMinute, error) {
	rows, err := r.db.QueryContext(ctx, `select
		(timestamp_ms / 60000) * 60000 as minute_ms,
		count(*) as requests,
		sum(case when failed = 0 then 1 else 0 end) as successful,
		sum(case when failed <> 0 then 1 else 0 end) as failed,
		sum(case
			when total_tokens > input_tokens + output_tokens + reasoning_tokens then total_tokens
			else input_tokens + output_tokens + reasoning_tokens
		end) as total_tokens,
		sum(case when failed = 0 and latency_ms > 0 then latency_ms else 0 end) as latency_sum_ms,
		sum(case when failed = 0 and latency_ms > 0 then 1 else 0 end) as latency_samples
		from usage_events
		where timestamp_ms >= ?
			and (
				lower(coalesce(provider, '')) like '%codex%'
				or lower(coalesce(provider, '')) like '%openai%'
				or lower(coalesce(executor_type, '')) like '%codex%'
				or lower(coalesce(executor_type, '')) like '%openai%'
				or lower(coalesce(auth_type, '')) like '%codex%'
				or lower(coalesce(auth_type, '')) like '%openai%'
				or lower(coalesce(auth_provider_snapshot, '')) like '%codex%'
				or lower(coalesce(auth_provider_snapshot, '')) like '%openai%'
				or coalesce(auth_index, '') <> ''
				or coalesce(account_snapshot, '') <> ''
				or coalesce(auth_file_snapshot, '') <> ''
			)
		group by minute_ms
		order by minute_ms asc`, sinceMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	minutes := make([]SupplyUsageMinute, 0, 30)
	for rows.Next() {
		var minute SupplyUsageMinute
		if err := rows.Scan(
			&minute.MinuteMS,
			&minute.Requests,
			&minute.Successful,
			&minute.Failed,
			&minute.TotalTokens,
			&minute.LatencySumMS,
			&minute.LatencySamples,
		); err != nil {
			return nil, err
		}
		minutes = append(minutes, minute)
	}
	return minutes, rows.Err()
}

// ListSupplyQuotaCalibrationEvents returns only the small set of fields needed
// to combine each account's historical Token use with its quota percentage.
// Header-less rows are included because their Token usage belongs to the same
// active account window and must be accumulated before the next quota header.
// The smart-supply warm path still avoids response bodies and raw JSON from the
// multi-gigabyte usage table.
func (r *repository) ListSupplyQuotaCalibrationEvents(ctx context.Context, sinceMS int64, limit int) ([]usage.Event, error) {
	if limit <= 0 {
		limit = 100_000
	}
	if limit > 200_000 {
		limit = 200_000
	}
	rows, err := r.db.QueryContext(ctx, `select
		timestamp_ms,
		coalesce(auth_index, ''),
		coalesce(account_snapshot, ''),
		coalesce(auth_file_snapshot, ''),
		coalesce(input_tokens, 0),
		coalesce(output_tokens, 0),
		coalesce(reasoning_tokens, 0),
		coalesce(total_tokens, 0),
		coalesce(failed, 0),
		coalesce(response_metadata_json, ''),
		header_quota_used_percent,
		coalesce(header_quota_recover_at_ms, 0),
		coalesce(header_quota_plan_type, '')
		from (
			select
				id,
				timestamp_ms,
				auth_index,
				account_snapshot,
				auth_file_snapshot,
				input_tokens,
				output_tokens,
				reasoning_tokens,
				total_tokens,
				failed,
				response_metadata_json,
				header_quota_used_percent,
				header_quota_recover_at_ms,
				header_quota_plan_type
			from usage_events indexed by idx_usage_events_timestamp
			where timestamp_ms >= ?
				and (
					coalesce(auth_file_snapshot, '') <> ''
					or coalesce(auth_index, '') <> ''
					or coalesce(account_snapshot, '') <> ''
				)
			order by timestamp_ms desc, id desc
			limit ?
		) as recent_usage_events
		order by timestamp_ms asc, id asc`, sinceMS, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]usage.Event, 0, min(limit, 16_384))
	for rows.Next() {
		var event usage.Event
		var failed int
		var usedPercent sql.NullFloat64
		if err := rows.Scan(
			&event.TimestampMS,
			&event.AuthIndex,
			&event.AccountSnapshot,
			&event.AuthFileSnapshot,
			&event.InputTokens,
			&event.OutputTokens,
			&event.ReasoningTokens,
			&event.TotalTokens,
			&failed,
			&event.ResponseMetadataJSON,
			&usedPercent,
			&event.HeaderQuotaRecoverAtMS,
			&event.HeaderQuotaPlanType,
		); err != nil {
			return nil, err
		}
		event.Failed = failed != 0
		if usedPercent.Valid {
			value := usedPercent.Float64
			event.HeaderQuotaUsedPercent = &value
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// ListSupplyQuotaWindowUsage aggregates complete current-window usage for a
// bounded set of credentials. The account-scoped index prevents a busy pool
// from truncating quiet accounts or forcing a scan of the global event tail.
func (r *repository) ListSupplyQuotaWindowUsage(ctx context.Context, targets []SupplyQuotaWindowUsageQuery) ([]SupplyQuotaWindowUsage, error) {
	if len(targets) == 0 {
		return []SupplyQuotaWindowUsage{}, nil
	}

	const batchSize = 400
	result := make([]SupplyQuotaWindowUsage, 0, len(targets))
	for start := 0; start < len(targets); start += batchSize {
		end := min(start+batchSize, len(targets))
		batch := targets[start:end]
		values := make([]string, 0, len(batch))
		args := make([]any, 0, len(batch)*5)
		for _, target := range batch {
			values = append(values, "(?, ?, ?, ?, ?)")
			args = append(args,
				target.RequestIndex,
				strings.TrimSpace(target.AuthFileSnapshot),
				strings.TrimSpace(target.AuthIndex),
				target.FromMS,
				target.ToMS,
			)
		}

		rows, err := r.db.QueryContext(ctx, `with quota_targets(
			request_index, auth_file_snapshot, auth_index, from_ms, to_ms
		) as (values `+strings.Join(values, ",")+`)
			select
				t.request_index,
				coalesce(sum(case when coalesce(e.failed, 0) = 0 then max(
					coalesce(e.total_tokens, 0),
					coalesce(e.input_tokens, 0) + coalesce(e.output_tokens, 0) + coalesce(e.reasoning_tokens, 0)
				) else 0 end), 0),
				coalesce(min(e.timestamp_ms), 0),
				coalesce(max(e.timestamp_ms), 0)
		from quota_targets t
		left join usage_events e indexed by idx_usage_events_latest_request_auth_file
			on e.auth_file_snapshot = t.auth_file_snapshot collate nocase
			and (t.auth_index = '' or e.auth_index = t.auth_index collate nocase)
			and e.timestamp_ms >= t.from_ms
			and e.timestamp_ms < t.to_ms
		group by t.request_index
		order by t.request_index`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var item SupplyQuotaWindowUsage
			if err := rows.Scan(&item.RequestIndex, &item.TotalTokens, &item.FirstSeenMS, &item.LastSeenMS); err != nil {
				_ = rows.Close()
				return nil, err
			}
			result = append(result, item)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (r *repository) ListRecent(ctx context.Context, limit int) ([]model.UsageEvent, error) {
	if limit <= 0 {
		limit = 50000
	}
	rows, err := r.db.QueryContext(ctx, `select
		request_id, event_hash, timestamp_ms, timestamp, provider, executor_type, model, endpoint, method, path,
		auth_type, auth_index, source, source_hash, api_key_hash,
		account_snapshot, auth_label_snapshot, auth_file_snapshot, auth_provider_snapshot, auth_project_id_snapshot, auth_snapshot_at_ms,
		requested_model, resolved_model, reasoning_effort, service_tier, request_service_tier, response_service_tier, cache_input_mode,
		input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_tokens, cache_read_tokens, cache_creation_tokens,
		normalized_uncached_input_tokens, normalized_total_input_tokens, normalized_cache_read_tokens, normalized_cache_creation_tokens, total_tokens,
		latency_ms, ttft_ms, failed, fail_status_code, fail_summary,
		coalesce(response_metadata_json, ''), header_quota_recover_at_ms, header_quota_used_percent, coalesce(header_quota_plan_type, ''), coalesce(header_error_kind, ''), coalesce(header_error_code, ''), coalesce(header_trace_id, ''),
		coalesce(raw_json, ''), created_at_ms
		from usage_events
		order by timestamp_ms desc, id desc
		limit ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := make([]model.UsageEvent, 0)
	for rows.Next() {
		var event model.UsageEvent
		var requestID, provider, executorType, endpoint, method, path, authType, authIndex, source, sourceHash, apiKeyHash, accountSnapshot, authLabelSnapshot, authFileSnapshot, authProviderSnapshot, authProjectIDSnapshot, requestedModel, resolvedModel, reasoningEffort, serviceTier, requestServiceTier, responseServiceTier, cacheInputMode, failSummary sql.NullString
		var responseMetadataJSON, quotaPlanType, errorKind, errorCode, traceID, rawJSON string
		var authSnapshotAt sql.NullInt64
		var latency, ttft sql.NullInt64
		var failStatusCode sql.NullInt64
		var quotaRecoverAt sql.NullInt64
		var quotaUsedPercent sql.NullFloat64
		var normalizedUncachedInput, normalizedTotalInput, normalizedCacheRead, normalizedCacheCreation sql.NullInt64
		var failed int
		if err := rows.Scan(
			&requestID,
			&event.EventHash,
			&event.TimestampMS,
			&event.Timestamp,
			&provider,
			&executorType,
			&event.Model,
			&endpoint,
			&method,
			&path,
			&authType,
			&authIndex,
			&source,
			&sourceHash,
			&apiKeyHash,
			&accountSnapshot,
			&authLabelSnapshot,
			&authFileSnapshot,
			&authProviderSnapshot,
			&authProjectIDSnapshot,
			&authSnapshotAt,
			&requestedModel,
			&resolvedModel,
			&reasoningEffort,
			&serviceTier,
			&requestServiceTier,
			&responseServiceTier,
			&cacheInputMode,
			&event.InputTokens,
			&event.OutputTokens,
			&event.ReasoningTokens,
			&event.CachedTokens,
			&event.CacheTokens,
			&event.CacheReadTokens,
			&event.CacheCreationTokens,
			&normalizedUncachedInput,
			&normalizedTotalInput,
			&normalizedCacheRead,
			&normalizedCacheCreation,
			&event.TotalTokens,
			&latency,
			&ttft,
			&failed,
			&failStatusCode,
			&failSummary,
			&responseMetadataJSON,
			&quotaRecoverAt,
			&quotaUsedPercent,
			&quotaPlanType,
			&errorKind,
			&errorCode,
			&traceID,
			&rawJSON,
			&event.CreatedAtMS,
		); err != nil {
			return nil, err
		}
		event.RequestID = requestID.String
		event.Provider = provider.String
		event.ExecutorType = executorType.String
		event.Endpoint = endpoint.String
		event.Method = method.String
		event.Path = path.String
		event.AuthType = authType.String
		event.AuthIndex = authIndex.String
		event.Source = source.String
		event.SourceHash = sourceHash.String
		event.APIKeyHash = apiKeyHash.String
		event.AccountSnapshot = accountSnapshot.String
		event.AuthLabelSnapshot = authLabelSnapshot.String
		event.AuthFileSnapshot = authFileSnapshot.String
		event.AuthProviderSnapshot = authProviderSnapshot.String
		event.AuthProjectIDSnapshot = authProjectIDSnapshot.String
		event.RequestedModel = requestedModel.String
		event.ResolvedModel = resolvedModel.String
		event.ReasoningEffort = reasoningEffort.String
		event.ServiceTier = serviceTier.String
		event.RequestServiceTier = requestServiceTier.String
		event.ResponseServiceTier = responseServiceTier.String
		hints := usage.RawCacheAccountingHintsFromJSON(rawJSON)
		accounting := usage.NormalizeCacheAccounting(usage.CacheInputContext{
			ExplicitMode:     hints.ExplicitMode,
			ExecutorType:     event.ExecutorType,
			Provider:         event.Provider,
			ProviderSnapshot: event.AuthProviderSnapshot,
			ResolvedModel:    event.ResolvedModel,
			RequestedModel:   event.RequestedModel,
			DisplayModel:     event.Model,
		}, event.InputTokens, event.CachedTokens, event.CacheTokens, event.CacheReadTokens, event.CacheCreationTokens)
		event.CacheInputMode = accounting.Mode
		event.NormalizedUncachedInputTokens = accounting.UncachedInputTokens
		event.NormalizedTotalInputTokens = accounting.TotalInputTokens
		event.NormalizedCacheReadTokens = accounting.CacheReadTokens
		event.NormalizedCacheCreationTokens = accounting.CacheCreationTokens
		if normalizedUncachedInput.Valid {
			event.NormalizedUncachedInputTokens = normalizedUncachedInput.Int64
		}
		if normalizedTotalInput.Valid {
			event.NormalizedTotalInputTokens = normalizedTotalInput.Int64
		}
		if normalizedCacheRead.Valid {
			event.NormalizedCacheReadTokens = normalizedCacheRead.Int64
		}
		if normalizedCacheCreation.Valid {
			event.NormalizedCacheCreationTokens = normalizedCacheCreation.Int64
		}
		if authSnapshotAt.Valid {
			event.AuthSnapshotAtMS = authSnapshotAt.Int64
		}
		if failStatusCode.Valid {
			event.FailStatusCode = int(failStatusCode.Int64)
		}
		event.FailSummary = failSummary.String
		event.ResponseMetadataJSON = responseMetadataJSON
		event.ResponseMetadata = usage.ResponseHeaderMetadataFromJSON(responseMetadataJSON)
		if quotaRecoverAt.Valid {
			event.HeaderQuotaRecoverAtMS = quotaRecoverAt.Int64
		}
		if quotaUsedPercent.Valid {
			value := quotaUsedPercent.Float64
			event.HeaderQuotaUsedPercent = &value
		}
		event.HeaderQuotaPlanType = quotaPlanType
		event.HeaderErrorKind = errorKind
		event.HeaderErrorCode = errorCode
		event.HeaderTraceID = traceID
		event.Failed = failed != 0
		if latency.Valid {
			value := latency.Int64
			event.LatencyMS = &value
		}
		if ttft.Valid {
			value := ttft.Int64
			event.TTFTMS = &value
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (r *repository) Count(ctx context.Context) (int64, error) {
	var count int64
	if err := r.db.QueryRowContext(ctx, `select count(*) from usage_events`).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullInt(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullPositiveInt64(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}

func responseHeaderDerivedForInsert(event model.UsageEvent) (string, int64, *float64, string, string, string, string) {
	metadataJSON := event.ResponseMetadataJSON
	quotaRecoverAtMS := event.HeaderQuotaRecoverAtMS
	quotaUsedPercent := event.HeaderQuotaUsedPercent
	quotaPlanType := event.HeaderQuotaPlanType
	errorKind := event.HeaderErrorKind
	errorCode := event.HeaderErrorCode
	traceID := event.HeaderTraceID

	derived := usage.DeriveResponseHeaderMetadata(event.ResponseMetadata)
	if metadataJSON == "" {
		metadataJSON = derived.MetadataJSON
	}
	if quotaRecoverAtMS == 0 {
		quotaRecoverAtMS = derived.QuotaRecoverAtMS
	}
	if quotaUsedPercent == nil {
		quotaUsedPercent = derived.QuotaUsedPercent
	}
	if quotaPlanType == "" {
		quotaPlanType = derived.QuotaPlanType
	}
	if errorKind == "" {
		errorKind = derived.ErrorKind
	}
	if errorCode == "" {
		errorCode = derived.ErrorCode
	}
	if traceID == "" {
		traceID = derived.TraceID
	}
	return metadataJSON, quotaRecoverAtMS, quotaUsedPercent, quotaPlanType, errorKind, errorCode, traceID
}
