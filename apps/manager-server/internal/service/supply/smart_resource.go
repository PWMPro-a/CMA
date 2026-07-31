package supply

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	collectorpkg "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/collector"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpaauthfiles"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/usage"
)

const (
	smartHealthMinRecentSamples          int64   = 5
	smartHealthMinRawSamples             int64   = 10
	smartHealthyAccountWeightThreshold   float64 = 0.75
	smartNewAccountConfidenceFromHistory float64 = 0.5
	smartEffectiveAccountEpsilon         float64 = 0.000001
)

const (
	smartActionHealthy          = "healthy"
	smartActionPrelock          = "prelock"
	smartActionWaitLocked       = "wait_locked"
	smartActionReleaseLocked    = "release_locked"
	smartActionTakeLocked       = "take_locked"
	smartActionBalanceBlocked   = "balance_blocked"
	smartActionInventoryBlocked = "inventory_blocked"
	smartActionConfigError      = "config_error"
	smartActionSnapshotStale    = "snapshot_stale"
	smartActionManualReview     = "manual_review"

	smartHealthHealthy  = "healthy"
	smartHealthWarning  = "warning"
	smartHealthCritical = "critical"
	smartHealthUnknown  = "unknown"

	smartConfidenceHigh   = "high"
	smartConfidenceMedium = "medium"
	smartConfidenceLow    = "low"

	smartSupplyPressurePlenty  = "plenty"
	smartSupplyPressureNormal  = "normal"
	smartSupplyPressureTight   = "tight"
	smartSupplyPressureScarce  = "scarce"
	smartSupplyPressureUnknown = "unknown"
)

type SmartResource struct {
	Enabled                  bool    `json:"enabled"`
	HealthLevel              string  `json:"healthLevel"`
	SuggestedAction          string  `json:"suggestedAction"`
	SuggestedQuantity        int     `json:"suggestedQuantity"`
	DecisionReason           string  `json:"decisionReason"`
	Confidence               string  `json:"confidence"`
	SnapshotFresh            bool    `json:"snapshotFresh"`
	GeneratedAtMS            int64   `json:"generatedAtMs"`
	AvailableAccounts        int     `json:"availableAccounts"`
	SchedulableAccounts      int     `json:"schedulableAccounts"`
	HealthyAccounts          int     `json:"healthyAccounts"`
	WeakAccounts             int     `json:"weakAccounts"`
	TargetAvailableAccounts  int     `json:"targetAvailableAccounts"`
	ConfiguredHealthyMinutes int     `json:"configuredHealthyMinutesTarget,omitempty"`
	EffectiveHealthyMinutes  int     `json:"effectiveHealthyMinutesTarget"`
	AccountLifetimeMinutes   int     `json:"accountLifetimeMinutes"`
	EstimatedSustainMinutes  float64 `json:"estimatedSustainMinutes"`
	HealthyMinutesTarget     int     `json:"healthyMinutesTarget"`
	WarningMinutes           int     `json:"warningMinutes"`
	CriticalMinutes          int     `json:"criticalMinutes"`
	RPM30M                   float64 `json:"rpm30m"`
	RPM5MPeak                float64 `json:"rpm5mPeak"`
	TPM30M                   float64 `json:"tpm30m"`
	ConsumeRCUPerMinute      float64 `json:"consumeRcuPerMinute"`
	CurrentCapacityRCU       float64 `json:"currentCapacityRcu"`
	RawCapacityRCU           float64 `json:"rawCapacityRcu,omitempty"`
	TimeLimitedCapacityRCU   float64 `json:"timeLimitedCapacityRcu,omitempty"`
	ExpiryWasteRiskRCU       float64 `json:"expiryWasteRiskRcu,omitempty"`
	TargetCapacityRCU        float64 `json:"targetCapacityRcu"`
	CapacityGapRCU           float64 `json:"capacityGapRcu"`
	UnitCapacityRCU          float64 `json:"unitCapacityRcu"`
	RecommendedCapacityRCU   float64 `json:"recommendedCapacityRcu"`
	PrelockedCapacityRCU     float64 `json:"prelockedCapacityRcu,omitempty"`
	SupplyPressureLevel      string  `json:"supplyPressureLevel,omitempty"`
	SupplyPressureReason     string  `json:"supplyPressureReason,omitempty"`
	SupplyInventoryAvailable int     `json:"supplyInventoryAvailable,omitempty"`
	SupplyInventoryMissing   int     `json:"supplyInventoryMissing,omitempty"`
	SupplyNeedsProduction    bool    `json:"supplyNeedsProduction,omitempty"`
	SupplyAvgFulfillSeconds  int     `json:"supplyAvgFulfillSeconds,omitempty"`
	SupplyRecentWaiting      int     `json:"supplyRecentWaiting,omitempty"`
	UsageSampleMinutes       int     `json:"usageSampleMinutes"`
	AccountCacheAgeSeconds   int     `json:"accountCacheAgeSeconds"`
	LockedOrderID            string  `json:"lockedOrderId,omitempty"`
	LockedOrderAgeSeconds    int     `json:"lockedOrderAgeSeconds,omitempty"`
	LockedConfirmRounds      int     `json:"lockedConfirmRounds,omitempty"`
}

