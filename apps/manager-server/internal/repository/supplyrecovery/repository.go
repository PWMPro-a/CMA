package supplyrecovery

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

type Summary struct {
	Total     int `json:"total"`
	External  int `json:"external"`
	Claimable int `json:"claimable"`
	Importing int `json:"importing"`
	Imported  int `json:"imported"`
	Refunded  int `json:"refunded"`
	Failed    int `json:"failed"`
}

type Repository interface {
	UpsertMany(ctx context.Context, recoveries []model.SupplyRecovery) (int, error)
	Get(ctx context.Context, recoveryID string) (model.SupplyRecovery, bool, error)
	GetByClaimOrder(ctx context.Context, claimOrderID string) (model.SupplyRecovery, bool, error)
	List(ctx context.Context, limit int, status string) ([]model.SupplyRecovery, error)
	ListBetween(ctx context.Context, fromMS int64, toMS int64, limit int) ([]model.SupplyRecovery, error)
	ListClaimable(ctx context.Context, limit int) ([]model.SupplyRecovery, error)
	ListImportPending(ctx context.Context, limit int) ([]model.SupplyRecovery, error)
	ClaimForProcessing(ctx context.Context, recoveryID string, nowMS int64) (model.SupplyRecovery, bool, error)
	PersistClaim(ctx context.Context, recovery model.SupplyRecovery, order model.SupplyOrder, items []model.SupplyImportItem, claimedAtMS int64) error
	ResetImport(ctx context.Context, recoveryID string, nowMS int64) (bool, error)
	MarkClaimed(ctx context.Context, recoveryID string, claimOrderID string, itemCount int, claimedAtMS int64) error
	MarkImportProgress(ctx context.Context, recoveryID string, itemCount int, importedCount int, lastError string) error
	MarkImported(ctx context.Context, recoveryID string, importedCount int) error
	MarkRefunded(ctx context.Context, recoveryID string, refundedFen int64) error
	MarkFailed(ctx context.Context, recoveryID string, lastError string) error
	SetLastError(ctx context.Context, recoveryID string, lastError string) error
	Summary(ctx context.Context) (Summary, error)
}

type repository struct {
	db        *sql.DB
	protector *security.Protector
}

func New(db *sql.DB, protector ...*security.Protector) Repository {
	var p *security.Protector
	if len(protector) > 0 {
		p = protector[0]
	}
	return &repository{db: db, protector: p}
}

