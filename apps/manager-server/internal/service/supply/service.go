package supply

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpaauthfiles"
	managerconfigsvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/supplyclient"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

var (
	ErrNotConfigured               = errors.New("account supply is not configured")
	ErrOrderInProgress             = errors.New("a supply order is already in progress")
	ErrInvalidQuantity             = errors.New("replenishment quantity must be between 1 and 100")
	ErrInsufficientBalance         = errors.New("supply account available balance is insufficient")
	ErrCreateUncertain             = errors.New("supply order creation result is uncertain")
	ErrOrderNotFound               = errors.New("supply order was not found")
	ErrNotCreateUncertain          = errors.New("supply order is not waiting for create-result confirmation")
	ErrCapacitySnapshotUnavailable = errors.New("quota inspection snapshot is unavailable")
)

const (
	// The supplier has no cancel/release endpoint. This local marker closes an
	// automatic reservation that is no longer needed; the supplier releases it
	// by its own order-expiry policy without any outbound cancellation request.
	remoteStatusAutomaticReleasePending = "auto_release_pending"
	automaticReleasePendingMessage      = "automatically released locally; supplier reservation will expire automatically"
)

type Overview struct {
	CheckedAtMS  int64                   `json:"checkedAtMs,omitempty"`
	CPAAvailable int                     `json:"cpaAvailable"`
	CPATarget    int                     `json:"cpaTarget"`
	CPADeficit   int                     `json:"cpaDeficit"`
	Inventory    *supplyclient.Inventory `json:"inventory,omitempty"`
	Balance      *supplyclient.Balance   `json:"balance,omitempty"`
	LastError    string                  `json:"lastError,omitempty"`
}

type Status struct {
	Config        store.ManagerSupplyConfig `json:"config"`
	Running       bool                      `json:"running"`
	Overview      Overview                  `json:"overview"`
	SmartResource SmartResource             `json:"smartResource"`
	Automation    AutomationExecution       `json:"automation"`
	ActiveOrder   *store.SupplyOrder        `json:"activeOrder,omitempty"`
	Orders        []store.SupplyOrder       `json:"orders"`
}

// AutomationExecution is the in-memory execution timeline for the automatic
// replenishment worker. It lets the management page show the exact next worker
// wake-up rather than guessing from a configured interval. It is intentionally
// not persisted: a process restart creates a new schedule immediately.
type AutomationExecution struct {
	Enabled           bool   `json:"enabled"`
	Running           bool   `json:"running"`
	NextExecutionAtMS int64  `json:"nextExecutionAtMs,omitempty"`
	IntervalSeconds   int    `json:"intervalSeconds,omitempty"`
	LastStartedAtMS   int64  `json:"lastStartedAtMs,omitempty"`
	LastFinishedAtMS  int64  `json:"lastFinishedAtMs,omitempty"`
	LastResult        string `json:"lastResult,omitempty"`
	LastAction        string `json:"lastAction,omitempty"`
	LastReason        string `json:"lastReason,omitempty"`
	LastError         string `json:"lastError,omitempty"`
}

type Service struct {
	store         *store.Store
	managerConfig *managerconfigsvc.Service
	supplyClient  *supplyclient.Client
	authFiles     *cpaauthfiles.Client

	runMu    sync.Mutex
	stateMu  sync.RWMutex
	running  bool
	overview Overview
	// overviewRefreshMu makes cold-start status hydration single-flight. The
	// management page polls every ten seconds, so every concurrent first-page
	// request must not independently log in to the supplier after a restart.
	overviewRefreshMu sync.Mutex

	smartMu            sync.RWMutex
	smartBuckets       map[int64]*smartUsageBucket
	authCacheMu        sync.Mutex
	authRefreshMu      sync.Mutex
	authCache          authFileSnapshot
	quotaSnapshotMu    sync.Mutex
	quotaRefreshMu     sync.Mutex
	quotaSnapshot      inspectionQuotaSnapshot
	smartResourceState SmartResource
	automation         AutomationExecution

	criticalConfirmMu       sync.Mutex
	criticalConfirmRounds   map[string]int
	lastAutomaticCreateAtMS int64
}

func New(st *store.Store, managerConfig *managerconfigsvc.Service, httpClient ...*http.Client) *Service {
	var client *http.Client
	if len(httpClient) > 0 {
		client = httpClient[0]
	}
	return &Service{
		store:                 st,
		managerConfig:         managerConfig,
		supplyClient:          supplyclient.New(client),
		authFiles:             cpaauthfiles.New(client),
		smartBuckets:          make(map[int64]*smartUsageBucket),
		criticalConfirmRounds: make(map[string]int),
	}
}

func (s *Service) GetStatus(ctx context.Context, limit int) (Status, error) {
	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return Status{}, err
	}
	resource := s.currentSmartResource(cfg.Supply)
	if resource.Enabled && !resource.SnapshotFresh {
		// Automatic replenishment may be disabled while operators still need a
		// current capacity view. Refresh the cold snapshot without placing orders.
		refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		refreshed, refreshErr := s.smartResource(refreshCtx, cfg, false)
		cancel()
		if refreshErr == nil {
			resource = refreshed
		} else {
			resource = s.currentSmartResource(cfg.Supply)
		}
	}
	// Overview used to live only in process memory. Recreating the Manager
	// therefore made inventory and balance look empty until a later automation
	// branch happened to refresh them; an open order can keep that branch from
	// running for a long time. Hydrate the read-only supplier snapshot once on
	// cold start so the dashboard keeps its operational data after deployment.
	s.hydrateOverviewIfNeeded(ctx, cfg.Supply)
	orders, err := s.store.ListSupplyOrders(ctx, limit)
	if err != nil {
		return Status{}, err
	}
	active, found, err := s.store.GetOpenSupplyOrder(ctx)
	if err != nil {
		return Status{}, err
	}
	s.stateMu.RLock()
	overview := s.overview
	running := s.running
	s.stateMu.RUnlock()
	overview.CPATarget = cfg.Supply.TargetAvailableAccounts
	if overview.CPATarget > overview.CPAAvailable {
		overview.CPADeficit = overview.CPATarget - overview.CPAAvailable
	} else {
		overview.CPADeficit = 0
	}
	status := Status{
		Config:        sanitizeConfig(cfg.Supply),
		Running:       running,
		Overview:      overview,
		SmartResource: resource,
		Automation:    s.currentAutomationExecution(managerconfigsvc.SupplyEnabled(cfg.Supply)),
		Orders:        orders,
	}
	if found {
		status.ActiveOrder = &active
	}
	return status, nil
}

func (s *Service) UpdateConfig(ctx context.Context, config store.ManagerSupplyConfig) (Status, error) {
	if _, err := s.managerConfig.UpdateSupply(ctx, config); err != nil {
		return Status{}, err
	}
	return s.GetStatus(ctx, 50)
}

func (s *Service) Check(ctx context.Context) (Status, error) {
	if err := s.run(ctx, false, 0, true); err != nil {
		s.recordError(err)
		return Status{}, err
	}
	return s.GetStatus(ctx, 50)
}

func (s *Service) Replenish(ctx context.Context, quantity int) (Status, error) {
	if quantity <= 0 || quantity > 100 {
		return Status{}, ErrInvalidQuantity
	}
	if err := s.run(ctx, true, quantity, true); err != nil {
		s.recordError(err)
		return Status{}, err
	}
	return s.GetStatus(ctx, 50)
}

func (s *Service) DismissCreateUncertain(ctx context.Context, orderID string) (Status, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	order, found, err := s.store.GetSupplyOrder(ctx, strings.TrimSpace(orderID))
	if err != nil {
		return Status{}, err
	}
	if !found {
		return Status{}, ErrOrderNotFound
	}
	if order.Status != "create_uncertain" {
		return Status{}, ErrNotCreateUncertain
	}
	order.Status = "dismissed"
	order.CompletedAtMS = time.Now().UnixMilli()
	order.NextPollAtMS = 0
	order.LastError = "create-result block dismissed after remote order verification"
	if err := s.store.UpdateSupplyOrder(ctx, order); err != nil {
		return Status{}, err
	}
	return s.GetStatus(ctx, 50)
}

func (s *Service) RunAutomatic(ctx context.Context) error {
	err := s.run(ctx, true, 0, false)
	if err != nil {
		s.recordError(err)
	}
	return err
}

func (s *Service) NextInterval(ctx context.Context) time.Duration {
	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return 30 * time.Second
	}
	if order, found, err := s.store.GetOpenSupplyOrder(ctx); err == nil && found {
		if order.Status == "creating" || order.Status == "create_uncertain" {
			return time.Minute
		}
		if wait := time.Until(time.UnixMilli(order.NextPollAtMS)); wait > 0 {
			if wait > time.Minute {
				return time.Minute
			}
			return wait
		}
		seconds := cfg.Supply.PollIntervalSeconds
		if seconds <= 0 {
			seconds = 3
		}
		return time.Duration(seconds) * time.Second
	}
	if !managerconfigsvc.SupplyEnabled(cfg.Supply) {
		return time.Minute
	}
	resource := s.currentSmartResource(cfg.Supply)
	seconds := smartAutomaticCheckIntervalSeconds(cfg.Supply, resource)
	return time.Duration(seconds) * time.Second
}

