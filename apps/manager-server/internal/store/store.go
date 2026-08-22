package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/config"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/accountaction"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/apikeyalias"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/codexinspection"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/codexquotaoperation"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/containeropsaudit"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/containeropsupgrade"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/datamigration"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/deadletter"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/modelprice"
	mysqlrepo "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/mysql"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/quotacooldown"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/quotasnapshot"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/setting"
	sqliterepo "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/sqlite"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/supplyorder"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/supplyrecovery"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/supplytask"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/usageaggregate"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/usageevent"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/usagemonitoring"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/usagepricing"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/usagerollup"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/security"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/usage"
)

type Setup = model.Setup
type ManagerConfig = model.ManagerConfig
type AdminCredential = model.AdminCredential
type BootstrapState = model.BootstrapState
type ManagerCPAConnectionConfig = model.ManagerCPAConnectionConfig
type ManagerCollectorConfig = model.ManagerCollectorConfig
type ManagerCodexInspectionConfig = model.ManagerCodexInspectionConfig
type ManagerCodexInspectionScheduleConfig = model.ManagerCodexInspectionScheduleConfig
type ManagerExternalUsageServiceConfig = model.ManagerExternalUsageServiceConfig
type ManagerSupplyConfig = model.ManagerSupplyConfig
type ManagerSupplyPlatformConfig = model.ManagerSupplyPlatformConfig
type ManagerSupplyQuotaEstimationPolicy = model.ManagerSupplyQuotaEstimationPolicy
type SupplyOrder = model.SupplyOrder
type SupplyPurchaseTask = model.SupplyPurchaseTask
type SupplyImportItem = model.SupplyImportItem
type SupplyRecovery = model.SupplyRecovery
type SupplyRecoveryImportItem = model.SupplyRecoveryImportItem
type CodexInspectionRun = model.CodexInspectionRun
type CodexInspectionResult = model.CodexInspectionResult
type CodexInspectionQuotaWindow = model.CodexInspectionQuotaWindow
type CodexInspectionLog = model.CodexInspectionLog
type ContainerOpsAuditEntry = model.ContainerOpsAuditEntry
type ContainerOpsUpgradeTask = model.ContainerOpsUpgradeTask
type CodexInspectionDisableOwnership = model.CodexInspectionDisableOwnership
type CodexInspectionLease = model.CodexInspectionLease
type InsertResult = model.InsertResult
type ModelPrice = model.ModelPrice
type ModelPriceContextTier = model.ModelPriceContextTier
type ModelPriceServiceTier = model.ModelPriceServiceTier
type ModelPriceSyncResult = model.ModelPriceSyncResult
type ModelUsageStat = model.ModelUsageStat
type ModelUsageSummary = model.ModelUsageSummary
type APIKeyAlias = model.APIKeyAlias
type QuotaCooldown = model.QuotaCooldown
type QuotaCooldownUpsert = model.QuotaCooldownUpsert
type AccountQuotaSnapshot = model.AccountQuotaSnapshot
type AccountActionCandidate = model.AccountActionCandidate
type AccountActionCandidateUpsert = model.AccountActionCandidateUpsert
type AutomationSettings = model.AutomationSettings
type DataMigrationState = datamigration.State
type DataMigrationBatchResult = datamigration.BatchResult

var DefaultCodexInspectionConfig = model.DefaultCodexInspectionConfig
var NormalizeCodexInspectionConfig = model.NormalizeCodexInspectionConfig

// Aggregation result types re-exported for service-layer consumers.
type Aggregate = usageevent.Aggregate
type ModelStat = usageevent.ModelStat
type RecentFailure = usageevent.RecentFailure
type AnalyticsFilter = usageevent.AnalyticsFilter
type TimelinePoint = usageevent.TimelinePoint
type LatencyPercentiles = usageevent.LatencyPercentiles
type LatencySummary = usageevent.LatencySummary
type HourlyPoint = usageevent.HourlyPoint
type FilterOptionValues = usageevent.FilterOptionValues
type FilterSelectorValues = usageevent.FilterSelectorValues
type HeatmapPoint = usageevent.HeatmapPoint
type ChannelModelStat = usageevent.ChannelModelStat
type FailureSourceStat = usageevent.FailureSourceStat
type AccountModelStat = usageevent.AccountModelStat
type CredentialModelStat = usageevent.CredentialModelStat
type CredentialTimelinePoint = usageevent.CredentialTimelinePoint
type APIKeyTimelinePoint = usageevent.APIKeyTimelinePoint
type APIKeyModelStat = usageevent.APIKeyModelStat
type TaskBucket = usageevent.TaskBucket
type EventPageItem = usageevent.EventPageItem
type EventsPage = usageevent.EventsPage
type HeaderSnapshot = usageevent.HeaderSnapshot
type SupplyUsageMinute = usageevent.SupplyUsageMinute
type SupplyQuotaWindowUsageQuery = usageevent.SupplyQuotaWindowUsageQuery
type SupplyQuotaWindowUsage = usageevent.SupplyQuotaWindowUsage
type AccountWindowUsageQuery = usageevent.AccountWindowUsageQuery
type AccountWindowModelStat = usageevent.AccountWindowModelStat
type LatestAccountRequestQuery = usageevent.LatestAccountRequestQuery
type LatestAccountRequest = usageevent.LatestAccountRequest
type RoutingDiagnostics = usageevent.RoutingDiagnostics
type RoutingDiagnosticCount = usageevent.RoutingDiagnosticCount
type UsageRollupCheckpoint = usagerollup.Checkpoint
type UsageRollupCatchUpResult = usagerollup.CatchUpResult
type AccountHistoryRollupRow = usagerollup.AccountHistoryRow
type DashboardHourlyRollupRow = usagerollup.DashboardHourlyRow
type UsageHourlyAggregateState = usageaggregate.State
type UsageHourlyAggregateCatchUpResult = usageaggregate.CatchUpResult
type UsageHourlyAggregateFilter = usageaggregate.Filter
type UsageHourlyAggregateRow = usageaggregate.Row
type UsagePricingState = usagepricing.State
type UsagePricingCatchUpResult = usagepricing.CatchUpResult
type UsagePricingHourlyFilter = usagepricing.HourlyFilter
type UsagePricingHourlyRow = usagepricing.HourlyRow
type UsagePricingAccountRow = usagepricing.AccountRow
type UsageMonitoringState = usagemonitoring.State
type UsageMonitoringCatchUpResult = usagemonitoring.CatchUpResult

type UsageHourlyPricingSnapshot struct {
	AggregateRows      []UsageHourlyAggregateRow
	AggregateState     UsageHourlyAggregateState
	AggregateAvailable bool
	PricingRows        []UsagePricingHourlyRow
	PricingState       UsagePricingState
	PricingAvailable   bool
	Prices             map[string]ModelPrice
}

type UsagePricingAccountSnapshot struct {
	Rows      []UsagePricingAccountRow
	State     UsagePricingState
	Available bool
	Prices    map[string]ModelPrice
}

type Store struct {
	db                   *sql.DB
	driver               string
	modelPricesMu        sync.RWMutex
	Settings             setting.Repository
	UsageEvents          usageevent.Repository
	DeadLetters          deadletter.Repository
	ModelPrices          modelprice.Repository
	APIKeyAliases        apikeyalias.Repository
	AccountActions       accountaction.Repository
	CodexInspections     codexinspection.Repository
	CodexQuotaOperations codexquotaoperation.Repository
	DataMigrations       datamigration.Repository
	QuotaCooldowns       quotacooldown.Repository
	QuotaSnapshots       quotasnapshot.Repository
	UsageAggregates      usageaggregate.Repository
	UsagePricing         usagepricing.Repository
	UsageMonitoring      usagemonitoring.Repository
	UsageRollups         usagerollup.Repository
	ContainerOpsAudits   containeropsaudit.Repository
	ContainerOpsUpgrades containeropsupgrade.Repository
	SupplyOrders         supplyorder.Repository
	SupplyTasks          supplytask.Repository
	SupplyRecoveries     supplyrecovery.Repository
}

