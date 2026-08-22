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
	"strings"
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
			_, _ = w.Write([]byte(`{"estimate":{"buyer_total_cents":240,"min_unit_price_cents":120,"max_unit_price_cents":120,"available_quantity":9}}`))
		case "/api/me":
			_, _ = w.Write([]byte(`{"balance_cents":1000,"frozen_balance_cents":100,"available_balance_cents":900}`))
		case "/api/workspace/extractions/batch":
			batchCalls.Add(1)
			assertNvtokensPurchaseFilters(t, r, "has_refresh_token", 800)
			if got := r.Header.Get("Idempotency-Key"); got != "cpam-attempt-1" {
				t.Fatalf("nvtokens idempotency key = %q", got)
			}
			_, _ = w.Write([]byte(`{"summary":{"requested":2,"extracted":1,"buyer_total_cents":240},"cpa_bundle":{"type":"sub2api-data","version":1,"accounts":[{"type":"codex","access_token":"a","refresh_token":"r"}]}}`))
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
	if err != nil || order.Status != "completed" || order.ReadyQuantity != 1 || order.ChargedFen != 240 {
		t.Fatalf("nvtokens order = %#v err=%v", order, err)
	}
	taken, err := client.Take(context.Background(), credentials, order.ID)
	if err != nil || taken.Pending || len(taken.Accounts) != 1 || len(taken.OrderItems) != 1 || taken.OrderItems[0].ChargedFen != 240 {
		t.Fatalf("nvtokens take = %#v err=%v", taken, err)
	}
	if loginCalls.Load() != 1 || estimateCalls.Load() != 1 || batchCalls.Load() != 1 {
		t.Fatalf("nvtokens calls login=%d estimate=%d batch=%d", loginCalls.Load(), estimateCalls.Load(), batchCalls.Load())
	}
}

