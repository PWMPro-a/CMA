package supply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/config"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpaauthfiles"
	managerconfigsvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/usage"
)

func intPtr(value int) *int {
	return &value
}

func floatPtr(value float64) *float64 {
	return &value
}

func seedCompletedQuotaInspection(t *testing.T, st *store.Store, results ...store.CodexInspectionResult) {
	t.Helper()
	now := time.Now().UnixMilli()
	run, err := st.CreateCodexInspectionRun(context.Background(), store.CodexInspectionRun{
		TriggerType:   "scheduled",
		TriggerKey:    fmt.Sprintf("supply-%d", now),
		Status:        "completed",
		ProbeSetCount: len(results),
		SampledCount:  len(results),
		FinishedAtMS:  now,
	})
	if err != nil {
		t.Fatalf("create quota inspection run: %v", err)
	}
	for index := range results {
		result := results[index]
		result.RunID = run.ID
		if result.AccountKey == "" {
			result.AccountKey = fmt.Sprintf("quota-%d", index)
		}
		if result.FileName == "" {
			result.FileName = result.AccountKey + ".json"
		}
		if result.DisplayAccount == "" {
			result.DisplayAccount = result.AccountKey
		}
		if result.Provider == "" {
			result.Provider = "codex"
		}
		if result.Action == "" {
			result.Action = "keep"
		}
		if _, err := st.InsertCodexInspectionResult(context.Background(), result); err != nil {
			t.Fatalf("insert quota inspection result: %v", err)
		}
	}
}

func quotaInspectionResult(usedPercent float64) store.CodexInspectionResult {
	return store.CodexInspectionResult{
		UsedPercent: &usedPercent,
		QuotaWindows: []model.CodexInspectionQuotaWindow{{
			ID:          "five-hour",
			UsedPercent: &usedPercent,
		}},
	}
}

func float64Ptr(value float64) *float64 {
	return &value
}

func quotaWindow(id string, usedPercent, limitWindowSeconds float64) model.CodexInspectionQuotaWindow {
	return model.CodexInspectionQuotaWindow{
		ID:                 id,
		UsedPercent:        float64Ptr(usedPercent),
		LimitWindowSeconds: float64Ptr(limitWindowSeconds),
	}
}

func liveQuotaUsageEvent(fileName string, at time.Time, primaryUsed, secondaryUsed float64) usage.Event {
	primaryMinutes := float64(5 * 60)
	secondaryMinutes := float64(7 * 24 * 60)
	return usage.Event{
		TimestampMS:      at.UnixMilli(),
		Provider:         "codex",
		AuthFileSnapshot: fileName,
		ResponseMetadata: &usage.ResponseHeaderMetadata{Quota: &usage.HeaderQuotaMetadata{
			PlanType: "team",
			Primary: &usage.HeaderQuotaWindow{
				UsedPercent:   float64Ptr(primaryUsed),
				WindowMinutes: float64Ptr(primaryMinutes),
			},
			Secondary: &usage.HeaderQuotaWindow{
				UsedPercent:   float64Ptr(secondaryUsed),
				WindowMinutes: float64Ptr(secondaryMinutes),
			},
		}},
	}
}

func TestSmartLiveQuotaDeltaUpdatesOnlyChangedAccount(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Millisecond)
	baselineAt := now.Add(-2 * time.Minute)
	service.recordSmartUsageEvents([]usage.Event{
		liveQuotaUsageEvent("changed.json", now, 20, 40),
	}, now)
	select {
	case <-service.AutomaticWake():
	default:
		t.Fatal("account-scoped quota change did not wake automatic supply")
	}

	snapshot := inspectionQuotaSnapshot{
		generatedAt: baselineAt,
		results: []store.CodexInspectionResult{
			{
				FileName:  "changed.json",
				AuthIndex: "changed",
				QuotaWindows: []model.CodexInspectionQuotaWindow{
					quotaWindow("five-hour", 10, smartQuotaFiveHourSeconds),
					quotaWindow("weekly", 20, smartQuotaWeekSeconds),
				},
			},
			{
				FileName:  "stable.json",
				AuthIndex: "stable",
				QuotaWindows: []model.CodexInspectionQuotaWindow{
					quotaWindow("five-hour", 30, smartQuotaFiveHourSeconds),
					quotaWindow("weekly", 40, smartQuotaWeekSeconds),
				},
			},
		},
	}
	overlaid, delta := service.applySmartLiveQuotaDelta(snapshot, now)
	if delta.accounts != 1 || delta.updatedAtMS != now.UnixMilli() {
		t.Fatalf("live quota delta = %#v, want one changed account", delta)
	}
	changedRemaining, ok := inspectionResultRemainingQuotaFraction(overlaid.results[0])
	if !ok || math.Abs(changedRemaining-0.8) > 0.0001 {
		t.Fatalf("changed account remaining = %v/%v, want 0.8/true", changedRemaining, ok)
	}
	stableRemaining, ok := inspectionResultRemainingQuotaFraction(overlaid.results[1])
	if !ok || math.Abs(stableRemaining-0.7) > 0.0001 {
		t.Fatalf("stable account remaining = %v/%v, want 0.7/true", stableRemaining, ok)
	}
	baselineRemaining, ok := inspectionResultRemainingQuotaFraction(snapshot.results[0])
	if !ok || math.Abs(baselineRemaining-0.9) > 0.0001 {
		t.Fatalf("cached baseline was mutated: remaining = %v/%v", baselineRemaining, ok)
	}
}

func TestSmartLiveQuotaDeltaIgnoresEvidenceOlderThanCompletedSnapshot(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Millisecond)
	service.recordSmartUsageEvents([]usage.Event{
		liveQuotaUsageEvent("account.json", now.Add(-time.Minute), 90, 90),
	}, now)
	snapshot := inspectionQuotaSnapshot{
		generatedAt: now,
		results: []store.CodexInspectionResult{{
			FileName: "account.json",
			QuotaWindows: []model.CodexInspectionQuotaWindow{
				quotaWindow("five-hour", 10, smartQuotaFiveHourSeconds),
			},
		}},
	}
	overlaid, delta := service.applySmartLiveQuotaDelta(snapshot, now)
	remaining, ok := inspectionResultRemainingQuotaFraction(overlaid.results[0])
	if delta.accounts != 0 || !ok || math.Abs(remaining-0.9) > 0.0001 {
		t.Fatalf("older quota evidence overlaid completed snapshot: delta=%#v remaining=%v/%v", delta, remaining, ok)
	}
}

func TestSmartLiveQuotaDeltaCoalescesSubPercentMovement(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Millisecond)
	service.recordSmartUsageEvents([]usage.Event{
		liveQuotaUsageEvent("account.json", now, 20, 40),
	}, now)
	<-service.AutomaticWake()
	service.recordSmartUsageEvents([]usage.Event{
		liveQuotaUsageEvent("account.json", now.Add(time.Second), 20.1, 40.1),
	}, now.Add(time.Second))
	select {
	case <-service.AutomaticWake():
		t.Fatal("sub-0.5% quota movement caused an unnecessary automatic wake")
	default:
	}
}

func TestInspectionResultQuotaFractionUsesShortestWindow(t *testing.T) {
	result := store.CodexInspectionResult{
		UsedPercent: float64Ptr(0),
		QuotaWindows: []model.CodexInspectionQuotaWindow{
			quotaWindow("five-hour", 95, smartQuotaFiveHourSeconds),
			quotaWindow("weekly", 1, smartQuotaWeekSeconds),
			quotaWindow("monthly", 0, 30*24*60*60),
		},
	}
	remaining, ok := inspectionResultRemainingQuotaFraction(result)
	if !ok || remaining < 0.049 || remaining > 0.051 {
		t.Fatalf("remaining=%v ok=%v, want 0.05/true", remaining, ok)
	}
}

func TestSmartResourceSeparatesNormallyAvailableFromQuotaRisk(t *testing.T) {
	service := New(nil, nil)
	now := time.Now()
	resource := service.buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{
		Product: "oauth_7d",
	}, inspectionQuotaSnapshot{
		run: store.CodexInspectionRun{ProbeSetCount: 2, SampledCount: 2, FinishedAtMS: now.UnixMilli()},
		results: []store.CodexInspectionResult{
			func() store.CodexInspectionResult {
				result := quotaInspectionResult(79)
				result.Provider = "codex"
				return result
			}(),
			func() store.CodexInspectionResult {
				result := quotaInspectionResult(81)
				result.Provider = "codex"
				return result
			}(),
		},
		generatedAt: now,
	}, now)
	if resource.SchedulableAccounts != 2 || resource.HealthyAccounts != 2 ||
		resource.NormalAccounts != 1 || resource.AtRiskAccounts != 1 {
		t.Fatalf("normal/risk account split = %#v", resource)
	}
}

func TestInspectionResultQuotaFractionUsesWeeklyWhenNoShorterWindowExists(t *testing.T) {
	result := store.CodexInspectionResult{
		QuotaWindows: []model.CodexInspectionQuotaWindow{
			quotaWindow("weekly", 58, smartQuotaWeekSeconds),
			quotaWindow("monthly", 0, 30*24*60*60),
		},
	}
	remaining, ok := inspectionResultRemainingQuotaFraction(result)
	if !ok || remaining < 0.419 || remaining > 0.421 {
		t.Fatalf("remaining=%v ok=%v, want 0.42/true", remaining, ok)
	}
}

func TestInspectionResultQuotaFractionUsesMonthlyWhenNoShorterWindowExists(t *testing.T) {
	result := store.CodexInspectionResult{
		UsedPercent: float64Ptr(0),
		QuotaWindows: []model.CodexInspectionQuotaWindow{
			quotaWindow("monthly", 0, 0),
		},
	}
	if remaining, ok := inspectionResultRemainingQuotaFraction(result); !ok || remaining != 1 {
		t.Fatalf("monthly-only result must be the fallback: remaining=%v ok=%v", remaining, ok)
	}
}

func TestSmartResourceUsesMonthlyQuotaFallbackForCurrentCapacity(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	used := 20.0
	resource := New(nil, nil).buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{
		Product: "oauth_30d",
	}, inspectionQuotaSnapshot{
		run: store.CodexInspectionRun{ProbeSetCount: 1, SampledCount: 1, FinishedAtMS: now.UnixMilli()},
		results: []store.CodexInspectionResult{{
			AccountKey: "monthly-only", FileName: "monthly-only.json", Provider: "codex", Status: "active",
			QuotaWindows: []model.CodexInspectionQuotaWindow{quotaWindow("monthly", used, 30*24*60*60)},
		}},
		generatedAt: now,
	}, now)

	if !resource.SnapshotFresh || resource.CapacityCoverage != 100 || resource.AvailableAccounts != 1 ||
		resource.CurrentCapacityRCU <= 0 || resource.RawCapacityRCU <= 0 {
		t.Fatalf("monthly fallback must restore usable current capacity: %#v", resource)
	}
}

func TestInspectionResultQuotaFractionRetainsLegacyAggregateFallback(t *testing.T) {
	usedPercent := 63.0
	remaining, ok := inspectionResultRemainingQuotaFraction(store.CodexInspectionResult{UsedPercent: &usedPercent})
	if !ok || remaining < 0.369 || remaining > 0.371 {
		t.Fatalf("remaining=%v ok=%v, want 0.37/true", remaining, ok)
	}
}

func TestInspectionResultQuotaFractionUsesLowestRemainingAtSameShortestWindow(t *testing.T) {
	result := store.CodexInspectionResult{
		QuotaWindows: []model.CodexInspectionQuotaWindow{
			quotaWindow("five-hour", 20, smartQuotaFiveHourSeconds),
			quotaWindow("additional-five-hour", 80, smartQuotaFiveHourSeconds),
			quotaWindow("weekly", 99, smartQuotaWeekSeconds),
		},
	}
	remaining, ok := inspectionResultRemainingQuotaFraction(result)
	if !ok || remaining < 0.199 || remaining > 0.201 {
		t.Fatalf("remaining=%v ok=%v, want 0.2/true", remaining, ok)
	}
}

func TestSmartResourceRecommendsPrelockFromUsageCapacity(t *testing.T) {
	service := New(nil, nil)
	now := time.Now()
	events := make([]usage.Event, 0, 120)
	for minute := 0; minute < 30; minute++ {
		for index := 0; index < 4; index++ {
			events = append(events, usage.Event{
				TimestampMS: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(),
				Provider:    "codex",
				AuthIndex:   "account-a",
				TotalTokens: 100,
			})
		}
	}
	service.recordSmartUsageEvents(events, now)

	resource := service.buildSmartResourceFromSnapshots(store.ManagerSupplyConfig{
		Product:                     "oauth_30d",
		TargetAvailableAccounts:     2,
		DefaultEmergencyMinAccounts: 1,
		HealthyMinutesTarget:        120,
		WarningMinutes:              60,
		CriticalMinutes:             30,
		PrelockMinQuantity:          1,
		PrelockMaxQuantity:          10,
		NewAccountConfidence:        0.7,
	}, authFileSnapshot{
		generatedAt: now,
		files: []cpaauthfiles.File{
			{Name: "a.json", Provider: "codex", Raw: map[string]any{"remaining_rcu": 80}},
			{Name: "b.json", Provider: "codex", Raw: map[string]any{"remaining_rcu": 80}},
		},
	}, now)

	if resource.HealthLevel != smartHealthWarning || resource.EmergencyShortage || resource.SuggestedQuantity < 1 {
		t.Fatalf("resource = %#v", resource)
	}
	if resource.RPM30M <= 0 || resource.CurrentCapacityRCU <= 0 || resource.CapacityGapRCU <= 0 {
		t.Fatalf("resource metrics were not computed: %#v", resource)
	}
}

func TestSmartResourceBlocksIncompleteInspectionQuotaEvidence(t *testing.T) {
	service := New(nil, nil)
	now := time.Now()
	resource := service.buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{
		Product:              "oauth_30d",
		HealthyMinutesTarget: 40,
	}, inspectionQuotaSnapshot{
		run: store.CodexInspectionRun{
			ProbeSetCount: 2,
			SampledCount:  2,
			FinishedAtMS:  now.UnixMilli(),
		},
		generatedAt: now,
		results: []store.CodexInspectionResult{
			quotaInspectionResult(0),
			{AccountKey: "missing-quota", FileName: "missing-quota.json", Provider: "codex", Action: "keep"},
		},
	}, now)

	if resource.SnapshotFresh || resource.DecisionReason != "inspection_quota_incomplete" || resource.SuggestedQuantity != 0 {
		t.Fatalf("incomplete inspection must pause instead of deriving capacity from account count: %#v", resource)
	}
}

func TestSmartResourceShowsVerifiedCapacityWhenInspectionQuotaEvidenceIsPartial(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	for minute := 0; minute < 10; minute++ {
		service.recordSmartUsageEvents([]usage.Event{{
			TimestampMS: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(),
			Provider:    "codex",
			AuthIndex:   "verified.json",
			TotalTokens: 100,
		}}, now)
	}
	unused := 0.0
	resource := service.buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{
		Product:              "oauth_7d",
		HealthyMinutesTarget: 40,
	}, inspectionQuotaSnapshot{
		run: store.CodexInspectionRun{ProbeSetCount: 2, SampledCount: 2, FinishedAtMS: now.UnixMilli()},
		results: []store.CodexInspectionResult{
			{AccountKey: "verified", FileName: "verified.json", Provider: "codex", UsedPercent: &unused},
			{AccountKey: "missing", FileName: "missing.json", Provider: "codex"},
		},
		generatedAt: now,
	}, now)

	if resource.SnapshotFresh || resource.DecisionReason != "inspection_quota_incomplete" || resource.SuggestedQuantity != 0 {
		t.Fatalf("incomplete inspection must still pause automation: %#v", resource)
	}
	if resource.CurrentCapacityRCU <= 0 || resource.ConsumeRCUPerMinute <= 0 ||
		resource.TargetCapacityRCU <= 0 || resource.EstimatedSustainMinutes <= 0 {
		t.Fatalf("verified capacity must remain visible during an incomplete inspection: %#v", resource)
	}
}

func TestSmartResourceIgnoresClearlyUnavailableCredentialsForCoverage(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	unused := 0.0
	resource := service.buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{
		Product:              "oauth_7d",
		HealthyMinutesTarget: 40,
	}, inspectionQuotaSnapshot{
		run:         store.CodexInspectionRun{ProbeSetCount: 4, SampledCount: 4, FinishedAtMS: now.UnixMilli()},
		generatedAt: now,
		results: []store.CodexInspectionResult{
			{AccountKey: "verified", FileName: "verified.json", Provider: "codex", Status: "active", Action: "keep", StatusCode: intPtr(200), UsedPercent: &unused},
			{AccountKey: "revoked", FileName: "revoked.json", Provider: "codex", Status: "active", Action: "reauth", StatusCode: intPtr(401), ErrorKind: "http_status"},
			{AccountKey: "deactivated", FileName: "deactivated.json", Provider: "codex", Status: "disabled", Action: "delete", StatusCode: intPtr(402), ErrorDetail: `{"detail":{"code":"deactivated_workspace"}}`, Disabled: true},
			{AccountKey: "quota-full", FileName: "quota-full.json", Provider: "codex", Status: "active", Action: "disable", StatusCode: intPtr(200), IsQuota: true, UsedPercent: floatPtr(100)},
		},
	}, now)

	if !resource.SnapshotFresh || resource.DecisionReason == "inspection_quota_incomplete" || resource.CapacityCoverage != 100 {
		t.Fatalf("unavailable credentials must not make the usable quota snapshot incomplete: %#v", resource)
	}
	if resource.TotalAccounts != 4 || resource.AvailableAccounts != 1 || resource.HealthyAccounts != 1 || resource.DisabledAccounts != 3 {
		t.Fatalf("unavailable credentials should remain visible as disabled without reducing coverage: %#v", resource)
	}
}

func TestSmartResourceExcludesInspectionErrorUntilUsabilityIsVerified(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	for minute := 0; minute < 10; minute++ {
		service.recordSmartUsageEvents([]usage.Event{{
			TimestampMS: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(),
			Provider:    "codex",
			AuthIndex:   "verified.json",
			TotalTokens: 100,
		}}, now)
	}
	unused := 0.0
	resource := service.buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{
		Product:              "oauth_7d",
		HealthyMinutesTarget: 40,
	}, inspectionQuotaSnapshot{
		run: store.CodexInspectionRun{ProbeSetCount: 2, SampledCount: 2, FinishedAtMS: now.UnixMilli()},
		results: []store.CodexInspectionResult{
			{AccountKey: "verified", FileName: "verified.json", Provider: "codex", Status: "active", UsedPercent: &unused},
			{AccountKey: "probe-error", FileName: "probe-error.json", Provider: "codex", Status: "error", UsedPercent: &unused},
		},
		generatedAt: now,
	}, now)

	if resource.SnapshotFresh || resource.DecisionReason != "inspection_usability_incomplete" || resource.SuggestedQuantity != 0 {
		t.Fatalf("an inspection error must pause automation until availability is verified: %#v", resource)
	}
	if resource.TotalAccounts != 2 || resource.AvailableAccounts != 1 || resource.HealthyAccounts != 1 ||
		resource.DisabledAccounts != 1 || resource.WeakAccounts != 0 || resource.RawCapacityRCU != 250 ||
		resource.RawCapacityTokenM != 10 {
		t.Fatalf("only the successfully verified credential may contribute capacity: %#v", resource)
	}
}

func TestSmartResourceUsesVerifiedLowerBoundDuringIncompleteInspection(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	for minute := 0; minute < 30; minute++ {
		service.recordSmartUsageEvents([]usage.Event{{
			TimestampMS: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(),
			Provider:    "codex",
			AuthIndex:   "verified.json",
			TotalTokens: 80_000_000,
		}}, now)
	}
	unused := 0.0
	resource := service.buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{
		Product:              "oauth_7d",
		HealthyMinutesTarget: 40,
		WarningMinutes:       25,
		CriticalMinutes:      15,
		ReplenishBatchSize:   15,
		PrelockMinQuantity:   1,
		PrelockMaxQuantity:   7,
		NewAccountConfidence: 0.7,
	}, inspectionQuotaSnapshot{
		run:         store.CodexInspectionRun{ProbeSetCount: 2, SampledCount: 2, FinishedAtMS: now.UnixMilli()},
		generatedAt: now,
		results: []store.CodexInspectionResult{
			{AccountKey: "verified", FileName: "verified.json", Provider: "codex", Status: "active", UsedPercent: &unused},
			{AccountKey: "probe-error", FileName: "probe-error.json", Provider: "codex", Status: "error", UsedPercent: &unused},
		},
	}, now)

	if resource.SnapshotFresh || resource.DecisionReason != "inspection_usability_incomplete_capacity_deficit" ||
		resource.HealthLevel != smartHealthCritical || resource.SuggestedQuantity != 3 {
		t.Fatalf("low verified capacity must retain a capacity recovery plan: %#v", resource)
	}
	if !smartPartialInspectionCapacityDeficitAllowed(resource) || resource.CurrentCapacityRCU <= 0 ||
		resource.EstimatedSustainMinutes > float64(resource.CriticalMinutes) {
		t.Fatalf("only the verified lower bound may enable replenishment: %#v", resource)
	}
}

func TestSmartResourceRetainsWarningPlanForRecentPartialInspectionCapacityDeficit(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	// 8.9M tokens/minute on oauth_7d is 222.5 RCU/minute. Twenty-two
	// verified credentials have 220M tokens, so the lower bound supports
	// about 24.7 minutes: below the 25-minute warning water and below the
	// 40-minute health target. This is the regression case where the UI previously
	// showed capacity unknown and a suggested quantity of zero.
	for minute := 0; minute < 30; minute++ {
		service.recordSmartUsageEvents([]usage.Event{{
			TimestampMS: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(),
			Provider:    "codex",
			AuthIndex:   "verified.json",
			TotalTokens: 8_900_000,
		}}, now)
	}
	unused := 0.0
	results := make([]store.CodexInspectionResult, 0, 23)
	for index := 0; index < 22; index++ {
		results = append(results, store.CodexInspectionResult{
			AccountKey:  fmt.Sprintf("verified-%d", index),
			FileName:    fmt.Sprintf("verified-%d.json", index),
			Provider:    "codex",
			Status:      "active",
			UsedPercent: &unused,
		})
	}
	results = append(results, store.CodexInspectionResult{
		AccountKey: "probe-error", FileName: "probe-error.json", Provider: "codex", Status: "error", UsedPercent: &unused,
	})
	resource := service.buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{
		Product:              "oauth_7d",
		HealthyMinutesTarget: 40,
		WarningMinutes:       25,
		CriticalMinutes:      10,
		ReplenishBatchSize:   15,
		PrelockMinQuantity:   1,
		PrelockMaxQuantity:   7,
		NewAccountConfidence: 0.7,
	}, inspectionQuotaSnapshot{
		run:         store.CodexInspectionRun{ProbeSetCount: len(results), SampledCount: len(results), FinishedAtMS: now.Add(-21 * time.Minute).UnixMilli()},
		generatedAt: now.Add(-21 * time.Minute),
		results:     results,
	}, now)

	if resource.SnapshotFresh || resource.HealthLevel != smartHealthWarning ||
		resource.DecisionReason != "inspection_usability_incomplete_capacity_deficit" || resource.SuggestedQuantity != 3 {
		t.Fatalf("recent partial inspection must retain warning capacity plan: %#v", resource)
	}
	if resource.CurrentCapacityTokenM != 220 || resource.EstimatedSustainMinutes >= float64(resource.WarningMinutes) ||
		resource.EstimatedSustainMinutes >= float64(resource.EffectiveHealthyMinutes) || !smartPartialInspectionCapacityDeficitAllowed(resource) {
		t.Fatalf("plan must use only the recent verified capacity lower bound: %#v", resource)
	}
}

