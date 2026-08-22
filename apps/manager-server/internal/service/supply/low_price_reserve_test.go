package supply

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/config"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpaauthfiles"
	managerconfigsvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

func TestLowPriceReserveLadderTargetThirty(t *testing.T) {
	if got := fmt.Sprint(lowPriceReserveLadder(30)); got != "[15 5 3 2 2 1 1 1]" {
		t.Fatalf("ladder = %s", got)
	}
}

func TestLowPriceReserveQuoteSnapshotUsesCostMultiplierWithPriceFallback(t *testing.T) {
	withCapacity := supplyPlatformTestOverview("high-price-high-capacity", 10, 1800, 10_000, 60, 170, 0)
	withCapacity.CostMultiplier, _ = supplierCostMultiplier(1800, 170)
	lowPriceUnknown := supplyPlatformTestOverview("low-price-unknown", 10, 1700, 10_000, 60, 0, 0)

	price, multiplier, platformID, ok := lowPriceReserveQuoteSnapshot([]PlatformOverview{withCapacity, lowPriceUnknown})
	if !ok || platformID != lowPriceUnknown.ID || price != 1700 || multiplier != 0 {
		t.Fatalf("unknown fallback snapshot = price=%d multiplier=%.6f platform=%q ok=%v", price, multiplier, platformID, ok)
	}

	lowPriceUnknown.Inventory.EstimatedUnitPriceFen = 1900
	price, multiplier, platformID, ok = lowPriceReserveQuoteSnapshot([]PlatformOverview{withCapacity, lowPriceUnknown})
	if !ok || platformID != withCapacity.ID || price != 1800 || multiplier != 0.105882 {
		t.Fatalf("capacity snapshot = price=%d multiplier=%.6f platform=%q ok=%v", price, multiplier, platformID, ok)
	}
}

func TestLowPriceReserveNextStageUsesCumulativeThreshold(t *testing.T) {
	for _, test := range []struct {
		reserve int
		want    int
	}{
		{reserve: 0, want: 15},
		{reserve: 18, want: 2},
		{reserve: 20, want: 3},
		{reserve: 27, want: 1},
		{reserve: 30, want: 0},
	} {
		if got := lowPriceReserveNextStageQuantity(30, test.reserve); got != test.want {
			t.Fatalf("reserve=%d next=%d, want %d", test.reserve, got, test.want)
		}
	}
}

func TestCountLowPriceReserveFilesUsesOnlyAvailableMarkedAccounts(t *testing.T) {
	reserveMarker := map[string]any{"method": lowPriceReserveTriggerReason}
	files := []cpaauthfiles.File{
		{Name: "reserve-ok.json", Provider: "codex", Raw: map[string]any{"status": "active", "cpamp_import": reserveMarker}},
		{Name: "reserve-disabled.json", Provider: "codex", Disabled: true, Raw: map[string]any{"status": "active", "cpamp_import": reserveMarker}},
		{Name: "reserve-invalid.json", Provider: "codex", Raw: map[string]any{"status": "invalid", "cpamp_import": reserveMarker}},
		{Name: "reserve-exhausted.json", Provider: "codex", Raw: map[string]any{"status": "active", "status_message": "quota_exhausted", "cpamp_import": reserveMarker}},
		{Name: "normal.json", Provider: "codex", Raw: map[string]any{"status": "active", "cpamp_import": map[string]any{"method": "automatic_supply"}}},
		{Name: "other.json", Provider: "xai", Raw: map[string]any{"status": "active", "cpamp_import": reserveMarker}},
	}
	if got := countLowPriceReserveFiles(files); got != 1 {
		t.Fatalf("reserve count = %d, want 1", got)
	}
}