type smartUsageBucket struct {
	minuteMS    int64
	requests    int64
	success     int64
	failed      int64
	zeroTokens  int64
	totalTokens int64
	accounts    map[string]*smartAccountUsage
}

type smartAccountUsage struct {
	requests   int64
	success    int64
	failed     int64
	zeroTokens int64
}

type authFileSnapshot struct {
	files       []cpaauthfiles.File
	generatedAt time.Time
	lastErr     error
}

type smartCapacityItem struct {
	capacityRCU      float64
	remainingMinutes float64
}

func defaultSmartResource(cfg store.ManagerSupplyConfig) SmartResource {
	configuredTarget := smartHealthyMinutesTarget(cfg)
	effectiveTarget := smartEffectiveHealthyMinutesTarget(cfg)
	return SmartResource{
		Enabled:                  smartSupplyEnabled(cfg),
		HealthLevel:              smartHealthUnknown,
		SuggestedAction:          smartActionSnapshotStale,
		DecisionReason:           "snapshot_not_ready",
		Confidence:               smartConfidenceLow,
		SnapshotFresh:            false,
		GeneratedAtMS:            time.Now().UnixMilli(),
		TargetAvailableAccounts:  cfg.TargetAvailableAccounts,
		ConfiguredHealthyMinutes: configuredTarget,
		EffectiveHealthyMinutes:  effectiveTarget,
		AccountLifetimeMinutes:   smartAccountLifetimeMinutes(),
		HealthyMinutesTarget:     effectiveTarget,
		WarningMinutes:           smartWarningMinutes(cfg),
		CriticalMinutes:          smartCriticalMinutes(cfg),
		UnitCapacityRCU:          smartProductUnitCapacity(cfg.Product),
	}
}

func (s *Service) HandleUsageEvents(ctx context.Context, _ collectorpkg.RuntimeConfig, events []usage.Event) {
	if s == nil || len(events) == 0 || ctx.Err() != nil {
		return
	}
	s.recordSmartUsageEvents(events, time.Now())
}

func (s *Service) recordSmartUsageEvents(events []usage.Event, now time.Time) {
	s.smartMu.Lock()
	defer s.smartMu.Unlock()
	if s.smartBuckets == nil {
		s.smartBuckets = make(map[int64]*smartUsageBucket)
	}
	oldestMinute := now.Add(-180*time.Minute).UnixMilli() / 60000 * 60000
	for _, event := range events {
		if !isSupplyCapacityEvent(event) {
			continue
		}
		ts := event.TimestampMS
		if ts <= 0 {
			ts = now.UnixMilli()
		}
		minute := ts / 60000 * 60000
		if minute < oldestMinute {
			continue
		}
		bucket := s.smartBuckets[minute]
		if bucket == nil {
			bucket = &smartUsageBucket{minuteMS: minute, accounts: make(map[string]*smartAccountUsage)}
			s.smartBuckets[minute] = bucket
		}
		bucket.requests++
		if event.Failed {
			bucket.failed++
		} else {
			bucket.success++
		}
		if event.TotalTokens <= 0 && event.InputTokens <= 0 && event.OutputTokens <= 0 {
			bucket.zeroTokens++
		}
		bucket.totalTokens += maxInt64(event.TotalTokens, event.InputTokens+event.OutputTokens+event.ReasoningTokens)
		accountKey := smartUsageAccountKey(event)
		if accountKey != "" {
			acct := bucket.accounts[accountKey]
			if acct == nil {
				acct = &smartAccountUsage{}
				bucket.accounts[accountKey] = acct
			}
			acct.requests++
			if event.Failed {
				acct.failed++
			} else {
				acct.success++
			}
			if event.TotalTokens <= 0 && event.InputTokens <= 0 && event.OutputTokens <= 0 {
				acct.zeroTokens++
			}
		}
	}
	for minute := range s.smartBuckets {
		if minute < oldestMinute {
			delete(s.smartBuckets, minute)
		}
	}
}

func (s *Service) smartResource(ctx context.Context, cfg store.ManagerConfig, forceAuthRefresh bool) (SmartResource, error) {
	if !smartSupplyEnabled(cfg.Supply) {
		resource := defaultSmartResource(cfg.Supply)
		resource.Enabled = false
		resource.HealthLevel = smartHealthUnknown
		resource.SuggestedAction = smartActionHealthy
		resource.DecisionReason = "smart_disabled"
		s.setSmartResource(resource)
		return resource, nil
	}
	authSnapshot, err := s.cachedAuthFiles(ctx, cfg, forceAuthRefresh)
	if err != nil && len(authSnapshot.files) == 0 {
		resource := defaultSmartResource(cfg.Supply)
		resource.SuggestedAction = smartActionSnapshotStale
		resource.DecisionReason = "auth_files_unavailable"
		s.setSmartResource(resource)
		return resource, err
	}
	resource := s.buildSmartResourceFromSnapshots(cfg.Supply, authSnapshot, time.Now())
	if err != nil {
		resource.SnapshotFresh = false
		resource.DecisionReason = "using_stale_auth_files"
	}
	s.setSmartResource(resource)
	return resource, nil
}

