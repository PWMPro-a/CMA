package supply

import (
	"fmt"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/usage"
)

func smartQuotaCalibrationFixture(identity, plan string, start time.Time, capacityM float64) []usage.Event {
	events := make([]usage.Event, 0, 11)
	for step := 0; step <= 10; step++ {
		usedPercent := float64(step * 10)
		tokens := int64(0)
		if step > 0 {
			tokens = int64(capacityM * 1_000_000 / 10)
		}
		events = append(events, usage.Event{
			EventHash:              fmt.Sprintf("%s-%d", identity, step),
			TimestampMS:            start.Add(time.Duration(step) * time.Second).UnixMilli(),
			Provider:               "codex",
			AuthFileSnapshot:       identity,
			HeaderQuotaUsedPercent: floatPtr(usedPercent),
			HeaderQuotaRecoverAtMS: start.Add(5 * time.Hour).UnixMilli(),
			HeaderQuotaPlanType:    plan,
			TotalTokens:            tokens,
			ResponseMetadata:       smartQuotaWeeklyMetadata(plan, usedPercent, start.Add(5*time.Hour).UnixMilli()),
		})
	}
	return events
}

func TestSmartQuotaCalibrationLearnsIdentityAndPlanCapacity(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Add(-time.Minute)
	for index := 0; index < 3; index++ {
		events := smartQuotaCalibrationFixture(fmt.Sprintf("team-%d.json", index), "team", now.Add(time.Duration(index)*time.Minute), 40)
		service.recordSmartUsageEvents(events, now.Add(5*time.Minute))
	}

	identity := service.smartQuotaEstimateFor("team", "file:team-0.json")
	if identity.Source != smartQuotaEstimateSourceCurrent || identity.CapacityM != 40 || identity.SampleCount != 1 || identity.EvidenceCount != 3 {
		t.Fatalf("identity estimate = %#v", identity)
	}
	plan := service.smartQuotaEstimateFor("team")
	if plan.Source != smartQuotaEstimateSourceRecentPlan || plan.CapacityM != 40 || plan.SampleCount != 3 || plan.EvidenceCount != 9 || plan.ObservedPercent != 90 {
		t.Fatalf("plan estimate = %#v", plan)
	}
}

func TestSmartQuotaCalibrationUsesWindowHistoryAndRemainingPercentage(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	recoverAt := now.Add(7 * 24 * time.Hour).UnixMilli()
	events := []usage.Event{
		{
			TimestampMS:      now.Add(-2 * time.Minute).UnixMilli(),
			Provider:         "codex",
			AuthFileSnapshot: "active.json",
			TotalTokens:      54_400_000,
		},
		{
			TimestampMS:            now.Add(-90 * time.Second).UnixMilli(),
			Provider:               "codex",
			AuthFileSnapshot:       "active.json",
			HeaderQuotaUsedPercent: floatPtr(80),
			HeaderQuotaRecoverAtMS: recoverAt,
			HeaderQuotaPlanType:    "team",
			ResponseMetadata:       smartQuotaWeeklyMetadata("team", 80, recoverAt),
		},
		{
			TimestampMS:            now.Add(-60 * time.Second).UnixMilli(),
			Provider:               "codex",
			AuthFileSnapshot:       "active.json",
			HeaderQuotaUsedPercent: floatPtr(85),
			HeaderQuotaRecoverAtMS: recoverAt,
			HeaderQuotaPlanType:    "team",
			TotalTokens:            3_000_000,
			ResponseMetadata:       smartQuotaWeeklyMetadata("team", 85, recoverAt),
		},
		{
			TimestampMS:            now.Add(-30 * time.Second).UnixMilli(),
			Provider:               "codex",
			AuthFileSnapshot:       "active.json",
			HeaderQuotaUsedPercent: floatPtr(90),
			HeaderQuotaRecoverAtMS: recoverAt,
			HeaderQuotaPlanType:    "team",
			TotalTokens:            3_000_000,
			ResponseMetadata:       smartQuotaWeeklyMetadata("team", 90, recoverAt),
		},
		{
			TimestampMS:            now.UnixMilli(),
			Provider:               "codex",
			AuthFileSnapshot:       "active.json",
			HeaderQuotaUsedPercent: floatPtr(95),
			HeaderQuotaRecoverAtMS: recoverAt,
			HeaderQuotaPlanType:    "team",
			TotalTokens:            3_000_000,
			ResponseMetadata:       smartQuotaWeeklyMetadata("team", 95, recoverAt),
		},
	}
	service.recordSmartUsageEvents(events, now)

	estimate := service.smartQuotaEstimateForAt(now, "team", "file:active.json")
	if estimate.CapacityM != 60 || estimate.Source != smartQuotaEstimateSourceCurrent ||
		estimate.SampleCount != 1 || estimate.EvidenceCount != 3 {
		t.Fatalf("window-history estimate = %#v", estimate)
	}
	observation := service.smartQuotaState.observations["file:active.json"]
	if observation.windowTokens != 63_400_000 || observation.lastFraction != 0.95 {
		t.Fatalf("window observation = %#v", observation)
	}
}

func TestSmartQuotaRuntimeEstimateRequiresTenPercentObservedDeltaAndConsistentSamples(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	fallback := smartQuotaEstimate{CapacityM: 60, Source: smartQuotaEstimateSourceDefault}

	insufficient := New(nil, nil)
	identity := "file:noisy.json"
	insufficient.smartQuotaState.samplesByIdentity[identity] = []smartQuotaCalibrationSample{
		{identity: identity, planType: "team", capacityM: 10, weight: 0.01, usedFraction: 0.71, observedMS: now.Add(-3 * time.Minute).UnixMilli()},
		{identity: identity, planType: "team", capacityM: 30, weight: 0.01, usedFraction: 0.72, observedMS: now.Add(-2 * time.Minute).UnixMilli()},
		{identity: identity, planType: "team", capacityM: 90, weight: 0.01, usedFraction: 0.73, observedMS: now.Add(-time.Minute).UnixMilli()},
	}
	got := insufficient.smartQuotaEstimateForInspectionResult(
		store.CodexInspectionResult{FileName: "noisy.json", PlanType: "team"},
		fallback,
		now,
	)
	if got.CapacityM != 60 || got.Source != smartQuotaEstimateSourceDefault {
		t.Fatalf("three noisy one-percent samples replaced fallback: %#v", got)
	}

	inconsistent := New(nil, nil)
	inconsistentIdentity := "file:inconsistent.json"
	inconsistent.smartQuotaState.samplesByIdentity[inconsistentIdentity] = []smartQuotaCalibrationSample{
		{identity: inconsistentIdentity, planType: "team", capacityM: 10, weight: 0.04, usedFraction: 0.24, observedMS: now.Add(-3 * time.Minute).UnixMilli()},
		{identity: inconsistentIdentity, planType: "team", capacityM: 30, weight: 0.04, usedFraction: 0.28, observedMS: now.Add(-2 * time.Minute).UnixMilli()},
		{identity: inconsistentIdentity, planType: "team", capacityM: 90, weight: 0.04, usedFraction: 0.32, observedMS: now.Add(-time.Minute).UnixMilli()},
	}
	got = inconsistent.smartQuotaEstimateForInspectionResult(
		store.CodexInspectionResult{FileName: "inconsistent.json", PlanType: "team"},
		fallback,
		now,
	)
	if got.CapacityM != 60 || got.Source != smartQuotaEstimateSourceDefault {
		t.Fatalf("inconsistent twelve-percent runtime samples replaced fallback: %#v", got)
	}

	trusted := New(nil, nil)
	trustedIdentity := "file:trusted.json"
	trusted.smartQuotaState.samplesByIdentity[trustedIdentity] = []smartQuotaCalibrationSample{
		{identity: trustedIdentity, planType: "team", capacityM: 58, weight: 0.04, usedFraction: 0.24, observedMS: now.Add(-3 * time.Minute).UnixMilli()},
		{identity: trustedIdentity, planType: "team", capacityM: 60, weight: 0.04, usedFraction: 0.28, observedMS: now.Add(-2 * time.Minute).UnixMilli()},
		{identity: trustedIdentity, planType: "team", capacityM: 62, weight: 0.04, usedFraction: 0.32, observedMS: now.Add(-time.Minute).UnixMilli()},
	}
	got = trusted.smartQuotaEstimateForInspectionResult(
		store.CodexInspectionResult{FileName: "trusted.json", PlanType: "team"},
		fallback,
		now,
	)
	if got.CapacityM != 60 || got.Source != smartQuotaEstimateSourceCurrent || got.CurrentEstimateM != 60 {
		t.Fatalf("stable twelve-percent runtime evidence was not adopted: %#v", got)
	}
}

func smartQuotaWeeklyMetadata(plan string, usedPercent float64, recoverAtMS int64) *usage.ResponseHeaderMetadata {
	windowMinutes := 10_080.0
	return &usage.ResponseHeaderMetadata{Quota: &usage.HeaderQuotaMetadata{
		PlanType: plan,
		Secondary: &usage.HeaderQuotaWindow{
			UsedPercent:   &usedPercent,
			ResetAtMS:     recoverAtMS,
			WindowMinutes: &windowMinutes,
		},
	}}
}

func TestEstimateSmartQuotaSamplesDropsHighestAndLowest(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	samples := make([]smartQuotaCalibrationSample, 0, 5)
	for index, capacityM := range []float64{1, 40, 41, 42, 400} {
		samples = append(samples, smartQuotaCalibrationSample{
			identity:     "file:active.json",
			planType:     "team",
			capacityM:    capacityM,
			weight:       0.1,
			usedFraction: 0.8,
			observedMS:   now.Add(time.Duration(index) * time.Millisecond).UnixMilli(),
		})
	}
	estimate, ok := estimateSmartQuotaSamplesAt(samples, smartQuotaEstimateSourceCurrent, 5, 0.1, now.Add(time.Second))
	if !ok || estimate.CapacityM != 41 {
		t.Fatalf("trimmed estimate = %#v/%v", estimate, ok)
	}
}