func TestRunLowPriceReserveCreatesOneBoundedLadderTask(t *testing.T) {
	var quoteCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files":
			files := make([]map[string]any, 0, 18)
			for index := 0; index < 18; index++ {
				files = append(files, map[string]any{
					"name": fmt.Sprintf("reserve-%02d.json", index), "provider": "codex", "status": "active",
					"cpamp_import": map[string]any{"method": lowPriceReserveTriggerReason},
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
		case "/api/workspace/seller-candidates":
			quoteCalls++
			_, _ = w.Write([]byte(`{"sellers":[{"sale_plans":["plus"],"sale_plan_counts":{"plus":20},"sale_plan_prices":{"plus":{"min_cents":50,"max_cents":50}}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "low-price-watcher.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	ceiling := int64(1300)
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, LowPriceReserveEnabled: &enabled, LowPriceReserveMaxUnitPriceFen: &ceiling,
			LowPriceReserveTargetAccounts: 30, LowPriceReserveCheckIntervalMilliseconds: 1000,
			Platforms: []store.ManagerSupplyPlatformConfig{{
				ID: "nv", Type: managerconfigsvc.SupplyPlatformNvtokens, Enabled: &enabled,
				BaseURL: server.URL, Token: "nv-token", Product: "plus",
			}},
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	first, err := service.RunLowPriceReserve(ctx)
	if err != nil {
		t.Fatalf("first watcher run: %v", err)
	}
	if first.ReserveAccounts != 18 || first.NextStageQuantity != 2 || first.LastResult != "task_created" || first.ActiveTaskID == "" {
		t.Fatalf("first execution = %#v", first)
	}
	second, err := service.RunLowPriceReserve(ctx)
	if err != nil {
		t.Fatalf("second watcher run: %v", err)
	}
	if second.LastResult != "active_task" || second.ActiveTaskID != first.ActiveTaskID || second.LastQuotedUnitPriceFen != 50 {
		t.Fatalf("second execution = %#v", second)
	}
	if quoteCalls < 2 {
		t.Fatalf("active task did not refresh the live quote: calls=%d", quoteCalls)
	}
	task, found, err := st.GetSupplyPurchaseTask(ctx, first.ActiveTaskID)
	if err != nil || !found || task.TargetQuantity != 2 {
		t.Fatalf("durable stage = %#v found=%v err=%v", task, found, err)
	}
}

func TestExactQuoteMissBacksOffWithoutRecreatingTasks(t *testing.T) {
	var catalogCalls atomic.Int32
	var estimateCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files":
			_, _ = w.Write([]byte(`{"files":[]}`))
		case "/api/workspace/seller-candidates":
			catalogCalls.Add(1)
			_, _ = w.Write([]byte(`{"sellers":[{"seller_token":"seller-a","selection_token":"select-a","sale_plans":["plus"],"sale_plan_counts":{"plus":20},"sale_plan_prices":{"plus":{"min_cents":1200,"max_cents":1200}}}]}`))
		case "/api/workspace/extractions/estimate":
			estimateCalls.Add(1)
			_, _ = w.Write([]byte(`{"estimate":{"available_quantity":20,"buyer_total_cents":30000,"min_unit_price_cents":1500,"max_unit_price_cents":1500}}`))
		case "/api/me":
			_, _ = w.Write([]byte(`{"available_balance_cents":100000,"balance_cents":100000}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "low-price-exact-backoff.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	ceiling := int64(1300)
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, LowPriceReserveEnabled: &enabled, LowPriceReserveMaxUnitPriceFen: &ceiling,
			LowPriceReserveTargetAccounts: 30, LowPriceReserveCheckIntervalMilliseconds: 1000,
			Platforms: []store.ManagerSupplyPlatformConfig{{
				ID: "nv", Type: managerconfigsvc.SupplyPlatformNvtokens, Enabled: &enabled,
				BaseURL: server.URL, Token: "nv-token", Product: "plus",
			}},
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	created, err := service.RunLowPriceReserve(ctx)
	if err != nil || created.LastResult != "task_created" || created.ActiveTaskID == "" {
		t.Fatalf("created execution = %#v err=%v", created, err)
	}
	if err := service.RunPurchaseTasks(ctx); err != nil {
		t.Fatalf("run purchase task: %v", err)
	}
	task, found, err := st.GetSupplyPurchaseTask(ctx, created.ActiveTaskID)
	if err != nil || !found || task.Status != purchaseTaskStatusCancelled || task.LastError != lowPriceReserveInventoryUnavailable {
		t.Fatalf("cancelled task = %#v found=%v err=%v", task, found, err)
	}
	if catalogCalls.Load() != 1 || estimateCalls.Load() == 0 {
		t.Fatalf("catalog=%d estimate=%d", catalogCalls.Load(), estimateCalls.Load())
	}

	for index := 0; index < 3; index++ {
		execution, runErr := service.RunLowPriceReserve(ctx)
		if runErr != nil || execution.LastResult != lowPriceReserveExactQuoteBackoff || execution.RetryAfterMS <= time.Now().UnixMilli() {
			t.Fatalf("backoff execution %d = %#v err=%v", index, execution, runErr)
		}
	}
	if catalogCalls.Load() != 1 {
		t.Fatalf("catalog was called during exact quote backoff: %d", catalogCalls.Load())
	}
	if interval := service.NextLowPriceReserveInterval(ctx); interval < 25*time.Second {
		t.Fatalf("backoff interval = %s, want close to 30s", interval)
	}

	service.stateMu.Lock()
	service.lowPriceReserve.RetryAfterMS = time.Now().Add(-time.Millisecond).UnixMilli()
	service.stateMu.Unlock()
	retried, err := service.RunLowPriceReserve(ctx)
	if err != nil || retried.LastResult != "task_created" || retried.ActiveTaskID == "" {
		t.Fatalf("retry after expiry = %#v err=%v", retried, err)
	}
	if catalogCalls.Load() != 2 {
		t.Fatalf("catalog calls after expiry = %d, want 2", catalogCalls.Load())
	}
}

func TestLowPriceReserveBoundsUnknownSellerTrialTaskToOne(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files":
			_, _ = w.Write([]byte(`{"files":[]}`))
		case "/api/workspace/seller-candidates":
			_, _ = w.Write([]byte(`{"sellers":[{"seller_token":"seller-new","selection_token":"select-new","sale_plans":["plus"],"sale_plan_counts":{"plus":20},"sale_plan_prices":{"plus":{"min_cents":1200,"max_cents":1200}}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "low-price-trial-task.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	ceiling := int64(1300)
	platformCeiling := int64(1400)
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, LowPriceReserveEnabled: &enabled, LowPriceReserveMaxUnitPriceFen: &ceiling,
			LowPriceReserveTargetAccounts: 30, LowPriceReserveCheckIntervalMilliseconds: 1000,
			Platforms: []store.ManagerSupplyPlatformConfig{{
				ID: "nv", Type: managerconfigsvc.SupplyPlatformNvtokens, Enabled: &enabled,
				BaseURL: server.URL, Token: "nv-token", Product: "plus", MaxUnitPriceFen: &platformCeiling,
				SupplierQuotaGateEnabled: &enabled, SupplierQuotaMinimumM: 90, SupplierQuotaTrialQuantity: 1,
			}},
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	execution, err := service.RunLowPriceReserve(ctx)
	if err != nil || execution.LastResult != "task_created" {
		t.Fatalf("execution = %#v err=%v", execution, err)
	}
	task, found, err := st.GetSupplyPurchaseTask(ctx, execution.ActiveTaskID)
	if err != nil || !found || task.TargetQuantity != 1 {
		t.Fatalf("trial task = %#v found=%v err=%v", task, found, err)
	}
}

func TestCurrentLowPriceReserveExecutionPublishesConfiguredRuntime(t *testing.T) {
	enabled := true
	ceiling := int64(1300)
	service := &Service{lowPriceReserve: LowPriceReserveExecution{
		ReserveAccounts: 20, LastCheckedAtMS: 10, NextCheckAtMS: 20,
		LastQuotedUnitPriceFen: 1200, SelectedPlatformID: "nv",
	}}
	status := service.currentLowPriceReserveExecution(store.ManagerSupplyConfig{
		Enabled: &enabled, LowPriceReserveEnabled: &enabled, LowPriceReserveMaxUnitPriceFen: &ceiling,
		LowPriceReserveTargetAccounts: 30, LowPriceReserveCheckIntervalMilliseconds: 750,
	}, []store.SupplyPurchaseTask{{
		TaskID: "reserve-task", Source: "automatic", Status: purchaseTaskStatusRunning,
		TriggerReason: lowPriceReserveTriggerReason,
	}})
	if !status.Enabled || status.ReserveAccounts != 20 || status.Gap != 10 || status.NextStageQuantity != 3 ||
		status.CheckIntervalMilliseconds != 750 || status.MaxUnitPriceFen != 1300 ||
		status.ActiveTaskID != "reserve-task" || fmt.Sprint(status.Ladder) != "[15 5 3 2 2 1 1 1]" {
		t.Fatalf("runtime status = %#v", status)
	}
}
