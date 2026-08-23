package supply

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	managerconfigsvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/supplyclient"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

var ErrSupplierQuotaGateNoEligibleSeller = errors.New("no marketplace seller currently passes the automatic quota gate")

// MarketplacePriceWaitError is a normal automatic-procurement wait state: the
// marketplace has stock, but every current seller quote is above the hard
// platform ceiling. It must not be reported as a supplier quota-gate failure.
type MarketplacePriceWaitError struct {
	MinimumUnitPriceFen int64
	CeilingFen          int64
	Available           int
}

func (e *MarketplacePriceWaitError) Error() string {
	if e == nil {
		return "marketplace price is above the automatic purchase ceiling"
	}
	return fmt.Sprintf(
		"marketplace minimum unit price %d exceeds automatic purchase ceiling %d",
		e.MinimumUnitPriceFen,
		e.CeilingFen,
	)
}

func marketplacePriceWaitError(err error) (*MarketplacePriceWaitError, bool) {
	var target *MarketplacePriceWaitError
	if !errors.As(err, &target) || target == nil {
		return nil, false
	}
	return target, true
}

const (
	maxSupplierQuotaScoringOrders  = 2000
	supplierQuotaRecentSampleLimit = 20
)

const (
	// Permanent supplier capacity decisions need a representative batch. Before
	// this point a weak account only keeps the seller in single-account
	// observation; one unlucky low-capacity delivery never blacklists the whole
	// seller. Revoked/invalid credentials are displayed for operators but do not
	// block a seller by themselves.
	supplierQuotaMinimumDecisionSamples = 10
)

// An observing seller is not permanently excluded. A bounded pause gives the
// inspection worker time to turn the last trial into quota evidence and also
// prevents a failed marketplace create from being retried every supply tick.
const supplierQuotaObservationRetryInterval = 10 * time.Minute

const (
	supplierQuotaStatusApproved  = "approved"
	supplierQuotaStatusBlocked   = "blocked"
	supplierQuotaStatusObserving = "observing"
	supplierQuotaStatusUntried   = "untried"
)

// SupplierQuotaScore is the operator-facing decision record for one concrete
// marketplace seller and sale plan. ScoreM is the trimmed mean of the seller's
// latest 20 independently observed account capacities: one minimum and one
// maximum are removed before averaging when at least three samples exist.
type SupplierQuotaScore struct {
	PlatformID             string  `json:"platformId"`
	PlatformName           string  `json:"platformName,omitempty"`
	SellerID               string  `json:"sellerId"`
	SellerName             string  `json:"sellerName,omitempty"`
	ChannelID              string  `json:"channelId,omitempty"`
	SelectionToken         string  `json:"selectionToken,omitempty"`
	Product                string  `json:"product"`
	Status                 string  `json:"status"`
	Reason                 string  `json:"reason"`
	ThresholdM             float64 `json:"thresholdM"`
	ScoreM                 float64 `json:"scoreM,omitempty"`
	MinimumObservedM       float64 `json:"minimumObservedM,omitempty"`
	MaximumObservedM       float64 `json:"maximumObservedM,omitempty"`
	SampleCount            int     `json:"sampleCount"`
	EvidenceCount          int     `json:"evidenceCount"`
	PassingSampleCount     int     `json:"passingSampleCount"`
	PassRatePercent        float64 `json:"passRatePercent,omitempty"`
	ImportedAccounts       int     `json:"importedAccounts"`
	InvalidCredentialCount int     `json:"invalidCredentialCount,omitempty"`
	AttemptCount           int     `json:"attemptCount,omitempty"`
	LastAttemptAtMS        int64   `json:"lastAttemptAtMs,omitempty"`
	RetryAfterMS           int64   `json:"retryAfterMs,omitempty"`
	InFlightTrial          bool    `json:"inFlightTrial,omitempty"`
	Available              int     `json:"available,omitempty"`
	MinUnitPriceFen        int64   `json:"minUnitPriceFen,omitempty"`
	MaxUnitPriceFen        int64   `json:"maxUnitPriceFen,omitempty"`
	// CostMultiplier is the normalized price/capacity ratio in yuan per 1M.
	// CostPerCapacityFen remains available for older panel clients.
	CostMultiplier        float64 `json:"costMultiplier,omitempty"`
	CostPerCapacityFen    float64 `json:"costPerCapacityFen,omitempty"`
	MarketplaceQuality    float64 `json:"marketplaceQuality,omitempty"`
	MarketplaceActiveRate float64 `json:"marketplaceActiveRate,omitempty"`
	CheckedAtMS           int64   `json:"checkedAtMs"`
}

type supplierQuotaScoreCacheEntry struct {
	scores    []SupplierQuotaScore
	generated time.Time
}

const supplierQuotaScoreCacheTTL = 15 * time.Second