func TestSmartResourceIncludesRecentImportedCapacityUntilNextInspection(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	for minute := 0; minute < 30; minute++ {
		service.recordSmartUsageEvents([]usage.Event{{
			TimestampMS: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(),
			Provider:    "codex",
			AuthIndex:   "inspected.json",
			TotalTokens: 80_000_000,
		}}, now)
	}
	unused := 0.0
	cfg := store.ManagerSupplyConfig{
		Product:              "oauth_7d",
		HealthyMinutesTarget: 40,
		WarningMinutes:       25,
		CriticalMinutes:      15,
		NewAccountConfidence: 0.7,
	}
	base := inspectionQuotaSnapshot{
		run:         store.CodexInspectionRun{ID: 88, ProbeSetCount: 1, SampledCount: 1, StartedAtMS: now.Add(-2 * time.Minute).UnixMilli(), FinishedAtMS: now.UnixMilli()},
		generatedAt: now,
		results: []store.CodexInspectionResult{{
			AccountKey: "inspected", FileName: "inspected.json", Provider: "codex", Status: "active", UsedPercent: &unused,
		}},
	}
	withoutRecentImport := service.buildSmartResourceFromInspectionSnapshot(cfg, base, now)
	base.activeImportItems = []store.SupplyImportItem{{
		OrderID:          "new-order",
		FileName:         "newly-imported.json",
		Status:           "imported",
		ImportedAtMS:     now.Add(-time.Minute).UnixMilli(),
		LeaseExpiresAtMS: now.Add(55 * time.Minute).UnixMilli(),
	}}
	withRecentImport := service.buildSmartResourceFromInspectionSnapshot(cfg, base, now)
	if withRecentImport.PendingInspectionAccounts != 1 || withRecentImport.PendingInspectionCapacityRCU <= 0 {
		t.Fatalf("recent import must be exposed as provisional capacity: %#v", withRecentImport)
	}
	if withRecentImport.CurrentCapacityRCU <= withoutRecentImport.CurrentCapacityRCU ||
		withRecentImport.EstimatedSustainMinutes <= withoutRecentImport.EstimatedSustainMinutes {
		t.Fatalf("recent imported capacity must affect the capacity plan: before=%#v after=%#v", withoutRecentImport, withRecentImport)
	}
	base.activeImportItems = append(base.activeImportItems, store.SupplyImportItem{
		OrderID:          "replacement-order",
		FileName:         "newly-imported.json",
		Status:           "imported",
		ImportedAtMS:     now.Add(-30 * time.Second).UnixMilli(),
		LeaseExpiresAtMS: now.Add(55 * time.Minute).UnixMilli(),
	})
	withoutImportDuplicate := service.buildSmartResourceFromInspectionSnapshot(cfg, base, now)
	if withoutImportDuplicate.PendingInspectionAccounts != 1 ||
		withoutImportDuplicate.CurrentCapacityRCU != withRecentImport.CurrentCapacityRCU {
		t.Fatalf("duplicate import rows for one CPA file must count once: %#v", withoutImportDuplicate)
	}
	base.activeImportItems = base.activeImportItems[:1]
	base.activeImportItems[0].FileName = "inspected.json"
	withoutDuplicate := service.buildSmartResourceFromInspectionSnapshot(cfg, base, now)
	if withoutDuplicate.PendingInspectionAccounts != 0 {
		t.Fatalf("an account already present in the completed inspection must not be double counted: %#v", withoutDuplicate)
	}
}

func TestLoadLatestInspectionQuotaSnapshotKeepsExpiredLeaseAsRoutingMetadata(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "lease-snapshot.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now().Truncate(time.Second)
	usedPercent := 0.0
	seedCompletedQuotaInspection(t, st, store.CodexInspectionResult{
		AccountKey: "expired", FileName: "expired.json", Provider: "codex", Action: "keep", UsedPercent: &usedPercent,
	})
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "lease-evidence", Product: "oauth_30d", RequestedQuantity: 2, Status: "importing",
	}); err != nil {
		t.Fatalf("create supply order: %v", err)
	}
	if _, err := st.InsertSupplyImportItems(context.Background(), "lease-evidence", []store.SupplyImportItem{
		{ItemKey: "expired", FileName: "expired.json", PayloadJSON: `{}`, LeaseExpiresAtMS: now.Add(-time.Minute).UnixMilli()},
		{ItemKey: "active", FileName: "active.json", PayloadJSON: `{}`, LeaseExpiresAtMS: now.Add(30 * time.Minute).UnixMilli()},
	}); err != nil {
		t.Fatalf("insert supply items: %v", err)
	}
	pending, err := st.ListPendingSupplyImportItems(context.Background(), "lease-evidence", time.Now().UnixMilli(), 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending supply items=%#v err=%v", pending, err)
	}
	for _, item := range pending {
		if err := st.MarkSupplyImportItemImported(context.Background(), item.ID, time.Now().Add(time.Second).UnixMilli()); err != nil {
			t.Fatalf("mark imported: %v", err)
		}
	}

	service := New(st, nil)
	snapshot, err := service.loadLatestInspectionQuotaSnapshot(context.Background())
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if snapshot.leaseExpiresByFile["expired.json"] <= 0 || snapshot.leaseExpiresByFile["active.json"] <= now.UnixMilli() {
		t.Fatalf("snapshot did not retain current lease evidence: %#v", snapshot.leaseExpiresByFile)
	}
	if len(snapshot.activeImportItems) != 2 {
		t.Fatalf("current overlay must retain both imports: %#v", snapshot.activeImportItems)
	}
	resource := service.buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{Product: "oauth_30d"}, snapshot, now)
	if resource.AvailableAccounts != 2 || resource.HealthyAccounts != 1 || resource.PendingInspectionAccounts != 1 || resource.RawCapacityRCU <= 0 {
		t.Fatalf("expired timestamp must not remove a healthy inspected account: %#v", resource)
	}
}

func TestLoadLatestInspectionQuotaSnapshotUsesSinglePlatformForUnattributedSamples(t *testing.T) {
	tests := []struct {
		name         string
		platforms    []store.ManagerSupplyPlatformConfig
		wantSupplier string
	}{
		{
			name: "single platform",
			platforms: []store.ManagerSupplyPlatformConfig{
				{ID: "sogouedu", Type: "legacy"},
			},
			wantSupplier: "sogouedu",
		},
		{
			name: "multiple platforms",
			platforms: []store.ManagerSupplyPlatformConfig{
				{ID: "supplier-a", Type: "legacy"},
				{ID: "supplier-b", Type: "legacy"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "supplier-snapshot.sqlite"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })
			now := time.Now().Truncate(time.Second)
			usedPercent := 20.0
			weeklySeconds := float64(smartQuotaWeekSeconds)
			seedCompletedQuotaInspection(t, st, store.CodexInspectionResult{
				AccountKey: "manual-account",
				FileName:   "manual-account.json",
				Provider:   "codex",
				Status:     "active",
				Action:     "keep",
				PlanType:   "team",
				QuotaWindows: []model.CodexInspectionQuotaWindow{{
					ID:                 "weekly",
					UsedPercent:        &usedPercent,
					ResetAtMS:          now.Add(6 * 24 * time.Hour).UnixMilli(),
					LimitWindowSeconds: &weeklySeconds,
				}},
			})

			service := New(st, nil)
			snapshot, err := service.loadLatestInspectionQuotaSnapshot(context.Background(), store.ManagerSupplyConfig{
				Platforms: tt.platforms,
			})
			if err != nil {
				t.Fatalf("load snapshot: %v", err)
			}
			if got := snapshot.supplierByFile["manual-account.json"]; got != tt.wantSupplier {
				t.Fatalf("supplier fallback = %q, want %q", got, tt.wantSupplier)
			}
			if len(snapshot.quotaWindowUsage) != 1 || snapshot.quotaWindowUsage[0].supplierID != tt.wantSupplier {
				t.Fatalf("quota sample supplier = %#v, want %q", snapshot.quotaWindowUsage, tt.wantSupplier)
			}
		})
	}
}

func TestLoadLatestInspectionQuotaSnapshotAcceptsTrustedEmptySupplySnapshot(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "empty-supply-snapshot.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now()
	settings := model.DefaultCodexInspectionConfig()
	settings.TargetTypes = []string{model.CodexInspectionTargetCodex}
	settings.TargetType = model.CodexInspectionTargetCodex
	settings.AutoActionMode = model.CodexInspectionAutoActionNone
	settings.AutoRecoverEnabled = false
	run, err := st.CreateCodexInspectionRun(context.Background(), store.CodexInspectionRun{
		TriggerType:   model.CodexInspectionTriggerSupplySnapshot,
		TriggerKey:    "supply-empty-pool",
		Status:        model.CodexInspectionStatusCompleted,
		StartedAtMS:   now.Add(-time.Second).UnixMilli(),
		FinishedAtMS:  now.UnixMilli(),
		ProbeSetCount: 0,
		SampledCount:  0,
		Settings:      settings,
	})
	if err != nil {
		t.Fatalf("create empty supply snapshot: %v", err)
	}

	service := New(st, nil)
	snapshot, err := service.loadLatestInspectionQuotaSnapshot(context.Background())
	if err != nil {
		t.Fatalf("load empty supply snapshot: %v", err)
	}
	if snapshot.run.ID != run.ID || len(snapshot.results) != 0 || !smartInspectionSnapshotComplete(snapshot) {
		t.Fatalf("empty supply snapshot was not accepted as complete: %#v", snapshot)
	}
	resource := service.buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{
		Product: "oauth_7d",
	}, snapshot, now)
	if !resource.SnapshotFresh || resource.AvailableAccounts != 0 || resource.CurrentCapacityRCU != 0 ||
		resource.DecisionReason == "inspection_snapshot_incomplete" {
		t.Fatalf("empty supply snapshot must produce a trusted zero-capacity baseline: %#v", resource)
	}
}

func TestLoadLatestInspectionQuotaSnapshotSkipsInterruptedRefreshBurst(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "interrupted-refresh-burst.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seedCompletedQuotaInspection(t, st, quotaInspectionResult(25))
	completed, err := st.ListCodexInspectionRuns(context.Background(), 1)
	if err != nil || len(completed) != 1 {
		t.Fatalf("load completed baseline: runs=%#v err=%v", completed, err)
	}
	settings := model.DefaultCodexInspectionConfig()
	settings.TargetTypes = []string{model.CodexInspectionTargetCodex}
	settings.TargetType = model.CodexInspectionTargetCodex
	for index := 0; index < 25; index++ {
		now := time.Now().Add(time.Duration(index+1) * time.Millisecond)
		if _, err := st.CreateCodexInspectionRun(context.Background(), store.CodexInspectionRun{
			TriggerType:  model.CodexInspectionTriggerSupplySnapshot,
			TriggerKey:   fmt.Sprintf("interrupted-%d", index),
			Status:       model.CodexInspectionStatusInterrupted,
			StartedAtMS:  now.UnixMilli(),
			FinishedAtMS: now.UnixMilli(),
			Error:        "lease lost",
			Settings:     settings,
		}); err != nil {
			t.Fatalf("create interrupted run %d: %v", index, err)
		}
	}

	snapshot, err := New(st, nil).loadLatestInspectionQuotaSnapshot(context.Background())
	if err != nil {
		t.Fatalf("load baseline behind interrupted burst: %v", err)
	}
	if snapshot.run.ID != completed[0].ID || len(snapshot.results) != 1 {
		t.Fatalf("recovered snapshot = run %d results %d, want run %d", snapshot.run.ID, len(snapshot.results), completed[0].ID)
	}
}

func TestLoadLatestInspectionQuotaSnapshotSkipsCalibrationForStaleRecoveredRun(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "stale-recovered-calibration.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	result := quotaInspectionResult(25)
	result.QuotaWindows = []model.CodexInspectionQuotaWindow{quotaWindow("weekly", 25, smartQuotaWeekSeconds)}
	seedCompletedQuotaInspection(t, st, result)
	runs, err := st.ListCodexInspectionRuns(context.Background(), 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("load completed run: runs=%#v err=%v", runs, err)
	}
	staleRun := runs[0]
	staleRun.FinishedAtMS = time.Now().Add(-smartInspectionSnapshotFreshTTL - time.Minute).UnixMilli()
	if err := st.UpdateCodexInspectionRun(context.Background(), staleRun); err != nil {
		t.Fatalf("age completed run: %v", err)
	}

	snapshot, err := New(st, nil).loadLatestInspectionQuotaSnapshot(context.Background())
	if err != nil {
		t.Fatalf("load stale recovered snapshot: %v", err)
	}
	if snapshot.run.ID != staleRun.ID || len(snapshot.results) != 1 {
		t.Fatalf("recovered snapshot = %#v", snapshot)
	}
	if len(snapshot.quotaWindowUsage) != 0 {
		t.Fatalf("stale recovered snapshot retained optional quota calibration: %#v", snapshot.quotaWindowUsage)
	}
}

func TestEmptyNonSupplyInspectionDoesNotBecomeCapacityBaseline(t *testing.T) {
	settings := model.DefaultCodexInspectionConfig()
	snapshot := inspectionQuotaSnapshot{
		run: store.CodexInspectionRun{
			TriggerType:   model.CodexInspectionTriggerScheduled,
			Status:        model.CodexInspectionStatusCompleted,
			ProbeSetCount: 0,
			SampledCount:  0,
			Settings:      settings,
		},
		generatedAt: time.Now(),
	}
	if smartInspectionSnapshotComplete(snapshot) {
		t.Fatal("an unrelated empty inspection must not be trusted as a supply capacity baseline")
	}
}

func TestSmartQuotaLowWaterRefillsToHealthyAndKeepsShortCooldown(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		ReplenishBatchSize:    15,
		PrelockMinQuantity:    1,
		PrelockMaxQuantity:    7,
		CreateCooldownSeconds: 120,
	}
	critical := SmartResource{
		HealthLevel:             smartHealthCritical,
		WarningMinutes:          25,
		CriticalMinutes:         15,
		EffectiveHealthyMinutes: 40,
		EstimatedSustainMinutes: 8,
		ConsumeRCUPerMinute:     2_000,
		CapacityGapRCU:          61_250,
		UnitCapacityRCU:         40,
		SuggestedQuantity:       20,
	}
	critical.SnapshotFresh = true
	if got := smartAutomaticOrderQuantityLimit(cfg, critical); got != 7 {
		t.Fatalf("critical quota batch limit=%d, want 7", got)
	}
	if got := New(nil, nil).smartSuggestedCreateQuantity(cfg, critical); got != 7 {
		t.Fatalf("critical suggested quantity=%d, want 7", got)
	}
	if got, reason := smartPrelockQuantityForSupplyPressure(cfg, critical, smartSupplyPressure{level: smartSupplyPressurePlenty}, 20); got != 7 || reason != "emergency_refill_to_healthy" {
		t.Fatalf("critical pressure adjustment=%d/%q, want 7/healthy", got, reason)
	}
	if got := smartCreateCooldownForResource(cfg, critical); got != 60 {
		t.Fatalf("critical cooldown=%d, want 60", got)
	}
	if got := smartAutomaticCheckIntervalSeconds(cfg, critical); got != 60 {
		t.Fatalf("critical worker interval=%d, want 60", got)
	}

	warning := critical
	warning.SnapshotFresh = false
	warning.HealthLevel = smartHealthWarning
	warning.EstimatedSustainMinutes = 20
	warning.CapacityGapRCU = 0
	warning.EffectiveHealthyMinutes = 0
	if got := smartCreateCooldownForResource(cfg, warning); got != 45 {
		t.Fatalf("warning cooldown=%d, want 45", got)
	}
	if got := smartAutomaticCheckIntervalSeconds(cfg, warning); got != 45 {
		t.Fatalf("warning worker interval=%d, want 45", got)
	}
	healthy := warning
	healthy.EstimatedSustainMinutes = 30
	healthy.CapacityGapRCU = 1
	if got := smartAutomaticOrderQuantityLimit(cfg, healthy); got != 7 {
		t.Fatalf("ordinary verified capacity-deficit batch limit=%d, want 7", got)
	}
	if got := smartCreateCooldownForResource(cfg, healthy); got != 120 {
		t.Fatalf("healthy cooldown=%d, want 120", got)
	}
	hardEmergency := critical
	hardEmergency.EmergencyReason = "critical_available_accounts"
	if got := smartAutomaticOrderQuantityLimit(cfg, hardEmergency); got != 7 {
		t.Fatalf("account-count emergency batch limit=%d, want 7", got)
	}
	if got, reason := smartPrelockQuantityForSupplyPressure(cfg, hardEmergency, smartSupplyPressure{level: smartSupplyPressurePlenty}, 10); got != 7 || reason != "critical_available_accounts" {
		t.Fatalf("account-count emergency pressure adjustment=%d/%q, want 7/full", got, reason)
	}
}

func TestSuccessfulOrderCooldownFollowsSupplyStrategyAndHealthLevel(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		CreateCooldownSeconds: 120,
		Strategy:              managerconfigsvc.SupplyStrategyStrongSupply,
	}
	buffered := SmartResource{
		HealthLevel:             smartHealthWarning,
		WarningMinutes:          20,
		CriticalMinutes:         15,
		EffectiveHealthyMinutes: 30,
		EstimatedSustainMinutes: 25,
		ConsumeRCUPerMinute:     100,
		CapacityGapRCU:          500,
	}
	if got := smartSuccessfulOrderCooldownForResource(cfg, buffered); got != 300 {
		t.Fatalf("strong-supply buffered cooldown=%d, want 300", got)
	}
	cfg.Strategy = managerconfigsvc.SupplyStrategyBalanced
	if got := smartSuccessfulOrderCooldownForResource(cfg, buffered); got != 600 {
		t.Fatalf("balanced buffered cooldown=%d, want 600", got)
	}
	cfg.Strategy = managerconfigsvc.SupplyStrategyCostFirst
	if got := smartSuccessfulOrderCooldownForResource(cfg, buffered); got != 900 {
		t.Fatalf("cost-first buffered cooldown=%d, want 900", got)
	}

	warning := buffered
	warning.EstimatedSustainMinutes = 20
	cfg.Strategy = managerconfigsvc.SupplyStrategyStrongSupply
	if got := smartSuccessfulOrderCooldownForResource(cfg, warning); got != 120 {
		t.Fatalf("strong-supply warning cooldown=%d, want 120", got)
	}
	cfg.Strategy = managerconfigsvc.SupplyStrategyBalanced
	if got := smartSuccessfulOrderCooldownForResource(cfg, warning); got != 180 {
		t.Fatalf("balanced warning cooldown=%d, want 180", got)
	}
	cfg.Strategy = managerconfigsvc.SupplyStrategyCostFirst
	if got := smartSuccessfulOrderCooldownForResource(cfg, warning); got != 300 {
		t.Fatalf("cost-first warning cooldown=%d, want 300", got)
	}

	emergency := warning
	emergency.EmergencyShortage = true
	cfg.Strategy = managerconfigsvc.SupplyStrategyStrongSupply
	if got := smartSuccessfulOrderCooldownForResource(cfg, emergency); got != 60 {
		t.Fatalf("emergency cooldown=%d, want 60", got)
	}
}

func TestSuccessfulOrderCooldownScalesWithDeliveredCapacity(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		CreateCooldownSeconds: 120,
		Strategy:              managerconfigsvc.SupplyStrategyStrongSupply,
	}
	resource := SmartResource{
		HealthLevel:                    smartHealthHealthy,
		CriticalMinutes:                15,
		EstimatedSustainMinutes:        55,
		ConsumeRCUPerMinute:            100,
		DemandPlanningRCUPerMinute:     100,
		TokenCapacityMode:              smartTokenCapacityMode,
		EstimatedNewAccountCapacityRCU: 1_000,
	}
	if got := smartSuccessfulOrderCooldownForDelivery(cfg, resource, 1); got != 300 {
		t.Fatalf("one-account observation cooldown=%d, want 300", got)
	}
	if got := smartSuccessfulOrderCooldownForDelivery(cfg, resource, 2); got != 600 {
		t.Fatalf("two-account observation cooldown=%d, want 600", got)
	}
	resource.EmergencyShortage = true
	if got := smartSuccessfulOrderCooldownForDelivery(cfg, resource, 1); got != 60 {
		t.Fatalf("emergency successful-delivery cooldown=%d, want configured emergency cadence 60", got)
	}
	cfg.CheckIntervalSeconds = 30
	if got := smartSuccessfulOrderCooldownForDelivery(cfg, resource, 1); got != 30 {
		t.Fatalf("thirty-second emergency successful-delivery cooldown=%d, want 30", got)
	}
}

func TestCustomSupplyEnablesConfiguredVerifiedHealthyFloor(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Strategy:                 managerconfigsvc.SupplyStrategyCustom,
		TargetAvailableAccounts:  100,
		HealthyAvailableAccounts: 100,
	}
	if got := smartHealthyAvailableAccounts(cfg); got != 100 || !smartHealthyFloorShortageEnabled(cfg) {
		t.Fatalf("custom supply healthy floor = %d/enabled=%v, want 100/true", got, smartHealthyFloorShortageEnabled(cfg))
	}
	cfg.Strategy = managerconfigsvc.SupplyStrategyStrongSupply
	if smartHealthyFloorShortageEnabled(cfg) {
		t.Fatal("preset strategy unexpectedly enabled full healthy-floor replenishment")
	}
}

func TestEmergencyCapacityGapRefillsToHealthyWaterline(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Product:              "oauth_30d",
		Strategy:             managerconfigsvc.SupplyStrategyStrongSupply,
		HealthyMinutesTarget: 60,
		WarningMinutes:       30,
		CriticalMinutes:      20,
		ReplenishBatchSize:   50,
		PrelockMinQuantity:   1,
		PrelockMaxQuantity:   20,
		NewAccountConfidence: 0.7,
	}
	resource := defaultSmartResource(cfg)
	resource.SnapshotFresh = true
	resource.CurrentCapacityRCU = 800
	resource.ConsumeRCUPerMinute = 40
	resource.DemandTrend = smartDemandTrendStable

	recalculateSmartResourceCapacityPlan(cfg, &resource)

	if !resource.EmergencyShortage || resource.SuggestedQuantity != 19 {
		t.Fatalf("emergency healthy-water refill = %#v, want 19 accounts", resource)
	}
	if resource.CapacityGapRCU != 1_600 || resource.CapacityGapTokenM != 128 {
		t.Fatalf("capacity gap = %.2f RCU / %.2fM, want 1600 RCU / 128M", resource.CapacityGapRCU, resource.CapacityGapTokenM)
	}
	if got := New(nil, nil).smartSuggestedCreateQuantity(cfg, resource); got != 19 {
		t.Fatalf("create quantity = %d, want 19", got)
	}
	if got, reason := smartPrelockQuantityForSupplyPressure(cfg, resource, smartSupplyPressure{level: smartSupplyPressurePlenty}, 19); got != 19 || reason != "emergency_refill_to_healthy" {
		t.Fatalf("pressure adjustment = %d/%q, want 19/healthy", got, reason)
	}
	if resource.ProjectedSustainAfterRefillMin < 60 {
		t.Fatalf("projected runway = %.1f minutes, want healthy waterline", resource.ProjectedSustainAfterRefillMin)
	}
}

func TestShortageRetryUsesFullBatchAfterDefiniteZeroDeliveryFailure(t *testing.T) {
	now := time.Now()
	cfg := store.ManagerSupplyConfig{Product: "oauth_30d"}
	resource := SmartResource{
		EmergencyShortage:       true,
		EffectiveHealthyMinutes: 55,
		EstimatedSustainMinutes: 18,
		ConsumeRCUPerMinute:     1_000,
		CapacityGapRCU:          40_000,
	}
	cancelled := store.SupplyOrder{
		OrderID: "cancelled-purchase", Product: cfg.Product, Automatic: true,
		Status: "cancelled", RemoteStatus: "cancelled", CompletedAtMS: now.Add(-20 * time.Second).UnixMilli(),
	}
	plan := smartEmergencyRetryPlanForOrder(cfg, resource, cancelled, now)
	if !plan.active || plan.quantityLimit != 10 || plan.reason != "emergency_retry_after_cancelled" || plan.cooldown != 10*time.Second {
		t.Fatalf("cancelled retry plan = %#v", plan)
	}

	retried := cancelled
	retried.Status = "failed"
	retried.TriggerReason = "emergency_retry_after_cancelled"
	retried.LastError = "inventory unavailable"
	plan = smartEmergencyRetryPlanForOrder(cfg, resource, retried, now)
	if plan.active {
		t.Fatalf("failed order must wait for ordinary reconciliation instead of fast retry: %#v", plan)
	}

	warning := resource
	warning.EmergencyShortage = false
	warning.SnapshotFresh = true
	warning.HealthLevel = smartHealthWarning
	warning.DemandTrend = smartDemandTrendStable
	plan = smartEmergencyRetryPlanForOrder(cfg, warning, cancelled, now)
	if !plan.active || plan.quantityLimit != 10 || plan.cooldown != 10*time.Second {
		t.Fatalf("warning shortage retry plan = %#v", plan)
	}

	blocked := []store.SupplyOrder{
		{OrderID: "uncertain", Product: cfg.Product, Automatic: true, Status: "create_uncertain", CreatedAtMS: now.UnixMilli()},
		{OrderID: "paid", Product: cfg.Product, Automatic: true, Status: "cancelled", ChargedFen: 100, CompletedAtMS: now.UnixMilli()},
		{OrderID: "delivered", Product: cfg.Product, Automatic: true, Status: "failed", ImportedCount: 1, CompletedAtMS: now.UnixMilli()},
		{OrderID: "auth-error", Product: cfg.Product, Automatic: true, Status: "failed", LastError: "supply API returned HTTP 401", CompletedAtMS: now.UnixMilli()},
		{OrderID: "recovery-1", Product: cfg.Product, Automatic: true, Strategy: "recovery", Status: "failed", CompletedAtMS: now.UnixMilli()},
	}
	for _, order := range blocked {
		if got := smartEmergencyRetryPlanForOrder(cfg, resource, order, now); got.active {
			t.Fatalf("order %s must not enter fast retry: %#v", order.OrderID, got)
		}
	}
}

