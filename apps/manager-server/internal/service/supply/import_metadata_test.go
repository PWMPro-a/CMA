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
