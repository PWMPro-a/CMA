package supply

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpaauthfiles"
	managerconfigsvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

var lowPriceReserveLadderWeights = []int{50, 17, 10, 7, 5, 4, 3, 2, 1, 1}

const (
	lowPriceReserveRetryBackoff         = 30 * time.Second
	lowPriceReserveExactQuoteBackoff    = "exact_quote_backoff"
	lowPriceReserveCatalogErrorBackoff  = "catalog_error_backoff"
	lowPriceReserveInventoryUnavailable = "low-price inventory is no longer available"
)

type LowPriceReserveExecution struct {
	Enabled                   bool    `json:"enabled"`
	Running                   bool    `json:"running"`
	ReserveAccounts           int     `json:"reserveAccounts"`
	TargetAccounts            int     `json:"targetAccounts"`
	Gap                       int     `json:"gap"`
	Ladder                    []int   `json:"ladder,omitempty"`
	NextStageQuantity         int     `json:"nextStageQuantity"`
	CheckIntervalMilliseconds int     `json:"checkIntervalMilliseconds"`
	MaxUnitPriceFen           int64   `json:"maxUnitPriceFen"`
	LastCheckedAtMS           int64   `json:"lastCheckedAtMs,omitempty"`
	NextCheckAtMS             int64   `json:"nextCheckAtMs,omitempty"`
	LastQuotedUnitPriceFen    int64   `json:"lastQuotedUnitPriceFen,omitempty"`
	LastQuotedCostMultiplier  float64 `json:"lastQuotedCostMultiplier,omitempty"`
	SelectedPlatformID        string  `json:"selectedPlatformId,omitempty"`
	ActiveTaskID              string  `json:"activeTaskId,omitempty"`
	RetryAfterMS              int64   `json:"retryAfterMs,omitempty"`
	LastResult                string  `json:"lastResult,omitempty"`
	LastError                 string  `json:"lastError,omitempty"`

	accountCountObserved bool
	quoteObserved        bool
}

func lowPriceReserveLadder(target int) []int {
	if target <= 0 {
		return nil
	}
	type remainder struct {
		index int
		value int
	}
	quantities := make([]int, len(lowPriceReserveLadderWeights))
	remainders := make([]remainder, len(lowPriceReserveLadderWeights))
	allocated := 0
	for index, weight := range lowPriceReserveLadderWeights {
		scaled := target * weight
		quantities[index] = scaled / 100
		allocated += quantities[index]
		remainders[index] = remainder{index: index, value: scaled % 100}
	}
	sort.SliceStable(remainders, func(i, j int) bool {
		if remainders[i].value != remainders[j].value {
			return remainders[i].value > remainders[j].value
		}
		return remainders[i].index < remainders[j].index
	})
	for remaining := target - allocated; remaining > 0; remaining-- {
		quantities[remainders[(target-allocated-remaining)%len(remainders)].index]++
	}
	result := make([]int, 0, len(quantities))
	for _, quantity := range quantities {
		if quantity > 0 {
			result = append(result, quantity)
		}
	}
	return result
}

func lowPriceReserveNextStageQuantity(target int, reserveAccounts int) int {
	reserveAccounts = max(0, reserveAccounts)
	cumulative := 0
	for _, quantity := range lowPriceReserveLadder(target) {
		cumulative += quantity
		if reserveAccounts < cumulative {
			return cumulative - reserveAccounts
		}
	}
	return 0
}

func lowPriceReserveCheckInterval(cfg store.ManagerSupplyConfig) time.Duration {
	milliseconds := cfg.LowPriceReserveCheckIntervalMilliseconds
	if milliseconds <= 0 {
		milliseconds = 1000
	}
	milliseconds = clampInt(milliseconds, 250, 600000)
	return time.Duration(milliseconds) * time.Millisecond
}