func TestSmartQuotaWindowBaselinesReplaceTruncatedTailAndTrimAccountExtremes(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	capacities := []float64{10, 59, 60, 61, 200}
	baselines := make([]smartQuotaWindowBaseline, 0, len(capacities))
	identities := make([]string, 0, len(capacities))
	for index, capacityM := range capacities {
		identity := fmt.Sprintf("file:account-%d.json", index)
		identities = append(identities, identity)
		service.smartQuotaState.samples = append(service.smartQuotaState.samples, smartQuotaCalibrationSample{
			identity:     identity,
			planType:     "team",
			capacityM:    2.88,
			weight:       0.05,
			usedFraction: 1,
			observedMS:   now.Add(-time.Minute).UnixMilli(),
		})
		service.smartQuotaState.observations[identity] = smartQuotaCalibrationObservation{
			lastEventMS:        now.Add(-time.Minute).UnixMilli(),
			lastFraction:       1,
			lastSampleFraction: 1,
			windowTokens:       2_880_000,
		}
		baselines = append(baselines, smartQuotaWindowBaseline{
			identity:     identity,
			planType:     "team",
			fraction:     1,
			fromMS:       now.Add(-7 * 24 * time.Hour).UnixMilli(),
			observedMS:   now.UnixMilli(),
			windowTokens: int64(capacityM * 1_000_000),
			firstSeenMS:  now.Add(-7 * 24 * time.Hour).UnixMilli(),
			lastSeenMS:   now.UnixMilli(),
		})
	}

	service.recordSmartQuotaWindowBaselines(baselines, now)
	estimate := service.smartQuotaEstimateForAt(now, "team", identities...)
	if estimate.CapacityM != 60 || estimate.Source != smartQuotaEstimateSourceCurrent || estimate.UniqueAccounts != 3 {
		t.Fatalf("baseline estimate = %#v", estimate)
	}
	for _, identity := range identities {
		observation := service.smartQuotaState.observations[identity]
		if observation.windowTokens <= 2_880_000 {
			t.Fatalf("truncated observation was not replaced for %s: %#v", identity, observation)
		}
	}
}

func TestSmartQuotaCompleteWindowUsesIndependentAccountFormula(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	service.recordSmartQuotaWindowBaselines([]smartQuotaWindowBaseline{{
		identity:     "file:active.json",
		planType:     "team",
		fraction:     0.11,
		fromMS:       now.Add(-7 * 24 * time.Hour).UnixMilli(),
		observedMS:   now.UnixMilli(),
		windowTokens: 4_400_000,
		firstSeenMS:  now.Add(-7 * 24 * time.Hour).UnixMilli(),
		lastSeenMS:   now.UnixMilli(),
	}}, now)

	estimate := service.smartQuotaEstimateForInspectionResult(store.CodexInspectionResult{
		FileName: "active.json",
		PlanType: "team",
	}, smartQuotaEstimate{
		CapacityM: 60,
		Source:    smartQuotaEstimateSourceRecentPlan,
	}, now)
	if estimate.CapacityM != 40 || estimate.Source != smartQuotaEstimateSourceCurrent ||
		!estimate.IndependentAccount || estimate.CurrentEstimateM != 40 {
		t.Fatalf("independent account estimate = %#v", estimate)
	}
}

func TestSmartQuotaWindowImportedAfterStartIsNotComplete(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	windowStart := now.Add(-7 * 24 * time.Hour)
	identity := "file:replaced-mid-window.json"
	service.appendSmartQuotaCalibrationSampleLocked(smartQuotaCalibrationSample{
		identity: identity, planType: "team", capacityM: 31,
		weight: 0.04, usedFraction: 0.9, observedMS: now.Add(-2 * time.Minute).UnixMilli(),
	})
	service.smartQuotaState.directSamples[identity] = smartQuotaCalibrationSample{
		identity: identity, planType: "team", capacityM: 30,
		weight: 1, usedFraction: 1, observedMS: now.Add(-time.Minute).UnixMilli(), completeWindow: true,
	}

	service.recordSmartQuotaWindowBaselines([]smartQuotaWindowBaseline{{
		identity:                  identity,
		planType:                  "team",
		fraction:                  1,
		fromMS:                    windowStart.UnixMilli(),
		observedMS:                now.UnixMilli(),
		credentialEffectiveFromMS: windowStart.Add(10 * time.Minute).UnixMilli(),
		windowTokens:              30_000_000,
		firstSeenMS:               windowStart.UnixMilli(),
		lastSeenMS:                now.UnixMilli(),
	}}, now)

	if _, ok := service.smartQuotaState.directSamples[identity]; ok {
		t.Fatal("mid-window replacement retained a false complete-window estimate")
	}
	if _, ok := service.smartQuotaState.provisionalSamples[identity]; ok {
		t.Fatal("mid-window replacement created a provisional complete-window estimate")
	}
	if len(service.smartQuotaState.samplesByIdentity[identity]) != 0 {
		t.Fatal("mid-window replacement retained runtime samples from the previous credential generation")
	}
	observation := service.smartQuotaState.observations[identity]
	if observation.windowTokens != 30_000_000 || observation.lastFraction != 1 ||
		observation.credentialEffectiveFromMS != windowStart.Add(10*time.Minute).UnixMilli() {
		t.Fatalf("mid-window replacement did not seed delta observation: %#v", observation)
	}

	currentSample := smartQuotaCalibrationSample{
		identity: identity, planType: "team", capacityM: 60,
		weight: 0.04, usedFraction: 1, observedMS: now.Add(time.Second).UnixMilli(),
	}
	service.appendSmartQuotaCalibrationSampleLocked(currentSample)
	service.recordSmartQuotaWindowBaselines([]smartQuotaWindowBaseline{{
		identity:                  identity,
		planType:                  "team",
		fraction:                  1,
		fromMS:                    windowStart.UnixMilli(),
		observedMS:                now.Add(2 * time.Second).UnixMilli(),
		credentialEffectiveFromMS: windowStart.Add(10 * time.Minute).UnixMilli(),
		windowTokens:              30_100_000,
		firstSeenMS:               windowStart.Add(10 * time.Minute).UnixMilli(),
		lastSeenMS:                now.Add(2 * time.Second).UnixMilli(),
	}}, now.Add(2*time.Second))
	if samples := service.smartQuotaState.samplesByIdentity[identity]; len(samples) != 1 || samples[0].capacityM != 60 {
		t.Fatalf("same credential generation did not retain warmed runtime sample: %#v", samples)
	}
}

func TestSmartQuotaWindowUsageStartsAtCurrentCredentialGeneration(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	usedPercent := 50.0
	weeklySeconds := float64(smartQuotaWeekSeconds)
	fileName := "replaced.json"
	effectiveFromMS := now.Add(-12 * time.Hour).UnixMilli()
	baselines, targets := smartQuotaWindowBaselinesForInspection([]store.CodexInspectionResult{{
		FileName:    fileName,
		AuthIndex:   "auth-replaced",
		Provider:    "codex",
		PlanType:    "team",
		CreatedAtMS: now.UnixMilli(),
		QuotaWindows: []model.CodexInspectionQuotaWindow{{
			ID:                 "weekly",
			UsedPercent:        &usedPercent,
			ResetAtMS:          now.Add(6 * 24 * time.Hour).UnixMilli(),
			LimitWindowSeconds: &weeklySeconds,
		}},
	}}, store.CodexInspectionRun{}, map[string]string{fileName: "supplier"}, map[string]int64{
		fileName: effectiveFromMS,
	})

	if len(baselines) != 1 || len(targets) != 1 {
		t.Fatalf("baseline/target count = %d/%d, want 1/1", len(baselines), len(targets))
	}
	if baselines[0].credentialEffectiveFromMS != effectiveFromMS || targets[0].FromMS != effectiveFromMS {
		t.Fatalf("credential generation bounds = baseline %d target %d, want %d",
			baselines[0].credentialEffectiveFromMS, targets[0].FromMS, effectiveFromMS)
	}
}

func TestSmartQuotaCalibrationRequiresStrictlyMoreThanTenPercentUsed(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	service.recordSmartQuotaWindowBaselines([]smartQuotaWindowBaseline{
		{
			identity:     "file:exact-ten.json",
			planType:     "team",
			fraction:     0.10,
			fromMS:       now.Add(-7 * 24 * time.Hour).UnixMilli(),
			observedMS:   now.UnixMilli(),
			windowTokens: 4_000_000,
			firstSeenMS:  now.Add(-7 * 24 * time.Hour).UnixMilli(),
			lastSeenMS:   now.UnixMilli(),
		},
		{
			identity:     "file:above-ten.json",
			planType:     "team",
			fraction:     0.11,
			fromMS:       now.Add(-7 * 24 * time.Hour).UnixMilli(),
			observedMS:   now.UnixMilli(),
			windowTokens: 4_400_000,
			firstSeenMS:  now.Add(-7 * 24 * time.Hour).UnixMilli(),
			lastSeenMS:   now.UnixMilli(),
		},
	}, now)

	if _, ok := service.smartQuotaState.directSamples["file:exact-ten.json"]; ok {
		t.Fatalf("an account at exactly 10%% entered direct calibration")
	}
	if sample, ok := service.smartQuotaState.provisionalSamples["file:exact-ten.json"]; !ok ||
		!sample.classificationOnly || sample.capacityM != 40 {
		t.Fatalf("an account at exactly 10%% was not retained for provisional classification: %#v/%v", sample, ok)
	}
	if sample, ok := service.smartQuotaState.directSamples["file:above-ten.json"]; !ok || sample.capacityM != 40 {
		t.Fatalf("account above 10%% was not calibrated: %#v/%v", sample, ok)
	}
	if estimate := service.smartQuotaEstimateForInspectionResult(
		store.CodexInspectionResult{FileName: "exact-ten.json", PlanType: "team"},
		smartQuotaEstimate{CapacityM: 60, Source: smartQuotaEstimateSourceDefault},
		now,
	); estimate.CapacityM != 60 || estimate.Source != smartQuotaEstimateSourceDefault {
		t.Fatalf("exactly 10%% account replaced fallback capacity: %#v", estimate)
	}
	if _, ok := estimateSmartQuotaSamplesAt([]smartQuotaCalibrationSample{{
		identity: "file:exact-ten.json", planType: "team", capacityM: 40,
		weight: 1, usedFraction: 0.10, observedMS: now.UnixMilli(),
	}}, smartQuotaEstimateSourceCurrent, 1, 0.1, now); ok {
		t.Fatalf("an in-memory sample at exactly 10%% entered estimation")
	}
}

