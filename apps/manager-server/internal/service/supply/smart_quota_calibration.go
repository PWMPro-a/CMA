package supply

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/usage"
)

const (
	smartQuotaCalibrationWarmWindow        = 6 * time.Hour
	smartQuotaCalibrationSampleTTL         = 24 * time.Hour
	smartQuotaCalibrationMaxObservationGap = 2 * time.Hour
	smartQuotaCalibrationMinDelta          = 0.0005
	smartQuotaCalibrationMinCapacityM      = 0.5
	smartQuotaCalibrationMaxCapacityM      = 500.0
	smartQuotaCalibrationMaxSamples        = 8_000

	smartQuotaEstimateSourceDefault  = "default"
	smartQuotaEstimateSourceGlobal   = "runtime_global"
	smartQuotaEstimateSourcePlan     = "runtime_plan"
	smartQuotaEstimateSourceIdentity = "runtime_identity"
)

type smartQuotaCalibrationObservation struct {
	lastEventMS   int64
	baseFraction  float64
	recoverAtMS   int64
	planType      string
	pendingTokens int64
}

type smartQuotaCalibrationSample struct {
	identity   string
	planType   string
	capacityM  float64
	weight     float64
	observedMS int64
}

type smartQuotaCalibrationState struct {
	observations map[string]smartQuotaCalibrationObservation
	samples      []smartQuotaCalibrationSample
}

type smartQuotaEstimate struct {
	CapacityM       float64
	Source          string
	SampleCount     int
	ObservedPercent float64
	Confidence      string
}

func newSmartQuotaCalibrationState() smartQuotaCalibrationState {
	return smartQuotaCalibrationState{
		observations: make(map[string]smartQuotaCalibrationObservation),
		samples:      make([]smartQuotaCalibrationSample, 0, 512),
	}
}

func normalizeSmartQuotaFraction(value float64) (float64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
		return 0, false
	}
	// x-cpa-quota-used-percent is stored as a percentage in the inclusive
	// 0..100 range. Values below one are fractional percentages (for example
	// 0.5 means 0.5%), not already-normalized fractions. Treating 0.5 as 50%
	// gives those early-window samples 100x weight and collapses the inferred
	// account capacity.
	return value / 100, true
}

func smartQuotaCalibrationEventIdentity(event usage.Event) string {
	if value := normalizeSmartQuotaIdentity(event.AuthFileSnapshot); value != "" {
		return "file:" + value
	}
	if value := normalizeSmartQuotaIdentity(event.AuthIndex); value != "" {
		return "auth:" + value
	}
	if value := normalizeSmartQuotaIdentity(event.AccountSnapshot); value != "" {
		return "account:" + value
	}
	return ""
}

func normalizeSmartQuotaIdentity(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func smartQuotaCalibrationResultIdentities(fileName, authIndex, accountKey, accountID string) []string {
	values := []struct {
		prefix string
		value  string
	}{
		{prefix: "file:", value: fileName},
		{prefix: "auth:", value: authIndex},
		{prefix: "account:", value: accountKey},
		{prefix: "account:", value: accountID},
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, item := range values {
		value := normalizeSmartQuotaIdentity(item.value)
		if value == "" {
			continue
		}
		key := item.prefix + value
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, key)
	}
	return result
}

func smartQuotaCalibrationEventTokens(event usage.Event) int64 {
	return maxInt64(event.TotalTokens, event.InputTokens+event.OutputTokens+event.ReasoningTokens)
}

func (s *Service) recordSmartQuotaCalibrationEventsLocked(events []usage.Event, now time.Time) {
	if s == nil || len(events) == 0 {
		return
	}
	if s.smartQuotaState.observations == nil {
		s.smartQuotaState = newSmartQuotaCalibrationState()
	}
	ordered := make([]usage.Event, 0, len(events))
	for _, event := range events {
		if event.HeaderQuotaUsedPercent == nil || smartQuotaCalibrationEventIdentity(event) == "" {
			continue
		}
		ordered = append(ordered, event)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].TimestampMS < ordered[j].TimestampMS
	})
	for _, event := range ordered {
		s.recordSmartQuotaCalibrationEventLocked(event, now)
	}
	s.pruneSmartQuotaCalibrationLocked(now)
}

