package quotathreshold

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

type Repository interface {
	List(ctx context.Context) ([]model.QuotaThresholdRule, error)
	UpsertBatch(ctx context.Context, rules []model.QuotaThresholdRule) ([]model.QuotaThresholdRule, error)
	Delete(ctx context.Context, id int64) error
	UpdateObservation(ctx context.Context, id int64, remaining *float64, disabled bool, inspectionAtMS, triggeredAtMS int64, lastError string) error
	DeleteCredential(ctx context.Context, identity model.CredentialIdentity) (int64, error)
}

type repository struct{ db *sql.DB }

func New(db *sql.DB) Repository { return &repository{db: db} }

func normalizeRule(rule model.QuotaThresholdRule) (model.QuotaThresholdRule, error) {
	rule.FileName = strings.TrimSpace(rule.FileName)
	rule.AuthIndex = strings.TrimSpace(rule.AuthIndex)
	rule.Provider = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(rule.Provider), "_", "-"))
	rule.AccountSnapshot = strings.TrimSpace(rule.AccountSnapshot)
	rule.AccountID = strings.TrimSpace(rule.AccountID)
	if rule.FileName == "" {
		return model.QuotaThresholdRule{}, errors.New("fileName is required")
	}
	if rule.ThresholdPercent < 0 || rule.ThresholdPercent > 100 {
		return model.QuotaThresholdRule{}, errors.New("thresholdPercent must be between 0 and 100")
	}
	return rule, nil
}

func (r *repository) List(ctx context.Context) ([]model.QuotaThresholdRule, error) {
	rows, err := r.db.QueryContext(ctx, `select id, file_name, auth_index, provider, account_snapshot, account_id,
		threshold_percent, enabled, last_observed_remaining_percent, last_disabled, last_triggered_at_ms,
		last_inspection_at_ms, last_error, created_at_ms, updated_at_ms
		from quota_threshold_rules order by updated_at_ms desc, id desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.QuotaThresholdRule, 0)
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func scan(scanner interface{ Scan(...any) error }) (model.QuotaThresholdRule, error) {
	var item model.QuotaThresholdRule
	var authIndex, provider, snapshot, accountID, lastErr sql.NullString
	var remaining sql.NullFloat64
	var lastTriggered, lastInspection, created, updated sql.NullInt64
	var enabled, disabled int
	if err := scanner.Scan(&item.ID, &item.FileName, &authIndex, &provider, &snapshot, &accountID,
		&item.ThresholdPercent, &enabled, &remaining, &disabled, &lastTriggered,
		&lastInspection, &lastErr, &created, &updated); err != nil {
		return item, err
	}
	item.AuthIndex, item.Provider, item.AccountSnapshot, item.AccountID, item.LastError = authIndex.String, provider.String, snapshot.String, accountID.String, lastErr.String
	item.Enabled, item.LastDisabled = enabled != 0, disabled != 0
	item.LastTriggeredAtMS, item.LastInspectionAtMS, item.CreatedAtMS, item.UpdatedAtMS = lastTriggered.Int64, lastInspection.Int64, created.Int64, updated.Int64
	if remaining.Valid {
		value := remaining.Float64
		item.LastObservedRemainingPercent = &value
	}
	return item, nil
}

func (r *repository) UpsertBatch(ctx context.Context, rules []model.QuotaThresholdRule) ([]model.QuotaThresholdRule, error) {
	if len(rules) == 0 {
		return r.List(ctx)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	for _, input := range rules {
		rule, err := normalizeRule(input)
		if err != nil {
			return nil, err
		}
		var id int64
		err = tx.QueryRowContext(ctx, `select id from quota_threshold_rules where lower(trim(file_name))=lower(trim(?))
			and lower(trim(coalesce(auth_index,'')))=lower(trim(?))
			and lower(trim(coalesce(provider,'')))=lower(trim(?))
			and lower(trim(coalesce(account_snapshot,'')))=lower(trim(?))
			and lower(trim(coalesce(account_id,'')))=lower(trim(?)) limit 1`, rule.FileName, rule.AuthIndex, rule.Provider, rule.AccountSnapshot, rule.AccountID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			created := rule.CreatedAtMS
			if created <= 0 {
				created = now
			}
			res, execErr := tx.ExecContext(ctx, `insert into quota_threshold_rules
				(file_name, auth_index, provider, account_snapshot, account_id, threshold_percent, enabled,
				 last_disabled, created_at_ms, updated_at_ms) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				rule.FileName, nullString(rule.AuthIndex), nullString(rule.Provider), nullString(rule.AccountSnapshot), nullString(rule.AccountID), rule.ThresholdPercent, boolInt(rule.Enabled), boolInt(rule.LastDisabled), created, now)
			if execErr != nil {
				return nil, execErr
			}
			id, err = res.LastInsertId()
			if err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		} else {
			_, err = tx.ExecContext(ctx, `update quota_threshold_rules set file_name=?, auth_index=?, provider=?, account_snapshot=?, account_id=?, threshold_percent=?, enabled=?, updated_at_ms=? where id=?`,
				rule.FileName, nullString(rule.AuthIndex), nullString(rule.Provider), nullString(rule.AccountSnapshot), nullString(rule.AccountID), rule.ThresholdPercent, boolInt(rule.Enabled), now, id)
			if err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return r.List(ctx)
}