func (s *Service) run(ctx context.Context, allowCreate bool, manualQuantity int, force bool) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	s.setRunning(true)
	defer s.setRunning(false)

	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return err
	}
	supplyCfg := cfg.Supply
	if restored, restoredFound, err := s.store.ActivateNextUnsupportedSupplyRelease(ctx); err != nil {
		return err
	} else if restoredFound {
		return s.processOrder(ctx, cfg, restored)
	}
	active, found, err := s.store.GetOpenSupplyOrder(ctx)
	if err != nil {
		return err
	}
	if found {
		if manualQuantity > 0 {
			return ErrOrderInProgress
		}
		switch active.Status {
		case "creating", "create_uncertain", "importing", "partial":
		default:
			if err := s.requireCredentials(supplyCfg); err != nil {
				return err
			}
		}
		if !force && active.NextPollAtMS > time.Now().UnixMilli() {
			return nil
		}
		return s.processOrder(ctx, cfg, active)
	}
	if repaired, repairedFound, err := s.store.ActivateNextLegacySupplyRepair(ctx); err != nil {
		return err
	} else if repairedFound {
		return s.processOrder(ctx, cfg, repaired)
	}
	if allowCreate && manualQuantity == 0 && !managerconfigsvc.SupplyEnabled(supplyCfg) {
		return nil
	}
	if err := s.requireCredentials(supplyCfg); err != nil {
		return err
	}
	if !allowCreate {
		return s.refreshOverview(ctx, cfg, supplyCfg.ReplenishBatchSize)
	}

	available := 0
	var resource SmartResource
	useSmart := manualQuantity == 0 && smartSupplyEnabled(supplyCfg)
	if useSmart {
		resource, err = s.smartResource(ctx, cfg, force)
		if err != nil {
			return err
		}
	} else {
		available, err = s.countAvailableAccounts(ctx, cfg)
		if err != nil {
			return err
		}
	}
	if manualQuantity == 0 {
		if recent, recentFound, err := s.store.GetLatestCompletedAutomaticSupplyOrder(ctx); err != nil {
			return err
		} else if recentFound && time.Since(time.UnixMilli(recent.CompletedAtMS)) < automaticSettleWindow(supplyCfg) {
			s.updateCPAOverview(available, supplyCfg.TargetAvailableAccounts)
			return nil
		}
	}
	quantity := manualQuantity
	if quantity == 0 {
		if useSmart {
			if !resource.SnapshotFresh && !smartPartialInspectionCapacityDeficitAllowed(resource) {
				return nil
			}
			if resource.DecisionReason == "usage_rate_not_ready" || resource.ConsumeRCUPerMinute <= 0 {
				return s.refreshSupplyOverview(ctx, supplyCfg, available, max(1, supplyCfg.ReplenishBatchSize))
			}
			if resource.CapacityGapRCU <= 0 {
				return s.refreshSupplyOverview(ctx, supplyCfg, available, max(1, supplyCfg.ReplenishBatchSize))
			}
			if resource.SuggestedAction == smartActionHealthy || resource.HealthLevel == smartHealthHealthy {
				return s.refreshSupplyOverview(ctx, supplyCfg, available, max(1, supplyCfg.ReplenishBatchSize))
			}
			if s.automaticCreateCooldownActive(supplyCfg, resource) {
				s.updateCPAOverview(available, supplyCfg.TargetAvailableAccounts)
				return nil
			}
			quantity = s.smartSuggestedCreateQuantity(supplyCfg, resource)
		} else {
			deficit := supplyCfg.TargetAvailableAccounts - available
			if deficit <= 0 {
				return s.refreshSupplyOverview(ctx, supplyCfg, available, max(1, supplyCfg.ReplenishBatchSize))
			}
			quantity = min(deficit, supplyCfg.ReplenishBatchSize)
		}
	}
	if quantity <= 0 {
		if useSmart {
			return s.refreshSupplyOverview(ctx, supplyCfg, available, max(1, supplyCfg.ReplenishBatchSize))
		}
		return ErrInvalidQuantity
	}
	if useSmart && supplyCfg.DailyMaxReplenishQuantity > 0 {
		remaining, err := s.remainingAutomaticDailyQuantity(ctx, supplyCfg)
		if err != nil {
			return err
		}
		if remaining <= 0 {
			resource.SuggestedAction = smartActionManualReview
			resource.DecisionReason = "daily_quantity_limit"
			s.setSmartResource(resource)
			s.updateCPAOverview(available, supplyCfg.TargetAvailableAccounts)
			return nil
		}
		if quantity > remaining {
			quantity = remaining
		}
	}

	inventory, balance, err := s.fetchSupplyOverview(ctx, supplyCfg, quantity)
	if err != nil {
		return err
	}
	if useSmart {
		pressure := s.smartSupplyPressure(ctx, supplyCfg, inventory, quantity)
		applySmartSupplyPressure(&resource, pressure)
		adjustedQuantity, pressureReason := smartPrelockQuantityForSupplyPressure(supplyCfg, resource, pressure, quantity)
		if adjustedQuantity > 0 && adjustedQuantity != quantity {
			quantity = adjustedQuantity
			inventory, balance, err = s.fetchSupplyOverview(ctx, supplyCfg, quantity)
			if err != nil {
				return err
			}
			pressure = s.smartSupplyPressure(ctx, supplyCfg, inventory, quantity)
			applySmartSupplyPressure(&resource, pressure)
		}
		if pressureReason != "" {
			resource.DecisionReason = pressureReason
		}
	}
	s.setOverview(Overview{
		CheckedAtMS:  time.Now().UnixMilli(),
		CPAAvailable: available,
		CPATarget:    supplyCfg.TargetAvailableAccounts,
		CPADeficit:   max(0, supplyCfg.TargetAvailableAccounts-available),
		Inventory:    &inventory,
		Balance:      &balance,
	})
	if inventory.EstimatedTotalFen > 0 && balance.AvailableFen < inventory.EstimatedTotalFen {
		if useSmart {
			resource.SuggestedAction = smartActionBalanceBlocked
			resource.DecisionReason = "balance_insufficient"
			s.setSmartResource(resource)
		}
		return ErrInsufficientBalance
	}
	if useSmart {
		if supplyCfg.MinBalanceReserveFen > 0 && inventory.EstimatedTotalFen > 0 && balance.AvailableFen-inventory.EstimatedTotalFen < supplyCfg.MinBalanceReserveFen {
			resource.SuggestedAction = smartActionBalanceBlocked
			resource.DecisionReason = "balance_reserve_protected"
			s.setSmartResource(resource)
			return ErrInsufficientBalance
		}
		if supplyCfg.DailyMaxHoldFen > 0 && inventory.EstimatedTotalFen > 0 && balance.HeldFen+inventory.EstimatedTotalFen > supplyCfg.DailyMaxHoldFen {
			resource.SuggestedAction = smartActionManualReview
			resource.DecisionReason = "daily_hold_limit"
			s.setSmartResource(resource)
			return nil
		}
		if inventory.Available <= 0 && !inventory.NeedsProduction && resource.HealthLevel != smartHealthCritical {
			resource.SuggestedAction = smartActionInventoryBlocked
			resource.DecisionReason = "inventory_unavailable"
			s.setSmartResource(resource)
			return nil
		}
		resource.SuggestedQuantity = quantity
		resource.PrelockedCapacityRCU = estimatedSupplyOrderCapacityRCU(supplyCfg, quantity)
		s.setSmartResource(resource)
	}

	attempt := store.SupplyOrder{
		OrderID:           newCreateAttemptID(),
		Product:           supplyCfg.Product,
		RequestedQuantity: quantity,
		Automatic:         manualQuantity == 0,
		Status:            "creating",
	}
	if _, err := s.store.CreateSupplyOrder(ctx, attempt); err != nil {
		return err
	}
	if attempt.Automatic {
		s.markAutomaticCreate()
	}

	credentials := credentialsFromConfig(supplyCfg)
	remote, err := s.supplyClient.CreateOrder(ctx, credentials, supplyCfg.Product, quantity)
	if err != nil {
		if isDefiniteCreateFailure(err) {
			attempt.Status = "failed"
			attempt.LastError = safeError(err)
			attempt.CompletedAtMS = time.Now().UnixMilli()
			if updateErr := s.store.UpdateSupplyOrder(ctx, attempt); updateErr != nil {
				return updateErr
			}
			return err
		}
		attempt.Status = "create_uncertain"
		attempt.LastError = safeError(err)
		if updateErr := s.store.UpdateSupplyOrder(ctx, attempt); updateErr != nil {
			return updateErr
		}
		return fmt.Errorf("%w: %v", ErrCreateUncertain, err)
	}
	order := store.SupplyOrder{
		OrderID:           remote.ID,
		Product:           supplyCfg.Product,
		RequestedQuantity: quantity,
		Automatic:         manualQuantity == 0,
		Status:            localOrderStatus(remote.Status),
		RemoteStatus:      remote.Status,
		ReadyQuantity:     remote.ReadyQuantity,
		Progress:          remote.Progress,
		StatusURL:         remote.StatusURL,
		TakeURL:           remote.TakeURL,
		ChargedFen:        remote.ChargedFen,
		ReleasedFen:       remote.ReleasedFen,
		NextPollAtMS:      nextPollAt(supplyCfg, remote.RetryAfterSeconds),
	}
	if err := s.store.PromoteSupplyCreateAttempt(ctx, attempt.OrderID, order); err != nil {
		attempt.Status = "create_uncertain"
		attempt.RemoteStatus = remote.Status
		attempt.StatusURL = remote.StatusURL
		attempt.TakeURL = remote.TakeURL
		attempt.LastError = safeError(fmt.Errorf("remote order %s was created but local persistence failed: %w", remote.ID, err))
		_ = s.store.UpdateSupplyOrder(ctx, attempt)
		return err
	}
	if order.Status == "ready" || order.Status == "taking" {
		return s.processOrder(ctx, cfg, order)
	}
	return nil
}

