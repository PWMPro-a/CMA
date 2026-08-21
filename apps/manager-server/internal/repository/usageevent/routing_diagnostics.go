package usageevent

import (
	"context"
	"fmt"
	"strings"
)

type RoutingDiagnosticCount struct {
	Key   string
	Count int64
}

type RoutingDiagnostics struct {
	TotalDiagnostics     int64
	CacheHits            int64
	ColdBinds            int64
	Failovers            int64
	ConcurrentReuses     int64
	FallbackAliasHits    int64
	MaxBindingGeneration int64
	QuotaSnapshotSamples int64
	AverageQuotaUsed     float64
	PCKShadowSamples     int64
	DistinctPCKs         int64
	PCKContextConflicts  int64
	Outcomes             []RoutingDiagnosticCount
	SessionSources       []RoutingDiagnosticCount
}

func (r *repository) RoutingDiagnosticsWithFilter(ctx context.Context, filter AnalyticsFilter) (RoutingDiagnostics, error) {
	scope := `d.timestamp_ms >= ? and d.timestamp_ms < ?`
	scopeArgs := []any{filter.FromMS, filter.ToMS}
	if routingDiagnosticsRequiresUsageEventFilter(filter) {
		where, args := analyticsWhere(filter)
		condition := strings.TrimSpace(strings.TrimPrefix(where, "where"))
		if condition == "" {
			condition = "1 = 1"
		}
		scope += fmt.Sprintf(` and exists (
			select 1 from usage_events
			where usage_events.event_hash = d.event_hash and %s
		)`, condition)
		scopeArgs = append(scopeArgs, args...)
	}

	// Materialize the filtered event set once. The previous implementation ran
	// four independent range scans and repeated the usage_events correlation for
	// every aggregate, which became expensive for the 30-day Overview window.
	query := `with scoped as materialized (
		select d.affinity_outcome, d.session_source, d.binding_generation,
			d.quota_used_percent, d.pck_shadow_sampled, d.pck_original_hash,
			d.pck_context_root_hash
		from usage_routing_diagnostics d
		where ` + scope + `
	), conflicts as (
		select count(*) as conflict_count from (
			select pck_original_hash
			from scoped
			where coalesce(pck_original_hash, '') <> ''
				and coalesce(pck_context_root_hash, '') <> ''
			group by pck_original_hash
			having count(distinct pck_context_root_hash) > 1
		) as conflict_rows
	)
	select 'summary' as row_kind, '' as row_key, 0 as row_count,
		count(*) as total_diagnostics,
		coalesce(sum(case when affinity_outcome = 'cache_hit' then 1 else 0 end), 0) as cache_hits,
		coalesce(sum(case when affinity_outcome = 'cold_bind' then 1 else 0 end), 0) as cold_binds,
		coalesce(sum(case when affinity_outcome = 'failover' then 1 else 0 end), 0) as failovers,
		coalesce(sum(case when affinity_outcome = 'concurrent_reuse' then 1 else 0 end), 0) as concurrent_reuses,
		coalesce(sum(case when affinity_outcome = 'fallback_alias_hit' then 1 else 0 end), 0) as fallback_alias_hits,
		coalesce(max(binding_generation), 0) as max_binding_generation,
		coalesce(sum(case when quota_used_percent is not null then 1 else 0 end), 0) as quota_snapshot_samples,
		coalesce(avg(quota_used_percent), 0) as average_quota_used,
		coalesce(sum(case when pck_shadow_sampled <> 0 then 1 else 0 end), 0) as pck_shadow_samples,
		count(distinct case when pck_original_hash <> '' then pck_original_hash end) as distinct_pcks,
		(select conflict_count from conflicts) as pck_context_conflicts
	from scoped
	union all
	select 'outcome', coalesce(affinity_outcome, ''), count(*), 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0
	from scoped group by affinity_outcome
	union all
	select 'session_source', coalesce(session_source, ''), count(*), 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0
	from scoped group by session_source
	order by row_kind, row_count desc, row_key asc`

	rows, err := r.db.QueryContext(ctx, query, scopeArgs...)
	if err != nil {
		return RoutingDiagnostics{}, err
	}
	defer rows.Close()
	var result RoutingDiagnostics
	for rows.Next() {
		var (
			rowKind, rowKey                                      string
			rowCount, total, cacheHits, coldBinds, failovers     int64
			concurrentReuses, fallbackAliasHits, maxGeneration   int64
			quotaSamples, pckSamples, distinctPCKs, pckConflicts int64
			averageQuota                                         float64
		)
		if err := rows.Scan(
			&rowKind, &rowKey, &rowCount, &total, &cacheHits, &coldBinds,
			&failovers, &concurrentReuses, &fallbackAliasHits, &maxGeneration,
			&quotaSamples, &averageQuota, &pckSamples, &distinctPCKs, &pckConflicts,
		); err != nil {
			return RoutingDiagnostics{}, err
		}
		if rowKind == "summary" {
			result.TotalDiagnostics = total
			result.CacheHits = cacheHits
			result.ColdBinds = coldBinds
			result.Failovers = failovers
			result.ConcurrentReuses = concurrentReuses
			result.FallbackAliasHits = fallbackAliasHits
			result.MaxBindingGeneration = maxGeneration
			result.QuotaSnapshotSamples = quotaSamples
			result.AverageQuotaUsed = averageQuota
			result.PCKShadowSamples = pckSamples
			result.DistinctPCKs = distinctPCKs
			result.PCKContextConflicts = pckConflicts
			continue
		}
		item := RoutingDiagnosticCount{Key: rowKey, Count: rowCount}
		if rowKind == "outcome" {
			result.Outcomes = append(result.Outcomes, item)
		} else if rowKind == "session_source" {
			result.SessionSources = append(result.SessionSources, item)
		}
	}
	if err := rows.Err(); err != nil {
		return RoutingDiagnostics{}, err
	}
	return result, nil
}

func routingDiagnosticsRequiresUsageEventFilter(filter AnalyticsFilter) bool {
	return strings.TrimSpace(filter.SearchQuery) != "" ||
		strings.TrimSpace(filter.SearchAPIKeyHash) != "" ||
		len(filter.Models) > 0 ||
		len(filter.Providers) > 0 ||
		len(filter.Accounts) > 0 ||
		len(filter.CredentialIDs) > 0 ||
		len(filter.AuthFiles) > 0 ||
		len(filter.AuthIndices) > 0 ||
		len(filter.APIKeyHashes) > 0 ||
		len(filter.SourceHashes) > 0 ||
		len(filter.ProjectIDs) > 0 ||
		len(filter.RequestTypes) > 0 ||
		!filter.IncludeFailed ||
		filter.FailedOnly ||
		filter.MinLatencyMS > 0 ||
		strings.TrimSpace(filter.CacheStatus) != "" ||
		len(filter.HeaderErrorKinds) > 0 ||
		len(filter.HeaderErrorCodes) > 0 ||
		len(filter.HeaderQuotaPlans) > 0 ||
		len(filter.HeaderTraceIDs) > 0
}
