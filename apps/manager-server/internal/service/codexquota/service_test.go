package codexquota

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	quotaoperationrepo "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/codexquotaoperation"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpaauthfiles"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

type staticSetupResolver struct {
	setup store.Setup
}

func (r staticSetupResolver) ResolveSetup(context.Context) (store.Setup, bool, error) {
	return r.setup, true, nil
}

type staticAuthFiles struct {
	file cpaauthfiles.File
}

func (f staticAuthFiles) Find(context.Context, string, string, string, string) (cpaauthfiles.File, bool, error) {
	return f.file, true, nil
}

func (f staticAuthFiles) Fetch(context.Context, string, string) ([]cpaauthfiles.File, error) {
	return []cpaauthfiles.File{f.file}, nil
}

type recordingAuthStatuses struct {
	mu            sync.Mutex
	file          cpaauthfiles.File
	patches       []bool
	patchFailures int
	patchErr      error
}

func (s *recordingAuthStatuses) ResolveVerifiedStatusMutationTarget(
	context.Context,
	string,
	string,
	cpaauthfiles.Identity,
) (cpaauthfiles.StatusMutationTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cpaauthfiles.StatusMutationTarget{
		Selector: s.file.ID,
		File:     s.file,
		Scope:    cpaauthfiles.StatusMutationScopeCredential,
	}, nil
}

func (s *recordingAuthStatuses) PatchDisabledTarget(
	_ context.Context,
	_ string,
	_ string,
	_ cpaauthfiles.StatusMutationTarget,
	disabled bool,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.patchFailures > 0 {
		s.patchFailures--
		if s.patchErr != nil {
			return s.patchErr
		}
		return errors.New("injected status patch failure")
	}
	s.file.Disabled = disabled
	s.patches = append(s.patches, disabled)
	return nil
}

type recordingGateway struct {
	mu                      sync.Mutex
	usageCalls              int
	consumeCalls            int
	localResetCalls         int
	resetCreditCalls        int
	consumeErr              error
	consumeAccepted         bool
	usageInitiallyAvailable bool
	usageExplicitAvailable  bool
	resetCreditsAvailable   int
}

type failOnceUpdateRepository struct {
	quotaoperationrepo.Repository
	mu        sync.Mutex
	remaining int
}

func (r *failOnceUpdateRepository) Update(ctx context.Context, operation model.CodexQuotaOperation) (model.CodexQuotaOperation, error) {
	r.mu.Lock()
	if r.remaining > 0 {
		r.remaining--
		r.mu.Unlock()
		return model.CodexQuotaOperation{}, errors.New("injected operation persistence failure")
	}
	r.mu.Unlock()
	return r.Repository.Update(ctx, operation)
}

func (g *recordingGateway) usage(context.Context, store.Setup, string, string) (apiCallResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.usageCalls++
	used := 100
	limitReached := true
	if g.consumeAccepted || g.usageInitiallyAvailable {
		used = 0
		limitReached = false
	}
	if g.usageExplicitAvailable {
		limitReached = false
	}
	body, _ := json.Marshal(map[string]any{
		"rate_limit": map[string]any{
			"allowed":        true,
			"limit_reached":  limitReached,
			"primary_window": map[string]any{"used_percent": used},
		},
	})
	return apiCallResult{StatusCode: 200, Body: body}, nil
}

func (g *recordingGateway) resetCredits(context.Context, store.Setup, string, string) (apiCallResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resetCreditCalls++
	available := g.resetCreditsAvailable
	if available == 0 && g.resetCreditCalls == 1 {
		available = 1
	}
	body, _ := json.Marshal(map[string]any{"available_count": available, "credits": []any{}})
	return apiCallResult{StatusCode: 200, Body: body}, nil
}

