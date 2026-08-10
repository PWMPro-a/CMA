package supply

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/config"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpaauthfiles"
	managerconfigsvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/supplyclient"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/usage"
)

func TestAutomationExecutionTracksScheduledAndCompletedCycles(t *testing.T) {
	service := New(nil, nil)
	now := time.Now().Truncate(time.Millisecond)
	service.ScheduleAutomaticExecution(now.Add(2 * time.Second))
	scheduled := service.currentAutomationExecution(true)
	if scheduled.LastResult != "scheduled" || scheduled.NextExecutionAtMS != now.Add(2*time.Second).UnixMilli() {
		t.Fatalf("scheduled automation = %#v", scheduled)
	}

	service.setSmartResource(SmartResource{
		SuggestedAction: smartActionTakeLocked,
		DecisionReason:  "critical_take_confirmed",
	})
	finishedAt := now.Add(300 * time.Millisecond)
	nextAt := finishedAt.Add(15 * time.Second)
	service.RecordAutomaticExecution(now, finishedAt, nextAt, nil)
	completed := service.currentAutomationExecution(true)
	if completed.LastResult != "completed" || completed.LastAction != smartActionTakeLocked ||
		completed.LastReason != "critical_take_confirmed" || completed.LastError != "" ||
		completed.IntervalSeconds != 15 || completed.NextExecutionAtMS != nextAt.UnixMilli() {
		t.Fatalf("completed automation = %#v", completed)
	}

	service.RecordAutomaticExecution(now, finishedAt, nextAt, errors.New("supplier unavailable"))
	failed := service.currentAutomationExecution(true)
	if failed.LastResult != "failed" || failed.LastError == "" {
		t.Fatalf("failed automation = %#v", failed)
	}

	disabled := service.currentAutomationExecution(false)
	if disabled.Enabled || disabled.NextExecutionAtMS != 0 || disabled.IntervalSeconds != 0 {
		t.Fatalf("disabled automation must not expose a future execution: %#v", disabled)
	}
}

func TestSupplyAuth401CandidateOnlyAcceptsManagedSupplyAccounts(t *testing.T) {
	nowMS := time.Now().UnixMilli()
	event := usage.Event{
		Failed:               true,
		FailStatusCode:       http.StatusUnauthorized,
		AuthFileSnapshot:     "codex-supply-account.json",
		AuthIndex:            "7",
		AuthProviderSnapshot: "codex",
		AccountSnapshot:      "account@example.com",
		TimestampMS:          nowMS - 1000,
		FailSummary:          "upstream returned 401",
	}
	candidate, ok := supplyAuth401CandidateFromEvent(event, nowMS)
	if !ok || candidate.FileName != event.AuthFileSnapshot || candidate.AuthIndex != "7" ||
		candidate.SeenAtMS != event.TimestampMS || candidate.Provider != "codex" ||
		candidate.FailureSummary != event.FailSummary || !strings.Contains(candidate.EvidenceJSON, `"statusCode":401`) {
		t.Fatalf("401 candidate = %#v, ok=%v", candidate, ok)
	}
	event.AuthFileSnapshot = "manual-account.json"
	if _, ok := supplyAuth401CandidateFromEvent(event, nowMS); ok {
		t.Fatal("non-supply account must not enter the supplier recovery workflow")
	}
	event.AuthFileSnapshot = "supply-account.json"
	event.FailStatusCode = http.StatusTooManyRequests
	if _, ok := supplyAuth401CandidateFromEvent(event, nowMS); ok {
		t.Fatal("non-401 failure must not enter the supplier recovery workflow")
	}
}

func TestEmergencyOrderProcessingHonorsSupplierRetryDeadline(t *testing.T) {
	service := New(nil, nil)
	resource := SmartResource{
		EffectiveHealthyMinutes: 40,
		CriticalMinutes:         5,
		EstimatedSustainMinutes: 10,
		ConsumeRCUPerMinute:     100,
		CapacityGapRCU:          3_000,
	}
	order := store.SupplyOrder{Automatic: true, Status: "waiting_inventory"}
	if !service.emergencyOrderProcessingAllowed(store.ManagerSupplyConfig{}, order, resource) {
		t.Fatal("emergency order without a supplier retry deadline should bypass local poll pacing")
	}
	order.SupplierRetryUntilMS = time.Now().Add(10 * time.Second).UnixMilli()
	if service.emergencyOrderProcessingAllowed(store.ManagerSupplyConfig{}, order, resource) {
		t.Fatal("supplier retry_after deadline must not be bypassed by emergency processing")
	}
}

func TestAutomaticCreateCooldownUsesPersistedLatestOrder(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply-cooldown.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now()
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID:           "automatic-recent",
		Product:           "oauth_7d",
		RequestedQuantity: 6,
		Automatic:         true,
		Status:            "completed",
		CreatedAtMS:       now.Add(-5 * time.Second).UnixMilli(),
		CompletedAtMS:     now.Add(-4 * time.Second).UnixMilli(),
	}); err != nil {
		t.Fatalf("create automatic order: %v", err)
	}
	service := New(st, nil)
	active, err := service.automaticCreateCooldownActive(context.Background(), store.ManagerSupplyConfig{
		CreateCooldownSeconds: 120,
	}, SmartResource{EmergencyShortage: true})
	if err != nil {
		t.Fatalf("check persisted cooldown: %v", err)
	}
	if !active {
		t.Fatal("a recent persisted automatic order must keep emergency replenishment in cooldown")
	}
}

func TestAutomaticSupplyGuardRequiresFreshBaselineAndSettledImports(t *testing.T) {
	service := New(nil, nil)
	nowMS := time.Now().UnixMilli()
	service.automaticEnabled = true
	service.automaticBaselineAtMS = nowMS
	service.automaticAccountAtMS = nowMS
	service.inspectionSnapshotRefresh.refresh = func(context.Context) error { return nil }

	resource := SmartResource{SnapshotFresh: true, CapacitySnapshotAtMS: nowMS - 1}
	if reason := service.automaticSupplyGuardReason(resource); reason != "initial_capacity_snapshot_pending" {
		t.Fatalf("old capacity baseline reason = %q", reason)
	}
	resource.CapacitySnapshotAtMS = nowMS
	if reason := service.automaticSupplyGuardReason(resource); reason != "" {
		t.Fatalf("fresh baseline reason = %q", reason)
	}
	resource.PendingInspectionAccounts = 6
	if reason := service.automaticSupplyGuardReason(resource); reason != "pending_account_inspection" {
		t.Fatalf("pending import guard reason = %q", reason)
	}
}

