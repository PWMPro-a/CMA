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