func (s *Service) buildSmartResourceFromSnapshots(cfg store.ManagerSupplyConfig, authSnapshot authFileSnapshot, now time.Time) SmartResource {
	resource := defaultSmartResource(cfg)
	resource.GeneratedAtMS = now.UnixMilli()
	resource.SnapshotFresh = !authSnapshot.generatedAt.IsZero() && now.Sub(authSnapshot.generatedAt) <= time.Duration(smartAuthFilesCacheTTLSeconds(cfg))*time.Second*2
	if !authSnapshot.generatedAt.IsZero() {
		resource.AccountCacheAgeSeconds = max(0, int(now.Sub(authSnapshot.generatedAt).Seconds()))
	}
	usageStats := s.smartUsageSnapshot(now)
	resource.RPM30M = usageStats.rpm30
	resource.RPM5MPeak = usageStats.rpm5Peak
	resource.TPM30M = usageStats.tpm30
	resource.UsageSampleMinutes = usageStats.sampleMinutes

	accountUsage := usageStats.accounts
	unit := smartProductUnitCapacity(cfg.Product)
	resource.UnitCapacityRCU = unit
	var weightedCapacity float64
	var effectiveAvailable float64
	capacityItems := make([]smartCapacityItem, 0, len(authSnapshot.files))
	for _, file := range authSnapshot.files {
		if !isSmartCapacityCodexFile(file) {
			continue
		}
		resource.SchedulableAccounts++
		weight := smartAccountHealthWeight(file, accountUsage)
		remainingMinutes := smartAccountRemainingMinutes(file.Raw, now, smartAccountLifetimeMinutes())
		rawCapacity, ok := smartAccountCapacityRCU(file.Raw, unit, remainingMinutes)
		if ok {
			rawCapacity *= weight
		} else {
			rawCapacity = smartEstimatedAccountCapacityRCU(unit, remainingMinutes) * weight
		}
		weightedCapacity += rawCapacity
		if rawCapacity > 0 {
			capacityItems = append(capacityItems, smartCapacityItem{
				capacityRCU:      rawCapacity,
				remainingMinutes: remainingMinutes,
			})
		}
		effectiveAvailable += weight
		if weight >= smartHealthyAccountWeightThreshold {
			resource.HealthyAccounts++
		} else {
			resource.WeakAccounts++
		}
	}
	resource.AvailableAccounts = int(math.Floor(effectiveAvailable + smartEffectiveAccountEpsilon))
	resource.RawCapacityRCU = round2(weightedCapacity)
	resource.CurrentCapacityRCU = resource.RawCapacityRCU
	consumeRCUPerMinute := smartConsumeRCUPerMinute(resource.RPM30M, resource.RPM5MPeak, resource.TPM30M, unit)
	resource.ConsumeRCUPerMinute = round2(consumeRCUPerMinute)
	if consumeRCUPerMinute > 0 {
		usableCapacity, wasteRisk := smartExpiryLimitedCapacity(capacityItems, consumeRCUPerMinute)
		resource.TimeLimitedCapacityRCU = round2(usableCapacity)
		resource.ExpiryWasteRiskRCU = round2(wasteRisk)
		resource.CurrentCapacityRCU = resource.TimeLimitedCapacityRCU
	}
	resource.TargetCapacityRCU = round2(consumeRCUPerMinute * float64(resource.EffectiveHealthyMinutes))
	resource.RecommendedCapacityRCU = resource.TargetCapacityRCU

	if usageStats.requests30 <= 0 || consumeRCUPerMinute <= 0 {
		resource.Confidence = smartConfidenceLow
		resource.HealthLevel = smartHealthUnknown
		resource.SuggestedAction = smartActionSnapshotStale
		resource.DecisionReason = "usage_rate_not_ready"
		resource.EstimatedSustainMinutes = 0
		return resource
	}

	resource.EstimatedSustainMinutes = round1(resource.CurrentCapacityRCU / consumeRCUPerMinute)
	resource.CapacityGapRCU = round2(math.Max(0, resource.TargetCapacityRCU-resource.CurrentCapacityRCU-resource.PrelockedCapacityRCU))
	if usageStats.sampleMinutes >= 20 && resource.SnapshotFresh {
		resource.Confidence = smartConfidenceHigh
	} else if usageStats.sampleMinutes >= 5 {
		resource.Confidence = smartConfidenceMedium
	} else {
		resource.Confidence = smartConfidenceLow
	}
	if resource.EstimatedSustainMinutes >= float64(resource.EffectiveHealthyMinutes) {
		resource.HealthLevel = smartHealthHealthy
		resource.SuggestedAction = smartActionHealthy
		resource.DecisionReason = "capacity_healthy"
		return resource
	}
	if resource.EstimatedSustainMinutes < float64(resource.CriticalMinutes) {
		resource.HealthLevel = smartHealthCritical
		resource.SuggestedAction = smartActionTakeLocked
		resource.DecisionReason = "capacity_critical"
	} else if resource.EstimatedSustainMinutes < float64(resource.WarningMinutes) {
		resource.HealthLevel = smartHealthWarning
		resource.SuggestedAction = smartActionPrelock
		resource.DecisionReason = "capacity_below_warning"
	} else {
		resource.HealthLevel = smartHealthWarning
		resource.SuggestedAction = smartActionPrelock
		resource.DecisionReason = "capacity_below_target"
	}
	unitForNew := smartEstimatedNewAccountCapacityRCU(cfg)
	if unitForNew <= 0 {
		unitForNew = smartEstimatedAccountCapacityRCU(unit, float64(smartUsefulAccountLifetimeMinutes()))
	}
	maxUsefulNewCapacity := math.Max(0, consumeRCUPerMinute*float64(smartUsefulAccountLifetimeMinutes())-resource.CurrentCapacityRCU-resource.PrelockedCapacityRCU)
	gapForOrder := math.Min(resource.CapacityGapRCU, maxUsefulNewCapacity)
	if gapForOrder <= 0 {
		resource.HealthLevel = smartHealthWarning
		resource.SuggestedAction = smartActionHealthy
		resource.DecisionReason = "expiry_limited_capacity"
		return resource
	}
	resource.SuggestedQuantity = clampInt(int(math.Ceil(gapForOrder/unitForNew)), smartPrelockMinQuantity(cfg), smartPrelockMaxQuantity(cfg))
	return resource
}

