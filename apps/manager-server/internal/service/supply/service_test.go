package supply

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/config"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpaauthfiles"
	managerconfigsvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/supplyclient"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/usage"
)

func TestAutomationExecutionTracksScheduledAndCompletedCycles(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Millisecond)
	service.ScheduleAutomaticExecution(now.Add(2 * time.Second))
	scheduled := service.currentAutomationExecution(true)
	if scheduled.LastResult != "scheduled" || scheduled.NextExecutionAtMS != now.Add(2*time.Second).UnixMilli() {
		t.Fatalf("scheduled automation = %#v", scheduled)
	}

	service.setSmartResource(SmartResource{
		SuggestedAction: smartActionTakeLocked,
		DecisionReason:  "critical_take_confirmed",
	})
	service.beginAutomaticRunDecision()
	service.recordAutomaticRunDecision(SmartResource{
		SuggestedAction: smartActionTakeLocked,
		DecisionReason:  "critical_take_confirmed",
	})
	// A dashboard refresh may publish a newer capacity recommendation before the
	// worker records its timeline. The execution card must keep this run's exact
	// decision rather than borrowing the refresh result.
	service.setSmartResource(SmartResource{
		SuggestedAction: smartActionEmergencyReplenish,
		DecisionReason:  "available_capacity_critical",
	})
	finishedAt := now.Add(300 * time.Millisecond)
	nextAt := finishedAt.Add(15 * time.Second)
	service.RecordAutomaticExecution(now, finishedAt, nextAt, nil)
	completed := service.currentAutomationExecution(true)
	if completed.LastResult != "completed" || completed.LastAction != smartActionTakeLocked ||
		completed.LastReason != "critical_take_confirmed" || completed.LastError != "" ||
		completed.IntervalSeconds != 15 || completed.NextExecutionAtMS != nextAt.UnixMilli() {
		t.Fatalf("completed automation = %#v", completed)
	}

	service.RecordAutomaticExecution(now, finishedAt, nextAt, errors.New("supplier unavailable"))
	failed := service.currentAutomationExecution(true)
	if failed.LastResult != "failed" || failed.LastError == "" {
		t.Fatalf("failed automation = %#v", failed)
	}

	transientErr := fmt.Errorf("nv: %w", &supplyclient.HTTPError{StatusCode: http.StatusBadGateway, Message: "origin overloaded"})
	service.RecordAutomaticExecution(now, finishedAt, nextAt, transientErr)
	retrying := service.currentAutomationExecution(true)
	if retrying.LastResult != "retrying" || retrying.LastError == "" {
		t.Fatalf("transient supplier failure should wait for an automatic retry: %#v", retrying)
	}
	if interval := automaticIntervalWithRunError(time.Second, transientErr); interval < automaticTransientRetryBackoff {
		t.Fatalf("transient supplier retry interval = %s, want at least %s", interval, automaticTransientRetryBackoff)
	}

	service.RecordAutomaticExecution(now, finishedAt, nextAt, errors.New("database is locked (5) (SQLITE_BUSY)"))
	deferred := service.currentAutomationExecution(true)
	if deferred.LastResult != "scheduled" || deferred.LastError != "" {
		t.Fatalf("busy automation should be deferred without an operator error: %#v", deferred)
	}

	service.beginAutomaticRunDecision()
	service.recordAutomaticRunDecision(SmartResource{
		SuggestedAction: smartActionPriceWait,
		DecisionReason:  "supplier_price_above_ceiling",
	})
	service.RecordAutomaticExecution(now, finishedAt, nextAt, nil)
	priceWait := service.currentAutomationExecution(true)
	if priceWait.LastResult != "price_wait" || priceWait.LastAction != smartActionPriceWait || priceWait.LastError != "" {
		t.Fatalf("price wait should be a normal no-order result: %#v", priceWait)
	}

	service.beginAutomaticRunDecision()
	service.recordAutomaticRunDecision(SmartResource{
		SuggestedAction: smartActionSupplierGateWait,
		DecisionReason:  "supplier_quota_gate_wait",
	})
	service.RecordAutomaticExecution(now, finishedAt, nextAt, nil)
	quotaWait := service.currentAutomationExecution(true)
	if quotaWait.LastResult != "quota_wait" || quotaWait.LastAction != smartActionSupplierGateWait || quotaWait.LastError != "" {
		t.Fatalf("supplier evidence wait should be a normal no-order result: %#v", quotaWait)
	}

	disabled := service.currentAutomationExecution(false)
	if disabled.Enabled || disabled.NextExecutionAtMS != 0 || disabled.IntervalSeconds != 0 {
		t.Fatalf("disabled automation must not expose a future execution: %#v", disabled)
	}
}

func TestRecordErrorDoesNotExposeTransientSQLiteContention(t *testing.T) {
	service := New(nil, nil)
	service.setOverview(Overview{LastError: "supplier credentials expired"})
	service.recordError(errors.New("database is locked (5) (SQLITE_BUSY)"))
	service.stateMu.RLock()
	defer service.stateMu.RUnlock()
	if service.overview.LastError != "supplier credentials expired" {
		t.Fatalf("transient SQLite error replaced actionable operator error: %#v", service.overview)
	}
}

func TestRecordErrorTreatsAutomaticWaitDecisionsAsHealthy(t *testing.T) {
	service := New(nil, nil)
	waits := []error{
		&MarketplacePriceWaitError{MinimumUnitPriceFen: 2200, CeilingFen: 2000, Available: 4},
		ErrSupplierQuotaGateNoEligibleSeller,
	}
	for _, waitErr := range waits {
		service.setOverview(Overview{LastError: "previous supplier failure"})
		service.recordError(waitErr)
		service.stateMu.RLock()
		lastError := service.overview.LastError
		service.stateMu.RUnlock()
		if lastError != "" {
			t.Fatalf("normal automatic wait %v remained an overview error: %q", waitErr, lastError)
		}
	}
}

func TestScheduleRecoverySyncIfDueIsNonBlockingAndSingleFlight(t *testing.T) {
	service := New(nil, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	completed := make(chan struct{})
	var calls atomic.Int32
	service.recoverySyncIfDue = func(ctx context.Context) (RecoverySummary, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return RecoverySummary{}, ctx.Err()
			}
			close(completed)
		}
		return RecoverySummary{}, nil
	}

	startedAt := time.Now()
	if !service.ScheduleRecoverySyncIfDue(context.Background()) {
		t.Fatal("first recovery sync was not scheduled")
	}
	if elapsed := time.Since(startedAt); elapsed > 250*time.Millisecond {
		t.Fatalf("recovery scheduling blocked for %s", elapsed)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background recovery sync did not start")
	}
	if service.ScheduleRecoverySyncIfDue(context.Background()) {
		t.Fatal("duplicate recovery sync was scheduled while the first was running")
	}
	close(release)
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("background recovery sync did not complete")
	}
	deadline := time.Now().Add(time.Second)
	for {
		service.recoveryAsyncMu.Lock()
		running := service.recoveryAsyncRunning
		service.recoveryAsyncMu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background recovery single-flight guard did not clear")
		}
		time.Sleep(time.Millisecond)
	}
	if !service.ScheduleRecoverySyncIfDue(context.Background()) {
		t.Fatal("recovery sync could not be scheduled after completion")
	}
}

func TestGetActiveOrderStatusUsesFastRemotePollAndHonorsRetryAfter(t *testing.T) {
	var statusCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case "/api/customer/pickup/orders/order-fast":
			statusCalls.Add(1)
			_, _ = w.Write([]byte(`{"id":"order-fast","status":"waiting_inventory","retry_after_seconds":30}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "active-order.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		Supply: store.ManagerSupplyConfig{
			BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_7d",
			PollIntervalSeconds: 60,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "order-fast", Product: "oauth_7d", RequestedQuantity: 2,
		Automatic: true, Status: "waiting_inventory", NextPollAtMS: 0,
	}); err != nil {
		t.Fatalf("create order: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	first, err := service.GetActiveOrderStatus(context.Background())
	if err != nil || !first.PollAttempted || first.ActiveOrder == nil {
		t.Fatalf("first active status = %#v err=%v", first, err)
	}
	if first.ActiveOrder.RemoteStatus != "waiting_inventory" || first.ActiveOrder.SupplierRetryUntilMS <= time.Now().UnixMilli() {
		t.Fatalf("retry-after was not persisted: %#v", first.ActiveOrder)
	}
	second, err := service.GetActiveOrderStatus(context.Background())
	if err != nil || second.PollAttempted {
		t.Fatalf("second active status should wait for supplier deadline = %#v err=%v", second, err)
	}
	if statusCalls.Load() != 1 {
		t.Fatalf("remote status calls = %d, want 1", statusCalls.Load())
	}
}

func createRecoverySourceOrder(t *testing.T, st *store.Store, orderID string) {
	t.Helper()
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID:           orderID,
		Product:           "oauth_30d",
		RequestedQuantity: 1,
		Automatic:         true,
		Status:            "completed",
		CompletedAtMS:     time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("create recovery source order %s: %v", orderID, err)
	}
}

func TestSupplyAuth401CandidateOnlyAcceptsManagedSupplyAccounts(t *testing.T) {
	nowMS := time.Now().UnixMilli()
	event := usage.Event{
		Failed:               true,
		FailStatusCode:       http.StatusUnauthorized,
		AuthFileSnapshot:     "codex-supply-account.json",
		AuthIndex:            "7",
		AuthProviderSnapshot: "codex",
		AccountSnapshot:      "account@example.com",
		TimestampMS:          nowMS - 1000,
		FailSummary:          "upstream returned 401",
	}
	candidate, ok := supplyAuth401CandidateFromEvent(event, nowMS)
	if !ok || candidate.FileName != event.AuthFileSnapshot || candidate.AuthIndex != "7" ||
		candidate.SeenAtMS != event.TimestampMS || candidate.Provider != "codex" ||
		candidate.FailureSummary != event.FailSummary || !strings.Contains(candidate.EvidenceJSON, `"statusCode":401`) {
		t.Fatalf("401 candidate = %#v, ok=%v", candidate, ok)
	}
	event.AuthFileSnapshot = "manual-account.json"
	if _, ok := supplyAuth401CandidateFromEvent(event, nowMS); ok {
		t.Fatal("non-supply account must not enter the supplier recovery workflow")
	}
	event.AuthFileSnapshot = "supply-account.json"
	event.FailStatusCode = http.StatusTooManyRequests
	if _, ok := supplyAuth401CandidateFromEvent(event, nowMS); ok {
		t.Fatal("non-401 failure must not enter the supplier recovery workflow")
	}
}

func TestEmergencyOrderProcessingHonorsSupplierRetryDeadline(t *testing.T) {
	service := New(nil, nil)
	resource := SmartResource{
		EffectiveHealthyMinutes: 40,
		CriticalMinutes:         5,
		EstimatedSustainMinutes: 10,
		ConsumeRCUPerMinute:     100,
		CapacityGapRCU:          3_000,
	}
	order := store.SupplyOrder{Automatic: true, Status: "waiting_inventory"}
	if !service.emergencyOrderProcessingAllowed(store.ManagerSupplyConfig{}, order, resource) {
		t.Fatal("emergency order without a supplier retry deadline should bypass local poll pacing")
	}
	order.SupplierRetryUntilMS = time.Now().Add(10 * time.Second).UnixMilli()
	if service.emergencyOrderProcessingAllowed(store.ManagerSupplyConfig{}, order, resource) {
		t.Fatal("supplier retry_after deadline must not be bypassed by emergency processing")
	}
}

func TestAutomaticCreateCooldownUsesPersistedLatestOrder(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply-cooldown.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now()
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID:           "automatic-recent",
		Product:           "oauth_7d",
		RequestedQuantity: 6,
		Automatic:         true,
		Status:            "completed",
		CreatedAtMS:       now.Add(-5 * time.Second).UnixMilli(),
		CompletedAtMS:     now.Add(-4 * time.Second).UnixMilli(),
	}); err != nil {
		t.Fatalf("create automatic order: %v", err)
	}
	service := New(st, nil)
	active, err := service.automaticCreateCooldownActive(context.Background(), store.ManagerSupplyConfig{
		CreateCooldownSeconds: 120,
	}, SmartResource{EmergencyShortage: true})
	if err != nil {
		t.Fatalf("check persisted cooldown: %v", err)
	}
	if !active {
		t.Fatal("a recent persisted automatic order must keep emergency replenishment in cooldown")
	}
}

func TestAutomaticCreateCooldownIgnoresRecentRecoveryImport(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply-recovery-cooldown.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now()
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID:           "automatic-old-purchase",
		Product:           "oauth_7d",
		RequestedQuantity: 3,
		Automatic:         true,
		Status:            "completed",
		CreatedAtMS:       now.Add(-10 * time.Minute).UnixMilli(),
		CompletedAtMS:     now.Add(-9 * time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatalf("create old automatic purchase: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID:           "recovery-recent-import",
		Product:           "oauth_7d",
		RequestedQuantity: 1,
		Automatic:         true,
		Strategy:          "recovery",
		RemoteStatus:      "recovery_claimed",
		Status:            "completed",
		CreatedAtMS:       now.Add(-5 * time.Second).UnixMilli(),
		CompletedAtMS:     now.Add(-4 * time.Second).UnixMilli(),
	}); err != nil {
		t.Fatalf("create recent recovery import: %v", err)
	}
	service := New(st, nil)
	active, err := service.automaticCreateCooldownActive(context.Background(), store.ManagerSupplyConfig{
		CreateCooldownSeconds: 120,
	}, SmartResource{EmergencyShortage: true})
	if err != nil {
		t.Fatalf("check persisted cooldown: %v", err)
	}
	if active {
		t.Fatal("recent recovery import must not start the supplier purchase cooldown")
	}
}

func TestRecentAutomaticSettlementOnlyBlocksWhenDeliveryCoversCurrentShortage(t *testing.T) {
	cfg := store.ManagerSupplyConfig{Product: "oauth_7d", NewAccountConfidence: 1}
	unit := smartEstimatedNewAccountCapacityRCU(cfg)
	resource := SmartResource{
		CapacityGapRCU: 5 * unit, AccountQuantityDeficit: 5,
		UnitCapacityRCU: smartProductUnitCapacity(cfg.Product),
	}
	recent := store.SupplyOrder{
		OrderID: "recent", Product: cfg.Product, Automatic: true,
		Status: "completed", RequestedQuantity: 3, ImportedCount: 3,
	}
	if recentAutomaticOrderCoversCurrentShortage(cfg, resource, recent) {
		t.Fatal("a recent three-account delivery must not hide a five-account current shortage")
	}
	recent.RequestedQuantity = 5
	recent.ImportedCount = 5
	if !recentAutomaticOrderCoversCurrentShortage(cfg, resource, recent) {
		t.Fatal("a recent delivery that covers the full current shortage should retain the settlement window")
	}
}

func TestSuccessfulPartialDeliveryKeepsProgressiveCooldownUnlessEmergency(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply-progressive-cooldown.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now()
	cfg := store.ManagerSupplyConfig{
		Product:               "oauth_7d",
		Strategy:              managerconfigsvc.SupplyStrategyStrongSupply,
		CreateCooldownSeconds: 120,
		NewAccountConfidence:  1,
	}
	unit := smartEstimatedNewAccountCapacityRCU(cfg)
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID:           "progressive-partial",
		Product:           cfg.Product,
		RequestedQuantity: 2,
		ImportedCount:     2,
		Automatic:         true,
		Status:            "completed",
		CreatedAtMS:       now.Add(-20 * time.Minute).UnixMilli(),
		CompletedAtMS:     now.Add(-5 * time.Second).UnixMilli(),
	}); err != nil {
		t.Fatalf("create automatic order: %v", err)
	}
	resource := SmartResource{
		HealthLevel:             smartHealthWarning,
		WarningMinutes:          20,
		CriticalMinutes:         15,
		EffectiveHealthyMinutes: 30,
		EstimatedSustainMinutes: 25,
		ConsumeRCUPerMinute:     100,
		CapacityGapRCU:          5 * unit,
		AccountQuantityDeficit:  5,
		UnitCapacityRCU:         smartProductUnitCapacity(cfg.Product),
	}
	service := New(st, nil)
	active, err := service.automaticCreateCooldownActive(context.Background(), cfg, resource)
	if err != nil {
		t.Fatalf("check progressive cooldown: %v", err)
	}
	if !active {
		t.Fatal("a successful small batch must start buffered strategy observation even when a larger shortage remains")
	}

	resource.EmergencyShortage = true
	active, err = service.automaticCreateCooldownActive(context.Background(), cfg, resource)
	if err != nil {
		t.Fatalf("check capacity-emergency cooldown: %v", err)
	}
	if !active {
		t.Fatal("a capacity-only emergency must briefly observe a delivered account before buying the same expiry batch again")
	}

	resource.EmergencyReason = "critical_available_accounts"
	resource.AvailableAccounts = 1
	resource.CriticalAvailableAccounts = 2
	active, err = service.automaticCreateCooldownActive(context.Background(), cfg, resource)
	if err != nil {
		t.Fatalf("check account-vacuum successful-delivery cooldown: %v", err)
	}
	if !active {
		t.Fatal("an account vacuum must observe a successful delivery before buying the same expiry batch again")
	}
}

func TestRecentSupplyOrdersCachesDefensiveCopiesAndInvalidates(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply-order-cache.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "cached-first", Product: "oauth_7d", RequestedQuantity: 1, Status: "completed",
	}); err != nil {
		t.Fatalf("create first order: %v", err)
	}
	service := New(st, nil)
	first, err := service.recentSupplyOrders(ctx)
	if err != nil || len(first) != 1 {
		t.Fatalf("first cached orders = %#v, err=%v", first, err)
	}
	first[0].Status = "mutated-by-caller"
	second, err := service.recentSupplyOrders(ctx)
	if err != nil || len(second) != 1 || second[0].Status == "mutated-by-caller" {
		t.Fatalf("cached orders must be defensively copied: %#v, err=%v", second, err)
	}
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "cached-second", Product: "oauth_7d", RequestedQuantity: 1, Status: "completed",
	}); err != nil {
		t.Fatalf("create second order: %v", err)
	}
	stillCached, err := service.recentSupplyOrders(ctx)
	if err != nil || len(stillCached) != 1 {
		t.Fatalf("short-lived cache was not reused: %#v, err=%v", stillCached, err)
	}
	service.invalidateSupplyOrdersCache()
	refreshed, err := service.recentSupplyOrders(ctx)
	if err != nil || len(refreshed) != 2 {
		t.Fatalf("invalidated cache did not reload orders: %#v, err=%v", refreshed, err)
	}
}

func TestEmergencyRetryNextIntervalHonorsShortCooldown(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name    string
		order   store.SupplyOrder
		wantMin time.Duration
		wantMax time.Duration
	}{
		{
			name: "cancelled order wakes the next retry immediately",
			order: store.SupplyOrder{
				OrderID: "cancelled-recent", Product: "oauth_30d", RequestedQuantity: 10, Automatic: true,
				Status: "cancelled", RemoteStatus: "cancelled", CompletedAtMS: now.Add(-4 * time.Second).UnixMilli(),
			},
			wantMin: 500 * time.Millisecond,
			wantMax: 1500 * time.Millisecond,
		},
		{
			name: "cancelled order retries promptly after cooldown",
			order: store.SupplyOrder{
				OrderID: "cancelled-ready", Product: "oauth_30d", RequestedQuantity: 10, Automatic: true,
				Status: "cancelled", RemoteStatus: "cancelled", CompletedAtMS: now.Add(-11 * time.Second).UnixMilli(),
			},
			wantMin: 500 * time.Millisecond,
			wantMax: 1500 * time.Millisecond,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "supply-retry-interval.sqlite"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })
			enabled := true
			cfg := store.ManagerSupplyConfig{
				Enabled: &enabled, Product: "oauth_30d", CheckIntervalSeconds: 60,
				HealthyMinutesTarget: 60, WarningMinutes: 30, CriticalMinutes: 20,
				ReplenishBatchSize: 20, PrelockMaxQuantity: 20,
			}
			if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{Supply: cfg}); err != nil {
				t.Fatalf("save config: %v", err)
			}
			if _, err := st.CreateSupplyOrder(context.Background(), tt.order); err != nil {
				t.Fatalf("create order: %v", err)
			}
			service := New(st, managerconfigsvc.New(config.Config{}, st, nil), nil)
			service.setSmartResource(SmartResource{
				GeneratedAtMS: time.Now().UnixMilli(), Enabled: true, SnapshotFresh: true,
				EmergencyShortage: true, EffectiveHealthyMinutes: 60, CriticalMinutes: 20,
				EstimatedSustainMinutes: 10, ConsumeRCUPerMinute: 1_000,
				CapacityGapRCU: 40_000, SuggestedQuantity: 13,
			})

			interval := service.NextInterval(context.Background())
			if interval < tt.wantMin || interval > tt.wantMax {
				t.Fatalf("next interval = %s, want [%s, %s]", interval, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestEmergencyRetryWakesAfterZeroDeliveryBurst(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply-retry-burst.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	enabled := true
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		Enabled: &enabled, Product: "oauth_30d", CheckIntervalSeconds: 60,
		HealthyMinutesTarget: 60, WarningMinutes: 30, CriticalMinutes: 20,
		ReplenishBatchSize: 20, PrelockMaxQuantity: 20,
	}}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	now := time.Now()
	for index, age := range []time.Duration{9 * time.Second, 6 * time.Second, 3 * time.Second} {
		if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
			OrderID:           fmt.Sprintf("cancelled-burst-%d", index),
			Product:           "oauth_30d",
			RequestedQuantity: 17,
			Automatic:         true,
			Status:            "cancelled",
			RemoteStatus:      "cancelled",
			CompletedAtMS:     now.Add(-age).UnixMilli(),
		}); err != nil {
			t.Fatalf("create cancelled order %d: %v", index, err)
		}
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), nil)
	service.setSmartResource(SmartResource{
		GeneratedAtMS: time.Now().UnixMilli(), Enabled: true, SnapshotFresh: true,
		EmergencyShortage: true, EffectiveHealthyMinutes: 60, CriticalMinutes: 20,
		EstimatedSustainMinutes: 10, ConsumeRCUPerMinute: 1_000,
		CapacityGapRCU: 40_000, SuggestedQuantity: 17,
	})

	interval := service.NextInterval(ctx)
	if interval < 500*time.Millisecond || interval > 1500*time.Millisecond {
		t.Fatalf("burst retry interval = %s, want immediate retry", interval)
	}
}

func TestUrgentCapacityRetryKeepsTenSecondRhythmAfterCancellationBurst(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "urgent-retry-rhythm.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	now := time.Now()
	var latest store.SupplyOrder
	for index := 0; index < 5; index++ {
		latest, err = st.CreateSupplyOrder(ctx, store.SupplyOrder{
			OrderID: fmt.Sprintf("cancelled-urgent-%d", index), Product: "oauth_7d", RequestedQuantity: 9,
			Automatic: true, Status: "cancelled", RemoteStatus: "cancelled",
			CreatedAtMS:   now.Add(time.Duration(index-6) * time.Second).UnixMilli(),
			CompletedAtMS: now.Add(time.Duration(index-5) * time.Second).UnixMilli(),
		})
		if err != nil {
			t.Fatalf("create cancelled order %d: %v", index, err)
		}
	}
	service := New(st, nil)
	resource := SmartResource{
		SnapshotFresh: true, EmergencyShortage: true, HealthLevel: smartHealthCritical,
		ConsumeRCUPerMinute: 500, CapacityGapRCU: 20_000,
		AccountClassificationObserved: true, AvailableAccounts: 1,
		CriticalAvailableAccounts: 2, AvailableSustainMinutes: 1.5,
	}
	plan, err := service.automaticRetryPlan(ctx, store.ManagerSupplyConfig{Product: "oauth_7d"}, resource, latest, now)
	if err != nil || !plan.active || plan.cooldown != automaticUrgentRetryCycleCooldown {
		t.Fatalf("urgent retry plan=%#v err=%v", plan, err)
	}

	resource.AvailableAccounts = 10
	resource.AvailableSustainMinutes = 20
	plan, err = service.automaticRetryPlan(ctx, store.ManagerSupplyConfig{Product: "oauth_7d"}, resource, latest, now)
	if err != nil || plan.cooldown != time.Minute {
		t.Fatalf("critical shortage retry plan=%#v err=%v, want default configured cadence", plan, err)
	}

	resource.EmergencyShortage = false
	resource.HealthLevel = smartHealthHealthy
	plan, err = service.automaticRetryPlan(ctx, store.ManagerSupplyConfig{Product: "oauth_7d"}, resource, latest, now)
	if err != nil || plan.cooldown != 5*time.Minute {
		t.Fatalf("buffered non-scarce retry plan=%#v err=%v", plan, err)
	}

	resource.EmergencyShortage = true
	resource.HealthLevel = smartHealthCritical
	resource.SupplyPressureLevel = smartSupplyPressureScarce
	resource.SupplyPressureReason = "supply_history_low_fulfillment"
	plan, err = service.automaticRetryPlan(ctx, store.ManagerSupplyConfig{
		Product: "oauth_7d", CheckIntervalSeconds: 30,
	}, resource, latest, now)
	if err != nil || plan.cooldown != 30*time.Second {
		t.Fatalf("scarce retry plan=%#v err=%v, want configured 30-second cadence", plan, err)
	}
}

func TestScarceRetryCycleUsesConfiguredCheckCadence(t *testing.T) {
	cfg := store.ManagerSupplyConfig{CheckIntervalSeconds: 30, CreateCooldownSeconds: 120}
	resource := SmartResource{
		SnapshotFresh: true, EmergencyShortage: true, HealthLevel: smartHealthCritical,
		ConsumeRCUPerMinute: 1_000, CapacityGapRCU: 30_000,
		AvailableAccounts: 9, CriticalAvailableAccounts: 2, AvailableSustainMinutes: 9.3,
		SupplyPressureLevel: smartSupplyPressureScarce,
	}
	if got := automaticRetryCycleCooldown(cfg, resource); got != 30*time.Second {
		t.Fatalf("scarce retry cycle cooldown=%s, want configured 30 seconds", got)
	}

	resource.AvailableAccounts = 1
	resource.AvailableSustainMinutes = 1.5
	if got := automaticRetryCycleCooldown(cfg, resource); got != automaticUrgentRetryCycleCooldown {
		t.Fatalf("urgent retry cycle cooldown=%s, want %s", got, automaticUrgentRetryCycleCooldown)
	}
}

func TestExhaustedScarceLadderSchedulesFreshWaveWithinConfiguredThirtySeconds(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "scarce-wave-cadence.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	enabled := true
	cfg := store.ManagerSupplyConfig{
		Enabled: &enabled, SmartEnabled: &enabled, Product: "oauth_7d",
		CheckIntervalSeconds: 30, CreateCooldownSeconds: 120,
		ReplenishBatchSize: 15, PrelockMaxQuantity: 9,
	}
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{Supply: cfg}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	now := time.Now()
	for index, quantity := range []int{9, 5, 2} {
		age := time.Duration(3-index) * time.Second
		reason := "emergency_retry_immediate_after_cancelled"
		if index == 0 {
			reason = "available_capacity_critical"
		}
		if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
			OrderID: fmt.Sprintf("scarce-wave-%d", quantity), Product: cfg.Product,
			RequestedQuantity: quantity, Automatic: true, TriggerReason: reason,
			Status: "cancelled", RemoteStatus: "cancelled",
			CreatedAtMS: now.Add(-age - time.Second).UnixMilli(), CompletedAtMS: now.Add(-age).UnixMilli(),
		}); err != nil {
			t.Fatalf("create cancelled quantity %d: %v", quantity, err)
		}
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), nil)
	service.setSmartResource(SmartResource{
		GeneratedAtMS: now.UnixMilli(), Enabled: true, SnapshotFresh: true,
		EmergencyShortage: true, HealthLevel: smartHealthCritical,
		ConsumeRCUPerMinute: 1_000, CapacityGapRCU: 30_000,
		AvailableAccounts: 9, CriticalAvailableAccounts: 2, AvailableSustainMinutes: 9.3,
		SupplyPressureLevel: smartSupplyPressureScarce,
	})

	interval := service.NextInterval(ctx)
	if interval < 27*time.Second || interval > 31*time.Second {
		t.Fatalf("fresh scarce wave interval=%s, want configured 30-second cycle from latest cancellation", interval)
	}
}

func TestAutomaticRetryLadderQuantityKeepsTheOriginalBase(t *testing.T) {
	tests := []struct {
		base     int
		failures int
		want     int
	}{
		{base: 10, failures: 1, want: 5},
		{base: 10, failures: 2, want: 2},
		{base: 10, failures: 3, want: 0},
		{base: 21, failures: 1, want: 11},
		{base: 21, failures: 2, want: 5},
	}
	for _, tt := range tests {
		if got := automaticRetryLadderQuantity(tt.base, tt.failures); got != tt.want {
			t.Errorf("ladder(%d, %d) = %d, want %d", tt.base, tt.failures, got, tt.want)
		}
	}
}

func TestAutomaticImmediateRetryRequiresConfirmedRemoteCancellation(t *testing.T) {
	nowMS := time.Now().UnixMilli()
	cfg := store.ManagerSupplyConfig{Product: "oauth_7d"}
	base := store.SupplyOrder{
		OrderID: "cancelled-10", Product: cfg.Product, RequestedQuantity: 10,
		Automatic: true, Status: "cancelled", CompletedAtMS: nowMS,
	}
	if automaticImmediateRetryEligible(cfg, base) {
		t.Fatal("local cancelled status without supplier terminal status must not unlock another order")
	}
	base.RemoteStatus = "cancelled"
	if !automaticImmediateRetryEligible(cfg, base) {
		t.Fatal("explicit zero-delivery supplier cancellation should unlock the next ladder rung")
	}
	base.LastError = "supply API returned HTTP 409"
	if automaticImmediateRetryEligible(cfg, base) {
		t.Fatal("a transport conflict inferred as cancellation must not unlock another order")
	}
}

func TestAutomaticRetryLadderSkipsRepeatedQuantitiesAndExhaustsOnce(t *testing.T) {
	now := time.Now()
	cancelled := func(id string, quantity int, age time.Duration, reason string) store.SupplyOrder {
		return store.SupplyOrder{
			OrderID: id, Product: "oauth_7d", RequestedQuantity: quantity,
			Automatic: true, TriggerReason: reason, Status: "cancelled", RemoteStatus: "cancelled",
			CreatedAtMS: now.Add(-age - time.Second).UnixMilli(), CompletedAtMS: now.Add(-age).UnixMilli(),
		}
	}
	repeated := []store.SupplyOrder{
		cancelled("retry-5-c", 5, time.Second, "emergency_retry_immediate_after_cancelled"),
		cancelled("retry-5-b", 5, 2*time.Second, "emergency_retry_immediate_after_cancelled"),
		cancelled("retry-5-a", 5, 3*time.Second, "emergency_refill_to_healthy"),
	}
	state := automaticRetryLadderStateForOrders(store.ManagerSupplyConfig{Product: "oauth_7d"}, repeated[0], repeated)
	if state.baseQuantity != 5 || state.nextQuantity != 3 {
		t.Fatalf("repeated quantity state = %#v, want base 5 and next untried rung 3", state)
	}

	exhausted := []store.SupplyOrder{
		cancelled("retry-2", 2, time.Second, "emergency_retry_immediate_after_cancelled"),
		cancelled("retry-5", 5, 2*time.Second, "emergency_retry_immediate_after_cancelled"),
		cancelled("base-10", 10, 3*time.Second, "emergency_refill_to_healthy"),
	}
	state = automaticRetryLadderStateForOrders(store.ManagerSupplyConfig{Product: "oauth_7d"}, exhausted[0], exhausted)
	if state.baseQuantity != 10 || state.nextQuantity != 0 {
		t.Fatalf("exhausted quantity state = %#v, want completed 10/5/2 cycle", state)
	}
}

func TestEmergencyParallelNextIntervalBuildsQuantityLadderImmediately(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply-emergency-ladder.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	enabled := true
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		Enabled: &enabled, Product: "oauth_7d", MaxConcurrentOrders: 3,
		SmartEnabled: &enabled, CheckIntervalSeconds: 60,
	}}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "emergency-quantity-10", Product: "oauth_7d", RequestedQuantity: 10,
		Automatic: true, Status: "waiting_inventory",
		SupplierRetryUntilMS: time.Now().Add(time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatalf("create waiting order: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), nil)
	service.setSmartResource(SmartResource{
		GeneratedAtMS: time.Now().UnixMilli(), Enabled: true, SnapshotFresh: true, EmergencyShortage: true,
		HealthLevel: smartHealthCritical, CapacityGapRCU: 10_000,
	})

	interval := service.NextInterval(ctx)
	if interval < 500*time.Millisecond || interval > 1500*time.Millisecond {
		t.Fatalf("parallel ladder interval = %s, want about one second", interval)
	}
}

func TestEmergencyParallelNextIntervalStopsAfterTwoCancelledRungsForSamePrimary(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply-emergency-ladder-exhausted.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	enabled := true
	cfg := store.ManagerSupplyConfig{
		Enabled: &enabled, Product: "oauth_7d", MaxConcurrentOrders: 3,
		SmartEnabled: &enabled, CheckIntervalSeconds: 60,
	}
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{Supply: cfg}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	now := time.Now()
	primary, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "primary-10", Product: "oauth_7d", RequestedQuantity: 10,
		Automatic: true, Status: "waiting_inventory", CreatedAtMS: now.Add(-10 * time.Second).UnixMilli(),
		SupplierRetryUntilMS: now.Add(time.Minute).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("create primary order: %v", err)
	}
	for index, quantity := range []int{5, 2} {
		if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
			OrderID: fmt.Sprintf("cancelled-rung-%d", quantity), Product: "oauth_7d", RequestedQuantity: quantity,
			Automatic: true, TriggerReason: "parallel_emergency_refill_to_healthy", Status: "cancelled",
			CreatedAtMS: now.Add(time.Duration(index-8) * time.Second).UnixMilli(), CompletedAtMS: now.Add(-time.Second).UnixMilli(),
		}); err != nil {
			t.Fatalf("create cancelled rung %d: %v", quantity, err)
		}
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), nil)
	service.setSmartResource(SmartResource{
		GeneratedAtMS: time.Now().UnixMilli(), Enabled: true, SnapshotFresh: true, EmergencyShortage: true,
		HealthLevel: smartHealthCritical, CapacityGapRCU: 10_000,
	})
	eligible, err := service.automaticParallelCreateEligible(ctx, cfg, []store.SupplyOrder{primary})
	if err != nil {
		t.Fatalf("parallel eligibility: %v", err)
	}
	if eligible {
		t.Fatal("cancelled 5/2 rungs must not be submitted again while the primary order remains active")
	}
	interval := service.NextInterval(ctx)
	if interval < 50*time.Second {
		t.Fatalf("exhausted ladder interval = %s, want primary-order pacing instead of one-second create churn", interval)
	}
}

func TestAutomaticParallelCreateReplansActiveTaskWithFreeSlots(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "active-task-free-slots.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	enabled := true
	cfg := store.ManagerSupplyConfig{
		Enabled: &enabled, SmartEnabled: &enabled, Product: "oauth_7d",
		MaxConcurrentOrders: 3,
	}
	task, err := st.CreateSupplyPurchaseTask(ctx, store.SupplyPurchaseTask{
		TaskID: "purchase-growing-shortage", Source: "automatic", Product: "oauth_7d",
		TargetQuantity: 20, Status: purchaseTaskStatusRunning, MaxConcurrentOrders: 3,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	oldWaiting, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "old-final-one", TaskID: task.TaskID, Product: "oauth_7d",
		RequestedQuantity: 1, Automatic: true, Status: "waiting_inventory",
		TriggerReason: "parallel_available_capacity_critical",
	})
	if err != nil {
		t.Fatalf("create old waiting order: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil))
	eligible, err := service.automaticParallelCreateEligible(ctx, cfg, []store.SupplyOrder{oldWaiting})
	if err != nil {
		t.Fatalf("parallel continuation eligibility: %v", err)
	}
	if !eligible {
		t.Fatal("one old task order must leave the other emergency purchase slots available for replanning")
	}
}

func TestAutomaticParallelCreateBlocksAfterZeroDeliveryBurst(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply-parallel-burst.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	now := time.Now()
	for index, age := range []time.Duration{8 * time.Second, 4 * time.Second} {
		if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
			OrderID:           fmt.Sprintf("cancelled-parallel-%d", index),
			Product:           "oauth_7d",
			RequestedQuantity: 16,
			Automatic:         true,
			Status:            "cancelled",
			CompletedAtMS:     now.Add(-age).UnixMilli(),
		}); err != nil {
			t.Fatalf("create cancelled order %d: %v", index, err)
		}
	}
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID:           "waiting-after-burst",
		Product:           "oauth_7d",
		RequestedQuantity: 16,
		Automatic:         true,
		Status:            "waiting_inventory",
	}); err != nil {
		t.Fatalf("create waiting order: %v", err)
	}
	service := New(st, nil, nil)
	blocked, err := service.automaticParallelCreateBlocked(ctx, store.ManagerSupplyConfig{Product: "oauth_7d"})
	if err != nil {
		t.Fatalf("parallel burst guard: %v", err)
	}
	if blocked {
		t.Fatal("parallel order creation must keep the bounded competition window open after cancellations")
	}
}

func TestEmergencyRetryNextIntervalExcludesUnsafeFailures(t *testing.T) {
	now := time.Now()
	tests := []store.SupplyOrder{
		{OrderID: "uncertain", Product: "oauth_30d", Automatic: true, Status: "create_uncertain", CreatedAtMS: now.Add(-20 * time.Second).UnixMilli()},
		{OrderID: "paid", Product: "oauth_30d", Automatic: true, Status: "cancelled", ChargedFen: 100, CompletedAtMS: now.Add(-20 * time.Second).UnixMilli()},
		{OrderID: "auth-failed", Product: "oauth_30d", Automatic: true, Status: "failed", LastError: "supply API returned HTTP 401", CompletedAtMS: now.Add(-20 * time.Second).UnixMilli()},
		{OrderID: "recovery-recent", Product: "oauth_30d", Automatic: true, Strategy: "recovery", Status: "failed", CompletedAtMS: now.Add(-20 * time.Second).UnixMilli()},
	}
	for _, order := range tests {
		t.Run(order.OrderID, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "supply-retry-exclusion.sqlite"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })
			enabled := true
			cfg := store.ManagerSupplyConfig{
				Enabled: &enabled, Product: "oauth_30d", CheckIntervalSeconds: 60,
				HealthyMinutesTarget: 60, WarningMinutes: 30, CriticalMinutes: 20,
				ReplenishBatchSize: 20, PrelockMaxQuantity: 20,
			}
			if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{Supply: cfg}); err != nil {
				t.Fatalf("save config: %v", err)
			}
			if _, err := st.CreateSupplyOrder(context.Background(), order); err != nil {
				t.Fatalf("create order: %v", err)
			}
			service := New(st, managerconfigsvc.New(config.Config{}, st, nil), nil)
			service.setSmartResource(SmartResource{
				GeneratedAtMS: time.Now().UnixMilli(), Enabled: true, SnapshotFresh: true,
				EmergencyShortage: true, EffectiveHealthyMinutes: 60, CriticalMinutes: 20,
				EstimatedSustainMinutes: 10, ConsumeRCUPerMinute: 1_000,
				CapacityGapRCU: 40_000, SuggestedQuantity: 13,
			})

			interval := service.NextInterval(context.Background())
			if order.Status == "create_uncertain" {
				if interval < 59*time.Second || interval > 61*time.Second {
					t.Fatalf("create_uncertain interval = %s, want normal open-order pacing", interval)
				}
				return
			}
			if interval < 59*time.Second || interval > 61*time.Second {
				t.Fatalf("unsafe failure interval = %s, want normal configured pacing", interval)
			}
		})
	}
}

func TestSmartSupplyPressureUsesRealizedFulfillmentInsteadOfInventorySnapshot(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply-fulfillment-pressure.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now()
	for index := 0; index < 4; index++ {
		createdAt := now.Add(-time.Duration(index+2) * time.Minute).UnixMilli()
		if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
			OrderID: fmt.Sprintf("cancelled-%d", index), Product: "oauth_7d",
			RequestedQuantity: 5, Automatic: true, Status: "cancelled",
			CreatedAtMS: createdAt, CompletedAtMS: createdAt + 1_000,
		}); err != nil {
			t.Fatalf("create cancelled order: %v", err)
		}
	}
	createdAt := now.Add(-time.Minute).UnixMilli()
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "completed-1", Product: "oauth_7d", RequestedQuantity: 5,
		Automatic: true, Status: "completed", ImportedCount: 5,
		CreatedAtMS: createdAt, CompletedAtMS: createdAt + 1_000,
	}); err != nil {
		t.Fatalf("create completed order: %v", err)
	}

	service := New(st, nil)
	pressure := service.smartSupplyPressure(context.Background(), store.ManagerSupplyConfig{Product: "oauth_7d"}, supplyclient.Inventory{
		Available: 100,
	}, 5)
	if pressure.level != smartSupplyPressureScarce || pressure.reason != "supply_history_low_fulfillment" {
		t.Fatalf("pressure=%#v, want scarce realized fulfillment", pressure)
	}
	if pressure.recentOrders != 5 || pressure.recentCancelled != 4 || pressure.recentZeroDelivery != 4 ||
		pressure.requestedQuantity != 25 || pressure.deliveredQuantity != 5 || pressure.fulfillmentRate != 20 {
		t.Fatalf("fulfillment pressure metrics=%#v", pressure)
	}
}

func TestSmartSupplyPressureRecentSuccessStreakOverridesOlderFailures(t *testing.T) {
	now := time.Now()
	orders := make([]store.SupplyOrder, 0, 7)
	for index := 0; index < 4; index++ {
		createdAt := now.Add(-time.Duration(index+10) * time.Minute)
		orders = append(orders, store.SupplyOrder{
			OrderID: fmt.Sprintf("old-cancelled-%d", index), Product: "oauth_7d",
			RequestedQuantity: 5, Automatic: true, Status: "cancelled",
			CreatedAtMS: createdAt.UnixMilli(), CompletedAtMS: createdAt.Add(time.Second).UnixMilli(),
		})
	}
	for index := 0; index < 3; index++ {
		createdAt := now.Add(-time.Duration(index+1) * time.Minute)
		orders = append(orders, store.SupplyOrder{
			OrderID: fmt.Sprintf("recent-completed-%d", index), Product: "oauth_7d",
			RequestedQuantity: 1, ImportedCount: 1, Automatic: true, Status: "completed",
			CreatedAtMS: createdAt.UnixMilli(), CompletedAtMS: createdAt.Add(10 * time.Second).UnixMilli(),
		})
	}

	pressure := smartSupplyPressureFromOrders(
		store.ManagerSupplyConfig{Product: "oauth_7d"},
		supplyclient.Inventory{Missing: 15, NeedsProduction: true},
		4,
		orders,
	)
	if !pressure.reliablyAvailable || pressure.level != smartSupplyPressurePlenty ||
		pressure.reason != "supply_history_reliably_available" {
		t.Fatalf("recent reliable supply was not recognized: %#v", pressure)
	}
	if pressure.recentSuccessStreak != 3 || pressure.shortWindowOrders != 3 ||
		pressure.shortWindowFulfillmentRate != 100 {
		// The reliability window closes at the first older failure once the latest
		// three orders have already established a new successful regime.
		t.Fatalf("short-window metrics = %#v", pressure)
	}
}

func TestSmartSupplyPressureTwoFastSuccessesEnterRecoveryProbe(t *testing.T) {
	now := time.Now()
	orders := make([]store.SupplyOrder, 0, 6)
	for index := 0; index < 4; index++ {
		createdAt := now.Add(-time.Duration(index+10) * time.Minute)
		orders = append(orders, store.SupplyOrder{
			OrderID: fmt.Sprintf("old-cancelled-%d", index), Product: "oauth_7d",
			RequestedQuantity: 5, Automatic: true, Status: "cancelled",
			CreatedAtMS: createdAt.UnixMilli(), CompletedAtMS: createdAt.Add(time.Second).UnixMilli(),
		})
	}
	for index := 0; index < 2; index++ {
		createdAt := now.Add(-time.Duration(index+1) * time.Minute)
		orders = append(orders, store.SupplyOrder{
			OrderID: fmt.Sprintf("recent-completed-%d", index), Product: "oauth_7d",
			RequestedQuantity: 1, ImportedCount: 1, Automatic: true, Status: "completed",
			CreatedAtMS: createdAt.UnixMilli(), CompletedAtMS: createdAt.Add(10 * time.Second).UnixMilli(),
		})
	}

	pressure := smartSupplyPressureFromOrders(
		store.ManagerSupplyConfig{Product: "oauth_7d"},
		supplyclient.Inventory{Available: 15, MinimumRemainingSeconds: 2735, MaximumRemainingSeconds: 2735},
		4,
		orders,
	)
	if pressure.reliablyAvailable || !pressure.recoveringAvailable || pressure.level != smartSupplyPressurePlenty ||
		pressure.reason != "supply_history_recovering" {
		t.Fatalf("recovering supply was not reduced to a probe: %#v", pressure)
	}
	if pressure.recentSuccessStreak != 2 || pressure.inventoryMinRemainSeconds != 2735 {
		t.Fatalf("recovery evidence = %#v", pressure)
	}
}

func TestSmartSupplyPressureSlowSuccessDoesNotOverrideProductionShortage(t *testing.T) {
	now := time.Now()
	orders := make([]store.SupplyOrder, 0, 3)
	for index := 0; index < 3; index++ {
		createdAt := now.Add(-time.Duration(index+1) * time.Minute)
		orders = append(orders, store.SupplyOrder{
			OrderID: fmt.Sprintf("slow-completed-%d", index), Product: "oauth_7d",
			RequestedQuantity: 1, ImportedCount: 1, Automatic: true, Status: "completed",
			CreatedAtMS: createdAt.UnixMilli(), CompletedAtMS: createdAt.Add(2 * time.Minute).UnixMilli(),
		})
	}

	pressure := smartSupplyPressureFromOrders(
		store.ManagerSupplyConfig{Product: "oauth_7d"},
		supplyclient.Inventory{Missing: 15, NeedsProduction: true},
		4,
		orders,
	)
	if pressure.reliablyAvailable || pressure.level != smartSupplyPressureScarce ||
		pressure.reason != "supply_inventory_scarce" {
		t.Fatalf("slow production-backed supply was treated as reliable: %#v", pressure)
	}
}

func TestAutomaticSupplyGuardRequiresFreshBaselineAndSettledImports(t *testing.T) {
	service := New(nil, nil)
	nowMS := time.Now().UnixMilli()
	service.automaticEnabled = true
	service.automaticBaselineAtMS = nowMS
	service.automaticAccountAtMS = nowMS
	service.inspectionSnapshotRefresh.refresh = func(context.Context) error { return nil }

	resource := SmartResource{SnapshotFresh: true, CapacitySnapshotAtMS: nowMS - 1}
	if reason := service.automaticSupplyGuardReason(resource); reason != "" {
		t.Fatalf("fresh persisted capacity snapshot must survive a process restart, reason = %q", reason)
	}
	resource.SnapshotRefreshInProgress = true
	if reason := service.automaticSupplyGuardReason(resource); reason != "" {
		t.Fatalf("background refresh must not hide a still-fresh completed snapshot, reason = %q", reason)
	}
	resource.SnapshotFresh = false
	if reason := service.automaticSupplyGuardReason(resource); reason != "initial_capacity_snapshot_pending" {
		t.Fatalf("stale capacity baseline reason = %q", reason)
	}
	resource.CurrentCapacityRCU = 34
	resource.CapacityGapRCU = 108
	resource.CapacitySnapshotAtMS = nowMS - int64((2*time.Minute)/time.Millisecond)
	resource.CapacitySnapshotAgeSeconds = 120
	resource.DecisionReason = "inspection_quota_incomplete_capacity_deficit"
	if reason := service.automaticSupplyGuardReason(resource); reason != "" {
		t.Fatalf("recent verified partial capacity deficit must remain orderable, reason = %q", reason)
	}
	resource.CapacityGapRCU = 0
	if reason := service.automaticSupplyGuardReason(resource); reason != "initial_capacity_snapshot_pending" {
		t.Fatalf("partial snapshot without a capacity deficit must remain blocked, reason = %q", reason)
	}
	resource.CurrentCapacityRCU = 0
	resource.CapacitySnapshotAtMS = 0
	resource.DecisionReason = "snapshot_not_ready"
	resource.EmergencyShortage = true
	resource.EmergencyReason = "available_capacity_critical"
	resource.SuggestedAction = smartActionEmergencyReplenish
	if reason := service.automaticSupplyGuardReason(resource); reason != "" {
		t.Fatalf("live account emergency must bypass the missing quota baseline, reason = %q", reason)
	}
	resource.EmergencyShortage = false
	resource.EmergencyReason = ""
	resource.SuggestedAction = smartActionSnapshotStale
	resource.SnapshotFresh = true
	resource.SnapshotRefreshInProgress = false
	resource.CapacitySnapshotAtMS = nowMS
	resource.QuotaEstimatePendingPlans = 1
	if reason := service.automaticSupplyGuardReason(resource); reason != "" {
		t.Fatalf("upward staged quota calibration must not pause ordering, reason = %q", reason)
	}
	resource.QuotaEstimatePendingPlans = 0
	resource.QuotaEstimateOrderingBlocked = true
	if reason := service.automaticSupplyGuardReason(resource); reason != "" {
		t.Fatalf("quota estimate self-check must use the fallback without pausing ordering, reason = %q", reason)
	}
	resource.QuotaEstimateOrderingBlocked = false
	resource.PendingInspectionAccounts = 6
	if reason := service.automaticSupplyGuardReason(resource); reason != "pending_account_inspection" {
		t.Fatalf("pending import guard reason = %q", reason)
	}
	resource.CapacitySource = smartCapacitySourceInspection
	resource.PendingInspectionCapacityRCU = 600
	if reason := service.automaticSupplyGuardReason(resource); reason != "" {
		t.Fatalf("fresh historical snapshot with local import delta must not pause ordering, reason = %q", reason)
	}
	resource.SnapshotFresh = false
	if reason := service.automaticSupplyGuardReason(resource); reason != "initial_capacity_snapshot_pending" {
		t.Fatalf("stale historical snapshot must retain pending import guard, reason = %q", reason)
	}
	resource.SnapshotFresh = true
	resource.EmergencyShortage = true
	if reason := service.automaticSupplyGuardReason(resource); reason != "" {
		t.Fatalf("emergency must bypass pending inspection guard, reason = %q", reason)
	}
}

func TestAutomaticBaselineDoesNotLoopRecentPartialSnapshotRefresh(t *testing.T) {
	service := New(nil, nil)
	nowMS := time.Now().UnixMilli()
	service.automaticEnabled = true
	service.automaticBaselineAtMS = nowMS
	service.automaticAccountAtMS = nowMS
	release := make(chan struct{})
	service.SetInspectionSnapshotRefresher(context.Background(), func(context.Context) error {
		<-release
		return nil
	})

	resource := SmartResource{
		Enabled:                    true,
		SnapshotFresh:              false,
		SnapshotEvidencePartial:    true,
		CapacitySnapshotAtMS:       time.Now().Add(-2 * time.Minute).UnixMilli(),
		CapacitySnapshotAgeSeconds: int((2 * time.Minute) / time.Second),
		DecisionReason:             "capacity_healthy",
	}
	if reason := service.automaticBaselineBlockReason(resource); reason != "initial_capacity_snapshot_pending" {
		t.Fatalf("partial snapshot guard reason = %q", reason)
	}
	service.inspectionSnapshotRefreshMu.Lock()
	running := service.inspectionSnapshotRefresh.running
	service.inspectionSnapshotRefreshMu.Unlock()
	if running {
		t.Fatal("recent partial snapshot started another full inspection")
	}

	resource.CapacitySnapshotAgeSeconds = int(smartInspectionSnapshotFreshTTL/time.Second) + 1
	if reason := service.automaticBaselineBlockReason(resource); reason != "initial_capacity_snapshot_pending" {
		t.Fatalf("expired partial snapshot guard reason = %q", reason)
	}
	service.inspectionSnapshotRefreshMu.Lock()
	running = service.inspectionSnapshotRefresh.running
	service.inspectionSnapshotRefreshMu.Unlock()
	if !running {
		t.Fatal("expired partial snapshot did not start a refresh")
	}
	close(release)
}

func TestInspectionSnapshotRefreshFailureUsesScheduledCadenceBackoff(t *testing.T) {
	service := New(nil, nil)
	failed := make(chan struct{})
	var calls atomic.Int32
	service.SetInspectionSnapshotRefresher(context.Background(), func(context.Context) error {
		calls.Add(1)
		close(failed)
		return errors.New("lease lost")
	})

	service.requestStaleInspectionSnapshotRefresh()
	select {
	case <-failed:
	case <-time.After(time.Second):
		t.Fatal("refresh did not run")
	}
	deadline := time.Now().Add(time.Second)
	for {
		service.inspectionSnapshotRefreshMu.Lock()
		running := service.inspectionSnapshotRefresh.running
		retryAfter := service.inspectionSnapshotRefresh.retryAfter
		lastAttempt := service.inspectionSnapshotRefresh.lastAttempt
		service.inspectionSnapshotRefreshMu.Unlock()
		if !running && retryAfter.After(time.Now()) && !lastAttempt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failure backoff was not recorded: running=%v retryAfter=%v lastAttempt=%v", running, retryAfter, lastAttempt)
		}
		time.Sleep(time.Millisecond)
	}

	service.requestStaleInspectionSnapshotRefresh()
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("failed refresh retried immediately: calls=%d", calls.Load())
	}
}

func TestAccountPoolStatsSeparateTotalAvailableHealthyAndDisabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/auth-files" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"files":[
			{"name":"healthy.json","provider":"codex","status":"active"},
			{"name":"unknown-quota.json","provider":"codex","status":"active"},
			{"name":"disabled.json","provider":"codex","status":"disabled","disabled":true},
			{"name":"xai.json","provider":"xai","status":"active"}
		]}`))
	}))
	t.Cleanup(server.Close)
	service := New(nil, nil, server.Client())
	stats, err := service.countAccountPoolStats(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{
			CPABaseURL:    server.URL,
			ManagementKey: "management-key",
		},
	})
	if err != nil {
		t.Fatalf("count account pool: %v", err)
	}
	resource := SmartResource{HealthyAccounts: 1, NormalAccounts: 1}
	applyAccountPoolStats(&resource, stats)
	if resource.TotalAccounts != 2 || resource.AvailableAccounts != 2 || resource.SchedulableAccounts != 2 ||
		resource.HealthyAccounts != 1 || resource.WeakAccounts != 1 || resource.AtRiskAccounts != 2 ||
		resource.DisabledAccounts != 1 {
		t.Fatalf("account pool statistics = %#v", resource)
	}
}