func TestNvtokensPaidExtractionFallsBackToWorkspaceLedger(t *testing.T) {
	const orderID = "paid-order-ledger"
	var directCalls atomic.Int32
	var ledgerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cookie, _ := r.Cookie(nvtokensSessionCookie); cookie == nil || cookie.Value != "session-token" {
			t.Fatalf("session cookie = %#v", cookie)
		}
		switch r.URL.Path {
		case "/api/workspace/extractions/" + orderID:
			directCalls.Add(1)
			http.NotFound(w, r)
		case "/api/workspace/extractions":
			ledgerCalls.Add(1)
			if got := r.URL.Query().Get("q"); got != orderID {
				t.Fatalf("ledger q = %q", got)
			}
			_, _ = w.Write([]byte(`{"orders":[{"id":"paid-order-ledger","amount_cents":2500,"status":"paid","warranty_until":"2099-01-01T00:00:00Z","card_payload":{"sub2api_account":{"type":"oauth","platform":"openai","credentials":{"email":"paid@example.com","access_token":"access-paid","refresh_token":"refresh-paid","account_id":"account-paid"}}}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.Client())
	credentials := Credentials{PlatformType: nvtokensPlatform, BaseURL: server.URL, Token: "session-token"}
	order, err := client.GetOrder(context.Background(), credentials, orderID)
	if err != nil || order.ID != orderID || order.Status != "completed" || order.ReadyQuantity != 1 || order.ChargedFen != 2500 {
		t.Fatalf("ledger order = %#v err=%v", order, err)
	}
	taken, err := client.Take(context.Background(), credentials, orderID)
	if err != nil || len(taken.Accounts) != 1 || len(taken.OrderItems) != 1 ||
		taken.OrderItems[0].ChargedFen != 2500 || !taken.OrderItems[0].HasRemaining {
		t.Fatalf("ledger take = %#v err=%v", taken, err)
	}
	if directCalls.Load() != 1 || ledgerCalls.Load() != 1 {
		t.Fatalf("calls direct=%d ledger=%d", directCalls.Load(), ledgerCalls.Load())
	}
}

func TestNvtokensPaidBatchFallsBackToBatchLedgerFilter(t *testing.T) {
	const batchID = "batch-paid-ledger"
	var batchFilterCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/workspace/extractions/"+batchID:
			http.NotFound(w, r)
		case r.URL.Path == "/api/workspace/extractions" && r.URL.Query().Get("q") == batchID:
			_, _ = w.Write([]byte(`{"orders":[],"pagination":{"total":0}}`))
		case r.URL.Path == "/api/workspace/extractions" && r.URL.Query().Get("batch_id") == batchID:
			batchFilterCalls.Add(1)
			if r.URL.Query().Get("page") == "1" {
				_, _ = w.Write([]byte(`{"orders":[
					{"id":"order-a","extraction_batch_id":"batch-paid-ledger","amount_cents":2588,"status":"paid","card_payload":{"sub2api_account":{"type":"oauth","platform":"openai","credentials":{"access_token":"access-a","refresh_token":"refresh-a","account_id":"account-a"}}}}
				],"pagination":{"page":1,"total_pages":2,"has_next":true}}`))
			} else {
				_, _ = w.Write([]byte(`{"orders":[
					{"id":"order-b","extraction_batch_id":"batch-paid-ledger","amount_cents":2588,"status":"paid","card_payload":{"sub2api_account":{"type":"oauth","platform":"openai","credentials":{"access_token":"access-b","refresh_token":"refresh-b","account_id":"account-b"}}}}
				],"pagination":{"page":2,"total_pages":2,"has_next":false}}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.Client())
	credentials := Credentials{PlatformType: nvtokensPlatform, BaseURL: server.URL, Token: "session-token"}
	order, err := client.GetOrder(context.Background(), credentials, batchID)
	if err != nil || order.ID != batchID || order.Status != "completed" || order.ReadyQuantity != 2 || order.ChargedFen != 5176 {
		t.Fatalf("batch ledger order = %#v err=%v", order, err)
	}
	taken, err := client.Take(context.Background(), credentials, batchID)
	if err != nil || len(taken.Accounts) != 2 || len(taken.OrderItems) != 2 ||
		taken.OrderItems[0].ChargedFen != 2588 || taken.OrderItems[1].ChargedFen != 2588 {
		t.Fatalf("batch ledger take = %#v err=%v", taken, err)
	}
	if batchFilterCalls.Load() != 2 {
		t.Fatalf("batch filter calls = %d", batchFilterCalls.Load())
	}
}

func TestNvtokensInventoryUsesMatchedQuantityForPartialQuote(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workspace/extractions/estimate" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"estimate":{"requested_quantity":10,"matched_quantity":3,"missing_quantity":7,"buyer_total_cents":4500,"min_unit_price_cents":1500,"max_unit_price_cents":1500}}`))
	}))
	defer server.Close()

	client := New(server.Client())
	inventory, err := client.Inventory(context.Background(), Credentials{
		PlatformType: "nvtokens", BaseURL: server.URL, Token: "session-snapshot",
	}, "plus", 10)
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if inventory.Available != 3 || inventory.Missing != 7 || inventory.EstimatedTotalFen != 4500 || inventory.EstimatedUnitPriceFen != 1500 {
		t.Fatalf("inventory = %#v", inventory)
	}
}

func TestNvtokensInventoryRetriesTransientNoContent(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workspace/extractions/estimate" {
			http.NotFound(w, r)
			return
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(`{"estimate":{"matched_quantity":2,"buyer_total_cents":3200,"min_unit_price_cents":1600,"max_unit_price_cents":1600}}`))
	}))
	defer server.Close()

	client := New(server.Client())
	inventory, err := client.Inventory(context.Background(), Credentials{
		PlatformType: "nvtokens", BaseURL: server.URL, Token: "session-snapshot",
	}, "plus", 2)
	if err != nil {
		t.Fatalf("inventory after retry: %v", err)
	}
	if calls.Load() != 2 || inventory.Available != 2 || inventory.EstimatedTotalFen != 3200 {
		t.Fatalf("calls=%d inventory=%#v", calls.Load(), inventory)
	}
}

