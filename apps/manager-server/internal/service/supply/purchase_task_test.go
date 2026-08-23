package supply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/config"
	managerconfigsvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

func TestSummarizePurchaseTaskOrdersCountsDeliveredBeforeCommitted(t *testing.T) {
	stats := summarizePurchaseTaskOrders([]store.SupplyOrder{
		{Status: "completed", RequestedQuantity: 5, ItemCount: 5, ImportedCount: 3},
		{Status: "failed", RequestedQuantity: 2, ItemCount: 2, ImportedCount: 0},
		{Status: "ready", RequestedQuantity: 4, ReadyQuantity: 4},
		{Status: "waiting_inventory", RequestedQuantity: 10},
		{Status: "failed", RequestedQuantity: 7},
	})
	if stats.fulfilled != 3 || stats.committedPending != 4 || stats.reservedPending != 14 || stats.orderCount != 5 || stats.activeOrderCount != 2 {
		t.Fatalf("task order stats = %#v", stats)
	}
}

func TestSummarizePurchaseTaskOrdersStopsCountingStaleAutomaticWaitingReservation(t *testing.T) {
	staleAt := time.Now().Add(-purchaseTaskStaleAutomaticWaitingAge - time.Second).UnixMilli()
	stats := summarizePurchaseTaskOrders([]store.SupplyOrder{
		{TaskID: "task", Automatic: true, Status: "waiting_inventory", RequestedQuantity: 7, CreatedAtMS: staleAt},
		{TaskID: "task", Automatic: true, Status: "waiting_inventory", RequestedQuantity: 2, CreatedAtMS: time.Now().UnixMilli()},
	})
	if stats.activeOrderCount != 1 {
		t.Fatalf("active order count = %d, want only fresh reservation 1", stats.activeOrderCount)
	}
	if stats.reservedPending != 2 {
		t.Fatalf("reserved pending = %d, want only fresh reservation 2", stats.reservedPending)
	}
}

func TestPurchaseTaskReadyTakeAllowedWhilePlannerSnapshotIsStale(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-ready-take.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	task, err := st.CreateSupplyPurchaseTask(ctx, store.SupplyPurchaseTask{
		TaskID: "task-stale-ready", Source: "automatic", SupplierID: "legacy", Product: "oauth_7d",
		TargetQuantity: 48, Status: purchaseTaskStatusRunning, MaxConcurrentOrders: 3,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "task-stale-delivered", TaskID: task.TaskID, SupplierID: "legacy", Product: "oauth_7d",
		RequestedQuantity: 18, Automatic: true, Status: "completed", ItemCount: 18, ImportedCount: 18,
	}); err != nil {
		t.Fatalf("create delivered order: %v", err)
	}
	ready, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "task-stale-ready-order", TaskID: task.TaskID, SupplierID: "legacy", Product: "oauth_7d",
		RequestedQuantity: 30, Automatic: true, Status: "ready", RemoteStatus: "completed", ChargedFen: 6720,
	})
	if err != nil {
		t.Fatalf("create ready order: %v", err)
	}
	service := New(st, nil)
	allowed, err := service.purchaseTaskReadyTakeAllowed(ctx, ready)
	if err != nil || !allowed {
		t.Fatalf("ready take allowed=%v err=%v", allowed, err)
	}
	task, found, err := st.GetSupplyPurchaseTask(ctx, task.TaskID)
	if err != nil || !found || task.FulfilledQuantity != 18 || task.Status != purchaseTaskStatusRunning {
		t.Fatalf("reconciled task=%#v found=%v err=%v", task, found, err)
	}
	service.setSmartResource(SmartResource{
		Enabled: true, GeneratedAtMS: time.Now().UnixMilli(), SnapshotFresh: false, SuggestedAction: smartActionTakeLocked,
		DecisionReason: "purchase_task_ready_stale_snapshot",
	})
	if !service.smartTakeAllowed(store.ManagerSupplyConfig{}, ready.OrderID) {
		t.Fatal("task-backed ready order must pass the stale-snapshot take gate")
	}
}

func TestPurchaseTaskPaidReadyOrderIgnoresMovingStatusPollDeadline(t *testing.T) {
	service := &Service{}
	nowMS := time.Now().UnixMilli()
	order := store.SupplyOrder{
		OrderID:              "paid-ready-deadline",
		TaskID:               "purchase-paid-ready-deadline",
		Automatic:            true,
		Status:               "ready",
		RemoteStatus:         "completed",
		ReadyQuantity:        1,
		ChargedFen:           1111,
		NextPollAtMS:         nowMS + time.Minute.Milliseconds(),
		SupplierRetryUntilMS: nowMS - time.Second.Milliseconds(),
	}
	if !service.purchaseTaskOrderPollDue(store.ManagerSupplyConfig{}, order, nowMS) {
		t.Fatal("paid ready order must be processed even when the dashboard moved its local poll deadline")
	}
}

func TestPurchaseTaskReadyTakeAllowedSharesBudgetAcrossReadySiblings(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-ready-siblings.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	task, err := st.CreateSupplyPurchaseTask(ctx, store.SupplyPurchaseTask{
		TaskID: "task-ready-siblings", Source: "automatic", SupplierID: "legacy", Product: "oauth_7d",
		TargetQuantity: 10, Status: purchaseTaskStatusRunning, MaxConcurrentOrders: 2,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	createdAt := time.Now().UnixMilli()
	earlier, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "task-ready-earlier", TaskID: task.TaskID, SupplierID: "legacy", Product: "oauth_7d",
		RequestedQuantity: 8, ReadyQuantity: 8, Automatic: true, Status: "ready", CreatedAtMS: createdAt,
	})
	if err != nil {
		t.Fatalf("create earlier ready order: %v", err)
	}
	later, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "task-ready-later", TaskID: task.TaskID, SupplierID: "legacy", Product: "oauth_7d",
		RequestedQuantity: 8, ReadyQuantity: 8, Automatic: true, Status: "ready", CreatedAtMS: createdAt + 1,
	})
	if err != nil {
		t.Fatalf("create later ready order: %v", err)
	}
	service := New(st, nil)
	allowed, err := service.purchaseTaskReadyTakeAllowed(ctx, earlier)
	if err != nil || !allowed {
		t.Fatalf("earlier ready allowed=%v err=%v", allowed, err)
	}
	allowed, err = service.purchaseTaskReadyTakeAllowed(ctx, later)
	if err != nil || allowed {
		t.Fatalf("later ready allowed=%v err=%v, want shared task budget rejection", allowed, err)
	}
}

