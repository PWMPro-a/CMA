package supplyclient

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientLogsInAndReadsInventoryAndBalance(t *testing.T) {
	var loginCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			loginCalls.Add(1)
			var payload map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload["username"] != "customer" || payload["password"] != "secret" {
				t.Fatalf("login payload = %#v", payload)
			}
			_, _ = w.Write([]byte(`{"token":"token-1"}`))
		case "/api/customer/inventory":
			if got := r.Header.Get("X-Customer-Token"); got != "token-1" {
				t.Fatalf("inventory token = %q", got)
			}
			_, _ = w.Write([]byte(`{"available":8,"missing":2,"needs_production":true,"estimated_total_fen":1000}`))
		case "/api/customer/balance":
			if got := r.Header.Get("X-Customer-Token"); got != "token-1" {
				t.Fatalf("balance token = %q", got)
			}
			_, _ = w.Write([]byte(`{"balance_fen":5000,"held_fen":500,"available_fen":4500,"currency":"CNY"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.Client())
	credentials := Credentials{BaseURL: server.URL, Username: "customer", Password: "secret"}
	inventory, err := client.Inventory(context.Background(), credentials, "oauth_30d", 10)
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if inventory.Available != 8 || inventory.Missing != 2 || !inventory.NeedsProduction || inventory.EstimatedTotalFen != 1000 {
		t.Fatalf("inventory = %#v", inventory)
	}
	balance, err := client.Balance(context.Background(), credentials)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if balance.AvailableFen != 4500 || balance.HeldFen != 500 || balance.Currency != "CNY" {
		t.Fatalf("balance = %#v", balance)
	}
	if got := loginCalls.Load(); got != 1 {
		t.Fatalf("login calls = %d, want 1", got)
	}
}

func TestNvtokensUsesSessionCookieAndImportsCPABundle(t *testing.T) {
	var loginCalls atomic.Int32
	var estimateCalls atomic.Int32
	var batchCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/login" {
			loginCalls.Add(1)
			var payload map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload["username"] != "buyer" || payload["password"] != "secret" {
				t.Fatalf("nvtokens login payload = %#v", payload)
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "nvtokens-session", Path: "/"})
			_, _ = w.Write([]byte(`{"user":{"id":1}}`))
			return
		}
		cookie, _ := r.Cookie("session")
		if cookie == nil || cookie.Value != "nvtokens-session" || r.Header.Get("X-Customer-Token") != "" {
			t.Fatalf("nvtokens authentication headers/cookies = %#v", r.Header)
		}
		switch r.URL.Path {
		case "/api/workspace/extractions/estimate":
			estimateCalls.Add(1)
			assertNvtokensPurchaseFilters(t, r, "has_refresh_token", 800)
			_, _ = w.Write([]byte(`{"estimate":{"total_cost_cents":240,"unit_price_cents":120,"available_quantity":9}}`))
		case "/api/me":
			_, _ = w.Write([]byte(`{"balance_cents":1000,"frozen_balance_cents":100,"available_balance_cents":900}`))
		case "/api/workspace/extractions/batch":
			batchCalls.Add(1)
			assertNvtokensPurchaseFilters(t, r, "has_refresh_token", 800)
			if got := r.Header.Get("Idempotency-Key"); got != "cpam-attempt-1" {
				t.Fatalf("nvtokens idempotency key = %q", got)
			}
			_, _ = w.Write([]byte(`{"summary":{"total_cost_cents":240},"cpa_bundle":{"type":"sub2api-data","version":1,"accounts":[{"type":"codex","access_token":"a","refresh_token":"r"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.Client())
	credentials := Credentials{
		PlatformType:        "nvtokens",
		BaseURL:             server.URL,
		Username:            "buyer",
		Password:            "secret",
		PurchaseAccountType: "access_refresh",
		MaxUnitPriceFen:     800,
	}
	inventory, err := client.Inventory(context.Background(), credentials, "oauth_30d", 2)
	if err != nil || inventory.Available != 9 || inventory.EstimatedTotalFen != 240 || inventory.EstimatedUnitPriceFen != 120 {
		t.Fatalf("nvtokens inventory = %#v err=%v", inventory, err)
	}
	balance, err := client.Balance(context.Background(), credentials)
	if err != nil || balance.AvailableFen != 900 || balance.HeldFen != 100 {
		t.Fatalf("nvtokens balance = %#v err=%v", balance, err)
	}
	order, err := client.CreateOrder(context.Background(), credentials, "oauth_30d", 2, "cpam-attempt-1")
	if err != nil || order.Status != "completed" || order.ReadyQuantity != 1 {
		t.Fatalf("nvtokens order = %#v err=%v", order, err)
	}
	taken, err := client.Take(context.Background(), credentials, order.ID)
	if err != nil || taken.Pending || len(taken.Accounts) != 1 {
		t.Fatalf("nvtokens take = %#v err=%v", taken, err)
	}
	if loginCalls.Load() != 1 || estimateCalls.Load() != 1 || batchCalls.Load() != 1 {
		t.Fatalf("nvtokens calls login=%d estimate=%d batch=%d", loginCalls.Load(), estimateCalls.Load(), batchCalls.Load())
	}
}

func TestNvtokensProductCatalogAggregatesNativeSalePlans(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "catalog-session", Path: "/"})
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/workspace/seller-candidates":
			cookie, err := r.Cookie("session")
			if err != nil || cookie.Value != "catalog-session" {
				t.Fatalf("catalog session cookie = %#v err=%v", cookie, err)
			}
			_, _ = w.Write([]byte(`{
				"sellers":[
					{"sale_plans":["plus","pro"],"sale_plan_counts":{"plus":5,"pro":2},"sale_plan_prices":{"plus":{"min_cents":190,"max_cents":350},"pro":{"min_cents":500,"max_cents":600}}},
					{"sale_plan_counts":{"plus":3,"team":4},"sale_plan_prices":{"plus":{"min_cents":220,"max_cents":300},"team":{"min_cents":420,"max_cents":620}}},
					{"sale_plan_stats":{"grokpro":{"available_count":7,"price_min_cents":80,"price_max_cents":120}}}
				]
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.Client())
	catalog, err := client.ProductCatalog(context.Background(), Credentials{
		ID:           "nvtokens-main",
		PlatformType: "nvtokens",
		BaseURL:      server.URL,
		Username:     "buyer",
		Password:     "secret",
	})
	if err != nil {
		t.Fatalf("product catalog: %v", err)
	}
	if len(catalog.Products) != 4 {
		t.Fatalf("products = %#v", catalog.Products)
	}
	byCode := make(map[string]ProductCatalogItem, len(catalog.Products))
	for _, product := range catalog.Products {
		byCode[product.Code] = product
	}
	if plus := byCode["plus"]; plus.Label != "Plus" || plus.Available != 8 || plus.MinUnitPriceFen != 190 || plus.MaxUnitPriceFen != 350 {
		t.Fatalf("plus = %#v", plus)
	}
	if team := byCode["team"]; team.Available != 4 || team.MinUnitPriceFen != 420 || team.MaxUnitPriceFen != 620 {
		t.Fatalf("team = %#v", team)
	}
	if grokPro := byCode["grokpro"]; grokPro.Label != "GrokPro" || grokPro.Available != 7 || grokPro.MinUnitPriceFen != 80 || grokPro.MaxUnitPriceFen != 120 {
		t.Fatalf("grokpro = %#v", grokPro)
	}
}

func assertNvtokensPurchaseFilters(t *testing.T, r *http.Request, accountType string, maxUnitPriceFen int64) {
	t.Helper()
	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Fatalf("decode nvtokens purchase payload: %v", err)
	}
	if got := payload["credential_type"]; got != accountType {
		t.Fatalf("credential_type = %#v, want %q", got, accountType)
	}
	if got := payload["max_unit_price_cents"]; got != float64(maxUnitPriceFen) {
		t.Fatalf("max_unit_price_cents = %#v, want %d", got, maxUnitPriceFen)
	}
	if got := payload["sale_plan_filter"]; got != "plus" {
		t.Fatalf("sale_plan_filter = %#v, want plus", got)
	}
}

func TestNvtokensPurchasePayloadPreservesNativeSalePlan(t *testing.T) {
	for _, product := range []string{"plus", "pro", "team", "bugteam", "k12", "grokfree", "grokpro", "free"} {
		payload := nvtokensPurchasePayload(Credentials{}, product, 1)
		if got := payload["sale_plan_filter"]; got != product {
			t.Fatalf("product %s sale_plan_filter = %#v", product, got)
		}
	}
	if got := nvtokensPurchasePayload(Credentials{}, "team_1h", 1)["sale_plan_filter"]; got != "team" {
		t.Fatalf("legacy team alias = %#v", got)
	}
}

func TestClientUsesBugTeamSessionAuthenticationForPasswordLogin(t *testing.T) {
	var loginCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			loginCalls.Add(1)
			var payload map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload["account"] != "customer" || payload["password"] != "secret" || payload["username"] != "" {
				t.Fatalf("BugTeam login payload = %#v", payload)
			}
			_, _ = w.Write([]byte(`{"session":"session-1"}`))
		case "/api/customer/inventory", "/api/customer/balance":
			if got := r.Header.Get("X-Customer-Session"); got != "session-1" {
				t.Fatalf("BugTeam session = %q", got)
			}
			if got := r.Header.Get("X-Customer-Token"); got != "" {
				t.Fatalf("unexpected BugTeam token header = %q", got)
			}
			if r.URL.Path == "/api/customer/inventory" {
				_, _ = w.Write([]byte(`{"product":"team_1h","quantity":1,"available":1}`))
			} else {
				_, _ = w.Write([]byte(`{"available_fen":300}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.Client())
	credentials := Credentials{PlatformType: "bugteam", BaseURL: server.URL, Username: "customer", Password: "secret"}
	if _, err := client.Inventory(context.Background(), credentials, "team_1h", 1); err != nil {
		t.Fatalf("BugTeam inventory: %v", err)
	}
	if _, err := client.Balance(context.Background(), credentials); err != nil {
		t.Fatalf("BugTeam balance: %v", err)
	}
	if loginCalls.Load() != 1 {
		t.Fatalf("BugTeam login calls = %d, want 1", loginCalls.Load())
	}
}

func TestClientFallsBackFromBugTeamAPITokenToPasswordSession(t *testing.T) {
	var loginCalls atomic.Int32
	var balanceCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			loginCalls.Add(1)
			_, _ = w.Write([]byte(`{"session":"fallback-session"}`))
		case "/api/customer/balance":
			balanceCalls.Add(1)
			if r.Header.Get("X-Customer-Token") == "expired-api-token" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"expired"}`))
				return
			}
			if got := r.Header.Get("X-Customer-Session"); got != "fallback-session" {
				t.Fatalf("fallback BugTeam session = %q", got)
			}
			_, _ = w.Write([]byte(`{"available_fen":4005}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	balance, err := New(server.Client()).Balance(context.Background(), Credentials{
		PlatformType: "bugteam", BaseURL: server.URL,
		Token: "expired-api-token", Username: "customer", Password: "secret",
	})
	if err != nil || balance.AvailableFen != 4005 || loginCalls.Load() != 1 || balanceCalls.Load() != 2 {
		t.Fatalf("balance=%#v err=%v login=%d balanceCalls=%d", balance, err, loginCalls.Load(), balanceCalls.Load())
	}
}

func TestClientRefreshesTokenOnceAfterUnauthorized(t *testing.T) {
	var loginCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			call := loginCalls.Add(1)
			_, _ = w.Write([]byte(`{"token":"token-` + string(rune('0'+call)) + `"}`))
		case "/api/customer/balance":
			if r.Header.Get("X-Customer-Token") == "token-1" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"expired"}`))
				return
			}
			_, _ = w.Write([]byte(`{"available_fen":2000}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.Client())
	balance, err := client.Balance(context.Background(), Credentials{BaseURL: server.URL, Username: "u", Password: "p"})
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if balance.AvailableFen != 2000 || loginCalls.Load() != 2 {
		t.Fatalf("balance=%#v loginCalls=%d", balance, loginCalls.Load())
	}
}

func TestClientPreservesIdempotencyKeyWhenUnauthorizedRefreshesToken(t *testing.T) {
	var loginCalls atomic.Int32
	var createCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			call := loginCalls.Add(1)
			_, _ = w.Write([]byte(`{"token":"token-` + strconv.Itoa(int(call)) + `"}`))
		case "/api/customer/pickup/orders":
			createCalls.Add(1)
			if got := r.Header.Get("Idempotency-Key"); got != "stable-create-key" {
				t.Fatalf("idempotency key = %q", got)
			}
			if r.Header.Get("X-Customer-Token") == "token-1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"order":{"id":"order-stable","status":"waiting_inventory"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.Client())
	order, err := client.CreateOrder(context.Background(), Credentials{BaseURL: server.URL, Username: "u", Password: "p"}, "oauth_30d", 1, "stable-create-key")
	if err != nil || order.ID != "order-stable" || loginCalls.Load() != 2 || createCalls.Load() != 2 {
		t.Fatalf("order=%#v err=%v login=%d create=%d", order, err, loginCalls.Load(), createCalls.Load())
	}
}

func TestClientCachesTokenForThirtyDayContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/customer/login" {
			_, _ = w.Write([]byte(`{"token":"token"}`))
			return
		}
		_, _ = w.Write([]byte(`{"available_fen":100}`))
	}))
	defer server.Close()

	client := New(server.Client())
	if _, err := client.Balance(context.Background(), Credentials{BaseURL: server.URL, Username: "u", Password: "p"}); err != nil {
		t.Fatalf("balance: %v", err)
	}
	if remaining := time.Until(client.token.expiresAt); remaining < 28*24*time.Hour || remaining > 30*24*time.Hour {
		t.Fatalf("cached token lifetime = %s", remaining)
	}
}

func TestClientCreatesPollsAndTakesOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"data":{"token":"token"}}`))
		case "/api/customer/pickup/orders":
			if got := r.Header.Get("Idempotency-Key"); got != "create-attempt-1" {
				t.Fatalf("idempotency key = %q", got)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"order":{"id":"order-1","status":"waiting_inventory","quantity":2}}`))
		case "/api/customer/pickup/orders/order-1":
			_, _ = w.Write([]byte(`{"id":"order-1","status":"ready","charged_fen":900}`))
		case "/api/customer/pickup/orders/order-1/take":
			_, _ = w.Write([]byte(`{"payload":{"accounts":[{"type":"codex","access_token":"a"},{"type":"codex","access_token":"b"}]},"status":"completed"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.Client())
	credentials := Credentials{BaseURL: server.URL, Username: "u", Password: "p"}
	order, err := client.CreateOrder(context.Background(), credentials, "oauth_30d", 2, "create-attempt-1")
	if err != nil || order.ID != "order-1" {
		t.Fatalf("create order=%#v err=%v", order, err)
	}
	order, err = client.GetOrder(context.Background(), credentials, order.ID)
	if err != nil || order.Status != "ready" || order.ChargedFen != 900 {
		t.Fatalf("get order=%#v err=%v", order, err)
	}
	taken, err := client.Take(context.Background(), credentials, order.ID)
	if err != nil || taken.Pending || len(taken.Accounts) != 2 {
		t.Fatalf("take=%#v err=%v", taken, err)
	}
}

func TestClientParsesBugTeamOrderStateAndDeliveredQuantity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Customer-Token"); got != "bugteam-token" {
			t.Fatalf("BugTeam API token = %q", got)
		}
		_, _ = w.Write([]byte(`{"order_id":"order-bugteam","product":"team_1h","quantity":3,"state":"completed","delivered_quantity":3,"charged_fen":840,"released_fen":60}`))
	}))
	defer server.Close()

	order, err := New(server.Client()).GetOrder(context.Background(), Credentials{
		PlatformType: "bugteam", BaseURL: server.URL, Token: "bugteam-token",
	}, "order-bugteam")
	if err != nil || order.ID != "order-bugteam" || order.Status != "completed" || order.ReadyQuantity != 3 || order.ChargedFen != 840 || order.ReleasedFen != 60 {
		t.Fatalf("BugTeam order=%#v err=%v", order, err)
	}
}

func TestClientDownloadsBugTeamCPAZIPWithManifestLease(t *testing.T) {
	account := []byte(`{"type":"codex","email":"lease@example.com","access_token":"access"}`)
	expiresAt := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339Nano)
	manifest := fmt.Sprintf(`{"schema_version":1,"items":[{"ordinal":1,"logical_name":"accounts/item-0001.json","content_sha256":"%x","expires_at":%q}]}`,
		sha256.Sum256(account), expiresAt)
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	manifestEntry, _ := writer.Create("manifest.json")
	_, _ = manifestEntry.Write([]byte(manifest))
	accountEntry, _ := writer.Create("accounts/item-0001.json")
	_, _ = accountEntry.Write(account)
	if err := writer.Close(); err != nil {
		t.Fatalf("close ZIP: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/customer/pickup/orders/order-zip/download" || r.URL.Query().Get("format") != "cpa" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("X-Customer-Token"); got != "bugteam-token" {
			t.Fatalf("BugTeam ZIP token = %q", got)
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(archive.Bytes())
	}))
	defer server.Close()

	result, err := New(server.Client()).Take(context.Background(), Credentials{
		PlatformType: "bugteam", BaseURL: server.URL, Token: "bugteam-token", DeliveryMode: "cpa_zip",
	}, "order-zip")
	if err != nil || len(result.Accounts) != 1 || len(result.OrderItems) != 1 || !result.OrderItems[0].HasRemaining ||
		result.OrderItems[0].RemainingSeconds < 590 || result.OrderItems[0].RemainingSeconds > 600 {
		t.Fatalf("BugTeam ZIP result=%#v err=%v", result, err)
	}
}

func TestCPAZIPRejectsTraversalEntry(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, _ := writer.Create("../account.json")
	_, _ = entry.Write([]byte(`{"type":"codex","access_token":"access"}`))
	if err := writer.Close(); err != nil {
		t.Fatalf("close ZIP: %v", err)
	}

	if _, _, err := cpaDeliveryFromZIP(archive.Bytes(), time.Now()); err == nil {
		t.Fatal("traversal ZIP entry was accepted")
	}
}

func TestClientTakeReadsOrderedItemRemainingSeconds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"token"}`))
		case "/api/customer/pickup/orders/order-lease/take":
			_, _ = w.Write([]byte(`{"order":{"id":"order-lease","status":"completed","items":[{"remaining_seconds":900,"base_price_fen":400,"charged_fen":100},{"remaining_seconds":"1800","base_price_fen":"400","charged_fen":"200"}]},"payload":{"accounts":[{"type":"codex","access_token":"a"},{"type":"codex","access_token":"b"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	taken, err := New(server.Client()).Take(context.Background(), Credentials{BaseURL: server.URL, Username: "u", Password: "p"}, "order-lease")
	if err != nil {
		t.Fatalf("take order: %v", err)
	}
	if len(taken.Accounts) != 2 || len(taken.ItemRemainingSeconds) != 2 || taken.ItemRemainingSeconds[0] != 900 || taken.ItemRemainingSeconds[1] != 1800 {
		t.Fatalf("take result = %#v", taken)
	}
	if len(taken.OrderItems) != 2 || taken.OrderItems[0].BasePriceFen != 400 || taken.OrderItems[0].ChargedFen != 100 ||
		taken.OrderItems[1].BasePriceFen != 400 || taken.OrderItems[1].ChargedFen != 200 {
		t.Fatalf("order item prices = %#v", taken.OrderItems)
	}
}

func TestClientTakeUsesExtendedTimeoutWithoutSlowingStatusRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"token"}`))
		case "/api/customer/pickup/orders/order-slow/take":
			time.Sleep(120 * time.Millisecond)
			_, _ = w.Write([]byte(`{"status":"completed","payload":{"accounts":[{"type":"codex","access_token":"a"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.Client(), 50*time.Millisecond)
	taken, err := client.Take(context.Background(), Credentials{BaseURL: server.URL, Username: "u", Password: "p"}, "order-slow")
	if err != nil || len(taken.Accounts) != 1 {
		t.Fatalf("take must use extended timeout: result=%#v err=%v", taken, err)
	}
}

func TestClientTreatsAcceptedTakeAsPending(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/customer/login" {
			_, _ = w.Write([]byte(`{"token":"token"}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"waiting_inventory","retry_after_seconds":7}`))
	}))
	defer server.Close()

	result, err := New(server.Client()).Take(context.Background(), Credentials{BaseURL: server.URL, Username: "u", Password: "p"}, "order-1")
	if err != nil || !result.Pending || result.Order.RetryAfterSeconds != 7 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestClientUsesReturnedStatusAndTakeURLs(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"token"}`))
		case "/api/customer/pickup/orders":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"order":{"id":"order-custom","status":"waiting_inventory"},"status_url":"` + server.URL + `/custom/status","take_url":"/custom/take"}`))
		case "/custom/status":
			if r.Header.Get("X-Customer-Token") != "token" {
				t.Fatalf("status token = %q", r.Header.Get("X-Customer-Token"))
			}
			_, _ = w.Write([]byte(`{"order":{"id":"order-custom","status":"ready","ready_quantity":2,"progress":100}}`))
		case "/custom/take":
			if r.Header.Get("X-Customer-Token") != "token" {
				t.Fatalf("take token = %q", r.Header.Get("X-Customer-Token"))
			}
			_, _ = w.Write([]byte(`{"status":"completed","payload":{"accounts":[{"type":"codex","access_token":"a"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.Client())
	credentials := Credentials{BaseURL: server.URL, Username: "u", Password: "p"}
	created, err := client.CreateOrder(context.Background(), credentials, "oauth_30d", 2)
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if created.StatusURL != server.URL+"/custom/status" || created.TakeURL != "/custom/take" {
		t.Fatalf("created URLs = status %q take %q", created.StatusURL, created.TakeURL)
	}
	polled, err := client.GetOrder(context.Background(), credentials, created.ID, created.StatusURL)
	if err != nil || polled.Status != "ready" || polled.ReadyQuantity != 2 || polled.Progress != 100 {
		t.Fatalf("polled=%#v err=%v", polled, err)
	}
	taken, err := client.Take(context.Background(), credentials, created.ID, created.TakeURL)
	if err != nil || len(taken.Accounts) != 1 || taken.Order.Status != "completed" {
		t.Fatalf("taken=%#v err=%v", taken, err)
	}
}

func TestClientListsAndClaimsRecoveries(t *testing.T) {
	var claimCalls atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"token"}`))
		case "/api/customer/recoveries":
			if got := r.Header.Get("X-Customer-Token"); got != "token" {
				t.Fatalf("recoveries token = %q", got)
			}
			_, _ = w.Write([]byte(`{"payload":{"recoveries":[{"recovery_id":"recovery-1","delivery_status":"claimable","product":"oauth_30d","source_order_id":8123,"original_email":"old@example.com","auth_file_name":"old.json","auth_index":"auth-1","claim_url":"` + server.URL + `/api/customer/recoveries/recovery-1/claim","claim_ticket":"ticket-1"}]}}`))
		case "/api/customer/recoveries/recovery-1/claim":
			claimCalls.Add(1)
			if got := r.URL.Query().Get("ticket"); got != "ticket-1" {
				t.Fatalf("claim ticket query = %q", got)
			}
			if got := r.Header.Get("X-Recovery-Ticket"); got != "ticket-1" {
				t.Fatalf("claim ticket header = %q", got)
			}
			if got := r.Header.Get("Idempotency-Key"); got != "cpam-recovery-recovery-1" {
				t.Fatalf("claim idempotency key = %q", got)
			}
			if got := r.Header.Get("X-Customer-Token"); got != "token" {
				t.Fatalf("claim token = %q", got)
			}
			if got := r.Header.Get("Accept"); got != "application/json" {
				t.Fatalf("claim accept = %q", got)
			}
			_, _ = w.Write([]byte(`{"credential_version":2,"payload":{"type":"oauth","credentials":{"access_token":"access","refresh_token":"refresh","email":"new@example.com","chatgpt_plan_type":"team"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.Client())
	credentials := Credentials{BaseURL: server.URL, Username: "u", Password: "p"}
	recoveries, err := client.Recoveries(context.Background(), credentials)
	if err != nil {
		t.Fatalf("recoveries: %v", err)
	}
	if len(recoveries) != 1 || recoveries[0].ID != "recovery-1" || recoveries[0].ClaimURL == "" ||
		recoveries[0].SourceOrderID != "8123" ||
		recoveries[0].OriginalAccount != "old.json" || recoveries[0].OriginalEmail != "old@example.com" ||
		recoveries[0].OriginalAuthIndex != "auth-1" {
		t.Fatalf("recoveries = %#v", recoveries)
	}
	claimed, err := client.ClaimRecovery(context.Background(), credentials, recoveries[0].ID, recoveries[0].ClaimURL, recoveries[0].ClaimTicket)
	if err != nil {
		t.Fatalf("claim recovery: %v", err)
	}
	if claimCalls.Load() != 1 || claimed.Recovery.ID != "recovery-1" || len(claimed.Accounts) != 1 || claimed.CredentialVersion != 2 {
		t.Fatalf("claimed=%#v claimCalls=%d", claimed, claimCalls.Load())
	}
}

func TestClientClaimsBugTeamRecoveryWithHeaderTicket(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"token"}`))
		case "/api/customer/recoveries/recovery-1/claim":
			if got := r.URL.Query().Get("ticket"); got != "" {
				t.Fatalf("BugTeam claim ticket leaked into URL = %q", got)
			}
			if got := r.Header.Get("X-Recovery-Ticket"); got != "ticket-1" {
				t.Fatalf("BugTeam claim ticket header = %q", got)
			}
			_, _ = w.Write([]byte(`{"credential_version":2,"payload":{"type":"oauth","access_token":"access"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.Client())
	credentials := Credentials{
		PlatformType: "bugteam",
		BaseURL:      server.URL,
		Username:     "u",
		Password:     "p",
	}
	claimed, err := client.ClaimRecovery(
		context.Background(),
		credentials,
		"recovery-1",
		server.URL+"/api/customer/recoveries/recovery-1/claim?ticket=ticket-1",
	)
	if err != nil {
		t.Fatalf("claim BugTeam recovery: %v", err)
	}
	if claimed.CredentialVersion != 2 || len(claimed.Accounts) != 1 {
		t.Fatalf("claimed=%#v", claimed)
	}
}

func TestClientPaginatesRecoveries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"token"}`))
		case "/api/customer/recoveries":
			if got := r.URL.Query().Get("limit"); got != "100" {
				t.Fatalf("limit = %q", got)
			}
			switch r.URL.Query().Get("before_id") {
			case "":
				_, _ = w.Write([]byte(`{"recoveries":[{"id":"rec-3","delivery_status":"pending"},{"id":"rec-2","delivery_status":"claimable","claim_url":"/claim-2"}],"next_before_id":2}`))
			case "2":
				_, _ = w.Write([]byte(`{"recoveries":[{"id":"rec-1","delivery_status":"pending"}]}`))
			default:
				t.Fatalf("unexpected before_id %q", r.URL.Query().Get("before_id"))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	recoveries, err := New(server.Client()).Recoveries(context.Background(), Credentials{BaseURL: server.URL, Username: "u", Password: "p"})
	if err != nil {
		t.Fatalf("recoveries: %v", err)
	}
	if len(recoveries) != 3 || recoveries[0].ID != "rec-3" || recoveries[2].ID != "rec-1" {
		t.Fatalf("recoveries = %#v", recoveries)
	}
}

func TestClientParsesReplacementFilesAndRefreshesStatusURL(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"token"}`))
		case "/api/customer/pickup/orders/order-replacement/take":
			_, _ = w.Write([]byte(`{"status":"completed","payload":{"accounts":[{"type":"codex","access_token":"old"}]},"replacement_files":[{"recovery_id":"rec-9","ready":true,"status_url":"/api/customer/recoveries/rec-9","credential_version":2}]}`))
		case "/api/customer/recoveries/rec-9":
			_, _ = w.Write([]byte(`{"recovery":{"id":"rec-9","delivery_status":"claimable","claim_url":"` + server.URL + `/api/customer/recoveries/rec-9/claim?ticket=fresh","credential_version":2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.Client())
	credentials := Credentials{BaseURL: server.URL, Username: "u", Password: "p"}
	taken, err := client.Take(context.Background(), credentials, "order-replacement")
	if err != nil || len(taken.ReplacementFiles) != 1 || !taken.ReplacementFiles[0].Ready {
		t.Fatalf("take=%#v err=%v", taken, err)
	}
	recovery, err := client.GetRecovery(context.Background(), credentials, "rec-9", taken.ReplacementFiles[0].StatusURL)
	if err != nil || recovery.ClaimURL == "" || recovery.CredentialVersion != 2 {
		t.Fatalf("recovery=%#v err=%v", recovery, err)
	}
}

func TestClientHTTPErrorIncludesRetryAfterAndCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/customer/login" {
			_, _ = w.Write([]byte(`{"token":"token"}`))
			return
		}
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"slow down"}}`))
	}))
	defer server.Close()

	_, err := New(server.Client()).Balance(context.Background(), Credentials{BaseURL: server.URL, Username: "u", Password: "p"})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusTooManyRequests || httpErr.RetryAfterSeconds != 17 || httpErr.Code != "rate_limited" {
		t.Fatalf("error = %#v", err)
	}
}

