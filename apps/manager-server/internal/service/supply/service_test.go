package supply

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/config"
	managerconfigsvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

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

func TestCreateResultUncertainBlocksDuplicateOrdersUntilDismissed(t *testing.T) {
	var createCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"customer-token"}`))
		case r.URL.Path == "/api/customer/inventory":
			_, _ = w.Write([]byte(`{"available":1,"estimated_total_fen":100}`))
		case r.URL.Path == "/api/customer/balance":
			_, _ = w.Write([]byte(`{"available_fen":10000}`))
		case r.URL.Path == "/v0/management/auth-files":
			_, _ = w.Write([]byte(`{"files":[]}`))
		case r.URL.Path == "/api/customer/pickup/orders":
			createCalls.Add(1)
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("response writer does not support hijacking")
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatalf("hijack: %v", err)
			}
			_ = connection.Close()
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
	if err := st.SaveManagerConfig(context.Background(), store.ManagerConfig{
		CPAConnection: store.ManagerCPAConnectionConfig{CPABaseURL: server.URL, ManagementKey: "management-key"},
		Supply: store.ManagerSupplyConfig{
			Enabled: &enabled, BaseURL: server.URL, Username: "customer", Password: "password",
			Product: "oauth_30d", TargetAvailableAccounts: 1, ReplenishBatchSize: 1,
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	service := New(st, managerconfigsvc.New(config.Config{}, st, nil), server.Client())
	if err := service.RunAutomatic(context.Background()); !errors.Is(err, ErrCreateUncertain) {
		t.Fatalf("first automatic run error = %v", err)
	}
	if err := service.RunAutomatic(context.Background()); err != nil {
		t.Fatalf("second automatic run: %v", err)
	}
	if createCalls.Load() != 1 {
		t.Fatalf("create calls = %d, want 1", createCalls.Load())
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil || status.ActiveOrder == nil || status.ActiveOrder.Status != "create_uncertain" {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	if _, err := service.DismissCreateUncertain(context.Background(), status.ActiveOrder.OrderID); err != nil {
		t.Fatalf("dismiss uncertain order: %v", err)
	}
	status, err = service.GetStatus(context.Background(), 10)
	if err != nil || status.ActiveOrder != nil || status.Orders[0].Status != "dismissed" {
		t.Fatalf("dismissed status=%#v err=%v", status, err)
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
	raw := `{"name":"team-user","type":"oauth","platform":"openai","priority":2,"concurrency":8,"extra":{"organization_id":"org-extra","lastRefresh":"2026-07-01T00:00:00Z"},"credentials":{"access_token":"access","refresh_token":"refresh","chatgpt_account_id":"account-1","email":"user@example.com","plan_type":"team","expires_at":"2026-07-30T00:00:00Z"}}`
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
		result["expired"] != "2026-07-30T00:00:00Z" || result["max_concurrency"] != float64(8) {
		t.Fatalf("normalized metadata = %#v", result)
	}
	if len(key) != 64 || len(fileName) != len("codex-supply-")+20+len(".json") || fileName[:13] != "codex-supply-" {
		t.Fatalf("stable identity outputs key=%q file=%q", key, fileName)
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

func TestAutomaticOrderReleasesInsteadOfTakingWhenCPATargetIsAlreadySatisfied(t *testing.T) {
	var createCalls atomic.Int32
	var releaseCalls atomic.Int32
	var takeCalls atomic.Int32
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
		case r.URL.Path == "/api/customer/pickup/orders/order-release" && r.Method == http.MethodDelete:
			releaseCalls.Add(1)
			_, _ = w.Write([]byte(`{"order":{"id":"order-release","status":"cancelled","released_fen":1000}}`))
		case r.URL.Path == "/api/customer/pickup/orders/order-release" && r.Method == http.MethodGet:
			t.Fatal("order status should not be polled after CPA target is already satisfied")
		case r.URL.Path == "/api/customer/pickup/orders/order-release/take":
			takeCalls.Add(1)
			t.Fatal("ready order should be released instead of taken when CPA target is already satisfied")
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
		t.Fatalf("release satisfied order: %v", err)
	}
	status, err := service.GetStatus(context.Background(), 10)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if status.ActiveOrder != nil {
		t.Fatalf("active order should be cleared: %#v", status.ActiveOrder)
	}
	if len(status.Orders) != 1 || status.Orders[0].Status != "released" || status.Orders[0].ReleasedFen != 1000 {
		t.Fatalf("orders = %#v", status.Orders)
	}
	if createCalls.Load() != 1 || releaseCalls.Load() != 1 || takeCalls.Load() != 0 {
		t.Fatalf("calls create=%d release=%d take=%d", createCalls.Load(), releaseCalls.Load(), takeCalls.Load())
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