func isLowPriceReserveAccount(file cpaauthfiles.File) bool {
	marker := mapFromMap(file.Raw, "cpamp_import")
	return strings.EqualFold(strings.TrimSpace(stringFromMap(marker, "method")), lowPriceReserveTriggerReason)
}

func countLowPriceReserveFiles(files []cpaauthfiles.File) int {
	count := 0
	for _, file := range files {
		if isLowPriceReserveAccount(file) && isAvailableCodexFile(file) {
			count++
		}
	}
	return count
}

func (s *Service) countLowPriceReserveAccounts(ctx context.Context, cfg store.ManagerConfig) (int, error) {
	snapshot, err := s.cachedAuthFiles(ctx, cfg, false)
	return countLowPriceReserveFiles(snapshot.files), err
}

func lowPriceReserveQuoteSnapshot(statuses []PlatformOverview) (int64, float64, string, bool) {
	bestKnown := -1
	bestUnknown := -1
	for index, status := range statuses {
		if status.Inventory == nil || status.Inventory.Available <= 0 || status.Inventory.EstimatedUnitPriceFen <= 0 {
			continue
		}
		if cost, known := platformOverviewCostMultiplier(status); known {
			if bestKnown < 0 {
				bestKnown = index
				continue
			}
			bestCost, _ := platformOverviewCostMultiplier(statuses[bestKnown])
			bestPrice := platformOverviewUnitPrice(statuses[bestKnown])
			if cost < bestCost || (cost == bestCost && status.Inventory.EstimatedUnitPriceFen < bestPrice) {
				bestKnown = index
			}
			continue
		}
		if bestUnknown < 0 || status.Inventory.EstimatedUnitPriceFen < platformOverviewUnitPrice(statuses[bestUnknown]) {
			bestUnknown = index
		}
	}
	selected := bestKnown
	if bestUnknown >= 0 && (bestKnown < 0 ||
		platformOverviewUnitPrice(statuses[bestUnknown]) < platformOverviewUnitPrice(statuses[bestKnown])) {
		selected = bestUnknown
	}
	if selected < 0 {
		return 0, 0, "", false
	}
	costMultiplier, _ := platformOverviewCostMultiplier(statuses[selected])
	return platformOverviewUnitPrice(statuses[selected]), costMultiplier, statuses[selected].ID, true
}

func lowPriceReserveExecutionFromConfig(cfg store.ManagerSupplyConfig) LowPriceReserveExecution {
	target := max(0, cfg.LowPriceReserveTargetAccounts)
	interval := lowPriceReserveCheckInterval(cfg)
	return LowPriceReserveExecution{
		Enabled:                   managerconfigsvc.SupplyEnabled(cfg) && supplyLowPriceReserveEnabled(cfg),
		TargetAccounts:            target,
		Gap:                       target,
		Ladder:                    lowPriceReserveLadder(target),
		NextStageQuantity:         lowPriceReserveNextStageQuantity(target, 0),
		CheckIntervalMilliseconds: int(interval / time.Millisecond),
		MaxUnitPriceFen:           valueOrZero(cfg.LowPriceReserveMaxUnitPriceFen),
	}
}

func isLowPriceReserveBackoffResult(result string) bool {
	switch strings.TrimSpace(result) {
	case lowPriceReserveExactQuoteBackoff, lowPriceReserveCatalogErrorBackoff:
		return true
	default:
		return false
	}
}

func (s *Service) beginLowPriceReserveBackoff(result string, backoffErr error, duration time.Duration) LowPriceReserveExecution {
	if s == nil {
		return LowPriceReserveExecution{}
	}
	if duration <= 0 {
		duration = lowPriceReserveRetryBackoff
	}
	now := time.Now()
	retryAfterMS := now.Add(duration).UnixMilli()
	s.stateMu.Lock()
	if retryAfterMS > s.lowPriceReserve.RetryAfterMS {
		s.lowPriceReserve.RetryAfterMS = retryAfterMS
	}
	s.lowPriceReserve.LastResult = strings.TrimSpace(result)
	if backoffErr != nil {
		s.lowPriceReserve.LastError = safeError(backoffErr)
	}
	execution := s.lowPriceReserve
	s.stateMu.Unlock()
	s.invalidateStatusCache()
	return execution
}

