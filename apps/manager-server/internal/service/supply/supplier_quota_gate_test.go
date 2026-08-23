package supply

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/supplyclient"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/testutil"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/usage"
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
		{SellerID: "trial", Name: "Trial", SelectionToken: "trial-token", Available: 20, MinUnitPriceFen: 130},
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
	scores[1].RetryAfterMS = time.Now().Add(time.Minute).UnixMilli()
	selection, err = chooseMarketplaceSellerForAutomaticPurchase(platform, 10, candidates, scores)
	if !errors.Is(err, ErrSupplierQuotaGateNoEligibleSeller) || selection != nil {
		t.Fatalf("cooling selection = %#v err=%v", selection, err)
	}
}

func TestChooseMarketplaceSellerLetsCheaperUnknownSellerRunSingleTrial(t *testing.T) {
	enabled := true
	platform := store.ManagerSupplyPlatformConfig{
		Type:                       "nvtokens",
		SupplierQuotaGateEnabled:   &enabled,
		SupplierQuotaMinimumM:      90,
		SupplierQuotaTrialQuantity: 1,
	}
	candidates := []supplyclient.MarketplaceSellerCandidate{
		{SellerID: "approved", Name: "Approved", SelectionToken: "approved-token", Available: 20, MinUnitPriceFen: 2300},
		{SellerID: "new-cheap", Name: "New Cheap", SelectionToken: "new-cheap-token", Available: 20, MinUnitPriceFen: 1920},
	}
	scores := []SupplierQuotaScore{
		{SellerID: "approved", Status: supplierQuotaStatusApproved, ScoreM: 120},
		{SellerID: "new-cheap", Status: supplierQuotaStatusUntried},
	}

	selection, err := chooseMarketplaceSellerForAutomaticPurchase(platform, 10, candidates, scores)
	if err != nil || selection == nil || selection.candidate.SellerID != "new-cheap" || selection.quantity != 1 || !selection.trial {
		t.Fatalf("low-price trial selection = %#v err=%v", selection, err)
	}
}

func TestChooseMarketplaceSellerRetriesCheapestObservingSellerAfterCooldown(t *testing.T) {
	enabled := true
	maxUnitPriceFen := int64(1400)
	platform := store.ManagerSupplyPlatformConfig{
		Type:                       "nvtokens",
		MaxUnitPriceFen:            &maxUnitPriceFen,
		SupplierQuotaGateEnabled:   &enabled,
		SupplierQuotaMinimumM:      90,
		SupplierQuotaTrialQuantity: 1,
	}
	candidates := []supplyclient.MarketplaceSellerCandidate{
		{SellerID: "observing-cheap", Name: "Observing Cheap", SelectionToken: "observing-token", Available: 20, MinUnitPriceFen: 1200},
		{SellerID: "approved", Name: "Approved", SelectionToken: "approved-token", Available: 20, MinUnitPriceFen: 1400},
	}
	scores := []SupplierQuotaScore{
		{SellerID: "observing-cheap", Status: supplierQuotaStatusObserving, RetryAfterMS: time.Now().Add(-time.Second).UnixMilli()},
		{SellerID: "approved", Status: supplierQuotaStatusApproved, ScoreM: 120},
	}

	selection, err := chooseMarketplaceSellerForAutomaticPurchase(platform, 10, candidates, scores)
	if err != nil || selection == nil || selection.candidate.SellerID != "observing-cheap" || selection.quantity != 1 || !selection.trial {
		t.Fatalf("observing low-price trial selection = %#v err=%v", selection, err)
	}
}

func TestChooseMarketplaceSellerSkipsPricesAbovePlatformCeiling(t *testing.T) {
	enabled := true
	maxUnitPriceFen := int64(1400)
	platform := store.ManagerSupplyPlatformConfig{
		Type:                     "nvtokens",
		MaxUnitPriceFen:          &maxUnitPriceFen,
		SupplierQuotaGateEnabled: &enabled,
		SupplierQuotaMinimumM:    90,
	}
	candidates := []supplyclient.MarketplaceSellerCandidate{
		{SellerID: "over-limit", Name: "Over Limit", SelectionToken: "over-token", Available: 20, MinUnitPriceFen: 1800},
	}
	scores := []SupplierQuotaScore{{SellerID: "over-limit", Status: supplierQuotaStatusApproved, ScoreM: 120}}

	selection, err := chooseMarketplaceSellerForAutomaticPurchase(platform, 10, candidates, scores)
	priceWait, ok := marketplacePriceWaitError(err)
	if !ok || priceWait.MinimumUnitPriceFen != 1800 || priceWait.CeilingFen != 1400 || selection != nil {
		t.Fatalf("over-limit selection = %#v err=%v", selection, err)
	}
}