func TestOperatorAccountPoolStatsSharesOneSnapshotUntilInvalidated(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/auth-files" {
			http.NotFound(w, r)
			return
		}
		request := requests.Add(1)
		if request == 1 {
			_, _ = w.Write([]byte(`{"files":[{"name":"first.json","provider":"codex","status":"active","auth_index":"first"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"files":[{"name":"second.json","provider":"codex","status":"disabled","disabled":true,"auth_index":"second"}]}`))
	}))
	t.Cleanup(server.Close)

	service := New(nil, nil, server.Client())
	cfg := store.ManagerConfig{CPAConnection: store.ManagerCPAConnectionConfig{
		CPABaseURL:    server.URL,
		ManagementKey: "management-key",
	}}
	first, err := service.countOperatorAccountPoolStats(context.Background(), cfg)
	if err != nil {
		t.Fatalf("load first operator snapshot: %v", err)
	}
	second, err := service.countOperatorAccountPoolStats(context.Background(), cfg)
	if err != nil {
		t.Fatalf("load shared operator snapshot: %v", err)
	}
	if requests.Load() != 1 || first.total != 1 || first.enabled != 1 || second.total != first.total || second.enabled != first.enabled {
		t.Fatalf("shared operator snapshot requests=%d first=%#v second=%#v", requests.Load(), first, second)
	}
	if !first.classificationObserved || first.normal != 1 || first.unconfirmed != 0 || first.operatorUsable != 1 {
		t.Fatalf("live schedulable fallback classification = %#v", first)
	}

	service.invalidateAuthFilesCache()
	refreshed, err := service.countOperatorAccountPoolStats(context.Background(), cfg)
	if err != nil {
		t.Fatalf("refresh invalidated operator snapshot: %v", err)
	}
	if requests.Load() != 2 || refreshed.total != 1 || refreshed.enabled != 0 {
		t.Fatalf("invalidated operator snapshot requests=%d refreshed=%#v", requests.Load(), refreshed)
	}
}

func TestOperatorAccountPoolStatsReturnsStaleSnapshotDuringRefresh(t *testing.T) {
	var requests atomic.Int32
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/auth-files" {
			http.NotFound(w, r)
			return
		}
		if requests.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"files":[{"name":"first.json","provider":"codex","status":"active","auth_index":"first"}]}`))
			return
		}
		close(refreshStarted)
		<-releaseRefresh
		_, _ = w.Write([]byte(`{"files":[{"name":"second.json","provider":"codex","status":"disabled","disabled":true,"auth_index":"second"}]}`))
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseRefresh) })
		server.Close()
	})

	service := New(nil, nil, server.Client())
	cfg := store.ManagerConfig{CPAConnection: store.ManagerCPAConnectionConfig{
		CPABaseURL:    server.URL,
		ManagementKey: "management-key",
	}}
	initial, err := service.countOperatorAccountPoolStats(context.Background(), cfg)
	if err != nil || initial.enabled != 1 {
		t.Fatalf("load initial operator snapshot: stats=%#v err=%v", initial, err)
	}
	expiredAt := time.Now().Add(-2 * time.Minute)
	service.operatorPoolMu.Lock()
	service.operatorPool.generated = expiredAt
	service.operatorPoolMu.Unlock()
	service.authCacheMu.Lock()
	service.authCache.generatedAt = expiredAt
	service.authCache.attemptedAt = expiredAt
	service.authCacheMu.Unlock()

	type result struct {
		stats accountPoolStats
		err   error
	}
	refreshResult := make(chan result, 1)
	go func() {
		stats, err := service.countOperatorAccountPoolStats(context.Background(), cfg)
		refreshResult <- result{stats: stats, err: err}
	}()
	select {
	case <-refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("operator snapshot refresh did not start")
	}

	staleResult := make(chan result, 1)
	go func() {
		stats, err := service.countOperatorAccountPoolStats(context.Background(), cfg)
		staleResult <- result{stats: stats, err: err}
	}()
	select {
	case stale := <-staleResult:
		if stale.err != nil || stale.stats.enabled != 1 {
			t.Fatalf("stale operator snapshot: stats=%#v err=%v", stale.stats, stale.err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("stale operator snapshot waited behind the active refresh")
	}
	if requests.Load() != 2 {
		t.Fatalf("concurrent operator snapshot requests = %d, want 2", requests.Load())
	}

	releaseOnce.Do(func() { close(releaseRefresh) })
	select {
	case refreshed := <-refreshResult:
		if refreshed.err != nil || refreshed.stats.enabled != 0 {
			t.Fatalf("refreshed operator snapshot: stats=%#v err=%v", refreshed.stats, refreshed.err)
		}
	case <-time.After(time.Second):
		t.Fatal("operator snapshot refresh did not finish")
	}
}

func TestDashboardAccountPoolReturnsInvalidatedSnapshotWhileRefreshing(t *testing.T) {
	var requests atomic.Int32
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/auth-files" {
			http.NotFound(w, r)
			return
		}
		if requests.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"files":[{"name":"first.json","provider":"codex","status":"active","auth_index":"first"}]}`))
			return
		}
		close(refreshStarted)
		<-releaseRefresh
		_, _ = w.Write([]byte(`{"files":[{"name":"second.json","provider":"codex","status":"disabled","disabled":true,"auth_index":"second"}]}`))
	}))
	t.Cleanup(func() {
		select {
		case <-releaseRefresh:
		default:
			close(releaseRefresh)
		}
		server.Close()
	})

	service := New(nil, nil, server.Client())
	cfg := store.ManagerConfig{CPAConnection: store.ManagerCPAConnectionConfig{
		CPABaseURL:    server.URL,
		ManagementKey: "management-key",
	}}
	initial, err := service.countOperatorAccountPoolStats(context.Background(), cfg)
	if err != nil || initial.enabled != 1 {
		t.Fatalf("initial operator snapshot=%#v err=%v", initial, err)
	}
	service.invalidateAuthFilesCache()

	stale, err := service.countOperatorAccountPoolStatsForDashboard(context.Background(), cfg)
	if err != nil || stale.enabled != 1 {
		t.Fatalf("invalidated dashboard snapshot=%#v err=%v", stale, err)
	}
	select {
	case <-refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("background account-pool refresh did not start")
	}
	second, err := service.countOperatorAccountPoolStatsForDashboard(context.Background(), cfg)
	if err != nil || second.enabled != 1 || requests.Load() != 2 {
		t.Fatalf("coalesced dashboard snapshot=%#v requests=%d err=%v", second, requests.Load(), err)
	}
	close(releaseRefresh)
	deadline := time.Now().Add(time.Second)
	for {
		service.operatorPoolMu.Lock()
		refreshed := service.operatorPool
		generation := service.operatorPoolGeneration
		service.operatorPoolMu.Unlock()
		if refreshed.generation == generation && refreshed.stats.enabled == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background account-pool refresh did not publish: cache=%#v generation=%d", refreshed, generation)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCPAAuthLifecyclePendingIsNotSchedulableCapacity(t *testing.T) {
	for _, status := range []string{
		"initializing",
		"refreshing_token",
		"refreshing_quota",
		"initialization_failed",
		"recovering_token",
		"recovering_quota",
		"recovery_failed",
	} {
		t.Run(status, func(t *testing.T) {
			file := cpaauthfiles.File{
				Name:     status + ".json",
				Provider: "codex",
				Raw:      map[string]any{"status": status},
			}
			if !isCPAAuthLifecyclePending(file) {
				t.Fatalf("status %q was not recognized as pending", status)
			}
			if isAvailableCodexFile(file) {
				t.Fatalf("status %q was counted as schedulable", status)
			}
			item := store.SupplyImportItem{Status: "imported"}
			if got := supplyAccountStatus(item, file, true, true, time.Now()); got != "cooldown" {
				t.Fatalf("supply account status = %q, want cooldown", got)
			}
		})
	}
}

func TestCPAAuthLifecyclePendingReadsPersistedLifecycleFields(t *testing.T) {
	for _, raw := range []map[string]any{
		{"status": "error", "initialization_state": "refreshing_quota"},
		{"status": "error", "recovery_state": "recovering_token"},
	} {
		file := cpaauthfiles.File{Name: "account.json", Provider: "codex", Raw: raw}
		if !isCPAAuthLifecyclePending(file) {
			t.Fatalf("lifecycle fields were not recognized as pending: %#v", raw)
		}
	}
}

func TestAccountPoolStatsKeepsPopulationIdentityAcrossLiveAndInspectionBuckets(t *testing.T) {
	remainingNormal := 10.0
	remainingRisk := 90.0
	files := []cpaauthfiles.File{
		{Name: "normal.json", Provider: "codex", AuthIndex: "normal", Raw: map[string]any{"status": "active"}},
		{Name: "risk.json", Provider: "codex", AuthIndex: "risk", Raw: map[string]any{"status": "active"}},
		{Name: "attention.json", Provider: "codex", AuthIndex: "attention", Raw: map[string]any{"status": "active"}},
		{Name: "disabled.json", Provider: "codex", AuthIndex: "disabled", Disabled: true, Raw: map[string]any{"status": "disabled"}},
	}
	results := []store.CodexInspectionResult{
		{FileName: "normal.json", Provider: "codex", AuthIndex: "normal", Action: "keep", UsedPercent: &remainingNormal},
		{FileName: "risk.json", Provider: "codex", AuthIndex: "risk", Action: "keep", UsedPercent: &remainingRisk},
		{FileName: "attention.json", Provider: "codex", AuthIndex: "attention", Action: "reauth"},
	}
	stats := accountPoolStatsFromFilesAndInspection(files, results)
	if stats.total != 4 || stats.enabled != 3 || stats.schedulable != 3 ||
		stats.normal != 1 || stats.quotaRisk != 1 || stats.needsAttention != 1 || stats.unconfirmed != 0 {
		t.Fatalf("live/inspection population identity = %#v", stats)
	}
	if got := stats.operatorAvailable(99); got != 1 {
		t.Fatalf("operator overview count = %d, want normal bucket 1", got)
	}
	unclassifiedStats := stats
	unclassifiedStats.classificationObserved = false
	if got := unclassifiedStats.operatorAvailable(99); got != 3 {
		t.Fatalf("unclassified operator overview count = %d, want live schedulable 3", got)
	}
	resource := SmartResource{}
	applyAccountPoolStats(&resource, stats)
	if resource.NormalAccounts+resource.NeedsAttentionAccounts+resource.QuotaRiskAccounts+
		resource.UnconfirmedAccounts != resource.TotalAccounts {
		t.Fatalf("exclusive account identity does not hold: %#v", resource)
	}
}

func TestAccountPoolSummaryCountsSchedulableQuotaRiskAsAvailable(t *testing.T) {
	checkedAt := time.UnixMilli(1_786_745_988_395)
	summary := accountPoolSummaryFromStats(accountPoolStats{
		total:                  46,
		enabled:                26,
		schedulable:            18,
		operatorUsable:         18,
		normal:                 3,
		needsAttention:         0,
		quotaRisk:              15,
		unconfirmed:            8,
		classificationObserved: true,
	}, checkedAt)
	if summary.CheckedAtMS != checkedAt.UnixMilli() || summary.Total != 26 || summary.Normal != 18 ||
		summary.NeedsAttention != 0 || summary.QuotaRisk != 15 || summary.Disabled != 20 ||
		summary.Unconfirmed != 8 || !summary.ClassificationObserved {
		t.Fatalf("account pool summary = %#v", summary)
	}
	if summary.Normal+summary.NeedsAttention+summary.QuotaRisk+summary.Unconfirmed <= summary.Total {
		t.Fatalf("schedulable availability did not include quota-risk credentials: %#v", summary)
	}
}

func TestAccountPoolSummarySeparatesEnabledPoolFromDisabledArchive(t *testing.T) {
	summary := accountPoolSummaryFromStats(accountPoolStats{
		total: 24, enabled: 14, schedulable: 3, operatorUsable: 3, normal: 3, needsAttention: 11,
		classificationObserved: true,
	}, time.Now())
	if summary.Total != 14 || summary.Normal != 3 || summary.NeedsAttention != 11 || summary.Disabled != 10 {
		t.Fatalf("14-account enabled pool summary = %#v", summary)
	}
}

func TestAccountPoolPlanSummariesUseLiveSchedulablePopulation(t *testing.T) {
	files := make([]cpaauthfiles.File, 0, 22)
	results := make([]store.CodexInspectionResult, 0, 22)
	add := func(planType string, count int) {
		for index := 0; index < count; index++ {
			name := fmt.Sprintf("%s-%02d.json", firstNonEmptyString(planType, "unknown"), index)
			authIndex := fmt.Sprintf("%s-%02d", firstNonEmptyString(planType, "unknown"), index)
			raw := map[string]any{
				"status": "active",
				"cpamp_import": map[string]any{
					"platform_id":   "legacy",
					"platform_name": "Legacy supplier",
				},
			}
			if planType != "" {
				raw["plan_type"] = planType
			}
			files = append(files, cpaauthfiles.File{
				Name: name, Provider: "codex", AuthIndex: authIndex, Raw: raw,
			})
			usedPercent := 10.0
			results = append(results, store.CodexInspectionResult{
				FileName: name, Provider: "codex", AuthIndex: authIndex,
				Action: "keep", Status: "active", UsedPercent: &usedPercent,
			})
		}
	}
	add("plus", 11)
	add("team", 3)
	add("", 7)
	// This account is still schedulable, but it needs attention and must not be
	// counted in either the top availability number or the live plan cards.
	files = append(files, cpaauthfiles.File{
		Name: "broken.json", Provider: "codex", AuthIndex: "broken",
		Raw: map[string]any{
			"status": "active", "plan_type": "plus",
			"cpamp_import": map[string]any{"platform_id": "legacy"},
		},
	})
	results = append(results, store.CodexInspectionResult{
		FileName: "broken.json", Provider: "codex", AuthIndex: "broken",
		Action: "reauth", Status: "error",
	})

	stats := accountPoolStatsFromFilesAndInspection(files, results)
	summary := accountPoolSummaryFromStats(stats, time.Now())
	summary.Plans = accountPoolPlanSummaries(stats, store.ManagerSupplyConfig{}, nil)
	if summary.Normal != 21 || summary.NeedsAttention != 1 {
		t.Fatalf("live account summary = %#v", summary)
	}
	counts := make(map[string]int, len(summary.Plans))
	planTotal := 0
	for _, plan := range summary.Plans {
		counts[plan.Key] = plan.AccountCount
		planTotal += plan.AccountCount
	}
	if counts["legacy:plus"] != 11 || counts["legacy:team"] != 3 ||
		counts["legacy:unknown"] != 7 || planTotal != summary.Normal {
		t.Fatalf("live plan summaries = %#v; available=%d", summary.Plans, summary.Normal)
	}
}

func TestAccountPoolCredentialSummariesPublishExactSharedBuckets(t *testing.T) {
	files := []cpaauthfiles.File{
		{Name: "normal.json", Provider: "codex", AuthIndex: "normal", AccountID: "account-normal", Raw: map[string]any{"status": "active"}},
		{Name: "risk.json", Provider: "codex", AuthIndex: "risk", Raw: map[string]any{"status": "active"}},
		{Name: "disabled.json", Provider: "codex", AuthIndex: "disabled", Disabled: true, Raw: map[string]any{"status": "disabled"}},
		{Name: "claude.json", Provider: "claude", Raw: map[string]any{"status": "active"}},
	}
	stats := accountPoolStatsFromFilesAndInspection(files, []store.CodexInspectionResult{
		{FileName: "normal.json", Provider: "codex", AuthIndex: "normal", Action: "keep", UsedPercent: float64Ptr(10)},
		{FileName: "risk.json", Provider: "codex", AuthIndex: "risk", Action: "keep", UsedPercent: float64Ptr(95)},
	})

	items := accountPoolCredentialSummaries(stats)
	if len(items) != 3 {
		t.Fatalf("credential summaries = %#v", items)
	}
	byName := make(map[string]AccountPoolCredentialSummary, len(items))
	for _, item := range items {
		byName[item.AuthFileName] = item
	}
	if byName["normal.json"].Bucket != "normal" || byName["normal.json"].AccountID != "account-normal" ||
		!byName["normal.json"].Schedulable || byName["risk.json"].Bucket != "quota_risk" ||
		!byName["risk.json"].Schedulable || byName["disabled.json"].Bucket != "disabled" ||
		byName["disabled.json"].Schedulable {
		t.Fatalf("credential summary buckets = %#v", items)
	}
}

