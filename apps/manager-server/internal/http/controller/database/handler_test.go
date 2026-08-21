package database_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/collector"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/httpapi"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/testutil"
)

func TestDatabaseManagementEndpointsRequirePanelAuthorization(t *testing.T) {
	cfg := testutil.NewConfig(t)
	st := testutil.NewStore(t, cfg)
	handler := httpapi.New(cfg, st, collector.NewManager(cfg, st)).Handler()

	for _, target := range []string{
		"/v0/management/database",
		"/v0/management/database/test",
		"/v0/management/database/migrations/plan",
	} {
		method := http.MethodGet
		body := ""
		if target != "/v0/management/database" {
			method = http.MethodPost
			body = `{"target":{"driver":"sqlite","path":"/tmp/target.sqlite"}}`
		}
		rr := testutil.Request(t, handler, method, target, body, "")
		testutil.RequireStatus(t, rr, http.StatusUnauthorized)
	}
}

func TestDatabaseManagementStatusAndSQLiteProbe(t *testing.T) {
	cfg := testutil.NewConfig(t)
	st := testutil.NewStore(t, cfg)
	handler := httpapi.New(cfg, st, collector.NewManager(cfg, st)).Handler()

	status := testutil.Request(t, handler, http.MethodGet, "/v0/management/database", "", testutil.AdminKey)
	testutil.RequireStatus(t, status, http.StatusOK)
	if body := status.Body.String(); !strings.Contains(body, `"driver":"sqlite"`) || !strings.Contains(body, `"supportedDrivers":["sqlite","mysql"]`) {
		t.Fatalf("status body = %s", body)
	}

	targetPath := filepath.Join(t.TempDir(), "target.sqlite")
	body := `{"target":{"driver":"sqlite","path":"` + targetPath + `"}}`
	probe := testutil.Request(t, handler, http.MethodPost, "/v0/management/database/test", body, testutil.AdminKey)
	testutil.RequireStatus(t, probe, http.StatusOK)
	if response := probe.Body.String(); !strings.Contains(response, `"healthy":true`) || !strings.Contains(response, `"exists":false`) {
		t.Fatalf("probe body = %s", response)
	}
	plan := testutil.Request(t, handler, http.MethodPost, "/v0/management/database/migrations/plan", body, testutil.AdminKey)
	testutil.RequireStatus(t, plan, http.StatusOK)
	if response := plan.Body.String(); !strings.Contains(response, `"targetEmpty":true`) || !strings.Contains(response, `"requiresRestart":true`) {
		t.Fatalf("plan body = %s", response)
	}
}
