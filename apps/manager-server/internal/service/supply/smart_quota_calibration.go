package supply

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
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
	smartQuotaCalibrationSamplesPerAccount  = 3
	smartQuotaCalibrationMinRuntimeSamples  = 3
	smartQuotaCalibrationMinObservedDelta   = 0.10
	smartQuotaCalibrationMaxSampleDeviation = 0.25
	smartQuotaCalibrationMaxRepresentatives = 24
	smartQuotaCalibrationDivergencePct      = 25.0

	// Supplier trials need a much faster account-scoped signal than the pool-wide
	// capacity planner. Once one unchanged credential has consumed at least 5%
	// of a quota window, its locally observed Token delta is sufficient for a
	// provisional seller estimate. The same identity is updated in place and is
	// finalized when the provider reports the quota as exhausted, so one account
	// never becomes two supplier samples.
	supplierQuotaEstimateMinUsedFraction   = 0.05
	supplierQuotaEstimateExhaustedFraction = 0.999

	smartQuotaEstimateSourceDefault       = "default"
	smartQuotaEstimateSourceGlobal        = "runtime_global"
	smartQuotaEstimateSourcePlan          = "runtime_plan"
	smartQuotaEstimateSourceIdentity      = "runtime_identity" // legacy API value
	smartQuotaEstimateSourceCurrent       = "runtime_current"
	smartQuotaEstimateSourceClassified    = "runtime_classified"
	smartQuotaEstimateSourceRecentPlan    = "runtime_recent_plan"
	smartQuotaEstimateSourceRecalibrated  = "runtime_recalibrated"
	smartQuotaEstimateSourceSupplierEarly = "supplier_5pct_estimate"
	smartQuotaEstimateSourceSupplierFinal = "supplier_exhausted_final"
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
	supplierBaselineFraction  float64
	supplierBaselineTokens    int64
	hasSupplierBaseline       bool
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

func smartQuotaCalibrationCapacityValid(capacityM float64) bool {
	return capacityM >= smartQuotaCalibrationMinCapacityM && !math.IsNaN(capacityM) && !math.IsInf(capacityM, 0)
}

func floorSmartQuotaCalibrationCapacity(capacityM float64) float64 {
	if !smartQuotaCalibrationCapacityValid(capacityM) {
		return smartQuotaCalibrationMinCapacityM
	}
	return capacityM
}

type smartQuotaCalibrationState struct {
	observations       map[string]smartQuotaCalibrationObservation
	samples            []smartQuotaCalibrationSample
	samplesByIdentity  map[string][]smartQuotaCalibrationSample
	directSamples      map[string]smartQuotaCalibrationSample
	provisionalSamples map[string]smartQuotaCalibrationSample
	supplierSamples    map[string]smartQuotaCalibrationSample
}

