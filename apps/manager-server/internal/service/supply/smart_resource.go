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
)

type SmartResource struct {
	Enabled                 bool    `json:"enabled"`
	HealthLevel             string  `json:"healthLevel"`
	SuggestedAction         string  `json:"suggestedAction"`
	SuggestedQuantity       int     `json:"suggestedQuantity"`
	DecisionReason          string  `json:"decisionReason"`
	Confidence              string  `json:"confidence"`
	SnapshotFresh           bool    `json:"snapshotFresh"`
	GeneratedAtMS           int64   `json:"generatedAtMs"`
	AvailableAccounts       int     `json:"availableAccounts"`
	HealthyAccounts         int     `json:"healthyAccounts"`
	WeakAccounts            int     `json:"weakAccounts"`
	TargetAvailableAccounts int     `json:"targetAvailableAccounts"`
	EstimatedSustainMinutes float64 `json:"estimatedSustainMinutes"`
	HealthyMinutesTarget    int     `json:"healthyMinutesTarget"`
	WarningMinutes          int     `json:"warningMinutes"`
	CriticalMinutes         int     `json:"criticalMinutes"`
	RPM30M                  float64 `json:"rpm30m"`
	RPM5MPeak               float64 `json:"rpm5mPeak"`
	TPM30M                  float64 `json:"tpm30m"`
	CurrentCapacityRCU      float64 `json:"currentCapacityRcu"`
	TargetCapacityRCU       float64 `json:"targetCapacityRcu"`
	CapacityGapRCU          float64 `json:"capacityGapRcu"`
	UnitCapacityRCU         float64 `json:"unitCapacityRcu"`
	RecommendedCapacityRCU  float64 `json:"recommendedCapacityRcu"`
	PrelockedCapacityRCU    float64 `json:"prelockedCapacityRcu,omitempty"`
	UsageSampleMinutes      int     `json:"usageSampleMinutes"`
	AccountCacheAgeSeconds  int     `json:"accountCacheAgeSeconds"`
	LockedOrderID           string  `json:"lockedOrderId,omitempty"`
	LockedOrderAgeSeconds   int     `json:"lockedOrderAgeSeconds,omitempty"`
	LockedConfirmRounds     int     `json:"lockedConfirmRounds,omitempty"`
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

func defaultSmartResource(cfg store.ManagerSupplyConfig) SmartResource {
	return SmartResource{
		Enabled:                 smartSupplyEnabled(cfg),
		HealthLevel:             smartHealthUnknown,
		SuggestedAction:         smartActionSnapshotStale,
		DecisionReason:          "snapshot_not_ready",
		Confidence:              smartConfidenceLow,
		SnapshotFresh:           false,
		GeneratedAtMS:           time.Now().UnixMilli(),
		TargetAvailableAccounts: cfg.TargetAvailableAccounts,
		HealthyMinutesTarget:    smartHealthyMinutesTarget(cfg),
		WarningMinutes:          smartWarningMinutes(cfg),
		CriticalMinutes:         smartCriticalMinutes(cfg),
		UnitCapacityRCU:         smartProductUnitCapacity(cfg.Product),
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
	for _, file := range authSnapshot.files {
		if !isAvailableCodexFile(file) {
			continue
		}
		resource.AvailableAccounts++
		weight := smartAccountHealthWeight(file, accountUsage)
		weightedCapacity += unit * weight
		if weight >= 0.75 {
			resource.HealthyAccounts++
		} else {
			resource.WeakAccounts++
		}
	}
	resource.CurrentCapacityRCU = round2(weightedCapacity)
	consumeRPM := math.Max(resource.RPM30M, resource.RPM5MPeak*0.7)
	resource.TargetCapacityRCU = round2(consumeRPM * float64(resource.HealthyMinutesTarget))
	resource.RecommendedCapacityRCU = resource.TargetCapacityRCU

	if usageStats.requests30 <= 0 || consumeRPM <= 0 {
		resource.Confidence = smartConfidenceLow
		if resource.AvailableAccounts >= cfg.TargetAvailableAccounts {
			resource.HealthLevel = smartHealthHealthy
			resource.SuggestedAction = smartActionHealthy
			resource.DecisionReason = "fallback_account_count_healthy"
			resource.EstimatedSustainMinutes = float64(resource.HealthyMinutesTarget)
			return resource
		}
		gapAccounts := max(0, cfg.TargetAvailableAccounts-resource.AvailableAccounts)
		resource.HealthLevel = smartHealthCritical
		resource.SuggestedAction = smartActionPrelock
		resource.DecisionReason = "fallback_account_deficit"
		resource.CapacityGapRCU = float64(gapAccounts) * unit
		resource.SuggestedQuantity = clampInt(gapAccounts, smartPrelockMinQuantity(cfg), smartPrelockMaxQuantity(cfg))
		return resource
	}

	resource.EstimatedSustainMinutes = round1(resource.CurrentCapacityRCU / consumeRPM)
	resource.CapacityGapRCU = round2(math.Max(0, resource.TargetCapacityRCU-resource.CurrentCapacityRCU))
	if usageStats.sampleMinutes >= 20 && resource.SnapshotFresh {
		resource.Confidence = smartConfidenceHigh
	} else if usageStats.sampleMinutes >= 5 {
		resource.Confidence = smartConfidenceMedium
	} else {
		resource.Confidence = smartConfidenceLow
	}
	if resource.EstimatedSustainMinutes >= float64(resource.HealthyMinutesTarget) {
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
		resource.HealthLevel = smartHealthCritical
		resource.SuggestedAction = smartActionPrelock
		resource.DecisionReason = "capacity_below_warning"
	} else {
		resource.HealthLevel = smartHealthWarning
		resource.SuggestedAction = smartActionPrelock
		resource.DecisionReason = "capacity_below_target"
	}
	unitForNew := unit * smartNewAccountConfidence(cfg)
	if unitForNew <= 0 {
		unitForNew = unit
	}
	resource.SuggestedQuantity = clampInt(int(math.Ceil(resource.CapacityGapRCU/unitForNew)), smartPrelockMinQuantity(cfg), smartPrelockMaxQuantity(cfg))
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
	resource.HealthyMinutesTarget = smartHealthyMinutesTarget(cfg)
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

func smartWarningMinutes(cfg store.ManagerSupplyConfig) int {
	value := positiveOr(cfg.WarningMinutes, 60)
	if value >= smartHealthyMinutesTarget(cfg) {
		value = max(1, smartHealthyMinutesTarget(cfg)/2)
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

func smartAccountHealthWeight(file cpaauthfiles.File, usageStats map[string]smartAccountUsage) float64 {
	if !isAvailableCodexFile(file) {
		return 0
	}
	weight := 1.0
	if runtimeReason := strings.ToLower(textField(file.Raw, "runtime_last_skip_reason", "last_skip_reason", "error_kind", "header_error_kind")); runtimeReason != "" {
		if strings.Contains(runtimeReason, "rate") || strings.Contains(runtimeReason, "quota") || strings.Contains(runtimeReason, "frozen") {
			weight *= 0.45
		}
		if strings.Contains(runtimeReason, "stability_budget_exhausted") {
			weight *= 0.35
		}
	}
	if usedPercent := numberField(file.Raw, "header_quota_used_percent", "quota_used_percent", "used_percent", "usage_percent"); usedPercent > 0 {
		if usedPercent > 1 {
			usedPercent = usedPercent / 100
		}
		remaining := 1 - usedPercent
		switch {
		case remaining <= 0.05:
			weight *= 0.2
		case remaining <= 0.15:
			weight *= 0.45
		case remaining <= 0.30:
			weight *= 0.7
		}
	}
	recoverAt := int64(numberField(file.Raw, "header_quota_recover_at_ms", "quota_recover_at_ms", "quotaRecoverAtMs"))
	if recoverAt > time.Now().UnixMilli() {
		weight *= 0.35
	}
	success := int64(numberField(file.Raw, "success", "success_count", "successCalls"))
	failed := int64(numberField(file.Raw, "failed", "failure_count", "failureCalls"))
	if total := success + failed; total >= 10 {
		rate := float64(success) / float64(total)
		switch {
		case rate < 0.60:
			weight *= 0.2
		case rate < 0.80:
			weight *= 0.5
		case rate < 0.92:
			weight *= 0.8
		}
	}
	for _, key := range smartFileAccountKeys(file) {
		if stats, ok := usageStats[key]; ok && stats.requests > 0 {
			rate := float64(stats.success) / float64(stats.requests)
			zeroRate := float64(stats.zeroTokens) / float64(stats.requests)
			switch {
			case rate < 0.60:
				weight *= 0.2
			case rate < 0.80:
				weight *= 0.5
			case rate < 0.92:
				weight *= 0.8
			}
			if zeroRate > 0.25 {
				weight *= 0.3
			} else if zeroRate > 0.10 {
				weight *= 0.65
			}
			break
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
	for _, key := range keys {
		value, ok := values[key]
		if !ok || value == nil {
			continue
		}
		switch typed := value.(type) {
		case int:
			return float64(typed)
		case int64:
			return float64(typed)
		case float64:
			return typed
		case jsonNumber:
			parsed, _ := typed.Float64()
			return parsed
		case string:
			if parsed, ok := parseFloat(typed); ok {
				return parsed
			}
		}
	}
	return 0
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