func (s *Service) recordSmartQuotaCalibrationEventLocked(event usage.Event, now time.Time) {
	identity := smartQuotaCalibrationEventIdentity(event)
	if identity == "" || event.HeaderQuotaUsedPercent == nil {
		return
	}
	fraction, ok := normalizeSmartQuotaFraction(*event.HeaderQuotaUsedPercent)
	if !ok {
		return
	}
	ts := event.TimestampMS
	if ts <= 0 {
		ts = now.UnixMilli()
	}
	planType := strings.ToLower(strings.TrimSpace(event.HeaderQuotaPlanType))
	observation, found := s.smartQuotaState.observations[identity]
	reset := !found ||
		(ts-observation.lastEventMS) > smartQuotaCalibrationMaxObservationGap.Milliseconds() ||
		fraction+smartQuotaCalibrationMinDelta < observation.baseFraction ||
		(event.HeaderQuotaRecoverAtMS > 0 && observation.recoverAtMS > 0 && event.HeaderQuotaRecoverAtMS != observation.recoverAtMS) ||
		(planType != "" && observation.planType != "" && planType != observation.planType)
	if reset {
		s.smartQuotaState.observations[identity] = smartQuotaCalibrationObservation{
			lastEventMS:  ts,
			baseFraction: fraction,
			recoverAtMS:  event.HeaderQuotaRecoverAtMS,
			planType:     planType,
		}
		return
	}
	if !event.Failed {
		observation.pendingTokens += maxInt64(0, smartQuotaCalibrationEventTokens(event))
	}
	observation.lastEventMS = ts
	if event.HeaderQuotaRecoverAtMS > 0 {
		observation.recoverAtMS = event.HeaderQuotaRecoverAtMS
	}
	if planType != "" {
		observation.planType = planType
	}
	delta := fraction - observation.baseFraction
	if delta >= smartQuotaCalibrationMinDelta {
		capacityM := float64(observation.pendingTokens) / delta / 1_000_000
		if observation.pendingTokens > 0 && capacityM >= smartQuotaCalibrationMinCapacityM && capacityM <= smartQuotaCalibrationMaxCapacityM {
			s.smartQuotaState.samples = append(s.smartQuotaState.samples, smartQuotaCalibrationSample{
				identity:   identity,
				planType:   observation.planType,
				capacityM:  capacityM,
				weight:     delta,
				observedMS: ts,
			})
		}
		observation.baseFraction = fraction
		observation.pendingTokens = 0
	}
	s.smartQuotaState.observations[identity] = observation
}

func (s *Service) pruneSmartQuotaCalibrationLocked(now time.Time) {
	cutoff := now.Add(-smartQuotaCalibrationSampleTTL).UnixMilli()
	samples := s.smartQuotaState.samples[:0]
	for _, sample := range s.smartQuotaState.samples {
		if sample.observedMS >= cutoff {
			samples = append(samples, sample)
		}
	}
	if len(samples) > smartQuotaCalibrationMaxSamples {
		samples = samples[len(samples)-smartQuotaCalibrationMaxSamples:]
	}
	s.smartQuotaState.samples = samples
	observationCutoff := now.Add(-smartQuotaCalibrationMaxObservationGap).UnixMilli()
	for identity, observation := range s.smartQuotaState.observations {
		if observation.lastEventMS < observationCutoff {
			delete(s.smartQuotaState.observations, identity)
		}
	}
}

func (s *Service) smartQuotaEstimateFor(planType string, identities ...string) smartQuotaEstimate {
	if s == nil {
		return defaultSmartQuotaEstimate()
	}
	now := time.Now()
	cutoff := now.Add(-smartQuotaCalibrationSampleTTL).UnixMilli()
	planType = strings.ToLower(strings.TrimSpace(planType))
	normalizedIdentities := make(map[string]struct{}, len(identities))
	for _, identity := range identities {
		identity = strings.TrimSpace(identity)
		if identity != "" {
			normalizedIdentities[identity] = struct{}{}
		}
	}
	s.smartMu.RLock()
	samples := make([]smartQuotaCalibrationSample, 0, len(s.smartQuotaState.samples))
	for _, sample := range s.smartQuotaState.samples {
		if sample.observedMS >= cutoff {
			samples = append(samples, sample)
		}
	}
	s.smartMu.RUnlock()
	if len(normalizedIdentities) > 0 {
		identitySamples := filterSmartQuotaSamples(samples, func(sample smartQuotaCalibrationSample) bool {
			_, ok := normalizedIdentities[sample.identity]
			return ok
		})
		if estimate, ok := estimateSmartQuotaSamples(identitySamples, smartQuotaEstimateSourceIdentity, 2, 0.005); ok {
			return estimate
		}
	}
	if planType != "" {
		planSamples := filterSmartQuotaSamples(samples, func(sample smartQuotaCalibrationSample) bool {
			return sample.planType == planType
		})
		if estimate, ok := estimateSmartQuotaSamples(planSamples, smartQuotaEstimateSourcePlan, 20, 0.10); ok {
			return estimate
		}
	}
	if estimate, ok := estimateSmartQuotaSamples(samples, smartQuotaEstimateSourceGlobal, 30, 0.15); ok {
		return estimate
	}
	return defaultSmartQuotaEstimate()
}