func TestAccountPoolStatsParsesConcurrencyAliasesWithoutConstrainingUnlimitedAccounts(t *testing.T) {
	files := []cpaauthfiles.File{
		{Name: "finite.json", Provider: "codex", Raw: map[string]any{"max_concurrency": 2}},
		{Name: "explicit-unlimited.json", Provider: "codex", Raw: map[string]any{"maxConcurrency": 0}},
		{Name: "missing.json", Provider: "codex", Raw: map[string]any{}},
		{Name: "disabled.json", Provider: "codex", Disabled: true, Raw: map[string]any{"concurrency_limit": 50}},
	}
	stats := accountPoolStatsFromFiles(files)
	if stats.schedulable != 3 || stats.concurrencyLimited != 1 || stats.concurrencyFiniteSlots != 2 ||
		stats.concurrencyUnlimited != 2 || stats.concurrencyMissing != 1 {
		t.Fatalf("concurrency pool statistics = %#v", stats)
	}
	resource := SmartResource{
		CurrentCapacityRCU:        1_000,
		RequiredConcurrencySlots:  20,
		ConcurrencyDemandObserved: true,
	}
	applyAccountPoolStats(&resource, stats)
	if !resource.ConcurrencyUnlimited || resource.ConcurrencyLimited || resource.ConcurrencyAccountDeficit != 0 ||
		resource.ConcurrencyCoverage != 100 || resource.ConcurrencyEffectiveCapacityRCU != 1_000 {
		t.Fatalf("unlimited concurrency must not constrain capacity: %#v", resource)
	}
}

func TestAccountPoolStatsPublishesFiniteConcurrencyShortage(t *testing.T) {
	files := []cpaauthfiles.File{
		{Name: "one.json", Provider: "codex", Raw: map[string]any{"concurrency": 1}},
		{Name: "two.json", Provider: "codex", Raw: map[string]any{"concurrencyLimit": "2"}},
	}
	stats := accountPoolStatsFromFiles(files)
	resource := SmartResource{
		CurrentCapacityRCU:        1_000,
		RequiredConcurrencySlots:  8,
		ConcurrencyDemandObserved: true,
	}
	applyAccountPoolStats(&resource, stats)
	if !resource.ConcurrencyLimited || resource.ConcurrencyUnlimited || resource.ConcurrencyFiniteSlots != 3 ||
		resource.ConcurrencyHeadroomSlots != -5 || resource.ConcurrencyAccountDeficit != 3 ||
		resource.ConcurrencyCoverage != 37.5 || resource.ConcurrencyEffectiveCapacityRCU != 375 {
		t.Fatalf("finite concurrency shortage = %#v", resource)
	}
}

func TestAccountPoolStatsMatchesCredentialStatusBuckets(t *testing.T) {
	remainingNormal := 10.0
	remainingRisk := 90.0
	files := []cpaauthfiles.File{
		{Name: "normal.json", Provider: "codex", AuthIndex: "normal", AccountSnapshot: "normal@example.com", Raw: map[string]any{"status": "active"}},
		{Name: "attention.json", Provider: "codex", AuthIndex: "attention", AccountSnapshot: "attention@example.com", Raw: map[string]any{"status": "active"}},
		{Name: "risk.json", Provider: "codex", AuthIndex: "risk", AccountSnapshot: "risk@example.com", Raw: map[string]any{"status": "active"}},
		{Name: "unconfirmed.json", Provider: "codex", AuthIndex: "unconfirmed", AccountSnapshot: "unconfirmed@example.com", Raw: map[string]any{"status": "active"}},
		{Name: "disabled.json", Provider: "codex", AuthIndex: "disabled", Disabled: true, Raw: map[string]any{"status": "disabled", "disabled": true}},
	}
	results := []store.CodexInspectionResult{
		// CPA may omit account_id while the inspection result retains it. The
		// stable auth_index must still join the same credential.
		{FileName: "normal.json", Provider: "codex", AuthIndex: "normal", AccountID: "acct-normal", AccountSnapshot: "normal@example.com", Action: "keep", Status: "active", UsedPercent: &remainingNormal},
		{FileName: "attention.json", Provider: "codex", AuthIndex: "attention", AccountID: "acct-attention", AccountSnapshot: "attention@example.com", Action: "reauth", Status: "active", StatusCode: intPtr(http.StatusUnauthorized)},
		{FileName: "risk.json", Provider: "codex", AuthIndex: "risk", AccountID: "acct-risk", AccountSnapshot: "risk@example.com", Action: "keep", Status: "active", UsedPercent: &remainingRisk},
	}
	stats := accountPoolStatsFromFilesAndInspection(files, results)
	resource := SmartResource{}
	applyAccountPoolStats(&resource, stats)
	if resource.TotalAccounts != 4 || resource.EnabledAccounts != 4 || resource.DisabledAccounts != 1 ||
		resource.NormalAccounts != 2 || resource.NeedsAttentionAccounts != 1 || resource.QuotaRiskAccounts != 1 ||
		resource.UnconfirmedAccounts != 0 || resource.AtRiskAccounts != 2 {
		t.Fatalf("operator account buckets = %#v", resource)
	}
	if resource.NormalAccounts+resource.AtRiskAccounts != resource.TotalAccounts {
		t.Fatalf("account bucket identity does not hold: %#v", resource)
	}
}

func TestAccountPoolStatsKeepsRateLimitedCredentialsAvailable(t *testing.T) {
	files := []cpaauthfiles.File{
		{
			Name: "rate-limit.json", Provider: "codex", AuthIndex: "rate-limit",
			Raw: map[string]any{
				"status":         "error",
				"status_message": `{"detail":"Rate limit exceeded"}`,
				"recent_requests": []any{
					map[string]any{"success": 8, "failed": 2},
				},
			},
		},
		{
			Name: "http-429.json", Provider: "codex", AuthIndex: "http-429",
			Raw: map[string]any{
				"status":         "error",
				"status_message": "HTTP 429 too many requests",
			},
		},
		{
			Name: "invalid-grant.json", Provider: "codex", AuthIndex: "invalid-grant",
			Raw: map[string]any{
				"status":         "error",
				"status_message": "invalid_grant login_required",
			},
		},
	}
	results := []store.CodexInspectionResult{
		{FileName: "rate-limit.json", Provider: "codex", AuthIndex: "rate-limit", Action: "keep"},
		{FileName: "http-429.json", Provider: "codex", AuthIndex: "http-429", Action: "keep"},
		{FileName: "invalid-grant.json", Provider: "codex", AuthIndex: "invalid-grant", Action: "keep"},
	}

	stats := accountPoolStatsFromFilesAndInspection(files, results)
	if stats.schedulable != 2 || stats.normal != 2 || stats.needsAttention != 1 ||
		stats.quotaRisk != 0 || stats.unconfirmed != 0 {
		t.Fatalf("rate-limit account buckets = %#v", stats)
	}
	if stats.normal+stats.needsAttention+stats.quotaRisk+stats.unconfirmed+(stats.total-stats.enabled) != stats.total {
		t.Fatalf("rate-limit account identity does not hold: %#v", stats)
	}
}

func TestEnabledPoolCapacitySplitUsesCredentialIdentity(t *testing.T) {
	resource := SmartResource{
		CurrentCapacityRCU:     1_000,
		TimeLimitedCapacityRCU: 1_000,
		TotalCapacityRCU:       1_000,
		capacityItems: []smartCapacityItem{
			{
				credentialKey:     operatorCredentialKey("normal.json", "normal"),
				fileKey:           operatorFileCredentialKey("normal.json"),
				usableCapacityRCU: 100,
			},
			{
				credentialKey:     operatorCredentialKey("attention.json", "attention"),
				fileKey:           operatorFileCredentialKey("attention.json"),
				usableCapacityRCU: 900,
			},
		},
	}
	stats := accountPoolStats{
		total: 2, enabled: 2, schedulable: 2, normal: 1, needsAttention: 1,
		classificationObserved: true, liveObserved: true,
		bucketByCredential: map[string]operatorAccountBucket{
			operatorCredentialKey("normal.json", "normal"):       operatorAccountNormal,
			operatorCredentialKey("attention.json", "attention"): operatorAccountNeedsAttention,
		},
	}
	applyAccountPoolStats(&resource, stats)
	if resource.TotalAccounts != 2 || resource.AvailableAccounts != 1 || resource.FrozenAccounts != 1 {
		t.Fatalf("account split counts = %#v", resource)
	}
	if resource.TotalCapacityRCU != 1_000 || resource.AvailableCapacityRCU != 100 || resource.FrozenCapacityRCU != 900 {
		t.Fatalf("credential capacity split = %#v", resource)
	}
}

func TestNormalLiveCredentialMissingFromInspectionUsesPlanFallbackCapacity(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	service := New(nil, nil)
	unit := smartProductUnitCapacity("oauth_7d")
	existingCapacity := smartTokenMillionToRCU(30, unit)
	resource := SmartResource{
		CapacitySource:         smartCapacitySourceInspection,
		CapacitySnapshotAtMS:   now.UnixMilli(),
		UnitCapacityRCU:        unit,
		RawCapacityRCU:         existingCapacity,
		CurrentCapacityRCU:     existingCapacity,
		TimeLimitedCapacityRCU: existingCapacity,
		TotalCapacityRCU:       existingCapacity,
		HealthyAccounts:        1,
		capacityItems: []smartCapacityItem{{
			credentialKey:     operatorCredentialKey("known.json", "known"),
			fileKey:           operatorFileCredentialKey("known.json"),
			capacityRCU:       existingCapacity,
			usableCapacityRCU: existingCapacity,
			remainingMinutes:  float64(smartAccountLifetimeMinutes()),
		}},
		AccountQuotaPlanEstimates: []SmartQuotaPlanEstimate{{
			PlanType: "team", AccountCount: 1, FallbackM: 60, AdoptedM: 60, Source: smartQuotaEstimateSourceDefault,
		}},
	}
	known := cpaauthfiles.File{
		Name: "known.json", AuthIndex: "known", Provider: "codex",
		Raw: map[string]any{"status": "ready", "plan_type": "team"},
	}
	missing := cpaauthfiles.File{
		Name: "missing.json", AuthIndex: "missing", Provider: "codex",
		Raw: map[string]any{"status": "ready", "plan_type": "team"},
	}
	stats := accountPoolStats{
		files: []cpaauthfiles.File{known, missing},
		total: 2, enabled: 2, schedulable: 2, normal: 2,
		classificationObserved: true, liveObserved: true,
		bucketByCredential: map[string]operatorAccountBucket{
			operatorCredentialKey("known.json", "known"):     operatorAccountNormal,
			operatorCredentialKey("missing.json", "missing"): operatorAccountNormal,
		},
		normalRemainingByCredential: map[string]float64{
			operatorCredentialKey("known.json", "known"):     0.5,
			operatorCredentialKey("missing.json", "missing"): 0.75,
		},
	}
	applyAccountPoolStats(&resource, stats)
	if !service.reconcileSmartNormalCapacityFloor(store.ManagerSupplyConfig{Product: "oauth_7d"}, &resource, stats, now) {
		t.Fatal("missing normal credential did not add fallback capacity")
	}
	if resource.CurrentCapacityTokenM != 75 || resource.RawCapacityTokenM != 75 ||
		resource.AvailableCapacityTokenM != 75 || resource.HealthyAccounts != 2 {
		t.Fatalf("normal capacity floor = %#v", resource)
	}
	if got := resource.AccountQuotaPlanEstimates[0].AccountCount; got != 2 {
		t.Fatalf("team account count = %d, want 2", got)
	}
}

func TestNormalLiveTeamPoolReconcilesAllSixteenAccountsIntoRawCapacity(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	service := New(nil, nil)
	unit := smartProductUnitCapacity("oauth_7d")
	resource := SmartResource{
		CapacitySource:       smartCapacitySourceInspection,
		CapacitySnapshotAtMS: now.UnixMilli(),
		UnitCapacityRCU:      unit,
		HealthyAccounts:      10,
		AccountQuotaPlanEstimates: []SmartQuotaPlanEstimate{{
			PlanType: "team", AccountCount: 10, FallbackM: 60, AdoptedM: 60, Source: smartQuotaEstimateSourceDefault,
		}},
	}
	stats := accountPoolStats{
		total: 16, enabled: 16, schedulable: 16, normal: 16,
		classificationObserved:      true,
		liveObserved:                true,
		bucketByCredential:          make(map[string]operatorAccountBucket, 16),
		normalRemainingByCredential: make(map[string]float64, 16),
	}
	for index := 0; index < 16; index++ {
		name := fmt.Sprintf("team-%02d.json", index)
		authIndex := fmt.Sprintf("team-%02d", index)
		file := cpaauthfiles.File{
			Name: name, AuthIndex: authIndex, Provider: "codex",
			Raw: map[string]any{"status": "ready", "plan_type": "team"},
		}
		stats.files = append(stats.files, file)
		key := operatorCredentialKey(name, authIndex)
		stats.bucketByCredential[key] = operatorAccountNormal
		stats.normalRemainingByCredential[key] = 1
		if index < 10 {
			// The completed inspection only saw 35M remaining on its ten
			// credentials. Six newer live credentials must add their full 60M
			// Team fallback instead of leaving the pool at the stale ten-account
			// total shown by the dashboard.
			capacity := smartTokenMillionToRCU(35, unit)
			resource.RawCapacityRCU += capacity
			resource.capacityItems = append(resource.capacityItems, smartCapacityItem{
				credentialKey: key,
				fileKey:       operatorFileCredentialKey(name),
				capacityRCU:   capacity,
			})
		}
	}
	resource.RawCapacityRCU = round2(resource.RawCapacityRCU)
	applySmartExpiryCapacity(&resource, resource.capacityItems, 0, now)
	applyAccountPoolStats(&resource, stats)

	if !service.reconcileSmartNormalCapacityFloor(store.ManagerSupplyConfig{Product: "oauth_7d"}, &resource, stats, now) {
		t.Fatal("six missing Team credentials did not add fallback capacity")
	}
	if resource.RawCapacityTokenM != 710 || resource.CurrentCapacityTokenM != 710 ||
		resource.HealthyAccounts != 16 || resource.AccountQuotaPlanEstimates[0].AccountCount != 16 {
		t.Fatalf("sixteen-account Team capacity = %#v", resource)
	}
}

func TestNormalLiveCredentialUsesItsSupplierQuotaPolicy(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	service := New(nil, nil)
	unit := smartProductUnitCapacity("oauth_7d")
	cfg := store.ManagerSupplyConfig{
		Product: "oauth_7d",
		Platforms: []store.ManagerSupplyPlatformConfig{
			{ID: "small", Type: managerconfigsvc.SupplyPlatformLegacy, Product: "oauth_7d", QuotaEstimationPolicies: map[string]store.ManagerSupplyQuotaEstimationPolicy{
				"team": {FallbackM: 40},
			}},
			{ID: "large", Type: managerconfigsvc.SupplyPlatformBugTeam, Product: "team_1h", QuotaEstimationPolicies: map[string]store.ManagerSupplyQuotaEstimationPolicy{
				"team": {FallbackM: 240},
			}},
		},
	}
	file := cpaauthfiles.File{
		Name: "large-team.json", AuthIndex: "large-team", Provider: "codex",
		Raw: map[string]any{"status": "ready", "plan_type": "team"},
	}
	key := operatorCredentialKey(file.Name, file.AuthIndex)
	resource := SmartResource{
		CapacitySource:       smartCapacitySourceInspection,
		CapacitySnapshotAtMS: now.UnixMilli(),
		UnitCapacityRCU:      unit,
		quotaSupplierByFile:  map[string]string{file.Name: "large"},
		AccountQuotaPlanEstimates: []SmartQuotaPlanEstimate{
			{SupplierID: "small", PlanType: "team", AccountCount: 1, FallbackM: 40, AdoptedM: 40, Source: smartQuotaEstimateSourceDefault},
			{SupplierID: "large", PlanType: "team", AccountCount: 0, FallbackM: 240, AdoptedM: 240, Source: smartQuotaEstimateSourceDefault},
		},
	}
	stats := accountPoolStats{
		files: []cpaauthfiles.File{file}, total: 1, enabled: 1, schedulable: 1, normal: 1,
		classificationObserved: true, liveObserved: true,
		bucketByCredential:          map[string]operatorAccountBucket{key: operatorAccountNormal},
		normalRemainingByCredential: map[string]float64{key: 1},
	}
	applyAccountPoolStats(&resource, stats)

	if !service.reconcileSmartNormalCapacityFloor(cfg, &resource, stats, now) {
		t.Fatal("supplier-scoped normal credential did not add capacity")
	}
	if resource.RawCapacityTokenM != 240 || resource.AccountQuotaPlanEstimates[0].AccountCount != 1 ||
		resource.AccountQuotaPlanEstimates[1].AccountCount != 1 {
		t.Fatalf("supplier-scoped capacity = %#v", resource)
	}
}

func TestAvailableCapacityEmergencyBuysOnlyMinimumCrossingQuantity(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Product: "oauth_30d", Strategy: managerconfigsvc.SupplyStrategyStrongSupply,
		HealthyMinutesTarget: 60, WarningMinutes: 20, CriticalMinutes: 5,
		CriticalAvailableAccounts: 2, HealthyAvailableAccounts: 10,
		ReplenishBatchSize: 10, PrelockMaxQuantity: 10, NewAccountConfidence: 1,
	}
	resource := defaultSmartResource(cfg)
	resource.SnapshotFresh = true
	resource.CapacitySource = smartCapacitySourceInspection
	resource.TotalAccounts = 14
	resource.EnabledAccounts = 14
	resource.AvailableAccounts = 3
	resource.SchedulableAccounts = 14
	resource.HealthyAccounts = 14
	resource.CurrentCapacityRCU = 1_000
	resource.TimeLimitedCapacityRCU = 1_000
	resource.TotalCapacityRCU = 1_000
	resource.AvailableCapacityRCU = 20
	resource.FrozenCapacityRCU = 980
	resource.ConsumeRCUPerMinute = 10

	recalculateSmartResourceCapacityPlan(cfg, &resource)
	if resource.HealthLevel != smartHealthHealthy {
		t.Fatalf("total capacity should remain healthy before available-runway guard: %#v", resource)
	}
	New(nil, nil).reconcileSmartAccountPoolGuard(cfg, &resource)
	if resource.DecisionReason != "available_capacity_critical" || resource.SuggestedQuantity != 1 ||
		resource.SuggestedAction != smartActionEmergencyReplenish {
		t.Fatalf("available emergency should buy one estimated account: %#v", resource)
	}
}

func TestAvailableCapacityEmergencyCountsPrelockedCapacityBeforeBuying(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Product: "oauth_30d", Strategy: managerconfigsvc.SupplyStrategyStrongSupply,
		HealthyMinutesTarget: 60, WarningMinutes: 20, CriticalMinutes: 5,
		CriticalAvailableAccounts: 2, HealthyAvailableAccounts: 10,
		ReplenishBatchSize: 10, PrelockMaxQuantity: 10, NewAccountConfidence: 1,
	}
	resource := defaultSmartResource(cfg)
	resource.TotalAccounts = 14
	resource.AvailableAccounts = 2
	resource.CurrentCapacityRCU = 1_000
	resource.AvailableCapacityRCU = 20
	resource.FrozenCapacityRCU = 980
	resource.TotalCapacityRCU = 1_000
	resource.ConsumeRCUPerMinute = 10
	resource.PrelockedCapacityRCU = smartEstimatedNewAccountCapacityForResource(cfg, resource)
	resource.EmergencyShortage = true
	resource.EmergencyReason = "available_capacity_critical"
	resource.DecisionReason = "available_capacity_critical"

	if got := New(nil, nil).smartSuggestedCreateQuantity(cfg, resource); got != 0 {
		t.Fatalf("prelocked capacity should cross both emergency lines without another order: got %d", got)
	}
}

func TestAccountPoolStatsUsesNewerHeaderEvidenceOverOlderInspectionFailures(t *testing.T) {
	now := time.UnixMilli(10_000)
	files := []cpaauthfiles.File{
		{Name: "normal.json", Provider: "codex", AuthIndex: "normal", Raw: map[string]any{"status": "active"}},
		{Name: "risk.json", Provider: "codex", AuthIndex: "risk", Raw: map[string]any{"status": "active"}},
		{Name: "attention.json", Provider: "codex", AuthIndex: "attention", Raw: map[string]any{"status": "active"}},
		{Name: "unconfirmed.json", Provider: "codex", AuthIndex: "unconfirmed", Raw: map[string]any{"status": "active"}},
	}
	results := []store.CodexInspectionResult{
		{FileName: "normal.json", Provider: "codex", AuthIndex: "normal", Action: "reauth", CreatedAtMS: 1_000},
		{FileName: "risk.json", Provider: "codex", AuthIndex: "risk", Action: "reauth", CreatedAtMS: 1_000},
		{FileName: "attention.json", Provider: "codex", AuthIndex: "attention", Action: "keep", CreatedAtMS: 1_000},
		{FileName: "unconfirmed.json", Provider: "codex", AuthIndex: "unconfirmed", Action: "reauth", CreatedAtMS: 1_000},
	}
	normalUsed := 10.0
	riskUsed := 90.0
	headers := []store.HeaderSnapshot{
		{
			ID: 1, EventHash: "normal", TimestampMS: 2_000,
			AuthFileSnapshot: "normal.json", AuthIndex: "normal",
			ResponseMetadata: &usage.ResponseHeaderMetadata{Quota: &usage.HeaderQuotaMetadata{
				Primary: &usage.HeaderQuotaWindow{UsedPercent: &normalUsed},
			}},
		},
		{
			ID: 2, EventHash: "risk", TimestampMS: 2_000,
			AuthFileSnapshot: "risk.json", AuthIndex: "risk",
			ResponseMetadata: &usage.ResponseHeaderMetadata{Quota: &usage.HeaderQuotaMetadata{
				Secondary: &usage.HeaderQuotaWindow{UsedPercent: &riskUsed},
			}},
		},
		{
			ID: 3, EventHash: "attention", TimestampMS: 2_000,
			AuthFileSnapshot: "attention.json", AuthIndex: "attention",
			HeaderErrorKind: "auth", HeaderErrorCode: "token_revoked",
		},
		{
			ID: 4, EventHash: "unconfirmed", TimestampMS: 2_000,
			AuthFileSnapshot: "unconfirmed.json", AuthIndex: "unconfirmed",
			HeaderTraceID: "trace-only",
		},
	}

	stats := accountPoolStatsFromFilesAndCurrentEvidence(
		files,
		results,
		headers,
		model.CodexInspectionTriggerManual,
		now,
	)
	if stats.normal != 1 || stats.quotaRisk != 1 || stats.needsAttention != 1 || stats.unconfirmed != 1 {
		t.Fatalf("header-overlaid operator account buckets = %#v", stats)
	}
}

func TestLatestOperatorInspectionEvidenceSkipsNewerSupplySnapshot(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "operator-inspection.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now().UnixMilli()
	settings := model.DefaultCodexInspectionConfig()
	settings.TargetTypes = []string{model.CodexInspectionTargetCodex}
	settings.TargetType = model.CodexInspectionTargetCodex

	scheduled, err := st.CreateCodexInspectionRun(context.Background(), store.CodexInspectionRun{
		TriggerType:   model.CodexInspectionTriggerScheduled,
		TriggerKey:    "scheduled",
		Status:        model.CodexInspectionStatusCompleted,
		StartedAtMS:   now - 2_000,
		FinishedAtMS:  now - 1_500,
		ProbeSetCount: 1,
		SampledCount:  1,
		Settings:      settings,
	})
	if err != nil {
		t.Fatalf("create scheduled run: %v", err)
	}
	if _, err := st.InsertCodexInspectionResult(context.Background(), store.CodexInspectionResult{
		RunID: scheduled.ID, AccountKey: "dead", FileName: "dead.json", Provider: "codex",
		AuthIndex: "dead", Action: "delete", StatusCode: intPtr(http.StatusPaymentRequired),
	}); err != nil {
		t.Fatalf("insert scheduled result: %v", err)
	}

	supplyRun, err := st.CreateCodexInspectionRun(context.Background(), store.CodexInspectionRun{
		TriggerType:   model.CodexInspectionTriggerSupplySnapshot,
		TriggerKey:    "supply",
		Status:        model.CodexInspectionStatusCompleted,
		StartedAtMS:   now - 1_000,
		FinishedAtMS:  now - 500,
		ProbeSetCount: 1,
		SampledCount:  1,
		Settings:      settings,
	})
	if err != nil {
		t.Fatalf("create supply run: %v", err)
	}
	usedPercent := 10.0
	if _, err := st.InsertCodexInspectionResult(context.Background(), store.CodexInspectionResult{
		RunID: supplyRun.ID, AccountKey: "dead", FileName: "dead.json", Provider: "codex",
		AuthIndex: "dead", Action: "keep", UsedPercent: &usedPercent,
	}); err != nil {
		t.Fatalf("insert supply result: %v", err)
	}

	results, triggerType, err := New(st, nil).loadLatestOperatorInspectionEvidence(context.Background())
	if err != nil {
		t.Fatalf("load operator evidence: %v", err)
	}
	if triggerType != model.CodexInspectionTriggerScheduled || len(results) != 1 || results[0].Action != "delete" {
		t.Fatalf("operator evidence trigger=%q results=%#v", triggerType, results)
	}
}

func TestAccountPoolStatsKeepsNewerInspectionOverOlderHeaderEvidence(t *testing.T) {
	usedPercent := 10.0
	files := []cpaauthfiles.File{{
		Name: "failed-again.json", Provider: "codex", AuthIndex: "failed-again",
		Raw: map[string]any{"status": "active"},
	}}
	results := []store.CodexInspectionResult{{
		FileName: "failed-again.json", Provider: "codex", AuthIndex: "failed-again",
		Action: "reauth", CreatedAtMS: 3_000,
	}}
	headers := []store.HeaderSnapshot{{
		ID: 1, EventHash: "older-success", TimestampMS: 2_000,
		AuthFileSnapshot: "failed-again.json", AuthIndex: "failed-again",
		ResponseMetadata: &usage.ResponseHeaderMetadata{Quota: &usage.HeaderQuotaMetadata{
			Primary: &usage.HeaderQuotaWindow{UsedPercent: &usedPercent},
		}},
	}}

	stats := accountPoolStatsFromFilesAndCurrentEvidence(
		files,
		results,
		headers,
		model.CodexInspectionTriggerManual,
		time.UnixMilli(10_000),
	)
	if stats.normal != 0 || stats.needsAttention != 1 || stats.quotaRisk != 0 || stats.unconfirmed != 0 {
		t.Fatalf("newer inspection must remain authoritative: %#v", stats)
	}
}

func TestAccountPoolStatsDoesNotLetSupplySnapshotOverrideLiveHeaderEvidence(t *testing.T) {
	now := time.UnixMilli(10_000)
	usedPercent := 35.0
	files := []cpaauthfiles.File{
		{Name: "healthy.json", Provider: "codex", AuthIndex: "healthy", Raw: map[string]any{"status": "active"}},
		{Name: "unknown.json", Provider: "codex", AuthIndex: "unknown", Raw: map[string]any{"status": "active"}},
	}
	results := []store.CodexInspectionResult{
		{FileName: "healthy.json", Provider: "codex", AuthIndex: "healthy", Action: "reauth", CreatedAtMS: 9_000},
		{FileName: "unknown.json", Provider: "codex", AuthIndex: "unknown", Action: "delete", CreatedAtMS: 9_000},
	}
	headers := []store.HeaderSnapshot{{
		ID: 1, EventHash: "real-traffic", TimestampMS: 2_000,
		AuthFileSnapshot: "healthy.json", AuthIndex: "healthy",
		ResponseMetadata: &usage.ResponseHeaderMetadata{Quota: &usage.HeaderQuotaMetadata{
			Primary: &usage.HeaderQuotaWindow{UsedPercent: &usedPercent},
		}},
	}}

	stats := accountPoolStatsFromFilesAndCurrentEvidence(
		files,
		results,
		headers,
		model.CodexInspectionTriggerSupplySnapshot,
		now,
	)
	if stats.normal != 1 || stats.needsAttention != 0 || stats.quotaRisk != 0 || stats.unconfirmed != 1 {
		t.Fatalf("supply snapshot leaked into operator buckets: %#v", stats)
	}
}

func TestOperatorHeaderAuthErrorWinsOverUnrelatedQuotaPercent(t *testing.T) {
	usedPercent := 90.0
	got := classifyOperatorAccountFromHeader(store.HeaderSnapshot{
		HeaderErrorKind: "auth",
		HeaderErrorCode: "token_revoked",
		ResponseMetadata: &usage.ResponseHeaderMetadata{Quota: &usage.HeaderQuotaMetadata{
			Primary: &usage.HeaderQuotaWindow{UsedPercent: &usedPercent},
		}},
	})
	if got != operatorAccountNeedsAttention {
		t.Fatalf("auth error bucket = %v, want needs attention", got)
	}
}

func TestOperatorInspectionQuotaActionRemainsQuotaRisk(t *testing.T) {
	file := cpaauthfiles.File{
		Name: "quota-risk.json", Provider: "codex", AuthIndex: "quota-risk",
		Raw: map[string]any{"status": "active"},
	}
	usedPercent := 100.0
	result := store.CodexInspectionResult{
		FileName: "quota-risk.json", Provider: "codex", AuthIndex: "quota-risk",
		Action: "disable", Status: "active", StatusCode: intPtr(http.StatusOK), IsQuota: true,
		UsedPercent: &usedPercent,
	}
	if got := classifyOperatorAccount(file, result); got != operatorAccountQuotaRisk {
		t.Fatalf("quota action bucket = %v, want quota risk", got)
	}

	stats := accountPoolStatsFromFilesAndInspection([]cpaauthfiles.File{file}, []store.CodexInspectionResult{result})
	if stats.quotaRisk != 1 || stats.needsAttention != 0 || stats.operatorUsable != 1 || stats.schedulable != 1 {
		t.Fatalf("quota action pool statistics = %#v", stats)
	}
}