type smartQuotaEstimate struct {
	CapacityM               float64
	Source                  string
	SampleCount             int
	EvidenceCount           int
	ObservedPercent         float64
	Confidence              string
	UniqueAccounts          int
	CompleteWindowAccounts  int
	CurrentEstimateM        float64
	RecentEstimateM         float64
	HistoricalEstimateM     float64
	DivergencePercent       float64
	IndependentAccount      bool
	Provisional             bool
	FallbackOnly            bool
	QuotaClassID            string
	QuotaClasses            []SmartQuotaClassEstimate
	CalibrationAccountCount int
	CurrentCohortClasses    bool
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
		supplierSamples:    make(map[string]smartQuotaCalibrationSample),
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
	if s.smartQuotaState.supplierSamples == nil {
		s.smartQuotaState.supplierSamples = make(map[string]smartQuotaCalibrationSample)
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
	if sample, ok := s.smartQuotaState.supplierSamples[identity]; ok && normalizeSmartQuotaSupplierID(sample.supplierID) == "" {
		sample.supplierID = supplierID
		s.smartQuotaState.supplierSamples[identity] = sample
	}
}

// upsertSmartQuotaSupplierSampleLocked keeps exactly one fast supplier sample
// per account identity. It reports only lifecycle changes that should evict the
// seller-score cache immediately: the first usable 5% estimate and the later
// transition to an exhausted/final sample. Intermediate refinements remain
// visible through the normal short-lived score cache without turning every
// quota header into a database-heavy seller rescore.
func (s *Service) upsertSmartQuotaSupplierSampleLocked(sample smartQuotaCalibrationSample) bool {
	if sample.identity == "" || !smartQuotaCalibrationCapacityValid(sample.capacityM) {
		return false
	}
	s.ensureSmartQuotaCalibrationStateLocked()
	previous, found := s.smartQuotaState.supplierSamples[sample.identity]
	if found && !previous.completeWindow && !sample.completeWindow {
		// Freeze the first >=5% estimate. Normal percentage movement must not
		// continuously rewrite seller history; the next revision is the final
		// exhausted-quota measurement requested by the operator.
		return false
	}
	if found && previous.completeWindow && !sample.completeWindow {
		return false
	}
	s.smartQuotaState.supplierSamples[sample.identity] = sample
	return !found || (!previous.completeWindow && sample.completeWindow)
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
	metadata := smartQuotaResponseMetadata(event)
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

func smartQuotaResponseMetadata(event usage.Event) *usage.ResponseHeaderMetadata {
	metadata := event.ResponseMetadata
	if metadata != nil || strings.TrimSpace(event.ResponseMetadataJSON) == "" {
		return metadata
	}
	var decoded usage.ResponseHeaderMetadata
	if err := json.Unmarshal([]byte(event.ResponseMetadataJSON), &decoded); err != nil {
		return nil
	}
	return &decoded
}

func smartLiveQuotaEventIdentities(event usage.Event) []string {
	credentialValues := []struct {
		prefix string
		value  string
	}{
		{prefix: "file:", value: event.AuthFileSnapshot},
		{prefix: "auth:", value: event.AuthIndex},
	}
	identities := make([]string, 0, len(credentialValues))
	seen := make(map[string]struct{}, len(credentialValues))
	for _, item := range credentialValues {
		value := normalizeSmartQuotaIdentity(item.value)
		if value == "" {
			continue
		}
		identity := item.prefix + value
		if _, found := seen[identity]; found {
			continue
		}
		seen[identity] = struct{}{}
		identities = append(identities, identity)
	}
	if len(identities) > 0 {
		return identities
	}
	if value := normalizeSmartQuotaIdentity(event.AccountSnapshot); value != "" {
		return []string{"account:" + value}
	}
	return nil
}

func smartLiveQuotaObservationFromEvent(event usage.Event, now time.Time) (smartLiveQuotaObservation, bool) {
	identities := smartLiveQuotaEventIdentities(event)
	if len(identities) == 0 {
		return smartLiveQuotaObservation{}, false
	}
	timestampMS := event.TimestampMS
	if timestampMS <= 0 {
		timestampMS = now.UnixMilli()
	}
	metadata := smartQuotaResponseMetadata(event)
	if metadata != nil && metadata.Quota != nil {
		quota := metadata.Quota
		windows := make([]model.CodexInspectionQuotaWindow, 0, 2)
		appendWindow := func(id string, window *usage.HeaderQuotaWindow, fallbackSeconds float64) {
			if window == nil || window.UsedPercent == nil {
				return
			}
			usedFraction, ok := normalizeSmartQuotaFraction(*window.UsedPercent)
			if !ok {
				return
			}
			seconds := fallbackSeconds
			if window.WindowMinutes != nil && *window.WindowMinutes > 0 &&
				!math.IsNaN(*window.WindowMinutes) && !math.IsInf(*window.WindowMinutes, 0) {
				seconds = *window.WindowMinutes * 60
			}
			windows = append(windows, model.CodexInspectionQuotaWindow{
				ID:                 id,
				LabelKey:           id,
				UsedPercent:        smartFloat64Ptr(usedFraction),
				ResetAtMS:          smartQuotaWindowRecoverAtMS(window, timestampMS),
				LimitWindowSeconds: smartFloat64Ptr(seconds),
			})
		}
		appendWindow("runtime-primary", quota.Primary, smartQuotaFiveHourSeconds)
		appendWindow("runtime-secondary", quota.Secondary, smartQuotaWeekSeconds)
		if len(windows) == 0 && quota.UsedPercent != nil {
			usedFraction, ok := normalizeSmartQuotaFraction(*quota.UsedPercent)
			windowKind := strings.ToLower(strings.TrimSpace(quota.SummaryWindowKind))
			if ok && windowKind != "" {
				seconds := float64(smartQuotaWeekSeconds)
				switch windowKind {
				case "five_hour", "five-hour", "5h":
					seconds = smartQuotaFiveHourSeconds
				case "monthly", "month":
					seconds = smartQuotaMonthSeconds
				case "weekly", "week", "seven_day", "seven-day", "7d":
				default:
					return smartLiveQuotaObservation{}, false
				}
				windows = append(windows, model.CodexInspectionQuotaWindow{
					ID:                 "runtime-summary",
					LabelKey:           quota.SummaryWindowKind,
					UsedPercent:        smartFloat64Ptr(usedFraction),
					ResetAtMS:          quota.RecoverAtMS,
					LimitWindowSeconds: smartFloat64Ptr(seconds),
				})
			}
		}
		if len(windows) > 0 {
			return smartLiveQuotaObservation{
				updatedAtMS: timestampMS,
				planType:    strings.ToLower(strings.TrimSpace(quota.PlanType)),
				windows:     windows,
			}, true
		}
	}
	// The legacy flattened percentage may switch between the short and weekly
	// windows. It remains useful for supplier calibration, but without a concrete
	// window identity it must not overwrite capacity for a specific horizon.
	return smartLiveQuotaObservation{}, false
}

func smartFloat64Ptr(value float64) *float64 {
	return &value
}

func (s *Service) recordSmartLiveQuotaObservationsLocked(events []usage.Event, now time.Time) bool {
	if s == nil || len(events) == 0 {
		return false
	}
	if s.smartLiveQuota == nil {
		s.smartLiveQuota = make(map[string]smartLiveQuotaObservation)
	}
	changed := false
	for _, event := range events {
		observation, ok := smartLiveQuotaObservationFromEvent(event, now)
		if !ok {
			continue
		}
		for _, identity := range smartLiveQuotaEventIdentities(event) {
			previous, found := s.smartLiveQuota[identity]
			if found && observation.updatedAtMS < previous.updatedAtMS {
				continue
			}
			if !found || smartLiveQuotaObservationMeaningfullyChanged(previous, observation) {
				changed = true
			}
			s.smartLiveQuota[identity] = cloneSmartLiveQuotaObservation(observation)
		}
	}
	cutoffMS := now.Add(-smartQuotaCalibrationWarmWindow).UnixMilli()
	for identity, observation := range s.smartLiveQuota {
		if observation.updatedAtMS < cutoffMS {
			delete(s.smartLiveQuota, identity)
		}
	}
	return changed
}

func smartLiveQuotaObservationMeaningfullyChanged(previous, current smartLiveQuotaObservation) bool {
	previousRemaining, previousOK := smartLiveQuotaObservationRemaining(previous, time.UnixMilli(previous.updatedAtMS))
	currentRemaining, currentOK := smartLiveQuotaObservationRemaining(current, time.UnixMilli(current.updatedAtMS))
	if previousOK != currentOK || !previousOK {
		return true
	}
	if math.Abs(previousRemaining-currentRemaining) >= 0.005 {
		return true
	}
	return smartLiveQuotaObservationResetAtMS(previous) != smartLiveQuotaObservationResetAtMS(current)
}

func smartLiveQuotaObservationResetAtMS(observation smartLiveQuotaObservation) int64 {
	resetAtMS := int64(0)
	for _, window := range observation.windows {
		if window.ResetAtMS > 0 && (resetAtMS == 0 || window.ResetAtMS < resetAtMS) {
			resetAtMS = window.ResetAtMS
		}
	}
	return resetAtMS
}

func cloneSmartLiveQuotaObservation(observation smartLiveQuotaObservation) smartLiveQuotaObservation {
	observation.windows = append([]model.CodexInspectionQuotaWindow(nil), observation.windows...)
	return observation
}

func smartLiveQuotaObservationWindowsAt(observation smartLiveQuotaObservation, now time.Time) []model.CodexInspectionQuotaWindow {
	windows := append([]model.CodexInspectionQuotaWindow(nil), observation.windows...)
	for index := range windows {
		if windows[index].ResetAtMS > 0 && now.UnixMilli() >= windows[index].ResetAtMS {
			used := 0.0
			windows[index].UsedPercent = &used
		}
	}
	return windows
}

func smartLiveQuotaObservationRemaining(observation smartLiveQuotaObservation, now time.Time) (float64, bool) {
	result := store.CodexInspectionResult{QuotaWindows: smartLiveQuotaObservationWindowsAt(observation, now)}
	return inspectionResultRemainingQuotaFraction(result)
}

// applySmartLiveQuotaDelta overlays only accounts whose request telemetry is
// newer than the completed inspection. The returned snapshot owns its result
// slice, so callers can safely adjust quota windows without mutating the cache.
func (s *Service) applySmartLiveQuotaDelta(snapshot inspectionQuotaSnapshot, now time.Time) (inspectionQuotaSnapshot, smartLiveQuotaDelta) {
	if s == nil || len(snapshot.results) == 0 || snapshot.generatedAt.IsZero() {
		return snapshot, smartLiveQuotaDelta{}
	}
	snapshot.results = append([]store.CodexInspectionResult(nil), snapshot.results...)
	baselineMS := snapshot.generatedAt.UnixMilli()
	s.smartMu.RLock()
	defer s.smartMu.RUnlock()
	if len(s.smartLiveQuota) == 0 {
		return snapshot, smartLiveQuotaDelta{}
	}
	delta := smartLiveQuotaDelta{}
	for index := range snapshot.results {
		result := snapshot.results[index]
		var selected smartLiveQuotaObservation
		for _, identity := range smartQuotaCalibrationResultIdentities(
			result.FileName,
			result.AuthIndex,
			result.AccountKey,
			result.AccountID,
		) {
			observation, found := s.smartLiveQuota[identity]
			if !found || observation.updatedAtMS <= baselineMS || observation.updatedAtMS < selected.updatedAtMS {
				continue
			}
			selected = observation
		}
		if selected.updatedAtMS <= baselineMS {
			continue
		}
		windows := smartLiveQuotaObservationWindowsAt(selected, now)
		if len(windows) == 0 {
			continue
		}
		result.QuotaWindows = windows
		result.QuotaWindowsJSON = ""
		result.QuotaInventoryObserved = true
		if selected.planType != "" {
			result.PlanType = selected.planType
		}
		snapshot.results[index] = result
		delta.accounts++
		if selected.updatedAtMS > delta.updatedAtMS {
			delta.updatedAtMS = selected.updatedAtMS
		}
	}
	return snapshot, delta
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
	s.ensureSmartQuotaCalibrationStateLocked()
	supplierScoreChanged := false

	for _, baseline := range baselines {
		if baseline.identity == "" || baseline.windowTokens <= 0 ||
			!smartQuotaClassificationFractionEligible(baseline.fraction) {
			continue
		}
		s.assignSmartQuotaSupplierToIdentityLocked(baseline.identity, baseline.supplierID)

		observation := s.smartQuotaState.observations[baseline.identity]
		credentialGenerationChanged := baseline.credentialEffectiveFromMS > 0 &&
			observation.credentialEffectiveFromMS != baseline.credentialEffectiveFromMS
		supplierChanged := observation.supplierID != "" && baseline.supplierID != "" &&
			observation.supplierID != baseline.supplierID
		if credentialGenerationChanged {
			// Warm replay can span two credentials that reused one filename. Drop
			// the prior generation once, then retain deltas learned after this marker.
			s.removeSmartQuotaSamplesThroughLocked(baseline.identity, baseline.observedMS)
			delete(s.smartQuotaState.directSamples, baseline.identity)
			delete(s.smartQuotaState.provisionalSamples, baseline.identity)
		}
		if credentialGenerationChanged || supplierChanged {
			if _, existed := s.smartQuotaState.supplierSamples[baseline.identity]; existed {
				delete(s.smartQuotaState.supplierSamples, baseline.identity)
				supplierScoreChanged = true
			}
		}
		if (observation.recoverAtMS > 0 && baseline.recoverAtMS > 0 && observation.recoverAtMS != baseline.recoverAtMS) ||
			supplierChanged {
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
		completeCoverage := smartQuotaWindowBaselineHasCompleteCoverage(baseline)
		if completeCoverage {
			// The local aggregate starts at the provider window boundary, so the
			// supplier estimate may continue to use the full cumulative numerator.
			observation.supplierBaselineFraction = 0
			observation.supplierBaselineTokens = 0
			observation.hasSupplierBaseline = true
		} else {
			// Mid-window imports seed a safe delta baseline. The first supplier
			// estimate is emitted only after another 5% has been consumed locally.
			observation.supplierBaselineFraction = baseline.fraction
			observation.supplierBaselineTokens = baseline.windowTokens
			observation.hasSupplierBaseline = true
		}
		s.smartQuotaState.observations[baseline.identity] = observation

		// Supplier credentials commonly arrive after their weekly quota window
		// has already started and may already show 20-80% used. The local Token
		// database then contains only post-import traffic. Treating that partial
		// tail as a complete-window numerator produced false 10-30M account
		// estimates and collapsed a 16 x 60M pool to roughly 250M. Keep the
		// observation as the baseline for future percentage deltas. The configured
		// fallback remains in use unless separately validated runtime deltas exist.
		if !completeCoverage {
			delete(s.smartQuotaState.directSamples, baseline.identity)
			delete(s.smartQuotaState.provisionalSamples, baseline.identity)
			continue
		}

		// A complete-window aggregate supersedes older runtime deltas for this
		// credential. Partial-window baselines only seed future deltas, so their
		// already validated runtime samples remain useful after refresh/restart.
		s.removeSmartQuotaSamplesThroughLocked(baseline.identity, baseline.observedMS)

		capacityM := float64(baseline.windowTokens) / baseline.fraction / 1_000_000
		if !smartQuotaCalibrationCapacityValid(capacityM) {
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
		if baseline.fraction+1e-9 >= supplierQuotaEstimateMinUsedFraction {
			supplierSample := sample
			supplierSample.weight = baseline.fraction
			supplierSample.classificationOnly = true
			supplierSample.completeWindow = baseline.fraction+1e-9 >= supplierQuotaEstimateExhaustedFraction
			if s.upsertSmartQuotaSupplierSampleLocked(supplierSample) {
				supplierScoreChanged = true
			}
		}
		if sample.classificationOnly {
			s.smartQuotaState.provisionalSamples[baseline.identity] = sample
		} else {
			delete(s.smartQuotaState.provisionalSamples, baseline.identity)
			s.smartQuotaState.directSamples[baseline.identity] = sample
		}
	}
	s.pruneSmartQuotaCalibrationLocked(now)
	s.smartMu.Unlock()
	if supplierScoreChanged {
		s.invalidateAllMarketplaceSupplierQuotaScores()
	}
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

func (s *Service) recordSmartQuotaCalibrationEventsLocked(events []usage.Event, now time.Time) bool {
	if s == nil || len(events) == 0 {
		return false
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
	supplierScoreChanged := false
	for _, event := range ordered {
		if s.recordSmartQuotaCalibrationEventLocked(event, now) {
			supplierScoreChanged = true
		}
	}
	s.pruneSmartQuotaCalibrationLocked(now)
	return supplierScoreChanged
}

func (s *Service) recordSmartQuotaCalibrationEventLocked(event usage.Event, now time.Time) bool {
	identity := smartQuotaCalibrationEventIdentity(event)
	if identity == "" {
		return false
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
		return false
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
		observation.supplierBaselineFraction = fraction
		observation.supplierBaselineTokens = observation.windowTokens
		observation.hasSupplierBaseline = true
		s.smartQuotaState.observations[identity] = observation
		return false
	}

	supplierScoreChanged := false
	if !observation.hasSupplierBaseline {
		observation.supplierBaselineFraction = observation.lastSampleFraction
		observation.supplierBaselineTokens = observation.lastSampleTokens
		observation.hasSupplierBaseline = true
	}
	supplierDelta := fraction - observation.supplierBaselineFraction
	supplierTokens := observation.windowTokens - observation.supplierBaselineTokens
	if supplierTokens > 0 && supplierDelta+1e-9 >= supplierQuotaEstimateMinUsedFraction {
		capacityM := float64(supplierTokens) / supplierDelta / 1_000_000
		if smartQuotaCalibrationCapacityValid(capacityM) {
			supplierSample := smartQuotaCalibrationSample{
				identity:           identity,
				supplierID:         observation.supplierID,
				planType:           observation.planType,
				capacityM:          capacityM,
				weight:             supplierDelta,
				usedFraction:       supplierDelta,
				observedMS:         ts,
				completeWindow:     fraction+1e-9 >= supplierQuotaEstimateExhaustedFraction,
				classificationOnly: true,
			}
			if s.upsertSmartQuotaSupplierSampleLocked(supplierSample) {
				supplierScoreChanged = true
			}
		}
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
		if smartQuotaCalibrationCapacityValid(capacityM) {
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
	return supplierScoreChanged
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
	for identity, sample := range s.smartQuotaState.supplierSamples {
		if sample.observedMS < cutoff {
			delete(s.smartQuotaState.supplierSamples, identity)
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
	return s.smartQuotaEstimateForSupplierAtWithMinimumOptions(
		now,
		supplierID,
		planType,
		minimumCapacityM,
		false,
		identities...,
	)
}

func (s *Service) smartQuotaEstimateForSupplierAtWithMinimumOptions(
	now time.Time,
	supplierID string,
	planType string,
	minimumCapacityM float64,
	includeUnassignedSupplier bool,
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
		if sample.observedMS >= cutoff &&
			smartQuotaSampleMatchesSupplier(sample, supplierID, includeUnassignedSupplier) {
			samples = append(samples, sample)
		}
	}
	for _, sample := range s.smartQuotaState.directSamples {
		if sample.observedMS >= cutoff &&
			smartQuotaSampleMatchesSupplier(sample, supplierID, includeUnassignedSupplier) {
			samples = append(samples, sample)
		}
	}
	for _, sample := range s.smartQuotaState.provisionalSamples {
		if sample.observedMS >= cutoff &&
			smartQuotaSampleMatchesSupplier(sample, supplierID, includeUnassignedSupplier) {
			samples = append(samples, sample)
		}
	}
	s.smartMu.RUnlock()
	if minimumCapacityM > 0 {
		// The policy floor applies to an independently observed quota class, not
		// to every account point in isolation. A legitimate class may straddle the
		// floor because integer percentages and unequal evidence weights spread its
		// members (for example 21.5M-31.6M around a 30M floor). Keep that whole
		// class when any representative reaches the trusted floor, while removing a
		// clearly separated class whose entire range stays below it.
		if planType != "" {
			samples = filterSmartQuotaSamples(samples, func(sample smartQuotaCalibrationSample) bool {
				return sample.planType == planType
			})
		}
		samples = filterSmartQuotaSamplesByClassFloor(samples, minimumCapacityM, now)
	}
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
	var currentQuotaClasses []SmartQuotaClassEstimate
	if len(normalizedIdentities) > 0 {
		currentSamples := filterSmartQuotaSamples(recentSamples, func(sample smartQuotaCalibrationSample) bool {
			_, ok := normalizedIdentities[sample.identity]
			return ok
		})
		currentClassSamples := currentSamples
		if planType != "" {
			currentClassSamples = filterSmartQuotaSamples(currentSamples, func(sample smartQuotaCalibrationSample) bool {
				return sample.planType == planType
			})
		}
		currentQuotaClasses = estimateSmartQuotaClassesAt(currentClassSamples, now)
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
		return attachSmartQuotaClassesForCurrentContext(calibrateSmartQuotaCurrentEstimate(
			currentEstimate,
			recentEstimate,
			recentOK,
			historicalEstimate,
			historicalOK,
		), currentQuotaClasses, quotaClasses)
	}
	if recentOK {
		recentEstimate.RecentEstimateM = recentEstimate.CapacityM
		if historicalOK {
			recentEstimate.HistoricalEstimateM = historicalEstimate.CapacityM
		}
		return attachSmartQuotaClassesForCurrentContext(recentEstimate, currentQuotaClasses, quotaClasses)
	}
	if allOK {
		allEstimate.HistoricalEstimateM = allEstimate.CapacityM
		return attachSmartQuotaClassesForCurrentContext(allEstimate, currentQuotaClasses, quotaClasses)
	}
	if classifiedEstimate, classifiedOK := estimateSmartQuotaTrustedRepresentativesAt(classSamples, now); classifiedOK {
		// The plan/global estimators intentionally require several delta rows per
		// account. A completed inspection can still leave one trustworthy (>10%
		// observed) representative for several independent accounts. Once abnormal
		// low rows have been removed, rebuild the displayed plan statistic from
		// those account representatives instead of publishing either the rejected
		// low current account or a contradictory "no data" value next to populated
		// quota classes.
		return attachSmartQuotaClassesForCurrentContext(classifiedEstimate, currentQuotaClasses, quotaClasses)
	}
	return attachSmartQuotaClassesForCurrentContext(defaultSmartQuotaEstimateForPlan(planType), currentQuotaClasses, quotaClasses)
}

func smartQuotaSampleMatchesSupplier(
	sample smartQuotaCalibrationSample,
	supplierID string,
	includeUnassignedSupplier bool,
) bool {
	if supplierID == "" {
		return true
	}
	sampleSupplierID := normalizeSmartQuotaSupplierID(sample.supplierID)
	return sampleSupplierID == supplierID || (includeUnassignedSupplier && sampleSupplierID == "")
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

// smartQuotaSupplierEstimateForAt is deliberately account-scoped. It exposes a
// provisional seller-gate estimate after 5% locally observed consumption while
// leaving the pool-wide planning estimator on its stricter multi-sample rules.
// When the quota reaches 100%, the same map entry becomes the final sample and
// therefore updates, rather than increments, the seller's evidence count.
func (s *Service) smartQuotaSupplierEstimateForAt(now time.Time, identities ...string) (smartQuotaEstimate, bool) {
	if s == nil || len(identities) == 0 {
		return smartQuotaEstimate{}, false
	}
	cutoff := now.Add(-smartQuotaCalibrationRecentWindow).UnixMilli()
	var best smartQuotaCalibrationSample
	found := false
	s.smartMu.RLock()
	for _, identity := range identities {
		identity = strings.TrimSpace(identity)
		if identity == "" {
			continue
		}
		sample, ok := s.smartQuotaState.supplierSamples[identity]
		if !ok || sample.observedMS < cutoff || sample.capacityM <= 0 {
			continue
		}
		if !found || (sample.completeWindow && !best.completeWindow) ||
			(sample.completeWindow == best.completeWindow && sample.observedMS > best.observedMS) ||
			(sample.completeWindow == best.completeWindow && sample.observedMS == best.observedMS && sample.weight > best.weight) {
			best = sample
			found = true
		}
	}
	s.smartMu.RUnlock()
	if found {
		source := smartQuotaEstimateSourceSupplierEarly
		confidence := smartConfidenceLow
		completeWindowAccounts := 0
		if best.completeWindow {
			source = smartQuotaEstimateSourceSupplierFinal
			confidence = smartConfidenceHigh
			completeWindowAccounts = 1
		}
		return smartQuotaEstimate{
			CapacityM:              round2(best.capacityM),
			Source:                 source,
			SampleCount:            1,
			EvidenceCount:          1,
			ObservedPercent:        round2(best.weight * 100),
			Confidence:             confidence,
			UniqueAccounts:         1,
			CompleteWindowAccounts: completeWindowAccounts,
			IndependentAccount:     true,
			Provisional:            !best.completeWindow,
		}, true
	}
	return s.smartQuotaCurrentEstimateForAt(now, identities...)
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
			!smartQuotaCalibrationCapacityValid(sample.capacityM) ||
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
		current.CapacityM = round2(floorSmartQuotaCalibrationCapacity(weightedCapacity / totalWeight))
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
	switch strings.ToLower(strings.TrimSpace(planType)) {
	case "team":
		return smartDefaultTeamAccountQuotaMillionTokens
	case "plus":
		return smartDefaultPlusAccountQuotaMillionTokens
	case "pro":
		return smartDefaultProAccountQuotaMillionTokens
	default:
		return smartDefaultAccountQuotaMillionTokens
	}
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
			policy.FallbackM = floorSmartQuotaCalibrationCapacity(configured.FallbackM)
		}
		if configured.FixedM > 0 {
			policy.FixedM = floorSmartQuotaCalibrationCapacity(configured.FixedM)
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
			policy.FallbackM = floorSmartQuotaCalibrationCapacity(configured.FallbackM)
		}
		if configured.FixedM > 0 {
			policy.FixedM = floorSmartQuotaCalibrationCapacity(configured.FixedM)
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
	return max(estimate.UniqueAccounts, estimate.CalibrationAccountCount, smartQuotaClassAccountCount(estimate.QuotaClasses))
}

func smartQuotaEstimateDisplayAccountCount(estimate smartQuotaEstimate) int {
	return max(estimate.UniqueAccounts, smartQuotaClassAccountCount(estimate.QuotaClasses))
}

func smartQuotaClassAccountCount(classes []SmartQuotaClassEstimate) int {
	accounts := 0
	for _, quotaClass := range classes {
		accounts += max(0, quotaClass.AccountCount)
	}
	return accounts
}

func smartQuotaPublishedObservedM(estimate smartQuotaEstimate, rejectedAccounts int) float64 {
	if estimate.CapacityM <= 0 || rejectedAccounts <= 0 || len(estimate.QuotaClasses) == 0 ||
		(estimate.Source != smartQuotaEstimateSourceCurrent && estimate.Source != smartQuotaEstimateSourceRecalibrated) {
		return estimate.CapacityM
	}
	for _, quotaClass := range estimate.QuotaClasses {
		if quotaClass.ID == estimate.QuotaClassID && quotaClass.CenterM > 0 {
			return quotaClass.CenterM
		}
	}
	best := estimate.QuotaClasses[0]
	bestDistance := math.Abs(estimate.CapacityM - best.CenterM)
	for _, quotaClass := range estimate.QuotaClasses[1:] {
		distance := math.Abs(estimate.CapacityM - quotaClass.CenterM)
		if distance < bestDistance {
			best = quotaClass
			bestDistance = distance
		}
	}
	return best.CenterM
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
		plans := make(map[string]struct{}, len(cfg.QuotaEstimationPolicies)+len(platform.QuotaEstimationPolicies)+4)
		plans["team"] = struct{}{}
		plans["plus"] = struct{}{}
		plans["pro"] = struct{}{}
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
		includeUnassignedSupplier := len(platforms) == 1 && context.supplierID != "" &&
			normalizeSmartQuotaSupplierID(platforms[0].ID) == context.supplierID
		state := s.quotaPolicyState[key]
		if state.mode != policy.Mode || state.adoptedM <= 0 {
			state = smartQuotaPlanAdoptionState{
				mode:            policy.Mode,
				adoptedM:        policy.FallbackM,
				requiredRounds:  smartQuotaPolicyRequiredRounds,
				validationState: smartQuotaValidationInsufficient,
			}
		}

		rawObserved := s.smartQuotaEstimateForSupplierAtWithMinimumOptions(
			now,
			context.supplierID,
			planType,
			0,
			includeUnassignedSupplier,
			context.identities...,
		)
		// Historical samples for a configured type remain useful once that type
		// appears again, but a type with zero current accounts must not raise a
		// calibration warning or influence this pool's ordering decision.
		rawHasObservedData := context.accounts > 0 && smartQuotaEstimateHasValidData(rawObserved)
		candidateObserved := rawObserved
		hasData := context.accounts > 0 && smartQuotaEstimateHasTrustedPlanData(candidateObserved)
		filteredObserved := rawObserved
		filteredTrusted := false
		rejectedAccounts := 0
		displayRejectedAccounts := 0
		if policy.Mode == smartQuotaPolicyModeAuto && context.accounts > 0 && state.adoptedM > 0 {
			minimumAcceptedM := state.adoptedM * (1 - smartQuotaPolicyExtremeDivergence)
			filteredObserved = s.smartQuotaEstimateForSupplierAtWithMinimumOptions(
				now,
				context.supplierID,
				planType,
				minimumAcceptedM,
				includeUnassignedSupplier,
				context.identities...,
			)
			rejectedAccounts = max(0,
				smartQuotaEstimateAccountCount(rawObserved)-smartQuotaEstimateAccountCount(filteredObserved),
			)
			displayRejectedAccounts = rejectedAccounts
			if rawObserved.CurrentCohortClasses && filteredObserved.CurrentCohortClasses {
				displayRejectedAccounts = max(0,
					smartQuotaEstimateDisplayAccountCount(rawObserved)-smartQuotaEstimateDisplayAccountCount(filteredObserved),
				)
			}
			filteredTrusted = rejectedAccounts > 0 && smartQuotaEstimateHasTrustedPlanData(filteredObserved)
			if rejectedAccounts > 0 {
				// A class that is entirely below the policy floor is never an adoption
				// candidate. Normal-range survivors may continue calibration once they
				// have enough independent accounts; otherwise ordering stays on fallback.
				candidateObserved = filteredObserved
				hasData = filteredTrusted
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
		if rejectedAccounts > 0 {
			// Rejected classes are never part of the published statistic. Adoption may
			// still remain on the trusted fallback until enough corrected samples exist.
			publishedObserved = filteredObserved
			publishedRejectedAccounts = displayRejectedAccounts
		}
		hasPublishedObservedData := context.accounts > 0 && smartQuotaEstimateHasValidData(publishedObserved)
		publishedObservedM := 0.0
		if hasPublishedObservedData {
			publishedObservedM = smartQuotaPublishedObservedM(publishedObserved, publishedRejectedAccounts)
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
		if !smartQuotaCalibrationCapacityValid(sample.capacityM) ||
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
		CapacityM:              round2(floorSmartQuotaCalibrationCapacity(median)),
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

func smartQuotaClassPointsAt(
	samples []smartQuotaCalibrationSample,
	now time.Time,
) ([]smartQuotaClassPoint, []smartQuotaClassPoint) {
	cutoff := now.Add(-smartQuotaCalibrationSampleTTL).UnixMilli()
	grouped := make(map[string][]smartQuotaCalibrationSample)
	for _, sample := range samples {
		if sample.identity == "" || sample.observedMS < cutoff || sample.weight <= 0 ||
			!smartQuotaCalibrationCapacityValid(sample.capacityM) ||
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
	return trustedPoints, provisionalPoints
}

func smartQuotaClassGroupsAt(samples []smartQuotaCalibrationSample, now time.Time) []smartQuotaClassGroup {
	trustedPoints, provisionalPoints := smartQuotaClassPointsAt(samples, now)
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
	return groups
}

func filterSmartQuotaSamplesByClassFloor(
	samples []smartQuotaCalibrationSample,
	minimumCapacityM float64,
	now time.Time,
) []smartQuotaCalibrationSample {
	if minimumCapacityM <= 0 || len(samples) == 0 {
		return append([]smartQuotaCalibrationSample(nil), samples...)
	}
	groups := smartQuotaClassGroupsAt(samples, now)
	acceptedIdentities := make(map[string]struct{})
	for _, group := range groups {
		maximumM := 0.0
		for _, point := range group.points {
			maximumM = math.Max(maximumM, point.capacityM)
		}
		if maximumM+1e-9 < minimumCapacityM {
			continue
		}
		for _, point := range group.points {
			acceptedIdentities[point.identity] = struct{}{}
		}
	}
	return filterSmartQuotaSamples(samples, func(sample smartQuotaCalibrationSample) bool {
		_, ok := acceptedIdentities[sample.identity]
		return ok
	})
}

func estimateSmartQuotaClassesAt(samples []smartQuotaCalibrationSample, now time.Time) []SmartQuotaClassEstimate {
	groups := smartQuotaClassGroupsAt(samples, now)
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

func estimateSmartQuotaTrustedRepresentativesAt(samples []smartQuotaCalibrationSample, now time.Time) (smartQuotaEstimate, bool) {
	cutoff := now.Add(-smartQuotaCalibrationSampleTTL).UnixMilli()
	grouped := make(map[string][]smartQuotaCalibrationSample)
	for _, sample := range samples {
		if sample.identity == "" || sample.observedMS < cutoff || sample.weight <= 0 ||
			!smartQuotaCalibrationCapacityValid(sample.capacityM) ||
			sample.classificationOnly || !smartQuotaCalibrationUsedFractionEligible(sample.usedFraction) {
			continue
		}
		grouped[sample.identity] = append(grouped[sample.identity], sample)
	}

	representatives := make([]float64, 0, len(grouped))
	totalObservedWeight := 0.0
	evidenceCount := 0
	completeWindowAccounts := 0
	for _, identitySamples := range grouped {
		complete := filterSmartQuotaSamples(identitySamples, func(sample smartQuotaCalibrationSample) bool {
			return sample.completeWindow
		})
		if len(complete) > 0 {
			identitySamples = complete
			completeWindowAccounts++
		}
		identitySamples = trimSmartQuotaSampleExtremes(identitySamples)
		weighted := make([]smartQuotaWeightedPoint, 0, len(identitySamples))
		for _, sample := range identitySamples {
			recencyWeight := smartQuotaSampleRecencyWeight(now, sample.observedMS)
			weight := sample.weight * recencyWeight * clampFloat(sample.usedFraction, 0.05, 1)
			if weight <= 0 {
				continue
			}
			weighted = append(weighted, smartQuotaWeightedPoint{capacityM: sample.capacityM, weight: weight})
			totalObservedWeight += sample.weight * recencyWeight
			evidenceCount++
		}
		if len(weighted) == 0 {
			continue
		}
		representatives = append(representatives, weightedSmartQuotaMedian(weighted))
	}
	if len(representatives) == 0 {
		return smartQuotaEstimate{}, false
	}

	sort.Float64s(representatives)
	if len(representatives) >= 5 {
		representatives = representatives[1 : len(representatives)-1]
	}
	middle := len(representatives) / 2
	median := representatives[middle]
	if len(representatives)%2 == 0 {
		median = (representatives[middle-1] + representatives[middle]) / 2
	}
	confidence := smartConfidenceLow
	if len(representatives) >= smartQuotaPolicyMinUniqueAccounts {
		confidence = smartConfidenceMedium
	}
	if len(representatives) >= 12 && totalObservedWeight >= 0.5 {
		confidence = smartConfidenceHigh
	}
	return smartQuotaEstimate{
		CapacityM:              round2(floorSmartQuotaCalibrationCapacity(median)),
		Source:                 smartQuotaEstimateSourceClassified,
		SampleCount:            len(representatives),
		EvidenceCount:          evidenceCount,
		ObservedPercent:        round2(totalObservedWeight * 100),
		Confidence:             confidence,
		UniqueAccounts:         len(representatives),
		CompleteWindowAccounts: completeWindowAccounts,
		IndependentAccount:     completeWindowAccounts > 0,
	}, true
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
	estimate.CalibrationAccountCount = smartQuotaClassAccountCount(classes)
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

func attachSmartQuotaCurrentClasses(
	estimate smartQuotaEstimate,
	currentClasses []SmartQuotaClassEstimate,
	calibrationClasses []SmartQuotaClassEstimate,
) smartQuotaEstimate {
	estimate = attachSmartQuotaClasses(estimate, currentClasses)
	estimate.CalibrationAccountCount = smartQuotaClassAccountCount(calibrationClasses)
	estimate.CurrentCohortClasses = true
	return estimate
}

func attachSmartQuotaClassesForCurrentContext(
	estimate smartQuotaEstimate,
	currentClasses []SmartQuotaClassEstimate,
	calibrationClasses []SmartQuotaClassEstimate,
) smartQuotaEstimate {
	if len(currentClasses) > 0 {
		return attachSmartQuotaCurrentClasses(estimate, currentClasses, calibrationClasses)
	}
	return attachSmartQuotaClasses(estimate, calibrationClasses)
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
