package supplyclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestClientCreatesPollsAndTakesOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"data":{"token":"token"}}`))
		case "/api/customer/pickup/orders":
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
	order, err := client.CreateOrder(context.Background(), credentials, "oauth_30d", 2)
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

func TestClientTakeReadsOrderedItemRemainingSeconds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/customer/login":
			_, _ = w.Write([]byte(`{"token":"token"}`))
		case "/api/customer/pickup/orders/order-lease/take":
			_, _ = w.Write([]byte(`{"order":{"id":"order-lease","status":"completed","items":[{"remaining_seconds":900},{"remaining_seconds":"1800"}]},"payload":{"accounts":[{"type":"codex","access_token":"a"},{"type":"codex","access_token":"b"}]}}`))
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