func TestSmartQuantityEstimateCapsAutomaticOrderByAccountDeficit(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Product:                     "oauth_7d",
		Strategy:                    managerconfigsvc.SupplyStrategyBalanced,
		HealthyAvailableAccounts:    5,
		ReplenishBatchSize:          10,
		PrelockMinQuantity:          6,
		PrelockMaxQuantity:          10,
		NewAccountConfidence:        0.7,
		AccountMaxRequestsBefore401: 40,
		AccountMaxUsefulSeconds401:  150,
	}
	resource := SmartResource{
		TotalAccounts:       80,
		AvailableAccounts:   3,
		TargetCapacityRCU:   280,
		CapacityGapRCU:      280,
		UnitCapacityRCU:     80,
		SuggestedQuantity:   10,
		ConsumeRCUPerMinute: 10,
	}
	applySmartAccountQuantityEstimate(cfg, &resource)
	if resource.EstimatedRequiredAccounts < 5 || resource.ProjectedAvailableAccounts != 3 ||
		resource.AccountQuantityDeficit != resource.EstimatedRequiredAccounts-3 {
		t.Fatalf("account estimate = %#v", resource)
	}
	quantity := New(nil, nil).smartSuggestedCreateQuantity(cfg, resource)
	if quantity > resource.AccountQuantityDeficit {
		t.Fatalf("prelock minimum raised quantity=%d above account deficit=%d", quantity, resource.AccountQuantityDeficit)
	}
	resource.AvailableAccounts = resource.EstimatedRequiredAccounts
	if quantity := New(nil, nil).smartSuggestedCreateQuantity(cfg, resource); quantity != 1 {
		t.Fatalf("enabled account count must not erase the real capacity deficit, quantity=%d, want 1", quantity)
	}
	resource.CapacityGapRCU = 0
	if quantity := New(nil, nil).smartSuggestedCreateQuantity(cfg, resource); quantity != 0 {
		t.Fatalf("cleared account and capacity deficits quantity=%d, want 0", quantity)
	}
}

func TestSmartEmergencyShortageKeepsHardCreateCooldown(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		ReplenishBatchSize:    15,
		PrelockMinQuantity:    1,
		PrelockMaxQuantity:    7,
		CreateCooldownSeconds: 120,
		CheckIntervalSeconds:  60,
	}
	emergency := SmartResource{
		EffectiveHealthyMinutes: 40,
		CriticalMinutes:         5,
		EstimatedSustainMinutes: 20,
		ConsumeRCUPerMinute:     500,
		CapacityGapRCU:          10_000,
	}
	if !smartEmergencyShortage(emergency) {
		t.Fatalf("20 minute runway for a 40 minute target must be emergency: %#v", emergency)
	}
	if got := smartCreateCooldownForResource(cfg, emergency); got != 60 {
		t.Fatalf("emergency create cooldown=%d, want 60", got)
	}
	if got := smartAutomaticCheckIntervalSeconds(cfg, emergency); got != 60 {
		t.Fatalf("emergency check interval=%d, want 60", got)
	}
	fastCheck := cfg
	fastCheck.CheckIntervalSeconds = 10
	if got := smartCreateCooldownForResource(fastCheck, emergency); got != 10 {
		t.Fatalf("configured ten-second emergency create cooldown=%d, want 10", got)
	}
	if got := smartAutomaticCheckIntervalSeconds(fastCheck, emergency); got != 10 {
		t.Fatalf("configured ten-second emergency observation interval=%d, want 10", got)
	}

	buffered := emergency
	buffered.EstimatedSustainMinutes = 32
	if smartEmergencyShortage(buffered) {
		t.Fatalf("32 minute runway for a 40 minute target must remain buffered observation: %#v", buffered)
	}
}

func TestWarmSmartUsageRestoresDemandWindowAndExcludesFailedRequestRPM(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().Truncate(time.Minute)
	events := make([]usage.Event, 0, 60)
	for minute := 0; minute < 30; minute++ {
		timestamp := now.Add(-time.Duration(minute) * time.Minute)
		events = append(events,
			usage.Event{
				EventHash:   fmt.Sprintf("success-%d", minute),
				TimestampMS: timestamp.UnixMilli(),
				Timestamp:   timestamp.Format(time.RFC3339),
				Provider:    "codex",
				AuthIndex:   "capacity-source",
				Model:       "gpt-test",
				TotalTokens: 40_000,
				CreatedAtMS: timestamp.UnixMilli(),
			},
			usage.Event{
				EventHash:   fmt.Sprintf("failed-%d", minute),
				TimestampMS: timestamp.UnixMilli(),
				Timestamp:   timestamp.Format(time.RFC3339),
				Provider:    "codex",
				AuthIndex:   "capacity-source",
				Model:       "gpt-test",
				Failed:      true,
				CreatedAtMS: timestamp.UnixMilli(),
			},
		)
	}
	if _, err := st.InsertEvents(context.Background(), events); err != nil {
		t.Fatalf("insert events: %v", err)
	}

	service := New(st, nil)
	if err := service.WarmSmartUsage(context.Background()); err != nil {
		t.Fatalf("warm smart usage: %v", err)
	}
	usageStats := service.smartUsageSnapshot(time.Now())
	if usageStats.sampleMinutes != 30 || usageStats.rpm30 != 1 || usageStats.rpm5Peak != 1 || usageStats.tpm30 != 40_000 {
		t.Fatalf("persisted demand window was not restored accurately: %#v", usageStats)
	}
}

func TestCachedInspectionSnapshotSeedsCompleteIndependentQuotaUsage(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "independent-quota-window.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().Truncate(time.Second)
	fileName := "active-team.json"
	authIndex := "active-team-auth"
	events := make([]usage.Event, 0, 4)
	for index := 0; index < 4; index++ {
		timestamp := now.Add(-time.Duration(4-index) * time.Minute)
		if index == 0 {
			// The provider window below starts one day before the inspection.
			// Seed one event at that boundary so this fixture genuinely proves a
			// complete local quota history rather than a mid-window import tail.
			timestamp = now.Add(-24 * time.Hour)
		}
		events = append(events, usage.Event{
			EventHash:        fmt.Sprintf("independent-window-%d", index),
			TimestampMS:      timestamp.UnixMilli(),
			Timestamp:        timestamp.Format(time.RFC3339Nano),
			Provider:         "codex",
			Model:            "gpt-test",
			AuthFileSnapshot: fileName,
			AuthIndex:        authIndex,
			TotalTokens:      1_100_000,
			CreatedAtMS:      timestamp.UnixMilli(),
		})
	}
	if _, err := st.InsertEvents(context.Background(), events); err != nil {
		t.Fatalf("insert usage events: %v", err)
	}
	usedPercent := 11.0
	weeklySeconds := float64(smartQuotaWeekSeconds)
	seedCompletedQuotaInspection(t, st, store.CodexInspectionResult{
		AccountKey:  "active-team",
		FileName:    fileName,
		AuthIndex:   authIndex,
		Provider:    "codex",
		Status:      "active",
		Action:      "keep",
		PlanType:    "team",
		UsedPercent: &usedPercent,
		QuotaWindows: []model.CodexInspectionQuotaWindow{{
			ID:                 "weekly",
			UsedPercent:        &usedPercent,
			ResetAtMS:          now.Add(6 * 24 * time.Hour).UnixMilli(),
			LimitWindowSeconds: &weeklySeconds,
		}},
	})

	service := New(st, nil)
	snapshot, err := service.cachedInspectionQuotaSnapshot(context.Background(), store.ManagerSupplyConfig{}, true)
	if err != nil || len(snapshot.results) != 1 {
		t.Fatalf("load snapshot: results=%d err=%v", len(snapshot.results), err)
	}
	estimate := service.smartQuotaEstimateForInspectionResult(snapshot.results[0], defaultSmartQuotaEstimate(), time.Now())
	if estimate.CapacityM != 40 || !estimate.IndependentAccount || estimate.Source != smartQuotaEstimateSourceCurrent {
		t.Fatalf("independent snapshot estimate = %#v", estimate)
	}
}

func TestCachedInspectionSnapshotKeepsFreshBaselineAfterRefreshTimeout(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	snapshot := inspectionQuotaSnapshot{
		run: store.CodexInspectionRun{
			Status:        model.CodexInspectionStatusCompleted,
			ProbeSetCount: 1,
			SampledCount:  1,
		},
		results: []store.CodexInspectionResult{{
			Provider: "codex",
			Status:   "active",
			Action:   "keep",
		}},
		generatedAt: now.Add(-time.Minute),
		attemptedAt: now,
		lastErr:     context.DeadlineExceeded,
	}
	service := &Service{
		store:         &store.Store{},
		quotaSnapshot: snapshot,
	}

	loaded, err := service.cachedInspectionQuotaSnapshot(
		context.Background(),
		store.ManagerSupplyConfig{AuthFilesCacheTTLSeconds: 60},
		false,
	)
	if err != nil {
		t.Fatalf("fresh historical snapshot inherited refresh timeout: %v", err)
	}
	if loaded.lastErr != context.DeadlineExceeded {
		t.Fatalf("refresh diagnostic was not retained: %v", loaded.lastErr)
	}
	if !smartInspectionSnapshotFresh(loaded, now) {
		t.Fatalf("loaded historical snapshot is not fresh: %#v", loaded)
	}

	loaded.generatedAt = now.Add(-smartInspectionSnapshotFreshTTL - time.Second)
	if err := inspectionSnapshotReadError(loaded, now); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stale historical snapshot hid refresh timeout: %v", err)
	}
}

func TestSmartResourceKeepsSixteenMidWindowTeamAccountsAboveSevenHundredMillion(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	weeklySeconds := float64(smartQuotaWeekSeconds)
	usedPercent := 20.0
	statusOK := http.StatusOK
	results := make([]store.CodexInspectionResult, 0, 16)
	baselines := make([]smartQuotaWindowBaseline, 0, 16)
	for index := 0; index < 16; index++ {
		fileName := fmt.Sprintf("mid-window-team-%02d.json", index)
		authIndex := fmt.Sprintf("mid-window-auth-%02d", index)
		results = append(results, store.CodexInspectionResult{
			FileName:    fileName,
			AuthIndex:   authIndex,
			Provider:    "codex",
			Status:      "active",
			Action:      "keep",
			StatusCode:  &statusOK,
			PlanType:    "team",
			UsedPercent: &usedPercent,
			QuotaWindows: []model.CodexInspectionQuotaWindow{{
				ID:                 "weekly",
				UsedPercent:        &usedPercent,
				ResetAtMS:          now.Add(6 * 24 * time.Hour).UnixMilli(),
				LimitWindowSeconds: &weeklySeconds,
			}},
		})
		baselines = append(baselines, smartQuotaWindowBaseline{
			identity:     "file:" + fileName,
			planType:     "team",
			fraction:     0.20,
			fromMS:       now.Add(-24 * time.Hour).UnixMilli(),
			observedMS:   now.UnixMilli(),
			windowTokens: 4_000_000,
			firstSeenMS:  now.Add(-30 * time.Minute).UnixMilli(),
			lastSeenMS:   now.UnixMilli(),
		})
	}
	service.recordSmartQuotaWindowBaselines(baselines, now)
	resource := service.buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{}, inspectionQuotaSnapshot{
		run: store.CodexInspectionRun{
			ID:            1,
			Status:        model.CodexInspectionStatusCompleted,
			ProbeSetCount: 16,
			SampledCount:  16,
			FinishedAtMS:  now.UnixMilli(),
		},
		results:     results,
		generatedAt: now,
	}, now)

	if resource.NormalAccounts != 16 || resource.AvailableAccounts != 16 || resource.AccountQuotaEstimateM != 60 {
		t.Fatalf("sixteen Team account counts/estimate = %#v", resource)
	}
	if resource.RawCapacityTokenM != 768 || resource.RawCapacityTokenM <= 700 {
		t.Fatalf("sixteen mid-window Team capacity = %.2fM, want 768M", resource.RawCapacityTokenM)
	}
	for _, result := range results {
		if _, ok := service.smartQuotaState.directSamples["file:"+result.FileName]; ok {
			t.Fatalf("mid-window account %q created an absolute quota sample", result.FileName)
		}
	}
}

func TestSmartResourceUsesPersistedSupplyLeaseOnlyAsRoutingHint(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	events := make([]usage.Event, 0, 30)
	for minute := 0; minute < 30; minute++ {
		events = append(events, usage.Event{
			TimestampMS: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(),
			Provider:    "codex",
			AuthIndex:   "lease-source",
			TotalTokens: 100,
		})
	}
	service.recordSmartUsageEvents(events, now)
	usedPercent := 0.0
	fileName := "codex-supply-short-lease.json"
	resource := service.buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{
		Product:              "oauth_30d",
		HealthyMinutesTarget: 40,
	}, inspectionQuotaSnapshot{
		run: store.CodexInspectionRun{ProbeSetCount: 1, SampledCount: 1, FinishedAtMS: now.UnixMilli()},
		results: []store.CodexInspectionResult{{
			AccountKey: "short-lease", FileName: fileName, Provider: "codex", Action: "keep",
			UsedPercent: &usedPercent,
		}},
		leaseExpiresByFile: map[string]int64{fileName: now.Add(5 * time.Minute).UnixMilli()},
		generatedAt:        now,
	}, now)

	if !resource.SnapshotFresh || resource.CapacityLifetimeCoverage != 100 {
		t.Fatalf("active supply lease should be a usable snapshot: %#v", resource)
	}
	if resource.RawCapacityRCU != 125 || resource.CurrentCapacityRCU != 55 ||
		resource.RawCapacityTokenM != 10 || resource.CurrentCapacityTokenM != 4.4 {
		t.Fatalf("supplier timestamp must not cap verified healthy capacity: %#v", resource)
	}
}

func TestSmartResourceKeepsExpiredSupplyLeaseWhenQuotaProbeSucceeds(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	usedPercent := 0.0
	statusCode := http.StatusOK
	service := New(nil, nil)
	fileName := "codex-supply-expired-lease.json"
	resource := service.buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{
		Product: "oauth_30d",
	}, inspectionQuotaSnapshot{
		run: store.CodexInspectionRun{ProbeSetCount: 1, SampledCount: 1, FinishedAtMS: now.UnixMilli()},
		results: []store.CodexInspectionResult{{
			AccountKey: "expired-lease", FileName: fileName, Provider: "codex", Action: "keep",
			Status: "error", StatusCode: &statusCode, UsedPercent: &usedPercent,
		}},
		leaseExpiresByFile: map[string]int64{fileName: now.Add(-time.Minute).UnixMilli()},
		generatedAt:        now,
	}, now)

	if !resource.SnapshotFresh || resource.CapacityLifetimeCoverage != 100 || resource.RawCapacityRCU <= 0 ||
		resource.CurrentCapacityRCU <= 0 || resource.RawCapacityTokenM <= 0 || resource.CurrentCapacityTokenM <= 0 ||
		resource.AvailableAccounts != 1 || resource.HealthyAccounts != 1 || resource.SchedulableAccounts != 1 ||
		resource.EnabledAccounts != 1 || resource.DisabledAccounts != 0 {
		t.Fatalf("a healthy account must retain capacity after its supplier timestamp: %#v", resource)
	}
}

func TestSmartResourceKeepsUnrelatedLegacyCredentialAfterUsefulLeaseWindow(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	usedPercent := 0.0
	resource := New(nil, nil).buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{
		Product: "oauth_30d",
	}, inspectionQuotaSnapshot{
		run: store.CodexInspectionRun{ProbeSetCount: 1, SampledCount: 1, FinishedAtMS: now.UnixMilli()},
		results: []store.CodexInspectionResult{{
			AccountKey: "legacy", FileName: "legacy-unmanaged.json", Provider: "codex", Action: "keep", UsedPercent: &usedPercent,
		}},
		generatedAt: now,
	}, now)

	if resource.AvailableAccounts != 1 || resource.HealthyAccounts != 1 || resource.RawCapacityRCU <= 0 {
		t.Fatalf("an account without a supplier lease remains governed by current quota evidence: %#v", resource)
	}
}

func TestSmartResourceFallbackKeepsHealthyExpiredLeaseMetadata(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	resource := New(nil, nil).buildSmartResourceFromSnapshots(store.ManagerSupplyConfig{
		Product: "oauth_30d",
	}, authFileSnapshot{
		generatedAt: now,
		files: []cpaauthfiles.File{{
			Name:     "expired-fallback.json",
			Provider: "codex",
			Raw: map[string]any{
				"status":                     "ready",
				"remaining_rcu":              125,
				"supply_lease_expires_at_ms": now.Add(-time.Minute).UnixMilli(),
			},
		}},
	}, now)

	if resource.AvailableAccounts != 1 || resource.HealthyAccounts != 1 || resource.SchedulableAccounts != 1 ||
		resource.EnabledAccounts != 1 || resource.DisabledAccounts != 0 ||
		resource.RawCapacityRCU <= 0 || resource.CurrentCapacityRCU <= 0 {
		t.Fatalf("fallback capacity must follow live health rather than the supplier timestamp: %#v", resource)
	}
}

func TestSmartResourceDoesNotExcludeCooldownOnlyInspectionAction(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	usedPercent := 0.0
	resource := New(nil, nil).buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{
		Product: "oauth_30d",
	}, inspectionQuotaSnapshot{
		run: store.CodexInspectionRun{ProbeSetCount: 1, SampledCount: 1, FinishedAtMS: now.UnixMilli()},
		results: []store.CodexInspectionResult{{
			AccountKey: "cooldown", FileName: "manual-cooldown.json", Provider: "codex", Action: "disable",
			Status: "cooldown", Disabled: true, UsedPercent: &usedPercent,
		}},
		generatedAt: now,
	}, now)

	if !resource.SnapshotFresh || resource.RawCapacityRCU <= 0 {
		t.Fatalf("cooldown action alone must not remove usable quota capacity: %#v", resource)
	}
}

func TestSmartResourcePublishesInspectionCredentialCountsForCachedPanels(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	unused := 0.0
	resource := service.buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{
		Product: "oauth_7d",
	}, inspectionQuotaSnapshot{
		run: store.CodexInspectionRun{ProbeSetCount: 4, SampledCount: 4, FinishedAtMS: now.UnixMilli()},
		results: []store.CodexInspectionResult{
			{AccountKey: "healthy", FileName: "healthy.json", Provider: "codex", UsedPercent: &unused},
			{AccountKey: "cooldown", FileName: "cooldown.json", Provider: "codex", Status: "cooldown", Disabled: true, UsedPercent: &unused},
			{AccountKey: "quota", FileName: "quota.json", Provider: "codex", IsQuota: true, UsedPercent: &unused},
			{AccountKey: "invalid", FileName: "invalid.json", Provider: "codex", Status: "unauthorized", Disabled: true, UsedPercent: &unused},
		},
		generatedAt: now,
	}, now)

	if resource.TotalAccounts != 4 || resource.SchedulableAccounts != 2 || resource.AvailableAccounts != 2 ||
		resource.HealthyAccounts != 2 || resource.WeakAccounts != 0 || resource.DisabledAccounts != 2 {
		t.Fatalf("inspection credential counts = %#v", resource)
	}
	encoded, err := json.Marshal(resource)
	if err != nil {
		t.Fatalf("marshal smart resource: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("unmarshal smart resource: %v", err)
	}
	if payload["totalAccounts"] != float64(4) || payload["availableAccounts"] != float64(2) ||
		payload["schedulableAccounts"] != float64(2) || payload["healthyAccounts"] != float64(2) ||
		payload["normalAccounts"] != float64(1) || payload["atRiskAccounts"] != float64(1) ||
		payload["weakAccounts"] != float64(0) ||
		payload["disabledAccounts"] != float64(2) {
		t.Fatalf("serialized inspection counts = %#v", payload)
	}
}

func TestInspectionCapacityExcludesQuotaEvenWhenCooldownIsPresent(t *testing.T) {
	if !inspectionResultCapacityExcluded(store.CodexInspectionResult{IsQuota: true, Status: "cooldown", Disabled: true}) {
		t.Fatal("quota exhaustion must stay excluded even when a cooldown label is present")
	}
	if !inspectionResultCapacityExcluded(store.CodexInspectionResult{Status: "quota cooldown", Disabled: true}) {
		t.Fatal("quota evidence in a cooldown message must stay excluded")
	}
	if inspectionResultCapacityExcluded(store.CodexInspectionResult{Status: "cooldown", Disabled: true}) {
		t.Fatal("a cooldown-only disabled state should retain verified capacity")
	}
}

func TestSmartAutomaticPausesWithoutInspectionSnapshotAfterPoolCheck(t *testing.T) {
	var authFileRequests atomic.Int32
	var supplyRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files":
			authFileRequests.Add(1)
			_, _ = w.Write([]byte(`{"files":[{"name":"a.json","provider":"codex"},{"name":"b.json","provider":"codex"},{"name":"c.json","provider":"codex"}]}`))
		case "/api/customer/login", "/api/customer/inventory", "/api/customer/balance", "/api/customer/pickup/orders":
			supplyRequests.Add(1)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "smart-no-quota-snapshot.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_30d",
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("run automatic: %v", err)
	}
	if authFileRequests.Load() != 1 || supplyRequests.Load() != 0 {
		t.Fatalf("automatic pause made unexpected requests auth=%d supply=%d", authFileRequests.Load(), supplyRequests.Load())
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status.SmartResource.DecisionReason != "inspection_snapshot_unavailable" || status.ActiveOrder != nil {
		t.Fatalf("status = %#v", status)
	}
}