func TestAutoResetCreditChecksEligibilityBeforeCreatingOperation(t *testing.T) {
	tests := []struct {
		name             string
		usageAvailable   bool
		creditsAvailable int
		wantEligible     bool
		wantReason       string
		wantConsume      int
	}{
		{name: "exhausted with credit", creditsAvailable: 2, wantEligible: true, wantReason: "reset_started", wantConsume: 1},
		{name: "quota already available", usageAvailable: true, creditsAvailable: 2, wantEligible: true, wantReason: "quota_already_recovered"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "auto-reset.sqlite"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			defer st.Close()
			gateway := &recordingGateway{
				usageInitiallyAvailable: tt.usageAvailable,
				resetCreditsAvailable:   tt.creditsAvailable,
			}
			service := &Service{
				operations:   st.CodexQuotaOperations,
				setupService: staticSetupResolver{setup: store.Setup{CPAUpstreamURL: "http://cpa", ManagementKey: "key"}},
				authFiles: staticAuthFiles{file: cpaauthfiles.File{
					Name: "codex.json", AuthIndex: "auth-1", Provider: "codex", AccountID: "ACCOUNT-1",
					Raw: map[string]any{"runtime_current_concurrency": float64(0)},
				}},
				gateway: gateway,
				locks:   newAccountLocks(),
			}
			response, eligibility, err := service.AutoResetCredit(context.Background(), ResetRequest{
				AuthIndex: "auth-1", OperationID: "d8b34d78-cfe5-5ad2-9f45-d8a5d5f1b530",
			})
			if err != nil {
				t.Fatalf("auto reset: %v", err)
			}
			if eligibility.Eligible != tt.wantEligible || eligibility.Reason != tt.wantReason || gateway.consumeCalls != tt.wantConsume {
				t.Fatalf("response=%#v eligibility=%#v consume=%d", response, eligibility, gateway.consumeCalls)
			}
			if tt.usageAvailable && response.State != model.CodexQuotaOperationStateCompleted {
				t.Fatalf("response state=%q, want completed", response.State)
			}
		})
	}
}

func TestAutoResetCreditRecoversExplicitlyAllowedQuotaPreempt(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "explicit-available.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	file := cpaauthfiles.File{
		ID: "runtime-explicit-available", Name: "codex.json", AuthIndex: "auth-explicit-available",
		Provider: "codex", AccountSnapshot: "available@example.com", AccountID: "ACCOUNT-AVAILABLE",
		Disabled: true,
		Raw: map[string]any{
			"runtime_last_skip_reason":    "usage_limit_reached",
			"runtime_current_concurrency": json.Number("0"),
		},
	}
	statuses := &recordingAuthStatuses{file: file}
	service := &Service{
		operations:        st.CodexQuotaOperations,
		setupService:      staticSetupResolver{setup: store.Setup{CPAUpstreamURL: "http://cpa", ManagementKey: "key"}},
		authFiles:         staticAuthFiles{file: file},
		authStatuses:      statuses,
		gateway:           &recordingGateway{usageExplicitAvailable: true},
		authFileMutations: cpaauthfiles.NewMutationCoordinator(),
		locks:             newAccountLocks(),
	}

	response, eligibility, err := service.AutoResetCredit(context.Background(), ResetRequest{
		AuthIndex: file.AuthIndex, OperationID: "2e1119c6-f9d0-4b75-a39d-6c6cf5a1a1a4",
	})
	if err != nil || !eligibility.Eligible || eligibility.Reason != "quota_already_recovered" || response.State != model.CodexQuotaOperationStateCompleted {
		t.Fatalf("response=%#v eligibility=%#v err=%v", response, eligibility, err)
	}
	statuses.mu.Lock()
	patches := append([]bool(nil), statuses.patches...)
	statuses.mu.Unlock()
	if len(patches) != 1 || patches[0] {
		t.Fatalf("status patches=%v, want one enable", patches)
	}
}

func TestAutoResetCreditRejectsActiveRequests(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "auto-reset-active-requests.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	gateway := &recordingGateway{resetCreditsAvailable: 2}
	service := &Service{
		operations:   st.CodexQuotaOperations,
		setupService: staticSetupResolver{setup: store.Setup{CPAUpstreamURL: "http://cpa", ManagementKey: "key"}},
		authFiles: staticAuthFiles{file: cpaauthfiles.File{
			Name: "codex.json", AuthIndex: "auth-active", Provider: "codex", AccountID: "ACCOUNT-ACTIVE",
			Raw: map[string]any{"runtime_current_concurrency": float64(1)},
		}},
		gateway: gateway,
		locks:   newAccountLocks(),
	}
	response, eligibility, err := service.AutoResetCredit(context.Background(), ResetRequest{
		AuthIndex: "auth-active", OperationID: "d8b34d78-cfe5-5ad2-9f45-d8a5d5f1b530",
	})
	if err != nil {
		t.Fatalf("auto reset: %v", err)
	}
	if response.State != "" || eligibility.Eligible || eligibility.Reason != "active_requests" {
		t.Fatalf("response=%#v eligibility=%#v", response, eligibility)
	}
	if gateway.consumeCalls != 0 || gateway.usageCalls != 0 || gateway.resetCreditCalls != 0 {
		t.Fatalf("gateway calls usage=%d credits=%d consume=%d, want none", gateway.usageCalls, gateway.resetCreditCalls, gateway.consumeCalls)
	}
}

