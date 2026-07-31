package supply

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	collectorpkg "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/collector"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
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
	smartActionObserveDemand    = "observe_demand"

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

	smartDemandTrendUnknown = "unknown"
	smartDemandTrendStable  = "stable"
	smartDemandTrendRising  = "rising"
	smartDemandTrendFalling = "falling"
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
	AvailableAccounts   int `json:"availableAccounts"`
	SchedulableAccounts int `json:"schedulableAccounts"`
	HealthyAccounts     int `json:"healthyAccounts"`
	WeakAccounts        int `json:"weakAccounts"`
	// Newly delivered credentials are only added as a conservative provisional
	// capacity overlay until the next completed usability inspection verifies
	// them. They are deliberately separate from HealthyAccounts.
	PendingInspectionAccounts    int     `json:"pendingInspectionAccounts,omitempty"`
	PendingInspectionCapacityRCU float64 `json:"pendingInspectionCapacityRcu,omitempty"`
	// A supplier-managed credential can have a verified usable probe but expose
	// only its monthly quota window. The quota selector always prefers a shorter
	// window; monthly data is used only as the fallback when no short window is
	// present. The delivery lease still bounds its usable lifetime.
	LeaseEstimatedAccounts     int     `json:"leaseEstimatedAccounts,omitempty"`
	LeaseEstimatedCapacityRCU  float64 `json:"leaseEstimatedCapacityRcu,omitempty"`
	TargetAvailableAccounts    int     `json:"-"`
	ConfiguredHealthyMinutes   int     `json:"configuredHealthyMinutesTarget,omitempty"`
	EffectiveHealthyMinutes    int     `json:"effectiveHealthyMinutesTarget"`
	AccountLifetimeMinutes     int     `json:"accountLifetimeMinutes"`
	EstimatedSustainMinutes    float64 `json:"estimatedSustainMinutes"`
	HealthyMinutesTarget       int     `json:"healthyMinutesTarget"`
	WarningMinutes             int     `json:"warningMinutes"`
	CriticalMinutes            int     `json:"criticalMinutes"`
	RPM30M                     float64 `json:"rpm30m"`
	RPM5MPeak                  float64 `json:"rpm5mPeak"`
	TPM30M                     float64 `json:"tpm30m"`
	RPM1M                      float64 `json:"rpm1m"`
	RPM5M                      float64 `json:"rpm5m"`
	RPM10M                     float64 `json:"rpm10m"`
	TPM1M                      float64 `json:"tpm1m"`
	TPM5M                      float64 `json:"tpm5m"`
	TPM10M                     float64 `json:"tpm10m"`
	ConsumeRCU1M               float64 `json:"consumeRcu1m"`
	ConsumeRCU5M               float64 `json:"consumeRcu5m"`
	ConsumeRCU10M              float64 `json:"consumeRcu10m"`
	DemandTrend                string  `json:"demandTrend"`
	DemandPlanningRCUPerMinute float64 `json:"demandPlanningRcuPerMinute"`
	ConsumeRCUPerMinute        float64 `json:"consumeRcuPerMinute"`
	CurrentCapacityRCU         float64 `json:"currentCapacityRcu"`
	RawCapacityRCU             float64 `json:"rawCapacityRcu,omitempty"`
	TimeLimitedCapacityRCU     float64 `json:"timeLimitedCapacityRcu,omitempty"`
	ExpiryWasteRiskRCU         float64 `json:"expiryWasteRiskRcu,omitempty"`
	TargetCapacityRCU          float64 `json:"targetCapacityRcu"`
	CapacityGapRCU             float64 `json:"capacityGapRcu"`
	UnitCapacityRCU            float64 `json:"unitCapacityRcu"`
	RecommendedCapacityRCU     float64 `json:"recommendedCapacityRcu"`
	PrelockedCapacityRCU       float64 `json:"prelockedCapacityRcu,omitempty"`
	SupplyPressureLevel        string  `json:"supplyPressureLevel,omitempty"`
	SupplyPressureReason       string  `json:"supplyPressureReason,omitempty"`
	SupplyInventoryAvailable   int     `json:"supplyInventoryAvailable,omitempty"`
	SupplyInventoryMissing     int     `json:"supplyInventoryMissing,omitempty"`
	SupplyNeedsProduction      bool    `json:"supplyNeedsProduction,omitempty"`
	SupplyAvgFulfillSeconds    int     `json:"supplyAvgFulfillSeconds,omitempty"`
	SupplyRecentWaiting        int     `json:"supplyRecentWaiting,omitempty"`
	UsageSampleMinutes         int     `json:"usageSampleMinutes"`
	AccountCacheAgeSeconds     int     `json:"accountCacheAgeSeconds"`
	LockedOrderID              string  `json:"lockedOrderId,omitempty"`
	LockedOrderAgeSeconds      int     `json:"lockedOrderAgeSeconds,omitempty"`
	LockedConfirmRounds        int     `json:"lockedConfirmRounds,omitempty"`
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
	activeImportItems  []store.SupplyImportItem
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
		DemandTrend:              smartDemandTrendUnknown,
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

