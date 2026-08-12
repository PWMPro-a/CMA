package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/testutil"
)

func TestCodexQuotaResetControllerRunsSagaAndReplaysOperation(t *testing.T) {
	var mu sync.Mutex
	consumeCalls := 0
	localResetCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"files":[{"id":"codex.json","name":"codex.json","provider":"codex","auth_index":"auth-1","email":"person@example.com","id_token":{"account_id":"account-1"}}]}`))
		case r.URL.Path == "/v0/management/api-call" && r.Method == http.MethodPost:
			var request struct {
				Method string `json:"method"`
				URL    string `json:"url"`
				Data   string `json:"data"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			body := `{"available_count":1,"credits":[]}`
			switch request.URL {
			case "https://chatgpt.com/backend-api/wham/usage":
				mu.Lock()
				consumed := consumeCalls > 0
				mu.Unlock()
				used := 100
				if consumed {
					used = 0
				}
				body = `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":` + strconv.Itoa(used) + `}}}`
			case "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume":
				mu.Lock()
				consumeCalls++
				mu.Unlock()
				body = `{"status":"accepted"}`
			}
			encodedBody, _ := json.Marshal(body)
			_, _ = w.Write([]byte(`{"status_code":200,"body":` + string(encodedBody) + `}`))
		case r.URL.Path == "/v0/management/reset-quota" && r.Method == http.MethodPost:
			mu.Lock()
			localResetCalls++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"status":"ok","auth_index":"auth-1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	cfg := testutil.NewConfig(t)
	handler, db := newCompatHandler(t, cfg, nil)
	managerCfg := store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: upstream.URL, ManagementKey: "cpa-key"},
		Collector: store.ManagerCollectorConfig{
			CollectorMode: "auto", Queue: "usage", PopSide: "right", BatchSize: 100, PollIntervalMS: 500, QueryLimit: 50000,
		},
		CodexInspection: store.DefaultCodexInspectionConfig(),
	}
	if err := db.SaveManagerConfig(context.Background(), managerCfg); err != nil {
		t.Fatalf("save manager config: %v", err)
	}

	body := `{"auth_index":"auth-1","operation_id":"025f897e-6e47-4d7d-a06f-6cf3b8315d78"}`
	first := testutil.Request(t, handler, http.MethodPost, "/v0/management/cpamp/codex-quota/reset-credit", body, testutil.AdminKey)
	testutil.RequireStatus(t, first, http.StatusOK)
	var operation struct {
		State    string `json:"state"`
		Consumed *bool  `json:"consumed"`
	}
	testutil.DecodeJSON(t, first, &operation)
	if operation.State != "completed" || operation.Consumed == nil || !*operation.Consumed {
		t.Fatalf("operation = %#v body=%s", operation, first.Body.String())
	}

	replayed := testutil.Request(t, handler, http.MethodPost, "/v0/management/cpamp/codex-quota/reset-credit", body, testutil.AdminKey)
	testutil.RequireStatus(t, replayed, http.StatusOK)
	status := testutil.Request(t, handler, http.MethodGet, "/v0/management/cpamp/codex-quota/reset-credit/operations/025f897e-6e47-4d7d-a06f-6cf3b8315d78", "", testutil.AdminKey)
	testutil.RequireStatus(t, status, http.StatusOK)

	mu.Lock()
	defer mu.Unlock()
	if consumeCalls != 1 || localResetCalls != 1 {
		t.Fatalf("consume calls=%d local reset calls=%d", consumeCalls, localResetCalls)
	}
}