func (s *Service) lowPriceReserveBackoff(now time.Time) (LowPriceReserveExecution, bool) {
	if s == nil {
		return LowPriceReserveExecution{}, false
	}
	nowMS := now.UnixMilli()
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.lowPriceReserve.RetryAfterMS <= nowMS {
		if s.lowPriceReserve.RetryAfterMS > 0 {
			s.lowPriceReserve.RetryAfterMS = 0
			if isLowPriceReserveBackoffResult(s.lowPriceReserve.LastResult) {
				s.lowPriceReserve.LastResult = "scheduled"
				s.lowPriceReserve.LastError = ""
			}
		}
		return LowPriceReserveExecution{}, false
	}
	return s.lowPriceReserve, true
}

func (s *Service) clearLowPriceReserveBackoff() {
	if s == nil {
		return
	}
	s.stateMu.Lock()
	changed := s.lowPriceReserve.RetryAfterMS != 0 || isLowPriceReserveBackoffResult(s.lowPriceReserve.LastResult)
	s.lowPriceReserve.RetryAfterMS = 0
	if isLowPriceReserveBackoffResult(s.lowPriceReserve.LastResult) {
		s.lowPriceReserve.LastResult = "scheduled"
		s.lowPriceReserve.LastError = ""
	}
	s.stateMu.Unlock()
	if changed {
		s.invalidateStatusCache()
	}
}

