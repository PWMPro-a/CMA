package supply

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
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
	smartQuotaClassificationMinDelta        = 0.01
	smartQuotaClassificationMinUsedFraction = 0.02
	smartQuotaCalibrationMinUsedFraction    = 0.10
	smartQuotaCalibrationMinCapacityM       = 0.5
	smartQuotaCalibrationMaxCapacityM       = 500.0
	smartQuotaCalibrationSamplesPerAccount  = 3
	smartQuotaCalibrationMinRuntimeSamples  = 3
	smartQuotaCalibrationMinObservedDelta   = 0.10
	smartQuotaCalibrationMaxSampleDeviation = 0.25
	smartQuotaCalibrationMaxRepresentatives = 24
	smartQuotaCalibrationDivergencePct      = 25.0

	smartQuotaEstimateSourceDefault      = "default"
	smartQuotaEstimateSourceGlobal       = "runtime_global"
	smartQuotaEstimateSourcePlan         = "runtime_plan"
	smartQuotaEstimateSourceIdentity     = "runtime_identity" // legacy API value
	smartQuotaEstimateSourceCurrent      = "runtime_current"
	smartQuotaEstimateSourceClassified   = "runtime_classified"
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
	lastEventMS               int64
	lastFraction              float64
	lastSampleFraction        float64
	lastSampleTokens          int64
	hasFraction               bool
	recoverAtMS               int64
	credentialEffectiveFromMS int64
	supplierID                string
	planType                  string
	windowTokens              int64
}

type smartQuotaCalibrationSample struct {
	identity           string
	supplierID         string
	planType           string
	capacityM          float64
	weight             float64
	usedFraction       float64
	observedMS         int64
	completeWindow     bool
	classificationOnly bool
}

type smartQuotaCalibrationState struct {
	observations       map[string]smartQuotaCalibrationObservation
	samples            []smartQuotaCalibrationSample
	samplesByIdentity  map[string][]smartQuotaCalibrationSample
	directSamples      map[string]smartQuotaCalibrationSample
	provisionalSamples map[string]smartQuotaCalibrationSample
}

type smartQuotaEstimate struct {
	CapacityM              float64
	Source                 string
	SampleCount            int
	EvidenceCount          int
	ObservedPercent        float64
	Confidence             string
	UniqueAccounts         int
	CompleteWindowAccounts int
	CurrentEstimateM       float64
	RecentEstimateM        float64
	HistoricalEstimateM    float64
	DivergencePercent      float64
	IndependentAccount     bool
	Provisional            bool
	FallbackOnly           bool
	QuotaClassID           string
	QuotaClasses           []SmartQuotaClassEstimate
}

type smartQuotaPlanAdoptionState struct {
	mode                string
	adoptedM            float64
	candidateM          float64
	lastObservedM       float64
	confirmationRounds  int
	requiredRounds      int
	lastInspectionRunID int64
	pending             bool
	validationState     string
}

const (
	smartQuotaPolicyModeAuto               = "auto"
	smartQuotaPolicyModeFixed              = "fixed"
	smartQuotaPolicyMaxStepFraction        = 0.10
	smartQuotaPolicyWarningFraction        = 0.10
	smartQuotaPolicyModerateDivergence     = 0.25
	smartQuotaPolicyExtremeDivergence      = 0.50
	smartQuotaPolicyRequiredRounds         = 2
	smartQuotaPolicyModerateRequiredRounds = 3
	smartQuotaPolicyExtremeRequiredRounds  = 5
	smartQuotaPolicyMinUniqueAccounts      = 3
	smartQuotaValidationFixed              = "fixed"
	smartQuotaValidationInsufficient       = "insufficient"
	smartQuotaValidationConfirming         = "confirming"
	smartQuotaValidationQuarantined        = "quarantined"
	smartQuotaValidationAccepted           = "accepted"
)

type smartQuotaWeightedPoint struct {
	capacityM float64
	weight    float64
}

type smartQuotaClassPoint struct {
	identity  string
	capacityM float64
	weight    float64
	trusted   bool
}

type smartQuotaClassGroup struct {
	points []smartQuotaClassPoint
}

type smartQuotaWindowEvidence struct {
	fraction    float64
	recoverAtMS int64
	planType    string
	concrete    bool
}

// smartQuotaWindowBaseline is an account-scoped Token aggregate for the quota
// window observed by one inspection result. firstSeenMS and the current
// credential's effective time prove whether the local database covered one
// unchanged provider window; an account imported or replaced mid-window must
// not divide mixed local usage by the replacement's absolute used percentage.
type smartQuotaWindowBaseline struct {
	requestIndex              int
	identity                  string
	supplierID                string
	planType                  string
	fraction                  float64
	fromMS                    int64
	toMS                      int64
	recoverAtMS               int64
	observedMS                int64
	credentialEffectiveFromMS int64
	windowTokens              int64
	firstSeenMS               int64
	lastSeenMS                int64
}

const smartQuotaCompleteWindowCoverageSlack = 5 * time.Minute

