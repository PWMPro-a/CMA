package supply

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	managerconfigsvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

const (
	purchaseTaskStatusPending   = "pending"
	purchaseTaskStatusRunning   = "running"
	purchaseTaskStatusCompleted = "completed"
	purchaseTaskStatusCancelled = "cancelled"
)

type purchaseTaskOrderStats struct {
	fulfilled        int
	committedPending int
	reservedPending  int
	orderCount       int
	activeOrderCount int
}

// A supplier can leave a zero-delivery automatic reservation in
// waiting_inventory for much longer than the normal fulfillment window. Stop
// treating it as an active local order after this bound so the task keeps only
// its configured number of meaningful live reservations. Manual tasks
// deliberately keep every waiting order because their explicit platform choice
// is an operator contract.
const purchaseTaskStaleAutomaticWaitingAge = 5 * time.Minute

func purchaseTaskAdmissionOrderCount(orders []store.SupplyOrder, now time.Time) int {
	count := 0
	for _, order := range orders {
		if purchaseTaskOrderReservationStale(order, now) {
			continue
		}
		count++
	}
	return count
}

func (s *Service) createManualPurchaseTask(ctx context.Context, quantity int, supplierID string, requestedProduct ...string) (store.SupplyPurchaseTask, error) {
	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return store.SupplyPurchaseTask{}, err
	}
	if err := s.requireCredentials(cfg.Supply); err != nil {
		return store.SupplyPurchaseTask{}, err
	}
	platform, err := resolveSupplyPlatform(cfg.Supply, supplierID, "")
	if err != nil {
		return store.SupplyPurchaseTask{}, err
	}
	if !supplyPlatformConfigured(platform) {
		return store.SupplyPurchaseTask{}, ErrNotConfigured
	}
	product := platform.Product
	if len(requestedProduct) > 0 && strings.TrimSpace(requestedProduct[0]) != "" {
		product = strings.ToLower(strings.TrimSpace(requestedProduct[0]))
		if !supplyProductSupportedByPlatform(platform, product) {
			return store.SupplyPurchaseTask{}, fmt.Errorf("product %s is not supported by supply platform %s", product, platform.ID)
		}
	}
	task, err := s.store.CreateSupplyPurchaseTask(ctx, store.SupplyPurchaseTask{
		TaskID:              "purchase-" + uuid.NewString(),
		Source:              "manual",
		SupplierID:          platform.ID,
		Product:             product,
		TargetQuantity:      quantity,
		Status:              purchaseTaskStatusPending,
		Strategy:            supplyOrderStrategy(cfg.Supply, false),
		TriggerReason:       "manual",
		MaxConcurrentOrders: 1,
	})
	if err == nil {
		s.invalidateStatusCache()
	}
	return task, err
}

// upsertAutomaticPurchaseTask keeps one durable automatic intent. The planner's
// quantity is the current remaining deficit, so an already fulfilled prefix is
// added back when expanding the durable target.
func (s *Service) upsertAutomaticPurchaseTask(ctx context.Context, planned store.SupplyPurchaseTask) (store.SupplyPurchaseTask, error) {
	existing, found, err := s.store.GetActiveAutomaticSupplyPurchaseTask(ctx)
	if err != nil {
		return store.SupplyPurchaseTask{}, err
	}
	if !found {
		return s.createAutomaticPurchaseTask(ctx, planned)
	}
	existing, err = s.reconcilePurchaseTask(ctx, existing)
	if err != nil {
		return store.SupplyPurchaseTask{}, err
	}
	if existing.Status != purchaseTaskStatusPending && existing.Status != purchaseTaskStatusRunning {
		return s.createAutomaticPurchaseTask(ctx, planned)
	}
	plannedLowPrice := isLowPriceReserveTrigger(planned.TriggerReason)
	existingLowPrice := isLowPriceReserveTrigger(existing.TriggerReason)
	if plannedLowPrice != existingLowPrice {
		if plannedLowPrice {
			// A normal capacity/emergency task owns the automatic executor until
			// it finishes. A bargain reserve must never dilute that shortage target.
			return existing, nil
		}
		if err := s.cancelLowPriceReservePurchaseTask(ctx, &existing, "superseded by live replenishment demand"); err != nil {
			return store.SupplyPurchaseTask{}, err
		}
		return s.createAutomaticPurchaseTask(ctx, planned)
	}
	if plannedLowPrice && existingLowPrice {
		// The watcher enqueues one bounded ladder stage. Repeated millisecond
		// quotes while that stage is pending must not turn it into an ever-growing
		// task target.
		return existing, nil
	}
	desiredTarget := existing.FulfilledQuantity + max(1, planned.TargetQuantity)
	if desiredTarget > existing.TargetQuantity {
		existing.TargetQuantity = desiredTarget
	}
	if strings.TrimSpace(planned.Product) != "" {
		existing.Product = planned.Product
	}
	if strings.TrimSpace(planned.Strategy) != "" {
		existing.Strategy = planned.Strategy
	}
	if strings.TrimSpace(planned.TriggerReason) != "" {
		existing.TriggerReason = planned.TriggerReason
	}
	existing.MaxConcurrentOrders = max(existing.MaxConcurrentOrders, planned.MaxConcurrentOrders)
	existing.NextAttemptAtMS = 0
	if err := s.store.UpdateSupplyPurchaseTask(ctx, existing); err != nil {
		return store.SupplyPurchaseTask{}, err
	}
	s.invalidateStatusCache()
	return existing, nil
}

func (s *Service) createAutomaticPurchaseTask(ctx context.Context, planned store.SupplyPurchaseTask) (store.SupplyPurchaseTask, error) {
	planned.TaskID = "purchase-" + uuid.NewString()
	planned.Source = "automatic"
	planned.SupplierID = ""
	if planned.Status == "" {
		planned.Status = purchaseTaskStatusPending
	}
	created, err := s.store.CreateSupplyPurchaseTask(ctx, planned)
	if err == nil {
		s.invalidateStatusCache()
	}
	return created, err
}

func (s *Service) ListPurchaseTasks(ctx context.Context, limit int) ([]store.SupplyPurchaseTask, error) {
	return s.listPurchaseTasks(ctx, limit)
}

