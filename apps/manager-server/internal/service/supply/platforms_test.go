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

func TestLowPriceReservePlatformCeilingUsesDedicatedLimitAndFallsBack(t *testing.T) {
	reserveCeiling := int64(1300)
	platformCeiling := int64(800)
	cfg := store.ManagerSupplyConfig{LowPriceReserveMaxUnitPriceFen: &reserveCeiling}
	if got := lowPriceReservePlatformCeiling(cfg, store.ManagerSupplyPlatformConfig{MaxUnitPriceFen: &platformCeiling}); got != 1300 {
		t.Fatalf("effective ceiling = %d, want dedicated reserve ceiling 1300", got)
	}
	cfg.LowPriceReserveMaxUnitPriceFen = nil
	if got := lowPriceReservePlatformCeiling(cfg, store.ManagerSupplyPlatformConfig{MaxUnitPriceFen: &platformCeiling}); got != 800 {
		t.Fatalf("fallback ceiling = %d, want platform ceiling 800", got)
	}
}

func TestManualSupplyPlatformCredentialsIgnoreAutomaticPriceCeiling(t *testing.T) {
	platformCeiling := int64(1400)
	credentials := manualSupplyPlatformCredentials(store.ManagerSupplyPlatformConfig{
		Type: managerconfigsvc.SupplyPlatformNvtokens, MaxUnitPriceFen: &platformCeiling,
	})
	if credentials.MaxUnitPriceFen != 0 {
		t.Fatalf("manual max unit price = %d, want unlimited preview/purchase", credentials.MaxUnitPriceFen)
	}
}

func TestRequireCredentialsAcceptsNativeNvtokensProducts(t *testing.T) {
	enabled := true
	service := New(nil, nil, nil)
	for _, product := range []string{"plus", "pro", "team", "bugteam", "k12", "grokfree", "grokpro", "free"} {
		cfg := store.ManagerSupplyConfig{Platforms: []store.ManagerSupplyPlatformConfig{{
			ID:      "nvtokens-main",
			Type:    managerconfigsvc.SupplyPlatformNvtokens,
			Enabled: &enabled,
			BaseURL: "https://nvtokens.com",
			Token:   "session-token",
			Product: product,
		}}}
		if err := service.requireCredentials(cfg); err != nil {
			t.Fatalf("product %s credentials: %v", product, err)
		}
	}
}

