package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/security"
)

func TestStoreEncryptsSetupAndManagerConfigSecrets(t *testing.T) {
	protector := newTestProtector(t)
	db, err := Open(t.TempDir()+"/usage.sqlite", protector)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})

	setup := Setup{
		CPAUpstreamURL: "http://cpa.local:8317",
		ManagementKey:  "management-key",
		Queue:          "usage",
		PopSide:        "right",
	}
	if err := db.SaveSetup(context.Background(), setup); err != nil {
		t.Fatalf("save setup: %v", err)
	}
	rawSetup := rawSettingValue(t, db, "setup")
	if strings.Contains(rawSetup, "management-key") || !strings.Contains(rawSetup, "enc:v1:") {
		t.Fatalf("setup was not encrypted at rest: %s", rawSetup)
	}
	loadedSetup, ok, err := db.LoadSetup(context.Background())
	if err != nil || !ok {
		t.Fatalf("load setup ok=%v err=%v", ok, err)
	}
	if loadedSetup.ManagementKey != "management-key" {
		t.Fatalf("loaded setup management key = %q", loadedSetup.ManagementKey)
	}

	managerCfg := ManagerConfig{
		CPAConnection: ManagerCPAConnectionConfig{
			CPABaseURL:    "http://cpa.local:8317",
			ManagementKey: "management-key",
		},
		Collector: ManagerCollectorConfig{
			Queue:          "usage",
			PopSide:        "right",
			BatchSize:      100,
			PollIntervalMS: 500,
			QueryLimit:     50000,
		},
		Supply: ManagerSupplyConfig{
			Username: "supply-user",
			Password: "supply-password",
			Platforms: []ManagerSupplyPlatformConfig{{
				ID: "nvtokens-main", Type: "nvtokens", BaseURL: "https://nvtokens.com", Product: "plus",
				ChallengeAPIKey: "challenge-secret",
			}},
		},
	}
	if err := db.SaveManagerConfig(context.Background(), managerCfg); err != nil {
		t.Fatalf("save manager config: %v", err)
	}
	rawManagerConfig := rawSettingValue(t, db, "manager_config_v1")
	if strings.Contains(rawManagerConfig, "management-key") || strings.Contains(rawManagerConfig, "supply-password") || strings.Contains(rawManagerConfig, "challenge-secret") || !strings.Contains(rawManagerConfig, "enc:v1:") {
		t.Fatalf("manager config was not encrypted at rest: %s", rawManagerConfig)
	}
	loadedManagerCfg, ok, err := db.LoadManagerConfig(context.Background())
	if err != nil || !ok {
		t.Fatalf("load manager config ok=%v err=%v", ok, err)
	}
	if loadedManagerCfg.CPAConnection.ManagementKey != "management-key" {
		t.Fatalf("loaded manager config management key = %q", loadedManagerCfg.CPAConnection.ManagementKey)
	}
	if loadedManagerCfg.Supply.Password != "supply-password" {
		t.Fatalf("loaded manager config supply password = %q", loadedManagerCfg.Supply.Password)
	}
	if loadedManagerCfg.Supply.Platforms[0].ChallengeAPIKey != "challenge-secret" {
		t.Fatalf("loaded challenge API key = %q", loadedManagerCfg.Supply.Platforms[0].ChallengeAPIKey)
	}
}

func TestStoreEncryptsPendingSupplyAccountPayloads(t *testing.T) {
	protector := newTestProtector(t)
	db, err := Open(t.TempDir()+"/supply.sqlite", protector)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.CreateSupplyOrder(context.Background(), SupplyOrder{
		OrderID: "order-1", Product: "oauth_30d", RequestedQuantity: 1, Status: "importing",
	}); err != nil {
		t.Fatalf("create supply order: %v", err)
	}
	payload := `{"type":"codex","access_token":"oauth-secret"}`
	if _, err := db.InsertSupplyImportItems(context.Background(), "order-1", []SupplyImportItem{{
		OrderID: "order-1", ItemKey: "item-1", FileName: "supply-item-1.json", PayloadJSON: payload,
	}}); err != nil {
		t.Fatalf("insert supply item: %v", err)
	}
	var raw string
	if err := db.db.QueryRow(`select payload_json from supply_import_items where item_key = 'item-1'`).Scan(&raw); err != nil {
		t.Fatalf("read raw supply payload: %v", err)
	}
	if strings.Contains(raw, "oauth-secret") || !strings.Contains(raw, "enc:v1:") {
		t.Fatalf("supply payload was not encrypted at rest: %s", raw)
	}
	items, err := db.ListPendingSupplyImportItems(context.Background(), "order-1", 1, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("load pending supply items=%#v err=%v", items, err)
	}
	if items[0].PayloadJSON != payload {
		t.Fatalf("decrypted supply payload = %q", items[0].PayloadJSON)
	}
}