type smartUsageAggregate struct {
	requests30    int64
	tokens30      int64
	rpm30         float64
	rpm5Peak      float64
	tpm30         float64
	sampleMinutes int
	accounts      map[string]smartAccountUsage
}

func (s *Service) smartUsageSnapshot(now time.Time) smartUsageAggregate {
	s.smartMu.RLock()
	defer s.smartMu.RUnlock()
	result := smartUsageAggregate{accounts: make(map[string]smartAccountUsage)}
	if len(s.smartBuckets) == 0 {
		return result
	}
	from30 := now.Add(-30*time.Minute).UnixMilli() / 60000 * 60000
	from5 := now.Add(-5*time.Minute).UnixMilli() / 60000 * 60000
	activeMinutes := map[int64]struct{}{}
	perMinute5 := map[int64]int64{}
	for minute, bucket := range s.smartBuckets {
		if bucket == nil || minute < from30 {
			continue
		}
		result.requests30 += bucket.requests
		result.tokens30 += bucket.totalTokens
		if bucket.requests > 0 {
			activeMinutes[minute] = struct{}{}
		}
		if minute >= from5 {
			perMinute5[minute] += bucket.requests
		}
		for key, acct := range bucket.accounts {
			if key == "" || acct == nil {
				continue
			}
			agg := result.accounts[key]
			agg.requests += acct.requests
			agg.success += acct.success
			agg.failed += acct.failed
			agg.zeroTokens += acct.zeroTokens
			result.accounts[key] = agg
		}
	}
	result.sampleMinutes = len(activeMinutes)
	if result.requests30 > 0 {
		denominator := math.Max(1, math.Min(30, float64(max(1, result.sampleMinutes))))
		// Use the full 30 minute window once there is enough data; otherwise keep early startup responsive.
		if result.sampleMinutes >= 10 {
			denominator = 30
		}
		result.rpm30 = float64(result.requests30) / denominator
		result.tpm30 = float64(result.tokens30) / denominator
	}
	for _, count := range perMinute5 {
		if float64(count) > result.rpm5Peak {
			result.rpm5Peak = float64(count)
		}
	}
	return result
}

func (s *Service) cachedAuthFiles(ctx context.Context, cfg store.ManagerConfig, force bool) (authFileSnapshot, error) {
	if strings.TrimSpace(cfg.CPAConnection.CPABaseURL) == "" || strings.TrimSpace(cfg.CPAConnection.ManagementKey) == "" {
		return authFileSnapshot{}, ErrNotConfigured
	}
	ttl := time.Duration(smartAuthFilesCacheTTLSeconds(cfg.Supply)) * time.Second
	now := time.Now()
	s.authCacheMu.Lock()
	if !force && !s.authCache.generatedAt.IsZero() && now.Sub(s.authCache.generatedAt) <= ttl {
		snapshot := cloneAuthSnapshot(s.authCache)
		s.authCacheMu.Unlock()
		return snapshot, snapshot.lastErr
	}
	s.authCacheMu.Unlock()

	s.authRefreshMu.Lock()
	defer s.authRefreshMu.Unlock()
	s.authCacheMu.Lock()
	if !force && !s.authCache.generatedAt.IsZero() && now.Sub(s.authCache.generatedAt) <= ttl {
		snapshot := cloneAuthSnapshot(s.authCache)
		s.authCacheMu.Unlock()
		return snapshot, snapshot.lastErr
	}
	s.authCacheMu.Unlock()

	files := make([]cpaauthfiles.File, 0)
	err := s.authFiles.Visit(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey, func(file cpaauthfiles.File) (bool, error) {
		files = append(files, file)
		return false, nil
	})
	s.authCacheMu.Lock()
	if err == nil {
		s.authCache = authFileSnapshot{files: files, generatedAt: time.Now()}
	} else if len(s.authCache.files) > 0 {
		s.authCache.lastErr = err
	}
	snapshot := cloneAuthSnapshot(s.authCache)
	s.authCacheMu.Unlock()
	return snapshot, err
}

