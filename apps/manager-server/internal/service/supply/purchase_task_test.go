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
	if err != nil || order.Status != "released" || order.RemoteStatus != "task_cancelled" {
		t.Fatalf("released child order = %#v err=%v", order, err)
	}
}

func TestCancelledAutomaticPurchaseTaskDefersChildOrderToLiveTakeDecision(t *testing.T) {
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
	if err != nil || stopped {
		t.Fatalf("automatic child must reach live take decision: stopped=%v err=%v", stopped, err)
	}
	order, _, err = st.GetSupplyOrder(ctx, "auto-cancel-child")
	if err != nil || order.Status != "waiting_inventory" {
		t.Fatalf("automatic child order = %#v err=%v", order, err)
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
