package supply

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/usage"
)

const (
	smartQuotaCalibrationWarmWindow         = 24 * time.Hour
	smartQuotaCalibrationSampleTTL          = 24 * time.Hour
	smartQuotaCalibrationRecentWindow       = 6 * time.Hour
	smartQuotaCalibrationMaxObservationGap  = 2 * time.Hour
	smartQuotaCalibrationMinDelta           = 0.005
	smartQuotaCalibrationMinUsedFraction    = 0.10
	smartQuotaCalibrationMinCapacityM       = 0.5
	smartQuotaCalibrationMaxCapacityM       = 500.0
	smartQuotaCalibrationSamplesPerAccount  = 3
	smartQuotaCalibrationMaxRepresentatives = 24
	smartQuotaCalibrationDivergencePct      = 25.0

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
	lastSampleTokens   int64
	hasFraction        bool
	recoverAtMS        int64
	planType           string
	windowTokens       int64
}

type smartQuotaCalibrationSample struct {
	identity       string
	planType       string
	capacityM      float64
	weight         float64
	usedFraction   float64
	observedMS     int64
	completeWindow bool
}

type smartQuotaCalibrationState struct {
	observations      map[string]smartQuotaCalibrationObservation
	samples           []smartQuotaCalibrationSample
	samplesByIdentity map[string][]smartQuotaCalibrationSample
	directSamples     map[string]smartQuotaCalibrationSample
}

type smartQuotaEstimate struct {
	CapacityM           float64
	Source              string
	SampleCount         int
	EvidenceCount       int
	ObservedPercent     float64
	Confidence          string
	UniqueAccounts      int
	CurrentEstimateM    float64
	RecentEstimateM     float64
	HistoricalEstimateM float64
	DivergencePercent   float64
	IndependentAccount  bool
}

type smartQuotaPlanAdoptionState struct {
	mode                string
	adoptedM            float64
	candidateM          float64
	lastObservedM       float64
	confirmationRounds  int
	lastInspectionRunID int64
	pending             bool
}

const (
	smartQuotaPolicyModeAuto        = "auto"
	smartQuotaPolicyModeFixed       = "fixed"
	smartQuotaPolicyMaxStepFraction = 0.10
	smartQuotaPolicyWarningFraction = 0.10
	smartQuotaPolicyRequiredRounds  = 2
)

type smartQuotaWeightedPoint struct {
	capacityM float64
	weight    float64
}

type smartQuotaWindowEvidence struct {
	fraction    float64
	recoverAtMS int64
	planType    string
	concrete    bool
}

// smartQuotaWindowBaseline is a complete account-scoped Token aggregate for
// the quota window observed by one inspection result. It repairs the in-memory
// tail sampler without carrying raw request rows into the supply planner.
type smartQuotaWindowBaseline struct {
	requestIndex int
	identity     string
	planType     string
	fraction     float64
	fromMS       int64
	toMS         int64
	recoverAtMS  int64
	observedMS   int64
	windowTokens int64
	lastSeenMS   int64
}

func newSmartQuotaCalibrationState() smartQuotaCalibrationState {
	return smartQuotaCalibrationState{
		observations:      make(map[string]smartQuotaCalibrationObservation),
		samples:           make([]smartQuotaCalibrationSample, 0, 256),
		samplesByIdentity: make(map[string][]smartQuotaCalibrationSample),
		directSamples:     make(map[string]smartQuotaCalibrationSample),
	}
}

func (s *Service) ensureSmartQuotaCalibrationStateLocked() {
	if s.smartQuotaState.observations == nil {
		s.smartQuotaState.observations = make(map[string]smartQuotaCalibrationObservation)
	}
	if s.smartQuotaState.samplesByIdentity == nil {
		s.smartQuotaState.samplesByIdentity = make(map[string][]smartQuotaCalibrationSample)
	}
	if s.smartQuotaState.directSamples == nil {
		s.smartQuotaState.directSamples = make(map[string]smartQuotaCalibrationSample)
	}
}