func (s *Service) processOrder(ctx context.Context, cfg store.ManagerConfig, order store.SupplyOrder) error {
	if order.Status == "taking" && order.NextPollAtMS > time.Now().UnixMilli() {
		return nil
	}
	// Keep the fact that a take call was already issued while reconciling an
	// expired lease. A timeout only means the client did not receive a response;
	// the supplier may still be preparing the same idempotent delivery.
	retryingTake := order.Status == "taking"
	if order.Status == "creating" {
		order.Status = "create_uncertain"
		order.LastError = "manager restarted while the create request was in progress"
		order.NextPollAtMS = 0
		return s.store.UpdateSupplyOrder(ctx, order)
	}
	if order.Status == "create_uncertain" {
		return nil
	}
	credentials := credentialsFromConfig(cfg.Supply)
	total, imported, err := s.store.SupplyImportCounts(ctx, order.OrderID)
	if err != nil {
		return err
	}
	if total > imported {
		return s.importItems(ctx, cfg, &order)
	}
	if released, err := s.autoReleaseAutomaticOrderIfNotNeeded(ctx, cfg, &order, true); released || err != nil {
		return err
	}
	remote, err := s.supplyClient.GetOrder(ctx, credentials, order.OrderID, order.StatusURL)
	if err != nil {
		if isHTTPStatus(err, http.StatusConflict) {
			return s.cancelOrder(ctx, &order, err)
		}
		return s.updateOrderError(ctx, &order, err, cfg.Supply)
	}
	applyRemoteOrder(&order, remote, cfg.Supply)
	if isTerminalRemoteStatus(remote.Status) && !isSuccessfulRemoteStatus(remote.Status) {
		order.Status = localOrderStatus(remote.Status)
		order.CompletedAtMS = time.Now().UnixMilli()
		return s.store.UpdateSupplyOrder(ctx, order)
	}
	if !isReadyForTake(remote.Status) {
		order.Status = "waiting_inventory"
		return s.store.UpdateSupplyOrder(ctx, order)
	}
	if released, err := s.autoReleaseAutomaticOrderIfNotNeeded(ctx, cfg, &order, true); released || err != nil {
		return err
	}
	if !retryingTake && order.Automatic && smartSupplyEnabled(cfg.Supply) && !s.smartTakeAllowed(cfg.Supply, order.OrderID) {
		resource := s.currentSmartResource(cfg.Supply)
		resource.LockedOrderID = order.OrderID
		resource.SuggestedAction = smartActionWaitLocked
		resource.DecisionReason = "critical_take_confirm_pending"
		resource.LockedConfirmRounds = s.currentCriticalConfirmRounds(order.OrderID)
		s.setSmartResource(resource)
		order.Status = "ready"
		order.NextPollAtMS = nextPollAt(cfg.Supply, 0)
		return s.store.UpdateSupplyOrder(ctx, order)
	}

	nowMS := time.Now().UnixMilli()
	leaseUntilMS := nowMS + int64(supplyTakeLeaseDuration(cfg.Supply)/time.Millisecond)
	claimed, err := s.store.ClaimSupplyOrderTaking(ctx, order.OrderID, nowMS, leaseUntilMS)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	order.Status = "taking"
	order.NextPollAtMS = leaseUntilMS
	order.LastError = ""
	taken, err := s.supplyClient.Take(ctx, credentials, order.OrderID, order.TakeURL)
	if err != nil {
		if isHTTPStatus(err, http.StatusConflict) {
			return s.cancelOrder(ctx, &order, err)
		}
		return s.updateOrderError(ctx, &order, err, cfg.Supply)
	}
	applyRemoteOrder(&order, taken.Order, cfg.Supply)
	if taken.Pending {
		order.Status = "waiting_inventory"
		return s.store.UpdateSupplyOrder(ctx, order)
	}
	normalized := make([]normalizedSupplyAccount, 0, len(taken.Accounts))
	for index, raw := range taken.Accounts {
		normalizedAccounts, err := normalizeAccountPayloads(raw)
		if err != nil {
			return s.updateOrderError(ctx, &order, fmt.Errorf("supply account %d format is unsupported: %w", index+1, err), cfg.Supply)
		}
		normalized = append(normalized, normalizedAccounts...)
	}
	// A supplier item describes delivery validity, whereas a returned payload may
	// be a Sub2API bundle that expands into several CPA files.  Apply the order
	// item leases only when that expansion is exactly one-to-one; otherwise keep
	// the payload lease (or the conservative one-hour default) instead of making
	// an unverifiable association.
	applySupplyOrderItemLeases(normalized, taken.ItemRemainingSeconds, time.Now())
	items := make([]store.SupplyImportItem, 0, len(normalized))
	seenItemKeys := make(map[string]struct{}, len(normalized))
	for _, account := range normalized {
		if _, duplicate := seenItemKeys[account.itemKey]; duplicate {
			continue
		}
		seenItemKeys[account.itemKey] = struct{}{}
		items = append(items, store.SupplyImportItem{
			OrderID:          order.OrderID,
			ItemKey:          account.itemKey,
			FileName:         account.fileName,
			PayloadJSON:      string(account.payload),
			LeaseExpiresAtMS: account.leaseExpiresAtMS,
		})
	}
	if len(items) == 0 {
		err := errors.New("supply take response did not include importable accounts")
		return s.updateOrderError(ctx, &order, err, cfg.Supply)
	}
	if _, err := s.store.InsertSupplyImportItems(ctx, order.OrderID, items); err != nil {
		return s.updateOrderError(ctx, &order, err, cfg.Supply)
	}
	s.resetCriticalConfirm(order.OrderID)
	order.Status = "importing"
	order.LastError = ""
	if err := s.store.UpdateSupplyOrder(ctx, order); err != nil {
		return err
	}
	return s.importItems(ctx, cfg, &order)
}

// autoReleaseAutomaticOrderIfNotNeeded finishes an automatic order locally
// when the verified pool no longer has a deficit. The supplier explicitly does
// not provide a cancellation/release API, so this function must never issue an
// HTTP request. The upstream reservation expires on the supplier's own timer.
func (s *Service) autoReleaseAutomaticOrderIfNotNeeded(ctx context.Context, cfg store.ManagerConfig, order *store.SupplyOrder, forceSmartRefresh bool) (bool, error) {
	if order == nil || !order.Automatic {
		return false, nil
	}
	switch order.Status {
	case "creating", "create_uncertain", "taking", "importing", "partial":
		return false, nil
	}
	if smartSupplyEnabled(cfg.Supply) {
		resource, err := s.smartResource(ctx, cfg, forceSmartRefresh)
		if err != nil || !resource.SnapshotFresh ||
			(resource.ConsumeRCUPerMinute <= 0 && resource.DemandTrend != smartDemandTrendFalling) {
			// A stale/unknown snapshot is not enough evidence to abandon a paid
			// reservation. Continue the normal status polling path instead.
			return false, nil
		}
		resource.LockedOrderID = order.OrderID
		resource.LockedOrderAgeSeconds = max(0, int(time.Since(time.UnixMilli(order.CreatedAtMS)).Seconds()))
		if resource.HealthLevel != smartHealthHealthy && resource.CapacityGapRCU > 0 {
			if resource.HealthLevel != smartHealthCritical {
				resource.SuggestedAction = smartActionTakeLocked
				resource.DecisionReason = "low_water_take_ready"
				s.setSmartResource(resource)
				return false, nil
			}
			rounds := s.incrementCriticalConfirm(order.OrderID)
			resource.LockedConfirmRounds = rounds
			if rounds < smartCriticalTakeConfirmRounds(cfg.Supply) {
				return true, s.waitLockedOrder(ctx, cfg.Supply, order, resource, smartActionWaitLocked, "critical_take_confirm_pending")
			}
			resource.SuggestedAction = smartActionTakeLocked
			resource.DecisionReason = "critical_take_confirmed"
			s.setSmartResource(resource)
			return false, nil
		}
		resource.SuggestedAction = smartActionReleaseLocked
		resource.DecisionReason = "capacity_recovered_before_take"
		s.setSmartResource(resource)
		return true, s.markAutomaticOrderReleasedLocally(ctx, order)
	}
	if cfg.Supply.TargetAvailableAccounts <= 0 {
		return false, nil
	}
	available, err := s.countAvailableAccounts(ctx, cfg)
	if err != nil {
		return false, err
	}
	s.updateCPAOverview(available, cfg.Supply.TargetAvailableAccounts)
	if available < cfg.Supply.TargetAvailableAccounts {
		return false, nil
	}
	return true, s.markAutomaticOrderReleasedLocally(ctx, order)
}

func (s *Service) markAutomaticOrderReleasedLocally(ctx context.Context, order *store.SupplyOrder) error {
	if order == nil {
		return nil
	}
	order.Status = "released"
	order.RemoteStatus = remoteStatusAutomaticReleasePending
	order.ItemCount = 0
	order.ImportedCount = 0
	order.NextPollAtMS = 0
	order.CompletedAtMS = time.Now().UnixMilli()
	order.LastError = automaticReleasePendingMessage
	s.resetCriticalConfirm(order.OrderID)
	return s.store.UpdateSupplyOrder(ctx, *order)
}

func (s *Service) importItems(ctx context.Context, cfg store.ManagerConfig, order *store.SupplyOrder) error {
	items, err := s.store.ListPendingSupplyImportItems(ctx, order.OrderID, time.Now().UnixMilli(), 100)
	if err != nil {
		return err
	}
	var firstErr error
	for _, item := range items {
		payload, _, fileName, err := normalizeAccountPayloadForImport(item.PayloadJSON)
		if err == nil && fileName != item.FileName {
			err = s.store.UpdateSupplyImportItemFileName(ctx, item.ID, fileName)
		}
		if err == nil {
			err = s.ensureCPAAccountImported(ctx, cfg, fileName, payload)
		}
		if err == nil {
			if markErr := s.store.MarkSupplyImportItemImported(ctx, item.ID, time.Now().UnixMilli()); markErr != nil {
				return markErr
			}
			continue
		}
		if firstErr == nil {
			firstErr = err
		}
		delay := retryDelay(item.AttemptCount + 1)
		if markErr := s.store.MarkSupplyImportItemFailed(ctx, item.ID, safeError(err), time.Now().Add(delay).UnixMilli()); markErr != nil {
			return markErr
		}
	}
	total, imported, err := s.store.SupplyImportCounts(ctx, order.OrderID)
	if err != nil {
		return err
	}
	order.ItemCount = total
	order.ImportedCount = imported
	if total > 0 && imported == total {
		order.Status = "completed"
		order.CompletedAtMS = time.Now().UnixMilli()
		order.NextPollAtMS = 0
		order.LastError = ""
	} else {
		order.Status = "partial"
		order.NextPollAtMS = time.Now().Add(retryDelay(1)).UnixMilli()
		if firstErr != nil {
			order.LastError = safeError(firstErr)
		}
	}
	if err := s.store.UpdateSupplyOrder(ctx, *order); err != nil {
		return err
	}
	return firstErr
}