func Open(path string, protector ...*security.Protector) (*Store, error) {
	db, err := sqliterepo.Open(path)
	if err != nil {
		return nil, err
	}
	st := New(db, protector...)
	st.driver = "sqlite"
	return st, nil
}

func OpenConfig(cfg config.Config, protector ...*security.Protector) (*Store, error) {
	if strings.EqualFold(strings.TrimSpace(cfg.DBDriver), "mysql") {
		if strings.TrimSpace(cfg.DBDSN) == "" {
			return nil, fmt.Errorf("USAGE_DB_DSN is required when USAGE_DB_DRIVER=mysql")
		}
		db, err := mysqlrepo.Open(cfg.DBDSN)
		if err != nil {
			return nil, err
		}
		st := New(db, protector...)
		st.driver = "mysql"
		return st, nil
	}
	return Open(cfg.DBPath, protector...)
}

func New(db *sql.DB, protector ...*security.Protector) *Store {
	return &Store{
		db:                   db,
		Settings:             setting.New(db, protector...),
		UsageEvents:          usageevent.New(db),
		DeadLetters:          deadletter.New(db),
		ModelPrices:          modelprice.New(db),
		APIKeyAliases:        apikeyalias.New(db),
		AccountActions:       accountaction.New(db),
		CodexInspections:     codexinspection.New(db),
		CodexQuotaOperations: codexquotaoperation.New(db),
		DataMigrations:       datamigration.New(db),
		QuotaCooldowns:       quotacooldown.New(db),
		QuotaSnapshots:       quotasnapshot.New(db),
		UsageAggregates:      usageaggregate.New(db),
		UsagePricing:         usagepricing.New(db),
		UsageMonitoring:      usagemonitoring.New(db),
		UsageRollups:         usagerollup.New(db),
		ContainerOpsAudits:   containeropsaudit.New(db),
		ContainerOpsUpgrades: containeropsupgrade.New(db),
		SupplyOrders:         supplyorder.New(db, protector...),
		SupplyTasks:          supplytask.New(db),
		SupplyRecoveries:     supplyrecovery.New(db, protector...),
	}
}

func (s *Store) Driver() string {
	if s == nil || strings.TrimSpace(s.driver) == "" {
		return "sqlite"
	}
	return s.driver
}

// SQLDB exposes the shared database handle to infrastructure services such as
// database health reporting and cross-backend migration. Business services
// should continue using the typed repositories on Store.
func (s *Store) SQLDB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}

func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) SaveSetup(ctx context.Context, setup Setup) error {
	return s.Settings.SaveSetup(ctx, setup)
}

func (s *Store) LoadSetup(ctx context.Context) (Setup, bool, error) {
	return s.Settings.LoadSetup(ctx)
}

func (s *Store) SaveManagerConfig(ctx context.Context, cfg ManagerConfig) error {
	return s.Settings.SaveManagerConfig(ctx, cfg)
}

func (s *Store) LoadManagerConfig(ctx context.Context) (ManagerConfig, bool, error) {
	return s.Settings.LoadManagerConfig(ctx)
}

func (s *Store) SaveAutomationSettings(ctx context.Context, settings AutomationSettings) (AutomationSettings, error) {
	return s.Settings.SaveAutomationSettings(ctx, settings)
}

func (s *Store) LoadAutomationSettings(ctx context.Context) (AutomationSettings, bool, error) {
	return s.Settings.LoadAutomationSettings(ctx)
}

func (s *Store) SaveAdminCredential(ctx context.Context, credential AdminCredential) error {
	return s.Settings.SaveAdminCredential(ctx, credential)
}

func (s *Store) LoadAdminCredential(ctx context.Context) (AdminCredential, bool, error) {
	return s.Settings.LoadAdminCredential(ctx)
}

func (s *Store) SaveBootstrapState(ctx context.Context, state BootstrapState) error {
	return s.Settings.SaveBootstrapState(ctx, state)
}

func (s *Store) LoadBootstrapState(ctx context.Context) (BootstrapState, bool, error) {
	return s.Settings.LoadBootstrapState(ctx)
}

func (s *Store) HasHistoricalData(ctx context.Context) (bool, error) {
	return s.Settings.HasHistoricalData(ctx)
}

func (s *Store) LoadModelPrices(ctx context.Context) (map[string]ModelPrice, error) {
	return s.ModelPrices.LoadAll(ctx)
}

func (s *Store) SaveModelPrices(ctx context.Context, prices map[string]ModelPrice) error {
	s.modelPricesMu.Lock()
	defer s.modelPricesMu.Unlock()
	return s.ModelPrices.ReplaceAll(ctx, prices)
}

func (s *Store) UpsertSyncedModelPrices(ctx context.Context, prices map[string]ModelPrice) (ModelPriceSyncResult, error) {
	s.modelPricesMu.Lock()
	defer s.modelPricesMu.Unlock()
	return s.ModelPrices.UpsertSynced(ctx, prices)
}

// WithModelPriceSnapshot prevents model-price mutations while a service reads
// usage bands and applies the corresponding price book across multiple queries.
func (s *Store) WithModelPriceSnapshot(read func() error) error {
	s.modelPricesMu.RLock()
	defer s.modelPricesMu.RUnlock()
	return read()
}

func (s *Store) ModelUsageSummary(ctx context.Context, limit int) (ModelUsageSummary, error) {
	return s.UsageEvents.ModelUsageSummary(ctx, limit)
}

func (s *Store) LoadAPIKeyAliases(ctx context.Context) ([]APIKeyAlias, error) {
	return s.APIKeyAliases.LoadAll(ctx)
}

func (s *Store) UpsertAPIKeyAliases(ctx context.Context, aliases []APIKeyAlias) error {
	return s.APIKeyAliases.UpsertMany(ctx, aliases, nil, false)
}

func (s *Store) UpsertAPIKeyAliasesWithActiveHashes(ctx context.Context, aliases []APIKeyAlias, activeHashes []string, allowOrphanCleanup bool) error {
	return s.APIKeyAliases.UpsertMany(ctx, aliases, activeHashes, allowOrphanCleanup)
}

func (s *Store) DeleteAPIKeyAlias(ctx context.Context, apiKeyHash string) error {
	return s.APIKeyAliases.Delete(ctx, apiKeyHash)
}

func (s *Store) UpsertAccountActionCandidate(ctx context.Context, input AccountActionCandidateUpsert) (AccountActionCandidate, error) {
	return s.AccountActions.Upsert(ctx, input)
}

func (s *Store) ListAccountActionCandidates(ctx context.Context, status string, limit int) ([]AccountActionCandidate, error) {
	return s.AccountActions.List(ctx, status, limit)
}

func (s *Store) ListAccountActionCandidatesByAuthFiles(ctx context.Context, authFiles []string, limit int) ([]AccountActionCandidate, error) {
	return s.AccountActions.ListByAuthFiles(ctx, authFiles, limit)
}

func (s *Store) ListAccountActionCandidatesBetween(ctx context.Context, fromMS int64, toMS int64, limit int) ([]AccountActionCandidate, error) {
	return s.AccountActions.ListBetween(ctx, fromMS, toMS, limit)
}

func (s *Store) CountAccountActionCandidates(ctx context.Context, status string) (int64, error) {
	return s.AccountActions.Count(ctx, status)
}

func (s *Store) GetAccountActionCandidate(ctx context.Context, id int64) (AccountActionCandidate, bool, error) {
	return s.AccountActions.Get(ctx, id)
}

func (s *Store) UpdateAccountActionCandidateStatus(ctx context.Context, id int64, status string) (AccountActionCandidate, error) {
	return s.AccountActions.UpdateStatus(ctx, id, status)
}

