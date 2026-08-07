package store

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/accountaction"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/apikeyalias"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/codexinspection"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/containeropsaudit"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/containeropsupgrade"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/datamigration"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/deadletter"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/modelprice"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/quotacooldown"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/setting"
	sqliterepo "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/sqlite"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/supplyorder"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/supplyrecovery"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/usageevent"
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
type SupplyOrder = model.SupplyOrder
type SupplyImportItem = model.SupplyImportItem
type SupplyRecovery = model.SupplyRecovery
type CodexInspectionRun = model.CodexInspectionRun
type CodexInspectionResult = model.CodexInspectionResult
type CodexInspectionLog = model.CodexInspectionLog
type ContainerOpsAuditEntry = model.ContainerOpsAuditEntry
type ContainerOpsUpgradeTask = model.ContainerOpsUpgradeTask
type CodexInspectionDisableOwnership = model.CodexInspectionDisableOwnership
type InsertResult = model.InsertResult
type ModelPrice = model.ModelPrice
type ModelPriceSyncResult = model.ModelPriceSyncResult
type ModelUsageStat = model.ModelUsageStat
type ModelUsageSummary = model.ModelUsageSummary
type APIKeyAlias = model.APIKeyAlias
type QuotaCooldown = model.QuotaCooldown
type QuotaCooldownUpsert = model.QuotaCooldownUpsert
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
type UsageRollupCheckpoint = usagerollup.Checkpoint
type UsageRollupCatchUpResult = usagerollup.CatchUpResult
type AccountHistoryRollupRow = usagerollup.AccountHistoryRow
type DashboardHourlyRollupRow = usagerollup.DashboardHourlyRow

type Store struct {
	db *sql.DB

	Settings             setting.Repository
	UsageEvents          usageevent.Repository
	DeadLetters          deadletter.Repository
	ModelPrices          modelprice.Repository
	APIKeyAliases        apikeyalias.Repository
	AccountActions       accountaction.Repository
	CodexInspections     codexinspection.Repository
	DataMigrations       datamigration.Repository
	QuotaCooldowns       quotacooldown.Repository
	UsageRollups         usagerollup.Repository
	ContainerOpsAudits   containeropsaudit.Repository
	ContainerOpsUpgrades containeropsupgrade.Repository
	SupplyOrders         supplyorder.Repository
	SupplyRecoveries     supplyrecovery.Repository
}

func Open(path string, protector ...*security.Protector) (*Store, error) {
	db, err := sqliterepo.Open(path)
	if err != nil {
		return nil, err
	}
	return New(db, protector...), nil
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
		DataMigrations:       datamigration.New(db),
		QuotaCooldowns:       quotacooldown.New(db),
		UsageRollups:         usagerollup.New(db),
		ContainerOpsAudits:   containeropsaudit.New(db),
		ContainerOpsUpgrades: containeropsupgrade.New(db),
		SupplyOrders:         supplyorder.New(db, protector...),
		SupplyRecoveries:     supplyrecovery.New(db, protector...),
	}
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
	return s.ModelPrices.ReplaceAll(ctx, prices)
}

func (s *Store) UpsertSyncedModelPrices(ctx context.Context, prices map[string]ModelPrice) (ModelPriceSyncResult, error) {
	return s.ModelPrices.UpsertSynced(ctx, prices)
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

func (s *Store) InsertCodexInspectionResult(ctx context.Context, result CodexInspectionResult) (CodexInspectionResult, error) {
	return s.CodexInspections.InsertResult(ctx, result)
}

func (s *Store) InsertCodexInspectionLog(ctx context.Context, entry CodexInspectionLog) (CodexInspectionLog, error) {
	return s.CodexInspections.InsertLog(ctx, entry)
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

func (s *Store) GetSupplyOrder(ctx context.Context, orderID string) (SupplyOrder, bool, error) {
	return s.SupplyOrders.Get(ctx, orderID)
}

func (s *Store) GetOpenSupplyOrder(ctx context.Context) (SupplyOrder, bool, error) {
	return s.SupplyOrders.GetOpen(ctx)
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

func (s *Store) ListSupplyOrdersBetween(ctx context.Context, fromMS int64, toMS int64, limit int) ([]SupplyOrder, error) {
	return s.SupplyOrders.ListBetween(ctx, fromMS, toMS, limit)
}

func (s *Store) InsertSupplyImportItems(ctx context.Context, orderID string, items []SupplyImportItem) (int, error) {
	return s.SupplyOrders.InsertItems(ctx, orderID, items)
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

func (s *Store) MarkSupplyImportItemImported(ctx context.Context, id int64, importedAtMS int64) error {
	return s.SupplyOrders.MarkItemImported(ctx, id, importedAtMS)
}

func (s *Store) MarkSupplyImportItemFailed(ctx context.Context, id int64, lastError string, nextRetryAtMS int64) error {
	return s.SupplyOrders.MarkItemFailed(ctx, id, lastError, nextRetryAtMS)
}

func (s *Store) UpdateSupplyImportItemFileName(ctx context.Context, id int64, fileName string) error {
	return s.SupplyOrders.UpdateItemFileName(ctx, id, fileName)
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

func (s *Store) DeleteCodexInspectionDisableOwnership(ctx context.Context, fileName string) error {
	return s.CodexInspections.DeleteDisableOwnership(ctx, fileName)
}

func (s *Store) RevokeCodexInspectionDisableOwnership(ctx context.Context, fileNames []string, clearAll bool) ([]CodexInspectionDisableOwnership, error) {
	return s.CodexInspections.RevokeDisableOwnership(ctx, fileNames, clearAll)
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

func (s *Store) AddDeadLetter(ctx context.Context, payload string, parseErr error) error {
	return s.DeadLetters.Insert(ctx, payload, parseErr.Error())
}

func (s *Store) RecentEvents(ctx context.Context, limit int) ([]usage.Event, error) {
	return s.UsageEvents.ListRecent(ctx, limit)
}

func (s *Store) BackfillUsageResponseMetadata(ctx context.Context, batchLimit int) (int, error) {
	return s.UsageEvents.BackfillResponseMetadata(ctx, batchLimit)
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