func TestPurchaseTaskNextOrderQuantityShardsRemainingTarget(t *testing.T) {
	tests := []struct {
		remaining int
		slots     int
		want      int
	}{
		{remaining: 20, slots: 3, want: 7},
		{remaining: 13, slots: 2, want: 7},
		{remaining: 6, slots: 1, want: 6},
		{remaining: 250, slots: 3, want: 84},
		{remaining: 400, slots: 3, want: 100},
	}
	for _, test := range tests {
		if got := purchaseTaskNextOrderQuantity(test.remaining, test.slots); got != test.want {
			t.Fatalf("remaining=%d slots=%d quantity=%d, want %d", test.remaining, test.slots, got, test.want)
		}
	}
}

func TestPurchaseTaskAdaptiveOrderQuantityWidensSlowScarceCaptureWindow(t *testing.T) {
	resource := SmartResource{
		SupplyPressureLevel:      smartSupplyPressureScarce,
		SupplyNeedsProduction:    true,
		SupplyAvgFulfillSeconds:  180,
		SupplyRecentWaiting:      3,
		SupplyRecentOrders:       6,
		SupplyFulfillmentRate:    70,
		SupplyRecentZeroDelivery: 2,
	}
	if got := purchaseTaskAdaptiveOrderQuantity(20, 3, true, resource); got != 9 {
		t.Fatalf("slow scarce adaptive quantity = %d, want 9", got)
	}
	if got := purchaseTaskAdaptiveOrderQuantity(20, 3, false, resource); got != 7 {
		t.Fatalf("manual quantity = %d, want normal shard 7", got)
	}
	if got := purchaseTaskAdaptiveOrderQuantity(20, 3, true, SmartResource{}); got != 7 {
		t.Fatalf("normal quantity = %d, want normal shard 7", got)
	}
}

func TestAutomaticLowPriceTaskDoesNotOverrideLiveReplenishmentTask(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-low-price-priority.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	service := New(st, nil)
	live, err := service.upsertAutomaticPurchaseTask(ctx, store.SupplyPurchaseTask{
		Product: "oauth_30d", TargetQuantity: 5, Status: purchaseTaskStatusPending,
		TriggerReason: "emergency_refill_to_healthy", MaxConcurrentOrders: 2,
	})
	if err != nil {
		t.Fatalf("create live task: %v", err)
	}
	selected, err := service.upsertAutomaticPurchaseTask(ctx, store.SupplyPurchaseTask{
		Product: "oauth_30d", TargetQuantity: 10, Status: purchaseTaskStatusPending,
		TriggerReason: lowPriceReserveTriggerReason, MaxConcurrentOrders: 1,
	})
	if err != nil {
		t.Fatalf("upsert low-price task: %v", err)
	}
	if selected.TaskID != live.TaskID || selected.TargetQuantity != 5 || isLowPriceReserveTrigger(selected.TriggerReason) {
		t.Fatalf("live task was overridden: live=%#v selected=%#v", live, selected)
	}
}

func TestRepeatedLowPriceWatcherDoesNotExpandActiveStage(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-low-price-bounded.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	service := New(st, nil)
	first, err := service.upsertAutomaticPurchaseTask(ctx, store.SupplyPurchaseTask{
		Product: "plus", TargetQuantity: 15, Status: purchaseTaskStatusPending,
		TriggerReason: lowPriceReserveTriggerReason, MaxConcurrentOrders: 1,
	})
	if err != nil {
		t.Fatalf("create low-price stage: %v", err)
	}
	second, err := service.upsertAutomaticPurchaseTask(ctx, store.SupplyPurchaseTask{
		Product: "plus", TargetQuantity: 15, Status: purchaseTaskStatusPending,
		TriggerReason: lowPriceReserveTriggerReason, MaxConcurrentOrders: 1,
	})
	if err != nil {
		t.Fatalf("repeat low-price stage: %v", err)
	}
	if second.TaskID != first.TaskID || second.TargetQuantity != 15 {
		t.Fatalf("repeated stage = %#v first=%#v", second, first)
	}
}

