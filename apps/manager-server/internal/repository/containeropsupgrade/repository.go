package containeropsupgrade

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

type Repository interface {
	Create(ctx context.Context, task model.ContainerOpsUpgradeTask) (model.ContainerOpsUpgradeTask, error)
	Get(ctx context.Context, taskID string) (model.ContainerOpsUpgradeTask, bool, error)
	Update(ctx context.Context, task model.ContainerOpsUpgradeTask) error
	List(ctx context.Context, limit int) ([]model.ContainerOpsUpgradeTask, error)
}

type repository struct {
	db *sql.DB
}

func New(db *sql.DB) Repository {
	return &repository{db: db}
}

func (r *repository) Create(ctx context.Context, task model.ContainerOpsUpgradeTask) (model.ContainerOpsUpgradeTask, error) {
	if task.TaskID == "" {
		return model.ContainerOpsUpgradeTask{}, errors.New("container ops upgrade task id is required")
	}
	now := time.Now().UnixMilli()
	if task.Status == "" {
		task.Status = "preparing"
	}
	if task.Phase == "" {
		task.Phase = "prepare"
	}
	if task.StartedAtMS <= 0 {
		task.StartedAtMS = now
	}
	if task.CreatedAtMS <= 0 {
		task.CreatedAtMS = now
	}
	task.UpdatedAtMS = now
	if task.Request != nil {
		task.RequestJSON = marshalJSON(task.Request)
	}
	if task.Result != nil {
		task.ResultJSON = marshalJSON(task.Result)
	}

	res, err := r.db.ExecContext(
		ctx,
		`insert into container_ops_upgrade_tasks (
			task_id, operation_id, status, phase, cpa_image, cpamp_image,
			rollback_backup_id, agent_base_url, message, error, next_action,
			request_json, result_json, started_at_ms, finished_at_ms,
			created_at_ms, updated_at_ms
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task.TaskID,
		nullString(task.OperationID),
		task.Status,
		nullString(task.Phase),
		nullString(task.CPAImage),
		nullString(task.CPAMPImage),
		nullString(task.RollbackBackupID),
		nullString(task.AgentBaseURL),
		nullString(task.Message),
		nullString(task.Error),
		nullString(task.NextAction),
		nullString(task.RequestJSON),
		nullString(task.ResultJSON),
		task.StartedAtMS,
		nullPositiveInt64(task.FinishedAtMS),
		task.CreatedAtMS,
		task.UpdatedAtMS,
	)
	if err != nil {
		return model.ContainerOpsUpgradeTask{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return model.ContainerOpsUpgradeTask{}, err
	}
	task.ID = id
	return task, nil
}

func (r *repository) Get(ctx context.Context, taskID string) (model.ContainerOpsUpgradeTask, bool, error) {
	if taskID == "" {
		return model.ContainerOpsUpgradeTask{}, false, errors.New("container ops upgrade task id is required")
	}
	row := r.db.QueryRowContext(
		ctx,
		`select
			id, task_id, operation_id, status, phase, cpa_image, cpamp_image,
			rollback_backup_id, agent_base_url, message, error, next_action,
			request_json, result_json, started_at_ms, finished_at_ms,
			created_at_ms, updated_at_ms
		from container_ops_upgrade_tasks
		where task_id = ?`,
		taskID,
	)
	task, err := scanTask(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.ContainerOpsUpgradeTask{}, false, nil
		}
		return model.ContainerOpsUpgradeTask{}, false, err
	}
	return task, true, nil
}

func (r *repository) Update(ctx context.Context, task model.ContainerOpsUpgradeTask) error {
	if task.TaskID == "" {
		return errors.New("container ops upgrade task id is required")
	}
	task.UpdatedAtMS = time.Now().UnixMilli()
	if task.Request != nil {
		task.RequestJSON = marshalJSON(task.Request)
	}
	if task.Result != nil {
		task.ResultJSON = marshalJSON(task.Result)
	}
	_, err := r.db.ExecContext(
		ctx,
		`update container_ops_upgrade_tasks set
			operation_id = ?,
			status = ?,
			phase = ?,
			cpa_image = ?,
			cpamp_image = ?,
			rollback_backup_id = ?,
			agent_base_url = ?,
			message = ?,
			error = ?,
			next_action = ?,
			request_json = coalesce(?, request_json),
			result_json = ?,
			started_at_ms = ?,
			finished_at_ms = ?,
			updated_at_ms = ?
		where task_id = ?`,
		nullString(task.OperationID),
		task.Status,
		nullString(task.Phase),
		nullString(task.CPAImage),
		nullString(task.CPAMPImage),
		nullString(task.RollbackBackupID),
		nullString(task.AgentBaseURL),
		nullString(task.Message),
		nullString(task.Error),
		nullString(task.NextAction),
		nullString(task.RequestJSON),
		nullString(task.ResultJSON),
		task.StartedAtMS,
		nullPositiveInt64(task.FinishedAtMS),
		task.UpdatedAtMS,
		task.TaskID,
	)
	return err
}

func (r *repository) List(ctx context.Context, limit int) ([]model.ContainerOpsUpgradeTask, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.QueryContext(
		ctx,
		`select
			id, task_id, operation_id, status, phase, cpa_image, cpamp_image,
			rollback_backup_id, agent_base_url, message, error, next_action,
			request_json, result_json, started_at_ms, finished_at_ms,
			created_at_ms, updated_at_ms
		from container_ops_upgrade_tasks
		order by created_at_ms desc, id desc
		limit ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tasks := make([]model.ContainerOpsUpgradeTask, 0)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanTask(row scanner) (model.ContainerOpsUpgradeTask, error) {
	var task model.ContainerOpsUpgradeTask
	var operationID, phase, cpaImage, cpampImage, rollbackBackupID sql.NullString
	var agentBaseURL, message, errorText, nextAction, requestJSON, resultJSON sql.NullString
	var finishedAtMS sql.NullInt64
	if err := row.Scan(
		&task.ID,
		&task.TaskID,
		&operationID,
		&task.Status,
		&phase,
		&cpaImage,
		&cpampImage,
		&rollbackBackupID,
		&agentBaseURL,
		&message,
		&errorText,
		&nextAction,
		&requestJSON,
		&resultJSON,
		&task.StartedAtMS,
		&finishedAtMS,
		&task.CreatedAtMS,
		&task.UpdatedAtMS,
	); err != nil {
		return model.ContainerOpsUpgradeTask{}, err
	}
	task.OperationID = operationID.String
	task.Phase = phase.String
	task.CPAImage = cpaImage.String
	task.CPAMPImage = cpampImage.String
	task.RollbackBackupID = rollbackBackupID.String
	task.AgentBaseURL = agentBaseURL.String
	task.Message = message.String
	task.Error = errorText.String
	task.NextAction = nextAction.String
	task.RequestJSON = requestJSON.String
	task.ResultJSON = resultJSON.String
	if finishedAtMS.Valid {
		task.FinishedAtMS = finishedAtMS.Int64
	}
	task.Request = unmarshalJSON(task.RequestJSON)
	task.Result = unmarshalJSON(task.ResultJSON)
	return task, nil
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
