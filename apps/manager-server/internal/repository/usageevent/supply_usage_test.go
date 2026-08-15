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

func TestListSupplyUsageMinutesRestoresSuccessfulLatencyAggregates(t *testing.T) {
	db, err := sqliterepo.Open(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	base := time.Date(2026, time.August, 14, 2, 0, 0, 0, time.UTC)
	latencyOne := int64(1_000)
	latencyThree := int64(3_000)
	failedLatency := int64(20_000)
	events := []usage.Event{
		supplyUsageEvent("one", base, 10, &latencyOne, false),
		supplyUsageEvent("two", base.Add(20*time.Second), 20, &latencyThree, false),
		supplyUsageEvent("failed", base.Add(40*time.Second), 30, &failedLatency, true),
	}
	repo := New(db)
	if _, err := repo.InsertBatch(context.Background(), events); err != nil {
		t.Fatalf("insert events: %v", err)
	}

	minutes, err := repo.ListSupplyUsageMinutes(context.Background(), base.Add(-time.Minute).UnixMilli())
	if err != nil {
		t.Fatalf("list supply usage minutes: %v", err)
	}
	if len(minutes) != 1 {
		t.Fatalf("usage minutes = %#v", minutes)
	}
	minute := minutes[0]
	if minute.Requests != 3 || minute.Successful != 2 || minute.Failed != 1 || minute.TotalTokens != 60 ||
		minute.LatencySumMS != 4_000 || minute.LatencySamples != 2 {
		t.Fatalf("usage minute = %#v", minute)
	}
}

func TestListSupplyQuotaCalibrationEventsReturnsMinimalQuotaHistory(t *testing.T) {
	db, err := sqliterepo.Open(filepath.Join(t.TempDir(), "quota-calibration.sqlite"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	base := time.Date(2026, time.August, 14, 3, 0, 0, 0, time.UTC)
	usedPercent := 12.5
	event := supplyUsageEvent("quota-event", base, 4_000_000, nil, false)
	event.AuthFileSnapshot = "team.json"
	event.HeaderQuotaUsedPercent = &usedPercent
	event.HeaderQuotaRecoverAtMS = base.Add(5 * time.Hour).UnixMilli()
	event.HeaderQuotaPlanType = "team"
	event.ResponseMetadataJSON = `{"quota":{"plan_type":"team","summary_window_kind":"weekly"}}`
	repo := New(db)
	if _, err := repo.InsertBatch(context.Background(), []usage.Event{event}); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	events, err := repo.ListSupplyQuotaCalibrationEvents(context.Background(), base.Add(-time.Minute).UnixMilli(), 10)
	if err != nil {
		t.Fatalf("list quota calibration events: %v", err)
	}
	if len(events) != 1 || events[0].AuthFileSnapshot != "team.json" || events[0].HeaderQuotaUsedPercent == nil ||
		*events[0].HeaderQuotaUsedPercent != usedPercent || events[0].HeaderQuotaPlanType != "team" || events[0].TotalTokens != 4_000_000 ||
		events[0].ResponseMetadataJSON != event.ResponseMetadataJSON {
		t.Fatalf("quota calibration events = %#v", events)
	}
}

func TestListSupplyQuotaCalibrationEventsIncludesHeaderlessUsage(t *testing.T) {
	db, err := sqliterepo.Open(filepath.Join(t.TempDir(), "quota-calibration-headerless.sqlite"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	base := time.Date(2026, time.August, 14, 3, 0, 0, 0, time.UTC)
	event := supplyUsageEvent("quota-history", base, 3_000_000, nil, false)
	event.AuthFileSnapshot = "team.json"
	repo := New(db)
	if _, err := repo.InsertBatch(context.Background(), []usage.Event{event}); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	events, err := repo.ListSupplyQuotaCalibrationEvents(context.Background(), base.Add(-time.Minute).UnixMilli(), 10)
	if err != nil {
		t.Fatalf("list quota calibration events: %v", err)
	}
	if len(events) != 1 || events[0].HeaderQuotaUsedPercent != nil || events[0].TotalTokens != 3_000_000 {
		t.Fatalf("headerless quota history = %#v", events)
	}
}

func TestListSupplyQuotaCalibrationEventsLimitsToNewestHistoryInChronologicalOrder(t *testing.T) {
	db, err := sqliterepo.Open(filepath.Join(t.TempDir(), "quota-calibration-limit.sqlite"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	base := time.Date(2026, time.August, 14, 3, 0, 0, 0, time.UTC)
	events := make([]usage.Event, 0, 3)
	for index := 0; index < 3; index++ {
		usedPercent := float64(index + 1)
		event := supplyUsageEvent(fmt.Sprintf("quota-event-%d", index), base.Add(time.Duration(index)*time.Minute), int64(index+1)*1_000_000, nil, false)
		event.AuthFileSnapshot = "team.json"
		event.HeaderQuotaUsedPercent = &usedPercent
		events = append(events, event)
	}
	repo := New(db)
	if _, err := repo.InsertBatch(context.Background(), events); err != nil {
		t.Fatalf("insert events: %v", err)
	}

	calibrationEvents, err := repo.ListSupplyQuotaCalibrationEvents(context.Background(), base.Add(-time.Minute).UnixMilli(), 2)
	if err != nil {
		t.Fatalf("list quota calibration events: %v", err)
	}
	if len(calibrationEvents) != 2 || calibrationEvents[0].TotalTokens != 2_000_000 || calibrationEvents[1].TotalTokens != 3_000_000 {
		t.Fatalf("quota calibration events = %#v", calibrationEvents)
	}
}

func TestListSupplyQuotaWindowUsageAggregatesCompleteCredentialWindow(t *testing.T) {
	db, err := sqliterepo.Open(filepath.Join(t.TempDir(), "quota-window-usage.sqlite"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	base := time.Date(2026, time.August, 7, 3, 0, 0, 0, time.UTC)
	events := []usage.Event{
		supplyUsageEvent("before-window", base.Add(-time.Second), 20_000_000, nil, false),
		supplyUsageEvent("account-a-total", base.Add(time.Minute), 30_000_000, nil, false),
		supplyUsageEvent("account-a-components", base.Add(2*time.Minute), 0, nil, false),
		supplyUsageEvent("account-a-failed", base.Add(3*time.Minute), 100_000_000, nil, true),
		supplyUsageEvent("account-a-other-auth", base.Add(4*time.Minute), 200_000_000, nil, false),
		supplyUsageEvent("account-b", base.Add(5*time.Minute), 7_000_000, nil, false),
	}
	for index := range events {
		events[index].AuthFileSnapshot = "account-a.json"
		events[index].AuthIndex = "auth-a"
	}
	events[2].InputTokens = 10_000_000
	events[2].OutputTokens = 5_000_000
	events[2].ReasoningTokens = 5_000_000
	events[4].AuthIndex = "auth-b"
	events[5].AuthFileSnapshot = "account-b.json"
	events[5].AuthIndex = ""

	repo := New(db)
	if _, err := repo.InsertBatch(context.Background(), events); err != nil {
		t.Fatalf("insert events: %v", err)
	}
	rows, err := repo.ListSupplyQuotaWindowUsage(context.Background(), []SupplyQuotaWindowUsageQuery{
		{RequestIndex: 10, AuthFileSnapshot: "ACCOUNT-A.JSON", AuthIndex: "AUTH-A", FromMS: base.UnixMilli(), ToMS: base.Add(time.Hour).UnixMilli()},
		{RequestIndex: 20, AuthFileSnapshot: "account-b.json", FromMS: base.UnixMilli(), ToMS: base.Add(time.Hour).UnixMilli()},
	})
	if err != nil {
		t.Fatalf("list quota window usage: %v", err)
	}
	if len(rows) != 2 || rows[0].RequestIndex != 10 || rows[0].TotalTokens != 50_000_000 ||
		rows[0].FirstSeenMS != base.Add(time.Minute).UnixMilli() || rows[0].LastSeenMS != base.Add(3*time.Minute).UnixMilli() ||
		rows[1].RequestIndex != 20 || rows[1].TotalTokens != 7_000_000 ||
		rows[1].FirstSeenMS != base.Add(5*time.Minute).UnixMilli() || rows[1].LastSeenMS != base.Add(5*time.Minute).UnixMilli() {
		t.Fatalf("quota window rows = %#v", rows)
	}
}

func supplyUsageEvent(hash string, timestamp time.Time, tokens int64, latency *int64, failed bool) usage.Event {
	return usage.Event{
		EventHash:   hash,
		TimestampMS: timestamp.UnixMilli(),
		Timestamp:   timestamp.Format(time.RFC3339Nano),
		Provider:    "codex",
		AuthIndex:   hash,
		TotalTokens: tokens,
		LatencyMS:   latency,
		Failed:      failed,
		CreatedAtMS: timestamp.UnixMilli(),
	}
}