func (s *Service) listPurchaseTasks(ctx context.Context, limit int) ([]store.SupplyPurchaseTask, error) {
	tasks, err := s.store.ListSupplyPurchaseTasks(ctx, limit)
	if err != nil {
		return nil, err
	}
	taskIDs := make([]string, 0, len(tasks))
	for _, task := range tasks {
		taskIDs = append(taskIDs, task.TaskID)
	}
	orders, err := s.store.ListSupplyOrdersByTaskIDs(ctx, taskIDs)
	if err != nil {
		return nil, err
	}
	ordersByTaskID := make(map[string][]store.SupplyOrder, len(tasks))
	for _, order := range orders {
		ordersByTaskID[order.TaskID] = append(ordersByTaskID[order.TaskID], order)
	}
	for index := range tasks {
		stats := summarizePurchaseTaskOrders(ordersByTaskID[tasks[index].TaskID])
		tasks[index].FulfilledQuantity = stats.fulfilled
		tasks[index].OrderCount = stats.orderCount
		tasks[index].ActiveOrderCount = stats.activeOrderCount
	}
	return tasks, nil
}

func (s *Service) CancelPurchaseTask(ctx context.Context, taskID string) (Status, error) {
	_, found, err := s.cancelPurchaseTaskAndChildren(ctx, taskID, time.Now().UnixMilli())
	if err != nil {
		return Status{}, err
	}
	if !found {
		return Status{}, ErrPurchaseTaskNotFound
	}
	s.invalidateStatusCache()
	s.signalPurchaseTaskWorker()
	return s.GetStatus(ctx, 50)
}