func TestRecoveryClaimEnvelopeUsesLatestNestedVersionAndDirectPayload(t *testing.T) {
	value := map[string]any{
		"credential_version": json.Number("0"),
		"payload": map[string]any{
			"credential_version": json.Number("3"),
			"credentials":        map[string]any{"access_token": "nested"},
		},
	}
	if version := findInt64(value, "credential_version", "credentialVersion"); version != 3 {
		t.Fatalf("credential version = %d, want 3", version)
	}
	direct := map[string]any{
		"type":        "oauth",
		"credentials": map[string]any{"access_token": "direct"},
	}
	if accounts := recoveryClaimAccounts(direct); len(accounts) != 1 {
		t.Fatalf("direct claim accounts = %#v", accounts)
	}
}

func TestClientRejectsCrossOriginOrderURL(t *testing.T) {
	var leaked atomic.Int32
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Customer-Token") != "" {
			leaked.Add(1)
		}
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	}))
	defer external.Close()

	supply := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/customer/login" {
			_, _ = w.Write([]byte(`{"token":"secret-token"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer supply.Close()

	_, err := New(supply.Client()).GetOrder(context.Background(), Credentials{
		BaseURL: supply.URL, Username: "u", Password: "p",
	}, "order-1", external.URL+"/order-1")
	if err == nil {
		t.Fatal("expected cross-origin URL rejection")
	}
	if leaked.Load() != 0 {
		t.Fatalf("customer token leaked to another origin %d time(s)", leaked.Load())
	}
}