func TestAvailableResetCreditCountAcceptsCreditInventory(t *testing.T) {
	body := json.RawMessage(`{"credits":[{"id":"credit-1","status":"available"},{"id":"credit-2","status":"available"},{"id":"credit-3","status":"consumed"}]}`)
	if got := availableResetCreditCount(body); got != 2 {
		t.Fatalf("availableResetCreditCount=%d, want 2", got)
	}
	for _, test := range []struct {
		name string
		body string
		want int64
	}{
		{name: "camel case count", body: `{"availableCount":"3"}`, want: 3},
		{name: "nested inventory", body: `{"rate_limit_reset_credits":{"available_count":4}}`, want: 4},
		{name: "nested credits array", body: `{"data":{"credits":[{"status":"available"},{"status":"consumed"}]}}`, want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := availableResetCreditCount(json.RawMessage(test.body)); got != test.want {
				t.Fatalf("availableResetCreditCount=%d, want %d", got, test.want)
			}
		})
	}
}

func TestCurrentRequestCountUsesHighestObservedConcurrency(t *testing.T) {
	count, ok := currentRequestCount(map[string]any{
		"runtime_current_concurrency": float64(0),
		"active_requests":             json.Number("2"),
	})
	if !ok || count != 2 {
		t.Fatalf("currentRequestCount=%d, ok=%v, want 2,true", count, ok)
	}
}

func TestInspectResetCreditsRequiresExhaustedQuotaCreditAndZeroRequests(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reset-inspection.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	gateway := &recordingGateway{resetCreditsAvailable: 1}
	service := &Service{
		operations:   st.CodexQuotaOperations,
		setupService: staticSetupResolver{setup: store.Setup{CPAUpstreamURL: "http://cpa", ManagementKey: "key"}},
		authFiles: staticAuthFiles{file: cpaauthfiles.File{
			Name: "codex.json", AuthIndex: "auth-1", Provider: "codex", AccountID: "ACCOUNT-1",
			Raw: map[string]any{"runtime_current_concurrency": float64(0)},
		}},
		gateway: gateway,
		locks:   newAccountLocks(),
	}
	items, err := service.InspectResetCredits(context.Background())
	if err != nil {
		t.Fatalf("inspect reset credits: %v", err)
	}
	if len(items) != 1 || !items[0].Eligible || items[0].AvailableCount != 1 || items[0].CurrentRequests == nil || *items[0].CurrentRequests != 0 {
		t.Fatalf("inspection items=%#v", items)
	}
}

func TestInspectResetCreditsAcceptsJSONNumberZeroConcurrency(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reset-inspection-json-number.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	service := &Service{
		operations:   st.CodexQuotaOperations,
		setupService: staticSetupResolver{setup: store.Setup{CPAUpstreamURL: "http://cpa", ManagementKey: "key"}},
		authFiles: staticAuthFiles{file: cpaauthfiles.File{
			Name: "codex.json", AuthIndex: "auth-json-number", Provider: "codex", AccountID: "ACCOUNT-JSON-NUMBER",
			Raw: map[string]any{"runtime_current_concurrency": json.Number("0")},
		}},
		gateway: &recordingGateway{resetCreditsAvailable: 1},
		locks:   newAccountLocks(),
	}
	items, err := service.InspectResetCredits(context.Background())
	if err != nil {
		t.Fatalf("inspect reset credits: %v", err)
	}
	if len(items) != 1 || !items[0].Eligible || items[0].CurrentRequests == nil || *items[0].CurrentRequests != 0 {
		t.Fatalf("inspection items=%#v", items)
	}
}

func (g *recordingGateway) consumeResetCredit(context.Context, store.Setup, string, string, string) (apiCallResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.consumeCalls++
	if g.consumeErr != nil {
		return apiCallResult{}, g.consumeErr
	}
	g.consumeAccepted = true
	return apiCallResult{StatusCode: 200, Body: json.RawMessage(`{"status":"accepted"}`)}, nil
}