func TestStoreListsOnlyActiveSupplyImportLeases(t *testing.T) {
	db, err := Open(t.TempDir() + "/supply.sqlite")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.CreateSupplyOrder(context.Background(), SupplyOrder{
		OrderID: "order-leases", Product: "oauth_30d", RequestedQuantity: 2, Status: "importing",
	}); err != nil {
		t.Fatalf("create order: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := db.InsertSupplyImportItems(context.Background(), "order-leases", []SupplyImportItem{
		{ItemKey: "active", FileName: "codex-supply-active.json", PayloadJSON: `{"access_token":"active"}`, LeaseExpiresAtMS: now + int64(time.Minute/time.Millisecond)},
		{ItemKey: "expired", FileName: "codex-supply-expired.json", PayloadJSON: `{"access_token":"expired"}`, LeaseExpiresAtMS: now - 1},
	}); err != nil {
		t.Fatalf("insert items: %v", err)
	}
	pending, err := db.ListPendingSupplyImportItems(context.Background(), "order-leases", now, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending items = %#v err=%v", pending, err)
	}
	for _, item := range pending {
		if err := db.MarkSupplyImportItemImported(context.Background(), item.ID, now); err != nil {
			t.Fatalf("mark imported: %v", err)
		}
	}
	active, err := db.ListActiveImportedSupplyItems(context.Background(), now)
	if err != nil || len(active) != 1 || active[0].FileName != "codex-supply-active.json" || active[0].ImportedAtMS != now {
		t.Fatalf("active leases = %#v err=%v", active, err)
	}
	current, err := db.ListCurrentImportedSupplyLeaseItems(context.Background())
	if err != nil || len(current) != 2 || current[0].FileName != "codex-supply-expired.json" || current[1].FileName != "codex-supply-active.json" {
		t.Fatalf("current leases = %#v err=%v", current, err)
	}
}

func TestStoreReadsLegacyPlaintextSecretsAndRewritesEncrypted(t *testing.T) {
	protector := newTestProtector(t)
	db, err := Open(t.TempDir()+"/usage.sqlite", protector)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})

	if _, err := db.db.Exec(
		`insert into settings(key, value, updated_at_ms) values('setup', ?, 1)`,
		`{"cpaBaseUrl":"http://cpa.local:8317","managementKey":"management-key","queue":"usage","popSide":"right"}`,
	); err != nil {
		t.Fatalf("insert legacy setup: %v", err)
	}
	setup, ok, err := db.LoadSetup(context.Background())
	if err != nil || !ok {
		t.Fatalf("load legacy setup ok=%v err=%v", ok, err)
	}
	if setup.ManagementKey != "management-key" {
		t.Fatalf("legacy setup management key = %q", setup.ManagementKey)
	}
	if err := db.SaveSetup(context.Background(), setup); err != nil {
		t.Fatalf("rewrite setup: %v", err)
	}
	rawSetup := rawSettingValue(t, db, "setup")
	if strings.Contains(rawSetup, "management-key") || !strings.Contains(rawSetup, "enc:v1:") {
		t.Fatalf("legacy setup was not rewritten encrypted: %s", rawSetup)
	}
}

func newTestProtector(t testing.TB) *security.Protector {
	t.Helper()
	protector, err := security.NewProtector([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("create protector: %v", err)
	}
	return protector
}

func rawSettingValue(t testing.TB, db *Store, key string) string {
	t.Helper()
	var raw string
	if err := db.db.QueryRow(`select value from settings where key = ?`, key).Scan(&raw); err != nil {
		t.Fatalf("load raw setting %s: %v", key, err)
	}
	return raw
}
