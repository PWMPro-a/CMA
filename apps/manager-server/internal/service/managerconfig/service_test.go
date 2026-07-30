package managerconfig

import (
	"testing"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

func TestNormalizeSupplyConfigKeepsPasswordWhenIdentityUnchanged(t *testing.T) {
	current := store.ManagerSupplyConfig{
		BaseURL:  "https://sogouedu.cc",
		Username: "customer-a",
		Password: "saved-password",
		Product:  "oauth_7d",
	}
	next := NormalizeSupplyConfig(store.ManagerSupplyConfig{
		BaseURL:  "https://sogouedu.cc/",
		Username: "customer-a",
		Product:  "oauth_30d",
	}, current)
	if next.Password != "saved-password" {
		t.Fatalf("password = %q, want saved password", next.Password)
	}
	if !next.PasswordConfigured {
		t.Fatal("password should stay configured")
	}
}

func TestNormalizeSupplyConfigClearsPasswordWhenSupplyIdentityChangesWithoutPassword(t *testing.T) {
	current := store.ManagerSupplyConfig{
		BaseURL:  "https://sogouedu.cc",
		Username: "customer-a",
		Password: "saved-password",
		Product:  "oauth_7d",
	}
	for _, submitted := range []store.ManagerSupplyConfig{
		{BaseURL: "https://other.example", Username: "customer-a", Product: "oauth_7d"},
		{BaseURL: "https://sogouedu.cc", Username: "customer-b", Product: "oauth_7d"},
	} {
		next := NormalizeSupplyConfig(submitted, current)
		if next.Password != "" {
			t.Fatalf("password = %q, want cleared for submitted=%#v", next.Password, submitted)
		}
		if next.PasswordConfigured {
			t.Fatalf("passwordConfigured = true, want false for submitted=%#v", submitted)
		}
	}
}

func TestNormalizeSupplyConfigReplacesPasswordWhenIdentityChangesWithPassword(t *testing.T) {
	current := store.ManagerSupplyConfig{
		BaseURL:  "https://sogouedu.cc",
		Username: "customer-a",
		Password: "saved-password",
		Product:  "oauth_7d",
	}
	next := NormalizeSupplyConfig(store.ManagerSupplyConfig{
		BaseURL:  "https://sogouedu.cc",
		Username: "customer-b",
		Password: "new-password",
		Product:  "oauth_7d",
	}, current)
	if next.Password != "new-password" {
		t.Fatalf("password = %q, want new password", next.Password)
	}
	if !next.PasswordConfigured {
		t.Fatal("password should be configured")
	}
}