func (r *repository) UpsertMany(ctx context.Context, recoveries []model.SupplyRecovery) (int, error) {
	if len(recoveries) == 0 {
		return 0, nil
	}
	now := time.Now().UnixMilli()
	updated := 0
	err := sqliterepo.WithTxBusyRetry(ctx, r.db, func(tx *sql.Tx) error {
		updated = 0
		for _, recovery := range recoveries {
			recovery.RecoveryID = strings.TrimSpace(recovery.RecoveryID)
			if recovery.RecoveryID == "" {
				continue
			}
			if recovery.Status == "" {
				recovery.Status = statusFromDelivery(recovery.DeliveryStatus, recovery.ClaimURL, recovery.RefundedFen)
			}
			if recovery.LastSeenAtMS <= 0 {
				recovery.LastSeenAtMS = now
			}
			claimURL, err := r.protect(recovery.ClaimURL)
			if err != nil {
				return err
			}
			claimTicket, err := r.protect(recovery.ClaimTicket)
			if err != nil {
				return err
			}
			rawJSON, err := r.protect(recovery.RawJSON)
			if err != nil {
				return err
			}
			result, err := tx.ExecContext(ctx, `insert into supply_recoveries (
			recovery_id, supplier_id, product, delivery_status, status, credential_version, source_order_id, original_file_name, original_auth_index,
			original_email, claim_url, claim_ticket, claim_order_id, item_count, imported_count, refunded_fen,
			last_error, raw_json, last_seen_at_ms, claimed_at_ms, created_at_ms, updated_at_ms
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		on conflict(recovery_id) do update set
			supplier_id = coalesce(nullif(excluded.supplier_id, ''), supply_recoveries.supplier_id),
			product = excluded.product,
			delivery_status = excluded.delivery_status,
			credential_version = case
				when excluded.credential_version > supply_recoveries.credential_version then excluded.credential_version
				else supply_recoveries.credential_version
			end,
			status = case
				when excluded.status = 'refunded' then 'refunded'
				when supply_recoveries.status in ('imported','importing','partial','failed') then supply_recoveries.status
				when excluded.status = 'claimable' then 'claimable'
				else excluded.status
			end,
			source_order_id = coalesce(nullif(excluded.source_order_id, ''), supply_recoveries.source_order_id),
			original_file_name = coalesce(nullif(excluded.original_file_name, ''), supply_recoveries.original_file_name),
			original_auth_index = coalesce(nullif(excluded.original_auth_index, ''), supply_recoveries.original_auth_index),
			original_email = coalesce(nullif(excluded.original_email, ''), supply_recoveries.original_email),
			claim_url = coalesce(nullif(excluded.claim_url, ''), supply_recoveries.claim_url),
			claim_ticket = coalesce(nullif(excluded.claim_ticket, ''), supply_recoveries.claim_ticket),
			refunded_fen = case when excluded.refunded_fen > 0 then excluded.refunded_fen else supply_recoveries.refunded_fen end,
			last_error = excluded.last_error,
			raw_json = coalesce(nullif(excluded.raw_json, ''), supply_recoveries.raw_json),
			last_seen_at_ms = excluded.last_seen_at_ms,
			updated_at_ms = excluded.updated_at_ms`,
				recovery.RecoveryID, strings.TrimSpace(recovery.SupplierID), nullString(recovery.Product), strings.ToLower(strings.TrimSpace(recovery.DeliveryStatus)), recovery.Status,
				recovery.CredentialVersion, nullString(recovery.SourceOrderID),
				nullString(recovery.OriginalFileName), nullString(recovery.OriginalAuthIndex), nullString(recovery.OriginalEmail),
				nullString(claimURL), nullString(claimTicket), nullString(recovery.ClaimOrderID), recovery.ItemCount, recovery.ImportedCount,
				recovery.RefundedFen, nullString(recovery.LastError), nullString(rawJSON), recovery.LastSeenAtMS,
				nullPositive(recovery.ClaimedAtMS), now, now,
			)
			if err != nil {
				return err
			}
			if affected, _ := result.RowsAffected(); affected > 0 {
				updated += int(affected)
			}
		}
		return nil
	})
	return updated, err
}

func (r *repository) Get(ctx context.Context, recoveryID string) (model.SupplyRecovery, bool, error) {
	row := r.db.QueryRowContext(ctx, recoverySelect+` where recovery_id = ?`, strings.TrimSpace(recoveryID))
	recovery, err := r.scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.SupplyRecovery{}, false, nil
	}
	return recovery, err == nil, err
}

func (r *repository) GetByClaimOrder(ctx context.Context, claimOrderID string) (model.SupplyRecovery, bool, error) {
	row := r.db.QueryRowContext(ctx, recoverySelect+` where claim_order_id = ? order by updated_at_ms desc limit 1`, strings.TrimSpace(claimOrderID))
	recovery, err := r.scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.SupplyRecovery{}, false, nil
	}
	return recovery, err == nil, err
}

func (r *repository) List(ctx context.Context, limit int, status string) ([]model.SupplyRecovery, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	status = strings.ToLower(strings.TrimSpace(status))
	query := recoverySelect
	args := []any{}
	if status != "" && status != "all" {
		query += ` where status = ?`
		args = append(args, status)
	}
	query += ` order by updated_at_ms desc, id desc limit ?`
	args = append(args, limit)
	return r.list(ctx, query, args...)
}