func TestSmartAutomaticCreatesFromRecentPartialInspectionAfterRestart(t *testing.T) {
	var createCalls atomic.Int32
	var createQuantity atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files":
			_, _ = w.Write([]byte(`{"files":[
				{"name":"verified.json","provider":"codex","status":"active"},
				{"name":"quota-pending.json","provider":"codex","status":"active"}
			]}`))
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":70,"missing":0,"needs_production":false,"estimated_total_fen":100}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":100000,"balance_fen":100000}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			createCalls.Add(1)
			var payload struct {
				Quantity int `json:"quantity"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode create payload: %v", err)
			}
			createQuantity.Store(int32(payload.Quantity))
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"order":{"id":"partial-snapshot-order","status":"waiting_inventory","quantity":1,"retry_after_seconds":60}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "smart-partial-restart.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, SmartEnabled: &enabled,
			BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_30d",
			TargetAvailableAccounts: 45, ReplenishBatchSize: 1,
			PrelockMinQuantity: 1, PrelockMaxQuantity: 1,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	verified := quotaInspectionResult(50)
	verified.AccountKey = "verified"
	verified.FileName = "verified.json"
	verified.PlanType = "team"
	pending := store.CodexInspectionResult{
		AccountKey: "quota-pending", FileName: "quota-pending.json", Provider: "codex", Action: "keep", PlanType: "team",
	}
	seedCompletedQuotaInspection(t, st, verified, pending)

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	now := time.Now()
	events := make([]usage.Event, 0, 30)
	for minute := 0; minute < 30; minute++ {
		events = append(events, usage.Event{
			TimestampMS: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(),
			Provider:    "codex",
			AuthIndex:   "load-source",
			TotalTokens: 10_000_000,
		})
	}
	service.recordSmartUsageEvents(events, now)

	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("run automatic: %v", err)
	}
	if createCalls.Load() != 1 || createQuantity.Load() != 1 {
		t.Fatalf("partial snapshot create calls/quantity = %d/%d, want 1/1", createCalls.Load(), createQuantity.Load())
	}
	tasks, err := st.ListSupplyPurchaseTasks(context.Background(), 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("partial snapshot purchase tasks=%#v err=%v", tasks, err)
	}
	orders, err := st.ListSupplyOrders(context.Background(), 10)
	if err != nil || len(orders) != 1 || orders[0].RequestedQuantity != 1 {
		t.Fatalf("partial snapshot orders=%#v err=%v", orders, err)
	}
}

func TestSmartResourceDoesNotFallbackToAccountCountWithoutUsageRate(t *testing.T) {
	service := New(nil, nil)
	now := time.Now()
	files := make([]cpaauthfiles.File, 0, 120)
	for index := 0; index < 120; index++ {
		files = append(files, cpaauthfiles.File{
			Name:     "account.json",
			Provider: "codex",
			Raw:      map[string]any{"status": "ready", "success": 100, "failed": 0},
		})
	}

	resource := service.buildSmartResourceFromSnapshots(store.ManagerSupplyConfig{
		Product:                 "oauth_30d",
		TargetAvailableAccounts: 1,
		HealthyMinutesTarget:    120,
		PrelockMinQuantity:      1,
		PrelockMaxQuantity:      10,
		NewAccountConfidence:    0.7,
	}, authFileSnapshot{generatedAt: now, files: files}, now)

	if resource.DecisionReason != "usage_rate_not_ready" || resource.SuggestedQuantity != 0 || resource.CapacityGapRCU != 0 {
		t.Fatalf("smart resource should wait for burn-rate samples, got %#v", resource)
	}
	if resource.AvailableAccounts == 0 || resource.CurrentCapacityRCU == 0 {
		t.Fatalf("capacity should still be reported for the dashboard: %#v", resource)
	}
}

func TestNoTrafficKeepsOnlyMinimumPoolAndCreatesNoCapacityOrder(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Strategy:                  managerconfigsvc.SupplyStrategyStrongSupply,
		CriticalAvailableAccounts: 2,
		HealthyAvailableAccounts:  10,
	}
	resource := SmartResource{
		AvailableAccounts:       3,
		SchedulableAccounts:     3,
		HealthyAccounts:         3,
		ConsumeRCUPerMinute:     0,
		DemandTrend:             smartDemandTrendUnknown,
		CurrentCapacityRCU:      200,
		CapacityGapRCU:          100,
		SuggestedQuantity:       8,
		EffectiveHealthyMinutes: 55,
	}
	recalculateSmartResourceCapacityPlan(cfg, &resource)
	if resource.SuggestedAction != smartActionObserveDemand || resource.DecisionReason != "no_traffic_minimum_pool" ||
		resource.TargetCapacityRCU != 0 || resource.CapacityGapRCU != 0 || resource.SuggestedQuantity != 0 {
		t.Fatalf("no-traffic pool must not buy unused capacity: %#v", resource)
	}
	applySmartEmergencyAvailability(cfg, &resource, time.Now())
	if resource.EmergencyShortage || resource.SuggestedQuantity != 0 {
		t.Fatalf("pool above the critical floor must stay idle: %#v", resource)
	}
	resource.AvailableAccounts = 2
	applySmartEmergencyAvailability(cfg, &resource, time.Now())
	if resource.EmergencyShortage || resource.SuggestedQuantity != 0 {
		t.Fatalf("idle pool at the critical floor must observe without buying to healthy: %#v", resource)
	}
	resource.AvailableAccounts = 1
	applySmartEmergencyAvailability(cfg, &resource, time.Now())
	if !resource.EmergencyShortage || resource.SuggestedQuantity != 5 {
		t.Fatalf("idle pool below critical must refill only to the critical floor: %#v", resource)
	}
	resource.AvailableAccounts = 0
	resource.SuggestedQuantity = 0
	applySmartEmergencyAvailability(cfg, &resource, time.Now())
	if !resource.EmergencyShortage || resource.SuggestedQuantity != 5 || resource.EmergencyReason != "emergency_pool_vacuum" {
		t.Fatalf("idle empty pool must keep a minimum emergency batch: %#v", resource)
	}
}

func TestStartupAccountFloorKeepsPurchasingWithoutTraffic(t *testing.T) {
	startupAccounts := 5
	cfg := store.ManagerSupplyConfig{
		Strategy:                    managerconfigsvc.SupplyStrategyStrongSupply,
		CriticalAvailableAccounts:   2,
		HealthyAvailableAccounts:    10,
		StartupAvailableAccounts:    &startupAccounts,
		DefaultEmergencyMinAccounts: 5,
		Product:                     "oauth_7d",
	}

	resource := SmartResource{AvailableAccounts: 4, DemandTrend: smartDemandTrendUnknown}
	applySmartEmergencyAvailability(cfg, &resource, time.Now())
	if !resource.EmergencyShortage || resource.EmergencyReason != "startup_account_floor" ||
		resource.SuggestedAction != smartActionEmergencyReplenish || resource.SuggestedQuantity != 1 {
		t.Fatalf("startup floor shortage = %#v", resource)
	}
	if !smartShortageFastRetryAllowed(resource) {
		t.Fatal("startup floor shortage must keep the fast supplier retry loop active")
	}

	ready := SmartResource{AvailableAccounts: 5, DemandTrend: smartDemandTrendUnknown}
	applySmartEmergencyAvailability(cfg, &ready, time.Now())
	if ready.EmergencyShortage || ready.SuggestedQuantity != 0 {
		t.Fatalf("startup floor should stop after the target is reached: %#v", ready)
	}

	empty := SmartResource{AvailableAccounts: 0, DemandTrend: smartDemandTrendUnknown}
	applySmartEmergencyAvailability(cfg, &empty, time.Now())
	if !empty.EmergencyShortage || empty.EmergencyReason != "emergency_pool_vacuum" ||
		empty.SuggestedQuantity != 5 {
		t.Fatalf("empty startup floor refill = %#v", empty)
	}
}

func TestStartupAccountFloorDoesNotOverrideActivePurchaseTiming(t *testing.T) {
	startupAccounts := 15
	cfg := store.ManagerSupplyConfig{
		Strategy:                  managerconfigsvc.SupplyStrategyStrongSupply,
		StartupAvailableAccounts:  &startupAccounts,
		CriticalAvailableAccounts: 2,
		HealthyAvailableAccounts:  10,
		PollIntervalSeconds:       2,
	}
	resource := SmartResource{
		AvailableAccounts:              10,
		ConsumeRCUPerMinute:            10,
		DemandPlanningRCUPerMinute:     10,
		AvailableCapacityRCU:           267,
		CurrentCapacityRCU:             410,
		TotalCapacityRCU:               410,
		EstimatedSustainMinutes:        41,
		AvailableSustainMinutes:        26.7,
		EffectiveHealthyMinutes:        40,
		WarningMinutes:                 25,
		CriticalMinutes:                20,
		TokenCapacityMode:              smartTokenCapacityMode,
		EstimatedNewAccountCapacityRCU: 22,
		SuggestedQuantity:              0,
	}

	if smartAvailableCapacityEmergency(cfg, resource) {
		t.Fatalf("active pool above the critical runway must not use the startup floor: %#v", resource)
	}
	applySmartEmergencyAvailability(cfg, &resource, time.Now())
	if resource.EmergencyShortage || resource.SuggestedAction == smartActionEmergencyReplenish ||
		resource.SuggestedQuantity != 0 {
		t.Fatalf("startup floor overrode active purchase timing: %#v", resource)
	}

	pressure := smartSupplyPressure{
		level:                       smartSupplyPressurePlenty,
		reliablyAvailable:           true,
		shortWindowOrders:           3,
		recentSuccessStreak:         3,
		shortWindowFulfillmentRate:  100,
		shortWindowAvgFulfillSecond: 5,
	}
	timing := smartJustInTimePurchase(cfg, resource, pressure, resource.SuggestedQuantity)
	if timing.triggerMinutes != 24.8 || timing.waitMinutes != 16.2 || timing.eligibleQuantity != 0 {
		t.Fatalf("active purchase timing = %#v, want trigger 24.8m and wait 16.2m", timing)
	}
}

func TestStartupAccountFloorUsesRecentDemandMemoryInsteadOfIdleEmergency(t *testing.T) {
	startupAccounts := 15
	cfg := store.ManagerSupplyConfig{
		StartupAvailableAccounts:  &startupAccounts,
		CriticalAvailableAccounts: 2,
	}
	resource := SmartResource{
		AvailableAccounts:          11,
		ConsumeRCUPerMinute:        0,
		DemandPlanningRCUPerMinute: 10,
		DemandMemoryRCUPerMinute:   10,
		VirtualDemandRCUPerMinute:  10,
		CurrentCapacityRCU:         410,
		AvailableCapacityRCU:       267,
		AvailableSustainMinutes:    26.7,
		EstimatedSustainMinutes:    41,
		EffectiveHealthyMinutes:    120,
		WarningMinutes:             100,
		CriticalMinutes:            80,
		CriticalAvailableAccounts:  2,
	}
	if smartStartupAccountFloorEmergency(cfg, resource) {
		t.Fatalf("recent demand history must keep startup floor on capacity timing: %#v", resource)
	}
	applySmartEmergencyAvailability(cfg, &resource, time.Now())
	if resource.EmergencyShortage || resource.SuggestedAction == smartActionEmergencyReplenish {
		t.Fatalf("recent demand history became an idle startup emergency: %#v", resource)
	}
}

func TestStartupAccountFloorDoesNotOverrideHealthyVirtualRunway(t *testing.T) {
	startupAccounts := 15
	cfg := store.ManagerSupplyConfig{
		Strategy:                  managerconfigsvc.SupplyStrategyStrongSupply,
		StartupAvailableAccounts:  &startupAccounts,
		CriticalAvailableAccounts: 2,
		HealthyAvailableAccounts:  10,
		CriticalMinutes:           80,
	}
	resource := SmartResource{
		AvailableAccounts:          10,
		CurrentCapacityRCU:         1_500,
		AvailableCapacityRCU:       1_500,
		DemandPlanningRCUPerMinute: 0.11,
		DemandMemoryAgeSeconds:     int((smartEmergencyDemandMemoryMaxAge / time.Second) + 1),
		CriticalMinutes:            80,
	}
	if smartStartupAccountFloorEmergency(cfg, resource) {
		t.Fatalf("healthy virtual runway must suppress startup-floor emergency: %#v", resource)
	}
	applySmartEmergencyAvailability(cfg, &resource, time.Now())
	if resource.EmergencyShortage || resource.EmergencyReason != "" || resource.SuggestedQuantity != 0 {
		t.Fatalf("healthy virtual runway triggered emergency refill: %#v", resource)
	}
}

func TestProgressiveStartupFloorUsesOneOrderAndNormalCooldown(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		ReplenishBatchSize:    10,
		PrelockMinQuantity:    1,
		PrelockMaxQuantity:    10,
		CreateCooldownSeconds: 30,
		CheckIntervalSeconds:  10,
	}
	resource := SmartResource{
		SnapshotFresh:             true,
		CapacitySource:            smartCapacitySourceInspection,
		CurrentCapacityRCU:        410,
		AvailableAccounts:         11,
		CriticalAvailableAccounts: 2,
		EmergencyShortage:         true,
		EmergencyReason:           "startup_account_floor",
		DecisionReason:            "startup_account_floor",
		EffectiveHealthyMinutes:   120,
		WarningMinutes:            100,
		CriticalMinutes:           80,
	}
	if !smartProgressiveStartupFloorRecovery(resource) {
		t.Fatalf("historical capacity baseline must make startup recovery progressive: %#v", resource)
	}
	quantity, reason, timing := smartPrelockQuantityForSupplyPressureWithTiming(
		cfg,
		resource,
		smartSupplyPressure{level: smartSupplyPressurePlenty},
		4,
	)
	if quantity != 1 || reason != "startup_account_floor" || timing.eligibleQuantity != 1 {
		t.Fatalf("progressive startup quantity = %d/%q %#v, want one", quantity, reason, timing)
	}
	if cooldown := smartCreateCooldownForResource(cfg, resource); cooldown != 120 {
		t.Fatalf("progressive startup create cooldown = %d, want 120", cooldown)
	}
	if cooldown := smartSuccessfulOrderCooldownForResource(cfg, resource); cooldown != 120 {
		t.Fatalf("progressive startup success cooldown = %d, want 120", cooldown)
	}

	resource.AvailableAccounts = 2
	if smartProgressiveStartupFloorRecovery(resource) {
		t.Fatalf("critical account floor must retain real rescue behavior: %#v", resource)
	}
	quantity, reason, _ = smartPrelockQuantityForSupplyPressureWithTiming(
		cfg,
		resource,
		smartSupplyPressure{level: smartSupplyPressurePlenty},
		4,
	)
	if quantity != 4 || reason != "startup_account_floor" {
		t.Fatalf("critical rescue quantity = %d/%q, want unchanged emergency rung", quantity, reason)
	}

	resource.AvailableAccounts = 3
	resource.AvailableSustainMinutes = 10
	resource.EstimatedSustainMinutes = 10
	if smartProgressiveStartupFloorRecovery(resource) {
		t.Fatalf("short historical runway must retain real rescue behavior: %#v", resource)
	}

	resource.AvailableSustainMinutes = 0
	resource.EstimatedSustainMinutes = 0
	if !smartProgressiveStartupFloorRecovery(resource) {
		t.Fatalf("missing runway telemetry with several accounts must default to progressive recovery: %#v", resource)
	}
}

func TestHistoricalCapacitySuppressesTransientStartupFloorPurchase(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	cfg := store.ManagerSupplyConfig{
		CheckIntervalSeconds: 30,
		HealthyMinutesTarget: 120,
		WarningMinutes:       100,
		CriticalMinutes:      80,
	}
	resource := SmartResource{
		GeneratedAtMS:             now.UnixMilli(),
		SnapshotFresh:             true,
		CapacitySource:            smartCapacitySourceInspection,
		CurrentCapacityRCU:        39_000,
		AvailableCapacityRCU:      33_000,
		AvailableAccounts:         12,
		CriticalAvailableAccounts: 2,
		EffectiveHealthyMinutes:   120,
		WarningMinutes:            100,
		CriticalMinutes:           80,
		EmergencyShortage:         true,
		EmergencyReason:           "startup_account_floor",
		DecisionReason:            "startup_account_floor",
		SuggestedAction:           smartActionEmergencyReplenish,
		SuggestedQuantity:         3,
	}
	previous := SmartResource{
		GeneratedAtMS:              now.Add(-30 * time.Second).UnixMilli(),
		SnapshotFresh:              true,
		CapacitySource:             smartCapacitySourceInspection,
		ConsumeRCUPerMinute:        280,
		DemandPlanningRCUPerMinute: 280,
		DemandMemoryRCUPerMinute:   290,
		DemandMemoryLastSeenMS:     now.Add(-45 * time.Second).UnixMilli(),
		AvailableSustainMinutes:    118,
		EstimatedSustainMinutes:    142,
		AvailableAccounts:          12,
		CriticalAvailableAccounts:  2,
	}
	if !applyHistoricalCapacityStartupObservation(cfg, &resource, previous, now) {
		t.Fatalf("recent verified capacity history was not applied: %#v", resource)
	}
	if resource.EmergencyShortage || resource.EmergencyReason != "" ||
		resource.SuggestedAction != smartActionObserveDemand || resource.SuggestedQuantity != 0 ||
		resource.DecisionReason != "historical_capacity_observe" || resource.AvailableSustainMinutes <= 100 {
		t.Fatalf("historical capacity observation = %#v", resource)
	}

	short := resource
	short.EmergencyShortage = true
	short.EmergencyReason = "startup_account_floor"
	short.DecisionReason = "startup_account_floor"
	short.SuggestedAction = smartActionEmergencyReplenish
	short.SuggestedQuantity = 3
	short.AvailableCapacityRCU = 3_000
	if applyHistoricalCapacityStartupObservation(cfg, &short, previous, now) {
		t.Fatalf("short verified runway must retain rescue behavior: %#v", short)
	}
}

func TestVirtualDemandSizesEmptyPoolEmergencyWithoutFixedBatchWaste(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Strategy:                    managerconfigsvc.SupplyStrategyStrongSupply,
		CriticalAvailableAccounts:   2,
		HealthyAvailableAccounts:    10,
		DefaultEmergencyMinAccounts: 5,
		ReplenishBatchSize:          15,
		PrelockMaxQuantity:          9,
	}
	resource := SmartResource{
		AvailableAccounts:              0,
		HealthyAccounts:                0,
		ConsumeRCUPerMinute:            0,
		DemandPlanningRCUPerMinute:     3.29,
		VirtualDemandRCUPerMinute:      3.29,
		DemandMemoryAgeSeconds:         120,
		EstimatedNewAccountCapacityRCU: 40.51,
		CriticalMinutes:                15,
		EffectiveHealthyMinutes:        30,
	}

	applySmartEmergencyAvailability(cfg, &resource, time.Now())

	if !resource.EmergencyShortage || resource.EmergencyReason != "emergency_pool_vacuum" ||
		resource.SuggestedQuantity != 3 {
		t.Fatalf("virtual-demand empty-pool refill = %#v, want demand-sized quantity 3", resource)
	}
	if resource.ConsumeRCUPerMinute != 0 {
		t.Fatalf("virtual demand leaked into displayed current burn rate: %#v", resource)
	}
}

func TestVirtualDemandSizesSubcriticalPoolProgressively(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Strategy:                    managerconfigsvc.SupplyStrategyStrongSupply,
		CriticalAvailableAccounts:   2,
		HealthyAvailableAccounts:    10,
		DefaultEmergencyMinAccounts: 5,
		ReplenishBatchSize:          15,
		PrelockMaxQuantity:          9,
	}
	resource := SmartResource{
		AvailableAccounts:              1,
		HealthyAccounts:                1,
		ConsumeRCUPerMinute:            0,
		DemandPlanningRCUPerMinute:     3.29,
		DemandMemoryAgeSeconds:         120,
		EstimatedNewAccountCapacityRCU: 40.51,
		CriticalMinutes:                15,
		EffectiveHealthyMinutes:        30,
	}

	applySmartEmergencyAvailability(cfg, &resource, time.Now())

	if !resource.EmergencyShortage || resource.EmergencyReason != "available_capacity_critical" ||
		resource.SuggestedQuantity != 2 {
		t.Fatalf("virtual-demand subcritical refill = %#v, want progressive quantity 2", resource)
	}
}

func TestSmartResourceKeepsTransientErrorsInCapacity(t *testing.T) {
	service := New(nil, nil)
	now := time.Now()
	events := make([]usage.Event, 0, 60)
	for minute := 0; minute < 30; minute++ {
		for index := 0; index < 2; index++ {
			events = append(events, usage.Event{
				TimestampMS: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(),
				Provider:    "codex",
				AuthIndex:   "load-source",
				TotalTokens: 100,
			})
		}
	}
	service.recordSmartUsageEvents(events, now)

	resource := service.buildSmartResourceFromSnapshots(store.ManagerSupplyConfig{
		Product:              "oauth_30d",
		HealthyMinutesTarget: 120,
		WarningMinutes:       60,
		CriticalMinutes:      30,
		PrelockMinQuantity:   1,
		PrelockMaxQuantity:   10,
		NewAccountConfidence: 0.7,
	}, authFileSnapshot{
		generatedAt: now,
		files: []cpaauthfiles.File{
			{
				Name:     "healthy.json",
				Provider: "codex",
				Raw: map[string]any{
					"status":        "ready",
					"remaining_rcu": 60,
					"recent_requests": []any{
						map[string]any{"success": 12, "failed": 0},
					},
				},
			},
			{
				Name:     "weak.json",
				Provider: "codex",
				Raw: map[string]any{
					"status":        "error",
					"remaining_rcu": 100,
					"recent_requests": []any{
						map[string]any{"success": 1, "failed": 9},
					},
				},
			},
		},
	}, now)

	if resource.SchedulableAccounts != 2 || resource.HealthyAccounts != 2 || resource.WeakAccounts != 0 {
		t.Fatalf("transient runtime errors must not reduce credential health: %#v", resource)
	}
	if resource.AvailableAccounts != 2 || resource.RawCapacityRCU != 160 || resource.CurrentCapacityRCU != 160 {
		t.Fatalf("transient runtime errors must retain all capacity without an upstream expiry, got %#v", resource)
	}
	if resource.ConsumeRCUPerMinute <= 0 || resource.HealthLevel != smartHealthWarning || resource.CapacityGapRCU != 80 {
		t.Fatalf("restored cooldown capacity should still honor the configured 120-minute target: %#v", resource)
	}
}

func TestSmartResourceTreatsActiveCredentialAsHealthyWithoutHistory(t *testing.T) {
	service := New(nil, nil)
	now := time.Now()

	resource := service.buildSmartResourceFromSnapshots(store.ManagerSupplyConfig{
		Product:              "oauth_7d",
		HealthyMinutesTarget: 30,
		PrelockMinQuantity:   1,
		PrelockMaxQuantity:   10,
	}, authFileSnapshot{
		generatedAt: now,
		files: []cpaauthfiles.File{{
			Name:     "active.json",
			Provider: "codex",
			Raw: map[string]any{
				"status":        "active",
				"remaining_rcu": 100,
				"success":       0,
				"failed":        20,
			},
		}},
	}, now)

	if resource.SchedulableAccounts != 1 || resource.AvailableAccounts != 1 || resource.HealthyAccounts != 1 || resource.WeakAccounts != 0 {
		t.Fatalf("active credential should be fully usable regardless of stale request counters: %#v", resource)
	}
	if resource.RawCapacityRCU != 100 || resource.CurrentCapacityRCU != 100 {
		t.Fatalf("active credential balance should not be weighted down: %#v", resource)
	}
}

func TestSmartResourceIgnoresCPAUnavailableDuringCooldown(t *testing.T) {
	now := time.Now()
	resource := New(nil, nil).buildSmartResourceFromSnapshots(store.ManagerSupplyConfig{
		Product:              "oauth_30d",
		HealthyMinutesTarget: 40,
	}, authFileSnapshot{
		generatedAt: now,
		files: []cpaauthfiles.File{
			{
				Name:     "still-schedulable.json",
				Provider: "codex",
				Raw: map[string]any{
					"status":        "error",
					"unavailable":   false,
					"remaining_rcu": 80,
				},
			},
			{
				Name:     "all-models-cooling.json",
				Provider: "codex",
				Raw: map[string]any{
					"status":           "error",
					"unavailable":      true,
					"next_retry_after": now.Add(time.Minute).Format(time.RFC3339),
					"remaining_rcu":    80,
				},
			},
			{
				Name:     "legacy-error.json",
				Provider: "codex",
				Raw: map[string]any{
					"status":        "error",
					"remaining_rcu": 80,
				},
			},
		},
	}, now)

	if resource.SchedulableAccounts != 3 || resource.AvailableAccounts != 3 || resource.HealthyAccounts != 3 || resource.WeakAccounts != 0 {
		t.Fatalf("cooldown and unavailable runtime fields must not change credential statistics: %#v", resource)
	}
	if resource.RawCapacityRCU != 240 {
		t.Fatalf("cooldown credentials must retain their remaining capacity: %#v", resource)
	}
}

func TestGetStatusRefreshesSmartSnapshotWhenAutomaticSupplyDisabled(t *testing.T) {
	var authFileRequests atomic.Int32
	var supplyRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files":
			authFileRequests.Add(1)
			if r.Header.Get("Authorization") != "Bearer management-key" {
				http.Error(w, "missing management key", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"files":[
				{"name":"ready-a.json","provider":"codex","status":"ready","remaining_rcu":100},
				{"name":"ready-b.json","provider":"codex","status":"ready","remaining_rcu":100},
				{"name":"disabled.json","provider":"codex","status":"disabled","disabled":true}
			]}`))
		case "/api/customer/login", "/api/customer/inventory", "/api/customer/balance", "/api/customer/pickup/orders":
			supplyRequests.Add(1)
			http.Error(w, "supply request is unexpected", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	automaticSupplyEnabled := false
	smartEnabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled:                  &automaticSupplyEnabled,
			SmartEnabled:             &smartEnabled,
			Product:                  "oauth_7d",
			HealthyMinutesTarget:     40,
			AuthFilesCacheTTLSeconds: 60,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	seedCompletedQuotaInspection(t, st, quotaInspectionResult(0))
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())

	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("first status: %v", err)
	}
	if !status.SmartResource.SnapshotFresh || status.SmartResource.CapacitySource != smartCapacitySourceInspection {
		t.Fatalf("cold status should load the completed quota inspection snapshot: %#v", status.SmartResource)
	}
	if status.SmartResource.TotalAccounts != 2 || status.SmartResource.AvailableAccounts != 2 ||
		status.SmartResource.HealthyAccounts != 2 || status.SmartResource.WeakAccounts != 0 ||
		status.SmartResource.DisabledAccounts != 1 {
		t.Fatalf("cold status did not combine live pool counts with inspection health: %#v", status.SmartResource)
	}
	firstRunID := status.SmartResource.CapacitySnapshotRunID
	if authFileRequests.Load() != 1 || supplyRequests.Load() != 0 {
		t.Fatalf("status refresh requests auth=%d supply=%d", authFileRequests.Load(), supplyRequests.Load())
	}

	status, err = service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("second status: %v", err)
	}
	if !status.SmartResource.SnapshotFresh || authFileRequests.Load() != 1 || supplyRequests.Load() != 0 {
		t.Fatalf("cached status should not refetch or create supply orders: status=%#v auth=%d supply=%d", status.SmartResource, authFileRequests.Load(), supplyRequests.Load())
	}
	seedCompletedQuotaInspection(t, st, quotaInspectionResult(25), quotaInspectionResult(50))
	// Completed inspections normally invalidate the status snapshot through the
	// inspection refresher. This test writes the repository directly, so mirror
	// that service-level notification before reading the new run.
	service.invalidateStatusCache()
	status, err = service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("status after newer inspection: %v", err)
	}
	if status.SmartResource.CapacitySnapshotRunID <= firstRunID || status.SmartResource.TotalAccounts != 2 ||
		status.SmartResource.AvailableAccounts != 2 || status.SmartResource.HealthyAccounts != 2 ||
		status.SmartResource.WeakAccounts != 0 || status.SmartResource.DisabledAccounts != 1 {
		t.Fatalf("status did not adopt the newer completed inspection: %#v", status.SmartResource)
	}
	if authFileRequests.Load() != 1 || supplyRequests.Load() != 0 {
		t.Fatalf("newer inspection refresh made upstream requests: auth=%d supply=%d", authFileRequests.Load(), supplyRequests.Load())
	}
}