func TestLowPriceReserveOrderUsesDedicatedHardPriceCeiling(t *testing.T) {
	var submittedCeiling atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files":
			_, _ = w.Write([]byte(`{"files":[]}`))
		case r.URL.Path == "/api/workspace/extractions/estimate":
			_, _ = w.Write([]byte(`{"estimate":{"available_quantity":10,"buyer_total_cents":500,"min_unit_price_cents":500,"max_unit_price_cents":500}}`))
		case r.URL.Path == "/api/me":
			_, _ = w.Write([]byte(`{"available_balance_cents":100000,"balance_cents":100000}`))
		case r.URL.Path == "/api/workspace/extractions/batch" && r.Method == http.MethodPost:
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode batch payload: %v", err)
			}
			if value, ok := payload["max_unit_price_cents"].(float64); ok {
				submittedCeiling.Store(int64(value))
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"pending-low-price","status":"processing"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-low-price-ceiling.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	lowCeiling := int64(1300)
	platformCeiling := int64(2400)
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, LowPriceReserveEnabled: &enabled, LowPriceReserveMaxUnitPriceFen: &lowCeiling,
			LowPriceReserveTargetAccounts: 50, LowPriceReserveCheckIntervalMilliseconds: 1000,
			Platforms: []store.ManagerSupplyPlatformConfig{{
				ID: "nv", Type: managerconfigsvc.SupplyPlatformNvtokens, Enabled: &enabled,
				BaseURL: server.URL, Token: "nv-token", Product: "plus", MaxUnitPriceFen: &platformCeiling,
			}},
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyPurchaseTask(ctx, store.SupplyPurchaseTask{
		TaskID: "low-price-hard-ceiling", Source: "automatic", Product: "plus", TargetQuantity: 1,
		Status: purchaseTaskStatusPending, TriggerReason: lowPriceReserveTriggerReason, MaxConcurrentOrders: 1,
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunPurchaseTasks(ctx); err != nil {
		t.Fatalf("run low-price task: %v", err)
	}
	if got := submittedCeiling.Load(); got != lowCeiling {
		t.Fatalf("submitted ceiling = %d, want dedicated low-price ceiling %d", got, lowCeiling)
	}
}

func TestPurchaseTaskStopsOrdinaryOrderWhenAccountTargetIsAlreadyReached(t *testing.T) {
	var supplierCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files":
			files := make([]map[string]any, 0, 51)
			for index := 0; index < 51; index++ {
				files = append(files, map[string]any{
					"name": fmt.Sprintf("available-%d.json", index), "provider": "codex", "status": "active",
					"auth_index": fmt.Sprintf("available-%d", index), "access_token": "token",
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
		default:
			supplierCalls.Add(1)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-account-target.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, SmartEnabled: &enabled, TargetAvailableAccounts: 45,
			BaseURL: server.URL, Username: "customer", Password: "password", Product: "plus",
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	created, err := st.CreateSupplyPurchaseTask(ctx, store.SupplyPurchaseTask{
		TaskID: "ordinary-capacity-drift", Source: "automatic", Product: "plus", TargetQuantity: 1,
		Status: purchaseTaskStatusPending, TriggerReason: "supply_plenty_small_batch", MaxConcurrentOrders: 1,
	})
	if err != nil {
		t.Fatalf("create ordinary task: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	stats, countErr := service.countAccountPoolStats(ctx, store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
	})
	if countErr != nil || stats.schedulable != 51 {
		t.Fatalf("account stats=%#v err=%v", stats, countErr)
	}
	service.setSmartResource(SmartResource{
		Enabled: true, GeneratedAtMS: time.Now().UnixMilli(), SnapshotFresh: true,
		CapacitySource: smartCapacitySourceInspection, CapacitySnapshotAtMS: time.Now().UnixMilli(),
		AvailableAccounts: 51, HealthLevel: smartHealthWarning, SuggestedAction: smartActionPrelock,
		SuggestedQuantity: 1, CapacityGapRCU: 17.3,
	})

	if err := service.RunPurchaseTasks(ctx); err != nil {
		t.Fatalf("run purchase tasks: %v", err)
	}
	task, found, err := st.GetSupplyPurchaseTask(ctx, created.TaskID)
	if err != nil || !found {
		t.Fatalf("get task found=%v err=%v", found, err)
	}
	if task.Status != purchaseTaskStatusCancelled || task.LastError != "ordinary account target reached; normal procurement stopped" {
		t.Fatalf("task = %#v", task)
	}
	if supplierCalls.Load() != 0 {
		t.Fatalf("supplier calls = %d, want 0", supplierCalls.Load())
	}
	orders, err := st.ListSupplyOrders(ctx, 10)
	if err != nil || len(orders) != 0 {
		t.Fatalf("orders = %#v err=%v", orders, err)
	}
}

func TestPurchaseTaskRechecksRecoveredTimingAfterSupplierQuote(t *testing.T) {
	var quoteCalls atomic.Int32
	var createCalls atomic.Int32
	var service *Service
	var supplyCfg store.ManagerSupplyConfig
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files":
			_, _ = w.Write([]byte(`{"files":[]}`))
		case r.URL.Path == "/api/customer/inventory":
			quoteCalls.Add(1)
			resource := service.currentSmartResource(supplyCfg)
			resource.HealthLevel = smartHealthWarning
			resource.EmergencyShortage = false
			resource.EmergencyReason = ""
			resource.CurrentCapacityRCU = 26770
			resource.AvailableCapacityRCU = 26770
			resource.TotalCapacityRCU = 26770
			resource.TargetCapacityRCU = 31658
			resource.CapacityGapRCU = 4888
			resource.EstimatedSustainMinutes = 101.5
			resource.AvailableSustainMinutes = 101.5
			resource.PurchaseTimingTriggerMinutes = 93.3
			resource.PurchaseTimingWaitMinutes = 8.2
			resource.PurchaseTimingEligibleQuantity = 0
			resource.SuggestedAction = smartActionObserveDemand
			resource.SuggestedQuantity = 0
			resource.DecisionReason = "purchase_timing_wait"
			service.setSmartResource(resource)
			_, _ = w.Write([]byte(`{"available":10,"missing":0,"estimated_total_fen":2486,"estimated_unit_price_fen":2486}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":100000,"balance_fen":100000}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			createCalls.Add(1)
			_, _ = w.Write([]byte(`{"order":{"id":"too-late-order","status":"waiting_inventory","quantity":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-recovered-timing.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	supplyCfg = store.ManagerSupplyConfig{
		Enabled: &enabled, SmartEnabled: &enabled, TargetAvailableAccounts: 45, CheckIntervalSeconds: 30,
		Product: "oauth_7d",
		Platforms: []store.ManagerSupplyPlatformConfig{{
			ID: "legacy", Type: managerconfigsvc.SupplyPlatformLegacy, Enabled: &enabled,
			BaseURL: server.URL, Token: "supplier-token", Product: "oauth_7d",
		}},
	}
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply:        supplyCfg,
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	task, err := st.CreateSupplyPurchaseTask(ctx, store.SupplyPurchaseTask{
		TaskID: "ordinary-recovered-during-quote", Source: "automatic", Product: "oauth_7d",
		TargetQuantity: 1, Status: purchaseTaskStatusPending, TriggerReason: "low_water_staged_batch",
		MaxConcurrentOrders: 1,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	service = New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	service.setSmartResource(SmartResource{
		Enabled: true, GeneratedAtMS: time.Now().UnixMilli(), SnapshotFresh: true,
		CapacitySource: smartCapacitySourceInspection, CapacitySnapshotAtMS: time.Now().UnixMilli(),
		AvailableAccounts: 13, HealthLevel: smartHealthWarning, SuggestedAction: smartActionPrelock,
		SuggestedQuantity: 1, CapacityGapRCU: 2305, UnitCapacityRCU: 40,
		ConsumeRCUPerMinute: 264, EstimatedSustainMinutes: 90, AvailableSustainMinutes: 90,
		ConfiguredHealthyMinutes: 120, EffectiveHealthyMinutes: 120, HealthyMinutesTarget: 120,
		WarningMinutes: 60, CriticalMinutes: 30, DecisionReason: "low_water_staged_batch",
		PurchaseTimingEligibleQuantity: 1,
	})

	if err := service.RunPurchaseTasks(ctx); err != nil {
		t.Fatalf("run purchase tasks: %v", err)
	}
	if quoteCalls.Load() == 0 {
		t.Fatal("supplier quote was not reached; test did not exercise the in-flight recovery race")
	}
	if createCalls.Load() != 0 {
		t.Fatalf("supplier create calls = %d, want 0; resource=%#v", createCalls.Load(), service.currentSmartResource(supplyCfg))
	}
	task, found, err := st.GetSupplyPurchaseTask(ctx, task.TaskID)
	if err != nil || !found {
		t.Fatalf("get task found=%v err=%v", found, err)
	}
	if task.Status != purchaseTaskStatusCancelled || task.CancelledAtMS == 0 || task.LastError != "" {
		t.Fatalf("cancelled timing task = %#v", task)
	}
	orders, err := st.ListSupplyOrdersByTaskID(ctx, task.TaskID)
	if err != nil || len(orders) != 0 {
		t.Fatalf("orders = %#v err=%v", orders, err)
	}
}

func TestManualNvtokensOrderIgnoresAutomaticPriceCeiling(t *testing.T) {
	var estimateMaxUnitPrice any = "missing"
	var createMaxUnitPrice any = "missing"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/workspace/extractions/estimate":
			payload := map[string]any{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode estimate payload: %v", err)
			}
			estimateMaxUnitPrice = payload["max_unit_price_cents"]
			_, _ = w.Write([]byte(`{"estimate":{"available_quantity":1,"buyer_total_cents":1650,"unit_price_cents":1650}}`))
		case r.URL.Path == "/api/me":
			_, _ = w.Write([]byte(`{"available_balance_cents":100000,"balance_cents":100000}`))
		case r.URL.Path == "/api/workspace/extractions/batch" && r.Method == http.MethodPost:
			payload := map[string]any{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode create payload: %v", err)
			}
			createMaxUnitPrice = payload["max_unit_price_cents"]
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"manual-above-auto-ceiling","status":"processing"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "manual-nvtokens-price-ceiling.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	automaticCeiling := int64(1400)
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		Enabled: &enabled,
		Platforms: []store.ManagerSupplyPlatformConfig{{
			ID: "nv", Type: managerconfigsvc.SupplyPlatformNvtokens, Enabled: &enabled,
			BaseURL: server.URL, Token: "nv-token", Product: "plus", MaxUnitPriceFen: &automaticCeiling,
		}},
	}}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if _, err := service.ReplenishProduct(ctx, 1, "nv", "plus"); err != nil {
		t.Fatalf("create manual task: %v", err)
	}
	if err := service.RunPurchaseTasks(ctx); err != nil {
		t.Fatalf("run manual task: %v", err)
	}
	if estimateMaxUnitPrice != nil || createMaxUnitPrice != nil {
		t.Fatalf("manual price ceilings estimate=%#v create=%#v, want null/null", estimateMaxUnitPrice, createMaxUnitPrice)
	}
	order, found, err := st.GetSupplyOrder(ctx, "manual-above-auto-ceiling")
	if err != nil || !found || order.Automatic {
		t.Fatalf("manual order = %#v found=%v err=%v", order, found, err)
	}
}

func TestLiveReplenishmentSupersedesAutomaticLowPriceTask(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-low-price-superseded.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	service := New(st, nil)
	reserve, err := service.upsertAutomaticPurchaseTask(ctx, store.SupplyPurchaseTask{
		Product: "oauth_30d", TargetQuantity: 8, Status: purchaseTaskStatusPending,
		TriggerReason: lowPriceReserveTriggerReason, MaxConcurrentOrders: 1,
	})
	if err != nil {
		t.Fatalf("create low-price task: %v", err)
	}
	reserveOrder, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "reserve-waiting", TaskID: reserve.TaskID, SupplierID: "nv", Product: "plus",
		RequestedQuantity: 8, Automatic: true, Status: "waiting_inventory", TriggerReason: lowPriceReserveTriggerReason,
	})
	if err != nil {
		t.Fatalf("create low-price child order: %v", err)
	}
	live, err := service.upsertAutomaticPurchaseTask(ctx, store.SupplyPurchaseTask{
		Product: "oauth_30d", TargetQuantity: 4, Status: purchaseTaskStatusPending,
		TriggerReason: "emergency_refill_to_healthy", MaxConcurrentOrders: 2,
	})
	if err != nil {
		t.Fatalf("replace with live task: %v", err)
	}
	if live.TaskID == reserve.TaskID || live.TargetQuantity != 4 || isLowPriceReserveTrigger(live.TriggerReason) {
		t.Fatalf("replacement live task = %#v reserve=%#v", live, reserve)
	}
	reserve, found, err := st.GetSupplyPurchaseTask(ctx, reserve.TaskID)
	if err != nil || !found || reserve.Status != purchaseTaskStatusCancelled {
		t.Fatalf("superseded reserve task = %#v found=%v err=%v", reserve, found, err)
	}
	reserveOrder, found, err = st.GetSupplyOrder(ctx, reserveOrder.OrderID)
	if err != nil || !found || reserveOrder.Status != "cancelled" || reserveOrder.RemoteStatus != "task_cancelled" {
		t.Fatalf("superseded reserve order = %#v found=%v err=%v", reserveOrder, found, err)
	}
}

func TestLowPriceReserveOpenOrdersReversibleOnlyForUncommittedStage(t *testing.T) {
	reversible := store.SupplyOrder{
		OrderID: "waiting", Status: "waiting_inventory", Automatic: true,
		TriggerReason: lowPriceReserveTriggerReason,
	}
	if !lowPriceReserveOpenOrdersReversible([]store.SupplyOrder{reversible}) {
		t.Fatal("uncommitted low-price waiting order should yield to live replenishment")
	}
	reversible.ReadyQuantity = 1
	if lowPriceReserveOpenOrdersReversible([]store.SupplyOrder{reversible}) {
		t.Fatal("ready low-price order must finish admission before another purchase")
	}
}

func TestUnavailablePlatformOrdersAreTerminatedAndTasksAreReplanned(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-platform-retired.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	cfg := store.ManagerSupplyConfig{Platforms: []store.ManagerSupplyPlatformConfig{{
		ID: "nvtokens-main", Type: managerconfigsvc.SupplyPlatformNvtokens, Enabled: &enabled,
		BaseURL: "https://nvtokens.com", Username: "buyer", Password: "secret", Product: "plus",
	}}}
	service := New(st, nil)

	automatic, err := st.CreateSupplyPurchaseTask(ctx, store.SupplyPurchaseTask{
		TaskID: "automatic-retired-platform", Source: "automatic", SupplierID: "legacy", Product: "oauth_7d",
		TargetQuantity: 5, Status: purchaseTaskStatusRunning, MaxConcurrentOrders: 1,
	})
	if err != nil {
		t.Fatalf("create automatic task: %v", err)
	}
	automaticOrder, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "automatic-old-order", TaskID: automatic.TaskID, SupplierID: "legacy", Product: "oauth_7d",
		RequestedQuantity: 5, Automatic: true, Status: "waiting_inventory",
	})
	if err != nil {
		t.Fatalf("create automatic order: %v", err)
	}
	remaining, err := service.reconcileUnavailableSupplyOrders(ctx, cfg, []store.SupplyOrder{automaticOrder})
	if err != nil || len(remaining) != 0 {
		t.Fatalf("automatic remaining = %#v err=%v", remaining, err)
	}
	automaticOrder, _, _ = st.GetSupplyOrder(ctx, automaticOrder.OrderID)
	automatic, _, _ = st.GetSupplyPurchaseTask(ctx, automatic.TaskID)
	if automaticOrder.Status != "failed" || automaticOrder.RemoteStatus != "platform_unavailable" {
		t.Fatalf("retired automatic order = %#v", automaticOrder)
	}
	if automatic.Status != purchaseTaskStatusRunning || automatic.SupplierID != "" || automatic.Product != "" || automatic.NextAttemptAtMS != 0 {
		t.Fatalf("replanned automatic task = %#v", automatic)
	}

	manual, err := st.CreateSupplyPurchaseTask(ctx, store.SupplyPurchaseTask{
		TaskID: "manual-retired-platform", Source: "manual", SupplierID: "legacy", Product: "oauth_7d",
		TargetQuantity: 2, Status: purchaseTaskStatusRunning, MaxConcurrentOrders: 1,
	})
	if err != nil {
		t.Fatalf("create manual task: %v", err)
	}
	manualOrder, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "manual-old-order", TaskID: manual.TaskID, SupplierID: "legacy", Product: "oauth_7d",
		RequestedQuantity: 2, Status: "waiting_inventory",
	})
	if err != nil {
		t.Fatalf("create manual order: %v", err)
	}
	if _, err := service.reconcileUnavailableSupplyOrders(ctx, cfg, []store.SupplyOrder{manualOrder}); err != nil {
		t.Fatalf("reconcile manual order: %v", err)
	}
	manual, _, _ = st.GetSupplyPurchaseTask(ctx, manual.TaskID)
	if manual.Status != purchaseTaskStatusCancelled || manual.CancelledAtMS == 0 || manual.LastError == "" {
		t.Fatalf("cancelled manual task = %#v", manual)
	}
}

func TestMissingPickupOrderDoesNotKeepCancelledTaskSlotOpen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case "/api/customer/pickup/orders/missing-order":
			http.Error(w, `{"message":"pickup order not found"}`, http.StatusNotFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-missing-order.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		Enabled: &enabled, BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_7d",
	}}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	task, err := st.CreateSupplyPurchaseTask(ctx, store.SupplyPurchaseTask{
		TaskID: "cancelled-task-with-missing-order", Source: "automatic", SupplierID: "legacy", Product: "oauth_7d",
		TargetQuantity: 7, Status: purchaseTaskStatusCancelled, MaxConcurrentOrders: 1, CancelledAtMS: time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "missing-order", TaskID: task.TaskID, SupplierID: "legacy", Product: "oauth_7d",
		RequestedQuantity: 7, Automatic: true, Status: "waiting_inventory",
	}); err != nil {
		t.Fatalf("create order: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunPurchaseTasks(ctx); err != nil {
		t.Fatalf("run purchase tasks: %v", err)
	}
	order, found, err := st.GetSupplyOrder(ctx, "missing-order")
	if err != nil || !found {
		t.Fatalf("load order found=%v err=%v", found, err)
	}
	if order.Status != "cancelled" || order.CompletedAtMS == 0 || order.NextPollAtMS != 0 {
		t.Fatalf("missing pickup order = %#v", order)
	}
	openOrders, err := st.ListOpenSupplyOrders(ctx, 10)
	if err != nil || len(openOrders) != 0 {
		t.Fatalf("open orders = %#v err=%v", openOrders, err)
	}
}

func TestPurchaseTaskParallelSlotsCreateSmallOrdersWithinTarget(t *testing.T) {
	var createCalls atomic.Int32
	quantities := make([]int, 0, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":100,"missing":0,"estimated_total_fen":1000,"estimated_unit_price_fen":50}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":100000,"balance_fen":100000}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			var request struct {
				Quantity int `json:"quantity"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode create request: %v", err)
			}
			quantities = append(quantities, request.Quantity)
			orderID := fmt.Sprintf("parallel-small-%d", createCalls.Add(1))
			_, _ = fmt.Fprintf(w, `{"order":{"id":%q,"status":"waiting_inventory","quantity":%d,"retry_after_seconds":60}}`, orderID, request.Quantity)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-parallel-small.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		Enabled: &enabled, MaxConcurrentOrders: 3,
		Platforms: []store.ManagerSupplyPlatformConfig{{
			ID: "legacy", Type: managerconfigsvc.SupplyPlatformLegacy, Enabled: &enabled,
			BaseURL: server.URL, Token: "supplier-token", Product: "oauth_7d",
		}},
	}}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyPurchaseTask(ctx, store.SupplyPurchaseTask{
		TaskID: "purchase-parallel-small", Source: "automatic", Product: "oauth_7d",
		TargetQuantity: 20, Status: "pending", MaxConcurrentOrders: 3,
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	for index := 0; index < 4; index++ {
		if err := service.RunPurchaseTasks(ctx); err != nil {
			t.Fatalf("run purchase tasks %d: %v", index+1, err)
		}
	}
	if got := fmt.Sprint(quantities); got != "[7 7 6]" {
		t.Fatalf("parallel child quantities = %s, want [7 7 6]", got)
	}
	orders, err := st.ListSupplyOrdersByTaskID(ctx, "purchase-parallel-small")
	if err != nil || len(orders) != 3 {
		t.Fatalf("parallel child orders = %#v err=%v", orders, err)
	}
	total := 0
	for _, order := range orders {
		total += order.RequestedQuantity
	}
	if total != 20 {
		t.Fatalf("parallel reserved quantity = %d, want exact target 20", total)
	}
}

func TestPurchaseTaskPollsOldWaitingOrderAndUsesRemainingSlotsSameTurn(t *testing.T) {
	var createCalls atomic.Int32
	var createdQuantity atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/pickup/orders/old-final-one" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"id":"old-final-one","status":"waiting_inventory","quantity":1,"retry_after_seconds":60}`))
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":0,"missing":100,"needs_production":true,"estimated_total_fen":4800,"estimated_unit_price_fen":300}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":100000,"balance_fen":100000}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			var request struct {
				Quantity int `json:"quantity"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode create request: %v", err)
			}
			createCalls.Add(1)
			createdQuantity.Store(int32(request.Quantity))
			_, _ = fmt.Fprintf(w, `{"order":{"id":"new-emergency-shard","status":"waiting_inventory","quantity":%d,"retry_after_seconds":60}}`, request.Quantity)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-old-slot.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	smartDisabled := false
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		Enabled: &enabled, SmartEnabled: &smartDisabled, MaxConcurrentOrders: 3,
		Platforms: []store.ManagerSupplyPlatformConfig{{
			ID: "legacy", Type: managerconfigsvc.SupplyPlatformLegacy, Enabled: &enabled,
			BaseURL: server.URL, Token: "supplier-token", Product: "oauth_7d",
		}},
	}}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	task, err := st.CreateSupplyPurchaseTask(ctx, store.SupplyPurchaseTask{
		TaskID: "purchase-expanded-emergency", Source: "automatic", Product: "oauth_7d",
		TargetQuantity: 51, Status: purchaseTaskStatusRunning, MaxConcurrentOrders: 3,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "completed-prefix", TaskID: task.TaskID, Product: "oauth_7d",
		RequestedQuantity: 19, Automatic: true, Status: "completed", ItemCount: 19, ImportedCount: 19,
	}); err != nil {
		t.Fatalf("create completed prefix: %v", err)
	}
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "old-final-one", TaskID: task.TaskID, SupplierID: "legacy", Product: "oauth_7d",
		RequestedQuantity: 1, Automatic: true, Status: "waiting_inventory",
		StatusURL:     "/api/customer/pickup/orders/old-final-one",
		TriggerReason: "parallel_available_capacity_critical",
	}); err != nil {
		t.Fatalf("create old waiting order: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunPurchaseTasks(ctx); err != nil {
		t.Fatalf("run expanded task: %v", err)
	}
	if createCalls.Load() != 1 || createdQuantity.Load() != 16 {
		t.Fatalf("new emergency shard calls/quantity = %d/%d, want 1/16", createCalls.Load(), createdQuantity.Load())
	}
	orders, err := st.ListOpenSupplyOrders(ctx, 10)
	if err != nil || len(orders) != 2 {
		t.Fatalf("open orders after same-turn admission = %#v err=%v", orders, err)
	}
}

func TestPurchaseTaskReplacesStaleWaitingReservationOnAnotherPlatform(t *testing.T) {
	var createPlatform atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		platform := r.Header.Get("X-Customer-Token")
		switch {
		case r.URL.Path == "/api/customer/pickup/orders/stale-order" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"id":"stale-order","status":"waiting_inventory","quantity":1}`))
		case r.URL.Path == "/api/customer/inventory":
			if platform == "bugteam" {
				_, _ = w.Write([]byte(`{"available":10,"missing":0,"estimated_total_fen":100,"estimated_unit_price_fen":10}`))
			} else {
				_, _ = w.Write([]byte(`{"available":0,"missing":1,"needs_production":true,"estimated_total_fen":100,"estimated_unit_price_fen":10}`))
			}
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":10000,"balance_fen":10000}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			createPlatform.Store(platform)
			_, _ = w.Write([]byte(`{"order":{"id":"replacement-order","status":"waiting_inventory","quantity":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-stale-failover.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		Enabled: &enabled, MaxConcurrentOrders: 3,
		Platforms: []store.ManagerSupplyPlatformConfig{
			{ID: "legacy", Type: managerconfigsvc.SupplyPlatformLegacy, Enabled: &enabled, BaseURL: server.URL, Token: "legacy", Product: "oauth_7d"},
			{ID: "bugteam", Type: managerconfigsvc.SupplyPlatformBugTeam, Enabled: &enabled, BaseURL: server.URL, Token: "bugteam", Product: "team_1h"},
		},
	}}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	task, err := st.CreateSupplyPurchaseTask(ctx, store.SupplyPurchaseTask{
		TaskID: "purchase-stale-failover", Source: "automatic", Product: "team_1h",
		TargetQuantity: 2, Status: purchaseTaskStatusRunning, MaxConcurrentOrders: 3,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "completed-prefix", TaskID: task.TaskID, SupplierID: "bugteam", Product: "team_1h",
		RequestedQuantity: 1, Automatic: true, Status: "completed", ItemCount: 1, ImportedCount: 1,
	}); err != nil {
		t.Fatalf("create completed prefix: %v", err)
	}
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "stale-order", TaskID: task.TaskID, SupplierID: "legacy", Product: "oauth_7d",
		RequestedQuantity: 1, Automatic: true, Status: "waiting_inventory",
		StatusURL:   "/api/customer/pickup/orders/stale-order",
		CreatedAtMS: time.Now().Add(-purchaseTaskStaleAutomaticWaitingAge - time.Second).UnixMilli(),
	}); err != nil {
		t.Fatalf("create stale order: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunPurchaseTasks(ctx); err != nil {
		t.Fatalf("run purchase task: %v", err)
	}
	if got, _ := createPlatform.Load().(string); got != "bugteam" {
		t.Fatalf("replacement platform = %q, want bugteam", got)
	}
	stale, found, err := st.GetSupplyOrder(ctx, "stale-order")
	if err != nil || !found || stale.Status != "released" || stale.CompletedAtMS == 0 {
		t.Fatalf("stale order after poll = %#v found=%v err=%v", stale, found, err)
	}
	orders, err := st.ListSupplyOrdersByTaskID(ctx, task.TaskID)
	if err != nil || len(orders) != 3 {
		t.Fatalf("task orders = %#v err=%v", orders, err)
	}
}

func TestPurchaseTaskRetriesCreateFailureUntilTargetIsFulfilled(t *testing.T) {
	var createCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":100,"missing":0,"estimated_total_fen":1000,"estimated_unit_price_fen":100}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":100000,"balance_fen":100000}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			if createCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"inventory reservation cancelled"}`))
				return
			}
			_, _ = w.Write([]byte(`{"order":{"id":"retry-order","status":"waiting_inventory","quantity":10,"retry_after_seconds":60}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-retry.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		Enabled: &enabled, MaxConcurrentOrders: 3,
		Platforms: []store.ManagerSupplyPlatformConfig{{
			ID: "legacy", Type: managerconfigsvc.SupplyPlatformLegacy, Enabled: &enabled,
			BaseURL: server.URL, Token: "supplier-token", Product: "oauth_7d",
		}},
	}}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	task, err := st.CreateSupplyPurchaseTask(ctx, store.SupplyPurchaseTask{
		TaskID: "purchase-retry", Source: "manual", SupplierID: "legacy",
		Product: "oauth_7d", TargetQuantity: 10, Status: "pending", MaxConcurrentOrders: 1,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunPurchaseTasks(ctx); err == nil {
		t.Fatal("first task attempt succeeded despite supplier conflict")
	}
	task, _, err = st.GetSupplyPurchaseTask(ctx, task.TaskID)
	if err != nil {
		t.Fatalf("load failed task: %v", err)
	}
	if task.AttemptCount != 1 || task.Status != "running" || task.LastError == "" {
		t.Fatalf("failed task state = %#v", task)
	}
	task.NextAttemptAtMS = 0
	if err := st.UpdateSupplyPurchaseTask(ctx, task); err != nil {
		t.Fatalf("make retry due: %v", err)
	}
	if err := service.RunPurchaseTasks(ctx); err != nil {
		t.Fatalf("second task attempt: %v", err)
	}
	if createCalls.Load() != 2 {
		t.Fatalf("create calls = %d, want 2", createCalls.Load())
	}
	order, found, err := st.GetSupplyOrder(ctx, "retry-order")
	if err != nil || !found || order.TaskID != task.TaskID {
		t.Fatalf("retry order = %#v found=%v err=%v", order, found, err)
	}
	order.Status = "completed"
	order.ItemCount = 10
	order.ImportedCount = 10
	if err := st.UpdateSupplyOrder(ctx, order); err != nil {
		t.Fatalf("settle retry order: %v", err)
	}
	if err := service.RunPurchaseTasks(ctx); err != nil {
		t.Fatalf("complete task: %v", err)
	}
	task, _, err = st.GetSupplyPurchaseTask(ctx, task.TaskID)
	if err != nil || task.Status != "completed" || task.FulfilledQuantity != 10 {
		t.Fatalf("completed task = %#v err=%v", task, err)
	}
}

func TestPaidUndeliverableTakenAccountBlocksPurchaseTaskRetry(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-invalid-delivery.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	task, err := st.CreateSupplyPurchaseTask(ctx, store.SupplyPurchaseTask{
		TaskID: "purchase-invalid-delivery", Source: "automatic", Product: "plus",
		TargetQuantity: 1, Status: purchaseTaskStatusRunning, MaxConcurrentOrders: 1,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	order, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "invalid-delivery", TaskID: task.TaskID, SupplierID: "nv", Product: "plus",
		RequestedQuantity: 1, ReadyQuantity: 1, Automatic: true, Status: "taking", RemoteStatus: "completed",
	})
	if err != nil {
		t.Fatalf("create taking order: %v", err)
	}
	service := New(st, nil)
	deliveryErr := fmt.Errorf("supply account 1 format is unsupported: account is not an OpenAI OAuth credential")
	if err := service.failUndeliverableOrder(ctx, &order, deliveryErr); err == nil || err.Error() != deliveryErr.Error() {
		t.Fatalf("invalid delivery error = %v", err)
	}

	stored, found, err := st.GetSupplyOrder(ctx, order.OrderID)
	if err != nil || !found {
		t.Fatalf("load failed delivery found=%v err=%v", found, err)
	}
	if stored.Status != "partial" || stored.RemoteStatus != "paid_delivery_unparsed" || stored.CompletedAtMS != 0 ||
		stored.NextPollAtMS <= time.Now().UnixMilli() || stored.LastError != deliveryErr.Error() {
		t.Fatalf("held paid delivery = %#v", stored)
	}
	openOrders, err := st.ListOpenSupplyOrders(ctx, 10)
	if err != nil || len(openOrders) != 1 || openOrders[0].OrderID != order.OrderID {
		t.Fatalf("paid delivery must occupy the order slot: orders=%#v err=%v", openOrders, err)
	}
	reconciled, err := service.reconcilePurchaseTask(ctx, task)
	if err != nil {
		t.Fatalf("reconcile task: %v", err)
	}
	if reconciled.Status != purchaseTaskStatusRunning || reconciled.FulfilledQuantity != 0 || reconciled.ActiveOrderCount != 1 {
		t.Fatalf("task after invalid delivery = %#v", reconciled)
	}
}

func TestDefiniteUnpaidUndeliverableAccountReleasesPurchaseTaskSlot(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-invalid-unpaid-delivery.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	order, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "invalid-unpaid-delivery", SupplierID: "nv", Product: "plus",
		RequestedQuantity: 1, Automatic: true, Status: "taking",
	})
	if err != nil {
		t.Fatalf("create taking order: %v", err)
	}
	service := New(st, nil)
	deliveryErr := errors.New("empty supplier payload")
	if err := service.failUndeliverableOrder(ctx, &order, deliveryErr); err == nil {
		t.Fatal("unpaid invalid delivery should return its parsing error")
	}
	stored, _, err := st.GetSupplyOrder(ctx, order.OrderID)
	if err != nil || stored.Status != "failed" || stored.RemoteStatus != "invalid_payload" || stored.CompletedAtMS <= 0 {
		t.Fatalf("unpaid invalid delivery = %#v err=%v", stored, err)
	}
}

func TestPurchaseTaskAdmissionStopsAfterPaidDeliveryNeedsReview(t *testing.T) {
	orders := []store.SupplyOrder{{
		OrderID: "paid-review", Status: "failed", RemoteStatus: "completed",
		RequestedQuantity: 1, ReadyQuantity: 1, Progress: 100,
	}}
	if !purchaseTaskOrdersRequireOperatorReview(orders) {
		t.Fatal("paid delivery evidence must stop another purchase attempt")
	}
	orders[0] = store.SupplyOrder{OrderID: "free-failure", Status: "failed", RemoteStatus: "failed"}
	if purchaseTaskOrdersRequireOperatorReview(orders) {
		t.Fatal("definite unpaid failure should remain retryable")
	}
}

func TestPurchaseTaskSplitsLargeTargetUntilEveryAccountIsImported(t *testing.T) {
	var createCalls atomic.Int32
	quantities := make([]int, 0, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":1000,"missing":0,"estimated_total_fen":1000,"estimated_unit_price_fen":10}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":100000,"balance_fen":100000}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			var request struct {
				Quantity int `json:"quantity"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode create request: %v", err)
			}
			quantities = append(quantities, request.Quantity)
			orderID := fmt.Sprintf("large-target-%d", createCalls.Add(1))
			_, _ = fmt.Fprintf(w, `{"order":{"id":%q,"status":"waiting_inventory","quantity":%d}}`, orderID, request.Quantity)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-large.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		Enabled: &enabled, MaxConcurrentOrders: 1,
		Platforms: []store.ManagerSupplyPlatformConfig{{
			ID: "legacy", Type: managerconfigsvc.SupplyPlatformLegacy, Enabled: &enabled,
			BaseURL: server.URL, Token: "supplier-token", Product: "oauth_7d",
		}},
	}}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	task, err := service.createManualPurchaseTask(ctx, 250, "legacy")
	if err != nil {
		t.Fatalf("create large task: %v", err)
	}

	for index, imported := range []int{100, 100, 50} {
		if err := service.RunPurchaseTasks(ctx); err != nil {
			t.Fatalf("create child order %d: %v", index+1, err)
		}
		orders, listErr := st.ListSupplyOrdersByTaskID(ctx, task.TaskID)
		if listErr != nil || len(orders) != index+1 {
			t.Fatalf("child orders after step %d = %#v err=%v", index+1, orders, listErr)
		}
		order := orders[len(orders)-1]
		order.Status = "completed"
		order.ItemCount = imported
		order.ImportedCount = imported
		if err := st.UpdateSupplyOrder(ctx, order); err != nil {
			t.Fatalf("complete child order %d: %v", index+1, err)
		}
	}
	if err := service.RunPurchaseTasks(ctx); err != nil {
		t.Fatalf("complete large task: %v", err)
	}
	task, _, err = st.GetSupplyPurchaseTask(ctx, task.TaskID)
	if err != nil || task.Status != purchaseTaskStatusCompleted || task.FulfilledQuantity != 250 {
		t.Fatalf("large task = %#v err=%v", task, err)
	}
	if fmt.Sprint(quantities) != "[100 100 50]" {
		t.Fatalf("child quantities = %v, want [100 100 50]", quantities)
	}
}