func (r *repository) ListBetween(ctx context.Context, fromMS int64, toMS int64, limit int) ([]model.SupplyRecovery, error) {
	if limit <= 0 || limit > 10000 {
		limit = 5000
	}
	return r.list(ctx, recoverySelect+` where (`+localRecoveryOwnershipClause+`) and (
		(created_at_ms >= ? and created_at_ms < ?) or
		(updated_at_ms >= ? and updated_at_ms < ?) or
		(last_seen_at_ms >= ? and last_seen_at_ms < ?) or
		(coalesce(claimed_at_ms, 0) >= ? and coalesce(claimed_at_ms, 0) < ?))
		order by updated_at_ms desc, id desc limit ?`,
		fromMS, toMS, fromMS, toMS, fromMS, toMS, fromMS, toMS, limit)
}

func (r *repository) ListClaimable(ctx context.Context, limit int) ([]model.SupplyRecovery, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	return r.list(ctx, recoverySelect+` where status = 'claimable' and coalesce(claim_url, '') <> ''
		and (`+localRecoveryOwnershipClause+`)
		order by updated_at_ms asc, id asc limit ?`, limit)
}

func (r *repository) ListImportPending(ctx context.Context, limit int) ([]model.SupplyRecovery, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	return r.list(ctx, recoverySelect+` where
		(status in ('importing','partial') and coalesce(claim_order_id, '') <> '')
		or (status in ('claimed','claiming') and exists (
			select 1 from supply_orders o
			where o.order_id = 'recovery-' || supply_recoveries.recovery_id
			and exists (select 1 from supply_import_items i where i.order_id = o.order_id)
		))
		order by updated_at_ms asc, id asc limit ?`, limit)
}

func (r *repository) ClaimForProcessing(ctx context.Context, recoveryID string, nowMS int64) (model.SupplyRecovery, bool, error) {
	recoveryID = strings.TrimSpace(recoveryID)
	if recoveryID == "" {
		return model.SupplyRecovery{}, false, errors.New("supply recovery id is required")
	}
	if nowMS <= 0 {
		nowMS = time.Now().UnixMilli()
	}
	var recovery model.SupplyRecovery
	claimed := false
	err := sqliterepo.WithTxBusyRetry(ctx, r.db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `update supply_recoveries set status = 'claiming', last_error = null, updated_at_ms = ?
			where recovery_id = ? and status = 'claimable' and coalesce(claim_url, '') <> ''
			and (`+localRecoveryOwnershipClause+`)`, nowMS, recoveryID)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			claimed = false
			return nil
		}
		recovery, err = r.scan(tx.QueryRowContext(ctx, recoverySelect+` where recovery_id = ?`, recoveryID))
		if err != nil {
			return err
		}
		claimed = true
		return nil
	})
	return recovery, claimed, err
}