func (s *Service) appendSmartQuotaCalibrationSampleLocked(sample smartQuotaCalibrationSample) {
	if sample.identity == "" {
		return
	}
	s.ensureSmartQuotaCalibrationStateLocked()
	s.smartQuotaState.samples = append(s.smartQuotaState.samples, sample)
	s.smartQuotaState.samplesByIdentity[sample.identity] = append(
		s.smartQuotaState.samplesByIdentity[sample.identity],
		sample,
	)
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

func smartQuotaCalibrationEventEvidence(event usage.Event) (smartQuotaWindowEvidence, bool) {
	metadata := event.ResponseMetadata
	if metadata == nil && strings.TrimSpace(event.ResponseMetadataJSON) != "" {
		var decoded usage.ResponseHeaderMetadata
		if err := json.Unmarshal([]byte(event.ResponseMetadataJSON), &decoded); err == nil {
			metadata = &decoded
		}
	}
	if metadata != nil && metadata.Quota != nil {
		quota := metadata.Quota
		// Codex exposes a short primary window and a 7-day secondary window.
		// The flattened header fields intentionally summarize whichever window
		// is currently more consumed, so they can switch between 5H and 7D from
		// one request to the next. Always prefer the longest concrete window for
		// total-account capacity inference; otherwise cumulative Token history is
		// divided by unrelated percentages and collapses toward one-request size.
		if window := longestSmartQuotaWindow(quota.Primary, quota.Secondary); window != nil && window.UsedPercent != nil {
			fraction, ok := normalizeSmartQuotaFraction(*window.UsedPercent)
			if ok {
				return smartQuotaWindowEvidence{
					fraction:    fraction,
					recoverAtMS: smartQuotaWindowRecoverAtMS(window, event.TimestampMS),
					planType:    strings.ToLower(strings.TrimSpace(quota.PlanType)),
					concrete:    true,
				}, true
			}
		}
		if quota.UsedPercent != nil {
			fraction, ok := normalizeSmartQuotaFraction(*quota.UsedPercent)
			if ok {
				return smartQuotaWindowEvidence{
					fraction:    fraction,
					recoverAtMS: quota.RecoverAtMS,
					planType:    strings.ToLower(strings.TrimSpace(quota.PlanType)),
					concrete:    strings.EqualFold(strings.TrimSpace(quota.SummaryWindowKind), "weekly"),
				}, true
			}
		}
	}
	if event.HeaderQuotaUsedPercent == nil {
		return smartQuotaWindowEvidence{}, false
	}
	fraction, ok := normalizeSmartQuotaFraction(*event.HeaderQuotaUsedPercent)
	if !ok {
		return smartQuotaWindowEvidence{}, false
	}
	return smartQuotaWindowEvidence{
		fraction:    fraction,
		recoverAtMS: event.HeaderQuotaRecoverAtMS,
		planType:    strings.ToLower(strings.TrimSpace(event.HeaderQuotaPlanType)),
		concrete:    false,
	}, true
}

func longestSmartQuotaWindow(windows ...*usage.HeaderQuotaWindow) *usage.HeaderQuotaWindow {
	var best *usage.HeaderQuotaWindow
	bestMinutes := -1.0
	for _, window := range windows {
		if window == nil || window.UsedPercent == nil {
			continue
		}
		minutes := 0.0
		if window.WindowMinutes != nil && *window.WindowMinutes > 0 {
			minutes = *window.WindowMinutes
		}
		// Secondary is normally weekly. If providers omit window_minutes, later
		// candidates win so the longer secondary window remains preferred.
		if best == nil || minutes >= bestMinutes {
			best = window
			bestMinutes = minutes
		}
	}
	return best
}

func smartQuotaWindowRecoverAtMS(window *usage.HeaderQuotaWindow, eventTimestampMS int64) int64 {
	if window == nil {
		return 0
	}
	if window.ResetAtMS > 0 {
		return window.ResetAtMS
	}
	if window.ResetAfterSeconds != nil && *window.ResetAfterSeconds > 0 && eventTimestampMS > 0 {
		return eventTimestampMS + int64(*window.ResetAfterSeconds*1000)
	}
	return 0
}

func smartQuotaWindowBaselinesForInspection(results []store.CodexInspectionResult, run store.CodexInspectionRun) ([]smartQuotaWindowBaseline, []store.SupplyQuotaWindowUsageQuery) {
	baselines := make([]smartQuotaWindowBaseline, 0, len(results))
	targets := make([]store.SupplyQuotaWindowUsageQuery, 0, len(results))
	for _, result := range results {
		fileName := strings.TrimSpace(result.FileName)
		if fileName == "" {
			continue
		}
		window, durationSeconds := longestSmartInspectionQuotaWindow(result.QuotaWindows)
		if window == nil || window.UsedPercent == nil || durationSeconds <= 0 || durationSeconds == math.MaxFloat64 {
			continue
		}
		fraction, ok := normalizeSmartQuotaFraction(*window.UsedPercent)
		if !ok || fraction < smartQuotaCalibrationMinUsedFraction {
			continue
		}
		observedMS := result.CreatedAtMS
		if observedMS <= 0 {
			observedMS = run.FinishedAtMS
		}
		if observedMS <= 0 {
			observedMS = run.UpdatedAtMS
		}
		if observedMS <= 0 {
			continue
		}
		durationMS := int64(durationSeconds * 1000)
		fromMS := observedMS - durationMS
		recoverAtMS := window.ResetAtMS
		if recoverAtMS > observedMS && recoverAtMS-durationMS < observedMS {
			fromMS = recoverAtMS - durationMS
		}
		requestIndex := len(baselines)
		baseline := smartQuotaWindowBaseline{
			requestIndex: requestIndex,
			identity:     "file:" + normalizeSmartQuotaIdentity(fileName),
			planType:     strings.ToLower(strings.TrimSpace(result.PlanType)),
			fraction:     fraction,
			fromMS:       fromMS,
			toMS:         observedMS + 1,
			recoverAtMS:  recoverAtMS,
			observedMS:   observedMS,
		}
		baselines = append(baselines, baseline)
		targets = append(targets, store.SupplyQuotaWindowUsageQuery{
			RequestIndex:     requestIndex,
			AuthFileSnapshot: fileName,
			AuthIndex:        result.AuthIndex,
			FromMS:           baseline.fromMS,
			ToMS:             baseline.toMS,
		})
	}
	return baselines, targets
}

func longestSmartInspectionQuotaWindow(windows []store.CodexInspectionQuotaWindow) (*store.CodexInspectionQuotaWindow, float64) {
	var best *store.CodexInspectionQuotaWindow
	bestSeconds := -1.0
	for index := range windows {
		window := &windows[index]
		if inspectionQuotaWindowExcludedFromCapacity(*window) || window.UsedPercent == nil {
			continue
		}
		seconds := inspectionQuotaWindowDurationSeconds(*window)
		if seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
			continue
		}
		if best == nil || seconds >= bestSeconds {
			best = window
			bestSeconds = seconds
		}
	}
	return best, bestSeconds
}