func TestResetCreditDoesNotRepeatAmbiguousConsume(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "quota-unknown.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	gateway := &recordingGateway{consumeErr: errors.New("connection reset after write")}
	service := &Service{
		operations:   st.CodexQuotaOperations,
		setupService: staticSetupResolver{setup: store.Setup{CPAUpstreamURL: "http://cpa", ManagementKey: "key"}},
		authFiles: staticAuthFiles{file: cpaauthfiles.File{
			Name: "codex.json", AuthIndex: "auth-1", Provider: "codex", AccountID: "ACCOUNT-1",
		}},
		gateway: gateway,
		locks:   newAccountLocks(),
	}
	request := ResetRequest{AuthIndex: "auth-1", OperationID: "c0f34e71-9952-44ec-8fa1-644326962fe9"}
	first, err := service.ResetCredit(context.Background(), request)
	if err != nil || first.State != "consume_status_unknown" || first.Consumed != nil {
		t.Fatalf("first ambiguous result=%#v err=%v", first, err)
	}
	second, err := service.ResetCredit(context.Background(), request)
	if err != nil || second.State != "consume_status_unknown" || second.Consumed != nil {
		t.Fatalf("second ambiguous result=%#v err=%v", second, err)
	}
	if gateway.consumeCalls != 1 {
		t.Fatalf("ambiguous consume calls=%d, want 1", gateway.consumeCalls)
	}
}

func (g *recordingGateway) resetLocalQuota(context.Context, store.Setup, string) (json.RawMessage, int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.localResetCalls++
	return json.RawMessage(`{"status":"ok"}`), 200, nil
}

func TestResetCreditCompletesOnceAndReplaysStoredResult(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "quota-service.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	gateway := &recordingGateway{}
	service := &Service{
		operations:   st.CodexQuotaOperations,
		setupService: staticSetupResolver{setup: store.Setup{CPAUpstreamURL: "http://cpa", ManagementKey: "key"}},
		authFiles: staticAuthFiles{file: cpaauthfiles.File{
			Name: "codex.json", AuthIndex: "auth-1", Provider: "codex", AccountID: "ACCOUNT-1",
		}},
		gateway: gateway,
		locks:   newAccountLocks(),
	}
	request := ResetRequest{AuthIndex: "auth-1", OperationID: "025f897e-6e47-4d7d-a06f-6cf3b8315d78"}
	first, err := service.ResetCredit(context.Background(), request)
	if err != nil {
		t.Fatalf("reset credit: %v", err)
	}
	if first.State != "completed" || first.Consumed == nil || !*first.Consumed || first.Result == nil || !first.Result.Verified {
		t.Fatalf("first result = %#v", first)
	}
	second, err := service.ResetCredit(context.Background(), request)
	if err != nil || second.State != "completed" {
		t.Fatalf("replay result=%#v err=%v", second, err)
	}
	if gateway.consumeCalls != 1 || gateway.localResetCalls != 1 {
		t.Fatalf("gateway calls consume=%d local-reset=%d", gateway.consumeCalls, gateway.localResetCalls)
	}
	if first.AccountKey == "codex:account-id:account-1" {
		t.Fatalf("account key leaked raw account identity: %q", first.AccountKey)
	}
}