func TestOperatorHeaderTemporaryRateLimitKeepsCredentialAvailable(t *testing.T) {
	tests := []struct {
		name     string
		kind     string
		code     string
		want     operatorAccountBucket
		usedRate *float64
	}{
		{name: "retry after", kind: "rate_limit", code: "retry_after", want: operatorAccountNormal},
		{name: "too many requests", code: "too_many_requests", want: operatorAccountNormal},
		{name: "http 429", code: "HTTP 429", want: operatorAccountNormal},
		{name: "cooldown", kind: "cooldown", want: operatorAccountNormal},
		{name: "low quota stays visible as quota risk", kind: "rate_limit", code: "retry_after", want: operatorAccountQuotaRisk, usedRate: float64Ptr(90)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := store.HeaderSnapshot{
				HeaderErrorKind: tt.kind,
				HeaderErrorCode: tt.code,
			}
			if tt.usedRate != nil {
				snapshot.ResponseMetadata = &usage.ResponseHeaderMetadata{Quota: &usage.HeaderQuotaMetadata{
					Primary: &usage.HeaderQuotaWindow{UsedPercent: tt.usedRate},
				}}
			}
			if got := classifyOperatorAccountFromHeader(snapshot); got != tt.want {
				t.Fatalf("temporary header limit bucket = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOperatorHeaderQuotaExhaustionRemainsQuotaRisk(t *testing.T) {
	for _, code := range []string{"usage_limit_reached", "quota_exhausted", "insufficient_quota"} {
		t.Run(code, func(t *testing.T) {
			got := classifyOperatorAccountFromHeader(store.HeaderSnapshot{
				HeaderErrorKind: "rate_limit",
				HeaderErrorCode: code,
			})
			if got != operatorAccountQuotaRisk {
				t.Fatalf("quota exhaustion bucket = %v, want quota risk", got)
			}
		})
	}
}

func TestAccountPoolStatsKeepsTemporaryHeaderRateLimitInAvailablePool(t *testing.T) {
	now := time.UnixMilli(10_000)
	files := []cpaauthfiles.File{{
		Name: "replenished.json", Provider: "codex", AuthIndex: "replenished",
		Raw: map[string]any{"status": "active"},
	}}
	headers := []store.HeaderSnapshot{{
		ID: 1, EventHash: "temporary-429", TimestampMS: 9_000,
		AuthFileSnapshot: "replenished.json", AuthIndex: "replenished",
		HeaderErrorKind: "rate_limit", HeaderErrorCode: "retry_after",
	}}

	stats := accountPoolStatsFromFilesAndCurrentEvidence(
		files,
		nil,
		headers,
		model.CodexInspectionTriggerSupplySnapshot,
		now,
	)
	if stats.normal != 1 || stats.needsAttention != 0 || stats.operatorUsable != 1 || stats.schedulable != 1 {
		t.Fatalf("temporary rate-limit pool statistics = %#v", stats)
	}
	items := accountPoolCredentialSummaries(stats)
	if len(items) != 1 || items[0].Bucket != "normal" || !items[0].Schedulable ||
		!items[0].TemporaryLimited || items[0].TemporaryLimitKind != "rate_limit" ||
		items[0].TemporaryLimitCode != "retry_after" {
		t.Fatalf("temporary rate-limit credential summary = %#v", items)
	}
}

func TestOperatorHeaderSnapshotExpiresRecoveredRetryAfter(t *testing.T) {
	now := time.UnixMilli(10_000)
	snapshot := store.HeaderSnapshot{
		ResponseMetadata: &usage.ResponseHeaderMetadata{Errors: &usage.HeaderErrorMetadata{
			Kind:                  "rate_limit",
			Code:                  "retry_after",
			RetryAfterRecoverAtMS: 9_000,
		}},
	}
	if !operatorHeaderSnapshotExpired(snapshot, now) {
		t.Fatal("recovered retry-after snapshot remained active")
	}
}

func TestAccountPoolStatsUsesUnconfirmedFallbackWhenInspectionIdentityIsMissing(t *testing.T) {
	files := []cpaauthfiles.File{
		{Name: "live.json", Provider: "codex", AuthIndex: "live", Raw: map[string]any{"status": "active"}},
	}
	stats := accountPoolStatsFromFiles(files)
	stats.classificationObserved = true
	stats.normal = 0
	stats.needsAttention = 0
	stats.quotaRisk = 0
	stats.unconfirmed = stats.enabled
	resource := SmartResource{}
	applyAccountPoolStats(&resource, stats)
	if resource.TotalAccounts != 1 || resource.EnabledAccounts != 1 || resource.DisabledAccounts != 0 ||
		resource.NormalAccounts != 0 || resource.NeedsAttentionAccounts != 0 || resource.QuotaRiskAccounts != 0 ||
		resource.UnconfirmedAccounts != 1 || resource.AtRiskAccounts != 1 {
		t.Fatalf("unconfirmed fallback statistics = %#v", resource)
	}
}

func TestAccountPoolStatsDoesNotMatchAmbiguousFileOnlyInspection(t *testing.T) {
	files := []cpaauthfiles.File{
		{Name: "shared.json", Provider: "codex", AuthIndex: "one", AccountSnapshot: "one@example.com"},
		{Name: "shared.json", Provider: "codex", AuthIndex: "two", AccountSnapshot: "two@example.com"},
	}
	results := []store.CodexInspectionResult{{
		FileName: "shared.json", Provider: "codex", Action: "keep", Status: "active",
	}}
	stats := accountPoolStatsFromFilesAndInspection(files, results)
	if stats.normal != 0 || stats.quotaRisk != 0 || stats.needsAttention != 0 || stats.unconfirmed != 2 {
		t.Fatalf("ambiguous inspection classification = %#v", stats)
	}
}

func TestAccountPoolStatsJoinsInspectionByAuthIndexWhenCPAOmitsAccountID(t *testing.T) {
	remaining := 10.0
	files := []cpaauthfiles.File{{
		Name: "team.json", Provider: "codex", AuthIndex: "auth-team",
		AccountSnapshot: "team@example.com", Raw: map[string]any{"status": "active"},
	}}
	results := []store.CodexInspectionResult{{
		FileName: "team.json", Provider: "codex", AuthIndex: "auth-team",
		AccountID: "account-from-inspection", AccountSnapshot: "team@example.com",
		Action: "keep", Status: "active", UsedPercent: &remaining,
	}}
	stats := accountPoolStatsFromFilesAndInspection(files, results)
	if stats.normal != 1 || stats.unconfirmed != 0 || stats.needsAttention != 0 || stats.quotaRisk != 0 {
		t.Fatalf("auth-index identity join = %#v", stats)
	}
}

func TestSupplyAccountLeaseMetadataKeepsDeadlineAsRoutingHint(t *testing.T) {
	leaseExpiresAtMS := time.Now().Add(45 * time.Minute).UnixMilli()
	account := normalizedSupplyAccount{payload: []byte(`{"type":"codex","expires_at":4102444800}`)}
	account = withSupplyAccountLeaseMetadata(account, leaseExpiresAtMS)
	var metadata map[string]any
	if err := json.Unmarshal(account.payload, &metadata); err != nil {
		t.Fatalf("decode account metadata: %v", err)
	}
	if got := int64(numberField(metadata, "supply_lease_expires_at_ms")); got != leaseExpiresAtMS {
		t.Fatalf("supplier lease = %d, want %d", got, leaseExpiresAtMS)
	}
	remaining := smartAccountRemainingMinutes(metadata, time.Now(), smartAccountLifetimeMinutes())
	if remaining <= float64(smartAccountLifetimeMinutes()) {
		t.Fatalf("upstream expires_at must remain the capacity horizon, got %.2f minutes", remaining)
	}
}

func TestListAccountLeasesIncludesRecoveryReplacementProvenance(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "account-provenance.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now().Truncate(time.Second)
	orderID := "recovery-account-provenance"
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: orderID, SupplierID: "supplier-a", Product: "oauth_30d", Strategy: "recovery", Status: "completed",
	}); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := st.InsertSupplyImportItems(ctx, orderID, []store.SupplyImportItem{{
		ItemKey: "replacement", FileName: "replacement.json", ImportAction: "replace",
		ReplacedFileName: "expired.json", Status: "pending", LeaseExpiresAtMS: now.Add(time.Hour).UnixMilli(),
	}}); err != nil {
		t.Fatalf("insert import item: %v", err)
	}
	items, err := st.ListSupplyImportItemsByOrderIDs(ctx, []string{orderID})
	if err != nil || len(items) != 1 {
		t.Fatalf("list import items: items=%#v err=%v", items, err)
	}
	if err := st.MarkSupplyImportItemImported(ctx, items[0].ID, now.UnixMilli()); err != nil {
		t.Fatalf("mark imported: %v", err)
	}
	if _, err := st.UpsertSupplyRecoveries(ctx, []store.SupplyRecovery{{
		RecoveryID: "recovery-1", ClaimOrderID: orderID, Status: "imported", UpdatedAtMS: now.UnixMilli(),
	}}); err != nil {
		t.Fatalf("upsert recovery: %v", err)
	}

	leases, err := New(st, nil).ListAccountLeases(ctx)
	if err != nil || len(leases) != 1 {
		t.Fatalf("list account provenance: leases=%#v err=%v", leases, err)
	}
	lease := leases[0]
	if lease.FileName != "replacement.json" || lease.Source != "recovery" || lease.ImportMethod != "reauth_replacement" ||
		lease.ImportAction != "replace" || lease.ReplacedFileName != "expired.json" || lease.RecoveryID != "recovery-1" ||
		lease.RecoveryStatus != "imported" || lease.ImportedAtMS != now.UnixMilli() || lease.LeaseExpiresAtMS <= now.UnixMilli() {
		t.Fatalf("replacement provenance = %#v", lease)
	}
}

func TestInspectionVerifiedCapacityOverridesMisleadingLiveActiveCount(t *testing.T) {
	resource := SmartResource{
		CapacitySource:       smartCapacitySourceInspection,
		CapacitySnapshotAtMS: time.Now().UnixMilli(),
		TotalAccounts:        90,
		AvailableAccounts:    12,
		SchedulableAccounts:  12,
		HealthyAccounts:      9,
		NormalAccounts:       9,
	}
	stats := reconcileAccountPoolStatsWithInspection(accountPoolStats{total: 90, schedulable: 28}, resource)
	applyAccountPoolStats(&resource, stats)
	if resource.AvailableAccounts != 12 || resource.SchedulableAccounts != 28 ||
		resource.WeakAccounts != 3 || resource.AtRiskAccounts != 19 || resource.DisabledAccounts != 62 {
		t.Fatalf("verified pool statistics = %#v", resource)
	}
}

func TestApplyAccountPoolStatsClearsStaleOperatorClassificationWithoutEvidence(t *testing.T) {
	resource := SmartResource{
		TotalAccounts:                  10,
		EnabledAccounts:                8,
		NormalAccounts:                 6,
		NeedsAttentionAccounts:         1,
		QuotaRiskAccounts:              1,
		UnconfirmedAccounts:            0,
		AccountClassificationObserved:  true,
		operatorClassificationObserved: true,
	}
	applyAccountPoolStats(&resource, accountPoolStats{
		total:        10,
		enabled:      8,
		schedulable:  8,
		liveObserved: true,
		// No matching completed inspection evidence.
	})
	if resource.AccountClassificationObserved || resource.operatorClassificationObserved ||
		resource.NormalAccounts != 0 || resource.NeedsAttentionAccounts != 0 ||
		resource.QuotaRiskAccounts != 0 || resource.UnconfirmedAccounts != 0 {
		t.Fatalf("stale operator classification was retained: %#v", resource)
	}
}

func TestPartialInspectionDoesNotHideUninspectedLiveAccounts(t *testing.T) {
	resource := SmartResource{
		CapacitySource:       smartCapacitySourceInspection,
		CapacitySnapshotAtMS: time.Now().UnixMilli(),
		TotalAccounts:        1,
		AvailableAccounts:    1,
		HealthyAccounts:      1,
	}
	stats := reconcileAccountPoolStatsWithInspection(accountPoolStats{total: 3, schedulable: 2}, resource)
	applyAccountPoolStats(&resource, stats)
	if resource.AvailableAccounts != 2 || resource.SchedulableAccounts != 2 || resource.DisabledAccounts != 1 {
		t.Fatalf("partial inspection pool statistics = %#v", resource)
	}
}

func TestCustomSupplyRefillsVerifiedPoolToConfiguredHealthyFloor(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Strategy:                  managerconfigsvc.SupplyStrategyCustom,
		TargetAvailableAccounts:   100,
		HealthyAvailableAccounts:  100,
		CriticalAvailableAccounts: 2,
		ReplenishBatchSize:        10,
		PrelockMaxQuantity:        10,
	}
	resource := SmartResource{
		SnapshotFresh:       true,
		CapacitySource:      smartCapacitySourceInspection,
		AvailableAccounts:   12,
		SchedulableAccounts: 28,
		ConsumeRCUPerMinute: 1,
	}
	New(nil, nil).reconcileSmartAccountPoolGuard(cfg, &resource)
	if resource.EmergencyShortage || resource.SuggestedAction == smartActionEmergencyReplenish || resource.SuggestedQuantity != 0 {
		t.Fatalf("healthy-floor-only shortage must not trigger emergency purchase = %#v", resource)
	}
}

func TestCustomSupplyClearsVerifiedHealthyFloorShortageAfterRecovery(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Strategy:                  managerconfigsvc.SupplyStrategyCustom,
		TargetAvailableAccounts:   100,
		HealthyAvailableAccounts:  100,
		CriticalAvailableAccounts: 2,
	}
	resource := SmartResource{
		SnapshotFresh:       true,
		CapacitySource:      smartCapacitySourceInspection,
		AvailableAccounts:   100,
		SchedulableAccounts: 100,
		EmergencyShortage:   true,
		EmergencyReason:     "healthy_available_accounts",
		HealthLevel:         smartHealthCritical,
		SuggestedAction:     smartActionEmergencyReplenish,
		DecisionReason:      "healthy_available_accounts",
		SuggestedQuantity:   10,
	}
	New(nil, nil).reconcileSmartAccountPoolGuard(cfg, &resource)
	if resource.EmergencyShortage || resource.EmergencyReason != "" || resource.SuggestedQuantity != 0 ||
		resource.DecisionReason != "usage_rate_not_ready" {
		t.Fatalf("recovered verified healthy-floor state = %#v", resource)
	}
}

func TestLiveAccountPoolCapsStaleInspectionCapacityAndRecalculatesShortage(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Product:                   "oauth_7d",
		Strategy:                  managerconfigsvc.SupplyStrategyStrongSupply,
		HealthyMinutesTarget:      60,
		WarningMinutes:            30,
		CriticalMinutes:           20,
		HealthyAvailableAccounts:  10,
		CriticalAvailableAccounts: 2,
		NewAccountConfidence:      0.7,
		ReplenishBatchSize:        50,
		PrelockMaxQuantity:        20,
	}
	resource := defaultSmartResource(cfg)
	resource.AvailableAccounts = 41
	resource.SchedulableAccounts = 41
	resource.HealthyAccounts = 41
	resource.PendingInspectionAccounts = 3
	resource.PendingInspectionCapacityRCU = 150
	resource.RequestDemandRCUPerMinute = 3_000
	resource.ConsumeRCUPerMinute = 3_000
	resource.DemandTrend = smartDemandTrendStable
	resource.CurrentCapacityRCU = 41_000
	resource.TimeLimitedCapacityRCU = 41_000

	applyAccountPoolStats(&resource, accountPoolStats{total: 1220, enabled: 5, schedulable: 5, liveObserved: true})
	if !reconcileSmartCapacityWithAccountPool(&resource, 41) {
		t.Fatal("live account decrease must cap stale inspection capacity")
	}
	wantCapacity := round2(41_000 * 5.0 / 41.0)
	if resource.CurrentCapacityRCU != wantCapacity || resource.TimeLimitedCapacityRCU != wantCapacity {
		t.Fatalf("live capacity ratio = %#v, want %.2f RCU", resource, wantCapacity)
	}
	if resource.PendingInspectionAccounts != 0 || resource.PendingInspectionCapacityRCU != 0 {
		t.Fatalf("stale pending capacity must not outlive the live pool: %#v", resource)
	}

	recalculateSmartResourceCapacityPlan(cfg, &resource)
	if resource.HealthLevel != smartHealthCritical || resource.EstimatedRequiredAccounts != 1005 ||
		resource.AccountQuantityDeficit != 1000 || resource.SuggestedQuantity != 20 {
		t.Fatalf("live five-account shortage plan = %#v", resource)
	}
	if quantity := New(nil, nil).smartSuggestedCreateQuantity(cfg, resource); quantity != 20 {
		t.Fatalf("live five-account quota shortage should fill the configured batch, got %d", quantity)
	}
}

func TestAutomaticReplenishmentCreatesTakesAndImportsOrder(t *testing.T) {
	var createCalls atomic.Int32
	var takeCalls atomic.Int32
	var uploadCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":10,"missing":0,"estimated_total_fen":1000}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":10000,"balance_fen":10000}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			createCalls.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"order":{"id":"order-1","status":"ready","quantity":1},"status_url":"/custom/order-status","take_url":"/custom/order-take"}`))
		case r.URL.Path == "/custom/order-status" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"id":"order-1","status":"ready","ready_quantity":1,"progress":100}`))
		case r.URL.Path == "/custom/order-take":
			takeCalls.Add(1)
			_, _ = w.Write([]byte(`{"payload":{"accounts":[{"type":"codex","account":"new@example.com","access_token":"secret"}]},"status":"completed"}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			if r.Header.Get("Authorization") != "Bearer management-key" {
				t.Fatalf("management authorization = %q", r.Header.Get("Authorization"))
			}
			if name := r.URL.Query().Get("name"); name != "" {
				if uploadCalls.Load() > 0 {
					_, _ = w.Write([]byte(`{"files":[{"name":"` + name + `","provider":"codex","disabled":false,"status":"ready"}]}`))
				} else {
					_, _ = w.Write([]byte(`{"files":[]}`))
				}
			} else {
				_, _ = w.Write([]byte(`{"files":[{"name":"existing.json","provider":"codex","disabled":false,"status":"ready"}]}`))
			}
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodPost:
			uploadCalls.Add(1)
			part, err := r.MultipartReader()
			if err != nil {
				t.Fatalf("multipart reader: %v", err)
			}
			foundPayload := false
			for {
				item, err := part.NextPart()
				if err != nil {
					break
				}
				data, _ := io.ReadAll(item)
				if item.FormName() == "file" {
					foundPayload = len(data) > 0 && item.FileName() != ""
					var payload map[string]any
					if err := json.Unmarshal(data, &payload); err != nil || payload["type"] != "codex" {
						t.Fatalf("uploaded payload was not normalized CPA Codex JSON: %s", data)
					}
					if _, nested := payload["credentials"]; nested {
						t.Fatal("uploaded payload still contains Sub2 credentials wrapper")
					}
				}
			}
			if !foundPayload {
				t.Fatal("uploaded account payload is missing")
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	smartDisabled := false
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled:                 &enabled,
			BaseURL:                 server.URL,
			Username:                "customer",
			Password:                "password",
			Product:                 "oauth_30d",
			TargetAvailableAccounts: 2,
			ReplenishBatchSize:      1,
			CheckIntervalSeconds:    60,
			PollIntervalSeconds:     1,
			DefaultWebsockets:       true,
			SmartEnabled:            &smartDisabled,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	managerConfig := managerconfigsvc.New(config.Config{}, st, nil)
	service := New(st, managerConfig, server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("run automatic: %v", err)
	}
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("settle-window automatic run: %v", err)
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if createCalls.Load() != 1 || takeCalls.Load() != 1 || uploadCalls.Load() != 1 {
		t.Fatalf("calls create=%d take=%d upload=%d", createCalls.Load(), takeCalls.Load(), uploadCalls.Load())
	}
	if len(status.Orders) != 1 || status.Orders[0].Status != "completed" || status.Orders[0].ImportedCount != 1 {
		t.Fatalf("orders = %#v", status.Orders)
	}
	if status.Orders[0].StatusURL != "/custom/order-status" || status.Orders[0].TakeURL != "/custom/order-take" ||
		status.Orders[0].ReadyQuantity != 1 || status.Orders[0].Progress != 100 {
		t.Fatalf("persisted remote order details = %#v", status.Orders[0])
	}
	if status.Config.Password != "" || !status.Config.PasswordConfigured {
		t.Fatalf("sanitized config = %#v", status.Config)
	}
}

func TestRecoverySyncClaimsImportsAndDisablesOriginalAccount(t *testing.T) {
	var claimCalls atomic.Int32
	var uploadCalls atomic.Int32
	var disableCalls atomic.Int32
	uploadedNames := sync.Map{}

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/recoveries" && r.Method == http.MethodGet:
			if got := r.Header.Get("X-Customer-Token"); got != "customer-token" {
				t.Fatalf("recoveries token = %q", got)
			}
			_, _ = w.Write([]byte(`{"payload":{"recoveries":[{"id":"rec-1","delivery_status":"claimable","product":"oauth_30d","source_order_id":"source-rec-1","original_email":"old@example.com","original_account":"old.json","original_auth_index":"auth-1","claim_url":"` + server.URL + `/api/customer/recoveries/rec-1/claim?ticket=ticket-1"}]}}`))
		case r.URL.Path == "/api/customer/recoveries/rec-1/claim" && r.Method == http.MethodPost:
			claimCalls.Add(1)
			if got := r.Header.Get("X-Recovery-Ticket"); got != "ticket-1" {
				t.Fatalf("claim ticket = %q", got)
			}
			if got := r.Header.Get("Idempotency-Key"); got != "cpam-recovery-rec-1" {
				t.Fatalf("claim idempotency key = %q", got)
			}
			if got := r.URL.Query().Get("ticket"); got != "ticket-1" {
				t.Fatalf("legacy claim ticket = %q", got)
			}
			_, _ = w.Write([]byte(`{"credential_version":2,"payload":{"type":"oauth","name":"replacement","platform":"openai","credentials":{"access_token":"access-new","refresh_token":"refresh-new","email":"new@example.com","chatgpt_account_id":"acct-new","chatgpt_plan_type":"team","workspace_id":"workspace-new"}}}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			if r.Header.Get("Authorization") != "Bearer management-key" {
				t.Fatalf("management authorization = %q", r.Header.Get("Authorization"))
			}
			name := r.URL.Query().Get("name")
			if name == "" {
				_, _ = w.Write([]byte(`{"files":[{"name":"old.json","provider":"codex","disabled":false,"status":"ready"}]}`))
				return
			}
			if _, ok := uploadedNames.Load(name); ok {
				_, _ = w.Write([]byte(`{"files":[{"name":"` + name + `","provider":"codex","disabled":false,"status":"ready"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"files":[]}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodPost:
			uploadCalls.Add(1)
			part, err := r.MultipartReader()
			if err != nil {
				t.Fatalf("multipart reader: %v", err)
			}
			for {
				item, err := part.NextPart()
				if err != nil {
					break
				}
				if item.FormName() != "file" {
					continue
				}
				if item.FileName() != "old.json" {
					t.Fatalf("replacement filename = %q", item.FileName())
				}
				uploadedNames.Store(item.FileName(), struct{}{})
				data, _ := io.ReadAll(item)
				var payload map[string]any
				if err := json.Unmarshal(data, &payload); err != nil {
					t.Fatalf("decode upload payload %s: %v", data, err)
				}
				if payload["type"] != "codex" || payload["access_token"] != "access-new" ||
					payload["refresh_token"] != "refresh-new" || payload["email"] != "new@example.com" {
					t.Fatalf("uploaded payload was not normalized replacement JSON: %#v", payload)
				}
				if _, nested := payload["credentials"]; nested {
					t.Fatalf("uploaded payload still contains credentials wrapper: %#v", payload)
				}
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case r.URL.Path == "/v0/management/auth-files/status" && r.Method == http.MethodPatch:
			disableCalls.Add(1)
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode disable payload: %v", err)
			}
			if payload["name"] != "old.json" || payload["auth_index"] != "auth-1" || payload["disabled"] != true {
				t.Fatalf("disable payload = %#v", payload)
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply-recovery.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled:                     &enabled,
			BaseURL:                     server.URL,
			Username:                    "customer",
			Password:                    "password",
			Product:                     "oauth_30d",
			PollIntervalSeconds:         1,
			RecoverySyncEnabled:         &enabled,
			RecoveryAutoClaim:           &enabled,
			RecoverySyncIntervalSeconds: 60,
			RecoveryClaimBatchSize:      10,
			RecoveryDisableOriginal:     &enabled,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	createRecoverySourceOrder(t, st, "source-rec-1")

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	autoClaim := true
	summary, err := service.SyncRecoveries(context.Background(), RecoverySyncRequest{
		Force:     true,
		AutoClaim: &autoClaim,
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("sync recoveries: %v", err)
	}
	if summary.Seen != 1 || summary.Claimed != 1 || summary.Imported != 1 ||
		summary.StoredImported != 1 || summary.LastResult != "completed" {
		t.Fatalf("summary = %#v", summary)
	}
	if claimCalls.Load() != 1 || uploadCalls.Load() != 1 || disableCalls.Load() != 0 {
		t.Fatalf("calls claim=%d upload=%d disable=%d", claimCalls.Load(), uploadCalls.Load(), disableCalls.Load())
	}
	recoveries, err := service.ListRecoveries(context.Background(), 10, "")
	if err != nil || len(recoveries) != 1 || recoveries[0].Status != "imported" ||
		recoveries[0].ImportedCount != 1 || recoveries[0].ClaimOrderID != "recovery-rec-1" ||
		recoveries[0].CredentialVersion != 2 || len(recoveries[0].ImportedFileNames) != 1 || recoveries[0].LastImportedAtMS <= 0 ||
		len(recoveries[0].ImportItems) != 1 || recoveries[0].ImportItems[0].Status != "imported" ||
		recoveries[0].ImportItems[0].ImportAction != "replace" || recoveries[0].ImportItems[0].ReplacedFileName != "old.json" ||
		recoveries[0].ImportItems[0].FileName != recoveries[0].ImportedFileNames[0] {
		t.Fatalf("recoveries=%#v err=%v", recoveries, err)
	}
	order, found, err := st.GetSupplyOrder(context.Background(), "recovery-rec-1")
	if err != nil || !found || order.Status != "completed" || order.ImportedCount != 1 {
		t.Fatalf("recovery order=%#v found=%v err=%v", order, found, err)
	}
}

func TestRecoveryOwnershipSeparatesLocalAndOtherPoolTickets(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "recovery-ownership.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	createRecoverySourceOrder(t, st, "source-local")
	nowMS := time.Now().UnixMilli()
	if _, err := st.UpsertSupplyRecoveries(ctx, []store.SupplyRecovery{
		{RecoveryID: "local-claimed", SourceOrderID: "source-local", DeliveryStatus: "claimed", Status: "claimed", CredentialVersion: 2, LastSeenAtMS: nowMS},
		{RecoveryID: "external-claimed", SourceOrderID: "source-other", DeliveryStatus: "claimed", Status: "claimed", CredentialVersion: 2, LastSeenAtMS: nowMS},
		{RecoveryID: "local-claimable", SourceOrderID: "source-local", DeliveryStatus: "claimable", Status: "claimable", ClaimURL: "/claim/local", LastSeenAtMS: nowMS},
		{RecoveryID: "external-claimable", SourceOrderID: "source-other", DeliveryStatus: "claimable", Status: "claimable", ClaimURL: "/claim/external", LastSeenAtMS: nowMS},
	}); err != nil {
		t.Fatalf("upsert recoveries: %v", err)
	}
	claimable, err := st.ListClaimableSupplyRecoveries(ctx, 20)
	if err != nil || len(claimable) != 1 || claimable[0].RecoveryID != "local-claimable" {
		t.Fatalf("owned claimable recoveries=%#v err=%v", claimable, err)
	}
	summary, err := st.SupplyRecoverySummary(ctx)
	if err != nil || summary.Total != 2 || summary.External != 2 || summary.Claimable != 1 {
		t.Fatalf("recovery ownership summary=%#v err=%v", summary, err)
	}
	recoveries, err := New(st, nil).ListRecoveries(ctx, 20, "")
	if err != nil {
		t.Fatalf("list recoveries: %v", err)
	}
	byID := make(map[string]store.SupplyRecovery, len(recoveries))
	for _, recovery := range recoveries {
		byID[recovery.RecoveryID] = recovery
	}
	if got := byID["local-claimed"]; got.Ownership != "local" || got.ImportStatus != "claimed_without_local_payload" || got.SourceOrderID != "source-local" {
		t.Fatalf("local claimed recovery=%#v", got)
	}
	if got := byID["external-claimed"]; got.Ownership != "external" || got.ImportStatus != "not_this_pool" {
		t.Fatalf("external claimed recovery=%#v", got)
	}
	if got := byID["local-claimable"]; got.Ownership != "local" || got.ImportStatus != "waiting_claim" {
		t.Fatalf("local claimable recovery=%#v", got)
	}
	if got := byID["external-claimable"]; got.Ownership != "external" || got.ImportStatus != "not_this_pool" {
		t.Fatalf("external claimable recovery=%#v", got)
	}
}

func TestRecoveryClaimConflictWaitsForFreshClaimURL(t *testing.T) {
	var listCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/recoveries" && r.Method == http.MethodGet:
			call := listCalls.Add(1)
			ticket := "old"
			if call > 1 {
				ticket = "fresh"
			}
			_, _ = w.Write([]byte(`{"recoveries":[{"id":"rec-conflict","delivery_status":"claimable","source_order_id":"source-conflict","claim_url":"/api/customer/recoveries/rec-conflict/claim?ticket=` + ticket + `"}]}`))
		case r.URL.Path == "/api/customer/recoveries/rec-conflict/claim":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"ticket expired"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "recovery-conflict.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_30d",
			RecoverySyncEnabled: &enabled, RecoveryAutoClaim: &enabled,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	createRecoverySourceOrder(t, st, "source-conflict")
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	autoClaim := true
	if _, err := service.SyncRecoveries(context.Background(), RecoverySyncRequest{AutoClaim: &autoClaim}); err == nil {
		t.Fatal("expected stale ticket conflict")
	}
	recovery, found, err := st.GetSupplyRecovery(context.Background(), "rec-conflict")
	if err != nil || !found || recovery.Status != "seen" || !strings.Contains(recovery.LastError, "HTTP 409") {
		t.Fatalf("recovery=%#v found=%v err=%v", recovery, found, err)
	}
	autoClaim = false
	if _, err := service.SyncRecoveries(context.Background(), RecoverySyncRequest{AutoClaim: &autoClaim}); err != nil {
		t.Fatalf("refresh recovery URL: %v", err)
	}
	recovery, found, err = st.GetSupplyRecovery(context.Background(), "rec-conflict")
	if err != nil || !found || recovery.Status != "claimable" || !strings.Contains(recovery.ClaimURL, "ticket=fresh") || recovery.LastError != "" {
		t.Fatalf("refreshed recovery=%#v found=%v err=%v", recovery, found, err)
	}
}

func TestRecoveryPayloadServerErrorKeepsTicketClaimable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/recoveries" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"recoveries":[{"id":"rec-retry","delivery_status":"claimable","source_order_id":"source-retry","claim_url":"/api/customer/recoveries/rec-retry/claim?ticket=keep-me"}]}`))
		case r.URL.Path == "/api/customer/recoveries/rec-retry/claim":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"recovery_payload_invalid","message":"payload temporarily unavailable"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "recovery-server-retry.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_30d",
			RecoverySyncEnabled: &enabled, RecoveryAutoClaim: &enabled,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	createRecoverySourceOrder(t, st, "source-retry")
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	autoClaim := true
	if _, err := service.SyncRecoveries(context.Background(), RecoverySyncRequest{AutoClaim: &autoClaim}); err == nil {
		t.Fatal("expected temporary claim failure")
	}
	recovery, found, err := st.GetSupplyRecovery(context.Background(), "rec-retry")
	if err != nil || !found || recovery.Status != "claimable" || !strings.Contains(recovery.ClaimURL, "ticket=keep-me") ||
		!strings.Contains(recovery.LastError, "recovery_payload_invalid") {
		t.Fatalf("recovery=%#v found=%v err=%v", recovery, found, err)
	}
}

func TestTakeReplacementFileRefreshesLatestClaimURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case "/api/customer/recoveries/rec-from-take":
			_, _ = w.Write([]byte(`{"recovery":{"id":"rec-from-take","delivery_status":"claimable","product":"oauth_30d","claim_url":"/api/customer/recoveries/rec-from-take/claim?ticket=latest","credential_version":3}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "take-replacement.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_30d",
	}}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.syncTakeReplacementFiles(context.Background(), cfg, "source-take", []supplyclient.ReplacementFile{{
		RecoveryID: "rec-from-take", Ready: true, StatusURL: "/api/customer/recoveries/rec-from-take", CredentialVersion: 2,
	}}); err != nil {
		t.Fatalf("sync replacement: %v", err)
	}
	recovery, found, err := st.GetSupplyRecovery(context.Background(), "rec-from-take")
	if err != nil || !found || recovery.Status != "claimable" || recovery.CredentialVersion != 3 ||
		recovery.SourceOrderID != "source-take" || !strings.Contains(recovery.ClaimURL, "ticket=latest") {
		t.Fatalf("recovery=%#v found=%v err=%v", recovery, found, err)
	}
}

func TestRecoverySyncIntervalHonorsRetryAfter(t *testing.T) {
	interval := recoverySyncInterval(store.ManagerSupplyConfig{RecoverySyncIntervalSeconds: 60}, &supplyclient.HTTPError{
		StatusCode: http.StatusTooManyRequests, RetryAfterSeconds: 17,
	})
	if interval != 17*time.Second {
		t.Fatalf("interval = %s", interval)
	}
}

func TestRecoveryNextSyncIntervalDrainsAutomaticBacklog(t *testing.T) {
	cfg := store.ManagerSupplyConfig{RecoverySyncIntervalSeconds: 30}
	if interval := recoveryNextSyncInterval(cfg, nil, true, 9); interval != 3*time.Second {
		t.Fatalf("backlog interval = %s, want 3s", interval)
	}
	if interval := recoveryNextSyncInterval(cfg, nil, false, 9); interval != 30*time.Second {
		t.Fatalf("manual interval = %s, want 30s", interval)
	}
	retryErr := &supplyclient.HTTPError{StatusCode: http.StatusTooManyRequests, RetryAfterSeconds: 17}
	if interval := recoveryNextSyncInterval(cfg, retryErr, true, 9); interval != 17*time.Second {
		t.Fatalf("retry interval = %s, want 17s", interval)
	}
}

func TestReportAggregatesSupplySpendRecoveriesAndUsageRevenue(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "supply-report.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().Truncate(time.Second)
	fromMS := now.Add(-2 * time.Hour).UnixMilli()
	toMS := now.Add(2 * time.Hour).UnixMilli()
	orderCreated := now.Add(-45 * time.Minute)
	completedAtMS := orderCreated.Add(3 * time.Minute).UnixMilli()
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID:           "order-report-1",
		Product:           "oauth_30d",
		RequestedQuantity: 2,
		Automatic:         true,
		Strategy:          managerconfigsvc.SupplyStrategyStrongSupply,
		TriggerReason:     "emergency_pool_vacuum",
		Status:            "completed",
		ChargedFen:        1200,
		ReleasedFen:       200,
		ItemCount:         2,
		ImportedCount:     2,
		CompletedAtMS:     completedAtMS,
		CreatedAtMS:       orderCreated.UnixMilli(),
	}); err != nil {
		t.Fatalf("create supply order: %v", err)
	}
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID:           "recovery-report-1",
		Product:           "oauth_30d",
		RequestedQuantity: 1,
		Automatic:         true,
		Strategy:          "recovery",
		TriggerReason:     "recovery_claimed",
		Status:            "completed",
		RemoteStatus:      "recovery_claimed",
		ItemCount:         1,
		ImportedCount:     1,
		CompletedAtMS:     now.Add(-28 * time.Minute).UnixMilli(),
		CreatedAtMS:       now.Add(-29 * time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatalf("create recovery import row: %v", err)
	}
	if inserted, err := st.InsertSupplyImportItems(ctx, "order-report-1", []store.SupplyImportItem{
		{OrderID: "order-report-1", ItemKey: "item-a", FileName: "codex-supply-a.json", PayloadJSON: `{"type":"codex"}`, LeaseExpiresAtMS: now.Add(10 * time.Minute).UnixMilli(), BasePriceFen: 700, ChargedFen: 500},
		{OrderID: "order-report-1", ItemKey: "item-b", FileName: "codex-supply-b.json", PayloadJSON: `{"type":"codex"}`, LeaseExpiresAtMS: now.Add(time.Hour).UnixMilli(), BasePriceFen: 700, ChargedFen: 700},
	}); err != nil || inserted != 2 {
		t.Fatalf("insert import items inserted=%d err=%v", inserted, err)
	}
	if inserted, err := st.InsertSupplyImportItems(ctx, "recovery-report-1", []store.SupplyImportItem{{
		OrderID: "recovery-report-1", ItemKey: "replacement-a", FileName: "codex-supply-a.json", PayloadJSON: `{"type":"codex"}`,
	}}); err != nil || inserted != 1 {
		t.Fatalf("insert recovery import item inserted=%d err=%v", inserted, err)
	}
	recoveryItems, err := st.ListPendingSupplyImportItems(ctx, "recovery-report-1", now.UnixMilli(), 10)
	if err != nil || len(recoveryItems) != 1 {
		t.Fatalf("pending recovery items=%#v err=%v", recoveryItems, err)
	}
	if err := st.MarkSupplyImportItemImported(ctx, recoveryItems[0].ID, now.Add(-27*time.Minute).UnixMilli()); err != nil {
		t.Fatalf("mark recovery item imported: %v", err)
	}
	items, err := st.ListPendingSupplyImportItems(ctx, "order-report-1", now.UnixMilli(), 10)
	if err != nil || len(items) != 2 {
		t.Fatalf("pending items=%#v err=%v", items, err)
	}
	for _, item := range items {
		if err := st.MarkSupplyImportItemImported(ctx, item.ID, now.Add(-30*time.Minute).UnixMilli()); err != nil {
			t.Fatalf("mark item imported: %v", err)
		}
	}
	if _, err := st.UpsertSupplyRecoveries(ctx, []store.SupplyRecovery{
		{
			RecoveryID:     "rec-imported",
			SourceOrderID:  "order-report-1",
			Product:        "oauth_30d",
			DeliveryStatus: "claimable",
			Status:         "imported",
			ItemCount:      1,
			ImportedCount:  1,
			LastSeenAtMS:   now.Add(-35 * time.Minute).UnixMilli(),
			ClaimedAtMS:    now.Add(-34 * time.Minute).UnixMilli(),
		},
		{
			RecoveryID:     "rec-refunded",
			SourceOrderID:  "order-report-1",
			Product:        "oauth_30d",
			DeliveryStatus: "refunded",
			Status:         "refunded",
			RefundedFen:    300,
			LastSeenAtMS:   now.Add(-25 * time.Minute).UnixMilli(),
		},
	}); err != nil {
		t.Fatalf("upsert recoveries: %v", err)
	}
	if err := st.SaveModelPrices(ctx, map[string]store.ModelPrice{
		"gpt-report": {Prompt: 2, Completion: 4, Cache: 1},
	}); err != nil {
		t.Fatalf("save model prices: %v", err)
	}
	if _, err := st.InsertEvents(ctx, []usage.Event{
		supplyReportEvent("usage-report-1", now.Add(-20*time.Minute).UnixMilli(), "gpt-report", "codex-supply-a.json", false, 1_000_000, 500_000, 0, 0, 0, 1_500_000, nil),
		supplyReportEvent("usage-report-other", now.Add(-10*time.Minute).UnixMilli(), "gpt-report", "non-supply.json", false, 1_000_000, 500_000, 0, 0, 0, 1_500_000, nil),
	}); err != nil {
		t.Fatalf("insert usage event: %v", err)
	}
	candidate, err := st.UpsertAccountActionCandidate(ctx, store.AccountActionCandidateUpsert{
		ActionType:          "reauth",
		Provider:            "codex",
		AuthFileName:        "codex-supply-a.json",
		ReasonCode:          "invalid_401",
		Reason:              "supplier account returned 401",
		AutoDisableEligible: true,
		SeenAtMS:            now.Add(-15 * time.Minute).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("upsert 401 candidate: %v", err)
	}
	if err := st.MarkAccountActionCandidateAutoDisabled(ctx, candidate.ID, now.Add(-14*time.Minute).UnixMilli()); err != nil {
		t.Fatalf("mark 401 candidate auto disabled: %v", err)
	}

	report, err := New(st, nil).Report(ctx, ReportRequest{FromMS: fromMS, ToMS: toMS})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if report.Executive.Orders != 1 || report.Executive.AutomaticOrders != 1 ||
		report.Executive.RecoveryOrders != 0 || report.Executive.RequestedAccounts != 2 || report.Executive.ImportedAccounts != 2 {
		t.Fatalf("executive order counts = %#v", report.Executive)
	}
	if report.Executive.EmergencyReplenishments != 1 || report.Executive.VacuumReplenishments != 1 ||
		report.Executive.Auth401Accounts != 1 || report.Executive.Auth401Events != 1 ||
		report.Executive.Auth401Rate != 1 || report.Executive.AutoQuarantined != 1 || report.Executive.VacuumTotalSeconds != 180 ||
		report.Executive.AverageVacuumRecoverySeconds != 180 {
		t.Fatalf("strategy and 401 metrics = %#v", report.Executive)
	}
	if report.Executive.ChargedFen != 1200 || report.Executive.ReleasedFen != 200 ||
		report.Executive.RefundedFen != 300 || report.Executive.SupplySpendFen != 1200 ||
		report.Executive.SupplyNetSpendFen != 900 || report.Executive.AverageUnitFen != 600 {
		t.Fatalf("executive money = %#v", report.Executive)
	}
	if report.Reconciliation.Summary.AllocationMethod != "supplier_item_exact_else_order_even_split" ||
		report.Reconciliation.Summary.OrderRows != 1 ||
		report.Reconciliation.Summary.AccountAllocatedChargedFen != 1200 ||
		report.Reconciliation.Summary.AccountAllocatedReleasedFen != 200 ||
		report.Reconciliation.Summary.AccountAllocatedNetFen != 1200 {
		t.Fatalf("reconciliation money = %#v", report.Reconciliation.Summary)
	}
	if report.Executive.Recoveries != 2 || report.Executive.ImportedRecoveries != 1 ||
		report.Executive.RefundedRecoveries != 1 || report.Executive.RecoveryClaimRate != 1 ||
		report.Executive.RecoveryImportRate != 1 || report.Executive.RecoveryRefundRate != 0.5 {
		t.Fatalf("executive recoveries = %#v", report.Executive)
	}
	if report.Executive.UsageCalls != 1 || report.Executive.UsageTokens != 1_500_000 ||
		report.Executive.UsageRevenueCurrency != "USD" ||
		math.Abs(report.Executive.UsageRevenue-0.24) > 0.000001 ||
		math.Abs(report.Executive.AverageRevenuePerCall-0.24) > 0.000001 {
		t.Fatalf("executive usage revenue = %#v", report.Executive)
	}
	if len(report.UsageModels) != 1 || report.UsageModels[0].Model != "gpt-report" ||
		report.UsageModels[0].Calls != 1 || math.Abs(report.UsageModels[0].Revenue-0.24) > 0.000001 {
		t.Fatalf("usage models = %#v", report.UsageModels)
	}
	// Replacement imports stay in import-health/account reconciliation even
	// though they are excluded from supplier purchase counts and spend.
	if report.ImportHealth.Items != 3 || report.ImportHealth.ImportedItems != 3 ||
		report.ImportHealth.ExpiringSoonItems != 1 || report.Executive.ImportSuccessRate != 1 {
		t.Fatalf("import health = %#v executive=%#v", report.ImportHealth, report.Executive)
	}
	foundUsageTimeline := false
	for _, point := range report.Timeline {
		if point.UsageCalls == 1 {
			foundUsageTimeline = point.UsageTokens == 1_500_000 && math.Abs(point.UsageRevenue-0.24) <= 0.000001
		}
	}
	if !foundUsageTimeline {
		t.Fatalf("timeline missing usage revenue: %#v", report.Timeline)
	}
	if len(report.Products) == 0 || len(report.Strategies) == 0 || len(report.TriggerReasons) == 0 ||
		len(report.Sources) == 0 || len(report.RecoveryStatuses) == 0 ||
		len(report.DeliveryStatuses) == 0 || len(report.OrderStatuses) == 0 {
		t.Fatalf("dimension stats were not populated: %#v", report)
	}
}

func TestNormalizeReportRequestDefaultsToToday(t *testing.T) {
	before := time.Now()
	req := normalizeReportRequest(ReportRequest{})
	after := time.Now()
	from := time.UnixMilli(req.FromMS).In(time.Local)
	to := time.UnixMilli(req.ToMS).In(time.Local)
	if req.FromMS <= 0 || req.ToMS <= 0 || req.ToMS <= req.FromMS {
		t.Fatalf("normalized range is invalid: %#v", req)
	}
	if to.Before(before.Add(-2*time.Second)) || to.After(after.Add(2*time.Second)) {
		t.Fatalf("toMs=%s is not near now [%s, %s]", to, before, after)
	}
	if from.Year() != to.Year() || from.YearDay() != to.YearDay() ||
		from.Hour() != 0 || from.Minute() != 0 || from.Second() != 0 {
		t.Fatalf("fromMs=%s should be start of the same local day as toMs=%s", from, to)
	}
}

func TestSupplyAccountStatusReasonExplainsAbnormalStates(t *testing.T) {
	now := time.Date(2026, 8, 9, 21, 45, 0, 0, time.Local)
	failedReason := supplyAccountStatusReason("failed", store.SupplyImportItem{
		Status:    "failed",
		LastError: "upload rejected by CPA",
	}, cpaauthfiles.File{}, false, false, now)
	if failedReason != "upload rejected by CPA" {
		t.Fatalf("failed reason = %q", failedReason)
	}

	expiredAt := now.Add(-time.Minute).UnixMilli()
	expiredReason := supplyAccountStatusReason("expired", store.SupplyImportItem{
		Status:           "imported",
		LeaseExpiresAtMS: now.Add(-2 * time.Hour).UnixMilli(),
		PayloadJSON:      fmt.Sprintf(`{"type":"codex","expires_at":%d}`, expiredAt/1000),
	}, cpaauthfiles.File{}, true, true, now)
	if !strings.Contains(expiredReason, "过期") || !strings.Contains(expiredReason, "2026-08-09") {
		t.Fatalf("expired reason = %q", expiredReason)
	}

	missingReason := supplyAccountStatusReason("missing", store.SupplyImportItem{
		Status: "imported",
	}, cpaauthfiles.File{}, true, false, now)
	if !strings.Contains(missingReason, "未找到") {
		t.Fatalf("missing reason = %q", missingReason)
	}

	disabledReason := supplyAccountStatusReason("disabled", store.SupplyImportItem{
		Status: "imported",
	}, cpaauthfiles.File{
		Provider: "codex",
		Disabled: true,
		Raw: map[string]any{
			"disabled_reason": "OAuth 401 reauthorization required",
		},
	}, true, true, now)
	if disabledReason != "OAuth 401 reauthorization required" {
		t.Fatalf("disabled reason = %q", disabledReason)
	}

	quotaReason := supplyAccountStatusReason("disabled", store.SupplyImportItem{
		Status: "imported",
	}, cpaauthfiles.File{
		Provider: "codex",
		Raw: map[string]any{
			"status":     "ready",
			"error_kind": "usage_limit_reached",
		},
	}, true, true, now)
	if !strings.Contains(quotaReason, "usage_limit_reached") {
		t.Fatalf("quota reason = %q", quotaReason)
	}
}

func TestRecoverySyncImportsLocalPendingWhenSupplierRecoveriesFail(t *testing.T) {
	var uploadCalls atomic.Int32
	uploadedNames := sync.Map{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/recoveries" && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"message":"supplier recoveries temporarily unavailable"}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			name := r.URL.Query().Get("name")
			if name == "" {
				_, _ = w.Write([]byte(`{"files":[]}`))
				return
			}
			if _, ok := uploadedNames.Load(name); ok {
				_, _ = w.Write([]byte(`{"files":[{"name":"` + name + `","provider":"codex","disabled":false,"status":"ready"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"files":[]}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodPost:
			uploadCalls.Add(1)
			part, err := r.MultipartReader()
			if err != nil {
				t.Fatalf("multipart reader: %v", err)
			}
			for {
				item, err := part.NextPart()
				if err != nil {
					break
				}
				if item.FormName() == "file" {
					uploadedNames.Store(item.FileName(), struct{}{})
				}
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply-recovery-local-pending.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	disabled := false
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			BaseURL:                 server.URL,
			Username:                "customer",
			Password:                "password",
			Product:                 "oauth_30d",
			RecoverySyncEnabled:     &enabled,
			RecoveryAutoClaim:       &disabled,
			RecoveryDisableOriginal: &disabled,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "recovery-local-pending", Product: "oauth_30d", RequestedQuantity: 1,
		Automatic: true, Status: "recovery_importing", RemoteStatus: "recovery_claimed", ItemCount: 1,
	}); err != nil {
		t.Fatalf("create recovery order: %v", err)
	}
	if _, err := st.InsertSupplyImportItems(context.Background(), "recovery-local-pending", []store.SupplyImportItem{{
		OrderID: "recovery-local-pending", ItemKey: "pending-key", FileName: "codex-supply-local.json",
		PayloadJSON: `{"type":"oauth","platform":"openai","credentials":{"access_token":"access","refresh_token":"refresh","account_id":"account-local","email":"local@example.com"}}`,
	}}); err != nil {
		t.Fatalf("insert import item: %v", err)
	}
	if _, err := st.UpsertSupplyRecoveries(context.Background(), []store.SupplyRecovery{{
		RecoveryID: "local-pending", Product: "oauth_30d", DeliveryStatus: "claimable", Status: "importing",
		ClaimOrderID: "recovery-local-pending", ItemCount: 1, LastSeenAtMS: time.Now().UnixMilli(),
	}}); err != nil {
		t.Fatalf("upsert recovery: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	autoClaim := false
	if _, err := service.SyncRecoveries(context.Background(), RecoverySyncRequest{AutoClaim: &autoClaim}); err == nil {
		t.Fatal("sync should surface supplier recoveries failure after processing local pending imports")
	}
	if uploadCalls.Load() != 1 {
		t.Fatalf("upload calls = %d, want 1", uploadCalls.Load())
	}
	order, found, err := st.GetSupplyOrder(context.Background(), "recovery-local-pending")
	if err != nil || !found || order.Status != "completed" || order.ImportedCount != 1 {
		t.Fatalf("order=%#v found=%v err=%v", order, found, err)
	}
	recovery, found, err := st.GetSupplyRecovery(context.Background(), "local-pending")
	if err != nil || !found || recovery.Status != "imported" || recovery.ImportedCount != 1 {
		t.Fatalf("recovery=%#v found=%v err=%v", recovery, found, err)
	}
}

func TestSupplyRecoveryRefundedStatusOverridesLocalImportingState(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply-recovery-refund.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	nowMS := time.Now().UnixMilli()
	if _, err := st.UpsertSupplyRecoveries(context.Background(), []store.SupplyRecovery{{
		RecoveryID: "rec-refund-priority", Product: "oauth_30d", DeliveryStatus: "claimable",
		Status: "partial", ClaimOrderID: "recovery-rec-refund-priority", ItemCount: 2, ImportedCount: 1, LastSeenAtMS: nowMS,
	}}); err != nil {
		t.Fatalf("insert recovery: %v", err)
	}
	if _, err := st.UpsertSupplyRecoveries(context.Background(), []store.SupplyRecovery{{
		RecoveryID: "rec-refund-priority", Product: "oauth_30d", DeliveryStatus: "refunded",
		Status: "refunded", RefundedFen: 900, LastSeenAtMS: nowMS + 1,
	}}); err != nil {
		t.Fatalf("refund recovery: %v", err)
	}
	recovery, found, err := st.GetSupplyRecovery(context.Background(), "rec-refund-priority")
	if err != nil || !found || recovery.Status != "refunded" || recovery.RefundedFen != 900 {
		t.Fatalf("recovery=%#v found=%v err=%v", recovery, found, err)
	}
}

func TestRecoveryIntervalCanDriveWorkerWhenAutoSupplyDisabled(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply-recovery-interval.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		Supply: store.ManagerSupplyConfig{
			BaseURL:                     "https://sogouedu.cc",
			Username:                    "customer",
			Password:                    "password",
			Product:                     "oauth_30d",
			RecoverySyncEnabled:         &enabled,
			RecoverySyncIntervalSeconds: 10,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), nil)
	service.recoveryState.NextSyncAtMS = time.Now().Add(10 * time.Second).UnixMilli()
	if interval := service.NextInterval(context.Background()); interval > 11*time.Second {
		t.Fatalf("interval = %s, want recovery interval to be honored", interval)
	}
}

func TestRecoverySyncDoesNotClaimBeforeCPAConnectionIsConfigured(t *testing.T) {
	var claimCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/recoveries" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"payload":{"recoveries":[{"id":"rec-wait","delivery_status":"claimable","claim_url":"/api/customer/recoveries/rec-wait/claim?ticket=ticket"}]}}`))
		case r.URL.Path == "/api/customer/recoveries/rec-wait/claim":
			claimCalls.Add(1)
			t.Fatal("claim should wait until CPA management connection is configured")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply-recovery-no-cpa.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		Supply: store.ManagerSupplyConfig{
			BaseURL:             server.URL,
			Username:            "customer",
			Password:            "password",
			Product:             "oauth_30d",
			RecoverySyncEnabled: &enabled,
			RecoveryAutoClaim:   &enabled,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	autoClaim := true
	summary, err := service.SyncRecoveries(context.Background(), RecoverySyncRequest{AutoClaim: &autoClaim})
	if err != nil {
		t.Fatalf("sync recoveries: %v", err)
	}
	if summary.Seen != 1 || summary.Claimed != 0 || summary.Claimable != 1 || claimCalls.Load() != 0 {
		t.Fatalf("summary=%#v claimCalls=%d", summary, claimCalls.Load())
	}
}

func TestCreateResultUncertainRetriesWithPersistedIdempotencyKey(t *testing.T) {
	var createCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/pickup/orders":
			createCalls.Add(1)
			if key := r.Header.Get("Idempotency-Key"); key != "create-attempt-persisted" {
				t.Fatalf("idempotency key = %q", key)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"order":{"id":"supplier-order-1","status":"waiting_inventory","quantity":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	smartDisabled := false
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, BaseURL: server.URL, Username: "customer", Password: "password",
			Product: "oauth_30d", TargetAvailableAccounts: 1, ReplenishBatchSize: 1,
			SmartEnabled: &smartDisabled,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "create-attempt-persisted", Product: "oauth_30d", RequestedQuantity: 1,
		Automatic: true, Strategy: "strong_supply", TriggerReason: "emergency_pool_vacuum",
		Status: "create_uncertain", LastError: "request timed out after supplier accepted it",
	}); err != nil {
		t.Fatalf("create uncertain attempt: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("retry automatic run: %v", err)
	}
	if createCalls.Load() != 1 {
		t.Fatalf("create calls = %d, want 1", createCalls.Load())
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil || status.ActiveOrder == nil || status.ActiveOrder.Status == "create_uncertain" || status.ActiveOrder.OrderID != "supplier-order-1" {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestOrderConflictIsPersistedAsCancelled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case "/api/customer/pickup/orders/order-cancelled":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"order cancelled"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		Enabled: &enabled, BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_30d",
	}}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "order-cancelled", Product: "oauth_30d", RequestedQuantity: 1, Status: "waiting_inventory",
	}); err != nil {
		t.Fatalf("create local order: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("process cancelled order: %v", err)
	}
	order, found, err := st.GetSupplyOrder(context.Background(), "order-cancelled")
	if err != nil || !found || order.Status != "cancelled" || order.CompletedAtMS == 0 {
		t.Fatalf("order=%#v found=%v err=%v", order, found, err)
	}
}

func TestActiveOrderStatusClosesMissingPickupOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case "/api/customer/pickup/orders/order-not-found":
			http.Error(w, `{"message":"pickup order not found"}`, http.StatusNotFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply-missing-order.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		Enabled: &enabled, BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_30d",
	}}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "order-not-found", Product: "oauth_30d", RequestedQuantity: 1, Status: "waiting_inventory",
	}); err != nil {
		t.Fatalf("create local order: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	status, err := service.GetActiveOrderStatus(context.Background())
	if err != nil {
		t.Fatalf("refresh active order status: %v", err)
	}
	if status.PollError != "" || status.ActiveOrder != nil || len(status.ActiveOrders) != 0 {
		t.Fatalf("active order status = %#v", status)
	}
	order, found, err := st.GetSupplyOrder(context.Background(), "order-not-found")
	if err != nil || !found || order.Status != "cancelled" || order.CompletedAtMS == 0 {
		t.Fatalf("order=%#v found=%v err=%v", order, found, err)
	}
}

func TestAutomaticCancelledOrderImmediatelyCreatesNextLadderRung(t *testing.T) {
	var createQuantity atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-cancelled":
			_, _ = w.Write([]byte(`{"order":{"id":"order-cancelled","status":"cancelled","quantity":10}}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"files":[]}`))
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":100,"missing":0,"estimated_total_fen":100}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":100000,"balance_fen":100000}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			var request struct {
				Quantity int `json:"quantity"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode replacement create request: %v", err)
			}
			createQuantity.Store(int32(request.Quantity))
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"order":{"id":"replacement-1","status":"waiting_inventory","quantity":5,"retry_after_seconds":60}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "cancelled-immediate-retry.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	smartDisabled := false
	ctx := context.Background()
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, SmartEnabled: &smartDisabled,
			BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_7d",
			TargetAvailableAccounts: 20, ReplenishBatchSize: 10,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "order-cancelled", Product: "oauth_7d", RequestedQuantity: 10,
		Automatic: true, Status: "waiting_inventory",
	}); err != nil {
		t.Fatalf("create cancelled seed order: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(ctx); err != nil {
		t.Fatalf("automatic retry after cancellation: %v", err)
	}
	if got := createQuantity.Load(); got != 5 {
		t.Fatalf("replacement quantity = %d, want 5 immediately after the cancelled 10-order", got)
	}
	orders, err := st.ListOpenSupplyOrders(ctx, 10)
	if err != nil || len(orders) != 1 || orders[0].OrderID != "replacement-1" || orders[0].RequestedQuantity != 5 {
		t.Fatalf("replacement open orders = %#v err=%v", orders, err)
	}
}

func TestManualReplenishmentQueuesTasksWhenConcurrentOrderSlotsAreFull(t *testing.T) {
	var creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"files":[]}`))
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":100,"missing":0,"estimated_total_fen":200,"estimated_unit_price_fen":100}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":100000,"balance_fen":100000}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			index := creates.Add(1) + 1
			_, _ = fmt.Fprintf(w, `{"order":{"id":"manual-%d","status":"waiting_inventory","quantity":2}}`, index)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "manual-1", Product: "oauth_30d", RequestedQuantity: 2, Status: "waiting_inventory",
	}); err != nil {
		t.Fatalf("create active order: %v", err)
	}
	enabled := true
	smartDisabled := false
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, SmartEnabled: &smartDisabled, BaseURL: server.URL,
			Username: "customer", Password: "password", Product: "oauth_30d", MaxConcurrentOrders: 3,
		},
	}); err != nil {
		t.Fatalf("save manager config: %v", err)
	}
	managerConfig := managerconfigsvc.New(config.Config{}, st, nil)
	service := New(st, managerConfig, server.Client())
	if _, err = service.Replenish(context.Background(), 2); err != nil {
		t.Fatalf("queue first manual task: %v", err)
	}
	if err := service.RunPurchaseTasks(context.Background()); err != nil {
		t.Fatalf("execute first manual task: %v", err)
	}
	if _, err = service.Replenish(context.Background(), 2); err != nil {
		t.Fatalf("queue second manual task: %v", err)
	}
	if err := service.RunPurchaseTasks(context.Background()); err != nil {
		t.Fatalf("execute second manual task: %v", err)
	}
	if _, err = service.Replenish(context.Background(), 2); err != nil {
		t.Fatalf("queue third manual task while slots are full: %v", err)
	}
	if err := service.RunPurchaseTasks(context.Background()); err != nil {
		t.Fatalf("full-slot task scan: %v", err)
	}
	orders, listErr := st.ListOpenSupplyOrders(context.Background(), 10)
	if listErr != nil || len(orders) != 3 || creates.Load() != 2 {
		t.Fatalf("manual open orders=%#v creates=%d err=%v", orders, creates.Load(), listErr)
	}
	tasks, taskErr := st.ListSupplyPurchaseTasks(context.Background(), 10)
	if taskErr != nil || len(tasks) != 3 {
		t.Fatalf("manual tasks=%#v err=%v", tasks, taskErr)
	}
	if tasks[0].Status != "pending" || tasks[1].Status != "running" || tasks[2].Status != "running" {
		t.Fatalf("manual task statuses = %#v", tasks)
	}
}