func (s *Service) recordSmartQuotaWindowBaselines(baselines []smartQuotaWindowBaseline, now time.Time) {
	if s == nil || len(baselines) == 0 {
		return
	}
	s.smartMu.Lock()
	defer s.smartMu.Unlock()
	s.ensureSmartQuotaCalibrationStateLocked()

	for _, baseline := range baselines {
		if baseline.identity == "" || baseline.windowTokens <= 0 || baseline.fraction < smartQuotaCalibrationMinUsedFraction {
			continue
		}
		capacityM := float64(baseline.windowTokens) / baseline.fraction / 1_000_000
		if capacityM < smartQuotaCalibrationMinCapacityM || capacityM > smartQuotaCalibrationMaxCapacityM {
			continue
		}

		// The targeted aggregate is authoritative through observedMS. Discard
		// samples for the same account that came from a truncated global tail,
		// while retaining newer samples produced after this inspection.
		kept := s.smartQuotaState.samples[:0]
		for _, sample := range s.smartQuotaState.samples {
			if sample.identity == baseline.identity && sample.observedMS <= baseline.observedMS {
				continue
			}
			kept = append(kept, sample)
		}
		s.smartQuotaState.samples = kept

		observation := s.smartQuotaState.observations[baseline.identity]
		if observation.recoverAtMS > 0 && baseline.recoverAtMS > 0 && observation.recoverAtMS != baseline.recoverAtMS {
			observation = smartQuotaCalibrationObservation{}
		}
		observation.windowTokens = baseline.windowTokens
		observation.lastEventMS = maxInt64(observation.lastEventMS, maxInt64(baseline.observedMS, baseline.lastSeenMS))
		observation.lastFraction = baseline.fraction
		observation.lastSampleFraction = baseline.fraction
		observation.lastSampleTokens = observation.windowTokens
		observation.hasFraction = true
		if baseline.recoverAtMS > 0 {
			observation.recoverAtMS = baseline.recoverAtMS
		}
		if baseline.planType != "" {
			observation.planType = baseline.planType
		}
		s.smartQuotaState.observations[baseline.identity] = observation
		s.smartQuotaState.directSamples[baseline.identity] = smartQuotaCalibrationSample{
			identity:       baseline.identity,
			planType:       observation.planType,
			capacityM:      capacityM,
			weight:         1,
			usedFraction:   baseline.fraction,
			observedMS:     baseline.observedMS,
			completeWindow: true,
		}
	}
	s.pruneSmartQuotaCalibrationLocked(now)
}

