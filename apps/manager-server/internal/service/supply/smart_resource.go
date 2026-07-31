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

	smartCapacitySourceInspection  = "inspection_snapshot"
	smartCapacitySourceUnavailable = "unavailable"
)

type SmartResource struct {
	Enabled                    bool    `json:"enabled"`
	HealthLevel                string  `json:"healthLevel"`
	SuggestedAction            string  `json:"suggestedAction"`
	SuggestedQuantity          int     `json:"suggestedQuantity"`
	DecisionReason             string  `json:"decisionReason"`
	Confidence                 string  `json:"confidence"`
	SnapshotFresh              bool    `json:"snapshotFresh"`
	GeneratedAtMS              int64   `json:"generatedAtMs"`
	CapacitySource             string  `json:"capacitySource"`
	CapacityCoverage           float64 `json:"capacityCoverage"`
	CapacityLifetimeCoverage   float64 `json:"capacityLifetimeCoverage"`
	CapacitySnapshotAtMS       int64   `json:"capacitySnapshotAtMs"`
	CapacitySnapshotAgeSeconds int     `json:"capacitySnapshotAgeSeconds"`
	CapacitySnapshotRunID      int64   `json:"capacitySnapshotRunId,omitempty"`
	// These counts are informational only. Smart replenishment is driven by
	// RCU capacity and burn rate, never by credential counts. They remain in
	// the response so an older cached management page does not render its
	// account-health summary as four misleading zeroes.
	AvailableAccounts        int     `json:"availableAccounts"`
	SchedulableAccounts      int     `json:"schedulableAccounts"`
	HealthyAccounts          int     `json:"healthyAccounts"`
	WeakAccounts             int     `json:"weakAccounts"`
	TargetAvailableAccounts  int     `json:"-"`
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
}

type authFileSnapshot struct {
	files       []cpaauthfiles.File
	generatedAt time.Time
	attemptedAt time.Time
	lastErr     error
}

// inspectionQuotaSnapshot is derived exclusively from a completed credential
// inspection. It deliberately does not read CPA's live auth-file list: that
// list contains transient scheduler state and is too volatile to drive a
// purchasing decision.
type inspectionQuotaSnapshot struct {
	run                store.CodexInspectionRun
	results            []store.CodexInspectionResult
	leaseExpiresByFile map[string]int64
	generatedAt        time.Time
	attemptedAt        time.Time
	lastErr            error
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
		CapacitySource:           smartCapacitySourceUnavailable,
		CapacityLifetimeCoverage: 100,
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
			bucket = &smartUsageBucket{minuteMS: minute}
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
	quotaSnapshot, err := s.cachedInspectionQuotaSnapshot(ctx, cfg.Supply, forceAuthRefresh)
	if err != nil && len(quotaSnapshot.results) == 0 {
		resource := defaultSmartResource(cfg.Supply)
		resource.SuggestedAction = smartActionSnapshotStale
		resource.DecisionReason = "inspection_snapshot_unavailable"
		s.setSmartResource(resource)
		// Missing quota evidence is an expected cold-start state. The automatic
		// loop must pause quietly rather than repeatedly recording an operational
		// error or falling back to credential counts.
		return resource, nil
	}
	resource := s.buildSmartResourceFromInspectionSnapshot(cfg.Supply, quotaSnapshot, time.Now())
	if err != nil {
		resource.SnapshotFresh = false
		resource.DecisionReason = "using_stale_inspection_snapshot"
	}
	s.setSmartResource(resource)
	return resource, nil
}

