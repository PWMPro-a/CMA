package supply

import (
	"context"
	"encoding/json"
	"errors"
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

	disabled := service.currentAutomationExecution(false)
	if disabled.Enabled || disabled.NextExecutionAtMS != 0 || disabled.IntervalSeconds != 0 {
		t.Fatalf("disabled automation must not expose a future execution: %#v", disabled)
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

func TestEmergencyRetryNextIntervalHonorsShortCooldown(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name    string
		order   store.SupplyOrder
		wantMin time.Duration
		wantMax time.Duration
	}{
		{
			name: "cancelled order waits for remaining retry cooldown",
			order: store.SupplyOrder{
				OrderID: "cancelled-recent", Product: "oauth_30d", Automatic: true,
				Status: "cancelled", CompletedAtMS: now.Add(-4 * time.Second).UnixMilli(),
			},
			wantMin: 5 * time.Second,
			wantMax: 7 * time.Second,
		},
		{
			name: "cancelled order retries promptly after cooldown",
			order: store.SupplyOrder{
				OrderID: "cancelled-ready", Product: "oauth_30d", Automatic: true,
				Status: "cancelled", CompletedAtMS: now.Add(-11 * time.Second).UnixMilli(),
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
	resource.SnapshotFresh = true
	resource.SnapshotRefreshInProgress = false
	resource.CapacitySnapshotAtMS = nowMS
	resource.PendingInspectionAccounts = 6
	if reason := service.automaticSupplyGuardReason(resource); reason != "pending_account_inspection" {
		t.Fatalf("pending import guard reason = %q", reason)
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
	resource := SmartResource{HealthyAccounts: 1}
	applyAccountPoolStats(&resource, stats)
	if resource.TotalAccounts != 3 || resource.AvailableAccounts != 2 || resource.SchedulableAccounts != 2 ||
		resource.HealthyAccounts != 1 || resource.WeakAccounts != 1 || resource.DisabledAccounts != 1 {
		t.Fatalf("account pool statistics = %#v", resource)
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
	}
	stats := reconcileAccountPoolStatsWithInspection(accountPoolStats{total: 90, schedulable: 28}, resource)
	applyAccountPoolStats(&resource, stats)
	if resource.AvailableAccounts != 12 || resource.SchedulableAccounts != 28 ||
		resource.WeakAccounts != 3 || resource.DisabledAccounts != 62 {
		t.Fatalf("verified pool statistics = %#v", resource)
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
	}
	New(nil, nil).reconcileSmartAccountPoolGuard(cfg, &resource)
	if resource.DecisionReason != "healthy_available_accounts" ||
		resource.SuggestedAction != smartActionEmergencyReplenish || resource.SuggestedQuantity != 10 ||
		resource.AccountQuantityDeficit != 88 {
		t.Fatalf("verified healthy-floor plan = %#v", resource)
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

	applyAccountPoolStats(&resource, accountPoolStats{total: 1220, schedulable: 5})
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
	if resource.HealthLevel != smartHealthCritical || resource.EstimatedRequiredAccounts != 109 ||
		resource.AccountQuantityDeficit != 104 || resource.SuggestedQuantity != 20 {
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
			if got := r.URL.Query().Get("ticket"); got != "ticket-1" {
				t.Fatalf("claim ticket = %q", got)
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
		LeaseExpiresAtMS: expiredAt,
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

func TestManualReplenishmentRejectsConcurrentOrder(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "active-order", Product: "oauth_30d", RequestedQuantity: 2, Status: "waiting_inventory",
	}); err != nil {
		t.Fatalf("create active order: %v", err)
	}
	managerConfig := managerconfigsvc.New(config.Config{}, st, nil)
	service := New(st, managerConfig)
	_, err = service.Replenish(context.Background(), 2)
	if err != ErrOrderInProgress {
		t.Fatalf("replenish error = %v, want %v", err, ErrOrderInProgress)
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
		result["chatgpt_plan_type"] != "team" || result["organization_id"] != "org-extra" ||
		result["workspace_id"] != "workspace-1" ||
		result["expired"] != "2026-07-30T00:00:00Z" || result["max_concurrency"] != float64(8) ||
		result["selection_error_freeze_seconds"] != float64(0) || result["codex_cli_only"] != true ||
		result["codex_cli_only_allow_app_server"] != false {
		t.Fatalf("normalized metadata = %#v", result)
	}
	if len(key) != 64 || fileName != "codex-user@example.com.json" {
		t.Fatalf("stable identity outputs key=%q file=%q", key, fileName)
	}
}

func TestNormalizeDirectCPAAccountPayloadDisablesSelectionErrorFreeze(t *testing.T) {
	payload, _, _, err := normalizeAccountPayload([]byte(`{"type":"codex","email":"direct@example.com","account_id":"direct-account","access_token":"access","max_concurrency":8,"selection_error_freeze_seconds":45}`))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode normalized payload: %v", err)
	}
	if result["max_concurrency"] != float64(8) || result["selection_error_freeze_seconds"] != float64(0) ||
		result["codex_cli_only"] != true || result["codex_cli_only_allow_app_server"] != false {
		t.Fatalf("normalized runtime limits = %#v", result)
	}
}

func TestSupplyFileNameStaysStableWhenCredentialsChange(t *testing.T) {
	first, err := normalizeAccountPayloads([]byte(`{"name":"普通 Team · 7D · 有效期 51 分钟","type":"oauth","platform":"openai","credentials":{"access_token":"access-one","refresh_token":"refresh-one","chatgpt_account_id":"account-one","email":"stable@example.com"}}`))
	if err != nil || len(first) != 1 {
		t.Fatalf("normalize first account: %#v err=%v", first, err)
	}
	second, err := normalizeAccountPayloads([]byte(`{"name":"普通 Team · 7D · 有效期 2 分钟","type":"oauth","platform":"openai","credentials":{"access_token":"access-two","refresh_token":"refresh-two","chatgpt_account_id":"account-two","email":"stable@example.com"}}`))
	if err != nil || len(second) != 1 {
		t.Fatalf("normalize replacement account: %#v err=%v", second, err)
	}
	if first[0].accountName != "stable@example.com" || second[0].accountName != first[0].accountName ||
		first[0].fileName != "codex-stable@example.com.json" || second[0].fileName != first[0].fileName {
		t.Fatalf("stable account filenames first=%q second=%q", first[0].fileName, second[0].fileName)
	}
	if first[0].itemKey == second[0].itemKey {
		t.Fatal("credential identity should still distinguish different underlying accounts")
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
	const expectedFileName = "codex-stable@example.com.json"
	if uploadedName != expectedFileName || deletedName != oldFileName {
		t.Fatalf("migration uploaded=%q deleted=%q", uploadedName, deletedName)
	}
	if items[0].AccountName != "stable@example.com" || items[0].NameKey != "stable@example.com" || items[0].FileName != expectedFileName {
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

func TestRecoveryWithoutOriginalFileReusesUniqueAccountNameBinding(t *testing.T) {
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
	if plan.action != "replace" || plan.fileName != "legacy-stable-file.json" || plan.replacedFileName != "legacy-stable-file.json" {
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
	if secondPlan.action != "add" || secondPlan.fileName != "codex-second@example.com.json" {
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
	if first["codex_cli_only"] != true || first["codex_cli_only_allow_app_server"] != false {
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
	if second["codex_cli_only"] != true || second["codex_cli_only_allow_app_server"] != false {
		t.Fatalf("second normalized payload client restriction = %#v", second)
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

func TestAutomaticOrderLocallyReleasesWhenCPATargetIsAlreadySatisfied(t *testing.T) {
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
			t.Fatal("locally released order must not be polled")
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
		t.Fatalf("locally release satisfied order: %v", err)
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status.ActiveOrder != nil {
		t.Fatalf("active order should be cleared: %#v", status.ActiveOrder)
	}
	if len(status.Orders) != 1 || status.Orders[0].Status != "released" ||
		status.Orders[0].RemoteStatus != remoteStatusAutomaticReleasePending || status.Orders[0].ReleasedFen != 0 {
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
			_, _ = w.Write([]byte(`{"id":"order-reserved","status":"ready","ready_quantity":10,"progress":100,"take_url":"/api/customer/pickup/orders/order-reserved/take"}`))
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
		OrderID: "order-reserved", Product: "oauth_7d", RequestedQuantity: 10, Automatic: true,
		Status: "ready", ReadyQuantity: 10, Progress: 100,
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
	if uploadCalls.Load() != 1 || uploadedName != "codex-legacy@example.com.json" {
		t.Fatalf("upload calls=%d uploaded name=%q", uploadCalls.Load(), uploadedName)
	}
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
