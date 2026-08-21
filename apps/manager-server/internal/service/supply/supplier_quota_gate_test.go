package supply

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/supplyclient"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/testutil"
)

func TestChooseMarketplaceSellerPrefersApprovedAndBoundsTrial(t *testing.T) {
	enabled := true
	platform := store.ManagerSupplyPlatformConfig{
		Type:                       "nvtokens",
		SupplierQuotaGateEnabled:   &enabled,
		SupplierQuotaTrialQuantity: 1,
		SupplierQuotaMinimumM:      30,
	}
	candidates := []supplyclient.MarketplaceSellerCandidate{
		{SellerID: "blocked", Name: "Blocked", SelectionToken: "blocked-token", Available: 20, MinUnitPriceFen: 100},
		{SellerID: "trial", Name: "Trial", SelectionToken: "trial-token", Available: 20, MinUnitPriceFen: 90},
		{SellerID: "approved", Name: "Approved", SelectionToken: "approved-token", Available: 20, MinUnitPriceFen: 120},
	}
	scores := []SupplierQuotaScore{
		{SellerID: "blocked", Status: supplierQuotaStatusBlocked, ScoreM: 10},
		{SellerID: "trial", Status: supplierQuotaStatusUntried},
		{SellerID: "approved", Status: supplierQuotaStatusApproved, ScoreM: 60},
	}

	selection, err := chooseMarketplaceSellerForAutomaticPurchase(platform, 10, candidates, scores)
	if err != nil || selection == nil || selection.candidate.SellerID != "approved" || selection.quantity != 10 || selection.trial {
		t.Fatalf("approved selection = %#v err=%v", selection, err)
	}

	scores[2].Status = supplierQuotaStatusBlocked
	selection, err = chooseMarketplaceSellerForAutomaticPurchase(platform, 10, candidates, scores)
	if err != nil || selection == nil || selection.candidate.SellerID != "trial" || selection.quantity != 1 || !selection.trial {
		t.Fatalf("trial selection = %#v err=%v", selection, err)
	}

	scores[1].Status = supplierQuotaStatusObserving
	selection, err = chooseMarketplaceSellerForAutomaticPurchase(platform, 10, candidates, scores)
	if !errors.Is(err, ErrSupplierQuotaGateNoEligibleSeller) || selection != nil {
		t.Fatalf("blocked selection = %#v err=%v", selection, err)
	}
}

func TestChooseMarketplaceSellerPrefersLowestPriceAmongApproved(t *testing.T) {
	enabled := true
	platform := store.ManagerSupplyPlatformConfig{
		Type:                     "nvtokens",
		SupplierQuotaGateEnabled: &enabled,
		SupplierQuotaMinimumM:    90,
	}
	candidates := []supplyclient.MarketplaceSellerCandidate{
		{SellerID: "higher-quota", Name: "Higher Quota", SelectionToken: "higher-quota-token", Available: 20, MinUnitPriceFen: 2300},
		{SellerID: "lower-price", Name: "Lower Price", SelectionToken: "lower-price-token", Available: 20, MinUnitPriceFen: 1900},
	}
	scores := []SupplierQuotaScore{
		{SellerID: "higher-quota", Status: supplierQuotaStatusApproved, ScoreM: 150},
		{SellerID: "lower-price", Status: supplierQuotaStatusApproved, ScoreM: 95},
	}

	selection, err := chooseMarketplaceSellerForAutomaticPurchase(platform, 10, candidates, scores)
	if err != nil || selection == nil || selection.candidate.SellerID != "lower-price" || selection.quantity != 10 || selection.trial {
		t.Fatalf("low-price approved selection = %#v err=%v", selection, err)
	}
}