func (s *Store) UpdatePendingAccountActionCandidateStatus(ctx context.Context, id int64, status string) (AccountActionCandidate, error) {
	return s.AccountActions.UpdatePendingStatus(ctx, id, status)
}

func (s *Store) RecordAccountActionCandidateFailure(ctx context.Context, id int64, reason string) error {
	return s.AccountActions.RecordFailure(ctx, id, reason)
}

func (s *Store) MarkAccountActionCandidateAutoDisabled(ctx context.Context, id int64, disabledAtMS int64) error {
	return s.AccountActions.MarkAutoDisabled(ctx, id, disabledAtMS)
}

func (s *Store) CreateCodexInspectionRun(ctx context.Context, run CodexInspectionRun) (CodexInspectionRun, error) {
	return s.CodexInspections.CreateRun(ctx, run)
}

func (s *Store) UpdateCodexInspectionRun(ctx context.Context, run CodexInspectionRun) error {
	return s.CodexInspections.UpdateRun(ctx, run)
}

func (s *Store) UpdateCodexInspectionRunProgress(ctx context.Context, run CodexInspectionRun, ownerID string) error {
	return s.CodexInspections.UpdateRunProgress(ctx, run, ownerID)
}

func (s *Store) AcquireCodexInspectionRun(ctx context.Context, run CodexInspectionRun, ownerID string, leaseDuration time.Duration) (codexinspection.AcquireRunResult, error) {
	return s.CodexInspections.AcquireRun(ctx, run, ownerID, leaseDuration)
}

func (s *Store) HeartbeatCodexInspectionRun(ctx context.Context, runID int64, ownerID string, leaseDuration time.Duration) error {
	return s.CodexInspections.HeartbeatRun(ctx, runID, ownerID, leaseDuration)
}

func (s *Store) MarkCodexInspectionRunCancelling(ctx context.Context, runID int64, ownerID string, reason string) (bool, error) {
	return s.CodexInspections.MarkRunCancelling(ctx, runID, ownerID, reason)
}

func (s *Store) FinalizeCodexInspectionRun(ctx context.Context, run CodexInspectionRun, ownerID string, finalLog *CodexInspectionLog) error {
	return s.CodexInspections.FinalizeRun(ctx, run, ownerID, finalLog)
}

func (s *Store) ForceFinalizeCodexInspectionRun(ctx context.Context, run CodexInspectionRun, ownerID string, finalLog *CodexInspectionLog) error {
	return s.CodexInspections.ForceFinalizeRun(ctx, run, ownerID, finalLog)
}

func (s *Store) GetActiveCodexInspectionLease(ctx context.Context, nowMS int64) (CodexInspectionLease, bool, error) {
	return s.CodexInspections.GetActiveLease(ctx, nowMS)
}

func (s *Store) RecoverStaleCodexInspectionRuns(ctx context.Context, nowMS int64, reason string) ([]CodexInspectionRun, error) {
	return s.CodexInspections.RecoverStaleRuns(ctx, nowMS, reason)
}

func (s *Store) GetLatestCodexInspectionRunByTriggerType(ctx context.Context, triggerType string) (CodexInspectionRun, bool, error) {
	return s.CodexInspections.GetLatestRunByTriggerType(ctx, triggerType)
}

func (s *Store) InsertCodexInspectionResult(ctx context.Context, result CodexInspectionResult) (CodexInspectionResult, error) {
	return s.CodexInspections.InsertResult(ctx, result)
}

func (s *Store) InsertCodexInspectionLog(ctx context.Context, entry CodexInspectionLog) (CodexInspectionLog, error) {
	return s.CodexInspections.InsertLog(ctx, entry)
}

func (s *Store) InsertCodexInspectionLogs(ctx context.Context, entries []CodexInspectionLog) ([]CodexInspectionLog, error) {
	if inserter, ok := s.CodexInspections.(interface {
		InsertLogs(context.Context, []CodexInspectionLog) ([]CodexInspectionLog, error)
	}); ok {
		return inserter.InsertLogs(ctx, entries)
	}
	stored := make([]CodexInspectionLog, 0, len(entries))
	for _, entry := range entries {
		item, err := s.CodexInspections.InsertLog(ctx, entry)
		if err != nil {
			return nil, err
		}
		stored = append(stored, item)
	}
	return stored, nil
}

func (s *Store) ListCodexInspectionRuns(ctx context.Context, limit int) ([]CodexInspectionRun, error) {
	return s.CodexInspections.ListRuns(ctx, limit)
}

func (s *Store) GetCodexInspectionRun(ctx context.Context, id int64) (CodexInspectionRun, bool, error) {
	return s.CodexInspections.GetRun(ctx, id)
}

func (s *Store) GetLatestCodexInspectionRunByTrigger(ctx context.Context, triggerType, triggerKey string) (CodexInspectionRun, bool, error) {
	return s.CodexInspections.GetLatestRunByTrigger(ctx, triggerType, triggerKey)
}

func (s *Store) ListCodexInspectionResults(ctx context.Context, runID int64) ([]CodexInspectionResult, error) {
	return s.CodexInspections.ListResults(ctx, runID)
}

func (s *Store) ListCodexInspectionLogs(ctx context.Context, runID int64) ([]CodexInspectionLog, error) {
	return s.CodexInspections.ListLogs(ctx, runID)
}

func (s *Store) CreateContainerOpsAudit(ctx context.Context, entry ContainerOpsAuditEntry) (ContainerOpsAuditEntry, error) {
	return s.ContainerOpsAudits.Create(ctx, entry)
}

func (s *Store) UpdateContainerOpsAudit(ctx context.Context, entry ContainerOpsAuditEntry) error {
	return s.ContainerOpsAudits.Update(ctx, entry)
}

func (s *Store) ListContainerOpsAudits(ctx context.Context, limit int) ([]ContainerOpsAuditEntry, error) {
	return s.ContainerOpsAudits.List(ctx, limit)
}

func (s *Store) CreateContainerOpsUpgradeTask(ctx context.Context, task ContainerOpsUpgradeTask) (ContainerOpsUpgradeTask, error) {
	return s.ContainerOpsUpgrades.Create(ctx, task)
}

func (s *Store) GetContainerOpsUpgradeTask(ctx context.Context, taskID string) (ContainerOpsUpgradeTask, bool, error) {
	return s.ContainerOpsUpgrades.Get(ctx, taskID)
}

func (s *Store) UpdateContainerOpsUpgradeTask(ctx context.Context, task ContainerOpsUpgradeTask) error {
	return s.ContainerOpsUpgrades.Update(ctx, task)
}

func (s *Store) ListContainerOpsUpgradeTasks(ctx context.Context, limit int) ([]ContainerOpsUpgradeTask, error) {
	return s.ContainerOpsUpgrades.List(ctx, limit)
}

func (s *Store) CreateSupplyOrder(ctx context.Context, order SupplyOrder) (SupplyOrder, error) {
	return s.SupplyOrders.Create(ctx, order)
}

func (s *Store) CreateSupplyPurchaseTask(ctx context.Context, task SupplyPurchaseTask) (SupplyPurchaseTask, error) {
	return s.SupplyTasks.Create(ctx, task)
}

func (s *Store) GetSupplyPurchaseTask(ctx context.Context, taskID string) (SupplyPurchaseTask, bool, error) {
	return s.SupplyTasks.Get(ctx, taskID)
}

func (s *Store) GetActiveAutomaticSupplyPurchaseTask(ctx context.Context) (SupplyPurchaseTask, bool, error) {
	return s.SupplyTasks.GetActiveAutomatic(ctx)
}

func (s *Store) UpdateSupplyPurchaseTask(ctx context.Context, task SupplyPurchaseTask) error {
	return s.SupplyTasks.Update(ctx, task)
}

func (s *Store) CancelSupplyPurchaseTask(ctx context.Context, taskID string, nowMS int64) (SupplyPurchaseTask, bool, error) {
	return s.SupplyTasks.Cancel(ctx, taskID, nowMS)
}