func TestChooseMarketplaceSellerPrefersApprovedAtEqualPrice(t *testing.T) {
	enabled := true
	platform := store.ManagerSupplyPlatformConfig{
		Type:                       "nvtokens",
		SupplierQuotaGateEnabled:   &enabled,
		SupplierQuotaMinimumM:      90,
		SupplierQuotaTrialQuantity: 1,
	}
	candidates := []supplyclient.MarketplaceSellerCandidate{
		{SellerID: "new", Name: "New", SelectionToken: "new-token", Available: 20, MinUnitPriceFen: 1900},
		{SellerID: "approved", Name: "Approved", SelectionToken: "approved-token", Available: 20, MinUnitPriceFen: 1900},
	}
	scores := []SupplierQuotaScore{
		{SellerID: "new", Status: supplierQuotaStatusUntried},
		{SellerID: "approved", Status: supplierQuotaStatusApproved, ScoreM: 95},
	}

	selection, err := chooseMarketplaceSellerForAutomaticPurchase(platform, 10, candidates, scores)
	if err != nil || selection == nil || selection.candidate.SellerID != "approved" || selection.quantity != 10 || selection.trial {
		t.Fatalf("equal-price approved selection = %#v err=%v", selection, err)
	}
}

func TestChooseMarketplaceSellerPrefersLowestCostPerCapacityAmongApproved(t *testing.T) {
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
		{SellerID: "higher-quota", Status: supplierQuotaStatusApproved, ScoreM: 170},
		{SellerID: "lower-price", Status: supplierQuotaStatusApproved, ScoreM: 95},
	}

	selection, err := chooseMarketplaceSellerForAutomaticPurchase(platform, 10, candidates, scores)
	if err != nil || selection == nil || selection.candidate.SellerID != "higher-quota" || selection.quantity != 10 || selection.trial {
		t.Fatalf("cost-per-capacity approved selection = %#v err=%v", selection, err)
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

func TestChooseMarketplaceSellerWaitsForImportedAccountCapacityEvidence(t *testing.T) {
	enabled := true
	platform := store.ManagerSupplyPlatformConfig{
		Type: "nvtokens", SupplierQuotaGateEnabled: &enabled,
		SupplierQuotaMinimumM: 90, SupplierQuotaTrialQuantity: 1,
	}
	candidates := []supplyclient.MarketplaceSellerCandidate{
		{SellerID: "pending-seller", SelectionToken: "pending-token", Available: 20, MinUnitPriceFen: 1900},
		{SellerID: "other-seller", SelectionToken: "other-token", Available: 20, MinUnitPriceFen: 2200},
	}
	scores := []SupplierQuotaScore{
		{
			SellerID: "pending-seller", Status: supplierQuotaStatusObserving,
			Reason: "waiting_for_account_quota_evidence", ImportedAccounts: 1,
		},
		{SellerID: "other-seller", Status: supplierQuotaStatusApproved, ScoreM: 150},
	}

	selection, err := chooseMarketplaceSellerForAutomaticPurchase(platform, 10, candidates, scores)
	if err != nil || selection == nil || selection.candidate.SellerID != "other-seller" || selection.trial {
		t.Fatalf("pending evidence seller should not be retried: selection=%#v err=%v", selection, err)
	}
}

func TestSortSupplierQuotaScoresShowsLowestCostPerCapacityFirstWithinStatus(t *testing.T) {
	scores := []SupplierQuotaScore{
		{SellerID: "high-score-no-stock", SellerName: "High Score No Stock", Status: supplierQuotaStatusApproved, ScoreM: 150},
		{SellerID: "higher-price", SellerName: "Higher Price", Status: supplierQuotaStatusApproved, ScoreM: 140, Available: 10, MinUnitPriceFen: 2300},
		{SellerID: "lower-price", SellerName: "Lower Price", Status: supplierQuotaStatusApproved, ScoreM: 95, Available: 2, MinUnitPriceFen: 1900},
		{SellerID: "trial", SellerName: "Trial", Status: supplierQuotaStatusUntried, Available: 20, MinUnitPriceFen: 1000},
	}

	sortSupplierQuotaScores(scores)

	if got := []string{scores[0].SellerID, scores[1].SellerID, scores[2].SellerID, scores[3].SellerID}; got[0] != "higher-price" || got[1] != "lower-price" || got[2] != "high-score-no-stock" || got[3] != "trial" {
		t.Fatalf("seller score order = %#v", got)
	}
}

func TestChooseMarketplaceSellerUsesPriceFallbackForUnknownCapacity(t *testing.T) {
	enabled := true
	platform := store.ManagerSupplyPlatformConfig{
		Type: "nvtokens", SupplierQuotaGateEnabled: &enabled,
		SupplierQuotaMinimumM: 90, SupplierQuotaTrialQuantity: 1,
	}
	candidates := []supplyclient.MarketplaceSellerCandidate{
		{SellerID: "sampled-value", SelectionToken: "sampled-token", Available: 20, MinUnitPriceFen: 2300},
		{SellerID: "unknown-expensive", SelectionToken: "unknown-token", Available: 20, MinUnitPriceFen: 2400},
	}
	scores := []SupplierQuotaScore{
		{SellerID: "sampled-value", Status: supplierQuotaStatusApproved, ScoreM: 170},
		{SellerID: "unknown-expensive", Status: supplierQuotaStatusUntried},
	}

	selection, err := chooseMarketplaceSellerForAutomaticPurchase(platform, 10, candidates, scores)
	if err != nil || selection == nil || selection.candidate.SellerID != "sampled-value" || selection.trial {
		t.Fatalf("unknown-capacity fallback selection = %#v err=%v", selection, err)
	}
}

func TestRecentSupplierQuotaSamplesKeepLatestTwenty(t *testing.T) {
	samples := make([]supplierQuotaAccountSample, 0, 25)
	for index := 1; index <= 25; index++ {
		samples = append(samples, supplierQuotaAccountSample{
			itemID: int64(index), observedAtMS: int64(index), capacityM: float64(index),
		})
	}

	recent := recentSupplierQuotaAccountSamples(samples, supplierQuotaRecentSampleLimit)
	if len(recent) != 20 || recent[0].capacityM != 25 || recent[19].capacityM != 6 {
		t.Fatalf("recent samples = %#v", recent)
	}
	capacities := make([]float64, 0, len(recent))
	for _, sample := range recent {
		capacities = append(capacities, sample.capacityM)
	}
	sort.Float64s(capacities)
	if got := trimmedSupplierQuotaCapacityMean(capacities); got != 15.5 {
		t.Fatalf("trimmed recent mean = %.2f, want 15.5", got)
	}
}

func TestRecentSupplierQuotaSamplesUseImportChronologyNotMetadataUpdates(t *testing.T) {
	items := []struct {
		id         int64
		importedAt int64
		createdAt  int64
		updatedAt  int64
	}{
		{id: 1, importedAt: 100, createdAt: 90, updatedAt: 9_999},
		{id: 2, importedAt: 200, createdAt: 190, updatedAt: 300},
	}
	samples := make([]supplierQuotaAccountSample, 0, len(items))
	for _, item := range items {
		samples = append(samples, supplierQuotaAccountSample{
			itemID: item.id,
			// This mirrors marketplaceSupplierQuotaScores: UpdatedAtMS is
			// deliberately excluded from the sample ordering key.
			observedAtMS: supplierQuotaSampleOrderMS(item.importedAt, item.createdAt),
			capacityM:    float64(item.id),
		})
	}
	recent := recentSupplierQuotaAccountSamples(samples, 1)
	if len(recent) != 1 || recent[0].itemID != 2 {
		t.Fatalf("import chronology sample = %#v", recent)
	}
}

func TestTrimmedSupplierQuotaCapacityMeanUsesAllSamplesBelowThree(t *testing.T) {
	if got := trimmedSupplierQuotaCapacityMean([]float64{100}); got != 100 {
		t.Fatalf("single-sample mean = %.2f", got)
	}
	if got := trimmedSupplierQuotaCapacityMean([]float64{100, 200}); got != 150 {
		t.Fatalf("two-sample mean = %.2f", got)
	}
	if got := trimmedSupplierQuotaCapacityMean([]float64{10, 100, 170, 300}); got != 135 {
		t.Fatalf("trimmed mean = %.2f, want 135", got)
	}
}

func TestSupplierCostPerCapacityFen(t *testing.T) {
	got, ok := supplierCostPerCapacityFen(1800, 170)
	if !ok || got != 10.5882 {
		t.Fatalf("cost per capacity = %.4f ok=%v", got, ok)
	}
}

func TestSupplierCostMultiplierUsesYuanPerMillion(t *testing.T) {
	got, ok := supplierCostMultiplier(1800, 170)
	if !ok || got != 0.105882 {
		t.Fatalf("cost multiplier = %.6f ok=%v, want 0.105882", got, ok)
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
	if score := bySeller["low-seller"]; score.Status != supplierQuotaStatusObserving ||
		score.Reason != "waiting_for_more_supplier_evidence" || score.ScoreM != 12 ||
		score.SampleCount != 1 || score.EvidenceCount != 1 || score.PassRatePercent != 0 {
		t.Fatalf("low score = %#v", score)
	}
	if score := bySeller["new-seller"]; score.Status != supplierQuotaStatusUntried {
		t.Fatalf("new score = %#v", score)
	}
	if score := bySeller["known-remote"]; score.Status != supplierQuotaStatusObserving {
		t.Fatalf("known remote score = %#v", score)
	}
}

func TestMarketplaceSupplierQuotaScoresUsesFivePercentEstimateBeforeInspectionAndUpdatesAtExhaustion(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t, testutil.NewConfig(t))
	service := New(st, nil)
	now := time.Now().Truncate(time.Second)
	recoverAt := now.Add(7 * 24 * time.Hour).UnixMilli()
	seedMarketplaceQuotaAccount(t, st, "fast-order", "fast-seller", "Fast Seller", "fast.json")

	service.recordSmartUsageEvents([]usage.Event{
		{
			TimestampMS: now.Add(-2 * time.Minute).UnixMilli(), Provider: "codex", AuthFileSnapshot: "fast.json",
			HeaderQuotaUsedPercent: floatPtr(0), HeaderQuotaRecoverAtMS: recoverAt,
			HeaderQuotaPlanType: "plus", ResponseMetadata: smartQuotaWeeklyMetadata("plus", 0, recoverAt),
		},
		{
			TimestampMS: now.Add(-time.Minute).UnixMilli(), Provider: "codex", AuthFileSnapshot: "fast.json",
			TotalTokens: 6_000_000, HeaderQuotaUsedPercent: floatPtr(5), HeaderQuotaRecoverAtMS: recoverAt,
			HeaderQuotaPlanType: "plus", ResponseMetadata: smartQuotaWeeklyMetadata("plus", 5, recoverAt),
		},
	}, now)

	enabled := true
	platform := store.ManagerSupplyPlatformConfig{
		ID: "nv", Name: "NV", Type: "nvtokens", Product: "plus",
		SupplierQuotaGateEnabled: &enabled, SupplierQuotaMinimumM: 90,
	}
	candidate := supplyclient.MarketplaceSellerCandidate{
		SellerID: "fast-seller", Name: "Fast Seller", SelectionToken: "fast-token",
		Product: "plus", Available: 20, MinUnitPriceFen: 1200,
	}

	// No inspection snapshot is seeded. The imported filename and live quota
	// events must be enough to release a low-price seller provisionally.
	scores, err := service.marketplaceSupplierQuotaScores(ctx, platform, []supplyclient.MarketplaceSellerCandidate{candidate}, nil)
	if err != nil || len(scores) != 1 {
		t.Fatalf("5%% seller scores = %#v err=%v", scores, err)
	}
	if score := scores[0]; score.Status != supplierQuotaStatusApproved || score.ScoreM != 120 ||
		score.CostMultiplier != 0.1 || score.CostPerCapacityFen != 10 || score.SampleCount != 1 || score.EvidenceCount != 1 ||
		score.Reason != "provisional_quota_meets_threshold" {
		t.Fatalf("5%% seller score = %#v", score)
	}
	items, itemErr := st.ListSupplyImportItems(ctx, 10, "imported")
	if itemErr != nil || len(items) != 1 || items[0].QuotaCapacityM != 120 || items[0].QuotaCapacityComplete {
		t.Fatalf("persisted provisional capacity = %#v err=%v", items, itemErr)
	}
	restarted := New(st, nil)
	restartedScores, restartErr := restarted.marketplaceSupplierQuotaScores(ctx, platform, []supplyclient.MarketplaceSellerCandidate{candidate}, nil)
	if restartErr != nil || len(restartedScores) != 1 || restartedScores[0].ScoreM != 120 || restartedScores[0].CostMultiplier != 0.1 {
		t.Fatalf("persisted capacity after manager restart = %#v err=%v", restartedScores, restartErr)
	}

	// Finishing the same account revises its existing evidence from 120M to 80M.
	// The seller therefore returns to bounded observation without gaining a
	// second sample from the same credential.
	service.recordSmartUsageEvents([]usage.Event{{
		TimestampMS: now.UnixMilli(), Provider: "codex", AuthFileSnapshot: "fast.json",
		TotalTokens: 74_000_000, HeaderQuotaUsedPercent: floatPtr(100), HeaderQuotaRecoverAtMS: recoverAt,
		HeaderQuotaPlanType: "plus", ResponseMetadata: smartQuotaWeeklyMetadata("plus", 100, recoverAt),
	}}, now)
	scores, err = service.marketplaceSupplierQuotaScores(ctx, platform, []supplyclient.MarketplaceSellerCandidate{candidate}, nil)
	if err != nil || len(scores) != 1 {
		t.Fatalf("exhausted seller scores = %#v err=%v", scores, err)
	}
	if score := scores[0]; score.Status != supplierQuotaStatusObserving || score.ScoreM != 80 ||
		score.CostMultiplier != 0.15 || score.CostPerCapacityFen != 15 || score.SampleCount != 1 || score.EvidenceCount != 1 ||
		score.Reason != "waiting_for_more_supplier_evidence" {
		t.Fatalf("exhausted seller score did not replace its provisional sample: %#v", score)
	}
	items, itemErr = st.ListSupplyImportItems(ctx, 10, "imported")
	if itemErr != nil || len(items) != 1 || items[0].QuotaCapacityM != 80 || !items[0].QuotaCapacityComplete {
		t.Fatalf("persisted final capacity = %#v err=%v", items, itemErr)
	}
}

func TestMarketplaceSupplierQuotaScoresRecordsInvalidCredentialWithoutBlocking(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t, testutil.NewConfig(t))
	service := New(st, nil)
	now := time.Now()

	seedMarketplaceQuotaAccount(t, st, "revoked-order", "revoked-seller", "Revoked Seller", "revoked.json")
	statusUnauthorized := 401
	service.quotaSnapshot = inspectionQuotaSnapshot{
		results: []store.CodexInspectionResult{{
			FileName: "revoked.json", AccountKey: "revoked", Provider: "codex",
			Action: "reauth", StatusCode: &statusUnauthorized, ErrorKind: "http_status",
			ErrorDetail: `{"error":{"code":"token_revoked"}}`,
		}},
		generatedAt: now,
		attemptedAt: now,
	}

	enabled := true
	platform := store.ManagerSupplyPlatformConfig{
		ID: "nv", Name: "NV", Type: "nvtokens", Product: "plus",
		SupplierQuotaGateEnabled: &enabled, SupplierQuotaMinimumM: 30,
	}
	candidates := []supplyclient.MarketplaceSellerCandidate{{
		SellerID: "revoked-seller", Name: "Revoked Seller", SelectionToken: "revoked-token",
		Product: "plus", Available: 10, MinUnitPriceFen: 900,
	}}

	scores, err := service.marketplaceSupplierQuotaScores(ctx, platform, candidates, nil)
	if err != nil || len(scores) != 1 {
		t.Fatalf("invalid credential scores = %#v err=%v", scores, err)
	}
	score := scores[0]
	if score.Status != supplierQuotaStatusObserving || score.Reason != "waiting_for_account_quota_evidence" ||
		score.InvalidCredentialCount != 1 || score.ImportedAccounts != 1 || score.SampleCount != 0 ||
		score.EvidenceCount != 0 || score.PassRatePercent != 0 {
		t.Fatalf("invalid credential score = %#v", score)
	}
	selection, selectErr := chooseMarketplaceSellerForAutomaticPurchase(platform, 10, candidates, scores)
	if selectErr != nil || selection == nil || selection.candidate.SellerID != "revoked-seller" ||
		selection.quantity != 1 || !selection.trial {
		t.Fatalf("invalid credential seller follow-up trial = %#v err=%v", selection, selectErr)
	}
}