type marketplaceSellerSelection struct {
	candidate supplyclient.MarketplaceSellerCandidate
	score     SupplierQuotaScore
	quantity  int
	trial     bool
}

func supplierQuotaGateEnabled(platform store.ManagerSupplyPlatformConfig) bool {
	return strings.EqualFold(strings.TrimSpace(platform.Type), managerconfigsvc.SupplyPlatformNvtokens) &&
		platform.SupplierQuotaGateEnabled != nil && *platform.SupplierQuotaGateEnabled
}

func supplierQuotaGateMinimumM(platform store.ManagerSupplyPlatformConfig) float64 {
	if platform.SupplierQuotaMinimumM >= 0.5 {
		return platform.SupplierQuotaMinimumM
	}
	return 30
}

func supplierQuotaTrialQuantity(platform store.ManagerSupplyPlatformConfig) int {
	if platform.SupplierQuotaTrialQuantity > 0 {
		return min(platform.SupplierQuotaTrialQuantity, 5)
	}
	return 1
}

func marketplaceSellerCredentials(
	platform store.ManagerSupplyPlatformConfig,
	selection *marketplaceSellerSelection,
) supplyclient.Credentials {
	credentials := supplyPlatformCredentials(platform)
	if selection == nil {
		return credentials
	}
	token := strings.TrimSpace(selection.candidate.SelectionToken)
	if token != "" {
		credentials.SellerWhitelist = []string{token}
		credentials.PreferredSellers = []string{token}
	}
	if channelID := strings.TrimSpace(selection.candidate.ChannelID); channelID != "" {
		credentials.PreferredChannelIDs = []string{channelID}
	}
	return credentials
}

func (s *Service) selectMarketplaceSellerForAutomaticPurchase(
	ctx context.Context,
	platform store.ManagerSupplyPlatformConfig,
	quantity int,
	openOrders []store.SupplyOrder,
) (*marketplaceSellerSelection, []SupplierQuotaScore, error) {
	if !supplierQuotaGateEnabled(platform) {
		return nil, nil, nil
	}
	candidates, err := s.supplyClient.MarketplaceSellerCandidates(
		ctx,
		supplyPlatformCredentials(platform),
		platform.Product,
	)
	if err != nil {
		return nil, nil, err
	}
	scores, err := s.marketplaceSupplierQuotaScores(ctx, platform, candidates, openOrders)
	if err != nil {
		return nil, nil, err
	}
	selection, selectErr := chooseMarketplaceSellerForAutomaticPurchase(platform, quantity, candidates, scores)
	return selection, scores, selectErr
}

func chooseMarketplaceSellerForAutomaticPurchase(
	platform store.ManagerSupplyPlatformConfig,
	quantity int,
	candidates []supplyclient.MarketplaceSellerCandidate,
	scores []SupplierQuotaScore,
) (*marketplaceSellerSelection, error) {
	byID := make(map[string]SupplierQuotaScore, len(scores))
	for _, score := range scores {
		byID[normalizeMarketplaceSellerID(score.SellerID)] = score
	}
	eligible := make([]marketplaceSellerSelection, 0)
	nowMS := time.Now().UnixMilli()
	ceiling := valueOrZero(platform.MaxUnitPriceFen)
	minimumAvailablePriceFen := int64(0)
	minimumAvailableAtPrice := 0
	hasPotentiallyPriceQualifiedStock := false
	for _, candidate := range candidates {
		if candidate.Available <= 0 || strings.TrimSpace(candidate.SelectionToken) == "" {
			continue
		}
		if candidate.MinUnitPriceFen > 0 &&
			(minimumAvailablePriceFen == 0 || candidate.MinUnitPriceFen < minimumAvailablePriceFen) {
			minimumAvailablePriceFen = candidate.MinUnitPriceFen
			minimumAvailableAtPrice = candidate.Available
		}
		if ceiling > 0 && candidate.MinUnitPriceFen > ceiling {
			continue
		}
		hasPotentiallyPriceQualifiedStock = true
		score, found := byID[normalizeMarketplaceSellerID(candidate.SellerID)]
		if !found {
			continue
		}
		// An imported account without a 5%/exhaustion observation is already
		// the seller's calibration trial. Keep waiting for that account's
		// capacity evidence instead of buying another account from the same
		// seller after the generic observation cooldown expires.
		if score.Status == supplierQuotaStatusObserving &&
			score.Reason == "waiting_for_account_quota_evidence" &&
			score.ImportedAccounts > score.SampleCount+score.InvalidCredentialCount {
			continue
		}
		selection := marketplaceSellerSelection{candidate: candidate, score: score, quantity: quantity}
		switch score.Status {
		case supplierQuotaStatusApproved:
			eligible = append(eligible, selection)
		case supplierQuotaStatusUntried:
			selection.trial = true
			selection.quantity = min(max(1, quantity), min(candidate.Available, supplierQuotaTrialQuantity(platform)))
			eligible = append(eligible, selection)
		case supplierQuotaStatusObserving:
			// Observing is a temporary evidence/cooldown state, not a permanent
			// blacklist. Once the bounded pause has elapsed, let the cheapest
			// non-blocked seller run another single-account trial.
			if score.InFlightTrial || score.RetryAfterMS > nowMS {
				continue
			}
			selection.trial = true
			selection.quantity = min(max(1, quantity), min(candidate.Available, supplierQuotaTrialQuantity(platform)))
			eligible = append(eligible, selection)
		}
	}
	// Blocked is the only permanent quota exclusion. Sellers with capacity
	// evidence are ranked by unit price / trimmed capacity mean. A seller without
	// a denominator can still win one bounded trial when its raw price is lower
	// than the best sampled seller's raw price.
	sortMarketplaceSellerSelections(eligible)
	if len(eligible) > 0 {
		return &eligible[0], nil
	}
	if ceiling > 0 && minimumAvailablePriceFen > ceiling && !hasPotentiallyPriceQualifiedStock {
		return nil, &MarketplacePriceWaitError{
			MinimumUnitPriceFen: minimumAvailablePriceFen,
			CeilingFen:          ceiling,
			Available:           minimumAvailableAtPrice,
		}
	}
	return nil, ErrSupplierQuotaGateNoEligibleSeller
}

