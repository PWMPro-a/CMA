package usageevent

import (
	"context"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/usage"
)

// BackfillRoutingDiagnostics projects routing metadata that was already stored
// on usage_events before the dedicated diagnostics table was introduced. It is
// idempotent and deliberately bounded so startup can interleave it with normal
// ingestion without holding the SQLite writer for a large historical range.
func (r *repository) BackfillRoutingDiagnostics(ctx context.Context, batchLimit int) (int, error) {
	if batchLimit <= 0 {
		batchLimit = defaultResponseMetadataBackfillBatch
	}
	rows, err := r.db.QueryContext(ctx, `select
		e.event_hash,
		e.timestamp_ms,
		coalesce(e.response_metadata_json, ''),
		coalesce(e.raw_json, '')
	from usage_events e
	where not exists (
			select 1 from usage_routing_diagnostics d where d.event_hash = e.event_hash
		)
		and (
			(
				coalesce(e.response_metadata_json, '') <> ''
				and json_valid(e.response_metadata_json)
				and json_type(e.response_metadata_json, '$.routing') = 'object'
				and (
					coalesce(json_extract(e.response_metadata_json, '$.routing.affinity_outcome'), '') <> ''
					or coalesce(json_extract(e.response_metadata_json, '$.routing.session_source'), '') <> ''
					or coalesce(json_extract(e.response_metadata_json, '$.routing.binding_generation'), 0) <> 0
					or json_type(e.response_metadata_json, '$.routing.quota_used_percent') is not null
					or json_type(e.response_metadata_json, '$.routing.pck_shadow_sampled') is not null
					or coalesce(json_extract(e.response_metadata_json, '$.routing.pck_original_hash'), '') <> ''
					or coalesce(json_extract(e.response_metadata_json, '$.routing.pck_context_root_hash'), '') <> ''
					or coalesce(json_extract(e.response_metadata_json, '$.routing.pck_prefix_generation'), '') <> ''
				)
			)
			or coalesce(e.raw_json, '') like '%X-Cpa-Affinity-Outcome%'
			or coalesce(e.raw_json, '') like '%X-Cpa-Session-Source%'
			or coalesce(e.raw_json, '') like '%X-Cpa-Binding-Generation%'
			or coalesce(e.raw_json, '') like '%X-Cpa-Quota-Used-Percent%'
			or coalesce(e.raw_json, '') like '%X-Cpa-Pck-Shadow-Sampled%'
			or coalesce(e.raw_json, '') like '%X-Cpa-Pck-Original-Hash%'
			or coalesce(e.raw_json, '') like '%X-Cpa-Pck-Context-Root-Hash%'
			or coalesce(e.raw_json, '') like '%X-Cpa-Pck-Prefix-Generation%'
		)
	order by e.id
	limit ?`, batchLimit)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		eventHash   string
		timestampMS int64
		routing     *usage.HeaderRoutingMetadata
	}
	candidates := make([]candidate, 0, batchLimit)
	for rows.Next() {
		var eventHash, metadataJSON, rawJSON string
		var timestampMS int64
		if err := rows.Scan(&eventHash, &timestampMS, &metadataJSON, &rawJSON); err != nil {
			_ = rows.Close()
			return 0, err
		}
		metadata := mergeResponseMetadata(
			metadataJSON,
			usage.ParseResponseHeaderMetadataFromRawJSON(rawJSON, time.UnixMilli(timestampMS)),
		)
		if metadata == nil || usageRoutingDiagnostics(metadata) == nil {
			continue
		}
		candidates = append(candidates, candidate{
			eventHash:   eventHash,
			timestampMS: timestampMS,
			routing:     metadata.Routing,
		})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `insert or ignore into usage_routing_diagnostics (
		event_hash, timestamp_ms, affinity_outcome, session_source, binding_generation,
		quota_used_percent, pck_shadow_sampled, pck_original_hash, pck_context_root_hash, pck_prefix_generation
	) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	inserted := 0
	for _, item := range candidates {
		routing := item.routing
		result, err := stmt.ExecContext(
			ctx,
			item.eventHash,
			item.timestampMS,
			nullString(routing.AffinityOutcome),
			nullString(routing.SessionSource),
			routing.BindingGeneration,
			nullFloat(routing.QuotaUsedPercent),
			boolInt(routing.PCKShadowSampled != nil && *routing.PCKShadowSampled),
			nullString(routing.PCKOriginalHash),
			nullString(routing.PCKContextRootHash),
			nullString(routing.PCKPrefixGeneration),
		)
		if err != nil {
			return 0, err
		}
		affected, _ := result.RowsAffected()
		inserted += int(affected)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return inserted, nil
}