func TestManualReplenishmentUsesExplicitSupplyPlatform(t *testing.T) {
	var legacyCreates atomic.Int32
	legacyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":100,"missing":0,"estimated_total_fen":100,"estimated_unit_price_fen":50}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":100000,"balance_fen":100000}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			legacyCreates.Add(1)
			_, _ = w.Write([]byte(`{"order":{"id":"legacy-order","status":"waiting_inventory","quantity":2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer legacyServer.Close()

	var bugTeamCreates atomic.Int32
	bugTeamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Customer-Token"); got != "bugteam-token" {
			t.Fatalf("BugTeam customer token = %q", got)
		}
		switch {
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":50,"missing":0,"estimated_total_fen":150,"estimated_unit_price_fen":75}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":100000,"balance_fen":100000}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			bugTeamCreates.Add(1)
			var request struct {
				Product  string `json:"product"`
				Quantity int    `json:"quantity"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode BugTeam create request: %v", err)
			}
			if request.Product != "team_1h" || request.Quantity != 2 {
				t.Fatalf("BugTeam create request = %#v", request)
			}
			_, _ = w.Write([]byte(`{"order_id":"bugteam-order","status":"waiting_inventory","product":"team_1h","quantity":2}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer bugTeamServer.Close()

	cpaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/management/auth-files" {
			_, _ = w.Write([]byte(`{"files":[]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer cpaServer.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "explicit-platform.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	ctx := context.Background()
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: cpaServer.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled:                   &enabled,
			PlatformSelectionStrategy: managerconfigsvc.SupplyPlatformSelectionPriorityFirst,
			Platforms: []store.ManagerSupplyPlatformConfig{
				{ID: "legacy", Name: "sogouedu", Type: managerconfigsvc.SupplyPlatformLegacy, Enabled: &enabled, BaseURL: legacyServer.URL, Token: "legacy-token", Product: "oauth_7d", Priority: 1},
				{ID: "bugteam", Name: "BugTeam", Type: managerconfigsvc.SupplyPlatformBugTeam, Enabled: &enabled, BaseURL: bugTeamServer.URL, Token: "bugteam-token", Product: "team_1h", Priority: 2, EmergencyOnly: true},
			},
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), bugTeamServer.Client())
	if _, err := service.Replenish(ctx, 2, "bugteam"); err != nil {
		t.Fatalf("manual BugTeam replenishment: %v", err)
	}
	if err := service.RunPurchaseTasks(ctx); err != nil {
		t.Fatalf("execute manual BugTeam task: %v", err)
	}
	if bugTeamCreates.Load() != 1 || legacyCreates.Load() != 0 {
		t.Fatalf("create calls: BugTeam=%d legacy=%d", bugTeamCreates.Load(), legacyCreates.Load())
	}
	order, found, err := st.GetSupplyOrder(ctx, "bugteam-order")
	if err != nil || !found {
		t.Fatalf("load BugTeam order: found=%v err=%v", found, err)
	}
	if order.SupplierID != "bugteam" || order.Product != "team_1h" || order.Automatic {
		t.Fatalf("persisted BugTeam order = %#v", order)
	}
}

func TestManualReplenishmentDoesNotFallbackFromUnknownPlatform(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "unknown-platform.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	ctx := context.Background()
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		Enabled: &enabled,
		Platforms: []store.ManagerSupplyPlatformConfig{{
			ID: "legacy", Type: managerconfigsvc.SupplyPlatformLegacy, Enabled: &enabled,
			BaseURL: "https://supplier.invalid", Token: "token", Product: "oauth_7d",
		}},
	}}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil))
	_, err = service.Replenish(ctx, 2, "bugteam")
	if !errors.Is(err, ErrNotConfigured) || !strings.Contains(err.Error(), "bugteam") {
		t.Fatalf("unknown platform error = %v", err)
	}
	orders, listErr := st.ListSupplyOrders(ctx, 10)
	if listErr != nil || len(orders) != 0 {
		t.Fatalf("orders after unknown platform request = %#v err=%v", orders, listErr)
	}
}

func TestAutomaticReplenishmentDoesNotParallelizeOutsideSmartEmergency(t *testing.T) {
	var createCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"files":[]}`))
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":50,"missing":0,"estimated_total_fen":500}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":100000,"balance_fen":100000}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			createCalls.Add(1)
			t.Fatal("non-smart replenishment must not open a parallel order")
		case strings.HasPrefix(r.URL.Path, "/api/customer/pickup/orders/"):
			t.Fatalf("an existing supplier retry-after deadline must not be polled: %s", r.URL.Path)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "parallel-supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	smartDisabled := false
	managerCfg := store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, SmartEnabled: &smartDisabled,
			BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_7d",
			TargetAvailableAccounts: 10, ReplenishBatchSize: 5, MaxConcurrentOrders: 2,
		},
	}
	if err := st.SaveManagerConfig(context.Background(), managerCfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "waiting-order", Product: "oauth_7d", RequestedQuantity: 3, Automatic: true,
		Status: "waiting_inventory", SupplierRetryUntilMS: time.Now().Add(time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatalf("create waiting order: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("reconcile waiting order: %v", err)
	}
	orders, err := st.ListOpenSupplyOrders(context.Background(), 10)
	if err != nil || len(orders) != 1 {
		t.Fatalf("open orders=%#v err=%v", orders, err)
	}
	if createCalls.Load() != 0 {
		t.Fatalf("parallel create calls = %d, want 0", createCalls.Load())
	}
}

func TestAutomaticStartupFloorWithHistoricalCapacityQueuesOneAccount(t *testing.T) {
	var createQuantity atomic.Int32
	authFiles := make([]map[string]string, 0, 11)
	results := make([]store.CodexInspectionResult, 0, 11)
	for index := 0; index < 11; index++ {
		name := fmt.Sprintf("capacity-%02d.json", index)
		authFiles = append(authFiles, map[string]string{"name": name, "provider": "codex", "status": "ready"})
		results = append(results, store.CodexInspectionResult{FileName: name, UsedPercent: floatPtr(0)})
	}
	authPayload, err := json.Marshal(map[string]any{"files": authFiles})
	if err != nil {
		t.Fatalf("marshal auth files: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_, _ = w.Write(authPayload)
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":100,"missing":0,"estimated_total_fen":2000,"estimated_unit_price_fen":2000}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":100000,"balance_fen":100000}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			var request struct {
				Quantity int `json:"quantity"`
			}
			if decodeErr := json.NewDecoder(r.Body).Decode(&request); decodeErr != nil {
				t.Fatalf("decode create request: %v", decodeErr)
			}
			createQuantity.Store(int32(request.Quantity))
			_, _ = fmt.Fprintf(w, `{"order":{"id":"progressive-startup","status":"waiting_inventory","quantity":%d}}`, request.Quantity)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "progressive-startup.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	startupAccounts := 15
	cfg := store.ManagerSupplyConfig{
		Enabled: &enabled, SmartEnabled: &enabled,
		BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_30d",
		Strategy:                 managerconfigsvc.SupplyStrategyStrongSupply,
		StartupAvailableAccounts: &startupAccounts, CriticalAvailableAccounts: 2, HealthyAvailableAccounts: 15,
		HealthyMinutesTarget: 120, WarningMinutes: 100, CriticalMinutes: 80,
		ReplenishBatchSize: 10, PrelockMinQuantity: 1, PrelockMaxQuantity: 10,
		MaxConcurrentOrders: 3, CreateCooldownSeconds: 30,
	}
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply:        cfg,
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	seedCompletedQuotaInspection(t, st, results...)
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("run automatic: %v", err)
	}
	if createQuantity.Load() != 1 {
		t.Fatalf("supplier create quantity = %d, want 1", createQuantity.Load())
	}
	task, found, err := st.GetActiveAutomaticSupplyPurchaseTask(context.Background())
	if err != nil || !found {
		t.Fatalf("active task: found=%v err=%v", found, err)
	}
	if task.TargetQuantity != 1 || task.MaxConcurrentOrders != 1 || task.TriggerReason != "startup_account_floor" {
		t.Fatalf("progressive startup task = %#v", task)
	}
	resource := service.currentSmartResource(cfg)
	if !smartProgressiveStartupFloorRecovery(resource) {
		t.Fatalf("runtime resource lost historical progressive state: %#v", resource)
	}
	orders, err := st.ListOpenSupplyOrders(context.Background(), 10)
	if err != nil || len(orders) != 1 || orders[0].RequestedQuantity != 1 {
		t.Fatalf("progressive startup orders = %#v err=%v", orders, err)
	}
	eligible, err := service.automaticParallelCreateEligible(context.Background(), cfg, orders)
	if err != nil || eligible {
		t.Fatalf("progressive startup parallel eligible=%v err=%v", eligible, err)
	}
}

func TestAutomaticEmergencyReplenishmentCreatesStrongParallelLadderAndStops(t *testing.T) {
	var quantitiesMu sync.Mutex
	quantities := make([]int, 0, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"files":[]}`))
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":100,"missing":0,"estimated_total_fen":100}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":100000,"balance_fen":100000}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			var request struct {
				Quantity int `json:"quantity"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode create request: %v", err)
			}
			quantitiesMu.Lock()
			quantities = append(quantities, request.Quantity)
			orderIndex := len(quantities)
			quantitiesMu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"order":{"id":"emergency-ladder-%d","status":"waiting_inventory","quantity":%d,"retry_after_seconds":60}}`, orderIndex, request.Quantity)
		case strings.HasPrefix(r.URL.Path, "/api/customer/pickup/orders/"):
			t.Fatalf("waiting ladder order was polled ahead of supplier retry-after: %s", r.URL.Path)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "emergency-order-ladder.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	enabled := true
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, SmartEnabled: &enabled,
			BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_7d",
			Strategy:                  managerconfigsvc.SupplyStrategyCustom,
			CriticalAvailableAccounts: 2, HealthyAvailableAccounts: 20, DefaultEmergencyMinAccounts: 5,
			ReplenishBatchSize: 10, PrelockMinQuantity: 1, PrelockMaxQuantity: 10,
			MaxConcurrentOrders: 3,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	seedCompletedQuotaInspection(t, st, store.CodexInspectionResult{
		Provider: "codex", Action: "reauth", Status: "unauthorized", StatusCode: intPtr(http.StatusUnauthorized),
	})

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	for turn := 0; turn < 4; turn++ {
		if err := service.RunAutomatic(ctx); err != nil {
			t.Fatalf("automatic ladder turn %d: %v", turn+1, err)
		}
	}

	quantitiesMu.Lock()
	gotQuantities := append([]int(nil), quantities...)
	quantitiesMu.Unlock()
	if fmt.Sprint(gotQuantities) != "[4 3 3]" {
		t.Fatalf("emergency competition quantities = %v, want [4 3 3]", gotQuantities)
	}
	orders, err := st.ListOpenSupplyOrders(ctx, 10)
	if err != nil || len(orders) != 3 {
		t.Fatalf("open emergency ladder orders=%#v err=%v", orders, err)
	}
	if !strings.HasPrefix(orders[1].TriggerReason, "parallel_") || !strings.HasPrefix(orders[2].TriggerReason, "parallel_") {
		t.Fatalf("secondary ladder orders are not marked parallel: %#v", orders)
	}
}

func TestSmartSuggestedCreateQuantitySubtractsAggregatePrelockedCapacity(t *testing.T) {
	off := false
	cfg := store.ManagerSupplyConfig{
		Product: "oauth_7d", NewAccountConfidence: 1,
		PrelockEnabled: &off, PrelockMaxQuantity: 10, ReplenishBatchSize: 10,
	}
	unit := smartEstimatedNewAccountCapacityRCU(cfg)
	service := New(nil, nil)

	covered := SmartResource{
		SuggestedQuantity: 4, CapacityGapRCU: 4 * unit, PrelockedCapacityRCU: 4 * unit,
		AccountQuantityDeficit: 4, UnitCapacityRCU: smartProductUnitCapacity(cfg.Product),
	}
	if quantity := service.smartSuggestedCreateQuantity(cfg, covered); quantity != 0 {
		t.Fatalf("covered aggregate capacity created %d extra accounts", quantity)
	}

	partial := SmartResource{
		SuggestedQuantity: 7, CapacityGapRCU: 7 * unit, PrelockedCapacityRCU: 4 * unit,
		AccountQuantityDeficit: 7, UnitCapacityRCU: smartProductUnitCapacity(cfg.Product),
	}
	if quantity := service.smartSuggestedCreateQuantity(cfg, partial); quantity != 3 {
		t.Fatalf("remaining aggregate deficit quantity = %d, want 3", quantity)
	}
}

func TestSupplyOrderCapacityUsesMillionTokenEstimate(t *testing.T) {
	cfg := store.ManagerSupplyConfig{Product: "oauth_7d", NewAccountConfidence: 0.7}
	resource := defaultSmartResource(cfg)
	got := estimatedSupplyOrderCapacityRCU(cfg, resource, 5)
	want := smartTokenMillionToRCU(35, resource.UnitCapacityRCU)
	if got != want {
		t.Fatalf("five-account in-flight capacity = %.2f RCU, want %.2f RCU (35M)", got, want)
	}
}

func TestParallelSupplyCreatePlanningCompetesOnWaitingOrdersAndStopsOnCommittedStock(t *testing.T) {
	off := false
	cfg := store.ManagerSupplyConfig{
		Product: "oauth_7d", NewAccountConfidence: 1,
		PrelockEnabled: &off, PrelockMaxQuantity: 10, ReplenishBatchSize: 10,
	}
	unit := smartEstimatedNewAccountCapacityRCU(cfg)
	resource := SmartResource{
		SuggestedQuantity: 7, CapacityGapRCU: 7 * unit, PrelockedCapacityRCU: 4 * unit,
		AccountQuantityDeficit: 7, UnitCapacityRCU: smartProductUnitCapacity(cfg.Product),
		EmergencyShortage: true, HealthLevel: smartHealthCritical,
	}
	waiting := store.SupplyOrder{
		OrderID: "waiting", RequestedQuantity: 4, Automatic: true, Status: "waiting_inventory",
	}
	planning := supplyCreatePlanningResource(cfg, resource, []store.SupplyOrder{waiting}, true)
	if planning.PrelockedCapacityRCU != 0 {
		t.Fatalf("waiting reservation planning capacity = %.2f, want 0", planning.PrelockedCapacityRCU)
	}
	if quantity := New(nil, nil).smartSuggestedCreateQuantity(cfg, planning); quantity != 7 {
		t.Fatalf("parallel waiting-order quantity = %d, want 7", quantity)
	}
	if !openOrdersAllowParallelCreate([]store.SupplyOrder{waiting}) {
		t.Fatal("waiting-inventory order should keep the competition window open")
	}

	ready := waiting
	ready.OrderID = "ready"
	ready.Status = "ready"
	ready.ReadyQuantity = 4
	planning = supplyCreatePlanningResource(cfg, resource, []store.SupplyOrder{ready}, true)
	if planning.PrelockedCapacityRCU != 4*unit {
		t.Fatalf("ready order planning capacity = %.2f, want %.2f", planning.PrelockedCapacityRCU, 4*unit)
	}
	if quantity := New(nil, nil).smartSuggestedCreateQuantity(cfg, planning); quantity != 3 {
		t.Fatalf("quantity after committed stock = %d, want 3", quantity)
	}
	if openOrdersAllowParallelCreate([]store.SupplyOrder{ready}) {
		t.Fatal("ready order should close the competition window")
	}
}