func (r *repository) PersistClaim(ctx context.Context, recovery model.SupplyRecovery, order model.SupplyOrder, items []model.SupplyImportItem, claimedAtMS int64) error {
	recovery.RecoveryID = strings.TrimSpace(recovery.RecoveryID)
	order.OrderID = strings.TrimSpace(order.OrderID)
	if recovery.RecoveryID == "" || order.OrderID == "" || len(items) == 0 {
		return errors.New("recovery claim persistence requires recovery, order, and import items")
	}
	if claimedAtMS <= 0 {
		claimedAtMS = time.Now().UnixMilli()
	}
	type protectedItem struct {
		item    model.SupplyImportItem
		payload string
	}
	protected := make([]protectedItem, 0, len(items))
	for _, item := range items {
		payload, err := r.protect(item.PayloadJSON)
		if err != nil {
			return err
		}
		protected = append(protected, protectedItem{item: item, payload: payload})
	}
	return sqliterepo.WithTxBusyRetry(ctx, r.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `insert or ignore into supply_orders (
			order_id, supplier_id, product, requested_quantity, automatic, strategy, trigger_reason, status, remote_status,
			ready_quantity, progress, status_url, take_url, charged_fen, released_fen,
			item_count, imported_count, last_error, next_poll_at_ms, supplier_retry_until_ms, completed_at_ms,
			created_at_ms, updated_at_ms
		) values (?, ?, ?, ?, 1, 'recovery', 'recovery_claimed', 'recovery_importing', 'recovery_claimed',
			0, 0, null, null, 0, 0, ?, 0, null, null, null, null, ?, ?)`,
			order.OrderID, strings.TrimSpace(order.SupplierID), order.Product, len(protected), len(protected), claimedAtMS, claimedAtMS)
		if err != nil {
			return err
		}
		for _, protectedItem := range protected {
			item := protectedItem.item
			if _, err := tx.ExecContext(ctx, `insert or ignore into supply_import_items (
				order_id, item_key, account_name, name_key, file_name, import_action, replaced_file_name,
				status, payload_json, attempt_count, lease_expires_at_ms, warranty_expires_at_ms,
				base_price_fen, charged_fen, created_at_ms, updated_at_ms
			) values (?, ?, ?, ?, ?, ?, ?, 'pending', ?, 0, ?, ?, ?, ?, ?, ?)`, order.OrderID, item.ItemKey,
				nullString(item.AccountName), nullString(item.NameKey), item.FileName, nullString(item.ImportAction), nullString(item.ReplacedFileName), protectedItem.payload,
				nullPositive(item.LeaseExpiresAtMS), nullPositive(item.WarrantyExpiresAtMS), item.BasePriceFen, item.ChargedFen, claimedAtMS, claimedAtMS); err != nil {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, `update supply_recoveries set status = 'importing',
			delivery_status = coalesce(nullif(?, ''), delivery_status), credential_version = case when ? > credential_version then ? else credential_version end,
			claim_url = null, claim_ticket = null, claim_order_id = ?, item_count = ?, imported_count = 0, last_error = null,
			claimed_at_ms = ?, updated_at_ms = ? where recovery_id = ?`,
			strings.ToLower(strings.TrimSpace(recovery.DeliveryStatus)), recovery.CredentialVersion, recovery.CredentialVersion,
			order.OrderID, len(protected), claimedAtMS, claimedAtMS, recovery.RecoveryID)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return errors.New("supply recovery disappeared while persisting claim")
		}
		return nil
	})
}

func (r *repository) ResetImport(ctx context.Context, recoveryID string, nowMS int64) (bool, error) {
	recoveryID = strings.TrimSpace(recoveryID)
	if recoveryID == "" {
		return false, errors.New("supply recovery id is required")
	}
	if nowMS <= 0 {
		nowMS = time.Now().UnixMilli()
	}
	reset := false
	err := sqliterepo.WithTxBusyRetry(ctx, r.db, func(tx *sql.Tx) error {
		var orderID string
		if err := tx.QueryRowContext(ctx, `select coalesce(claim_order_id, '') from supply_recoveries where recovery_id = ?`, recoveryID).Scan(&orderID); err != nil {
			return err
		}
		if strings.TrimSpace(orderID) == "" {
			reset = false
			return nil
		}
		var total, imported int
		if err := tx.QueryRowContext(ctx, `select count(*), coalesce(sum(case when status = 'imported' then 1 else 0 end), 0)
			from supply_import_items where order_id = ?`, orderID).Scan(&total, &imported); err != nil {
			return err
		}
		if total == 0 || imported >= total {
			reset = false
			return nil
		}
		if _, err := tx.ExecContext(ctx, `update supply_import_items set status = 'pending', last_error = null,
			next_retry_at_ms = null, updated_at_ms = ? where order_id = ? and status in ('pending','failed')`, nowMS, orderID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update supply_orders set status = 'recovery_importing', imported_count = ?,
			last_error = null, next_poll_at_ms = null, completed_at_ms = null, updated_at_ms = ? where order_id = ?`, imported, nowMS, orderID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update supply_recoveries set status = 'importing', item_count = ?, imported_count = ?,
			last_error = null, updated_at_ms = ? where recovery_id = ?`, total, imported, nowMS, recoveryID); err != nil {
			return err
		}
		reset = true
		return nil
	})
	return reset, err
}