func (s *Service) buildSmartResourceFromInspectionSnapshot(cfg store.ManagerSupplyConfig, snapshot inspectionQuotaSnapshot, now time.Time) SmartResource {
	resource := defaultSmartResource(cfg)
	resource.GeneratedAtMS = now.UnixMilli()
	resource.CapacitySource = smartCapacitySourceInspection
	resource.CapacitySnapshotRunID = snapshot.run.ID
	if !snapshot.generatedAt.IsZero() {
		resource.CapacitySnapshotAtMS = snapshot.generatedAt.UnixMilli()
		resource.CapacitySnapshotAgeSeconds = max(0, int(now.Sub(snapshot.generatedAt).Seconds()))
		resource.AccountCacheAgeSeconds = resource.CapacitySnapshotAgeSeconds
	}
	resource.SnapshotFresh = smartInspectionSnapshotFresh(snapshot, now)

	usageStats := s.smartUsageSnapshot(now)
	resource.RPM30M = usageStats.rpm30
	resource.RPM5MPeak = usageStats.rpm5Peak
	resource.TPM30M = usageStats.tpm30
	resource.UsageSampleMinutes = usageStats.sampleMinutes
	resource.UnitCapacityRCU = smartProductUnitCapacity(cfg.Product)

	if !smartInspectionSnapshotComplete(snapshot) {
		resource.SnapshotFresh = false
		resource.Confidence = smartConfidenceLow
		resource.HealthLevel = smartHealthUnknown
		resource.SuggestedAction = smartActionSnapshotStale
		resource.DecisionReason = "inspection_snapshot_incomplete"
		return resource
	}

	capacityItems := make([]smartCapacityItem, 0, len(snapshot.results))
	eligible := 0
	withQuota := 0
	leaseRequired := 0
	withActiveLease := 0
	for _, result := range snapshot.results {
		if !isSmartCapacityInspectionResult(result) {
			continue
		}
		resource.SchedulableAccounts++
		if inspectionResultCapacityExcluded(result) {
			resource.WeakAccounts++
			continue
		}
		eligible++
		remaining, ok := inspectionResultRemainingQuotaFraction(result)
		if !ok {
			resource.WeakAccounts++
			continue
		}
		withQuota++
		remainingMinutes := float64(smartUsefulAccountLifetimeMinutes())
		if smartSupplyManagedFileName(result.FileName) {
			leaseRequired++
			leaseExpiresAtMS, found := snapshot.leaseExpiresByFile[result.FileName]
			if !found || leaseExpiresAtMS <= now.UnixMilli() {
				resource.WeakAccounts++
				continue
			}
			withActiveLease++
			remainingMinutes = clampFloat(time.UnixMilli(leaseExpiresAtMS).Sub(now).Minutes(), 0, float64(smartUsefulAccountLifetimeMinutes()))
		}
		capacity := smartEstimatedAccountCapacityRCU(resource.UnitCapacityRCU, remainingMinutes) * remaining
		if capacity <= 0 {
			resource.WeakAccounts++
			continue
		}
		capacityItems = append(capacityItems, smartCapacityItem{
			capacityRCU:      capacity,
			remainingMinutes: remainingMinutes,
		})
		resource.AvailableAccounts++
		resource.HealthyAccounts++
	}
	if eligible > 0 {
		resource.CapacityCoverage = round2(float64(withQuota) / float64(eligible) * 100)
	} else {
		// A complete inspection which has no usable credential is a trusted zero
		// capacity state, not a missing data state.
		resource.CapacityCoverage = 100
	}
	if leaseRequired > 0 {
		resource.CapacityLifetimeCoverage = round2(float64(withActiveLease) / float64(leaseRequired) * 100)
	} else {
		resource.CapacityLifetimeCoverage = 100
	}
	quotaEvidenceIncomplete := eligible > 0 && withQuota != eligible
	leaseEvidenceIncomplete := leaseRequired > 0 && withActiveLease != leaseRequired

	for _, item := range capacityItems {
		resource.RawCapacityRCU += item.capacityRCU
	}
	resource.RawCapacityRCU = round2(resource.RawCapacityRCU)
	resource.CurrentCapacityRCU = resource.RawCapacityRCU
	consumeRCUPerMinute := smartConsumeRCUPerMinute(resource.RPM30M, resource.RPM5MPeak, resource.TPM30M, resource.UnitCapacityRCU)
	resource.ConsumeRCUPerMinute = round2(consumeRCUPerMinute)
	if consumeRCUPerMinute > 0 {
		usableCapacity, wasteRisk := smartExpiryLimitedCapacity(capacityItems, consumeRCUPerMinute)
		resource.TimeLimitedCapacityRCU = round2(usableCapacity)
		resource.ExpiryWasteRiskRCU = round2(wasteRisk)
		resource.CurrentCapacityRCU = resource.TimeLimitedCapacityRCU
	}
	resource.TargetCapacityRCU = round2(consumeRCUPerMinute * float64(resource.EffectiveHealthyMinutes))
	resource.RecommendedCapacityRCU = resource.TargetCapacityRCU

	// Keep the verified portion visible even while one or more credentials did
	// not return quota evidence. Returning early here used to turn the entire
	// dashboard into 0 RCU, despite most capacity being known. Automation still
	// pauses below; these figures are display-only lower bounds.
	if quotaEvidenceIncomplete || leaseEvidenceIncomplete {
		if usageStats.requests30 > 0 && consumeRCUPerMinute > 0 {
			recalculateSmartResourceCapacityPlan(cfg, &resource)
		}
		resource.SnapshotFresh = false
		resource.Confidence = smartConfidenceLow
		resource.HealthLevel = smartHealthUnknown
		resource.SuggestedAction = smartActionSnapshotStale
		resource.SuggestedQuantity = 0
		if quotaEvidenceIncomplete {
			resource.DecisionReason = "inspection_quota_incomplete"
		} else {
			resource.DecisionReason = "inspection_lease_incomplete"
		}
		return resource
	}
	if usageStats.requests30 <= 0 || consumeRCUPerMinute <= 0 {
		resource.Confidence = smartConfidenceLow
		resource.HealthLevel = smartHealthUnknown
		resource.SuggestedAction = smartActionSnapshotStale
		resource.DecisionReason = "usage_rate_not_ready"
		return resource
	}
	recalculateSmartResourceCapacityPlan(cfg, &resource)
	if usageStats.sampleMinutes >= 20 && resource.SnapshotFresh {
		resource.Confidence = smartConfidenceHigh
	} else if usageStats.sampleMinutes >= 5 {
		resource.Confidence = smartConfidenceMedium
	} else {
		resource.Confidence = smartConfidenceLow
	}
	return resource
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
		if !isAvailableCodexFile(file) {
			resource.WeakAccounts++
			continue
		}
		// 临时运行态（包括冷却与上游异常）不是凭证健康信号。请求历史
		// 只用于全局消耗速度，不能折减单凭证余额或可用数量。
		resource.AvailableAccounts++
		resource.HealthyAccounts++
		remainingMinutes := smartAccountRemainingMinutes(file.Raw, now, smartAccountLifetimeMinutes())
		rawCapacity, ok := smartAccountCapacityRCU(file.Raw, unit, remainingMinutes)
		if !ok {
			rawCapacity = smartEstimatedAccountCapacityRCU(unit, remainingMinutes)
		}
		weightedCapacity += rawCapacity
		if rawCapacity > 0 {
			capacityItems = append(capacityItems, smartCapacityItem{
				capacityRCU:      rawCapacity,
				remainingMinutes: remainingMinutes,
			})
		}
		effectiveAvailable++
	}
	resource.AvailableAccounts = int(effectiveAvailable)
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

	if usageStats.sampleMinutes >= 20 && resource.SnapshotFresh {
		resource.Confidence = smartConfidenceHigh
	} else if usageStats.sampleMinutes >= 5 {
		resource.Confidence = smartConfidenceMedium
	} else {
		resource.Confidence = smartConfidenceLow
	}
	recalculateSmartResourceCapacityPlan(cfg, &resource)
	return resource
}