func (s *Store) ListSupplyPurchaseTasks(ctx context.Context, limit int) ([]SupplyPurchaseTask, error) {
	return s.SupplyTasks.List(ctx, limit)
}

func (s *Store) ListActiveSupplyPurchaseTasks(ctx context.Context, limit int) ([]SupplyPurchaseTask, error) {
	return s.SupplyTasks.ListActive(ctx, limit)
}

func (s *Store) GetSupplyOrder(ctx context.Context, orderID string) (SupplyOrder, bool, error) {
	return s.SupplyOrders.Get(ctx, orderID)
}

func (s *Store) GetOpenSupplyOrder(ctx context.Context) (SupplyOrder, bool, error) {
	return s.SupplyOrders.GetOpen(ctx)
}

func (s *Store) ListOpenSupplyOrders(ctx context.Context, limit int) ([]SupplyOrder, error) {
	return s.SupplyOrders.ListOpen(ctx, limit)
}

func (s *Store) GetLatestAutomaticSupplyOrder(ctx context.Context) (SupplyOrder, bool, error) {
	return s.SupplyOrders.GetLatestAutomatic(ctx)
}

func (s *Store) GetLatestCompletedAutomaticSupplyOrder(ctx context.Context) (SupplyOrder, bool, error) {
	return s.SupplyOrders.GetLatestCompletedAutomatic(ctx)
}

func (s *Store) ActivateNextLegacySupplyRepair(ctx context.Context) (SupplyOrder, bool, error) {
	return s.SupplyOrders.ActivateNextLegacyRepair(ctx)
}

func (s *Store) ActivateNextUnsupportedSupplyRelease(ctx context.Context) (SupplyOrder, bool, error) {
	return s.SupplyOrders.ActivateNextUnsupportedRelease(ctx)
}

func (s *Store) PromoteSupplyCreateAttempt(ctx context.Context, localOrderID string, order SupplyOrder) error {
	return s.SupplyOrders.PromoteCreateAttempt(ctx, localOrderID, order)
}

func (s *Store) ClaimSupplyOrderTaking(ctx context.Context, orderID string, nowMS int64, leaseUntilMS int64) (bool, error) {
	return s.SupplyOrders.ClaimTaking(ctx, orderID, nowMS, leaseUntilMS)
}

func (s *Store) UpdateSupplyOrder(ctx context.Context, order SupplyOrder) error {
	return s.SupplyOrders.Update(ctx, order)
}

func (s *Store) ListSupplyOrders(ctx context.Context, limit int) ([]SupplyOrder, error) {
	return s.SupplyOrders.List(ctx, limit)
}

func (s *Store) ListSupplyOrdersByTaskID(ctx context.Context, taskID string) ([]SupplyOrder, error) {
	return s.SupplyOrders.ListByTaskID(ctx, taskID)
}

func (s *Store) ListSupplyOrdersByTaskIDs(ctx context.Context, taskIDs []string) ([]SupplyOrder, error) {
	return s.SupplyOrders.ListByTaskIDs(ctx, taskIDs)
}

func (s *Store) ListSupplyOrdersByIDs(ctx context.Context, orderIDs []string) ([]SupplyOrder, error) {
	return s.SupplyOrders.ListByOrderIDs(ctx, orderIDs)
}

func (s *Store) ListSupplyOrdersBetween(ctx context.Context, fromMS int64, toMS int64, limit int) ([]SupplyOrder, error) {
	return s.SupplyOrders.ListBetween(ctx, fromMS, toMS, limit)
}

func (s *Store) ListMarketplaceSellerSupplyOrders(ctx context.Context, supplierID string, product string) ([]SupplyOrder, error) {
	return s.SupplyOrders.ListMarketplaceSellerOrders(ctx, supplierID, product)
}

func (s *Store) InsertSupplyImportItems(ctx context.Context, orderID string, items []SupplyImportItem) (int, error) {
	return s.SupplyOrders.InsertItems(ctx, orderID, items)
}

func (s *Store) ListSupplyImportItems(ctx context.Context, limit int, status string) ([]SupplyImportItem, error) {
	return s.SupplyOrders.ListItems(ctx, limit, status)
}

func (s *Store) ListSupplyImportItemsByOrderIDs(ctx context.Context, orderIDs []string) ([]SupplyImportItem, error) {
	return s.SupplyOrders.ListItemsByOrderIDs(ctx, orderIDs)
}

func (s *Store) ListSupplyImportItemsBetween(ctx context.Context, fromMS int64, toMS int64, limit int) ([]SupplyImportItem, error) {
	return s.SupplyOrders.ListItemsBetween(ctx, fromMS, toMS, limit)
}

func (s *Store) ListImportedSupplyItemsOverlapping(ctx context.Context, fromMS int64, toMS int64, limit int) ([]SupplyImportItem, error) {
	return s.SupplyOrders.ListImportedItemsOverlapping(ctx, fromMS, toMS, limit)
}

func (s *Store) ListPendingSupplyImportItems(ctx context.Context, orderID string, nowMS int64, limit int) ([]SupplyImportItem, error) {
	return s.SupplyOrders.ListPendingItems(ctx, orderID, nowMS, limit)
}

func (s *Store) ListActiveImportedSupplyItems(ctx context.Context, nowMS int64) ([]SupplyImportItem, error) {
	return s.SupplyOrders.ListActiveImportedItems(ctx, nowMS)
}

func (s *Store) ListCurrentImportedSupplyLeaseItems(ctx context.Context) ([]SupplyImportItem, error) {
	return s.SupplyOrders.ListCurrentImportedLeaseItems(ctx)
}

func (s *Store) ListCurrentImportedSupplyItems(ctx context.Context) ([]SupplyImportItem, error) {
	return s.SupplyOrders.ListCurrentImportedItems(ctx)
}

func (s *Store) MarkSupplyImportItemImported(ctx context.Context, id int64, importedAtMS int64) error {
	return s.SupplyOrders.MarkItemImported(ctx, id, importedAtMS)
}

func (s *Store) MarkSupplyImportItemFailed(ctx context.Context, id int64, lastError string, nextRetryAtMS int64) error {
	return s.SupplyOrders.MarkItemFailed(ctx, id, lastError, nextRetryAtMS)
}

func (s *Store) UpdateSupplyImportItemFileName(ctx context.Context, id int64, fileName string) error {
	return s.SupplyOrders.UpdateItemFileName(ctx, id, fileName)
}

func (s *Store) UpdateSupplyImportItemPlan(ctx context.Context, id int64, accountName string, nameKey string, fileName string, importAction string, replacedFileName string) error {
	return s.SupplyOrders.UpdateItemImportPlan(ctx, id, accountName, nameKey, fileName, importAction, replacedFileName)
}

func (s *Store) UpdateSupplyImportItemAccountMetadata(ctx context.Context, id int64, accountName string, nameKey string) error {
	return s.SupplyOrders.UpdateItemAccountMetadata(ctx, id, accountName, nameKey)
}

func (s *Store) UpdateSupplyImportItemWarrantyMetadata(ctx context.Context, id int64, leaseExpiresAtMS int64, warrantyExpiresAtMS int64) error {
	return s.SupplyOrders.UpdateItemWarrantyMetadata(ctx, id, leaseExpiresAtMS, warrantyExpiresAtMS)
}

func (s *Store) UpdateSupplyImportItemQuotaCapacity(ctx context.Context, id int64, capacityM float64, observedAtMS int64, complete bool) error {
	return s.SupplyOrders.UpdateItemQuotaCapacity(ctx, id, capacityM, observedAtMS, complete)
}

func (s *Store) ListSupplyImportItemsMissingAccountMetadata(ctx context.Context, limit int) ([]SupplyImportItem, error) {
	return s.SupplyOrders.ListItemsMissingAccountMetadata(ctx, limit)
}