func (s *Service) refreshOverview(ctx context.Context, cfg store.ManagerConfig, quantity int) error {
	available := 0
	if smartSupplyEnabled(cfg.Supply) {
		if _, err := s.smartResource(ctx, cfg, true); err != nil {
			return err
		}
	} else {
		var err error
		available, err = s.countAvailableAccounts(ctx, cfg)
		if err != nil {
			return err
		}
	}
	return s.refreshSupplyOverview(ctx, cfg.Supply, available, max(1, quantity))
}

func (s *Service) refreshSupplyOverview(ctx context.Context, cfg store.ManagerSupplyConfig, available int, quantity int) error {
	inventory, balance, err := s.fetchSupplyOverview(ctx, cfg, quantity)
	if err != nil {
		return err
	}
	s.setOverview(Overview{
		CheckedAtMS:  time.Now().UnixMilli(),
		CPAAvailable: available,
		CPATarget:    cfg.TargetAvailableAccounts,
		CPADeficit:   max(0, cfg.TargetAvailableAccounts-available),
		Inventory:    &inventory,
		Balance:      &balance,
	})
	return nil
}

func (s *Service) hydrateOverviewIfNeeded(ctx context.Context, cfg store.ManagerSupplyConfig) {
	if s == nil || !supplyCredentialsConfigured(cfg) {
		return
	}

	s.overviewRefreshMu.Lock()
	defer s.overviewRefreshMu.Unlock()

	s.stateMu.RLock()
	current := s.overview
	s.stateMu.RUnlock()
	if current.Inventory != nil && current.Balance != nil {
		return
	}
	// Keep a failed supplier request from being retried by every 10-second UI
	// refresh while still recovering promptly from a short upstream outage.
	if current.CheckedAtMS > 0 && time.Since(time.UnixMilli(current.CheckedAtMS)) < 20*time.Second {
		return
	}

	refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	inventory, balance, err := s.fetchSupplyOverview(refreshCtx, cfg, max(1, cfg.ReplenishBatchSize))
	cancel()

	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	// Automation may have refreshed the overview while the supplier request was
	// in flight. Do not replace fresher complete data with this cold-start copy.
	if s.overview.Inventory != nil && s.overview.Balance != nil {
		return
	}
	s.overview.CheckedAtMS = time.Now().UnixMilli()
	s.overview.CPATarget = cfg.TargetAvailableAccounts
	s.overview.CPADeficit = max(0, cfg.TargetAvailableAccounts-s.overview.CPAAvailable)
	if err != nil {
		s.overview.LastError = safeError(err)
		return
	}
	s.overview.Inventory = &inventory
	s.overview.Balance = &balance
	s.overview.LastError = ""
}

func supplyCredentialsConfigured(cfg store.ManagerSupplyConfig) bool {
	return strings.TrimSpace(cfg.BaseURL) != "" &&
		strings.TrimSpace(cfg.Username) != "" &&
		strings.TrimSpace(cfg.Password) != ""
}

func (s *Service) fetchSupplyOverview(ctx context.Context, cfg store.ManagerSupplyConfig, quantity int) (supplyclient.Inventory, supplyclient.Balance, error) {
	credentials := credentialsFromConfig(cfg)
	inventory, err := s.supplyClient.Inventory(ctx, credentials, cfg.Product, quantity)
	if err != nil {
		return supplyclient.Inventory{}, supplyclient.Balance{}, err
	}
	balance, err := s.supplyClient.Balance(ctx, credentials)
	if err != nil {
		return supplyclient.Inventory{}, supplyclient.Balance{}, err
	}
	return inventory, balance, nil
}

func (s *Service) countAvailableAccounts(ctx context.Context, cfg store.ManagerConfig) (int, error) {
	if strings.TrimSpace(cfg.CPAConnection.CPABaseURL) == "" || strings.TrimSpace(cfg.CPAConnection.ManagementKey) == "" {
		return 0, errors.New("CPA connection is not configured")
	}
	count := 0
	err := s.authFiles.Visit(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey, func(file cpaauthfiles.File) (bool, error) {
		if isAvailableCodexFile(file) {
			count++
		}
		return false, nil
	})
	return count, err
}

type smartSupplyPressure struct {
	level              string
	reason             string
	inventoryAvailable int
	inventoryMissing   int
	needsProduction    bool
	avgFulfillSeconds  int
	recentWaiting      int
}

func (s *Service) smartSupplyPressure(ctx context.Context, cfg store.ManagerSupplyConfig, inventory supplyclient.Inventory, requestedQuantity int) smartSupplyPressure {
	quantity := max(1, requestedQuantity)
	pressure := smartSupplyPressure{
		level:              smartSupplyPressureUnknown,
		reason:             "supply_pressure_unknown",
		inventoryAvailable: max(0, inventory.Available),
		inventoryMissing:   max(0, inventory.Missing),
		needsProduction:    inventory.NeedsProduction,
	}
	switch {
	case inventory.Available >= quantity && !inventory.NeedsProduction:
		pressure.level = smartSupplyPressurePlenty
		pressure.reason = "supply_inventory_plenty"
	case inventory.Available >= quantity:
		pressure.level = smartSupplyPressureNormal
		pressure.reason = "supply_inventory_ready_with_production"
	case inventory.Available > 0:
		pressure.level = smartSupplyPressureTight
		pressure.reason = "supply_inventory_partial"
	case inventory.NeedsProduction || inventory.Missing > 0:
		pressure.level = smartSupplyPressureScarce
		pressure.reason = "supply_inventory_scarce"
	default:
		pressure.level = smartSupplyPressureUnknown
		pressure.reason = "supply_inventory_unknown"
	}

	if s == nil || s.store == nil {
		return pressure
	}
	orders, err := s.store.ListSupplyOrders(ctx, 200)
	if err != nil {
		return pressure
	}
	nowMS := time.Now().UnixMilli()
	cutoffMS := nowMS - int64((24*time.Hour)/time.Millisecond)
	fulfillSamples := 0
	totalFulfillMS := int64(0)
	for _, order := range orders {
		if order.CreatedAtMS > 0 && order.CreatedAtMS < cutoffMS {
			break
		}
		if !order.Automatic || !sameSupplyProduct(order.Product, cfg.Product) {
			continue
		}
		switch order.Status {
		case "completed":
			if order.CompletedAtMS > order.CreatedAtMS {
				totalFulfillMS += order.CompletedAtMS - order.CreatedAtMS
				fulfillSamples++
			}
		case "released":
			if order.CompletedAtMS > order.CreatedAtMS && (order.ReadyQuantity > 0 || order.Progress >= 100) {
				totalFulfillMS += order.CompletedAtMS - order.CreatedAtMS
				fulfillSamples++
			}
		case "created", "waiting_inventory":
			if order.CreatedAtMS > 0 && nowMS-order.CreatedAtMS >= int64((60*time.Second)/time.Millisecond) {
				pressure.recentWaiting++
			}
		}
		if fulfillSamples >= 20 {
			break
		}
	}
	if fulfillSamples > 0 {
		pressure.avgFulfillSeconds = int(math.Round(float64(totalFulfillMS) / float64(fulfillSamples) / 1000))
	}
	if pressure.level == smartSupplyPressurePlenty {
		return pressure
	}
	switch {
	case pressure.avgFulfillSeconds > 0 && pressure.avgFulfillSeconds <= 45 && inventory.Available > 0 && !inventory.NeedsProduction:
		pressure.level = smartSupplyPressurePlenty
		pressure.reason = "supply_history_fast"
	case pressure.avgFulfillSeconds >= 180 || pressure.recentWaiting >= 2:
		if inventory.Available <= 0 || inventory.NeedsProduction || inventory.Missing > 0 {
			pressure.level = smartSupplyPressureScarce
			pressure.reason = "supply_history_slow"
		} else if pressure.level == smartSupplyPressureNormal {
			pressure.level = smartSupplyPressureTight
			pressure.reason = "supply_history_waiting"
		}
	case pressure.avgFulfillSeconds > 0 && pressure.avgFulfillSeconds <= 90 && pressure.level == smartSupplyPressureUnknown:
		pressure.level = smartSupplyPressureNormal
		pressure.reason = "supply_history_normal"
	}
	return pressure
}

func applySmartSupplyPressure(resource *SmartResource, pressure smartSupplyPressure) {
	if resource == nil {
		return
	}
	resource.SupplyPressureLevel = pressure.level
	resource.SupplyPressureReason = pressure.reason
	resource.SupplyInventoryAvailable = pressure.inventoryAvailable
	resource.SupplyInventoryMissing = pressure.inventoryMissing
	resource.SupplyNeedsProduction = pressure.needsProduction
	resource.SupplyAvgFulfillSeconds = pressure.avgFulfillSeconds
	resource.SupplyRecentWaiting = pressure.recentWaiting
}