func TestRequireCredentialsRejectsProductFromAnotherPlatformType(t *testing.T) {
	enabled := true
	service := New(nil, nil, nil)
	err := service.requireCredentials(store.ManagerSupplyConfig{Platforms: []store.ManagerSupplyPlatformConfig{{
		ID:      "nvtokens-main",
		Type:    managerconfigsvc.SupplyPlatformNvtokens,
		Enabled: &enabled,
		BaseURL: "https://nvtokens.com",
		Token:   "session-token",
		Product: "oauth_30d",
	}}})
	if err == nil {
		t.Fatal("legacy product should be rejected for nvtokens")
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
	if status.CostPerCapacityFen != 0.5 {
		t.Fatalf("capacity cost = %.2f fen/M, want 0.5", status.CostPerCapacityFen)
	}
	if status.CostMultiplier != 0.005 {
		t.Fatalf("cost multiplier = %.6f, want 0.005", status.CostMultiplier)
	}
}

func TestSupplyPlatformSelectionPrefersLowerCostPerCapacity(t *testing.T) {
	higherPriceHigherQuota := supplyPlatformTestOverview("high-quota", 10, 200, 2_000, 120, 100, 2)
	lowerPriceLowerQuota := supplyPlatformTestOverview("low-quota", 10, 100, 2_000, 120, 20, 5)

	if !supplyPlatformLess(higherPriceHigherQuota, lowerPriceLowerQuota, 2, 0, nil, false) {
		t.Fatal("higher unit price with lower price/capacity should be selected")
	}
}

func TestBestSupplyPlatformCandidateUsesRawPriceForUnknownCapacity(t *testing.T) {
	statuses := []PlatformOverview{
		supplyPlatformTestOverview("sampled", 10, 2300, 10_000, 120, 170, 13.5294),
		supplyPlatformTestOverview("unknown-cheap", 10, 1800, 10_000, 120, 0, 0),
		supplyPlatformTestOverview("unknown-expensive", 10, 2400, 10_000, 120, 0, 0),
	}
	if got := bestSupplyPlatformCandidateIndex(statuses, []int{0, 1, 2}, 1, 0, nil, false, false); got != 1 {
		t.Fatalf("best platform index = %d, want cheap unknown trial", got)
	}
	statuses[1].Inventory.EstimatedUnitPriceFen = 2350
	if got := bestSupplyPlatformCandidateIndex(statuses, []int{0, 1, 2}, 1, 0, nil, false, false); got != 0 {
		t.Fatalf("best platform index = %d, want sampled value", got)
	}
}

func TestSupplyPlatformExpectedQuotaUsesProductPlan(t *testing.T) {
	platform := store.ManagerSupplyPlatformConfig{ID: "nv", Product: "plus"}
	resource := SmartResource{AccountQuotaPlanEstimates: []SmartQuotaPlanEstimate{
		{SupplierID: "nv", PlanType: "team", AdoptedM: 40},
		{SupplierID: "nv", PlanType: "plus", AdoptedM: 170},
	}}
	if got := supplyPlatformExpectedQuotaM(store.ManagerSupplyConfig{}, resource, platform); got != 170 {
		t.Fatalf("plus expected quota = %.2f, want 170", got)
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

func TestSelectLowPriceReservePlatformUsesOnlyCheapImmediateStock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, _ := r.Cookie("scm_session")
		token := ""
		if cookie != nil {
			token = cookie.Value
		}
		switch r.URL.Path {
		case "/api/workspace/extractions/estimate":
			switch token {
			case "expensive":
				_, _ = w.Write([]byte(`{"available_quantity":20,"total_cost_cents":500,"unit_price_cents":250}`))
			case "cheap":
				_, _ = w.Write([]byte(`{"available_quantity":3,"total_cost_cents":150,"unit_price_cents":50}`))
			default:
				_, _ = w.Write([]byte(`{"available_quantity":20,"total_cost_cents":20,"unit_price_cents":10}`))
			}
		case "/api/me":
			_, _ = w.Write([]byte(`{"user":{"available_balance_cents":100000,"balance_cents":100000}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	enabled := true
	ceiling := int64(100)
	cfg := store.ManagerSupplyConfig{
		LowPriceReserveEnabled: &enabled, LowPriceReserveMaxUnitPriceFen: &ceiling,
		LowPriceReserveTargetAccounts: 20,
		Platforms: []store.ManagerSupplyPlatformConfig{
			{ID: "expensive", Type: "nvtokens", Enabled: &enabled, BaseURL: server.URL, Token: "expensive", Product: "plus", Priority: 1},
			{ID: "cheap", Type: "nvtokens", Enabled: &enabled, BaseURL: server.URL, Token: "cheap", Product: "plus", Priority: 2},
			{ID: "emergency", Type: "nvtokens", Enabled: &enabled, BaseURL: server.URL, Token: "emergency", Product: "plus", Priority: 3, EmergencyOnly: true},
		},
	}
	service := New(nil, nil, server.Client())
	selection, matched, err := service.selectLowPriceReservePlatform(context.Background(), cfg, 5, nil)
	if err != nil || !matched || selection.platform.ID != "cheap" {
		t.Fatalf("low-price selection = %q matched=%v err=%v statuses=%#v", selection.platform.ID, matched, err, selection.all)
	}
	ceiling = 40
	cfg.LowPriceReserveMaxUnitPriceFen = &ceiling
	_, matched, err = service.selectLowPriceReservePlatform(context.Background(), cfg, 5, nil)
	if err != nil || matched {
		t.Fatalf("over-ceiling stock matched=%v err=%v", matched, err)
	}
}

func TestSelectLowPriceReservePlatformIgnoresPriorityFirstAfterPriceQualification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspace/extractions/estimate":
			cookie, _ := r.Cookie("scm_session")
			if cookie != nil && cookie.Value == "priority" {
				_, _ = w.Write([]byte(`{"available_quantity":10,"total_cost_cents":180,"unit_price_cents":90}`))
				return
			}
			_, _ = w.Write([]byte(`{"available_quantity":10,"total_cost_cents":100,"unit_price_cents":50}`))
		case "/api/me":
			_, _ = w.Write([]byte(`{"user":{"available_balance_cents":100000,"balance_cents":100000}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	enabled := true
	ceiling := int64(100)
	cfg := store.ManagerSupplyConfig{
		LowPriceReserveEnabled: &enabled, LowPriceReserveMaxUnitPriceFen: &ceiling,
		LowPriceReserveTargetAccounts: 20,
		PlatformSelectionStrategy:     managerconfigsvc.SupplyPlatformSelectionPriorityFirst,
		Platforms: []store.ManagerSupplyPlatformConfig{
			{ID: "priority", Type: "nvtokens", Enabled: &enabled, BaseURL: server.URL, Token: "priority", Product: "plus", Priority: 1},
			{ID: "cheapest", Type: "nvtokens", Enabled: &enabled, BaseURL: server.URL, Token: "cheapest", Product: "plus", Priority: 2},
		},
	}
	service := New(nil, nil, server.Client())
	selection, matched, err := service.selectLowPriceReservePlatform(context.Background(), cfg, 2, nil)
	if err != nil || !matched || selection.platform.ID != "cheapest" {
		t.Fatalf("low-price priority selection = %q matched=%v err=%v", selection.platform.ID, matched, err)
	}
}

func TestSelectLowPriceReservePlatformDoesNotQuoteOtherPlatformTypes(t *testing.T) {
	var legacyCalls atomic.Int32
	nvServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspace/extractions/estimate":
			_, _ = w.Write([]byte(`{"available_quantity":5,"total_cost_cents":100,"unit_price_cents":20}`))
		case "/api/me":
			_, _ = w.Write([]byte(`{"user":{"available_balance_cents":100000,"balance_cents":100000}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer nvServer.Close()
	legacyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		legacyCalls.Add(1)
		http.Error(w, "legacy platform should not be quoted", http.StatusInternalServerError)
	}))
	defer legacyServer.Close()

	enabled := true
	ceiling := int64(100)
	service := New(nil, nil, nvServer.Client())
	selection, matched, err := service.selectLowPriceReservePlatform(context.Background(), store.ManagerSupplyConfig{
		LowPriceReserveEnabled: &enabled, LowPriceReserveMaxUnitPriceFen: &ceiling,
		LowPriceReserveTargetAccounts: 10,
		Platforms: []store.ManagerSupplyPlatformConfig{
			{ID: "legacy", Type: "legacy", Enabled: &enabled, BaseURL: legacyServer.URL, Token: "legacy", Product: "oauth_30d"},
			{ID: "nv", Type: "nvtokens", Enabled: &enabled, BaseURL: nvServer.URL, Token: "nv-session", Product: "plus"},
		},
	}, 2, nil)
	if err != nil || !matched || selection.platform.ID != "nv" {
		t.Fatalf("selection=%q matched=%v err=%v", selection.platform.ID, matched, err)
	}
	if legacyCalls.Load() != 0 {
		t.Fatalf("legacy platform quote calls = %d, want 0", legacyCalls.Load())
	}
}

func TestSelectLowPriceReserveCatalogPlatformPublishesLatestPriceWithoutEstimate(t *testing.T) {
	var catalogCalls atomic.Int32
	var estimateCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspace/seller-candidates":
			catalogCalls.Add(1)
			_, _ = w.Write([]byte(`{"sellers":[{"sale_plans":["plus"],"sale_plan_counts":{"plus":200},"sale_plan_prices":{"plus":{"min_cents":1699,"max_cents":99900}}}]}`))
		case "/api/workspace/extractions/estimate":
			estimateCalls.Add(1)
			http.Error(w, "estimate endpoint should not be polled by the watcher", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	enabled := true
	ceiling := int64(1500)
	service := New(nil, nil, server.Client())
	selection, matched, err := service.selectLowPriceReserveCatalogPlatform(context.Background(), store.ManagerSupplyConfig{
		LowPriceReserveEnabled: &enabled, LowPriceReserveMaxUnitPriceFen: &ceiling,
		Platforms: []store.ManagerSupplyPlatformConfig{{
			ID: "nv", Type: "nvtokens", Enabled: &enabled, BaseURL: server.URL, Token: "nv-session", Product: "plus",
		}},
	}, 100)
	if err != nil || matched {
		t.Fatalf("matched=%v err=%v selection=%#v", matched, err, selection)
	}
	if catalogCalls.Load() != 1 || estimateCalls.Load() != 0 || len(selection.all) != 1 || selection.all[0].Inventory == nil || selection.all[0].Inventory.EstimatedUnitPriceFen != 1699 {
		t.Fatalf("catalog=%d estimate=%d statuses=%#v", catalogCalls.Load(), estimateCalls.Load(), selection.all)
	}
}

func TestSelectLowPriceReserveCatalogPlatformAppliesSupplierQuotaGate(t *testing.T) {
	var approvedPrice atomic.Int64
	approvedPrice.Store(1399)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workspace/seller-candidates" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, `{"sellers":[
			{"seller_token":"seller-blocked","selection_token":"select-blocked","sale_plan_counts":{"plus":10},"sale_plan_prices":{"plus":{"min_cents":1200,"max_cents":1200}}},
			{"seller_token":"seller-approved","selection_token":"select-approved","sale_plan_counts":{"plus":10},"sale_plan_prices":{"plus":{"min_cents":%d,"max_cents":%d}}}
		]}`, approvedPrice.Load(), approvedPrice.Load())
	}))
	defer server.Close()

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "low-price-catalog-gate.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	ceiling := int64(1300)
	platformCeiling := int64(1400)
	platform := store.ManagerSupplyPlatformConfig{
		ID: "nv", Name: "NV", Type: managerconfigsvc.SupplyPlatformNvtokens, Enabled: &enabled,
		BaseURL: server.URL, Token: "nv-session", Product: "plus", MaxUnitPriceFen: &platformCeiling,
		SupplierQuotaGateEnabled: &enabled, SupplierQuotaMinimumM: 90, SupplierQuotaTrialQuantity: 1,
	}
	service := New(st, nil, server.Client())
	service.setCachedMarketplaceSupplierQuotaScores(supplierQuotaScoreCacheKey(platform), []SupplierQuotaScore{
		{SellerID: "seller-blocked", Status: supplierQuotaStatusBlocked, ScoreM: 20, SampleCount: 1},
		{SellerID: "seller-approved", Status: supplierQuotaStatusApproved, ScoreM: 120, SampleCount: 1},
	}, time.Now())
	cfg := store.ManagerSupplyConfig{
		LowPriceReserveEnabled: &enabled, LowPriceReserveMaxUnitPriceFen: &ceiling,
		LowPriceReserveTargetAccounts: 30, Platforms: []store.ManagerSupplyPlatformConfig{platform},
	}

	selection, matched, err := service.selectLowPriceReserveCatalogPlatform(ctx, cfg, 15)
	if err != nil || matched || len(selection.all) != 1 || selection.all[0].Inventory != nil ||
		len(selection.all[0].SupplierQuotaScores) != 2 {
		t.Fatalf("over-ceiling eligible catalog = %#v matched=%v err=%v", selection, matched, err)
	}

	approvedPrice.Store(1250)
	selection, matched, err = service.selectLowPriceReserveCatalogPlatform(ctx, cfg, 15)
	if err != nil || !matched || selection.marketplaceSeller == nil ||
		selection.marketplaceSeller.candidate.SellerID != "seller-approved" ||
		selection.status.Inventory == nil || selection.status.Inventory.EstimatedUnitPriceFen != 1250 {
		t.Fatalf("eligible catalog = %#v matched=%v err=%v", selection, matched, err)
	}
}

func TestSelectLowPriceReserveCatalogPlatformUsesDedicatedCeilingBeforeSellerGate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workspace/seller-candidates" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"sellers":[{
			"seller_token":"seller-low","selection_token":"select-low",
			"sale_plan_counts":{"plus":14},
			"sale_plan_prices":{"plus":{"min_cents":1738,"max_cents":1788}}
		}]}`))
	}))
	defer server.Close()

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "low-price-dedicated-ceiling.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	reserveCeiling := int64(1800)
	regularCeiling := int64(1400)
	platform := store.ManagerSupplyPlatformConfig{
		ID: "nv", Name: "NV", Type: managerconfigsvc.SupplyPlatformNvtokens, Enabled: &enabled,
		BaseURL: server.URL, Token: "nv-session", Product: "plus", MaxUnitPriceFen: &regularCeiling,
		SupplierQuotaGateEnabled: &enabled, SupplierQuotaMinimumM: 90, SupplierQuotaTrialQuantity: 1,
	}
	service := New(st, nil, server.Client())
	service.setCachedMarketplaceSupplierQuotaScores(supplierQuotaScoreCacheKey(platform), []SupplierQuotaScore{{
		SellerID: "seller-low", Status: supplierQuotaStatusObserving,
		Reason: "waiting_for_more_supplier_evidence", RetryAfterMS: time.Now().Add(-time.Second).UnixMilli(),
	}}, time.Now())

	selection, matched, err := service.selectLowPriceReserveCatalogPlatform(ctx, store.ManagerSupplyConfig{
		LowPriceReserveEnabled: &enabled, LowPriceReserveMaxUnitPriceFen: &reserveCeiling,
		LowPriceReserveTargetAccounts: 30, Platforms: []store.ManagerSupplyPlatformConfig{platform},
	}, 15)
	if err != nil || !matched || selection.marketplaceSeller == nil ||
		selection.marketplaceSeller.candidate.SellerID != "seller-low" ||
		selection.quantity != 1 || selection.status.Inventory == nil ||
		selection.status.Inventory.EstimatedUnitPriceFen != 1738 {
		t.Fatalf("dedicated reserve ceiling selection = %#v matched=%v err=%v", selection, matched, err)
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

func TestQuotePlatformProductFallsBackToNvtokensCatalogAfterNoContent(t *testing.T) {
	var estimateCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspace/extractions/estimate":
			estimateCalls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case "/api/workspace/seller-candidates":
			_, _ = w.Write([]byte(`{"sellers":[{"sale_plans":["plus"],"sale_plan_counts":{"plus":75},"sale_plan_prices":{"plus":{"min_cents":1700,"max_cents":2000}}}]}`))
		case "/api/me":
			_, _ = w.Write([]byte(`{"available_balance_cents":100000}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "catalog-fallback.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		Platforms: []store.ManagerSupplyPlatformConfig{{
			ID: "nv", Type: "nvtokens", Enabled: &enabled, BaseURL: server.URL, Token: "session", Product: "plus",
		}},
	}}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	quote, err := service.QuotePlatformProduct(ctx, 10, "nv", "plus")
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if estimateCalls.Load() != 2 || quote.Inventory == nil || quote.Inventory.Available != 75 || quote.Inventory.EstimatedUnitPriceFen != 1700 || quote.Inventory.EstimatedTotalFen != 17000 {
		t.Fatalf("estimate calls=%d quote=%#v", estimateCalls.Load(), quote)
	}
}

func TestQuotePlatformProductDoesNotApplyAutomaticPriceCeiling(t *testing.T) {
	var observedMaxUnitPrice any = "missing"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspace/extractions/estimate":
			payload := map[string]any{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode estimate payload: %v", err)
			}
			observedMaxUnitPrice = payload["max_unit_price_cents"]
			_, _ = w.Write([]byte(`{"estimate":{"available_quantity":2,"buyer_total_cents":3300,"unit_price_cents":1650}}`))
		case "/api/me":
			_, _ = w.Write([]byte(`{"available_balance_cents":100000}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "manual-quote-unlimited.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	automaticCeiling := int64(1400)
	if err := st.SaveManagerConfig(ctx, store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		Platforms: []store.ManagerSupplyPlatformConfig{{
			ID: "nv", Type: managerconfigsvc.SupplyPlatformNvtokens, Enabled: &enabled,
			BaseURL: server.URL, Token: "session", Product: "plus", MaxUnitPriceFen: &automaticCeiling,
		}},
	}}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	quote, err := service.QuotePlatformProduct(ctx, 2, "nv", "plus")
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if observedMaxUnitPrice != nil {
		t.Fatalf("manual quote max_unit_price_cents = %#v, want null", observedMaxUnitPrice)
	}
	if quote.Inventory == nil || quote.Inventory.EstimatedUnitPriceFen != 1650 || quote.Inventory.EstimatedTotalFen != 3300 {
		t.Fatalf("quote = %#v", quote)
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
		ExpectedQuotaM:        usableQuotaM,
		UsableQuotaM:          usableQuotaM,
		CostPerCapacityFen:    effectiveCostFen,
		CostPerUsableQuotaFen: effectiveCostFen,
	}
}