func (s *Store) ListCurrentSupplyImportItemsByItemKey(ctx context.Context, itemKey string) ([]SupplyImportItem, error) {
	return s.SupplyOrders.ListCurrentItemsByItemKey(ctx, itemKey)
}

func (s *Store) ListCurrentSupplyImportItemsByNameKey(ctx context.Context, nameKey string) ([]SupplyImportItem, error) {
	return s.SupplyOrders.ListCurrentItemsByNameKey(ctx, nameKey)
}

func (s *Store) SupplyImportCounts(ctx context.Context, orderID string) (int, int, error) {
	return s.SupplyOrders.Counts(ctx, orderID)
}

func (s *Store) UpsertSupplyRecoveries(ctx context.Context, recoveries []SupplyRecovery) (int, error) {
	return s.SupplyRecoveries.UpsertMany(ctx, recoveries)
}

func (s *Store) GetSupplyRecovery(ctx context.Context, recoveryID string) (SupplyRecovery, bool, error) {
	return s.SupplyRecoveries.Get(ctx, recoveryID)
}

func (s *Store) GetSupplyRecoveryByClaimOrder(ctx context.Context, claimOrderID string) (SupplyRecovery, bool, error) {
	return s.SupplyRecoveries.GetByClaimOrder(ctx, claimOrderID)
}

func (s *Store) ListSupplyRecoveries(ctx context.Context, limit int, status string) ([]SupplyRecovery, error) {
	return s.SupplyRecoveries.List(ctx, limit, status)
}

func (s *Store) ListSupplyRecoveriesBetween(ctx context.Context, fromMS int64, toMS int64, limit int) ([]SupplyRecovery, error) {
	return s.SupplyRecoveries.ListBetween(ctx, fromMS, toMS, limit)
}

func (s *Store) ListClaimableSupplyRecoveries(ctx context.Context, limit int) ([]SupplyRecovery, error) {
	return s.SupplyRecoveries.ListClaimable(ctx, limit)
}

func (s *Store) ListImportPendingSupplyRecoveries(ctx context.Context, limit int) ([]SupplyRecovery, error) {
	return s.SupplyRecoveries.ListImportPending(ctx, limit)
}

func (s *Store) ClaimSupplyRecoveryForProcessing(ctx context.Context, recoveryID string, nowMS int64) (SupplyRecovery, bool, error) {
	return s.SupplyRecoveries.ClaimForProcessing(ctx, recoveryID, nowMS)
}

func (s *Store) PersistSupplyRecoveryClaim(ctx context.Context, recovery SupplyRecovery, order SupplyOrder, items []SupplyImportItem, claimedAtMS int64) error {
	return s.SupplyRecoveries.PersistClaim(ctx, recovery, order, items, claimedAtMS)
}

func (s *Store) ResetSupplyRecoveryImport(ctx context.Context, recoveryID string, nowMS int64) (bool, error) {
	return s.SupplyRecoveries.ResetImport(ctx, recoveryID, nowMS)
}

func (s *Store) MarkSupplyRecoveryClaimed(ctx context.Context, recoveryID string, claimOrderID string, itemCount int, claimedAtMS int64) error {
	return s.SupplyRecoveries.MarkClaimed(ctx, recoveryID, claimOrderID, itemCount, claimedAtMS)
}

func (s *Store) MarkSupplyRecoveryImportProgress(ctx context.Context, recoveryID string, itemCount int, importedCount int, lastError string) error {
	return s.SupplyRecoveries.MarkImportProgress(ctx, recoveryID, itemCount, importedCount, lastError)
}

func (s *Store) MarkSupplyRecoveryImported(ctx context.Context, recoveryID string, importedCount int) error {
	return s.SupplyRecoveries.MarkImported(ctx, recoveryID, importedCount)
}

func (s *Store) MarkSupplyRecoveryRefunded(ctx context.Context, recoveryID string, refundedFen int64) error {
	return s.SupplyRecoveries.MarkRefunded(ctx, recoveryID, refundedFen)
}

func (s *Store) MarkSupplyRecoveryFailed(ctx context.Context, recoveryID string, lastError string) error {
	return s.SupplyRecoveries.MarkFailed(ctx, recoveryID, lastError)
}

func (s *Store) SetSupplyRecoveryLastError(ctx context.Context, recoveryID string, lastError string) error {
	return s.SupplyRecoveries.SetLastError(ctx, recoveryID, lastError)
}

func (s *Store) SupplyRecoverySummary(ctx context.Context) (supplyrecovery.Summary, error) {
	return s.SupplyRecoveries.Summary(ctx)
}

func (s *Store) ListCodexInspectionDisableOwnership(ctx context.Context) ([]CodexInspectionDisableOwnership, error) {
	return s.CodexInspections.ListDisableOwnership(ctx)
}

func (s *Store) UpsertCodexInspectionDisableOwnership(ctx context.Context, item CodexInspectionDisableOwnership) error {
	return s.CodexInspections.UpsertDisableOwnership(ctx, item)
}

func (s *Store) UpsertCodexInspectionDisableOwnerships(ctx context.Context, items []CodexInspectionDisableOwnership) error {
	return s.CodexInspections.UpsertDisableOwnerships(ctx, items)
}

func (s *Store) DeleteCodexInspectionDisableOwnership(ctx context.Context, target model.CodexInspectionDisableOwnershipTarget) error {
	return s.CodexInspections.DeleteDisableOwnership(ctx, target)
}

func (s *Store) RevokeCodexInspectionDisableOwnership(ctx context.Context, targets []model.CodexInspectionDisableOwnershipTarget, clearAll bool) ([]CodexInspectionDisableOwnership, error) {
	return s.CodexInspections.RevokeDisableOwnership(ctx, targets, clearAll)
}

func (s *Store) RestoreCodexInspectionDisableOwnership(ctx context.Context, items []CodexInspectionDisableOwnership) error {
	return s.CodexInspections.RestoreDisableOwnership(ctx, items)
}

func (s *Store) InsertEvents(ctx context.Context, events []usage.Event) (InsertResult, error) {
	return s.UsageEvents.InsertBatch(ctx, events)
}

// ListSupplyUsageMinutes reads only the current short rolling window as
// aggregate minute buckets. Smart replenishment uses this once during startup
// to preserve its demand window across a Manager restart; request and UI paths
// continue to read the in-memory snapshot.
func (s *Store) ListSupplyUsageMinutes(ctx context.Context, sinceMS int64) ([]SupplyUsageMinute, error) {
	return s.UsageEvents.ListSupplyUsageMinutes(ctx, sinceMS)
}

func (s *Store) ListSupplyQuotaCalibrationEvents(ctx context.Context, sinceMS int64, limit int) ([]usage.Event, error) {
	return s.UsageEvents.ListSupplyQuotaCalibrationEvents(ctx, sinceMS, limit)
}

func (s *Store) ListSupplyQuotaWindowUsage(ctx context.Context, targets []SupplyQuotaWindowUsageQuery) ([]SupplyQuotaWindowUsage, error) {
	return s.UsageEvents.ListSupplyQuotaWindowUsage(ctx, targets)
}

func (s *Store) UsageCacheAccountingMigrationState(ctx context.Context) (DataMigrationState, error) {
	state, found, err := s.DataMigrations.UsageCacheAccountingState(ctx)
	if err != nil {
		return DataMigrationState{}, err
	}
	if found {
		return state, nil
	}
	return DataMigrationState{
		Name:   datamigration.UsageCacheAccountingMigrationName,
		Status: datamigration.StatusDiscovering,
	}, nil
}

func (s *Store) DiscoverUsageCacheAccounting(ctx context.Context) (DataMigrationState, error) {
	return s.DataMigrations.DiscoverUsageCacheAccounting(ctx)
}