func (r *repository) MarkClaimed(ctx context.Context, recoveryID string, claimOrderID string, itemCount int, claimedAtMS int64) error {
	if claimedAtMS <= 0 {
		claimedAtMS = time.Now().UnixMilli()
	}
	return sqliterepo.WithBusyRetry(ctx, func() error {
		_, err := r.db.ExecContext(ctx, `update supply_recoveries set status = 'importing',
		claim_order_id = ?, item_count = ?, imported_count = 0, last_error = null,
		claimed_at_ms = ?, updated_at_ms = ? where recovery_id = ?`,
			strings.TrimSpace(claimOrderID), itemCount, claimedAtMS, claimedAtMS, strings.TrimSpace(recoveryID))
		return err
	})
}

func (r *repository) MarkImportProgress(ctx context.Context, recoveryID string, itemCount int, importedCount int, lastError string) error {
	now := time.Now().UnixMilli()
	status := "importing"
	if itemCount > 0 && importedCount < itemCount {
		status = "partial"
	}
	return sqliterepo.WithBusyRetry(ctx, func() error {
		_, err := r.db.ExecContext(ctx, `update supply_recoveries set status = ?,
		item_count = ?, imported_count = ?, last_error = ?, updated_at_ms = ? where recovery_id = ?`,
			status, itemCount, importedCount, nullString(lastError), now, strings.TrimSpace(recoveryID))
		return err
	})
}

func (r *repository) MarkImported(ctx context.Context, recoveryID string, importedCount int) error {
	now := time.Now().UnixMilli()
	return sqliterepo.WithBusyRetry(ctx, func() error {
		_, err := r.db.ExecContext(ctx, `update supply_recoveries set status = 'imported',
		imported_count = ?, last_error = null, updated_at_ms = ? where recovery_id = ?`,
			importedCount, now, strings.TrimSpace(recoveryID))
		return err
	})
}

func (r *repository) MarkRefunded(ctx context.Context, recoveryID string, refundedFen int64) error {
	now := time.Now().UnixMilli()
	return sqliterepo.WithBusyRetry(ctx, func() error {
		_, err := r.db.ExecContext(ctx, `update supply_recoveries set status = 'refunded',
		refunded_fen = ?, updated_at_ms = ? where recovery_id = ?`,
			refundedFen, now, strings.TrimSpace(recoveryID))
		return err
	})
}

func (r *repository) MarkFailed(ctx context.Context, recoveryID string, lastError string) error {
	now := time.Now().UnixMilli()
	return sqliterepo.WithBusyRetry(ctx, func() error {
		_, err := r.db.ExecContext(ctx, `update supply_recoveries set status = 'failed',
		last_error = ?, updated_at_ms = ? where recovery_id = ?`,
			nullString(lastError), now, strings.TrimSpace(recoveryID))
		return err
	})
}

func (r *repository) SetLastError(ctx context.Context, recoveryID string, lastError string) error {
	return sqliterepo.WithBusyRetry(ctx, func() error {
		_, err := r.db.ExecContext(ctx, `update supply_recoveries set last_error = ?, updated_at_ms = ? where recovery_id = ?`,
			nullString(lastError), time.Now().UnixMilli(), strings.TrimSpace(recoveryID))
		return err
	})
}

func (r *repository) Summary(ctx context.Context) (Summary, error) {
	rows, err := r.db.QueryContext(ctx, `select status,
		case when (`+localRecoveryOwnershipClause+`) then 1 else 0 end as local_owner,
		count(*) from supply_recoveries group by status, local_owner`)
	if err != nil {
		return Summary{}, err
	}
	defer rows.Close()
	var summary Summary
	for rows.Next() {
		var status string
		var localOwner int
		var count int
		if err := rows.Scan(&status, &localOwner, &count); err != nil {
			return Summary{}, err
		}
		if localOwner == 0 {
			summary.External += count
			continue
		}
		summary.Total += count
		switch status {
		case "claimable":
			summary.Claimable += count
		case "claiming", "importing", "partial":
			summary.Importing += count
		case "imported":
			summary.Imported += count
		case "refunded":
			summary.Refunded += count
		case "failed":
			summary.Failed += count
		}
	}
	return summary, rows.Err()
}

func (r *repository) list(ctx context.Context, query string, args ...any) ([]model.SupplyRecovery, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	recoveries := make([]model.SupplyRecovery, 0)
	for rows.Next() {
		recovery, err := r.scan(rows)
		if err != nil {
			return nil, err
		}
		recoveries = append(recoveries, recovery)
	}
	return recoveries, rows.Err()
}