func (s *Service) cancelPurchaseTaskAndChildren(ctx context.Context, taskID string, nowMS int64) (store.SupplyPurchaseTask, bool, error) {
	if s == nil || s.store == nil {
		return store.SupplyPurchaseTask{}, false, nil
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()

	task, _, err := s.store.CancelSupplyPurchaseTask(ctx, taskID, nowMS)
	if err != nil {
		return store.SupplyPurchaseTask{}, false, err
	}
	if strings.TrimSpace(task.TaskID) == "" {
		return store.SupplyPurchaseTask{}, false, nil
	}
	if task.Status == purchaseTaskStatusCancelled {
		if err := s.cancelReversiblePurchaseTaskOrders(ctx, task.TaskID, nowMS); err != nil {
			return store.SupplyPurchaseTask{}, true, err
		}
	}
	return task, true, nil
}

func (s *Service) cancelReversiblePurchaseTaskOrders(ctx context.Context, taskID string, nowMS int64) error {
	orders, err := s.store.ListSupplyOrdersByTaskID(ctx, taskID)
	if err != nil {
		return err
	}
	changed := false
	for _, order := range orders {
		if !cancelReversiblePurchaseTaskOrder(&order, nowMS) {
			continue
		}
		if err := s.store.UpdateSupplyOrder(ctx, order); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		s.invalidateSupplyOrdersCache()
	}
	return nil
}

func cancelReversiblePurchaseTaskOrder(order *store.SupplyOrder, nowMS int64) bool {
	if order == nil || !reportOpenOrderStatus(order.Status) {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(order.Status))
	if status == "taking" || status == "importing" || status == "partial" ||
		order.ItemCount > order.ImportedCount || order.ChargedFen > 0 {
		return false
	}
	if nowMS <= 0 {
		nowMS = time.Now().UnixMilli()
	}
	order.Status = "cancelled"
	order.RemoteStatus = "task_cancelled"
	order.LastError = "purchase task cancelled; supplier reservation left to expire"
	order.NextPollAtMS = 0
	order.SupplierRetryUntilMS = 0
	order.CompletedAtMS = nowMS
	return true
}

func (s *Service) RunPurchaseTasks(ctx context.Context) error {
	if s == nil || s.store == nil {
		return nil
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()

	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return err
	}
	nowMS := time.Now().UnixMilli()
	openOrders, err := s.store.ListOpenSupplyOrders(ctx, maxTrackedOpenSupplyOrders)
	if err != nil {
		return err
	}
	resource := s.currentSmartResource(cfg.Supply)
	if resource.SnapshotFresh && !smartResourceEmergency(resource) {
		if active, found, activeErr := s.store.GetActiveAutomaticSupplyPurchaseTask(ctx); activeErr != nil {
			return activeErr
		} else if found && !isLowPriceReserveTrigger(active.TriggerReason) {
			available, countErr := s.countAvailableAccounts(ctx, cfg)
			if countErr != nil {
				return countErr
			}
			if applyOrdinaryAccountTargetGate(cfg.Supply, &resource, available) {
				cancelled, cancelErr := s.cancelSatisfiedOrdinaryAutomaticTask(ctx, cfg, resource, available)
				if cancelErr != nil {
					return cancelErr
				}
				if cancelled {
					s.setSmartResource(resource)
					s.updateCPAOverview(available, cfg.Supply.TargetAvailableAccounts)
					openOrders, err = s.store.ListOpenSupplyOrders(ctx, maxTrackedOpenSupplyOrders)
					if err != nil {
						return err
					}
				}
			}
		}
	}
	openOrders, err = s.reconcileUnavailableSupplyOrders(ctx, cfg.Supply, openOrders)
	if err != nil {
		return err
	}
	// Supplier lifecycle reconciliation runs before creating another reservation.
	// A polled order that is still only waiting for inventory keeps its own slot,
	// but must not consume the entire worker turn when the task has other slots and
	// a newly enlarged shortage to cover.
	for _, order := range openOrders {
		if strings.TrimSpace(order.TaskID) == "" || !s.purchaseTaskOrderPollDue(cfg.Supply, order, nowMS) {
			continue
		}
		processErr := s.processOrder(ctx, cfg, order)
		if task, found, taskErr := s.store.GetSupplyPurchaseTask(ctx, order.TaskID); taskErr != nil {
			return taskErr
		} else if found {
			if _, reconcileErr := s.reconcilePurchaseTask(ctx, task); reconcileErr != nil {
				return reconcileErr
			}
		}
		if processErr != nil {
			return processErr
		}
		processed, found, getErr := s.store.GetSupplyOrder(ctx, order.OrderID)
		if getErr != nil {
			return getErr
		}
		if !found || isSupplyOrderCapacityCommitted(processed) {
			return nil
		}
		if purchaseTaskOrderRequiresOperatorReview(processed) {
			return nil
		}
		switch strings.ToLower(strings.TrimSpace(processed.Status)) {
		case "created", "waiting_inventory", "released":
			openOrders, err = s.store.ListOpenSupplyOrders(ctx, maxTrackedOpenSupplyOrders)
			if err != nil {
				return err
			}
			// Continue into task admission below. One supplier order was either polled
			// or expired locally, and at most one new reservation is still created in
			// this worker turn.
		case "creating", "create_uncertain", "taking", "importing", "partial":
			return nil
		default:
			return nil
		}
		break
	}

	tasks, err := s.store.ListActiveSupplyPurchaseTasks(ctx, 20)
	if err != nil {
		return err
	}
	for _, candidate := range tasks {
		if candidate.Source == "automatic" && isLowPriceReserveTrigger(candidate.TriggerReason) {
			normalActive := false
			for _, other := range tasks {
				if other.Source == "automatic" && !isLowPriceReserveTrigger(other.TriggerReason) &&
					(other.Status == purchaseTaskStatusPending || other.Status == purchaseTaskStatusRunning) {
					normalActive = true
					break
				}
			}
			if normalActive {
				continue
			}
		}
		task, reconcileErr := s.reconcilePurchaseTask(ctx, candidate)
		if reconcileErr != nil {
			return reconcileErr
		}
		if task.Status != purchaseTaskStatusPending && task.Status != purchaseTaskStatusRunning {
			continue
		}
		if task.NextAttemptAtMS > nowMS {
			continue
		}
		orders, listErr := s.store.ListSupplyOrdersByTaskID(ctx, task.TaskID)
		if listErr != nil {
			return listErr
		}
		if purchaseTaskOrdersRequireOperatorReview(orders) {
			continue
		}
		stats := summarizePurchaseTaskOrders(orders)
		if stats.activeOrderCount >= max(1, task.MaxConcurrentOrders) {
			continue
		}
		if task.MaxConcurrentOrders <= 1 && stats.activeOrderCount > 0 {
			continue
		}
		openOrders, err = s.store.ListOpenSupplyOrders(ctx, maxTrackedOpenSupplyOrders)
		if err != nil {
			return err
		}
		admissionOrderCount := purchaseTaskAdmissionOrderCount(openOrders, time.UnixMilli(nowMS))
		if admissionOrderCount >= maxConcurrentSupplyOrders(cfg.Supply) {
			continue
		}
		// An active supplier reservation already occupies part of the task target,
		// even before inventory is ready or payment is committed. Subtract it here
		// so parallel worker slots split one target instead of submitting several
		// identical full-deficit orders.
		remaining := task.TargetQuantity - stats.fulfilled - stats.reservedPending
		if remaining <= 0 {
			continue
		}
		if task.Source == "automatic" && isLowPriceReserveTrigger(task.TriggerReason) {
			if !supplyLowPriceReserveEnabled(cfg.Supply) {
				return s.cancelLowPriceReservePurchaseTask(ctx, &task, "low-price reserve is disabled")
			}
			available, countErr := s.countLowPriceReserveAccounts(ctx, cfg)
			if countErr != nil {
				return countErr
			}
			liveRemaining := cfg.Supply.LowPriceReserveTargetAccounts - available - stats.reservedPending
			if liveRemaining <= 0 {
				return s.cancelLowPriceReservePurchaseTask(ctx, &task, "low-price reserve target is already satisfied")
			}
			remaining = min(remaining, liveRemaining)
		}
		availableTaskSlots := max(1, task.MaxConcurrentOrders) - stats.activeOrderCount
		availableGlobalSlots := maxConcurrentSupplyOrders(cfg.Supply) - admissionOrderCount
		availableSlots := min(availableTaskSlots, availableGlobalSlots)
		quantity := purchaseTaskAdaptiveOrderQuantity(
			remaining,
			availableSlots,
			task.Source == "automatic",
			s.currentSmartResource(cfg.Supply),
		)
		return s.createPurchaseTaskOrder(ctx, cfg, &task, quantity, openOrders, stats.activeOrderCount)
	}
	return nil
}

// cancelSatisfiedOrdinaryAutomaticTask runs while the caller already owns
// runMu. It cancels only the ordinary automatic intent and reversible,
// uncharged child reservations. Paid/importing orders remain intact so supplier
// funds and delivered credentials are never discarded. The dedicated bargain
// reserve task is intentionally left active.
func (s *Service) cancelSatisfiedOrdinaryAutomaticTask(
	ctx context.Context,
	cfg store.ManagerConfig,
	resource SmartResource,
	available int,
) (bool, error) {
	if s == nil || s.store == nil || !ordinaryAccountTargetReached(cfg.Supply, resource, available) {
		return false, nil
	}
	task, found, err := s.store.GetActiveAutomaticSupplyPurchaseTask(ctx)
	if err != nil || !found {
		return false, err
	}
	if task.Source != "automatic" || isLowPriceReserveTrigger(task.TriggerReason) {
		return false, nil
	}
	nowMS := time.Now().UnixMilli()
	task.Status = purchaseTaskStatusCancelled
	task.CancelledAtMS = nowMS
	task.NextAttemptAtMS = 0
	task.LastError = "ordinary account target reached; normal procurement stopped"
	if err := s.store.UpdateSupplyPurchaseTask(ctx, task); err != nil {
		return false, err
	}
	if err := s.cancelReversiblePurchaseTaskOrders(ctx, task.TaskID, nowMS); err != nil {
		return false, err
	}
	s.invalidateStatusCache()
	return true, nil
}

// reconcileUnavailableSupplyOrders closes local reservations whose configured
// platform identity no longer exists. Automatic intents are immediately
// replanned against the remaining healthy platform/product candidates; manual
// intents are terminated because their explicit operator selection is gone.
func (s *Service) reconcileUnavailableSupplyOrders(
	ctx context.Context,
	cfg store.ManagerSupplyConfig,
	orders []store.SupplyOrder,
) ([]store.SupplyOrder, error) {
	changed := false
	nowMS := time.Now().UnixMilli()
	for _, order := range orders {
		platform, resolveErr := resolveSupplyPlatform(cfg, order.SupplierID, order.Product)
		if resolveErr == nil && supplyPlatformConfigured(platform) {
			continue
		}
		if resolveErr == nil {
			resolveErr = fmt.Errorf("%w: supply platform %s is not configured", ErrNotConfigured, order.SupplierID)
		}
		message := safeError(resolveErr)
		order.Status = "failed"
		order.RemoteStatus = "platform_unavailable"
		order.LastError = message
		order.NextPollAtMS = 0
		order.SupplierRetryUntilMS = 0
		order.CompletedAtMS = nowMS
		if err := s.store.UpdateSupplyOrder(ctx, order); err != nil {
			return nil, err
		}
		changed = true
		if strings.TrimSpace(order.TaskID) == "" {
			continue
		}
		task, found, err := s.store.GetSupplyPurchaseTask(ctx, order.TaskID)
		if err != nil {
			return nil, err
		}
		if !found || task.Status == purchaseTaskStatusCompleted || task.Status == purchaseTaskStatusCancelled {
			continue
		}
		task.LastError = message
		if task.Source == "manual" {
			task.Status = purchaseTaskStatusCancelled
			task.CancelledAtMS = nowMS
			task.NextAttemptAtMS = 0
		} else {
			task.Status = purchaseTaskStatusRunning
			task.SupplierID = ""
			task.Product = ""
			task.NextAttemptAtMS = 0
		}
		if err := s.store.UpdateSupplyPurchaseTask(ctx, task); err != nil {
			return nil, err
		}
	}
	if !changed {
		return orders, nil
	}
	s.invalidateSupplyOrdersCache()
	return s.store.ListOpenSupplyOrders(ctx, maxTrackedOpenSupplyOrders)
}

// purchaseTaskNextOrderQuantity shards the uncovered target across the worker
// slots that can still be filled. For example, a target of 20 with three slots
// becomes 7 + 7 + 6 instead of three competing orders of 20. Smaller supplier
// reservations are easier to fill, while the aggregate reservation remains
// bounded by the task target and the ready-order take budget chooses the final
// live-deficit combination.
func purchaseTaskNextOrderQuantity(remaining int, availableSlots int) int {
	if remaining <= 0 || availableSlots <= 0 {
		return 0
	}
	availableSlots = min(availableSlots, remaining)
	return min(100, (remaining+availableSlots-1)/availableSlots)
}

// purchaseTaskAdaptiveOrderQuantity keeps normal procurement evenly sharded,
// but widens the aggregate capture window when the current supplier is both
// inventory-scarce and historically slow. The widened quantity is still split
// across the available child-order slots, so a partial delivery can advance
// the task without submitting one oversized reservation. Reserved quantities
// remain bounded by the task's next remaining amount plus this single-order
// overage; the next worker turn recalculates from actual delivery/reservation
// state.
func purchaseTaskAdaptiveOrderQuantity(
	remaining int,
	availableSlots int,
	automatic bool,
	resource SmartResource,
) int {
	base := purchaseTaskNextOrderQuantity(remaining, availableSlots)
	if base <= 0 || !automatic {
		return base
	}
	scarce := resource.SupplyPressureLevel == smartSupplyPressureScarce ||
		resource.SupplyNeedsProduction ||
		resource.SupplyInventoryAvailable <= 0 ||
		resource.SupplyInventoryMissing > 0
	slow := resource.SupplyAvgFulfillSeconds >= 120 ||
		resource.SupplyRecentWaiting >= 2 ||
		resource.PurchaseLeadMinutes >= 10
	if !scarce || !slow {
		return base
	}
	overagePercent := 20
	if (resource.SupplyFulfillmentRate > 0 && resource.SupplyFulfillmentRate < 85) ||
		resource.SupplyRecentZeroDelivery >= 2 || resource.SupplyRecentCancelled >= 2 {
		overagePercent = 35
	}
	extra := max(1, (remaining*overagePercent+99)/100)
	boosted := remaining + extra
	return min(100, (boosted+availableSlots-1)/availableSlots)
}

func (s *Service) createPurchaseTaskOrder(
	ctx context.Context,
	cfg store.ManagerConfig,
	task *store.SupplyPurchaseTask,
	quantity int,
	openOrders []store.SupplyOrder,
	activeTaskOrders int,
) error {
	if task == nil || quantity <= 0 {
		return nil
	}
	if stopped, err := s.stopOrdinaryAutomaticTaskOnRecoveredTiming(ctx, cfg.Supply, task, activeTaskOrders); stopped || err != nil {
		return err
	}
	requestedSupplierID := ""
	if task.Source == "manual" {
		requestedSupplierID = task.SupplierID
	}
	lowPriceReserve := task.Source == "automatic" && isLowPriceReserveTrigger(task.TriggerReason)
	var selection supplyPlatformSelection
	var err error
	if lowPriceReserve {
		if !supplyLowPriceReserveEnabled(cfg.Supply) {
			return s.cancelLowPriceReservePurchaseTask(ctx, task, "low-price reserve is disabled")
		}
		matched := false
		selection, matched, err = s.selectLowPriceReservePlatform(ctx, cfg.Supply, quantity, openOrders, task.Product)
		if err != nil {
			s.beginLowPriceReserveBackoff(lowPriceReserveExactQuoteBackoff, err, lowPriceReserveRetryBackoff)
			return s.recordPurchaseTaskError(ctx, task, err)
		}
		if !matched {
			return s.cancelLowPriceReservePurchaseTask(ctx, task, lowPriceReserveInventoryUnavailable)
		}
		quantity = min(quantity, selection.status.Inventory.Available)
		if quantity <= 0 {
			return s.cancelLowPriceReservePurchaseTask(ctx, task, lowPriceReserveInventoryUnavailable)
		}
		// Re-quote the exact reduced quantity before CreateOrder so a partially
		// available bargain never inherits the price of a larger stale quote.
		exact, exactMatched, exactErr := s.selectLowPriceReservePlatform(ctx, cfg.Supply, quantity, openOrders, task.Product)
		if exactErr != nil {
			s.beginLowPriceReserveBackoff(lowPriceReserveExactQuoteBackoff, exactErr, lowPriceReserveRetryBackoff)
			return s.recordPurchaseTaskError(ctx, task, exactErr)
		}
		if !exactMatched {
			return s.cancelLowPriceReservePurchaseTask(ctx, task, lowPriceReserveInventoryUnavailable)
		}
		selection = exact
	} else {
		selection, err = s.selectSupplyPlatformProduct(ctx, cfg.Supply, quantity, openOrders, requestedSupplierID, task.Product)
		if err != nil {
			return s.recordPurchaseTaskError(ctx, task, err)
		}
	}
	if selection.quantity > 0 && selection.quantity < quantity {
		quantity = selection.quantity
	}
	platform := selection.platform
	inventory := *selection.status.Inventory
	balance := *selection.status.Balance
	s.stateMu.RLock()
	overview := s.overview
	s.stateMu.RUnlock()
	overview.CheckedAtMS = time.Now().UnixMilli()
	overview.Inventory = &inventory
	overview.Balance = &balance
	overview.SelectedPlatformID = platform.ID
	overview.Platforms = selection.all
	s.setOverview(overview)
	if inventory.EstimatedTotalFen > 0 && balance.AvailableFen < inventory.EstimatedTotalFen {
		return s.recordPurchaseTaskError(ctx, task, ErrInsufficientBalance)
	}
	if cfg.Supply.MinBalanceReserveFen > 0 && inventory.EstimatedTotalFen > 0 &&
		balance.AvailableFen-inventory.EstimatedTotalFen < cfg.Supply.MinBalanceReserveFen {
		return s.recordPurchaseTaskError(ctx, task, ErrInsufficientBalance)
	}
	// Exact supplier selection can take several seconds. Real request quota
	// headers may recover the pool above its just-in-time purchase line while
	// that quote is in flight. Re-check the current local capacity decision at
	// the last reversible boundary so a durable task planned from the previous
	// sample cannot create an already-unnecessary supplier order.
	if stopped, err := s.stopOrdinaryAutomaticTaskOnRecoveredTiming(ctx, cfg.Supply, task, activeTaskOrders); stopped || err != nil {
		return err
	}

	triggerReason := strings.TrimSpace(task.TriggerReason)
	if triggerReason == "" {
		triggerReason = task.Source
	}
	if activeTaskOrders > 0 {
		triggerReason = parallelSupplyTriggerReason(triggerReason)
	}
	attempt := store.SupplyOrder{
		OrderID:                   newCreateAttemptID(),
		TaskID:                    task.TaskID,
		SupplierID:                platform.ID,
		MarketplaceSellerID:       marketplaceSellerID(selection.marketplaceSeller),
		MarketplaceSellerName:     marketplaceSellerName(selection.marketplaceSeller),
		MarketplaceChannelID:      marketplaceSellerChannelID(selection.marketplaceSeller),
		MarketplaceSelectionToken: marketplaceSellerSelectionToken(selection.marketplaceSeller),
		Product:                   platform.Product,
		RequestedQuantity:         quantity,
		Automatic:                 task.Source == "automatic",
		Strategy:                  task.Strategy,
		TriggerReason:             triggerReason,
		Status:                    "creating",
	}
	attempt, err = s.store.CreateSupplyOrder(ctx, attempt)
	if err != nil {
		return s.recordPurchaseTaskError(ctx, task, err)
	}
	s.invalidateMarketplaceSupplierQuotaScores(platform.ID, platform.Product)
	s.invalidateSupplyOrdersCache()
	defer s.invalidateSupplyOrdersCache()
	task.Status = purchaseTaskStatusRunning
	task.AttemptCount++
	task.SupplierID = platform.ID
	task.Product = platform.Product
	task.NextAttemptAtMS = 0
	task.LastError = ""
	if err := s.store.UpdateSupplyPurchaseTask(ctx, *task); err != nil {
		return err
	}
	if attempt.Automatic {
		s.markAutomaticCreate()
	}

	credentials := marketplaceSellerCredentials(platform, selection.marketplaceSeller)
	if task.Source == "manual" {
		credentials.MaxUnitPriceFen = 0
	} else if lowPriceReserve {
		credentials.MaxUnitPriceFen = lowPriceReservePlatformCeiling(cfg.Supply, platform)
	}
	remote, err := s.supplyClient.CreateOrder(ctx, credentials, platform.Product, quantity, attempt.OrderID)
	if err != nil {
		if isDefiniteCreateFailure(err) {
			attempt.Status = "failed"
			attempt.LastError = safeError(err)
			attempt.CompletedAtMS = time.Now().UnixMilli()
			if updateErr := s.store.UpdateSupplyOrder(ctx, attempt); updateErr != nil {
				return updateErr
			}
		} else {
			attempt.Status = "create_uncertain"
			attempt.LastError = safeError(err)
			if updateErr := s.store.UpdateSupplyOrder(ctx, attempt); updateErr != nil {
				return updateErr
			}
		}
		return s.recordPurchaseTaskError(ctx, task, err)
	}
	if lowPriceReserve {
		s.clearLowPriceReserveBackoff()
	}
	order := supplyOrderFromCreateResponse(attempt, remote, cfg.Supply)
	if err := s.store.PromoteSupplyCreateAttempt(ctx, attempt.OrderID, order); err != nil {
		attempt.Status = "create_uncertain"
		attempt.RemoteStatus = remote.Status
		attempt.StatusURL = remote.StatusURL
		attempt.TakeURL = remote.TakeURL
		attempt.LastError = safeError(fmt.Errorf("remote order %s was created but local persistence failed: %w", remote.ID, err))
		_ = s.store.UpdateSupplyOrder(ctx, attempt)
		return s.recordPurchaseTaskError(ctx, task, err)
	}
	if order.Status == "failed" {
		return s.recordPurchaseTaskError(ctx, task, fmt.Errorf("supplier purchase returned status %q without a delivery", remote.Status))
	}
	if order.Status == "ready" || order.Status == "taking" {
		if err := s.processOrder(ctx, cfg, order); err != nil {
			_ = s.recordPurchaseTaskError(ctx, task, err)
			return err
		}
		_, err = s.reconcilePurchaseTask(ctx, *task)
		return err
	}
	return nil
}

func (s *Service) cancelLowPriceReservePurchaseTask(
	ctx context.Context,
	task *store.SupplyPurchaseTask,
	reason string,
) error {
	if task == nil {
		return nil
	}
	nowMS := time.Now().UnixMilli()
	task.Status = purchaseTaskStatusCancelled
	task.CancelledAtMS = nowMS
	task.NextAttemptAtMS = 0
	task.LastError = strings.TrimSpace(reason)
	if err := s.store.UpdateSupplyPurchaseTask(ctx, *task); err != nil {
		return err
	}
	if err := s.cancelReversiblePurchaseTaskOrders(ctx, task.TaskID, nowMS); err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(reason), lowPriceReserveInventoryUnavailable) {
		s.beginLowPriceReserveBackoff(
			lowPriceReserveExactQuoteBackoff,
			errors.New(lowPriceReserveInventoryUnavailable),
			lowPriceReserveRetryBackoff,
		)
	}
	s.invalidateStatusCache()
	return nil
}