func TestSmartQuotaIncompleteWindowKeepsTeamFallbackUntilDeltaEvidence(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	identity := "file:mid-window-import.json"
	service.smartQuotaState.directSamples[identity] = smartQuotaCalibrationSample{
		identity: identity, planType: "team", capacityM: 20,
		weight: 1, usedFraction: 0.60, observedMS: now.Add(-time.Minute).UnixMilli(), completeWindow: true,
	}
	service.recordSmartQuotaWindowBaselines([]smartQuotaWindowBaseline{{
		identity:     identity,
		planType:     "team",
		fraction:     0.60,
		fromMS:       now.Add(-7 * 24 * time.Hour).UnixMilli(),
		observedMS:   now.UnixMilli(),
		windowTokens: 12_000_000,
		firstSeenMS:  now.Add(-30 * time.Minute).UnixMilli(),
		lastSeenMS:   now.UnixMilli(),
	}}, now)

	if _, ok := service.smartQuotaState.directSamples[identity]; ok {
		t.Fatal("partial local window retained a false complete-window estimate")
	}
	estimate := service.smartQuotaEstimateForInspectionResult(
		store.CodexInspectionResult{FileName: "mid-window-import.json", PlanType: "team"},
		smartQuotaEstimate{CapacityM: 60, Source: smartQuotaEstimateSourceDefault},
		now,
	)
	if estimate.CapacityM != 60 || estimate.Source != smartQuotaEstimateSourceDefault {
		t.Fatalf("partial local window replaced Team fallback: %#v", estimate)
	}
	observation := service.smartQuotaState.observations[identity]
	if observation.windowTokens != 12_000_000 || observation.lastFraction != 0.60 ||
		observation.lastSampleTokens != 12_000_000 || observation.lastSampleFraction != 0.60 {
		t.Fatalf("partial window did not seed the future delta baseline: %#v", observation)
	}
}

func TestSmartQuotaIncompleteWindowKeepsWarmedRuntimeSamplesAndAssignsSupplier(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	identity := "file:warmed-runtime.json"
	for index, capacityM := range []float64{58, 60, 62} {
		service.appendSmartQuotaCalibrationSampleLocked(smartQuotaCalibrationSample{
			identity:     identity,
			planType:     "team",
			capacityM:    capacityM,
			weight:       0.04,
			usedFraction: 0.20 + float64(index)*0.04,
			observedMS:   now.Add(time.Duration(index-3) * time.Minute).UnixMilli(),
		})
	}

	service.recordSmartQuotaWindowBaselines([]smartQuotaWindowBaseline{{
		identity:     identity,
		supplierID:   "legacy",
		planType:     "team",
		fraction:     0.35,
		fromMS:       now.Add(-7 * 24 * time.Hour).UnixMilli(),
		observedMS:   now.UnixMilli(),
		windowTokens: 8_000_000,
		firstSeenMS:  now.Add(-2 * time.Hour).UnixMilli(),
		lastSeenMS:   now.UnixMilli(),
	}}, now)

	estimate := service.smartQuotaEstimateForSupplierAt(now, "legacy", "team", identity)
	if estimate.Source != smartQuotaEstimateSourceCurrent || estimate.SampleCount != 1 ||
		estimate.EvidenceCount != 3 || estimate.UniqueAccounts != 1 || estimate.CapacityM != 60 {
		t.Fatalf("warmed supplier estimate = %#v", estimate)
	}
	for _, sample := range service.smartQuotaState.samples {
		if sample.identity == identity && sample.supplierID != "legacy" {
			t.Fatalf("runtime sample supplier = %q, want legacy", sample.supplierID)
		}
	}
}

func TestSmartQuotaCompleteWindowOverridesRuntimeDeltasForSameAccount(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	samples := []smartQuotaCalibrationSample{
		{
			identity: "file:active.json", planType: "team", capacityM: 60,
			weight: 1, usedFraction: 0.20, observedMS: now.UnixMilli(), completeWindow: true,
		},
		{
			identity: "file:active.json", planType: "team", capacityM: 6,
			weight: 0.01, usedFraction: 0.18, observedMS: now.Add(-time.Second).UnixMilli(),
		},
		{
			identity: "file:active.json", planType: "team", capacityM: 7,
			weight: 0.01, usedFraction: 0.19, observedMS: now.Add(-500 * time.Millisecond).UnixMilli(),
		},
	}
	estimate, ok := estimateSmartQuotaSamplesAt(samples, smartQuotaEstimateSourceCurrent, 1, 0.1, now)
	if !ok || estimate.CapacityM != 60 || estimate.SampleCount != 1 || !estimate.IndependentAccount {
		t.Fatalf("complete-window precedence estimate = %#v/%v", estimate, ok)
	}
}

func TestEstimateSmartQuotaSamplesTrimsThreePointExtremes(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	samples := make([]smartQuotaCalibrationSample, 0, 3)
	for index, capacityM := range []float64{5, 60, 300} {
		samples = append(samples, smartQuotaCalibrationSample{
			identity:     fmt.Sprintf("file:%d.json", index),
			planType:     "team",
			capacityM:    capacityM,
			weight:       1,
			usedFraction: 1,
			observedMS:   now.UnixMilli(),
		})
	}
	estimate, ok := estimateSmartQuotaSamplesAt(samples, smartQuotaEstimateSourceCurrent, 3, 0.1, now)
	if !ok || estimate.CapacityM != 60 {
		t.Fatalf("three-point trimmed estimate = %#v/%v", estimate, ok)
	}
}

func TestSmartQuotaCalibrationRecalibratesOldRegimeAroundCurrentUsage(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	service.smartQuotaState.samples = append(
		quotaSamplesForEstimate("file:old-a.json", "team", 10, now.Add(-8*time.Hour), 3),
		quotaSamplesForEstimate("file:old-b.json", "team", 10, now.Add(-8*time.Hour), 3)...,
	)
	service.smartQuotaState.samples = append(
		service.smartQuotaState.samples,
		quotaSamplesForEstimate("file:active.json", "team", 60, now.Add(-time.Minute), 6)...,
	)

	estimate := service.smartQuotaEstimateForAt(now, "team", "file:active.json")
	if estimate.Source != smartQuotaEstimateSourceRecalibrated || estimate.CapacityM != 57.5 ||
		estimate.CurrentEstimateM != 60 || estimate.RecentEstimateM != 60 || estimate.HistoricalEstimateM != 10 ||
		estimate.DivergencePercent != 500 {
		t.Fatalf("recalibrated estimate = %#v", estimate)
	}
}

func quotaSamplesForEstimate(identity, plan string, capacityM float64, observedAt time.Time, count int) []smartQuotaCalibrationSample {
	samples := make([]smartQuotaCalibrationSample, 0, count)
	for index := 0; index < count; index++ {
		samples = append(samples, smartQuotaCalibrationSample{
			identity:     identity,
			planType:     plan,
			capacityM:    capacityM,
			weight:       0.1,
			usedFraction: 0.8,
			observedMS:   observedAt.Add(time.Duration(index) * time.Second).UnixMilli(),
		})
	}
	return samples
}

func TestNormalizeSmartQuotaFractionTreatsSubOneValuesAsPercent(t *testing.T) {
	for _, tt := range []struct {
		value float64
		want  float64
	}{
		{value: 0.5, want: 0.005},
		{value: 1, want: 0.01},
		{value: 50, want: 0.5},
		{value: 100, want: 1},
	} {
		got, ok := normalizeSmartQuotaFraction(tt.value)
		if !ok || got != tt.want {
			t.Fatalf("normalize quota percent %.2f = %.6f/%v, want %.6f/true", tt.value, got, ok, tt.want)
		}
	}
	if _, ok := normalizeSmartQuotaFraction(100.01); ok {
		t.Fatal("quota percentage above 100 must be rejected")
	}
}

func TestSmartQuotaResultIdentitiesKeepTeamSpacesIndependent(t *testing.T) {
	first := smartQuotaCalibrationResultIdentities("space-a.json", "auth-a", "member@example.com", "shared-account")
	second := smartQuotaCalibrationResultIdentities("space-b.json", "auth-b", "member@example.com", "shared-account")
	if len(first) != 2 || first[0] != "file:space-a.json" || first[1] != "auth:auth-a" {
		t.Fatalf("first space identities = %#v", first)
	}
	if len(second) != 2 || second[0] != "file:space-b.json" || second[1] != "auth:auth-b" {
		t.Fatalf("second space identities = %#v", second)
	}
	for _, identity := range append(append([]string{}, first...), second...) {
		if identity == "account:shared-account" || identity == "account:member@example.com" {
			t.Fatalf("shared account alias leaked into space-scoped identities: %#v / %#v", first, second)
		}
	}

	legacy := smartQuotaCalibrationResultIdentities("", "", "member@example.com", "shared-account")
	if len(legacy) != 2 || legacy[0] != "account:member@example.com" || legacy[1] != "account:shared-account" {
		t.Fatalf("legacy account-only identities = %#v", legacy)
	}
}

func TestSmartQuotaCalibrationRequiresAtLeastTenPercentUsage(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	recoverAt := now.Add(7 * 24 * time.Hour).UnixMilli()
	service.recordSmartUsageEvents([]usage.Event{
		{
			TimestampMS: now.Add(-time.Minute).UnixMilli(), Provider: "codex", AuthFileSnapshot: "low.json",
			HeaderQuotaUsedPercent: floatPtr(0), HeaderQuotaRecoverAtMS: recoverAt,
			HeaderQuotaPlanType: "team", ResponseMetadata: smartQuotaWeeklyMetadata("team", 0, recoverAt),
		},
		{
			TimestampMS: now.UnixMilli(), Provider: "codex", AuthFileSnapshot: "low.json", TotalTokens: 3_000_000,
			HeaderQuotaUsedPercent: floatPtr(5), HeaderQuotaRecoverAtMS: recoverAt,
			HeaderQuotaPlanType: "team", ResponseMetadata: smartQuotaWeeklyMetadata("team", 5, recoverAt),
		},
	}, now)
	if len(service.smartQuotaState.samples) != 0 {
		t.Fatalf("usage below 10%% created quota samples: %#v", service.smartQuotaState.samples)
	}
	estimate := service.smartQuotaEstimateForAt(now, "team", "file:low.json")
	if len(service.smartQuotaState.provisionalSamples) != 1 {
		t.Fatalf("usage below 10%% did not create one provisional sample: %#v", service.smartQuotaState.provisionalSamples)
	}
	if estimate.CapacityM != 60 || estimate.Source != smartQuotaEstimateSourceDefault ||
		estimate.Provisional || estimate.SampleCount != 0 || estimate.QuotaClassID == "" ||
		len(estimate.QuotaClasses) != 1 || estimate.QuotaClasses[0].TrustedAccounts != 0 {
		t.Fatalf("low-usage team pre-classification = %#v", estimate)
	}
}