func TestChooseMarketplaceSellerUsesQuotaScoreForEqualPrice(t *testing.T) {
	enabled := true
	platform := store.ManagerSupplyPlatformConfig{
		Type:                     "nvtokens",
		SupplierQuotaGateEnabled: &enabled,
		SupplierQuotaMinimumM:    90,
	}
	candidates := []supplyclient.MarketplaceSellerCandidate{
		{SellerID: "lower-quota", Name: "Lower Quota", SelectionToken: "lower-quota-token", Available: 20, MinUnitPriceFen: 1900},
		{SellerID: "higher-quota", Name: "Higher Quota", SelectionToken: "higher-quota-token", Available: 20, MinUnitPriceFen: 1900},
	}
	scores := []SupplierQuotaScore{
		{SellerID: "lower-quota", Status: supplierQuotaStatusApproved, ScoreM: 95},
		{SellerID: "higher-quota", Status: supplierQuotaStatusApproved, ScoreM: 150},
	}

	selection, err := chooseMarketplaceSellerForAutomaticPurchase(platform, 10, candidates, scores)
	if err != nil || selection == nil || selection.candidate.SellerID != "higher-quota" {
		t.Fatalf("equal-price quota selection = %#v err=%v", selection, err)
	}
}

func TestChooseMarketplaceSellerNeverLetsCheapBlockedSellerBypassGate(t *testing.T) {
	enabled := true
	platform := store.ManagerSupplyPlatformConfig{
		Type:                     "nvtokens",
		SupplierQuotaGateEnabled: &enabled,
		SupplierQuotaMinimumM:    90,
	}
	candidates := []supplyclient.MarketplaceSellerCandidate{
		{SellerID: "blocked", Name: "Blocked", SelectionToken: "blocked-token", Available: 20, MinUnitPriceFen: 1000},
		{SellerID: "approved", Name: "Approved", SelectionToken: "approved-token", Available: 20, MinUnitPriceFen: 2000},
	}
	scores := []SupplierQuotaScore{
		{SellerID: "blocked", Status: supplierQuotaStatusBlocked, ScoreM: 20},
		{SellerID: "approved", Status: supplierQuotaStatusApproved, ScoreM: 100},
	}

	selection, err := chooseMarketplaceSellerForAutomaticPurchase(platform, 10, candidates, scores)
	if err != nil || selection == nil || selection.candidate.SellerID != "approved" {
		t.Fatalf("quota-gated low-price selection = %#v err=%v", selection, err)
	}
}

func TestSortSupplierQuotaScoresShowsCheapestAvailableFirstWithinStatus(t *testing.T) {
	scores := []SupplierQuotaScore{
		{SellerID: "high-score-no-stock", SellerName: "High Score No Stock", Status: supplierQuotaStatusApproved, ScoreM: 150},
		{SellerID: "higher-price", SellerName: "Higher Price", Status: supplierQuotaStatusApproved, ScoreM: 140, Available: 10, MinUnitPriceFen: 2300},
		{SellerID: "lower-price", SellerName: "Lower Price", Status: supplierQuotaStatusApproved, ScoreM: 95, Available: 2, MinUnitPriceFen: 1900},
		{SellerID: "trial", SellerName: "Trial", Status: supplierQuotaStatusUntried, Available: 20, MinUnitPriceFen: 1000},
	}

	sortSupplierQuotaScores(scores)

	if got := []string{scores[0].SellerID, scores[1].SellerID, scores[2].SellerID, scores[3].SellerID}; got[0] != "lower-price" || got[1] != "higher-price" || got[2] != "high-score-no-stock" || got[3] != "trial" {
		t.Fatalf("seller score order = %#v", got)
	}
}