func TestResetCreditReleasesOwnedQuotaCooldownAndEnablesAuthFile(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "quota-cooldown-release.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	file := cpaauthfiles.File{
		ID:              "runtime-codex-1",
		Name:            "codex.json",
		AuthIndex:       "auth-1",
		Provider:        "codex",
		AccountSnapshot: "codex@example.com",
		AccountID:       "ACCOUNT-1",
		Disabled:        true,
	}
	if _, err := st.QuotaCooldowns.UpsertActive(context.Background(), model.QuotaCooldownUpsert{
		AuthFileName:    file.Name,
		AuthIndex:       file.AuthIndex,
		AccountSnapshot: file.AccountSnapshot,
		Provider:        file.Provider,
		Owner:           model.QuotaCooldownOwnerUsage429,
		RecoverAtMS:     time.Now().Add(time.Hour).UnixMilli(),
		DisabledAtMS:    time.Now().Add(-time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatalf("seed cooldown: %v", err)
	}
	statuses := &recordingAuthStatuses{file: file}
	gateway := &recordingGateway{}
	service := &Service{
		operations:        st.CodexQuotaOperations,
		setupService:      staticSetupResolver{setup: store.Setup{CPAUpstreamURL: "http://cpa", ManagementKey: "key"}},
		authFiles:         staticAuthFiles{file: file},
		authStatuses:      statuses,
		gateway:           gateway,
		quotaCooldowns:    st.QuotaCooldowns,
		authFileMutations: cpaauthfiles.NewMutationCoordinator(),
		locks:             newAccountLocks(),
	}

	result, err := service.ResetCredit(context.Background(), ResetRequest{
		AuthIndex:   file.AuthIndex,
		OperationID: "85c9f054-5586-4c6b-8480-42391d0e0fc7",
	})
	if err != nil || result.State != model.CodexQuotaOperationStateCompleted {
		t.Fatalf("reset result=%#v err=%v", result, err)
	}
	statuses.mu.Lock()
	patches := append([]bool(nil), statuses.patches...)
	disabled := statuses.file.Disabled
	statuses.mu.Unlock()
	if len(patches) != 1 || patches[0] || disabled {
		t.Fatalf("status patches=%v disabled=%t, want one enable", patches, disabled)
	}
	active, err := st.QuotaCooldowns.ListActive(context.Background())
	if err != nil {
		t.Fatalf("list active cooldowns: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("active cooldowns=%#v, want none", active)
	}
}

func TestResetCreditLeavesManuallyDisabledAuthFileUntouched(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "quota-manual-disabled.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	file := cpaauthfiles.File{
		ID:              "runtime-codex-manual",
		Name:            "manual.json",
		AuthIndex:       "auth-manual",
		Provider:        "codex",
		AccountSnapshot: "manual@example.com",
		AccountID:       "ACCOUNT-MANUAL",
		Disabled:        true,
	}
	statuses := &recordingAuthStatuses{file: file}
	service := &Service{
		operations:        st.CodexQuotaOperations,
		setupService:      staticSetupResolver{setup: store.Setup{CPAUpstreamURL: "http://cpa", ManagementKey: "key"}},
		authFiles:         staticAuthFiles{file: file},
		authStatuses:      statuses,
		gateway:           &recordingGateway{},
		quotaCooldowns:    st.QuotaCooldowns,
		authFileMutations: cpaauthfiles.NewMutationCoordinator(),
		locks:             newAccountLocks(),
	}

	result, err := service.ResetCredit(context.Background(), ResetRequest{
		AuthIndex:   file.AuthIndex,
		OperationID: "58f6758e-fe85-421d-9de3-dd4b24efcdac",
	})
	if err != nil || result.State != model.CodexQuotaOperationStateCompleted {
		t.Fatalf("reset result=%#v err=%v", result, err)
	}
	statuses.mu.Lock()
	defer statuses.mu.Unlock()
	if len(statuses.patches) != 0 || !statuses.file.Disabled {
		t.Fatalf("status patches=%v disabled=%t, want manual disable preserved", statuses.patches, statuses.file.Disabled)
	}
}

func TestResetCreditReleasesCooldownWithoutConsumingWhenQuotaAlreadyRecovered(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "quota-already-recovered.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	file := cpaauthfiles.File{
		ID:              "runtime-codex-recovered",
		Name:            "recovered.json",
		AuthIndex:       "auth-recovered",
		Provider:        "codex",
		AccountSnapshot: "recovered@example.com",
		AccountID:       "ACCOUNT-RECOVERED",
		Disabled:        true,
	}
	if _, err := st.QuotaCooldowns.UpsertActive(context.Background(), model.QuotaCooldownUpsert{
		AuthFileName:    file.Name,
		AuthIndex:       file.AuthIndex,
		AccountSnapshot: file.AccountSnapshot,
		Provider:        file.Provider,
		Owner:           model.QuotaCooldownOwnerUsage429,
		RecoverAtMS:     time.Now().Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatalf("seed cooldown: %v", err)
	}
	statuses := &recordingAuthStatuses{file: file}
	gateway := &recordingGateway{usageInitiallyAvailable: true}
	service := &Service{
		operations:        st.CodexQuotaOperations,
		setupService:      staticSetupResolver{setup: store.Setup{CPAUpstreamURL: "http://cpa", ManagementKey: "key"}},
		authFiles:         staticAuthFiles{file: file},
		authStatuses:      statuses,
		gateway:           gateway,
		quotaCooldowns:    st.QuotaCooldowns,
		authFileMutations: cpaauthfiles.NewMutationCoordinator(),
		locks:             newAccountLocks(),
	}

	result, err := service.ResetCredit(context.Background(), ResetRequest{
		AuthIndex:   file.AuthIndex,
		OperationID: "85e44e2b-3079-47ca-9716-4666579d015f",
	})
	if err != nil || result.State != model.CodexQuotaOperationStateCompleted {
		t.Fatalf("reset result=%#v err=%v", result, err)
	}
	if result.Consumed == nil || *result.Consumed {
		t.Fatalf("consumed=%v, want false", result.Consumed)
	}
	if gateway.consumeCalls != 0 || gateway.localResetCalls != 1 {
		t.Fatalf("gateway calls consume=%d local-reset=%d", gateway.consumeCalls, gateway.localResetCalls)
	}
	statuses.mu.Lock()
	patches := append([]bool(nil), statuses.patches...)
	statuses.mu.Unlock()
	if len(patches) != 1 || patches[0] {
		t.Fatalf("status patches=%v, want one enable", patches)
	}
}

func TestResetCreditClearsStaleRuntimeQuotaPreemptFreeze(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "quota-preempt-recovery.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	file := cpaauthfiles.File{
		ID:              "runtime-codex-preempt",
		Name:            "preempt.json",
		AuthIndex:       "auth-preempt",
		Provider:        "codex",
		AccountSnapshot: "preempt@example.com",
		AccountID:       "ACCOUNT-PREEMPT",
		Disabled:        true,
		Raw: map[string]any{
			"runtime_last_skip_reason":    "quota_preempt",
			"runtime_current_concurrency": json.Number("0"),
		},
	}
	statuses := &recordingAuthStatuses{file: file}
	service := &Service{
		operations:        st.CodexQuotaOperations,
		setupService:      staticSetupResolver{setup: store.Setup{CPAUpstreamURL: "http://cpa", ManagementKey: "key"}},
		authFiles:         staticAuthFiles{file: file},
		authStatuses:      statuses,
		gateway:           &recordingGateway{usageInitiallyAvailable: true},
		authFileMutations: cpaauthfiles.NewMutationCoordinator(),
		locks:             newAccountLocks(),
	}

	result, err := service.ResetCredit(context.Background(), ResetRequest{
		AuthIndex:   file.AuthIndex,
		OperationID: "f6ef8cbf-c5d5-5c4b-86f0-5c3f5d6e6ee2",
	})
	if err != nil || result.State != model.CodexQuotaOperationStateCompleted {
		t.Fatalf("reset result=%#v err=%v", result, err)
	}
	statuses.mu.Lock()
	patches := append([]bool(nil), statuses.patches...)
	disabled := statuses.file.Disabled
	statuses.mu.Unlock()
	if len(patches) != 1 || patches[0] || disabled {
		t.Fatalf("status patches=%v disabled=%t, want one enable", patches, disabled)
	}
}

