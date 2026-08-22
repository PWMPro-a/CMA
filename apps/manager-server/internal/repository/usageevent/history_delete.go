package usageevent

import (
	"context"
	"fmt"
	"strings"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/usageidentity"
)

// DeleteCredentialHistory clears raw request data and credential-scoped
// projections after a successful Codex reset. The reset operation remains as
// the durable audit record.
func (r *repository) DeleteCredentialHistory(ctx context.Context, authFileSnapshot, authIndex string) (int64, error) {
	return r.DeleteCredentialIdentityHistory(ctx, model.CredentialIdentity{
		AuthFileName: authFileSnapshot,
		AuthIndex:    authIndex,
	})
}

func (r *repository) DeleteCredentialIdentityHistory(ctx context.Context, identity model.CredentialIdentity) (int64, error) {
	authFileSnapshot := strings.TrimSpace(identity.AuthFileName)
	if authFileSnapshot == "" {
		return 0, fmt.Errorf("credential history identity is incomplete")
	}
	authIndex := strings.TrimSpace(identity.AuthIndex)
	accountID := strings.TrimSpace(identity.AccountID)
	provider := strings.TrimSpace(identity.Provider)
	accountSnapshot := strings.TrimSpace(identity.AccountSnapshot)
	if authIndex == "" && accountID == "" && (provider == "" || accountSnapshot == "") {
		return 0, fmt.Errorf("credential history identity is incomplete")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	where, args := credentialHistoryWhere(authFileSnapshot, authIndex, accountID, provider, accountSnapshot)
	rows, err := tx.QueryContext(ctx, `select id, event_hash,
		coalesce(account_snapshot, ''), coalesce(auth_label_snapshot, ''),
		coalesce(auth_file_snapshot, ''), coalesce(auth_provider_snapshot, ''),
		coalesce(auth_index, ''), coalesce(source, ''), coalesce(provider, '')
		from usage_events
		where `+where, args...)
	if err != nil {
		return 0, err
	}
	var ids []int64
	var hashes []string
	accountKeys := make(map[string]struct{})
	for rows.Next() {
		var id int64
		var hash, account, label, file, provider, index, source, eventProvider string
		if err := rows.Scan(&id, &hash, &account, &label, &file, &provider, &index, &source, &eventProvider); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		hashes = append(hashes, hash)
		key, ok := usageidentity.AccountKey(usageidentity.Fields{
			AuthFileSnapshot: file, AuthIndex: index, AuthProviderSnapshot: provider,
			AccountSnapshot: account, AuthLabelSnapshot: label, Source: source,
		})
		if !ok {
			key, _ = usageidentity.AccountKey(usageidentity.Fields{
				AuthFileSnapshot: authFileSnapshot, AuthIndex: authIndex,
				AuthProviderSnapshot: eventProvider, AccountSnapshot: account,
			})
		}
		if key != "" {
			accountKeys[key] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(ids) > 0 {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		if _, err := tx.ExecContext(ctx, `delete from usage_event_identity_ledger where raw_event_id in (`+placeholders+`)`, args...); err != nil {
			return 0, err
		}
		hashPlaceholders := strings.TrimRight(strings.Repeat("?,", len(hashes)), ",")
		hashArgs := make([]any, len(hashes))
		for i, hash := range hashes {
			hashArgs[i] = hash
		}
		if _, err := tx.ExecContext(ctx, `delete from usage_event_identity_ledger where event_hash in (`+hashPlaceholders+`)`, hashArgs...); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `delete from usage_monitoring_event_projection_v1 where event_id in (`+placeholders+`)`, args...); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `delete from usage_monitoring_header_latest_v1 where event_id in (`+placeholders+`)`, args...); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `delete from usage_events where id in (`+placeholders+`)`, args...); err != nil {
			return 0, err
		}
	}

	if len(accountKeys) > 0 {
		keys := make([]string, 0, len(accountKeys))
		for key := range accountKeys {
			keys = append(keys, key)
		}
		keyPlaceholders := strings.TrimRight(strings.Repeat("?,", len(keys)), ",")
		keyArgs := make([]any, len(keys))
		for i, key := range keys {
			keyArgs[i] = key
		}
		for _, table := range []string{"usage_account_model_rollups", "usage_pricing_account_rollups_v1"} {
			if _, err := tx.ExecContext(ctx, `delete from `+table+` where account_key in (`+keyPlaceholders+`)`, keyArgs...); err != nil {
				return 0, err
			}
		}
	}
	// Clear legacy account rollups even when the raw events were already
	// removed by an earlier partial cleanup.
	for _, table := range []string{"usage_account_model_rollups", "usage_pricing_account_rollups_v1"} {
		legacyWhere, legacyArgs := credentialRollupWhere(authFileSnapshot, authIndex, provider, accountSnapshot)
		if legacyWhere == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `delete from `+table+`
			where `+legacyWhere, legacyArgs...); err != nil {
			return 0, err
		}
	}
	monitoringWhere, monitoringArgs := credentialMonitoringWhere(authFileSnapshot, authIndex, provider, accountSnapshot)
	if _, err := tx.ExecContext(ctx, `delete from usage_monitoring_account_daily_rollups_v1
		where `+monitoringWhere, monitoringArgs...); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int64(len(ids)), nil
}