func TestNvtokensInventoryRejectsPersistentNoContent(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := New(server.Client())
	_, err := client.Inventory(context.Background(), Credentials{
		PlatformType: "nvtokens", BaseURL: server.URL, Token: "session-snapshot",
	}, "plus", 10)
	if !errors.Is(err, ErrNvtokensEstimateUnavailable) || calls.Load() != 2 {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
}

func TestNvtokensBatchResultsCaptureRemoteOrderIDsAndActualPrices(t *testing.T) {
	var single any
	if err := json.Unmarshal([]byte(`{
		"summary":{"requested":1,"extracted":1},
		"results":[{
			"status":"extracted",
			"order":{"id":"order-single","buyer_total_cents":1200},
			"account_json":{"type":"codex","access_token":"access-one","refresh_token":"refresh-one"}
		}]
	}`), &single); err != nil {
		t.Fatalf("decode single result: %v", err)
	}
	if got := nvtokensBatchOrderID(single); got != "order-single" {
		t.Fatalf("single batch order id = %q", got)
	}
	if got := nvtokensBatchChargedFen(single); got != 1200 {
		t.Fatalf("single batch charged = %d", got)
	}
	items := nvtokensResultOrderItems(single, 1200, 1)
	if len(items) != 1 || items[0].BasePriceFen != 1200 || items[0].ChargedFen != 1200 {
		t.Fatalf("single batch order items = %#v", items)
	}

	var batch any
	if err := json.Unmarshal([]byte(`{
		"summary":{"requested":3,"extracted":2,"failed":1},
		"results":[
			{"status":"extracted","order":{"id":"order-a","extraction_batch_id":"batch-a","amount_cents":1680},"account_json":{"type":"codex","access_token":"access-a","refresh_token":"refresh-a"}},
			{"status":"failed","message":"inventory changed"},
			{"status":"extracted","order":{"id":"order-b","extraction_batch_id":"batch-a","buyer_total_cents":2250},"account_json":{"type":"codex","access_token":"access-b","refresh_token":"refresh-b"}}
		]
	}`), &batch); err != nil {
		t.Fatalf("decode batch result: %v", err)
	}
	if got := nvtokensBatchOrderID(batch); got != "batch-a" {
		t.Fatalf("multi-order batch id = %q, want shared extraction batch id", got)
	}
	if got := nvtokensBatchChargedFen(batch); got != 3930 {
		t.Fatalf("multi-order batch charged = %d", got)
	}
	items = nvtokensResultOrderItems(batch, 3930, 2)
	if len(items) != 2 || items[0].ChargedFen != 1680 || items[1].ChargedFen != 2250 {
		t.Fatalf("multi-order batch items = %#v", items)
	}
}

func TestNvtokensBatchKeepsWarrantyWhenOnlySummaryHasPrice(t *testing.T) {
	var value any
	if err := json.Unmarshal([]byte(`{
		"summary":{"requested":3,"extracted":3,"buyer_total_cents":4465},
		"results":[
			{"status":"extracted","remaining_seconds":3600,"account_json":{"type":"codex","access_token":"access-a","refresh_token":"refresh-a"}},
			{"status":"extracted","order":{"remaining_seconds":"3599"},"account_json":{"type":"codex","access_token":"access-b","refresh_token":"refresh-b"}},
			{"status":"extracted","remaining_valid_seconds":3598,"account_json":{"type":"codex","access_token":"access-c","refresh_token":"refresh-c"}}
		]
	}`), &value); err != nil {
		t.Fatalf("decode batch result: %v", err)
	}

	items := nvtokensResultOrderItems(value, 4465, 3)
	if len(items) != 3 {
		t.Fatalf("batch order items = %#v", items)
	}
	if items[0].RemainingSeconds != 3600 || items[1].RemainingSeconds != 3599 || items[2].RemainingSeconds != 3598 ||
		!items[0].HasRemaining || !items[1].HasRemaining || !items[2].HasRemaining {
		t.Fatalf("batch warranty windows = %#v", items)
	}
	if items[0].ChargedFen != 1489 || items[1].ChargedFen != 1488 || items[2].ChargedFen != 1488 {
		t.Fatalf("batch price split = %#v", items)
	}
}

func TestNvtokensResultAccountsPrefersResultsWhenCPABundleIsEmpty(t *testing.T) {
	var value any
	if err := json.Unmarshal([]byte(`{
		"status":"completed",
		"ready_quantity":1,
		"cpa_bundle":{"type":"sub2api-data","accounts":[]},
		"results":[{"success":true,"account_json":{"type":"oauth","platform":"openai","credentials":{"access_token":"access-result","refresh_token":"refresh-result","account_id":"account-result"}}}]
	}`), &value); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	accounts := nvtokensResultAccounts(value)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	var account map[string]any
	if err := json.Unmarshal(accounts[0], &account); err != nil {
		t.Fatalf("decode account: %v", err)
	}
	credentials, _ := account["credentials"].(map[string]any)
	if credentials["access_token"] != "access-result" || credentials["refresh_token"] != "refresh-result" {
		t.Fatalf("account = %#v", account)
	}
}

func TestNvtokensResultAccountsSupportsNativeBundlesAndCardPayloads(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{
			name: "sub2api bundle",
			raw:  `{"sub2api_bundle":{"type":"sub2api-data","accounts":[{"type":"oauth","platform":"openai","credentials":{"access_token":"access-one","refresh_token":"refresh-one"}},{"type":"oauth","platform":"openai","credentials":{"access_token":"access-two","refresh_token":"refresh-two"}}]}}`,
			want: 2,
		},
		{
			name: "card payload sub2api account",
			raw:  `{"results":[{"card_payload":{"sub2api_account":{"type":"oauth","platform":"openai","credentials":{"access_token":"access-sub2","refresh_token":"refresh-sub2"}}}}]}`,
			want: 1,
		},
		{
			name: "card payload codex account",
			raw:  `{"results":[{"card_payload":{"codex_account":{"type":"codex","access_token":"access-codex","refresh_token":"refresh-codex","account_id":"account-codex"}}}]}`,
			want: 1,
		},
		{
			name: "JSON string account",
			raw:  `{"results":[{"account_json":"{\"type\":\"codex\",\"access_token\":\"access-string\",\"refresh_token\":\"refresh-string\"}"}]}`,
			want: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var value any
			if err := json.Unmarshal([]byte(test.raw), &value); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if accounts := nvtokensResultAccounts(value); len(accounts) != test.want {
				t.Fatalf("accounts = %d, want %d: %s", len(accounts), test.want, test.raw)
			}
		})
	}
}