func TestMarketplaceSupplierQuotaScoresUsesIndependentAccountEvidence(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t, testutil.NewConfig(t))
	service := New(st, nil)
	now := time.Now()

	seedMarketplaceQuotaAccount(t, st, "good-order", "good-seller", "Good Seller", "good.json")
	seedMarketplaceQuotaAccount(t, st, "low-order", "low-seller", "Low Seller", "low.json")
	service.quotaSnapshot = inspectionQuotaSnapshot{
		results: []store.CodexInspectionResult{
			{FileName: "good.json", AccountKey: "good", Provider: "codex"},
			{FileName: "low.json", AccountKey: "low", Provider: "codex"},
		},
		generatedAt: now,
		attemptedAt: now,
	}
	service.smartQuotaState.directSamples["file:good.json"] = smartQuotaCalibrationSample{
		identity: "file:good.json", capacityM: 60, weight: 1, usedFraction: 0.2,
		observedMS: now.UnixMilli(), completeWindow: true,
	}
	service.smartQuotaState.directSamples["file:low.json"] = smartQuotaCalibrationSample{
		identity: "file:low.json", capacityM: 12, weight: 1, usedFraction: 0.2,
		observedMS: now.UnixMilli(), completeWindow: true,
	}

	enabled := true
	platform := store.ManagerSupplyPlatformConfig{
		ID: "nv", Name: "NV", Type: "nvtokens", Product: "plus",
		SupplierQuotaGateEnabled: &enabled, SupplierQuotaMinimumM: 30,
	}
	candidates := []supplyclient.MarketplaceSellerCandidate{
		{SellerID: "good-seller", Name: "Good Seller", SelectionToken: "good-token", Product: "plus", Available: 10},
		{SellerID: "low-seller", Name: "Low Seller", SelectionToken: "low-token", Product: "plus", Available: 10},
		{SellerID: "new-seller", Name: "New Seller", SelectionToken: "new-token", Product: "plus", Available: 10},
		{SellerID: "known-remote", Name: "Known Remote", SelectionToken: "known-token", Product: "plus", Available: 10, PurchasedBefore: true},
	}
	scores, err := service.marketplaceSupplierQuotaScores(ctx, platform, candidates, nil)
	if err != nil {
		t.Fatalf("score sellers: %v", err)
	}
	bySeller := make(map[string]SupplierQuotaScore, len(scores))
	for _, score := range scores {
		bySeller[score.SellerID] = score
	}
	if score := bySeller["good-seller"]; score.Status != supplierQuotaStatusApproved || score.ScoreM != 60 || score.SampleCount != 1 {
		t.Fatalf("good score = %#v", score)
	}
	if score := bySeller["low-seller"]; score.Status != supplierQuotaStatusBlocked || score.ScoreM != 12 || score.SampleCount != 1 {
		t.Fatalf("low score = %#v", score)
	}
	if score := bySeller["new-seller"]; score.Status != supplierQuotaStatusUntried {
		t.Fatalf("new score = %#v", score)
	}
	if score := bySeller["known-remote"]; score.Status != supplierQuotaStatusObserving {
		t.Fatalf("known remote score = %#v", score)
	}
}

func TestMarketplaceSupplierQuotaScoresBlocksDuplicateInFlightTrial(t *testing.T) {
	st := testutil.NewStore(t, testutil.NewConfig(t))
	service := New(st, nil)
	enabled := true
	platform := store.ManagerSupplyPlatformConfig{
		ID: "nv", Type: "nvtokens", Product: "plus",
		SupplierQuotaGateEnabled: &enabled, SupplierQuotaMinimumM: 30,
	}
	candidate := supplyclient.MarketplaceSellerCandidate{
		SellerID: "trial-seller", SelectionToken: "trial-token", Product: "plus", Available: 5,
	}
	scores, err := service.marketplaceSupplierQuotaScores(context.Background(), platform, []supplyclient.MarketplaceSellerCandidate{candidate}, []store.SupplyOrder{{
		SupplierID: "nv", Product: "plus", MarketplaceSellerID: "trial-seller", Status: "creating",
	}})
	if err != nil || len(scores) != 1 || scores[0].Status != supplierQuotaStatusObserving || !scores[0].InFlightTrial {
		t.Fatalf("in-flight scores = %#v err=%v", scores, err)
	}
}

