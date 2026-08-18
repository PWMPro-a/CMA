package supply

import (
	"context"
	"errors"
	"fmt"
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
	orderCount       int
	activeOrderCount int
}

func (s *Service) createManualPurchaseTask(ctx context.Context, quantity int, supplierID string) (store.SupplyPurchaseTask, error) {
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
	return s.store.CreateSupplyPurchaseTask(ctx, store.SupplyPurchaseTask{
		TaskID:              "purchase-" + uuid.NewString(),
		Source:              "manual",
		SupplierID:          platform.ID,
		Product:             platform.Product,
		TargetQuantity:      quantity,
		Status:              purchaseTaskStatusPending,
		Strategy:            supplyOrderStrategy(cfg.Supply, false),
		TriggerReason:       "manual",
		MaxConcurrentOrders: 1,
	})
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
		planned.TaskID = "purchase-" + uuid.NewString()
		planned.Source = "automatic"
		planned.SupplierID = ""
		if planned.Status == "" {
			planned.Status = purchaseTaskStatusPending
		}
		return s.store.CreateSupplyPurchaseTask(ctx, planned)
	}
	existing, err = s.reconcilePurchaseTask(ctx, existing)
	if err != nil {
		return store.SupplyPurchaseTask{}, err
	}
	if existing.Status != purchaseTaskStatusPending && existing.Status != purchaseTaskStatusRunning {
		planned.TaskID = "purchase-" + uuid.NewString()
		planned.Source = "automatic"
		planned.SupplierID = ""
		return s.store.CreateSupplyPurchaseTask(ctx, planned)
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
	return existing, nil
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
	_, _, err := s.store.CancelSupplyPurchaseTask(ctx, taskID, time.Now().UnixMilli())
	if err != nil {
		return Status{}, err
	}
	if _, found, getErr := s.store.GetSupplyPurchaseTask(ctx, taskID); getErr != nil {
		return Status{}, getErr
	} else if !found {
		return Status{}, ErrPurchaseTaskNotFound
	}
	s.signalPurchaseTaskWorker()
	return s.GetStatus(ctx, 50)
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
	// Supplier lifecycle reconciliation has priority over creating another
	// reservation. This keeps retries idempotent and makes cancellation/completion
	// close surplus child orders before a new attempt is admitted.
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
		return processErr
	}

	tasks, err := s.store.ListActiveSupplyPurchaseTasks(ctx, 20)
	if err != nil {
		return err
	}
	for _, candidate := range tasks {
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
		if len(openOrders) >= maxConcurrentSupplyOrders(cfg.Supply) {
			continue
		}
		remaining := task.TargetQuantity - stats.fulfilled - stats.committedPending
		if remaining <= 0 {
			continue
		}
		quantity := min(100, remaining)
		return s.createPurchaseTaskOrder(ctx, cfg, &task, quantity, openOrders, stats.activeOrderCount)
	}
	return nil
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
	requestedSupplierID := ""
	if task.Source == "manual" {
		requestedSupplierID = task.SupplierID
	}
	selection, err := s.selectSupplyPlatform(ctx, cfg.Supply, quantity, openOrders, requestedSupplierID)
	if err != nil {
		return s.recordPurchaseTaskError(ctx, task, err)
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

	triggerReason := strings.TrimSpace(task.TriggerReason)
	if triggerReason == "" {
		triggerReason = task.Source
	}
	if activeTaskOrders > 0 {
		triggerReason = parallelSupplyTriggerReason(triggerReason)
	}
	attempt := store.SupplyOrder{
		OrderID:           newCreateAttemptID(),
		TaskID:            task.TaskID,
		SupplierID:        platform.ID,
		Product:           platform.Product,
		RequestedQuantity: quantity,
		Automatic:         task.Source == "automatic",
		Strategy:          task.Strategy,
		TriggerReason:     triggerReason,
		Status:            "creating",
	}
	attempt, err = s.store.CreateSupplyOrder(ctx, attempt)
	if err != nil {
		return s.recordPurchaseTaskError(ctx, task, err)
	}
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

	remote, err := s.supplyClient.CreateOrder(ctx, supplyPlatformCredentials(platform), platform.Product, quantity, attempt.OrderID)
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
		stats.activeOrderCount++
		committed := purchaseTaskOrderCommittedQuantity(order)
		if committed > delivered {
			stats.committedPending += committed - delivered
		}
	}
	return stats
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

func (s *Service) purchaseTaskOrderPollDue(cfg store.ManagerSupplyConfig, order store.SupplyOrder, nowMS int64) bool {
	if order.SupplierRetryUntilMS > nowMS {
		return false
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
	if task.Status != purchaseTaskStatusCompleted && task.Status != purchaseTaskStatusCancelled {
		return false, nil
	}
	status := strings.ToLower(strings.TrimSpace(order.Status))
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
		_, _, err = s.store.CancelSupplyPurchaseTask(ctx, task.TaskID, time.Now().UnixMilli())
		return err
	}
	// Give a freshly planned task enough time for its first worker step. This
	// avoids interpreting the pre-order snapshot used to create it as a later
	// cancellation decision.
	if time.Since(time.UnixMilli(task.CreatedAtMS)) < 10*time.Second {
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
		_, _, err = s.store.CancelSupplyPurchaseTask(ctx, task.TaskID, time.Now().UnixMilli())
	}
	return err
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
