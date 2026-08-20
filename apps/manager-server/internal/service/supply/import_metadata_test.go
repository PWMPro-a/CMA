package supply

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

func TestWithSupplyAccountImportMetadata(t *testing.T) {
	importedAt := time.Date(2026, 8, 16, 15, 30, 45, 0, time.FixedZone("CST", 8*60*60))
	cfg := store.ManagerSupplyConfig{
		Platforms: []store.ManagerSupplyPlatformConfig{{
			ID:      "supplier-a",
			Name:    "平台 A",
			Type:    "sub2api",
			BaseURL: "https://supplier.example",
			Product: "oauth_30d",
		}},
	}
	account := normalizedSupplyAccount{payload: []byte(`{"type":"codex","access_token":"TOKEN"}`)}
	marked := withSupplyAccountImportMetadata(account, cfg, store.SupplyOrder{
		SupplierID: "supplier-a",
		Product:    "oauth_30d",
		Automatic:  false,
	}, importedAt)

	var payload map[string]any
	if err := json.Unmarshal(marked.payload, &payload); err != nil {
		t.Fatalf("decode marked payload: %v", err)
	}
	if payload["access_token"] != "TOKEN" {
		t.Fatalf("access_token = %#v", payload["access_token"])
	}
	marker, ok := payload["cpamp_import"].(map[string]any)
	if !ok {
		t.Fatalf("cpamp_import = %#v", payload["cpamp_import"])
	}
	if marker["platform_id"] != "supplier-a" || marker["platform_name"] != "平台 A" {
		t.Fatalf("platform marker = %#v", marker)
	}
	if marker["method"] != "manual_supply" || marker["source"] != "supply" {
		t.Fatalf("method marker = %#v", marker)
	}
	if marker["imported_at"] != "2026-08-16T07:30:45Z" {
		t.Fatalf("imported_at = %#v", marker["imported_at"])
	}
}

func TestWithSupplyAccountImportMetadataMarksAutomaticSupply(t *testing.T) {
	account := normalizedSupplyAccount{payload: []byte(`{"type":"codex","access_token":"TOKEN"}`)}
	marked := withSupplyAccountImportMetadata(account, store.ManagerSupplyConfig{}, store.SupplyOrder{
		SupplierID: "supplier-b",
		Automatic:  true,
	}, time.Unix(1, 0))

	var payload struct {
		Import struct {
			Method       string `json:"method"`
			PlatformID   string `json:"platform_id"`
			PlatformName string `json:"platform_name"`
		} `json:"cpamp_import"`
	}
	if err := json.Unmarshal(marked.payload, &payload); err != nil {
		t.Fatalf("decode marked payload: %v", err)
	}
	if payload.Import.Method != "automatic_supply" {
		t.Fatalf("method = %q", payload.Import.Method)
	}
	if payload.Import.PlatformID != "supplier-b" || payload.Import.PlatformName != "supplier-b" {
		t.Fatalf("platform = %#v", payload.Import)
	}
}

func TestWithSupplyAccountImportMetadataMarksLowPriceReserve(t *testing.T) {
	account := normalizedSupplyAccount{payload: []byte(`{"type":"codex","access_token":"TOKEN"}`)}
	marked := withSupplyAccountImportMetadata(account, store.ManagerSupplyConfig{}, store.SupplyOrder{
		SupplierID:    "nvtokens",
		Automatic:     true,
		TriggerReason: lowPriceReserveTriggerReason,
	}, time.Unix(1, 0))

	var payload struct {
		Import struct {
			Method string `json:"method"`
		} `json:"cpamp_import"`
	}
	if err := json.Unmarshal(marked.payload, &payload); err != nil {
		t.Fatalf("decode marked payload: %v", err)
	}
	if payload.Import.Method != lowPriceReserveTriggerReason {
		t.Fatalf("method = %q", payload.Import.Method)
	}
}

func TestNvtokensWarrantyMetadataDoesNotBecomeSchedulingExpiry(t *testing.T) {
	now := time.Date(2026, 8, 21, 6, 0, 0, 0, time.UTC)
	warrantyExpiresAtMS := now.Add(45 * time.Minute).UnixMilli()
	account := normalizedSupplyAccount{payload: []byte(`{
		"type":"codex",
		"expires_at":"2026-08-31T06:00:00Z",
		"supply_lease_expires_at_ms":123,
		"supply_lease_expires_at":"1970-01-01T00:00:00.123Z"
	}`)}
	account = withSupplyAccountWarrantyMetadata(account, warrantyExpiresAtMS)

	var payload map[string]any
	if err := json.Unmarshal(account.payload, &payload); err != nil {
		t.Fatalf("decode warranty metadata: %v", err)
	}
	if got := int64(numberField(payload, "supply_warranty_expires_at_ms")); got != warrantyExpiresAtMS {
		t.Fatalf("warranty expiry = %d, want %d", got, warrantyExpiresAtMS)
	}
	if got := int64(numberField(payload, "supply_lease_expires_at_ms")); got != 0 || stringFromMap(payload, "supply_lease_expires_at") != "" {
		t.Fatalf("supplier lease metadata survived warranty conversion: %#v", payload)
	}
	if _, found := smartSupplyLeaseExpiry(payload, now); found {
		t.Fatal("warranty metadata must not be read as a scheduling lease")
	}
	if got := supplyAccountExpiryAtMS(payload, "", now); got != time.Date(2026, 8, 31, 6, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("real account expiry = %d", got)
	}
}