func TestSmartQuotaSupplierEstimateStartsAtFivePercentAndFinalizesAtExhaustion(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	recoverAt := now.Add(7 * 24 * time.Hour).UnixMilli()
	identity := "file:supplier-fast.json"

	service.recordSmartUsageEvents([]usage.Event{
		{
			TimestampMS: now.Add(-2 * time.Minute).UnixMilli(), Provider: "codex", AuthFileSnapshot: "supplier-fast.json",
			HeaderQuotaUsedPercent: floatPtr(0), HeaderQuotaRecoverAtMS: recoverAt,
			HeaderQuotaPlanType: "plus", ResponseMetadata: smartQuotaWeeklyMetadata("plus", 0, recoverAt),
		},
		{
			TimestampMS: now.Add(-time.Minute).UnixMilli(), Provider: "codex", AuthFileSnapshot: "supplier-fast.json",
			TotalTokens: 6_000_000, HeaderQuotaUsedPercent: floatPtr(5), HeaderQuotaRecoverAtMS: recoverAt,
			HeaderQuotaPlanType: "plus", ResponseMetadata: smartQuotaWeeklyMetadata("plus", 5, recoverAt),
		},
	}, now)

	early, ok := service.smartQuotaSupplierEstimateForAt(now, identity)
	if !ok || early.CapacityM != 120 || !early.Provisional || early.ObservedPercent != 5 ||
		early.Source != smartQuotaEstimateSourceSupplierEarly || early.SampleCount != 1 {
		t.Fatalf("5%% supplier estimate = %#v ok=%v", early, ok)
	}
	// The pool-wide planner intentionally keeps its stricter evidence rule.
	if current, currentOK := service.smartQuotaCurrentEstimateForAt(now, identity); currentOK {
		t.Fatalf("5%% supplier estimate leaked into global capacity planning: %#v", current)
	}
	service.recordSmartUsageEvents([]usage.Event{{
		TimestampMS: now.Add(-30 * time.Second).UnixMilli(), Provider: "codex", AuthFileSnapshot: "supplier-fast.json",
		TotalTokens: 34_000_000, HeaderQuotaUsedPercent: floatPtr(50), HeaderQuotaRecoverAtMS: recoverAt,
		HeaderQuotaPlanType: "plus", ResponseMetadata: smartQuotaWeeklyMetadata("plus", 50, recoverAt),
	}}, now)
	if frozen, frozenOK := service.smartQuotaSupplierEstimateForAt(now, identity); !frozenOK || frozen.CapacityM != 120 {
		t.Fatalf("supplier estimate changed before exhaustion: %#v ok=%v", frozen, frozenOK)
	}

	service.recordSmartUsageEvents([]usage.Event{{
		TimestampMS: now.UnixMilli(), Provider: "codex", AuthFileSnapshot: "supplier-fast.json",
		TotalTokens: 50_000_000, HeaderQuotaUsedPercent: floatPtr(100), HeaderQuotaRecoverAtMS: recoverAt,
		HeaderQuotaPlanType: "plus", ResponseMetadata: smartQuotaWeeklyMetadata("plus", 100, recoverAt),
	}}, now)

	final, ok := service.smartQuotaSupplierEstimateForAt(now, identity)
	if !ok || final.CapacityM != 90 || final.Provisional || final.ObservedPercent != 100 ||
		final.Source != smartQuotaEstimateSourceSupplierFinal || final.CompleteWindowAccounts != 1 ||
		final.SampleCount != 1 {
		t.Fatalf("exhausted supplier estimate = %#v ok=%v", final, ok)
	}
	if len(service.smartQuotaState.supplierSamples) != 1 {
		t.Fatalf("supplier sample was duplicated instead of updated: %#v", service.smartQuotaState.supplierSamples)
	}
}

func TestSmartQuotaClassesSeparateStandardAndHighCapacityTeamAccounts(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	samples := []smartQuotaCalibrationSample{
		{identity: "file:standard-1.json", planType: "team", capacityM: 44, weight: 1, usedFraction: 0.20, observedMS: now.UnixMilli()},
		{identity: "file:standard-2.json", planType: "team", capacityM: 48, weight: 1, usedFraction: 0.30, observedMS: now.UnixMilli()},
		{identity: "file:standard-provisional.json", planType: "team", capacityM: 46, weight: 1, usedFraction: 0.05, observedMS: now.UnixMilli(), classificationOnly: true},
		{identity: "file:high-1.json", planType: "team", capacityM: 238, weight: 1, usedFraction: 0.25, observedMS: now.UnixMilli()},
		{identity: "file:high-provisional.json", planType: "team", capacityM: 244, weight: 1, usedFraction: 0.06, observedMS: now.UnixMilli(), classificationOnly: true},
	}

	classes := estimateSmartQuotaClassesAt(samples, now)
	if len(classes) != 2 {
		t.Fatalf("quota classes = %#v, want two distinct tiers", classes)
	}
	if classes[0].CenterM < 44 || classes[0].CenterM > 48 || classes[0].AccountCount != 3 ||
		classes[0].TrustedAccounts != 2 || classes[0].ProvisionalAccounts != 1 {
		t.Fatalf("standard quota class = %#v", classes[0])
	}
	if classes[1].CenterM < 238 || classes[1].CenterM > 244 || classes[1].AccountCount != 2 ||
		classes[1].TrustedAccounts != 1 || classes[1].ProvisionalAccounts != 1 {
		t.Fatalf("high quota class = %#v", classes[1])
	}
}

func TestSmartQuotaCalibrationDoesNotUseFirstMidWindowAbsoluteRatio(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	recoverAt := now.Add(7 * 24 * time.Hour).UnixMilli()
	service.recordSmartUsageEvents([]usage.Event{{
		TimestampMS: now.UnixMilli(), Provider: "codex", AuthFileSnapshot: "mid-window.json", TotalTokens: 1_000_000,
		HeaderQuotaUsedPercent: floatPtr(20), HeaderQuotaRecoverAtMS: recoverAt,
		HeaderQuotaPlanType: "team", ResponseMetadata: smartQuotaWeeklyMetadata("team", 20, recoverAt),
	}}, now)
	if len(service.smartQuotaState.samples) != 0 {
		t.Fatalf("first mid-window ratio created a partial-history estimate: %#v", service.smartQuotaState.samples)
	}
}

func TestSmartQuotaCalibrationKeepsOnlyThreeRecentDynamicSamplesPerAccount(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	service.recordSmartUsageEvents(smartQuotaCalibrationFixture("bounded.json", "team", now.Add(-time.Minute), 60), now)
	samples := service.smartQuotaState.samplesByIdentity["file:bounded.json"]
	if len(samples) != smartQuotaCalibrationSamplesPerAccount || len(service.smartQuotaState.samples) != smartQuotaCalibrationSamplesPerAccount {
		t.Fatalf("bounded samples = per-account %d / total %d, want %d", len(samples), len(service.smartQuotaState.samples), smartQuotaCalibrationSamplesPerAccount)
	}
}

func TestSmartResourceSumsTeamRemainingQuotaAgainstStableBaseline(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	used := []float64{20, 12, 11, 16, 9}
	results := make([]store.CodexInspectionResult, 0, len(used))
	for index, usedPercent := range used {
		value := usedPercent
		results = append(results, store.CodexInspectionResult{
			AccountKey: fmt.Sprintf("account-%d", index), FileName: fmt.Sprintf("account-%d.json", index),
			Provider: "codex", Status: "active", PlanType: "team", UsedPercent: &value,
		})
	}
	resource := service.buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{Product: "oauth_7d"}, inspectionQuotaSnapshot{
		run:     store.CodexInspectionRun{ProbeSetCount: len(results), SampledCount: len(results), FinishedAtMS: now.UnixMilli()},
		results: results, generatedAt: now,
	}, now)
	if resource.AccountQuotaEstimateM != 60 || resource.RawCapacityTokenM != 259.2 {
		t.Fatalf("team remaining capacity = estimate %.2fM / total %.2fM, want 60M baseline and 259.2M sum", resource.AccountQuotaEstimateM, resource.RawCapacityTokenM)
	}
}

func TestSmartResourceUsesPlanDefaultOnlyForAccountsWithoutIndependentData(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	samples := quotaSamplesForEstimate("file:measured.json", "team", 40, now.Add(-time.Minute), 3)
	service.smartQuotaState.samples = append(service.smartQuotaState.samples, samples...)
	service.smartQuotaState.samplesByIdentity["file:measured.json"] = append([]smartQuotaCalibrationSample(nil), samples...)
	unused := 0.0
	resource := service.buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{Product: "oauth_7d"}, inspectionQuotaSnapshot{
		run: store.CodexInspectionRun{ID: 1, ProbeSetCount: 2, SampledCount: 2, FinishedAtMS: now.UnixMilli()},
		results: []store.CodexInspectionResult{
			{FileName: "measured.json", Provider: "codex", Status: "active", PlanType: "team", UsedPercent: &unused},
			{FileName: "unseen.json", Provider: "codex", Status: "active", PlanType: "team", UsedPercent: &unused},
		},
		generatedAt: now,
	}, now)

	if resource.AccountQuotaEstimateM != 60 || resource.RawCapacityTokenM != 100 ||
		resource.QuotaEstimateOrderingBlocked || len(resource.AccountQuotaPlanEstimates) != 1 ||
		resource.AccountQuotaPlanEstimates[0].ValidationState != smartQuotaValidationInsufficient {
		t.Fatalf("mixed measured/default capacity = %#v", resource)
	}
	// measured.json contributes its independent 40M estimate; only unseen.json
	// receives the staged Team default of 60M.
	if got := service.smartQuotaEstimateForInspectionResult(
		store.CodexInspectionResult{FileName: "measured.json", PlanType: "team"},
		smartQuotaEstimate{CapacityM: 60, Source: smartQuotaEstimateSourceRecalibrated},
		now,
	); got.CapacityM != 40 || got.Source != smartQuotaEstimateSourceCurrent || got.CurrentEstimateM != 40 {
		t.Fatalf("measured account estimate = %#v, want independent 40M", got)
	}
}