func TestEmergencyParallelOrderQuantityUsesFullSizeCompetition(t *testing.T) {
	resource := SmartResource{
		EmergencyShortage: true,
		HealthLevel:       smartHealthCritical,
	}
	competition := parallelSupplyCompetition{
		anchor: store.SupplyOrder{OrderID: "primary", RequestedQuantity: 10, Status: "waiting_inventory"},
	}

	if got := emergencyParallelOrderQuantity(resource, 10, parallelSupplyCompetition{}, false); got != 10 {
		t.Fatalf("first emergency quantity = %d, want 10", got)
	}
	if got := emergencyParallelOrderQuantity(resource, 10, competition, true); got != 10 {
		t.Fatalf("second emergency quantity = %d, want 10", got)
	}
	continuation := parallelSupplyCompetition{
		anchor: store.SupplyOrder{OrderID: "retry-3", RequestedQuantity: 3, Status: "waiting_inventory"},
	}
	if got := emergencyParallelOrderQuantity(resource, 5, continuation, true); got != 5 {
		t.Fatalf("full-size emergency competition quantity = %d, want 5", got)
	}
	competition.attempts = 1
	if got := emergencyParallelOrderQuantity(resource, 10, competition, true); got != 10 {
		t.Fatalf("third emergency quantity = %d, want 10", got)
	}
	competition.anchor.RequestedQuantity = 17
	if got := emergencyParallelOrderQuantity(resource, 17, competition, true); got != 17 {
		t.Fatalf("third full-size emergency quantity = %d, want 17", got)
	}
	competition.attempts = 2
	if got := emergencyParallelOrderQuantity(resource, 17, competition, true); got != 0 {
		t.Fatalf("exhausted emergency ladder quantity = %d, want 0", got)
	}

	normal := resource
	normal.EmergencyShortage = false
	normal.HealthLevel = smartHealthWarning
	if got := emergencyParallelOrderQuantity(normal, 10, competition, true); got != 10 {
		t.Fatalf("normal parallel quantity = %d, want unchanged 10", got)
	}
}

func TestReadyPartialSupplierStatusIsTakeable(t *testing.T) {
	for _, status := range []string{"ready_partial", "partial_ready", "partially_ready"} {
		if !isReadyForTake(status) || localOrderStatus(status) != "ready" {
			t.Fatalf("supplier status %q was not normalized as ready", status)
		}
	}
}

func TestReadySupplyOrderTakeBudgetIgnoresWaitingReservations(t *testing.T) {
	cfg := store.ManagerSupplyConfig{Product: "oauth_7d", NewAccountConfidence: 1}
	unit := smartEstimatedNewAccountCapacityRCU(cfg)
	ready := store.SupplyOrder{
		ID: 1, OrderID: "ready", RequestedQuantity: 5, ReadyQuantity: 5,
		Automatic: true, Status: "ready", CreatedAtMS: 1,
	}
	waiting := store.SupplyOrder{
		ID: 2, OrderID: "waiting", RequestedQuantity: 100,
		Automatic: true, Status: "waiting_inventory", CreatedAtMS: 2,
	}
	if !readySupplyOrderAccepted(cfg, SmartResource{}, []store.SupplyOrder{ready, waiting}, &ready, 5*unit, unit) {
		t.Fatal("a waiting-inventory reservation displaced the ready order")
	}
}

func TestReadySupplyOrderTakeBudgetPrefersUsefulUnderfillOverLargeOverage(t *testing.T) {
	cfg := store.ManagerSupplyConfig{Product: "oauth_7d", NewAccountConfidence: 1}
	unit := smartEstimatedNewAccountCapacityRCU(cfg)
	orders := []store.SupplyOrder{
		{ID: 1, OrderID: "ready-oldest", RequestedQuantity: 5, ReadyQuantity: 5, Automatic: true, Status: "ready", CreatedAtMS: 1},
		{ID: 2, OrderID: "ready-middle", RequestedQuantity: 5, ReadyQuantity: 5, Automatic: true, Status: "ready", CreatedAtMS: 2},
		{ID: 3, OrderID: "ready-surplus", RequestedQuantity: 5, ReadyQuantity: 5, Automatic: true, Status: "ready", CreatedAtMS: 3},
	}
	need := 6 * unit
	allowance := unit
	if !readySupplyOrderAccepted(cfg, SmartResource{}, orders, &orders[0], need, allowance) {
		t.Fatal("the oldest useful underfill should be accepted")
	}
	if readySupplyOrderAccepted(cfg, SmartResource{}, orders, &orders[1], need, allowance) ||
		readySupplyOrderAccepted(cfg, SmartResource{}, orders, &orders[2], need, allowance) {
		t.Fatal("adding a second quantity-5 order would exceed the aggregate take allowance")
	}
}

func TestReadySupplyOrderTakeBudgetReleasesLaterOrderWhenFirstAlreadyCoversTarget(t *testing.T) {
	cfg := store.ManagerSupplyConfig{Product: "oauth_7d", NewAccountConfidence: 1}
	unit := smartEstimatedNewAccountCapacityRCU(cfg)
	orders := []store.SupplyOrder{
		{ID: 1, OrderID: "ready-first", RequestedQuantity: 5, ReadyQuantity: 5, Automatic: true, Status: "ready", CreatedAtMS: 1},
		{ID: 2, OrderID: "ready-later", RequestedQuantity: 5, ReadyQuantity: 5, Automatic: true, Status: "ready", CreatedAtMS: 2},
	}
	need := 4 * unit
	if !readySupplyOrderAccepted(cfg, SmartResource{}, orders, &orders[0], need, unit) {
		t.Fatal("the first ready order should satisfy the target")
	}
	if readySupplyOrderAccepted(cfg, SmartResource{}, orders, &orders[1], need, unit) {
		t.Fatal("the later ready order should be released as aggregate surplus")
	}
}

func TestReadySupplyOrderTakeBudgetKeepsFirstScarceOrderWithinOverageAllowance(t *testing.T) {
	cfg := store.ManagerSupplyConfig{Product: "oauth_7d", NewAccountConfidence: 1}
	unit := smartEstimatedNewAccountCapacityRCU(cfg)
	orders := []store.SupplyOrder{
		{ID: 1, OrderID: "ready-26", RequestedQuantity: 26, ReadyQuantity: 26, Automatic: true, Status: "ready", CreatedAtMS: 1},
		{ID: 2, OrderID: "ready-15", RequestedQuantity: 15, ReadyQuantity: 15, Automatic: true, Status: "ready", CreatedAtMS: 2},
		{ID: 3, OrderID: "ready-10", RequestedQuantity: 10, ReadyQuantity: 10, Automatic: true, Status: "ready", CreatedAtMS: 3},
	}
	need := 24 * unit
	allowance := need * 0.15
	if !readySupplyOrderAccepted(cfg, SmartResource{}, orders, &orders[0], need, allowance) {
		t.Fatal("the first quantity-26 order should be taken for a live quantity-24 shortage")
	}
	if readySupplyOrderAccepted(cfg, SmartResource{}, orders, &orders[1], need, allowance) ||
		readySupplyOrderAccepted(cfg, SmartResource{}, orders, &orders[2], need, allowance) {
		t.Fatal("later ready orders should wait until the accepted scarce order becomes irreversible")
	}
}

func TestReadySupplyOrderTakeBudgetSelectsExactSmallOrderCombination(t *testing.T) {
	cfg := store.ManagerSupplyConfig{Product: "oauth_7d", NewAccountConfidence: 1}
	unit := smartEstimatedNewAccountCapacityRCU(cfg)
	orders := []store.SupplyOrder{
		{ID: 1, OrderID: "ready-7-a", RequestedQuantity: 7, ReadyQuantity: 7, Automatic: true, Status: "ready", CreatedAtMS: 1},
		{ID: 2, OrderID: "ready-7-b", RequestedQuantity: 7, ReadyQuantity: 7, Automatic: true, Status: "ready", CreatedAtMS: 2},
		{ID: 3, OrderID: "ready-6", RequestedQuantity: 6, ReadyQuantity: 6, Automatic: true, Status: "ready", CreatedAtMS: 3},
	}
	need := 13 * unit
	allowance := unit
	if !readySupplyOrderAccepted(cfg, SmartResource{}, orders, &orders[0], need, allowance) {
		t.Fatal("the first quantity-7 order should be accepted")
	}
	if readySupplyOrderAccepted(cfg, SmartResource{}, orders, &orders[1], need, allowance) {
		t.Fatal("the second quantity-7 order should be excluded in favor of the exact 7 + 6 combination")
	}
	if !readySupplyOrderAccepted(cfg, SmartResource{}, orders, &orders[2], need, allowance) {
		t.Fatal("the quantity-6 order should complete the exact live-deficit combination")
	}
}

func TestReadyOrderUsesLiveDeficitBeforeItsOwnPrelockedCapacity(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "ready-live-deficit.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := store.ManagerSupplyConfig{Product: "oauth_7d", NewAccountConfidence: 1}
	unit := smartEstimatedNewAccountCapacityRCU(cfg)
	order, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "ready-26", Product: cfg.Product, RequestedQuantity: 26, ReadyQuantity: 26,
		Automatic: true, Status: "ready", CreatedAtMS: 1,
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil))
	resource := SmartResource{
		HealthLevel:          smartHealthCritical,
		ConsumeRCUPerMinute:  unit,
		TargetCapacityRCU:    24 * unit,
		CurrentCapacityRCU:   0,
		PrelockedCapacityRCU: 26 * unit,
		CapacityGapRCU:       0,
	}
	release, accepted, reason, err := service.shouldReleaseOversizedOpenOrder(ctx, cfg, resource, &order)
	if err != nil {
		t.Fatalf("evaluate ready order: %v", err)
	}
	if release || !accepted || reason != "low_water_take_ready" {
		t.Fatalf("ready order decision release=%v accepted=%v reason=%q", release, accepted, reason)
	}
}

func TestReadySupplyOrderTakeBudgetChoosesClosestCapacityCombination(t *testing.T) {
	cfg := store.ManagerSupplyConfig{Product: "oauth_7d", NewAccountConfidence: 1}
	unit := smartEstimatedNewAccountCapacityRCU(cfg)
	orders := []store.SupplyOrder{
		{ID: 1, OrderID: "ready-30", RequestedQuantity: 30, ReadyQuantity: 30, Automatic: true, Status: "ready", CreatedAtMS: 1},
		{ID: 2, OrderID: "ready-15", RequestedQuantity: 15, ReadyQuantity: 15, Automatic: true, Status: "ready", CreatedAtMS: 2},
		{ID: 3, OrderID: "ready-6", RequestedQuantity: 6, ReadyQuantity: 6, Automatic: true, Status: "ready", CreatedAtMS: 3},
	}
	need := 20 * unit
	if readySupplyOrderAccepted(cfg, SmartResource{}, orders, &orders[0], need, unit) {
		t.Fatal("older quantity-30 order should not beat the closer quantity-15 + quantity-6 combination")
	}
	if !readySupplyOrderAccepted(cfg, SmartResource{}, orders, &orders[1], need, unit) ||
		!readySupplyOrderAccepted(cfg, SmartResource{}, orders, &orders[2], need, unit) {
		t.Fatal("quantity-15 + quantity-6 orders should cover the deficit with minimum overage")
	}
}

func TestReadySupplyOrderTakeBudgetCountsInProgressAndActualReadyQuantity(t *testing.T) {
	cfg := store.ManagerSupplyConfig{Product: "oauth_7d", NewAccountConfidence: 1}
	unit := smartEstimatedNewAccountCapacityRCU(cfg)
	inProgress := store.SupplyOrder{
		ID: 1, OrderID: "importing-5", RequestedQuantity: 20, ItemCount: 5,
		Automatic: true, Status: "importing", CreatedAtMS: 1,
	}
	readyLargeRequest := store.SupplyOrder{
		ID: 2, OrderID: "ready-6-of-30", RequestedQuantity: 30, ReadyQuantity: 6,
		Automatic: true, Status: "ready", CreatedAtMS: 2,
	}
	if got := supplyOrderCapacityRCU(cfg, SmartResource{}, readyLargeRequest); got != 6*unit {
		t.Fatalf("partial ready capacity = %.2f, want %.2f", got, 6*unit)
	}
	orders := []store.SupplyOrder{inProgress, readyLargeRequest}
	if !readySupplyOrderAccepted(cfg, SmartResource{}, orders, &inProgress, 10*unit, unit) {
		t.Fatal("an in-progress import is an irreversible accepted commitment")
	}
	if !readySupplyOrderAccepted(cfg, SmartResource{}, orders, &readyLargeRequest, 10*unit, unit) {
		t.Fatal("actual six ready accounts should supplement the five already importing")
	}
	if readySupplyOrderAccepted(cfg, SmartResource{}, orders, &readyLargeRequest, 5*unit, unit) {
		t.Fatal("no additional order should be taken after in-progress accounts already cover the deficit")
	}
}

func TestNormalizeSub2AccountPayloadForCPA(t *testing.T) {
	raw := `{"name":"team-user","type":"oauth","platform":"openai","priority":2,"concurrency":8,"extra":{"organization_id":"org-extra","lastRefresh":"2026-07-01T00:00:00Z"},"credentials":{"access_token":"access","refresh_token":"refresh","chatgpt_account_id":"account-1","email":"user@example.com","plan_type":"free","chatgpt_plan_type":"team","workspaceId":"workspace-1","expires_at":"2026-07-30T00:00:00Z"}}`
	payload, key, fileName, err := normalizeAccountPayload([]byte(raw))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode normalized payload: %v", err)
	}
	if result["type"] != "codex" || result["access_token"] != "access" || result["refresh_token"] != "refresh" {
		t.Fatalf("normalized credentials = %#v", result)
	}
	if _, ok := result["credentials"]; ok {
		t.Fatalf("Sub2 credentials wrapper was not removed: %#v", result)
	}
	if result["account_id"] != "account-1" || result["chatgpt_account_id"] != "account-1" ||
		result["email"] != "user@example.com" || result["plan_type"] != "team" ||
		result["chatgpt_plan_type"] != "team" || result["codex_plan_type_pinned"] != true ||
		result["organization_id"] != "org-extra" ||
		result["workspace_id"] != "workspace-1" ||
		result["expired"] != "2026-07-30T00:00:00Z" ||
		result["selection_error_freeze_seconds"] != float64(0) || result["codex_cli_only"] != true ||
		result["codex_cli_only_allow_app_server"] != true || stringFromMap(result, "codex_identity_fingerprint") == "" {
		t.Fatalf("normalized metadata = %#v", result)
	}
	if _, exists := result["max_concurrency"]; exists {
		t.Fatalf("supplier concurrency must not be imported into CPA metadata: %#v", result)
	}
	if _, exists := result["priority"]; exists {
		t.Fatalf("supplier priority must not split the CPA scheduling pool: %#v", result)
	}
	if len(key) != 64 || fileName != stableSupplyAccountFileName("user@example.com", "workspace-1") {
		t.Fatalf("stable identity outputs key=%q file=%q", key, fileName)
	}
}

func TestNormalizeDirectCPAAccountPayloadStripsSupplierSchedulerLimitsAndDisablesSelectionErrorFreeze(t *testing.T) {
	payload, _, _, err := normalizeAccountPayload([]byte(`{"type":"codex","email":"direct@example.com","account_id":"direct-account","access_token":"access","priority":50,"max_concurrency":8,"selection_error_freeze_seconds":45}`))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode normalized payload: %v", err)
	}
	if _, exists := result["max_concurrency"]; exists {
		t.Fatalf("supplier concurrency must not be imported into CPA metadata: %#v", result)
	}
	if _, exists := result["priority"]; exists {
		t.Fatalf("supplier priority must not split the CPA scheduling pool: %#v", result)
	}
	if result["selection_error_freeze_seconds"] != float64(0) ||
		result["codex_cli_only"] != true || result["codex_cli_only_allow_app_server"] != true ||
		stringFromMap(result, "codex_identity_fingerprint") == "" {
		t.Fatalf("normalized runtime limits = %#v", result)
	}
}

func TestSupplyFileNameSeparatesDifferentAccountsWithSameEmail(t *testing.T) {
	first, err := normalizeAccountPayloads([]byte(`{"name":"普通 Team · 7D · 有效期 51 分钟","type":"oauth","platform":"openai","credentials":{"access_token":"access-one","refresh_token":"refresh-one","chatgpt_account_id":"account-one","email":"stable@example.com"}}`))
	if err != nil || len(first) != 1 {
		t.Fatalf("normalize first account: %#v err=%v", first, err)
	}
	second, err := normalizeAccountPayloads([]byte(`{"name":"普通 Team · 7D · 有效期 2 分钟","type":"oauth","platform":"openai","credentials":{"access_token":"access-two","refresh_token":"refresh-two","chatgpt_account_id":"account-two","email":"stable@example.com"}}`))
	if err != nil || len(second) != 1 {
		t.Fatalf("normalize replacement account: %#v err=%v", second, err)
	}
	if first[0].accountName != "stable@example.com" || second[0].accountName != first[0].accountName ||
		first[0].fileName == second[0].fileName {
		t.Fatalf("different account filenames first=%q second=%q", first[0].fileName, second[0].fileName)
	}
	if first[0].itemKey == second[0].itemKey {
		t.Fatal("credential identity should still distinguish different underlying accounts")
	}
}

func TestNormalizeDirectCPAJWTSeparatesSharedWorkspaceMembers(t *testing.T) {
	const workspaceID = "3bbcf0cc-c729-4062-9455-649a9190d673"
	buildAccount := func(email string, memberID string, tokenMarker string) normalizedSupplyAccount {
		t.Helper()
		token := buildSupplyTestJWT(t, map[string]any{
			"sub":   "auth0|" + tokenMarker,
			"email": email,
			"https://api.openai.com/auth": map[string]any{
				"chatgpt_account_id":      workspaceID,
				"chatgpt_account_user_id": memberID + "__" + workspaceID,
				"chatgpt_user_id":         memberID,
				"user_id":                 memberID,
				"poid":                    "org-" + tokenMarker,
			},
		})
		raw, err := json.Marshal(map[string]any{
			"type":               "codex",
			"email":              email,
			"account_id":         workspaceID,
			"chatgpt_account_id": workspaceID,
			"access_token":       token,
		})
		if err != nil {
			t.Fatalf("marshal account: %v", err)
		}
		account, err := normalizeAccountForImport(string(raw))
		if err != nil {
			t.Fatalf("normalize account: %v", err)
		}
		return account
	}

	first := buildAccount("taliapalk11093@outlook.com", "user-Lu9u9QrXpBTOuEwK14qBHUIv", "first")
	second := buildAccount("yoshiohumpal42085@outlook.com", "user-H3Ibbb6Qvpx4FJ29qBlI8GFG", "second")
	if first.itemKey == second.itemKey || first.nameKey == second.nameKey || first.fileName == second.fileName {
		t.Fatalf("shared-workspace members collided: first=%#v second=%#v", first, second)
	}
	var firstPayload map[string]any
	if err := json.Unmarshal(first.payload, &firstPayload); err != nil {
		t.Fatalf("decode first payload: %v", err)
	}
	if stringFromMap(firstPayload, "workspace_id") != workspaceID ||
		stringFromMap(firstPayload, "chatgpt_user_id") != "user-Lu9u9QrXpBTOuEwK14qBHUIv" {
		t.Fatalf("JWT identity was not extracted: %#v", firstPayload)
	}
	firstFile := cpaauthfiles.File{
		AccountID:       workspaceID,
		AccountSnapshot: "taliapalk11093@outlook.com",
		Raw: map[string]any{
			"account_id":      workspaceID,
			"workspace_id":    workspaceID,
			"chatgpt_user_id": "user-Lu9u9QrXpBTOuEwK14qBHUIv",
			"email":           "taliapalk11093@outlook.com",
		},
	}
	if !supplyCPAFileMatchesAccount(firstFile, first) {
		t.Fatal("same workspace member should match its CPA file")
	}
	if supplyCPAFileMatchesAccount(firstFile, second) {
		t.Fatal("different members in one shared workspace must not match")
	}

	firstRefreshed := buildAccount("taliapalk11093@outlook.com", "user-Lu9u9QrXpBTOuEwK14qBHUIv", "refreshed")
	if firstRefreshed.itemKey != first.itemKey || firstRefreshed.fileName != first.fileName {
		t.Fatalf("same member refresh changed identity: first=%#v refreshed=%#v", first, firstRefreshed)
	}
}

func TestNormalizeSupplyAccountKeepsExplicitCodexIdentityFingerprint(t *testing.T) {
	payload, _, _, err := normalizeAccountPayload([]byte(`{"type":"codex","email":"stable@example.com","account_id":"account-a","access_token":"access","codex_identity_fingerprint":"persisted-device"}`))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got := stringFromMap(result, "codex_identity_fingerprint"); got != "persisted-device" {
		t.Fatalf("codex_identity_fingerprint = %q, want persisted-device", got)
	}
}

func TestNormalizeSupplyAccountClearsSupplierRuntimeState(t *testing.T) {
	payload, _, _, err := normalizeAccountPayload([]byte(`{
		"platform":"openai",
		"type":"oauth",
		"email":"fresh@example.com",
		"disabled":true,
		"status":"disabled",
		"credentials":{
			"type":"codex",
			"email":"fresh@example.com",
			"access_token":"access",
			"refresh_token":"refresh",
			"disabled":true,
			"status":"unauthorized",
			"status_message":"stale supplier runtime state",
			"runtime_status":"quota_exhausted",
			"last_error":"stale supplier error",
			"initialization_state":"initialization_failed",
			"recovery_state":"recovery_failed"
		}
	}`))
	if err != nil {
		t.Fatalf("normalize supplied account: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode normalized payload: %v", err)
	}
	if disabled, ok := result["disabled"].(bool); !ok || disabled {
		t.Fatalf("disabled = %#v, want false", result["disabled"])
	}
	for _, key := range []string{
		"status", "status_message", "runtime_status", "last_error",
		"initialization_state", "recovery_state",
	} {
		if value, exists := result[key]; exists {
			t.Fatalf("runtime field %q survived normalization: %#v", key, value)
		}
	}
}

func TestPreserveCodexIdentityFingerprintOnReplacement(t *testing.T) {
	next := []byte(`{"type":"codex","email":"new@example.com","codex_identity_fingerprint":"new-device","access_token":"new"}`)
	existing := []byte(`{"type":"codex","email":"old@example.com","codex_identity_fingerprint":"stable-device","access_token":"old"}`)
	preserved := preserveCodexSupplyMetadata(next, existing)
	var result map[string]any
	if err := json.Unmarshal(preserved, &result); err != nil {
		t.Fatalf("decode preserved payload: %v", err)
	}
	if got := stringFromMap(result, "codex_identity_fingerprint"); got != "stable-device" {
		t.Fatalf("codex_identity_fingerprint = %q, want stable-device", got)
	}
	if got := stringFromMap(result, "access_token"); got != "new" {
		t.Fatalf("access_token = %q, want new", got)
	}
}

func TestPreserveCodexIdentityFingerprintBackfillsLegacyExistingAccount(t *testing.T) {
	next := []byte(`{"type":"codex","email":"new@example.com","codex_identity_fingerprint":"new-device","access_token":"new"}`)
	existing := []byte(`{"type":"codex","email":"old@example.com","workspace_id":"workspace-one","chatgpt_user_id":"member-one","access_token":"old"}`)
	preserved := preserveCodexSupplyMetadata(next, existing)
	var result map[string]any
	if err := json.Unmarshal(preserved, &result); err != nil {
		t.Fatalf("decode preserved payload: %v", err)
	}
	var existingMetadata map[string]any
	if err := json.Unmarshal(existing, &existingMetadata); err != nil {
		t.Fatalf("decode existing payload: %v", err)
	}
	want := stableCodexIdentityFingerprint(supplyAccountIdentity(existingMetadata))
	if got := stringFromMap(result, "codex_identity_fingerprint"); got == "" || got != want {
		t.Fatalf("codex_identity_fingerprint = %q, want %q", got, want)
	}
}

func TestPreservePinnedSupplyTeamPlanOnTransientFreeReplacement(t *testing.T) {
	next := []byte(`{"type":"codex","email":"team@example.com","plan_type":"free","chatgpt_plan_type":"free","access_token":"new"}`)
	existing := []byte(`{"type":"codex","email":"team@example.com","import_format":"sub2api","plan_type":"team","chatgpt_plan_type":"team","access_token":"old"}`)
	preserved := preserveCodexSupplyMetadata(next, existing)
	var result map[string]any
	if err := json.Unmarshal(preserved, &result); err != nil {
		t.Fatalf("decode preserved payload: %v", err)
	}
	if got := stringFromMap(result, "plan_type"); got != "team" {
		t.Fatalf("plan_type = %q, want team", got)
	}
	if got := stringFromMap(result, "chatgpt_plan_type"); got != "team" {
		t.Fatalf("chatgpt_plan_type = %q, want team", got)
	}
	if !boolField(result, "codex_plan_type_pinned") {
		t.Fatalf("codex_plan_type_pinned = %#v, want true", result["codex_plan_type_pinned"])
	}
	if got := stringFromMap(result, "access_token"); got != "new" {
		t.Fatalf("access_token = %q, want new", got)
	}
}

func TestPreserveCodexSupplyMetadataDoesNotRestoreLeaseWhenNextPayloadHasWarranty(t *testing.T) {
	warrantyExpiresAtMS := time.Now().Add(45 * time.Minute).UnixMilli()
	legacyLeaseExpiresAtMS := time.Now().Add(30 * time.Minute).UnixMilli()
	next := []byte(fmt.Sprintf(`{"type":"codex","access_token":"new","supply_warranty_expires_at_ms":%d,"supply_warranty_expires_at":"%s"}`,
		warrantyExpiresAtMS, time.UnixMilli(warrantyExpiresAtMS).UTC().Format(time.RFC3339)))
	existing := []byte(fmt.Sprintf(`{"type":"codex","access_token":"old","supply_lease_expires_at_ms":%d,"supply_lease_expires_at":"%s"}`,
		legacyLeaseExpiresAtMS, time.UnixMilli(legacyLeaseExpiresAtMS).UTC().Format(time.RFC3339)))
	preserved := preserveCodexSupplyMetadata(next, existing)
	var result map[string]any
	if err := json.Unmarshal(preserved, &result); err != nil {
		t.Fatalf("decode preserved payload: %v", err)
	}
	if got := int64(numberField(result, "supply_warranty_expires_at_ms")); got != warrantyExpiresAtMS {
		t.Fatalf("warranty expiry = %d, want %d", got, warrantyExpiresAtMS)
	}
	if got := int64(numberField(result, "supply_lease_expires_at_ms")); got != 0 {
		t.Fatalf("scheduling lease returned = %d", got)
	}
	if got := stringFromMap(result, "supply_lease_expires_at"); got != "" {
		t.Fatalf("scheduling lease timestamp returned = %q", got)
	}
	if got := stringFromMap(result, "access_token"); got != "new" {
		t.Fatalf("access_token = %q, want new", got)
	}
}

func TestBackfillSupplyAccountMetadataRenamesLegacySupplierLabelFile(t *testing.T) {
	const oldFileName = "codex-普通-team-7d-有效期-51-分钟.json"
	files := map[string]map[string]any{
		oldFileName: {
			"name":     oldFileName,
			"id":       oldFileName,
			"provider": "codex",
			"account":  "stable@example.com",
		},
	}
	var uploadedName string
	var deletedName string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/auth-files" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			name := r.URL.Query().Get("name")
			listed := make([]map[string]any, 0, len(files))
			for fileName, file := range files {
				if name == "" || name == fileName {
					listed = append(listed, file)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"files": listed})
		case http.MethodPost:
			file, header, err := r.FormFile("file")
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer file.Close()
			payload, err := io.ReadAll(file)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var account map[string]any
			if err := json.Unmarshal(payload, &account); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			uploadedName = header.Filename
			files[uploadedName] = map[string]any{
				"name":     uploadedName,
				"id":       uploadedName,
				"provider": "codex",
				"account":  account["email"],
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case http.MethodDelete:
			deletedName = r.URL.Query().Get("name")
			delete(files, deletedName)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply-name-backfill.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{OrderID: "legacy-name-order", Product: "oauth_7d", Status: "completed"}); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := st.InsertSupplyImportItems(ctx, "legacy-name-order", []store.SupplyImportItem{{
		ItemKey: "legacy-name-key", AccountName: "普通 Team · 7D · 有效期 51 分钟", NameKey: "普通-team-7d-有效期-51-分钟",
		FileName: oldFileName, ImportAction: "add",
		PayloadJSON: `{"type":"codex","name":"普通 Team · 7D · 有效期 51 分钟","account":"stable@example.com","account_id":"account-stable","access_token":"access"}`,
	}}); err != nil {
		t.Fatalf("insert import item: %v", err)
	}
	items, err := st.ListSupplyImportItems(ctx, 10, "")
	if err != nil || len(items) != 1 {
		t.Fatalf("list import items: %#v err=%v", items, err)
	}
	if err := st.MarkSupplyImportItemImported(ctx, items[0].ID, time.Now().UnixMilli()); err != nil {
		t.Fatalf("mark imported: %v", err)
	}

	service := New(st, nil, server.Client())
	cfg := store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
	}
	if err := service.backfillSupplyAccountMetadata(ctx, cfg); err != nil {
		t.Fatalf("backfill supply account metadata: %v", err)
	}
	items, err = st.ListSupplyImportItems(ctx, 10, "")
	if err != nil || len(items) != 1 {
		t.Fatalf("reload import items: %#v err=%v", items, err)
	}
	expectedFileName := stableSupplyAccountFileName("stable@example.com", "account-stable")
	if uploadedName != expectedFileName || deletedName != oldFileName {
		t.Fatalf("migration uploaded=%q deleted=%q", uploadedName, deletedName)
	}
	if items[0].AccountName != "stable@example.com" || items[0].NameKey != supplyAccountNameKey("stable@example.com", "account-stable") || items[0].FileName != expectedFileName {
		t.Fatalf("migrated import item = %#v", items[0])
	}
	if _, exists := files[oldFileName]; exists {
		t.Fatalf("legacy file still exists: %#v", files)
	}
	if _, exists := files[expectedFileName]; !exists {
		t.Fatalf("canonical file missing: %#v", files)
	}
}

func TestReplacingSupplyImportSupersedesPreviousFileVersion(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply-lineage.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for _, orderID := range []string{"original-order", "recovery-order"} {
		if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{OrderID: orderID, Product: "oauth_30d", Status: "completed"}); err != nil {
			t.Fatalf("create %s: %v", orderID, err)
		}
	}
	if _, err := st.InsertSupplyImportItems(ctx, "original-order", []store.SupplyImportItem{{
		ItemKey: "original", AccountName: "Stable Team", NameKey: "stable-team", FileName: "codex-stable-team.json",
		ImportAction: "add", PayloadJSON: `{"type":"codex","name":"Stable Team","account_id":"original"}`,
	}}); err != nil {
		t.Fatalf("insert original: %v", err)
	}
	items, _ := st.ListSupplyImportItems(ctx, 10, "")
	if err := st.MarkSupplyImportItemImported(ctx, items[0].ID, 1000); err != nil {
		t.Fatalf("mark original imported: %v", err)
	}
	if _, err := st.InsertSupplyImportItems(ctx, "recovery-order", []store.SupplyImportItem{{
		ItemKey: "replacement", AccountName: "Stable Team", NameKey: "stable-team", FileName: "codex-stable-team.json",
		ImportAction: "replace", ReplacedFileName: "codex-stable-team.json", PayloadJSON: `{"type":"codex","name":"Stable Team","account_id":"replacement"}`,
	}}); err != nil {
		t.Fatalf("insert replacement: %v", err)
	}
	items, _ = st.ListSupplyImportItems(ctx, 10, "")
	var replacementID int64
	for _, item := range items {
		if item.OrderID == "recovery-order" {
			replacementID = item.ID
		}
	}
	if err := st.MarkSupplyImportItemImported(ctx, replacementID, 2000); err != nil {
		t.Fatalf("mark replacement imported: %v", err)
	}
	items, err = st.ListSupplyImportItems(ctx, 10, "")
	if err != nil || len(items) != 2 {
		t.Fatalf("lineage items=%#v err=%v", items, err)
	}
	var original, replacement store.SupplyImportItem
	for _, item := range items {
		if item.OrderID == "original-order" {
			original = item
		} else {
			replacement = item
		}
	}
	if original.SupersededAtMS != 2000 || replacement.SupersedesItemID != original.ID || replacement.EffectiveFromMS != 2000 {
		t.Fatalf("lineage original=%#v replacement=%#v", original, replacement)
	}
}

func TestStableFileCredentialVersionsRemainDistinctAndUseTheOverlappingVersion(t *testing.T) {
	items := []store.SupplyImportItem{
		{
			ID: 1, OrderID: "original-order", ItemKey: "original", FileName: "codex-stable-team.json",
			Status: "imported", ImportedAtMS: 100, EffectiveFromMS: 100, SupersededAtMS: 250,
		},
		{
			ID: 2, OrderID: "recovery-order", ItemKey: "replacement", FileName: "codex-stable-team.json",
			Status: "imported", ImportedAtMS: 250, EffectiveFromMS: 250,
		},
	}
	merged := mergeSupplyImportItems(items, items[:1])
	if len(merged) != 2 {
		t.Fatalf("merged credential versions = %#v, want two distinct rows", merged)
	}
	usage := map[string]supplyAccountUsage{
		"codex-stable-team.json": {Calls: 7, SuccessCalls: 6, FailureCalls: 1, Tokens: 700},
	}
	beforeReplacement := buildReportReconciliation(
		ReportRequest{FromMS: 150, ToMS: 200}, nil, nil, merged, map[string]store.SupplyOrder{}, usage, nil, time.UnixMilli(200),
	)
	if len(beforeReplacement.Accounts) != 2 || beforeReplacement.Accounts[0].UsageCalls != 7 || beforeReplacement.Accounts[1].UsageCalls != 0 {
		t.Fatalf("historical usage attribution = %#v", beforeReplacement.Accounts)
	}
	afterReplacement := buildReportReconciliation(
		ReportRequest{FromMS: 260, ToMS: 300}, nil, nil, merged, map[string]store.SupplyOrder{}, usage, nil, time.UnixMilli(300),
	)
	if len(afterReplacement.Accounts) != 2 || afterReplacement.Accounts[0].UsageCalls != 0 || afterReplacement.Accounts[1].UsageCalls != 7 {
		t.Fatalf("replacement usage attribution = %#v", afterReplacement.Accounts)
	}
}