func sortMarketplaceSellerSelections(values []marketplaceSellerSelection) {
	if len(values) < 2 {
		return
	}
	known := make([]marketplaceSellerSelection, 0, len(values))
	unknown := make([]marketplaceSellerSelection, 0, len(values))
	for _, value := range values {
		if _, ok := supplierCostMultiplier(marketplaceSellerSelectionPrice(value), value.score.ScoreM); ok {
			known = append(known, value)
		} else {
			unknown = append(unknown, value)
		}
	}
	sort.SliceStable(known, func(i, j int) bool {
		leftCost, _ := supplierCostMultiplier(marketplaceSellerSelectionPrice(known[i]), known[i].score.ScoreM)
		rightCost, _ := supplierCostMultiplier(marketplaceSellerSelectionPrice(known[j]), known[j].score.ScoreM)
		if leftCost != rightCost {
			return leftCost < rightCost
		}
		return marketplaceSellerSelectionTieLess(known[i], known[j])
	})
	sort.SliceStable(unknown, func(i, j int) bool {
		leftPrice := marketplaceSellerSelectionPrice(unknown[i])
		rightPrice := marketplaceSellerSelectionPrice(unknown[j])
		if leftPrice != rightPrice {
			return leftPrice < rightPrice
		}
		return marketplaceSellerSelectionTieLess(unknown[i], unknown[j])
	})

	ordered := make([]marketplaceSellerSelection, 0, len(values))
	if len(unknown) > 0 && (len(known) == 0 ||
		marketplaceSellerSelectionPrice(unknown[0]) < marketplaceSellerSelectionPrice(known[0])) {
		// Only the cheapest unknown seller is promoted ahead of sampled inventory.
		// The purchase quantity is already bounded to one account, after which the
		// seller becomes observing and cannot repeatedly bypass capacity scoring.
		ordered = append(ordered, unknown[0])
		ordered = append(ordered, known...)
		ordered = append(ordered, unknown[1:]...)
	} else {
		ordered = append(ordered, known...)
		ordered = append(ordered, unknown...)
	}
	copy(values, ordered)
}

func marketplaceSellerSelectionPrice(value marketplaceSellerSelection) int64 {
	if value.candidate.MinUnitPriceFen <= 0 {
		return math.MaxInt64
	}
	return value.candidate.MinUnitPriceFen
}

func marketplaceSellerSelectionTieLess(left marketplaceSellerSelection, right marketplaceSellerSelection) bool {
	if left.trial != right.trial {
		return !left.trial
	}
	if left.score.ScoreM != right.score.ScoreM {
		return left.score.ScoreM > right.score.ScoreM
	}
	leftQuality := valueOrNegativeInfinity(left.candidate.QualityScore)
	rightQuality := valueOrNegativeInfinity(right.candidate.QualityScore)
	if leftQuality != rightQuality {
		return leftQuality > rightQuality
	}
	leftActiveRate := valueOrNegativeInfinity(left.candidate.ActiveRatePercent)
	rightActiveRate := valueOrNegativeInfinity(right.candidate.ActiveRatePercent)
	if leftActiveRate != rightActiveRate {
		return leftActiveRate > rightActiveRate
	}
	if left.candidate.Available != right.candidate.Available {
		return left.candidate.Available > right.candidate.Available
	}
	leftID := normalizeMarketplaceSellerID(left.candidate.SellerID)
	rightID := normalizeMarketplaceSellerID(right.candidate.SellerID)
	if leftID != rightID {
		return leftID < rightID
	}
	return strings.ToLower(left.candidate.Name) < strings.ToLower(right.candidate.Name)
}