func TestGetStatusRecomputesDemandWhenTrafficStops(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/auth-files" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"files":[
			{"name":"ready-a.json","provider":"codex","status":"ready"},
			{"name":"ready-b.json","provider":"codex","status":"ready"}
		]}`))
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "status-demand-decay.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	automaticEnabled := false
	smartEnabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &automaticEnabled, SmartEnabled: &smartEnabled, Product: "oauth_7d",
			Strategy: managerconfigsvc.SupplyStrategyCustom, HealthyAvailableAccounts: 2,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	seedCompletedQuotaInspection(t, st, quotaInspectionResult(0))
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	service.setSmartResource(SmartResource{
		Enabled: true, SnapshotFresh: true, GeneratedAtMS: time.Now().UnixMilli(),
		CapacitySnapshotRunID: 1, ConsumeRCUPerMinute: 537.1,
		RequestDemandRCUPerMinute: 208.9, TokenDemandRCUPerMinute: 537.1, DemandDriver: "tokens",
	})

	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status.SmartResource.ConsumeRCUPerMinute != 0 || status.SmartResource.RequestDemandRCUPerMinute != 0 ||
		status.SmartResource.TokenDemandRCUPerMinute != 0 || status.SmartResource.DemandDriver != "none" ||
		status.SmartResource.DecisionReason != "usage_rate_not_ready" {
		t.Fatalf("status retained stale demand after traffic stopped: %#v", status.SmartResource)
	}
}

func TestGetStatusStartsSingleFlightRefreshForStaleInspectionSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	automaticSupplyEnabled := false
	smartEnabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled:                  &automaticSupplyEnabled,
			SmartEnabled:             &smartEnabled,
			Product:                  "oauth_7d",
			HealthyMinutesTarget:     40,
			AuthFilesCacheTTLSeconds: 60,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	seedCompletedQuotaInspection(t, st, quotaInspectionResult(0))
	runs, err := st.ListCodexInspectionRuns(context.Background(), 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("list seeded inspection runs: runs=%#v err=%v", runs, err)
	}
	staleRun := runs[0]
	staleRun.FinishedAtMS = time.Now().Add(-21 * time.Minute).UnixMilli()
	if err := st.UpdateCodexInspectionRun(context.Background(), staleRun); err != nil {
		t.Fatalf("age inspection run: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	var refreshCalls atomic.Int32
	service.SetInspectionSnapshotRefresher(context.Background(), func(context.Context) error {
		refreshCalls.Add(1)
		close(started)
		<-release
		latest, err := st.ListCodexInspectionRuns(context.Background(), 1)
		if err != nil {
			return err
		}
		updated := latest[0]
		updated.FinishedAtMS = time.Now().UnixMilli()
		if err := st.UpdateCodexInspectionRun(context.Background(), updated); err != nil {
			return err
		}
		close(finished)
		return nil
	})

	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("load stale status: %v", err)
	}
	if status.SmartResource.SnapshotFresh || !status.SmartResource.SnapshotRefreshInProgress {
		t.Fatalf("stale status did not report the background refresh: %#v", status.SmartResource)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stale status did not start an inspection refresh")
	}

	if _, err := service.GetStatus(context.Background(), 10); err != nil {
		t.Fatalf("load while refresh is active: %v", err)
	}
	if calls := refreshCalls.Load(); calls != 1 {
		t.Fatalf("stale status started %d refreshes, want one", calls)
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("background refresh did not finish")
	}

	deadline := time.Now().Add(time.Second)
	for {
		status, err = service.GetStatus(context.Background(), 10)
		if err != nil {
			t.Fatalf("load refreshed status: %v", err)
		}
		if status.SmartResource.SnapshotFresh {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("completed refresh did not invalidate stale quota cache: %#v", status.SmartResource)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInspectionSnapshotRefreshNeededDefersRecentPartialEvidence(t *testing.T) {
	recent := SmartResource{
		Enabled:                    true,
		SnapshotFresh:              false,
		CapacitySnapshotAtMS:       time.Now().Add(-2 * time.Minute).UnixMilli(),
		CapacitySnapshotAgeSeconds: int((2 * time.Minute) / time.Second),
	}
	for _, reason := range []string{
		"inspection_quota_incomplete",
		"inspection_quota_incomplete_capacity_deficit",
		"inspection_usability_incomplete",
		"inspection_usability_incomplete_capacity_deficit",
	} {
		resource := recent
		resource.DecisionReason = reason
		if smartInspectionSnapshotRefreshNeeded(resource) {
			t.Fatalf("recent partial snapshot %q must wait for the normal inspection cadence", reason)
		}
	}

	stale := recent
	stale.DecisionReason = "inspection_quota_incomplete"
	stale.CapacitySnapshotAgeSeconds = int(smartInspectionSnapshotFreshTTL/time.Second) + 1
	if !smartInspectionSnapshotRefreshNeeded(stale) {
		t.Fatal("expired partial snapshot must request a refresh")
	}

	missing := recent
	missing.DecisionReason = "inspection_quota_incomplete"
	missing.CapacitySnapshotAtMS = 0
	if !smartInspectionSnapshotRefreshNeeded(missing) {
		t.Fatal("partial evidence without a completed snapshot must request a refresh")
	}

	unavailable := recent
	unavailable.DecisionReason = "inspection_snapshot_unavailable"
	if !smartInspectionSnapshotRefreshNeeded(unavailable) {
		t.Fatal("an unavailable snapshot must request a refresh")
	}

	fresh := recent
	fresh.SnapshotFresh = true
	if smartInspectionSnapshotRefreshNeeded(fresh) {
		t.Fatal("a fresh snapshot must not request a refresh")
	}

	recalculated := recent
	recalculated.DecisionReason = "capacity_healthy"
	recalculated.SnapshotEvidencePartial = true
	if smartInspectionSnapshotRefreshNeeded(recalculated) {
		t.Fatal("a recalculated recent partial snapshot must retain the deferred refresh state")
	}
}

func TestCurrentSmartResourceRecalculatesHealthForUpdatedWaterLevel(t *testing.T) {
	service := New(nil, nil)
	service.setSmartResource(SmartResource{
		Enabled:             true,
		SnapshotFresh:       true,
		GeneratedAtMS:       time.Now().UnixMilli(),
		HealthLevel:         smartHealthCritical,
		SuggestedAction:     smartActionTakeLocked,
		DecisionReason:      "capacity_critical",
		CurrentCapacityRCU:  6600,
		ConsumeRCUPerMinute: 100,
		UnitCapacityRCU:     40,
	})

	resource := service.currentSmartResource(store.ManagerSupplyConfig{
		Product:              "oauth_7d",
		HealthyMinutesTarget: 40,
		WarningMinutes:       15,
		CriticalMinutes:      10,
		PrelockMinQuantity:   1,
		PrelockMaxQuantity:   10,
	})
	if resource.EstimatedSustainMinutes != 66 || resource.HealthLevel != smartHealthHealthy || resource.SuggestedAction != smartActionHealthy || resource.SuggestedQuantity != 0 {
		t.Fatalf("updated water level must recompute a cached capacity state: %#v", resource)
	}
	if resource.TargetCapacityRCU != 4000 || resource.CapacityGapRCU != 0 {
		t.Fatalf("updated target capacity = %#v", resource)
	}
}

func TestSmartResourceCapacityOnlyCountsNonDisabledUsableCredentials(t *testing.T) {
	service := New(nil, nil)
	now := time.Now()

	resource := service.buildSmartResourceFromSnapshots(store.ManagerSupplyConfig{
		Product:              "oauth_7d",
		HealthyMinutesTarget: 30,
		PrelockMinQuantity:   1,
		PrelockMaxQuantity:   10,
		NewAccountConfidence: 0.7,
	}, authFileSnapshot{
		generatedAt: now,
		files: []cpaauthfiles.File{
			{
				Name:     "usable.json",
				Provider: "codex",
				Raw: map[string]any{
					"status":        "ready",
					"remaining_rcu": 100,
					"recent_requests": []any{
						map[string]any{"success": 12, "failed": 0},
					},
				},
			},
			{
				Name:     "disabled-field.json",
				Provider: "codex",
				Disabled: true,
				Raw: map[string]any{
					"status":        "ready",
					"remaining_rcu": 1000,
				},
			},
			{
				Name:     "disabled-status.json",
				Provider: "codex",
				Raw: map[string]any{
					"status":        "disabled",
					"remaining_rcu": 1000,
				},
			},
			{
				Name:     "quota-exhausted.json",
				Provider: "codex",
				Raw: map[string]any{
					"status":         "error",
					"status_message": `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`,
					"remaining_rcu":  1000,
				},
			},
			{
				Name:     "invalidated.json",
				Provider: "codex",
				Raw: map[string]any{
					"status":         "error",
					"status_message": "credential invalidated",
					"remaining_rcu":  1000,
				},
			},
		},
	}, now)

	if resource.SchedulableAccounts != 1 || resource.HealthyAccounts != 1 || resource.WeakAccounts != 0 {
		t.Fatalf("only one usable non-disabled credential should be counted: %#v", resource)
	}
	if resource.AvailableAccounts != 1 || resource.RawCapacityRCU != 100 || resource.CurrentCapacityRCU != 100 {
		t.Fatalf("effective capacity should exclude disabled/exhausted credentials: %#v", resource)
	}
}

func TestSmartResourceUsesLifetimeCapacityForFallbackAccounts(t *testing.T) {
	service := New(nil, nil)
	now := time.Now()
	events := make([]usage.Event, 0, 30)
	for minute := 0; minute < 30; minute++ {
		events = append(events, usage.Event{
			TimestampMS: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(),
			Provider:    "codex",
			AuthIndex:   "steady-source",
			TotalTokens: 100,
		})
	}
	service.recordSmartUsageEvents(events, now)

	files := make([]cpaauthfiles.File, 0, 10)
	for index := 0; index < 10; index++ {
		files = append(files, cpaauthfiles.File{
			Name:     "fallback.json",
			Provider: "codex",
			Raw: map[string]any{
				"status":            "ready",
				"remaining_seconds": 3600,
			},
		})
	}

	resource := service.buildSmartResourceFromSnapshots(store.ManagerSupplyConfig{
		Product:              "oauth_7d",
		HealthyMinutesTarget: 20,
		WarningMinutes:       10,
		CriticalMinutes:      5,
		PrelockMinQuantity:   1,
		PrelockMaxQuantity:   30,
		NewAccountConfidence: 0.7,
	}, authFileSnapshot{generatedAt: now, files: files}, now)

	if resource.RawCapacityRCU != 2500 || resource.RawCapacityTokenM != 100 {
		t.Fatalf("fallback capacity should use the conservative 10M quota per account, got %#v", resource)
	}
	if resource.HealthLevel != smartHealthHealthy || resource.SuggestedQuantity != 0 {
		t.Fatalf("steady low burn should not recommend excessive replenishment, got %#v", resource)
	}
}

func TestSmartResourceLimitsCapacityByOneHourExpiry(t *testing.T) {
	service := New(nil, nil)
	now := time.Now()
	events := make([]usage.Event, 0, 30)
	for minute := 0; minute < 30; minute++ {
		events = append(events, usage.Event{
			TimestampMS: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(),
			Provider:    "codex",
			AuthIndex:   "slow-source",
			TotalTokens: 100,
		})
	}
	service.recordSmartUsageEvents(events, now)

	files := make([]cpaauthfiles.File, 0, 10)
	for index := 0; index < 10; index++ {
		files = append(files, cpaauthfiles.File{
			Name:     "capacity.json",
			Provider: "codex",
			Raw: map[string]any{
				"status":            "ready",
				"remaining_rcu":     80,
				"remaining_seconds": 3600,
				"recent_requests": []any{
					map[string]any{"success": 12, "failed": 0},
				},
			},
		})
	}

	resource := service.buildSmartResourceFromSnapshots(store.ManagerSupplyConfig{
		Product:              "oauth_30d",
		HealthyMinutesTarget: 120,
		WarningMinutes:       60,
		CriticalMinutes:      30,
		PrelockMinQuantity:   1,
		PrelockMaxQuantity:   10,
		NewAccountConfidence: 0.7,
	}, authFileSnapshot{generatedAt: now, files: files}, now)

	if resource.RawCapacityRCU != 800 {
		t.Fatalf("raw capacity = %#v", resource)
	}
	if resource.CurrentCapacityRCU != 60 {
		t.Fatalf("capacity should be limited by one-hour burn window, got %#v", resource)
	}
	if resource.ExpiryWasteRiskRCU != 740 {
		t.Fatalf("waste risk should report capacity that cannot be consumed before expiry, got %#v", resource)
	}
	if resource.EffectiveHealthyMinutes != 120 || resource.TargetCapacityRCU != 120 {
		t.Fatalf("configured healthy target must remain effective, got %#v", resource)
	}
	if resource.HealthLevel != smartHealthWarning || resource.EmergencyShortage || resource.SuggestedQuantity != 1 {
		t.Fatalf("one-hour credential expiry must trigger a paced refill toward the 120-minute target, got %#v", resource)
	}
}

func TestSmartResourceReportsExpiringAccountCapacity(t *testing.T) {
	service := New(nil, nil)
	now := time.Now()
	resource := service.buildSmartResourceFromSnapshots(store.ManagerSupplyConfig{
		Product:              "oauth_30d",
		HealthyMinutesTarget: 40,
		WarningMinutes:       20,
		CriticalMinutes:      10,
	}, authFileSnapshot{generatedAt: now, files: []cpaauthfiles.File{{
		Name: "expires-soon.json", Provider: "codex", Raw: map[string]any{
			"status": "ready", "remaining_rcu": 100,
			"supply_lease_expires_at_ms": now.Add(12 * time.Minute).UnixMilli(),
		},
	}}}, now)
	if resource.ExpiringAccounts != 1 || resource.ExpiringWithinMinutes < 11 ||
		resource.ExpiringWithinMinutes > 13 || resource.ExpiringCapacityRCU != 100 {
		t.Fatalf("expiring account metrics = %#v", resource)
	}
}

func TestApplySmartExpiryCapacityReportsEventTimeline(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	resource := SmartResource{
		GeneratedAtMS:              now.UnixMilli(),
		UnitCapacityRCU:            1,
		RawCapacityRCU:             200,
		ConsumeRCUPerMinute:        2,
		DemandPlanningRCUPerMinute: 2,
	}
	items := []smartCapacityItem{
		{capacityRCU: 100, remainingMinutes: 10, expiresAtMS: now.Add(10 * time.Minute).UnixMilli()},
		{capacityRCU: 100, remainingMinutes: 60, expiresAtMS: now.Add(60 * time.Minute).UnixMilli()},
	}
	applySmartExpiryCapacity(&resource, items, 2, now)
	applySmartTokenMetrics(&resource)

	if resource.TimeLimitedCapacityRCU != 120 || resource.ExpiryWasteRiskRCU != 80 || resource.CurrentCapacityRCU != 120 {
		t.Fatalf("expiry-limited capacity = %#v", resource)
	}
	if resource.RawSustainMinutes != 100 || resource.ExpiryLimitedSustainMinutes != 60 || resource.ForecastSustainMinutes != 60 {
		t.Fatalf("runway timeline = %#v", resource)
	}
	if resource.NearestExpiryAtMS != now.Add(10*time.Minute).UnixMilli() || resource.NearestExpiryMinutes != 10 {
		t.Fatalf("nearest expiry = %#v", resource)
	}
	if resource.NextCapacityDeficitAtMS != now.Add(60*time.Minute).UnixMilli() {
		t.Fatalf("next capacity deficit = %d, want %d", resource.NextCapacityDeficitAtMS, now.Add(60*time.Minute).UnixMilli())
	}
}

func TestEqualExpiryCapacitySplitsProportionallyAcrossAccountStates(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	resource := SmartResource{RawCapacityRCU: 400, UnitCapacityRCU: 1}
	items := []smartCapacityItem{
		{
			credentialKey: operatorCredentialKey("normal.json", "normal"),
			capacityRCU:   100, remainingMinutes: 10,
		},
		{
			credentialKey: operatorCredentialKey("frozen.json", "frozen"),
			capacityRCU:   300, remainingMinutes: 10,
		},
	}
	resource.capacityItems = items
	applySmartExpiryCapacity(&resource, resource.capacityItems, 20, now)
	applyAccountPoolStats(&resource, accountPoolStats{
		total: 2, enabled: 2, schedulable: 2, normal: 1, needsAttention: 1,
		classificationObserved: true, liveObserved: true,
		bucketByCredential: map[string]operatorAccountBucket{
			operatorCredentialKey("normal.json", "normal"): operatorAccountNormal,
			operatorCredentialKey("frozen.json", "frozen"): operatorAccountNeedsAttention,
		},
	})
	if resource.TotalCapacityRCU != 200 || resource.AvailableCapacityRCU != 50 || resource.FrozenCapacityRCU != 150 {
		t.Fatalf("equal-expiry proportional split = %#v", resource)
	}
}

func TestStrongSupplyCreatesMinimumEmergencyOrderWhenPoolIsEmptyAndUsageRateNotReady(t *testing.T) {
	var createCalls atomic.Int32
	var createQuantity atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":10,"estimated_total_fen":100}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":10000}`))
		case r.URL.Path == "/v0/management/auth-files":
			_, _ = w.Write([]byte(`{"files":[]}`))
		case r.URL.Path == "/api/customer/pickup/orders":
			createCalls.Add(1)
			var payload struct {
				Quantity int `json:"quantity"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode create payload: %v", err)
			}
			createQuantity.Store(int32(payload.Quantity))
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"order":{"id":"order-vacuum","status":"waiting_inventory","quantity":5},"status_url":"/api/customer/pickup/orders/order-vacuum"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "smart-no-usage.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, BaseURL: server.URL, Username: "customer", Password: "password",
			Product: "oauth_30d", TargetAvailableAccounts: 100, ReplenishBatchSize: 5,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	seedCompletedQuotaInspection(t, st, quotaInspectionResult(0))
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())

	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("run automatic: %v", err)
	}
	if createCalls.Load() != 1 || createQuantity.Load() != 2 {
		t.Fatalf("create calls/quantity = %d/%d", createCalls.Load(), createQuantity.Load())
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status.ActiveOrder == nil || status.SmartResource.DecisionReason != "emergency_pool_vacuum" || !status.SmartResource.PoolVacuumActive {
		t.Fatalf("status = %#v", status)
	}
}

func TestStrongSupplySkipsCreateWithoutUsageAboveCriticalWaterline(t *testing.T) {
	var createCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files":
			_, _ = w.Write([]byte(`{"files":[{"name":"a.json","provider":"codex"},{"name":"b.json","provider":"codex"},{"name":"c.json","provider":"codex"}]}`))
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":10,"estimated_total_fen":100}`))
		case "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":10000}`))
		case "/api/customer/pickup/orders":
			createCalls.Add(1)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "smart-no-usage-above-waterline.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, BaseURL: server.URL, Username: "customer", Password: "password",
			Product: "oauth_30d", TargetAvailableAccounts: 100, ReplenishBatchSize: 5,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	seedCompletedQuotaInspection(t, st,
		store.CodexInspectionResult{FileName: "a.json", UsedPercent: floatPtr(0)},
		store.CodexInspectionResult{FileName: "b.json", UsedPercent: floatPtr(0)},
		store.CodexInspectionResult{FileName: "c.json", UsedPercent: floatPtr(0)},
	)
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())

	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("run automatic: %v", err)
	}
	if createCalls.Load() != 0 {
		t.Fatalf("create calls = %d", createCalls.Load())
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status.ActiveOrder != nil || status.SmartResource.DecisionReason != "usage_rate_not_ready" {
		t.Fatalf("status = %#v", status)
	}
}

func TestStartupAccountFloorCreatesWithoutUsageAboveCriticalWaterline(t *testing.T) {
	var createCalls atomic.Int32
	var createQuantity atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files":
			_, _ = w.Write([]byte(`{"files":[{"name":"a.json","provider":"codex"},{"name":"b.json","provider":"codex"},{"name":"c.json","provider":"codex"}]}`))
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":10,"estimated_total_fen":100}`))
		case "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":10000}`))
		case "/api/customer/pickup/orders":
			createCalls.Add(1)
			var payload struct {
				Quantity int `json:"quantity"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode create payload: %v", err)
			}
			createQuantity.Store(int32(payload.Quantity))
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"order":{"id":"order-startup-floor","status":"waiting_inventory","quantity":2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "smart-startup-floor.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	startupAccounts := 5
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, BaseURL: server.URL, Username: "customer", Password: "password",
			Product: "oauth_30d", TargetAvailableAccounts: 100, ReplenishBatchSize: 5,
			Strategy: managerconfigsvc.SupplyStrategyStrongSupply, CriticalAvailableAccounts: 2,
			StartupAvailableAccounts: &startupAccounts,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	seedCompletedQuotaInspection(t, st,
		store.CodexInspectionResult{FileName: "a.json", UsedPercent: floatPtr(0)},
		store.CodexInspectionResult{FileName: "b.json", UsedPercent: floatPtr(0)},
		store.CodexInspectionResult{FileName: "c.json", UsedPercent: floatPtr(0)},
	)
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())

	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("run automatic: %v", err)
	}
	if createCalls.Load() != 1 || createQuantity.Load() != 1 {
		t.Fatalf("create calls/quantity = %d/%d, want 1/1", createCalls.Load(), createQuantity.Load())
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status.ActiveOrder == nil || status.SmartResource.DecisionReason != "startup_account_floor" {
		t.Fatalf("status = %#v", status)
	}
}