func TestAccountPoolStatsSeparateTotalAvailableHealthyAndDisabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/auth-files" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"files":[
			{"name":"healthy.json","provider":"codex","status":"active"},
			{"name":"unknown-quota.json","provider":"codex","status":"active"},
			{"name":"disabled.json","provider":"codex","status":"disabled","disabled":true},
			{"name":"xai.json","provider":"xai","status":"active"}
		]}`))
	}))
	t.Cleanup(server.Close)
	service := New(nil, nil, server.Client())
	stats, err := service.countAccountPoolStats(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{
			CPABaseURL:    server.URL,
			ManagementKey: "management-key",
		},
	})
	if err != nil {
		t.Fatalf("count account pool: %v", err)
	}
	resource := SmartResource{HealthyAccounts: 1}
	applyAccountPoolStats(&resource, stats)
	if resource.TotalAccounts != 3 || resource.AvailableAccounts != 2 || resource.SchedulableAccounts != 2 ||
		resource.HealthyAccounts != 1 || resource.WeakAccounts != 1 || resource.DisabledAccounts != 1 {
		t.Fatalf("account pool statistics = %#v", resource)
	}
}

func TestAutomaticReplenishmentCreatesTakesAndImportsOrder(t *testing.T) {
	var createCalls atomic.Int32
	var takeCalls atomic.Int32
	var uploadCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":10,"missing":0,"estimated_total_fen":1000}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":10000,"balance_fen":10000}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			createCalls.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"order":{"id":"order-1","status":"ready","quantity":1},"status_url":"/custom/order-status","take_url":"/custom/order-take"}`))
		case r.URL.Path == "/custom/order-status" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"id":"order-1","status":"ready","ready_quantity":1,"progress":100}`))
		case r.URL.Path == "/custom/order-take":
			takeCalls.Add(1)
			_, _ = w.Write([]byte(`{"payload":{"accounts":[{"type":"codex","account":"new@example.com","access_token":"secret"}]},"status":"completed"}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			if r.Header.Get("Authorization") != "Bearer management-key" {
				t.Fatalf("management authorization = %q", r.Header.Get("Authorization"))
			}
			if name := r.URL.Query().Get("name"); name != "" {
				if uploadCalls.Load() > 0 {
					_, _ = w.Write([]byte(`{"files":[{"name":"` + name + `","provider":"codex","disabled":false,"status":"ready"}]}`))
				} else {
					_, _ = w.Write([]byte(`{"files":[]}`))
				}
			} else {
				_, _ = w.Write([]byte(`{"files":[{"name":"existing.json","provider":"codex","disabled":false,"status":"ready"}]}`))
			}
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodPost:
			uploadCalls.Add(1)
			part, err := r.MultipartReader()
			if err != nil {
				t.Fatalf("multipart reader: %v", err)
			}
			foundPayload := false
			for {
				item, err := part.NextPart()
				if err != nil {
					break
				}
				data, _ := io.ReadAll(item)
				if item.FormName() == "file" {
					foundPayload = len(data) > 0 && item.FileName() != ""
					var payload map[string]any
					if err := json.Unmarshal(data, &payload); err != nil || payload["type"] != "codex" {
						t.Fatalf("uploaded payload was not normalized CPA Codex JSON: %s", data)
					}
					if _, nested := payload["credentials"]; nested {
						t.Fatal("uploaded payload still contains Sub2 credentials wrapper")
					}
				}
			}
			if !foundPayload {
				t.Fatal("uploaded account payload is missing")
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
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
	enabled := true
	smartDisabled := false
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled:                 &enabled,
			BaseURL:                 server.URL,
			Username:                "customer",
			Password:                "password",
			Product:                 "oauth_30d",
			TargetAvailableAccounts: 2,
			ReplenishBatchSize:      1,
			CheckIntervalSeconds:    60,
			PollIntervalSeconds:     1,
			DefaultWebsockets:       true,
			SmartEnabled:            &smartDisabled,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	managerConfig := managerconfigsvc.New(config.Config{}, st, nil)
	service := New(st, managerConfig, server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("run automatic: %v", err)
	}
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("settle-window automatic run: %v", err)
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if createCalls.Load() != 1 || takeCalls.Load() != 1 || uploadCalls.Load() != 1 {
		t.Fatalf("calls create=%d take=%d upload=%d", createCalls.Load(), takeCalls.Load(), uploadCalls.Load())
	}
	if len(status.Orders) != 1 || status.Orders[0].Status != "completed" || status.Orders[0].ImportedCount != 1 {
		t.Fatalf("orders = %#v", status.Orders)
	}
	if status.Orders[0].StatusURL != "/custom/order-status" || status.Orders[0].TakeURL != "/custom/order-take" ||
		status.Orders[0].ReadyQuantity != 1 || status.Orders[0].Progress != 100 {
		t.Fatalf("persisted remote order details = %#v", status.Orders[0])
	}
	if status.Config.Password != "" || !status.Config.PasswordConfigured {
		t.Fatalf("sanitized config = %#v", status.Config)
	}
}