func TestRecoveryWithoutOriginalIdentityDoesNotReplaceDifferentAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/auth-files" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"files":[{"name":"legacy-stable-file.json","auth_index":"17","provider":"codex","account":"old@example.com","account_id":"old-account","disabled":true,"status":"unauthorized"}]}`))
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "recovery-name-binding.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{OrderID: "original", Product: "oauth_30d", Status: "completed"}); err != nil {
		t.Fatalf("create original order: %v", err)
	}
	if _, err := st.InsertSupplyImportItems(ctx, "original", []store.SupplyImportItem{{
		ItemKey: "old-key", AccountName: "old@example.com", NameKey: "old@example.com", FileName: "legacy-stable-file.json",
		ImportAction: "add", PayloadJSON: `{"type":"codex","name":"Stable Team","email":"old@example.com","account_id":"old-account"}`,
	}}); err != nil {
		t.Fatalf("insert original item: %v", err)
	}
	items, _ := st.ListSupplyImportItems(ctx, 10, "")
	if err := st.MarkSupplyImportItemImported(ctx, items[0].ID, 1000); err != nil {
		t.Fatalf("mark original imported: %v", err)
	}
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{OrderID: "recovery-rec-name", Product: "oauth_30d", Strategy: "recovery", Status: "completed"}); err != nil {
		t.Fatalf("create recovery order: %v", err)
	}
	if _, err := st.UpsertSupplyRecoveries(ctx, []store.SupplyRecovery{{
		RecoveryID: "rec-name", DeliveryStatus: "claimed", Status: "importing", ClaimOrderID: "recovery-rec-name", LastSeenAtMS: 1000,
	}}); err != nil {
		t.Fatalf("insert recovery: %v", err)
	}
	account, err := normalizeAccountForImport(`{"type":"codex","name":"Stable Team","email":"old@example.com","account_id":"new-account","access_token":"new-token"}`)
	if err != nil {
		t.Fatalf("normalize replacement: %v", err)
	}
	service := New(st, nil, server.Client())
	plan, err := service.resolveSupplyImportPlan(ctx, store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
	}, store.SupplyOrder{OrderID: "recovery-rec-name", Strategy: "recovery"}, store.SupplyImportItem{}, account, true)
	if err != nil {
		t.Fatalf("resolve recovery plan: %v", err)
	}
	if plan.action != "add" || plan.fileName != stableSupplyAccountFileName("old@example.com", "new-account") || plan.replacedFileName != "" {
		t.Fatalf("recovery plan = %#v", plan)
	}
	secondAccount, err := normalizeAccountForImport(`{"type":"codex","name":"Second Team","email":"second@example.com","account_id":"second-account","access_token":"second-token"}`)
	if err != nil {
		t.Fatalf("normalize second replacement: %v", err)
	}
	secondPlan, err := service.resolveSupplyImportPlan(ctx, store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
	}, store.SupplyOrder{OrderID: "recovery-rec-name", Strategy: "recovery"}, store.SupplyImportItem{}, secondAccount, false)
	if err != nil {
		t.Fatalf("resolve second recovery plan: %v", err)
	}
	if secondPlan.action != "add" || secondPlan.fileName != stableSupplyAccountFileName("second@example.com", "second-account") {
		t.Fatalf("second recovery plan reused original file: %#v", secondPlan)
	}
}

func TestNormalizeSub2BundlePayloadForCPA(t *testing.T) {
	raw := `{"type":"sub2api-data","exported_at":"2026-07-30T17:28:18Z","accounts":[{"name":"team-one","type":"oauth","platform":"openai","priority":2,"concurrency":8,"credentials":{"access_token":"access-one","refresh_token":"refresh-one","chatgpt_account_id":"account-shared","email":"one@example.com","plan_type":"team","expires_at":1786296161,"expires_in":864000,"workspace_id":"workspace-one"}},{"name":"team-two","type":"oauth","platform":"openai","credentials":{"session_access_token":"access-two","refresh_token":"refresh-two","account_id":"account-shared","email":"two@example.com","chatgpt_plan_type":"team"}}]}`
	accounts, err := normalizeAccountPayloads([]byte(raw))
	if err != nil {
		t.Fatalf("normalize bundle: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("normalized accounts = %d, want 2", len(accounts))
	}
	if accounts[0].fileName == accounts[1].fileName || accounts[0].itemKey == accounts[1].itemKey {
		t.Fatalf("bundle accounts should keep distinct identities: %#v", accounts)
	}
	var first map[string]any
	if err := json.Unmarshal(accounts[0].payload, &first); err != nil {
		t.Fatalf("decode first payload: %v", err)
	}
	if first["type"] != "codex" || first["import_format"] != "sub2api" || first["access_token"] != "access-one" || first["refresh_token"] != "refresh-one" {
		t.Fatalf("first normalized payload = %#v", first)
	}
	if first["selection_error_freeze_seconds"] != float64(0) {
		t.Fatalf("first normalized payload freeze setting = %#v", first)
	}
	if first["codex_cli_only"] != true || first["codex_cli_only_allow_app_server"] != true {
		t.Fatalf("first normalized payload client restriction = %#v", first)
	}
	if _, nested := first["credentials"]; nested {
		t.Fatalf("credentials wrapper was not removed: %#v", first)
	}
	if first["chatgpt_account_id"] != "account-shared" || first["account_id"] != "account-shared" || first["email"] != "one@example.com" || first["workspace_id"] != "workspace-one" {
		t.Fatalf("credential metadata was not preserved: %#v", first)
	}
	if first["expired"] != "1786296161" || first["expires_at"] != "1786296161" || first["last_refresh"] != "2026-07-30T17:28:18Z" {
		t.Fatalf("time metadata was not normalized: %#v", first)
	}
	var second map[string]any
	if err := json.Unmarshal(accounts[1].payload, &second); err != nil {
		t.Fatalf("decode second payload: %v", err)
	}
	if second["access_token"] != "access-two" || second["account_id"] != "account-shared" || second["email"] != "two@example.com" {
		t.Fatalf("session access token account was not normalized: %#v", second)
	}
	if second["selection_error_freeze_seconds"] != float64(0) {
		t.Fatalf("second normalized payload freeze setting = %#v", second)
	}
	if second["codex_cli_only"] != true || second["codex_cli_only_allow_app_server"] != true {
		t.Fatalf("second normalized payload client restriction = %#v", second)
	}
}

func TestNormalizeNvtokensWrappedPayloadsForCPA(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		access string
	}{
		{
			name:   "account json",
			raw:    `{"account_json":{"type":"oauth","platform":"openai","credentials":{"access_token":"access-json","refresh_token":"refresh-json","account_id":"account-json"}}}`,
			access: "access-json",
		},
		{
			name:   "card payload sub2api account",
			raw:    `{"card_payload":{"sub2api_account":{"type":"oauth","platform":"openai","credentials":{"access_token":"access-sub2","refresh_token":"refresh-sub2","account_id":"account-sub2"}}}}`,
			access: "access-sub2",
		},
		{
			name:   "card payload codex account",
			raw:    `{"card_payload":{"codex_account":{"type":"codex","access_token":"access-codex","refresh_token":"refresh-codex","account_id":"account-codex"}}}`,
			access: "access-codex",
		},
		{
			name:   "string account json",
			raw:    `{"account_json":"{\"type\":\"codex\",\"access_token\":\"access-string\",\"refresh_token\":\"refresh-string\",\"account_id\":\"account-string\"}"}`,
			access: "access-string",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			accounts, err := normalizeAccountPayloads([]byte(test.raw))
			if err != nil {
				t.Fatalf("normalize payload: %v", err)
			}
			if len(accounts) != 1 {
				t.Fatalf("accounts = %d, want 1", len(accounts))
			}
			var account map[string]any
			if err := json.Unmarshal(accounts[0].payload, &account); err != nil {
				t.Fatalf("decode normalized account: %v", err)
			}
			if account["access_token"] != test.access {
				t.Fatalf("normalized account = %#v", account)
			}
		})
	}
}

func TestNormalizeNvtokensOAuthCredentialTypeForCPA(t *testing.T) {
	raw := `{"name":"buyer@example.com","type":"oauth","platform":"openai","credentials":{"name":"Buyer","type":"oauth","email":"buyer@example.com","access_token":"access-real","refresh_token":"refresh-real","account_id":"account-real","chatgpt_plan_type":"plus"}}`
	accounts, err := normalizeAccountPayloads([]byte(raw))
	if err != nil || len(accounts) != 1 {
		t.Fatalf("normalize real nvtokens payload: accounts=%d err=%v", len(accounts), err)
	}
	var account map[string]any
	if err := json.Unmarshal(accounts[0].payload, &account); err != nil {
		t.Fatalf("decode normalized account: %v", err)
	}
	if account["access_token"] != "access-real" || account["refresh_token"] != "refresh-real" ||
		account["email"] != "buyer@example.com" || account["chatgpt_plan_type"] != "plus" {
		t.Fatalf("normalized account = %#v", account)
	}
}

func TestSupplyDeliveryLeaseUsesRemainingValidityInsteadOfOAuthExpiry(t *testing.T) {
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	shortLease := supplyDeliveryLeaseExpiresAtMS(map[string]any{
		"remaining_seconds": 900,
		"expires_at":        now.Add(24 * time.Hour).Unix(),
	}, now)
	if got, want := shortLease, now.Add(15*time.Minute).UnixMilli(); got != want {
		t.Fatalf("short supplier lease = %d, want %d", got, want)
	}
	defaultLease := supplyDeliveryLeaseExpiresAtMS(map[string]any{
		"expires_at": now.Add(24 * time.Hour).Unix(),
	}, now)
	if got, want := defaultLease, now.Add(time.Hour).UnixMilli(); got != want {
		t.Fatalf("OAuth expiry must not extend supplier lease: got %d want %d", got, want)
	}
}

func TestSupplyOrderItemLeasesRequireExactExpandedAccountMapping(t *testing.T) {
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	accounts := []normalizedSupplyAccount{
		{leaseExpiresAtMS: now.Add(time.Hour).UnixMilli()},
		{leaseExpiresAtMS: now.Add(time.Hour).UnixMilli()},
	}
	if !applySupplyOrderItemLeases(accounts, []int64{900, 1800}, now) {
		t.Fatal("exactly expanded accounts should accept ordered item leases")
	}
	if got, want := accounts[0].leaseExpiresAtMS, now.Add(15*time.Minute).UnixMilli(); got != want {
		t.Fatalf("first lease = %d, want %d", got, want)
	}
	if got, want := accounts[1].leaseExpiresAtMS, now.Add(30*time.Minute).UnixMilli(); got != want {
		t.Fatalf("second lease = %d, want %d", got, want)
	}
	if !applySupplyOrderItemDetails(accounts, []supplyclient.OrderItem{
		{RemainingSeconds: 600, HasRemaining: true, BasePriceFen: 400, ChargedFen: 100},
		{RemainingSeconds: 1200, HasRemaining: true, BasePriceFen: 400, ChargedFen: 200},
	}, now) {
		t.Fatal("exactly expanded accounts should accept ordered item prices")
	}
	if accounts[0].basePriceFen != 400 || accounts[0].chargedFen != 100 ||
		accounts[1].basePriceFen != 400 || accounts[1].chargedFen != 200 {
		t.Fatalf("account costs were not assigned: %#v", accounts)
	}
	if got := supplyOrderItemsChargedFen([]supplyclient.OrderItem{
		{ChargedFen: 100},
		{ChargedFen: 200},
		{ChargedFen: 0},
	}); got != 300 {
		t.Fatalf("summed order item charge = %d, want 300", got)
	}
	original := accounts[0].leaseExpiresAtMS
	if applySupplyOrderItemLeases(accounts, []int64{300}, now) {
		t.Fatal("mismatched order items must not be assigned to expanded accounts")
	}
	if accounts[0].leaseExpiresAtMS != original {
		t.Fatalf("mismatched mapping changed original lease: %d", accounts[0].leaseExpiresAtMS)
	}
}

func TestTakeResponseSub2BundleIsExpandedAndUploadedAsCPACodex(t *testing.T) {
	var takeCalls atomic.Int32
	var uploadCalls atomic.Int32
	uploadedNames := sync.Map{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-bundle" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"id":"order-bundle","status":"ready","ready_quantity":2,"progress":100,"take_url":"/custom/take-bundle"}`))
		case r.URL.Path == "/custom/take-bundle" && r.Method == http.MethodPost:
			takeCalls.Add(1)
			_, _ = w.Write([]byte(`{"order":{"id":"order-bundle","status":"completed","items":[{"remaining_seconds":900,"base_price_fen":400,"charged_fen":100},{"remaining_seconds":1800,"base_price_fen":400,"charged_fen":200}]},"payload":{"accounts":[{"type":"sub2api-data","exported_at":"2026-07-30T17:28:18Z","accounts":[{"name":"team-one","type":"oauth","platform":"openai","credentials":{"access_token":"access-one","refresh_token":"refresh-one","chatgpt_account_id":"account-one","email":"one@example.com","plan_type":"team"}},{"name":"team-two","type":"oauth","platform":"openai","credentials":{"session_access_token":"access-two","refresh_token":"refresh-two","account_id":"account-two","email":"two@example.com","chatgpt_plan_type":"team"}}]}]}}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			name := r.URL.Query().Get("name")
			if name == "" {
				_, _ = w.Write([]byte(`{"files":[]}`))
				return
			}
			if _, ok := uploadedNames.Load(name); ok {
				_, _ = w.Write([]byte(`{"files":[{"name":"` + name + `","provider":"codex","disabled":false,"status":"ready"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"files":[]}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodPost:
			uploadCalls.Add(1)
			part, err := r.MultipartReader()
			if err != nil {
				t.Fatalf("multipart reader: %v", err)
			}
			for {
				item, err := part.NextPart()
				if err != nil {
					break
				}
				if item.FormName() != "file" {
					continue
				}
				uploadedNames.Store(item.FileName(), struct{}{})
				data, _ := io.ReadAll(item)
				var payload map[string]any
				if err := json.Unmarshal(data, &payload); err != nil {
					t.Fatalf("decode upload payload %s: %v", data, err)
				}
				if payload["type"] != "codex" || payload["import_format"] != "sub2api" || payload["access_token"] == "" || payload["refresh_token"] == "" {
					t.Fatalf("uploaded payload was not CPA Codex JSON: %#v", payload)
				}
				if _, nested := payload["credentials"]; nested {
					t.Fatalf("uploaded payload still contains credentials wrapper: %#v", payload)
				}
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			BaseURL: server.URL, Username: "customer", Password: "password",
			Product: "oauth_30d", PollIntervalSeconds: 1,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "order-bundle", Product: "oauth_30d", RequestedQuantity: 2, Status: "ready", TakeURL: "/custom/take-bundle",
	}); err != nil {
		t.Fatalf("create order: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("run automatic: %v", err)
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if takeCalls.Load() != 1 || uploadCalls.Load() != 2 {
		t.Fatalf("calls take=%d upload=%d", takeCalls.Load(), uploadCalls.Load())
	}
	if len(status.Orders) != 1 || status.Orders[0].Status != "completed" || status.Orders[0].ImportedCount != 2 || status.Orders[0].ItemCount != 2 {
		t.Fatalf("orders = %#v", status.Orders)
	}
	items, err := st.ListActiveImportedSupplyItems(context.Background(), time.Now().UnixMilli())
	if err != nil || len(items) != 2 {
		t.Fatalf("active imported items=%#v err=%v", items, err)
	}
	for index, expected := range []int64{900, 1800} {
		actualSeconds := (items[index].LeaseExpiresAtMS - time.Now().UnixMilli()) / 1000
		if actualSeconds < expected-2 || actualSeconds > expected+1 {
			t.Fatalf("item %d lease seconds=%d, want approximately %d; items=%#v", index, actualSeconds, expected, items)
		}
	}
	importedItems, err := st.ListSupplyImportItems(context.Background(), 10, "imported")
	if err != nil || len(importedItems) != 2 {
		t.Fatalf("imported items=%#v err=%v", importedItems, err)
	}
	costs := map[int64]int64{}
	for _, item := range importedItems {
		costs[item.ChargedFen] = item.BasePriceFen
	}
	if costs[100] != 400 || costs[200] != 400 {
		t.Fatalf("imported item costs=%#v items=%#v", costs, importedItems)
	}
}

func TestTakingLeasePreventsDuplicateTake(t *testing.T) {
	var takeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case "/api/customer/pickup/orders/order-taking", "/api/customer/pickup/orders/order-taking/take":
			takeCalls.Add(1)
			t.Fatal("taking lease should block status polling and take retry")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		Supply: store.ManagerSupplyConfig{BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_30d"},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	leaseUntil := time.Now().Add(time.Minute).UnixMilli()
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "order-taking", Product: "oauth_30d", RequestedQuantity: 1, Status: "taking", NextPollAtMS: leaseUntil,
	}); err != nil {
		t.Fatalf("create taking order: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("run automatic: %v", err)
	}
	if takeCalls.Load() != 0 {
		t.Fatalf("take/status calls = %d, want 0", takeCalls.Load())
	}
}

func TestTimedOutTakingOrderRetriesWithoutAutomaticRelease(t *testing.T) {
	var takeCalls atomic.Int32
	var releaseCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-timeout" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"id":"order-timeout","status":"ready","ready_quantity":1,"progress":100}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-timeout/take" && r.Method == http.MethodPost:
			takeCalls.Add(1)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"order-timeout","status":"waiting_inventory","retry_after_seconds":1}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-timeout" && r.Method == http.MethodDelete:
			releaseCalls.Add(1)
			t.Fatal("a timed-out take attempt must reconcile and retry, not release the reserved order")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply-timeout.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		Supply: store.ManagerSupplyConfig{
			BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_30d", PollIntervalSeconds: 1,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "order-timeout", Product: "oauth_30d", RequestedQuantity: 1, Automatic: true,
		Status: "taking", NextPollAtMS: time.Now().Add(-time.Second).UnixMilli(),
	}); err != nil {
		t.Fatalf("create taking order: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("reconcile timed-out take: %v", err)
	}
	order, found, err := st.GetSupplyOrder(context.Background(), "order-timeout")
	if err != nil || !found {
		t.Fatalf("load order found=%v err=%v", found, err)
	}
	if takeCalls.Load() != 1 || releaseCalls.Load() != 0 || order.Status != "waiting_inventory" {
		t.Fatalf("take=%d release=%d order=%#v", takeCalls.Load(), releaseCalls.Load(), order)
	}
}

func TestProcessOrderCompletesPersistedImportsAfterRequestCancellation(t *testing.T) {
	account, err := normalizeAccountForImport(`{"type":"codex","email":"durable-import@example.com","account_id":"workspace-durable","workspace_id":"workspace-durable","chatgpt_user_id":"member-durable","access_token":"access"}`)
	if err != nil {
		t.Fatalf("normalize account: %v", err)
	}
	var registered map[string]any
	if err := json.Unmarshal(account.payload, &registered); err != nil {
		t.Fatalf("decode normalized account: %v", err)
	}
	registered["name"] = account.fileName
	registered["provider"] = "codex"
	registered["status"] = "active"
	registered["disabled"] = false

	lookupStarted := make(chan struct{})
	releaseLookup := make(chan struct{})
	var firstLookup sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v0/management/auth-files" {
			http.NotFound(w, r)
			return
		}
		firstLookup.Do(func() {
			close(lookupStarted)
			<-releaseLookup
		})
		_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{registered}})
	}))
	t.Cleanup(server.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "durable-import.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	order, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "order-durable-import", Product: "oauth_7d", RequestedQuantity: 1,
		Status: "partial", RemoteStatus: "completed", ItemCount: 1,
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := st.InsertSupplyImportItems(ctx, order.OrderID, []store.SupplyImportItem{{
		OrderID: order.OrderID, ItemKey: account.itemKey, AccountName: account.accountName,
		NameKey: account.nameKey, FileName: account.fileName, ImportAction: "add",
		PayloadJSON: string(account.payload), LeaseExpiresAtMS: time.Now().Add(time.Hour).UnixMilli(),
	}}); err != nil {
		t.Fatalf("insert import item: %v", err)
	}

	service := New(st, nil, server.Client())
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- service.processOrder(requestCtx, store.ManagerConfig{
			CPAConnection: store.ManagerCPAConnectionConfig{
				CPABaseURL: server.URL, ManagementKey: "management-key",
			},
		}, order)
	}()

	select {
	case <-lookupStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("CPA lookup did not start")
	}
	cancelRequest()
	close(releaseLookup)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("process order after request cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("durable import did not complete")
	}

	items, err := st.ListSupplyImportItemsByOrderIDs(context.Background(), []string{order.OrderID})
	if err != nil || len(items) != 1 || items[0].Status != "imported" {
		t.Fatalf("import items=%#v err=%v", items, err)
	}
	completed, found, err := st.GetSupplyOrder(context.Background(), order.OrderID)
	if err != nil || !found || completed.Status != "completed" || completed.ImportedCount != 1 || completed.LastError != "" {
		t.Fatalf("completed order=%#v found=%v err=%v", completed, found, err)
	}
}

func TestClaimSupplyOrderTakingAllowsOnlyOneWorker(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "order-claim", Product: "oauth_30d", RequestedQuantity: 1, Status: "ready",
	}); err != nil {
		t.Fatalf("create order: %v", err)
	}
	nowMS := time.Now().UnixMilli()
	leaseUntilMS := nowMS + int64(time.Minute/time.Millisecond)
	claimed, err := st.ClaimSupplyOrderTaking(context.Background(), "order-claim", nowMS, leaseUntilMS)
	if err != nil || !claimed {
		t.Fatalf("first claim=%v err=%v", claimed, err)
	}
	claimed, err = st.ClaimSupplyOrderTaking(context.Background(), "order-claim", nowMS, leaseUntilMS)
	if err != nil || claimed {
		t.Fatalf("second claim=%v err=%v, want false nil", claimed, err)
	}
	order, found, err := st.GetSupplyOrder(context.Background(), "order-claim")
	if err != nil || !found || order.Status != "taking" || order.NextPollAtMS != leaseUntilMS {
		t.Fatalf("order=%#v found=%v err=%v", order, found, err)
	}
}