func newSmartQuotaCalibrationState() smartQuotaCalibrationState {
	return smartQuotaCalibrationState{
		observations:       make(map[string]smartQuotaCalibrationObservation),
		samples:            make([]smartQuotaCalibrationSample, 0, 256),
		samplesByIdentity:  make(map[string][]smartQuotaCalibrationSample),
		directSamples:      make(map[string]smartQuotaCalibrationSample),
		provisionalSamples: make(map[string]smartQuotaCalibrationSample),
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
	if s.smartQuotaState.provisionalSamples == nil {
		s.smartQuotaState.provisionalSamples = make(map[string]smartQuotaCalibrationSample)
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

func (s *Service) assignSmartQuotaSupplierToIdentityLocked(identity, supplierID string) {
	identity = strings.TrimSpace(identity)
	supplierID = normalizeSmartQuotaSupplierID(supplierID)
	if identity == "" || supplierID == "" {
		return
	}
	for index := range s.smartQuotaState.samples {
		sample := &s.smartQuotaState.samples[index]
		if sample.identity == identity && normalizeSmartQuotaSupplierID(sample.supplierID) == "" {
			sample.supplierID = supplierID
		}
	}
	if samples, ok := s.smartQuotaState.samplesByIdentity[identity]; ok {
		for index := range samples {
			if normalizeSmartQuotaSupplierID(samples[index].supplierID) == "" {
				samples[index].supplierID = supplierID
			}
		}
		s.smartQuotaState.samplesByIdentity[identity] = samples
	}
	if sample, ok := s.smartQuotaState.directSamples[identity]; ok && normalizeSmartQuotaSupplierID(sample.supplierID) == "" {
		sample.supplierID = supplierID
		s.smartQuotaState.directSamples[identity] = sample
	}
	if sample, ok := s.smartQuotaState.provisionalSamples[identity]; ok && normalizeSmartQuotaSupplierID(sample.supplierID) == "" {
		sample.supplierID = supplierID
		s.smartQuotaState.provisionalSamples[identity] = sample
	}
}

func (s *Service) removeSmartQuotaSamplesThroughLocked(identity string, observedMS int64) {
	kept := s.smartQuotaState.samples[:0]
	for _, sample := range s.smartQuotaState.samples {
		if sample.identity == identity && sample.observedMS <= observedMS {
			continue
		}
		kept = append(kept, sample)
	}
	s.smartQuotaState.samples = kept
	if samples, ok := s.smartQuotaState.samplesByIdentity[identity]; ok {
		keptByIdentity := samples[:0]
		for _, sample := range samples {
			if sample.observedMS <= observedMS {
				continue
			}
			keptByIdentity = append(keptByIdentity, sample)
		}
		if len(keptByIdentity) == 0 {
			delete(s.smartQuotaState.samplesByIdentity, identity)
		} else {
			s.smartQuotaState.samplesByIdentity[identity] = keptByIdentity
		}
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

func smartQuotaCalibrationUsedFractionEligible(fraction float64) bool {
	// Ten percent is still too sensitive to provider percentage rounding. An
	// account becomes calibration evidence only after it has moved strictly past
	// the 10% mark.
	return fraction > smartQuotaCalibrationMinUsedFraction
}

func smartQuotaClassificationFractionEligible(fraction float64) bool {
	return fraction >= smartQuotaClassificationMinUsedFraction
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

func normalizeSmartQuotaSupplierID(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func smartQuotaContextKey(supplierID, planType string) string {
	return normalizeSmartQuotaSupplierID(supplierID) + "\x00" + strings.ToLower(strings.TrimSpace(planType))
}

func smartQuotaPublicContextKey(supplierID, planType string) string {
	supplierID = normalizeSmartQuotaSupplierID(supplierID)
	if supplierID == "" {
		supplierID = "unassigned"
	}
	return supplierID + ":" + strings.ToLower(strings.TrimSpace(planType))
}

func smartQuotaCalibrationResultIdentities(fileName, authIndex, accountKey, accountID string) []string {
	credentialValues := []struct {
		prefix string
		value  string
	}{
		{prefix: "file:", value: fileName},
		{prefix: "auth:", value: authIndex},
	}
	accountValues := []struct {
		prefix string
		value  string
	}{
		{prefix: "account:", value: accountKey},
		{prefix: "account:", value: accountID},
	}
	result := make([]string, 0, len(credentialValues))
	seen := make(map[string]struct{}, len(credentialValues))
	appendValues := func(values []struct {
		prefix string
		value  string
	}) {
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
	}
	appendValues(credentialValues)
	if len(result) > 0 {
		// Team deliveries may expose multiple independent spaces under one shared
		// account_id. File/auth identities are space-specific; adding the shared
		// account alias here would merge sibling quota samples and corrupt each
		// space's independent capacity estimate.
		return result
	}
	result = make([]string, 0, len(accountValues))
	seen = make(map[string]struct{}, len(accountValues))
	for _, item := range accountValues {
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

func smartQuotaWindowBaselinesForInspection(
	results []store.CodexInspectionResult,
	run store.CodexInspectionRun,
	supplierByFile map[string]string,
	credentialEffectiveFromByFile map[string]int64,
) ([]smartQuotaWindowBaseline, []store.SupplyQuotaWindowUsageQuery) {
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
		if !ok || !smartQuotaClassificationFractionEligible(fraction) {
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
			requestIndex:              requestIndex,
			identity:                  "file:" + normalizeSmartQuotaIdentity(fileName),
			supplierID:                normalizeSmartQuotaSupplierID(supplierByFile[fileName]),
			planType:                  strings.ToLower(strings.TrimSpace(result.PlanType)),
			fraction:                  fraction,
			fromMS:                    fromMS,
			toMS:                      observedMS + 1,
			recoverAtMS:               recoverAtMS,
			observedMS:                observedMS,
			credentialEffectiveFromMS: credentialEffectiveFromByFile[fileName],
		}
		baselines = append(baselines, baseline)
		usageFromMS := baseline.fromMS
		if baseline.credentialEffectiveFromMS > usageFromMS {
			usageFromMS = baseline.credentialEffectiveFromMS
		}
		targets = append(targets, store.SupplyQuotaWindowUsageQuery{
			RequestIndex:     requestIndex,
			AuthFileSnapshot: fileName,
			AuthIndex:        result.AuthIndex,
			FromMS:           usageFromMS,
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
		if baseline.identity == "" || baseline.windowTokens <= 0 ||
			!smartQuotaClassificationFractionEligible(baseline.fraction) {
			continue
		}
		s.assignSmartQuotaSupplierToIdentityLocked(baseline.identity, baseline.supplierID)

		observation := s.smartQuotaState.observations[baseline.identity]
		credentialGenerationChanged := baseline.credentialEffectiveFromMS > 0 &&
			observation.credentialEffectiveFromMS != baseline.credentialEffectiveFromMS
		if credentialGenerationChanged {
			// Warm replay can span two credentials that reused one filename. Drop
			// the prior generation once, then retain deltas learned after this marker.
			s.removeSmartQuotaSamplesThroughLocked(baseline.identity, baseline.observedMS)
			delete(s.smartQuotaState.directSamples, baseline.identity)
			delete(s.smartQuotaState.provisionalSamples, baseline.identity)
		}
		if (observation.recoverAtMS > 0 && baseline.recoverAtMS > 0 && observation.recoverAtMS != baseline.recoverAtMS) ||
			(observation.supplierID != "" && baseline.supplierID != "" && observation.supplierID != baseline.supplierID) {
			observation = resetSmartQuotaCalibrationObservation(observation)
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
		if baseline.credentialEffectiveFromMS > 0 {
			observation.credentialEffectiveFromMS = baseline.credentialEffectiveFromMS
		}
		if baseline.planType != "" {
			observation.planType = baseline.planType
		}
		if baseline.supplierID != "" {
			observation.supplierID = baseline.supplierID
		}
		s.smartQuotaState.observations[baseline.identity] = observation

		// Supplier credentials commonly arrive after their weekly quota window
		// has already started and may already show 20-80% used. The local Token
		// database then contains only post-import traffic. Treating that partial
		// tail as a complete-window numerator produced false 10-30M account
		// estimates and collapsed a 16 x 60M pool to roughly 250M. Keep the
		// observation as the baseline for future percentage deltas. The configured
		// fallback remains in use unless separately validated runtime deltas exist.
		if !smartQuotaWindowBaselineHasCompleteCoverage(baseline) {
			delete(s.smartQuotaState.directSamples, baseline.identity)
			delete(s.smartQuotaState.provisionalSamples, baseline.identity)
			continue
		}

		// A complete-window aggregate supersedes older runtime deltas for this
		// credential. Partial-window baselines only seed future deltas, so their
		// already validated runtime samples remain useful after refresh/restart.
		s.removeSmartQuotaSamplesThroughLocked(baseline.identity, baseline.observedMS)

		capacityM := float64(baseline.windowTokens) / baseline.fraction / 1_000_000
		if capacityM < smartQuotaCalibrationMinCapacityM || capacityM > smartQuotaCalibrationMaxCapacityM {
			delete(s.smartQuotaState.directSamples, baseline.identity)
			delete(s.smartQuotaState.provisionalSamples, baseline.identity)
			continue
		}
		sample := smartQuotaCalibrationSample{
			identity:           baseline.identity,
			supplierID:         observation.supplierID,
			planType:           observation.planType,
			capacityM:          capacityM,
			weight:             1,
			usedFraction:       baseline.fraction,
			observedMS:         baseline.observedMS,
			completeWindow:     true,
			classificationOnly: !smartQuotaCalibrationUsedFractionEligible(baseline.fraction),
		}
		if sample.classificationOnly {
			s.smartQuotaState.provisionalSamples[baseline.identity] = sample
		} else {
			delete(s.smartQuotaState.provisionalSamples, baseline.identity)
			s.smartQuotaState.directSamples[baseline.identity] = sample
		}
	}
	s.pruneSmartQuotaCalibrationLocked(now)
}

func smartQuotaWindowBaselineHasCompleteCoverage(baseline smartQuotaWindowBaseline) bool {
	if baseline.fromMS <= 0 || baseline.firstSeenMS <= 0 || baseline.observedMS <= baseline.fromMS {
		return false
	}
	coverageBoundaryMS := baseline.fromMS + smartQuotaCompleteWindowCoverageSlack.Milliseconds()
	if baseline.credentialEffectiveFromMS > coverageBoundaryMS {
		return false
	}
	return baseline.firstSeenMS <= coverageBoundaryMS
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
			observation = resetSmartQuotaCalibrationObservation(observation)
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
		observation = resetSmartQuotaCalibrationObservation(observation)
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
	minimumDelta := smartQuotaCalibrationMinDelta
	if !smartQuotaCalibrationUsedFractionEligible(fraction) {
		minimumDelta = smartQuotaClassificationMinDelta
	}
	if deltaTokens > 0 && smartQuotaClassificationFractionEligible(fraction) && delta >= minimumDelta {
		// Runtime events are valid only as an observed delta. Dividing a partial
		// post-restart Token tail by an absolute 10%+ quota percentage creates the
		// false 8M/30M account estimates seen in production. Complete inspection
		// windows use the independent absolute formula in recordSmartQuotaWindowBaselines.
		capacityM := float64(deltaTokens) / delta / 1_000_000
		if capacityM >= smartQuotaCalibrationMinCapacityM && capacityM <= smartQuotaCalibrationMaxCapacityM {
			sample := smartQuotaCalibrationSample{
				identity:           identity,
				supplierID:         observation.supplierID,
				planType:           observation.planType,
				capacityM:          capacityM,
				weight:             math.Max(delta, smartQuotaCalibrationMinDelta),
				usedFraction:       fraction,
				observedMS:         ts,
				classificationOnly: !smartQuotaCalibrationUsedFractionEligible(fraction),
			}
			if sample.classificationOnly {
				s.smartQuotaState.provisionalSamples[identity] = sample
			} else {
				delete(s.smartQuotaState.provisionalSamples, identity)
				s.appendSmartQuotaCalibrationSampleLocked(sample)
			}
		}
		observation.lastSampleFraction = fraction
		observation.lastSampleTokens = observation.windowTokens
	}
	s.smartQuotaState.observations[identity] = observation
}

func resetSmartQuotaCalibrationObservation(observation smartQuotaCalibrationObservation) smartQuotaCalibrationObservation {
	return smartQuotaCalibrationObservation{
		credentialEffectiveFromMS: observation.credentialEffectiveFromMS,
	}
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
	for identity, sample := range s.smartQuotaState.provisionalSamples {
		if sample.observedMS < cutoff {
			delete(s.smartQuotaState.provisionalSamples, identity)
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
	return s.smartQuotaEstimateForSupplierAt(now, "", planType, identities...)
}

func (s *Service) smartQuotaEstimateForSupplierAt(now time.Time, supplierID string, planType string, identities ...string) smartQuotaEstimate {
	return s.smartQuotaEstimateForSupplierAtWithMinimum(now, supplierID, planType, 0, identities...)
}

func (s *Service) smartQuotaEstimateForSupplierAtWithMinimum(
	now time.Time,
	supplierID string,
	planType string,
	minimumCapacityM float64,
	identities ...string,
) smartQuotaEstimate {
	if s == nil {
		return defaultSmartQuotaEstimate()
	}
	cutoff := now.Add(-smartQuotaCalibrationSampleTTL).UnixMilli()
	recentCutoff := now.Add(-smartQuotaCalibrationRecentWindow).UnixMilli()
	supplierID = normalizeSmartQuotaSupplierID(supplierID)
	planType = strings.ToLower(strings.TrimSpace(planType))
	normalizedIdentities := make(map[string]struct{}, len(identities))
	for _, identity := range identities {
		identity = strings.TrimSpace(identity)
		if identity != "" {
			normalizedIdentities[identity] = struct{}{}
		}
	}
	s.smartMu.RLock()
	samples := make([]smartQuotaCalibrationSample, 0, len(s.smartQuotaState.samples)+len(s.smartQuotaState.directSamples)+len(s.smartQuotaState.provisionalSamples))
	for _, sample := range s.smartQuotaState.samples {
		if sample.observedMS >= cutoff && sample.capacityM >= minimumCapacityM &&
			(supplierID == "" || sample.supplierID == supplierID) {
			samples = append(samples, sample)
		}
	}
	for _, sample := range s.smartQuotaState.directSamples {
		if sample.observedMS >= cutoff && sample.capacityM >= minimumCapacityM &&
			(supplierID == "" || sample.supplierID == supplierID) {
			samples = append(samples, sample)
		}
	}
	for _, sample := range s.smartQuotaState.provisionalSamples {
		if sample.observedMS >= cutoff && sample.capacityM >= minimumCapacityM &&
			(supplierID == "" || sample.supplierID == supplierID) {
			samples = append(samples, sample)
		}
	}
	s.smartMu.RUnlock()
	classSamples := samples
	if planType != "" {
		classSamples = filterSmartQuotaSamples(samples, func(sample smartQuotaCalibrationSample) bool {
			return sample.planType == planType
		})
	}
	quotaClasses := estimateSmartQuotaClassesAt(classSamples, now)

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
		supplierID,
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
		supplierID,
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
		supplierID,
		planType,
		smartQuotaEstimateSourcePlan,
		20,
		0.10,
		30,
		0.15,
		now,
	)

	if currentOK {
		return attachSmartQuotaClasses(calibrateSmartQuotaCurrentEstimate(
			currentEstimate,
			recentEstimate,
			recentOK,
			historicalEstimate,
			historicalOK,
		), quotaClasses)
	}
	if recentOK {
		recentEstimate.RecentEstimateM = recentEstimate.CapacityM
		if historicalOK {
			recentEstimate.HistoricalEstimateM = historicalEstimate.CapacityM
		}
		return attachSmartQuotaClasses(recentEstimate, quotaClasses)
	}
	if allOK {
		allEstimate.HistoricalEstimateM = allEstimate.CapacityM
		return attachSmartQuotaClasses(allEstimate, quotaClasses)
	}
	return attachSmartQuotaClasses(defaultSmartQuotaEstimateForPlan(planType), quotaClasses)
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
	completeWindowSamples := filterSmartQuotaSamples(samples, func(sample smartQuotaCalibrationSample) bool {
		return sample.completeWindow
	})
	if len(completeWindowSamples) > 0 {
		// A complete local quota window proves the absolute numerator and may be
		// adopted immediately for this exact credential.
		return estimateSmartQuotaSamplesAtMode(
			completeWindowSamples,
			smartQuotaEstimateSourceCurrent,
			1,
			0.005,
			now,
			false,
		)
	}

	runtimeSamples := filterSmartQuotaSamples(samples, func(sample smartQuotaCalibrationSample) bool {
		return !sample.completeWindow && !sample.classificationOnly
	})
	if !smartQuotaRuntimeEvidenceEligible(runtimeSamples, now) {
		return smartQuotaEstimate{}, false
	}
	// Runtime percentages are integer-rounded and often advance one point well
	// after the requests that consumed it. Do not let three 1% movements replace
	// a 60M fallback with a noisy 3M/10M/120M estimate. The precheck requires at
	// least three mutually consistent samples and more than ten percentage
	// points of locally observed movement. The 0.06 recency-weighted floor is
	// the equivalent of 10% raw evidence at the six-hour cutoff (weight 0.60).
	return estimateSmartQuotaSamplesAtMode(
		runtimeSamples,
		smartQuotaEstimateSourceCurrent,
		smartQuotaCalibrationMinRuntimeSamples,
		smartQuotaCalibrationMinObservedDelta*0.60,
		now,
		false,
	)
}

func smartQuotaRuntimeEvidenceEligible(samples []smartQuotaCalibrationSample, now time.Time) bool {
	cutoff := now.Add(-smartQuotaCalibrationRecentWindow).UnixMilli()
	capacities := make([]float64, 0, len(samples))
	observedDelta := 0.0
	for _, sample := range samples {
		if sample.observedMS < cutoff || sample.weight <= 0 ||
			sample.capacityM < smartQuotaCalibrationMinCapacityM ||
			sample.capacityM > smartQuotaCalibrationMaxCapacityM ||
			!smartQuotaCalibrationUsedFractionEligible(sample.usedFraction) {
			continue
		}
		capacities = append(capacities, sample.capacityM)
		// Runtime sample weight is the percentage delta observed since the prior
		// sample. Complete-window samples use weight=1 and were handled above.
		observedDelta += sample.weight
	}
	if len(capacities) < smartQuotaCalibrationMinRuntimeSamples ||
		observedDelta <= smartQuotaCalibrationMinObservedDelta {
		return false
	}

	sort.Float64s(capacities)
	median := capacities[len(capacities)/2]
	if median <= 0 {
		return false
	}
	consistent := 0
	for _, capacityM := range capacities {
		if smartQuotaRelativeDifference(capacityM, median) <= smartQuotaCalibrationMaxSampleDeviation {
			consistent++
		}
	}
	return consistent >= 2
}

func smartQuotaPlanOrGlobalEstimate(
	samples []smartQuotaCalibrationSample,
	supplierID string,
	planType string,
	planSource string,
	planMinSamples int,
	planMinWeight float64,
	globalMinSamples int,
	globalMinWeight float64,
	now time.Time,
) (smartQuotaEstimate, bool) {
	supplierID = normalizeSmartQuotaSupplierID(supplierID)
	if supplierID != "" {
		samples = filterSmartQuotaSamples(samples, func(sample smartQuotaCalibrationSample) bool {
			return sample.supplierID == supplierID
		})
	}
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
	current.CurrentEstimateM = current.CapacityM
	if current.Provisional {
		current.Source = smartQuotaEstimateSourceClassified
		current.Confidence = smartConfidenceLow
		return current
	}
	current.Source = smartQuotaEstimateSourceCurrent
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

func dominantSmartQuotaContext(
	results []store.CodexInspectionResult,
	supplierByFile map[string]string,
	platforms []store.ManagerSupplyPlatformConfig,
) (string, string) {
	counts := make(map[string]int)
	for _, result := range results {
		if !isSmartCapacityInspectionResult(result) || inspectionResultCapacityExcluded(result) {
			continue
		}
		planType := strings.ToLower(strings.TrimSpace(result.PlanType))
		if planType == "" {
			planType = "unknown"
		}
		supplierID := normalizeSmartQuotaSupplierID(supplierByFile[strings.TrimSpace(result.FileName)])
		if supplierID == "" && len(platforms) == 1 {
			supplierID = normalizeSmartQuotaSupplierID(platforms[0].ID)
		}
		counts[smartQuotaContextKey(supplierID, planType)]++
	}
	bestKey := ""
	bestCount := 0
	for key, count := range counts {
		if count > bestCount || (count == bestCount && key < bestKey) {
			bestKey = key
			bestCount = count
		}
	}
	if bestKey == "" {
		if len(platforms) == 1 {
			return normalizeSmartQuotaSupplierID(platforms[0].ID), "team"
		}
		return "", dominantSmartQuotaPlan(results)
	}
	parts := strings.SplitN(bestKey, "\x00", 2)
	if len(parts) != 2 {
		return "", "team"
	}
	return parts[0], parts[1]
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

func smartQuotaPolicyForSupplier(cfg store.ManagerSupplyConfig, supplierID, planType string) store.ManagerSupplyQuotaEstimationPolicy {
	policy := smartQuotaPolicyForPlan(cfg, planType)
	supplierID = normalizeSmartQuotaSupplierID(supplierID)
	if supplierID == "" {
		return policy
	}
	planType = strings.ToLower(strings.TrimSpace(planType))
	for _, platform := range supplyPlatforms(cfg) {
		if normalizeSmartQuotaSupplierID(platform.ID) != supplierID {
			continue
		}
		configured, ok := platform.QuotaEstimationPolicies[planType]
		if !ok {
			return policy
		}
		if strings.EqualFold(configured.Mode, smartQuotaPolicyModeFixed) {
			policy.Mode = smartQuotaPolicyModeFixed
		} else {
			policy.Mode = smartQuotaPolicyModeAuto
		}
		if configured.FallbackM > 0 {
			policy.FallbackM = clampFloat(configured.FallbackM, smartQuotaCalibrationMinCapacityM, smartQuotaCalibrationMaxCapacityM)
		}
		if configured.FixedM > 0 {
			policy.FixedM = clampFloat(configured.FixedM, smartQuotaCalibrationMinCapacityM, smartQuotaCalibrationMaxCapacityM)
		}
		return policy
	}
	return policy
}

func smartQuotaEstimateHasValidData(estimate smartQuotaEstimate) bool {
	return estimate.Source != smartQuotaEstimateSourceDefault && !estimate.Provisional &&
		estimate.SampleCount > 0 && estimate.CapacityM > 0
}

func smartQuotaEstimateHasTrustedPlanData(estimate smartQuotaEstimate) bool {
	return smartQuotaEstimateHasValidData(estimate) &&
		estimate.UniqueAccounts >= smartQuotaPolicyMinUniqueAccounts
}

func smartQuotaPolicyRoundsForDifference(difference float64) int {
	switch {
	case difference > smartQuotaPolicyExtremeDivergence:
		return smartQuotaPolicyExtremeRequiredRounds
	case difference > smartQuotaPolicyModerateDivergence:
		return smartQuotaPolicyModerateRequiredRounds
	default:
		return smartQuotaPolicyRequiredRounds
	}
}

func smartQuotaExtremeDownwardEstimateTrusted(estimate smartQuotaEstimate) bool {
	return estimate.CompleteWindowAccounts >= smartQuotaPolicyMinUniqueAccounts
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

func smartQuotaEstimateAccountCount(estimate smartQuotaEstimate) int {
	classAccounts := 0
	for _, quotaClass := range estimate.QuotaClasses {
		classAccounts += max(0, quotaClass.AccountCount)
	}
	return max(estimate.UniqueAccounts, classAccounts)
}

func (s *Service) smartQuotaPlanEstimatesForInspection(
	cfg store.ManagerSupplyConfig,
	results []store.CodexInspectionResult,
	runID int64,
	now time.Time,
	supplierMaps ...map[string]string,
) ([]SmartQuotaPlanEstimate, map[string]smartQuotaEstimate) {
	type planContext struct {
		supplierID   string
		supplierName string
		planType     string
		accounts     int
		identities   []string
	}
	var supplierByFile map[string]string
	if len(supplierMaps) > 0 {
		supplierByFile = supplierMaps[0]
	}
	contexts := make(map[string]*planContext)
	ensureContext := func(supplierID, supplierName, planType string) *planContext {
		supplierID = normalizeSmartQuotaSupplierID(supplierID)
		planType = strings.ToLower(strings.TrimSpace(planType))
		if planType == "" {
			planType = "unknown"
		}
		key := smartQuotaContextKey(supplierID, planType)
		if existing := contexts[key]; existing != nil {
			return existing
		}
		context := &planContext{supplierID: supplierID, supplierName: strings.TrimSpace(supplierName), planType: planType}
		contexts[key] = context
		return context
	}

	platforms := supplyPlatforms(cfg)
	platformNames := make(map[string]string, len(platforms))
	for _, platform := range platforms {
		supplierID := normalizeSmartQuotaSupplierID(platform.ID)
		platformNames[supplierID] = firstNonEmptyString(platform.Name, platform.ID)
		plans := make(map[string]struct{}, len(cfg.QuotaEstimationPolicies)+len(platform.QuotaEstimationPolicies)+2)
		plans["team"] = struct{}{}
		plans["free"] = struct{}{}
		for planType := range cfg.QuotaEstimationPolicies {
			plans[strings.ToLower(strings.TrimSpace(planType))] = struct{}{}
		}
		for planType := range platform.QuotaEstimationPolicies {
			plans[strings.ToLower(strings.TrimSpace(planType))] = struct{}{}
		}
		for planType := range plans {
			if planType != "" {
				ensureContext(supplierID, platformNames[supplierID], planType)
			}
		}
	}
	if len(platforms) == 0 {
		for planType := range cfg.QuotaEstimationPolicies {
			if strings.TrimSpace(planType) != "" {
				ensureContext("", "", planType)
			}
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
		supplierID := normalizeSmartQuotaSupplierID(supplierByFile[strings.TrimSpace(result.FileName)])
		if supplierID == "" && len(platforms) == 1 {
			supplierID = normalizeSmartQuotaSupplierID(platforms[0].ID)
		}
		supplierName := platformNames[supplierID]
		if supplierID == "" && len(platforms) > 1 {
			supplierName = "Unassigned/manual"
		}
		context := ensureContext(supplierID, supplierName, planType)
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
		ensureContext("", "", "team")
	}

	keys := make([]string, 0, len(contexts))
	for key := range contexts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left := contexts[keys[i]]
		right := contexts[keys[j]]
		leftSupplier := strings.ToLower(firstNonEmptyString(left.supplierName, left.supplierID))
		rightSupplier := strings.ToLower(firstNonEmptyString(right.supplierName, right.supplierID))
		if leftSupplier != rightSupplier {
			return leftSupplier < rightSupplier
		}
		return left.planType < right.planType
	})

	s.quotaPolicyMu.Lock()
	defer s.quotaPolicyMu.Unlock()
	if s.quotaPolicyState == nil {
		s.quotaPolicyState = make(map[string]smartQuotaPlanAdoptionState)
	}
	items := make([]SmartQuotaPlanEstimate, 0, len(keys))
	planning := make(map[string]smartQuotaEstimate, len(keys))
	for _, key := range keys {
		context := contexts[key]
		planType := context.planType
		policy := smartQuotaPolicyForSupplier(cfg, context.supplierID, planType)
		state := s.quotaPolicyState[key]
		if state.mode != policy.Mode || state.adoptedM <= 0 {
			state = smartQuotaPlanAdoptionState{
				mode:            policy.Mode,
				adoptedM:        policy.FallbackM,
				requiredRounds:  smartQuotaPolicyRequiredRounds,
				validationState: smartQuotaValidationInsufficient,
			}
		}

		rawObserved := s.smartQuotaEstimateForSupplierAt(now, context.supplierID, planType, context.identities...)
		// Historical samples for a configured type remain useful once that type
		// appears again, but a type with zero current accounts must not raise a
		// calibration warning or influence this pool's ordering decision.
		rawHasObservedData := context.accounts > 0 && smartQuotaEstimateHasValidData(rawObserved)
		candidateObserved := rawObserved
		hasData := context.accounts > 0 && smartQuotaEstimateHasTrustedPlanData(candidateObserved)
		filteredObserved := rawObserved
		filteredTrusted := false
		rejectedAccounts := 0
		if policy.Mode == smartQuotaPolicyModeAuto && context.accounts > 0 && state.adoptedM > 0 {
			minimumAcceptedM := state.adoptedM * (1 - smartQuotaPolicyExtremeDivergence)
			filteredObserved = s.smartQuotaEstimateForSupplierAtWithMinimum(
				now,
				context.supplierID,
				planType,
				minimumAcceptedM,
				context.identities...,
			)
			rejectedAccounts = max(0,
				smartQuotaEstimateAccountCount(rawObserved)-smartQuotaEstimateAccountCount(filteredObserved),
			)
			filteredTrusted = rejectedAccounts > 0 && smartQuotaEstimateHasTrustedPlanData(filteredObserved)
			if filteredTrusted {
				// When enough normal-range accounts remain, completely remove the
				// abnormal low cluster before adoption as well as presentation. Old
				// failures can no longer drag an otherwise healthy estimate down.
				candidateObserved = filteredObserved
				hasData = true
			}
		}
		planningObserved := candidateObserved
		if !hasData {
			planningObserved = smartQuotaEstimate{
				CapacityM:  policy.FallbackM,
				Source:     smartQuotaEstimateSourceDefault,
				Confidence: smartConfidenceLow,
			}
		}
		if policy.Mode == smartQuotaPolicyModeFixed {
			state.adoptedM = policy.FixedM
			state.candidateM = policy.FixedM
			state.lastObservedM = planningObserved.CapacityM
			state.confirmationRounds = smartQuotaPolicyRequiredRounds
			state.requiredRounds = smartQuotaPolicyRequiredRounds
			state.pending = false
			state.validationState = smartQuotaValidationFixed
			state.lastInspectionRunID = runID
		} else if context.accounts == 0 {
			state.adoptedM = policy.FallbackM
			state.candidateM = 0
			state.lastObservedM = 0
			state.confirmationRounds = 0
			state.requiredRounds = smartQuotaPolicyRequiredRounds
			state.pending = false
			state.validationState = smartQuotaValidationInsufficient
			state.lastInspectionRunID = runID
		} else if !hasData {
			validationObservedM := 0.0
			if rawHasObservedData {
				validationObservedM = rawObserved.CapacityM
			}
			difference := smartQuotaRelativeDifference(validationObservedM, state.adoptedM)
			extremeDownward := validationObservedM > 0 && validationObservedM < state.adoptedM &&
				difference > smartQuotaPolicyExtremeDivergence
			state.candidateM = validationObservedM
			state.lastObservedM = validationObservedM
			state.confirmationRounds = 0
			state.requiredRounds = smartQuotaPolicyRoundsForDifference(difference)
			state.pending = extremeDownward
			if extremeDownward {
				state.validationState = smartQuotaValidationQuarantined
			} else {
				state.validationState = smartQuotaValidationInsufficient
			}
			state.lastInspectionRunID = runID
		} else {
			newInspection := (runID > 0 && runID != state.lastInspectionRunID) ||
				(runID <= 0 && state.lastObservedM <= 0)
			if newInspection {
				candidateShifted := state.candidateM > 0 &&
					smartQuotaRelativeDifference(planningObserved.CapacityM, state.candidateM) > smartQuotaPolicyWarningFraction
				if candidateShifted {
					state.confirmationRounds = 1
				} else {
					state.confirmationRounds++
				}
				state.candidateM = planningObserved.CapacityM
				state.lastObservedM = planningObserved.CapacityM
				state.lastInspectionRunID = runID
				difference := smartQuotaRelativeDifference(planningObserved.CapacityM, state.adoptedM)
				state.requiredRounds = smartQuotaPolicyRoundsForDifference(difference)
				extremeDownward := planningObserved.CapacityM < state.adoptedM &&
					difference > smartQuotaPolicyExtremeDivergence
				switch {
				case difference <= smartQuotaPolicyWarningFraction:
					state.adoptedM = planningObserved.CapacityM
					state.pending = false
					state.validationState = smartQuotaValidationAccepted
				case extremeDownward && (!smartQuotaExtremeDownwardEstimateTrusted(planningObserved) ||
					state.confirmationRounds < state.requiredRounds):
					state.pending = true
					state.validationState = smartQuotaValidationQuarantined
				case state.confirmationRounds >= state.requiredRounds:
					state.adoptedM = smartQuotaMoveAtMostTenPercent(state.adoptedM, planningObserved.CapacityM)
					state.pending = smartQuotaRelativeDifference(planningObserved.CapacityM, state.adoptedM) > 0.001
					state.validationState = smartQuotaValidationAccepted
				default:
					state.pending = true
					state.validationState = smartQuotaValidationConfirming
				}
			}
		}
		s.quotaPolicyState[key] = state
		validationObserved := candidateObserved
		if !hasData {
			validationObserved = rawObserved
		}
		publishedObserved := rawObserved
		publishedRejectedAccounts := 0
		if filteredTrusted || (rejectedAccounts > 0 && state.validationState == smartQuotaValidationQuarantined) {
			publishedObserved = filteredObserved
			publishedRejectedAccounts = rejectedAccounts
		}
		hasPublishedObservedData := context.accounts > 0 && smartQuotaEstimateHasValidData(publishedObserved)
		publishedObservedM := 0.0
		if hasPublishedObservedData {
			publishedObservedM = publishedObserved.CapacityM
		}

		// A downward observation under confirmation is not reliable enough to size
		// purchases. Keep collecting evidence, but plan every account in this
		// supplier/plan context from the configured no-data fallback until the
		// candidate is accepted. This keeps the warning visible without stranding
		// the pool behind a calibration-only ordering gate.
		usingFallback := policy.Mode == smartQuotaPolicyModeAuto && state.pending &&
			context.accounts > 0 && smartQuotaEstimateHasValidData(validationObserved) &&
			validationObserved.CapacityM < state.adoptedM &&
			(state.validationState == smartQuotaValidationConfirming ||
				state.validationState == smartQuotaValidationQuarantined)
		effectiveAdoptedM := state.adoptedM
		if usingFallback {
			effectiveAdoptedM = policy.FallbackM
		}
		divergence := 0.0
		if context.accounts > 0 && smartQuotaEstimateHasValidData(validationObserved) {
			divergence = smartQuotaRelativeDifference(validationObserved.CapacityM, state.adoptedM) * 100
		}
		source := publishedObserved.Source
		sampleCount := publishedObserved.SampleCount
		uniqueAccounts := publishedObserved.UniqueAccounts
		completeWindowAccounts := publishedObserved.CompleteWindowAccounts
		if context.accounts == 0 {
			source = smartQuotaEstimateSourceDefault
			sampleCount = 0
			uniqueAccounts = 0
			completeWindowAccounts = 0
		}
		if policy.Mode == smartQuotaPolicyModeFixed {
			source = smartQuotaPolicyModeFixed
		}
		items = append(items, SmartQuotaPlanEstimate{
			Key:                    smartQuotaPublicContextKey(context.supplierID, planType),
			SupplierID:             context.supplierID,
			SupplierName:           context.supplierName,
			PlanType:               planType,
			Mode:                   policy.Mode,
			AccountCount:           context.accounts,
			FallbackM:              round2(policy.FallbackM),
			FixedM:                 round2(policy.FixedM),
			ObservedM:              round2(publishedObservedM),
			AdoptedM:               round2(effectiveAdoptedM),
			Source:                 source,
			SampleCount:            sampleCount,
			UniqueAccounts:         uniqueAccounts,
			CompleteWindowAccounts: completeWindowAccounts,
			MinimumUniqueAccounts:  smartQuotaPolicyMinUniqueAccounts,
			DivergencePercent:      round2(divergence),
			PendingConfirmation:    state.pending,
			ConfirmationRounds:     state.confirmationRounds,
			RequiredRounds:         state.requiredRounds,
			ValidationState:        state.validationState,
			UsingFallback:          usingFallback,
			RejectedAccounts:       publishedRejectedAccounts,
			OrderingBlocked:        false,
			LastInspectionRunID:    state.lastInspectionRunID,
			QuotaClasses:           publishedObserved.QuotaClasses,
		})
		planningSource := source
		if policy.Mode == smartQuotaPolicyModeAuto && !hasData {
			planningSource = smartQuotaEstimateSourceDefault
		}
		if usingFallback {
			planningSource = smartQuotaEstimateSourceDefault
		} else if policy.Mode == smartQuotaPolicyModeAuto && hasData &&
			smartQuotaRelativeDifference(state.adoptedM, planningObserved.CapacityM) > 0.001 {
			planningSource = smartQuotaEstimateSourceRecalibrated
		}
		planningEstimate := planningObserved
		planningEstimate.CapacityM = effectiveAdoptedM
		planningEstimate.Source = planningSource
		planningEstimate.FallbackOnly = usingFallback
		planning[key] = planningEstimate
		if context.supplierID == "" {
			planning[planType] = planningEstimate
		}
	}
	return items, planning
}

func smartQuotaPlanningEstimateForPlan(planning map[string]smartQuotaEstimate, supplierID, planType string) smartQuotaEstimate {
	supplierID = normalizeSmartQuotaSupplierID(supplierID)
	planType = strings.ToLower(strings.TrimSpace(planType))
	if estimate, ok := planning[smartQuotaContextKey(supplierID, planType)]; ok && estimate.CapacityM > 0 {
		return estimate
	}
	if supplierID == "" {
		if estimate, ok := planning[planType]; ok && estimate.CapacityM > 0 {
			return estimate
		}
	}
	if estimate, ok := planning[smartQuotaContextKey(supplierID, "team")]; ok && estimate.CapacityM > 0 {
		return estimate
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
	// valid >10% sample, use its independent estimate directly and never blend
	// it with the plan default. Consumption must be strictly above 10% so an
	// integer-rounded 10% header never becomes account-capacity evidence.
	if fallback.Source == smartQuotaPolicyModeFixed && fallback.CapacityM > 0 {
		return fallback
	}
	if fallback.FallbackOnly && fallback.CapacityM > 0 {
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
	return estimateSmartQuotaSamplesAtMode(samples, source, minSamples, minWeight, now, false)
}

func estimateSmartQuotaSamplesAtMode(
	samples []smartQuotaCalibrationSample,
	source string,
	minSamples int,
	minWeight float64,
	now time.Time,
	allowClassification bool,
) (smartQuotaEstimate, bool) {
	valid := make([]smartQuotaCalibrationSample, 0, len(samples))
	hasTrustedEligibleSample := false
	if allowClassification {
		for _, sample := range samples {
			if !sample.classificationOnly && smartQuotaCalibrationUsedFractionEligible(sample.usedFraction) {
				hasTrustedEligibleSample = true
				break
			}
		}
	}
	totalObservedWeight := 0.0
	identitySet := make(map[string]struct{})
	independentAccount := false
	hasTrustedSample := false
	for _, sample := range samples {
		if allowClassification && hasTrustedEligibleSample && sample.classificationOnly {
			continue
		}
		fractionEligible := smartQuotaCalibrationUsedFractionEligible(sample.usedFraction)
		if allowClassification && sample.classificationOnly {
			fractionEligible = smartQuotaClassificationFractionEligible(sample.usedFraction)
		}
		if sample.capacityM < smartQuotaCalibrationMinCapacityM || sample.capacityM > smartQuotaCalibrationMaxCapacityM ||
			sample.weight <= 0 || sample.observedMS <= 0 ||
			!fractionEligible || (!allowClassification && sample.classificationOnly) {
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
		if !sample.classificationOnly {
			hasTrustedSample = true
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
	completeWindowAccounts := 0
	for _, identitySamples := range grouped {
		// A complete account-window sample is the requested history/used-percent
		// formula for that exact account. Do not mix it with small runtime delta
		// samples from the same identity: integer percentage headers can turn a
		// one-point transition into a false 5M/10M estimate and overwhelm the
		// authoritative complete-window value during extreme trimming.
		completeWindowSamples := filterSmartQuotaSamples(identitySamples, func(sample smartQuotaCalibrationSample) bool {
			return sample.completeWindow
		})
		if len(completeWindowSamples) > 0 {
			completeWindowAccounts++
			identitySamples = completeWindowSamples
		}
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
		CapacityM:              round2(clampFloat(median, smartQuotaCalibrationMinCapacityM, smartQuotaCalibrationMaxCapacityM)),
		Source:                 source,
		SampleCount:            len(accountPoints),
		EvidenceCount:          len(valid),
		ObservedPercent:        round2(totalObservedWeight * 100),
		Confidence:             confidence,
		UniqueAccounts:         len(accountPoints),
		CompleteWindowAccounts: completeWindowAccounts,
		IndependentAccount:     independentAccount,
		Provisional:            !hasTrustedSample,
	}, true
}

func estimateSmartQuotaClassesAt(samples []smartQuotaCalibrationSample, now time.Time) []SmartQuotaClassEstimate {
	cutoff := now.Add(-smartQuotaCalibrationSampleTTL).UnixMilli()
	grouped := make(map[string][]smartQuotaCalibrationSample)
	for _, sample := range samples {
		if sample.identity == "" || sample.observedMS < cutoff || sample.weight <= 0 ||
			sample.capacityM < smartQuotaCalibrationMinCapacityM || sample.capacityM > smartQuotaCalibrationMaxCapacityM ||
			!smartQuotaClassificationFractionEligible(sample.usedFraction) {
			continue
		}
		grouped[sample.identity] = append(grouped[sample.identity], sample)
	}

	trustedPoints := make([]smartQuotaClassPoint, 0, len(grouped))
	provisionalPoints := make([]smartQuotaClassPoint, 0, len(grouped))
	for identity, identitySamples := range grouped {
		trusted := filterSmartQuotaSamples(identitySamples, func(sample smartQuotaCalibrationSample) bool {
			return !sample.classificationOnly && smartQuotaCalibrationUsedFractionEligible(sample.usedFraction)
		})
		selected := trusted
		isTrusted := len(trusted) > 0
		if !isTrusted {
			selected = filterSmartQuotaSamples(identitySamples, func(sample smartQuotaCalibrationSample) bool {
				return sample.classificationOnly && smartQuotaClassificationFractionEligible(sample.usedFraction)
			})
		}
		if len(selected) == 0 {
			continue
		}
		if isTrusted {
			complete := filterSmartQuotaSamples(selected, func(sample smartQuotaCalibrationSample) bool {
				return sample.completeWindow
			})
			if len(complete) > 0 {
				selected = complete
			}
		}
		selected = trimSmartQuotaSampleExtremes(selected)
		weighted := make([]smartQuotaWeightedPoint, 0, len(selected))
		totalWeight := 0.0
		for _, sample := range selected {
			weight := sample.weight * smartQuotaSampleRecencyWeight(now, sample.observedMS) *
				clampFloat(sample.usedFraction, smartQuotaClassificationMinUsedFraction, 1)
			if weight <= 0 {
				continue
			}
			weighted = append(weighted, smartQuotaWeightedPoint{capacityM: sample.capacityM, weight: weight})
			totalWeight += weight
		}
		if len(weighted) == 0 || totalWeight <= 0 {
			continue
		}
		point := smartQuotaClassPoint{
			identity:  identity,
			capacityM: weightedSmartQuotaMedian(weighted),
			weight:    math.Min(totalWeight, 1),
			trusted:   isTrusted,
		}
		if isTrusted {
			trustedPoints = append(trustedPoints, point)
		} else {
			provisionalPoints = append(provisionalPoints, point)
		}
	}

	groups := clusterSmartQuotaClassPoints(trustedPoints)
	unassigned := make([]smartQuotaClassPoint, 0, len(provisionalPoints))
	for _, point := range provisionalPoints {
		groupIndex, distance := nearestSmartQuotaClassGroup(groups, point.capacityM, true)
		if groupIndex < 0 {
			unassigned = append(unassigned, point)
			continue
		}
		center := smartQuotaClassGroupCenter(groups[groupIndex], true)
		if distance > math.Max(15, center*0.35) {
			unassigned = append(unassigned, point)
			continue
		}
		groups[groupIndex].points = append(groups[groupIndex].points, point)
	}
	groups = append(groups, clusterSmartQuotaClassPoints(unassigned)...)
	if len(groups) == 0 {
		return nil
	}
	sort.SliceStable(groups, func(i, j int) bool {
		return smartQuotaClassGroupCenter(groups[i], false) < smartQuotaClassGroupCenter(groups[j], false)
	})
	totalAccounts := 0
	for _, group := range groups {
		totalAccounts += len(group.points)
	}
	items := make([]SmartQuotaClassEstimate, 0, len(groups))
	usedIDs := make(map[string]int)
	for _, group := range groups {
		if len(group.points) == 0 {
			continue
		}
		center := smartQuotaClassGroupCenter(group, false)
		minimumM := group.points[0].capacityM
		maximumM := group.points[0].capacityM
		trustedAccounts := 0
		for _, point := range group.points {
			minimumM = math.Min(minimumM, point.capacityM)
			maximumM = math.Max(maximumM, point.capacityM)
			if point.trusted {
				trustedAccounts++
			}
		}
		confidence := smartConfidenceLow
		if trustedAccounts >= smartQuotaPolicyMinUniqueAccounts {
			confidence = smartConfidenceHigh
		} else if trustedAccounts > 0 {
			confidence = smartConfidenceMedium
		}
		baseID := "quota-" + strconv.Itoa(int(math.Round(center))) + "m"
		usedIDs[baseID]++
		id := baseID
		if usedIDs[baseID] > 1 {
			id += "-" + strconv.Itoa(usedIDs[baseID])
		}
		items = append(items, SmartQuotaClassEstimate{
			ID:                  id,
			CenterM:             round2(center),
			MinimumM:            round2(minimumM),
			MaximumM:            round2(maximumM),
			AccountCount:        len(group.points),
			TrustedAccounts:     trustedAccounts,
			ProvisionalAccounts: len(group.points) - trustedAccounts,
			SharePercent:        round2(float64(len(group.points)) / float64(max(1, totalAccounts)) * 100),
			Confidence:          confidence,
		})
	}
	return items
}

func clusterSmartQuotaClassPoints(points []smartQuotaClassPoint) []smartQuotaClassGroup {
	if len(points) == 0 {
		return nil
	}
	ordered := append([]smartQuotaClassPoint(nil), points...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].capacityM < ordered[j].capacityM
	})
	groups := []smartQuotaClassGroup{{points: []smartQuotaClassPoint{ordered[0]}}}
	for _, point := range ordered[1:] {
		last := &groups[len(groups)-1]
		center := smartQuotaClassGroupCenter(*last, false)
		if point.capacityM-center > math.Max(15, center*0.35) {
			groups = append(groups, smartQuotaClassGroup{points: []smartQuotaClassPoint{point}})
			continue
		}
		last.points = append(last.points, point)
	}
	return groups
}

func smartQuotaClassGroupCenter(group smartQuotaClassGroup, trustedOnly bool) float64 {
	points := make([]smartQuotaWeightedPoint, 0, len(group.points))
	for _, point := range group.points {
		if trustedOnly && !point.trusted {
			continue
		}
		points = append(points, smartQuotaWeightedPoint{capacityM: point.capacityM, weight: point.weight})
	}
	if len(points) == 0 && trustedOnly {
		return smartQuotaClassGroupCenter(group, false)
	}
	return weightedSmartQuotaMedian(points)
}

func nearestSmartQuotaClassGroup(groups []smartQuotaClassGroup, capacityM float64, trustedOnly bool) (int, float64) {
	bestIndex := -1
	bestDistance := math.MaxFloat64
	for index, group := range groups {
		center := smartQuotaClassGroupCenter(group, trustedOnly)
		distance := math.Abs(capacityM - center)
		if distance < bestDistance {
			bestIndex = index
			bestDistance = distance
		}
	}
	return bestIndex, bestDistance
}

func attachSmartQuotaClasses(estimate smartQuotaEstimate, classes []SmartQuotaClassEstimate) smartQuotaEstimate {
	estimate.QuotaClasses = append([]SmartQuotaClassEstimate(nil), classes...)
	if estimate.CapacityM <= 0 || len(classes) == 0 {
		return estimate
	}
	best := classes[0]
	bestDistance := math.Abs(estimate.CapacityM - best.CenterM)
	for _, class := range classes[1:] {
		distance := math.Abs(estimate.CapacityM - class.CenterM)
		if distance < bestDistance {
			best = class
			bestDistance = distance
		}
	}
	estimate.QuotaClassID = best.ID
	if estimate.Provisional {
		estimate.CapacityM = best.CenterM
	}
	return estimate
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