func TestCostFirstDoesNotCreateCapacityOrderFromIdleDemandMemory(t *testing.T) {
	var createCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":100,"missing":0,"needs_production":false,"estimated_total_fen":100}`))
		case "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":100000,"balance_fen":100000}`))
		case "/v0/management/auth-files":
			_, _ = w.Write([]byte(`{"files":[
				{"name":"ready-a.json","provider":"codex","status":"ready"},
				{"name":"ready-b.json","provider":"codex","status":"ready"}
			]}`))
		case "/api/customer/pickup/orders":
			createCalls.Add(1)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "idle-demand-memory.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_7d",
			Strategy: managerconfigsvc.SupplyStrategyCostFirst, CriticalAvailableAccounts: 0,
			HealthyAvailableAccounts: 2, DefaultEmergencyMinAccounts: 1, VirtualDemandTTLMinutes: 15,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	seedCompletedQuotaInspection(t, st, quotaInspectionResult(0), quotaInspectionResult(0))
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	now := time.Now().Truncate(time.Minute)
	service.recordSmartUsageEvents([]usage.Event{{
		TimestampMS: now.Add(-10 * time.Minute).UnixMilli(), Provider: "codex", AuthIndex: "old-load", TotalTokens: 4_000_000,
	}}, now)

	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("run automatic: %v", err)
	}
	if createCalls.Load() != 0 {
		t.Fatalf("idle demand memory created %d capacity orders", createCalls.Load())
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status.SmartResource.ConsumeRCUPerMinute != 0 || status.SmartResource.VirtualDemandRCUPerMinute <= 0 ||
		status.SmartResource.SuggestedQuantity != 0 || status.ActiveOrder != nil {
		t.Fatalf("idle demand memory status = %#v", status.SmartResource)
	}
}

func TestSupplyStrategyVirtualDemandMemoryUsesConfiguredTTL(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Minute)
	service.recordSmartUsageEvents([]usage.Event{{
		TimestampMS: now.Add(-40 * time.Minute).UnixMilli(),
		Provider:    "codex",
		AuthIndex:   "recent-demand.json",
		TotalTokens: 80_000,
	}}, now)

	strong := defaultSmartResource(store.ManagerSupplyConfig{
		Product:                     "oauth_30d",
		Strategy:                    managerconfigsvc.SupplyStrategyStrongSupply,
		VirtualDemandTTLMinutes:     60,
		EmergencyBypassUsageRate:    managerconfigsvc.BoolPtr(true),
		AccountMaxRequestsBefore401: 30,
		AccountMaxUsefulSeconds401:  120,
	})
	if got := service.applySmartDemandMemory(store.ManagerSupplyConfig{
		Product:                  "oauth_30d",
		Strategy:                 managerconfigsvc.SupplyStrategyStrongSupply,
		VirtualDemandTTLMinutes:  60,
		EmergencyBypassUsageRate: managerconfigsvc.BoolPtr(true),
	}, &strong, now, 0); got != 0 || strong.ConsumeRCUPerMinute != 0 || strong.VirtualDemandRCUPerMinute != 1 ||
		strong.DemandPlanningRCUPerMinute != 1 || strong.DemandTrend != smartDemandTrendVirtual || strong.DecisionReason != "virtual_demand_memory" {
		t.Fatalf("strong supply demand memory = %f, resource=%#v", got, strong)
	}

	balanced := defaultSmartResource(store.ManagerSupplyConfig{
		Product:                  "oauth_30d",
		Strategy:                 managerconfigsvc.SupplyStrategyBalanced,
		VirtualDemandTTLMinutes:  30,
		EmergencyBypassUsageRate: managerconfigsvc.BoolPtr(true),
	})
	if got := service.applySmartDemandMemory(store.ManagerSupplyConfig{
		Product:                  "oauth_30d",
		Strategy:                 managerconfigsvc.SupplyStrategyBalanced,
		VirtualDemandTTLMinutes:  30,
		EmergencyBypassUsageRate: managerconfigsvc.BoolPtr(true),
	}, &balanced, now, 0); got != 0 || balanced.VirtualDemandRCUPerMinute != 0 {
		t.Fatalf("balanced demand memory should expire after 30 minutes: %f, resource=%#v", got, balanced)
	}
}

func TestSupplyOrderTriggerReasonPreservesWaterlineAndVirtualDemandReasons(t *testing.T) {
	if got := supplyOrderTriggerReason(SmartResource{
		DecisionReason:            "emergency_capacity_shortage",
		VirtualDemandRCUPerMinute: 2,
		DemandTrend:               smartDemandTrendVirtual,
	}, true); got != "virtual_demand_memory" {
		t.Fatalf("virtual demand trigger reason = %q", got)
	}
	if got := supplyOrderTriggerReason(SmartResource{
		DecisionReason:            "emergency_capacity_shortage",
		EmergencyReason:           "emergency_pool_vacuum",
		VirtualDemandRCUPerMinute: 2,
		DemandTrend:               smartDemandTrendVirtual,
	}, true); got != "emergency_pool_vacuum" {
		t.Fatalf("waterline trigger reason = %q", got)
	}
	if got := supplyOrderTriggerReason(SmartResource{}, false); got != "manual" {
		t.Fatalf("manual trigger reason = %q", got)
	}
}

func Test401ChurnThresholdsDoNotChangeQuotaCapacityOrPoolWaterline(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Product:                     "oauth_30d",
		Strategy:                    managerconfigsvc.SupplyStrategyStrongSupply,
		AccountMaxRequestsBefore401: 30,
		AccountMaxUsefulSeconds401:  120,
		NewAccountConfidence:        0.7,
	}
	resource := defaultSmartResource(cfg)
	if got := smartEstimatedNewAccountCapacityForResource(cfg, resource); got != 87.5 {
		t.Fatalf("new-account conservative quota capacity = %f", got)
	}
	otherThresholds := cfg
	otherThresholds.AccountMaxRequestsBefore401 = 50
	otherThresholds.AccountMaxUsefulSeconds401 = 180
	if got := smartEstimatedNewAccountCapacityForResource(otherThresholds, defaultSmartResource(otherThresholds)); got != 87.5 {
		t.Fatalf("401 warning thresholds must not change quota capacity, got %f", got)
	}
	if got := smartEffectiveHealthyMinutesTarget(cfg); got != 120 {
		t.Fatalf("pool-wide healthy runway = %d, want 120", got)
	}
	if got := smartEffectiveHealthyMinutesTarget(otherThresholds); got != 120 {
		t.Fatalf("401 warning thresholds must not change pool waterline, got %d", got)
	}
}

func TestConfiguredWaterlinesRemainExactAtRuntime(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		HealthyMinutesTarget: 120,
		WarningMinutes:       100,
		CriticalMinutes:      80,
	}
	resource := defaultSmartResource(cfg)
	if resource.ConfiguredHealthyMinutes != 120 || resource.EffectiveHealthyMinutes != 120 ||
		resource.HealthyMinutesTarget != 120 || resource.WarningMinutes != 100 || resource.CriticalMinutes != 80 {
		t.Fatalf("configured/runtime waterlines diverged: %#v", resource)
	}
	if resource.AccountLifetimeMinutes != 120 {
		t.Fatalf("no-expiry planning horizon = %d, want 120", resource.AccountLifetimeMinutes)
	}
}

func TestExtendedConfiguredWaterlinesRecoverProgressively(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Product:               "oauth_30d",
		HealthyMinutesTarget:  120,
		WarningMinutes:        100,
		CriticalMinutes:       80,
		PrelockMinQuantity:    1,
		PrelockMaxQuantity:    16,
		ReplenishBatchSize:    26,
		CreateCooldownSeconds: 120,
		NewAccountConfidence:  0.7,
	}
	resource := defaultSmartResource(cfg)
	resource.SnapshotFresh = true
	resource.CurrentCapacityRCU = 240
	resource.AvailableCapacityRCU = 240
	resource.AvailableAccounts = 5
	resource.ConsumeRCUPerMinute = 10
	resource.DemandPlanningRCUPerMinute = 10
	resource.DemandTrend = smartDemandTrendStable

	recalculateSmartResourceCapacityPlan(cfg, &resource)
	if resource.HealthLevel != smartHealthCritical || resource.EmergencyShortage ||
		resource.SuggestedAction != smartActionTakeLocked || resource.SuggestedQuantity <= 1 {
		t.Fatalf("extended critical waterline must stay visible without burst mode: %#v", resource)
	}
	if smartAvailableCapacityEmergency(cfg, resource) {
		t.Fatalf("configured 80-minute critical line must not bypass progressive recovery at 24 minutes: %#v", resource)
	}
	quantity, reason, timing := smartPrelockQuantityForSupplyPressureWithTiming(
		cfg,
		resource,
		smartSupplyPressure{level: smartSupplyPressurePlenty},
		resource.SuggestedQuantity,
	)
	if quantity != 1 || reason != "low_water_staged_batch" || timing.eligibleQuantity != 1 {
		t.Fatalf("extended waterline refill = %d/%q %#v, want one staged account", quantity, reason, timing)
	}
	if cooldown := smartSuccessfulOrderCooldownForResource(cfg, resource); cooldown < 120 {
		t.Fatalf("successful progressive cooldown = %d, want at least 120", cooldown)
	}

	resource.EstimatedSustainMinutes = 12
	resource.AvailableCapacityRCU = 120
	resource.CapacityGapRCU = 1
	if !smartEmergencyShortage(resource) || !smartAvailableCapacityEmergency(cfg, resource) {
		t.Fatalf("twelve-minute rescue runway must still bypass pacing: %#v", resource)
	}
}

func TestInspectionCapacityUsesConfiguredHorizonUnlessUpstreamExpiresEarlier(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Second)
	events := make([]usage.Event, 0, 30)
	for minute := 0; minute < 30; minute++ {
		events = append(events, usage.Event{
			TimestampMS: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(),
			Provider:    "codex",
			AuthIndex:   "waterline-account",
			TotalTokens: 100,
		})
	}
	service.recordSmartUsageEvents(events, now)
	unused := 0.0
	cfg := store.ManagerSupplyConfig{
		Product:              "oauth_30d",
		HealthyMinutesTarget: 120,
		WarningMinutes:       100,
		CriticalMinutes:      80,
	}
	snapshot := inspectionQuotaSnapshot{
		run: store.CodexInspectionRun{
			ProbeSetCount: 1,
			SampledCount:  1,
			StartedAtMS:   now.Add(-time.Minute).UnixMilli(),
			FinishedAtMS:  now.UnixMilli(),
		},
		results: []store.CodexInspectionResult{{
			AccountKey: "waterline-account", FileName: "waterline.json", Provider: "codex",
			Status: "active", Action: "keep", PlanType: "team", UsedPercent: &unused,
		}},
		generatedAt: now,
	}

	withoutExpiry := service.buildSmartResourceFromInspectionSnapshot(cfg, snapshot, now)
	if withoutExpiry.CurrentCapacityRCU != 120 || withoutExpiry.ExpiryLimitedSustainMinutes != 120 {
		t.Fatalf("no-expiry capacity must cover the configured horizon: %#v", withoutExpiry)
	}

	snapshot.accountExpiresByFile = map[string]int64{
		"waterline.json": now.Add(30 * time.Minute).UnixMilli(),
	}
	withExpiry := service.buildSmartResourceFromInspectionSnapshot(cfg, snapshot, now)
	if withExpiry.CurrentCapacityRCU != 30 || withExpiry.ExpiryLimitedSustainMinutes != 30 ||
		withExpiry.NearestExpiryAtMS != snapshot.accountExpiresByFile["waterline.json"] {
		t.Fatalf("real upstream expiry must cap credential capacity: %#v", withExpiry)
	}
}

func TestMillionTokenRunwayUsesSameCapacityAndDemandDimension(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Product:              "oauth_7d",
		HealthyMinutesTarget: 55,
		WarningMinutes:       27,
		CriticalMinutes:      13,
		PrelockMinQuantity:   1,
		PrelockMaxQuantity:   100,
		ReplenishBatchSize:   100,
		NewAccountConfidence: 0.7,
	}
	resource := defaultSmartResource(cfg)
	resource.CurrentCapacityRCU = smartTokenMillionToRCU(57.2, resource.UnitCapacityRCU)
	resource.RawCapacityRCU = resource.CurrentCapacityRCU
	resource.TimeLimitedCapacityRCU = resource.CurrentCapacityRCU
	resource.ConsumeRCUPerMinute = smartTokenMillionToRCU(7.94, resource.UnitCapacityRCU)
	resource.DemandPlanningRCUPerMinute = resource.ConsumeRCUPerMinute
	resource.DemandTrend = smartDemandTrendStable

	recalculateSmartResourceCapacityPlan(cfg, &resource)

	if resource.CurrentCapacityTokenM != 57.2 || resource.ConsumeTokenMPerMinute != 7.94 ||
		resource.EstimatedSustainMinutes != 7.2 {
		t.Fatalf("million-token runway mismatch: %#v", resource)
	}
	if resource.HealthLevel != smartHealthCritical || !resource.EmergencyShortage || resource.CapacityGapTokenM <= 0 {
		t.Fatalf("7.2-minute runway must be a visible capacity emergency: %#v", resource)
	}
}

func TestLowQuotaRunwayStagesSmallRefillInsteadOfReportingTwoMinuteHealthy(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Product:                     "oauth_7d",
		Strategy:                    managerconfigsvc.SupplyStrategyCustom,
		HealthyMinutesTarget:        60,
		WarningMinutes:              30,
		CriticalMinutes:             20,
		CriticalAvailableAccounts:   5,
		HealthyAvailableAccounts:    10,
		AccountMaxRequestsBefore401: 30,
		AccountMaxUsefulSeconds401:  120,
		PrelockMinQuantity:          1,
		PrelockMaxQuantity:          20,
		ReplenishBatchSize:          50,
		NewAccountConfidence:        0.7,
	}
	resource := defaultSmartResource(cfg)
	resource.AvailableAccounts = 9
	resource.HealthyAccounts = 9
	resource.CurrentCapacityRCU = 5_000
	resource.ConsumeRCUPerMinute = 1_000
	resource.DemandTrend = smartDemandTrendRising

	recalculateSmartResourceCapacityPlan(cfg, &resource)

	if resource.EffectiveHealthyMinutes != 60 || resource.WarningMinutes != 30 || resource.CriticalMinutes != 20 {
		t.Fatalf("pool waterlines = effective:%d warning:%d critical:%d, want 60/30/20", resource.EffectiveHealthyMinutes, resource.WarningMinutes, resource.CriticalMinutes)
	}
	if resource.EstimatedSustainMinutes != 5 || resource.TargetCapacityRCU != 60_000 || resource.CapacityGapRCU != 55_000 {
		t.Fatalf("capacity plan = %#v", resource)
	}
	if resource.HealthLevel != smartHealthCritical || !resource.EmergencyShortage || resource.SuggestedQuantity != 20 {
		t.Fatalf("low quota must fill the configured emergency batch toward healthy: %#v", resource)
	}
	if smartAccountAvailabilityEmergency(resource) {
		t.Fatalf("nine available accounts must not be treated as account-count emergency: %#v", resource)
	}
	if got := New(nil, nil).smartSuggestedCreateQuantity(cfg, resource); got != 20 {
		t.Fatalf("healthy refill create quantity=%d, want 20", got)
	}
}

func Test401AgeDoesNotEraseVerifiedAccountQuotaCapacity(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	unused := 0.0
	resource := New(nil, nil).buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{
		Product:                     "oauth_7d",
		Strategy:                    managerconfigsvc.SupplyStrategyCostFirst,
		AccountMaxRequestsBefore401: 50,
		AccountMaxUsefulSeconds401:  180,
	}, inspectionQuotaSnapshot{
		run: store.CodexInspectionRun{
			ProbeSetCount: 1,
			SampledCount:  1,
			StartedAtMS:   now.Add(-10 * time.Minute).UnixMilli(),
			FinishedAtMS:  now.UnixMilli(),
		},
		results: []store.CodexInspectionResult{{
			AccountKey: "aged-supply", FileName: "codex-supply-aged.json", Provider: "codex",
			Status: "active", UsedPercent: &unused,
		}},
		activeImportItems: []store.SupplyImportItem{{
			FileName:         "codex-supply-aged.json",
			Status:           "imported",
			ImportedAtMS:     now.Add(-5 * time.Minute).UnixMilli(),
			LeaseExpiresAtMS: now.Add(30 * time.Minute).UnixMilli(),
		}},
		generatedAt: now,
	}, now)

	if resource.AvailableAccounts != 1 || resource.HealthyAccounts != 1 || resource.WeakAccounts != 0 {
		t.Fatalf("401 churn warning must not change account health counts: %#v", resource)
	}
	if resource.RawCapacityRCU != 250 || resource.CurrentCapacityRCU != 250 ||
		resource.RawCapacityTokenM != 10 || resource.CurrentCapacityTokenM != 10 {
		t.Fatalf("a fresh successful inspection must retain verified quota capacity: %#v", resource)
	}
}

func TestStrongSupplyUsesQuotaCapacityForPlanning(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Product:                     "oauth_7d",
		Strategy:                    managerconfigsvc.SupplyStrategyStrongSupply,
		HealthyMinutesTarget:        60,
		WarningMinutes:              30,
		CriticalMinutes:             20,
		HealthyAvailableAccounts:    10,
		CriticalAvailableAccounts:   2,
		AccountMaxRequestsBefore401: 30,
		AccountMaxUsefulSeconds401:  120,
		NewAccountConfidence:        0.7,
	}
	resource := defaultSmartResource(cfg)
	resource.AvailableAccounts = 21
	resource.SchedulableAccounts = 21
	resource.HealthyAccounts = 21
	resource.RequestDemandRCUPerMinute = 20
	resource.ConsumeRCUPerMinute = 42.5
	unit := smartEstimatedNewAccountCapacityForResource(cfg, resource)
	resource.RiskAdjustedUnitCapacityRCU = unit
	resource.CurrentCapacityRCU = float64(resource.AvailableAccounts) * unit
	resource.DemandTrend = smartDemandTrendStable

	recalculateSmartResourceCapacityPlan(cfg, &resource)

	if resource.EffectiveHealthyMinutes != 60 || resource.EstimatedSustainMinutes <= 60 ||
		resource.HealthLevel != smartHealthHealthy ||
		resource.SuggestedQuantity != 0 || resource.AccountQuantityDeficit != 0 ||
		resource.EstimatedRequiredAccounts != 21 {
		t.Fatalf("21-account reliable capacity plan = %#v", resource)
	}

	resource.AvailableAccounts = 5
	resource.SchedulableAccounts = 5
	resource.HealthyAccounts = 5
	resource.CurrentCapacityRCU = 5 * unit
	recalculateSmartResourceCapacityPlan(cfg, &resource)
	if resource.HealthLevel != smartHealthCritical || resource.AccountQuantityDeficit != 10 || resource.SuggestedQuantity != 10 {
		t.Fatalf("five-account quota capacity plan = %#v", resource)
	}
	if quantity := New(nil, nil).smartSuggestedCreateQuantity(cfg, resource); quantity != 10 {
		t.Fatalf("five-account quota shortage should refill to healthy, got %d", quantity)
	}
}

func TestStrongSupplyRunwayUsesActualPoolQuotaInsteadOfRequestThreshold(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Product:                     "oauth_30d",
		Strategy:                    managerconfigsvc.SupplyStrategyStrongSupply,
		HealthyMinutesTarget:        60,
		HealthyAvailableAccounts:    10,
		AccountMaxRequestsBefore401: 30,
		AccountMaxUsefulSeconds401:  120,
		NewAccountConfidence:        0.7,
	}
	resource := defaultSmartResource(cfg)
	resource.AvailableAccounts = 38
	resource.SchedulableAccounts = 38
	resource.HealthyAccounts = 38
	resource.RawCapacityRCU = smartTokenMillionToRCU(57.2, resource.UnitCapacityRCU)
	resource.CurrentCapacityRCU = resource.RawCapacityRCU
	resource.TimeLimitedCapacityRCU = resource.RawCapacityRCU
	resource.ConsumeRCUPerMinute = smartTokenMillionToRCU(7.94, resource.UnitCapacityRCU)
	resource.RequestDemandRCUPerMinute = 50
	resource.DemandTrend = smartDemandTrendStable

	recalculateSmartResourceCapacityPlan(cfg, &resource)

	if resource.CurrentCapacityTokenM != 57.2 || resource.ConsumeTokenMPerMinute != 7.94 || resource.EstimatedSustainMinutes != 7.2 {
		t.Fatalf("actual quota runway was altered by 401 thresholds: %#v", resource)
	}
	if resource.HealthLevel != smartHealthCritical || resource.SuggestedQuantity != 10 ||
		resource.EstimatedRequiredAccounts != 98 || resource.AccountQuantityDeficit != 60 {
		t.Fatalf("38-account quota plan = %#v", resource)
	}
}

func TestSmartAutomaticDoesNotCreateWhenCapacityHealthy(t *testing.T) {
	var createCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":10,"estimated_total_fen":100}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":10000}`))
		case r.URL.Path == "/v0/management/auth-files":
			_, _ = w.Write([]byte(`{"files":[{"name":"a.json","provider":"codex"},{"name":"b.json","provider":"codex"},{"name":"c.json","provider":"codex"},{"name":"d.json","provider":"codex"}]}`))
		case r.URL.Path == "/api/customer/pickup/orders":
			createCalls.Add(1)
			t.Fatal("healthy smart capacity should not create an order")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "smart-healthy.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, BaseURL: server.URL, Username: "customer", Password: "password",
			Product: "oauth_30d", TargetAvailableAccounts: 1, ReplenishBatchSize: 5,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	seedCompletedQuotaInspection(t, st,
		quotaInspectionResult(0),
		quotaInspectionResult(0),
		quotaInspectionResult(0),
		quotaInspectionResult(0),
	)
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	now := time.Now()
	for minute := 0; minute < 30; minute++ {
		service.recordSmartUsageEvents([]usage.Event{{
			TimestampMS: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(),
			Provider:    "codex",
			AuthIndex:   "a.json",
			TotalTokens: 10,
		}}, now)
	}

	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("run automatic: %v", err)
	}
	if createCalls.Load() != 0 {
		t.Fatalf("create calls = %d", createCalls.Load())
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status.SmartResource.HealthLevel != smartHealthHealthy || status.ActiveOrder != nil {
		t.Fatalf("status = %#v", status)
	}
}

func TestSmartAutomaticPurchaseTimingWaitDoesNotCreateOrder(t *testing.T) {
	var createCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":100,"missing":0,"needs_production":false,"estimated_total_fen":100}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":100000,"balance_fen":100000}`))
		case r.URL.Path == "/v0/management/auth-files":
			_, _ = w.Write([]byte(`{"files":[{"name":"quota-0.json","provider":"codex"},{"name":"quota-1.json","provider":"codex"},{"name":"quota-2.json","provider":"codex"},{"name":"quota-3.json","provider":"codex"},{"name":"quota-4.json","provider":"codex"},{"name":"quota-5.json","provider":"codex"},{"name":"quota-6.json","provider":"codex"},{"name":"quota-7.json","provider":"codex"},{"name":"quota-8.json","provider":"codex"},{"name":"quota-9.json","provider":"codex"}]}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			createCalls.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"order":{"id":"unexpected-early-order","status":"waiting_inventory","quantity":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "smart-purchase-timing-wait.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, BaseURL: server.URL, Username: "customer", Password: "password",
			Product: "oauth_30d", TargetAvailableAccounts: 20, ReplenishBatchSize: 10,
			PrelockMinQuantity: 1, PrelockMaxQuantity: 10,
			HealthyMinutesTarget: 55, WarningMinutes: 40, CriticalMinutes: 25,
			CriticalAvailableAccounts: 1, HealthyAvailableAccounts: 3,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	results := make([]store.CodexInspectionResult, 0, 10)
	for index := 0; index < 10; index++ {
		result := quotaInspectionResult(0)
		result.AccountKey = fmt.Sprintf("quota-%d", index)
		result.FileName = result.AccountKey + ".json"
		result.PlanType = "team"
		results = append(results, result)
	}
	seedCompletedQuotaInspection(t, st, results...)
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	now := time.Now()
	events := make([]usage.Event, 0, 30)
	for minute := 0; minute < 30; minute++ {
		events = append(events, usage.Event{
			TimestampMS: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(),
			Provider:    "codex",
			AuthIndex:   "load-source",
			TotalTokens: 11_000_000,
		})
	}
	service.recordSmartUsageEvents(events, now)

	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("run automatic: %v", err)
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if createCalls.Load() != 0 {
		t.Fatalf("create calls = %d, want 0; resource=%#v orders=%#v", createCalls.Load(), status.SmartResource, status.Orders)
	}
	if status.ActiveOrder != nil || len(status.Orders) != 0 {
		t.Fatalf("purchase timing wait persisted an order: active=%#v orders=%#v", status.ActiveOrder, status.Orders)
	}
	if status.SmartResource.DecisionReason != "purchase_timing_wait" ||
		status.SmartResource.SuggestedQuantity != 0 ||
		status.SmartResource.PurchaseTimingEligibleQuantity != 0 ||
		status.SmartResource.PurchaseTimingWaitMinutes <= 0 {
		t.Fatalf("purchase timing wait resource = %#v", status.SmartResource)
	}
}