func cloneAuthSnapshot(snapshot authFileSnapshot) authFileSnapshot {
	files := make([]cpaauthfiles.File, len(snapshot.files))
	copy(files, snapshot.files)
	snapshot.files = files
	return snapshot
}

func (s *Service) currentSmartResource(cfg store.ManagerSupplyConfig) SmartResource {
	s.stateMu.RLock()
	resource := s.smartResourceState
	s.stateMu.RUnlock()
	if resource.GeneratedAtMS == 0 {
		return defaultSmartResource(cfg)
	}
	resource.Enabled = smartSupplyEnabled(cfg)
	resource.ConfiguredHealthyMinutes = smartHealthyMinutesTarget(cfg)
	resource.EffectiveHealthyMinutes = smartEffectiveHealthyMinutesTarget(cfg)
	resource.AccountLifetimeMinutes = smartAccountLifetimeMinutes()
	resource.HealthyMinutesTarget = resource.EffectiveHealthyMinutes
	resource.WarningMinutes = smartWarningMinutes(cfg)
	resource.CriticalMinutes = smartCriticalMinutes(cfg)
	resource.TargetAvailableAccounts = cfg.TargetAvailableAccounts
	return resource
}

func (s *Service) setSmartResource(resource SmartResource) {
	s.stateMu.Lock()
	s.smartResourceState = resource
	s.stateMu.Unlock()
}

func smartSupplyEnabled(cfg store.ManagerSupplyConfig) bool {
	return cfg.SmartEnabled == nil || *cfg.SmartEnabled
}

func smartHealthyMinutesTarget(cfg store.ManagerSupplyConfig) int {
	return positiveOr(cfg.HealthyMinutesTarget, 120)
}

func smartAccountLifetimeMinutes() int {
	return 60
}

func smartUsefulAccountLifetimeMinutes() int {
	// Supplier accounts are short-lived. Keep a small tail as safety because
	// taking, importing, scheduling and in-flight requests all consume part of
	// the one-hour lifetime.
	return 55
}

func smartEffectiveHealthyMinutesTarget(cfg store.ManagerSupplyConfig) int {
	return min(smartHealthyMinutesTarget(cfg), smartUsefulAccountLifetimeMinutes())
}

func smartWarningMinutes(cfg store.ManagerSupplyConfig) int {
	target := smartEffectiveHealthyMinutesTarget(cfg)
	value := positiveOr(cfg.WarningMinutes, max(1, target/2))
	if value >= target {
		value = max(1, target/2)
	}
	return value
}

func smartCriticalMinutes(cfg store.ManagerSupplyConfig) int {
	value := positiveOr(cfg.CriticalMinutes, 30)
	if value >= smartWarningMinutes(cfg) {
		value = max(1, smartWarningMinutes(cfg)/2)
	}
	return value
}

func smartPrelockEnabled(cfg store.ManagerSupplyConfig) bool {
	return cfg.PrelockEnabled == nil || *cfg.PrelockEnabled
}

func smartPrelockMinQuantity(cfg store.ManagerSupplyConfig) int {
	return clampInt(positiveOr(cfg.PrelockMinQuantity, 1), 1, 100)
}

func smartPrelockMaxQuantity(cfg store.ManagerSupplyConfig) int {
	maxQuantity := clampInt(positiveOr(cfg.PrelockMaxQuantity, 10), 1, 100)
	if maxQuantity < smartPrelockMinQuantity(cfg) {
		maxQuantity = smartPrelockMinQuantity(cfg)
	}
	return maxQuantity
}

func smartCriticalTakeConfirmRounds(cfg store.ManagerSupplyConfig) int {
	return clampInt(positiveOr(cfg.CriticalTakeConfirmRounds, 2), 1, 5)
}

func smartCreateCooldownSeconds(cfg store.ManagerSupplyConfig) int {
	return clampInt(positiveOr(cfg.CreateCooldownSeconds, 120), 0, 3600)
}

func smartReleaseCooldownSeconds(cfg store.ManagerSupplyConfig) int {
	return clampInt(positiveOr(cfg.ReleaseCooldownSeconds, 60), 0, 3600)
}

func smartAuthFilesCacheTTLSeconds(cfg store.ManagerSupplyConfig) int {
	return clampInt(positiveOr(cfg.AuthFilesCacheTTLSeconds, 60), 10, 600)
}

func smartMinHoldSeconds(cfg store.ManagerSupplyConfig) int {
	return clampInt(positiveOr(cfg.MinHoldSeconds, 30), 0, 3600)
}

func smartNewAccountConfidence(cfg store.ManagerSupplyConfig) float64 {
	if cfg.NewAccountConfidence <= 0 {
		return 0.7
	}
	if cfg.NewAccountConfidence > 1 {
		return 1
	}
	return cfg.NewAccountConfidence
}

func smartProductUnitCapacity(product string) float64 {
	switch strings.ToLower(strings.TrimSpace(product)) {
	case "oauth_7d":
		return 40
	case "oauth_30d":
		return 80
	default:
		return 50
	}
}

func smartEstimatedAccountCapacityRCU(unitPerMinute float64, remainingMinutes float64) float64 {
	if unitPerMinute <= 0 {
		unitPerMinute = 1
	}
	remainingMinutes = clampFloat(remainingMinutes, 0, float64(smartAccountLifetimeMinutes()))
	return unitPerMinute * remainingMinutes
}