func TestNvtokensResultAccountsDeduplicatesRepeatedRepresentations(t *testing.T) {
	var value any
	if err := json.Unmarshal([]byte(`{
		"results":[{
			"account_json":{"type":"oauth","platform":"openai","credentials":{"access_token":"same-access","refresh_token":"same-refresh","account_id":"same-account"}},
			"card_payload":{"codex_account":{"type":"codex","access_token":"same-access","refresh_token":"same-refresh","account_id":"same-account"}}
		}],
		"sub2api_bundle":{"accounts":[{"type":"codex","access_token":"same-access","refresh_token":"same-refresh","account_id":"same-account"}]}
	}`), &value); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if accounts := nvtokensResultAccounts(value); len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
}

func TestNvtokensCreateOrderWithoutParsedAccountsStaysPending(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workspace/extractions/batch" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"status":"completed","summary":{"requested":1,"extracted":1,"buyer_total_cents":1666},"results":[{"status":"extracted","order":{"id":"paid-order-1","buyer_total_cents":1666}}]}`))
	}))
	defer server.Close()

	order, err := New(server.Client()).CreateOrder(context.Background(), Credentials{
		PlatformType: "nvtokens",
		BaseURL:      server.URL,
		Token:        "session-token",
	}, "plus", 1, "empty-extraction")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if order.ID != "paid-order-1" || order.Status != "processing" || order.ReadyQuantity != 1 || order.Progress != 0 || order.ChargedFen != 1666 {
		t.Fatalf("order = %#v, want processing without phantom progress", order)
	}
}