func TestSmartAutomaticUsesCapacitySizedBatchBelowWarningWhenSupplyIsPlenty(t *testing.T) {
	var createQuantity atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":100,"missing":0,"needs_production":false,"estimated_total_fen":100}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":100000,"balance_fen":100000}`))
		case r.URL.Path == "/v0/management/auth-files":
			_, _ = w.Write([]byte(`{"files":[{"name":"a.json","provider":"codex"},{"name":"b.json","provider":"codex"},{"name":"c.json","provider":"codex"}]}`))
		case r.URL.Path == "/api/customer/pickup/orders":
			var payload struct {
				Quantity int `json:"quantity"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode create payload: %v", err)
			}
			createQuantity.Store(int32(payload.Quantity))
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"order":{"id":"order-plenty","status":"waiting_inventory","quantity":1},"status_url":"/api/customer/pickup/orders/order-plenty"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "smart-plenty-small.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, BaseURL: server.URL, Username: "customer", Password: "password",
			Product: "oauth_30d", TargetAvailableAccounts: 1, ReplenishBatchSize: 10,
			PrelockMinQuantity: 1, PrelockMaxQuantity: 10,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	seedCompletedQuotaInspection(t, st, quotaInspectionResult(99.9))
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	now := time.Now()
	events := make([]usage.Event, 0, 30)
	for minute := 0; minute < 30; minute++ {
		events = append(events, usage.Event{
			TimestampMS: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(),
			Provider:    "codex",
			AuthIndex:   "load-source",
			TotalTokens: 10_000_000,
		})
	}
	service.recordSmartUsageEvents(events, now)

	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("run automatic: %v", err)
	}
	if createQuantity.Load() != 4 {
		t.Fatalf("quota-capacity shortage should create the first small parallel shard, quantity=%d", createQuantity.Load())
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status.SmartResource.SupplyPressureLevel != smartSupplyPressurePlenty ||
		status.SmartResource.SuggestedAction != smartActionEmergencyReplenish ||
		status.SmartResource.DecisionReason != "available_capacity_critical" {
		t.Fatalf("smart resource = %#v", status.SmartResource)
	}
	if len(status.Orders) == 0 || status.Orders[0].TriggerReason != "available_capacity_critical" || status.Orders[0].RequestedQuantity != 4 {
		t.Fatalf("staged order = %#v", status.Orders)
	}
}

func TestSmartAutomaticUsesQuotaSizedBatchWhenSupplyIsScarce(t *testing.T) {
	var createQuantity atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":0,"missing":10,"needs_production":true,"estimated_total_fen":100}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":100000,"balance_fen":100000}`))
		case r.URL.Path == "/v0/management/auth-files":
			_, _ = w.Write([]byte(`{"files":[{"name":"a.json","provider":"codex"},{"name":"b.json","provider":"codex"},{"name":"c.json","provider":"codex"}]}`))
		case r.URL.Path == "/api/customer/pickup/orders":
			var payload struct {
				Quantity int `json:"quantity"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode create payload: %v", err)
			}
			createQuantity.Store(int32(payload.Quantity))
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"order":{"id":"order-scarce","status":"waiting_inventory","quantity":3},"status_url":"/api/customer/pickup/orders/order-scarce"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "smart-scarce-full.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, BaseURL: server.URL, Username: "customer", Password: "password",
			Product: "oauth_30d", TargetAvailableAccounts: 1, ReplenishBatchSize: 10,
			PrelockMinQuantity: 1, PrelockMaxQuantity: 10,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	seedCompletedQuotaInspection(t, st, quotaInspectionResult(99.9))
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	now := time.Now()
	events := make([]usage.Event, 0, 30)
	for minute := 0; minute < 30; minute++ {
		events = append(events, usage.Event{
			TimestampMS: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(),
			Provider:    "codex",
			AuthIndex:   "load-source",
			TotalTokens: 10_000_000,
		})
	}
	service.recordSmartUsageEvents(events, now)

	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("run automatic: %v", err)
	}
	if createQuantity.Load() != 4 {
		t.Fatalf("scarce supply must still create the first small parallel shard, quantity=%d", createQuantity.Load())
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status.SmartResource.SupplyPressureLevel != smartSupplyPressureScarce ||
		status.SmartResource.SuggestedAction != smartActionEmergencyReplenish ||
		status.SmartResource.DecisionReason != "available_capacity_critical" {
		t.Fatalf("smart resource = %#v", status.SmartResource)
	}
	if len(status.Orders) == 0 || status.Orders[0].TriggerReason != "available_capacity_critical" || status.Orders[0].RequestedQuantity != 4 {
		t.Fatalf("staged scarce order = %#v", status.Orders)
	}
}

func TestSmartReadySmallOrderReleasesWhenQuotaCapacityIsHealthy(t *testing.T) {
	var takeCalls atomic.Int32
	var uploadCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":100,"missing":0,"needs_production":false,"estimated_total_fen":100}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-small":
			_, _ = w.Write([]byte(`{"id":"order-small","status":"ready","ready_quantity":1,"progress":100,"take_url":"/custom/take-small"}`))
		case r.URL.Path == "/custom/take-small":
			takeCalls.Add(1)
			_, _ = w.Write([]byte(`{"payload":{"accounts":[{"type":"codex","account":"small@example.com","access_token":"secret"}]},"status":"completed"}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			if name := r.URL.Query().Get("name"); name != "" && uploadCalls.Load() > 0 {
				_, _ = w.Write([]byte(`{"files":[{"name":"` + name + `","provider":"codex","status":"ready"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"files":[{"name":"a.json","provider":"codex","status":"ready","remaining_rcu":80},{"name":"b.json","provider":"codex","status":"ready","remaining_rcu":80},{"name":"c.json","provider":"codex","status":"ready","remaining_rcu":80},{"name":"d.json","provider":"codex","status":"ready","remaining_rcu":80}]}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodPost:
			uploadCalls.Add(1)
			part, err := r.MultipartReader()
			if err != nil {
				t.Fatalf("multipart reader: %v", err)
			}
			for {
				item, err := part.NextPart()
				if err != nil {
					break
				}
				if item.FormName() == "file" {
					data, _ := io.ReadAll(item)
					var payload map[string]any
					if err := json.Unmarshal(data, &payload); err != nil || payload["type"] != "codex" {
						t.Fatalf("uploaded payload = %s err=%v", data, err)
					}
				}
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "smart-small-take.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, BaseURL: server.URL, Username: "customer", Password: "password",
			Product: "oauth_30d", TargetAvailableAccounts: 1, ReplenishBatchSize: 10,
			PollIntervalSeconds: 1, PrelockMinQuantity: 1, PrelockMaxQuantity: 10,
			CriticalMinutes: 30, WarningMinutes: 60, HealthyMinutesTarget: 120,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "order-small", Product: "oauth_30d", RequestedQuantity: 1, Automatic: true, Status: "ready", TakeURL: "/custom/take-small",
	}); err != nil {
		t.Fatalf("create order: %v", err)
	}
	seedCompletedQuotaInspection(t, st,
		quotaInspectionResult(0), quotaInspectionResult(0),
		quotaInspectionResult(0), quotaInspectionResult(0),
	)
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	now := time.Now()
	events := make([]usage.Event, 0, 120)
	for minute := 0; minute < 30; minute++ {
		for index := 0; index < 4; index++ {
			events = append(events, usage.Event{
				TimestampMS: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(),
				Provider:    "codex",
				AuthIndex:   "a.json",
				TotalTokens: 100,
			})
		}
	}
	service.recordSmartUsageEvents(events, now)

	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("run automatic: %v", err)
	}
	if takeCalls.Load() != 0 || uploadCalls.Load() != 0 {
		t.Fatalf("healthy quota capacity should release an unnecessary ready order, take=%d upload=%d", takeCalls.Load(), uploadCalls.Load())
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if len(status.Orders) == 0 || status.Orders[0].Status != "released" {
		t.Fatalf("orders = %#v", status.Orders)
	}
}

func TestSmartEmergencyAccountWaterlineKeepsAutomaticOrderWhenQuotaIsHealthy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"files":[{"name":"live.json","provider":"codex","status":"ready","remaining_rcu":2000}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "smart-live-waterline.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	cfg := store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, Product: "oauth_7d", Strategy: managerconfigsvc.SupplyStrategyBalanced,
			HealthyMinutesTarget: 60, NewAccountConfidence: 0.7,
			CriticalAvailableAccounts: 1, HealthyAvailableAccounts: 5, DefaultEmergencyMinAccounts: 3,
		},
	}
	if err := st.SaveManagerConfig(context.Background(), cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	seedCompletedQuotaInspection(t, st, quotaInspectionResult(0), quotaInspectionResult(0))
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	now := time.Now()
	events := make([]usage.Event, 0, 120)
	for minute := 0; minute < 30; minute++ {
		for index := 0; index < 4; index++ {
			events = append(events, usage.Event{
				TimestampMS: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(),
				Provider:    "codex",
				AuthIndex:   "live.json",
				TotalTokens: 100,
			})
		}
	}
	service.recordSmartUsageEvents(events, now)
	order := store.SupplyOrder{
		OrderID: "order-live-waterline", Product: "oauth_7d", RequestedQuantity: 4,
		Automatic: true, Status: "waiting_inventory", CreatedAtMS: now.Add(-time.Minute).UnixMilli(),
	}

	released, err := service.autoReleaseAutomaticOrderIfNotNeeded(context.Background(), cfg, &order, true)
	if err != nil {
		t.Fatalf("check automatic release: %v", err)
	}
	if released {
		t.Fatal("critical live-account waterline must keep the existing reservation")
	}
	resource := service.currentSmartResource(cfg.Supply)
	if !smartResourceEmergency(resource) || resource.DecisionReason != "available_capacity_critical" ||
		resource.LockedOrderID != order.OrderID || resource.SuggestedQuantity != 1 {
		t.Fatalf("live-account emergency resource = %#v", resource)
	}
}

func TestIdleHealthyFloorReleasesReadyOrderWithoutUsageRate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"files":[{"name":"verified.json","provider":"codex","status":"ready","remaining_rcu":2000}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "smart-verified-healthy-floor.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	cfg := store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, Product: "oauth_7d", Strategy: managerconfigsvc.SupplyStrategyCustom,
			TargetAvailableAccounts: 2, HealthyAvailableAccounts: 2, CriticalAvailableAccounts: 0,
			ReplenishBatchSize: 1, PrelockMaxQuantity: 1,
		},
	}
	if err := st.SaveManagerConfig(context.Background(), cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	seedCompletedQuotaInspection(t, st, quotaInspectionResult(0))
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	order := store.SupplyOrder{
		OrderID: "verified-floor-ready", Product: "oauth_7d", RequestedQuantity: 1,
		Automatic: true, Status: "ready", CreatedAtMS: time.Now().Add(-time.Minute).UnixMilli(),
	}

	released, err := service.autoReleaseAutomaticOrderIfNotNeeded(context.Background(), cfg, &order, true)
	if err != nil {
		t.Fatalf("check automatic release: %v", err)
	}
	if !released {
		t.Fatal("idle healthy-floor shortage must release an unnecessary ready order")
	}
	resource := service.currentSmartResource(cfg.Supply)
	if resource.DecisionReason != "no_traffic_minimum_pool" || resource.SuggestedQuantity != 0 ||
		smartResourceEmergency(resource) {
		t.Fatalf("published idle healthy-floor decision = %#v", resource)
	}
}

func TestGetStatusPublishesLiveAccountEmergencyToWorkerState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"files":[{"name":"live-only.json","provider":"codex","status":"ready","remaining_rcu":2000}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "status-live-emergency.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	cfg := store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, Product: "oauth_7d", Strategy: managerconfigsvc.SupplyStrategyBalanced,
			CriticalAvailableAccounts: 1, HealthyAvailableAccounts: 5, DefaultEmergencyMinAccounts: 3,
		},
	}
	if err := st.SaveManagerConfig(context.Background(), cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	seedCompletedQuotaInspection(t, st, quotaInspectionResult(0), quotaInspectionResult(0))
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())

	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status.SmartResource.AvailableAccounts != 1 || status.SmartResource.EmergencyReason != "" ||
		status.SmartResource.DecisionReason != "no_traffic_minimum_pool" {
		t.Fatalf("live idle status = %#v", status.SmartResource)
	}
	workerState := service.currentSmartResource(cfg.Supply)
	if workerState.AvailableAccounts != 1 || workerState.EmergencyReason != "" || smartResourceEmergency(workerState) {
		t.Fatalf("worker did not retain reconciled idle floor = %#v", workerState)
	}
}

func TestSmartEmergencyReadyOrderTakesWithoutConfirmationRounds(t *testing.T) {
	var takeCalls atomic.Int32
	var uploadCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-critical":
			_, _ = w.Write([]byte(`{"id":"order-critical","status":"ready","ready_quantity":1,"progress":100,"take_url":"/custom/take-critical"}`))
		case r.URL.Path == "/custom/take-critical":
			takeCalls.Add(1)
			_, _ = w.Write([]byte(`{"payload":{"accounts":[{"type":"codex","account":"critical@example.com","access_token":"secret"}]},"status":"completed"}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			if name := r.URL.Query().Get("name"); name != "" && uploadCalls.Load() > 0 {
				_, _ = w.Write([]byte(`{"files":[{"name":"` + name + `","provider":"codex","status":"ready"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"files":[{"name":"a.json","provider":"codex","status":"ready","remaining_rcu":1}]}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodPost:
			uploadCalls.Add(1)
			part, err := r.MultipartReader()
			if err != nil {
				t.Fatalf("multipart reader: %v", err)
			}
			for {
				item, err := part.NextPart()
				if err != nil {
					break
				}
				if item.FormName() == "file" {
					data, _ := io.ReadAll(item)
					var payload map[string]any
					if err := json.Unmarshal(data, &payload); err != nil || payload["type"] != "codex" {
						t.Fatalf("uploaded payload = %s err=%v", data, err)
					}
				}
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "smart-critical.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, BaseURL: server.URL, Username: "customer", Password: "password",
			Product: "oauth_30d", TargetAvailableAccounts: 1, ReplenishBatchSize: 1,
			PollIntervalSeconds: 1, CriticalTakeConfirmRounds: 2,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "order-critical", Product: "oauth_30d", RequestedQuantity: 1, Automatic: true, Status: "ready", TakeURL: "/custom/take-critical",
	}); err != nil {
		t.Fatalf("create order: %v", err)
	}
	// Keep the inspected quota healthy. The ready order must still be taken
	// because the live account waterline is critical; quota capacity must not
	// hide a real schedulable-account shortage.
	seedCompletedQuotaInspection(t, st, quotaInspectionResult(0))
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	now := time.Now()
	events := make([]usage.Event, 0, 180)
	for minute := 0; minute < 30; minute++ {
		for index := 0; index < 6; index++ {
			events = append(events, usage.Event{
				TimestampMS: now.Add(-time.Duration(minute) * time.Minute).UnixMilli(),
				Provider:    "codex",
				AuthIndex:   "a.json",
				TotalTokens: 100,
			})
		}
	}
	service.recordSmartUsageEvents(events, now)

	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if takeCalls.Load() != 1 || uploadCalls.Load() != 1 {
		t.Fatalf("emergency ready order must take immediately, take=%d upload=%d", takeCalls.Load(), uploadCalls.Load())
	}
}

func TestSmartPrelockKeepsFullBatchWhenSupplyIsTight(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		ReplenishBatchSize: 10,
		PrelockMinQuantity: 1,
		PrelockMaxQuantity: 10,
	}
	resource := SmartResource{HealthLevel: smartHealthWarning}

	tightQuantity, tightReason := smartPrelockQuantityForSupplyPressure(cfg, resource, smartSupplyPressure{level: smartSupplyPressureTight}, 10)
	if tightQuantity != 10 || tightReason != "supply_tight_full_batch" {
		t.Fatalf("tight quantity=%d reason=%q, want 10/full", tightQuantity, tightReason)
	}

	scarceQuantity, scarceReason := smartPrelockQuantityForSupplyPressure(cfg, resource, smartSupplyPressure{level: smartSupplyPressureScarce}, 10)
	if scarceQuantity != 10 || scarceReason != "supply_scarce_full_batch" {
		t.Fatalf("scarce quantity=%d reason=%q, want 10/full", scarceQuantity, scarceReason)
	}
}

func TestSmartPrelockKeepsFullBatchWhenCapacityCritical(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		ReplenishBatchSize: 10,
		PrelockMinQuantity: 1,
		PrelockMaxQuantity: 10,
	}
	resource := SmartResource{HealthLevel: smartHealthCritical}

	quantity, reason := smartPrelockQuantityForSupplyPressure(cfg, resource, smartSupplyPressure{level: smartSupplyPressureScarce}, 10)
	if quantity != 10 || reason != "supply_scarce_full_batch" {
		t.Fatalf("critical quantity=%d reason=%q, want 10/full", quantity, reason)
	}
}

func TestSmartPrelockUsesProgressiveBatchWhilePoolIsHealthy(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		ReplenishBatchSize: 10,
		PrelockMinQuantity: 1,
		PrelockMaxQuantity: 10,
	}
	resource := SmartResource{HealthLevel: smartHealthHealthy}

	for _, pressure := range []string{
		smartSupplyPressurePlenty,
		smartSupplyPressureNormal,
		smartSupplyPressureTight,
		smartSupplyPressureScarce,
	} {
		quantity, reason := smartPrelockQuantityForSupplyPressure(
			cfg,
			resource,
			smartSupplyPressure{level: pressure},
			10,
		)
		if quantity != 3 || reason != "pool_healthy_progressive_batch" {
			t.Fatalf("pressure=%s quantity=%d reason=%q, want 3/progressive", pressure, quantity, reason)
		}
	}
}

func TestSmartPurchaseTimingWaitsUntilOneAccountIsUseful(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		ReplenishBatchSize:   10,
		PrelockMinQuantity:   1,
		PrelockMaxQuantity:   10,
		PollIntervalSeconds:  3,
		HealthyMinutesTarget: 55,
		WarningMinutes:       40,
		CriticalMinutes:      25,
	}
	resource := SmartResource{
		HealthLevel:                    smartHealthWarning,
		DemandTrend:                    smartDemandTrendStable,
		EffectiveHealthyMinutes:        55,
		WarningMinutes:                 40,
		EstimatedSustainMinutes:        54,
		CurrentCapacityRCU:             5_400,
		CapacityGapRCU:                 100,
		ConsumeRCUPerMinute:            100,
		DemandPlanningRCUPerMinute:     100,
		TokenCapacityMode:              smartTokenCapacityMode,
		EstimatedNewAccountCapacityRCU: 1_600,
	}
	quantity, reason, timing := smartPrelockQuantityForSupplyPressureWithTiming(
		cfg,
		resource,
		smartSupplyPressure{level: smartSupplyPressurePlenty, avgFulfillSeconds: 5, fulfillmentRate: 100},
		1,
	)
	if quantity != 0 || reason != "purchase_timing_wait" {
		t.Fatalf("early purchase = %d/%q, want 0/purchase_timing_wait", quantity, reason)
	}
	if timing.leadMinutes != 2 || timing.triggerMinutes != 41 || timing.waitMinutes != 13 || timing.eligibleQuantity != 0 {
		t.Fatalf("early purchase timing = %#v", timing)
	}
	idleTiming := smartJustInTimePurchase(
		cfg,
		resource,
		smartSupplyPressure{level: smartSupplyPressurePlenty, avgFulfillSeconds: 5, fulfillmentRate: 100},
		0,
	)
	if idleTiming.leadMinutes != 2 || idleTiming.triggerMinutes != 41 || idleTiming.waitMinutes != 13 {
		t.Fatalf("idle purchase timing visibility = %#v", idleTiming)
	}

	resource.EstimatedSustainMinutes = 41
	resource.CurrentCapacityRCU = 4_100
	resource.CapacityGapRCU = 1_400
	quantity, reason, timing = smartPrelockQuantityForSupplyPressureWithTiming(
		cfg,
		resource,
		smartSupplyPressure{level: smartSupplyPressurePlenty, avgFulfillSeconds: 5, fulfillmentRate: 100},
		1,
	)
	if quantity != 1 || reason != "supply_plenty_small_batch" || timing.eligibleQuantity != 1 || timing.waitMinutes != 0 {
		t.Fatalf("just-in-time purchase = %d/%q %#v, want one eligible account", quantity, reason, timing)
	}
}

func TestSmartPurchaseTimingUsesDemandAndLeadToStageQuantity(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		ReplenishBatchSize:   10,
		PrelockMinQuantity:   1,
		PrelockMaxQuantity:   10,
		PollIntervalSeconds:  3,
		HealthyMinutesTarget: 55,
		WarningMinutes:       40,
		CriticalMinutes:      25,
	}
	resource := SmartResource{
		HealthLevel:                    smartHealthWarning,
		DemandTrend:                    smartDemandTrendStable,
		EffectiveHealthyMinutes:        55,
		WarningMinutes:                 40,
		EstimatedSustainMinutes:        45,
		CurrentCapacityRCU:             4_500,
		CapacityGapRCU:                 1_000,
		ConsumeRCUPerMinute:            100,
		DemandPlanningRCUPerMinute:     100,
		TokenCapacityMode:              smartTokenCapacityMode,
		EstimatedNewAccountCapacityRCU: 1_000,
	}
	quantity, reason, timing := smartPrelockQuantityForSupplyPressureWithTiming(
		cfg,
		resource,
		smartSupplyPressure{level: smartSupplyPressurePlenty, avgFulfillSeconds: 5, fulfillmentRate: 100},
		3,
	)
	if quantity != 1 || reason != "supply_plenty_small_batch" || timing.eligibleQuantity != 1 {
		t.Fatalf("staged purchase = %d/%q %#v, want one fully useful account", quantity, reason, timing)
	}

	resource.EstimatedSustainMinutes = 54
	resource.CurrentCapacityRCU = 5_400
	resource.CapacityGapRCU = 100
	resource.EstimatedNewAccountCapacityRCU = 1_600
	quantity, reason, timing = smartPrelockQuantityForSupplyPressureWithTiming(
		cfg,
		resource,
		smartSupplyPressure{
			level:           smartSupplyPressureScarce,
			fulfillmentRate: 50,
			recentCancelled: 2,
		},
		1,
	)
	if quantity != 1 || reason != "supply_scarce_full_batch" || timing.leadMinutes != 27 || timing.eligibleQuantity != 1 {
		t.Fatalf("scarce lead purchase = %d/%q %#v, want earlier one-account order", quantity, reason, timing)
	}
}

func TestSmartPurchaseTimingReliableSupplyLowersEntryLineAndFollowsConsumption(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		ReplenishBatchSize:   10,
		PrelockMinQuantity:   1,
		PrelockMaxQuantity:   10,
		PollIntervalSeconds:  3,
		HealthyMinutesTarget: 55,
		WarningMinutes:       40,
		CriticalMinutes:      25,
	}
	pressure := smartSupplyPressure{
		level:                       smartSupplyPressurePlenty,
		reliablyAvailable:           true,
		shortWindowOrders:           3,
		recentSuccessStreak:         3,
		shortWindowFulfillmentRate:  100,
		shortWindowAvgFulfillSecond: 5,
	}
	resource := SmartResource{
		HealthLevel:                    smartHealthWarning,
		DemandTrend:                    smartDemandTrendStable,
		EffectiveHealthyMinutes:        55,
		WarningMinutes:                 40,
		CriticalMinutes:                25,
		EstimatedSustainMinutes:        40,
		CurrentCapacityRCU:             4_000,
		CapacityGapRCU:                 1_500,
		ConsumeRCUPerMinute:            100,
		DemandPlanningRCUPerMinute:     100,
		TokenCapacityMode:              smartTokenCapacityMode,
		EstimatedNewAccountCapacityRCU: 1_000,
	}
	quantity, reason, timing := smartPrelockQuantityForSupplyPressureWithTiming(cfg, resource, pressure, 4)
	if quantity != 0 || reason != "purchase_timing_wait" || timing.triggerMinutes != 32 ||
		timing.waitMinutes != 8 || timing.eligibleQuantity != 0 {
		t.Fatalf("reliable supply entered too early: quantity=%d reason=%q timing=%#v", quantity, reason, timing)
	}

	resource.EstimatedSustainMinutes = 32
	resource.CurrentCapacityRCU = 3_200
	quantity, reason, timing = smartPrelockQuantityForSupplyPressureWithTiming(cfg, resource, pressure, 4)
	if quantity != 1 || reason != "low_water_staged_batch" || timing.eligibleQuantity != 1 {
		t.Fatalf("reliable supply one-step purchase = %d/%q %#v, want 1", quantity, reason, timing)
	}

	resource.ConsumeRCUPerMinute = 200
	resource.DemandPlanningRCUPerMinute = 200
	resource.CurrentCapacityRCU = 6_400
	quantity, reason, timing = smartPrelockQuantityForSupplyPressureWithTiming(cfg, resource, pressure, 4)
	if quantity != 2 || reason != "low_water_staged_batch" || timing.eligibleQuantity != 2 {
		t.Fatalf("doubled consumption purchase = %d/%q %#v, want 2", quantity, reason, timing)
	}
}

