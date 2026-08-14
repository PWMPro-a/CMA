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
	smartQuotaCalibrationWarmWindow        = 24 * time.Hour
	smartQuotaCalibrationSampleTTL         = 24 * time.Hour
	smartQuotaCalibrationRecentWindow      = 6 * time.Hour
	smartQuotaCalibrationMaxObservationGap = 2 * time.Hour
	smartQuotaCalibrationMinDelta          = 0.0005
	smartQuotaCalibrationMinUsedFraction   = 0.005
	smartQuotaCalibrationMinCapacityM      = 0.5
	smartQuotaCalibrationMaxCapacityM      = 500.0
	smartQuotaCalibrationMaxSamples        = 8_000
	smartQuotaCalibrationDivergencePct     = 25.0

	smartQuotaEstimateSourceDefault      = "default"
	smartQuotaEstimateSourceGlobal       = "runtime_global"
	smartQuotaEstimateSourcePlan         = "runtime_plan"
	smartQuotaEstimateSourceIdentity     = "runtime_identity" // legacy API value
	smartQuotaEstimateSourceCurrent      = "runtime_current"
	smartQuotaEstimateSourceRecentPlan   = "runtime_recent_plan"
	smartQuotaEstimateSourceRecalibrated = "runtime_recalibrated"
)

// smartQuotaCalibrationObservation follows one account inside one quota
// recovery window. windowTokens is the account's real historical usage in that
// window. Combining it with the latest used percentage gives the absolute
// account budget:
//
//	capacity = windowTokens / usedFraction
//
// The UI presents the inverse value as remaining percentage, so this is also
// windowTokens / (1 - remainingFraction).
type smartQuotaCalibrationObservation struct {
	lastEventMS        int64
	lastFraction       float64
	lastSampleFraction float64
	recoverAtMS        int64
	planType           string
	windowTokens       int64
}

type smartQuotaCalibrationSample struct {
	identity     string
	planType     string
	capacityM    float64
	weight       float64
	usedFraction float64
	observedMS   int64
}

type smartQuotaCalibrationState struct {
	observations map[string]smartQuotaCalibrationObservation
	samples      []smartQuotaCalibrationSample
}

type smartQuotaEstimate struct {
	CapacityM           float64
	Source              string
	SampleCount         int
	ObservedPercent     float64
	Confidence          string
	UniqueAccounts      int
	CurrentEstimateM    float64
	RecentEstimateM     float64
	HistoricalEstimateM float64
	DivergencePercent   float64
}