func smartPrelockQuantityForSupplyPressure(cfg store.ManagerSupplyConfig, resource SmartResource, pressure smartSupplyPressure, quantity int) (int, string) {
	if quantity <= 0 {
		return quantity, ""
	}
	if resource.DemandTrend == smartDemandTrendFalling {
		return 0, "demand_falling_observe"
	}
	if resource.DemandTrend == smartDemandTrendRising {
		return min(quantity, smartRisingObservationQuantity(cfg, resource)), "demand_rising_observe"
	}
	maxQuantity := smartAutomaticOrderQuantityLimit(cfg, resource)
	minimumQuantity := min(smartPrelockMinQuantity(cfg), maxQuantity)
	quantity = clampInt(quantity, minimumQuantity, maxQuantity)
	if smartResourceAtOrBelowWarning(resource) {
		if resource.HealthLevel == smartHealthCritical {
			return quantity, "low_water_critical_full_batch"
		}
		return quantity, "low_water_warning_full_batch"
	}
	if !smartPrelockEnabled(cfg) {
		return quantity, ""
	}
	minQuantity := minimumQuantity
	fallbackBatch := smartFallbackBatchQuantity(cfg)
	switch pressure.level {
	case smartSupplyPressurePlenty:
		// 货源充足时仍然少量多次，但批量跟随容量缺口分档：1/2/3。
		// quantity 已由消耗速率、账号剩余额度、有效期和健康水位共同计算。
		smallBatch := smartPlentySmallBatchQuantity(cfg, quantity)
		if quantity > smallBatch {
			return smallBatch, "supply_plenty_small_batch"
		}
		return quantity, "supply_plenty_small_batch"
	case smartSupplyPressureNormal:
		moderateBatch := clampInt(int(math.Ceil(float64(quantity)/2)), minQuantity, maxQuantity)
		moderateBatch = min(moderateBatch, fallbackBatch)
		if quantity > moderateBatch {
			return moderateBatch, "supply_normal_moderate_batch"
		}
		return quantity, "supply_normal_moderate_batch"
	case smartSupplyPressureTight:
		// 货源紧张时，健康度不足意味着需要尽快补足容量。不要再按
		// fallbackBatch 固定拆成 5 个，避免补货速度落后于消耗速度。
		return quantity, "supply_tight_full_batch"
	case smartSupplyPressureScarce:
		// 货源稀缺时同样按智能计算出的缺口一次锁定，数量仍已受
		// PrelockMaxQuantity、ReplenishBatchSize 和日限额约束。
		return quantity, "supply_scarce_full_batch"
	default:
		if resource.HealthLevel == smartHealthCritical {
			return quantity, ""
		}
		conservativeBatch := min(clampInt(2, minQuantity, maxQuantity), fallbackBatch)
		if quantity > conservativeBatch {
			return conservativeBatch, "supply_unknown_conservative_batch"
		}
		return quantity, ""
	}
}

func smartFallbackBatchQuantity(cfg store.ManagerSupplyConfig) int {
	maxQuantity := smartPrelockMaxQuantity(cfg)
	if cfg.ReplenishBatchSize > 0 {
		maxQuantity = min(maxQuantity, cfg.ReplenishBatchSize)
	}
	// 自动预锁的保护批量：最大允许 10 时优先降到 5；最大低于 5 时尊重配置。
	return clampInt(5, 1, maxQuantity)
}

func smartPlentySmallBatchQuantity(cfg store.ManagerSupplyConfig, quantity int) int {
	maxQuantity := smartPrelockMaxQuantity(cfg)
	if cfg.ReplenishBatchSize > 0 {
		maxQuantity = min(maxQuantity, cfg.ReplenishBatchSize)
	}
	if maxQuantity <= 0 || quantity <= 0 {
		return 0
	}

	// quantity 是智能容量模型算出的实际缺口，不再使用固定的最小配置值。
	// 货源充足时最多预锁 3 个，避免浪费；缺口小于 3 个时按缺口补齐。
	return min(clampInt(quantity, 1, maxQuantity), 3)
}

func smartPlentyTakeBatchQuantity(cfg store.ManagerSupplyConfig) int {
	return smartFallbackBatchQuantity(cfg)
}

func sameSupplyProduct(a string, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func (s *Service) smartSuggestedCreateQuantity(cfg store.ManagerSupplyConfig, resource SmartResource) int {
	if resource.DemandTrend == smartDemandTrendFalling {
		return 0
	}
	quantity := resource.SuggestedQuantity
	if quantity <= 0 && resource.CapacityGapRCU > 0 && resource.UnitCapacityRCU > 0 {
		unit := smartEstimatedNewAccountCapacityRCU(cfg)
		if unit <= 0 {
			unit = smartEstimatedAccountCapacityRCU(resource.UnitCapacityRCU, float64(smartUsefulAccountLifetimeMinutes()))
		}
		quantity = int(math.Ceil(resource.CapacityGapRCU / unit))
	}
	if quantity <= 0 {
		return 0
	}
	limit := smartAutomaticOrderQuantityLimit(cfg, resource)
	quantity = min(quantity, limit)
	if smartPrelockEnabled(cfg) {
		quantity = max(quantity, min(smartPrelockMinQuantity(cfg), limit))
	}
	if cfg.DailyMaxReplenishQuantity > 0 {
		quantity = min(quantity, cfg.DailyMaxReplenishQuantity)
	}
	if resource.DemandTrend == smartDemandTrendRising {
		quantity = min(quantity, smartRisingObservationQuantity(cfg, resource))
	}
	return clampInt(quantity, 1, 100)
}

func estimatedSupplyOrderCapacityRCU(cfg store.ManagerSupplyConfig, quantity int) float64 {
	if quantity <= 0 {
		return 0
	}
	unit := smartEstimatedNewAccountCapacityRCU(cfg)
	return round2(float64(quantity) * unit)
}

func (s *Service) remainingAutomaticDailyQuantity(ctx context.Context, cfg store.ManagerSupplyConfig) (int, error) {
	limit := cfg.DailyMaxReplenishQuantity
	if limit <= 0 {
		return 100, nil
	}
	orders, err := s.store.ListSupplyOrders(ctx, 200)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	dayStartMS := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).UnixMilli()
	used := 0
	for _, order := range orders {
		if order.CreatedAtMS > 0 && order.CreatedAtMS < dayStartMS {
			break
		}
		if !order.Automatic {
			continue
		}
		switch order.Status {
		case "failed", "cancelled", "dismissed", "released":
			continue
		}
		used += max(0, order.RequestedQuantity)
	}
	if used >= limit {
		return 0, nil
	}
	return limit - used, nil
}

func (s *Service) waitLockedOrder(ctx context.Context, cfg store.ManagerSupplyConfig, order *store.SupplyOrder, resource SmartResource, action string, reason string) error {
	if order == nil {
		return nil
	}
	resource.SuggestedAction = action
	resource.DecisionReason = reason
	if resource.LockedOrderID == "" {
		resource.LockedOrderID = order.OrderID
	}
	resource.LockedConfirmRounds = s.currentCriticalConfirmRounds(order.OrderID)
	s.setSmartResource(resource)
	order.NextPollAtMS = nextPollAt(cfg, 0)
	return s.store.UpdateSupplyOrder(ctx, *order)
}

func (s *Service) automaticCreateCooldownActive(cfg store.ManagerSupplyConfig, resource SmartResource) bool {
	seconds := smartCreateCooldownForResource(cfg, resource)
	s.stateMu.RLock()
	last := s.lastAutomaticCreateAtMS
	s.stateMu.RUnlock()
	if seconds <= 0 || last <= 0 {
		return false
	}
	return time.Since(time.UnixMilli(last)) < time.Duration(seconds)*time.Second
}

// smartAutomaticCheckIntervalSeconds keeps the worker responsive while the
// capacity lower bound is below a warning line. Without this adjustment a
// 60-second normal check interval could make a 15-minute emergency decision
// wait a full minute before the next order attempt.
func smartAutomaticCheckIntervalSeconds(cfg store.ManagerSupplyConfig, resource SmartResource) int {
	seconds := cfg.CheckIntervalSeconds
	if seconds <= 0 {
		seconds = 60
	}
	if smartResourceAtOrBelowWarning(resource) {
		seconds = min(seconds, smartCreateCooldownForResource(cfg, resource))
	}
	return max(1, seconds)
}

func (s *Service) markAutomaticCreate() {
	s.stateMu.Lock()
	s.lastAutomaticCreateAtMS = time.Now().UnixMilli()
	s.stateMu.Unlock()
}

func (s *Service) incrementCriticalConfirm(orderID string) int {
	s.criticalConfirmMu.Lock()
	defer s.criticalConfirmMu.Unlock()
	if s.criticalConfirmRounds == nil {
		s.criticalConfirmRounds = make(map[string]int)
	}
	s.criticalConfirmRounds[orderID]++
	return s.criticalConfirmRounds[orderID]
}

func (s *Service) currentCriticalConfirmRounds(orderID string) int {
	s.criticalConfirmMu.Lock()
	defer s.criticalConfirmMu.Unlock()
	if s.criticalConfirmRounds == nil {
		return 0
	}
	return s.criticalConfirmRounds[orderID]
}

func (s *Service) smartCriticalTakeConfirmed(cfg store.ManagerSupplyConfig, orderID string) bool {
	return s.currentCriticalConfirmRounds(orderID) >= smartCriticalTakeConfirmRounds(cfg)
}

func (s *Service) smartTakeAllowed(cfg store.ManagerSupplyConfig, orderID string) bool {
	resource := s.currentSmartResource(cfg)
	if resource.SuggestedAction == smartActionTakeLocked {
		switch resource.DecisionReason {
		case "critical_take_confirmed", "critical_take_confirmed_stale_lower_bound", "supply_plenty_small_take", "low_water_take_ready", "low_water_take_ready_stale_lower_bound":
			return true
		}
	}
	return s.smartCriticalTakeConfirmed(cfg, orderID)
}

