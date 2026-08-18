package supply

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	managerconfigsvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/supplyclient"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

func TestSupplyPlatformCredentialsIncludeNvtokensPurchaseFilters(t *testing.T) {
	maxUnitPriceFen := int64(800)
	credentials := supplyPlatformCredentials(store.ManagerSupplyPlatformConfig{
		ID:                  "nvtokens-main",
		Type:                managerconfigsvc.SupplyPlatformNvtokens,
		BaseURL:             "https://nvtokens.com/",
		PurchaseAccountType: managerconfigsvc.SupplyPurchaseAccountHasRefreshToken,
		MaxUnitPriceFen:     &maxUnitPriceFen,
	})
	if credentials.PurchaseAccountType != managerconfigsvc.SupplyPurchaseAccountHasRefreshToken || credentials.MaxUnitPriceFen != 800 {
		t.Fatalf("nvtokens credentials = %#v", credentials)
	}
}

func TestSupplyPlatformEconomicsUsesSupplierQuotaAndLifetimeDemand(t *testing.T) {
	status := PlatformOverview{Inventory: &supplyclient.Inventory{
		EstimatedUnitPriceFen:   120,
		MaximumRemainingSeconds: 60 * 60,
	}}
	cfg := store.ManagerSupplyConfig{Platforms: []store.ManagerSupplyPlatformConfig{{
		ID: "supplier-a",
		QuotaEstimationPolicies: map[string]store.ManagerSupplyQuotaEstimationPolicy{
			"team": {FallbackM: 240},
		},
	}}}
	resource := SmartResource{
		ConsumeTokenMPerMinute: 1,
		AccountQuotaPlanEstimates: []SmartQuotaPlanEstimate{{
			SupplierID: "supplier-a", PlanType: "team", AdoptedM: 240,
		}},
	}

	applySupplyPlatformEconomics(&status, cfg, resource, cfg.Platforms[0], 2)

	if status.ExpectedQuotaM != 240 {
		t.Fatalf("expected quota = %.2f, want 240", status.ExpectedQuotaM)
	}
	if status.UsableQuotaM != 30 {
		t.Fatalf("usable quota = %.2f, want 30", status.UsableQuotaM)
	}
	if status.CostPerUsableQuotaFen != 4 {
		t.Fatalf("effective cost = %.2f fen/M, want 4", status.CostPerUsableQuotaFen)
	}
}

func TestSupplyPlatformSelectionPrefersLowerEffectiveQuotaCost(t *testing.T) {
	higherPriceHigherQuota := supplyPlatformTestOverview("high-quota", 10, 200, 2_000, 120, 100, 2)
	lowerPriceLowerQuota := supplyPlatformTestOverview("low-quota", 10, 100, 2_000, 120, 20, 5)

	if !supplyPlatformLess(higherPriceHigherQuota, lowerPriceLowerQuota, 2, 0, nil, false) {
		t.Fatal("higher unit price with lower effective fen/M should be selected")
	}
}

func TestSupplyPlatformSelectionPrefersImmediateCompleteInventory(t *testing.T) {
	inStock := supplyPlatformTestOverview("in-stock", 2, 300, 2_000, 120, 30, 10)
	production := supplyPlatformTestOverview("production", 0, 50, 2_000, 360, 100, 0.5)
	production.Inventory.NeedsProduction = true

	if !supplyPlatformLess(inStock, production, 2, 0, nil, false) {
		t.Fatal("complete in-stock delivery should beat cheaper production inventory")
	}
}

func TestSupplyPlatformSelectionSkipsInsufficientBalanceAndReserve(t *testing.T) {
	reserveBlocked := supplyPlatformTestOverview("reserve-blocked", 5, 100, 250, 120, 100, 1)
	reserveBlocked.Inventory.EstimatedTotalFen = 200
	eligible := supplyPlatformTestOverview("eligible", 5, 180, 1_000, 120, 30, 6)
	eligible.Inventory.EstimatedTotalFen = 360

	if supplyPlatformLess(reserveBlocked, eligible, 2, 100, nil, false) {
		t.Fatal("platform that would consume the protected balance reserve was selected")
	}
	if !supplyPlatformLess(eligible, reserveBlocked, 2, 100, nil, false) {
		t.Fatal("platform with enough post-purchase reserve should be selected")
	}
}

