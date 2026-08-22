package codexquotaoperation

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

var ErrNotFound = errors.New("codex quota operation not found")

var ErrAccountBusy = errors.New("codex quota account already has an active operation")

type Repository interface {
	Create(ctx context.Context, operation model.CodexQuotaOperation) (model.CodexQuotaOperation, bool, error)
	Get(ctx context.Context, operationID string) (model.CodexQuotaOperation, bool, error)
	GetActiveByAccount(ctx context.Context, accountKey string) (model.CodexQuotaOperation, bool, error)
	Update(ctx context.Context, operation model.CodexQuotaOperation) (model.CodexQuotaOperation, error)
	UpdateIfState(ctx context.Context, operation model.CodexQuotaOperation, expectedState string) (model.CodexQuotaOperation, bool, error)
	CountCompletedByAccount(ctx context.Context, accountKey string) (int64, error)
	CountCompletedByCredential(ctx context.Context, accountKey string, authIndex string) (int64, error)
}

type repository struct {
	db *sql.DB
}

func New(db *sql.DB) Repository {
	return &repository{db: db}
}

func (r *repository) Create(ctx context.Context, operation model.CodexQuotaOperation) (model.CodexQuotaOperation, bool, error) {
	operation.OperationID = strings.TrimSpace(operation.OperationID)
	operation.AccountKey = strings.TrimSpace(operation.AccountKey)
	operation.AuthIndex = strings.TrimSpace(operation.AuthIndex)
	operation.AuthFileName = strings.TrimSpace(operation.AuthFileName)
	operation.State = strings.TrimSpace(operation.State)
	if operation.OperationID == "" || operation.AccountKey == "" || operation.AuthIndex == "" || operation.State == "" {
		return model.CodexQuotaOperation{}, false, errors.New("codex quota operation identity is incomplete")
	}
	now := time.Now().UnixMilli()
	if operation.CreatedAtMS <= 0 {
		operation.CreatedAtMS = now
	}
	if operation.UpdatedAtMS <= 0 {
		operation.UpdatedAtMS = now
	}
	result, err := r.db.ExecContext(ctx, `insert into codex_quota_operations (
		operation_id, account_key, auth_index, auth_file_name, state, consumed,
		upstream_status, warning_codes_json, result_json, last_error, created_at_ms, updated_at_ms
	) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	 on conflict(operation_id) do nothing`,
		operation.OperationID,
		operation.AccountKey,
		operation.AuthIndex,
		nullString(operation.AuthFileName),
		operation.State,
		nullBool(operation.Consumed),
		nullInt(operation.UpstreamStatus),
		nullString(operation.WarningCodesJSON),
		nullString(operation.ResultJSON),
		nullString(operation.LastError),
		operation.CreatedAtMS,
		operation.UpdatedAtMS,
	)
	if err != nil {
		if existing, found, lookupErr := r.Get(ctx, operation.OperationID); lookupErr == nil && found {
			return existing, false, nil
		}
		if existing, found, lookupErr := r.GetActiveByAccount(ctx, operation.AccountKey); lookupErr == nil && found && existing.OperationID != operation.OperationID {
			return existing, false, ErrAccountBusy
		}
		return model.CodexQuotaOperation{}, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return model.CodexQuotaOperation{}, false, err
	}
	stored, found, err := r.Get(ctx, operation.OperationID)
	return stored, rows > 0 && found, err
}

func (r *repository) GetActiveByAccount(ctx context.Context, accountKey string) (model.CodexQuotaOperation, bool, error) {
	row := r.db.QueryRowContext(ctx, selectOperation+` where account_key = ? and state in (?, ?, ?, ?, ?, ?, ?)
		order by created_at_ms asc limit 1`,
		strings.TrimSpace(accountKey),
		model.CodexQuotaOperationStateCreated,
		model.CodexQuotaOperationStateConsuming,
		model.CodexQuotaOperationStateUpstreamAccepted,
		model.CodexQuotaOperationStateVerifying,
		model.CodexQuotaOperationStateLocallyRecovered,
		model.CodexQuotaOperationStateConsumeStatusUnknown,
		model.CodexQuotaOperationStatePartialSuccess,
	)
	operation, err := scanOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.CodexQuotaOperation{}, false, nil
	}
	return operation, err == nil, err
}

func (r *repository) CountCompletedByAccount(ctx context.Context, accountKey string) (int64, error) {
	var count int64
	err := r.db.QueryRowContext(ctx, `select count(*) from codex_quota_operations
		where account_key = ? and state = ? and consumed = true`,
		strings.TrimSpace(accountKey), model.CodexQuotaOperationStateCompleted).Scan(&count)
	return count, err
}