func (r *repository) Delete(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("rule id is required")
	}
	res, err := r.db.ExecContext(ctx, `delete from quota_threshold_rules where id = ?`, id)
	if err != nil {
		return err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (r *repository) UpdateObservation(ctx context.Context, id int64, remaining *float64, disabled bool, inspectionAtMS, triggeredAtMS int64, lastError string) error {
	if id <= 0 {
		return errors.New("rule id is required")
	}
	now := time.Now().UnixMilli()
	_, err := r.db.ExecContext(ctx, `update quota_threshold_rules set last_observed_remaining_percent=?, last_disabled=?, last_inspection_at_ms=coalesce(?, last_inspection_at_ms), last_triggered_at_ms=coalesce(?, last_triggered_at_ms), last_error=?, updated_at_ms=? where id=?`,
		nullFloat(remaining), boolInt(disabled), nullPositive(inspectionAtMS), nullPositive(triggeredAtMS), nullString(strings.TrimSpace(lastError)), now, id)
	return err
}

func (r *repository) DeleteCredential(ctx context.Context, identity model.CredentialIdentity) (int64, error) {
	fileName := strings.TrimSpace(identity.AuthFileName)
	if fileName == "" {
		return 0, errors.New("quota threshold credential file name is required")
	}
	where := `lower(trim(file_name)) = lower(trim(?))`
	args := []any{fileName}
	if authIndex := strings.TrimSpace(identity.AuthIndex); authIndex != "" {
		where += ` and (lower(trim(coalesce(auth_index, ''))) = lower(trim(?))`
		args = append(args, authIndex)
		where += `)`
	} else if accountID := strings.TrimSpace(identity.AccountID); accountID != "" {
		where += ` and lower(trim(coalesce(account_id, ''))) = lower(trim(?))`
		args = append(args, accountID)
	} else {
		provider := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(identity.Provider), "_", "-"))
		snapshot := strings.TrimSpace(identity.AccountSnapshot)
		if provider == "" || snapshot == "" {
			return 0, nil
		}
		where += ` and lower(trim(coalesce(provider, ''))) = lower(trim(?)) and lower(trim(coalesce(account_snapshot, ''))) = lower(trim(?))`
		args = append(args, provider, snapshot)
	}
	res, err := r.db.ExecContext(ctx, `delete from quota_threshold_rules where `+where, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func nullString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
func nullPositive(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}
func nullFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