func (s *Service) RunLowPriceReserve(ctx context.Context) (LowPriceReserveExecution, error) {
	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return LowPriceReserveExecution{}, err
	}
	execution := lowPriceReserveExecutionFromConfig(cfg.Supply)
	if !execution.Enabled {
		execution.LastResult = "disabled"
		return execution, nil
	}

	reserveAccounts, err := s.countLowPriceReserveAccounts(ctx, cfg)
	execution.ReserveAccounts = reserveAccounts
	execution.Gap = max(0, execution.TargetAccounts-reserveAccounts)
	execution.NextStageQuantity = lowPriceReserveNextStageQuantity(execution.TargetAccounts, reserveAccounts)
	execution.accountCountObserved = err == nil
	if err != nil {
		return execution, err
	}
	if execution.NextStageQuantity <= 0 {
		execution.LastResult = "target_reached"
		return execution, nil
	}

	// Only hold the shared supply mutex while inspecting or mutating durable
	// task state. Supplier quoting runs outside it so a normal replenishment can
	// start immediately and win the second admission check below.
	if !s.runMu.TryLock() {
		execution.LastResult = "busy"
		return execution, nil
	}
	active, found, err := s.store.GetActiveAutomaticSupplyPurchaseTask(ctx)
	s.runMu.Unlock()
	if err != nil {
		return execution, err
	}
	if found {
		execution.ActiveTaskID = active.TaskID
	}
	if backoff, coolingDown := s.lowPriceReserveBackoff(time.Now()); coolingDown {
		execution.RetryAfterMS = backoff.RetryAfterMS
		execution.LastError = backoff.LastError
		if found {
			if isLowPriceReserveTrigger(active.TriggerReason) {
				execution.LastResult = "active_task"
			} else {
				execution.LastResult = "normal_task_active"
			}
		} else {
			execution.LastResult = backoff.LastResult
		}
		return execution, nil
	}

	selection, matched, err := s.selectLowPriceReserveCatalogPlatform(ctx, cfg.Supply, execution.NextStageQuantity)
	if price, costMultiplier, platformID, observed := lowPriceReserveQuoteSnapshot(selection.all); observed {
		execution.LastQuotedUnitPriceFen = price
		execution.LastQuotedCostMultiplier = costMultiplier
		execution.SelectedPlatformID = platformID
		execution.quoteObserved = true
	}
	if err != nil {
		backoff := s.beginLowPriceReserveBackoff(lowPriceReserveCatalogErrorBackoff, err, lowPriceReserveRetryBackoff)
		execution.RetryAfterMS = backoff.RetryAfterMS
		execution.LastError = safeError(err)
		if found {
			// A transient catalog failure must not hide the task that is already
			// being delivered. Keep its state visible and retry the quote next tick.
			if isLowPriceReserveTrigger(active.TriggerReason) {
				execution.LastResult = "active_task"
			} else {
				execution.LastResult = "normal_task_active"
			}
			return execution, nil
		}
		execution.LastResult = lowPriceReserveCatalogErrorBackoff
		return execution, err
	}
	if found {
		// Keep the displayed quote current while an earlier task is waiting for
		// delivery. The task worker performs its own exact re-quote before paying,
		// so this observation never creates a second order or changes the charge.
		if isLowPriceReserveTrigger(active.TriggerReason) {
			execution.LastResult = "active_task"
		} else {
			execution.LastResult = "normal_task_active"
		}
		return execution, nil
	}
	if !matched || selection.status.Inventory == nil {
		execution.LastResult = "price_wait"
		return execution, nil
	}
	execution.LastQuotedUnitPriceFen = selection.status.Inventory.EstimatedUnitPriceFen
	execution.LastQuotedCostMultiplier, _ = platformOverviewCostMultiplier(selection.status)
	execution.SelectedPlatformID = selection.platform.ID
	execution.quoteObserved = true
	quantity := min(execution.NextStageQuantity, selection.status.Inventory.Available)
	if selection.quantity > 0 {
		quantity = min(quantity, selection.quantity)
	}
	if quantity <= 0 {
		execution.LastResult = "price_wait"
		return execution, nil
	}

	if !s.runMu.TryLock() {
		execution.LastResult = "busy"
		return execution, nil
	}
	defer s.runMu.Unlock()
	active, found, err = s.store.GetActiveAutomaticSupplyPurchaseTask(ctx)
	if err != nil {
		return execution, err
	}
	if found {
		execution.ActiveTaskID = active.TaskID
		if isLowPriceReserveTrigger(active.TriggerReason) {
			execution.LastResult = "active_task"
		} else {
			execution.LastResult = "normal_task_active"
		}
		return execution, nil
	}
	task, err := s.upsertAutomaticPurchaseTask(ctx, store.SupplyPurchaseTask{
		Source:              "automatic",
		Product:             selection.platform.Product,
		TargetQuantity:      quantity,
		Status:              purchaseTaskStatusPending,
		Strategy:            supplyOrderStrategy(cfg.Supply, true),
		TriggerReason:       lowPriceReserveTriggerReason,
		MaxConcurrentOrders: 1,
	})
	if err != nil {
		return execution, err
	}
	execution.ActiveTaskID = task.TaskID
	if isLowPriceReserveTrigger(task.TriggerReason) {
		execution.LastResult = "task_created"
		s.signalPurchaseTaskWorker()
	} else {
		execution.LastResult = "normal_task_active"
	}
	return execution, nil
}

func (s *Service) NextLowPriceReserveInterval(ctx context.Context) time.Duration {
	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil || !managerconfigsvc.SupplyEnabled(cfg.Supply) || !supplyLowPriceReserveEnabled(cfg.Supply) {
		return 5 * time.Second
	}
	interval := lowPriceReserveCheckInterval(cfg.Supply)
	if backoff, coolingDown := s.lowPriceReserveBackoff(time.Now()); coolingDown {
		remaining := time.Until(time.UnixMilli(backoff.RetryAfterMS))
		if remaining > interval {
			return remaining
		}
	}
	return interval
}

func (s *Service) ScheduleLowPriceReserveExecution(at time.Time) {
	if s == nil {
		return
	}
	s.stateMu.Lock()
	s.lowPriceReserve.NextCheckAtMS = at.UnixMilli()
	s.stateMu.Unlock()
	s.invalidateStatusCache()
}