// CountCompletedByCredential keeps reset history visible when a credential is
// deleted and imported again. The account key may change with a fresh import,
// while the CPA auth index remains the stable credential identifier.
func (r *repository) CountCompletedByCredential(ctx context.Context, accountKey string, authIndex string) (int64, error) {
	var count int64
	err := r.db.QueryRowContext(ctx, `select count(*) from codex_quota_operations
		where state = ? and consumed = true and (
			account_key = ? or auth_index = ?
		)`,
		model.CodexQuotaOperationStateCompleted,
		strings.TrimSpace(accountKey),
		strings.TrimSpace(authIndex),
	).Scan(&count)
	return count, err
}

func (r *repository) Get(ctx context.Context, operationID string) (model.CodexQuotaOperation, bool, error) {
	row := r.db.QueryRowContext(ctx, selectOperation+` where operation_id = ?`, strings.TrimSpace(operationID))
	operation, err := scanOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.CodexQuotaOperation{}, false, nil
	}
	return operation, err == nil, err
}

func (r *repository) Update(ctx context.Context, operation model.CodexQuotaOperation) (model.CodexQuotaOperation, error) {
	updated, _, err := r.update(ctx, operation, "")
	return updated, err
}

func (r *repository) UpdateIfState(ctx context.Context, operation model.CodexQuotaOperation, expectedState string) (model.CodexQuotaOperation, bool, error) {
	return r.update(ctx, operation, strings.TrimSpace(expectedState))
}

func (r *repository) update(ctx context.Context, operation model.CodexQuotaOperation, expectedState string) (model.CodexQuotaOperation, bool, error) {
	operation.OperationID = strings.TrimSpace(operation.OperationID)
	if operation.OperationID == "" {
		return model.CodexQuotaOperation{}, false, errors.New("codex quota operation id is required")
	}
	operation.UpdatedAtMS = time.Now().UnixMilli()
	query := `update codex_quota_operations set
		state = ?, consumed = ?, upstream_status = ?, warning_codes_json = ?,
		result_json = ?, last_error = ?, updated_at_ms = ?
	 where operation_id = ?`
	args := []any{
		operation.State,
		nullBool(operation.Consumed),
		nullInt(operation.UpstreamStatus),
		nullString(operation.WarningCodesJSON),
		nullString(operation.ResultJSON),
		nullString(operation.LastError),
		operation.UpdatedAtMS,
		operation.OperationID,
	}
	if expectedState != "" {
		query += ` and state = ?`
		args = append(args, expectedState)
	}
	result, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return model.CodexQuotaOperation{}, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return model.CodexQuotaOperation{}, false, err
	}
	if rows == 0 {
		stored, found, getErr := r.Get(ctx, operation.OperationID)
		if getErr != nil {
			return model.CodexQuotaOperation{}, false, getErr
		}
		if !found {
			return model.CodexQuotaOperation{}, false, ErrNotFound
		}
		return stored, false, nil
	}
	stored, found, err := r.Get(ctx, operation.OperationID)
	if err != nil {
		return model.CodexQuotaOperation{}, false, err
	}
	if !found {
		return model.CodexQuotaOperation{}, false, ErrNotFound
	}
	return stored, true, nil
}

const selectOperation = `select
	operation_id, account_key, auth_index, coalesce(auth_file_name, ''), state,
	consumed, upstream_status, coalesce(warning_codes_json, ''),
	coalesce(result_json, ''), coalesce(last_error, ''), created_at_ms, updated_at_ms
from codex_quota_operations`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanOperation(row rowScanner) (model.CodexQuotaOperation, error) {
	var operation model.CodexQuotaOperation
	var consumed sql.NullInt64
	var upstreamStatus sql.NullInt64
	err := row.Scan(
		&operation.OperationID,
		&operation.AccountKey,
		&operation.AuthIndex,
		&operation.AuthFileName,
		&operation.State,
		&consumed,
		&upstreamStatus,
		&operation.WarningCodesJSON,
		&operation.ResultJSON,
		&operation.LastError,
		&operation.CreatedAtMS,
		&operation.UpdatedAtMS,
	)
	if err != nil {
		return model.CodexQuotaOperation{}, err
	}
	if consumed.Valid {
		value := consumed.Int64 != 0
		operation.Consumed = &value
	}
	if upstreamStatus.Valid {
		value := int(upstreamStatus.Int64)
		operation.UpstreamStatus = &value
	}
	return operation, nil
}

func nullString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func nullBool(value *bool) any {
	if value == nil {
		return nil
	}
	if *value {
		return 1
	}
	return 0
}

func nullInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}