func TestNvtokensCreateOrderEmptyUnpaidResultFailsWithBackoff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workspace/extractions/batch" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"status":"completed","summary":{"requested":1,"extracted":0,"failed":1},"results":[{"status":"failed","message":"inventory changed"}]}`))
	}))
	defer server.Close()

	order, err := New(server.Client()).CreateOrder(context.Background(), Credentials{
		PlatformType: "nvtokens",
		BaseURL:      server.URL,
		Token:        "session-token",
	}, "plus", 1, "empty-unpaid-extraction")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if order.Status != "failed" || order.ReadyQuantity != 0 || order.RetryAfterSeconds < 30 {
		t.Fatalf("order = %#v, want failed unpaid result with backoff", order)
	}
}

func TestNvtokensTokenSkipsCaptchaLoginAndUsesSessionCookie(t *testing.T) {
	var loginCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/login":
			loginCalls.Add(1)
			http.Error(w, "captcha required", http.StatusBadRequest)
		case "/api/workspace/seller-candidates":
			cookie, err := r.Cookie("session")
			if err != nil || cookie.Value != "session-token" {
				t.Fatalf("session cookie = %#v err=%v", cookie, err)
			}
			if got := r.Header.Get(customerTokenHeader); got != "session-token" {
				t.Fatalf("customer token = %q", got)
			}
			_, _ = w.Write([]byte(`{"sellers":[{"sale_plans":["plus"],"sale_plan_counts":{"plus":3}}]}`))
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
		Token:        "session=session-token",
	})
	if err != nil || len(catalog.Products) != 1 || catalog.Products[0].Code != "plus" {
		t.Fatalf("catalog = %#v err=%v", catalog, err)
	}
	if got := loginCalls.Load(); got != 0 {
		t.Fatalf("login calls = %d, want 0", got)
	}
}