func TestCancelledPurchaseTaskReleasesReversibleChildOrder(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-cancel.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	task, err := st.CreateSupplyPurchaseTask(ctx, store.SupplyPurchaseTask{
		TaskID: "purchase-cancel", Source: "manual", Product: "oauth_7d",
		TargetQuantity: 10, Status: "running", MaxConcurrentOrders: 1,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "cancel-child", TaskID: task.TaskID, SupplierID: "legacy", Product: "oauth_7d",
		RequestedQuantity: 10, Status: "waiting_inventory",
	}); err != nil {
		t.Fatalf("create child order: %v", err)
	}
	if _, changed, err := st.CancelSupplyPurchaseTask(ctx, task.TaskID, 1234); err != nil || !changed {
		t.Fatalf("cancel task changed=%v err=%v", changed, err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil))
	order, _, err := st.GetSupplyOrder(ctx, "cancel-child")
	if err != nil {
		t.Fatalf("load child order: %v", err)
	}
	stopped, err := service.stopPurchaseTaskOrderIfNeeded(ctx, &order)
	if err != nil || !stopped {
		t.Fatalf("stop child order stopped=%v err=%v", stopped, err)
	}
	order, _, err = st.GetSupplyOrder(ctx, "cancel-child")
	if err != nil || order.Status != "cancelled" || order.RemoteStatus != "task_cancelled" {
		t.Fatalf("cancelled child order = %#v err=%v", order, err)
	}
}

