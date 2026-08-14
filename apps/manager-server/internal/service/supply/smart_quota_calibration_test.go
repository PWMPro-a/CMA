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
	if identity.Source != smartQuotaEstimateSourceIdentity || identity.CapacityM != 40 || identity.SampleCount != 10 {
		t.Fatalf("identity estimate = %#v", identity)
	}
	plan := service.smartQuotaEstimateFor("team")
	if plan.Source != smartQuotaEstimateSourcePlan || plan.CapacityM != 40 || plan.SampleCount != 30 || plan.ObservedPercent != 300 {
		t.Fatalf("plan estimate = %#v", plan)
	}
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

	if resource.AccountQuotaEstimateM != 40 || resource.AccountQuotaEstimateSource != smartQuotaEstimateSourcePlan ||
		resource.AccountQuotaCalibrationSamples != 30 || resource.RawCapacityTokenM != 40 {
		t.Fatalf("calibrated resource = %#v", resource)
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