func TestSmartResourceFixedQuotaPolicyOverridesIndependentAccountData(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	samples := quotaSamplesForEstimate("file:measured.json", "team", 40, now.Add(-time.Minute), 3)
	service.smartQuotaState.samples = append(service.smartQuotaState.samples, samples...)
	service.smartQuotaState.samplesByIdentity["file:measured.json"] = append([]smartQuotaCalibrationSample(nil), samples...)
	unused := 0.0
	resource := service.buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{
		Product: "oauth_7d",
		QuotaEstimationPolicies: map[string]store.ManagerSupplyQuotaEstimationPolicy{
			"team": {Mode: smartQuotaPolicyModeFixed, FallbackM: 60, FixedM: 50},
		},
	}, inspectionQuotaSnapshot{
		run: store.CodexInspectionRun{ID: 1, ProbeSetCount: 2, SampledCount: 2, FinishedAtMS: now.UnixMilli()},
		results: []store.CodexInspectionResult{
			{FileName: "measured.json", Provider: "codex", Status: "active", PlanType: "team", UsedPercent: &unused},
			{FileName: "unseen.json", Provider: "codex", Status: "active", PlanType: "team", UsedPercent: &unused},
		},
		generatedAt: now,
	}, now)

	if resource.AccountQuotaEstimateM != 50 || resource.RawCapacityTokenM != 100 ||
		resource.QuotaEstimateOrderingBlocked || len(resource.AccountQuotaPlanEstimates) != 1 ||
		resource.AccountQuotaPlanEstimates[0].Mode != smartQuotaPolicyModeFixed {
		t.Fatalf("fixed quota policy resource = %#v", resource)
	}
}

func TestSmartQuotaPlanAdoptionScalesInspectionRoundsAndMovesTenPercent(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	for index := 0; index < 3; index++ {
		samples := quotaSamplesForEstimate(fmt.Sprintf("file:source-%d.json", index), "team", 40, now.Add(-time.Minute), 3)
		service.smartQuotaState.samples = append(service.smartQuotaState.samples, samples...)
		service.smartQuotaState.samplesByIdentity[fmt.Sprintf("file:source-%d.json", index)] = append([]smartQuotaCalibrationSample(nil), samples...)
	}
	results := []store.CodexInspectionResult{{FileName: "source.json", Provider: "codex", Status: "active", PlanType: "team"}}

	first, firstPlanning := service.smartQuotaPlanEstimatesForInspection(store.ManagerSupplyConfig{}, results, 101, now)
	if len(first) != 1 || first[0].ObservedM != 40 || first[0].AdoptedM != 60 ||
		first[0].ConfirmationRounds != 1 || first[0].RequiredRounds != 3 ||
		first[0].ValidationState != smartQuotaValidationConfirming || !first[0].UsingFallback ||
		first[0].OrderingBlocked || firstPlanning["team"].CapacityM != 60 ||
		firstPlanning["team"].Source != smartQuotaEstimateSourceDefault || !firstPlanning["team"].FallbackOnly {
		t.Fatalf("first quota adoption = %#v", first)
	}
	repeated, _ := service.smartQuotaPlanEstimatesForInspection(store.ManagerSupplyConfig{}, results, 101, now)
	if repeated[0].ConfirmationRounds != 1 || repeated[0].AdoptedM != 60 {
		t.Fatalf("same inspection run advanced confirmation = %#v", repeated[0])
	}
	second, secondPlanning := service.smartQuotaPlanEstimatesForInspection(store.ManagerSupplyConfig{}, results, 102, now.Add(time.Minute))
	if second[0].ConfirmationRounds != 2 || second[0].AdoptedM != 60 || !second[0].UsingFallback ||
		second[0].OrderingBlocked || !secondPlanning["team"].FallbackOnly {
		t.Fatalf("second quota adoption = %#v", second[0])
	}
	third, _ := service.smartQuotaPlanEstimatesForInspection(store.ManagerSupplyConfig{}, results, 103, now.Add(2*time.Minute))
	if third[0].AdoptedM != 54 || third[0].ValidationState != smartQuotaValidationAccepted ||
		third[0].UsingFallback || third[0].OrderingBlocked {
		t.Fatalf("third quota adoption = %#v, want first bounded 10%% step after three confirmations", third[0])
	}
	fourth, _ := service.smartQuotaPlanEstimatesForInspection(store.ManagerSupplyConfig{}, results, 104, now.Add(3*time.Minute))
	if fourth[0].AdoptedM != 48.6 {
		t.Fatalf("fourth quota adoption = %#v, want another bounded 10%% step", fourth[0])
	}
}

func TestSmartQuotaPlanUpwardDivergenceWarnsWithoutBlockingOrdering(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	for index := 0; index < 3; index++ {
		samples := quotaSamplesForEstimate(fmt.Sprintf("file:source-%d.json", index), "team", 72, now.Add(-time.Minute), 3)
		service.smartQuotaState.samples = append(service.smartQuotaState.samples, samples...)
		service.smartQuotaState.samplesByIdentity[fmt.Sprintf("file:source-%d.json", index)] = append([]smartQuotaCalibrationSample(nil), samples...)
	}
	results := []store.CodexInspectionResult{{FileName: "source.json", Provider: "codex", Status: "active", PlanType: "team"}}

	first, _ := service.smartQuotaPlanEstimatesForInspection(store.ManagerSupplyConfig{}, results, 111, now)
	if len(first) != 1 || first[0].ObservedM != 72 || first[0].AdoptedM != 60 ||
		first[0].ConfirmationRounds != 1 || first[0].RequiredRounds != 2 ||
		first[0].ValidationState != smartQuotaValidationConfirming || !first[0].PendingConfirmation || first[0].OrderingBlocked {
		t.Fatalf("first upward quota adoption = %#v", first)
	}
	second, _ := service.smartQuotaPlanEstimatesForInspection(store.ManagerSupplyConfig{}, results, 112, now.Add(time.Minute))
	if second[0].ConfirmationRounds != 2 || second[0].AdoptedM != 66 ||
		second[0].ValidationState != smartQuotaValidationAccepted || !second[0].PendingConfirmation || second[0].OrderingBlocked {
		t.Fatalf("second upward quota adoption = %#v", second[0])
	}
	third, _ := service.smartQuotaPlanEstimatesForInspection(store.ManagerSupplyConfig{}, results, 113, now.Add(2*time.Minute))
	if third[0].AdoptedM != 72 || third[0].PendingConfirmation || third[0].OrderingBlocked {
		t.Fatalf("third upward quota adoption = %#v", third[0])
	}
}

func TestSmartQuotaPlanAdoptionRestartsConfirmationWhenCandidateShifts(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	results := []store.CodexInspectionResult{{FileName: "unseen.json", Provider: "codex", Status: "active", PlanType: "team"}}
	setSamples := func(capacityM float64, observedAt time.Time) {
		service.smartQuotaState.samples = nil
		service.smartQuotaState.samplesByIdentity = make(map[string][]smartQuotaCalibrationSample)
		for index := 0; index < 3; index++ {
			identity := fmt.Sprintf("file:source-%d.json", index)
			samples := quotaSamplesForEstimate(identity, "team", capacityM, observedAt, 3)
			service.smartQuotaState.samples = append(service.smartQuotaState.samples, samples...)
			service.smartQuotaState.samplesByIdentity[identity] = append([]smartQuotaCalibrationSample(nil), samples...)
		}
	}
	setSamples(40, now.Add(-time.Minute))
	first, _ := service.smartQuotaPlanEstimatesForInspection(store.ManagerSupplyConfig{}, results, 201, now)
	if first[0].ConfirmationRounds != 1 || !first[0].UsingFallback || first[0].OrderingBlocked {
		t.Fatalf("first candidate state = %#v", first[0])
	}

	setSamples(20, now)
	shifted, shiftedPlanning := service.smartQuotaPlanEstimatesForInspection(store.ManagerSupplyConfig{}, results, 202, now.Add(time.Minute))
	if shifted[0].ObservedM != 0 || shifted[0].RejectedAccounts != 3 || shifted[0].UniqueAccounts != 0 ||
		shifted[0].ConfirmationRounds != 0 || shifted[0].RequiredRounds != 5 ||
		shifted[0].ValidationState != smartQuotaValidationQuarantined || !shifted[0].UsingFallback ||
		shifted[0].OrderingBlocked || shifted[0].AdoptedM != 60 || !shiftedPlanning["team"].FallbackOnly {
		t.Fatalf("shifted candidate state = %#v", shifted[0])
	}
}

func TestSmartQuotaPlanRejectsExtremeLowSamplesBeforeMinimumAccountThreshold(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	for index := 0; index < 2; index++ {
		identity := fmt.Sprintf("file:source-%d.json", index)
		samples := quotaSamplesForEstimate(identity, "team", 0.81, now.Add(-time.Minute), 3)
		service.smartQuotaState.samples = append(service.smartQuotaState.samples, samples...)
		service.smartQuotaState.samplesByIdentity[identity] = append([]smartQuotaCalibrationSample(nil), samples...)
	}
	results := []store.CodexInspectionResult{{FileName: "unseen.json", Provider: "codex", Status: "active", PlanType: "team"}}

	items, planning := service.smartQuotaPlanEstimatesForInspection(store.ManagerSupplyConfig{}, results, 301, now)
	if len(items) != 1 || items[0].ObservedM != 0 || items[0].UniqueAccounts != 0 ||
		items[0].MinimumUniqueAccounts != 3 || items[0].AdoptedM != 60 ||
		items[0].RejectedAccounts != 2 || len(items[0].QuotaClasses) != 0 ||
		items[0].ValidationState != smartQuotaValidationQuarantined || !items[0].PendingConfirmation ||
		items[0].OrderingBlocked || planning["team"].CapacityM != 60 ||
		planning["team"].Source != smartQuotaEstimateSourceDefault || !planning["team"].FallbackOnly {
		t.Fatalf("insufficient plan estimate = items %#v planning %#v", items, planning["team"])
	}
}

func TestSmartQuotaPlanQuarantinesExtremeDownwardRuntimeEstimate(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	for index := 0; index < 3; index++ {
		identity := fmt.Sprintf("file:source-%d.json", index)
		samples := quotaSamplesForEstimate(identity, "team", 0.81, now.Add(-time.Minute), 3)
		service.smartQuotaState.samples = append(service.smartQuotaState.samples, samples...)
		service.smartQuotaState.samplesByIdentity[identity] = append([]smartQuotaCalibrationSample(nil), samples...)
	}
	results := []store.CodexInspectionResult{{FileName: "unseen.json", Provider: "codex", Status: "active", PlanType: "team"}}

	var items []SmartQuotaPlanEstimate
	var planning map[string]smartQuotaEstimate
	for runID := int64(401); runID <= 406; runID++ {
		items, planning = service.smartQuotaPlanEstimatesForInspection(
			store.ManagerSupplyConfig{},
			results,
			runID,
			now.Add(time.Duration(runID-401)*time.Minute),
		)
	}
	if len(items) != 1 || items[0].ObservedM != 0 || items[0].AdoptedM != 60 ||
		items[0].RequiredRounds != 5 || items[0].CompleteWindowAccounts != 0 ||
		items[0].UniqueAccounts != 0 || items[0].RejectedAccounts != 3 || len(items[0].QuotaClasses) != 0 ||
		items[0].ValidationState != smartQuotaValidationQuarantined ||
		!items[0].PendingConfirmation || !items[0].UsingFallback || items[0].OrderingBlocked ||
		planning["team"].CapacityM != 60 || planning["team"].Source != smartQuotaEstimateSourceDefault ||
		!planning["team"].FallbackOnly {
		t.Fatalf("extreme runtime estimate escaped quarantine = %#v", items)
	}
}

