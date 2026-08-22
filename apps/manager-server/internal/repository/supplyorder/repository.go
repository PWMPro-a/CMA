package supplyorder

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	sqliterepo "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/sqlite"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/security"
)

type Repository interface {
	Create(ctx context.Context, order model.SupplyOrder) (model.SupplyOrder, error)
	Get(ctx context.Context, orderID string) (model.SupplyOrder, bool, error)
	GetOpen(ctx context.Context) (model.SupplyOrder, bool, error)
	ListOpen(ctx context.Context, limit int) ([]model.SupplyOrder, error)
	GetLatestAutomatic(ctx context.Context) (model.SupplyOrder, bool, error)
	GetLatestCompletedAutomatic(ctx context.Context) (model.SupplyOrder, bool, error)
	ActivateNextLegacyRepair(ctx context.Context) (model.SupplyOrder, bool, error)
	ActivateNextUnsupportedRelease(ctx context.Context) (model.SupplyOrder, bool, error)
	PromoteCreateAttempt(ctx context.Context, localOrderID string, order model.SupplyOrder) error
	ClaimTaking(ctx context.Context, orderID string, nowMS int64, leaseUntilMS int64) (bool, error)
	Update(ctx context.Context, order model.SupplyOrder) error
	List(ctx context.Context, limit int) ([]model.SupplyOrder, error)
	ListByTaskID(ctx context.Context, taskID string) ([]model.SupplyOrder, error)
	ListByTaskIDs(ctx context.Context, taskIDs []string) ([]model.SupplyOrder, error)
	ListByOrderIDs(ctx context.Context, orderIDs []string) ([]model.SupplyOrder, error)
	ListBetween(ctx context.Context, fromMS int64, toMS int64, limit int) ([]model.SupplyOrder, error)
	ListMarketplaceSellerOrders(ctx context.Context, supplierID string, product string) ([]model.SupplyOrder, error)
	InsertItems(ctx context.Context, orderID string, items []model.SupplyImportItem) (int, error)
	ListItems(ctx context.Context, limit int, status string) ([]model.SupplyImportItem, error)
	ListItemsByOrderIDs(ctx context.Context, orderIDs []string) ([]model.SupplyImportItem, error)
	ListItemsBetween(ctx context.Context, fromMS int64, toMS int64, limit int) ([]model.SupplyImportItem, error)
	ListImportedItemsOverlapping(ctx context.Context, fromMS int64, toMS int64, limit int) ([]model.SupplyImportItem, error)
	ListPendingItems(ctx context.Context, orderID string, nowMS int64, limit int) ([]model.SupplyImportItem, error)
	ListActiveImportedItems(ctx context.Context, nowMS int64) ([]model.SupplyImportItem, error)
	ListCurrentImportedLeaseItems(ctx context.Context) ([]model.SupplyImportItem, error)
	ListCurrentImportedItems(ctx context.Context) ([]model.SupplyImportItem, error)
	MarkItemImported(ctx context.Context, id int64, importedAtMS int64) error
	MarkItemFailed(ctx context.Context, id int64, lastError string, nextRetryAtMS int64) error
	UpdateItemFileName(ctx context.Context, id int64, fileName string) error
	UpdateItemImportPlan(ctx context.Context, id int64, accountName string, nameKey string, fileName string, importAction string, replacedFileName string) error
	UpdateItemAccountMetadata(ctx context.Context, id int64, accountName string, nameKey string) error
	UpdateItemWarrantyMetadata(ctx context.Context, id int64, leaseExpiresAtMS int64, warrantyExpiresAtMS int64) error
	UpdateItemQuotaCapacity(ctx context.Context, id int64, capacityM float64, observedAtMS int64, complete bool) error
	ListItemsMissingAccountMetadata(ctx context.Context, limit int) ([]model.SupplyImportItem, error)
	ListCurrentItemsByItemKey(ctx context.Context, itemKey string) ([]model.SupplyImportItem, error)
	ListCurrentItemsByNameKey(ctx context.Context, nameKey string) ([]model.SupplyImportItem, error)
	Counts(ctx context.Context, orderID string) (total int, imported int, err error)
}

type repository struct {
	db        *sql.DB
	protector *security.Protector
}

const openOrderStatusClause = `'creating','create_uncertain','created','waiting_inventory','ready','taking','importing','partial'`

// supplyPurchaseOrderPredicate excludes synthetic recovery import rows that
// share the supply_orders table only to reuse the credential import pipeline.
// Those rows are not supplier purchases and must never block or pace buying.
const supplyPurchaseOrderPredicate = `lower(coalesce(strategy, '')) <> 'recovery'
	and lower(coalesce(remote_status, '')) <> 'recovery_claimed'
	and lower(order_id) not like 'recovery-%'`

// supplyHistoryOrderPredicate hides local idempotency attempts that ended in a
// definite, unpaid create failure. They remain attached to the durable task for
// auditing and retry accounting, but they are not supplier purchase orders and
// otherwise crowd paid deliveries out of the operator's bounded history page.
// Any fulfillment or payment evidence keeps the row visible for reconciliation.
const supplyHistoryOrderPredicate = `not (
	lower(coalesce(status, '')) = 'failed'
	and lower(order_id) like 'create-%'
	and charged_fen = 0
	and ready_quantity = 0
	and progress < 100
	and item_count = 0
)`

func New(db *sql.DB, protector ...*security.Protector) Repository {
	var p *security.Protector
	if len(protector) > 0 {
		p = protector[0]
	}
	return &repository{db: db, protector: p}
}