func TestMarketplaceSupplierQuotaScoreCacheMergesFreshInventoryAndOpenOrders(t *testing.T) {
	service := New(nil, nil)
	enabled := true
	platform := store.ManagerSupplyPlatformConfig{
		ID: "nv", Type: "nvtokens", Product: "plus",
		SupplierQuotaGateEnabled: &enabled, SupplierQuotaMinimumM: 30,
	}
	now := time.Now()
	service.setCachedMarketplaceSupplierQuotaScores(supplierQuotaScoreCacheKey(platform), []SupplierQuotaScore{{
		PlatformID: "nv", SellerID: "approved", SellerName: "Old Name", Product: "plus",
		Status: supplierQuotaStatusApproved, Reason: "observed_quota_meets_threshold", ScoreM: 60, SampleCount: 2,
		Available: 99, MinUnitPriceFen: 1,
	}, {
		PlatformID: "nv", SellerID: "trial", Product: "plus",
		Status: supplierQuotaStatusUntried, Reason: "eligible_for_single_trial",
	}}, now)

	merged := service.cachedMarketplaceSupplierQuotaScores(supplierQuotaScoreCacheKey(platform), now.Add(time.Second))
	merged = mergeMarketplaceSupplierQuotaScores(merged, platform, []supplyclient.MarketplaceSellerCandidate{
		{SellerID: "approved", Name: "Fresh Name", SelectionToken: "approved-token", Available: 4, MinUnitPriceFen: 1200},
		{SellerID: "trial", Name: "Trial", SelectionToken: "trial-token", Available: 2, MinUnitPriceFen: 900},
	}, []store.SupplyOrder{{
		SupplierID: "nv", Product: "plus", MarketplaceSellerID: "trial", Status: "creating",
	}}, now.Add(time.Second))
	bySeller := make(map[string]SupplierQuotaScore, len(merged))
	for _, score := range merged {
		bySeller[score.SellerID] = score
	}
	if score := bySeller["approved"]; score.Status != supplierQuotaStatusApproved || score.SellerName != "Fresh Name" || score.Available != 4 || score.MinUnitPriceFen != 1200 || score.ScoreM != 60 {
		t.Fatalf("cached approved score = %#v", score)
	}
	if score := bySeller["trial"]; score.Status != supplierQuotaStatusObserving || !score.InFlightTrial {
		t.Fatalf("cached in-flight trial score = %#v", score)
	}
}

func seedMarketplaceQuotaAccount(
	t *testing.T,
	st *store.Store,
	orderID string,
	sellerID string,
	sellerName string,
	fileName string,
) {
	t.Helper()
	ctx := context.Background()
	order, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: orderID, SupplierID: "nv", Product: "plus", RequestedQuantity: 1,
		MarketplaceSellerID: sellerID, MarketplaceSellerName: sellerName,
		MarketplaceSelectionToken: sellerID + "-token",
		Status:                    "completed", RemoteStatus: "completed", ChargedFen: 100, ItemCount: 1, ImportedCount: 1,
	})
	if err != nil {
		t.Fatalf("create seller order: %v", err)
	}
	if _, err := st.InsertSupplyImportItems(ctx, order.OrderID, []store.SupplyImportItem{{
		ItemKey: sellerID + "-item", FileName: fileName, PayloadJSON: `{}`,
		MarketplaceSellerID: sellerID, MarketplaceSellerName: sellerName,
		MarketplaceSelectionToken: sellerID + "-token",
	}}); err != nil {
		t.Fatalf("insert seller item: %v", err)
	}
	items, err := st.ListSupplyImportItemsByOrderIDs(ctx, []string{order.OrderID})
	if err != nil || len(items) != 1 {
		t.Fatalf("list seller item = %#v err=%v", items, err)
	}
	if err := st.MarkSupplyImportItemImported(ctx, items[0].ID, time.Now().UnixMilli()); err != nil {
		t.Fatalf("mark seller item imported: %v", err)
	}
}