// recalculateSmartResourceCapacityPlan applies the active configuration to an
// existing capacity snapshot. GetStatus uses it too, so a changed health-water
// level never keeps an obsolete health decision until the next CPA refresh.
func recalculateSmartResourceCapacityPlan(cfg store.ManagerSupplyConfig, resource *SmartResource) {
	if resource == nil || resource.ConsumeRCUPerMinute <= 0 {
		return
	}
	resource.TargetCapacityRCU = round2(resource.ConsumeRCUPerMinute * float64(resource.EffectiveHealthyMinutes))
	resource.RecommendedCapacityRCU = resource.TargetCapacityRCU
	resource.EstimatedSustainMinutes = round1(resource.CurrentCapacityRCU / resource.ConsumeRCUPerMinute)
	resource.CapacityGapRCU = round2(math.Max(0, resource.TargetCapacityRCU-resource.CurrentCapacityRCU-resource.PrelockedCapacityRCU))
	resource.SuggestedQuantity = 0

	if resource.EstimatedSustainMinutes >= float64(resource.EffectiveHealthyMinutes) {
		resource.HealthLevel = smartHealthHealthy
		resource.SuggestedAction = smartActionHealthy
		resource.DecisionReason = "capacity_healthy"
		return
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
		unitForNew = smartEstimatedAccountCapacityRCU(resource.UnitCapacityRCU, float64(smartUsefulAccountLifetimeMinutes()))
	}
	maxUsefulNewCapacity := math.Max(0, resource.ConsumeRCUPerMinute*float64(smartUsefulAccountLifetimeMinutes())-resource.CurrentCapacityRCU-resource.PrelockedCapacityRCU)
	gapForOrder := math.Min(resource.CapacityGapRCU, maxUsefulNewCapacity)
	if gapForOrder <= 0 {
		resource.HealthLevel = smartHealthWarning
		resource.SuggestedAction = smartActionHealthy
		resource.DecisionReason = "expiry_limited_capacity"
		return
	}
	resource.SuggestedQuantity = clampInt(int(math.Ceil(gapForOrder/unitForNew)), smartPrelockMinQuantity(cfg), smartPrelockMaxQuantity(cfg))
}