func (s *Service) recordPurchaseTaskError(ctx context.Context, task *store.SupplyPurchaseTask, taskErr error) error {
	if task == nil {
		return taskErr
	}
	task.Status = purchaseTaskStatusRunning
	task.LastError = safeError(taskErr)
	now := time.Now()
	retryAtMS := supplierRetryAtMS(taskErr)
	if retryAtMS <= now.UnixMilli() {
		backoff := 3 * time.Second
		if errors.Is(taskErr, ErrInsufficientBalance) {
			backoff = 30 * time.Second
		} else if task.AttemptCount > 1 {
			backoff = minDuration(time.Minute, time.Duration(task.AttemptCount)*5*time.Second)
		}
		retryAtMS = now.Add(backoff).UnixMilli()
	}
	if task.Source == "automatic" && isLowPriceReserveTrigger(task.TriggerReason) && task.AttemptCount == 0 {
		retryAtMS = max(retryAtMS, now.Add(lowPriceReserveRetryBackoff).UnixMilli())
	}
	task.NextAttemptAtMS = retryAtMS
	if updateErr := s.store.UpdateSupplyPurchaseTask(ctx, *task); updateErr != nil {
		return updateErr
	}
	return taskErr
}

func (s *Service) reconcilePurchaseTask(ctx context.Context, task store.SupplyPurchaseTask) (store.SupplyPurchaseTask, error) {
	orders, err := s.store.ListSupplyOrdersByTaskID(ctx, task.TaskID)
	if err != nil {
		return store.SupplyPurchaseTask{}, err
	}
	stats := summarizePurchaseTaskOrders(orders)
	durableChanged := task.FulfilledQuantity != stats.fulfilled
	task.FulfilledQuantity = stats.fulfilled
	task.OrderCount = stats.orderCount
	task.ActiveOrderCount = stats.activeOrderCount
	if task.FulfilledQuantity >= task.TargetQuantity &&
		task.Status != purchaseTaskStatusCompleted && task.Status != purchaseTaskStatusCancelled {
		task.Status = purchaseTaskStatusCompleted
		task.CompletedAtMS = time.Now().UnixMilli()
		task.NextAttemptAtMS = 0
		task.LastError = ""
		durableChanged = true
	} else if task.AttemptCount > 0 && task.Status == purchaseTaskStatusPending {
		task.Status = purchaseTaskStatusRunning
		durableChanged = true
	}
	// OrderCount and ActiveOrderCount are derived UI fields and are not stored;
	// persist only durable state changes.
	if durableChanged {
		if err := s.store.UpdateSupplyPurchaseTask(ctx, task); err != nil {
			return store.SupplyPurchaseTask{}, err
		}
	}
	return task, nil
}