func TestSmartPurchaseTimingPlentySupplyStagesOneAtWarning(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		ReplenishBatchSize: 10,
		PrelockMinQuantity: 1,
		PrelockMaxQuantity: 10,
		WarningMinutes:     40,
		CriticalMinutes:    25,
	}
	resource := SmartResource{
		HealthLevel:                    smartHealthWarning,
		EffectiveHealthyMinutes:        55,
		WarningMinutes:                 40,
		CriticalMinutes:                25,
		EstimatedSustainMinutes:        40,
		CurrentCapacityRCU:             4_000,
		CapacityGapRCU:                 1_500,
		ConsumeRCUPerMinute:            100,
		DemandPlanningRCUPerMinute:     100,
		EstimatedNewAccountCapacityRCU: 1_000,
	}
	quantity, reason, timing := smartPrelockQuantityForSupplyPressureWithTiming(
		cfg,
		resource,
		smartSupplyPressure{level: smartSupplyPressurePlenty},
		4,
	)
	if quantity != 1 || reason != "low_water_staged_batch" || timing.eligibleQuantity != 1 {
		t.Fatalf("plenty warning purchase = %d/%q %#v, want one staged account", quantity, reason, timing)
	}
}

func TestSmartPurchaseTimingDoesNotDelayLowWaterRefill(t *testing.T) {
	cfg := store.ManagerSupplyConfig{ReplenishBatchSize: 10, PrelockMinQuantity: 1, PrelockMaxQuantity: 10}
	resource := SmartResource{
		HealthLevel:                    smartHealthWarning,
		EffectiveHealthyMinutes:        55,
		WarningMinutes:                 40,
		EstimatedSustainMinutes:        40,
		CapacityGapRCU:                 1,
		ConsumeRCUPerMinute:            100,
		TokenCapacityMode:              smartTokenCapacityMode,
		EstimatedNewAccountCapacityRCU: 10_000,
	}
	quantity, reason, _ := smartPrelockQuantityForSupplyPressureWithTiming(
		cfg,
		resource,
		smartSupplyPressure{level: smartSupplyPressurePlenty},
		1,
	)
	if quantity != 1 || reason != "low_water_staged_batch" {
		t.Fatalf("low-water purchase = %d/%q, want immediate staged refill", quantity, reason)
	}
}

func TestSmartPurchaseTimingCapsOrdersBySupplierBatchLifetime(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		ReplenishBatchSize:   10,
		PrelockMinQuantity:   1,
		PrelockMaxQuantity:   10,
		PollIntervalSeconds:  3,
		HealthyMinutesTarget: 30,
		WarningMinutes:       20,
		CriticalMinutes:      15,
	}
	pressure := smartSupplyPressure{
		level:                     smartSupplyPressureScarce,
		inventoryAvailable:        15,
		inventoryMinRemainSeconds: 2735,
		inventoryMaxRemainSeconds: 2735,
		fulfillmentRate:           0.8,
		recentCancelled:           98,
	}
	resource := SmartResource{
		HealthLevel:                    smartHealthWarning,
		DemandTrend:                    smartDemandTrendStable,
		EffectiveHealthyMinutes:        30,
		WarningMinutes:                 20,
		CriticalMinutes:                15,
		EstimatedSustainMinutes:        55,
		CurrentCapacityRCU:             5_493.8,
		CapacityGapRCU:                 1_000,
		ConsumeRCUPerMinute:            99.89,
		DemandPlanningRCUPerMinute:     99.89,
		TokenCapacityMode:              smartTokenCapacityMode,
		EstimatedNewAccountCapacityRCU: 1_050,
	}
	quantity, reason, timing := smartPrelockQuantityForSupplyPressureWithTiming(cfg, resource, pressure, 4)
	if quantity != 0 || reason != "supply_lifetime_capacity_wait" || !timing.lifetimeLimited ||
		timing.supplyLifetimeMinutes != 45.6 || timing.lifetimeQuantityLimit != 0 {
		t.Fatalf("lifetime-overflow purchase = %d/%q timing=%#v, want blocked", quantity, reason, timing)
	}

	resource.CurrentCapacityRCU = 3_000
	resource.EstimatedSustainMinutes = round1(resource.CurrentCapacityRCU / resource.ConsumeRCUPerMinute)
	quantity, reason, timing = smartPrelockQuantityForSupplyPressureWithTiming(cfg, resource, pressure, 4)
	if quantity != 1 || reason != "supply_scarce_full_batch" || timing.lifetimeQuantityLimit != 1 ||
		timing.eligibleQuantity != 1 {
		t.Fatalf("one-useful-account purchase = %d/%q timing=%#v, want one", quantity, reason, timing)
	}
}

func TestSmartPurchaseTimingKeepsAccountVacuumRefillAboveLifetimeCap(t *testing.T) {
	cfg := store.ManagerSupplyConfig{ReplenishBatchSize: 10, PrelockMinQuantity: 1, PrelockMaxQuantity: 10}
	resource := SmartResource{
		HealthLevel:                    smartHealthCritical,
		EmergencyShortage:              true,
		EmergencyReason:                "critical_available_accounts",
		AvailableAccounts:              1,
		CriticalAvailableAccounts:      2,
		ConsumeRCUPerMinute:            100,
		DemandPlanningRCUPerMinute:     100,
		CurrentCapacityRCU:             5_000,
		TokenCapacityMode:              smartTokenCapacityMode,
		EstimatedNewAccountCapacityRCU: 1_000,
	}
	quantity, reason, timing := smartPrelockQuantityForSupplyPressureWithTiming(
		cfg,
		resource,
		smartSupplyPressure{level: smartSupplyPressurePlenty, inventoryMinRemainSeconds: 600},
		5,
	)
	if quantity != 5 || reason != resource.EmergencyReason || timing.lifetimeLimited {
		t.Fatalf("account-vacuum refill = %d/%q timing=%#v, want uncapped 5", quantity, reason, timing)
	}
}

func TestSmartPrelockDoesNotRaiseEmergencyRetryLadderRung(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		ReplenishBatchSize: 10,
		PrelockMinQuantity: 1,
		PrelockMaxQuantity: 10,
	}
	resource := SmartResource{
		HealthLevel:       smartHealthCritical,
		EmergencyShortage: true,
		EmergencyReason:   "critical_available_accounts",
		DecisionReason:    "emergency_retry_immediate_after_cancelled",
		AvailableAccounts: 0,
	}
	quantity, reason := smartPrelockQuantityForSupplyPressure(
		cfg,
		resource,
		smartSupplyPressure{level: smartSupplyPressureScarce},
		2,
	)
	if quantity != 2 || reason != resource.DecisionReason {
		t.Fatalf("retry pressure adjustment = %d/%q, want final ladder cap 2/%q", quantity, reason, resource.DecisionReason)
	}
}

func TestSmartTakeAllowedWhenReadyBelowWarningWater(t *testing.T) {
	service := New(nil, nil)
	service.setSmartResource(SmartResource{
		Enabled:         true,
		GeneratedAtMS:   time.Now().UnixMilli(),
		SnapshotFresh:   true,
		SuggestedAction: smartActionTakeLocked,
		DecisionReason:  "low_water_take_ready",
	})
	if !service.smartTakeAllowed(store.ManagerSupplyConfig{}, "ready-low-water") {
		t.Fatal("ready low-water order must be allowed to take without the plenty-supply release path")
	}
}

func TestSmartStaleVerifiedLowWaterReadyTakeAllowed(t *testing.T) {
	resource := SmartResource{
		SnapshotFresh:              false,
		Confidence:                 smartConfidenceMedium,
		CapacitySource:             smartCapacitySourceInspection,
		CapacitySnapshotRunID:      42,
		CapacityCoverage:           100,
		CapacitySnapshotAtMS:       time.Now().Add(-25 * time.Minute).UnixMilli(),
		CapacitySnapshotAgeSeconds: 25 * 60,
		CurrentCapacityRCU:         12_000,
		ConsumeRCUPerMinute:        1_000,
		EstimatedSustainMinutes:    12,
		WarningMinutes:             25,
		HealthLevel:                smartHealthCritical,
	}
	if !smartStaleVerifiedLowWaterReadyTakeAllowed(resource) {
		t.Fatalf("complete but aging critical lower bound must take an already-ready order: %#v", resource)
	}
	resource.CapacityCoverage = 99
	if smartStaleVerifiedLowWaterReadyTakeAllowed(resource) {
		t.Fatalf("incomplete inspection coverage must not take from stale data: %#v", resource)
	}
	resource.CapacityCoverage = 100
	resource.CapacitySnapshotAgeSeconds = 91 * 60
	if smartStaleVerifiedLowWaterReadyTakeAllowed(resource) {
		t.Fatalf("expired inspection lower bound must not take: %#v", resource)
	}
}

func TestSmartPlentyTakeBatchAllowsFiveAccountReadyOrder(t *testing.T) {
	cfg := store.ManagerSupplyConfig{ReplenishBatchSize: 10, PrelockMaxQuantity: 10}
	if got := smartPlentyTakeBatchQuantity(cfg); got != 5 {
		t.Fatalf("take batch threshold=%d, want 5", got)
	}
}

func TestSmartPlentySmallBatchFollowsCapacityGap(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		ReplenishBatchSize: 10,
		PrelockMinQuantity: 1,
		PrelockMaxQuantity: 10,
	}
	for _, test := range []struct {
		quantity int
		want     int
	}{
		{quantity: 1, want: 1},
		{quantity: 2, want: 2},
		{quantity: 10, want: 3},
	} {
		if got := smartPlentySmallBatchQuantity(cfg, test.quantity); got != test.want {
			t.Fatalf("quantity=%d batch=%d, want %d", test.quantity, got, test.want)
		}
	}
}

func TestOrdinaryAccountTargetGateStopsOnlyNonEmergencyCapacityPurchases(t *testing.T) {
	cfg := store.ManagerSupplyConfig{TargetAvailableAccounts: 45}
	resource := SmartResource{
		SnapshotFresh:           true,
		AvailableAccounts:       51,
		HealthLevel:             smartHealthWarning,
		SuggestedAction:         smartActionPrelock,
		SuggestedQuantity:       1,
		CapacityGapRCU:          17.3,
		ConsumeRCUPerMinute:     8.33,
		EstimatedSustainMinutes: 52.9,
		EffectiveHealthyMinutes: 55,
	}
	if !applyOrdinaryAccountTargetGate(cfg, &resource, 51) {
		t.Fatal("filled account pool should stop an ordinary non-emergency purchase")
	}
	if resource.SuggestedAction != smartActionObserveDemand || resource.SuggestedQuantity != 0 ||
		resource.DecisionReason != "account_target_reached_reserve_only" || resource.CapacityGapRCU != 17.3 {
		t.Fatalf("gated resource = %#v", resource)
	}

	belowTarget := resource
	belowTarget.SuggestedAction = smartActionPrelock
	belowTarget.SuggestedQuantity = 1
	if applyOrdinaryAccountTargetGate(cfg, &belowTarget, 44) {
		t.Fatal("account deficit must retain ordinary replenishment")
	}

	emergency := resource
	emergency.EmergencyShortage = true
	emergency.SuggestedAction = smartActionEmergencyReplenish
	emergency.SuggestedQuantity = 5
	if applyOrdinaryAccountTargetGate(cfg, &emergency, 51) {
		t.Fatal("a real emergency shortage must bypass the account-target gate")
	}

	stale := resource
	stale.SnapshotFresh = false
	stale.SuggestedAction = smartActionPrelock
	stale.SuggestedQuantity = 1
	if applyOrdinaryAccountTargetGate(cfg, &stale, 51) {
		t.Fatal("a stale snapshot must not cancel procurement")
	}
}

func TestSmartDemandPlanUsesShortTermRateAndTrendWindows(t *testing.T) {
	tests := []struct {
		name         string
		usage        smartUsageAggregate
		wantTrend    string
		wantConsume  float64
		wantPlanning float64
	}{
		{
			name: "one minute window not ready retains historical fallback",
			usage: smartUsageAggregate{
				rpm30: 20, rpm5Peak: 30, tpm30: 1_600,
			},
			wantTrend: smartDemandTrendUnknown, wantConsume: 21, wantPlanning: 21,
		},
		{
			name: "stable demand follows latest completed minute",
			usage: smartUsageAggregate{
				oneMinuteReady: true,
				rpm1:           100,
				rpm5:           100,
				rpm10:          100,
			},
			wantTrend: smartDemandTrendStable, wantConsume: 100, wantPlanning: 100,
		},
		{
			name: "spike keeps immediate capacity risk but uses trend baseline for purchase planning",
			usage: smartUsageAggregate{
				oneMinuteReady: true,
				rpm1:           1_000,
				rpm5:           100,
				rpm10:          100,
			},
			wantTrend: smartDemandTrendRising, wantConsume: 1_000, wantPlanning: 100,
		},
		{
			name: "completed low minute pauses new procurement immediately",
			usage: smartUsageAggregate{
				oneMinuteReady: true,
				rpm1:           0,
				rpm5:           100,
				rpm10:          100,
			},
			wantTrend: smartDemandTrendFalling, wantConsume: 0, wantPlanning: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := smartDemandPlanForUsage(tt.usage, 80)
			if plan.trend != tt.wantTrend || plan.consumeRCU != tt.wantConsume || plan.planningRCU != tt.wantPlanning {
				t.Fatalf("plan = %#v, want trend=%q consume=%v planning=%v", plan, tt.wantTrend, tt.wantConsume, tt.wantPlanning)
			}
		})
	}
}

func TestApplySmartUsagePublishesTokenDrivenDemandBreakdown(t *testing.T) {
	resource := SmartResource{}
	applySmartUsage(&resource, smartUsageAggregate{
		oneMinuteReady: true,
		rpm1:           224,
		tpm1:           16_555_789,
		rpm5:           224,
		tpm5:           16_555_789,
		rpm10:          224,
		tpm10:          16_555_789,
	}, 40)

	if resource.RequestDemandRCUPerMinute != 224 || resource.TokenDemandRCUPerMinute != 413.89 ||
		resource.ConsumeRCUPerMinute != 413.89 || resource.DemandDriver != "tokens" {
		t.Fatalf("token-driven demand breakdown = %#v", resource)
	}
}

func TestSmartDemandFallingPausesOrdersWhileKeepingCapacityHealthVisible(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Product:              "oauth_30d",
		HealthyMinutesTarget: 40,
		WarningMinutes:       15,
		CriticalMinutes:      5,
		PrelockMinQuantity:   1,
		PrelockMaxQuantity:   10,
		ReplenishBatchSize:   10,
		NewAccountConfidence: 1,
	}
	resource := SmartResource{
		CurrentCapacityRCU:      300,
		ConsumeRCUPerMinute:     10,
		UnitCapacityRCU:         80,
		EffectiveHealthyMinutes: 40,
		WarningMinutes:          15,
		CriticalMinutes:         5,
		DemandTrend:             smartDemandTrendFalling,
	}

	recalculateSmartResourceCapacityPlan(cfg, &resource)
	if resource.HealthLevel != smartHealthWarning || resource.SuggestedAction != smartActionObserveDemand ||
		resource.DecisionReason != "capacity_below_target_falling_observe" || resource.SuggestedQuantity != 0 || resource.EmergencyShortage {
		t.Fatalf("falling demand decision = %#v", resource)
	}
	if got := New(nil, nil).smartSuggestedCreateQuantity(cfg, resource); got != 0 {
		t.Fatalf("falling demand must create no new order, got %d", got)
	}

	zeroTraffic := resource
	zeroTraffic.ConsumeRCUPerMinute = 0
	zeroTraffic.CurrentCapacityRCU = 500
	recalculateSmartResourceCapacityPlan(cfg, &zeroTraffic)
	if zeroTraffic.HealthLevel != smartHealthHealthy || zeroTraffic.SuggestedAction != smartActionObserveDemand ||
		zeroTraffic.DecisionReason != "demand_falling_observe" || zeroTraffic.TargetCapacityRCU != 0 || zeroTraffic.SuggestedQuantity != 0 {
		t.Fatalf("completed zero-traffic minute must pause procurement, got %#v", zeroTraffic)
	}
}

func TestSmartDemandRisingCapsFirstOrderToObservationBatch(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Product:              "oauth_30d",
		HealthyMinutesTarget: 40,
		WarningMinutes:       15,
		CriticalMinutes:      5,
		PrelockMinQuantity:   1,
		PrelockMaxQuantity:   10,
		ReplenishBatchSize:   10,
		NewAccountConfidence: 1,
	}
	resource := SmartResource{
		CurrentCapacityRCU:      11_000,
		ConsumeRCUPerMinute:     500,
		UnitCapacityRCU:         80,
		EffectiveHealthyMinutes: 40,
		WarningMinutes:          15,
		CriticalMinutes:         5,
		DemandTrend:             smartDemandTrendRising,
	}

	recalculateSmartResourceCapacityPlan(cfg, &resource)
	if resource.SuggestedQuantity != 3 || resource.DecisionReason != "demand_rising_observe" {
		t.Fatalf("rising demand must use a 1-3 account observation batch, got %#v", resource)
	}
	if got := New(nil, nil).smartSuggestedCreateQuantity(cfg, resource); got != 3 {
		t.Fatalf("rising demand create quantity = %d, want 3", got)
	}
}

func TestSmartEmergencyShortageOverridesTrendObservation(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Product:              "oauth_30d",
		HealthyMinutesTarget: 40,
		WarningMinutes:       15,
		CriticalMinutes:      5,
		PrelockMinQuantity:   1,
		PrelockMaxQuantity:   3,
		ReplenishBatchSize:   10,
		NewAccountConfidence: 1,
	}
	falling := SmartResource{
		CurrentCapacityRCU:      100,
		ConsumeRCUPerMinute:     10,
		UnitCapacityRCU:         80,
		EffectiveHealthyMinutes: 40,
		WarningMinutes:          15,
		CriticalMinutes:         5,
		DemandTrend:             smartDemandTrendFalling,
	}
	recalculateSmartResourceCapacityPlan(cfg, &falling)
	if !falling.EmergencyShortage || falling.SuggestedAction != smartActionEmergencyReplenish ||
		falling.DecisionReason != "emergency_capacity_shortage" || falling.SuggestedQuantity != 3 {
		t.Fatalf("falling emergency should bypass observation: %#v", falling)
	}
	if got := New(nil, nil).smartSuggestedCreateQuantity(cfg, falling); got != 3 {
		t.Fatalf("falling emergency quantity=%d, want 3", got)
	}
	if got, reason := smartPrelockQuantityForSupplyPressure(cfg, falling, smartSupplyPressure{level: smartSupplyPressurePlenty}, 10); got != 5 || reason != "emergency_refill_to_healthy" {
		t.Fatalf("capacity emergency pressure adjustment=%d/%q, want healthy refill", got, reason)
	}

	rising := falling
	rising.CurrentCapacityRCU = 0
	rising.ConsumeRCUPerMinute = 1_000
	rising.DemandTrend = smartDemandTrendRising
	recalculateSmartResourceCapacityPlan(cfg, &rising)
	if !rising.EmergencyShortage || rising.SuggestedQuantity != 5 || rising.SuggestedAction != smartActionEmergencyReplenish {
		t.Fatalf("rising capacity emergency should retain the healthy-floor batch: %#v", rising)
	}
}

func TestSmartResourceTreatsCompletedZeroMinuteAsFallingDemand(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Minute).Add(10 * time.Second)
	service.recordSmartUsageEvents([]usage.Event{{
		TimestampMS: now.Add(-2 * time.Minute).UnixMilli(),
		Provider:    "codex",
		AuthIndex:   "previous-load",
		TotalTokens: 100,
	}}, now)

	resource := service.buildSmartResourceFromSnapshots(store.ManagerSupplyConfig{
		Product:              "oauth_30d",
		HealthyMinutesTarget: 40,
		WarningMinutes:       15,
		CriticalMinutes:      5,
		PrelockMinQuantity:   1,
		PrelockMaxQuantity:   10,
	}, authFileSnapshot{
		generatedAt: now,
		files: []cpaauthfiles.File{{
			Name:     "ready.json",
			Provider: "codex",
			Raw: map[string]any{
				"status":        "ready",
				"remaining_rcu": 100,
			},
		}},
	}, now)

	if resource.DemandTrend != smartDemandTrendFalling || resource.ConsumeRCUPerMinute != 0 ||
		resource.SuggestedAction != smartActionObserveDemand || resource.DecisionReason != "demand_falling_observe" ||
		resource.SuggestedQuantity != 0 {
		t.Fatalf("completed zero minute must be an observable falling-demand pause, got %#v", resource)
	}
}

func TestSmartRequiredConcurrencySlotsUsesObservedDurationAndHeadroom(t *testing.T) {
	if got := smartRequiredConcurrencySlots(120, 5_000); got != 12 {
		t.Fatalf("required concurrency slots = %d, want 12", got)
	}
	if got := smartRequiredConcurrencySlots(0, 5_000); got != 0 {
		t.Fatalf("zero traffic slots = %d, want 0", got)
	}
	if got := smartRequiredConcurrencySlots(120, 0); got != 0 {
		t.Fatalf("missing latency slots = %d, want 0", got)
	}
}

func TestSmartUsageTracksSuccessfulRequestLatencyForConcurrencyDemand(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Minute).Add(10 * time.Second)
	latencyOne := int64(1_000)
	latencyThree := int64(3_000)
	failedLatency := int64(20_000)
	service.recordSmartUsageEvents([]usage.Event{
		{TimestampMS: now.Add(-time.Minute).UnixMilli(), Provider: "codex", AuthIndex: "one", LatencyMS: &latencyOne},
		{TimestampMS: now.Add(-time.Minute).UnixMilli(), Provider: "codex", AuthIndex: "two", LatencyMS: &latencyThree},
		{TimestampMS: now.Add(-time.Minute).UnixMilli(), Provider: "codex", AuthIndex: "failed", LatencyMS: &failedLatency, Failed: true},
	}, now)

	usageSnapshot := service.smartUsageSnapshot(now)
	resource := SmartResource{}
	applySmartUsage(&resource, usageSnapshot, 80)
	if usageSnapshot.latencySamples != 2 || usageSnapshot.avgLatencyMS != 2_000 ||
		resource.AverageRequestLatencyMS != 2_000 || !resource.ConcurrencyDemandObserved ||
		resource.RequiredConcurrencySlots != 1 {
		t.Fatalf("latency-backed concurrency demand = usage %#v, resource %#v", usageSnapshot, resource)
	}
}

func TestConcurrencyShortageStagesSmallOrderWithoutChangingQuotaCapacity(t *testing.T) {
	cfg := store.ManagerSupplyConfig{
		Product:                  "oauth_7d",
		ReplenishBatchSize:       10,
		PrelockMaxQuantity:       10,
		HealthyAvailableAccounts: 1,
	}
	resource := SmartResource{
		SnapshotFresh:                   true,
		HealthLevel:                     smartHealthHealthy,
		SuggestedAction:                 smartActionHealthy,
		DecisionReason:                  "capacity_healthy",
		CurrentCapacityRCU:              9_000,
		ConcurrencyEffectiveCapacityRCU: 3_000,
		AvailableAccounts:               4,
		ConcurrencyDemandObserved:       true,
		ConcurrencyLimited:              true,
		ConcurrencyFiniteSlots:          4,
		RequiredConcurrencySlots:        10,
		ConcurrencyAccountDeficit:       6,
	}
	applySmartAccountQuantityEstimate(cfg, &resource)
	if resource.CurrentCapacityRCU != 9_000 || resource.ConcurrencyEffectiveCapacityRCU != 3_000 ||
		resource.AccountQuantityDeficit != 6 || resource.SuggestedQuantity != 3 ||
		resource.SuggestedAction != smartActionPrelock || resource.DecisionReason != "concurrency_capacity_shortage" {
		t.Fatalf("concurrency shortage plan = %#v", resource)
	}
}