func TestRecoverySyncClaimsImportsAndDisablesOriginalAccount(t *testing.T) {
	var claimCalls atomic.Int32
	var uploadCalls atomic.Int32
	var disableCalls atomic.Int32
	uploadedNames := sync.Map{}

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/recoveries" && r.Method == http.MethodGet:
			if got := r.Header.Get("X-Customer-Token"); got != "customer-token" {
				t.Fatalf("recoveries token = %q", got)
			}
			_, _ = w.Write([]byte(`{"payload":{"recoveries":[{"id":"rec-1","delivery_status":"claimable","product":"oauth_30d","original_email":"old@example.com","original_account":"old.json","original_auth_index":"auth-1","claim_url":"` + server.URL + `/api/customer/recoveries/rec-1/claim?ticket=ticket-1"}]}}`))
		case r.URL.Path == "/api/customer/recoveries/rec-1/claim" && r.Method == http.MethodPost:
			claimCalls.Add(1)
			if got := r.URL.Query().Get("ticket"); got != "ticket-1" {
				t.Fatalf("claim ticket = %q", got)
			}
			_, _ = w.Write([]byte(`{"credential_version":2,"payload":{"type":"oauth","name":"replacement","platform":"openai","credentials":{"access_token":"access-new","refresh_token":"refresh-new","email":"new@example.com","chatgpt_account_id":"acct-new","chatgpt_plan_type":"team","workspace_id":"workspace-new"}}}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			if r.Header.Get("Authorization") != "Bearer management-key" {
				t.Fatalf("management authorization = %q", r.Header.Get("Authorization"))
			}
			name := r.URL.Query().Get("name")
			if name == "" {
				_, _ = w.Write([]byte(`{"files":[{"name":"old.json","provider":"codex","disabled":false,"status":"ready"}]}`))
				return
			}
			if _, ok := uploadedNames.Load(name); ok {
				_, _ = w.Write([]byte(`{"files":[{"name":"` + name + `","provider":"codex","disabled":false,"status":"ready"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"files":[]}`))
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
				if item.FormName() != "file" {
					continue
				}
				if !strings.Contains(item.FileName(), "codex-recovery-rec-1-v2-") {
					t.Fatalf("replacement filename = %q", item.FileName())
				}
				uploadedNames.Store(item.FileName(), struct{}{})
				data, _ := io.ReadAll(item)
				var payload map[string]any
				if err := json.Unmarshal(data, &payload); err != nil {
					t.Fatalf("decode upload payload %s: %v", data, err)
				}
				if payload["type"] != "codex" || payload["access_token"] != "access-new" ||
					payload["refresh_token"] != "refresh-new" || payload["email"] != "new@example.com" {
					t.Fatalf("uploaded payload was not normalized replacement JSON: %#v", payload)
				}
				if _, nested := payload["credentials"]; nested {
					t.Fatalf("uploaded payload still contains credentials wrapper: %#v", payload)
				}
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case r.URL.Path == "/v0/management/auth-files/status" && r.Method == http.MethodPatch:
			disableCalls.Add(1)
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode disable payload: %v", err)
			}
			if payload["name"] != "old.json" || payload["auth_index"] != "auth-1" || payload["disabled"] != true {
				t.Fatalf("disable payload = %#v", payload)
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply-recovery.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled:                     &enabled,
			BaseURL:                     server.URL,
			Username:                    "customer",
			Password:                    "password",
			Product:                     "oauth_30d",
			PollIntervalSeconds:         1,
			RecoverySyncEnabled:         &enabled,
			RecoveryAutoClaim:           &enabled,
			RecoverySyncIntervalSeconds: 60,
			RecoveryClaimBatchSize:      10,
			RecoveryDisableOriginal:     &enabled,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	autoClaim := true
	summary, err := service.SyncRecoveries(context.Background(), RecoverySyncRequest{
		Force:     true,
		AutoClaim: &autoClaim,
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("sync recoveries: %v", err)
	}
	if summary.Seen != 1 || summary.Claimed != 1 || summary.Imported != 1 ||
		summary.StoredImported != 1 || summary.LastResult != "completed" {
		t.Fatalf("summary = %#v", summary)
	}
	if claimCalls.Load() != 1 || uploadCalls.Load() != 1 || disableCalls.Load() != 1 {
		t.Fatalf("calls claim=%d upload=%d disable=%d", claimCalls.Load(), uploadCalls.Load(), disableCalls.Load())
	}
	recoveries, err := service.ListRecoveries(context.Background(), 10, "")
	if err != nil || len(recoveries) != 1 || recoveries[0].Status != "imported" ||
		recoveries[0].ImportedCount != 1 || recoveries[0].ClaimOrderID != "recovery-rec-1" ||
		recoveries[0].CredentialVersion != 2 || len(recoveries[0].ImportedFileNames) != 1 || recoveries[0].LastImportedAtMS <= 0 ||
		len(recoveries[0].ImportItems) != 1 || recoveries[0].ImportItems[0].Status != "imported" ||
		recoveries[0].ImportItems[0].FileName != recoveries[0].ImportedFileNames[0] {
		t.Fatalf("recoveries=%#v err=%v", recoveries, err)
	}
	order, found, err := st.GetSupplyOrder(context.Background(), "recovery-rec-1")
	if err != nil || !found || order.Status != "completed" || order.ImportedCount != 1 {
		t.Fatalf("recovery order=%#v found=%v err=%v", order, found, err)
	}
}

func TestRecoveryClaimConflictWaitsForFreshClaimURL(t *testing.T) {
	var listCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/recoveries" && r.Method == http.MethodGet:
			call := listCalls.Add(1)
			ticket := "old"
			if call > 1 {
				ticket = "fresh"
			}
			_, _ = w.Write([]byte(`{"recoveries":[{"id":"rec-conflict","delivery_status":"claimable","claim_url":"/api/customer/recoveries/rec-conflict/claim?ticket=` + ticket + `"}]}`))
		case r.URL.Path == "/api/customer/recoveries/rec-conflict/claim":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"ticket expired"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "recovery-conflict.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_30d",
			RecoverySyncEnabled: &enabled, RecoveryAutoClaim: &enabled,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	autoClaim := true
	if _, err := service.SyncRecoveries(context.Background(), RecoverySyncRequest{AutoClaim: &autoClaim}); err == nil {
		t.Fatal("expected stale ticket conflict")
	}
	recovery, found, err := st.GetSupplyRecovery(context.Background(), "rec-conflict")
	if err != nil || !found || recovery.Status != "seen" || !strings.Contains(recovery.LastError, "HTTP 409") {
		t.Fatalf("recovery=%#v found=%v err=%v", recovery, found, err)
	}
	autoClaim = false
	if _, err := service.SyncRecoveries(context.Background(), RecoverySyncRequest{AutoClaim: &autoClaim}); err != nil {
		t.Fatalf("refresh recovery URL: %v", err)
	}
	recovery, found, err = st.GetSupplyRecovery(context.Background(), "rec-conflict")
	if err != nil || !found || recovery.Status != "claimable" || !strings.Contains(recovery.ClaimURL, "ticket=fresh") || recovery.LastError != "" {
		t.Fatalf("refreshed recovery=%#v found=%v err=%v", recovery, found, err)
	}
}

func TestRecoveryPayloadServerErrorKeepsTicketClaimable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/recoveries" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"recoveries":[{"id":"rec-retry","delivery_status":"claimable","claim_url":"/api/customer/recoveries/rec-retry/claim?ticket=keep-me"}]}`))
		case r.URL.Path == "/api/customer/recoveries/rec-retry/claim":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"recovery_payload_invalid","message":"payload temporarily unavailable"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "recovery-server-retry.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_30d",
			RecoverySyncEnabled: &enabled, RecoveryAutoClaim: &enabled,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	autoClaim := true
	if _, err := service.SyncRecoveries(context.Background(), RecoverySyncRequest{AutoClaim: &autoClaim}); err == nil {
		t.Fatal("expected temporary claim failure")
	}
	recovery, found, err := st.GetSupplyRecovery(context.Background(), "rec-retry")
	if err != nil || !found || recovery.Status != "claimable" || !strings.Contains(recovery.ClaimURL, "ticket=keep-me") ||
		!strings.Contains(recovery.LastError, "recovery_payload_invalid") {
		t.Fatalf("recovery=%#v found=%v err=%v", recovery, found, err)
	}
}

func TestTakeReplacementFileRefreshesLatestClaimURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case "/api/customer/recoveries/rec-from-take":
			_, _ = w.Write([]byte(`{"recovery":{"id":"rec-from-take","delivery_status":"claimable","product":"oauth_30d","claim_url":"/api/customer/recoveries/rec-from-take/claim?ticket=latest","credential_version":3}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "take-replacement.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_30d",
	}}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.syncTakeReplacementFiles(context.Background(), cfg, []supplyclient.ReplacementFile{{
		RecoveryID: "rec-from-take", Ready: true, StatusURL: "/api/customer/recoveries/rec-from-take", CredentialVersion: 2,
	}}); err != nil {
		t.Fatalf("sync replacement: %v", err)
	}
	recovery, found, err := st.GetSupplyRecovery(context.Background(), "rec-from-take")
	if err != nil || !found || recovery.Status != "claimable" || recovery.CredentialVersion != 3 ||
		!strings.Contains(recovery.ClaimURL, "ticket=latest") {
		t.Fatalf("recovery=%#v found=%v err=%v", recovery, found, err)
	}
}

func TestRecoverySyncIntervalHonorsRetryAfter(t *testing.T) {
	interval := recoverySyncInterval(store.ManagerSupplyConfig{RecoverySyncIntervalSeconds: 60}, &supplyclient.HTTPError{
		StatusCode: http.StatusTooManyRequests, RetryAfterSeconds: 17,
	})
	if interval != 17*time.Second {
		t.Fatalf("interval = %s", interval)
	}
}

func TestRecoveryNextSyncIntervalDrainsAutomaticBacklog(t *testing.T) {
	cfg := store.ManagerSupplyConfig{RecoverySyncIntervalSeconds: 30}
	if interval := recoveryNextSyncInterval(cfg, nil, true, 9); interval != 3*time.Second {
		t.Fatalf("backlog interval = %s, want 3s", interval)
	}
	if interval := recoveryNextSyncInterval(cfg, nil, false, 9); interval != 30*time.Second {
		t.Fatalf("manual interval = %s, want 30s", interval)
	}
	retryErr := &supplyclient.HTTPError{StatusCode: http.StatusTooManyRequests, RetryAfterSeconds: 17}
	if interval := recoveryNextSyncInterval(cfg, retryErr, true, 9); interval != 17*time.Second {
		t.Fatalf("retry interval = %s, want 17s", interval)
	}
}

func TestReportAggregatesSupplySpendRecoveriesAndUsageRevenue(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "supply-report.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().Truncate(time.Second)
	fromMS := now.Add(-2 * time.Hour).UnixMilli()
	toMS := now.Add(2 * time.Hour).UnixMilli()
	orderCreated := now.Add(-45 * time.Minute)
	completedAtMS := orderCreated.Add(3 * time.Minute).UnixMilli()
	if _, err := st.CreateSupplyOrder(ctx, store.SupplyOrder{
		OrderID:           "order-report-1",
		Product:           "oauth_30d",
		RequestedQuantity: 2,
		Automatic:         true,
		Strategy:          managerconfigsvc.SupplyStrategyStrongSupply,
		TriggerReason:     "emergency_pool_vacuum",
		Status:            "completed",
		ChargedFen:        1200,
		ReleasedFen:       200,
		ItemCount:         2,
		ImportedCount:     2,
		CompletedAtMS:     completedAtMS,
		CreatedAtMS:       orderCreated.UnixMilli(),
	}); err != nil {
		t.Fatalf("create supply order: %v", err)
	}
	if inserted, err := st.InsertSupplyImportItems(ctx, "order-report-1", []store.SupplyImportItem{
		{OrderID: "order-report-1", ItemKey: "item-a", FileName: "codex-supply-a.json", PayloadJSON: `{"type":"codex"}`, LeaseExpiresAtMS: now.Add(10 * time.Minute).UnixMilli(), BasePriceFen: 700, ChargedFen: 500},
		{OrderID: "order-report-1", ItemKey: "item-b", FileName: "codex-supply-b.json", PayloadJSON: `{"type":"codex"}`, LeaseExpiresAtMS: now.Add(time.Hour).UnixMilli(), BasePriceFen: 700, ChargedFen: 700},
	}); err != nil || inserted != 2 {
		t.Fatalf("insert import items inserted=%d err=%v", inserted, err)
	}
	items, err := st.ListPendingSupplyImportItems(ctx, "order-report-1", now.UnixMilli(), 10)
	if err != nil || len(items) != 2 {
		t.Fatalf("pending items=%#v err=%v", items, err)
	}
	for _, item := range items {
		if err := st.MarkSupplyImportItemImported(ctx, item.ID, now.Add(-30*time.Minute).UnixMilli()); err != nil {
			t.Fatalf("mark item imported: %v", err)
		}
	}
	if _, err := st.UpsertSupplyRecoveries(ctx, []store.SupplyRecovery{
		{
			RecoveryID:     "rec-imported",
			Product:        "oauth_30d",
			DeliveryStatus: "claimable",
			Status:         "imported",
			ItemCount:      1,
			ImportedCount:  1,
			LastSeenAtMS:   now.Add(-35 * time.Minute).UnixMilli(),
			ClaimedAtMS:    now.Add(-34 * time.Minute).UnixMilli(),
		},
		{
			RecoveryID:     "rec-refunded",
			Product:        "oauth_30d",
			DeliveryStatus: "refunded",
			Status:         "refunded",
			RefundedFen:    300,
			LastSeenAtMS:   now.Add(-25 * time.Minute).UnixMilli(),
		},
	}); err != nil {
		t.Fatalf("upsert recoveries: %v", err)
	}
	if err := st.SaveModelPrices(ctx, map[string]store.ModelPrice{
		"gpt-report": {Prompt: 2, Completion: 4, Cache: 1},
	}); err != nil {
		t.Fatalf("save model prices: %v", err)
	}
	if _, err := st.InsertEvents(ctx, []usage.Event{
		supplyReportEvent("usage-report-1", now.Add(-20*time.Minute).UnixMilli(), "gpt-report", "codex-supply-a.json", false, 1_000_000, 500_000, 0, 0, 0, 1_500_000, nil),
		supplyReportEvent("usage-report-other", now.Add(-10*time.Minute).UnixMilli(), "gpt-report", "non-supply.json", false, 1_000_000, 500_000, 0, 0, 0, 1_500_000, nil),
	}); err != nil {
		t.Fatalf("insert usage event: %v", err)
	}
	candidate, err := st.UpsertAccountActionCandidate(ctx, store.AccountActionCandidateUpsert{
		ActionType:          "reauth",
		Provider:            "codex",
		AuthFileName:        "codex-supply-a.json",
		ReasonCode:          "invalid_401",
		Reason:              "supplier account returned 401",
		AutoDisableEligible: true,
		SeenAtMS:            now.Add(-15 * time.Minute).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("upsert 401 candidate: %v", err)
	}
	if err := st.MarkAccountActionCandidateAutoDisabled(ctx, candidate.ID, now.Add(-14*time.Minute).UnixMilli()); err != nil {
		t.Fatalf("mark 401 candidate auto disabled: %v", err)
	}

	report, err := New(st, nil).Report(ctx, ReportRequest{FromMS: fromMS, ToMS: toMS})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if report.Executive.Orders != 1 || report.Executive.AutomaticOrders != 1 ||
		report.Executive.RequestedAccounts != 2 || report.Executive.ImportedAccounts != 2 {
		t.Fatalf("executive order counts = %#v", report.Executive)
	}
	if report.Executive.EmergencyReplenishments != 1 || report.Executive.VacuumReplenishments != 1 ||
		report.Executive.Auth401Accounts != 1 || report.Executive.Auth401Events != 1 ||
		report.Executive.Auth401Rate != 1 || report.Executive.AutoQuarantined != 1 || report.Executive.VacuumTotalSeconds != 180 ||
		report.Executive.AverageVacuumRecoverySeconds != 180 {
		t.Fatalf("strategy and 401 metrics = %#v", report.Executive)
	}
	if report.Executive.ChargedFen != 1200 || report.Executive.ReleasedFen != 200 ||
		report.Executive.RefundedFen != 300 || report.Executive.SupplySpendFen != 1200 ||
		report.Executive.SupplyNetSpendFen != 900 || report.Executive.AverageUnitFen != 600 {
		t.Fatalf("executive money = %#v", report.Executive)
	}
	if report.Reconciliation.Summary.AllocationMethod != "supplier_item_exact_else_order_even_split" ||
		report.Reconciliation.Summary.AccountAllocatedChargedFen != 1200 ||
		report.Reconciliation.Summary.AccountAllocatedReleasedFen != 200 ||
		report.Reconciliation.Summary.AccountAllocatedNetFen != 1200 {
		t.Fatalf("reconciliation money = %#v", report.Reconciliation.Summary)
	}
	if report.Executive.Recoveries != 2 || report.Executive.ImportedRecoveries != 1 ||
		report.Executive.RefundedRecoveries != 1 || report.Executive.RecoveryClaimRate != 1 ||
		report.Executive.RecoveryImportRate != 1 || report.Executive.RecoveryRefundRate != 0.5 {
		t.Fatalf("executive recoveries = %#v", report.Executive)
	}
	if report.Executive.UsageCalls != 1 || report.Executive.UsageTokens != 1_500_000 ||
		report.Executive.UsageRevenueCurrency != "USD" ||
		math.Abs(report.Executive.UsageRevenue-0.24) > 0.000001 ||
		math.Abs(report.Executive.AverageRevenuePerCall-0.24) > 0.000001 {
		t.Fatalf("executive usage revenue = %#v", report.Executive)
	}
	if len(report.UsageModels) != 1 || report.UsageModels[0].Model != "gpt-report" ||
		report.UsageModels[0].Calls != 1 || math.Abs(report.UsageModels[0].Revenue-0.24) > 0.000001 {
		t.Fatalf("usage models = %#v", report.UsageModels)
	}
	if report.ImportHealth.Items != 2 || report.ImportHealth.ImportedItems != 2 ||
		report.ImportHealth.ExpiringSoonItems != 1 || report.Executive.ImportSuccessRate != 1 {
		t.Fatalf("import health = %#v executive=%#v", report.ImportHealth, report.Executive)
	}
	foundUsageTimeline := false
	for _, point := range report.Timeline {
		if point.UsageCalls == 1 {
			foundUsageTimeline = point.UsageTokens == 1_500_000 && math.Abs(point.UsageRevenue-0.24) <= 0.000001
		}
	}
	if !foundUsageTimeline {
		t.Fatalf("timeline missing usage revenue: %#v", report.Timeline)
	}
	if len(report.Products) == 0 || len(report.Strategies) == 0 || len(report.TriggerReasons) == 0 ||
		len(report.Sources) == 0 || len(report.RecoveryStatuses) == 0 ||
		len(report.DeliveryStatuses) == 0 || len(report.OrderStatuses) == 0 {
		t.Fatalf("dimension stats were not populated: %#v", report)
	}
}

func TestNormalizeReportRequestDefaultsToToday(t *testing.T) {
	before := time.Now()
	req := normalizeReportRequest(ReportRequest{})
	after := time.Now()
	from := time.UnixMilli(req.FromMS).In(time.Local)
	to := time.UnixMilli(req.ToMS).In(time.Local)
	if req.FromMS <= 0 || req.ToMS <= 0 || req.ToMS <= req.FromMS {
		t.Fatalf("normalized range is invalid: %#v", req)
	}
	if to.Before(before.Add(-2*time.Second)) || to.After(after.Add(2*time.Second)) {
		t.Fatalf("toMs=%s is not near now [%s, %s]", to, before, after)
	}
	if from.Year() != to.Year() || from.YearDay() != to.YearDay() ||
		from.Hour() != 0 || from.Minute() != 0 || from.Second() != 0 {
		t.Fatalf("fromMs=%s should be start of the same local day as toMs=%s", from, to)
	}
}

func TestSupplyAccountStatusReasonExplainsAbnormalStates(t *testing.T) {
	now := time.Date(2026, 8, 9, 21, 45, 0, 0, time.Local)
	failedReason := supplyAccountStatusReason("failed", store.SupplyImportItem{
		Status:    "failed",
		LastError: "upload rejected by CPA",
	}, cpaauthfiles.File{}, false, false, now)
	if failedReason != "upload rejected by CPA" {
		t.Fatalf("failed reason = %q", failedReason)
	}

	expiredAt := now.Add(-time.Minute).UnixMilli()
	expiredReason := supplyAccountStatusReason("expired", store.SupplyImportItem{
		Status:           "imported",
		LeaseExpiresAtMS: expiredAt,
	}, cpaauthfiles.File{}, true, true, now)
	if !strings.Contains(expiredReason, "过期") || !strings.Contains(expiredReason, "2026-08-09") {
		t.Fatalf("expired reason = %q", expiredReason)
	}

	missingReason := supplyAccountStatusReason("missing", store.SupplyImportItem{
		Status: "imported",
	}, cpaauthfiles.File{}, true, false, now)
	if !strings.Contains(missingReason, "未找到") {
		t.Fatalf("missing reason = %q", missingReason)
	}

	disabledReason := supplyAccountStatusReason("disabled", store.SupplyImportItem{
		Status: "imported",
	}, cpaauthfiles.File{
		Provider: "codex",
		Disabled: true,
		Raw: map[string]any{
			"disabled_reason": "OAuth 401 reauthorization required",
		},
	}, true, true, now)
	if disabledReason != "OAuth 401 reauthorization required" {
		t.Fatalf("disabled reason = %q", disabledReason)
	}

	quotaReason := supplyAccountStatusReason("disabled", store.SupplyImportItem{
		Status: "imported",
	}, cpaauthfiles.File{
		Provider: "codex",
		Raw: map[string]any{
			"status":     "ready",
			"error_kind": "usage_limit_reached",
		},
	}, true, true, now)
	if !strings.Contains(quotaReason, "usage_limit_reached") {
		t.Fatalf("quota reason = %q", quotaReason)
	}
}

func TestRecoverySyncImportsLocalPendingWhenSupplierRecoveriesFail(t *testing.T) {
	var uploadCalls atomic.Int32
	uploadedNames := sync.Map{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/recoveries" && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"message":"supplier recoveries temporarily unavailable"}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			name := r.URL.Query().Get("name")
			if name == "" {
				_, _ = w.Write([]byte(`{"files":[]}`))
				return
			}
			if _, ok := uploadedNames.Load(name); ok {
				_, _ = w.Write([]byte(`{"files":[{"name":"` + name + `","provider":"codex","disabled":false,"status":"ready"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"files":[]}`))
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
					uploadedNames.Store(item.FileName(), struct{}{})
				}
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply-recovery-local-pending.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	disabled := false
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			BaseURL:                 server.URL,
			Username:                "customer",
			Password:                "password",
			Product:                 "oauth_30d",
			RecoverySyncEnabled:     &enabled,
			RecoveryAutoClaim:       &disabled,
			RecoveryDisableOriginal: &disabled,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "recovery-local-pending", Product: "oauth_30d", RequestedQuantity: 1,
		Automatic: true, Status: "recovery_importing", RemoteStatus: "recovery_claimed", ItemCount: 1,
	}); err != nil {
		t.Fatalf("create recovery order: %v", err)
	}
	if _, err := st.InsertSupplyImportItems(context.Background(), "recovery-local-pending", []store.SupplyImportItem{{
		OrderID: "recovery-local-pending", ItemKey: "pending-key", FileName: "codex-supply-local.json",
		PayloadJSON: `{"type":"oauth","platform":"openai","credentials":{"access_token":"access","refresh_token":"refresh","account_id":"account-local","email":"local@example.com"}}`,
	}}); err != nil {
		t.Fatalf("insert import item: %v", err)
	}
	if _, err := st.UpsertSupplyRecoveries(context.Background(), []store.SupplyRecovery{{
		RecoveryID: "local-pending", Product: "oauth_30d", DeliveryStatus: "claimable", Status: "importing",
		ClaimOrderID: "recovery-local-pending", ItemCount: 1, LastSeenAtMS: time.Now().UnixMilli(),
	}}); err != nil {
		t.Fatalf("upsert recovery: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	autoClaim := false
	if _, err := service.SyncRecoveries(context.Background(), RecoverySyncRequest{AutoClaim: &autoClaim}); err == nil {
		t.Fatal("sync should surface supplier recoveries failure after processing local pending imports")
	}
	if uploadCalls.Load() != 1 {
		t.Fatalf("upload calls = %d, want 1", uploadCalls.Load())
	}
	order, found, err := st.GetSupplyOrder(context.Background(), "recovery-local-pending")
	if err != nil || !found || order.Status != "completed" || order.ImportedCount != 1 {
		t.Fatalf("order=%#v found=%v err=%v", order, found, err)
	}
	recovery, found, err := st.GetSupplyRecovery(context.Background(), "local-pending")
	if err != nil || !found || recovery.Status != "imported" || recovery.ImportedCount != 1 {
		t.Fatalf("recovery=%#v found=%v err=%v", recovery, found, err)
	}
}

func TestSupplyRecoveryRefundedStatusOverridesLocalImportingState(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply-recovery-refund.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	nowMS := time.Now().UnixMilli()
	if _, err := st.UpsertSupplyRecoveries(context.Background(), []store.SupplyRecovery{{
		RecoveryID: "rec-refund-priority", Product: "oauth_30d", DeliveryStatus: "claimable",
		Status: "partial", ClaimOrderID: "recovery-rec-refund-priority", ItemCount: 2, ImportedCount: 1, LastSeenAtMS: nowMS,
	}}); err != nil {
		t.Fatalf("insert recovery: %v", err)
	}
	if _, err := st.UpsertSupplyRecoveries(context.Background(), []store.SupplyRecovery{{
		RecoveryID: "rec-refund-priority", Product: "oauth_30d", DeliveryStatus: "refunded",
		Status: "refunded", RefundedFen: 900, LastSeenAtMS: nowMS + 1,
	}}); err != nil {
		t.Fatalf("refund recovery: %v", err)
	}
	recovery, found, err := st.GetSupplyRecovery(context.Background(), "rec-refund-priority")
	if err != nil || !found || recovery.Status != "refunded" || recovery.RefundedFen != 900 {
		t.Fatalf("recovery=%#v found=%v err=%v", recovery, found, err)
	}
}

func TestRecoveryIntervalCanDriveWorkerWhenAutoSupplyDisabled(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply-recovery-interval.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		Supply: store.ManagerSupplyConfig{
			BaseURL:                     "https://sogouedu.cc",
			Username:                    "customer",
			Password:                    "password",
			Product:                     "oauth_30d",
			RecoverySyncEnabled:         &enabled,
			RecoverySyncIntervalSeconds: 10,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), nil)
	service.recoveryState.NextSyncAtMS = time.Now().Add(10 * time.Second).UnixMilli()
	if interval := service.NextInterval(context.Background()); interval > 11*time.Second {
		t.Fatalf("interval = %s, want recovery interval to be honored", interval)
	}
}

func TestRecoverySyncDoesNotClaimBeforeCPAConnectionIsConfigured(t *testing.T) {
	var claimCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/recoveries" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"payload":{"recoveries":[{"id":"rec-wait","delivery_status":"claimable","claim_url":"/api/customer/recoveries/rec-wait/claim?ticket=ticket"}]}}`))
		case r.URL.Path == "/api/customer/recoveries/rec-wait/claim":
			claimCalls.Add(1)
			t.Fatal("claim should wait until CPA management connection is configured")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply-recovery-no-cpa.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		Supply: store.ManagerSupplyConfig{
			BaseURL:             server.URL,
			Username:            "customer",
			Password:            "password",
			Product:             "oauth_30d",
			RecoverySyncEnabled: &enabled,
			RecoveryAutoClaim:   &enabled,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	autoClaim := true
	summary, err := service.SyncRecoveries(context.Background(), RecoverySyncRequest{AutoClaim: &autoClaim})
	if err != nil {
		t.Fatalf("sync recoveries: %v", err)
	}
	if summary.Seen != 1 || summary.Claimed != 0 || summary.Claimable != 1 || claimCalls.Load() != 0 {
		t.Fatalf("summary=%#v claimCalls=%d", summary, claimCalls.Load())
	}
}

func TestCreateResultUncertainRetriesWithPersistedIdempotencyKey(t *testing.T) {
	var createCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/pickup/orders":
			createCalls.Add(1)
			if key := r.Header.Get("Idempotency-Key"); key != "create-attempt-persisted" {
				t.Fatalf("idempotency key = %q", key)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"order":{"id":"supplier-order-1","status":"waiting_inventory","quantity":1}}`))
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
	enabled := true
	smartDisabled := false
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, BaseURL: server.URL, Username: "customer", Password: "password",
			Product: "oauth_30d", TargetAvailableAccounts: 1, ReplenishBatchSize: 1,
			SmartEnabled: &smartDisabled,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "create-attempt-persisted", Product: "oauth_30d", RequestedQuantity: 1,
		Automatic: true, Strategy: "strong_supply", TriggerReason: "emergency_pool_vacuum",
		Status: "create_uncertain", LastError: "request timed out after supplier accepted it",
	}); err != nil {
		t.Fatalf("create uncertain attempt: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("retry automatic run: %v", err)
	}
	if createCalls.Load() != 1 {
		t.Fatalf("create calls = %d, want 1", createCalls.Load())
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil || status.ActiveOrder == nil || status.ActiveOrder.Status == "create_uncertain" || status.ActiveOrder.OrderID != "supplier-order-1" {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestOrderConflictIsPersistedAsCancelled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case "/api/customer/pickup/orders/order-cancelled":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"order cancelled"}`))
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
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		Enabled: &enabled, BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_30d",
	}}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "order-cancelled", Product: "oauth_30d", RequestedQuantity: 1, Status: "waiting_inventory",
	}); err != nil {
		t.Fatalf("create local order: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("process cancelled order: %v", err)
	}
	order, found, err := st.GetSupplyOrder(context.Background(), "order-cancelled")
	if err != nil || !found || order.Status != "cancelled" || order.CompletedAtMS == 0 {
		t.Fatalf("order=%#v found=%v err=%v", order, found, err)
	}
}