func (s *Store) RunUsageCacheAccountingBatch(ctx context.Context, batchSize int) (DataMigrationBatchResult, error) {
	return s.DataMigrations.RunUsageCacheAccountingBatch(ctx, batchSize)
}

func (s *Store) RecordUsageCacheAccountingFailure(ctx context.Context, migrationErr error) error {
	return s.DataMigrations.RecordUsageCacheAccountingFailure(ctx, migrationErr)
}

func (s *Store) UsageCacheAccountingMigrationReady(ctx context.Context) (bool, error) {
	state, err := s.UsageCacheAccountingMigrationState(ctx)
	if err != nil {
		return false, err
	}
	return state.Status == datamigration.StatusCompleted, nil
}

func (s *Store) CatchUpUsageHourlyAggregate(ctx context.Context, limit int, nowMS int64) (UsageHourlyAggregateCatchUpResult, error) {
	ready, err := s.UsageCacheAccountingMigrationReady(ctx)
	if err != nil {
		return UsageHourlyAggregateCatchUpResult{}, err
	}
	if !ready {
		return UsageHourlyAggregateCatchUpResult{Pending: true}, nil
	}
	return s.UsageAggregates.CatchUp(ctx, limit, nowMS)
}

func (s *Store) RecordUsageHourlyAggregateFailure(ctx context.Context, aggregateErr error, nowMS int64) error {
	return s.UsageAggregates.RecordFailure(ctx, aggregateErr, nowMS)
}

func (s *Store) UsageHourlyAggregateState(ctx context.Context) (UsageHourlyAggregateState, error) {
	return s.UsageAggregates.State(ctx)
}

func (s *Store) UsageHourlyAggregateRows(ctx context.Context, filter UsageHourlyAggregateFilter) ([]UsageHourlyAggregateRow, UsageHourlyAggregateState, bool, error) {
	return s.UsageAggregates.LoadRows(ctx, filter)
}

func (s *Store) CatchUpUsagePricing(ctx context.Context, limit int, nowMS int64) (UsagePricingCatchUpResult, error) {
	ready, err := s.UsageCacheAccountingMigrationReady(ctx)
	if err != nil {
		return UsagePricingCatchUpResult{}, err
	}
	if !ready {
		return UsagePricingCatchUpResult{Pending: true}, nil
	}
	return s.UsagePricing.CatchUp(ctx, limit, nowMS)
}

func (s *Store) RecordUsagePricingFailure(ctx context.Context, rollupErr error, nowMS int64) error {
	return s.UsagePricing.RecordFailure(ctx, rollupErr, nowMS)
}

func (s *Store) CatchUpUsageMonitoringStats(ctx context.Context, limit int, nowMS int64) (UsageMonitoringCatchUpResult, error) {
	ready, err := s.UsageCacheAccountingMigrationReady(ctx)
	if err != nil {
		return UsageMonitoringCatchUpResult{}, err
	}
	if !ready {
		return UsageMonitoringCatchUpResult{Pending: true}, nil
	}
	return s.UsageMonitoring.CatchUpStats(ctx, limit, nowMS)
}

func (s *Store) CatchUpUsageMonitoringProjection(ctx context.Context, limit int, nowMS int64) (UsageMonitoringCatchUpResult, error) {
	ready, err := s.UsageCacheAccountingMigrationReady(ctx)
	if err != nil {
		return UsageMonitoringCatchUpResult{}, err
	}
	if !ready {
		return UsageMonitoringCatchUpResult{Pending: true}, nil
	}
	return s.UsageMonitoring.CatchUpProjection(ctx, limit, nowMS)
}

func (s *Store) CatchUpUsageMonitoringMetadata(ctx context.Context, limit int, nowMS int64) (UsageMonitoringCatchUpResult, error) {
	return s.UsageMonitoring.CatchUpMetadata(ctx, limit, nowMS)
}

func (s *Store) RecordUsageMonitoringFailure(ctx context.Context, rollupName string, rollupErr error, nowMS int64) error {
	return s.UsageMonitoring.RecordFailure(ctx, rollupName, rollupErr, nowMS)
}

func (s *Store) UsageMonitoringState(ctx context.Context, rollupName string) (UsageMonitoringState, error) {
	return s.UsageMonitoring.State(ctx, rollupName)
}

func (s *Store) UsageMonitoringAggregate(ctx context.Context, filter AnalyticsFilter) (Aggregate, UsageMonitoringState, bool, error) {
	return s.UsageMonitoring.LoadAggregate(ctx, filter)
}

func (s *Store) UsageMonitoringModelStats(ctx context.Context, filter AnalyticsFilter) ([]ModelStat, UsageMonitoringState, bool, error) {
	return s.UsageMonitoring.LoadModelStats(ctx, filter)
}

func (s *Store) UsageMonitoringAccountStats(ctx context.Context, filter AnalyticsFilter) ([]AccountModelStat, UsageMonitoringState, bool, error) {
	return s.UsageMonitoring.LoadAccountStats(ctx, filter)
}

func (s *Store) UsageMonitoringAccountWindowStats(ctx context.Context, windows []AccountWindowUsageQuery) ([]AccountWindowModelStat, UsageMonitoringState, bool, error) {
	return s.UsageMonitoring.LoadAccountWindowStats(ctx, windows)
}

func (s *Store) UsageMonitoringAPIKeyStats(ctx context.Context, filter AnalyticsFilter) ([]APIKeyModelStat, UsageMonitoringState, bool, error) {
	return s.UsageMonitoring.LoadAPIKeyStats(ctx, filter)
}

func (s *Store) UsageMonitoringFilterOptions(ctx context.Context, filter AnalyticsFilter) (FilterOptionValues, UsageMonitoringState, bool, error) {
	return s.UsageMonitoring.LoadFilterOptions(ctx, filter)
}

func (s *Store) UsageMonitoringFilterSelectors(ctx context.Context, filter AnalyticsFilter) (FilterSelectorValues, UsageMonitoringState, bool, error) {
	return s.UsageMonitoring.LoadFilterSelectors(ctx, filter)
}

func (s *Store) UsageMonitoringEventsCount(ctx context.Context, filter AnalyticsFilter) (int64, UsageMonitoringState, bool, error) {
	return s.UsageMonitoring.LoadEventsCount(ctx, filter)
}

func (s *Store) UsageMonitoringEventsPage(ctx context.Context, filter AnalyticsFilter, beforeMS, beforeID int64, limit int) (EventsPage, UsageMonitoringState, bool, error) {
	return s.UsageMonitoring.LoadEventsPage(ctx, filter, beforeMS, beforeID, limit)
}

func (s *Store) UsageMonitoringHeaderSnapshots(ctx context.Context, sinceMS int64, limit int) ([]HeaderSnapshot, UsageMonitoringState, bool, error) {
	return s.UsageMonitoring.LoadHeaderSnapshots(ctx, sinceMS, limit)
}

func (s *Store) UsagePricingState(ctx context.Context) (UsagePricingState, error) {
	return s.UsagePricing.State(ctx)
}

func (s *Store) UsagePricingHourlyRows(ctx context.Context, filter UsagePricingHourlyFilter) ([]UsagePricingHourlyRow, UsagePricingState, bool, error) {
	return s.UsagePricing.LoadHourlyRows(ctx, filter)
}

func (s *Store) LoadUsageHourlyPricingSnapshot(
	ctx context.Context,
	aggregateFilter UsageHourlyAggregateFilter,
	pricingFilter UsagePricingHourlyFilter,
) (UsageHourlyPricingSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return UsageHourlyPricingSnapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()

	aggregateRows, aggregateState, aggregateAvailable, err := s.UsageAggregates.LoadRowsTx(ctx, tx, aggregateFilter)
	if err != nil {
		return UsageHourlyPricingSnapshot{}, err
	}
	pricingRows, pricingState, pricingAvailable, err := s.UsagePricing.LoadHourlyRowsTx(ctx, tx, pricingFilter)
	if err != nil {
		return UsageHourlyPricingSnapshot{}, err
	}
	prices, err := s.ModelPrices.LoadAllTx(ctx, tx)
	if err != nil {
		return UsageHourlyPricingSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return UsageHourlyPricingSnapshot{}, err
	}
	return UsageHourlyPricingSnapshot{
		AggregateRows:      aggregateRows,
		AggregateState:     aggregateState,
		AggregateAvailable: aggregateAvailable,
		PricingRows:        pricingRows,
		PricingState:       pricingState,
		PricingAvailable:   pricingAvailable,
		Prices:             prices,
	}, nil
}

