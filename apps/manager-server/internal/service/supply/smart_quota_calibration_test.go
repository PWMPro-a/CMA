package supply

import (
	"fmt"
	"testing"
	"time"

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
	if identity.Source != smartQuotaEstimateSourceCurrent || identity.CapacityM != 40 || identity.SampleCount != 10 {
		t.Fatalf("identity estimate = %#v", identity)
	}
	plan := service.smartQuotaEstimateFor("team")
	if plan.Source != smartQuotaEstimateSourceRecentPlan || plan.CapacityM != 40 || plan.SampleCount != 30 || plan.ObservedPercent != 285 {
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
			TimestampMS:            now.Add(-time.Minute).UnixMilli(),
			Provider:               "codex",
			AuthFileSnapshot:       "active.json",
			HeaderQuotaUsedPercent: floatPtr(90),
			HeaderQuotaRecoverAtMS: recoverAt,
			HeaderQuotaPlanType:    "team",
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
	if estimate.CapacityM != 60.42 || estimate.Source != smartQuotaEstimateSourceCurrent || estimate.SampleCount != 2 {
		t.Fatalf("window-history estimate = %#v", estimate)
	}
	observation := service.smartQuotaState.observations["file:active.json"]
	if observation.windowTokens != 57_400_000 || observation.lastFraction != 0.95 {
		t.Fatalf("window observation = %#v", observation)
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
	primary80, primary10 := 80.0, 10.0
	weekly90, weekly95 := 90.0, 95.0
	events := []usage.Event{
		{
			TimestampMS:            now.Add(-time.Minute).UnixMilli(),
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
					UsedPercent:   &weekly95,
					ResetAtMS:     weeklyReset,
					WindowMinutes: &weeklyMinutes,
				},
			}},
		},
	}
	service.recordSmartUsageEvents(events, now)
	estimate := service.smartQuotaEstimateForAt(now, "team", "file:switching.json")
	if estimate.CapacityM != 60 || estimate.SampleCount != 2 {
		t.Fatalf("switching-window estimate = %#v", estimate)
	}
	if observation := service.smartQuotaState.observations["file:switching.json"]; observation.windowTokens != 57_000_000 || observation.recoverAtMS != weeklyReset {
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
	for index := 0; index < 3; index++ {
		service.recordSmartUsageEvents(
			smartQuotaCalibrationFixture(fmt.Sprintf("source-%d.json", index), "team", now.Add(-10*time.Minute), 40),
			now,
		)
	}
	unused := 0.0
	resource := service.buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{
		Product: "oauth_7d",
	}, inspectionQuotaSnapshot{
		run: store.CodexInspectionRun{ProbeSetCount: 1, SampledCount: 1, FinishedAtMS: now.UnixMilli()},
		results: []store.CodexInspectionResult{{
			AccountKey: "unseen", FileName: "unseen.json", Provider: "codex", Status: "active",
			PlanType: "team", UsedPercent: &unused,
		}},
		generatedAt: now,
	}, now)

	if resource.AccountQuotaEstimateM != 40 || resource.AccountQuotaEstimateSource != smartQuotaEstimateSourceRecentPlan ||
		resource.AccountQuotaCalibrationSamples != 30 || resource.RawCapacityTokenM != 40 {
		t.Fatalf("calibrated resource = %#v", resource)
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

	if resource.AccountQuotaEstimateSource != smartQuotaEstimateSourceRecalibrated ||
		resource.AccountQuotaEstimateM != 57.5 || resource.AccountQuotaCurrentEstimateM != 60 ||
		resource.AccountQuotaHistoricalEstimateM != 10 || resource.AccountQuotaCalibrationUniqueAccounts != 1 ||
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