func TestManualReplenishmentRejectsConcurrentOrder(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "active-order", Product: "oauth_30d", RequestedQuantity: 2, Status: "waiting_inventory",
	}); err != nil {
		t.Fatalf("create active order: %v", err)
	}
	managerConfig := managerconfigsvc.New(config.Config{}, st, nil)
	service := New(st, managerConfig)
	_, err = service.Replenish(context.Background(), 2)
	if err != ErrOrderInProgress {
		t.Fatalf("replenish error = %v, want %v", err, ErrOrderInProgress)
	}
}

func TestNormalizeSub2AccountPayloadForCPA(t *testing.T) {
	raw := `{"name":"team-user","type":"oauth","platform":"openai","priority":2,"concurrency":8,"extra":{"organization_id":"org-extra","lastRefresh":"2026-07-01T00:00:00Z"},"credentials":{"access_token":"access","refresh_token":"refresh","chatgpt_account_id":"account-1","email":"user@example.com","plan_type":"free","chatgpt_plan_type":"team","workspaceId":"workspace-1","expires_at":"2026-07-30T00:00:00Z"}}`
	payload, key, fileName, err := normalizeAccountPayload([]byte(raw))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode normalized payload: %v", err)
	}
	if result["type"] != "codex" || result["access_token"] != "access" || result["refresh_token"] != "refresh" {
		t.Fatalf("normalized credentials = %#v", result)
	}
	if _, ok := result["credentials"]; ok {
		t.Fatalf("Sub2 credentials wrapper was not removed: %#v", result)
	}
	if result["account_id"] != "account-1" || result["chatgpt_account_id"] != "account-1" ||
		result["email"] != "user@example.com" || result["plan_type"] != "team" ||
		result["chatgpt_plan_type"] != "team" || result["organization_id"] != "org-extra" ||
		result["workspace_id"] != "workspace-1" ||
		result["expired"] != "2026-07-30T00:00:00Z" || result["max_concurrency"] != float64(8) {
		t.Fatalf("normalized metadata = %#v", result)
	}
	if len(key) != 64 || len(fileName) != len("codex-supply-")+20+len(".json") || fileName[:13] != "codex-supply-" {
		t.Fatalf("stable identity outputs key=%q file=%q", key, fileName)
	}
}