func credentialHistoryWhere(fileName, authIndex, accountID, provider, accountSnapshot string) (string, []any) {
	fileWhere := `(lower(trim(coalesce(auth_file_snapshot, ''))) = lower(trim(?))
		or (trim(coalesce(auth_file_snapshot, '')) = '' and lower(trim(coalesce(source, ''))) = lower(trim(?))))`
	args := []any{fileName, fileName}
	if authIndex != "" {
		indexWhere := `lower(trim(coalesce(auth_index, ''))) = lower(trim(?))`
		fallbackWhere, fallbackArgs := credentialHistoryIdentityFallbackWhere(accountID, provider, accountSnapshot)
		if fallbackWhere != "" {
			return `(` + indexWhere + ` or (trim(coalesce(auth_index, '')) = '' and ` + fallbackWhere + `)) and ` + fileWhere,
				append(append([]any{authIndex}, fallbackArgs...), args...)
		}
		return indexWhere + ` and ` + fileWhere, append([]any{authIndex}, args...)
	}
	if accountID != "" {
		if provider != "" && accountSnapshot != "" {
			return `(lower(trim(coalesce(auth_project_id_snapshot, ''))) = lower(trim(?))
				or (lower(trim(coalesce(nullif(auth_provider_snapshot, ''), provider, ''))) = lower(trim(?))
					and lower(trim(coalesce(account_snapshot, ''))) = lower(trim(?)))) and ` + fileWhere,
				append([]any{accountID, provider, accountSnapshot}, args...)
		}
		return `lower(trim(coalesce(auth_project_id_snapshot, ''))) = lower(trim(?)) and ` + fileWhere,
			append([]any{accountID}, args...)
	}
	return `lower(trim(coalesce(nullif(auth_provider_snapshot, ''), provider, ''))) = lower(trim(?))
		and lower(trim(coalesce(account_snapshot, ''))) = lower(trim(?)) and ` + fileWhere,
		append([]any{provider, accountSnapshot}, args...)
}

func credentialIdentityFallbackWhere(accountID, provider, accountSnapshot string) (string, []any) {
	return credentialIdentityFallbackWhereWithProvider(accountID, provider, accountSnapshot, "auth_provider_snapshot")
}

func credentialHistoryIdentityFallbackWhere(accountID, provider, accountSnapshot string) (string, []any) {
	return credentialIdentityFallbackWhereWithProvider(accountID, provider, accountSnapshot, "coalesce(nullif(auth_provider_snapshot, ''), provider, '')")
}

func credentialIdentityFallbackWhereWithProvider(accountID, provider, accountSnapshot, providerExpression string) (string, []any) {
	clauses := make([]string, 0, 2)
	args := make([]any, 0, 3)
	accountID = strings.TrimSpace(accountID)
	provider = strings.TrimSpace(provider)
	accountSnapshot = strings.TrimSpace(accountSnapshot)
	if accountID != "" {
		clauses = append(clauses, `lower(trim(coalesce(auth_project_id_snapshot, ''))) = lower(trim(?))`)
		args = append(args, accountID)
	}
	if provider != "" && accountSnapshot != "" {
		clauses = append(clauses, `(lower(trim(`+providerExpression+`)) = lower(trim(?))
			and lower(trim(coalesce(account_snapshot, ''))) = lower(trim(?)))`)
		args = append(args, provider, accountSnapshot)
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return `(` + strings.Join(clauses, ` or `) + `)`, args
}

func credentialRollupWhere(fileName, authIndex, provider, accountSnapshot string) (string, []any) {
	if authIndex != "" {
		indexWhere := `lower(trim(coalesce(auth_index, ''))) = lower(trim(?))`
		fallbackWhere, fallbackArgs := credentialIdentityFallbackWhere("", provider, accountSnapshot)
		if fallbackWhere != "" {
			return `(` + indexWhere + ` or (trim(coalesce(auth_index, '')) = '' and ` + fallbackWhere + `))
			and lower(trim(coalesce(source, ''))) = lower(trim(?))`, append(append([]any{authIndex}, fallbackArgs...), fileName)
		}
		return indexWhere + ` and lower(trim(coalesce(source, ''))) = lower(trim(?))`, []any{authIndex, fileName}
	}
	if provider == "" || accountSnapshot == "" {
		return "", nil
	}
	return `lower(trim(coalesce(auth_provider_snapshot, ''))) = lower(trim(?))
		and lower(trim(coalesce(account_snapshot, ''))) = lower(trim(?))
		and lower(trim(coalesce(source, ''))) = lower(trim(?))`, []any{provider, accountSnapshot, fileName}
}

func credentialMonitoringWhere(fileName, authIndex, provider, accountSnapshot string) (string, []any) {
	fileWhere := `(lower(trim(coalesce(auth_file_snapshot, ''))) = lower(trim(?))
		or (trim(coalesce(auth_file_snapshot, '')) = '' and lower(trim(coalesce(source, ''))) = lower(trim(?))))`
	if authIndex != "" {
		indexWhere := `lower(trim(coalesce(auth_index, ''))) = lower(trim(?))`
		fallbackWhere, fallbackArgs := credentialIdentityFallbackWhere("", provider, accountSnapshot)
		if fallbackWhere != "" {
			return `(` + indexWhere + ` or (trim(coalesce(auth_index, '')) = '' and ` + fallbackWhere + `)) and ` + fileWhere,
				append(append([]any{authIndex}, fallbackArgs...), fileName, fileName)
		}
		return indexWhere + ` and ` + fileWhere, []any{authIndex, fileName, fileName}
	}
	return `lower(trim(coalesce(provider, ''))) = lower(trim(?))
		and lower(trim(coalesce(account_snapshot, ''))) = lower(trim(?)) and ` + fileWhere,
		[]any{provider, accountSnapshot, fileName, fileName}
}