func TestSmartQuotaPlanRecalculatesStatisticsAfterRejectingExtremeLowCluster(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	for index := 0; index < 5; index++ {
		identity := fmt.Sprintf("file:low-%d.json", index)
		samples := quotaSamplesForEstimate(identity, "team", 1.91, now.Add(-time.Minute), 3)
		service.smartQuotaState.samples = append(service.smartQuotaState.samples, samples...)
		service.smartQuotaState.samplesByIdentity[identity] = append([]smartQuotaCalibrationSample(nil), samples...)
	}
	for index := 0; index < 2; index++ {
		identity := fmt.Sprintf("file:normal-%d.json", index)
		samples := quotaSamplesForEstimate(identity, "team", 31.6, now.Add(-time.Minute), 3)
		service.smartQuotaState.samples = append(service.smartQuotaState.samples, samples...)
		service.smartQuotaState.samplesByIdentity[identity] = append([]smartQuotaCalibrationSample(nil), samples...)
	}
	results := []store.CodexInspectionResult{{FileName: "unseen.json", Provider: "codex", Status: "active", PlanType: "team"}}

	items, planning := service.smartQuotaPlanEstimatesForInspection(store.ManagerSupplyConfig{}, results, 451, now)
	if len(items) != 1 || items[0].ObservedM != 31.6 || items[0].UniqueAccounts != 2 ||
		items[0].RejectedAccounts != 5 || len(items[0].QuotaClasses) != 1 ||
		items[0].QuotaClasses[0].CenterM != 31.6 || items[0].QuotaClasses[0].AccountCount != 2 ||
		items[0].ValidationState != smartQuotaValidationQuarantined || !items[0].UsingFallback ||
		items[0].AdoptedM != 60 || planning["team"].CapacityM != 60 ||
		planning["team"].Source != smartQuotaEstimateSourceDefault || !planning["team"].FallbackOnly {
		t.Fatalf("recalculated quota statistics = items %#v planning %#v", items, planning["team"])
	}
}

func TestSmartQuotaPlanRejectsWholeLowClassWithoutSplittingNormalClass(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	for index, capacityM := range []float64{1.3, 1.6, 1.91, 2.4, 11.3} {
		identity := fmt.Sprintf("file:low-range-%d.json", index)
		samples := quotaSamplesForEstimate(identity, "team", capacityM, now.Add(-time.Minute), 3)
		service.smartQuotaState.samples = append(service.smartQuotaState.samples, samples...)
		service.smartQuotaState.samplesByIdentity[identity] = append([]smartQuotaCalibrationSample(nil), samples...)
	}
	for index, capacityM := range []float64{21.5, 31.6} {
		identity := fmt.Sprintf("file:normal-range-%d.json", index)
		samples := quotaSamplesForEstimate(identity, "team", capacityM, now.Add(-time.Minute), 3)
		service.smartQuotaState.samples = append(service.smartQuotaState.samples, samples...)
		service.smartQuotaState.samplesByIdentity[identity] = append([]smartQuotaCalibrationSample(nil), samples...)
	}
	results := []store.CodexInspectionResult{{FileName: "unseen.json", Provider: "codex", Status: "active", PlanType: "team"}}

	items, planning := service.smartQuotaPlanEstimatesForInspection(store.ManagerSupplyConfig{}, results, 456, now)
	if len(items) != 1 || items[0].ObservedM <= 0 || items[0].UniqueAccounts != 2 || items[0].RejectedAccounts != 5 ||
		len(items[0].QuotaClasses) != 1 || items[0].QuotaClasses[0].AccountCount != 2 ||
		items[0].ObservedM != items[0].QuotaClasses[0].CenterM ||
		items[0].QuotaClasses[0].MinimumM != 21.5 || items[0].QuotaClasses[0].MaximumM != 31.6 ||
		items[0].ValidationState != smartQuotaValidationQuarantined || !items[0].UsingFallback ||
		items[0].AdoptedM != 60 || planning["team"].CapacityM != 60 ||
		planning["team"].Source != smartQuotaEstimateSourceDefault || !planning["team"].FallbackOnly {
		t.Fatalf("class-aware quota statistics = items %#v planning %#v", items, planning["team"])
	}
}

func TestSmartQuotaPlanQuotaClassesUseCurrentInspectionCohort(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	results := make([]store.CodexInspectionResult, 0, 7)
	for index, capacityM := range []float64{1.3, 1.6, 1.91, 2.4, 11.3, 21.5, 31.6} {
		fileName := fmt.Sprintf("current-%d.json", index)
		identity := "file:" + fileName
		samples := quotaSamplesForEstimate(identity, "team", capacityM, now.Add(-time.Minute), 3)
		service.smartQuotaState.samples = append(service.smartQuotaState.samples, samples...)
		service.smartQuotaState.samplesByIdentity[identity] = append([]smartQuotaCalibrationSample(nil), samples...)
		results = append(results, store.CodexInspectionResult{
			FileName: fileName,
			Provider: "codex",
			Status:   "active",
			PlanType: "team",
		})
	}
	for index := 0; index < 20; index++ {
		identity := fmt.Sprintf("file:historical-%d.json", index)
		samples := quotaSamplesForEstimate(identity, "team", 150, now.Add(-2*time.Minute), 3)
		service.smartQuotaState.samples = append(service.smartQuotaState.samples, samples...)
		service.smartQuotaState.samplesByIdentity[identity] = append([]smartQuotaCalibrationSample(nil), samples...)
	}

	items, planning := service.smartQuotaPlanEstimatesForInspection(store.ManagerSupplyConfig{}, results, 457, now)
	if len(items) != 1 || items[0].ObservedM <= 0 || items[0].UniqueAccounts != 2 ||
		items[0].RejectedAccounts != 5 || len(items[0].QuotaClasses) != 1 ||
		items[0].QuotaClasses[0].AccountCount != 2 || items[0].ObservedM != items[0].QuotaClasses[0].CenterM ||
		items[0].QuotaClasses[0].MinimumM != 21.5 || items[0].QuotaClasses[0].MaximumM != 31.6 ||
		items[0].QuotaClasses[0].SharePercent != 100 || items[0].AdoptedM != 60 ||
		planning["team"].CapacityM != 60 || planning["team"].Source != smartQuotaEstimateSourceDefault {
		t.Fatalf("current-cohort quota statistics = items %#v planning %#v", items, planning["team"])
	}
}

func TestSmartQuotaPlanRemovesCurrentLowClassWhileSamplesAreInsufficient(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	trustedIdentity := "file:trusted.json"
	trustedSamples := quotaSamplesForEstimate(trustedIdentity, "team", 78.15, now.Add(-time.Minute), 3)
	service.smartQuotaState.samples = append(service.smartQuotaState.samples, trustedSamples...)
	service.smartQuotaState.samplesByIdentity[trustedIdentity] = append([]smartQuotaCalibrationSample(nil), trustedSamples...)
	for _, sample := range []smartQuotaCalibrationSample{
		{identity: "file:low-provisional.json", planType: "team", capacityM: 19.29, weight: 0.05, usedFraction: 0.05, observedMS: now.UnixMilli(), classificationOnly: true},
		{identity: "file:normal-provisional.json", planType: "team", capacityM: 46.36, weight: 0.05, usedFraction: 0.05, observedMS: now.UnixMilli(), classificationOnly: true},
	} {
		service.smartQuotaState.samples = append(service.smartQuotaState.samples, sample)
		service.smartQuotaState.samplesByIdentity[sample.identity] = []smartQuotaCalibrationSample{sample}
	}
	results := []store.CodexInspectionResult{
		{FileName: "trusted.json", Provider: "codex", Status: "active", PlanType: "team"},
		{FileName: "low-provisional.json", Provider: "codex", Status: "active", PlanType: "team"},
		{FileName: "normal-provisional.json", Provider: "codex", Status: "active", PlanType: "team"},
	}

	items, planning := service.smartQuotaPlanEstimatesForInspection(store.ManagerSupplyConfig{}, results, 458, now)
	if len(items) != 1 || items[0].ObservedM != 78.15 || items[0].UniqueAccounts != 1 ||
		items[0].RejectedAccounts != 1 || items[0].ValidationState != smartQuotaValidationInsufficient ||
		len(items[0].QuotaClasses) != 2 || items[0].QuotaClasses[0].MinimumM != 46.36 ||
		items[0].QuotaClasses[1].MaximumM != 78.15 || items[0].AdoptedM != 60 ||
		planning["team"].CapacityM != 60 || planning["team"].Source != smartQuotaEstimateSourceDefault {
		t.Fatalf("insufficient corrected quota statistics = items %#v planning %#v", items, planning["team"])
	}
}