func valueOrNegativeInfinity(value *float64) float64 {
	if value == nil {
		return math.Inf(-1)
	}
	return *value
}

func (s *Service) marketplaceSupplierQuotaScores(
	ctx context.Context,
	platform store.ManagerSupplyPlatformConfig,
	candidates []supplyclient.MarketplaceSellerCandidate,
	openOrders []store.SupplyOrder,
) ([]SupplierQuotaScore, error) {
	now := time.Now()
	cacheKey := supplierQuotaScoreCacheKey(platform)
	if cached := s.cachedMarketplaceSupplierQuotaScores(cacheKey, now); len(cached) > 0 {
		return mergeMarketplaceSupplierQuotaScores(cached, platform, candidates, openOrders, now), nil
	}
	threshold := supplierQuotaGateMinimumM(platform)
	orders, err := s.store.ListMarketplaceSellerSupplyOrders(ctx, platform.ID, platform.Product)
	if err != nil {
		return nil, err
	}
	orderByID := make(map[string]store.SupplyOrder, len(orders))
	orderIDs := make([]string, 0, len(orders))
	for _, order := range orders {
		if len(orderIDs) >= maxSupplierQuotaScoringOrders {
			break
		}
		orderByID[order.OrderID] = order
		orderIDs = append(orderIDs, order.OrderID)
	}
	items, err := s.store.ListSupplyImportItemsByOrderIDs(ctx, orderIDs)
	if err != nil {
		return nil, err
	}
	snapshot, snapshotErr := s.cachedInspectionQuotaSnapshot(ctx, store.ManagerSupplyConfig{}, false)
	resultByFile := make(map[string]store.CodexInspectionResult)
	if snapshotErr == nil {
		for _, result := range snapshot.results {
			if fileName := strings.TrimSpace(result.FileName); fileName != "" {
				resultByFile[fileName] = result
			}
		}
	}
	type evidence struct {
		candidate      supplyclient.MarketplaceSellerCandidate
		accountSamples []supplierQuotaAccountSample
		imported       int
		invalid        int
		attempted      int
		purchased      int
		lastAttemptMS  int64
		inFlight       bool
		selectionToken string
		channelID      string
		sellerName     string
	}
	bySeller := make(map[string]*evidence)
	ensure := func(sellerID string) *evidence {
		key := normalizeMarketplaceSellerID(sellerID)
		if key == "" {
			return nil
		}
		entry := bySeller[key]
		if entry == nil {
			entry = &evidence{}
			bySeller[key] = entry
		}
		return entry
	}
	for _, candidate := range candidates {
		entry := ensure(candidate.SellerID)
		if entry == nil {
			continue
		}
		entry.candidate = candidate
		entry.selectionToken = candidate.SelectionToken
		entry.channelID = candidate.ChannelID
		entry.sellerName = candidate.Name
	}
	for _, order := range orders {
		entry := ensure(order.MarketplaceSellerID)
		if entry == nil {
			continue
		}
		entry.attempted++
		entry.lastAttemptMS = max(entry.lastAttemptMS, order.CreatedAtMS)
		if supplyOrderHasPaymentEvidence(order) || order.ImportedCount > 0 {
			entry.purchased++
		}
		entry.selectionToken = firstNonEmptyString(entry.selectionToken, order.MarketplaceSelectionToken)
		entry.channelID = firstNonEmptyString(entry.channelID, order.MarketplaceChannelID)
		entry.sellerName = firstNonEmptyString(entry.sellerName, order.MarketplaceSellerName)
	}
	for _, order := range openOrders {
		if !strings.EqualFold(strings.TrimSpace(order.SupplierID), strings.TrimSpace(platform.ID)) ||
			!sameSupplyProduct(order.Product, platform.Product) {
			continue
		}
		entry := ensure(order.MarketplaceSellerID)
		if entry != nil {
			entry.inFlight = true
		}
	}
	for _, item := range items {
		if !strings.EqualFold(strings.TrimSpace(item.Status), "imported") {
			continue
		}
		order := orderByID[item.OrderID]
		sellerID := firstNonEmptyString(item.MarketplaceSellerID, order.MarketplaceSellerID)
		entry := ensure(sellerID)
		if entry == nil {
			continue
		}
		entry.imported++
		entry.selectionToken = firstNonEmptyString(entry.selectionToken, item.MarketplaceSelectionToken, order.MarketplaceSelectionToken)
		entry.channelID = firstNonEmptyString(entry.channelID, item.MarketplaceChannelID, order.MarketplaceChannelID)
		entry.sellerName = firstNonEmptyString(entry.sellerName, item.MarketplaceSellerName, order.MarketplaceSellerName)
		// The filename is enough to join an imported account with live quota
		// headers. Do not wait for the next full inspection snapshot before using
		// the account's 5% supplier estimate; inspection-only aliases are appended
		// when available.
		identities := smartQuotaCalibrationResultIdentities(item.FileName, "", "", "")
		result, found := resultByFile[strings.TrimSpace(item.FileName)]
		if found {
			identities = append(identities, smartQuotaCalibrationResultIdentities(
				result.FileName,
				result.AuthIndex,
				result.AccountKey,
				result.AccountID,
			)...)
		}
		// A revoked/invalid credential is operator evidence only. It must not
		// participate in supplier blocking; the automatic gate is strictly a
		// capacity gate.
		// Sample recency follows account import chronology. Metadata repairs and
		// later quota observations must not make an old account displace a newer
		// account from the seller's latest-20 evidence window.
		sampleOrderMS := supplierQuotaSampleOrderMS(item.ImportedAtMS, item.CreatedAtMS)
		if found && inspectionResultCredentialInvalid(result) {
			entry.accountSamples = append(entry.accountSamples, supplierQuotaAccountSample{
				itemID: item.ID, observedAtMS: sampleOrderMS, invalid: true,
			})
			continue
		}
		estimate, ok := s.smartQuotaSupplierEstimateForAt(now, identities...)
		capacityM := item.QuotaCapacityM
		capacityObservedAtMS := item.QuotaCapacityObservedAtMS
		complete := item.QuotaCapacityComplete
		if ok && estimate.CapacityM > 0 && (capacityM <= 0 || (estimate.Source == smartQuotaEstimateSourceSupplierFinal && !complete)) {
			capacityM = estimate.CapacityM
			capacityObservedAtMS = sampleOrderMS
			if capacityObservedAtMS <= 0 {
				capacityObservedAtMS = now.UnixMilli()
			}
			complete = estimate.Source == smartQuotaEstimateSourceSupplierFinal
			if s.store != nil {
				_ = s.store.UpdateSupplyImportItemQuotaCapacity(ctx, item.ID, capacityM, capacityObservedAtMS, complete)
			}
		}
		if capacityM > 0 {
			entry.accountSamples = append(entry.accountSamples, supplierQuotaAccountSample{
				itemID:       item.ID,
				observedAtMS: sampleOrderMS,
				capacityM:    capacityM,
			})
		}
	}
	scores := make([]SupplierQuotaScore, 0, len(bySeller))
	for key, entry := range bySeller {
		candidate := entry.candidate
		score := SupplierQuotaScore{
			PlatformID:             platform.ID,
			PlatformName:           platform.Name,
			SellerID:               firstNonEmptyString(candidate.SellerID, key),
			SellerName:             entry.sellerName,
			ChannelID:              entry.channelID,
			SelectionToken:         entry.selectionToken,
			Product:                platform.Product,
			ThresholdM:             threshold,
			ImportedAccounts:       entry.imported,
			InvalidCredentialCount: entry.invalid,
			AttemptCount:           entry.attempted,
			LastAttemptAtMS:        entry.lastAttemptMS,
			InFlightTrial:          entry.inFlight,
			Available:              candidate.Available,
			MinUnitPriceFen:        candidate.MinUnitPriceFen,
			MaxUnitPriceFen:        candidate.MaxUnitPriceFen,
			CheckedAtMS:            now.UnixMilli(),
		}
		if candidate.QualityScore != nil {
			score.MarketplaceQuality = *candidate.QualityScore
		}
		if candidate.ActiveRatePercent != nil {
			score.MarketplaceActiveRate = *candidate.ActiveRatePercent
		}
		recentSamples := recentSupplierQuotaAccountSamples(entry.accountSamples, supplierQuotaRecentSampleLimit)
		capacities := make([]float64, 0, len(recentSamples))
		for _, sample := range recentSamples {
			if sample.invalid {
				entry.invalid++
				continue
			}
			if sample.capacityM > 0 {
				capacities = append(capacities, sample.capacityM)
			}
		}
		if len(capacities) > 0 {
			sort.Float64s(capacities)
			score.SampleCount = len(capacities)
			score.MinimumObservedM = round2(capacities[0])
			score.MaximumObservedM = round2(capacities[len(capacities)-1])
			score.ScoreM = trimmedSupplierQuotaCapacityMean(capacities)
			for _, capacity := range capacities {
				if capacity >= threshold {
					score.PassingSampleCount++
				}
			}
		}
		score.InvalidCredentialCount = entry.invalid
		score.CostMultiplier, _ = supplierCostMultiplier(score.MinUnitPriceFen, score.ScoreM)
		score.CostPerCapacityFen, _ = supplierCostPerCapacityFen(score.MinUnitPriceFen, score.ScoreM)
		score.EvidenceCount = score.SampleCount
		if score.SampleCount > 0 {
			score.PassRatePercent = round2(float64(score.PassingSampleCount) / float64(score.SampleCount) * 100)
		}
		switch {
		case score.SampleCount >= supplierQuotaMinimumDecisionSamples:
			if score.ScoreM >= threshold {
				score.Status = supplierQuotaStatusApproved
				score.Reason = "observed_quota_meets_threshold"
			} else {
				score.Status = supplierQuotaStatusBlocked
				score.Reason = "observed_quota_below_threshold"
			}
		case score.SampleCount > 0:
			// A clean high-quota sample may provisionally release a seller, but a
			// weak early sample only asks for more single-account trials. Invalid
			// credentials are not capacity samples and therefore do not affect this
			// decision.
			if score.ScoreM >= threshold {
				score.Status = supplierQuotaStatusApproved
				score.Reason = "provisional_quota_meets_threshold"
			} else {
				score.Status = supplierQuotaStatusObserving
				score.Reason = "waiting_for_more_supplier_evidence"
				// The last account has already produced usable capacity evidence.
				// Keep the seller on bounded single-account trials, but do not
				// impose the marketplace-failure cooldown before the representative
				// batch is full.
			}
		case entry.invalid > 0:
			score.Status = supplierQuotaStatusObserving
			score.Reason = "waiting_for_account_quota_evidence"
		case entry.imported > 0 || entry.inFlight || entry.attempted > 0 || entry.purchased > 0 || candidate.PurchasedBefore || candidate.PurchaseCount > 0:
			score.Status = supplierQuotaStatusObserving
			if entry.inFlight {
				score.Reason = "trial_in_flight"
			} else {
				score.Reason = "waiting_for_account_quota_evidence"
				if entry.lastAttemptMS > 0 {
					score.RetryAfterMS = entry.lastAttemptMS + supplierQuotaObservationRetryInterval.Milliseconds()
				}
			}
		default:
			score.Status = supplierQuotaStatusUntried
			score.Reason = "eligible_for_single_trial"
		}
		scores = append(scores, score)
	}
	sortSupplierQuotaScores(scores)
	s.setCachedMarketplaceSupplierQuotaScores(cacheKey, scores, now)
	return scores, nil
}