func TestCancelledAutomaticPurchaseTaskCancelsReversibleChildOrder(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-auto-cancel.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	task, err := st.CreateSupplyPurchaseTask(ctx, store.SupplyPurchaseTask{
		TaskID: "purchase-auto-cancel", Source: "automatic", Product: "oauth_7d",
		TargetQuantity: 10, Status: "running", MaxConcurrentOrders: 3,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "auto-cancel-child", TaskID: task.TaskID, SupplierID: "legacy", Product: "oauth_7d",
		RequestedQuantity: 10, Automatic: true, Status: "waiting_inventory",
	}); err != nil {
		t.Fatalf("create child order: %v", err)
	}
	if _, changed, err := st.CancelSupplyPurchaseTask(ctx, task.TaskID, 1234); err != nil || !changed {
		t.Fatalf("cancel task changed=%v err=%v", changed, err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil))
	order, _, err := st.GetSupplyOrder(ctx, "auto-cancel-child")
	if err != nil {
		t.Fatalf("load child order: %v", err)
	}
	stopped, err := service.stopPurchaseTaskOrderIfNeeded(ctx, &order)
	if err != nil || !stopped {
		t.Fatalf("automatic child cancellation stopped=%v err=%v", stopped, err)
	}
	order, _, err = st.GetSupplyOrder(ctx, "auto-cancel-child")
	if err != nil || order.Status != "cancelled" || order.RemoteStatus != "task_cancelled" {
		t.Fatalf("automatic child order = %#v err=%v", order, err)
	}
}

