package supply

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

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
	if second.LastResult != "active_task" || second.ActiveTaskID != first.ActiveTaskID {
		t.Fatalf("second execution = %#v", second)
	}
	task, found, err := st.GetSupplyPurchaseTask(ctx, first.ActiveTaskID)
	if err != nil || !found || task.TargetQuantity != 2 {
		t.Fatalf("durable stage = %#v found=%v err=%v", task, found, err)
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