func TestNvtokensFallsBackFromExpiredSessionToPasswordLogin(t *testing.T) {
	var loginCalls atomic.Int32
	var catalogCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/login":
			loginCalls.Add(1)
			var payload map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload["username"] != "buyer" || payload["password"] != "secret" {
				t.Fatalf("nvtokens refresh login payload = %#v", payload)
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "fresh-session", Path: "/"})
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/workspace/seller-candidates":
			catalogCalls.Add(1)
			cookie, _ := r.Cookie("session")
			if cookie != nil && cookie.Value == "expired-session" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":"AUTH_REQUIRED","message":"登录状态已失效，请重新登录"}`))
				return
			}
			if cookie == nil || cookie.Value != "fresh-session" {
				t.Fatalf("refreshed nvtokens session cookie = %#v", cookie)
			}
			if got := r.Header.Get(customerTokenHeader); got != "" {
				t.Fatalf("refreshed nvtokens customer token = %q", got)
			}
			_, _ = w.Write([]byte(`{"sellers":[{"sale_plan_counts":{"plus":3}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	catalog, err := New(server.Client()).ProductCatalog(context.Background(), Credentials{
		ID:           "nvtokens-main",
		PlatformType: "nvtokens",
		BaseURL:      server.URL,
		Token:        "expired-session",
		Username:     "buyer",
		Password:     "secret",
	})
	if err != nil || len(catalog.Products) != 1 || catalog.Products[0].Code != "plus" {
		t.Fatalf("catalog = %#v err=%v", catalog, err)
	}
	if got := loginCalls.Load(); got != 1 {
		t.Fatalf("login calls = %d, want 1", got)
	}
	if got := catalogCalls.Load(); got != 2 {
		t.Fatalf("catalog calls = %d, want 2", got)
	}
}

func TestNvtokensExpiredSessionWithoutPasswordDoesNotRetry(t *testing.T) {
	var loginCalls atomic.Int32
	var catalogCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/login":
			loginCalls.Add(1)
			http.Error(w, "unexpected login", http.StatusInternalServerError)
		case "/api/workspace/seller-candidates":
			catalogCalls.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"AUTH_REQUIRED","message":"登录状态已失效，请重新登录"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	_, err := New(server.Client()).ProductCatalog(context.Background(), Credentials{
		ID:           "nvtokens-main",
		PlatformType: "nvtokens",
		BaseURL:      server.URL,
		Token:        "expired-session",
	})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized || httpErr.Code != "AUTH_REQUIRED" {
		t.Fatalf("error = %#v, want AUTH_REQUIRED HTTP 401", err)
	}
	if loginCalls.Load() != 0 || catalogCalls.Load() != 1 {
		t.Fatalf("login=%d catalog=%d, want login=0 catalog=1", loginCalls.Load(), catalogCalls.Load())
	}
}

func TestNvtokensFailedPasswordRefreshRetriesOnlyOnce(t *testing.T) {
	var loginCalls atomic.Int32
	var catalogCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/login":
			loginCalls.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"AUTH_REQUIRED","message":"账号或密码错误"}`))
		case "/api/workspace/seller-candidates":
			catalogCalls.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"AUTH_REQUIRED","message":"登录状态已失效，请重新登录"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	_, err := New(server.Client()).ProductCatalog(context.Background(), Credentials{
		ID:           "nvtokens-main",
		PlatformType: "nvtokens",
		BaseURL:      server.URL,
		Token:        "expired-session",
		Username:     "buyer",
		Password:     "wrong-secret",
	})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized || httpErr.Code != "AUTH_REQUIRED" {
		t.Fatalf("error = %#v, want password refresh AUTH_REQUIRED HTTP 401", err)
	}
	if loginCalls.Load() != 1 || catalogCalls.Load() != 1 {
		t.Fatalf("login=%d catalog=%d, want login=1 catalog=1", loginCalls.Load(), catalogCalls.Load())
	}
}

