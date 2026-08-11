package usageevent

import (
	"context"
	"path/filepath"
	"testing"

	sqliterepo "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/sqlite"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/usage"
)

func TestRoutingDiagnosticsWithFilterAggregatesReuseAndPCKConflicts(t *testing.T) {
	db, err := sqliterepo.Open(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := New(db)

	events := []usage.Event{
		routingDiagnosticEvent("cache-hit", 100, "codex", "cache_hit", "pck", 1, floatPointer(80), true, "pck-a", "context-a"),
		routingDiagnosticEvent("concurrent", 200, "codex", "concurrent_reuse", "pck", 2, floatPointer(90), true, "pck-a", "context-b"),
		routingDiagnosticEvent("fallback", 300, "codex", "fallback_alias_hit", "conversation", 2, nil, true, "pck-b", "context-c"),
		routingDiagnosticEvent("cold", 400, "codex", "cold_bind", "header", 1, nil, false, "", ""),
		routingDiagnosticEvent("failover-other-provider", 500, "openai", "failover", "header", 3, nil, false, "", ""),
	}
	if _, err := repo.InsertBatch(context.Background(), events); err != nil {
		t.Fatalf("insert events: %v", err)
	}

	result, err := repo.RoutingDiagnosticsWithFilter(context.Background(), AnalyticsFilter{
		FromMS:        0,
		ToMS:          1000,
		Providers:     []string{"codex"},
		IncludeFailed: true,
	})
	if err != nil {
		t.Fatalf("routing diagnostics: %v", err)
	}
	if result.TotalDiagnostics != 4 || result.CacheHits != 1 || result.ColdBinds != 1 || result.ConcurrentReuses != 1 || result.FallbackAliasHits != 1 || result.Failovers != 0 {
		t.Fatalf("outcome totals = %#v", result)
	}
	if result.MaxBindingGeneration != 2 || result.QuotaSnapshotSamples != 2 || result.AverageQuotaUsed != 85 {
		t.Fatalf("binding/quota totals = %#v", result)
	}
	if result.PCKShadowSamples != 3 || result.DistinctPCKs != 2 || result.PCKContextConflicts != 1 {
		t.Fatalf("PCK totals = %#v", result)
	}
	if len(result.Outcomes) != 4 || len(result.SessionSources) != 3 {
		t.Fatalf("diagnostic dimensions = outcomes:%#v sources:%#v", result.Outcomes, result.SessionSources)
	}
}

func TestBackfillRoutingDiagnosticsRestoresLegacyProjectionAndDeleteCascades(t *testing.T) {
	db, err := sqliterepo.Open(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := New(db)
	event := routingDiagnosticEvent("legacy-routing", 100, "codex", "cache_hit", "pck", 2, floatPointer(42), true, "pck-a", "context-a")
	if _, err := repo.InsertBatch(context.Background(), []usage.Event{event}); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if _, err := db.Exec(`delete from usage_routing_diagnostics where event_hash = ?`, event.EventHash); err != nil {
		t.Fatalf("remove diagnostic projection: %v", err)
	}

	updated, err := repo.BackfillRoutingDiagnostics(context.Background(), 10)
	if err != nil || updated != 1 {
		t.Fatalf("BackfillRoutingDiagnostics() = %d, %v, want 1", updated, err)
	}
	updated, err = repo.BackfillRoutingDiagnostics(context.Background(), 10)
	if err != nil || updated != 0 {
		t.Fatalf("second BackfillRoutingDiagnostics() = %d, %v, want 0", updated, err)
	}

	if _, err := db.Exec(`delete from usage_events where event_hash = ?`, event.EventHash); err != nil {
		t.Fatalf("delete usage event: %v", err)
	}
	var diagnosticCount int
	if err := db.QueryRow(`select count(*) from usage_routing_diagnostics where event_hash = ?`, event.EventHash).Scan(&diagnosticCount); err != nil {
		t.Fatalf("count diagnostic projection: %v", err)
	}
	if diagnosticCount != 0 {
		t.Fatalf("diagnostic projection survived source deletion: count=%d", diagnosticCount)
	}
}

func TestRoutingDiagnosticsBackfillParsesLegacyRawCPAHeaders(t *testing.T) {
	db, err := sqliterepo.Open(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := New(db)
	rawJSON := `{"response_headers":{"X-Cpa-Affinity-Outcome":["concurrent_reuse"],"X-Cpa-Session-Source":["pck"],"X-Cpa-Binding-Generation":["3"],"X-Cpa-Quota-Used-Percent":["72.5"],"X-Cpa-Pck-Shadow-Sampled":["true"],"X-Cpa-Pck-Original-Hash":["pck-raw"],"X-Cpa-Pck-Context-Root-Hash":["context-raw"]}}`
	if _, err := db.Exec(`insert into usage_events (
		event_hash, timestamp_ms, timestamp, model, failed, response_metadata_json, raw_json, created_at_ms
	) values (?, ?, ?, ?, ?, ?, ?, ?)`,
		"legacy-raw-routing", int64(200), "2026-01-01T00:00:00Z", "gpt-test", 0, "{}", rawJSON, int64(200),
	); err != nil {
		t.Fatalf("insert legacy raw event: %v", err)
	}

	metadataUpdated, err := repo.BackfillResponseMetadata(context.Background(), 10)
	if err != nil || metadataUpdated != 1 {
		t.Fatalf("BackfillResponseMetadata() = %d, %v, want 1", metadataUpdated, err)
	}
	var metadataJSON string
	if err := db.QueryRow(`select response_metadata_json from usage_events where event_hash = ?`, "legacy-raw-routing").Scan(&metadataJSON); err != nil {
		t.Fatalf("read backfilled metadata: %v", err)
	}
	metadata := usage.ResponseHeaderMetadataFromJSON(metadataJSON)
	if metadata == nil || metadata.Routing == nil || metadata.Routing.AffinityOutcome != "concurrent_reuse" {
		t.Fatalf("backfilled routing metadata = %#v", metadata)
	}

	diagnosticsUpdated, err := repo.BackfillRoutingDiagnostics(context.Background(), 10)
	if err != nil || diagnosticsUpdated != 1 {
		t.Fatalf("BackfillRoutingDiagnostics() = %d, %v, want 1", diagnosticsUpdated, err)
	}
	result, err := repo.RoutingDiagnosticsWithFilter(context.Background(), AnalyticsFilter{
		FromMS: 0, ToMS: 1000, IncludeFailed: true,
	})
	if err != nil {
		t.Fatalf("routing diagnostics: %v", err)
	}
	if result.TotalDiagnostics != 1 || result.ConcurrentReuses != 1 || result.MaxBindingGeneration != 3 || result.QuotaSnapshotSamples != 1 {
		t.Fatalf("raw routing diagnostics = %#v", result)
	}
}

func routingDiagnosticEvent(hash string, timestampMS int64, provider, outcome, source string, generation int64, quota *float64, shadow bool, pckHash, contextHash string) usage.Event {
	return usage.Event{
		EventHash:   hash,
		TimestampMS: timestampMS,
		Timestamp:   "2026-01-01T00:00:00Z",
		Provider:    provider,
		Model:       "gpt-test",
		CreatedAtMS: timestampMS,
		ResponseMetadata: &usage.ResponseHeaderMetadata{
			Routing: &usage.HeaderRoutingMetadata{
				AffinityOutcome:    outcome,
				SessionSource:      source,
				BindingGeneration:  generation,
				QuotaUsedPercent:   quota,
				PCKShadowSampled:   boolPointer(shadow),
				PCKOriginalHash:    pckHash,
				PCKContextRootHash: contextHash,
			},
		},
	}
}

func floatPointer(value float64) *float64 { return &value }

func boolPointer(value bool) *bool { return &value }