func (r *repository) Create(ctx context.Context, order model.SupplyOrder) (model.SupplyOrder, error) {
	order.OrderID = strings.TrimSpace(order.OrderID)
	if order.OrderID == "" {
		return model.SupplyOrder{}, errors.New("supply order id is required")
	}
	if order.Status == "" {
		order.Status = "created"
	}
	now := time.Now().UnixMilli()
	if order.CreatedAtMS <= 0 {
		order.CreatedAtMS = now
	}
	order.UpdatedAtMS = now
	statement := `insert into supply_orders (
		order_id, task_id, supplier_id, marketplace_seller_id, marketplace_seller_name, marketplace_channel_id, marketplace_selection_token,
		product, requested_quantity, automatic, strategy, trigger_reason, status, remote_status,
		ready_quantity, progress, status_url, take_url, charged_fen, released_fen,
		item_count, imported_count, last_error, next_poll_at_ms, supplier_retry_until_ms, completed_at_ms,
		created_at_ms, updated_at_ms
	) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if isOpenOrderStatus(order.Status) && isPurchaseOrder(order) && !isParallelPurchaseOrder(order) {
		statement = `insert into supply_orders (
			order_id, task_id, supplier_id, marketplace_seller_id, marketplace_seller_name, marketplace_channel_id, marketplace_selection_token,
			product, requested_quantity, automatic, strategy, trigger_reason, status, remote_status,
			ready_quantity, progress, status_url, take_url, charged_fen, released_fen,
			item_count, imported_count, last_error, next_poll_at_ms, supplier_retry_until_ms, completed_at_ms,
			created_at_ms, updated_at_ms
		) select ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		where not exists (select 1 from supply_orders where status in (` + openOrderStatusClause + `)
			and ` + supplyPurchaseOrderPredicate + `)`
	}
	var result sql.Result
	err := sqliterepo.WithBusyRetry(ctx, func() error {
		var execErr error
		result, execErr = r.db.ExecContext(ctx, statement,
			order.OrderID, nullString(order.TaskID), strings.TrimSpace(order.SupplierID), nullString(order.MarketplaceSellerID),
			nullString(order.MarketplaceSellerName), nullString(order.MarketplaceChannelID), nullString(order.MarketplaceSelectionToken),
			order.Product, order.RequestedQuantity, boolInt(order.Automatic),
			nullString(order.Strategy), nullString(order.TriggerReason), order.Status,
			nullString(order.RemoteStatus), order.ReadyQuantity, order.Progress, nullString(order.StatusURL),
			nullString(order.TakeURL), order.ChargedFen, order.ReleasedFen, order.ItemCount,
			order.ImportedCount, nullString(order.LastError), nullPositive(order.NextPollAtMS),
			nullPositive(order.SupplierRetryUntilMS), nullPositive(order.CompletedAtMS), order.CreatedAtMS, order.UpdatedAtMS,
		)
		return execErr
	})
	if err != nil {
		return model.SupplyOrder{}, err
	}
	if isOpenOrderStatus(order.Status) && isPurchaseOrder(order) && !isParallelPurchaseOrder(order) {
		affected, err := result.RowsAffected()
		if err != nil {
			return model.SupplyOrder{}, err
		}
		if affected == 0 {
			return model.SupplyOrder{}, errors.New("supply open order already exists")
		}
	}
	order.ID, err = result.LastInsertId()
	return order, err
}

func (r *repository) Get(ctx context.Context, orderID string) (model.SupplyOrder, bool, error) {
	row := r.db.QueryRowContext(ctx, orderSelect+` where order_id = ?`, strings.TrimSpace(orderID))
	order, err := scanOrder(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.SupplyOrder{}, false, nil
	}
	return order, err == nil, err
}

func (r *repository) GetOpen(ctx context.Context) (model.SupplyOrder, bool, error) {
	row := r.db.QueryRowContext(ctx, orderSelect+` where status in (`+openOrderStatusClause+`)
		and `+supplyPurchaseOrderPredicate+` order by created_at_ms asc limit 1`)
	order, err := scanOrder(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.SupplyOrder{}, false, nil
	}
	return order, err == nil, err
}

func (r *repository) ListOpen(ctx context.Context, limit int) ([]model.SupplyOrder, error) {
	if limit <= 0 || limit > 16 {
		limit = 4
	}
	rows, err := r.db.QueryContext(ctx, orderSelect+` where status in (`+openOrderStatusClause+`)
		and `+supplyPurchaseOrderPredicate+` order by created_at_ms asc, id asc limit ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := make([]model.SupplyOrder, 0, limit)
	for rows.Next() {
		order, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		orders = append(orders, order)
	}
	return orders, rows.Err()
}

func (r *repository) GetLatestAutomatic(ctx context.Context) (model.SupplyOrder, bool, error) {
	row := r.db.QueryRowContext(ctx, orderSelect+` where automatic = 1 and `+supplyPurchaseOrderPredicate+`
		order by created_at_ms desc, id desc limit 1`)
	order, err := scanOrder(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.SupplyOrder{}, false, nil
	}
	return order, err == nil, err
}

func (r *repository) GetLatestCompletedAutomatic(ctx context.Context) (model.SupplyOrder, bool, error) {
	row := r.db.QueryRowContext(ctx, orderSelect+` where automatic = 1 and status = 'completed'
		and `+supplyPurchaseOrderPredicate+`
		order by completed_at_ms desc, id desc limit 1`)
	order, err := scanOrder(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.SupplyOrder{}, false, nil
	}
	return order, err == nil, err
}