func (s *Store) UsagePricingAccountRows(ctx context.Context, accountKeys []string) ([]UsagePricingAccountRow, UsagePricingState, bool, error) {
	return s.UsagePricing.LoadAccountRows(ctx, accountKeys)
}

func (s *Store) LoadUsagePricingAccountSnapshot(ctx context.Context, accountKeys []string) (UsagePricingAccountSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return UsagePricingAccountSnapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, state, available, err := s.UsagePricing.LoadAccountRowsTx(ctx, tx, accountKeys)
	if err != nil {
		return UsagePricingAccountSnapshot{}, err
	}
	prices, err := s.ModelPrices.LoadAllTx(ctx, tx)
	if err != nil {
		return UsagePricingAccountSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return UsagePricingAccountSnapshot{}, err
	}
	return UsagePricingAccountSnapshot{
		Rows:      rows,
		State:     state,
		Available: available,
		Prices:    prices,
	}, nil
}

func (s *Store) CatchUpAccountHistoryRollups(ctx context.Context, limit int, nowMS int64) (UsageRollupCatchUpResult, error) {
	ready, err := s.UsageCacheAccountingMigrationReady(ctx)
	if err != nil {
		return UsageRollupCatchUpResult{}, err
	}
	if !ready {
		return UsageRollupCatchUpResult{Pending: true}, nil
	}
	return s.UsageRollups.CatchUpAccountHistory(ctx, limit, nowMS)
}

func (s *Store) CatchUpDashboardHourlyRollups(ctx context.Context, limit int, nowMS int64) (UsageRollupCatchUpResult, error) {
	ready, err := s.UsageCacheAccountingMigrationReady(ctx)
	if err != nil {
		return UsageRollupCatchUpResult{}, err
	}
	if !ready {
		return UsageRollupCatchUpResult{Pending: true}, nil
	}
	return s.UsageRollups.CatchUpDashboardHourly(ctx, limit, nowMS)
}

func (s *Store) AccountHistoryRollupCheckpoint(ctx context.Context) (UsageRollupCheckpoint, error) {
	return s.UsageRollups.Checkpoint(ctx, usagerollup.AccountHistoryCheckpointName)
}

func (s *Store) DashboardHourlyRollupCheckpoint(ctx context.Context) (UsageRollupCheckpoint, error) {
	return s.UsageRollups.Checkpoint(ctx, usagerollup.DashboardHourlyCheckpointName)
}

func (s *Store) LatestUsageEventID(ctx context.Context) (int64, error) {
	return s.UsageRollups.LatestEventID(ctx)
}

func (s *Store) AccountHistoryRollupRows(ctx context.Context, accountKeys []string) ([]AccountHistoryRollupRow, error) {
	return s.UsageRollups.AccountHistoryRows(ctx, accountKeys)
}

func (s *Store) RecentAccountRequests(ctx context.Context, targets []LatestAccountRequestQuery, limit int) ([]LatestAccountRequest, error) {
	return s.UsageEvents.RecentAccountRequests(ctx, targets, limit)
}

func (s *Store) DashboardHourlyRollupRows(ctx context.Context, fromMS, toMS int64) ([]DashboardHourlyRollupRow, error) {
	return s.UsageRollups.DashboardHourlyRows(ctx, fromMS, toMS)
}

func (s *Store) DashboardHourlyRollupModelRows(ctx context.Context, fromMS, toMS int64) ([]DashboardHourlyRollupRow, error) {
	return s.UsageRollups.DashboardHourlyModelRows(ctx, fromMS, toMS)
}

func (s *Store) DashboardDailyRollupRows(ctx context.Context, fromMS, toMS int64) ([]DashboardHourlyRollupRow, error) {
	return s.UsageRollups.DashboardDailyRows(ctx, fromMS, toMS)
}

func AccountHistoryKey(accountSnapshot, authLabelSnapshot, source, authIndex string) string {
	return usagerollup.AccountKey(accountSnapshot, authLabelSnapshot, source, authIndex)
}

func (s *Store) UpsertQuotaCooldown(ctx context.Context, cooldown QuotaCooldownUpsert) (QuotaCooldown, error) {
	return s.QuotaCooldowns.UpsertActive(ctx, cooldown)
}

func (s *Store) ListDueQuotaCooldowns(ctx context.Context, nowMS int64, limit int) ([]QuotaCooldown, error) {
	return s.QuotaCooldowns.ListDue(ctx, nowMS, limit)
}

func (s *Store) MarkQuotaCooldownRecovered(ctx context.Context, id int64, recoveredAtMS int64) error {
	return s.QuotaCooldowns.MarkRecovered(ctx, id, recoveredAtMS)
}

func (s *Store) MarkQuotaCooldownSkipped(ctx context.Context, id int64, reason string) error {
	return s.QuotaCooldowns.MarkSkipped(ctx, id, reason)
}

func (s *Store) RecordQuotaCooldownFailure(ctx context.Context, id int64, reason string) error {
	return s.QuotaCooldowns.RecordFailure(ctx, id, reason)
}