func TestResetCreditRetriesRuntimeQuotaPreemptRecoveryWhenResumed(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "quota-preempt-retry.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	file := cpaauthfiles.File{
		ID:              "runtime-codex-preempt-retry",
		Name:            "preempt-retry.json",
		AuthIndex:       "auth-preempt-retry",
		Provider:        "codex",
		AccountSnapshot: "preempt-retry@example.com",
		AccountID:       "ACCOUNT-PREEMPT-RETRY",
		Disabled:        true,
		Raw: map[string]any{
			"runtime_last_skip_reason":    "quota_preempt",
			"runtime_current_concurrency": json.Number("0"),
		},
	}
	statuses := &recordingAuthStatuses{file: file, patchFailures: 1, patchErr: errors.New("temporary status failure")}
	service := &Service{
		operations:        st.CodexQuotaOperations,
		setupService:      staticSetupResolver{setup: store.Setup{CPAUpstreamURL: "http://cpa", ManagementKey: "key"}},
		authFiles:         staticAuthFiles{file: file},
		authStatuses:      statuses,
		gateway:           &recordingGateway{usageInitiallyAvailable: true},
		authFileMutations: cpaauthfiles.NewMutationCoordinator(),
		locks:             newAccountLocks(),
	}

	request := ResetRequest{AuthIndex: file.AuthIndex, OperationID: "a2e1f1e4-05d9-5d12-9640-369ab4b1581a"}
	first, err := service.ResetCredit(context.Background(), request)
	if err != nil || first.State != model.CodexQuotaOperationStateLocallyRecovered {
		t.Fatalf("first reset result=%#v err=%v, want resumable local recovery", first, err)
	}
	second, err := service.ResetCredit(context.Background(), request)
	if err != nil || second.State != model.CodexQuotaOperationStateCompleted {
		t.Fatalf("resumed reset result=%#v err=%v, want completed", second, err)
	}
	statuses.mu.Lock()
	patches := append([]bool(nil), statuses.patches...)
	disabled := statuses.file.Disabled
	statuses.mu.Unlock()
	if len(patches) != 1 || patches[0] || disabled {
		t.Fatalf("status patches=%v disabled=%t, want one successful enable on resume", patches, disabled)
	}
}