func TestNvtokensCaptchaRefreshReturnsActionableAuthenticationError(t *testing.T) {
	var loginCalls atomic.Int32
	var catalogCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/login":
			loginCalls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"请完成人机验证"}`))
		case "/api/workspace/seller-candidates":
			catalogCalls.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"AUTH_REQUIRED","message":"登录状态已失效，请重新登录"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	_, err := New(server.Client()).ProductCatalog(context.Background(), Credentials{
		ID:           "nvtokens-main",
		PlatformType: "nvtokens",
		BaseURL:      server.URL,
		Token:        "expired-session",
		Username:     "buyer",
		Password:     "secret",
	})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized || httpErr.Code != "AUTH_REQUIRED" ||
		!strings.Contains(httpErr.Message, "更新 Session") {
		t.Fatalf("error = %#v, want actionable AUTH_REQUIRED error", err)
	}
	if loginCalls.Load() != 1 || catalogCalls.Load() != 1 {
		t.Fatalf("login=%d catalog=%d, want login=1 catalog=1", loginCalls.Load(), catalogCalls.Load())
	}
}

func TestNvtokensAutomaticRefresherRetriesOriginalRequestWithNewSession(t *testing.T) {
	var refreshCalls atomic.Int32
	var catalogCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspace/seller-candidates":
			catalogCalls.Add(1)
			cookie, _ := r.Cookie("session")
			if cookie == nil || cookie.Value != "fresh-session" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":"AUTH_REQUIRED"}`))
				return
			}
			_, _ = w.Write([]byte(`{"sellers":[{"sale_plan_counts":{"plus":2}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.Client())
	client.SetNvtokensSessionRefresher(func(ctx context.Context, credentials Credentials) (string, error) {
		refreshCalls.Add(1)
		return "fresh-session", nil
	})
	catalog, err := client.ProductCatalog(context.Background(), Credentials{
		ID:           "nvtokens-main",
		PlatformType: "nvtokens",
		BaseURL:      server.URL,
		Token:        "expired-session",
		Username:     "buyer",
		Password:     "secret",
	})
	if err != nil || len(catalog.Products) != 1 || catalog.Products[0].Available != 2 {
		t.Fatalf("catalog = %#v err=%v", catalog, err)
	}
	if refreshCalls.Load() != 1 || catalogCalls.Load() != 2 {
		t.Fatalf("refresh=%d catalog=%d, want refresh=1 catalog=2", refreshCalls.Load(), catalogCalls.Load())
	}
}

func TestNvtokensChallengeLoginReturnsAndValidatesSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/challenge-config":
			_, _ = w.Write([]byte(`{"provider":"turnstile","site_key":"site-key"}`))
		case "/api/login":
			var payload map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload["username"] != "buyer" || payload["password"] != "secret" || payload["cf-turnstile-response"] != "challenge-token" {
				t.Fatalf("login payload = %#v", payload)
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "renewed-session", Path: "/"})
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/me":
			cookie, _ := r.Cookie("session")
			if cookie == nil || cookie.Value != "renewed-session" {
				t.Fatalf("session cookie = %#v", cookie)
			}
			_, _ = w.Write([]byte(`{"id":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.Client())
	credentials := Credentials{PlatformType: "nvtokens", BaseURL: server.URL, Username: "buyer", Password: "secret"}
	challenge, err := client.NvtokensChallenge(context.Background(), credentials)
	if err != nil || challenge.Provider != "turnstile" || challenge.SiteKey != "site-key" {
		t.Fatalf("challenge = %#v err=%v", challenge, err)
	}
	session, err := client.LoginNvtokensWithChallenge(context.Background(), credentials, "challenge-token")
	if err != nil || session != "renewed-session" {
		t.Fatalf("session = %q err=%v", session, err)
	}
}

func TestNvtokensChallengeLoginUsesCurrentSCMSessionCookie(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/login":
			http.SetCookie(w, &http.Cookie{Name: "scm_session", Value: "current-session", Path: "/", HttpOnly: true, Secure: false})
			_, _ = w.Write([]byte(`{"user":{"id":"buyer"}}`))
		case "/api/me":
			cookie, err := r.Cookie("scm_session")
			if err != nil || cookie.Value != "current-session" {
				t.Fatalf("scm_session cookie = %#v err=%v", cookie, err)
			}
			_, _ = w.Write([]byte(`{"user":{"id":"buyer"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.Client())
	session, err := client.LoginNvtokensWithChallenge(context.Background(), Credentials{
		PlatformType: "nvtokens",
		BaseURL:      server.URL,
		Username:     "buyer",
		Password:     "secret",
	}, "challenge-token")
	if err != nil || session != "current-session" {
		t.Fatalf("session = %q err=%v", session, err)
	}
}

func TestNvtokensConfiguredSessionSendsCurrentAndLegacyCookies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workspace/seller-candidates" {
			http.NotFound(w, r)
			return
		}
		current, currentErr := r.Cookie("scm_session")
		legacy, legacyErr := r.Cookie("session")
		if currentErr != nil || current.Value != "saved-session" || legacyErr != nil || legacy.Value != "saved-session" {
			t.Fatalf("current=%#v currentErr=%v legacy=%#v legacyErr=%v", current, currentErr, legacy, legacyErr)
		}
		_, _ = w.Write([]byte(`{"sellers":[{"sale_plan_counts":{"plus":1}}]}`))
	}))
	defer server.Close()

	catalog, err := New(server.Client()).ProductCatalog(context.Background(), Credentials{
		ID:           "nvtokens-main",
		PlatformType: "nvtokens",
		BaseURL:      server.URL,
		Token:        "saved-session",
	})
	if err != nil || len(catalog.Products) != 1 {
		t.Fatalf("catalog = %#v err=%v", catalog, err)
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

func TestNvtokensMarketplaceSellerCandidatesAndPurchaseFilters(t *testing.T) {
	var estimateCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspace/seller-candidates":
			if got := r.URL.Query().Get("sale_plan_filter"); got != "plus" {
				t.Fatalf("sale_plan_filter = %q", got)
			}
			_, _ = w.Write([]byte(`{
				"data":{"sellers":[
					{"seller_token":"seller-a","selection_token":"select-a","display_name":"Seller A","channel_id":"channel-a","sale_plan_counts":{"plus":8},"sale_plan_prices":{"plus":{"min_cents":1200,"max_cents":1500}},"purchase_count":3,"purchased_before":true,"quality_score":92.5,"active_rate_percent":98.1},
					{"seller_token":"seller-b","display_name":"Seller B","available_count":2,"sale_plan_stats":{"plus":{"available_count":4,"price_min_cents":900,"price_max_cents":1000}}}
				]}
			}`))
		case "/api/workspace/extractions/estimate":
			estimateCalls.Add(1)
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode estimate payload: %v", err)
			}
			assertStringSlice := func(key, want string) {
				t.Helper()
				values, _ := payload[key].([]any)
				if len(values) != 1 || values[0] != want {
					t.Fatalf("%s = %#v, want [%q]", key, payload[key], want)
				}
			}
			assertStringSlice("preferred_sellers", "select-a")
			assertStringSlice("seller_whitelist", "select-a")
			assertStringSlice("seller_blacklist", "blocked-token")
			assertStringSlice("preferred_channel_ids", "channel-a")
			_, _ = w.Write([]byte(`{"estimate":{"matched_quantity":1,"buyer_total_cents":1200,"min_unit_price_cents":1200}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.Client())
	credentials := Credentials{PlatformType: "nvtokens", BaseURL: server.URL, Token: "session-token"}
	candidates, err := client.MarketplaceSellerCandidates(context.Background(), credentials, "oauth_30d")
	if err != nil || len(candidates) != 2 {
		t.Fatalf("candidates = %#v err=%v", candidates, err)
	}
	byID := make(map[string]MarketplaceSellerCandidate, len(candidates))
	for _, candidate := range candidates {
		byID[candidate.SellerID] = candidate
	}
	if seller := byID["seller-a"]; seller.SelectionToken != "select-a" || seller.ChannelID != "channel-a" || seller.Available != 8 || seller.MinUnitPriceFen != 1200 || seller.MaxUnitPriceFen != 1500 || seller.PurchaseCount != 3 || !seller.PurchasedBefore || seller.QualityScore == nil || *seller.QualityScore != 92.5 || seller.ActiveRatePercent == nil || *seller.ActiveRatePercent != 98.1 {
		t.Fatalf("seller-a = %#v", seller)
	}
	if seller := byID["seller-b"]; seller.SelectionToken != "seller-b" || seller.Available != 4 || seller.MinUnitPriceFen != 900 || seller.MaxUnitPriceFen != 1000 {
		t.Fatalf("seller-b = %#v", seller)
	}

	credentials.PreferredSellers = []string{"select-a"}
	credentials.SellerWhitelist = []string{"select-a"}
	credentials.SellerBlacklist = []string{"blocked-token"}
	credentials.PreferredChannelIDs = []string{"channel-a"}
	if _, err := client.Inventory(context.Background(), credentials, "plus", 1); err != nil {
		t.Fatalf("filtered inventory: %v", err)
	}
	if estimateCalls.Load() != 1 {
		t.Fatalf("estimate calls = %d", estimateCalls.Load())
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