func summarizePurchaseTaskOrders(orders []store.SupplyOrder) purchaseTaskOrderStats {
	stats := purchaseTaskOrderStats{orderCount: len(orders)}
	for _, order := range orders {
		delivered := purchaseTaskOrderDeliveredQuantity(order)
		stats.fulfilled += delivered
		if !reportOpenOrderStatus(order.Status) {
			continue
		}
		staleReservation := purchaseTaskOrderReservationStale(order, time.Now())
		if !staleReservation {
			stats.activeOrderCount++
		}
		committed := purchaseTaskOrderCommittedQuantity(order)
		if !staleReservation && committed > delivered {
			stats.committedPending += committed - delivered
		}
		reserved := purchaseTaskOrderReservedQuantity(order)
		if staleReservation {
			reserved = delivered
		}
		if reserved > delivered {
			stats.reservedPending += reserved - delivered
		}
	}
	return stats
}

func purchaseTaskOrdersRequireOperatorReview(orders []store.SupplyOrder) bool {
	for _, order := range orders {
		if purchaseTaskOrderRequiresOperatorReview(order) {
			return true
		}
	}
	return false
}

func purchaseTaskOrderRequiresOperatorReview(order store.SupplyOrder) bool {
	remoteStatus := strings.ToLower(strings.TrimSpace(order.RemoteStatus))
	if remoteStatus == "paid_delivery_unparsed" {
		return true
	}
	return strings.ToLower(strings.TrimSpace(order.Status)) == "failed" && supplyOrderHasPaymentEvidence(order)
}

