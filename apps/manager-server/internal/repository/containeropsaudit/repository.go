package containeropsaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

type Repository interface {
	Create(ctx context.Context, entry model.ContainerOpsAuditEntry) (model.ContainerOpsAuditEntry, error)
	Update(ctx context.Context, entry model.ContainerOpsAuditEntry) error
	List(ctx context.Context, limit int) ([]model.ContainerOpsAuditEntry, error)
}

type repository struct {
	db *sql.DB
}

func New(db *sql.DB) Repository {
	return &repository{db: db}
}

func (r *repository) Create(ctx context.Context, entry model.ContainerOpsAuditEntry) (model.ContainerOpsAuditEntry, error) {
	if entry.OperationID == "" {
		return model.ContainerOpsAuditEntry{}, errors.New("container ops operation id is required")
	}
	if entry.Operation == "" {
		return model.ContainerOpsAuditEntry{}, errors.New("container ops operation is required")
	}
	now := time.Now().UnixMilli()
	if entry.StartedAtMS <= 0 {
		entry.StartedAtMS = now
	}
	if entry.CreatedAtMS <= 0 {
		entry.CreatedAtMS = now
	}
	entry.UpdatedAtMS = now
	if entry.Status == "" {
		entry.Status = "in_progress"
	}
	if entry.RequestJSON == "" && entry.Request != nil {
		entry.RequestJSON = marshalJSON(entry.Request)
	}
	if entry.ResultJSON == "" && entry.Result != nil {
		entry.ResultJSON = marshalJSON(entry.Result)
	}
	if entry.FinishedAtMS > 0 && entry.DurationMS <= 0 {
		entry.DurationMS = entry.FinishedAtMS - entry.StartedAtMS
	}

	res, err := r.db.ExecContext(
		ctx,
		`insert into container_ops_audits (
			operation_id, operation, phase, status, backup_id, agent_base_url,
			message, error, request_json, result_json, started_at_ms, finished_at_ms,
			duration_ms, created_at_ms, updated_at_ms
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.OperationID,
		entry.Operation,
		nullString(entry.Phase),
		entry.Status,
		nullString(entry.BackupID),
		nullString(entry.AgentBaseURL),
		nullString(entry.Message),
		nullString(entry.Error),
		nullString(entry.RequestJSON),
		nullString(entry.ResultJSON),
		entry.StartedAtMS,
		nullPositiveInt64(entry.FinishedAtMS),
		nullPositiveInt64(entry.DurationMS),
		entry.CreatedAtMS,
		entry.UpdatedAtMS,
	)
	if err != nil {
		return model.ContainerOpsAuditEntry{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return model.ContainerOpsAuditEntry{}, err
	}
	entry.ID = id
	return entry, nil
}

func (r *repository) Update(ctx context.Context, entry model.ContainerOpsAuditEntry) error {
	if entry.OperationID == "" {
		return errors.New("container ops operation id is required")
	}
	entry.UpdatedAtMS = time.Now().UnixMilli()
	if entry.ResultJSON == "" && entry.Result != nil {
		entry.ResultJSON = marshalJSON(entry.Result)
	}
	if entry.RequestJSON == "" && entry.Request != nil {
		entry.RequestJSON = marshalJSON(entry.Request)
	}
	if entry.FinishedAtMS > 0 && entry.DurationMS <= 0 && entry.StartedAtMS > 0 {
		entry.DurationMS = entry.FinishedAtMS - entry.StartedAtMS
	}
	_, err := r.db.ExecContext(
		ctx,
		`update container_ops_audits set
			operation = ?,
			phase = ?,
			status = ?,
			backup_id = ?,
			agent_base_url = ?,
			message = ?,
			error = ?,
			request_json = coalesce(?, request_json),
			result_json = ?,
			started_at_ms = ?,
			finished_at_ms = ?,
			duration_ms = ?,
			updated_at_ms = ?
		where operation_id = ?`,
		entry.Operation,
		nullString(entry.Phase),
		entry.Status,
		nullString(entry.BackupID),
		nullString(entry.AgentBaseURL),
		nullString(entry.Message),
		nullString(entry.Error),
		nullString(entry.RequestJSON),
		nullString(entry.ResultJSON),
		entry.StartedAtMS,
		nullPositiveInt64(entry.FinishedAtMS),
		nullPositiveInt64(entry.DurationMS),
		entry.UpdatedAtMS,
		entry.OperationID,
	)
	return err
}

func (r *repository) List(ctx context.Context, limit int) ([]model.ContainerOpsAuditEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.QueryContext(
		ctx,
		`select
			id, operation_id, operation, phase, status, backup_id, agent_base_url,
			message, error, request_json, result_json, started_at_ms, finished_at_ms,
			duration_ms, created_at_ms, updated_at_ms
		from container_ops_audits
		order by started_at_ms desc, id desc
		limit ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]model.ContainerOpsAuditEntry, 0)
	for rows.Next() {
		item, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanEntry(row scanner) (model.ContainerOpsAuditEntry, error) {
	var entry model.ContainerOpsAuditEntry
	var phase, backupID, agentBaseURL, message, errorText, requestJSON, resultJSON sql.NullString
	var finishedAtMS, durationMS sql.NullInt64
	if err := row.Scan(
		&entry.ID,
		&entry.OperationID,
		&entry.Operation,
		&phase,
		&entry.Status,
		&backupID,
		&agentBaseURL,
		&message,
		&errorText,
		&requestJSON,
		&resultJSON,
		&entry.StartedAtMS,
		&finishedAtMS,
		&durationMS,
		&entry.CreatedAtMS,
		&entry.UpdatedAtMS,
	); err != nil {
		return model.ContainerOpsAuditEntry{}, err
	}
	entry.Phase = phase.String
	entry.BackupID = backupID.String
	entry.AgentBaseURL = agentBaseURL.String
	entry.Message = message.String
	entry.Error = errorText.String
	entry.RequestJSON = requestJSON.String
	entry.ResultJSON = resultJSON.String
	if finishedAtMS.Valid {
		entry.FinishedAtMS = finishedAtMS.Int64
	}
	if durationMS.Valid {
		entry.DurationMS = durationMS.Int64
	}
	entry.Request = unmarshalJSON(entry.RequestJSON)
	entry.Result = unmarshalJSON(entry.ResultJSON)
	return entry, nil
}

func marshalJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

func unmarshalJSON(raw string) any {
	if raw == "" {
		return nil
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil
	}
	return value
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullPositiveInt64(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}