func TestSupplyPlatformSelectionEmergencyPrefersLongerValidity(t *testing.T) {
	short := supplyPlatformTestOverview("short", 5, 100, 2_000, 10, 50, 2)
	long := supplyPlatformTestOverview("long", 5, 100, 2_000, 60, 50, 2)

	if supplyPlatformLess(short, long, 2, 0, nil, true) {
		t.Fatal("emergency selection preferred the shorter validity window")
	}
	if !supplyPlatformLess(long, short, 2, 0, nil, true) {
		t.Fatal("emergency selection should prefer the longer validity window")
	}
}

func TestSupplyPlatformSelectionSpreadsEqualOrdersAcrossSuppliers(t *testing.T) {
	usedPlatform := supplyPlatformTestOverview("supplier-a", 5, 100, 2_000, 60, 50, 2)
	freshPlatform := supplyPlatformTestOverview("supplier-b", 5, 100, 2_000, 60, 50, 2)
	used := map[string]struct{}{"supplier-a": {}}

	if supplyPlatformLess(usedPlatform, freshPlatform, 2, 0, used, false) {
		t.Fatal("equal procurement should not concentrate another order on the active supplier")
	}
	if !supplyPlatformLess(freshPlatform, usedPlatform, 2, 0, used, false) {
		t.Fatal("equal procurement should spread the order to the unused supplier")
	}
}

func TestSupplyPlatformSelectionPriorityFirstPrefersConfiguredPriority(t *testing.T) {
	preferred := supplyPlatformTestOverview("sogouedu", 5, 300, 10_000, 60, 30, 10)
	preferred.Priority = 1
	cheaper := supplyPlatformTestOverview("bugteam", 5, 100, 10_000, 60, 100, 1)
	cheaper.Priority = 2

	if !supplyPlatformLessWithPriority(preferred, cheaper, 2, 0, nil, false, true) {
		t.Fatal("priority-first selection should prefer the lower configured priority")
	}
	if supplyPlatformLessWithPriority(cheaper, preferred, 2, 0, nil, false, true) {
		t.Fatal("priority-first selection should not let lower cost override configured priority")
	}
}

func TestSupplyPlatformSelectionPriorityFirstStillFallsBackToDeliverableStock(t *testing.T) {
	production := supplyPlatformTestOverview("sogouedu", 0, 300, 10_000, 60, 30, 10)
	production.Priority = 1
	production.Inventory.NeedsProduction = true
	inStock := supplyPlatformTestOverview("bugteam", 5, 100, 10_000, 60, 100, 1)
	inStock.Priority = 2

	if supplyPlatformLessWithPriority(production, inStock, 2, 0, nil, false, true) {
		t.Fatal("priority-first selection should fall back when the preferred platform has no deliverable stock")
	}
	if !supplyPlatformLessWithPriority(inStock, production, 2, 0, nil, false, true) {
		t.Fatal("deliverable fallback stock should beat production-only preferred inventory")
	}
}