func TestCancelPurchaseTaskImmediatelyCancelsAllReversibleChildren(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-eager-cancel.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	task, err := st.CreateSupplyPurchaseTask(ctx, store.SupplyPurchaseTask{
		TaskID: "purchase-eager-cancel", Source: "automatic", Product: "team_1h",
		TargetQuantity: 3, Status: purchaseTaskStatusRunning, MaxConcurrentOrders: 3,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	for index, status := range []string{"creating", "waiting_inventory", "ready"} {
		if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
			OrderID: fmt.Sprintf("cancel-child-%d", index+1), TaskID: task.TaskID,
			SupplierID: "bugteam", Product: "team_1h", RequestedQuantity: 1,
			Automatic: true, Status: status,
		}); err != nil {
			t.Fatalf("create child %d: %v", index+1, err)
		}
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil))
	if _, found, err := service.cancelPurchaseTaskAndChildren(ctx, task.TaskID, time.Now().UnixMilli()); err != nil || !found {
		t.Fatalf("cancel task found=%v err=%v", found, err)
	}
	orders, err := st.ListSupplyOrdersByTaskID(ctx, task.TaskID)
	if err != nil || len(orders) != 3 {
		t.Fatalf("child orders = %#v err=%v", orders, err)
	}
	for _, order := range orders {
		if order.Status != "cancelled" || order.RemoteStatus != "task_cancelled" || order.CompletedAtMS == 0 {
			t.Fatalf("child order not cancelled = %#v", order)
		}
	}
}