// CleanupDeletedCredential removes local automation and usage state after CPA
// confirms that a credential has been deleted.
func (s *Store) CleanupDeletedCredential(ctx context.Context, identity model.CredentialIdentity) error {
	if s == nil {
		return nil
	}
	var cleanupErr error
	if s.UsageEvents != nil {
		if _, err := s.UsageEvents.DeleteCredentialIdentityHistory(ctx, identity); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if s.QuotaCooldowns != nil {
		if _, err := s.QuotaCooldowns.DeleteCredential(ctx, identity); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if s.AccountActions != nil {
		if _, err := s.AccountActions.DeleteCredential(ctx, identity); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	return cleanupErr
}

func (s *Store) AddDeadLetter(ctx context.Context, payload string, parseErr error) error {
	return s.DeadLetters.Insert(ctx, payload, parseErr.Error())
}

func (s *Store) RecentEvents(ctx context.Context, limit int) ([]usage.Event, error) {
	return s.UsageEvents.ListRecent(ctx, limit)
}

func (s *Store) BackfillUsageResponseMetadata(ctx context.Context, batchLimit int) (int, error) {
	return s.UsageEvents.BackfillResponseMetadata(ctx, batchLimit)
}

func (s *Store) BackfillUsageRoutingDiagnostics(ctx context.Context, batchLimit int) (int, error) {
	return s.UsageEvents.BackfillRoutingDiagnostics(ctx, batchLimit)
}

func (s *Store) Counts(ctx context.Context) (events int64, deadLetters int64, err error) {
	events, err = s.UsageEvents.Count(ctx)
	if err != nil {
		return 0, 0, err
	}
	deadLetters, err = s.DeadLetters.Count(ctx)
	if err != nil {
		return 0, 0, err
	}
	return events, deadLetters, nil
}

func (s *Store) ExportJSONL(ctx context.Context) ([]byte, error) {
	var output bytes.Buffer
	if err := s.WriteExportJSONL(ctx, &output, 0); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (s *Store) WriteCompatibleUsage(ctx context.Context, writer io.Writer, limit int) error {
	return s.UsageEvents.WriteCompatibleUsage(ctx, writer, limit)
}

func (s *Store) WriteExportJSONL(ctx context.Context, writer io.Writer, limit int) error {
	return s.UsageEvents.WriteExportJSONL(ctx, writer, limit)
}

// AggregateBetween computes summary metrics over [fromMs, toMs).
func (s *Store) AggregateBetween(ctx context.Context, fromMs, toMs int64) (Aggregate, error) {
	return s.UsageEvents.AggregateBetween(ctx, fromMs, toMs)
}

// TopModelsBetween returns the most active models ordered by call count.
func (s *Store) TopModelsBetween(ctx context.Context, fromMs, toMs int64, limit int) ([]ModelStat, error) {
	return s.UsageEvents.TopModelsBetween(ctx, fromMs, toMs, limit)
}

// ModelStatsBetween returns per-model totals for all models in a window.
func (s *Store) ModelStatsBetween(ctx context.Context, fromMs, toMs int64) ([]ModelStat, error) {
	return s.UsageEvents.ModelStatsBetween(ctx, fromMs, toMs)
}

// RecentFailuresBetween returns the most recent failed events in window.
func (s *Store) RecentFailuresBetween(ctx context.Context, fromMs, toMs int64, limit int) ([]RecentFailure, error) {
	return s.UsageEvents.RecentFailuresBetween(ctx, fromMs, toMs, limit)
}

func (s *Store) HourlyTimelineBetween(ctx context.Context, fromMs, toMs int64) ([]TimelinePoint, error) {
	return s.UsageEvents.HourlyTimelineBetween(ctx, fromMs, toMs)
}

func (s *Store) BucketTimelineBetween(ctx context.Context, fromMs, toMs int64, bucketMs int64) ([]TimelinePoint, error) {
	return s.UsageEvents.BucketTimelineBetween(ctx, fromMs, toMs, bucketMs)
}

func (s *Store) AggregateWithFilter(ctx context.Context, filter AnalyticsFilter) (Aggregate, error) {
	return s.UsageEvents.AggregateWithFilter(ctx, filter)
}

func (s *Store) ModelStatsWithFilter(ctx context.Context, filter AnalyticsFilter, limit int) ([]ModelStat, error) {
	return s.UsageEvents.ModelStatsWithFilter(ctx, filter, limit)
}

func (s *Store) TimelineWithFilter(ctx context.Context, filter AnalyticsFilter, granularity string, location *time.Location) ([]TimelinePoint, error) {
	return s.UsageEvents.TimelineWithFilter(ctx, filter, granularity, location)
}

func (s *Store) LatencyAnalyticsWithFilter(ctx context.Context, filter AnalyticsFilter, granularity string, location *time.Location) ([]LatencyPercentiles, LatencySummary, error) {
	return s.UsageEvents.LatencyAnalyticsWithFilter(ctx, filter, granularity, location)
}

func (s *Store) LatencyPercentilesWithFilter(ctx context.Context, filter AnalyticsFilter, granularity string, location *time.Location) ([]LatencyPercentiles, error) {
	return s.UsageEvents.LatencyPercentilesWithFilter(ctx, filter, granularity, location)
}

func (s *Store) LatencySummaryWithFilter(ctx context.Context, filter AnalyticsFilter) (LatencySummary, error) {
	return s.UsageEvents.LatencySummaryWithFilter(ctx, filter)
}

func (s *Store) HourlyDistributionWithFilter(ctx context.Context, filter AnalyticsFilter, location *time.Location) ([]HourlyPoint, error) {
	return s.UsageEvents.HourlyDistributionWithFilter(ctx, filter, location)
}

func (s *Store) FilterOptionValuesWithFilter(ctx context.Context, filter AnalyticsFilter) (FilterOptionValues, error) {
	return s.UsageEvents.FilterOptionValuesWithFilter(ctx, filter)
}

func (s *Store) FilterSelectorValuesWithFilter(ctx context.Context, filter AnalyticsFilter) (FilterSelectorValues, error) {
	return s.UsageEvents.FilterSelectorValuesWithFilter(ctx, filter)
}

func (s *Store) HeatmapWithFilter(ctx context.Context, filter AnalyticsFilter, location *time.Location) ([]HeatmapPoint, error) {
	return s.UsageEvents.HeatmapWithFilter(ctx, filter, location)
}

func (s *Store) ChannelModelStatsWithFilter(ctx context.Context, filter AnalyticsFilter) ([]ChannelModelStat, error) {
	return s.UsageEvents.ChannelModelStatsWithFilter(ctx, filter)
}

func (s *Store) FailureSourcesWithFilter(ctx context.Context, filter AnalyticsFilter) ([]FailureSourceStat, error) {
	return s.UsageEvents.FailureSourcesWithFilter(ctx, filter)
}

func (s *Store) AccountModelStatsWithFilter(ctx context.Context, filter AnalyticsFilter) ([]AccountModelStat, error) {
	return s.UsageEvents.AccountModelStatsWithFilter(ctx, filter)
}

func (s *Store) AccountWindowModelStats(ctx context.Context, windows []AccountWindowUsageQuery) ([]AccountWindowModelStat, error) {
	return s.UsageEvents.AccountWindowModelStats(ctx, windows)
}

func (s *Store) CredentialModelStatsWithFilter(ctx context.Context, filter AnalyticsFilter) ([]CredentialModelStat, error) {
	return s.UsageEvents.CredentialModelStatsWithFilter(ctx, filter)
}

func (s *Store) CredentialTimelineWithFilter(ctx context.Context, filter AnalyticsFilter, granularity string, location *time.Location) ([]CredentialTimelinePoint, error) {
	return s.UsageEvents.CredentialTimelineWithFilter(ctx, filter, granularity, location)
}

func (s *Store) APIKeyTimelineWithFilter(ctx context.Context, filter AnalyticsFilter, granularity string, location *time.Location) ([]APIKeyTimelinePoint, error) {
	return s.UsageEvents.APIKeyTimelineWithFilter(ctx, filter, granularity, location)
}

func (s *Store) APIKeyModelStatsWithFilter(ctx context.Context, filter AnalyticsFilter) ([]APIKeyModelStat, error) {
	return s.UsageEvents.APIKeyModelStatsWithFilter(ctx, filter)
}

func (s *Store) TaskBucketsWithFilter(ctx context.Context, filter AnalyticsFilter) ([]TaskBucket, error) {
	return s.UsageEvents.TaskBucketsWithFilter(ctx, filter)
}

func (s *Store) RecentFailuresWithFilter(ctx context.Context, filter AnalyticsFilter, limit int) ([]RecentFailure, error) {
	return s.UsageEvents.RecentFailuresWithFilter(ctx, filter, limit)
}

func (s *Store) EventsPageWithFilter(ctx context.Context, filter AnalyticsFilter, beforeMS int64, beforeID int64, limit int) (EventsPage, error) {
	return s.UsageEvents.EventsPageWithFilter(ctx, filter, beforeMS, beforeID, limit)
}

func (s *Store) EventsCountWithFilter(ctx context.Context, filter AnalyticsFilter) (int64, error) {
	return s.UsageEvents.EventsCountWithFilter(ctx, filter)
}

func (s *Store) LatestHeaderSnapshots(ctx context.Context, sinceMS int64, limit int) ([]HeaderSnapshot, error) {
	return s.UsageEvents.LatestHeaderSnapshots(ctx, sinceMS, limit)
}

func (s *Store) ActiveDaysWithFilter(ctx context.Context, filter AnalyticsFilter, location *time.Location) (int64, error) {
	return s.UsageEvents.ActiveDaysWithFilter(ctx, filter, location)
}

func (s *Store) ZeroTokenModelsWithFilter(ctx context.Context, filter AnalyticsFilter) ([]string, error) {
	return s.UsageEvents.ZeroTokenModelsWithFilter(ctx, filter)
}

func (s *Store) RoutingDiagnosticsWithFilter(ctx context.Context, filter AnalyticsFilter) (RoutingDiagnostics, error) {
	return s.UsageEvents.RoutingDiagnosticsWithFilter(ctx, filter)
}