type supplierQuotaAccountSample struct {
	itemID       int64
	observedAtMS int64
	capacityM    float64
	invalid      bool
}

func supplierQuotaSampleOrderMS(importedAtMS, createdAtMS int64) int64 {
	if importedAtMS > 0 {
		return importedAtMS
	}
	if createdAtMS > 0 {
		return createdAtMS
	}
	return 0
}

func recentSupplierQuotaAccountSamples(samples []supplierQuotaAccountSample, limit int) []supplierQuotaAccountSample {
	if len(samples) == 0 || limit <= 0 {
		return nil
	}
	ordered := append([]supplierQuotaAccountSample(nil), samples...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].observedAtMS != ordered[j].observedAtMS {
			return ordered[i].observedAtMS > ordered[j].observedAtMS
		}
		return ordered[i].itemID > ordered[j].itemID
	})
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	return ordered
}

func trimmedSupplierQuotaCapacityMean(sortedCapacities []float64) float64 {
	if len(sortedCapacities) == 0 {
		return 0
	}
	values := sortedCapacities
	if len(values) >= 3 {
		values = values[1 : len(values)-1]
	}
	total := 0.0
	for _, value := range values {
		total += value
	}
	return round2(total / float64(len(values)))
}

func supplierCostPerCapacityFen(unitPriceFen int64, capacityM float64) (float64, bool) {
	multiplier, ok := supplierCostMultiplier(unitPriceFen, capacityM)
	if !ok {
		return 0, false
	}
	return math.Round(multiplier*100*10_000) / 10_000, true
}