type smartUsageAggregate struct {
	requests30    int64
	tokens30      int64
	rpm30         float64
	rpm5Peak      float64
	tpm30         float64
	sampleMinutes int
}

func (s *Service) smartUsageSnapshot(now time.Time) smartUsageAggregate {
	s.smartMu.RLock()
	defer s.smartMu.RUnlock()
	result := smartUsageAggregate{}
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
	if !force && authSnapshotCacheUsable(s.authCache, now, ttl) {
		snapshot := cloneAuthSnapshot(s.authCache)
		s.authCacheMu.Unlock()
		return snapshot, snapshot.lastErr
	}
	s.authCacheMu.Unlock()

	s.authRefreshMu.Lock()
	defer s.authRefreshMu.Unlock()
	s.authCacheMu.Lock()
	if !force && authSnapshotCacheUsable(s.authCache, now, ttl) {
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
	attemptedAt := time.Now()
	if err == nil {
		s.authCache = authFileSnapshot{files: files, generatedAt: attemptedAt, attemptedAt: attemptedAt}
	} else {
		s.authCache.attemptedAt = attemptedAt
		s.authCache.lastErr = err
	}
	snapshot := cloneAuthSnapshot(s.authCache)
	s.authCacheMu.Unlock()
	return snapshot, err
}

func (s *Service) cachedInspectionQuotaSnapshot(ctx context.Context, cfg store.ManagerSupplyConfig, force bool) (inspectionQuotaSnapshot, error) {
	if s == nil || s.store == nil {
		return inspectionQuotaSnapshot{}, ErrCapacitySnapshotUnavailable
	}
	ttl := time.Duration(smartAuthFilesCacheTTLSeconds(cfg)) * time.Second
	now := time.Now()
	s.quotaSnapshotMu.Lock()
	if !force && inspectionSnapshotCacheUsable(s.quotaSnapshot, now, ttl) {
		snapshot := cloneInspectionQuotaSnapshot(s.quotaSnapshot)
		s.quotaSnapshotMu.Unlock()
		return snapshot, snapshot.lastErr
	}
	s.quotaSnapshotMu.Unlock()

	s.quotaRefreshMu.Lock()
	defer s.quotaRefreshMu.Unlock()
	s.quotaSnapshotMu.Lock()
	if !force && inspectionSnapshotCacheUsable(s.quotaSnapshot, now, ttl) {
		snapshot := cloneInspectionQuotaSnapshot(s.quotaSnapshot)
		s.quotaSnapshotMu.Unlock()
		return snapshot, snapshot.lastErr
	}
	s.quotaSnapshotMu.Unlock()

	refreshed, err := s.loadLatestInspectionQuotaSnapshot(ctx)
	attemptedAt := time.Now()
	s.quotaSnapshotMu.Lock()
	if err == nil {
		refreshed.attemptedAt = attemptedAt
		refreshed.lastErr = nil
		s.quotaSnapshot = refreshed
	} else {
		s.quotaSnapshot.attemptedAt = attemptedAt
		s.quotaSnapshot.lastErr = err
	}
	snapshot := cloneInspectionQuotaSnapshot(s.quotaSnapshot)
	s.quotaSnapshotMu.Unlock()
	return snapshot, err
}

func (s *Service) loadLatestInspectionQuotaSnapshot(ctx context.Context) (inspectionQuotaSnapshot, error) {
	runs, err := s.store.ListCodexInspectionRuns(ctx, 20)
	if err != nil {
		return inspectionQuotaSnapshot{}, err
	}
	for _, run := range runs {
		if run.Status != "completed" {
			continue
		}
		results, err := s.store.ListCodexInspectionResults(ctx, run.ID)
		if err != nil {
			return inspectionQuotaSnapshot{}, err
		}
		filtered := make([]store.CodexInspectionResult, 0, len(results))
		for _, result := range results {
			if isSmartCapacityInspectionResult(result) {
				filtered = append(filtered, result)
			}
		}
		if len(filtered) == 0 {
			continue
		}
		leaseItems, err := s.store.ListActiveImportedSupplyItems(ctx, time.Now().UnixMilli())
		if err != nil {
			return inspectionQuotaSnapshot{}, err
		}
		leaseExpiresByFile := make(map[string]int64, len(leaseItems))
		for _, item := range leaseItems {
			fileName := strings.TrimSpace(item.FileName)
			if fileName == "" || item.LeaseExpiresAtMS <= 0 {
				continue
			}
			leaseExpiresByFile[fileName] = maxInt64(leaseExpiresByFile[fileName], item.LeaseExpiresAtMS)
		}
		generatedAt := time.UnixMilli(run.FinishedAtMS)
		if run.FinishedAtMS <= 0 {
			generatedAt = time.UnixMilli(run.UpdatedAtMS)
		}
		return inspectionQuotaSnapshot{run: run, results: filtered, leaseExpiresByFile: leaseExpiresByFile, generatedAt: generatedAt}, nil
	}
	return inspectionQuotaSnapshot{}, ErrCapacitySnapshotUnavailable
}

func inspectionSnapshotCacheUsable(snapshot inspectionQuotaSnapshot, now time.Time, ttl time.Duration) bool {
	if !snapshot.generatedAt.IsZero() && now.Sub(snapshot.attemptedAt) <= ttl {
		return true
	}
	return !snapshot.attemptedAt.IsZero() && now.Sub(snapshot.attemptedAt) <= ttl
}

func cloneInspectionQuotaSnapshot(snapshot inspectionQuotaSnapshot) inspectionQuotaSnapshot {
	results := make([]store.CodexInspectionResult, len(snapshot.results))
	copy(results, snapshot.results)
	snapshot.results = results
	leases := make(map[string]int64, len(snapshot.leaseExpiresByFile))
	for fileName, expiresAtMS := range snapshot.leaseExpiresByFile {
		leases[fileName] = expiresAtMS
	}
	snapshot.leaseExpiresByFile = leases
	return snapshot
}

func smartInspectionSnapshotFresh(snapshot inspectionQuotaSnapshot, now time.Time) bool {
	if snapshot.generatedAt.IsZero() || now.Before(snapshot.generatedAt) {
		return false
	}
	return now.Sub(snapshot.generatedAt) <= 20*time.Minute
}

func smartInspectionSnapshotComplete(snapshot inspectionQuotaSnapshot) bool {
	return snapshot.run.ProbeSetCount > 0 && snapshot.run.SampledCount >= snapshot.run.ProbeSetCount
}

func isSmartCapacityInspectionResult(result store.CodexInspectionResult) bool {
	switch strings.ToLower(strings.TrimSpace(result.Provider)) {
	case "codex", "openai", "openai-codex":
		return true
	default:
		return false
	}
}

func inspectionResultCapacityExcluded(result store.CodexInspectionResult) bool {
	if result.IsQuota {
		return true
	}
	message := strings.ToLower(strings.Join([]string{result.Status, result.State, result.ErrorKind, result.Error, result.ErrorDetail}, " "))
	if strings.Contains(message, "invalid") ||
		strings.Contains(message, "expired") ||
		strings.Contains(message, "revoked") ||
		strings.Contains(message, "unauthorized") ||
		strings.Contains(message, "forbidden") ||
		strings.Contains(message, "quota") ||
		strings.Contains(message, "usage_limit_reached") {
		return true
	}
	// A transient cooldown may temporarily mark a file disabled. It does not
	// erase a credential's verified remaining quota, so retain it in capacity.
	if inspectionResultInCooldown(result) {
		return false
	}
	return result.Disabled
}

func inspectionResultInCooldown(result store.CodexInspectionResult) bool {
	message := strings.ToLower(strings.Join([]string{result.Status, result.State, result.ActionReason, result.ErrorKind, result.Error, result.ErrorDetail}, " "))
	return strings.Contains(message, "cooldown") ||
		strings.Contains(message, "cooling") ||
		strings.Contains(message, "retry_after")
}

func smartSupplyManagedFileName(fileName string) bool {
	fileName = strings.ToLower(strings.TrimSpace(fileName))
	return strings.HasPrefix(fileName, "codex-supply-") || strings.HasPrefix(fileName, "supply-")
}

func inspectionResultRemainingQuotaFraction(result store.CodexInspectionResult) (float64, bool) {
	minimum := 1.0
	found := false
	for _, window := range result.QuotaWindows {
		id := strings.ToLower(strings.TrimSpace(window.ID + " " + window.LabelKey))
		if strings.Contains(id, "code-review") || window.UsedPercent == nil {
			continue
		}
		used := *window.UsedPercent
		if used > 1 {
			used /= 100
		}
		minimum = math.Min(minimum, clampFloat(1-used, 0, 1))
		found = true
	}
	if found {
		return minimum, true
	}
	if result.UsedPercent == nil {
		return 0, false
	}
	used := *result.UsedPercent
	if used > 1 {
		used /= 100
	}
	return clampFloat(1-used, 0, 1), true
}

func authSnapshotCacheUsable(snapshot authFileSnapshot, now time.Time, ttl time.Duration) bool {
	if !snapshot.generatedAt.IsZero() && now.Sub(snapshot.generatedAt) <= ttl {
		return true
	}
	return !snapshot.attemptedAt.IsZero() && now.Sub(snapshot.attemptedAt) <= ttl
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
	previousEffectiveMinutes := resource.EffectiveHealthyMinutes
	previousWarningMinutes := resource.WarningMinutes
	previousCriticalMinutes := resource.CriticalMinutes
	previousUnitCapacity := resource.UnitCapacityRCU
	resource.Enabled = smartSupplyEnabled(cfg)
	resource.ConfiguredHealthyMinutes = smartHealthyMinutesTarget(cfg)
	resource.EffectiveHealthyMinutes = smartEffectiveHealthyMinutesTarget(cfg)
	resource.AccountLifetimeMinutes = smartAccountLifetimeMinutes()
	if resource.CapacitySource == smartCapacitySourceInspection {
		resource.CapacitySnapshotAgeSeconds = max(0, int(time.Since(time.UnixMilli(resource.CapacitySnapshotAtMS)).Seconds()))
		if resource.CapacitySnapshotAgeSeconds > 20*60 {
			resource.SnapshotFresh = false
		}
	}
	resource.HealthyMinutesTarget = resource.EffectiveHealthyMinutes
	resource.WarningMinutes = smartWarningMinutes(cfg)
	resource.CriticalMinutes = smartCriticalMinutes(cfg)
	resource.TargetAvailableAccounts = cfg.TargetAvailableAccounts
	configChanged := previousEffectiveMinutes != resource.EffectiveHealthyMinutes ||
		previousWarningMinutes != resource.WarningMinutes ||
		previousCriticalMinutes != resource.CriticalMinutes ||
		previousUnitCapacity != smartProductUnitCapacity(cfg.Product)
	capacityDecision := strings.HasPrefix(resource.DecisionReason, "capacity_") || resource.DecisionReason == "expiry_limited_capacity"
	if resource.Enabled && resource.SnapshotFresh && (configChanged || capacityDecision) {
		recalculateSmartResourceCapacityPlan(cfg, &resource)
	}
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

func isSupplyCapacityEvent(event usage.Event) bool {
	identity := strings.ToLower(strings.Join([]string{event.Provider, event.ExecutorType, event.AuthType, event.AuthProviderSnapshot}, " "))
	if strings.Contains(identity, "codex") || strings.Contains(identity, "openai") {
		return true
	}
	return event.AuthIndex != "" || event.AccountSnapshot != "" || event.AuthFileSnapshot != ""
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