func TestSmartQuotaPlanRebuildsObservedValueFromTrustedNormalRepresentatives(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	activeIdentity := "file:active-low.json"
	activeSamples := quotaSamplesForEstimate(activeIdentity, "team", 12.14, now.Add(-time.Minute), 3)
	service.smartQuotaState.samples = append(service.smartQuotaState.samples, activeSamples...)
	service.smartQuotaState.samplesByIdentity[activeIdentity] = append([]smartQuotaCalibrationSample(nil), activeSamples...)

	// These are independent normal-range accounts retained from recent
	// inspections. Each has one trusted representative, so the stricter
	// multi-delta estimator has no aggregate yet even though the classification
	// statistic has enough independent accounts to be meaningful.
	for index, capacityM := range []float64{38, 40, 64, 150} {
		identity := fmt.Sprintf("file:normal-%d.json", index)
		sample := smartQuotaCalibrationSample{
			identity: identity, planType: "team", capacityM: capacityM,
			weight: 0.12, usedFraction: 0.40, observedMS: now.Add(-2 * time.Minute).UnixMilli(),
		}
		service.smartQuotaState.samples = append(service.smartQuotaState.samples, sample)
		service.smartQuotaState.samplesByIdentity[identity] = []smartQuotaCalibrationSample{sample}
	}

	items, planning := service.smartQuotaPlanEstimatesForInspection(
		store.ManagerSupplyConfig{},
		[]store.CodexInspectionResult{{FileName: "active-low.json", Provider: "codex", Status: "active", PlanType: "team"}},
		461,
		now,
	)
	if len(items) != 1 || items[0].ObservedM != 52 || items[0].UniqueAccounts != 4 ||
		items[0].RejectedAccounts != 1 || len(items[0].QuotaClasses) != 3 ||
		items[0].Source != smartQuotaEstimateSourceClassified ||
		items[0].ValidationState != smartQuotaValidationConfirming || !items[0].UsingFallback ||
		items[0].AdoptedM != 60 || planning["team"].CapacityM != 60 ||
		planning["team"].Source != smartQuotaEstimateSourceDefault || !planning["team"].FallbackOnly {
		t.Fatalf("trusted representative recalculation = items %#v planning %#v", items, planning["team"])
	}
}

func TestSmartQuotaPlanRestoresSupplierlessWarmHistoryForSolePlatform(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	activeIdentity := "file:active-low.json"
	activeSamples := quotaSamplesForEstimate(activeIdentity, "team", 12.14, now.Add(-time.Minute), 3)
	for index := range activeSamples {
		activeSamples[index].supplierID = "legacy"
	}
	service.smartQuotaState.samples = append(service.smartQuotaState.samples, activeSamples...)
	service.smartQuotaState.samplesByIdentity[activeIdentity] = append([]smartQuotaCalibrationSample(nil), activeSamples...)

	// WarmSmartUsage can rebuild historical quota samples after restart, but
	// raw usage rows do not carry the supply-platform ID. With exactly one
	// configured platform these samples still belong to that platform and must
	// remain available to correct a newly observed abnormal low current account.
	for index, capacityM := range []float64{38, 40, 64, 150} {
		identity := fmt.Sprintf("file:warm-normal-%d.json", index)
		sample := smartQuotaCalibrationSample{
			identity: identity, planType: "team", capacityM: capacityM,
			weight: 0.12, usedFraction: 0.40, observedMS: now.Add(-2 * time.Minute).UnixMilli(),
		}
		service.smartQuotaState.samples = append(service.smartQuotaState.samples, sample)
		service.smartQuotaState.samplesByIdentity[identity] = []smartQuotaCalibrationSample{sample}
	}

	cfg := store.ManagerSupplyConfig{
		BaseURL: "https://supplier.test", Username: "user", Password: "secret", Product: "oauth_30d",
	}
	items, planning := service.smartQuotaPlanEstimatesForInspection(
		cfg,
		[]store.CodexInspectionResult{{FileName: "active-low.json", Provider: "codex", Status: "active", PlanType: "team"}},
		471,
		now,
		map[string]string{"active-low.json": "legacy"},
	)
	var team SmartQuotaPlanEstimate
	for _, item := range items {
		if item.SupplierID == "legacy" && item.PlanType == "team" && item.AccountCount > 0 {
			team = item
			break
		}
	}
	planningTeam := planning[smartQuotaContextKey("legacy", "team")]
	if team.ObservedM != 52 || team.UniqueAccounts != 4 || team.RejectedAccounts != 1 ||
		team.Source != smartQuotaEstimateSourceClassified || len(team.QuotaClasses) != 3 ||
		team.ValidationState != smartQuotaValidationConfirming || !team.UsingFallback ||
		planningTeam.CapacityM != 60 || planningTeam.Source != smartQuotaEstimateSourceDefault ||
		!planningTeam.FallbackOnly {
		t.Fatalf("sole-platform warm history = team %#v planning %#v all %#v", team, planningTeam, items)
	}
}

func TestSmartQuotaPlanKeepsSupplierlessWarmHistoryIsolatedWithMultiplePlatforms(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	activeIdentity := "file:active-low.json"
	activeSamples := quotaSamplesForEstimate(activeIdentity, "team", 12.14, now.Add(-time.Minute), 3)
	for index := range activeSamples {
		activeSamples[index].supplierID = "supplier-a"
	}
	service.smartQuotaState.samples = append(service.smartQuotaState.samples, activeSamples...)
	service.smartQuotaState.samplesByIdentity[activeIdentity] = append([]smartQuotaCalibrationSample(nil), activeSamples...)

	warmIdentity := "file:warm-unassigned.json"
	warmSample := smartQuotaCalibrationSample{
		identity: warmIdentity, planType: "team", capacityM: 64,
		weight: 0.12, usedFraction: 0.40, observedMS: now.Add(-2 * time.Minute).UnixMilli(),
	}
	service.smartQuotaState.samples = append(service.smartQuotaState.samples, warmSample)
	service.smartQuotaState.samplesByIdentity[warmIdentity] = []smartQuotaCalibrationSample{warmSample}

	enabled := true
	cfg := store.ManagerSupplyConfig{Platforms: []store.ManagerSupplyPlatformConfig{
		{ID: "supplier-a", Type: "legacy", Enabled: &enabled, BaseURL: "https://a.test", Product: "oauth_30d"},
		{ID: "supplier-b", Type: "legacy", Enabled: &enabled, BaseURL: "https://b.test", Product: "oauth_30d"},
	}}
	items, _ := service.smartQuotaPlanEstimatesForInspection(
		cfg,
		[]store.CodexInspectionResult{{FileName: "active-low.json", Provider: "codex", Status: "active", PlanType: "team"}},
		472,
		now,
		map[string]string{"active-low.json": "supplier-a"},
	)
	var team SmartQuotaPlanEstimate
	for _, item := range items {
		if item.SupplierID == "supplier-a" && item.PlanType == "team" && item.AccountCount > 0 {
			team = item
			break
		}
	}
	if team.ObservedM != 0 || team.UniqueAccounts != 0 || team.RejectedAccounts != 1 ||
		len(team.QuotaClasses) != 0 || team.Source != smartQuotaEstimateSourceDefault {
		t.Fatalf("multi-platform supplierless history leaked into supplier-a: team %#v all %#v", team, items)
	}
}

func TestSmartQuotaPlanNeverAdoptsClassBelowPolicyFloor(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	for index := 0; index < 3; index++ {
		identity := fmt.Sprintf("file:source-%d.json", index)
		samples := quotaSamplesForEstimate(identity, "team", 20, now.Add(-time.Minute), 2)
		for sampleIndex := range samples {
			samples[sampleIndex].completeWindow = true
		}
		service.smartQuotaState.samples = append(service.smartQuotaState.samples, samples...)
		service.smartQuotaState.samplesByIdentity[identity] = append([]smartQuotaCalibrationSample(nil), samples...)
	}
	results := []store.CodexInspectionResult{{FileName: "unseen.json", Provider: "codex", Status: "active", PlanType: "team"}}

	var items []SmartQuotaPlanEstimate
	var planning map[string]smartQuotaEstimate
	for runID := int64(501); runID <= 505; runID++ {
		items, planning = service.smartQuotaPlanEstimatesForInspection(
			store.ManagerSupplyConfig{},
			results,
			runID,
			now.Add(time.Duration(runID-501)*time.Minute),
		)
	}
	if len(items) != 1 || items[0].ObservedM != 0 || items[0].CompleteWindowAccounts != 0 ||
		items[0].RejectedAccounts != 3 || items[0].RequiredRounds != 5 ||
		items[0].ConfirmationRounds != 0 || items[0].AdoptedM != 60 ||
		items[0].ValidationState != smartQuotaValidationQuarantined ||
		!items[0].PendingConfirmation || !items[0].UsingFallback || items[0].OrderingBlocked ||
		planning["team"].CapacityM != 60 || planning["team"].Source != smartQuotaEstimateSourceDefault ||
		!planning["team"].FallbackOnly {
		t.Fatalf("below-floor complete-window estimate = items %#v planning %#v", items, planning["team"])
	}
}

func TestSmartQuotaPlanWithoutCurrentAccountsKeepsFallbackQuietly(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	samples := quotaSamplesForEstimate("file:historical-free.json", "free", 2.5, now.Add(-time.Minute), 3)
	service.smartQuotaState.samples = append(service.smartQuotaState.samples, samples...)
	service.smartQuotaState.samplesByIdentity["file:historical-free.json"] = append([]smartQuotaCalibrationSample(nil), samples...)

	items, planning := service.smartQuotaPlanEstimatesForInspection(store.ManagerSupplyConfig{
		QuotaEstimationPolicies: map[string]store.ManagerSupplyQuotaEstimationPolicy{
			"free": {Mode: smartQuotaPolicyModeAuto, FallbackM: 10, FixedM: 10},
		},
	}, nil, 301, now)
	if len(items) != 1 || items[0].AccountCount != 0 || items[0].ObservedM != 0 ||
		items[0].AdoptedM != 10 || items[0].PendingConfirmation || items[0].OrderingBlocked ||
		items[0].Source != smartQuotaEstimateSourceDefault || items[0].SampleCount != 0 ||
		items[0].UniqueAccounts != 0 || items[0].CompleteWindowAccounts != 0 ||
		planning["free"].CapacityM != 10 || planning["free"].Source != smartQuotaEstimateSourceDefault {
		t.Fatalf("zero-account plan estimate = items %#v planning %#v", items, planning["free"])
	}
}

func TestSmartQuotaCalibrationPrefersWeeklySecondaryWindow(t *testing.T) {
	base := time.Now().Truncate(time.Second)
	primaryUsed := 80.0
	primaryMinutes := 300.0
	secondaryUsed := 95.0
	secondaryMinutes := 10_080.0
	secondaryReset := base.Add(7 * 24 * time.Hour).UnixMilli()
	evidence, ok := smartQuotaCalibrationEventEvidence(usage.Event{
		TimestampMS:            base.UnixMilli(),
		HeaderQuotaUsedPercent: floatPtr(primaryUsed),
		HeaderQuotaRecoverAtMS: base.Add(5 * time.Hour).UnixMilli(),
		ResponseMetadata: &usage.ResponseHeaderMetadata{Quota: &usage.HeaderQuotaMetadata{
			PlanType: "team",
			Primary: &usage.HeaderQuotaWindow{
				UsedPercent:   &primaryUsed,
				ResetAtMS:     base.Add(5 * time.Hour).UnixMilli(),
				WindowMinutes: &primaryMinutes,
			},
			Secondary: &usage.HeaderQuotaWindow{
				UsedPercent:   &secondaryUsed,
				ResetAtMS:     secondaryReset,
				WindowMinutes: &secondaryMinutes,
			},
		}},
	})
	if !ok || !evidence.concrete || evidence.fraction != 0.95 || evidence.recoverAtMS != secondaryReset || evidence.planType != "team" {
		t.Fatalf("weekly evidence = %#v/%v", evidence, ok)
	}
}

