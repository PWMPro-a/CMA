package quotacooldown_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

func TestUpsertActiveUsesCredentialIdentity(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	first, err := st.QuotaCooldowns.UpsertActive(ctx, model.QuotaCooldownUpsert{
		AuthFileName:    "shared.json",
		AuthIndex:       "auth-1",
		AccountSnapshot: "old@example.com",
		Provider:        "x_ai",
		RecoverAtMS:     1_000,
		Owner:           model.QuotaCooldownOwnerXAIFreeUsage,
	})
	if err != nil {
		t.Fatalf("insert indexed cooldown: %v", err)
	}
	updated, err := st.QuotaCooldowns.UpsertActive(ctx, model.QuotaCooldownUpsert{
		AuthFileName:    "shared.json",
		AuthIndex:       "auth-1",
		AccountSnapshot: "renamed@example.com",
		Provider:        "xai",
		RecoverAtMS:     2_000,
		Owner:           model.QuotaCooldownOwnerXAIFreeUsage,
	})
	if err != nil {
		t.Fatalf("update indexed cooldown: %v", err)
	}
	if updated.ID != first.ID || updated.AccountSnapshot != "renamed@example.com" || updated.Provider != "xai" || updated.RecoverAtMS != 2_000 {
		t.Fatalf("updated indexed cooldown = %#v, first = %#v", updated, first)
	}

	for _, account := range []string{"alice@example.com", "bob@example.com"} {
		if _, err := st.QuotaCooldowns.UpsertActive(ctx, model.QuotaCooldownUpsert{
			AuthFileName:    "shared.json",
			AccountSnapshot: account,
			Provider:        "codex",
			RecoverAtMS:     3_000,
			Owner:           model.QuotaCooldownOwnerUsage429,
		}); err != nil {
			t.Fatalf("insert fallback cooldown %q: %v", account, err)
		}
	}

	active, err := st.QuotaCooldowns.ListActive(ctx)
	if err != nil {
		t.Fatalf("list active cooldowns: %v", err)
	}
	if len(active) != 3 {
		t.Fatalf("active cooldowns = %#v, want three credential identities", active)
	}
}

func TestDeleteCredentialRemovesOnlyMatchingCooldown(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "cleanup.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for _, account := range []string{"elise@example.com", "other@example.com"} {
		if _, err := st.QuotaCooldowns.UpsertActive(ctx, model.QuotaCooldownUpsert{
			AuthFileName: "shared.json", AccountSnapshot: account, Provider: "codex",
			RecoverAtMS: 10_000, Owner: model.QuotaCooldownOwnerUsage429,
		}); err != nil {
			t.Fatalf("seed cooldown %s: %v", account, err)
		}
	}
	deleted, err := st.QuotaCooldowns.DeleteCredential(ctx, model.CredentialIdentity{
		AuthFileName: "shared.json", AccountSnapshot: "elise@example.com", Provider: "codex",
	})
	if err != nil || deleted != 1 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	active, err := st.QuotaCooldowns.ListActive(ctx)
	if err != nil || len(active) != 1 || active[0].AccountSnapshot != "other@example.com" {
		t.Fatalf("remaining cooldowns=%#v err=%v", active, err)
	}
}

