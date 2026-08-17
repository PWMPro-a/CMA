package supply

import (
	"testing"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/supplyclient"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

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