func purchaseTaskOrderReservationStale(order store.SupplyOrder, now time.Time) bool {
	if !order.Automatic || !reportOpenOrderStatus(order.Status) ||
		order.ImportedCount > 0 || order.ItemCount > 0 || order.ReadyQuantity > 0 {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(order.Status))
	if status != "created" && status != "waiting_inventory" {
		return false
	}
	if order.CreatedAtMS <= 0 || now.IsZero() {
		return false
	}
	return now.Sub(time.UnixMilli(order.CreatedAtMS)) >= purchaseTaskStaleAutomaticWaitingAge
}

func purchaseTaskOrderDeliveredQuantity(order store.SupplyOrder) int {
	// A purchase task promises usable CPA accounts, not merely payloads returned
	// by the supplier. Failed or still-pending imports must remain in the task's
	// remaining quantity so the worker continues purchasing replacements.
	return max(0, order.ImportedCount)
}

func purchaseTaskOrderCommittedQuantity(order store.SupplyOrder) int {
	if order.ImportedCount > 0 {
		return order.ImportedCount
	}
	if order.ItemCount > 0 {
		return order.ItemCount
	}
	status := strings.ToLower(strings.TrimSpace(order.Status))
	if status == "ready" && order.ReadyQuantity > 0 {
		return order.ReadyQuantity
	}
	if status == "taking" || status == "importing" || status == "partial" || order.ChargedFen > 0 {
		return max(order.ReadyQuantity, order.RequestedQuantity)
	}
	return 0
}

