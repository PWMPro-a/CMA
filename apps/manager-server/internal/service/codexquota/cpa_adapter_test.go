package codexquota

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

func TestCPAAdapterQuotaCallsRequireFreshOAuthToken(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/api-call" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status_code": http.StatusOK,
			"body":        `{"plan_type":"team"}`,
		})
	}))
	t.Cleanup(server.Close)

	adapter := newCPAAdapter(server.Client())
	result, err := adapter.usage(context.Background(), store.Setup{
		CPAUpstreamURL: server.URL,
		ManagementKey:  "management-key",
	}, "auth-index", "account-id")
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", result.StatusCode)
	}
	if request["ensureFreshToken"] != true {
		t.Fatalf("ensureFreshToken = %#v, want true", request["ensureFreshToken"])
	}
	if request["authIndex"] != "auth-index" {
		t.Fatalf("authIndex = %#v", request["authIndex"])
	}
}