func (s *Service) resetCriticalConfirm(orderID string) {
	s.criticalConfirmMu.Lock()
	defer s.criticalConfirmMu.Unlock()
	if s.criticalConfirmRounds != nil {
		delete(s.criticalConfirmRounds, orderID)
	}
}

func (s *Service) requireCredentials(cfg store.ManagerSupplyConfig) error {
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.Username) == "" || strings.TrimSpace(cfg.Password) == "" {
		return ErrNotConfigured
	}
	parsed, err := url.Parse(cfg.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.New("account supply base URL is invalid")
	}
	if cfg.Product != "oauth_30d" && cfg.Product != "oauth_7d" {
		return errors.New("account supply product is invalid")
	}
	return nil
}

func (s *Service) updateOrderError(ctx context.Context, order *store.SupplyOrder, err error, cfg store.ManagerSupplyConfig) error {
	order.LastError = safeError(err)
	if order.Status == "taking" {
		order.NextPollAtMS = time.Now().Add(supplyTakeRetryDelay(cfg)).UnixMilli()
	} else {
		order.NextPollAtMS = nextPollAt(cfg, 0)
	}
	if updateErr := s.store.UpdateSupplyOrder(ctx, *order); updateErr != nil {
		return updateErr
	}
	return err
}

func (s *Service) setRunning(running bool) {
	s.stateMu.Lock()
	s.running = running
	s.stateMu.Unlock()
}

// ScheduleAutomaticExecution publishes the worker's actual wake-up time. The
// worker owns this value because it may be shortened for an order poll or a
// critical capacity check; deriving it from configuration in the HTTP handler
// would otherwise show a misleading countdown.
func (s *Service) ScheduleAutomaticExecution(at time.Time) {
	if s == nil {
		return
	}
	s.stateMu.Lock()
	s.automation.NextExecutionAtMS = at.UnixMilli()
	if s.automation.LastResult == "" {
		s.automation.LastResult = "scheduled"
	}
	s.stateMu.Unlock()
}

// RecordAutomaticExecution saves the result of a completed automatic cycle
// and the next scheduled worker wake-up. This is a compact runtime snapshot,
// so it has no database writes on the automatic replenishment hot path.
func (s *Service) RecordAutomaticExecution(startedAt, finishedAt, nextAt time.Time, err error) {
	if s == nil {
		return
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.automation.LastStartedAtMS = startedAt.UnixMilli()
	s.automation.LastFinishedAtMS = finishedAt.UnixMilli()
	s.automation.NextExecutionAtMS = nextAt.UnixMilli()
	s.automation.IntervalSeconds = max(0, int(math.Ceil(nextAt.Sub(finishedAt).Seconds())))
	s.automation.LastAction = s.smartResourceState.SuggestedAction
	s.automation.LastReason = s.smartResourceState.DecisionReason
	if err != nil {
		s.automation.LastResult = "failed"
		s.automation.LastError = safeError(err)
		return
	}
	s.automation.LastResult = "completed"
	s.automation.LastError = ""
}

func (s *Service) currentAutomationExecution(enabled bool) AutomationExecution {
	if s == nil {
		return AutomationExecution{Enabled: enabled}
	}
	s.stateMu.RLock()
	status := s.automation
	status.Running = s.running
	s.stateMu.RUnlock()
	status.Enabled = enabled
	if !enabled {
		// The worker remains alive so a later configuration change takes effect,
		// but it has no automatic supply action scheduled while disabled.
		status.NextExecutionAtMS = 0
		status.IntervalSeconds = 0
		if status.LastResult == "" || status.LastResult == "scheduled" {
			status.LastResult = "disabled"
		}
	}
	return status
}

func (s *Service) setOverview(overview Overview) {
	s.stateMu.Lock()
	s.overview = overview
	s.stateMu.Unlock()
}

func (s *Service) updateCPAOverview(available int, target int) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.overview.CPAAvailable = available
	s.overview.CPATarget = target
	s.overview.CPADeficit = max(0, target-available)
	s.overview.CheckedAtMS = time.Now().UnixMilli()
}

// Legacy replenishment is count-based, so it owns the CPA account overview.
// Smart replenishment is quota-capacity based and must not overwrite that view
// with a stale or synthetic account count while releasing a prelocked order.
func (s *Service) updateCPAOverviewIfLegacy(cfg store.ManagerSupplyConfig, available int) {
	if smartSupplyEnabled(cfg) {
		return
	}
	s.updateCPAOverview(available, cfg.TargetAvailableAccounts)
}

func (s *Service) ensureCPAAccountImported(ctx context.Context, cfg store.ManagerConfig, fileName string, payload []byte) error {
	find := func() error {
		file, found, err := s.authFiles.Find(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey, fileName, "")
		if err != nil {
			return err
		}
		if !found {
			return errors.New("CPA did not register the imported auth file")
		}
		provider := strings.ToLower(strings.TrimSpace(file.Provider))
		if provider != "codex" && provider != "openai-codex" {
			return fmt.Errorf("CPA registered imported auth file with unsupported provider %q", provider)
		}
		if !isAvailableCodexFile(file) {
			return errors.New("CPA registered imported auth file but it is not available")
		}
		return nil
	}
	if err := find(); err == nil {
		return nil
	}
	if err := s.authFiles.Upload(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey,
		fileName, payload, cfg.Supply.DefaultWebsockets); err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if err := find(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(time.Duration(100*(1<<attempt)) * time.Millisecond)
	}
	return lastErr
}

func (s *Service) recordError(err error) {
	if err == nil {
		return
	}
	s.stateMu.Lock()
	s.overview.LastError = safeError(err)
	s.stateMu.Unlock()
}

func sanitizeConfig(cfg store.ManagerSupplyConfig) store.ManagerSupplyConfig {
	cfg.PasswordConfigured = strings.TrimSpace(cfg.Password) != ""
	cfg.Password = ""
	return cfg
}

func credentialsFromConfig(cfg store.ManagerSupplyConfig) supplyclient.Credentials {
	return supplyclient.Credentials{BaseURL: cfg.BaseURL, Username: cfg.Username, Password: cfg.Password}
}

func applyRemoteOrder(order *store.SupplyOrder, remote supplyclient.Order, cfg store.ManagerSupplyConfig) {
	if remote.Status != "" {
		order.RemoteStatus = remote.Status
	}
	if remote.ChargedFen > 0 {
		order.ChargedFen = remote.ChargedFen
	}
	if remote.ReleasedFen > 0 {
		order.ReleasedFen = remote.ReleasedFen
	}
	if remote.ReadyQuantity > order.ReadyQuantity {
		order.ReadyQuantity = remote.ReadyQuantity
	}
	if remote.Progress > order.Progress {
		order.Progress = remote.Progress
	}
	if strings.TrimSpace(remote.StatusURL) != "" {
		order.StatusURL = remote.StatusURL
	}
	if strings.TrimSpace(remote.TakeURL) != "" {
		order.TakeURL = remote.TakeURL
	}
	order.NextPollAtMS = nextPollAt(cfg, remote.RetryAfterSeconds)
	order.LastError = ""
}

func (s *Service) cancelOrder(ctx context.Context, order *store.SupplyOrder, err error) error {
	order.Status = "cancelled"
	order.RemoteStatus = "cancelled"
	order.LastError = safeError(err)
	order.NextPollAtMS = 0
	order.CompletedAtMS = time.Now().UnixMilli()
	return s.store.UpdateSupplyOrder(ctx, *order)
}

func isHTTPStatus(err error, status int) bool {
	var upstreamErr *supplyclient.HTTPError
	return errors.As(err, &upstreamErr) && upstreamErr.StatusCode == status
}

func isDefiniteCreateFailure(err error) bool {
	var upstreamErr *supplyclient.HTTPError
	if !errors.As(err, &upstreamErr) {
		return false
	}
	switch upstreamErr.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusPaymentRequired, http.StatusConflict,
		http.StatusUnprocessableEntity, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

func newCreateAttemptID() string {
	random := make([]byte, 6)
	if _, err := cryptorand.Read(random); err != nil {
		return fmt.Sprintf("create-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("create-%d-%s", time.Now().UnixMilli(), hex.EncodeToString(random))
}

func localOrderStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ready", "ready_for_pickup":
		return "ready"
	case "completed", "done", "taken", "delivered":
		return "ready"
	case "cancelled", "canceled":
		return "cancelled"
	case "failed", "error", "expired":
		return "failed"
	default:
		return "waiting_inventory"
	}
}

func isTerminalRemoteStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "done", "taken", "delivered", "cancelled", "canceled", "failed", "error", "expired":
		return true
	default:
		return false
	}
}

func isSuccessfulRemoteStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "done", "taken", "delivered":
		return true
	default:
		return false
	}
}

func isReadyForTake(status string) bool {
	status = strings.ToLower(strings.TrimSpace(status))
	return status == "ready" || status == "ready_for_pickup" || isSuccessfulRemoteStatus(status)
}

func supplyTakeLeaseDuration(cfg store.ManagerSupplyConfig) time.Duration {
	seconds := cfg.PollIntervalSeconds
	if seconds <= 0 {
		seconds = 3
	}
	// The database claim must outlive the long take request plus a short
	// recovery buffer. This protects the supplier's idempotent pickup while a
	// manager restarts or another worker wakes up.
	lease := supplyclient.DefaultTakeTimeout() + time.Duration(seconds)*time.Second + 30*time.Second
	if lease < 2*time.Minute+30*time.Second {
		return 2*time.Minute + 30*time.Second
	}
	if lease > 5*time.Minute {
		return 5 * time.Minute
	}
	return lease
}

func supplyTakeRetryDelay(cfg store.ManagerSupplyConfig) time.Duration {
	seconds := cfg.PollIntervalSeconds
	if seconds < 15 {
		seconds = 15
	}
	if seconds > 60 {
		seconds = 60
	}
	return time.Duration(seconds) * time.Second
}