// purchaseTaskOrderReservedQuantity counts the quantity already assigned to an
// active child order. Waiting/creating orders reserve their requested quantity
// for task sizing, while a ready order uses the quantity the supplier actually
// secured so a partial fill can still enqueue only the uncovered remainder.
func purchaseTaskOrderReservedQuantity(order store.SupplyOrder) int {
	delivered := max(0, order.ImportedCount)
	status := strings.ToLower(strings.TrimSpace(order.Status))
	switch status {
	case "creating", "create_uncertain", "created", "waiting_inventory":
		return max(delivered, max(order.RequestedQuantity, max(order.ReadyQuantity, order.ItemCount)))
	case "ready":
		actual := max(order.ReadyQuantity, order.ItemCount)
		if actual <= 0 {
			actual = order.RequestedQuantity
		}
		return max(delivered, actual)
	case "taking", "importing", "partial", "recovery_importing", "recovery_partial":
		return max(delivered, max(order.RequestedQuantity, max(order.ReadyQuantity, order.ItemCount)))
	default:
		return delivered
	}
}

// purchaseTaskReadyTakeAllowed keeps the executor moving when quota inspection
// evidence is temporarily stale. The task target was persisted before the
// supplier order was created, so a ready child order that still fits the
// remaining target can be taken without consulting the stale capacity planner.
// A small overage allowance matches the ready-order combination policy and
// absorbs live quantity drift while the supplier is preparing stock.
func (s *Service) purchaseTaskReadyTakeAllowed(ctx context.Context, order store.SupplyOrder) (bool, error) {
	if s == nil || s.store == nil || strings.TrimSpace(order.TaskID) == "" ||
		!isReadyForTake(order.Status) && !isReadyForTake(order.RemoteStatus) {
		return false, nil
	}
	task, found, err := s.store.GetSupplyPurchaseTask(ctx, order.TaskID)
	if err != nil || !found {
		return false, err
	}
	task, err = s.reconcilePurchaseTask(ctx, task)
	if err != nil {
		return false, err
	}
	if task.Status != purchaseTaskStatusPending && task.Status != purchaseTaskStatusRunning {
		return false, nil
	}
	remaining := max(0, task.TargetQuantity-task.FulfilledQuantity)
	if remaining <= 0 {
		return false, nil
	}
	orders, err := s.store.ListSupplyOrdersByTaskID(ctx, task.TaskID)
	if err != nil {
		return false, err
	}
	// Reserve the earlier ready/taking siblings first. ListByTaskID is ordered by
	// creation time and ID, so concurrent child orders receive one deterministic
	// shared budget instead of each independently consuming the whole remainder.
	for _, sibling := range orders {
		if sibling.OrderID == order.OrderID || sibling.ID == order.ID && order.ID > 0 {
			break
		}
		if !reportOpenOrderStatus(sibling.Status) {
			continue
		}
		committed := purchaseTaskOrderCommittedQuantity(sibling) - purchaseTaskOrderDeliveredQuantity(sibling)
		remaining = max(0, remaining-max(0, committed))
	}
	if remaining <= 0 {
		return false, nil
	}
	quantity := max(order.ReadyQuantity, order.ItemCount)
	if quantity <= 0 {
		quantity = max(0, order.RequestedQuantity)
	}
	if quantity <= 0 {
		return false, nil
	}
	allowance := max(1, int(math.Ceil(float64(remaining)*0.15)))
	return quantity <= remaining+allowance, nil
}

func (s *Service) purchaseTaskOrderPollDue(cfg store.ManagerSupplyConfig, order store.SupplyOrder, nowMS int64) bool {
	if order.SupplierRetryUntilMS > nowMS {
		return false
	}
	// A ready order with payment/delivery evidence is already committed
	// supplier inventory. The dashboard's read-only status refresh may update
	// NextPollAtMS while the purchase worker is waiting; that must never strand
	// a paid delivery behind a moving local polling deadline.
	if supplyOrderHasPaymentEvidence(order) &&
		(isReadyForTake(order.Status) || isReadyForTake(order.RemoteStatus)) {
		return true
	}
	if order.Status == "taking" && order.NextPollAtMS > nowMS {
		return false
	}
	deadline := supplyOrderPollDeadline(order)
	if deadline <= 0 || deadline <= nowMS {
		return true
	}
	return s.emergencyOrderProcessingAllowed(cfg, order, s.currentSmartResource(cfg))
}

func (s *Service) stopPurchaseTaskOrderIfNeeded(ctx context.Context, order *store.SupplyOrder) (bool, error) {
	if order == nil || strings.TrimSpace(order.TaskID) == "" {
		return false, nil
	}
	task, found, err := s.store.GetSupplyPurchaseTask(ctx, order.TaskID)
	if err != nil || !found {
		return false, err
	}
	if purchaseTaskOrderReservationStale(*order, time.Now()) {
		order.Status = "released"
		order.RemoteStatus = remoteStatusAutomaticReleasePending
		order.LastError = "automatic waiting reservation expired locally; replacement will be scheduled"
		order.NextPollAtMS = 0
		order.SupplierRetryUntilMS = 0
		order.CompletedAtMS = time.Now().UnixMilli()
		return true, s.store.UpdateSupplyOrder(ctx, *order)
	}
	if task.Status != purchaseTaskStatusCompleted && task.Status != purchaseTaskStatusCancelled {
		return false, nil
	}
	if task.Status == purchaseTaskStatusCancelled {
		if !cancelReversiblePurchaseTaskOrder(order, time.Now().UnixMilli()) {
			return false, nil
		}
		return true, s.store.UpdateSupplyOrder(ctx, *order)
	}
	status := strings.ToLower(strings.TrimSpace(order.Status))
	if task.Source == "automatic" || order.Automatic {
		// Automatic reservations must reach the live-deficit decision in
		// autoReleaseAutomaticOrderIfNotNeeded. A task status can become completed
		// or cancelled from an older planner snapshot; treating that status as the
		// final decision would either discard newly scarce stock or take surplus
		// without checking the pool immediately before Take.
		return false, nil
	}
	if status == "taking" || status == "importing" || status == "partial" || order.ItemCount > order.ImportedCount || order.ChargedFen > 0 {
		return false, nil
	}
	if status == "creating" || status == "create_uncertain" {
		// The idempotent create must first be reconciled so an upstream order is
		// not orphaned. The recursive ready/waiting pass will close it locally.
		return false, nil
	}
	order.Status = "released"
	if task.Status == purchaseTaskStatusCancelled {
		order.RemoteStatus = "task_cancelled"
		order.LastError = "purchase task cancelled; supplier reservation left to expire"
	} else {
		order.RemoteStatus = "task_completed"
		order.LastError = "purchase task target completed; surplus supplier reservation left to expire"
	}
	order.NextPollAtMS = 0
	order.CompletedAtMS = time.Now().UnixMilli()
	return true, s.store.UpdateSupplyOrder(ctx, *order)
}