// supplierCostMultiplier normalizes all purchase comparisons to yuan per 1M
// quota. Keeping the normalized unit explicit avoids comparing raw account
// prices when suppliers deliver very different capacities.
func supplierCostMultiplier(unitPriceFen int64, capacityM float64) (float64, bool) {
	if unitPriceFen <= 0 || capacityM <= 0 {
		return 0, false
	}
	return math.Round((float64(unitPriceFen)/100/capacityM)*1_000_000) / 1_000_000, true
}

func inspectionResultCredentialInvalid(result store.CodexInspectionResult) bool {
	if result.IsQuota {
		return false
	}
	if result.StatusCode != nil {
		switch *result.StatusCode {
		case 401, 403, 410:
			return true
		}
	}
	action := strings.ToLower(strings.TrimSpace(result.Action))
	if action == "reauth" {
		return true
	}
	message := strings.ToLower(strings.Join([]string{
		result.Status,
		result.State,
		result.ActionReason,
		result.ErrorKind,
		result.Error,
		result.ErrorDetail,
	}, " "))
	for _, marker := range []string{
		"credential invalidated",
		"token_invalidated",
		"token_revoked",
		"refresh_token_invalidated",
		"invalid_grant",
		"oauth token was revoked",
		"login_required",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func supplierQuotaScoreCacheKey(platform store.ManagerSupplyPlatformConfig) string {
	return strings.ToLower(strings.TrimSpace(platform.ID)) + "\x00" +
		strings.ToLower(strings.TrimSpace(platform.Product)) + "\x00" +
		strings.TrimSpace(formatSupplierQuotaThreshold(supplierQuotaGateMinimumM(platform)))
}

func formatSupplierQuotaThreshold(value float64) string {
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(value, 'f', 2, 64), "0"), ".")
}

