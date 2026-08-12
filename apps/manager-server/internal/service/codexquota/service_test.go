package codexquota

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

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

type recordingGateway struct {
	mu               sync.Mutex
	usageCalls       int
	consumeCalls     int
	localResetCalls  int
	resetCreditCalls int
	consumeErr       error
	consumeAccepted  bool
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
	if g.consumeAccepted {
		used = 0
	}
	body, _ := json.Marshal(map[string]any{
		"rate_limit": map[string]any{
			"allowed":        true,
			"primary_window": map[string]any{"used_percent": used},
		},
	})
	return apiCallResult{StatusCode: 200, Body: body}, nil
}

func (g *recordingGateway) resetCredits(context.Context, store.Setup, string, string) (apiCallResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resetCreditCalls++
	return apiCallResult{StatusCode: 200, Body: json.RawMessage(`{"available_count":1,"credits":[]}`)}, nil
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
