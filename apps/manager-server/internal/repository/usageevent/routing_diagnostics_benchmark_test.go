package usageevent

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	sqliterepo "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/sqlite"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/usage"
)

func BenchmarkRoutingDiagnosticsWithFilter100000(b *testing.B) {
	db, err := sqliterepo.Open(filepath.Join(b.TempDir(), "usage.sqlite"))
	if err != nil {
		b.Fatalf("open database: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })
	repo := New(db)
	ctx := context.Background()
	const count = 100_000
	const batchSize = 500
	for offset := 0; offset < count; offset += batchSize {
		events := make([]usage.Event, 0, batchSize)
		for index := offset; index < min(offset+batchSize, count); index++ {
			outcome := "cache_hit"
			if index%100 == 0 {
				outcome = "failover"
			}
			shadowSampled := index%100 == 0
			pckHash := ""
			contextHash := ""
			if shadowSampled {
				pckHash = fmt.Sprintf("pck-%d", index%10_000)
				contextHash = fmt.Sprintf("context-%d", index%10_000)
			}
			events = append(events, routingDiagnosticEvent(
				fmt.Sprintf("routing-benchmark-%d", index),
				int64(index+1),
				"codex",
				outcome,
				"pck",
				int64(index%4+1),
				floatPointer(float64(index%90)),
				shadowSampled,
				pckHash,
				contextHash,
			))
		}
		if _, err := repo.InsertBatch(ctx, events); err != nil {
			b.Fatalf("insert diagnostics fixture: %v", err)
		}
	}
	filters := map[string]AnalyticsFilter{
		"default_overview": {FromMS: 0, ToMS: count + 1, IncludeFailed: true},
		"provider_filter":  {FromMS: 0, ToMS: count + 1, Providers: []string{"codex"}, IncludeFailed: true},
	}
	for name, filter := range filters {
		filter := filter
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if _, err := repo.RoutingDiagnosticsWithFilter(ctx, filter); err != nil {
					b.Fatalf("routing diagnostics: %v", err)
				}
			}
		})
	}
}