func TestSmartQuotaCalibrationKeepsHistoryWhenSummaryWindowSwitches(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	weeklyReset := now.Add(7 * 24 * time.Hour).UnixMilli()
	primaryMinutes := 300.0
	weeklyMinutes := 10_080.0
	primary80, primary10, primary20, primary30 := 80.0, 10.0, 20.0, 30.0
	weekly80, weekly85, weekly90, weekly95 := 80.0, 85.0, 90.0, 95.0
	events := []usage.Event{
		{
			TimestampMS:            now.Add(-90 * time.Second).UnixMilli(),
			Provider:               "codex",
			AuthFileSnapshot:       "switching.json",
			HeaderQuotaUsedPercent: &primary80,
			HeaderQuotaRecoverAtMS: now.Add(5 * time.Hour).UnixMilli(),
			TotalTokens:            54_000_000,
			ResponseMetadata: &usage.ResponseHeaderMetadata{Quota: &usage.HeaderQuotaMetadata{
				PlanType: "team",
				Primary: &usage.HeaderQuotaWindow{
					UsedPercent:   &primary80,
					ResetAtMS:     now.Add(5 * time.Hour).UnixMilli(),
					WindowMinutes: &primaryMinutes,
				},
				Secondary: &usage.HeaderQuotaWindow{
					UsedPercent:   &weekly80,
					ResetAtMS:     weeklyReset,
					WindowMinutes: &weeklyMinutes,
				},
			}},
		},
		{
			TimestampMS:            now.Add(-60 * time.Second).UnixMilli(),
			Provider:               "codex",
			AuthFileSnapshot:       "switching.json",
			HeaderQuotaUsedPercent: &primary10,
			HeaderQuotaRecoverAtMS: now.Add(10 * time.Hour).UnixMilli(),
			TotalTokens:            3_000_000,
			ResponseMetadata: &usage.ResponseHeaderMetadata{Quota: &usage.HeaderQuotaMetadata{
				PlanType: "team",
				Primary: &usage.HeaderQuotaWindow{
					UsedPercent:   &primary10,
					ResetAtMS:     now.Add(10 * time.Hour).UnixMilli(),
					WindowMinutes: &primaryMinutes,
				},
				Secondary: &usage.HeaderQuotaWindow{
					UsedPercent:   &weekly85,
					ResetAtMS:     weeklyReset,
					WindowMinutes: &weeklyMinutes,
				},
			}},
		},
		{
			TimestampMS:            now.Add(-30 * time.Second).UnixMilli(),
			Provider:               "codex",
			AuthFileSnapshot:       "switching.json",
			HeaderQuotaUsedPercent: &primary20,
			HeaderQuotaRecoverAtMS: now.Add(10 * time.Hour).UnixMilli(),
			TotalTokens:            3_000_000,
			ResponseMetadata: &usage.ResponseHeaderMetadata{Quota: &usage.HeaderQuotaMetadata{
				PlanType: "team",
				Primary: &usage.HeaderQuotaWindow{
					UsedPercent:   &primary20,
					ResetAtMS:     now.Add(10 * time.Hour).UnixMilli(),
					WindowMinutes: &primaryMinutes,
				},
				Secondary: &usage.HeaderQuotaWindow{
					UsedPercent:   &weekly90,
					ResetAtMS:     weeklyReset,
					WindowMinutes: &weeklyMinutes,
				},
			}},
		},
		{
			TimestampMS:            now.UnixMilli(),
			Provider:               "codex",
			AuthFileSnapshot:       "switching.json",
			HeaderQuotaUsedPercent: &primary30,
			HeaderQuotaRecoverAtMS: now.Add(10 * time.Hour).UnixMilli(),
			TotalTokens:            3_000_000,
			ResponseMetadata: &usage.ResponseHeaderMetadata{Quota: &usage.HeaderQuotaMetadata{
				PlanType: "team",
				Primary: &usage.HeaderQuotaWindow{
					UsedPercent:   &primary30,
					ResetAtMS:     now.Add(10 * time.Hour).UnixMilli(),
					WindowMinutes: &primaryMinutes,
				},
				Secondary: &usage.HeaderQuotaWindow{
					UsedPercent:   &weekly95,
					ResetAtMS:     weeklyReset,
					WindowMinutes: &weeklyMinutes,
				},
			}},
		},
	}
	service.recordSmartUsageEvents(events, now)
	estimate := service.smartQuotaEstimateForAt(now, "team", "file:switching.json")
	if estimate.CapacityM != 60 || estimate.SampleCount != 1 || estimate.EvidenceCount != 3 {
		t.Fatalf("switching-window estimate = %#v", estimate)
	}
	if observation := service.smartQuotaState.observations["file:switching.json"]; observation.windowTokens != 63_000_000 || observation.recoverAtMS != weeklyReset {
		t.Fatalf("switching-window observation = %#v", observation)
	}
}

func TestSmartQuotaCalibrationDoesNotTreatFlattenedSummaryAsOneWindow(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	service.recordSmartUsageEvents([]usage.Event{
		{
			TimestampMS:            now.Add(-time.Minute).UnixMilli(),
			Provider:               "codex",
			AuthFileSnapshot:       "summary-only.json",
			HeaderQuotaUsedPercent: floatPtr(80),
			HeaderQuotaRecoverAtMS: now.Add(5 * time.Hour).UnixMilli(),
			TotalTokens:            2_000_000,
		},
		{
			TimestampMS:            now.UnixMilli(),
			Provider:               "codex",
			AuthFileSnapshot:       "summary-only.json",
			HeaderQuotaUsedPercent: floatPtr(10),
			HeaderQuotaRecoverAtMS: now.Add(10 * time.Hour).UnixMilli(),
			TotalTokens:            2_000_000,
		},
	}, now)
	if len(service.smartQuotaState.samples) != 0 {
		t.Fatalf("flattened summary created samples: %#v", service.smartQuotaState.samples)
	}
}

func TestSmartResourceUsesRuntimeQuotaCalibration(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	results := make([]store.CodexInspectionResult, 0, 3)
	for index := 0; index < 3; index++ {
		accountKey := fmt.Sprintf("source-%d", index)
		service.recordSmartUsageEvents(
			smartQuotaCalibrationFixture(accountKey+".json", "team", now.Add(-10*time.Minute), 40),
			now,
		)
		unused := 0.0
		results = append(results, store.CodexInspectionResult{
			AccountKey:  accountKey,
			FileName:    accountKey + ".json",
			Provider:    "codex",
			Status:      "active",
			PlanType:    "team",
			UsedPercent: &unused,
		})
	}
	resource := service.buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{
		Product: "oauth_7d",
	}, inspectionQuotaSnapshot{
		run:         store.CodexInspectionRun{ProbeSetCount: 3, SampledCount: 3, FinishedAtMS: now.UnixMilli()},
		results:     results,
		generatedAt: now,
	}, now)

	if resource.AccountQuotaEstimateM != 60 || resource.AccountQuotaEstimateSource != smartQuotaEstimateSourceDefault ||
		resource.AccountQuotaCalibrationSamples != 3 || resource.RawCapacityTokenM != 180 ||
		resource.QuotaEstimateOrderingBlocked || len(resource.AccountQuotaPlanEstimates) == 0 ||
		resource.AccountQuotaPlanEstimates[0].ObservedM != 40 || resource.AccountQuotaPlanEstimates[0].AdoptedM != 60 ||
		!resource.AccountQuotaPlanEstimates[0].UsingFallback {
		t.Fatalf("calibrated resource = %#v", resource)
	}
	if reason := service.automaticSupplyGuardReason(resource); reason != "" {
		t.Fatalf("fallback quota calibration blocked automatic ordering: %q", reason)
	}
}

func TestSmartResourcePrioritizesSamplesFromCurrentPoolAccounts(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	service.smartQuotaState.samples = append(
		service.smartQuotaState.samples,
		quotaSamplesForEstimate("file:active.json", "team", 60, now.Add(-time.Minute), 6)...,
	)
	service.smartQuotaState.samples = append(
		service.smartQuotaState.samples,
		quotaSamplesForEstimate("file:old.json", "team", 10, now.Add(-8*time.Hour), 6)...,
	)
	unused := 0.0
	resource := service.buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{
		Product: "oauth_7d",
	}, inspectionQuotaSnapshot{
		run: store.CodexInspectionRun{ProbeSetCount: 1, SampledCount: 1, FinishedAtMS: now.UnixMilli()},
		results: []store.CodexInspectionResult{{
			AccountKey: "active", FileName: "active.json", Provider: "codex", Status: "active",
			PlanType: "team", UsedPercent: &unused,
		}},
		generatedAt: now,
	}, now)

	if resource.AccountQuotaEstimateSource != smartQuotaEstimateSourceDefault ||
		resource.AccountQuotaEstimateM != 60 || resource.AccountQuotaCurrentEstimateM != 0 ||
		resource.AccountQuotaHistoricalEstimateM != 0 || resource.AccountQuotaCalibrationUniqueAccounts != 0 ||
		resource.RawCapacityTokenM != 60 {
		t.Fatalf("current-pool calibrated resource = %#v", resource)
	}
}

func TestSmartTokenMetricsForecastsRunwayFromPlanningDemand(t *testing.T) {
	resource := SmartResource{
		UnitCapacityRCU:                40,
		CurrentCapacityRCU:             smartTokenMillionToRCU(40, 40),
		DemandPlanningRCUPerMinute:     smartTokenMillionToRCU(7.3, 40),
		AccountQuotaEstimateM:          40,
		AccountQuotaEstimateSource:     smartQuotaEstimateSourcePlan,
		AccountQuotaCalibrationSamples: 100,
	}
	applySmartTokenMetrics(&resource)
	if resource.CurrentCapacityTokenM != 40 || resource.DemandPlanningTokenMPerMinute != 7.3 || resource.ForecastSustainMinutes != 5.5 {
		t.Fatalf("forecast metrics = %#v", resource)
	}
}