func TestEmergencyOnlySupplyPlatformIsReservedForEmergencyOrExplicitSelection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Customer-Token")
		switch r.URL.Path {
		case "/api/customer/inventory":
			if token == "bugteam-token" {
				_, _ = w.Write([]byte(`{"available":50,"estimated_total_fen":150,"estimated_unit_price_fen":75}`))
				return
			}
			_, _ = w.Write([]byte(`{"available":0,"needs_production":true,"estimated_total_fen":100,"estimated_unit_price_fen":50}`))
		case "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":100000,"balance_fen":100000}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	enabled := true
	cfg := store.ManagerSupplyConfig{Platforms: []store.ManagerSupplyPlatformConfig{
		{ID: "legacy", Name: "sogouedu", Type: "legacy", Enabled: &enabled, BaseURL: server.URL, Token: "legacy-token", Product: "oauth_7d", Priority: 1},
		{ID: "bugteam", Name: "BugTeam", Type: "bugteam", Enabled: &enabled, BaseURL: server.URL, Token: "bugteam-token", Product: "team_1h", Priority: 2, EmergencyOnly: true},
	}}
	service := New(nil, nil, server.Client())

	selection, err := service.selectSupplyPlatform(context.Background(), cfg, 2, nil)
	if err != nil || selection.platform.ID != "legacy" {
		t.Fatalf("normal selection = %q err=%v, want legacy", selection.platform.ID, err)
	}

	service.setSmartResource(SmartResource{GeneratedAtMS: time.Now().UnixMilli(), EmergencyShortage: true})
	selection, err = service.selectSupplyPlatform(context.Background(), cfg, 2, nil)
	if err != nil || selection.platform.ID != "bugteam" {
		t.Fatalf("emergency selection = %q err=%v, want bugteam", selection.platform.ID, err)
	}

	service.setSmartResource(SmartResource{GeneratedAtMS: time.Now().UnixMilli()})
	selection, err = service.selectSupplyPlatform(context.Background(), cfg, 2, nil, "bugteam")
	if err != nil || selection.platform.ID != "bugteam" {
		t.Fatalf("explicit selection = %q err=%v, want bugteam", selection.platform.ID, err)
	}
}

func TestResolveSupplyPlatformDoesNotFallbackWhenProductIsUnknown(t *testing.T) {
	enabled := true
	cfg := store.ManagerSupplyConfig{Platforms: []store.ManagerSupplyPlatformConfig{{
		ID: "supplier-a", Type: "legacy", Enabled: &enabled, BaseURL: "https://example.com", Token: "token", Product: "oauth_30d",
	}}}
	if _, err := resolveSupplyPlatform(cfg, "", "missing-product"); err == nil {
		t.Fatal("unknown product should not fall back to the first platform")
	}
}

func TestExplicitNvtokensProductQuoteUsesNativeProduct(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "quote-session", Path: "/"})
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/workspace/extractions/estimate":
			payload := map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload["sale_plan_filter"] != "pro" {
				t.Fatalf("sale_plan_filter = %#v, want pro", payload["sale_plan_filter"])
			}
			_, _ = w.Write([]byte(`{"estimate":{"available_quantity":6,"total_cost_cents":1200,"unit_price_cents":600}}`))
		case "/api/me":
			_, _ = w.Write([]byte(`{"available_balance_cents":10000}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	enabled := true
	cfg := store.ManagerSupplyConfig{Platforms: []store.ManagerSupplyPlatformConfig{{
		ID: "nvtokens-main", Type: managerconfigsvc.SupplyPlatformNvtokens, Enabled: &enabled,
		BaseURL: server.URL, Username: "buyer", Password: "secret", Product: "plus",
	}}}
	service := New(nil, nil, server.Client())
	selection, err := service.selectSupplyPlatformProduct(context.Background(), cfg, 2, nil, "nvtokens-main", "pro")
	if err != nil {
		t.Fatalf("quote native product: %v", err)
	}
	if selection.platform.Product != "pro" || selection.status.Product != "pro" || selection.status.Inventory == nil || selection.status.Inventory.Available != 6 {
		t.Fatalf("selection = %#v status=%#v", selection.platform, selection.status)
	}
}

func supplyPlatformTestOverview(id string, available int, unitPriceFen int64, balanceFen int64, lifetimeMinutes int64, usableQuotaM float64, effectiveCostFen float64) PlatformOverview {
	return PlatformOverview{
		ID: id,
		Inventory: &supplyclient.Inventory{
			Available:               available,
			EstimatedUnitPriceFen:   unitPriceFen,
			EstimatedTotalFen:       unitPriceFen * 2,
			MaximumRemainingSeconds: lifetimeMinutes * 60,
		},
		Balance:               &supplyclient.Balance{AvailableFen: balanceFen},
		UsableQuotaM:          usableQuotaM,
		CostPerUsableQuotaFen: effectiveCostFen,
	}
}