type smartQuotaWeightedPoint struct {
	capacityM float64
	weight    float64
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
	// x-cpa-quota-used-percent is a percentage in the inclusive 0..100 range.
	// Values below one are fractional percentages: 0.5 means 0.5%, not 50%.
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
		if smartQuotaCalibrationEventIdentity(event) == "" {
			continue
		}
		// Header-less events are retained after an observation has started so
		// their Token usage is not lost before the next percentage update.
		if event.HeaderQuotaUsedPercent == nil && smartQuotaCalibrationEventTokens(event) <= 0 {
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
	if identity == "" {
		return
	}
	ts := event.TimestampMS
	if ts <= 0 {
		ts = now.UnixMilli()
	}
	tokens := int64(0)
	if !event.Failed {
		tokens = maxInt64(0, smartQuotaCalibrationEventTokens(event))
	}
	observation, found := s.smartQuotaState.observations[identity]
	if event.HeaderQuotaUsedPercent == nil {
		windowExpired := found && observation.recoverAtMS > 0 && ts >= observation.recoverAtMS
		gapExpired := found && observation.recoverAtMS <= 0 &&
			(ts-observation.lastEventMS) > smartQuotaCalibrationMaxObservationGap.Milliseconds()
		if windowExpired || gapExpired {
			observation = smartQuotaCalibrationObservation{}
		}
		observation.windowTokens += tokens
		observation.lastEventMS = ts
		s.smartQuotaState.observations[identity] = observation
		return
	}
	fraction, ok := normalizeSmartQuotaFraction(*event.HeaderQuotaUsedPercent)
	if !ok {
		return
	}
	planType := strings.ToLower(strings.TrimSpace(event.HeaderQuotaPlanType))
	gapReset := found && observation.recoverAtMS <= 0 && event.HeaderQuotaRecoverAtMS <= 0 &&
		(ts-observation.lastEventMS) > smartQuotaCalibrationMaxObservationGap.Milliseconds()
	windowExpired := found && observation.recoverAtMS > 0 && ts >= observation.recoverAtMS
	reset := !found || gapReset || windowExpired ||
		fraction+smartQuotaCalibrationMinDelta < observation.lastFraction ||
		(event.HeaderQuotaRecoverAtMS > 0 && observation.recoverAtMS > 0 && event.HeaderQuotaRecoverAtMS != observation.recoverAtMS) ||
		(planType != "" && observation.planType != "" && planType != observation.planType)
	if reset {
		observation = smartQuotaCalibrationObservation{}
	}
	observation.windowTokens += tokens
	observation.lastEventMS = ts
	observation.lastFraction = fraction
	if event.HeaderQuotaRecoverAtMS > 0 {
		observation.recoverAtMS = event.HeaderQuotaRecoverAtMS
	}
	if planType != "" {
		observation.planType = planType
	}

	delta := fraction - observation.lastSampleFraction
	if observation.windowTokens > 0 && fraction >= smartQuotaCalibrationMinUsedFraction &&
		(delta >= smartQuotaCalibrationMinDelta || observation.lastSampleFraction == 0) {
		capacityM := float64(observation.windowTokens) / fraction / 1_000_000
		if capacityM >= smartQuotaCalibrationMinCapacityM && capacityM <= smartQuotaCalibrationMaxCapacityM {
			sampleWeight := math.Max(delta, smartQuotaCalibrationMinDelta)
			if observation.lastSampleFraction == 0 {
				// The first percentage seen after startup is a point-in-time ratio,
				// not proof that this process observed the full 0..fraction change.
				// Cap its influence so later current-window samples take precedence.
				sampleWeight = math.Min(sampleWeight, 0.05)
			}
			s.smartQuotaState.samples = append(s.smartQuotaState.samples, smartQuotaCalibrationSample{
				identity:     identity,
				planType:     observation.planType,
				capacityM:    capacityM,
				weight:       sampleWeight,
				usedFraction: fraction,
				observedMS:   ts,
			})
		}
		observation.lastSampleFraction = fraction
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
	for identity, observation := range s.smartQuotaState.observations {
		if observation.lastEventMS < cutoff {
			delete(s.smartQuotaState.observations, identity)
		}
	}
}

func (s *Service) smartQuotaEstimateFor(planType string, identities ...string) smartQuotaEstimate {
	return s.smartQuotaEstimateForAt(time.Now(), planType, identities...)
}

func (s *Service) smartQuotaEstimateForAt(now time.Time, planType string, identities ...string) smartQuotaEstimate {
	if s == nil {
		return defaultSmartQuotaEstimate()
	}
	cutoff := now.Add(-smartQuotaCalibrationSampleTTL).UnixMilli()
	recentCutoff := now.Add(-smartQuotaCalibrationRecentWindow).UnixMilli()
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

	recentSamples := filterSmartQuotaSamples(samples, func(sample smartQuotaCalibrationSample) bool {
		return sample.observedMS >= recentCutoff
	})
	olderSamples := filterSmartQuotaSamples(samples, func(sample smartQuotaCalibrationSample) bool {
		return sample.observedMS < recentCutoff
	})

	var currentEstimate smartQuotaEstimate
	currentOK := false
	if len(normalizedIdentities) > 0 {
		currentSamples := filterSmartQuotaSamples(recentSamples, func(sample smartQuotaCalibrationSample) bool {
			_, ok := normalizedIdentities[sample.identity]
			return ok
		})
		currentEstimate, currentOK = estimateSmartQuotaSamplesAt(
			currentSamples,
			smartQuotaEstimateSourceCurrent,
			2,
			0.005,
			now,
		)
	}

	recentEstimate, recentOK := smartQuotaPlanOrGlobalEstimate(
		recentSamples,
		planType,
		smartQuotaEstimateSourceRecentPlan,
		6,
		0.02,
		10,
		0.03,
		now,
	)
	historicalEstimate, historicalOK := smartQuotaPlanOrGlobalEstimate(
		olderSamples,
		planType,
		smartQuotaEstimateSourcePlan,
		6,
		0.02,
		10,
		0.03,
		now,
	)
	allEstimate, allOK := smartQuotaPlanOrGlobalEstimate(
		samples,
		planType,
		smartQuotaEstimateSourcePlan,
		20,
		0.10,
		30,
		0.15,
		now,
	)

	if currentOK {
		return calibrateSmartQuotaCurrentEstimate(
			currentEstimate,
			recentEstimate,
			recentOK,
			historicalEstimate,
			historicalOK,
		)
	}
	if recentOK {
		recentEstimate.RecentEstimateM = recentEstimate.CapacityM
		if historicalOK {
			recentEstimate.HistoricalEstimateM = historicalEstimate.CapacityM
		}
		return recentEstimate
	}
	if allOK {
		allEstimate.HistoricalEstimateM = allEstimate.CapacityM
		return allEstimate
	}
	return defaultSmartQuotaEstimate()
}

func (s *Service) smartQuotaCurrentEstimateForAt(now time.Time, identities ...string) (smartQuotaEstimate, bool) {
	if s == nil || len(identities) == 0 {
		return smartQuotaEstimate{}, false
	}
	normalizedIdentities := make(map[string]struct{}, len(identities))
	for _, identity := range identities {
		identity = strings.TrimSpace(identity)
		if identity != "" {
			normalizedIdentities[identity] = struct{}{}
		}
	}
	if len(normalizedIdentities) == 0 {
		return smartQuotaEstimate{}, false
	}
	cutoff := now.Add(-smartQuotaCalibrationRecentWindow).UnixMilli()
	s.smartMu.RLock()
	samples := make([]smartQuotaCalibrationSample, 0, 16)
	for _, sample := range s.smartQuotaState.samples {
		if sample.observedMS < cutoff {
			continue
		}
		if _, ok := normalizedIdentities[sample.identity]; ok {
			samples = append(samples, sample)
		}
	}
	s.smartMu.RUnlock()
	return estimateSmartQuotaSamplesAt(samples, smartQuotaEstimateSourceCurrent, 2, 0.005, now)
}

func smartQuotaPlanOrGlobalEstimate(
	samples []smartQuotaCalibrationSample,
	planType string,
	planSource string,
	planMinSamples int,
	planMinWeight float64,
	globalMinSamples int,
	globalMinWeight float64,
	now time.Time,
) (smartQuotaEstimate, bool) {
	if planType != "" {
		planSamples := filterSmartQuotaSamples(samples, func(sample smartQuotaCalibrationSample) bool {
			return sample.planType == planType
		})
		if estimate, ok := estimateSmartQuotaSamplesAt(planSamples, planSource, planMinSamples, planMinWeight, now); ok {
			return estimate, true
		}
	}
	return estimateSmartQuotaSamplesAt(samples, smartQuotaEstimateSourceGlobal, globalMinSamples, globalMinWeight, now)
}

func calibrateSmartQuotaCurrentEstimate(
	current smartQuotaEstimate,
	recent smartQuotaEstimate,
	recentOK bool,
	historical smartQuotaEstimate,
	historicalOK bool,
) smartQuotaEstimate {
	current.Source = smartQuotaEstimateSourceCurrent
	current.CurrentEstimateM = current.CapacityM
	if recentOK {
		current.RecentEstimateM = recent.CapacityM
	}
	if historicalOK {
		current.HistoricalEstimateM = historical.CapacityM
	}
	basis := smartQuotaEstimate{}
	basisOK := false
	if historicalOK {
		basis, basisOK = historical, true
	} else if recentOK {
		basis, basisOK = recent, true
	}
	if !basisOK || basis.CapacityM <= 0 {
		return current
	}
	current.DivergencePercent = round2(math.Abs(current.CapacityM-basis.CapacityM) / basis.CapacityM * 100)
	if current.DivergencePercent < smartQuotaCalibrationDivergencePct {
		return current
	}

	currentWeight, recentWeight, historicalWeight := 0.80, 0.15, 0.05
	if current.SampleCount < 6 || current.ObservedPercent < 2 {
		currentWeight, recentWeight, historicalWeight = 0.60, 0.25, 0.15
	}
	weightedCapacity := current.CapacityM * currentWeight
	totalWeight := currentWeight
	if recentOK {
		weightedCapacity += recent.CapacityM * recentWeight
		totalWeight += recentWeight
	}
	if historicalOK {
		weightedCapacity += historical.CapacityM * historicalWeight
		totalWeight += historicalWeight
	}
	if totalWeight > 0 {
		current.CapacityM = round2(clampFloat(
			weightedCapacity/totalWeight,
			smartQuotaCalibrationMinCapacityM,
			smartQuotaCalibrationMaxCapacityM,
		))
		current.Source = smartQuotaEstimateSourceRecalibrated
	}
	return current
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

func currentSmartQuotaInspectionIdentities(results []store.CodexInspectionResult) []string {
	seen := make(map[string]struct{})
	identities := make([]string, 0, len(results))
	for _, result := range results {
		if !isSmartCapacityInspectionResult(result) || inspectionResultCapacityExcluded(result) || inspectionResultUsabilityUnverified(result) {
			continue
		}
		for _, identity := range smartQuotaCalibrationResultIdentities(result.FileName, result.AuthIndex, result.AccountKey, result.AccountID) {
			if _, ok := seen[identity]; ok {
				continue
			}
			seen[identity] = struct{}{}
			identities = append(identities, identity)
		}
	}
	return identities
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
	resource.AccountQuotaCalibrationUniqueAccounts = estimate.UniqueAccounts
	resource.AccountQuotaCurrentEstimateM = estimate.CurrentEstimateM
	resource.AccountQuotaRecentEstimateM = estimate.RecentEstimateM
	resource.AccountQuotaHistoricalEstimateM = estimate.HistoricalEstimateM
	resource.AccountQuotaDivergencePercent = estimate.DivergencePercent
	applySmartTokenCapacityDefaults(cfg, resource)
}

func (s *Service) smartQuotaEstimateForInspectionResult(result store.CodexInspectionResult, fallback smartQuotaEstimate, now time.Time) smartQuotaEstimate {
	identities := smartQuotaCalibrationResultIdentities(result.FileName, result.AuthIndex, result.AccountKey, result.AccountID)
	estimate, ok := s.smartQuotaCurrentEstimateForAt(now, identities...)
	if ok {
		if fallback.CapacityM > 0 {
			return calibrateSmartQuotaCurrentEstimate(estimate, fallback, true, smartQuotaEstimate{}, false)
		}
		return estimate
	}
	if fallback.CapacityM > 0 {
		return fallback
	}
	return defaultSmartQuotaEstimate()
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
	return estimateSmartQuotaSamplesAt(samples, source, minSamples, minWeight, time.Now())
}

func estimateSmartQuotaSamplesAt(samples []smartQuotaCalibrationSample, source string, minSamples int, minWeight float64, now time.Time) (smartQuotaEstimate, bool) {
	valid := make([]smartQuotaCalibrationSample, 0, len(samples))
	totalObservedWeight := 0.0
	identitySet := make(map[string]struct{})
	for _, sample := range samples {
		if sample.capacityM < smartQuotaCalibrationMinCapacityM || sample.capacityM > smartQuotaCalibrationMaxCapacityM ||
			sample.weight <= 0 || sample.observedMS <= 0 {
			continue
		}
		recencyWeight := smartQuotaSampleRecencyWeight(now, sample.observedMS)
		if recencyWeight <= 0 {
			continue
		}
		valid = append(valid, sample)
		totalObservedWeight += sample.weight * recencyWeight
		identitySet[sample.identity] = struct{}{}
	}
	if len(valid) < minSamples || totalObservedWeight < minWeight {
		return smartQuotaEstimate{}, false
	}

	grouped := make(map[string][]smartQuotaCalibrationSample, len(identitySet))
	for _, sample := range valid {
		grouped[sample.identity] = append(grouped[sample.identity], sample)
	}
	accountPoints := make([]smartQuotaWeightedPoint, 0, len(grouped))
	for _, identitySamples := range grouped {
		// Explicitly discard the highest and lowest estimate for an account once
		// enough observations exist. Percentage rounding, delayed header updates,
		// and a single abnormal response therefore cannot pull the account budget.
		identitySamples = trimSmartQuotaSampleExtremes(identitySamples)
		points := make([]smartQuotaWeightedPoint, 0, len(identitySamples))
		groupWeight := 0.0
		for _, sample := range identitySamples {
			weight := sample.weight * smartQuotaSampleRecencyWeight(now, sample.observedMS)
			// A sample observed late in the current quota window is stronger than
			// an early extrapolation based on only a few consumed percentage points.
			weight *= clampFloat(sample.usedFraction, 0.05, 1)
			if weight <= 0 {
				continue
			}
			points = append(points, smartQuotaWeightedPoint{capacityM: sample.capacityM, weight: weight})
			groupWeight += weight
		}
		if len(points) == 0 || groupWeight <= 0 {
			continue
		}
		accountPoints = append(accountPoints, smartQuotaWeightedPoint{
			capacityM: weightedSmartQuotaMedian(points),
			// Cap one very busy account's influence. Its recent estimate remains
			// primary when it is the current account, while multiple other accounts
			// still provide independent supporting evidence.
			weight: math.Min(groupWeight, 1),
		})
	}
	if len(accountPoints) == 0 {
		return smartQuotaEstimate{}, false
	}
	accountPoints = trimSmartQuotaPointExtremes(accountPoints)
	median := weightedSmartQuotaMedian(accountPoints)
	confidence := smartConfidenceMedium
	if len(valid) >= 30 && len(identitySet) >= 3 && totalObservedWeight >= 0.5 {
		confidence = smartConfidenceHigh
	}
	return smartQuotaEstimate{
		CapacityM:       round2(clampFloat(median, smartQuotaCalibrationMinCapacityM, smartQuotaCalibrationMaxCapacityM)),
		Source:          source,
		SampleCount:     len(valid),
		ObservedPercent: round2(totalObservedWeight * 100),
		Confidence:      confidence,
		UniqueAccounts:  len(identitySet),
	}, true
}

func trimSmartQuotaSampleExtremes(samples []smartQuotaCalibrationSample) []smartQuotaCalibrationSample {
	if len(samples) < 5 {
		return append([]smartQuotaCalibrationSample(nil), samples...)
	}
	ordered := append([]smartQuotaCalibrationSample(nil), samples...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].capacityM < ordered[j].capacityM
	})
	return ordered[1 : len(ordered)-1]
}

func trimSmartQuotaPointExtremes(points []smartQuotaWeightedPoint) []smartQuotaWeightedPoint {
	if len(points) < 5 {
		return points
	}
	ordered := append([]smartQuotaWeightedPoint(nil), points...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].capacityM < ordered[j].capacityM
	})
	return ordered[1 : len(ordered)-1]
}

func weightedSmartQuotaMedian(points []smartQuotaWeightedPoint) float64 {
	if len(points) == 0 {
		return 0
	}
	ordered := append([]smartQuotaWeightedPoint(nil), points...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].capacityM < ordered[j].capacityM
	})
	totalWeight := 0.0
	for _, point := range ordered {
		totalWeight += math.Max(0, point.weight)
	}
	half := totalWeight / 2
	accumulated := 0.0
	for _, point := range ordered {
		accumulated += math.Max(0, point.weight)
		if accumulated >= half {
			return point.capacityM
		}
	}
	return ordered[len(ordered)-1].capacityM
}

func smartQuotaSampleRecencyWeight(now time.Time, observedMS int64) float64 {
	age := now.Sub(time.UnixMilli(observedMS))
	switch {
	case age <= 2*time.Hour:
		return 1
	case age <= 6*time.Hour:
		return 0.60
	case age <= 12*time.Hour:
		return 0.25
	case age <= smartQuotaCalibrationSampleTTL:
		return 0.10
	default:
		return 0
	}
}
