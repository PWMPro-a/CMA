package supply

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		Product:                 "oauth_30d",
		TargetAvailableAccounts: 2,
		HealthyMinutesTarget:    120,
		WarningMinutes:          60,
		CriticalMinutes:         30,
		PrelockMinQuantity:      1,
		PrelockMaxQuantity:      10,
		NewAccountConfidence:    0.7,
	}, authFileSnapshot{
		generatedAt: now,
		files: []cpaauthfiles.File{
			{Name: "a.json", Provider: "codex", Raw: map[string]any{"remaining_rcu": 80}},
			{Name: "b.json", Provider: "codex", Raw: map[string]any{"remaining_rcu": 80}},
		},
	}, now)

	if resource.HealthLevel != smartHealthWarning || resource.SuggestedQuantity < 1 {
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
	if resource.AvailableAccounts != 1 || resource.HealthyAccounts != 1 || resource.WeakAccounts != 1 || resource.RawCapacityRCU != 2200 {
		t.Fatalf("only the successfully verified credential may contribute capacity: %#v", resource)
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

func TestSmartResourceUsesPersistedSupplyLeaseForCapacity(t *testing.T) {
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
	if resource.RawCapacityRCU != 400 || resource.CurrentCapacityRCU != 5 {
		t.Fatalf("capacity must use the remaining five-minute lease rather than an account count: %#v", resource)
	}
}

func TestSmartResourcePausesWhenSupplyLeaseEvidenceIsMissing(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	usedPercent := 0.0
	resource := New(nil, nil).buildSmartResourceFromInspectionSnapshot(store.ManagerSupplyConfig{
		Product: "oauth_30d",
	}, inspectionQuotaSnapshot{
		run: store.CodexInspectionRun{ProbeSetCount: 1, SampledCount: 1, FinishedAtMS: now.UnixMilli()},
		results: []store.CodexInspectionResult{{
			AccountKey: "missing-lease", FileName: "codex-supply-missing-lease.json", Provider: "codex", Action: "keep",
			UsedPercent: &usedPercent,
		}},
		generatedAt: now,
	}, now)

	if resource.SnapshotFresh || resource.DecisionReason != "inspection_lease_incomplete" || resource.CapacityLifetimeCoverage != 0 {
		t.Fatalf("missing supply lease must pause automatic purchase: %#v", resource)
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

	if resource.SchedulableAccounts != 4 || resource.AvailableAccounts != 2 ||
		resource.HealthyAccounts != 2 || resource.WeakAccounts != 2 {
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
	if payload["availableAccounts"] != float64(2) || payload["schedulableAccounts"] != float64(4) ||
		payload["healthyAccounts"] != float64(2) || payload["weakAccounts"] != float64(2) {
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

func TestSmartAutomaticPausesWithoutInspectionSnapshotOrAuthFileRead(t *testing.T) {
	var authFileRequests atomic.Int32
	var supplyRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files":
			authFileRequests.Add(1)
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
	if authFileRequests.Load() != 0 || supplyRequests.Load() != 0 {
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
	if resource.AvailableAccounts != 2 || resource.RawCapacityRCU != 160 || resource.CurrentCapacityRCU != 120 {
		t.Fatalf("transient runtime errors must retain capacity before the normal expiry cap, got %#v", resource)
	}
	if resource.ConsumeRCUPerMinute <= 0 || resource.HealthLevel != smartHealthHealthy || resource.CapacityGapRCU != 0 {
		t.Fatalf("restored cooldown capacity should produce a healthy no-replenishment decision: %#v", resource)
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
			_, _ = w.Write([]byte(`{"files":[{"name":"ready.json","provider":"codex","status":"ready","remaining_rcu":100}]}`))
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
	if authFileRequests.Load() != 0 || supplyRequests.Load() != 0 {
		t.Fatalf("status refresh requests auth=%d supply=%d", authFileRequests.Load(), supplyRequests.Load())
	}

	status, err = service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("second status: %v", err)
	}
	if !status.SmartResource.SnapshotFresh || authFileRequests.Load() != 0 || supplyRequests.Load() != 0 {
		t.Fatalf("cached status should not refetch or create supply orders: status=%#v auth=%d supply=%d", status.SmartResource, authFileRequests.Load(), supplyRequests.Load())
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

	if resource.RawCapacityRCU != 24000 {
		t.Fatalf("fallback capacity should include the one-hour lifetime, got %#v", resource)
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
	if resource.EffectiveHealthyMinutes != 55 || resource.TargetCapacityRCU != 55 {
		t.Fatalf("healthy target should be capped by useful account lifetime, got %#v", resource)
	}
	if resource.HealthLevel != smartHealthHealthy || resource.SuggestedQuantity != 0 {
		t.Fatalf("low burn with enough expiry-limited capacity should not replenish, got %#v", resource)
	}
}

func TestSmartAutomaticSkipsCreateWhenUsageRateNotReady(t *testing.T) {
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
			_, _ = w.Write([]byte(`{"files":[{"name":"a.json","provider":"codex","success":100,"failed":0},{"name":"b.json","provider":"codex","success":100,"failed":0}]}`))
		case r.URL.Path == "/api/customer/pickup/orders":
			createCalls.Add(1)
			t.Fatal("smart mode must not create from account count when burn rate is unavailable")
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

func TestSmartAutomaticUsesSmallBatchWhenSupplyIsPlenty(t *testing.T) {
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
			_, _ = w.Write([]byte(`{"files":[]}`))
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
	if createQuantity.Load() != 3 {
		t.Fatalf("plenty supply should create a capacity-sized small batch, quantity=%d", createQuantity.Load())
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status.SmartResource.SupplyPressureLevel != smartSupplyPressurePlenty || status.SmartResource.DecisionReason != "supply_plenty_small_batch" {
		t.Fatalf("smart resource = %#v", status.SmartResource)
	}
}

func TestSmartAutomaticKeepsFullBatchWhenSupplyIsScarce(t *testing.T) {
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
			_, _ = w.Write([]byte(`{"files":[]}`))
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
	if createQuantity.Load() <= 1 {
		t.Fatalf("scarce supply should keep the recommended batch, quantity=%d", createQuantity.Load())
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status.SmartResource.SupplyPressureLevel != smartSupplyPressureScarce || status.SmartResource.DecisionReason != "supply_scarce_full_batch" {
		t.Fatalf("smart resource = %#v", status.SmartResource)
	}
}

func TestSmartReadySmallOrderTakesWhenSupplyIsPlentyBeforeCritical(t *testing.T) {
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
			_, _ = w.Write([]byte(`{"files":[{"name":"a.json","provider":"codex","status":"ready","remaining_rcu":80},{"name":"b.json","provider":"codex","status":"ready","remaining_rcu":80}]}`))
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
	seedCompletedQuotaInspection(t, st, quotaInspectionResult(98), quotaInspectionResult(98))
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
	if takeCalls.Load() != 1 || uploadCalls.Load() != 1 {
		t.Fatalf("small plenty order should be taken once, take=%d upload=%d", takeCalls.Load(), uploadCalls.Load())
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if len(status.Orders) == 0 || status.Orders[0].Status != "completed" {
		t.Fatalf("orders = %#v", status.Orders)
	}
}

func TestSmartReadyOrderWaitsForCriticalConfirmRoundsBeforeTake(t *testing.T) {
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
	seedCompletedQuotaInspection(t, st, quotaInspectionResult(99.9))
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
	if takeCalls.Load() != 0 {
		t.Fatalf("take should wait for confirmation, calls=%d", takeCalls.Load())
	}
	order, found, err := st.GetSupplyOrder(context.Background(), "order-critical")
	if err != nil || !found {
		t.Fatalf("load order found=%v err=%v", found, err)
	}
	order.NextPollAtMS = 0
	if err := st.UpdateSupplyOrder(context.Background(), order); err != nil {
		t.Fatalf("reset poll: %v", err)
	}
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if takeCalls.Load() != 1 || uploadCalls.Load() != 1 {
		t.Fatalf("calls take=%d upload=%d", takeCalls.Load(), uploadCalls.Load())
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