func (s *Service) SetLowPriceReserveRunning(running bool) {
	if s == nil {
		return
	}
	s.stateMu.Lock()
	s.lowPriceReserve.Running = running
	s.stateMu.Unlock()
}

func (s *Service) RecordLowPriceReserveExecution(
	execution LowPriceReserveExecution,
	finishedAt time.Time,
	nextAt time.Time,
	err error,
) {
	if s == nil {
		return
	}
	s.stateMu.Lock()
	previous := s.lowPriceReserve
	if previous.RetryAfterMS > finishedAt.UnixMilli() && previous.RetryAfterMS > execution.RetryAfterMS {
		execution.RetryAfterMS = previous.RetryAfterMS
		execution.LastResult = previous.LastResult
		execution.LastError = previous.LastError
	}
	if !execution.accountCountObserved {
		execution.ReserveAccounts = previous.ReserveAccounts
		execution.Gap = max(0, execution.TargetAccounts-execution.ReserveAccounts)
		execution.NextStageQuantity = lowPriceReserveNextStageQuantity(execution.TargetAccounts, execution.ReserveAccounts)
	}
	if !execution.quoteObserved {
		execution.LastQuotedUnitPriceFen = previous.LastQuotedUnitPriceFen
		execution.LastQuotedCostMultiplier = previous.LastQuotedCostMultiplier
		execution.SelectedPlatformID = previous.SelectedPlatformID
	}
	execution.Running = false
	execution.LastCheckedAtMS = finishedAt.UnixMilli()
	execution.NextCheckAtMS = nextAt.UnixMilli()
	if err != nil {
		if !isLowPriceReserveBackoffResult(execution.LastResult) {
			execution.LastResult = "failed"
		}
		execution.LastError = safeError(err)
	} else if execution.RetryAfterMS <= finishedAt.UnixMilli() {
		execution.LastError = ""
	}
	s.lowPriceReserve = execution
	s.stateMu.Unlock()
	s.invalidateStatusCache()
}

func (s *Service) currentLowPriceReserveExecution(
	cfg store.ManagerSupplyConfig,
	tasks []store.SupplyPurchaseTask,
) LowPriceReserveExecution {
	configured := lowPriceReserveExecutionFromConfig(cfg)
	s.stateMu.RLock()
	execution := s.lowPriceReserve
	s.stateMu.RUnlock()
	execution.Enabled = configured.Enabled
	execution.TargetAccounts = configured.TargetAccounts
	execution.Ladder = configured.Ladder
	execution.CheckIntervalMilliseconds = configured.CheckIntervalMilliseconds
	execution.MaxUnitPriceFen = configured.MaxUnitPriceFen
	execution.Gap = max(0, execution.TargetAccounts-execution.ReserveAccounts)
	execution.NextStageQuantity = lowPriceReserveNextStageQuantity(execution.TargetAccounts, execution.ReserveAccounts)
	execution.ActiveTaskID = ""
	for _, task := range tasks {
		if (task.Status == purchaseTaskStatusPending || task.Status == purchaseTaskStatusRunning) &&
			isLowPriceReserveTrigger(task.TriggerReason) {
			execution.ActiveTaskID = task.TaskID
			break
		}
	}
	if !execution.Enabled {
		execution.Running = false
		execution.NextCheckAtMS = 0
		execution.RetryAfterMS = 0
		if execution.LastResult == "" || execution.LastResult == "scheduled" {
			execution.LastResult = "disabled"
		}
	}
	if execution.RetryAfterMS > 0 && execution.RetryAfterMS <= time.Now().UnixMilli() {
		execution.RetryAfterMS = 0
		if isLowPriceReserveBackoffResult(execution.LastResult) {
			execution.LastResult = "scheduled"
			execution.LastError = ""
		}
	}
	return execution
}