func (s *Service) reconcileAutomaticPurchaseTaskCancellation(ctx context.Context) error {
	task, found, err := s.store.GetActiveAutomaticSupplyPurchaseTask(ctx)
	if err != nil || !found {
		return err
	}
	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return err
	}
	if !managerconfigsvc.SupplyEnabled(cfg.Supply) {
		_, _, err = s.cancelPurchaseTaskAndChildren(ctx, task.TaskID, time.Now().UnixMilli())
		return err
	}
	// Give a freshly planned task enough time for its first worker step. This
	// avoids interpreting the pre-order snapshot used to create it as a later
	// cancellation decision.
	if time.Since(time.UnixMilli(task.CreatedAtMS)) < 10*time.Second {
		return nil
	}
	orders, err := s.store.ListSupplyOrdersByTaskID(ctx, task.TaskID)
	if err != nil {
		return err
	}
	if summarizePurchaseTaskOrders(orders).activeOrderCount > 0 {
		// The smart snapshot counts ready/in-flight child orders as prelocked
		// capacity. Let processOrder apply the live-deficit overage budget before
		// cancelling the task; otherwise the order can cancel its own shortage and
		// be released at the exact moment supplier stock becomes available.
		return nil
	}
	resource := s.currentSmartResource(cfg.Supply)
	shouldCancel := resource.Enabled && resource.SnapshotFresh && !smartResourceEmergency(resource) &&
		resource.CapacityGapRCU <= 0 && resource.AccountQuantityDeficit <= 0 && resource.SuggestedQuantity <= 0
	if !resource.Enabled {
		s.stateMu.RLock()
		overview := s.overview
		s.stateMu.RUnlock()
		shouldCancel = overview.CPATarget > 0 && overview.CPAAvailable >= overview.CPATarget
	}
	if shouldCancel {
		if isLowPriceReserveTrigger(task.TriggerReason) && supplyLowPriceReserveEnabled(cfg.Supply) {
			available, countErr := s.countLowPriceReserveAccounts(ctx, cfg)
			if countErr != nil {
				return countErr
			}
			shouldCancel = available >= cfg.Supply.LowPriceReserveTargetAccounts
		}
	}
	if shouldCancel {
		_, _, err = s.cancelPurchaseTaskAndChildren(ctx, task.TaskID, time.Now().UnixMilli())
	}
	return err
}

func (s *Service) stopOrdinaryAutomaticTaskOnRecoveredTiming(
	ctx context.Context,
	cfg store.ManagerSupplyConfig,
	task *store.SupplyPurchaseTask,
	activeTaskOrders int,
) (bool, error) {
	if s == nil || s.store == nil || task == nil || task.Source != "automatic" ||
		isLowPriceReserveTrigger(task.TriggerReason) {
		return false, nil
	}
	resource := s.currentSmartResource(cfg)
	if !smartResourceOrdinaryPurchaseTimingWait(resource) {
		return false, nil
	}

	now := time.Now()
	if activeTaskOrders > 0 {
		// Existing reservations retain their normal lifecycle, but this worker
		// must not fill another slot from an obsolete shortage. Delay the intent
		// until the next automatic capacity cycle has had time to reconcile it.
		delay := time.Duration(max(3, cfg.CheckIntervalSeconds)) * time.Second
		task.NextAttemptAtMS = now.Add(delay).UnixMilli()
		if err := s.store.UpdateSupplyPurchaseTask(ctx, *task); err != nil {
			return true, err
		}
		s.invalidateStatusCache()
		return true, nil
	}

	// With no child reservation there is nothing supplier-side to release. End
	// the stale intent without recording a normal timing wait as an error.
	task.Status = purchaseTaskStatusCancelled
	task.CancelledAtMS = now.UnixMilli()
	task.NextAttemptAtMS = 0
	task.LastError = ""
	if err := s.store.UpdateSupplyPurchaseTask(ctx, *task); err != nil {
		return true, err
	}
	s.invalidateStatusCache()
	return true, nil
}

func smartResourceOrdinaryPurchaseTimingWait(resource SmartResource) bool {
	if !resource.Enabled || !resource.SnapshotFresh || smartResourceEmergency(resource) {
		return false
	}
	switch strings.TrimSpace(resource.DecisionReason) {
	case "purchase_timing_wait", "supply_lifetime_capacity_wait":
		return true
	}
	return resource.SuggestedQuantity <= 0 && resource.PurchaseTimingEligibleQuantity <= 0 &&
		(resource.PurchaseTimingWaitMinutes > 0 || resource.PurchaseLifetimeLimited)
}

func (s *Service) signalPurchaseTaskWorker() {
	if s == nil || s.purchaseTaskWake == nil {
		return
	}
	select {
	case s.purchaseTaskWake <- struct{}{}:
	default:
	}
}

func (s *Service) PurchaseTaskWake() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.purchaseTaskWake
}

func (s *Service) NextPurchaseTaskInterval(ctx context.Context) time.Duration {
	base := 3 * time.Second
	nowMS := time.Now().UnixMilli()
	if orders, err := s.store.ListOpenSupplyOrders(ctx, maxTrackedOpenSupplyOrders); err == nil {
		for _, order := range orders {
			if strings.TrimSpace(order.TaskID) == "" {
				continue
			}
			if supplyOrderHasPaymentEvidence(order) &&
				(isReadyForTake(order.Status) || isReadyForTake(order.RemoteStatus)) {
				return time.Second
			}
			deadline := order.SupplierRetryUntilMS
			if deadline <= 0 {
				deadline = supplyOrderPollDeadline(order)
			}
			if deadline <= nowMS {
				return time.Second
			}
			if wait := time.Until(time.UnixMilli(deadline)); wait > 0 && wait < base {
				base = wait
			}
		}
	}
	if tasks, err := s.store.ListActiveSupplyPurchaseTasks(ctx, 20); err == nil {
		for _, task := range tasks {
			if task.NextAttemptAtMS <= nowMS {
				return time.Second
			}
			if wait := time.Until(time.UnixMilli(task.NextAttemptAtMS)); wait > 0 && wait < base {
				base = wait
			}
		}
	}
	if base < time.Second {
		return time.Second
	}
	return base
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}