func smartEstimatedNewAccountCapacityRCU(cfg store.ManagerSupplyConfig) float64 {
	unit := smartProductUnitCapacity(cfg.Product)
	capacity := smartEstimatedAccountCapacityRCU(unit, float64(smartUsefulAccountLifetimeMinutes()))
	confidence := smartNewAccountConfidence(cfg)
	if confidence <= 0 {
		confidence = 1
	}
	return capacity * confidence
}

func smartConsumeRCUPerMinute(rpm30 float64, rpm5Peak float64, tpm30 float64, unit float64) float64 {
	requestRate := math.Max(rpm30, rpm5Peak*0.7)
	requestRCU := requestRate
	tokenRCU := 0.0
	if unit > 0 && tpm30 > 0 {
		// 40k TPM ~= one 7D account-minute; 80k TPM ~= one 30D account-minute.
		tokenRCU = tpm30 / 1000 / unit
	}
	return math.Max(requestRCU, tokenRCU)
}

func smartAccountHealthWeight(file cpaauthfiles.File, usageStats map[string]smartAccountUsage) float64 {
	if !isSmartCapacityCodexFile(file) {
		return 0
	}
	if weight, ok := smartUsageHealthWeightForFile(file, usageStats); ok {
		return weight
	}
	if weight, ok := smartRawHealthWeight(file.Raw); ok {
		return weight
	}
	if weight, ok := smartStatusHealthWeight(file.Raw); ok {
		return weight
	}
	return smartNewAccountConfidenceFromHistory
}

func smartUsageHealthWeightForFile(file cpaauthfiles.File, usageStats map[string]smartAccountUsage) (float64, bool) {
	for _, key := range smartFileAccountKeys(file) {
		stats, ok := usageStats[key]
		if !ok || stats.requests < smartHealthMinRecentSamples {
			continue
		}
		return smartHealthWeight(stats.success, stats.failed, stats.zeroTokens), true
	}
	return 0, false
}

func smartRawHealthWeight(values map[string]any) (float64, bool) {
	if weight, ok := smartRecentRequestHealthWeight(values); ok {
		return weight, true
	}
	success := int64(numberField(values, "success", "success_count", "successCalls"))
	failed := int64(numberField(values, "failed", "failure_count", "failureCalls"))
	total := success + failed
	if total < smartHealthMinRawSamples {
		return 0, false
	}
	return smartHealthWeight(success, failed, 0), true
}

func smartStatusHealthWeight(values map[string]any) (float64, bool) {
	status := strings.ToLower(textField(values, "status", "state", "runtime_status", "runtimeStatus"))
	message := strings.ToLower(textField(values, "status_message", "statusMessage", "error_kind", "errorKind", "header_error_kind", "headerErrorKind", "last_error", "lastError"))
	combined := strings.TrimSpace(status + " " + message)
	if combined == "" {
		return 0, false
	}
	if smartAccountCapacityHardBlocked(values) {
		return 0, true
	}
	switch {
	case strings.Contains(combined, "invalid_grant"),
		strings.Contains(combined, "unauthorized"),
		strings.Contains(combined, "forbidden"),
		strings.Contains(combined, "revoked"),
		strings.Contains(combined, "expired"),
		strings.Contains(combined, "login_required"),
		strings.Contains(combined, "reauth"):
		return 0.05, true
	case status == "error" || status == "failed" || status == "unavailable",
		strings.Contains(combined, "server overloaded"),
		strings.Contains(combined, "stability_budget_exhausted"),
		strings.Contains(combined, "rate_limit"),
		strings.Contains(combined, "quota"):
		return 0.25, true
	default:
		return 0, false
	}
}

func smartAccountCapacityHardBlocked(values map[string]any) bool {
	status := strings.ToLower(textField(values, "status", "state", "runtime_status", "runtimeStatus"))
	message := strings.ToLower(textField(values, "status_message", "statusMessage", "error_kind", "errorKind", "header_error_kind", "headerErrorKind", "last_error", "lastError"))
	combined := strings.TrimSpace(status + " " + message)
	if combined == "" {
		return false
	}
	switch {
	case strings.Contains(combined, "usage_limit_reached"),
		strings.Contains(combined, "quota_exhausted"),
		strings.Contains(combined, "insufficient_quota"),
		strings.Contains(combined, "billing_hard_limit"),
		strings.Contains(combined, "hard_limit_reached"),
		strings.Contains(combined, "credit_grant_exhausted"),
		strings.Contains(combined, "exceeded your current quota"):
		return true
	case strings.Contains(combined, "credential invalidated"),
		strings.Contains(combined, "token_invalidated"),
		strings.Contains(combined, "invalid_grant"),
		strings.Contains(combined, "invalid token"),
		strings.Contains(combined, "invalid_token"),
		strings.Contains(combined, "login_required"),
		strings.Contains(combined, "reauth"),
		strings.Contains(combined, "unauthorized"),
		strings.Contains(combined, "forbidden"),
		strings.Contains(combined, "revoked"),
		strings.Contains(combined, "expired"),
		strings.Contains(combined, " 401 "):
		return true
	default:
		return false
	}
}