func (s *Service) cachedMarketplaceSupplierQuotaScores(key string, now time.Time) []SupplierQuotaScore {
	if s == nil || key == "" {
		return nil
	}
	s.supplierQuotaScoreMu.Lock()
	defer s.supplierQuotaScoreMu.Unlock()
	if s.supplierQuotaScores == nil {
		return nil
	}
	entry, ok := s.supplierQuotaScores[key]
	if !ok || now.Sub(entry.generated) > supplierQuotaScoreCacheTTL {
		return nil
	}
	return append([]SupplierQuotaScore(nil), entry.scores...)
}

func (s *Service) setCachedMarketplaceSupplierQuotaScores(key string, scores []SupplierQuotaScore, now time.Time) {
	if s == nil || key == "" {
		return
	}
	s.supplierQuotaScoreMu.Lock()
	if s.supplierQuotaScores == nil {
		s.supplierQuotaScores = make(map[string]supplierQuotaScoreCacheEntry)
	}
	s.supplierQuotaScores[key] = supplierQuotaScoreCacheEntry{
		scores: append([]SupplierQuotaScore(nil), scores...), generated: now,
	}
	s.supplierQuotaScoreMu.Unlock()
}

func mergeMarketplaceSupplierQuotaScores(
	cached []SupplierQuotaScore,
	platform store.ManagerSupplyPlatformConfig,
	candidates []supplyclient.MarketplaceSellerCandidate,
	openOrders []store.SupplyOrder,
	now time.Time,
) []SupplierQuotaScore {
	bySeller := make(map[string]SupplierQuotaScore, len(cached)+len(candidates))
	for _, score := range cached {
		score.InFlightTrial = false
		score.Available = 0
		score.MinUnitPriceFen = 0
		score.MaxUnitPriceFen = 0
		score.CostPerCapacityFen = 0
		score.CostMultiplier = 0
		score.CheckedAtMS = now.UnixMilli()
		bySeller[normalizeMarketplaceSellerID(score.SellerID)] = score
	}
	for _, candidate := range candidates {
		key := normalizeMarketplaceSellerID(candidate.SellerID)
		if key == "" {
			continue
		}
		score, found := bySeller[key]
		if !found {
			score = SupplierQuotaScore{
				PlatformID: platform.ID, PlatformName: platform.Name,
				SellerID: candidate.SellerID, Product: platform.Product,
				ThresholdM: supplierQuotaGateMinimumM(platform), Status: supplierQuotaStatusUntried,
				Reason: "eligible_for_single_trial",
			}
		}
		score.SellerID = firstNonEmptyString(candidate.SellerID, score.SellerID)
		score.SellerName = firstNonEmptyString(candidate.Name, score.SellerName)
		score.ChannelID = firstNonEmptyString(candidate.ChannelID, score.ChannelID)
		score.SelectionToken = firstNonEmptyString(candidate.SelectionToken, score.SelectionToken)
		score.Available = candidate.Available
		score.MinUnitPriceFen = candidate.MinUnitPriceFen
		score.MaxUnitPriceFen = candidate.MaxUnitPriceFen
		score.CostMultiplier, _ = supplierCostMultiplier(score.MinUnitPriceFen, score.ScoreM)
		score.CostPerCapacityFen, _ = supplierCostPerCapacityFen(score.MinUnitPriceFen, score.ScoreM)
		score.CheckedAtMS = now.UnixMilli()
		if candidate.QualityScore != nil {
			score.MarketplaceQuality = *candidate.QualityScore
		}
		if candidate.ActiveRatePercent != nil {
			score.MarketplaceActiveRate = *candidate.ActiveRatePercent
		}
		if score.Status == supplierQuotaStatusUntried && (candidate.PurchasedBefore || candidate.PurchaseCount > 0) {
			score.Status = supplierQuotaStatusObserving
			score.Reason = "waiting_for_account_quota_evidence"
		}
		bySeller[key] = score
	}
	for _, order := range openOrders {
		if !strings.EqualFold(strings.TrimSpace(order.SupplierID), strings.TrimSpace(platform.ID)) ||
			!sameSupplyProduct(order.Product, platform.Product) {
			continue
		}
		key := normalizeMarketplaceSellerID(order.MarketplaceSellerID)
		score, found := bySeller[key]
		if !found {
			continue
		}
		score.InFlightTrial = true
		if score.Status == supplierQuotaStatusUntried {
			score.Status = supplierQuotaStatusObserving
			score.Reason = "waiting_for_account_quota_evidence"
		}
		bySeller[key] = score
	}
	result := make([]SupplierQuotaScore, 0, len(bySeller))
	for _, score := range bySeller {
		result = append(result, score)
	}
	sortSupplierQuotaScores(result)
	return result
}

