package supplytask

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	sqliterepo "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/sqlite"
)

type Repository interface {
	Create(ctx context.Context, task model.SupplyPurchaseTask) (model.SupplyPurchaseTask, error)
	Get(ctx context.Context, taskID string) (model.SupplyPurchaseTask, bool, error)
	GetActiveAutomatic(ctx context.Context) (model.SupplyPurchaseTask, bool, error)
	Update(ctx context.Context, task model.SupplyPurchaseTask) error
	Cancel(ctx context.Context, taskID string, nowMS int64) (model.SupplyPurchaseTask, bool, error)
	List(ctx context.Context, limit int) ([]model.SupplyPurchaseTask, error)
	ListActive(ctx context.Context, limit int) ([]model.SupplyPurchaseTask, error)
}

type repository struct {
	db *sql.DB
}

func New(db *sql.DB) Repository {
	return &repository{db: db}
}

func (r *repository) Create(ctx context.Context, task model.SupplyPurchaseTask) (model.SupplyPurchaseTask, error) {
	task.TaskID = strings.TrimSpace(task.TaskID)
	if task.TaskID == "" {
		return model.SupplyPurchaseTask{}, errors.New("supply purchase task id is required")
	}
	task.Source = strings.ToLower(strings.TrimSpace(task.Source))
	if task.Source != "manual" && task.Source != "automatic" {
		return model.SupplyPurchaseTask{}, errors.New("supply purchase task source must be manual or automatic")
	}
	if task.TargetQuantity <= 0 {
		return model.SupplyPurchaseTask{}, errors.New("supply purchase task target quantity must be positive")
	}
	if task.Status == "" {
		task.Status = "pending"
	}
	if task.MaxConcurrentOrders <= 0 {
		task.MaxConcurrentOrders = 1
	}
	now := time.Now().UnixMilli()
	if task.CreatedAtMS <= 0 {
		task.CreatedAtMS = now
	}
	task.UpdatedAtMS = now
	var result sql.Result
	err := sqliterepo.WithBusyRetry(ctx, func() error {
		var execErr error
		result, execErr = r.db.ExecContext(ctx, `insert into supply_purchase_tasks (
			task_id, source, supplier_id, product, target_quantity, fulfilled_quantity,
			status, strategy, trigger_reason, max_concurrent_orders, attempt_count,
			next_attempt_at_ms, last_error, cancelled_at_ms, completed_at_ms,
			created_at_ms, updated_at_ms
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			task.TaskID, task.Source, nullString(task.SupplierID), nullString(task.Product),
			task.TargetQuantity, task.FulfilledQuantity, task.Status, nullString(task.Strategy),
			nullString(task.TriggerReason), task.MaxConcurrentOrders, task.AttemptCount,
			nullPositive(task.NextAttemptAtMS), nullString(task.LastError),
			nullPositive(task.CancelledAtMS), nullPositive(task.CompletedAtMS),
			task.CreatedAtMS, task.UpdatedAtMS)
		return execErr
	})
	if err != nil {
		return model.SupplyPurchaseTask{}, err
	}
	task.ID, err = result.LastInsertId()
	return task, err
}

func (r *repository) Get(ctx context.Context, taskID string) (model.SupplyPurchaseTask, bool, error) {
	row := r.db.QueryRowContext(ctx, taskSelect+` where task_id = ?`, strings.TrimSpace(taskID))
	task, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.SupplyPurchaseTask{}, false, nil
	}
	return task, err == nil, err
}

func (r *repository) GetActiveAutomatic(ctx context.Context) (model.SupplyPurchaseTask, bool, error) {
	row := r.db.QueryRowContext(ctx, taskSelect+` where source = 'automatic' and status in ('pending','running')
		order by created_at_ms asc, id asc limit 1`)
	task, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.SupplyPurchaseTask{}, false, nil
	}
	return task, err == nil, err
}

func (r *repository) Update(ctx context.Context, task model.SupplyPurchaseTask) error {
	task.TaskID = strings.TrimSpace(task.TaskID)
	if task.TaskID == "" {
		return errors.New("supply purchase task id is required")
	}
	if task.MaxConcurrentOrders <= 0 {
		task.MaxConcurrentOrders = 1
	}
	task.UpdatedAtMS = time.Now().UnixMilli()
	return sqliterepo.WithBusyRetry(ctx, func() error {
		_, err := r.db.ExecContext(ctx, `update supply_purchase_tasks set
			supplier_id = ?, product = ?, target_quantity = ?, fulfilled_quantity = ?,
			status = ?, strategy = ?, trigger_reason = ?, max_concurrent_orders = ?,
			attempt_count = ?, next_attempt_at_ms = ?, last_error = ?, cancelled_at_ms = ?,
			completed_at_ms = ?, updated_at_ms = ? where task_id = ?`,
			nullString(task.SupplierID), nullString(task.Product), task.TargetQuantity,
			task.FulfilledQuantity, task.Status, nullString(task.Strategy),
			nullString(task.TriggerReason), task.MaxConcurrentOrders, task.AttemptCount,
			nullPositive(task.NextAttemptAtMS), nullString(task.LastError),
			nullPositive(task.CancelledAtMS), nullPositive(task.CompletedAtMS),
			task.UpdatedAtMS, task.TaskID)
		return err
	})
}

func (r *repository) Cancel(ctx context.Context, taskID string, nowMS int64) (model.SupplyPurchaseTask, bool, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return model.SupplyPurchaseTask{}, false, errors.New("supply purchase task id is required")
	}
	if nowMS <= 0 {
		nowMS = time.Now().UnixMilli()
	}
	var affected int64
	err := sqliterepo.WithBusyRetry(ctx, func() error {
		result, err := r.db.ExecContext(ctx, `update supply_purchase_tasks set status = 'cancelled',
			cancelled_at_ms = ?, next_attempt_at_ms = null, updated_at_ms = ?
			where task_id = ? and status in ('pending','running')`, nowMS, nowMS, taskID)
		if err != nil {
			return err
		}
		affected, err = result.RowsAffected()
		return err
	})
	if err != nil {
		return model.SupplyPurchaseTask{}, false, err
	}
	task, found, err := r.Get(ctx, taskID)
	if err != nil || !found {
		return model.SupplyPurchaseTask{}, false, err
	}
	return task, affected > 0, nil
}

func (r *repository) List(ctx context.Context, limit int) ([]model.SupplyPurchaseTask, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return r.list(ctx, taskSelect+` order by created_at_ms desc, id desc limit ?`, limit)
}

func (r *repository) ListActive(ctx context.Context, limit int) ([]model.SupplyPurchaseTask, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	return r.list(ctx, taskSelect+` where status in ('pending','running')
		order by coalesce(next_attempt_at_ms, 0) asc, created_at_ms asc, id asc limit ?`, limit)
}

func (r *repository) list(ctx context.Context, query string, limit int) ([]model.SupplyPurchaseTask, error) {
	rows, err := r.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := make([]model.SupplyPurchaseTask, 0, limit)
	for rows.Next() {
		task, scanErr := scanTask(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

const taskSelect = `select id, task_id, source, supplier_id, product, target_quantity,
	fulfilled_quantity, status, strategy, trigger_reason, max_concurrent_orders,
	attempt_count, next_attempt_at_ms, last_error, cancelled_at_ms, completed_at_ms,
	created_at_ms, updated_at_ms from supply_purchase_tasks`

type scanner interface{ Scan(...any) error }

func scanTask(row scanner) (model.SupplyPurchaseTask, error) {
	var task model.SupplyPurchaseTask
	var supplierID, product, strategy, triggerReason, lastError sql.NullString
	var nextAttemptAtMS, cancelledAtMS, completedAtMS sql.NullInt64
	err := row.Scan(&task.ID, &task.TaskID, &task.Source, &supplierID, &product,
		&task.TargetQuantity, &task.FulfilledQuantity, &task.Status, &strategy,
		&triggerReason, &task.MaxConcurrentOrders, &task.AttemptCount,
		&nextAttemptAtMS, &lastError, &cancelledAtMS, &completedAtMS,
		&task.CreatedAtMS, &task.UpdatedAtMS)
	if err != nil {
		return model.SupplyPurchaseTask{}, err
	}
	task.SupplierID = supplierID.String
	task.Product = product.String
	task.Strategy = strategy.String
	task.TriggerReason = triggerReason.String
	task.NextAttemptAtMS = nextAttemptAtMS.Int64
	task.LastError = lastError.String
	task.CancelledAtMS = cancelledAtMS.Int64
	task.CompletedAtMS = completedAtMS.Int64
	return task, nil
}

func nullString(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
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