func smartRecentRequestHealthWeight(values map[string]any) (float64, bool) {
	success, failed, zeroTokens := int64(0), int64(0), int64(0)
	for _, key := range []string{"recent_requests", "recentRequests", "runtime_recent_requests", "runtimeRecentRequests"} {
		raw, ok := values[key]
		if !ok || raw == nil {
			continue
		}
		items, ok := raw.([]any)
		if !ok {
			continue
		}
		for _, item := range items {
			object, ok := item.(map[string]any)
			if !ok {
				continue
			}
			success += int64(numberField(object, "success", "success_count", "successful", "ok"))
			failed += int64(numberField(object, "failed", "failure_count", "failures", "error", "errors"))
			zeroTokens += int64(numberField(object, "zero_tokens", "zeroTokens", "empty_output", "emptyOutput"))
		}
	}
	if success+failed < smartHealthMinRecentSamples {
		return 0, false
	}
	return smartHealthWeight(success, failed, zeroTokens), true
}

func smartHealthWeight(success int64, failed int64, zeroTokens int64) float64 {
	total := success + failed
	if total <= 0 {
		return smartNewAccountConfidenceFromHistory
	}
	rate := float64(success) / float64(total)
	var weight float64
	switch {
	case rate >= 0.95:
		weight = 1
	case rate >= 0.90:
		weight = 0.85
	case rate >= 0.80:
		weight = 0.6
	case rate >= 0.65:
		weight = 0.35
	case rate >= 0.50:
		weight = 0.2
	default:
		weight = 0.05
	}
	if zeroTokens > 0 {
		zeroRate := float64(zeroTokens) / float64(total)
		if zeroRate > 0.25 {
			weight *= 0.3
		} else if zeroRate > 0.10 {
			weight *= 0.65
		}
	}
	if weight < 0 {
		return 0
	}
	if weight > 1 {
		return 1
	}
	return weight
}

func smartAccountCapacityRCU(values map[string]any, unit float64, remainingMinutes float64) (float64, bool) {
	if unit <= 0 {
		unit = 1
	}
	if smartAccountCapacityHardBlocked(values) {
		return 0, true
	}
	if capacity := numberField(
		values,
		"remaining_rcu", "remainingRcu", "quota_remaining_rcu", "quotaRemainingRcu",
		"available_rcu", "availableRcu", "quota_balance_rcu", "quotaBalanceRcu",
	); capacity > 0 {
		return capacity, true
	}
	if capacity := numberField(
		values,
		"quota_remaining", "quotaRemaining", "remaining_quota", "remainingQuota",
		"available_quota", "availableQuota", "quota_balance", "quotaBalance",
		"remaining_budget", "remainingBudget",
	); capacity > 0 {
		return capacity, true
	}
	if tokens := numberField(
		values,
		"remaining_tokens", "remainingTokens", "quota_remaining_tokens", "quotaRemainingTokens",
		"available_tokens", "availableTokens",
	); tokens > 0 {
		return tokens / 1000, true
	}
	if usedPercent := numberField(values, "header_quota_used_percent", "quota_used_percent", "quotaUsedPercent", "used_percent", "usage_percent"); usedPercent > 0 {
		if usedPercent > 1 {
			usedPercent = usedPercent / 100
		}
		remaining := 1 - usedPercent
		if remaining < 0 {
			remaining = 0
		}
		return smartEstimatedAccountCapacityRCU(unit, remainingMinutes) * remaining, true
	}
	return 0, false
}

func smartExpiryLimitedCapacity(items []smartCapacityItem, consumeRCUPerMinute float64) (float64, float64) {
	if consumeRCUPerMinute <= 0 || len(items) == 0 {
		total := 0.0
		for _, item := range items {
			total += math.Max(0, item.capacityRCU)
		}
		return total, 0
	}
	ordered := make([]smartCapacityItem, 0, len(items))
	for _, item := range items {
		if item.capacityRCU <= 0 {
			continue
		}
		item.remainingMinutes = math.Max(0, math.Min(float64(smartAccountLifetimeMinutes()), item.remainingMinutes))
		ordered = append(ordered, item)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].remainingMinutes < ordered[j].remainingMinutes
	})
	usable := 0.0
	wasteRisk := 0.0
	for _, item := range ordered {
		maxConsumableBeforeExpiry := consumeRCUPerMinute * item.remainingMinutes
		remainingDemandBeforeExpiry := maxConsumableBeforeExpiry - usable
		if remainingDemandBeforeExpiry <= 0 {
			wasteRisk += item.capacityRCU
			continue
		}
		use := math.Min(item.capacityRCU, remainingDemandBeforeExpiry)
		usable += use
		wasteRisk += math.Max(0, item.capacityRCU-use)
	}
	return usable, wasteRisk
}

