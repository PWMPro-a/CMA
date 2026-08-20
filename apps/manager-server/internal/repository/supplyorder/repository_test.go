package supplyorder_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/testutil"
)

func TestRecoveryImportDoesNotBlockPurchaseOrder(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t, testutil.NewConfig(t))

	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID:           "recovery-12362",
		Product:           "oauth_7d",
		RequestedQuantity: 1,
		Automatic:         true,
		Strategy:          "recovery",
		Status:            "importing",
		RemoteStatus:      "recovery_claimed",
	}); err != nil {
		t.Fatalf("create recovery import: %v", err)
	}
	if order, found, err := st.GetOpenSupplyOrder(ctx); err != nil || found {
		t.Fatalf("recovery import returned as open purchase: order=%#v found=%v err=%v", order, found, err)
	}

	purchase, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID:           "56812",
		Product:           "oauth_7d",
		RequestedQuantity: 3,
		Automatic:         true,
		Strategy:          "strong_supply",
		Status:            "waiting_inventory",
	})
	if err != nil {
		t.Fatalf("create purchase while recovery import is active: %v", err)
	}
	if order, found, err := st.GetOpenSupplyOrder(ctx); err != nil || !found || order.OrderID != purchase.OrderID {
		t.Fatalf("open purchase = %#v found=%v err=%v", order, found, err)
	}

	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "56813", Product: "oauth_7d", RequestedQuantity: 1, Automatic: true, Status: "created",
	}); err == nil || !strings.Contains(err.Error(), "open order already exists") {
		t.Fatalf("second purchase error = %v", err)
	}

	parallel, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "56814", Product: "oauth_7d", RequestedQuantity: 2, Automatic: true,
		TriggerReason: "parallel_emergency_capacity_shortage", Status: "waiting_inventory",
	})
	if err != nil {
		t.Fatalf("parallel purchase should be accepted: %v", err)
	}
	open, err := st.ListOpenSupplyOrders(ctx, 10)
	if err != nil || len(open) != 2 || open[0].OrderID != purchase.OrderID || open[1].OrderID != parallel.OrderID {
		t.Fatalf("parallel open purchases = %#v err=%v", open, err)
	}

	manualParallel, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "56815", Product: "oauth_7d", RequestedQuantity: 4,
		TriggerReason: "parallel_manual", Status: "waiting_inventory",
	})
	if err != nil {
		t.Fatalf("parallel manual purchase should be accepted: %v", err)
	}
	open, err = st.ListOpenSupplyOrders(ctx, 10)
	if err != nil || len(open) != 3 || open[2].OrderID != manualParallel.OrderID {
		t.Fatalf("parallel manual open purchase = %#v err=%v", open, err)
	}
}

func TestPurchaseQueriesExcludeRecoveryImportRows(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t, testutil.NewConfig(t))
	now := time.Now().Truncate(time.Millisecond)

	purchaseCreatedAt := now.Add(-10 * time.Minute).UnixMilli()
	purchaseCompletedAt := now.Add(-9 * time.Minute).UnixMilli()
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID:           "56808",
		Product:           "oauth_7d",
		RequestedQuantity: 3,
		Automatic:         true,
		Strategy:          "strong_supply",
		Status:            "completed",
		ChargedFen:        885,
		ItemCount:         3,
		ImportedCount:     3,
		CreatedAtMS:       purchaseCreatedAt,
		CompletedAtMS:     purchaseCompletedAt,
	}); err != nil {
		t.Fatalf("create purchase: %v", err)
	}

	recoveryRows := []store.SupplyOrder{
		{
			OrderID: "synthetic-by-strategy", Product: "oauth_7d", RequestedQuantity: 1, Automatic: true,
			Strategy: "recovery", Status: "completed", CreatedAtMS: now.Add(-3 * time.Minute).UnixMilli(), CompletedAtMS: now.Add(-2 * time.Minute).UnixMilli(),
		},
		{
			OrderID: "synthetic-by-remote", Product: "oauth_7d", RequestedQuantity: 1, Automatic: true,
			RemoteStatus: "recovery_claimed", Status: "completed", CreatedAtMS: now.Add(-2 * time.Minute).UnixMilli(), CompletedAtMS: now.Add(-time.Minute).UnixMilli(),
		},
		{
			OrderID: "recovery-12363", Product: "oauth_7d", RequestedQuantity: 1, Automatic: true,
			Status: "completed", CreatedAtMS: now.Add(-time.Minute).UnixMilli(), CompletedAtMS: now.Add(-30 * time.Second).UnixMilli(),
		},
	}
	for _, order := range recoveryRows {
		if _, err := st.CreateSupplyOrder(ctx, order); err != nil {
			t.Fatalf("create recovery row %q: %v", order.OrderID, err)
		}
	}

	latest, found, err := st.GetLatestAutomaticSupplyOrder(ctx)
	if err != nil || !found || latest.OrderID != "56808" {
		t.Fatalf("latest automatic purchase = %#v found=%v err=%v", latest, found, err)
	}
	latestCompleted, found, err := st.GetLatestCompletedAutomaticSupplyOrder(ctx)
	if err != nil || !found || latestCompleted.OrderID != "56808" {
		t.Fatalf("latest completed purchase = %#v found=%v err=%v", latestCompleted, found, err)
	}

	orders, err := st.ListSupplyOrders(ctx, 50)
	if err != nil || len(orders) != 1 || orders[0].OrderID != "56808" {
		t.Fatalf("purchase list = %#v err=%v", orders, err)
	}
	orders, err = st.ListSupplyOrdersBetween(ctx, now.Add(-time.Hour).UnixMilli(), now.Add(time.Hour).UnixMilli(), 50)
	if err != nil || len(orders) != 1 || orders[0].OrderID != "56808" {
		t.Fatalf("purchase range list = %#v err=%v", orders, err)
	}
}