func TestCancelPurchaseTaskPreservesCommittedChildImport(t *testing.T) {
	order := store.SupplyOrder{
		Status: "importing", RequestedQuantity: 2, ItemCount: 2, ImportedCount: 1, ChargedFen: 300,
	}
	if cancelReversiblePurchaseTaskOrder(&order, time.Now().UnixMilli()) {
		t.Fatalf("committed child must continue importing: %#v", order)
	}
}

func TestAutomaticPurchaseTaskKeepsActiveOrderUntilTakeDecision(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "purchase-task-active-order.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	cfg := store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		Enabled: &enabled, SmartEnabled: &enabled, Product: "oauth_7d",
	}}
	if err := st.SaveManagerConfig(ctx, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	task, err := st.CreateSupplyPurchaseTask(ctx, store.SupplyPurchaseTask{
		TaskID: "purchase-active", Source: "automatic", Product: "oauth_7d",
		TargetQuantity: 26, Status: "running", MaxConcurrentOrders: 1,
		CreatedAtMS: time.Now().Add(-time.Minute).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "ready-26", TaskID: task.TaskID, Product: "oauth_7d",
		RequestedQuantity: 26, ReadyQuantity: 26, Automatic: true, Status: "ready",
	}); err != nil {
		t.Fatalf("create order: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil))
	service.setSmartResource(SmartResource{
		GeneratedAtMS: time.Now().UnixMilli(), Enabled: true, SnapshotFresh: true,
		HealthLevel: smartHealthHealthy, DecisionReason: "capacity_healthy",
	})
	if err := service.reconcileAutomaticPurchaseTaskCancellation(ctx); err != nil {
		t.Fatalf("reconcile automatic task: %v", err)
	}
	task, _, err = st.GetSupplyPurchaseTask(ctx, task.TaskID)
	if err != nil {
		t.Fatalf("load task: %v", err)
	}
	if task.Status != purchaseTaskStatusRunning {
		t.Fatalf("active task status = %q, want running", task.Status)
	}
}