func TestResetCreditReplaysAfterAcceptedConsumePersistenceFailure(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "quota-persistence-replay.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	gateway := &recordingGateway{}
	service := &Service{
		operations: &failOnceUpdateRepository{
			Repository: st.CodexQuotaOperations,
			remaining:  1,
		},
		setupService: staticSetupResolver{setup: store.Setup{CPAUpstreamURL: "http://cpa", ManagementKey: "key"}},
		authFiles: staticAuthFiles{file: cpaauthfiles.File{
			Name: "codex.json", AuthIndex: "auth-1", Provider: "codex", AccountID: "ACCOUNT-1",
		}},
		gateway: gateway,
		locks:   newAccountLocks(),
	}
	request := ResetRequest{AuthIndex: "auth-1", OperationID: "251146dd-f03c-43d9-bb1d-7b13fe800b04"}
	if _, err = service.ResetCredit(context.Background(), request); err == nil {
		t.Fatal("first reset should expose the injected persistence failure")
	}
	second, err := service.ResetCredit(context.Background(), request)
	if err != nil || second.State != "completed" || second.Consumed == nil || !*second.Consumed {
		t.Fatalf("replayed result=%#v err=%v", second, err)
	}
	if gateway.consumeCalls != 1 || gateway.localResetCalls != 1 {
		t.Fatalf("gateway calls consume=%d local-reset=%d", gateway.consumeCalls, gateway.localResetCalls)
	}
}

func TestCodexUsageLimitStateUsesRecoveryThreshold(t *testing.T) {
	observed, limited := codexUsageLimitState(json.RawMessage(`{
		"rate_limit":{"allowed":true,"primary_window":{"used_percent":91}}
	}`), 90)
	if !observed || !limited {
		t.Fatalf("91 percent should be treated as low quota: observed=%v limited=%v", observed, limited)
	}
	observed, limited = codexUsageLimitState(json.RawMessage(`{
		"rate_limit":{"allowed":true,"primary_window":{"used_percent":2}}
	}`), 90)
	if !observed || limited {
		t.Fatalf("2 percent should be recovered: observed=%v limited=%v", observed, limited)
	}
}

func TestCodexUsageLimitStateTrustsExplicitAllowedState(t *testing.T) {
	observed, limited := codexUsageLimitState(json.RawMessage(`{
		"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":100}}
	}`), 90)
	if !observed || limited {
		t.Fatalf("explicitly allowed account should be recovered: observed=%v limited=%v", observed, limited)
	}

	observed, limited = codexUsageLimitState(json.RawMessage(`{
		"rate_limit":{"allowed":true,"limit_reached":true,"primary_window":{"used_percent":2}}
	}`), 90)
	if !observed || !limited {
		t.Fatalf("explicit limit_reached must remain exhausted: observed=%v limited=%v", observed, limited)
	}

	observed, limited = codexUsageLimitState(json.RawMessage(`{
		"rate_limit":{"allowed":false,"limit_reached":false,"primary_window":{"used_percent":2}}
	}`), 90)
	if !observed || !limited {
		t.Fatalf("explicitly disallowed account must remain exhausted: observed=%v limited=%v", observed, limited)
	}
}