func (s *Service) recordSmartQuotaCalibrationEventsLocked(events []usage.Event, now time.Time) {
	if s == nil || len(events) == 0 {
		return
	}
	s.ensureSmartQuotaCalibrationStateLocked()
	ordered := make([]usage.Event, 0, len(events))
	for _, event := range events {
		if smartQuotaCalibrationEventIdentity(event) == "" {
			continue
		}
		// Header-less events are retained after an observation has started so
		// their Token usage is not lost before the next percentage update.
		hasRawQuotaEvidence := event.ResponseMetadata != nil ||
			strings.TrimSpace(event.ResponseMetadataJSON) != "" ||
			event.HeaderQuotaUsedPercent != nil
		if !hasRawQuotaEvidence && smartQuotaCalibrationEventTokens(event) <= 0 {
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
	evidence, hasQuotaEvidence := smartQuotaCalibrationEventEvidence(event)
	if !hasQuotaEvidence || !evidence.concrete {
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
	fraction := evidence.fraction
	planType := evidence.planType
	gapReset := found && observation.recoverAtMS <= 0 && evidence.recoverAtMS <= 0 &&
		(ts-observation.lastEventMS) > smartQuotaCalibrationMaxObservationGap.Milliseconds()
	windowExpired := found && observation.recoverAtMS > 0 && ts >= observation.recoverAtMS
	reset := !found || gapReset || windowExpired ||
		fraction+smartQuotaCalibrationMinDelta < observation.lastFraction ||
		(evidence.recoverAtMS > 0 && observation.recoverAtMS > 0 && evidence.recoverAtMS != observation.recoverAtMS) ||
		(planType != "" && observation.planType != "" && planType != observation.planType)
	if reset {
		observation = smartQuotaCalibrationObservation{}
	}
	observation.windowTokens += tokens
	observation.lastEventMS = ts
	observation.lastFraction = fraction
	if evidence.recoverAtMS > 0 {
		observation.recoverAtMS = evidence.recoverAtMS
	}
	if planType != "" {
		observation.planType = planType
	}

	if !observation.hasFraction {
		observation.lastSampleFraction = fraction
		observation.lastSampleTokens = observation.windowTokens
		observation.hasFraction = true
		s.smartQuotaState.observations[identity] = observation
		return
	}

	delta := fraction - observation.lastSampleFraction
	deltaTokens := observation.windowTokens - observation.lastSampleTokens
	if deltaTokens > 0 && fraction >= smartQuotaCalibrationMinUsedFraction && delta >= smartQuotaCalibrationMinDelta {
		// Runtime events are valid only as an observed delta. Dividing a partial
		// post-restart Token tail by an absolute 10%+ quota percentage creates the
		// false 8M/30M account estimates seen in production. Complete inspection
		// windows use the independent absolute formula in recordSmartQuotaWindowBaselines.
		capacityM := float64(deltaTokens) / delta / 1_000_000
		if capacityM >= smartQuotaCalibrationMinCapacityM && capacityM <= smartQuotaCalibrationMaxCapacityM {
			s.appendSmartQuotaCalibrationSampleLocked(smartQuotaCalibrationSample{
				identity:     identity,
				planType:     observation.planType,
				capacityM:    capacityM,
				weight:       math.Max(delta, smartQuotaCalibrationMinDelta),
				usedFraction: fraction,
				observedMS:   ts,
			})
		}
		observation.lastSampleFraction = fraction
		observation.lastSampleTokens = observation.windowTokens
	}
	s.smartQuotaState.observations[identity] = observation
}

func (s *Service) pruneSmartQuotaCalibrationLocked(now time.Time) {
	cutoff := now.Add(-smartQuotaCalibrationSampleTTL).UnixMilli()
	s.ensureSmartQuotaCalibrationStateLocked()
	grouped := make(map[string][]smartQuotaCalibrationSample)
	for _, sample := range s.smartQuotaState.samples {
		if sample.observedMS >= cutoff {
			grouped[sample.identity] = append(grouped[sample.identity], sample)
		}
	}
	samples := make([]smartQuotaCalibrationSample, 0, len(grouped)*smartQuotaCalibrationSamplesPerAccount)
	s.smartQuotaState.samplesByIdentity = make(map[string][]smartQuotaCalibrationSample, len(grouped))
	for identity, identitySamples := range grouped {
		sort.SliceStable(identitySamples, func(i, j int) bool {
			return identitySamples[i].observedMS < identitySamples[j].observedMS
		})
		if len(identitySamples) > smartQuotaCalibrationSamplesPerAccount {
			identitySamples = identitySamples[len(identitySamples)-smartQuotaCalibrationSamplesPerAccount:]
		}
		copied := append([]smartQuotaCalibrationSample(nil), identitySamples...)
		s.smartQuotaState.samplesByIdentity[identity] = copied
		samples = append(samples, copied...)
	}
	s.smartQuotaState.samples = samples
	for identity, sample := range s.smartQuotaState.directSamples {
		if sample.observedMS < cutoff {
			delete(s.smartQuotaState.directSamples, identity)
		}
	}
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
	samples := make([]smartQuotaCalibrationSample, 0, len(s.smartQuotaState.samples)+len(s.smartQuotaState.directSamples))
	for _, sample := range s.smartQuotaState.samples {
		if sample.observedMS >= cutoff {
			samples = append(samples, sample)
		}
	}
	for _, sample := range s.smartQuotaState.directSamples {
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
		currentEstimate, currentOK = estimateSmartQuotaCurrentSamplesAt(currentSamples, now)
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
	return defaultSmartQuotaEstimateForPlan(planType)
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
	samples := make([]smartQuotaCalibrationSample, 0, len(normalizedIdentities)*4)
	for identity := range normalizedIdentities {
		if sample, ok := s.smartQuotaState.directSamples[identity]; ok && sample.observedMS >= cutoff {
			samples = append(samples, sample)
		}
		if identitySamples, ok := s.smartQuotaState.samplesByIdentity[identity]; ok {
			for _, sample := range identitySamples {
				if sample.observedMS >= cutoff {
					samples = append(samples, sample)
				}
			}
		}
	}
	// Tests and legacy in-memory state may predate the identity index. Keep a
	// bounded compatibility scan only when no indexed samples were found.
	if len(samples) == 0 {
		for _, sample := range s.smartQuotaState.samples {
			if sample.observedMS >= cutoff {
				if _, ok := normalizedIdentities[sample.identity]; ok {
					samples = append(samples, sample)
				}
			}
		}
	}
	s.smartMu.RUnlock()
	return estimateSmartQuotaCurrentSamplesAt(samples, now)
}

func estimateSmartQuotaCurrentSamplesAt(samples []smartQuotaCalibrationSample, now time.Time) (smartQuotaEstimate, bool) {
	// One complete-window estimate or one observed >=0.5% runtime delta is
	// sufficient for that exact account. Absolute point-in-time ratios never
	// enter this sample set.
	return estimateSmartQuotaSamplesAt(samples, smartQuotaEstimateSourceCurrent, 1, 0.005, now)
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
	// A complete account-window aggregate already implements the independent
	// account formula (window Tokens / used fraction). Peer history remains
	// diagnostic context but must not pull this account back toward an old pool.
	if current.IndependentAccount {
		return current
	}
	if current.DivergencePercent < smartQuotaCalibrationDivergencePct {
		return current
	}

	currentWeight, recentWeight, historicalWeight := 0.80, 0.15, 0.05
	if current.EvidenceCount < 6 || current.ObservedPercent < 2 {
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

func defaultSmartQuotaEstimateForPlan(planType string) smartQuotaEstimate {
	estimate := defaultSmartQuotaEstimate()
	if strings.EqualFold(strings.TrimSpace(planType), "team") {
		estimate.CapacityM = smartDefaultTeamAccountQuotaMillionTokens
	}
	return estimate
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

func smartQuotaFallbackForPlan(planType string) float64 {
	if strings.EqualFold(strings.TrimSpace(planType), "team") {
		return smartDefaultTeamAccountQuotaMillionTokens
	}
	return smartDefaultAccountQuotaMillionTokens
}

func smartQuotaPolicyForPlan(cfg store.ManagerSupplyConfig, planType string) store.ManagerSupplyQuotaEstimationPolicy {
	planType = strings.ToLower(strings.TrimSpace(planType))
	policy := store.ManagerSupplyQuotaEstimationPolicy{
		Mode:      smartQuotaPolicyModeAuto,
		FallbackM: smartQuotaFallbackForPlan(planType),
		FixedM:    smartQuotaFallbackForPlan(planType),
	}
	if configured, ok := cfg.QuotaEstimationPolicies[planType]; ok {
		if strings.EqualFold(configured.Mode, smartQuotaPolicyModeFixed) {
			policy.Mode = smartQuotaPolicyModeFixed
		}
		if configured.FallbackM > 0 {
			policy.FallbackM = clampFloat(configured.FallbackM, smartQuotaCalibrationMinCapacityM, smartQuotaCalibrationMaxCapacityM)
		}
		if configured.FixedM > 0 {
			policy.FixedM = clampFloat(configured.FixedM, smartQuotaCalibrationMinCapacityM, smartQuotaCalibrationMaxCapacityM)
		}
	}
	return policy
}

func smartQuotaEstimateHasValidData(estimate smartQuotaEstimate) bool {
	return estimate.Source != smartQuotaEstimateSourceDefault && estimate.SampleCount > 0 && estimate.CapacityM > 0
}

func smartQuotaMoveAtMostTenPercent(current, target float64) float64 {
	if current <= 0 {
		return target
	}
	return clampFloat(target, current*(1-smartQuotaPolicyMaxStepFraction), current*(1+smartQuotaPolicyMaxStepFraction))
}

func smartQuotaRelativeDifference(left, right float64) float64 {
	if left <= 0 || right <= 0 {
		return 0
	}
	return math.Abs(left-right) / right
}

func (s *Service) smartQuotaPlanEstimatesForInspection(
	cfg store.ManagerSupplyConfig,
	results []store.CodexInspectionResult,
	runID int64,
	now time.Time,
) ([]SmartQuotaPlanEstimate, map[string]smartQuotaEstimate) {
	type planContext struct {
		accounts   int
		identities []string
	}
	contexts := make(map[string]*planContext)
	for planType := range cfg.QuotaEstimationPolicies {
		planType = strings.ToLower(strings.TrimSpace(planType))
		if planType != "" {
			contexts[planType] = &planContext{}
		}
	}
	for _, result := range results {
		if !isSmartCapacityInspectionResult(result) || inspectionResultCapacityExcluded(result) {
			continue
		}
		planType := strings.ToLower(strings.TrimSpace(result.PlanType))
		if planType == "" {
			planType = "unknown"
		}
		context := contexts[planType]
		if context == nil {
			context = &planContext{}
			contexts[planType] = context
		}
		context.accounts++
		if inspectionResultUsabilityUnverified(result) {
			continue
		}
		context.identities = append(context.identities, smartQuotaCalibrationResultIdentities(
			result.FileName,
			result.AuthIndex,
			result.AccountKey,
			result.AccountID,
		)...)
	}
	if len(contexts) == 0 {
		contexts["team"] = &planContext{}
	}

	plans := make([]string, 0, len(contexts))
	for planType := range contexts {
		plans = append(plans, planType)
	}
	sort.Strings(plans)

	s.quotaPolicyMu.Lock()
	defer s.quotaPolicyMu.Unlock()
	if s.quotaPolicyState == nil {
		s.quotaPolicyState = make(map[string]smartQuotaPlanAdoptionState)
	}
	items := make([]SmartQuotaPlanEstimate, 0, len(plans))
	planning := make(map[string]smartQuotaEstimate, len(plans))
	for _, planType := range plans {
		context := contexts[planType]
		policy := smartQuotaPolicyForPlan(cfg, planType)
		observed := s.smartQuotaEstimateForAt(now, planType, context.identities...)
		// Historical samples for a configured type remain useful once that type
		// appears again, but a type with zero current accounts must not raise a
		// calibration warning or influence this pool's ordering decision.
		hasData := context.accounts > 0 && smartQuotaEstimateHasValidData(observed)
		observedM := 0.0
		if hasData {
			observedM = observed.CapacityM
		}
		if !hasData {
			observed = smartQuotaEstimate{
				CapacityM:  policy.FallbackM,
				Source:     smartQuotaEstimateSourceDefault,
				Confidence: smartConfidenceLow,
			}
		}

		state := s.quotaPolicyState[planType]
		if state.mode != policy.Mode || state.adoptedM <= 0 {
			state = smartQuotaPlanAdoptionState{mode: policy.Mode, adoptedM: policy.FallbackM}
		}
		if policy.Mode == smartQuotaPolicyModeFixed {
			state.adoptedM = policy.FixedM
			state.candidateM = policy.FixedM
			state.lastObservedM = observed.CapacityM
			state.confirmationRounds = smartQuotaPolicyRequiredRounds
			state.pending = false
			state.lastInspectionRunID = runID
		} else if !hasData {
			state.adoptedM = policy.FallbackM
			state.candidateM = 0
			state.lastObservedM = 0
			state.confirmationRounds = 0
			state.pending = false
			state.lastInspectionRunID = runID
		} else {
			newInspection := (runID > 0 && runID != state.lastInspectionRunID) ||
				(runID <= 0 && state.lastObservedM <= 0)
			if newInspection {
				candidateShifted := state.candidateM > 0 &&
					smartQuotaRelativeDifference(observed.CapacityM, state.candidateM) > smartQuotaPolicyWarningFraction
				if candidateShifted {
					state.confirmationRounds = 1
				} else {
					state.confirmationRounds++
				}
				state.candidateM = observed.CapacityM
				state.lastObservedM = observed.CapacityM
				state.lastInspectionRunID = runID
				difference := smartQuotaRelativeDifference(observed.CapacityM, state.adoptedM)
				switch {
				case difference <= smartQuotaPolicyWarningFraction:
					state.adoptedM = observed.CapacityM
					state.pending = false
				case state.confirmationRounds >= smartQuotaPolicyRequiredRounds:
					state.adoptedM = smartQuotaMoveAtMostTenPercent(state.adoptedM, observed.CapacityM)
					state.pending = smartQuotaRelativeDifference(observed.CapacityM, state.adoptedM) > 0.001
				default:
					state.pending = true
				}
			}
		}
		s.quotaPolicyState[planType] = state

		orderingBlocked := policy.Mode == smartQuotaPolicyModeAuto && hasData && state.pending &&
			state.confirmationRounds < smartQuotaPolicyRequiredRounds && context.accounts > 0
		divergence := 0.0
		if hasData {
			divergence = smartQuotaRelativeDifference(observed.CapacityM, state.adoptedM) * 100
		}
		source := observed.Source
		if policy.Mode == smartQuotaPolicyModeFixed {
			source = smartQuotaPolicyModeFixed
		}
		items = append(items, SmartQuotaPlanEstimate{
			PlanType:            planType,
			Mode:                policy.Mode,
			AccountCount:        context.accounts,
			FallbackM:           round2(policy.FallbackM),
			FixedM:              round2(policy.FixedM),
			ObservedM:           round2(observedM),
			AdoptedM:            round2(state.adoptedM),
			Source:              source,
			SampleCount:         observed.SampleCount,
			UniqueAccounts:      observed.UniqueAccounts,
			DivergencePercent:   round2(divergence),
			PendingConfirmation: state.pending,
			ConfirmationRounds:  state.confirmationRounds,
			RequiredRounds:      smartQuotaPolicyRequiredRounds,
			OrderingBlocked:     orderingBlocked,
			LastInspectionRunID: state.lastInspectionRunID,
		})
		planningSource := source
		if policy.Mode == smartQuotaPolicyModeAuto && hasData &&
			smartQuotaRelativeDifference(state.adoptedM, observed.CapacityM) > 0.001 {
			planningSource = smartQuotaEstimateSourceRecalibrated
		}
		planningEstimate := observed
		planningEstimate.CapacityM = state.adoptedM
		planningEstimate.Source = planningSource
		planning[planType] = planningEstimate
	}
	return items, planning
}

func smartQuotaPlanningEstimateForPlan(planning map[string]smartQuotaEstimate, planType string) smartQuotaEstimate {
	planType = strings.ToLower(strings.TrimSpace(planType))
	if estimate, ok := planning[planType]; ok && estimate.CapacityM > 0 {
		return estimate
	}
	if estimate, ok := planning["team"]; ok && estimate.CapacityM > 0 {
		return estimate
	}
	for _, estimate := range planning {
		if estimate.CapacityM > 0 {
			return estimate
		}
	}
	return defaultSmartQuotaEstimateForPlan(planType)
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
	// A fixed policy is an explicit operator override. Automatic policies are
	// different: their adopted value is only the default for accounts that do
	// not yet have enough account-scoped evidence. Once this exact account has a
	// valid >=10% sample, use its independent estimate directly and never blend
	// it with the plan default.
	if fallback.Source == smartQuotaPolicyModeFixed && fallback.CapacityM > 0 {
		return fallback
	}
	identities := smartQuotaCalibrationResultIdentities(result.FileName, result.AuthIndex, result.AccountKey, result.AccountID)
	estimate, ok := s.smartQuotaCurrentEstimateForAt(now, identities...)
	if ok {
		estimate.CurrentEstimateM = estimate.CapacityM
		return estimate
	}
	if fallback.CapacityM > 0 {
		return fallback
	}
	return defaultSmartQuotaEstimateForPlan(result.PlanType)
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
	independentAccount := false
	for _, sample := range samples {
		if sample.capacityM < smartQuotaCalibrationMinCapacityM || sample.capacityM > smartQuotaCalibrationMaxCapacityM ||
			sample.weight <= 0 || sample.observedMS <= 0 ||
			sample.usedFraction < smartQuotaCalibrationMinUsedFraction {
			continue
		}
		recencyWeight := smartQuotaSampleRecencyWeight(now, sample.observedMS)
		if recencyWeight <= 0 {
			continue
		}
		valid = append(valid, sample)
		if sample.completeWindow {
			independentAccount = true
		}
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
	accountPoints = selectSmartQuotaRepresentativePoints(accountPoints, smartQuotaCalibrationMaxRepresentatives)
	median := weightedSmartQuotaMedian(accountPoints)
	confidence := smartConfidenceMedium
	if len(accountPoints) >= 12 && totalObservedWeight >= 0.5 {
		confidence = smartConfidenceHigh
	}
	return smartQuotaEstimate{
		CapacityM:          round2(clampFloat(median, smartQuotaCalibrationMinCapacityM, smartQuotaCalibrationMaxCapacityM)),
		Source:             source,
		SampleCount:        len(accountPoints),
		EvidenceCount:      len(valid),
		ObservedPercent:    round2(totalObservedWeight * 100),
		Confidence:         confidence,
		UniqueAccounts:     len(accountPoints),
		IndependentAccount: independentAccount,
	}, true
}

func selectSmartQuotaRepresentativePoints(points []smartQuotaWeightedPoint, limit int) []smartQuotaWeightedPoint {
	if limit <= 0 || len(points) <= limit {
		return points
	}
	ordered := append([]smartQuotaWeightedPoint(nil), points...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].capacityM < ordered[j].capacityM
	})
	selected := make([]smartQuotaWeightedPoint, 0, limit)
	for index := 0; index < limit; index++ {
		position := int(math.Round(float64(index) * float64(len(ordered)-1) / float64(limit-1)))
		selected = append(selected, ordered[position])
	}
	return selected
}

func trimSmartQuotaSampleExtremes(samples []smartQuotaCalibrationSample) []smartQuotaCalibrationSample {
	if len(samples) < 3 {
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
