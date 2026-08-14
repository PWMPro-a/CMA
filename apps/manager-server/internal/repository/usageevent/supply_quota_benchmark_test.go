package usageevent

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	sqliterepo "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/sqlite"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/usage"
)

func BenchmarkListSupplyQuotaWindowUsage1000Accounts(b *testing.B) {
	db, err := sqliterepo.Open(filepath.Join(b.TempDir(), "supply-quota-window.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	base := time.Date(2026, time.August, 7, 0, 0, 0, 0, time.UTC)
	const accountCount = 1000
	const eventsPerAccount = 60
	events := make([]usage.Event, 0, accountCount*eventsPerAccount)
	targets := make([]SupplyQuotaWindowUsageQuery, 0, accountCount)
	for account := 0; account < accountCount; account++ {
		fileName := fmt.Sprintf("account-%04d.json", account)
		authIndex := fmt.Sprintf("auth-%04d", account)
		targets = append(targets, SupplyQuotaWindowUsageQuery{
			RequestIndex:     account,
			AuthFileSnapshot: fileName,
			AuthIndex:        authIndex,
			FromMS:           base.UnixMilli(),
			ToMS:             base.Add(7 * 24 * time.Hour).UnixMilli(),
		})
		for eventIndex := 0; eventIndex < eventsPerAccount; eventIndex++ {
			timestamp := base.Add(time.Duration(eventIndex) * time.Minute)
			event := supplyUsageEvent(
				fmt.Sprintf("quota-window-%04d-%02d", account, eventIndex),
				timestamp,
				1_000_000,
				nil,
				false,
			)
			event.AuthFileSnapshot = fileName
			event.AuthIndex = authIndex
			events = append(events, event)
		}
	}
	repo := New(db)
	if _, err := repo.InsertBatch(ctx, events); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rows, err := repo.ListSupplyQuotaWindowUsage(ctx, targets)
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != accountCount {
			b.Fatalf("rows = %d, want %d", len(rows), accountCount)
		}
	}
}