func TestMarketplaceSupplierQuotaScoresIgnoresInvalidCredentialsForCapacityBlocking(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t, testutil.NewConfig(t))
	service := New(st, nil)
	now := time.Now()
	results := make([]store.CodexInspectionResult, 0, 10)
	for index := 0; index < 10; index++ {
		fileName := fmt.Sprintf("decision-%02d.json", index)
		seedMarketplaceQuotaAccount(t, st, fmt.Sprintf("decision-order-%02d", index), "decision-seller", "Decision Seller", fileName)
		result := store.CodexInspectionResult{FileName: fileName, AccountKey: fileName, Provider: "codex"}
		if index < 3 {
			statusUnauthorized := 401
			result.Action = "reauth"
			result.StatusCode = &statusUnauthorized
			result.ErrorDetail = `{"error":{"code":"token_revoked"}}`
		} else if index < 9 {
			service.smartQuotaState.directSamples["file:"+fileName] = smartQuotaCalibrationSample{
				identity: "file:" + fileName, capacityM: 120, weight: 1, usedFraction: 0.2,
				observedMS: now.UnixMilli(), completeWindow: true,
			}
		}
		results = append(results, result)
	}
	service.quotaSnapshot = inspectionQuotaSnapshot{results: results[:9], generatedAt: now, attemptedAt: now}

	enabled := true
	platform := store.ManagerSupplyPlatformConfig{
		ID: "nv", Name: "NV", Type: "nvtokens", Product: "plus",
		SupplierQuotaGateEnabled: &enabled, SupplierQuotaMinimumM: 90,
	}
	candidate := supplyclient.MarketplaceSellerCandidate{
		SellerID: "decision-seller", Name: "Decision Seller", SelectionToken: "decision-token",
		Product: "plus", Available: 20, MinUnitPriceFen: 1200,
	}
	scores, err := service.marketplaceSupplierQuotaScores(ctx, platform, []supplyclient.MarketplaceSellerCandidate{candidate}, nil)
	if err != nil || len(scores) != 1 {
		t.Fatalf("nine-evidence scores = %#v err=%v", scores, err)
	}
	if score := scores[0]; score.Status != supplierQuotaStatusApproved || score.EvidenceCount != 6 || score.InvalidCredentialCount != 3 || score.PassRatePercent != 100 {
		t.Fatalf("invalid credentials must not block capacity-approved seller: %#v", score)
	}
	selection, selectErr := chooseMarketplaceSellerForAutomaticPurchase(platform, 5, []supplyclient.MarketplaceSellerCandidate{candidate}, scores)
	if selectErr != nil || selection == nil || selection.trial || selection.quantity != 5 {
		t.Fatalf("capacity-approved selection = %#v err=%v", selection, selectErr)
	}

	service.supplierQuotaScores = nil
	service.smartQuotaState.directSamples["file:decision-09.json"] = smartQuotaCalibrationSample{
		identity: "file:decision-09.json", capacityM: 120, weight: 1, usedFraction: 0.2,
		observedMS: now.UnixMilli(), completeWindow: true,
	}
	service.quotaSnapshot = inspectionQuotaSnapshot{results: results, generatedAt: now, attemptedAt: now}
	scores, err = service.marketplaceSupplierQuotaScores(ctx, platform, []supplyclient.MarketplaceSellerCandidate{candidate}, nil)
	if err != nil || len(scores) != 1 {
		t.Fatalf("ten-evidence scores = %#v err=%v", scores, err)
	}
	if score := scores[0]; score.Status != supplierQuotaStatusApproved || score.EvidenceCount != 7 ||
		score.PassingSampleCount != 7 || score.InvalidCredentialCount != 3 || score.PassRatePercent != 100 {
		t.Fatalf("invalid credentials must remain display-only once capacity is known: %#v", score)
	}
}