func (s *Service) invalidateMarketplaceSupplierQuotaScores(platformID string, product string) {
	if s == nil {
		return
	}
	prefix := strings.ToLower(strings.TrimSpace(platformID)) + "\x00" +
		strings.ToLower(strings.TrimSpace(product)) + "\x00"
	s.supplierQuotaScoreMu.Lock()
	for key := range s.supplierQuotaScores {
		if strings.HasPrefix(key, prefix) {
			delete(s.supplierQuotaScores, key)
		}
	}
	s.supplierQuotaScoreMu.Unlock()
}

func (s *Service) invalidateAllMarketplaceSupplierQuotaScores() {
	if s == nil {
		return
	}
	s.supplierQuotaScoreMu.Lock()
	s.supplierQuotaScores = make(map[string]supplierQuotaScoreCacheEntry)
	s.supplierQuotaScoreMu.Unlock()
}

func sortSupplierQuotaScores(scores []SupplierQuotaScore) {
	sort.SliceStable(scores, func(i, j int) bool {
		statusOrder := map[string]int{
			supplierQuotaStatusApproved:  0,
			supplierQuotaStatusUntried:   1,
			supplierQuotaStatusObserving: 2,
			supplierQuotaStatusBlocked:   3,
		}
		if statusOrder[scores[i].Status] != statusOrder[scores[j].Status] {
			return statusOrder[scores[i].Status] < statusOrder[scores[j].Status]
		}
		leftPrice := supplierQuotaScoreSelectablePrice(scores[i])
		rightPrice := supplierQuotaScoreSelectablePrice(scores[j])
		leftCost, leftCostKnown := supplierCostMultiplier(leftPrice, scores[i].ScoreM)
		rightCost, rightCostKnown := supplierCostMultiplier(rightPrice, scores[j].ScoreM)
		if leftCostKnown != rightCostKnown {
			return leftCostKnown
		}
		if leftCostKnown && leftCost != rightCost {
			return leftCost < rightCost
		}
		if !leftCostKnown && leftPrice != rightPrice {
			return leftPrice < rightPrice
		}
		if scores[i].ScoreM != scores[j].ScoreM {
			return scores[i].ScoreM > scores[j].ScoreM
		}
		if scores[i].MarketplaceQuality != scores[j].MarketplaceQuality {
			return scores[i].MarketplaceQuality > scores[j].MarketplaceQuality
		}
		if scores[i].MarketplaceActiveRate != scores[j].MarketplaceActiveRate {
			return scores[i].MarketplaceActiveRate > scores[j].MarketplaceActiveRate
		}
		if scores[i].Available != scores[j].Available {
			return scores[i].Available > scores[j].Available
		}
		leftID := normalizeMarketplaceSellerID(scores[i].SellerID)
		rightID := normalizeMarketplaceSellerID(scores[j].SellerID)
		if leftID != rightID {
			return leftID < rightID
		}
		return strings.ToLower(scores[i].SellerName) < strings.ToLower(scores[j].SellerName)
	})
}

func supplierQuotaScoreSelectablePrice(score SupplierQuotaScore) int64 {
	if score.Available <= 0 || score.MinUnitPriceFen <= 0 {
		return math.MaxInt64
	}
	return score.MinUnitPriceFen
}

func normalizeMarketplaceSellerID(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func marketplaceSellerSelectionFromOrder(order store.SupplyOrder) *marketplaceSellerSelection {
	if strings.TrimSpace(order.MarketplaceSellerID) == "" && strings.TrimSpace(order.MarketplaceSelectionToken) == "" {
		return nil
	}
	return &marketplaceSellerSelection{candidate: supplyclient.MarketplaceSellerCandidate{
		SellerID:       order.MarketplaceSellerID,
		Name:           order.MarketplaceSellerName,
		ChannelID:      order.MarketplaceChannelID,
		SelectionToken: order.MarketplaceSelectionToken,
	}}
}

func marketplaceSellerID(selection *marketplaceSellerSelection) string {
	if selection == nil {
		return ""
	}
	return selection.candidate.SellerID
}

func marketplaceSellerName(selection *marketplaceSellerSelection) string {
	if selection == nil {
		return ""
	}
	return selection.candidate.Name
}

func marketplaceSellerChannelID(selection *marketplaceSellerSelection) string {
	if selection == nil {
		return ""
	}
	return selection.candidate.ChannelID
}

func marketplaceSellerSelectionToken(selection *marketplaceSellerSelection) string {
	if selection == nil {
		return ""
	}
	return selection.candidate.SelectionToken
}