func TestPurchaseHistoryHidesUnpaidLocalCreateFailures(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t, testutil.NewConfig(t))
	orders := []store.SupplyOrder{
		{
			OrderID: "create-unpaid", Product: "plus", RequestedQuantity: 1,
			Status: "failed", RemoteStatus: "failed",
		},
		{
			OrderID: "create-paid-evidence", Product: "plus", RequestedQuantity: 1,
			Status: "failed", RemoteStatus: "invalid_payload", ReadyQuantity: 1, Progress: 100,
		},
		{
			OrderID: "remote-failed-order", Product: "plus", RequestedQuantity: 1,
			Status: "failed", RemoteStatus: "failed",
		},
		{
			OrderID: "remote-completed-order", Product: "plus", RequestedQuantity: 1,
			Status: "completed", RemoteStatus: "completed", ChargedFen: 1666, ItemCount: 1, ImportedCount: 1,
		},
	}
	for index := range orders {
		orders[index].CreatedAtMS = time.Now().Add(time.Duration(index) * time.Second).UnixMilli()
		if _, err := st.CreateSupplyOrder(ctx, orders[index]); err != nil {
			t.Fatalf("create order %q: %v", orders[index].OrderID, err)
		}
	}

	history, err := st.ListSupplyOrders(ctx, 50)
	if err != nil {
		t.Fatalf("list purchase history: %v", err)
	}
	got := make([]string, 0, len(history))
	for _, order := range history {
		got = append(got, order.OrderID)
	}
	want := []string{"remote-completed-order", "remote-failed-order", "create-paid-evidence"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("purchase history = %v, want %v", got, want)
	}
}

func TestLegacyPurchaseRepairSkipsRecoveryRows(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t, testutil.NewConfig(t))
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "recovery-legacy", Product: "oauth_7d", RequestedQuantity: 1,
		Automatic: true, Strategy: "recovery", Status: "completed",
	}); err != nil {
		t.Fatalf("create recovery row: %v", err)
	}
	if _, err := st.InsertSupplyImportItems(ctx, "recovery-legacy", []store.SupplyImportItem{{
		OrderID: "recovery-legacy", ItemKey: "legacy", FileName: "supply-legacy.json", PayloadJSON: `{}`,
	}}); err != nil {
		t.Fatalf("insert recovery import: %v", err)
	}
	items, err := st.ListPendingSupplyImportItems(ctx, "recovery-legacy", time.Now().UnixMilli(), 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("pending recovery items=%#v err=%v", items, err)
	}
	if err := st.MarkSupplyImportItemImported(ctx, items[0].ID, time.Now().UnixMilli()); err != nil {
		t.Fatalf("mark recovery import completed: %v", err)
	}

	if order, found, err := st.ActivateNextLegacySupplyRepair(ctx); err != nil || found {
		t.Fatalf("recovery row activated as legacy purchase repair: order=%#v found=%v err=%v", order, found, err)
	}
}