const recoverySelect = `select id, recovery_id, supplier_id, product, delivery_status, status, original_file_name,
	credential_version, source_order_id, original_auth_index, original_email, claim_url, claim_ticket, claim_order_id, item_count, imported_count,
	refunded_fen, last_error, raw_json, last_seen_at_ms, claimed_at_ms, created_at_ms, updated_at_ms
	from supply_recoveries`

type scanner interface{ Scan(...any) error }

func (r *repository) scan(row scanner) (model.SupplyRecovery, error) {
	var recovery model.SupplyRecovery
	var supplierID, product, originalFileName, sourceOrderID, originalAuthIndex, originalEmail, claimURL, claimTicket, claimOrderID, lastError, rawJSON sql.NullString
	var claimedAtMS sql.NullInt64
	if err := row.Scan(&recovery.ID, &recovery.RecoveryID, &supplierID, &product, &recovery.DeliveryStatus, &recovery.Status,
		&originalFileName, &recovery.CredentialVersion, &sourceOrderID, &originalAuthIndex, &originalEmail, &claimURL, &claimTicket, &claimOrderID, &recovery.ItemCount,
		&recovery.ImportedCount, &recovery.RefundedFen, &lastError, &rawJSON, &recovery.LastSeenAtMS,
		&claimedAtMS, &recovery.CreatedAtMS, &recovery.UpdatedAtMS); err != nil {
		return model.SupplyRecovery{}, err
	}
	unprotectedClaimURL, err := r.unprotect(claimURL.String)
	if err != nil {
		return model.SupplyRecovery{}, err
	}
	unprotectedClaimTicket, err := r.unprotect(claimTicket.String)
	if err != nil {
		return model.SupplyRecovery{}, err
	}
	unprotectedRawJSON, err := r.unprotect(rawJSON.String)
	if err != nil {
		return model.SupplyRecovery{}, err
	}
	recovery.SupplierID = supplierID.String
	recovery.Product = product.String
	recovery.SourceOrderID = sourceOrderID.String
	recovery.OriginalFileName = originalFileName.String
	recovery.OriginalAuthIndex = originalAuthIndex.String
	recovery.OriginalEmail = originalEmail.String
	recovery.ClaimURL = unprotectedClaimURL
	recovery.ClaimTicket = unprotectedClaimTicket
	recovery.ClaimOrderID = claimOrderID.String
	recovery.LastError = lastError.String
	recovery.RawJSON = unprotectedRawJSON
	recovery.ClaimedAtMS = claimedAtMS.Int64
	return recovery, nil
}

// localRecoveryOwnershipClause scopes one-time supplier recovery tickets to
// the CPAM instance that owns the original pickup order. Claim-order linkage
// keeps already imported legacy rows local, while the filename fallback covers
// older supplier payloads that predate source_order_id.
const localRecoveryOwnershipClause = `(coalesce(supply_recoveries.claim_order_id, '') <> ''
	or (coalesce(supply_recoveries.source_order_id, '') <> '' and exists (
		select 1 from supply_orders source_order where source_order.order_id = supply_recoveries.source_order_id
	))
	or (coalesce(supply_recoveries.source_order_id, '') = '' and coalesce(supply_recoveries.original_file_name, '') <> '' and exists (
		select 1 from supply_import_items source_item
		where lower(source_item.file_name) = lower(supply_recoveries.original_file_name)
	)))`

func statusFromDelivery(deliveryStatus string, claimURL string, refundedFen int64) string {
	status := strings.ToLower(strings.TrimSpace(deliveryStatus))
	switch status {
	case "claimable", "ready", "available":
		if strings.TrimSpace(claimURL) != "" {
			return "claimable"
		}
	case "refunded", "refund", "failed_refunded":
		return "refunded"
	case "claimed", "completed", "done":
		return "claimed"
	}
	if refundedFen > 0 {
		return "refunded"
	}
	return "seen"
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