func nextPollAt(cfg store.ManagerSupplyConfig, retryAfterSeconds int) int64 {
	seconds := retryAfterSeconds
	if seconds <= 0 {
		seconds = cfg.PollIntervalSeconds
	}
	if seconds <= 0 {
		seconds = 3
	}
	return time.Now().Add(time.Duration(seconds) * time.Second).UnixMilli()
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	seconds := 5 * (1 << min(attempt-1, 6))
	if seconds > 300 {
		seconds = 300
	}
	return time.Duration(seconds) * time.Second
}

func automaticSettleWindow(cfg store.ManagerSupplyConfig) time.Duration {
	seconds := cfg.CheckIntervalSeconds * 2
	if seconds < 30 {
		seconds = 30
	}
	if seconds > 120 {
		seconds = 120
	}
	return time.Duration(seconds) * time.Second
}

type normalizedSupplyAccount struct {
	payload          []byte
	itemKey          string
	fileName         string
	leaseExpiresAtMS int64
}

func normalizeAccountPayload(raw json.RawMessage) ([]byte, string, string, error) {
	accounts, err := normalizeAccountPayloads(raw)
	if err != nil {
		return nil, "", "", err
	}
	if len(accounts) != 1 {
		return nil, "", "", fmt.Errorf("expected one supply account payload, got %d", len(accounts))
	}
	account := accounts[0]
	return account.payload, account.itemKey, account.fileName, nil
}

func normalizeAccountPayloads(raw json.RawMessage) ([]normalizedSupplyAccount, error) {
	value, err := decodeSupplyAccountPayload(raw)
	if err != nil {
		return nil, err
	}
	accounts, err := normalizeSupplyAccountValue(value, nil)
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return nil, errors.New("supply account payload did not include importable OpenAI OAuth accounts")
	}
	return accounts, nil
}