func TestNormalizeSub2BundlePayloadForCPA(t *testing.T) {
	raw := `{"type":"sub2api-data","exported_at":"2026-07-30T17:28:18Z","accounts":[{"name":"team-one","type":"oauth","platform":"openai","priority":2,"concurrency":8,"credentials":{"access_token":"access-one","refresh_token":"refresh-one","chatgpt_account_id":"account-shared","email":"one@example.com","plan_type":"team","expires_at":1786296161,"expires_in":864000,"workspace_id":"workspace-one"}},{"name":"team-two","type":"oauth","platform":"openai","credentials":{"session_access_token":"access-two","refresh_token":"refresh-two","account_id":"account-shared","email":"two@example.com","chatgpt_plan_type":"team"}}]}`
	accounts, err := normalizeAccountPayloads([]byte(raw))
	if err != nil {
		t.Fatalf("normalize bundle: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("normalized accounts = %d, want 2", len(accounts))
	}
	if accounts[0].fileName == accounts[1].fileName || accounts[0].itemKey == accounts[1].itemKey {
		t.Fatalf("bundle accounts should keep distinct identities: %#v", accounts)
	}
	var first map[string]any
	if err := json.Unmarshal(accounts[0].payload, &first); err != nil {
		t.Fatalf("decode first payload: %v", err)
	}
	if first["type"] != "codex" || first["import_format"] != "sub2api" || first["access_token"] != "access-one" || first["refresh_token"] != "refresh-one" {
		t.Fatalf("first normalized payload = %#v", first)
	}
	if _, nested := first["credentials"]; nested {
		t.Fatalf("credentials wrapper was not removed: %#v", first)
	}
	if first["chatgpt_account_id"] != "account-shared" || first["account_id"] != "account-shared" || first["email"] != "one@example.com" || first["workspace_id"] != "workspace-one" {
		t.Fatalf("credential metadata was not preserved: %#v", first)
	}
	if first["expired"] != "1786296161" || first["expires_at"] != "1786296161" || first["last_refresh"] != "2026-07-30T17:28:18Z" {
		t.Fatalf("time metadata was not normalized: %#v", first)
	}
	var second map[string]any
	if err := json.Unmarshal(accounts[1].payload, &second); err != nil {
		t.Fatalf("decode second payload: %v", err)
	}
	if second["access_token"] != "access-two" || second["account_id"] != "account-shared" || second["email"] != "two@example.com" {
		t.Fatalf("session access token account was not normalized: %#v", second)
	}
}

func TestSupplyDeliveryLeaseUsesRemainingValidityInsteadOfOAuthExpiry(t *testing.T) {
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	shortLease := supplyDeliveryLeaseExpiresAtMS(map[string]any{
		"remaining_seconds": 900,
		"expires_at":        now.Add(24 * time.Hour).Unix(),
	}, now)
	if got, want := shortLease, now.Add(15*time.Minute).UnixMilli(); got != want {
		t.Fatalf("short supplier lease = %d, want %d", got, want)
	}
	defaultLease := supplyDeliveryLeaseExpiresAtMS(map[string]any{
		"expires_at": now.Add(24 * time.Hour).Unix(),
	}, now)
	if got, want := defaultLease, now.Add(time.Hour).UnixMilli(); got != want {
		t.Fatalf("OAuth expiry must not extend supplier lease: got %d want %d", got, want)
	}
}

func TestSupplyOrderItemLeasesRequireExactExpandedAccountMapping(t *testing.T) {
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	accounts := []normalizedSupplyAccount{
		{leaseExpiresAtMS: now.Add(time.Hour).UnixMilli()},
		{leaseExpiresAtMS: now.Add(time.Hour).UnixMilli()},
	}
	if !applySupplyOrderItemLeases(accounts, []int64{900, 1800}, now) {
		t.Fatal("exactly expanded accounts should accept ordered item leases")
	}
	if got, want := accounts[0].leaseExpiresAtMS, now.Add(15*time.Minute).UnixMilli(); got != want {
		t.Fatalf("first lease = %d, want %d", got, want)
	}
	if got, want := accounts[1].leaseExpiresAtMS, now.Add(30*time.Minute).UnixMilli(); got != want {
		t.Fatalf("second lease = %d, want %d", got, want)
	}
	if !applySupplyOrderItemDetails(accounts, []supplyclient.OrderItem{
		{RemainingSeconds: 600, HasRemaining: true, BasePriceFen: 400, ChargedFen: 100},
		{RemainingSeconds: 1200, HasRemaining: true, BasePriceFen: 400, ChargedFen: 200},
	}, now) {
		t.Fatal("exactly expanded accounts should accept ordered item prices")
	}
	if accounts[0].basePriceFen != 400 || accounts[0].chargedFen != 100 ||
		accounts[1].basePriceFen != 400 || accounts[1].chargedFen != 200 {
		t.Fatalf("account costs were not assigned: %#v", accounts)
	}
	original := accounts[0].leaseExpiresAtMS
	if applySupplyOrderItemLeases(accounts, []int64{300}, now) {
		t.Fatal("mismatched order items must not be assigned to expanded accounts")
	}
	if accounts[0].leaseExpiresAtMS != original {
		t.Fatalf("mismatched mapping changed original lease: %d", accounts[0].leaseExpiresAtMS)
	}
}

func TestTakeResponseSub2BundleIsExpandedAndUploadedAsCPACodex(t *testing.T) {
	var takeCalls atomic.Int32
	var uploadCalls atomic.Int32
	uploadedNames := sync.Map{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-bundle" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"id":"order-bundle","status":"ready","ready_quantity":2,"progress":100,"take_url":"/custom/take-bundle"}`))
		case r.URL.Path == "/custom/take-bundle" && r.Method == http.MethodPost:
			takeCalls.Add(1)
			_, _ = w.Write([]byte(`{"order":{"id":"order-bundle","status":"completed","items":[{"remaining_seconds":900,"base_price_fen":400,"charged_fen":100},{"remaining_seconds":1800,"base_price_fen":400,"charged_fen":200}]},"payload":{"accounts":[{"type":"sub2api-data","exported_at":"2026-07-30T17:28:18Z","accounts":[{"name":"team-one","type":"oauth","platform":"openai","credentials":{"access_token":"access-one","refresh_token":"refresh-one","chatgpt_account_id":"account-one","email":"one@example.com","plan_type":"team"}},{"name":"team-two","type":"oauth","platform":"openai","credentials":{"session_access_token":"access-two","refresh_token":"refresh-two","account_id":"account-two","email":"two@example.com","chatgpt_plan_type":"team"}}]}]}}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			name := r.URL.Query().Get("name")
			if name == "" {
				_, _ = w.Write([]byte(`{"files":[]}`))
				return
			}
			if _, ok := uploadedNames.Load(name); ok {
				_, _ = w.Write([]byte(`{"files":[{"name":"` + name + `","provider":"codex","disabled":false,"status":"ready"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"files":[]}`))
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
				if item.FormName() != "file" {
					continue
				}
				uploadedNames.Store(item.FileName(), struct{}{})
				data, _ := io.ReadAll(item)
				var payload map[string]any
				if err := json.Unmarshal(data, &payload); err != nil {
					t.Fatalf("decode upload payload %s: %v", data, err)
				}
				if payload["type"] != "codex" || payload["import_format"] != "sub2api" || payload["access_token"] == "" || payload["refresh_token"] == "" {
					t.Fatalf("uploaded payload was not CPA Codex JSON: %#v", payload)
				}
				if _, nested := payload["credentials"]; nested {
					t.Fatalf("uploaded payload still contains credentials wrapper: %#v", payload)
				}
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
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
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			BaseURL: server.URL, Username: "customer", Password: "password",
			Product: "oauth_30d", PollIntervalSeconds: 1,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "order-bundle", Product: "oauth_30d", RequestedQuantity: 2, Status: "ready", TakeURL: "/custom/take-bundle",
	}); err != nil {
		t.Fatalf("create order: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("run automatic: %v", err)
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if takeCalls.Load() != 1 || uploadCalls.Load() != 2 {
		t.Fatalf("calls take=%d upload=%d", takeCalls.Load(), uploadCalls.Load())
	}
	if len(status.Orders) != 1 || status.Orders[0].Status != "completed" || status.Orders[0].ImportedCount != 2 || status.Orders[0].ItemCount != 2 {
		t.Fatalf("orders = %#v", status.Orders)
	}
	items, err := st.ListActiveImportedSupplyItems(context.Background(), time.Now().UnixMilli())
	if err != nil || len(items) != 2 {
		t.Fatalf("active imported items=%#v err=%v", items, err)
	}
	for index, expected := range []int64{900, 1800} {
		actualSeconds := (items[index].LeaseExpiresAtMS - time.Now().UnixMilli()) / 1000
		if actualSeconds < expected-2 || actualSeconds > expected+1 {
			t.Fatalf("item %d lease seconds=%d, want approximately %d; items=%#v", index, actualSeconds, expected, items)
		}
	}
	importedItems, err := st.ListSupplyImportItems(context.Background(), 10, "imported")
	if err != nil || len(importedItems) != 2 {
		t.Fatalf("imported items=%#v err=%v", importedItems, err)
	}
	costs := map[int64]int64{}
	for _, item := range importedItems {
		costs[item.ChargedFen] = item.BasePriceFen
	}
	if costs[100] != 400 || costs[200] != 400 {
		t.Fatalf("imported item costs=%#v items=%#v", costs, importedItems)
	}
}

func TestTakingLeasePreventsDuplicateTake(t *testing.T) {
	var takeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case "/api/customer/pickup/orders/order-taking", "/api/customer/pickup/orders/order-taking/take":
			takeCalls.Add(1)
			t.Fatal("taking lease should block status polling and take retry")
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
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		Supply: store.ManagerSupplyConfig{BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_30d"},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	leaseUntil := time.Now().Add(time.Minute).UnixMilli()
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "order-taking", Product: "oauth_30d", RequestedQuantity: 1, Status: "taking", NextPollAtMS: leaseUntil,
	}); err != nil {
		t.Fatalf("create taking order: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("run automatic: %v", err)
	}
	if takeCalls.Load() != 0 {
		t.Fatalf("take/status calls = %d, want 0", takeCalls.Load())
	}
}

func TestTimedOutTakingOrderRetriesWithoutAutomaticRelease(t *testing.T) {
	var takeCalls atomic.Int32
	var releaseCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-timeout" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"id":"order-timeout","status":"ready","ready_quantity":1,"progress":100}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-timeout/take" && r.Method == http.MethodPost:
			takeCalls.Add(1)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"order-timeout","status":"waiting_inventory","retry_after_seconds":1}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-timeout" && r.Method == http.MethodDelete:
			releaseCalls.Add(1)
			t.Fatal("a timed-out take attempt must reconcile and retry, not release the reserved order")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply-timeout.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		Supply: store.ManagerSupplyConfig{
			BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_30d", PollIntervalSeconds: 1,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "order-timeout", Product: "oauth_30d", RequestedQuantity: 1, Automatic: true,
		Status: "taking", NextPollAtMS: time.Now().Add(-time.Second).UnixMilli(),
	}); err != nil {
		t.Fatalf("create taking order: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("reconcile timed-out take: %v", err)
	}
	order, found, err := st.GetSupplyOrder(context.Background(), "order-timeout")
	if err != nil || !found {
		t.Fatalf("load order found=%v err=%v", found, err)
	}
	if takeCalls.Load() != 1 || releaseCalls.Load() != 0 || order.Status != "waiting_inventory" {
		t.Fatalf("take=%d release=%d order=%#v", takeCalls.Load(), releaseCalls.Load(), order)
	}
}

func TestClaimSupplyOrderTakingAllowsOnlyOneWorker(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "order-claim", Product: "oauth_30d", RequestedQuantity: 1, Status: "ready",
	}); err != nil {
		t.Fatalf("create order: %v", err)
	}
	nowMS := time.Now().UnixMilli()
	leaseUntilMS := nowMS + int64(time.Minute/time.Millisecond)
	claimed, err := st.ClaimSupplyOrderTaking(context.Background(), "order-claim", nowMS, leaseUntilMS)
	if err != nil || !claimed {
		t.Fatalf("first claim=%v err=%v", claimed, err)
	}
	claimed, err = st.ClaimSupplyOrderTaking(context.Background(), "order-claim", nowMS, leaseUntilMS)
	if err != nil || claimed {
		t.Fatalf("second claim=%v err=%v, want false nil", claimed, err)
	}
	order, found, err := st.GetSupplyOrder(context.Background(), "order-claim")
	if err != nil || !found || order.Status != "taking" || order.NextPollAtMS != leaseUntilMS {
		t.Fatalf("order=%#v found=%v err=%v", order, found, err)
	}
}

func TestImportVerificationFailureBlocksDuplicateAutomaticOrders(t *testing.T) {
	var createCalls atomic.Int32
	var uploadCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":10,"missing":0,"estimated_total_fen":1000}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":10000,"balance_fen":10000}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			createCalls.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"order":{"id":"order-verify-fail","status":"ready","quantity":1},"take_url":"/custom/take-verify-fail"}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-verify-fail" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"id":"order-verify-fail","status":"ready","ready_quantity":1}`))
		case r.URL.Path == "/custom/take-verify-fail":
			_, _ = w.Write([]byte(`{"payload":{"accounts":[{"type":"oauth","platform":"openai","credentials":{"access_token":"access","refresh_token":"refresh","account_id":"account-verify","email":"verify@example.com"}}]},"status":"completed"}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"files":[]}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodPost:
			uploadCalls.Add(1)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
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
	enabled := true
	smartDisabled := false
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled:                 &enabled,
			BaseURL:                 server.URL,
			Username:                "customer",
			Password:                "password",
			Product:                 "oauth_30d",
			TargetAvailableAccounts: 11,
			ReplenishBatchSize:      1,
			CheckIntervalSeconds:    60,
			PollIntervalSeconds:     1,
			SmartEnabled:            &smartDisabled,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err == nil {
		t.Fatal("first automatic run succeeded despite missing CPA registration")
	}
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("second automatic run should keep the open partial order without creating a new one: %v", err)
	}
	if createCalls.Load() != 1 || uploadCalls.Load() != 1 {
		t.Fatalf("calls create=%d upload=%d", createCalls.Load(), uploadCalls.Load())
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil || status.ActiveOrder == nil || status.ActiveOrder.Status != "partial" {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestAutomaticOrderLocallyReleasesWhenCPATargetIsAlreadySatisfied(t *testing.T) {
	var createCalls atomic.Int32
	var authListCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":10,"missing":0,"estimated_total_fen":1000}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":10000,"balance_fen":10000}`))
		case r.URL.Path == "/api/customer/pickup/orders" && r.Method == http.MethodPost:
			createCalls.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"order":{"id":"order-release","status":"waiting_inventory","quantity":1}}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-release" && r.Method == http.MethodGet:
			t.Fatal("locally released order must not be polled")
		case r.URL.Path == "/api/customer/pickup/orders/order-release/take":
			t.Fatal("locally released order must not be taken")
		case (r.URL.Path == "/api/customer/pickup/orders/order-release" && r.Method == http.MethodDelete) ||
			r.URL.Path == "/api/customer/pickup/orders/order-release/cancel" ||
			r.URL.Path == "/api/customer/pickup/orders/order-release/release":
			t.Fatal("local automatic release must not call a supplier cancellation endpoint")
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			call := authListCalls.Add(1)
			if call == 1 {
				_, _ = w.Write([]byte(`{"files":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"files":[{"name":"existing.json","provider":"codex","disabled":false,"status":"ready"}]}`))
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
	enabled := true
	smartDisabled := false
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled:                 &enabled,
			BaseURL:                 server.URL,
			Username:                "customer",
			Password:                "password",
			Product:                 "oauth_30d",
			TargetAvailableAccounts: 1,
			ReplenishBatchSize:      1,
			CheckIntervalSeconds:    60,
			PollIntervalSeconds:     1,
			SmartEnabled:            &smartDisabled,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("create automatic order: %v", err)
	}
	order, found, err := st.GetSupplyOrder(context.Background(), "order-release")
	if err != nil || !found {
		t.Fatalf("load created order found=%v err=%v", found, err)
	}
	order.NextPollAtMS = 0
	if err := st.UpdateSupplyOrder(context.Background(), order); err != nil {
		t.Fatalf("make order due: %v", err)
	}
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("locally release satisfied order: %v", err)
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status.ActiveOrder != nil {
		t.Fatalf("active order should be cleared: %#v", status.ActiveOrder)
	}
	if len(status.Orders) != 1 || status.Orders[0].Status != "released" ||
		status.Orders[0].RemoteStatus != remoteStatusAutomaticReleasePending || status.Orders[0].ReleasedFen != 0 {
		t.Fatalf("orders = %#v", status.Orders)
	}
	if createCalls.Load() != 1 {
		t.Fatalf("create calls = %d, want 1", createCalls.Load())
	}
}

func TestAutomaticOrderTakesWhenCapacityStillNeededWithoutReleaseProbe(t *testing.T) {
	var takeCalls atomic.Int32
	var uploadCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-reserved" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"id":"order-reserved","status":"ready","ready_quantity":10,"progress":100,"take_url":"/api/customer/pickup/orders/order-reserved/take"}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-reserved/take" && r.Method == http.MethodPost:
			takeCalls.Add(1)
			_, _ = w.Write([]byte(`{"payload":{"accounts":[{"type":"codex","account":"reserved@example.com","access_token":"secret"}]},"status":"completed"}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			if name := r.URL.Query().Get("name"); name != "" {
				if uploadCalls.Load() > 0 {
					_, _ = w.Write([]byte(`{"files":[{"name":"` + name + `","provider":"codex","disabled":false,"status":"ready"}]}`))
					return
				}
				_, _ = w.Write([]byte(`{"files":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"files":[]}`))
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodPost:
			uploadCalls.Add(1)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
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
	enabled := true
	smartDisabled := false
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, BaseURL: server.URL, Username: "customer", Password: "password", Product: "oauth_7d",
			TargetAvailableAccounts: 1, PollIntervalSeconds: 1, SmartEnabled: &smartDisabled,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "order-reserved", Product: "oauth_7d", RequestedQuantity: 10, Automatic: true,
		Status: "ready", ReadyQuantity: 10, Progress: 100,
	}); err != nil {
		t.Fatalf("create order: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("first automatic run: %v", err)
	}
	order, found, err := st.GetSupplyOrder(context.Background(), "order-reserved")
	if err != nil || !found {
		t.Fatalf("load order found=%v err=%v", found, err)
	}
	if err != nil || !found || order.Status != "completed" || order.ImportedCount != 1 {
		t.Fatalf("needed order was not imported: %#v found=%v err=%v", order, found, err)
	}
	if takeCalls.Load() != 1 || uploadCalls.Load() != 1 {
		t.Fatalf("take=%d upload=%d, want 1/1", takeCalls.Load(), uploadCalls.Load())
	}
}

func TestStoreReactivatesLocallyReleasedUnsupportedOrder(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "old-unsupported-release", Product: "oauth_7d", RequestedQuantity: 10, Automatic: true,
		Status: "released", RemoteStatus: "release_unsupported", Progress: 100,
	}); err != nil {
		t.Fatalf("create legacy order: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "newer-open-order", Product: "oauth_7d", RequestedQuantity: 5, Automatic: true, Status: "waiting_inventory",
	}); err != nil {
		t.Fatalf("create newer open order: %v", err)
	}
	order, found, err := st.ActivateNextUnsupportedSupplyRelease(context.Background())
	if err != nil || !found || order.Status != "ready" || order.CompletedAtMS != 0 {
		t.Fatalf("legacy unsupported release was not reactivated: %#v found=%v err=%v", order, found, err)
	}
}

func TestLegacySupplyImportRepairConvertsAndVerifiesCPAFile(t *testing.T) {
	var uploadCalls atomic.Int32
	var uploadedName string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/auth-files" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			name := r.URL.Query().Get("name")
			if name != "" && uploadCalls.Load() > 0 {
				_, _ = w.Write([]byte(`{"files":[{"name":"` + name + `","provider":"codex","status":"ready"}]}`))
			} else {
				_, _ = w.Write([]byte(`{"files":[]}`))
			}
		case http.MethodPost:
			part, err := r.MultipartReader()
			if err != nil {
				t.Fatalf("multipart reader: %v", err)
			}
			for {
				item, nextErr := part.NextPart()
				if nextErr != nil {
					break
				}
				if item.FormName() == "file" {
					uploadedName = item.FileName()
					data, _ := io.ReadAll(item)
					var normalized map[string]any
					if err := json.Unmarshal(data, &normalized); err != nil || normalized["type"] != "codex" {
						t.Fatalf("legacy payload was not normalized: %s", data)
					}
				}
			}
			uploadCalls.Add(1)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			response := http.StatusMethodNotAllowed
			w.WriteHeader(response)
		}
	}))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply:        store.ManagerSupplyConfig{Product: "oauth_30d"},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	order, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "legacy-order", Product: "oauth_30d", RequestedQuantity: 1, Automatic: true, Status: "completed",
	})
	if err != nil {
		t.Fatalf("create legacy order: %v", err)
	} else {
		order.ItemCount = 1
		order.ImportedCount = 1
		order.CompletedAtMS = time.Now().UnixMilli()
		if err := st.UpdateSupplyOrder(context.Background(), order); err != nil {
			t.Fatalf("update legacy order: %v", err)
		}
	}
	if _, err := st.InsertSupplyImportItems(context.Background(), "legacy-order", []store.SupplyImportItem{{
		OrderID: "legacy-order", ItemKey: "legacy-key", FileName: "supply-legacy-key.json",
		PayloadJSON: `{"name":"legacy","type":"oauth","platform":"openai","credentials":{"access_token":"access","refresh_token":"refresh","account_id":"account-legacy","email":"legacy@example.com"}}`,
	}}); err != nil {
		t.Fatalf("insert legacy item: %v", err)
	}
	if err := st.MarkSupplyImportItemImported(context.Background(), 1, time.Now().UnixMilli()); err != nil {
		t.Fatalf("mark legacy item imported: %v", err)
	}

	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("repair legacy import: %v", err)
	}
	repaired, found, err := st.GetSupplyOrder(context.Background(), "legacy-order")
	if err != nil || !found || repaired.Status != "completed" || repaired.ImportedCount != 1 {
		t.Fatalf("repaired order=%#v found=%v err=%v", repaired, found, err)
	}
	if uploadCalls.Load() != 1 || len(uploadedName) < 13 || uploadedName[:13] != "codex-supply-" {
		t.Fatalf("upload calls=%d uploaded name=%q", uploadCalls.Load(), uploadedName)
	}
}

func TestSupplyOrderDatabaseAllowsOnlyOneOpenOrder(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "supply.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "open-1", Product: "oauth_30d", RequestedQuantity: 1, Status: "waiting_inventory",
	}); err != nil {
		t.Fatalf("create first open order: %v", err)
	}
	if _, err := st.CreateSupplyOrder(context.Background(), store.SupplyOrder{
		OrderID: "open-2", Product: "oauth_30d", RequestedQuantity: 1, Status: "creating",
	}); err == nil {
		t.Fatal("second open order was accepted")
	}
}

func TestHydrateOverviewIfNeededRestoresSupplierSnapshotAfterRestart(t *testing.T) {
	var inventoryCalls atomic.Int32
	var balanceCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case "/api/customer/inventory":
			inventoryCalls.Add(1)
			_, _ = w.Write([]byte(`{"available":7,"missing":0,"estimated_total_fen":7000}`))
		case "/api/customer/balance":
			balanceCalls.Add(1)
			_, _ = w.Write([]byte(`{"balance_fen":12000,"held_fen":2000,"available_fen":10000,"currency":"CNY"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	service := New(nil, nil, server.Client())
	cfg := store.ManagerSupplyConfig{
		BaseURL:            server.URL,
		Username:           "customer",
		Password:           "password",
		Product:            "oauth_30d",
		ReplenishBatchSize: 5,
	}
	service.hydrateOverviewIfNeeded(context.Background(), cfg)

	service.stateMu.RLock()
	overview := service.overview
	service.stateMu.RUnlock()
	if overview.Inventory == nil || overview.Inventory.Available != 7 ||
		overview.Balance == nil || overview.Balance.AvailableFen != 10_000 || overview.CheckedAtMS <= 0 {
		t.Fatalf("cold-start overview was not restored: %#v", overview)
	}

	service.hydrateOverviewIfNeeded(context.Background(), cfg)
	if inventoryCalls.Load() != 1 || balanceCalls.Load() != 1 {
		t.Fatalf("complete overview must not refetch on each UI poll: inventory=%d balance=%d", inventoryCalls.Load(), balanceCalls.Load())
	}
}

func supplyReportEvent(
	hash string,
	timestampMS int64,
	model string,
	authFile string,
	failed bool,
	inputTokens int64,
	outputTokens int64,
	reasoningTokens int64,
	cachedTokens int64,
	cacheTokens int64,
	totalTokens int64,
	latencyMS *int64,
) usage.Event {
	return usage.Event{
		EventHash:        hash,
		TimestampMS:      timestampMS,
		Timestamp:        time.UnixMilli(timestampMS).UTC().Format(time.RFC3339Nano),
		Model:            model,
		Endpoint:         "POST /v1/chat/completions",
		Method:           "POST",
		Path:             "/v1/chat/completions",
		AuthIndex:        "auth-1",
		AuthFileSnapshot: authFile,
		Source:           "ops@example.com",
		SourceHash:       "source-hash",
		APIKeyHash:       "api-key-hash",
		AccountSnapshot:  "ops@example.com",
		InputTokens:      inputTokens,
		OutputTokens:     outputTokens,
		ReasoningTokens:  reasoningTokens,
		CachedTokens:     cachedTokens,
		CacheTokens:      cacheTokens,
		TotalTokens:      totalTokens,
		LatencyMS:        latencyMS,
		Failed:           failed,
		CreatedAtMS:      timestampMS,
	}
}