func (r *repository) ActivateNextLegacyRepair(ctx context.Context) (model.SupplyOrder, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return model.SupplyOrder{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var orderID string
	err = tx.QueryRowContext(ctx, `select o.order_id from supply_orders o
		where o.status = 'completed'
			and lower(coalesce(o.strategy, '')) <> 'recovery'
			and lower(coalesce(o.remote_status, '')) <> 'recovery_claimed'
			and lower(o.order_id) not like 'recovery-%'
			and exists (
			select 1 from supply_import_items i
			where i.order_id = o.order_id and i.status = 'imported' and i.file_name like 'supply-%'
		)
		order by o.created_at_ms asc, o.id asc limit 1`).Scan(&orderID)
	if errors.Is(err, sql.ErrNoRows) {
		return model.SupplyOrder{}, false, nil
	}
	if err != nil {
		return model.SupplyOrder{}, false, err
	}
	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, `update supply_orders set status = 'importing', imported_count = 0,
		completed_at_ms = null, next_poll_at_ms = null, last_error = null, updated_at_ms = ?
		where order_id = ? and status = 'completed'`, now, orderID)
	if err != nil {
		return model.SupplyOrder{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return model.SupplyOrder{}, false, err
	}
	if affected != 1 {
		return model.SupplyOrder{}, false, nil
	}
	if _, err := tx.ExecContext(ctx, `update supply_import_items set status = 'pending', last_error = null,
		next_retry_at_ms = null, imported_at_ms = null, updated_at_ms = ?
		where order_id = ? and status = 'imported' and file_name like 'supply-%'`, now, orderID); err != nil {
		return model.SupplyOrder{}, false, err
	}
	order, err := scanOrder(tx.QueryRowContext(ctx, orderSelect+` where order_id = ?`, orderID))
	if err != nil {
		return model.SupplyOrder{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return model.SupplyOrder{}, false, err
	}
	return order, true, nil
}

// ActivateNextUnsupportedRelease restores orders that were only released in the
// local database even though supplier inventory may still be reserved or paid.
// It also repairs the historical NV failure mode where a paid ledger entry was
// marked cancelled because the unsupported /extractions/{id} route returned
// 404. Those orders must be reconciled with the supplier and picked up when
// ready.
func (r *repository) ActivateNextUnsupportedRelease(ctx context.Context) (model.SupplyOrder, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return model.SupplyOrder{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	var orderID string
	err = tx.QueryRowContext(ctx, `select o.order_id from supply_orders o
		where o.automatic = 1
			and ((o.status = 'released'
				and (o.remote_status = 'release_unsupported'
					or (o.remote_status = 'auto_release_pending' and o.charged_fen > o.released_fen)))
				or (o.status = 'cancelled'
					and o.remote_status = 'cancelled'
					and o.charged_fen > o.released_fen
					and o.ready_quantity > 0
					and o.last_error like '%/api/workspace/extractions/%'))
			and o.released_fen = 0
			and o.item_count = 0
			and o.imported_count = 0
		order by o.created_at_ms asc, o.id asc limit 1`).Scan(&orderID)
	if errors.Is(err, sql.ErrNoRows) {
		return model.SupplyOrder{}, false, nil
	}
	if err != nil {
		return model.SupplyOrder{}, false, err
	}

	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, `update supply_orders set
		status = case when ready_quantity > 0 or progress >= 100 then 'ready' else 'waiting_inventory' end,
		completed_at_ms = null,
		next_poll_at_ms = null,
		last_error = 'restored: locally released supplier order still requires reconciliation and pickup',
		updated_at_ms = ?
		where order_id = ?
			and automatic = 1
			and ((status = 'released'
				and (remote_status = 'release_unsupported'
					or (remote_status = 'auto_release_pending' and charged_fen > released_fen)))
				or (status = 'cancelled'
					and remote_status = 'cancelled'
					and charged_fen > released_fen
					and ready_quantity > 0
					and last_error like '%/api/workspace/extractions/%'))
			and released_fen = 0
			and item_count = 0
			and imported_count = 0`, now, orderID)
	if err != nil {
		return model.SupplyOrder{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return model.SupplyOrder{}, false, err
	}
	if affected != 1 {
		return model.SupplyOrder{}, false, nil
	}
	order, err := scanOrder(tx.QueryRowContext(ctx, orderSelect+` where order_id = ?`, orderID))
	if err != nil {
		return model.SupplyOrder{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return model.SupplyOrder{}, false, err
	}
	return order, true, nil
}

func (r *repository) PromoteCreateAttempt(ctx context.Context, localOrderID string, order model.SupplyOrder) error {
	localOrderID = strings.TrimSpace(localOrderID)
	order.OrderID = strings.TrimSpace(order.OrderID)
	if localOrderID == "" || order.OrderID == "" {
		return errors.New("local and remote supply order ids are required")
	}
	order.UpdatedAtMS = time.Now().UnixMilli()
	result, err := r.db.ExecContext(ctx, `update supply_orders set
		order_id = ?, supplier_id = ?, marketplace_seller_id = ?, marketplace_seller_name = ?, marketplace_channel_id = ?, marketplace_selection_token = ?,
		strategy = ?, trigger_reason = ?, status = ?, remote_status = ?, ready_quantity = ?, progress = ?,
		status_url = ?, take_url = ?, charged_fen = ?, released_fen = ?, last_error = null,
		next_poll_at_ms = ?, supplier_retry_until_ms = ?, completed_at_ms = ?, updated_at_ms = ?
		where order_id = ? and status in ('creating','create_uncertain')`,
		order.OrderID, strings.TrimSpace(order.SupplierID), nullString(order.MarketplaceSellerID), nullString(order.MarketplaceSellerName),
		nullString(order.MarketplaceChannelID), nullString(order.MarketplaceSelectionToken), nullString(order.Strategy), nullString(order.TriggerReason),
		order.Status, nullString(order.RemoteStatus), order.ReadyQuantity, order.Progress,
		nullString(order.StatusURL), nullString(order.TakeURL), order.ChargedFen, order.ReleasedFen,
		nullPositive(order.NextPollAtMS), nullPositive(order.SupplierRetryUntilMS), nullPositive(order.CompletedAtMS), order.UpdatedAtMS, localOrderID,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errors.New("supply create attempt is no longer pending")
	}
	return nil
}

func (r *repository) ClaimTaking(ctx context.Context, orderID string, nowMS int64, leaseUntilMS int64) (bool, error) {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return false, errors.New("supply order id is required")
	}
	if nowMS <= 0 {
		nowMS = time.Now().UnixMilli()
	}
	if leaseUntilMS <= nowMS {
		leaseUntilMS = nowMS + int64(45*time.Second/time.Millisecond)
	}
	result, err := r.db.ExecContext(ctx, `update supply_orders set status = 'taking', last_error = null, next_poll_at_ms = ?, updated_at_ms = ?
		where order_id = ? and (status in ('ready','waiting_inventory') or (status = 'taking' and coalesce(next_poll_at_ms, 0) <= ?))`,
		leaseUntilMS, nowMS, orderID, nowMS,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

func (r *repository) Update(ctx context.Context, order model.SupplyOrder) error {
	if strings.TrimSpace(order.OrderID) == "" {
		return errors.New("supply order id is required")
	}
	order.UpdatedAtMS = time.Now().UnixMilli()
	return sqliterepo.WithBusyRetry(ctx, func() error {
		_, err := r.db.ExecContext(ctx, `update supply_orders set
			task_id = ?, supplier_id = ?, marketplace_seller_id = ?, marketplace_seller_name = ?, marketplace_channel_id = ?, marketplace_selection_token = ?,
			strategy = ?, trigger_reason = ?, status = ?, remote_status = ?, ready_quantity = ?, progress = ?, status_url = ?,
			take_url = ?, charged_fen = ?, released_fen = ?, item_count = ?, imported_count = ?,
			last_error = ?, next_poll_at_ms = ?, supplier_retry_until_ms = ?, completed_at_ms = ?, updated_at_ms = ? where order_id = ?`,
			nullString(order.TaskID), strings.TrimSpace(order.SupplierID), nullString(order.MarketplaceSellerID), nullString(order.MarketplaceSellerName),
			nullString(order.MarketplaceChannelID), nullString(order.MarketplaceSelectionToken), nullString(order.Strategy), nullString(order.TriggerReason),
			order.Status, nullString(order.RemoteStatus), order.ReadyQuantity, order.Progress,
			nullString(order.StatusURL), nullString(order.TakeURL), order.ChargedFen, order.ReleasedFen,
			order.ItemCount, order.ImportedCount, nullString(order.LastError), nullPositive(order.NextPollAtMS),
			nullPositive(order.SupplierRetryUntilMS), nullPositive(order.CompletedAtMS), order.UpdatedAtMS, order.OrderID,
		)
		return err
	})
}

func (r *repository) ListByTaskID(ctx context.Context, taskID string) ([]model.SupplyOrder, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return []model.SupplyOrder{}, nil
	}
	rows, err := r.db.QueryContext(ctx, orderSelect+` where task_id = ? order by created_at_ms asc, id asc`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := make([]model.SupplyOrder, 0)
	for rows.Next() {
		order, scanErr := scanOrder(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		orders = append(orders, order)
	}
	return orders, rows.Err()
}

func (r *repository) ListByTaskIDs(ctx context.Context, taskIDs []string) ([]model.SupplyOrder, error) {
	unique := make([]string, 0, len(taskIDs))
	seen := make(map[string]struct{}, len(taskIDs))
	for _, taskID := range taskIDs {
		taskID = strings.TrimSpace(taskID)
		if taskID == "" {
			continue
		}
		if _, ok := seen[taskID]; ok {
			continue
		}
		seen[taskID] = struct{}{}
		unique = append(unique, taskID)
	}
	if len(unique) == 0 {
		return []model.SupplyOrder{}, nil
	}
	placeholders := make([]string, len(unique))
	args := make([]any, len(unique))
	for index, taskID := range unique {
		placeholders[index], args[index] = "?", taskID
	}
	rows, err := r.db.QueryContext(ctx, orderSelect+` where task_id in (`+strings.Join(placeholders, ",")+
		`) order by created_at_ms asc, id asc`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := make([]model.SupplyOrder, 0)
	for rows.Next() {
		order, scanErr := scanOrder(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		orders = append(orders, order)
	}
	return orders, rows.Err()
}

func (r *repository) List(ctx context.Context, limit int) ([]model.SupplyOrder, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, orderSelect+` where `+supplyPurchaseOrderPredicate+`
		and `+supplyHistoryOrderPredicate+`
		order by created_at_ms desc, id desc limit ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := make([]model.SupplyOrder, 0)
	for rows.Next() {
		order, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		orders = append(orders, order)
	}
	return orders, rows.Err()
}

func (r *repository) ListByOrderIDs(ctx context.Context, orderIDs []string) ([]model.SupplyOrder, error) {
	unique := make([]string, 0, len(orderIDs))
	seen := make(map[string]struct{}, len(orderIDs))
	for _, orderID := range orderIDs {
		orderID = strings.TrimSpace(orderID)
		if orderID == "" {
			continue
		}
		if _, ok := seen[orderID]; ok {
			continue
		}
		seen[orderID] = struct{}{}
		unique = append(unique, orderID)
	}
	if len(unique) == 0 {
		return []model.SupplyOrder{}, nil
	}
	placeholders := make([]string, len(unique))
	args := make([]any, len(unique))
	for i, orderID := range unique {
		placeholders[i], args[i] = "?", orderID
	}
	rows, err := r.db.QueryContext(ctx, orderSelect+` where order_id in (`+strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := make([]model.SupplyOrder, 0, len(unique))
	for rows.Next() {
		order, scanErr := scanOrder(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		orders = append(orders, order)
	}
	return orders, rows.Err()
}

func (r *repository) ListBetween(ctx context.Context, fromMS int64, toMS int64, limit int) ([]model.SupplyOrder, error) {
	if limit <= 0 || limit > 10000 {
		limit = 5000
	}
	rows, err := r.db.QueryContext(ctx, orderSelect+` where `+supplyPurchaseOrderPredicate+` and (
		(created_at_ms >= ? and created_at_ms < ?) or
		(updated_at_ms >= ? and updated_at_ms < ?) or
		(coalesce(completed_at_ms, 0) >= ? and coalesce(completed_at_ms, 0) < ?))
		order by created_at_ms desc, id desc limit ?`,
		fromMS, toMS, fromMS, toMS, fromMS, toMS, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := make([]model.SupplyOrder, 0)
	for rows.Next() {
		order, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		orders = append(orders, order)
	}
	return orders, rows.Err()
}

func (r *repository) ListMarketplaceSellerOrders(ctx context.Context, supplierID string, product string) ([]model.SupplyOrder, error) {
	query := orderSelect + ` where coalesce(marketplace_seller_id, '') <> ''`
	args := make([]any, 0, 2)
	if supplierID = strings.TrimSpace(supplierID); supplierID != "" {
		query += ` and lower(supplier_id) = lower(?)`
		args = append(args, supplierID)
	}
	if product = strings.TrimSpace(product); product != "" {
		query += ` and lower(product) = lower(?)`
		args = append(args, product)
	}
	query += ` order by created_at_ms desc, id desc`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := make([]model.SupplyOrder, 0)
	for rows.Next() {
		order, scanErr := scanOrder(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		orders = append(orders, order)
	}
	return orders, rows.Err()
}

func (r *repository) InsertItems(ctx context.Context, orderID string, items []model.SupplyImportItem) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	now := time.Now().UnixMilli()
	inserted := 0
	err := sqliterepo.WithTxBusyRetry(ctx, r.db, func(tx *sql.Tx) error {
		inserted = 0
		for _, item := range items {
			payload, err := r.protect(item.PayloadJSON)
			if err != nil {
				return err
			}
			result, err := tx.ExecContext(ctx, `insert or ignore into supply_import_items (
			order_id, item_key, account_name, name_key, file_name, import_action, replaced_file_name,
				status, payload_json, attempt_count, lease_expires_at_ms, warranty_expires_at_ms,
			marketplace_seller_id, marketplace_seller_name, marketplace_channel_id, marketplace_selection_token,
			base_price_fen, charged_fen, quota_capacity_m, quota_capacity_observed_at_ms, quota_capacity_complete, created_at_ms, updated_at_ms
			) values (?, ?, ?, ?, ?, ?, ?, 'pending', ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, orderID, item.ItemKey,
				nullString(item.AccountName), nullString(item.NameKey), item.FileName, nullString(item.ImportAction), nullString(item.ReplacedFileName), payload,
				nullPositive(item.LeaseExpiresAtMS), nullPositive(item.WarrantyExpiresAtMS), nullString(item.MarketplaceSellerID),
				nullString(item.MarketplaceSellerName), nullString(item.MarketplaceChannelID), nullString(item.MarketplaceSelectionToken),
				item.BasePriceFen, item.ChargedFen, item.QuotaCapacityM, nullPositive(item.QuotaCapacityObservedAtMS), boolInt(item.QuotaCapacityComplete), now, now)
			if err != nil {
				return err
			}
			if affected, _ := result.RowsAffected(); affected > 0 {
				inserted += int(affected)
			}
		}
		return nil
	})
	return inserted, err
}

func (r *repository) ListItems(ctx context.Context, limit int, status string) ([]model.SupplyImportItem, error) {
	if limit <= 0 {
		limit = 200
	} else if limit > 5000 {
		limit = 5000
	}
	status = strings.ToLower(strings.TrimSpace(status))
	query := importItemSelect + ` from supply_import_items`
	args := make([]any, 0, 2)
	if status != "" && status != "all" {
		query += ` where status = ?`
		args = append(args, status)
	}
	query += ` order by coalesce(imported_at_ms, updated_at_ms, created_at_ms) desc, id desc limit ?`
	args = append(args, limit)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.SupplyImportItem, 0)
	for rows.Next() {
		item, err := r.scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *repository) ListItemsByOrderIDs(ctx context.Context, orderIDs []string) ([]model.SupplyImportItem, error) {
	unique := make([]string, 0, len(orderIDs))
	seen := make(map[string]struct{}, len(orderIDs))
	for _, orderID := range orderIDs {
		orderID = strings.TrimSpace(orderID)
		if orderID == "" {
			continue
		}
		if _, exists := seen[orderID]; exists {
			continue
		}
		seen[orderID] = struct{}{}
		unique = append(unique, orderID)
	}
	if len(unique) == 0 {
		return []model.SupplyImportItem{}, nil
	}

	placeholders := make([]string, len(unique))
	args := make([]any, len(unique))
	for index, orderID := range unique {
		placeholders[index] = "?"
		args[index] = orderID
	}
	query := importItemSelect + ` from supply_import_items where order_id in (` + strings.Join(placeholders, ",") + `)
		order by order_id asc, id asc`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.SupplyImportItem, 0)
	for rows.Next() {
		item, err := r.scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *repository) ListItemsBetween(ctx context.Context, fromMS int64, toMS int64, limit int) ([]model.SupplyImportItem, error) {
	if limit <= 0 || limit > 10000 {
		limit = 5000
	}
	rows, err := r.db.QueryContext(ctx, importItemSelect+` from supply_import_items where
		(created_at_ms >= ? and created_at_ms < ?) or
		(updated_at_ms >= ? and updated_at_ms < ?) or
		(coalesce(imported_at_ms, 0) >= ? and coalesce(imported_at_ms, 0) < ?)
		order by created_at_ms desc, id desc limit ?`,
		fromMS, toMS, fromMS, toMS, fromMS, toMS, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.SupplyImportItem, 0)
	for rows.Next() {
		item, err := r.scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *repository) ListImportedItemsOverlapping(ctx context.Context, fromMS int64, toMS int64, limit int) ([]model.SupplyImportItem, error) {
	if limit <= 0 || limit > 20000 {
		limit = 10000
	}
	rows, err := r.db.QueryContext(ctx, importItemSelect+` from supply_import_items
		where status = 'imported'
			and coalesce(file_name, '') <> ''
			and coalesce(imported_at_ms, 0) < ?
			and (coalesce(lease_expires_at_ms, 0) = 0 or lease_expires_at_ms >= ?)
			and (coalesce(superseded_at_ms, 0) = 0 or superseded_at_ms > ?)
		order by imported_at_ms desc, id desc limit ?`, toMS, fromMS, fromMS, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.SupplyImportItem, 0)
	for rows.Next() {
		item, err := r.scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *repository) ListPendingItems(ctx context.Context, orderID string, nowMS int64, limit int) ([]model.SupplyImportItem, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, importItemSelect+` from supply_import_items
		where order_id = ? and status in ('pending','failed') and coalesce(next_retry_at_ms, 0) <= ?
		order by id asc limit ?`, orderID, nowMS, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.SupplyImportItem, 0)
	for rows.Next() {
		item, err := r.scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ListActiveImportedItems only reads the short supplier lease window. It is
// used by the cached smart-capacity snapshot and never scans historical orders.
func (r *repository) ListActiveImportedItems(ctx context.Context, nowMS int64) ([]model.SupplyImportItem, error) {
	if nowMS <= 0 {
		nowMS = time.Now().UnixMilli()
	}
	rows, err := r.db.QueryContext(ctx, `select file_name, imported_at_ms, lease_expires_at_ms
		from supply_import_items
		where status = 'imported' and lease_expires_at_ms > ? and coalesce(superseded_at_ms, 0) = 0
		order by lease_expires_at_ms asc`, nowMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.SupplyImportItem, 0)
	for rows.Next() {
		var item model.SupplyImportItem
		if err := rows.Scan(&item.FileName, &item.ImportedAtMS, &item.LeaseExpiresAtMS); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ListCurrentImportedLeaseItems returns the current supplier lease metadata
// attached to every imported file, including timestamps that have passed.
// The timestamp remains useful for provenance and routing priority, but is not
// itself a credential-validity boundary.
func (r *repository) ListCurrentImportedLeaseItems(ctx context.Context) ([]model.SupplyImportItem, error) {
	rows, err := r.db.QueryContext(ctx, `select id, file_name, imported_at_ms, effective_from_ms, lease_expires_at_ms
		from supply_import_items
		where status = 'imported' and lease_expires_at_ms > 0 and coalesce(superseded_at_ms, 0) = 0
		order by lease_expires_at_ms asc, id asc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.SupplyImportItem, 0)
	for rows.Next() {
		var item model.SupplyImportItem
		if err := rows.Scan(&item.ID, &item.FileName, &item.ImportedAtMS, &item.EffectiveFromMS, &item.LeaseExpiresAtMS); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ListCurrentImportedItems returns the current imported provenance rows even
// when a marketplace warranty is informational rather than a scheduler lease.
// This is the source-of-truth mapping from a CPA auth file to its marketplace
// seller for quota-quality scoring.
func (r *repository) ListCurrentImportedItems(ctx context.Context) ([]model.SupplyImportItem, error) {
	rows, err := r.db.QueryContext(ctx, importItemSelect+` from supply_import_items
		where status = 'imported' and coalesce(superseded_at_ms, 0) = 0
		order by coalesce(effective_from_ms, imported_at_ms, updated_at_ms) desc, id desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.SupplyImportItem, 0)
	for rows.Next() {
		item, scanErr := r.scanItem(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *repository) MarkItemImported(ctx context.Context, id int64, importedAtMS int64) error {
	if importedAtMS <= 0 {
		importedAtMS = time.Now().UnixMilli()
	}
	return sqliterepo.WithTxBusyRetry(ctx, r.db, func(tx *sql.Tx) error {
		var fileName string
		if err := tx.QueryRowContext(ctx, `select file_name from supply_import_items where id = ?`, id).Scan(&fileName); err != nil {
			return err
		}
		var supersedesItemID any
		var previousID int64
		err := tx.QueryRowContext(ctx, `select id from supply_import_items
			where id <> ? and file_name = ? and status = 'imported' and coalesce(superseded_at_ms, 0) = 0
			order by coalesce(effective_from_ms, imported_at_ms, updated_at_ms) desc, id desc limit 1`, id, fileName).Scan(&previousID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			supersedesItemID = previousID
			if _, err := tx.ExecContext(ctx, `update supply_import_items set superseded_at_ms = ?, updated_at_ms = ?
				where id <> ? and file_name = ? and status = 'imported' and coalesce(superseded_at_ms, 0) = 0`, importedAtMS, importedAtMS, id, fileName); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `update supply_import_items set status = 'imported', last_error = null,
		attempt_count = attempt_count + 1, next_retry_at_ms = null, imported_at_ms = ?, effective_from_ms = ?,
		supersedes_item_id = ?, updated_at_ms = ? where id = ?`,
			importedAtMS, importedAtMS, supersedesItemID, importedAtMS, id); err != nil {
			return err
		}
		return nil
	})
}

func (r *repository) MarkItemFailed(ctx context.Context, id int64, lastError string, nextRetryAtMS int64) error {
	return sqliterepo.WithBusyRetry(ctx, func() error {
		_, err := r.db.ExecContext(ctx, `update supply_import_items set status = 'failed', last_error = ?,
			attempt_count = attempt_count + 1, next_retry_at_ms = ?, updated_at_ms = ? where id = ?`,
			strings.TrimSpace(lastError), nextRetryAtMS, time.Now().UnixMilli(), id)
		return err
	})
}

func (r *repository) UpdateItemFileName(ctx context.Context, id int64, fileName string) error {
	fileName = strings.TrimSpace(fileName)
	if fileName == "" {
		return errors.New("supply import file name is required")
	}
	return sqliterepo.WithBusyRetry(ctx, func() error {
		_, err := r.db.ExecContext(ctx, `update supply_import_items set file_name = ?, updated_at_ms = ? where id = ?`,
			fileName, time.Now().UnixMilli(), id)
		return err
	})
}

func (r *repository) UpdateItemImportPlan(ctx context.Context, id int64, accountName string, nameKey string, fileName string, importAction string, replacedFileName string) error {
	fileName = strings.TrimSpace(fileName)
	if fileName == "" {
		return errors.New("supply import file name is required")
	}
	importAction = strings.ToLower(strings.TrimSpace(importAction))
	if importAction != "add" && importAction != "replace" {
		return errors.New("supply import action must be add or replace")
	}
	return sqliterepo.WithBusyRetry(ctx, func() error {
		_, err := r.db.ExecContext(ctx, `update supply_import_items set account_name = ?, name_key = ?, file_name = ?,
			import_action = ?, replaced_file_name = ?, updated_at_ms = ? where id = ?`,
			nullString(accountName), nullString(nameKey), fileName, importAction, nullString(replacedFileName), time.Now().UnixMilli(), id)
		return err
	})
}

func (r *repository) UpdateItemAccountMetadata(ctx context.Context, id int64, accountName string, nameKey string) error {
	return sqliterepo.WithBusyRetry(ctx, func() error {
		_, err := r.db.ExecContext(ctx, `update supply_import_items set account_name = ?, name_key = ?, updated_at_ms = ? where id = ?`,
			nullString(accountName), nullString(nameKey), time.Now().UnixMilli(), id)
		return err
	})
}

func (r *repository) UpdateItemWarrantyMetadata(ctx context.Context, id int64, leaseExpiresAtMS int64, warrantyExpiresAtMS int64) error {
	return sqliterepo.WithBusyRetry(ctx, func() error {
		_, err := r.db.ExecContext(ctx, `update supply_import_items
			set lease_expires_at_ms = ?, warranty_expires_at_ms = ?, updated_at_ms = ? where id = ?`,
			nullPositive(leaseExpiresAtMS), nullPositive(warrantyExpiresAtMS), time.Now().UnixMilli(), id)
		return err
	})
}

func (r *repository) UpdateItemQuotaCapacity(ctx context.Context, id int64, capacityM float64, observedAtMS int64, complete bool) error {
	if capacityM <= 0 || observedAtMS <= 0 {
		return nil
	}
	return sqliterepo.WithBusyRetry(ctx, func() error {
		// Keep provisional and exhausted updates as separate conditional writes.
		// MySQL evaluates multi-column assignments left-to-right, so a single
		// CASE-based UPDATE can observe the newly assigned capacity and skip the
		// timestamp update. These predicates are deterministic on all databases.
		if complete {
			_, err := r.db.ExecContext(ctx, `update supply_import_items set
				quota_capacity_m = ?, quota_capacity_observed_at_ms = ?, quota_capacity_complete = 1,
				updated_at_ms = ? where id = ? and quota_capacity_complete = 0`,
				capacityM, observedAtMS, time.Now().UnixMilli(), id)
			return err
		}
		_, err := r.db.ExecContext(ctx, `update supply_import_items set
			quota_capacity_m = ?, quota_capacity_observed_at_ms = ?, updated_at_ms = ?
			where id = ? and quota_capacity_m <= 0`,
			capacityM, observedAtMS, time.Now().UnixMilli(), id)
		return err
	})
}

func (r *repository) ListItemsMissingAccountMetadata(ctx context.Context, limit int) ([]model.SupplyImportItem, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := r.db.QueryContext(ctx, importItemSelect+` from supply_import_items
		where coalesce(name_key, '') = '' order by id asc limit ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.SupplyImportItem, 0)
	for rows.Next() {
		item, err := r.scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *repository) ListCurrentItemsByItemKey(ctx context.Context, itemKey string) ([]model.SupplyImportItem, error) {
	rows, err := r.db.QueryContext(ctx, importItemSelect+` from supply_import_items
		where item_key = ? and status = 'imported' and coalesce(superseded_at_ms, 0) = 0
		order by coalesce(effective_from_ms, imported_at_ms, updated_at_ms) desc, id desc`, strings.TrimSpace(itemKey))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.SupplyImportItem, 0)
	for rows.Next() {
		item, err := r.scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *repository) ListCurrentItemsByNameKey(ctx context.Context, nameKey string) ([]model.SupplyImportItem, error) {
	rows, err := r.db.QueryContext(ctx, importItemSelect+` from supply_import_items
		where name_key = ? and status = 'imported' and coalesce(superseded_at_ms, 0) = 0
		order by coalesce(effective_from_ms, imported_at_ms, updated_at_ms) desc, id desc`, strings.TrimSpace(nameKey))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.SupplyImportItem, 0)
	for rows.Next() {
		item, err := r.scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *repository) Counts(ctx context.Context, orderID string) (int, int, error) {
	var total, imported int
	err := r.db.QueryRowContext(ctx, `select count(*), coalesce(sum(case when status = 'imported' then 1 else 0 end), 0)
		from supply_import_items where order_id = ?`, orderID).Scan(&total, &imported)
	return total, imported, err
}

const orderSelect = `select id, order_id, task_id, supplier_id, marketplace_seller_id, marketplace_seller_name, marketplace_channel_id, marketplace_selection_token,
	product, requested_quantity, automatic, strategy, trigger_reason, status, remote_status,
	ready_quantity, progress, status_url, take_url, charged_fen, released_fen, item_count,
	imported_count, last_error, next_poll_at_ms, supplier_retry_until_ms, completed_at_ms, created_at_ms, updated_at_ms
	from supply_orders`

const importItemSelect = `select id, order_id, item_key, account_name, name_key, file_name,
	import_action, replaced_file_name, supersedes_item_id, status,
	payload_json, last_error, attempt_count, next_retry_at_ms, imported_at_ms, effective_from_ms, superseded_at_ms,
	lease_expires_at_ms, warranty_expires_at_ms, marketplace_seller_id, marketplace_seller_name, marketplace_channel_id, marketplace_selection_token,
	base_price_fen, charged_fen, quota_capacity_m, quota_capacity_observed_at_ms, quota_capacity_complete, created_at_ms, updated_at_ms`

type scanner interface{ Scan(...any) error }

func scanOrder(row scanner) (model.SupplyOrder, error) {
	var order model.SupplyOrder
	var automatic int
	var taskID, marketplaceSellerID, marketplaceSellerName, marketplaceChannelID, marketplaceSelectionToken sql.NullString
	var strategy, triggerReason, remoteStatus, statusURL, takeURL, lastError sql.NullString
	var nextPollAtMS, supplierRetryUntilMS, completedAtMS sql.NullInt64
	err := row.Scan(&order.ID, &order.OrderID, &taskID, &order.SupplierID, &marketplaceSellerID, &marketplaceSellerName,
		&marketplaceChannelID, &marketplaceSelectionToken, &order.Product, &order.RequestedQuantity, &automatic,
		&strategy, &triggerReason, &order.Status, &remoteStatus, &order.ReadyQuantity, &order.Progress, &statusURL, &takeURL,
		&order.ChargedFen, &order.ReleasedFen, &order.ItemCount,
		&order.ImportedCount, &lastError, &nextPollAtMS, &supplierRetryUntilMS, &completedAtMS, &order.CreatedAtMS, &order.UpdatedAtMS)
	if err != nil {
		return model.SupplyOrder{}, err
	}
	order.Automatic = automatic != 0
	order.TaskID = taskID.String
	order.MarketplaceSellerID = marketplaceSellerID.String
	order.MarketplaceSellerName = marketplaceSellerName.String
	order.MarketplaceChannelID = marketplaceChannelID.String
	order.MarketplaceSelectionToken = marketplaceSelectionToken.String
	order.Strategy = strategy.String
	order.TriggerReason = triggerReason.String
	order.RemoteStatus = remoteStatus.String
	order.StatusURL = statusURL.String
	order.TakeURL = takeURL.String
	order.LastError = lastError.String
	order.NextPollAtMS = nextPollAtMS.Int64
	order.SupplierRetryUntilMS = supplierRetryUntilMS.Int64
	order.CompletedAtMS = completedAtMS.Int64
	return order, nil
}

func isPurchaseOrder(order model.SupplyOrder) bool {
	return !strings.EqualFold(strings.TrimSpace(order.Strategy), "recovery") &&
		!strings.EqualFold(strings.TrimSpace(order.RemoteStatus), "recovery_claimed") &&
		!strings.HasPrefix(strings.ToLower(strings.TrimSpace(order.OrderID)), "recovery-")
}

// Parallel reservations are explicitly marked by the service. The service
// applies the configured concurrency limit before creating either automatic or
// manual competitors; this repository-level marker only permits that bounded
// create to coexist with the anchor order.
func isParallelPurchaseOrder(order model.SupplyOrder) bool {
	return strings.TrimSpace(order.TaskID) != "" ||
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(order.TriggerReason)), "parallel_")
}

func (r *repository) scanItem(row scanner) (model.SupplyImportItem, error) {
	var item model.SupplyImportItem
	var payload string
	var lastError sql.NullString
	var accountName, nameKey, importAction, replacedFileName sql.NullString
	var marketplaceSellerID, marketplaceSellerName, marketplaceChannelID, marketplaceSelectionToken sql.NullString
	var supersedesItemID, nextRetryAtMS, importedAtMS, effectiveFromMS, supersededAtMS, leaseExpiresAtMS, warrantyExpiresAtMS, quotaCapacityObservedAtMS sql.NullInt64
	var quotaCapacityM sql.NullFloat64
	var quotaCapacityComplete int
	if err := row.Scan(&item.ID, &item.OrderID, &item.ItemKey, &accountName, &nameKey, &item.FileName,
		&importAction, &replacedFileName, &supersedesItemID, &item.Status,
		&payload, &lastError, &item.AttemptCount, &nextRetryAtMS, &importedAtMS, &effectiveFromMS, &supersededAtMS, &leaseExpiresAtMS, &warrantyExpiresAtMS,
		&marketplaceSellerID, &marketplaceSellerName, &marketplaceChannelID, &marketplaceSelectionToken,
		&item.BasePriceFen, &item.ChargedFen, &quotaCapacityM, &quotaCapacityObservedAtMS, &quotaCapacityComplete, &item.CreatedAtMS, &item.UpdatedAtMS); err != nil {
		return model.SupplyImportItem{}, err
	}
	unprotected, err := r.unprotect(payload)
	if err != nil {
		return model.SupplyImportItem{}, err
	}
	item.PayloadJSON = unprotected
	item.AccountName = accountName.String
	item.NameKey = nameKey.String
	item.ImportAction = importAction.String
	item.ReplacedFileName = replacedFileName.String
	item.SupersedesItemID = supersedesItemID.Int64
	item.LastError = lastError.String
	item.NextRetryAtMS = nextRetryAtMS.Int64
	item.ImportedAtMS = importedAtMS.Int64
	item.EffectiveFromMS = effectiveFromMS.Int64
	item.SupersededAtMS = supersededAtMS.Int64
	item.LeaseExpiresAtMS = leaseExpiresAtMS.Int64
	item.WarrantyExpiresAtMS = warrantyExpiresAtMS.Int64
	item.QuotaCapacityM = quotaCapacityM.Float64
	item.QuotaCapacityObservedAtMS = quotaCapacityObservedAtMS.Int64
	item.QuotaCapacityComplete = quotaCapacityComplete != 0
	item.MarketplaceSellerID = marketplaceSellerID.String
	item.MarketplaceSellerName = marketplaceSellerName.String
	item.MarketplaceChannelID = marketplaceChannelID.String
	item.MarketplaceSelectionToken = marketplaceSelectionToken.String
	return item, nil
}

func (r *repository) protect(value string) (string, error) {
	if r.protector == nil {
		return value, nil
	}
	return r.protector.ProtectString(value)
}

func (r *repository) unprotect(value string) (string, error) {
	if r.protector == nil {
		return value, nil
	}
	return r.protector.UnprotectString(value)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
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

func isOpenOrderStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case "creating", "create_uncertain", "created", "waiting_inventory", "ready", "taking", "importing", "partial":
		return true
	default:
		return false
	}
}