func decodeSupplyAccountPayload(raw json.RawMessage) (any, error) {
	payload := bytes.TrimSpace(raw)
	if len(payload) == 0 {
		return nil, errors.New("empty supply account payload")
	}
	for unwrap := 0; unwrap < 3 && len(payload) > 0 && payload[0] == '"'; unwrap++ {
		var text string
		if err := json.Unmarshal(payload, &text); err != nil {
			return nil, err
		}
		payload = []byte(strings.TrimSpace(text))
	}
	if len(payload) == 0 {
		return nil, errors.New("empty supply account payload")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func normalizeSupplyAccountValue(value any, inheritedExportedAt any) ([]normalizedSupplyAccount, error) {
	switch typed := value.(type) {
	case map[string]any:
		if child, exportedAt, ok := nestedSupplyAccountList(typed, inheritedExportedAt); ok {
			return normalizeSupplyAccountList(child, exportedAt)
		}
		account, err := normalizeSupplyAccountObject(typed, inheritedExportedAt)
		if err != nil {
			return nil, err
		}
		return []normalizedSupplyAccount{account}, nil
	case []any:
		return normalizeSupplyAccountList(typed, inheritedExportedAt)
	default:
		return nil, errors.New("supply account payload must be a JSON object or account array")
	}
}

func nestedSupplyAccountList(object map[string]any, inheritedExportedAt any) ([]any, any, bool) {
	exportedAt := firstValueOrNil(inheritedExportedAt, object["exported_at"], object["exportedAt"])
	for _, key := range []string{"accounts", "items"} {
		if list, ok := object[key].([]any); ok {
			return list, exportedAt, true
		}
	}
	for _, key := range []string{"payload", "data", "result"} {
		if child, ok := object[key].(map[string]any); ok {
			if list, childExportedAt, found := nestedSupplyAccountList(child, exportedAt); found {
				return list, childExportedAt, true
			}
		}
	}
	return nil, exportedAt, false
}

func normalizeSupplyAccountList(values []any, exportedAt any) ([]normalizedSupplyAccount, error) {
	if len(values) == 0 {
		return nil, errors.New("supply account list is empty")
	}
	accounts := make([]normalizedSupplyAccount, 0, len(values))
	for index, value := range values {
		children, err := normalizeSupplyAccountValue(value, exportedAt)
		if err != nil {
			return nil, fmt.Errorf("account %d: %w", index+1, err)
		}
		accounts = append(accounts, children...)
	}
	return accounts, nil
}

func normalizeSupplyAccountObject(object map[string]any, exportedAt any) (normalizedSupplyAccount, error) {
	metadata := cloneMap(object)
	if credentials, ok := object["credentials"].(map[string]any); ok {
		if !isSupportedSupplyOAuth(object, credentials) {
			return normalizedSupplyAccount{}, errors.New("account is not an OpenAI OAuth credential")
		}
		metadata = convertSub2AccountToCPAPayload(object, credentials, exportedAt)
	} else if hasSupplyOAuthToken(metadata) {
		metadata["type"] = "codex"
		normalizeCodexPayloadAliases(metadata)
	} else {
		return normalizedSupplyAccount{}, errors.New("account does not contain OAuth token data")
	}

	identity := supplyAccountIdentity(metadata)
	if identity == "" {
		return normalizedSupplyAccount{}, errors.New("stable account identity is missing")
	}

	normalized, err := json.Marshal(metadata)
	if err != nil {
		return normalizedSupplyAccount{}, err
	}
	sum := sha256.Sum256([]byte(identity))
	digest := hex.EncodeToString(sum[:])
	return normalizedSupplyAccount{
		payload:          normalized,
		itemKey:          digest,
		fileName:         "codex-supply-" + digest[:20] + ".json",
		leaseExpiresAtMS: supplyDeliveryLeaseExpiresAtMS(object, time.Now()),
	}, nil
}

// supplyDeliveryLeaseExpiresAtMS is deliberately based on the supplier's
// delivery validity rather than OAuth token expiry. The supplier documents a
// one-hour usable lifetime; when it omits per-item evidence we persist that
// conservative import-time lease instead of treating the credential as fresh
// on every subsequent capacity refresh.
func supplyDeliveryLeaseExpiresAtMS(payload map[string]any, now time.Time) int64 {
	defaultExpiry := now.Add(time.Hour)
	if seconds, ok := numberFieldOK(payload,
		"remaining_seconds", "remainingSeconds", "remaining_valid_seconds", "remainingValidSeconds",
		"minimum_remaining_seconds", "minimumRemainingSeconds", "ttl_seconds", "ttlSeconds",
	); ok {
		return now.Add(time.Duration(clampFloat(seconds, 0, float64(time.Hour/time.Second))) * time.Second).UnixMilli()
	}
	if minutes, ok := numberFieldOK(payload, "remaining_minutes", "remainingMinutes", "ttl_minutes", "ttlMinutes"); ok {
		return now.Add(time.Duration(clampFloat(minutes, 0, 60) * float64(time.Minute))).UnixMilli()
	}
	for _, key := range []string{"lease_expires_at", "leaseExpiresAt", "valid_until", "validUntil", "expires_at", "expiresAt", "expired"} {
		raw, found := payload[key]
		if !found || raw == nil {
			continue
		}
		expiresAt, ok := parseSmartExpiryTime(raw, now)
		if !ok || expiresAt.Before(now) {
			continue
		}
		// OAuth refresh-token expiry may be measured in days. It must not extend
		// the supplier's separate one-hour delivery lease.
		if expiresAt.Before(defaultExpiry) {
			return expiresAt.UnixMilli()
		}
	}
	return defaultExpiry.UnixMilli()
}

func supplyDeliveryLeaseExpiresAtMSFromSeconds(seconds int64, now time.Time) int64 {
	if seconds < 0 {
		seconds = 0
	}
	maxSeconds := int64(time.Hour / time.Second)
	if seconds > maxSeconds {
		seconds = maxSeconds
	}
	return now.Add(time.Duration(seconds) * time.Second).UnixMilli()
}

// applySupplyOrderItemLeases returns true only for an exact, ordered mapping
// between supplier order items and CPA import files. A bundled delivery may
// expand one supplied payload into many files, so a partial mapping would make
// a valid account appear expired (or vice versa).
func applySupplyOrderItemLeases(accounts []normalizedSupplyAccount, remainingSeconds []int64, now time.Time) bool {
	if len(accounts) == 0 || len(accounts) != len(remainingSeconds) {
		return false
	}
	for index := range accounts {
		accounts[index].leaseExpiresAtMS = supplyDeliveryLeaseExpiresAtMSFromSeconds(remainingSeconds[index], now)
	}
	return true
}

func normalizeAccountPayloadForImport(payloadJSON string) ([]byte, string, string, error) {
	return normalizeAccountPayload(json.RawMessage(strings.TrimSpace(payloadJSON)))
}

func convertSub2AccountToCPAPayload(account map[string]any, credentials map[string]any, exportedAt any) map[string]any {
	extra := mapFromMap(account, "extra")
	metadata := cloneMap(credentials)
	metadata["type"] = "codex"
	metadata["import_format"] = "sub2api"
	metadata["sub2_platform"] = strings.ToLower(stringFromMap(account, "platform", "provider"))
	setString(metadata, "source_product", stringFromMap(account, "product"))

	if firstNonEmptyString(stringFromMap(metadata, "access_token"), stringFromMap(metadata, "accessToken")) == "" {
		if accessToken := stringFromMap(metadata, "session_access_token", "sessionAccessToken"); accessToken != "" {
			metadata["access_token"] = accessToken
		}
	}
	normalizeCodexPayloadAliases(metadata)

	email := stringFromMaps([]map[string]any{metadata, extra}, "email", "email_address", "emailAddress")
	accountID := stringFromMaps([]map[string]any{metadata, extra}, "chatgpt_account_id", "chatgptAccountId", "account_id", "accountId")
	userID := stringFromMaps([]map[string]any{metadata, extra}, "chatgpt_user_id", "chatgptUserId", "user_id", "userId")
	organizationID := stringFromMaps([]map[string]any{metadata, extra}, "organization_id", "organizationId", "org_id", "orgId", "poid")
	workspaceID := stringFromMaps([]map[string]any{metadata, extra}, "workspace_id", "workspaceId", "chatgpt_workspace_id", "chatgptWorkspaceId", "workspace")
	planType := resolveSupplyPlanType(metadata, extra)
	expiresAt := stringFromMaps([]map[string]any{metadata, account}, "expires_at", "expiresAt", "expired")
	lastRefresh := stringFromMaps([]map[string]any{metadata, extra, account}, "last_refresh", "lastRefresh", "exported_at", "exportedAt")
	if lastRefresh == "" && exportedAt != nil {
		lastRefresh = stringFromAny(exportedAt)
	}
	name := firstNonEmptyString(stringFromMap(account, "name"), email, accountID, "OpenAI OAuth Account")

	setString(metadata, "name", name)
	setString(metadata, "email", email)
	if accountID != "" {
		metadata["account_id"] = accountID
		metadata["chatgpt_account_id"] = accountID
	}
	setString(metadata, "chatgpt_user_id", userID)
	setString(metadata, "organization_id", organizationID)
	setString(metadata, "workspace_id", workspaceID)
	if planType != "" {
		metadata["plan_type"] = planType
		metadata["chatgpt_plan_type"] = planType
	}
	if expiresAt != "" {
		metadata["expired"] = expiresAt
		metadata["expires_at"] = expiresAt
	}
	setString(metadata, "last_refresh", lastRefresh)
	copyOptionalSupplyField(metadata, account, "priority", "priority")
	copyOptionalSupplyField(metadata, account, "max_concurrency", "concurrency")
	copyOptionalSupplyField(metadata, account, "proxy_url", "proxy_url")
	copyOptionalSupplyField(metadata, account, "proxy_url", "proxyUrl")
	copyOptionalSupplyField(metadata, extra, "proxy_url", "proxy_url")
	copyOptionalSupplyField(metadata, extra, "proxy_url", "proxyUrl")
	copyOptionalSupplyField(metadata, extra, "websockets", "websockets")
	copyOptionalSupplyField(metadata, extra, "openai_oauth_responses_websockets_v2_enabled", "openai_oauth_responses_websockets_v2_enabled")
	if value, ok := account["disabled"].(bool); ok && value {
		metadata["disabled"] = true
	} else if status := strings.ToLower(stringFromMap(account, "status", "state")); status == "disabled" || status == "inactive" || status == "expired" || status == "revoked" || status == "deleted" {
		metadata["disabled"] = true
	}
	return stripEmptyValues(metadata)
}

func normalizeCodexPayloadAliases(metadata map[string]any) {
	if value := firstNonEmptyString(stringFromMap(metadata, "access_token", "accessToken"), stringFromMap(metadata, "session_access_token", "sessionAccessToken")); value != "" {
		metadata["access_token"] = value
	}
	if accountID := stringFromMap(metadata, "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId"); accountID != "" {
		metadata["account_id"] = accountID
		metadata["chatgpt_account_id"] = accountID
	}
	if organizationID := stringFromMap(metadata, "organization_id", "organizationId", "org_id", "orgId", "poid"); organizationID != "" {
		metadata["organization_id"] = organizationID
	}
	if workspaceID := stringFromMap(metadata, "workspace_id", "workspaceId", "chatgpt_workspace_id", "chatgptWorkspaceId", "workspace"); workspaceID != "" {
		metadata["workspace_id"] = workspaceID
	}
	if planType := resolveSupplyPlanType(metadata); planType != "" {
		metadata["plan_type"] = planType
		metadata["chatgpt_plan_type"] = planType
	}
	if expiresAt := stringFromMap(metadata, "expired", "expires_at", "expiresAt"); expiresAt != "" {
		metadata["expired"] = expiresAt
	}
}

// Supply payloads may contain an old generic `plan_type: free` together with
// the authoritative ChatGPT workspace plan. Preserve the more specific plan
// so a Team import is never downgraded to Free.
func resolveSupplyPlanType(values ...map[string]any) string {
	candidates := make([]string, 0, len(values)*4)
	for _, value := range values {
		if len(value) == 0 {
			continue
		}
		for _, key := range []string{"chatgpt_plan_type", "chatgptPlanType", "plan_type", "planType"} {
			if candidate := strings.ToLower(strings.TrimSpace(stringFromMap(value, key))); candidate != "" {
				candidates = append(candidates, candidate)
			}
		}
	}
	for _, candidate := range candidates {
		if candidate != "free" {
			return candidate
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}

func supplyAccountIdentity(metadata map[string]any) string {
	accountID := stringFromMap(metadata, "account_id", "chatgpt_account_id")
	email := stringFromMap(metadata, "email")
	planType := stringFromMap(metadata, "plan_type", "chatgpt_plan_type")
	if accountID != "" && email != "" {
		return accountID + "|" + email
	}
	if accountID != "" {
		return accountID
	}
	if userID := stringFromMap(metadata, "chatgpt_user_id"); userID != "" {
		return userID
	}
	if email != "" {
		return email + "|" + planType
	}
	return stringFromMap(metadata, "refresh_token", "access_token", "id_token")
}

func cloneMap(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func stringFromMap(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key]; ok && value != nil {
			if text := stringFromAny(value); text != "" {
				return text
			}
		}
	}
	return ""
}

func stringFromAny(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case json.Number:
		return strings.TrimSpace(typed.String())
	case string:
		return strings.TrimSpace(typed)
	default:
		text := strings.TrimSpace(fmt.Sprint(value))
		if text == "<nil>" {
			return ""
		}
		return text
	}
}

func stringFromMaps(values []map[string]any, keys ...string) string {
	for _, value := range values {
		if len(value) == 0 {
			continue
		}
		if text := stringFromMap(value, keys...); text != "" {
			return text
		}
	}
	return ""
}

func firstValueOrNil(values ...any) any {
	for _, value := range values {
		if value == nil {
			continue
		}
		if text := stringFromAny(value); text != "" {
			return value
		}
	}
	return nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func setString(values map[string]any, key string, value string) {
	if value = strings.TrimSpace(value); value != "" {
		values[key] = value
	}
}

func mapFromMap(values map[string]any, key string) map[string]any {
	if child, ok := values[key].(map[string]any); ok {
		return child
	}
	return nil
}

func copyOptionalSupplyField(target map[string]any, source map[string]any, targetKey string, sourceKey string) {
	if len(source) == 0 {
		return
	}
	if _, exists := target[targetKey]; exists {
		return
	}
	if value, exists := source[sourceKey]; exists && value != nil && strings.TrimSpace(fmt.Sprint(value)) != "" {
		target[targetKey] = value
	}
}

func stripEmptyValues(values map[string]any) map[string]any {
	for key, value := range values {
		if value == nil || strings.TrimSpace(fmt.Sprint(value)) == "" || strings.TrimSpace(fmt.Sprint(value)) == "<nil>" {
			delete(values, key)
		}
	}
	return values
}

func hasSupplyOAuthToken(values map[string]any) bool {
	return firstNonEmptyString(
		stringFromMap(values, "access_token", "accessToken"),
		stringFromMap(values, "session_access_token", "sessionAccessToken"),
		stringFromMap(values, "refresh_token", "refreshToken"),
		stringFromMap(values, "id_token", "idToken"),
		stringFromMap(values, "session_token", "sessionToken"),
	) != ""
}

func hasSupplyAccessToken(values map[string]any) bool {
	return firstNonEmptyString(stringFromMap(values, "access_token", "accessToken"), stringFromMap(values, "session_access_token", "sessionAccessToken")) != ""
}

func isSupportedSupplyOAuth(account map[string]any, credentials map[string]any) bool {
	platform := strings.ToLower(stringFromMap(account, "platform", "provider"))
	typeName := strings.ToLower(stringFromMap(account, "type"))
	credentialType := strings.ToLower(stringFromMap(credentials, "type"))
	if platform != "" && platform != "openai" && platform != "codex" {
		return false
	}
	if credentialType != "" && credentialType != "codex" && credentialType != "openai" {
		return false
	}
	if typeName != "" && typeName != "oauth" && typeName != "codex" {
		return false
	}
	return hasSupplyOAuthToken(credentials)
}

func isAvailableCodexFile(file cpaauthfiles.File) bool {
	// Runtime unavailable/error states include model cooldowns and transient
	// upstream failures. They are not credential health signals and must not
	// lower capacity or trigger replenishment. Only explicit disablement,
	// credential invalidation, or hard quota exhaustion are excluded.
	return isSmartCapacityCodexFile(file)
}

func isSmartCapacityCodexFile(file cpaauthfiles.File) bool {
	if file.Disabled {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(file.Provider))
	if provider != "codex" && provider != "openai-codex" {
		return false
	}
	if boolField(file.Raw, "disabled", "expired", "revoked", "deleted") {
		return false
	}
	status := strings.ToLower(textField(file.Raw, "status", "state"))
	switch status {
	case "disabled", "inactive", "invalid", "expired", "revoked", "deleted":
		return false
	}
	if smartAccountCapacityHardBlocked(file.Raw) {
		return false
	}
	return true
}

func boolField(values map[string]any, keys ...string) bool {
	for _, key := range keys {
		value, ok := values[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case bool:
			return typed
		case string:
			parsed, _ := strconv.ParseBool(strings.TrimSpace(typed))
			return parsed
		case json.Number:
			parsed, _ := typed.Int64()
			return parsed != 0
		case float64:
			return typed != 0
		}
	}
	return false
}

func textField(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key]; ok && value != nil {
			return strings.TrimSpace(fmt.Sprint(value))
		}
	}
	return ""
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 1000 {
		message = message[:1000]
	}
	return message
}