func TestMarketplaceSupplierQuotaScoresKeepsInvalidCredentialsOutOfCapacityEvidence(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t, testutil.NewConfig(t))
	service := New(st, nil)
	now := time.Now()
	results := make([]store.CodexInspectionResult, 0, 10)
	for index := 0; index < 10; index++ {
		fileName := fmt.Sprintf("mixed-%02d.json", index)
		seedMarketplaceQuotaAccount(t, st, fmt.Sprintf("mixed-order-%02d", index), "mixed-seller", "Mixed Seller", fileName)
		result := store.CodexInspectionResult{FileName: fileName, AccountKey: fileName, Provider: "codex"}
		if index < 2 {
			statusUnauthorized := 401
			result.Action = "reauth"
			result.StatusCode = &statusUnauthorized
			result.ErrorDetail = `{"error":{"code":"token_revoked"}}`
		} else {
			service.smartQuotaState.directSamples["file:"+fileName] = smartQuotaCalibrationSample{
				identity: "file:" + fileName, capacityM: 120, weight: 1, usedFraction: 0.2,
				observedMS: now.UnixMilli(), completeWindow: true,
			}
		}
		results = append(results, result)
	}
	service.quotaSnapshot = inspectionQuotaSnapshot{results: results, generatedAt: now, attemptedAt: now}

	enabled := true
	platform := store.ManagerSupplyPlatformConfig{
		ID: "nv", Name: "NV", Type: "nvtokens", Product: "plus",
		SupplierQuotaGateEnabled: &enabled, SupplierQuotaMinimumM: 90,
	}
	candidate := supplyclient.MarketplaceSellerCandidate{
		SellerID: "mixed-seller", Name: "Mixed Seller", SelectionToken: "mixed-token",
		Product: "plus", Available: 20, MinUnitPriceFen: 1200,
	}
	scores, err := service.marketplaceSupplierQuotaScores(ctx, platform, []supplyclient.MarketplaceSellerCandidate{candidate}, nil)
	if err != nil || len(scores) != 1 {
		t.Fatalf("mixed seller scores = %#v err=%v", scores, err)
	}
	score := scores[0]
	if score.Status != supplierQuotaStatusApproved || score.SampleCount != 8 || score.EvidenceCount != 8 ||
		score.PassingSampleCount != 8 || score.InvalidCredentialCount != 2 || score.PassRatePercent != 100 {
		t.Fatalf("invalid credentials must not dilute capacity pass rate: %#v", score)
	}
	selection, selectErr := chooseMarketplaceSellerForAutomaticPurchase(platform, 5, []supplyclient.MarketplaceSellerCandidate{candidate}, scores)
	if selectErr != nil || selection == nil || selection.candidate.SellerID != "mixed-seller" || selection.trial {
		t.Fatalf("mixed seller selection = %#v err=%v", selection, selectErr)
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

func TestMarketplaceSupplierQuotaScoresCoolsDownFailedTrialAttempt(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t, testutil.NewConfig(t))
	service := New(st, nil)
	enabled := true
	platform := store.ManagerSupplyPlatformConfig{
		ID: "nv", Type: "nvtokens", Product: "plus",
		SupplierQuotaGateEnabled: &enabled, SupplierQuotaMinimumM: 30,
	}
	candidate := supplyclient.MarketplaceSellerCandidate{
		SellerID: "failed-trial", SelectionToken: "failed-token", Product: "plus", Available: 5,
	}
	createdAtMS := time.Now().Add(-time.Minute).UnixMilli()
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID: "failed-trial-order", SupplierID: "nv", Product: "plus", RequestedQuantity: 1,
		MarketplaceSellerID: "failed-trial", MarketplaceSelectionToken: "failed-token",
		Status: "failed", RemoteStatus: "failed", CreatedAtMS: createdAtMS, UpdatedAtMS: createdAtMS,
	}); err != nil {
		t.Fatalf("create failed trial: %v", err)
	}

	scores, err := service.marketplaceSupplierQuotaScores(ctx, platform, []supplyclient.MarketplaceSellerCandidate{candidate}, nil)
	if err != nil || len(scores) != 1 {
		t.Fatalf("failed trial scores = %#v err=%v", scores, err)
	}
	score := scores[0]
	if score.Status != supplierQuotaStatusObserving || score.AttemptCount != 1 || score.LastAttemptAtMS != createdAtMS || score.RetryAfterMS <= time.Now().UnixMilli() {
		t.Fatalf("failed trial cooldown score = %#v", score)
	}
	selection, selectErr := chooseMarketplaceSellerForAutomaticPurchase(platform, 5, []supplyclient.MarketplaceSellerCandidate{candidate}, scores)
	if !errors.Is(selectErr, ErrSupplierQuotaGateNoEligibleSeller) || selection != nil {
		t.Fatalf("failed trial selected during cooldown = %#v err=%v", selection, selectErr)
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
		ItemKey: orderID + "-item", FileName: fileName, PayloadJSON: `{}`,
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