func TestDeleteCredentialWithAuthIndexAlsoRemovesLegacyFallbackCooldown(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "cleanup-legacy.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	now := int64(1_800_000_000_000)
	if _, err := st.SQLDB().ExecContext(ctx, `insert into quota_cooldowns (
		auth_file_name, account_snapshot, provider, recover_at_ms, owner, pre_disabled_state, status,
		disabled_at_ms, created_at_ms, updated_at_ms
	) values (?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
		"shared.json", "elise@example.com", "codex", now+60_000,
		model.QuotaCooldownOwnerUsage429, model.QuotaCooldownStatusActive, now, now, now); err != nil {
		t.Fatalf("seed legacy cooldown: %v", err)
	}
	deleted, err := st.QuotaCooldowns.DeleteCredential(ctx, model.CredentialIdentity{
		AuthFileName: "shared.json", AuthIndex: "new-auth-index", Provider: "codex", AccountSnapshot: "elise@example.com",
	})
	if err != nil || deleted != 1 {
		t.Fatalf("deleted=%d err=%v, want legacy fallback cooldown removed", deleted, err)
	}
}

func TestUpsertActiveUpgradesFallbackIdentityAndPreservesOwnershipOrigin(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	first, err := st.QuotaCooldowns.UpsertActive(ctx, model.QuotaCooldownUpsert{
		AuthFileName:     "shared.json",
		AccountSnapshot:  "user@example.com",
		Provider:         "x_ai",
		RecoverAtMS:      1_000,
		Owner:            model.QuotaCooldownOwnerUsage429,
		PreDisabledState: false,
		DisabledAtMS:     100,
	})
	if err != nil {
		t.Fatalf("insert fallback cooldown: %v", err)
	}
	upgraded, err := st.QuotaCooldowns.UpsertActive(ctx, model.QuotaCooldownUpsert{
		AuthFileName:     "shared.json",
		AuthIndex:        "auth-1",
		AccountSnapshot:  "user@example.com",
		Provider:         "xai",
		RecoverAtMS:      2_000,
		Owner:            model.QuotaCooldownOwnerUsage429,
		PreDisabledState: true,
		DisabledAtMS:     200,
	})
	if err != nil {
		t.Fatalf("upgrade fallback cooldown: %v", err)
	}
	if upgraded.ID != first.ID || upgraded.AuthIndex != "auth-1" || upgraded.Provider != "xai" || upgraded.RecoverAtMS != 2_000 {
		t.Fatalf("upgraded cooldown = %#v, first = %#v", upgraded, first)
	}
	if upgraded.PreDisabledState || upgraded.DisabledAtMS != 100 {
		t.Fatalf("ownership origin changed during extension: %#v", upgraded)
	}
}

func TestUpsertActiveKeepsMetadataForWinningRecovery(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	first, err := st.QuotaCooldowns.UpsertActive(ctx, model.QuotaCooldownUpsert{
		AuthFileName: "codex.json",
		AuthIndex:    "auth-1",
		Provider:     "codex",
		ReasonCode:   "weekly_limit",
		WindowKind:   "weekly",
		EvidenceJSON: `{"recover_at_ms":2000,"source":"weekly"}`,
		RecoverAtMS:  2_000,
		Owner:        model.QuotaCooldownOwnerUsage429,
		EventHash:    "evt-weekly",
	})
	if err != nil {
		t.Fatalf("insert winning cooldown: %v", err)
	}

	shorter, err := st.QuotaCooldowns.UpsertActive(ctx, model.QuotaCooldownUpsert{
		AuthFileName: "codex.json",
		AuthIndex:    "auth-1",
		Provider:     "codex",
		ReasonCode:   "five_hour_limit",
		WindowKind:   "five_hour",
		EvidenceJSON: `{"recover_at_ms":1000,"source":"five-hour"}`,
		RecoverAtMS:  1_000,
		Owner:        model.QuotaCooldownOwnerUsage429,
		EventHash:    "evt-five-hour",
	})
	if err != nil {
		t.Fatalf("upsert shorter cooldown: %v", err)
	}
	if shorter.ID != first.ID || shorter.RecoverAtMS != 2_000 || shorter.ReasonCode != "weekly_limit" || shorter.WindowKind != "weekly" || shorter.EvidenceJSON != `{"recover_at_ms":2000,"source":"weekly"}` || shorter.EventHash != "evt-weekly" {
		t.Fatalf("shorter cooldown replaced winning metadata: %#v", shorter)
	}

	longer, err := st.QuotaCooldowns.UpsertActive(ctx, model.QuotaCooldownUpsert{
		AuthFileName: "codex.json",
		AuthIndex:    "auth-1",
		Provider:     "codex",
		ReasonCode:   "monthly_limit",
		WindowKind:   "monthly",
		EvidenceJSON: `{"recover_at_ms":3000,"source":"monthly"}`,
		RecoverAtMS:  3_000,
		Owner:        model.QuotaCooldownOwnerUsage429,
		EventHash:    "evt-monthly",
	})
	if err != nil {
		t.Fatalf("upsert longer cooldown: %v", err)
	}
	if longer.RecoverAtMS != 3_000 || longer.ReasonCode != "monthly_limit" || longer.WindowKind != "monthly" || longer.EvidenceJSON != `{"recover_at_ms":3000,"source":"monthly"}` || longer.EventHash != "evt-monthly" {
		t.Fatalf("longer cooldown did not replace winning metadata: %#v", longer)
	}
}
