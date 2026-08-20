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

type LowPriceReserveExecution struct {
	Enabled                   bool   `json:"enabled"`
	Running                   bool   `json:"running"`
	ReserveAccounts           int    `json:"reserveAccounts"`
	TargetAccounts            int    `json:"targetAccounts"`
	Gap                       int    `json:"gap"`
	Ladder                    []int  `json:"ladder,omitempty"`
	NextStageQuantity         int    `json:"nextStageQuantity"`
	CheckIntervalMilliseconds int    `json:"checkIntervalMilliseconds"`
	MaxUnitPriceFen           int64  `json:"maxUnitPriceFen"`
	LastCheckedAtMS           int64  `json:"lastCheckedAtMs,omitempty"`
	NextCheckAtMS             int64  `json:"nextCheckAtMs,omitempty"`
	LastQuotedUnitPriceFen    int64  `json:"lastQuotedUnitPriceFen,omitempty"`
	SelectedPlatformID        string `json:"selectedPlatformId,omitempty"`
	ActiveTaskID              string `json:"activeTaskId,omitempty"`
	LastResult                string `json:"lastResult,omitempty"`
	LastError                 string `json:"lastError,omitempty"`

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

func lowPriceReserveQuoteSnapshot(statuses []PlatformOverview) (int64, string, bool) {
	var price int64
	platformID := ""
	observed := false
	for _, status := range statuses {
		if status.Inventory == nil || status.Inventory.Available <= 0 || status.Inventory.EstimatedUnitPriceFen <= 0 {
			continue
		}
		if !observed || status.Inventory.EstimatedUnitPriceFen < price {
			price = status.Inventory.EstimatedUnitPriceFen
			platformID = status.ID
			observed = true
		}
	}
	return price, platformID, observed
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
		if isLowPriceReserveTrigger(active.TriggerReason) {
			execution.LastResult = "active_task"
		} else {
			execution.LastResult = "normal_task_active"
		}
		return execution, nil
	}

	selection, matched, err := s.selectLowPriceReserveCatalogPlatform(ctx, cfg.Supply, execution.NextStageQuantity)
	if price, platformID, observed := lowPriceReserveQuoteSnapshot(selection.all); observed {
		execution.LastQuotedUnitPriceFen = price
		execution.SelectedPlatformID = platformID
		execution.quoteObserved = true
	}
	if err != nil {
		return execution, err
	}
	if !matched || selection.status.Inventory == nil {
		execution.LastResult = "price_wait"
		return execution, nil
	}
	execution.LastQuotedUnitPriceFen = selection.status.Inventory.EstimatedUnitPriceFen
	execution.SelectedPlatformID = selection.platform.ID
	execution.quoteObserved = true
	quantity := min(execution.NextStageQuantity, selection.status.Inventory.Available)
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
	return lowPriceReserveCheckInterval(cfg.Supply)
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
	if !execution.accountCountObserved {
		execution.ReserveAccounts = previous.ReserveAccounts
		execution.Gap = max(0, execution.TargetAccounts-execution.ReserveAccounts)
		execution.NextStageQuantity = lowPriceReserveNextStageQuantity(execution.TargetAccounts, execution.ReserveAccounts)
	}
	if !execution.quoteObserved {
		execution.LastQuotedUnitPriceFen = previous.LastQuotedUnitPriceFen
		execution.SelectedPlatformID = previous.SelectedPlatformID
	}
	execution.Running = false
	execution.LastCheckedAtMS = finishedAt.UnixMilli()
	execution.NextCheckAtMS = nextAt.UnixMilli()
	if err != nil {
		execution.LastResult = "failed"
		execution.LastError = safeError(err)
	} else {
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
		if execution.LastResult == "" || execution.LastResult == "scheduled" {
			execution.LastResult = "disabled"
		}
	}
	return execution
}