func TestImportVerificationFailureBlocksDuplicateAutomaticOrders(t *testing.T) {
	var createCalls atomic.Int32
	var uploadCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":10,"missing":0,"estimated_total_fen":1000}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":10000,"balance_fen":10000}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			createCalls.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"order":{"id":"order-verify-fail","status":"ready","quantity":1},"take_url":"/custom/take-verify-fail"}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-verify-fail" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"id":"order-verify-fail","status":"ready","ready_quantity":1}`))
		case r.URL.Path == "/custom/take-verify-fail":
			_, _ = w.Write([]byte(`{"payload":{"accounts":[{"type":"oauth","platform":"openai","credentials":{"access_token":"access","refresh_token":"refresh","account_id":"account-verify","email":"verify@example.com"}}]},"status":"completed"}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"files":[]}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodPost:
			uploadCalls.Add(1)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	smartDisabled := false
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled:                 &enabled,
			BaseURL:                 server.URL,
			Username:                "customer",
			Password:                "password",
			Product:                 "oauth_30d",
			TargetAvailableAccounts: 11,
			ReplenishBatchSize:      1,
			CheckIntervalSeconds:    60,
			PollIntervalSeconds:     1,
			SmartEnabled:            &smartDisabled,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err == nil {
		t.Fatal("first automatic run succeeded despite missing CPA registration")
	}
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("second automatic run should keep the open partial order without creating a new one: %v", err)
	}
	if createCalls.Load() != 1 || uploadCalls.Load() != 1 {
		t.Fatalf("calls create=%d upload=%d", createCalls.Load(), uploadCalls.Load())
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil || status.ActiveOrder == nil || status.ActiveOrder.Status != "partial" {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestExpiredPartialImportSettlesOrderAndReleasesOpenSlot(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "expired-partial-import.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	order, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "order-expired-partial", Product: "oauth_7d", RequestedQuantity: 2,
		Automatic: true, Status: "partial", RemoteStatus: "completed",
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	now := time.Now()
	if _, err := st.InsertSupplyImportItems(ctx, order.OrderID, []store.SupplyImportItem{
		{OrderID: order.OrderID, ItemKey: "good", FileName: "good.json", PayloadJSON: `{}`,
			LeaseExpiresAtMS: now.Add(30 * time.Minute).UnixMilli()},
		{OrderID: order.OrderID, ItemKey: "expired", FileName: "expired.json", PayloadJSON: `{}`,
			LeaseExpiresAtMS: now.Add(-time.Minute).UnixMilli()},
	}); err != nil {
		t.Fatalf("insert items: %v", err)
	}
	items, err := st.ListSupplyImportItemsByOrderIDs(ctx, []string{order.OrderID})
	if err != nil || len(items) != 2 {
		t.Fatalf("list items=%#v err=%v", items, err)
	}
	for _, item := range items {
		if item.ItemKey == "good" {
			err = st.MarkSupplyImportItemImported(ctx, item.ID, now.UnixMilli())
		} else {
			err = st.MarkSupplyImportItemFailed(ctx, item.ID, "CPA auth initialization did not become ready", now.Add(time.Hour).UnixMilli())
		}
		if err != nil {
			t.Fatalf("mark item %s: %v", item.ItemKey, err)
		}
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), nil)
	if err := service.importItems(ctx, store.ManagerConfig{}, &order); err != nil {
		t.Fatalf("settle partial import: %v", err)
	}
	settled, found, err := st.GetSupplyOrder(ctx, order.OrderID)
	if err != nil || !found {
		t.Fatalf("load settled order found=%v err=%v", found, err)
	}
	if settled.Status != "completed_partial" || settled.ImportedCount != 1 || settled.ItemCount != 2 ||
		settled.CompletedAtMS <= 0 || !strings.Contains(settled.LastError, "1 unusable account") {
		t.Fatalf("settled order = %#v", settled)
	}
	open, err := st.ListOpenSupplyOrders(ctx, 10)
	if err != nil || len(open) != 0 {
		t.Fatalf("open orders=%#v err=%v", open, err)
	}
}

func TestFinishedPartialDeliverySettlesPermanentImportFailure(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "finished-partial-import.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	order, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "order-finished-partial", Product: "oauth_7d", RequestedQuantity: 3,
		Automatic: true, Status: "partial", RemoteStatus: "partial", Progress: 100,
		ReleasedFen: 100,
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	now := time.Now()
	if _, err := st.InsertSupplyImportItems(ctx, order.OrderID, []store.SupplyImportItem{
		{OrderID: order.OrderID, ItemKey: "good", FileName: "good.json", PayloadJSON: `{}`,
			LeaseExpiresAtMS: now.Add(30 * time.Minute).UnixMilli()},
		{OrderID: order.OrderID, ItemKey: "revoked", FileName: "revoked.json", PayloadJSON: `{}`,
			LeaseExpiresAtMS: now.Add(30 * time.Minute).UnixMilli()},
	}); err != nil {
		t.Fatalf("insert items: %v", err)
	}
	items, err := st.ListSupplyImportItemsByOrderIDs(ctx, []string{order.OrderID})
	if err != nil || len(items) != 2 {
		t.Fatalf("list items=%#v err=%v", items, err)
	}
	for _, item := range items {
		if item.ItemKey == "good" {
			err = st.MarkSupplyImportItemImported(ctx, item.ID, now.UnixMilli())
		} else {
			err = st.MarkSupplyImportItemFailed(ctx, item.ID, "token refresh failed: refresh_token_invalidated", now.Add(time.Hour).UnixMilli())
		}
		if err != nil {
			t.Fatalf("mark item %s: %v", item.ItemKey, err)
		}
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), nil)
	if err := service.importItems(ctx, store.ManagerConfig{}, &order); err != nil {
		t.Fatalf("settle finished partial import: %v", err)
	}
	settled, found, err := st.GetSupplyOrder(ctx, order.OrderID)
	if err != nil || !found {
		t.Fatalf("load settled order found=%v err=%v", found, err)
	}
	if settled.Status != "completed_partial" || settled.ImportedCount != 1 || settled.ItemCount != 2 ||
		settled.CompletedAtMS <= 0 || !strings.Contains(settled.LastError, "refresh_token_invalidated") {
		t.Fatalf("settled order = %#v", settled)
	}

	inProgress := order
	inProgress.Progress = 99
	if supplyDeliveryFinishedForImportSettlement(inProgress) {
		t.Fatal("in-progress partial delivery was treated as terminal")
	}
}

func TestFinishedDeliverySettlesStableUnavailableImportFailure(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "stable-unavailable-import.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	order, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "order-stable-unavailable", Product: "team_1h", RequestedQuantity: 2,
		Automatic: true, Status: "partial", RemoteStatus: "completed",
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	now := time.Now()
	if _, err := st.InsertSupplyImportItems(ctx, order.OrderID, []store.SupplyImportItem{
		{OrderID: order.OrderID, ItemKey: "good", FileName: "good.json", PayloadJSON: `{}`,
			LeaseExpiresAtMS: now.Add(30 * time.Minute).UnixMilli()},
		{OrderID: order.OrderID, ItemKey: "disabled", FileName: "disabled.json", PayloadJSON: `{}`,
			LeaseExpiresAtMS: now.Add(30 * time.Minute).UnixMilli()},
	}); err != nil {
		t.Fatalf("insert items: %v", err)
	}
	items, err := st.ListSupplyImportItemsByOrderIDs(ctx, []string{order.OrderID})
	if err != nil || len(items) != 2 {
		t.Fatalf("list items=%#v err=%v", items, err)
	}
	for _, item := range items {
		if item.ItemKey == "good" {
			err = st.MarkSupplyImportItemImported(ctx, item.ID, now.UnixMilli())
		} else {
			err = st.MarkSupplyImportItemFailed(ctx, item.ID,
				terminalCPAAuthUnavailableMessage+`: name="disabled.json" status="disabled" disabled=true`,
				now.Add(time.Hour).UnixMilli())
		}
		if err != nil {
			t.Fatalf("mark item %s: %v", item.ItemKey, err)
		}
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), nil)
	if err := service.importItems(ctx, store.ManagerConfig{}, &order); err != nil {
		t.Fatalf("settle stable unavailable import: %v", err)
	}
	settled, found, err := st.GetSupplyOrder(ctx, order.OrderID)
	if err != nil || !found {
		t.Fatalf("load settled order found=%v err=%v", found, err)
	}
	if settled.Status != "completed_partial" || settled.ImportedCount != 1 ||
		settled.CompletedAtMS <= 0 || !strings.Contains(settled.LastError, terminalCPAAuthUnavailableMessage) {
		t.Fatalf("settled order = %#v", settled)
	}
}

func TestWaitForCPAAuthLifecycleMarksStableUnavailableFilePermanent(t *testing.T) {
	service := New(nil, nil)
	err := service.waitForCPAAuthLifecycle(context.Background(), func(context.Context) (cpaauthfiles.File, error) {
		return cpaauthfiles.File{
			Name:     "disabled.json",
			Provider: "codex",
			Disabled: true,
			Raw:      map[string]any{"status": "disabled"},
		}, errors.New("CPA registered imported auth file but it is not available")
	})
	if err == nil || !strings.Contains(err.Error(), terminalCPAAuthUnavailableMessage) {
		t.Fatalf("stable unavailable lifecycle error = %v", err)
	}
	if !permanentCPAAuthLifecycleFailure(err.Error()) {
		t.Fatalf("stable unavailable lifecycle error was not permanent: %v", err)
	}
}

func TestTerminalCPAAuthLifecycleErrorOnlyMatchesPermanentCredentialFailure(t *testing.T) {
	permanent := cpaauthfiles.File{Name: "account.json", Raw: map[string]any{
		"status":         "initialization_failed",
		"status_message": `token refresh failed: {"code":"refresh_token_invalidated"}`,
	}}
	if err := terminalCPAAuthLifecycleError(permanent); err == nil || !strings.Contains(err.Error(), "refresh_token_invalidated") {
		t.Fatalf("permanent lifecycle error = %v", err)
	}

	recoverable := cpaauthfiles.File{Name: "account.json", Raw: map[string]any{
		"status":         "initialization_failed",
		"status_message": "Rate limit exceeded; retrying after cooldown",
	}}
	if err := terminalCPAAuthLifecycleError(recoverable); err != nil {
		t.Fatalf("recoverable lifecycle error = %v", err)
	}

	quotaExhausted := cpaauthfiles.File{Name: "account.json", Raw: map[string]any{
		"status":         "initialization_failed",
		"status_message": "initialization failed; retrying: usage endpoint returned 402",
	}}
	if err := terminalCPAAuthLifecycleError(quotaExhausted); err == nil || !strings.Contains(err.Error(), "returned 402") {
		t.Fatalf("quota-exhausted lifecycle error = %v", err)
	}

	deactivated := cpaauthfiles.File{Name: "account.json", Raw: map[string]any{
		"status":         "disabled",
		"status_message": "workspace_deactivated",
	}}
	if err := terminalCPAAuthLifecycleError(deactivated); err == nil || !strings.Contains(err.Error(), "workspace_deactivated") {
		t.Fatalf("deactivated lifecycle error = %v", err)
	}
}

func TestEnsureCPAAccountImportedReplacesAvailableDifferentAccountAndKeepsFingerprint(t *testing.T) {
	const fileName = "codex-pool.json"
	existingPayload := []byte(`{"type":"codex","email":"old@example.com","workspace_id":"workspace-old","chatgpt_user_id":"member-old","codex_identity_fingerprint":"stable-cache-prefix","access_token":"old"}`)
	account, err := normalizeAccountForImport(`{"type":"codex","email":"new@example.com","workspace_id":"workspace-new","chatgpt_user_id":"member-new","access_token":"new"}`)
	if err != nil {
		t.Fatalf("normalize replacement account: %v", err)
	}

	var uploaded atomic.Bool
	var uploadedPayload []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			_, _ = w.Write(existingPayload)
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/auth-files":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			file, _, errFile := r.FormFile("file")
			if errFile != nil {
				http.Error(w, errFile.Error(), http.StatusBadRequest)
				return
			}
			defer file.Close()
			uploadedPayload, _ = io.ReadAll(file)
			uploaded.Store(true)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			if uploaded.Load() {
				_, _ = w.Write([]byte(`{"files":[{"name":"codex-pool.json","provider":"codex","status":"active","account_id":"workspace-new","workspace_id":"workspace-new","chatgpt_user_id":"member-new","email":"new@example.com"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"files":[{"name":"codex-pool.json","provider":"codex","status":"active","account_id":"workspace-old","workspace_id":"workspace-old","chatgpt_user_id":"member-old","email":"old@example.com"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	service := New(nil, nil, server.Client())
	cfg := store.ManagerConfig{CPAConnection: store.ManagerCPAConnectionConfig{
		CPABaseURL:    server.URL,
		ManagementKey: "management-key",
	}}
	if err := service.ensureCPAAccountImported(context.Background(), cfg, fileName, account.payload, "replace", account); err != nil {
		t.Fatalf("replace imported account: %v", err)
	}
	if !uploaded.Load() {
		t.Fatal("replacement upload was skipped")
	}
	var metadata map[string]any
	if err := json.Unmarshal(uploadedPayload, &metadata); err != nil {
		t.Fatalf("decode uploaded replacement: %v", err)
	}
	if got := stringFromMap(metadata, "codex_identity_fingerprint"); got != "stable-cache-prefix" {
		t.Fatalf("replacement fingerprint = %q, want stable-cache-prefix", got)
	}
	if got := stringFromMap(metadata, "access_token"); got != "new" {
		t.Fatalf("replacement access token = %q, want new", got)
	}
}

func TestRecoveryImportSettlesTerminalQuotaFailure(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "recovery-terminal.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	now := time.Now()
	const orderID = "recovery-terminal-quota"
	order := store.SupplyOrder{
		OrderID: orderID, Product: "oauth_30d", RequestedQuantity: 1, Automatic: true,
		Strategy: "recovery", Status: "recovery_partial", RemoteStatus: "recovery_claimed", ItemCount: 1,
	}
	if _, err := st.CreateSupplyOrder(ctx, order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := st.InsertSupplyImportItems(ctx, orderID, []store.SupplyImportItem{{
		OrderID: orderID, ItemKey: "terminal-quota", AccountName: "quota@example.com",
		FileName: "codex-quota@example.com-space-test.json", PayloadJSON: `{"type":"codex"}`,
	}}); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	items, err := st.ListSupplyImportItemsByOrderIDs(ctx, []string{orderID})
	if err != nil || len(items) != 1 {
		t.Fatalf("list items=%#v err=%v", items, err)
	}
	if err := st.MarkSupplyImportItemFailed(ctx, items[0].ID, "initialization failed; retrying: usage endpoint returned 402", now.Add(time.Hour).UnixMilli()); err != nil {
		t.Fatalf("mark item failed: %v", err)
	}
	recovery := store.SupplyRecovery{
		RecoveryID: "terminal-quota", DeliveryStatus: "claimed", Status: "partial",
		ClaimOrderID: orderID, ItemCount: 1, LastSeenAtMS: now.UnixMilli(),
	}
	if _, err := st.UpsertSupplyRecoveries(ctx, []store.SupplyRecovery{recovery}); err != nil {
		t.Fatalf("upsert recovery: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), nil)
	imported, failed, err := service.processRecoveryImport(ctx, store.ManagerConfig{}, recovery)
	if err != nil || imported || !failed {
		t.Fatalf("process recovery imported=%v failed=%v err=%v", imported, failed, err)
	}
	settledRecovery, found, err := st.GetSupplyRecovery(ctx, recovery.RecoveryID)
	if err != nil || !found || settledRecovery.Status != "failed" || !strings.Contains(settledRecovery.LastError, "returned 402") {
		t.Fatalf("settled recovery=%#v found=%v err=%v", settledRecovery, found, err)
	}
	settledOrder, found, err := st.GetSupplyOrder(ctx, orderID)
	if err != nil || !found || settledOrder.Status != "failed" || settledOrder.CompletedAtMS <= 0 {
		t.Fatalf("settled order=%#v found=%v err=%v", settledOrder, found, err)
	}
}

func TestAutomaticWaitingOrderRemainsTrackedWhenCPATargetIsAlreadySatisfied(t *testing.T) {
	var createCalls atomic.Int32
	var authListCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":10,"missing":0,"estimated_total_fen":1000}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":10000,"balance_fen":10000}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			createCalls.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"order":{"id":"order-release","status":"waiting_inventory","quantity":1}}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-release" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"id":"order-release","status":"waiting_inventory","quantity":1}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-release/take":
			t.Fatal("locally released order must not be taken")
		case (r.URL.Path == "/api/customer/pickup/orders/order-release" && r.Method == http.MethodDelete) ||
			r.URL.Path == "/api/customer/pickup/orders/order-release/cancel" ||
			r.URL.Path == "/api/customer/pickup/orders/order-release/release":
			t.Fatal("local automatic release must not call a supplier cancellation endpoint")
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			call := authListCalls.Add(1)
			if call == 1 {
				_, _ = w.Write([]byte(`{"files":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"files":[{"name":"existing.json","provider":"codex","disabled":false,"status":"ready"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	smartDisabled := false
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled:                 &enabled,
			BaseURL:                 server.URL,
			Username:                "customer",
			Password:                "password",
			Product:                 "oauth_30d",
			TargetAvailableAccounts: 1,
			ReplenishBatchSize:      1,
			CheckIntervalSeconds:    60,
			PollIntervalSeconds:     1,
			SmartEnabled:            &smartDisabled,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("create automatic order: %v", err)
	}
	order, found, err := st.GetSupplyOrder(context.Background(), "order-release")
	if err != nil || !found {
		t.Fatalf("load created order found=%v err=%v", found, err)
	}
	order.NextPollAtMS = 0
	if err := st.UpdateSupplyOrder(context.Background(), order); err != nil {
		t.Fatalf("make order due: %v", err)
	}
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("reconcile satisfied waiting order: %v", err)
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status.ActiveOrder == nil || status.ActiveOrder.OrderID != "order-release" || status.ActiveOrder.Status != "waiting_inventory" {
		t.Fatalf("waiting reservation should remain active until supplier terminal state: %#v", status.ActiveOrder)
	}
	if len(status.Orders) != 1 || status.Orders[0].Status != "waiting_inventory" || status.Orders[0].ReleasedFen != 0 {
		t.Fatalf("orders = %#v", status.Orders)
	}
	if createCalls.Load() != 1 {
		t.Fatalf("create calls = %d, want 1", createCalls.Load())
	}
}

func TestAutomaticOrderTakesWhenCapacityStillNeededWithoutReleaseProbe(t *testing.T) {
	var takeCalls atomic.Int32
	var uploadCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-reserved" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"id":"order-reserved","status":"ready","ready_quantity":1,"progress":100,"take_url":"/api/customer/pickup/orders/order-reserved/take"}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-reserved/take" && r.Method == http.MethodPost:
			takeCalls.Add(1)
			_, _ = w.Write([]byte(`{"payload":{"accounts":[{"type":"codex","account":"reserved@example.com","access_token":"secret"}]},"status":"completed"}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			if name := r.URL.Query().Get("name"); name != "" {
				if uploadCalls.Load() > 0 {
					_, _ = w.Write([]byte(`{"files":[{"name":"` + name + `","provider":"codex","disabled":false,"status":"ready"}]}`))
					return
				}
				_, _ = w.Write([]byte(`{"files":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"files":[]}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodPost:
			uploadCalls.Add(1)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	smartDisabled := false
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_7d",
			TargetAvailableAccounts: 1, PollIntervalSeconds: 1, SmartEnabled: &smartDisabled,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "order-reserved", Product: "oauth_7d", RequestedQuantity: 1, Automatic: true,
		Status: "ready", ReadyQuantity: 1, Progress: 100,
	}); err != nil {
		t.Fatalf("create order: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("first automatic run: %v", err)
	}
	order, found, err := st.GetSupplyOrder(context.Background(), "order-reserved")
	if err != nil || !found {
		t.Fatalf("load order found=%v err=%v", found, err)
	}
	if err != nil || !found || order.Status != "completed" || order.ImportedCount != 1 {
		t.Fatalf("needed order was not imported: %#v found=%v err=%v", order, found, err)
	}
	if takeCalls.Load() != 1 || uploadCalls.Load() != 1 {
		t.Fatalf("take=%d upload=%d, want 1/1", takeCalls.Load(), uploadCalls.Load())
	}
}

func TestNonSmartAutomaticOrderReleasesQuantityFarAboveLiveDeficit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"files":[]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "non-smart-final-take-budget.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	smartDisabled := false
	cfg := store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, SmartEnabled: &smartDisabled, Product: "oauth_7d",
			TargetAvailableAccounts: 1, NewAccountConfidence: 1,
		},
	}
	if err := st.SaveManagerConfig(ctx, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	order, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "oversized-ready-10", Product: "oauth_7d", RequestedQuantity: 10,
		ReadyQuantity: 10, Automatic: true, Status: "ready",
	})
	if err != nil {
		t.Fatalf("create oversized order: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	released, err := service.autoReleaseAutomaticOrderIfNotNeeded(ctx, cfg, &order, true)
	if err != nil || !released {
		t.Fatalf("oversized final decision released=%v err=%v", released, err)
	}
	order, _, err = st.GetSupplyOrder(ctx, order.OrderID)
	if err != nil || order.Status != "released" {
		t.Fatalf("oversized order after final decision = %#v err=%v", order, err)
	}
}

func TestNVTokensPaidReadyOrderAlwaysContinuesToTake(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "nv-paid-ready.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	cfg := store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		Enabled: &enabled,
		Platforms: []store.ManagerSupplyPlatformConfig{{
			ID: "nv", Type: managerconfigsvc.SupplyPlatformNvtokens, Enabled: &enabled, Product: "plus",
		}},
	}}
	order, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "nv-paid-ready", SupplierID: "nv", Product: "plus", RequestedQuantity: 1,
		ReadyQuantity: 1, ChargedFen: 2500, Automatic: true, Status: "ready", RemoteStatus: "completed",
	})
	if err != nil {
		t.Fatalf("create paid ready order: %v", err)
	}
	service := New(st, nil)
	released, err := service.autoReleaseAutomaticOrderIfNotNeeded(context.Background(), cfg, &order, true)
	if err != nil || released {
		t.Fatalf("NV paid ready admission released=%v err=%v", released, err)
	}
	resource := service.currentSmartResource(cfg.Supply)
	if resource.SuggestedAction != smartActionTakeLocked || resource.DecisionReason != "paid_ready_order_take" ||
		resource.LockedOrderID != order.OrderID || !service.smartTakeAllowed(cfg.Supply, order.OrderID) {
		t.Fatalf("NV paid ready resource = %#v", resource)
	}
}

func TestStoreReactivatesLocallyReleasedUnsupportedOrder(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "old-unsupported-release", Product: "oauth_7d", RequestedQuantity: 10, Automatic: true,
		Status: "released", RemoteStatus: "release_unsupported", Progress: 100,
	}); err != nil {
		t.Fatalf("create legacy order: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "newer-open-order", Product: "oauth_7d", RequestedQuantity: 5, Automatic: true, Status: "waiting_inventory",
	}); err != nil {
		t.Fatalf("create newer open order: %v", err)
	}
	order, found, err := st.ActivateNextUnsupportedSupplyRelease(context.Background())
	if err != nil || !found || order.Status != "ready" || order.CompletedAtMS != 0 {
		t.Fatalf("legacy unsupported release was not reactivated: %#v found=%v err=%v", order, found, err)
	}
}

func TestStoreReactivatesPaidAutomaticReleasePendingOrder(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "paid-auto-release", Product: "plus", RequestedQuantity: 1, Automatic: true,
		Status: "released", RemoteStatus: remoteStatusAutomaticReleasePending,
		ReadyQuantity: 1, Progress: 100, ChargedFen: 2500,
	}); err != nil {
		t.Fatalf("create paid local release: %v", err)
	}
	order, found, err := st.ActivateNextUnsupportedSupplyRelease(context.Background())
	if err != nil || !found || order.Status != "ready" || order.CompletedAtMS != 0 || order.ChargedFen != 2500 {
		t.Fatalf("paid local release was not reactivated: %#v found=%v err=%v", order, found, err)
	}
}

func TestStoreReactivatesPaidNvtokensOrderCancelledByMissingDetailRoute(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "paid-nv-ledger-order", SupplierID: "nv", Product: "plus", RequestedQuantity: 1,
		Automatic: true, Status: "cancelled", RemoteStatus: "cancelled", ReadyQuantity: 1,
		Progress: 100, ChargedFen: 2500, CompletedAtMS: time.Now().UnixMilli(),
		LastError: "supply API returned HTTP 404: GET /api/workspace/extractions/paid-nv-ledger-order",
	}); err != nil {
		t.Fatalf("create cancelled paid NV order: %v", err)
	}
	order, found, err := st.ActivateNextUnsupportedSupplyRelease(context.Background())
	if err != nil || !found || order.Status != "ready" || order.CompletedAtMS != 0 ||
		order.ChargedFen != 2500 || order.ReadyQuantity != 1 {
		t.Fatalf("paid NV ledger order was not reactivated: %#v found=%v err=%v", order, found, err)
	}
}

func TestNvtokensPaidOrder404RemainsReconcilable(t *testing.T) {
	enabled := true
	platform := store.ManagerSupplyPlatformConfig{
		ID: "nv", Type: managerconfigsvc.SupplyPlatformNvtokens, Enabled: &enabled, Product: "plus",
	}
	paid := store.SupplyOrder{
		OrderID: "paid", SupplierID: "nv", Product: "plus", Status: "ready", RemoteStatus: "completed",
		ReadyQuantity: 1, Progress: 100, ChargedFen: 2500,
	}
	if !nvtokensPaidOrderLookupUncertain(platform, paid) {
		t.Fatal("paid ready NV order must remain open when the detail route returns 404")
	}
	paid.ChargedFen = 0
	if nvtokensPaidOrderLookupUncertain(platform, paid) {
		t.Fatal("unpaid NV order must not use paid-ledger reconciliation")
	}
}

func TestLegacySupplyImportRepairConvertsAndVerifiesCPAFile(t *testing.T) {
	var uploadCalls atomic.Int32
	var uploadedName string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/auth-files" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			name := r.URL.Query().Get("name")
			if name != "" && uploadCalls.Load() > 0 {
				_, _ = w.Write([]byte(`{"files":[{"name":"` + name + `","provider":"codex","status":"ready"}]}`))
			} else {
				_, _ = w.Write([]byte(`{"files":[]}`))
			}
		case http.MethodPost:
			part, err := r.MultipartReader()
			if err != nil {
				t.Fatalf("multipart reader: %v", err)
			}
			for {
				item, nextErr := part.NextPart()
				if nextErr != nil {
					break
				}
				if item.FormName() == "file" {
					uploadedName = item.FileName()
					data, _ := io.ReadAll(item)
					var normalized map[string]any
					if err := json.Unmarshal(data, &normalized); err != nil || normalized["type"] != "codex" {
						t.Fatalf("legacy payload was not normalized: %s", data)
					}
				}
			}
			uploadCalls.Add(1)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			response := http.StatusMethodNotAllowed
			w.WriteHeader(response)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply:        store.ManagerSupplyConfig{Product: "oauth_30d"},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	order, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "legacy-order", Product: "oauth_30d", RequestedQuantity: 1, Automatic: true, Status: "completed",
	})
	if err != nil {
		t.Fatalf("create legacy order: %v", err)
	} else {
		order.ItemCount = 1
		order.ImportedCount = 1
		order.CompletedAtMS = time.Now().UnixMilli()
		if err := st.UpdateSupplyOrder(context.Background(), order); err != nil {
			t.Fatalf("update legacy order: %v", err)
		}
	}
	if _, err := st.InsertSupplyImportItems(context.Background(), "legacy-order", []store.SupplyImportItem{{
		OrderID: "legacy-order", ItemKey: "legacy-key", FileName: "supply-legacy-key.json",
		PayloadJSON: `{"name":"legacy","type":"oauth","platform":"openai","credentials":{"access_token":"access","refresh_token":"refresh","account_id":"account-legacy","email":"legacy@example.com"}}`,
	}}); err != nil {
		t.Fatalf("insert legacy item: %v", err)
	}
	if err := st.MarkSupplyImportItemImported(context.Background(), 1, time.Now().UnixMilli()); err != nil {
		t.Fatalf("mark legacy item imported: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("repair legacy import: %v", err)
	}
	repaired, found, err := st.GetSupplyOrder(context.Background(), "legacy-order")
	if err != nil || !found || repaired.Status != "completed" || repaired.ImportedCount != 1 {
		t.Fatalf("repaired order=%#v found=%v err=%v", repaired, found, err)
	}
	if uploadCalls.Load() != 1 || uploadedName != stableSupplyAccountFileName("legacy@example.com", "account-legacy") {
		t.Fatalf("upload calls=%d uploaded name=%q", uploadCalls.Load(), uploadedName)
	}
}

func TestResolveSupplyImportPlanUsesAccountIdentityBeforeFileName(t *testing.T) {
	const boundFileName = "legacy-item-0001.json"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/auth-files" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		if name := r.URL.Query().Get("name"); name != "" && name != boundFileName {
			_, _ = w.Write([]byte(`{"files":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"files":[{"name":"legacy-item-0001.json","auth_index":"17","provider":"codex","account":"old@example.com","account_id":"workspace-shared","workspace_id":"workspace-shared","chatgpt_user_id":"member-one"}]}`))
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply-item-key-binding.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{OrderID: "original", Product: "team_1h", Status: "completed"}); err != nil {
		t.Fatalf("create original order: %v", err)
	}
	original, err := normalizeAccountForImport(`{"type":"codex","email":"old@example.com","account_id":"workspace-shared","workspace_id":"workspace-shared","chatgpt_user_id":"member-one","access_token":"old"}`)
	if err != nil {
		t.Fatalf("normalize original: %v", err)
	}
	if _, err := st.InsertSupplyImportItems(ctx, "original", []store.SupplyImportItem{{
		ItemKey: original.itemKey, AccountName: original.accountName, NameKey: original.nameKey,
		FileName: boundFileName, ImportAction: "add", PayloadJSON: string(original.payload),
	}}); err != nil {
		t.Fatalf("insert original item: %v", err)
	}
	items, err := st.ListSupplyImportItems(ctx, 10, "")
	if err != nil || len(items) != 1 {
		t.Fatalf("list original item: %#v err=%v", items, err)
	}
	if err := st.MarkSupplyImportItemImported(ctx, items[0].ID, 1000); err != nil {
		t.Fatalf("mark original imported: %v", err)
	}

	refreshed, err := normalizeAccountForImport(`{"type":"codex","email":"renamed@example.com","account_id":"workspace-shared","workspace_id":"workspace-shared","chatgpt_user_id":"member-one","access_token":"new"}`)
	if err != nil {
		t.Fatalf("normalize refreshed account: %v", err)
	}
	if refreshed.itemKey != original.itemKey || refreshed.nameKey == original.nameKey {
		t.Fatalf("test identity setup invalid: original=%#v refreshed=%#v", original, refreshed)
	}
	service := New(st, nil, server.Client())
	plan, err := service.resolveSupplyImportPlan(ctx, store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
	}, store.SupplyOrder{OrderID: "replacement", Product: "team_1h"}, store.SupplyImportItem{}, refreshed, false)
	if err != nil {
		t.Fatalf("resolve import plan: %v", err)
	}
	if plan.action != "replace" || plan.fileName != boundFileName || plan.replacedFileName != boundFileName {
		t.Fatalf("identity binding was not reused: %#v", plan)
	}
}

func buildSupplyTestJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal JWT header: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal JWT claims: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestSupplyOrderDatabaseAllowsOnlyOneOpenOrder(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "open-1", Product: "oauth_30d", RequestedQuantity: 1, Status: "waiting_inventory",
	}); err != nil {
		t.Fatalf("create first open order: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "open-2", Product: "oauth_30d", RequestedQuantity: 1, Status: "creating",
	}); err == nil {
		t.Fatal("second open order was accepted")
	}
}

func TestHydrateOverviewIfNeededRestoresSupplierSnapshotAfterRestart(t *testing.T) {
	var inventoryCalls atomic.Int32
	var balanceCalls atomic.Int32
	enabled := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case "/api/customer/inventory":
			inventoryCalls.Add(1)
			_, _ = w.Write([]byte(`{"available":7,"missing":0,"estimated_total_fen":7000}`))
		case "/api/customer/balance":
			balanceCalls.Add(1)
			_, _ = w.Write([]byte(`{"balance_fen":12000,"held_fen":2000,"available_fen":10000,"currency":"CNY"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	service := New(nil, nil, server.Client())
	cfg := store.ManagerSupplyConfig{
		Enabled:            &enabled,
		BaseURL:            server.URL,
		Username:           "customer",
		Password:           "password",
		Product:            "oauth_30d",
		ReplenishBatchSize: 5,
	}
	service.hydrateOverviewIfNeeded(context.Background(), cfg)

	service.stateMu.RLock()
	overview := service.overview
	service.stateMu.RUnlock()
	if overview.Inventory == nil || overview.Inventory.Available != 7 ||
		overview.Balance == nil || overview.Balance.AvailableFen != 10_000 || overview.CheckedAtMS <= 0 {
		t.Fatalf("cold-start overview was not restored: %#v", overview)
	}

	service.hydrateOverviewIfNeeded(context.Background(), cfg)
	if inventoryCalls.Load() != 1 || balanceCalls.Load() != 1 {
		t.Fatalf("complete overview must not refetch on each UI poll: inventory=%d balance=%d", inventoryCalls.Load(), balanceCalls.Load())
	}
}

func TestHydrateOverviewIfNeededSkipsDisabledSupply(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Fatalf("disabled supply made an upstream request: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	disabled := false
	service := New(nil, nil, server.Client())
	service.hydrateOverviewIfNeeded(context.Background(), store.ManagerSupplyConfig{
		Enabled:            &disabled,
		BaseURL:            server.URL,
		Username:           "customer",
		Password:           "password",
		Product:            "oauth_7d",
		ReplenishBatchSize: 5,
	})

	if calls.Load() != 0 {
		t.Fatalf("disabled supply upstream calls = %d, want 0", calls.Load())
	}
}

func TestRetryRecoveryImportRunsImmediatelyWithoutClaimingAgain(t *testing.T) {
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			name := r.URL.Query().Get("name")
			if name != "" && uploads.Load() > 0 {
				_, _ = w.Write([]byte(`{"files":[{"name":"` + name + `","provider":"codex","disabled":false,"status":"ready"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"files":[]}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodPost:
			uploads.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			t.Fatalf("unexpected request during local import retry: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "recovery-manual-retry.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply:        store.ManagerSupplyConfig{Product: "oauth_30d"},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	const orderID = "recovery-manual-retry"
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: orderID, Product: "oauth_30d", RequestedQuantity: 1, Automatic: true,
		Strategy: "recovery", Status: "recovery_partial", RemoteStatus: "recovery_claimed", ItemCount: 1,
	}); err != nil {
		t.Fatalf("create recovery order: %v", err)
	}
	if _, err := st.InsertSupplyImportItems(context.Background(), orderID, []store.SupplyImportItem{{
		OrderID: orderID, ItemKey: "retry-account", FileName: "retry-account.json",
		PayloadJSON: `{"type":"codex","access_token":"access","refresh_token":"refresh","account_id":"account-retry","email":"retry@example.com"}`,
	}}); err != nil {
		t.Fatalf("insert recovery item: %v", err)
	}
	items, err := st.ListSupplyImportItemsByOrderIDs(context.Background(), []string{orderID})
	if err != nil || len(items) != 1 {
		t.Fatalf("recovery items=%#v err=%v", items, err)
	}
	if err := st.MarkSupplyImportItemFailed(context.Background(), items[0].ID, "database is locked (517)", time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if _, err := st.UpsertSupplyRecoveries(context.Background(), []store.SupplyRecovery{{
		RecoveryID: "manual-retry", Product: "oauth_30d", DeliveryStatus: "claimed", Status: "partial",
		ClaimOrderID: orderID, ItemCount: 1, LastError: "database is locked (517)", LastSeenAtMS: time.Now().UnixMilli(),
	}}); err != nil {
		t.Fatalf("upsert recovery: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	recovery, err := service.RetryRecoveryImport(context.Background(), "manual-retry")
	if err != nil {
		t.Fatalf("retry recovery import: %v", err)
	}
	if uploads.Load() != 1 {
		t.Fatalf("uploads = %d, want 1", uploads.Load())
	}
	if recovery.Status != "imported" || recovery.ImportedCount != 1 {
		t.Fatalf("recovery after retry = %#v", recovery)
	}
	items, err = st.ListSupplyImportItemsByOrderIDs(context.Background(), []string{orderID})
	if err != nil || len(items) != 1 || items[0].Status != "imported" || items[0].LastError != "" || items[0].NextRetryAtMS != 0 {
		t.Fatalf("items after retry=%#v err=%v", items, err)
	}
}

func TestListRecoveriesSeparatesClaimedFromImported(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "recovery-stage.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	createRecoverySourceOrder(t, st, "source-claimed-no-task")
	if _, err := st.UpsertSupplyRecoveries(context.Background(), []store.SupplyRecovery{{
		RecoveryID: "claimed-no-task", SourceOrderID: "source-claimed-no-task", DeliveryStatus: "claimed", Status: "claimed", LastSeenAtMS: time.Now().UnixMilli(),
	}}); err != nil {
		t.Fatalf("upsert recovery: %v", err)
	}
	service := New(st, nil)
	recoveries, err := service.ListRecoveries(context.Background(), 10, "")
	if err != nil || len(recoveries) != 1 {
		t.Fatalf("recoveries=%#v err=%v", recoveries, err)
	}
	if recoveries[0].ImportStatus != "claimed_without_local_payload" || recoveries[0].ImportedCount != 0 {
		t.Fatalf("recovery import stage = %#v", recoveries[0])
	}
}

func TestImportPendingFindsLegacyClaimWithPersistedLocalTask(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "recovery-legacy-link.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const orderID = "recovery-legacy-link"
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: orderID, Product: "oauth_30d", RequestedQuantity: 1, Automatic: true,
		Strategy: "recovery", Status: "recovery_importing", ItemCount: 1,
	}); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := st.InsertSupplyImportItems(context.Background(), orderID, []store.SupplyImportItem{{
		OrderID: orderID, ItemKey: "legacy-link-item", FileName: "legacy-link.json", PayloadJSON: `{"type":"codex"}`,
	}}); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	if _, err := st.UpsertSupplyRecoveries(context.Background(), []store.SupplyRecovery{{
		RecoveryID: "legacy-link", DeliveryStatus: "claimed", Status: "claimed", LastSeenAtMS: time.Now().UnixMilli(),
	}}); err != nil {
		t.Fatalf("upsert recovery: %v", err)
	}
	pending, err := st.ListImportPendingSupplyRecoveries(context.Background(), 10)
	if err != nil || len(pending) != 1 || pending[0].RecoveryID != "legacy-link" {
		t.Fatalf("legacy pending=%#v err=%v", pending, err)
	}
}

func TestRecoverySyncPreservesLocallySettledFailure(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "recovery-failed-sync.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if _, err := st.UpsertSupplyRecoveries(ctx, []store.SupplyRecovery{{
		RecoveryID: "failed-sync", DeliveryStatus: "claimed", Status: "partial",
		ClaimOrderID: "recovery-failed-sync", ItemCount: 1, LastSeenAtMS: now,
	}}); err != nil {
		t.Fatalf("insert recovery: %v", err)
	}
	if err := st.MarkSupplyRecoveryFailed(ctx, "failed-sync", "usage endpoint returned 402"); err != nil {
		t.Fatalf("mark recovery failed: %v", err)
	}
	if _, err := st.UpsertSupplyRecoveries(ctx, []store.SupplyRecovery{{
		RecoveryID: "failed-sync", DeliveryStatus: "claimed", Status: "claimed", LastSeenAtMS: now + 1,
	}}); err != nil {
		t.Fatalf("sync remote recovery: %v", err)
	}
	recovery, found, err := st.GetSupplyRecovery(ctx, "failed-sync")
	if err != nil || !found || recovery.Status != "failed" {
		t.Fatalf("recovery=%#v found=%v err=%v", recovery, found, err)
	}
}

func supplyReportEvent(
	hash string,
	timestampMS int64,
	model string,
	authFile string,
	failed bool,
	inputTokens int64,
	outputTokens int64,
	reasoningTokens int64,
	cachedTokens int64,
	cacheTokens int64,
	totalTokens int64,
	latencyMS *int64,
) usage.Event {
	return usage.Event{
		EventHash:        hash,
		TimestampMS:      timestampMS,
		Timestamp:        time.UnixMilli(timestampMS).UTC().Format(time.RFC3339Nano),
		Model:            model,
		Endpoint:         "POST /v1/chat/completions",
		Method:           "POST",
		Path:             "/v1/chat/completions",
		AuthIndex:        "auth-1",
		AuthFileSnapshot: authFile,
		Source:           "ops@example.com",
		SourceHash:       "source-hash",
		APIKeyHash:       "api-key-hash",
		AccountSnapshot:  "ops@example.com",
		InputTokens:      inputTokens,
		OutputTokens:     outputTokens,
		ReasoningTokens:  reasoningTokens,
		CachedTokens:     cachedTokens,
		CacheTokens:      cacheTokens,
		TotalTokens:      totalTokens,
		LatencyMS:        latencyMS,
		Failed:           failed,
		CreatedAtMS:      timestampMS,
	}
}

func TestApplyNvtokensOrderItemsStoresWarrantyWithoutLease(t *testing.T) {
	now := time.Date(2026, time.August, 21, 6, 0, 0, 0, time.UTC)
	accounts := []normalizedSupplyAccount{
		{leaseExpiresAtMS: now.Add(time.Hour).UnixMilli()},
		{leaseExpiresAtMS: now.Add(time.Hour).UnixMilli()},
	}
	if !applySupplyOrderItemDetails(accounts, []supplyclient.OrderItem{
		{RemainingSeconds: 900, HasRemaining: true, BasePriceFen: 1700, ChargedFen: 1700},
		{RemainingSeconds: 1800, HasRemaining: true, BasePriceFen: 1749, ChargedFen: 1749},
	}, now, true) {
		t.Fatal("exact nvtokens item mapping should be accepted")
	}
	for index, expected := range []int64{900, 1800} {
		if accounts[index].leaseExpiresAtMS != 0 {
			t.Fatalf("account %d scheduling lease = %d, want 0", index, accounts[index].leaseExpiresAtMS)
		}
		if got := (accounts[index].warrantyExpiresAtMS - now.UnixMilli()) / 1000; got != expected {
			t.Fatalf("account %d warranty seconds = %d, want %d", index, got, expected)
		}
	}
}

func TestMigrateNvtokensLeaseToWarrantyLeavesOtherSuppliersUntouched(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	leaseExpiresAtMS := now.Add(45 * time.Minute).UnixMilli()
	legacyExpiresAtMS := now.Add(30 * time.Minute).UnixMilli()
	payloads := map[string][]byte{
		"nv.json":     []byte(fmt.Sprintf(`{"type":"codex","access_token":"nv","expires_at":%d,"supply_lease_expires_at_ms":%d,"supply_lease_expires_at":"%s"}`, now.Add(10*24*time.Hour).Unix(), leaseExpiresAtMS, time.UnixMilli(leaseExpiresAtMS).UTC().Format(time.RFC3339))),
		"legacy.json": []byte(fmt.Sprintf(`{"type":"codex","access_token":"legacy","expires_at":%d,"supply_lease_expires_at_ms":%d}`, now.Add(7*24*time.Hour).Unix(), legacyExpiresAtMS)),
	}
	var payloadMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files/download" && r.Method == http.MethodGet:
			payloadMu.Lock()
			payload, ok := payloads[r.URL.Query().Get("name")]
			payloadMu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(payload)
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodPost:
			reader, err := r.MultipartReader()
			if err != nil {
				t.Fatalf("multipart reader: %v", err)
			}
			for {
				part, partErr := reader.NextPart()
				if partErr == io.EOF {
					break
				}
				if partErr != nil {
					t.Fatalf("multipart part: %v", partErr)
				}
				if part.FormName() != "file" {
					continue
				}
				payload, readErr := io.ReadAll(part)
				if readErr != nil {
					t.Fatalf("read upload: %v", readErr)
				}
				payloadMu.Lock()
				payloads[part.FileName()] = payload
				payloadMu.Unlock()
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "nvtokens-warranty.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{Platforms: []store.ManagerSupplyPlatformConfig{
			{ID: "nv", Type: managerconfigsvc.SupplyPlatformNvtokens, BaseURL: "https://nvtokens.test", Product: "plus"},
			{ID: "legacy", Type: managerconfigsvc.SupplyPlatformLegacy, BaseURL: "https://legacy.test", Product: "oauth_7d"},
		}},
	}
	for _, order := range []store.SupplyOrder{
		{OrderID: "order-nv", SupplierID: "nv", Product: "plus", RequestedQuantity: 1, Status: "completed"},
		{OrderID: "order-legacy", SupplierID: "legacy", Product: "oauth_7d", RequestedQuantity: 1, Status: "completed"},
	} {
		if _, err := st.CreateSupplyOrder(ctx, order); err != nil {
			t.Fatalf("create order %s: %v", order.OrderID, err)
		}
	}
	for _, input := range []struct {
		orderID, itemKey, fileName string
		leaseExpiresAtMS           int64
	}{
		{"order-nv", "nv-item", "nv.json", leaseExpiresAtMS},
		{"order-legacy", "legacy-item", "legacy.json", legacyExpiresAtMS},
	} {
		if _, err := st.InsertSupplyImportItems(ctx, input.orderID, []store.SupplyImportItem{{
			ItemKey: input.itemKey, FileName: input.fileName, PayloadJSON: string(payloads[input.fileName]), LeaseExpiresAtMS: input.leaseExpiresAtMS,
		}}); err != nil {
			t.Fatalf("insert %s: %v", input.itemKey, err)
		}
		items, err := st.ListSupplyImportItemsByOrderIDs(ctx, []string{input.orderID})
		if err != nil || len(items) != 1 {
			t.Fatalf("list %s items=%#v err=%v", input.orderID, items, err)
		}
		if err := st.MarkSupplyImportItemImported(ctx, items[0].ID, now.UnixMilli()); err != nil {
			t.Fatalf("mark %s imported: %v", input.itemKey, err)
		}
	}

	service := New(st, nil, server.Client())
	items, err := st.ListSupplyImportItems(ctx, 10, "imported")
	if err != nil {
		t.Fatalf("list imports: %v", err)
	}
	if err := service.migrateNvtokensWarrantyMetadata(ctx, cfg, items); err != nil {
		t.Fatalf("migrate warranty: %v", err)
	}

	migrated, err := st.ListSupplyImportItemsByOrderIDs(ctx, []string{"order-nv", "order-legacy"})
	if err != nil || len(migrated) != 2 {
		t.Fatalf("migrated items=%#v err=%v", migrated, err)
	}
	byOrder := map[string]store.SupplyImportItem{}
	for _, item := range migrated {
		byOrder[item.OrderID] = item
	}
	if got := byOrder["order-nv"]; got.LeaseExpiresAtMS != 0 || got.WarrantyExpiresAtMS != leaseExpiresAtMS {
		t.Fatalf("nvtokens item = %#v", got)
	}
	if got := byOrder["order-legacy"]; got.LeaseExpiresAtMS != legacyExpiresAtMS || got.WarrantyExpiresAtMS != 0 {
		t.Fatalf("legacy item = %#v", got)
	}
	active, err := st.ListActiveImportedSupplyItems(ctx, now.UnixMilli())
	if err != nil || len(active) != 1 || active[0].FileName != "legacy.json" {
		t.Fatalf("scheduling leases after migration = %#v err=%v", active, err)
	}
	payloadMu.Lock()
	nvPayload := append([]byte(nil), payloads["nv.json"]...)
	legacyPayload := append([]byte(nil), payloads["legacy.json"]...)
	payloadMu.Unlock()
	var nvMetadata, legacyMetadata map[string]any
	if err := json.Unmarshal(nvPayload, &nvMetadata); err != nil {
		t.Fatalf("decode migrated nv payload: %v", err)
	}
	if err := json.Unmarshal(legacyPayload, &legacyMetadata); err != nil {
		t.Fatalf("decode legacy payload: %v", err)
	}
	if int64(numberField(nvMetadata, "supply_warranty_expires_at_ms")) != leaseExpiresAtMS ||
		int64(numberField(nvMetadata, "supply_lease_expires_at_ms")) != 0 ||
		stringFromMap(nvMetadata, "supply_lease_expires_at") != "" {
		t.Fatalf("migrated nv metadata = %#v", nvMetadata)
	}
	if int64(numberField(legacyMetadata, "supply_lease_expires_at_ms")) != legacyExpiresAtMS ||
		int64(numberField(legacyMetadata, "supply_warranty_expires_at_ms")) != 0 {
		t.Fatalf("legacy metadata changed = %#v", legacyMetadata)
	}
}