func smartAccountRemainingMinutes(values map[string]any, now time.Time, fallbackMinutes int) float64 {
	if fallbackMinutes <= 0 {
		fallbackMinutes = smartAccountLifetimeMinutes()
	}
	if seconds, ok := numberFieldOK(values,
		"remaining_seconds", "remainingSeconds", "remaining_valid_seconds", "remainingValidSeconds",
		"minimum_remaining_seconds", "minimumRemainingSeconds", "ttl_seconds", "ttlSeconds",
	); ok {
		return clampFloat(seconds/60, 0, float64(smartAccountLifetimeMinutes()))
	}
	if minutes, ok := numberFieldOK(values, "remaining_minutes", "remainingMinutes", "ttl_minutes", "ttlMinutes"); ok {
		return clampFloat(minutes, 0, float64(smartAccountLifetimeMinutes()))
	}
	if seconds, ok := numberFieldOK(values, "expires_in", "expiresIn", "expire_in", "expireIn"); ok {
		return clampFloat(seconds/60, 0, float64(smartAccountLifetimeMinutes()))
	}
	for _, key := range []string{"expired", "expires_at", "expiresAt", "expire_at", "expireAt", "valid_until", "validUntil"} {
		raw, ok := values[key]
		if !ok || raw == nil {
			continue
		}
		if expiry, ok := parseSmartExpiryTime(raw, now); ok {
			return clampFloat(expiry.Sub(now).Minutes(), 0, float64(smartAccountLifetimeMinutes()))
		}
	}
	return float64(fallbackMinutes)
}

func parseSmartExpiryTime(value any, now time.Time) (time.Time, bool) {
	switch typed := value.(type) {
	case int:
		return unixLikeToTime(float64(typed), now)
	case int64:
		return unixLikeToTime(float64(typed), now)
	case float64:
		return unixLikeToTime(typed, now)
	case jsonNumber:
		parsed, err := typed.Float64()
		if err != nil {
			return time.Time{}, false
		}
		return unixLikeToTime(parsed, now)
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return time.Time{}, false
		}
		if parsed, ok := parseFloat(text); ok {
			return unixLikeToTime(parsed, now)
		}
		for _, layout := range []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02 15:04:05",
			"2006-01-02T15:04:05",
			"2006-01-02",
		} {
			if parsed, err := time.Parse(layout, text); err == nil {
				return parsed, true
			}
		}
	}
	return time.Time{}, false
}

func unixLikeToTime(value float64, now time.Time) (time.Time, bool) {
	if value <= 0 {
		return time.Time{}, false
	}
	// Values smaller than 10 years are durations from now rather than absolute unix timestamps.
	if value < 10*365*24*60*60 {
		return now.Add(time.Duration(value) * time.Second), true
	}
	if value > 1e12 {
		return time.UnixMilli(int64(value)), true
	}
	return time.Unix(int64(value), 0), true
}

func smartFileAccountKeys(file cpaauthfiles.File) []string {
	values := []string{file.AuthIndex, file.AccountID, file.AccountSnapshot, file.Name}
	keys := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		keys = append(keys, value)
	}
	return keys
}

func isSupplyCapacityEvent(event usage.Event) bool {
	identity := strings.ToLower(strings.Join([]string{event.Provider, event.ExecutorType, event.AuthType, event.AuthProviderSnapshot}, " "))
	if strings.Contains(identity, "codex") || strings.Contains(identity, "openai") {
		return true
	}
	return event.AuthIndex != "" || event.AccountSnapshot != "" || event.AuthFileSnapshot != ""
}

func smartUsageAccountKey(event usage.Event) string {
	for _, value := range []string{event.AuthIndex, event.AccountSnapshot, event.AuthLabelSnapshot, event.AuthFileSnapshot, event.Source} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func numberField(values map[string]any, keys ...string) float64 {
	value, _ := numberFieldOK(values, keys...)
	return value
}

func numberFieldOK(values map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		value, ok := values[key]
		if !ok || value == nil {
			continue
		}
		switch typed := value.(type) {
		case int:
			return float64(typed), true
		case int64:
			return float64(typed), true
		case float64:
			return typed, true
		case jsonNumber:
			parsed, _ := typed.Float64()
			return parsed, true
		case string:
			if parsed, ok := parseFloat(typed); ok {
				return parsed, true
			}
		}
	}
	return 0, false
}

type jsonNumber interface{ Float64() (float64, error) }

func parseFloat(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	result, err := strconv.ParseFloat(value, 64)
	return result, err == nil
}

func positiveOr(value int, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func clampInt(value, minValue, maxValue int) int {
	if maxValue < minValue {
		maxValue = minValue
	}
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func clampFloat(value, minValue, maxValue float64) float64 {
	if maxValue < minValue {
		maxValue = minValue
	}
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func round1(value float64) float64 {
	return math.Round(value*10) / 10
}

func round2(value float64) float64 {
	return math.Round(value*100) / 100
}

func supplySmartActionPriority(action string) int {
	order := []string{smartActionHealthy, smartActionPrelock, smartActionWaitLocked, smartActionReleaseLocked, smartActionTakeLocked, smartActionBalanceBlocked, smartActionInventoryBlocked, smartActionConfigError, smartActionSnapshotStale, smartActionManualReview}
	for index, item := range order {
		if item == action {
			return index
		}
	}
	return len(order)
}

func sortSmartResources(resources []SmartResource) {
	sort.Slice(resources, func(i, j int) bool {
		return supplySmartActionPriority(resources[i].SuggestedAction) < supplySmartActionPriority(resources[j].SuggestedAction)
	})
}