func defaultSmartQuotaEstimate() smartQuotaEstimate {
	return smartQuotaEstimate{
		CapacityM:  smartDefaultAccountQuotaMillionTokens,
		Source:     smartQuotaEstimateSourceDefault,
		Confidence: smartConfidenceLow,
	}
}

func dominantSmartQuotaPlan(results []store.CodexInspectionResult) string {
	counts := make(map[string]int)
	for _, result := range results {
		if !isSmartCapacityInspectionResult(result) || inspectionResultCapacityExcluded(result) {
			continue
		}
		plan := strings.ToLower(strings.TrimSpace(result.PlanType))
		if plan != "" {
			counts[plan]++
		}
	}
	best := ""
	bestCount := 0
	for plan, count := range counts {
		if count > bestCount || (count == bestCount && plan < best) {
			best = plan
			bestCount = count
		}
	}
	return best
}

func (s *Service) applySmartQuotaEstimate(cfg store.ManagerSupplyConfig, resource *SmartResource, estimate smartQuotaEstimate) {
	if resource == nil {
		return
	}
	if estimate.CapacityM <= 0 {
		estimate = defaultSmartQuotaEstimate()
	}
	resource.AccountQuotaEstimateM = estimate.CapacityM
	resource.AccountQuotaEstimateSource = estimate.Source
	resource.AccountQuotaCalibrationConfidence = estimate.Confidence
	resource.AccountQuotaCalibrationSamples = estimate.SampleCount
	resource.AccountQuotaCalibrationObservedPct = estimate.ObservedPercent
	applySmartTokenCapacityDefaults(cfg, resource)
}

func (s *Service) smartQuotaEstimateForInspectionResult(result store.CodexInspectionResult, fallback smartQuotaEstimate) smartQuotaEstimate {
	identities := smartQuotaCalibrationResultIdentities(result.FileName, result.AuthIndex, result.AccountKey, result.AccountID)
	estimate := s.smartQuotaEstimateFor(result.PlanType, identities...)
	if estimate.Source == smartQuotaEstimateSourceDefault && fallback.CapacityM > 0 {
		return fallback
	}
	return estimate
}

func filterSmartQuotaSamples(samples []smartQuotaCalibrationSample, keep func(smartQuotaCalibrationSample) bool) []smartQuotaCalibrationSample {
	result := make([]smartQuotaCalibrationSample, 0, len(samples))
	for _, sample := range samples {
		if keep(sample) {
			result = append(result, sample)
		}
	}
	return result
}

func estimateSmartQuotaSamples(samples []smartQuotaCalibrationSample, source string, minSamples int, minWeight float64) (smartQuotaEstimate, bool) {
	if len(samples) < minSamples {
		return smartQuotaEstimate{}, false
	}
	totalWeight := 0.0
	for _, sample := range samples {
		totalWeight += math.Max(0, sample.weight)
	}
	if totalWeight < minWeight {
		return smartQuotaEstimate{}, false
	}
	ordered := append([]smartQuotaCalibrationSample(nil), samples...)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].capacityM < ordered[j].capacityM
	})
	half := totalWeight / 2
	accumulated := 0.0
	median := ordered[len(ordered)-1].capacityM
	for _, sample := range ordered {
		accumulated += math.Max(0, sample.weight)
		if accumulated >= half {
			median = sample.capacityM
			break
		}
	}
	confidence := smartConfidenceMedium
	if len(samples) >= 100 && totalWeight >= 1 {
		confidence = smartConfidenceHigh
	}
	return smartQuotaEstimate{
		CapacityM:       round2(clampFloat(median, smartQuotaCalibrationMinCapacityM, smartQuotaCalibrationMaxCapacityM)),
		Source:          source,
		SampleCount:     len(samples),
		ObservedPercent: round2(totalWeight * 100),
		Confidence:      confidence,
	}, true
}