// WarmSmartUsage restores the small rolling demand window after a Manager
// restart. It reads one grouped 30-minute slice during startup only; runtime
// collectors continue to update smartBuckets incrementally in memory.
func (s *Service) WarmSmartUsage(ctx context.Context) error {
	if s == nil || s.store == nil {
		return nil
	}
	now := time.Now()
	from := now.Add(-30 * time.Minute).UnixMilli()
	minutes, err := s.store.ListSupplyUsageMinutes(ctx, from)
	if err != nil {
		return err
	}
	oldestMinute := now.Add(-180*time.Minute).UnixMilli() / 60000 * 60000
	s.smartMu.Lock()
	defer s.smartMu.Unlock()
	if s.smartBuckets == nil {
		s.smartBuckets = make(map[int64]*smartUsageBucket)
	}
	for _, minute := range minutes {
		if minute.MinuteMS < oldestMinute {
			continue
		}
		s.smartBuckets[minute.MinuteMS] = &smartUsageBucket{
			minuteMS:    minute.MinuteMS,
			requests:    minute.Requests,
			success:     minute.Successful,
			failed:      minute.Failed,
			totalTokens: minute.TotalTokens,
		}
	}
	for minute := range s.smartBuckets {
		if minute < oldestMinute {
			delete(s.smartBuckets, minute)
		}
	}
	return nil
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
	resource.UnitCapacityRCU = smartProductUnitCapacity(cfg.Product)
	consumeRCUPerMinute := applySmartUsage(&resource, usageStats, resource.UnitCapacityRCU)

	if !smartInspectionSnapshotComplete(snapshot) {
		resource.SnapshotFresh = false
		resource.Confidence = smartConfidenceLow
		resource.HealthLevel = smartHealthUnknown
		resource.SuggestedAction = smartActionSnapshotStale
		resource.DecisionReason = "inspection_snapshot_incomplete"
		return resource
	}

	capacityItems := make([]smartCapacityItem, 0, len(snapshot.results))
	inspectedFiles := make(map[string]struct{}, len(snapshot.results))
	eligible := 0
	withQuotaEvidence := 0
	usabilityRequired := 0
	withVerifiedUsability := 0
	leaseRequired := 0
	withActiveLease := 0
	for _, result := range snapshot.results {
		if fileName := strings.TrimSpace(result.FileName); fileName != "" {
			inspectedFiles[fileName] = struct{}{}
		}
		if !isSmartCapacityInspectionResult(result) {
			continue
		}
		resource.SchedulableAccounts++
		if inspectionResultCapacityExcluded(result) {
			resource.WeakAccounts++
			continue
		}
		eligible++
		usabilityRequired++
		remaining, hasCapacityQuota := inspectionResultRemainingQuotaFraction(result)
		if hasCapacityQuota {
			withQuotaEvidence++
		}
		// A completed inspection with status=error has quota headers but did
		// not prove that the credential can serve a request. Keep it out of
		// verified capacity and pause automation rather than purchasing against
		// a possibly unavailable account.
		if inspectionResultUsabilityUnverified(result) {
			resource.WeakAccounts++
			continue
		}
		withVerifiedUsability++
		remainingMinutes := float64(smartUsefulAccountLifetimeMinutes())
		hasActiveLease := false
		if smartSupplyManagedFileName(result.FileName) {
			leaseRequired++
			leaseExpiresAtMS, found := snapshot.leaseExpiresByFile[result.FileName]
			if found && leaseExpiresAtMS > now.UnixMilli() {
				withActiveLease++
				hasActiveLease = true
				remainingMinutes = clampFloat(time.UnixMilli(leaseExpiresAtMS).Sub(now).Minutes(), 0, float64(smartUsefulAccountLifetimeMinutes()))
			}
			// A successful current quota probe is stronger evidence than the
			// historical delivery lease. Supplier leases are useful for limiting
			// freshly imported credentials before their first inspection, but an
			// older credential that still returns a 2xx quota response must retain
			// its verified capacity. Keep the missing lease visible through
			// CapacityLifetimeCoverage instead of turning a working credential into
			// zero capacity.
		}
		if !hasCapacityQuota && hasActiveLease {
			withQuotaEvidence++
		}
		capacity := 0.0
		switch {
		case hasCapacityQuota:
			capacity = smartEstimatedAccountCapacityRCU(resource.UnitCapacityRCU, remainingMinutes) * remaining
		case hasActiveLease:
			// The completed probe proved that the credential works, while the
			// supplier's delivery record bounds how long it can remain usable.
			// This is intentionally a conservative lease estimate, not a
			// conversion of the excluded monthly allowance.
			capacity = smartEstimatedAccountCapacityRCU(resource.UnitCapacityRCU, remainingMinutes) * smartNewAccountConfidence(cfg)
			resource.LeaseEstimatedAccounts++
			resource.LeaseEstimatedCapacityRCU += capacity
		default:
			resource.WeakAccounts++
			continue
		}
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
	// A completed inspection is intentionally snapshot based, so an account
	// delivered just after that run used to contribute zero capacity until the
	// next full scan completed. That made a manual purchase appear to have no
	// effect for several minutes. Overlay only active imports that were added
	// after this snapshot, use the configured new-account confidence discount,
	// and keep them visibly separate from verified healthy credentials.
	for _, item := range snapshot.activeImportItems {
		fileName := strings.TrimSpace(item.FileName)
		inspectionStartedAtMS := snapshot.run.StartedAtMS
		if inspectionStartedAtMS <= 0 {
			inspectionStartedAtMS = snapshot.generatedAt.UnixMilli()
		}
		// The inspection's file set is captured at its start, not when its
		// results finish writing. Accounts imported while a long inspection is
		// running are absent from that completed result and need this overlay.
		if fileName == "" || item.ImportedAtMS <= inspectionStartedAtMS || item.LeaseExpiresAtMS <= now.UnixMilli() {
			continue
		}
		if _, alreadyInspected := inspectedFiles[fileName]; alreadyInspected {
			continue
		}
		remainingMinutes := clampFloat(time.UnixMilli(item.LeaseExpiresAtMS).Sub(now).Minutes(), 0, float64(smartUsefulAccountLifetimeMinutes()))
		capacity := smartEstimatedAccountCapacityRCU(resource.UnitCapacityRCU, remainingMinutes) * smartNewAccountConfidence(cfg)
		if capacity <= 0 {
			continue
		}
		capacityItems = append(capacityItems, smartCapacityItem{capacityRCU: capacity, remainingMinutes: remainingMinutes})
		resource.PendingInspectionAccounts++
		resource.PendingInspectionCapacityRCU += capacity
	}
	resource.PendingInspectionCapacityRCU = round2(resource.PendingInspectionCapacityRCU)
	resource.LeaseEstimatedCapacityRCU = round2(resource.LeaseEstimatedCapacityRCU)
	if eligible > 0 {
		resource.CapacityCoverage = round2(float64(withQuotaEvidence) / float64(eligible) * 100)
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
	quotaEvidenceIncomplete := eligible > 0 && withQuotaEvidence != eligible
	usabilityEvidenceIncomplete := usabilityRequired > 0 && withVerifiedUsability != usabilityRequired
	// Delivery-lease coverage is retained as an observability signal. It does
	// not override a current successful quota probe: live usability and quota
	// evidence decide whether a credential contributes capacity.
	for _, item := range capacityItems {
		resource.RawCapacityRCU += item.capacityRCU
	}
	resource.RawCapacityRCU = round2(resource.RawCapacityRCU)
	resource.CurrentCapacityRCU = resource.RawCapacityRCU
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
	// dashboard into 0 RCU, despite most capacity being known. The resulting
	// figure is a lower bound: a bounded purchase may use its capacity deficit
	// while it remains recent, without counting unverified credentials as usable.
	if quotaEvidenceIncomplete || usabilityEvidenceIncomplete {
		if usageStats.successful30 > 0 && consumeRCUPerMinute > 0 {
			recalculateSmartResourceCapacityPlan(cfg, &resource)
		}
		resource.SnapshotFresh = false
		resource.Confidence = smartConfidenceLow
		incompleteReason := "inspection_usability_incomplete"
		if quotaEvidenceIncomplete {
			incompleteReason = "inspection_quota_incomplete"
		}
		resource.DecisionReason = incompleteReason
		if smartPartialInspectionCapacityDeficitAllowed(resource) {
			// Keep the capacity plan derived exclusively from verified credentials.
			// The incomplete result never increases available capacity; it only
			// permits a bounded order against the recent verified lower bound.
			resource.DecisionReason = incompleteReason + "_capacity_deficit"
		} else {
			resource.HealthLevel = smartHealthUnknown
			resource.SuggestedAction = smartActionSnapshotStale
			resource.SuggestedQuantity = 0
		}
		return resource
	}
	if usageStats.successful30 <= 0 || (consumeRCUPerMinute <= 0 && resource.DemandTrend != smartDemandTrendFalling) {
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

	unit := smartProductUnitCapacity(cfg.Product)
	resource.UnitCapacityRCU = unit
	consumeRCUPerMinute := applySmartUsage(&resource, usageStats, unit)
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
	if consumeRCUPerMinute > 0 {
		usableCapacity, wasteRisk := smartExpiryLimitedCapacity(capacityItems, consumeRCUPerMinute)
		resource.TimeLimitedCapacityRCU = round2(usableCapacity)
		resource.ExpiryWasteRiskRCU = round2(wasteRisk)
		resource.CurrentCapacityRCU = resource.TimeLimitedCapacityRCU
	}
	resource.TargetCapacityRCU = round2(consumeRCUPerMinute * float64(resource.EffectiveHealthyMinutes))
	resource.RecommendedCapacityRCU = resource.TargetCapacityRCU

	if usageStats.successful30 <= 0 || (consumeRCUPerMinute <= 0 && resource.DemandTrend != smartDemandTrendFalling) {
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
	if resource == nil {
		return
	}
	if resource.ConsumeRCUPerMinute <= 0 {
		if resource.DemandTrend != smartDemandTrendFalling {
			return
		}
		// A completed zero-traffic minute after a previously observed demand is
		// a valid falling signal, not a cold-start data gap. It has no active
		// capacity target and must visibly pause new short-lived purchases.
		resource.TargetCapacityRCU = 0
		resource.RecommendedCapacityRCU = 0
		resource.EstimatedSustainMinutes = 0
		resource.CapacityGapRCU = 0
		resource.SuggestedQuantity = 0
		resource.HealthLevel = smartHealthHealthy
		resource.SuggestedAction = smartActionObserveDemand
		resource.DecisionReason = "demand_falling_observe"
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
	} else if resource.EstimatedSustainMinutes < float64(resource.CriticalMinutes) {
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
	if resource.DemandTrend == smartDemandTrendFalling {
		// One completed low-minute sample is sufficient to stop creating more
		// short-lived credentials. Keep the calculated health level visible, but
		// pause new purchases while existing reservations follow their normal
		// minimum-hold and release checks.
		resource.SuggestedAction = smartActionObserveDemand
		resource.DecisionReason = "demand_falling_observe"
		return
	}

	if resource.EstimatedSustainMinutes >= float64(resource.EffectiveHealthyMinutes) {
		return
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
	batchLimit := smartAutomaticOrderQuantityLimit(cfg, *resource)
	minimumQuantity := min(smartPrelockMinQuantity(cfg), batchLimit)
	resource.SuggestedQuantity = clampInt(int(math.Ceil(gapForOrder/unitForNew)), minimumQuantity, batchLimit)
	if resource.DemandTrend == smartDemandTrendRising {
		resource.SuggestedQuantity = min(resource.SuggestedQuantity, smartRisingObservationQuantity(cfg, *resource))
		resource.DecisionReason = "demand_rising_observe"
	}
}

type smartUsageAggregate struct {
	requests30     int64
	successful30   int64
	tokens30       int64
	rpm30          float64
	rpm5Peak       float64
	tpm30          float64
	successful1    int64
	successful5    int64
	successful10   int64
	tokens1        int64
	tokens5        int64
	tokens10       int64
	rpm1           float64
	rpm5           float64
	rpm10          float64
	tpm1           float64
	tpm5           float64
	tpm10          float64
	oneMinuteReady bool
	sampleMinutes  int
}

type smartDemandPlan struct {
	consumeRCU  float64
	planningRCU float64
	rcu1        float64
	rcu5        float64
	rcu10       float64
	trend       string
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
	currentMinute := now.UnixMilli() / 60000 * 60000
	lastCompletedMinute := currentMinute - int64(time.Minute/time.Millisecond)
	from5Completed := lastCompletedMinute - 4*int64(time.Minute/time.Millisecond)
	from10Completed := lastCompletedMinute - 9*int64(time.Minute/time.Millisecond)
	firstMinute := int64(0)
	perMinute5 := map[int64]int64{}
	for minute, bucket := range s.smartBuckets {
		if bucket == nil || minute < from30 {
			continue
		}
		result.requests30 += bucket.requests
		result.successful30 += bucket.success
		result.tokens30 += bucket.totalTokens
		if bucket.success > 0 && (firstMinute == 0 || minute < firstMinute) {
			firstMinute = minute
		}
		if minute >= from5 {
			// Failed calls do not consume output/input tokens and should not make
			// the replenishment planner buy capacity for malformed/retried traffic.
			perMinute5[minute] += bucket.success
		}
		// Capacity planning uses whole, completed minute buckets only. The
		// in-progress minute is intentionally excluded so a partial bucket never
		// looks like a sudden demand collapse at the beginning of each minute.
		if minute == lastCompletedMinute {
			result.successful1 += bucket.success
			result.tokens1 += bucket.totalTokens
		}
		if minute >= from5Completed && minute <= lastCompletedMinute {
			result.successful5 += bucket.success
			result.tokens5 += bucket.totalTokens
		}
		if minute >= from10Completed && minute <= lastCompletedMinute {
			result.successful10 += bucket.success
			result.tokens10 += bucket.totalTokens
		}
	}
	if firstMinute > 0 {
		observedMinutes := int(now.Sub(time.UnixMilli(firstMinute)).Minutes()) + 1
		result.sampleMinutes = clampInt(observedMinutes, 1, 30)
	}
	if result.successful30 > 0 {
		// A warm Manager has a persisted 30-minute baseline. A cold Manager
		// divides by its actual observed span instead of pretending the partial
		// in-memory history already covers 30 minutes.
		denominator := math.Max(1, float64(result.sampleMinutes))
		result.rpm30 = float64(result.successful30) / denominator
		result.tpm30 = float64(result.tokens30) / denominator
	}
	// A zero in the most recently completed bucket is a meaningful demand
	// signal once at least two minutes have been observed: it should stop new
	// purchases quickly instead of leaving an old 30-minute average in charge.
	result.oneMinuteReady = result.sampleMinutes >= 2
	if result.oneMinuteReady {
		result.rpm1 = float64(result.successful1)
		result.tpm1 = float64(result.tokens1)
		result.rpm5 = float64(result.successful5) / 5
		result.tpm5 = float64(result.tokens5) / 5
		result.rpm10 = float64(result.successful10) / 10
		result.tpm10 = float64(result.tokens10) / 10
	}
	for _, count := range perMinute5 {
		if float64(count) > result.rpm5Peak {
			result.rpm5Peak = float64(count)
		}
	}
	return result
}

func smartDemandPlanForUsage(usage smartUsageAggregate, unit float64) smartDemandPlan {
	result := smartDemandPlan{
		rcu1:        smartWindowConsumeRCU(usage.rpm1, usage.tpm1, unit),
		rcu5:        smartWindowConsumeRCU(usage.rpm5, usage.tpm5, unit),
		rcu10:       smartWindowConsumeRCU(usage.rpm10, usage.tpm10, unit),
		trend:       smartDemandTrendUnknown,
		consumeRCU:  smartConsumeRCUPerMinute(usage.rpm30, usage.rpm5Peak, usage.tpm30, unit),
		planningRCU: smartConsumeRCUPerMinute(usage.rpm30, usage.rpm5Peak, usage.tpm30, unit),
	}
	if !usage.oneMinuteReady {
		return result
	}

	baseline := math.Max(result.rcu5, result.rcu10)
	result.consumeRCU = result.rcu1
	result.planningRCU = result.rcu1
	result.trend = smartDemandTrendStable
	// A rise is confirmed by comparing the last complete minute with two
	// broader windows. Its immediate burn still drives the health calculation,
	// while purchase sizing is held to the 5/10 minute baseline and capped to a
	// small observation batch elsewhere.
	if result.rcu1 > 0 && (baseline <= 0 || result.rcu1 >= baseline*1.6) {
		result.trend = smartDemandTrendRising
		if baseline > 0 {
			result.planningRCU = baseline
		}
		return result
	}
	// Demand falls are intentionally asymmetric. A completed low one-minute
	// bucket immediately becomes the active rate to prevent buying credentials
	// that will expire within an hour. New orders are paused by the caller.
	if baseline > 0 && result.rcu1 <= baseline*0.55 {
		result.trend = smartDemandTrendFalling
	}
	return result
}

func smartWindowConsumeRCU(rpm, tpm, unit float64) float64 {
	requestRCU := math.Max(0, rpm)
	tokenRCU := 0.0
	if unit > 0 && tpm > 0 {
		tokenRCU = tpm / 1000 / unit
	}
	return math.Max(requestRCU, tokenRCU)
}

func applySmartUsage(resource *SmartResource, usage smartUsageAggregate, unit float64) float64 {
	if resource == nil {
		return 0
	}
	demand := smartDemandPlanForUsage(usage, unit)
	resource.RPM30M = usage.rpm30
	resource.RPM5MPeak = usage.rpm5Peak
	resource.TPM30M = usage.tpm30
	resource.RPM1M = usage.rpm1
	resource.RPM5M = usage.rpm5
	resource.RPM10M = usage.rpm10
	resource.TPM1M = usage.tpm1
	resource.TPM5M = usage.tpm5
	resource.TPM10M = usage.tpm10
	resource.ConsumeRCU1M = round2(demand.rcu1)
	resource.ConsumeRCU5M = round2(demand.rcu5)
	resource.ConsumeRCU10M = round2(demand.rcu10)
	resource.DemandTrend = demand.trend
	resource.DemandPlanningRCUPerMinute = round2(demand.planningRCU)
	resource.ConsumeRCUPerMinute = round2(demand.consumeRCU)
	resource.UsageSampleMinutes = usage.sampleMinutes
	return demand.consumeRCU
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
		return inspectionQuotaSnapshot{
			run:                run,
			results:            filtered,
			leaseExpiresByFile: leaseExpiresByFile,
			activeImportItems:  leaseItems,
			generatedAt:        generatedAt,
		}, nil
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
	items := make([]store.SupplyImportItem, len(snapshot.activeImportItems))
	copy(items, snapshot.activeImportItems)
	snapshot.activeImportItems = items
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

func inspectionResultUsabilityUnverified(result store.CodexInspectionResult) bool {
	if inspectionResultInCooldown(result) {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(result.Status))
	state := strings.ToLower(strings.TrimSpace(result.State))
	if status != "error" && state != "error" {
		return false
	}
	// The file's runtime status may lag behind a completed inspection. A fresh
	// successful quota response with quota evidence proves the credential is
	// usable now, so a stale `status=error` must not remove it from capacity.
	// Failed probes still have no 2xx status (or no quota evidence) and remain
	// excluded by the conservative path below.
	if result.StatusCode != nil && *result.StatusCode >= 200 && *result.StatusCode < 300 &&
		strings.TrimSpace(result.Error) == "" && strings.TrimSpace(result.ErrorKind) == "" &&
		strings.TrimSpace(result.ErrorDetail) == "" {
		_, hasQuota := inspectionResultRemainingQuotaFraction(result)
		return !hasQuota
	}
	return true
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

const (
	smartQuotaFiveHourSeconds = 5 * 60 * 60
	smartQuotaWeekSeconds     = 7 * 24 * 60 * 60
	smartQuotaMonthSeconds    = 30 * 24 * 60 * 60
)

// inspectionResultRemainingQuotaFraction returns the capacity fraction that
// can be used before the credential's next limiting quota window is
// exhausted. The shortest available window is always selected. Monthly quota
// therefore acts only as a fallback for providers that do not expose a 5-hour
// or weekly window; it is never added to a shorter-window allowance.
//
// A credential can expose both five-hour and weekly limits. The shortest
// window is the one that will block it first, so only that window contributes
// to the supply capacity calculation. If multiple windows have the same
// shortest duration, use the lowest remaining fraction as the conservative
// bound. Results written before quota windows were persisted retain the legacy
// UsedPercent fallback.
func inspectionResultRemainingQuotaFraction(result store.CodexInspectionResult) (float64, bool) {
	if len(result.QuotaWindows) == 0 {
		if result.UsedPercent == nil {
			return 0, false
		}
		used := *result.UsedPercent
		if used > 1 {
			used /= 100
		}
		return clampFloat(1-used, 0, 1), true
	}

	shortestSeconds := math.MaxFloat64
	remainingAtShortest := 1.0
	found := false
	for _, window := range result.QuotaWindows {
		if inspectionQuotaWindowExcludedFromCapacity(window) || window.UsedPercent == nil {
			continue
		}
		used := *window.UsedPercent
		if used > 1 {
			used /= 100
		}
		remaining := clampFloat(1-used, 0, 1)
		seconds := inspectionQuotaWindowDurationSeconds(window)
		switch {
		case !found || seconds < shortestSeconds:
			shortestSeconds = seconds
			remainingAtShortest = remaining
			found = true
		case seconds == shortestSeconds:
			remainingAtShortest = math.Min(remainingAtShortest, remaining)
		}
	}
	if found {
		return remainingAtShortest, true
	}
	return 0, false
}

func inspectionQuotaWindowExcludedFromCapacity(window model.CodexInspectionQuotaWindow) bool {
	metadata := strings.ToLower(strings.TrimSpace(window.ID + " " + window.LabelKey))
	if strings.Contains(metadata, "code-review") ||
		strings.Contains(metadata, "code_review") {
		return true
	}
	return false
}

func inspectionQuotaWindowDurationSeconds(window model.CodexInspectionQuotaWindow) float64 {
	if window.LimitWindowSeconds != nil && !math.IsNaN(*window.LimitWindowSeconds) && !math.IsInf(*window.LimitWindowSeconds, 0) && *window.LimitWindowSeconds > 0 {
		return *window.LimitWindowSeconds
	}
	metadata := strings.ToLower(strings.TrimSpace(window.ID + " " + window.LabelKey))
	switch {
	case strings.Contains(metadata, "five-hour"),
		strings.Contains(metadata, "five_hour"),
		strings.Contains(metadata, "5-hour"),
		strings.Contains(metadata, "5_hour"):
		return smartQuotaFiveHourSeconds
	case strings.Contains(metadata, "weekly"),
		strings.Contains(metadata, "seven-day"),
		strings.Contains(metadata, "seven_day"),
		strings.Contains(metadata, "7-day"),
		strings.Contains(metadata, "7_day"):
		return smartQuotaWeekSeconds
	case strings.Contains(metadata, "monthly"),
		strings.Contains(metadata, "month"):
		return smartQuotaMonthSeconds
	default:
		// Keep unclassified windows usable for backward compatibility, but only
		// after every duration that is known or can be inferred.
		return math.MaxFloat64
	}
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

// smartResourceAtOrBelowWarning distinguishes a true warning-water event from
// the ordinary "below healthy target" prelock state. Only the former may use
// the larger ReplenishBatchSize cap and the shorter retry cooldown.
func smartResourceAtOrBelowWarning(resource SmartResource) bool {
	return resource.ConsumeRCUPerMinute > 0 && resource.WarningMinutes > 0 &&
		resource.EstimatedSustainMinutes <= float64(resource.WarningMinutes)
}

// smartPartialInspectionCapacityDeficitAllowed permits a bounded purchase
// from the verified capacity lower bound when usability/quota evidence is
// incomplete. It deliberately excludes missing snapshots and old evidence;
// the 45-minute upper bound accommodates the regular full-pool inspection
// duration while avoiding decisions from a stale historic snapshot.
func smartPartialInspectionCapacityDeficitAllowed(resource SmartResource) bool {
	if resource.CurrentCapacityRCU <= 0 || resource.CapacityGapRCU <= 0 ||
		resource.CapacitySnapshotAtMS <= 0 || resource.CapacitySnapshotAgeSeconds > 45*60 {
		return false
	}
	baseReason := strings.TrimSuffix(resource.DecisionReason, "_capacity_deficit")
	switch baseReason {
	case "inspection_quota_incomplete", "inspection_usability_incomplete":
		return true
	default:
		return false
	}
}

// smartStaleVerifiedLowWaterReadyTakeAllowed is intentionally narrower than
// normal automation: it never creates another order from stale data. It only
// permits taking an already-reserved ready order when the last completed
// inspection has complete quota/usability coverage and its verified lower
// bound remains at or below warning water. This prevents a long inspection of
// a large pool from stranding needed capacity after the normal freshness
// window, without acting on an incomplete inspection.
func smartStaleVerifiedLowWaterReadyTakeAllowed(resource SmartResource) bool {
	if resource.SnapshotFresh || !smartResourceAtOrBelowWarning(resource) ||
		resource.CapacitySource != smartCapacitySourceInspection || resource.CapacitySnapshotRunID <= 0 ||
		resource.CapacityCoverage < 100 || resource.CurrentCapacityRCU < 0 ||
		resource.ConsumeRCUPerMinute <= 0 || resource.CapacitySnapshotAtMS <= 0 ||
		resource.CapacitySnapshotAgeSeconds > 90*60 {
		return false
	}
	return resource.Confidence == smartConfidenceMedium || resource.Confidence == smartConfidenceHigh
}

func smartReplenishBatchLimit(cfg store.ManagerSupplyConfig) int {
	return clampInt(positiveOr(cfg.ReplenishBatchSize, smartPrelockMaxQuantity(cfg)), 1, 100)
}

// smartAutomaticOrderQuantityLimit keeps normal prelocks inside their
// conservative cap. At or below warning water, ReplenishBatchSize becomes the
// active safety cap so a configured 15-account recovery batch is not silently
// reduced by a smaller routine-prelock setting such as 7.
func smartAutomaticOrderQuantityLimit(cfg store.ManagerSupplyConfig, resource SmartResource) int {
	if smartResourceAtOrBelowWarning(resource) {
		return smartReplenishBatchLimit(cfg)
	}
	limit := smartReplenishBatchLimit(cfg)
	if smartPrelockEnabled(cfg) {
		limit = min(limit, smartPrelockMaxQuantity(cfg))
	}
	return max(1, limit)
}

func smartRisingObservationQuantity(cfg store.ManagerSupplyConfig, resource SmartResource) int {
	limit := smartAutomaticOrderQuantityLimit(cfg, resource)
	if limit <= 0 {
		return 1
	}
	// A 1-minute spike is real enough to lower the capacity health immediately,
	// but it is not enough evidence to buy a full one-hour batch. Cap the first
	// reservation to 1/2/3 credentials according to the currently observed
	// shortfall, then let the next complete minute and 5/10-minute windows
	// decide whether another batch is justified.
	unit := smartEstimatedNewAccountCapacityRCU(cfg)
	if unit <= 0 {
		unit = smartEstimatedAccountCapacityRCU(resource.UnitCapacityRCU, float64(smartUsefulAccountLifetimeMinutes()))
	}
	needed := 1
	if unit > 0 && resource.CapacityGapRCU > 0 {
		needed = int(math.Ceil(resource.CapacityGapRCU / unit))
	}
	return clampInt(needed, 1, min(limit, 3))
}

func smartCriticalTakeConfirmRounds(cfg store.ManagerSupplyConfig) int {
	return clampInt(positiveOr(cfg.CriticalTakeConfirmRounds, 2), 1, 5)
}

func smartCreateCooldownSeconds(cfg store.ManagerSupplyConfig) int {
	return clampInt(positiveOr(cfg.CreateCooldownSeconds, 120), 0, 3600)
}

// smartCreateCooldownForResource shortens recovery retries without removing
// the configured cooldown entirely. One open order remains enforced by the
// store, daily limits and balance limits still apply.
func smartCreateCooldownForResource(cfg store.ManagerSupplyConfig, resource SmartResource) int {
	cooldown := smartCreateCooldownSeconds(cfg)
	if !smartResourceAtOrBelowWarning(resource) {
		return cooldown
	}
	if resource.HealthLevel == smartHealthCritical {
		return min(cooldown, 15)
	}
	return min(cooldown, 45)
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
	order := []string{smartActionHealthy, smartActionObserveDemand, smartActionPrelock, smartActionWaitLocked, smartActionReleaseLocked, smartActionTakeLocked, smartActionBalanceBlocked, smartActionInventoryBlocked, smartActionConfigError, smartActionSnapshotStale, smartActionManualReview}
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
