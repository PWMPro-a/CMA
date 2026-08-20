package supply

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/config"
	managerconfigsvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

func TestRefreshNvtokensSessionSolvesChallengePersistsAndRetriesCatalog(t *testing.T) {
	var createCalls atomic.Int32
	var loginCalls atomic.Int32
	var catalogCalls atomic.Int32
	var challengeServer *httptest.Server
	challengeServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/createTask":
			createCalls.Add(1)
			_, _ = w.Write([]byte(`{"errorId":0,"taskId":"task-1"}`))
		case "/getTaskResult":
			_, _ = w.Write([]byte(`{"errorId":0,"status":"ready","solution":{"token":"turnstile-token"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer challengeServer.Close()

	nvServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/challenge-config":
			_, _ = w.Write([]byte(`{"provider":"turnstile","site_key":"site-key"}`))
		case "/api/login":
			loginCalls.Add(1)
			var payload map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload["cf-turnstile-response"] != "turnstile-token" {
				t.Fatalf("challenge token = %q", payload["cf-turnstile-response"])
			}
			http.SetCookie(w, &http.Cookie{Name: "scm_session", Value: "fresh-session", Path: "/"})
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/me":
			cookie, _ := r.Cookie("scm_session")
			if cookie == nil || cookie.Value != "fresh-session" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"id":1}`))
		case "/api/workspace/seller-candidates":
			catalogCalls.Add(1)
			cookie, _ := r.Cookie("scm_session")
			if cookie == nil || cookie.Value != "fresh-session" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":"AUTH_REQUIRED"}`))
				return
			}
			_, _ = w.Write([]byte(`{"sellers":[{"sale_plan_counts":{"plus":3}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer nvServer.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "manager.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	enabled := true
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		Enabled: &enabled,
		Platforms: []store.ManagerSupplyPlatformConfig{{
			ID: "nvtokens-main", Type: managerconfigsvc.SupplyPlatformNvtokens, Enabled: &enabled,
			BaseURL: nvServer.URL, Username: "buyer", Password: "secret", Token: "expired-session", Product: "plus",
			SessionRefreshEnabled: &enabled, ChallengeProvider: managerconfigsvc.SupplyChallengeProviderCapMonster,
			ChallengeAPIBase: challengeServer.URL, ChallengeAPIKey: "solver-key", RefreshCooldownSeconds: 30,
		}},
	}}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), nvServer.Client())
	catalog, err := service.GetPlatformProductCatalog(context.Background(), store.ManagerSupplyPlatformConfig{ID: "nvtokens-main"})
	if err != nil || len(catalog.Products) != 1 || catalog.Products[0].Available != 3 {
		t.Fatalf("catalog = %#v err=%v", catalog, err)
	}
	loaded, ok, err := st.LoadManagerConfig(context.Background())
	if err != nil || !ok || loaded.Supply.Platforms[0].Token != "fresh-session" {
		t.Fatalf("persisted config = %#v ok=%v err=%v", loaded.Supply.Platforms, ok, err)
	}
	if createCalls.Load() != 1 || loginCalls.Load() != 1 || catalogCalls.Load() != 2 {
		t.Fatalf("create=%d login=%d catalog=%d", createCalls.Load(), loginCalls.Load(), catalogCalls.Load())
	}
	status := service.nvtokensRefreshStatus("nvtokens-main")
	if status.State != "healthy" || status.LastRefreshAtMS == 0 || status.LastError != "" {
		t.Fatalf("refresh status = %#v", status)
	}
}

func TestNvtokensSessionRefreshIsSingleFlightPerPlatform(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var sidecarCalls atomic.Int32
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sidecarCalls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`{"session":"fresh-session"}`))
	}))
	defer sidecar.Close()
	nvServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/me" {
			http.NotFound(w, r)
			return
		}
		cookie, _ := r.Cookie("session")
		if cookie == nil || cookie.Value != "fresh-session" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer nvServer.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "manager.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	enabled := true
	platform := store.ManagerSupplyPlatformConfig{
		ID: "nvtokens-main", Type: managerconfigsvc.SupplyPlatformNvtokens, Enabled: &enabled,
		BaseURL: nvServer.URL, Username: "buyer", Password: "secret", Token: "expired", Product: "plus",
		SessionRefreshEnabled: &enabled, ChallengeProvider: managerconfigsvc.SupplyChallengeProviderSessionSidecar,
		ChallengeAPIBase: sidecar.URL, ChallengeAPIKey: "sidecar-key", RefreshCooldownSeconds: 30,
	}
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{Supply: store.ManagerSupplyConfig{
		Enabled: &enabled, Platforms: []store.ManagerSupplyPlatformConfig{platform},
	}}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), nvServer.Client())

	firstDone := make(chan error, 1)
	go func() {
		_, err := service.refreshNvtokensPlatform(context.Background(), platform, false)
		firstDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first refresh did not start")
	}
	secondDone := make(chan error, 1)
	go func() {
		_, err := service.refreshNvtokensPlatform(context.Background(), platform, false)
		secondDone <- err
	}()
	time.Sleep(25 * time.Millisecond)
	close(release)
	for index, result := range []<-chan error{firstDone, secondDone} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("refresh %d: %v", index+1, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("refresh %d did not finish", index+1)
		}
	}
	if sidecarCalls.Load() != 1 {
		t.Fatalf("sidecar calls = %d, want 1", sidecarCalls.Load())
	}
}

func TestNvtokensSessionRefreshCooldownSuppressesRepeatedFailures(t *testing.T) {
	var sidecarCalls atomic.Int32
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sidecarCalls.Add(1)
		http.Error(w, "temporary failure", http.StatusBadGateway)
	}))
	defer sidecar.Close()
	enabled := true
	platform := store.ManagerSupplyPlatformConfig{
		ID: "nvtokens-main", Type: managerconfigsvc.SupplyPlatformNvtokens, Enabled: &enabled,
		BaseURL: "https://nvtokens.invalid", Username: "buyer", Password: "secret", Product: "plus",
		SessionRefreshEnabled: &enabled, ChallengeProvider: managerconfigsvc.SupplyChallengeProviderSessionSidecar,
		ChallengeAPIBase: sidecar.URL, ChallengeAPIKey: "sidecar-key", RefreshCooldownSeconds: 30,
	}
	service := New(nil, nil, sidecar.Client())
	if _, err := service.refreshNvtokensPlatform(context.Background(), platform, false); err == nil {
		t.Fatal("first refresh should fail")
	}
	_, err := service.refreshNvtokensPlatform(context.Background(), platform, false)
	if err == nil || !strings.Contains(err.Error(), "cooling down") {
		t.Fatalf("cooldown error = %v", err)
	}
	if sidecarCalls.Load() != 1 {
		t.Fatalf("sidecar calls = %d, want 1", sidecarCalls.Load())
	}
}
