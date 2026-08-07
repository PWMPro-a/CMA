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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpaauthfiles"
	managerconfigsvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/pricing"
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
	Recovery      RecoverySummary           `json:"recovery"`
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

type RecoverySummary struct {
	Enabled        bool   `json:"enabled"`
	AutoClaim      bool   `json:"autoClaim"`
	Running        bool   `json:"running"`
	LastSyncAtMS   int64  `json:"lastSyncAtMs,omitempty"`
	NextSyncAtMS   int64  `json:"nextSyncAtMs,omitempty"`
	LastResult     string `json:"lastResult,omitempty"`
	LastError      string `json:"lastError,omitempty"`
	Seen           int    `json:"seen"`
	Claimable      int    `json:"claimable"`
	Claimed        int    `json:"claimed"`
	Imported       int    `json:"imported"`
	Refunded       int    `json:"refunded"`
	Failed         int    `json:"failed"`
	Total          int    `json:"total"`
	Importing      int    `json:"importing"`
	StoredImported int    `json:"storedImported"`
	StoredRefunded int    `json:"storedRefunded"`
	StoredFailed   int    `json:"storedFailed"`
}

type RecoverySyncRequest struct {
	Force      bool   `json:"force,omitempty"`
	AutoClaim  *bool  `json:"autoClaim,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	RecoveryID string `json:"recoveryId,omitempty"`
}

type RecoverySyncResult struct {
	Seen      int `json:"seen"`
	Claimable int `json:"claimable"`
	Claimed   int `json:"claimed"`
	Imported  int `json:"imported"`
	Refunded  int `json:"refunded"`
	Failed    int `json:"failed"`
}

type ReportRequest struct {
	FromMS int64 `json:"fromMs,omitempty"`
	ToMS   int64 `json:"toMs,omitempty"`
	Limit  int   `json:"limit,omitempty"`
}

type ReportRange struct {
	FromMS        int64 `json:"fromMs"`
	ToMS          int64 `json:"toMs"`
	GeneratedAtMS int64 `json:"generatedAtMs"`
	Days          int   `json:"days"`
	Truncated     bool  `json:"truncated"`
}

type ReportExecutive struct {
	Orders                int     `json:"orders"`
	ManualOrders          int     `json:"manualOrders"`
	AutomaticOrders       int     `json:"automaticOrders"`
	RecoveryOrders        int     `json:"recoveryOrders"`
	RequestedAccounts     int     `json:"requestedAccounts"`
	ImportedAccounts      int     `json:"importedAccounts"`
	ChargedFen            int64   `json:"chargedFen"`
	ReleasedFen           int64   `json:"releasedFen"`
	NetFen                int64   `json:"netFen"`
	SupplySpendFen        int64   `json:"supplySpendFen"`
	SupplyNetSpendFen     int64   `json:"supplyNetSpendFen"`
	AverageUnitFen        float64 `json:"averageUnitFen"`
	UsageCalls            int64   `json:"usageCalls"`
	UsageTokens           int64   `json:"usageTokens"`
	UsageRevenue          float64 `json:"usageRevenue"`
	UsageRevenueCurrency  string  `json:"usageRevenueCurrency"`
	AverageRevenuePerCall float64 `json:"averageRevenuePerCall"`
	Recoveries            int     `json:"recoveries"`
	ClaimableRecoveries   int     `json:"claimableRecoveries"`
	ClaimedRecoveries     int     `json:"claimedRecoveries"`
	ImportedRecoveries    int     `json:"importedRecoveries"`
	RefundedRecoveries    int     `json:"refundedRecoveries"`
	FailedRecoveries      int     `json:"failedRecoveries"`
	RefundedFen           int64   `json:"refundedFen"`
	RecoveryClaimRate     float64 `json:"recoveryClaimRate"`
	RecoveryImportRate    float64 `json:"recoveryImportRate"`
	RecoveryRefundRate    float64 `json:"recoveryRefundRate"`
	ImportSuccessRate     float64 `json:"importSuccessRate"`
}

type ReportDimensionStat struct {
	Key         string  `json:"key"`
	Label       string  `json:"label,omitempty"`
	Count       int     `json:"count"`
	Orders      int     `json:"orders"`
	Recoveries  int     `json:"recoveries"`
	Quantity    int     `json:"quantity"`
	Imported    int     `json:"imported"`
	ChargedFen  int64   `json:"chargedFen"`
	ReleasedFen int64   `json:"releasedFen"`
	RefundedFen int64   `json:"refundedFen"`
	SuccessRate float64 `json:"successRate"`
}

type ReportUsageModelStat struct {
	Model        string  `json:"model"`
	BillingModel string  `json:"billingModel"`
	ServiceTier  string  `json:"serviceTier,omitempty"`
	Calls        int64   `json:"calls"`
	SuccessCalls int64   `json:"successCalls"`
	Tokens       int64   `json:"tokens"`
	Revenue      float64 `json:"revenue"`
}

type ReportTimelinePoint struct {
	BucketMS         int64   `json:"bucketMs"`
	Label            string  `json:"label"`
	Orders           int     `json:"orders"`
	Requested        int     `json:"requested"`
	Imported         int     `json:"imported"`
	ChargedFen       int64   `json:"chargedFen"`
	UsageCalls       int64   `json:"usageCalls"`
	UsageTokens      int64   `json:"usageTokens"`
	UsageRevenue     float64 `json:"usageRevenue"`
	Recoveries       int     `json:"recoveries"`
	RecoveryClaimed  int     `json:"recoveryClaimed"`
	RecoveryImported int     `json:"recoveryImported"`
	RecoveryRefunded int     `json:"recoveryRefunded"`
	ImportFailures   int     `json:"importFailures"`
}

type ReportImportHealth struct {
	Items             int     `json:"items"`
	ImportedItems     int     `json:"importedItems"`
	FailedItems       int     `json:"failedItems"`
	PendingItems      int     `json:"pendingItems"`
	RetryingItems     int     `json:"retryingItems"`
	AverageAttempts   float64 `json:"averageAttempts"`
	SuccessRate       float64 `json:"successRate"`
	ExpiringSoonItems int     `json:"expiringSoonItems"`
	ExpiredItems      int     `json:"expiredItems"`
}

type ReportTiming struct {
	AverageOrderFulfillmentSeconds   float64 `json:"averageOrderFulfillmentSeconds"`
	AverageRecoveryClaimSeconds      float64 `json:"averageRecoveryClaimSeconds"`
	AverageRecoveryImportSeconds     float64 `json:"averageRecoveryImportSeconds"`
	AverageImportRegistrationSeconds float64 `json:"averageImportRegistrationSeconds"`
}

type ReportRiskBucket struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

type ReportRisk struct {
	OpenOrders               int                `json:"openOrders"`
	UnclaimedRecoveries      int                `json:"unclaimedRecoveries"`
	ImportBacklogItems       int                `json:"importBacklogItems"`
	FailedImportItems        int                `json:"failedImportItems"`
	PartialRecoveries        int                `json:"partialRecoveries"`
	StaleClaimableRecoveries int                `json:"staleClaimableRecoveries"`
	ClaimableAgeBuckets      []ReportRiskBucket `json:"claimableAgeBuckets"`
}

type Report struct {
	Range            ReportRange            `json:"range"`
	Executive        ReportExecutive        `json:"executive"`
	ImportHealth     ReportImportHealth     `json:"importHealth"`
	Timing           ReportTiming           `json:"timing"`
	Risk             ReportRisk             `json:"risk"`
	Timeline         []ReportTimelinePoint  `json:"timeline"`
	Products         []ReportDimensionStat  `json:"products"`
	OrderStatuses    []ReportDimensionStat  `json:"orderStatuses"`
	RecoveryStatuses []ReportDimensionStat  `json:"recoveryStatuses"`
	DeliveryStatuses []ReportDimensionStat  `json:"deliveryStatuses"`
	Sources          []ReportDimensionStat  `json:"sources"`
	UsageModels      []ReportUsageModelStat `json:"usageModels"`
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
	recoveryMu         sync.Mutex
	recoveryState      RecoverySummary

	inspectionSnapshotRefreshMu sync.Mutex
	inspectionSnapshotRefresh   inspectionSnapshotRefreshState

	criticalConfirmMu       sync.Mutex
	criticalConfirmRounds   map[string]int
	lastAutomaticCreateAtMS int64
}

const (
	staleInspectionSnapshotRefreshCooldown = 30 * time.Second
	staleInspectionSnapshotRefreshTimeout  = 15 * time.Minute
)

type inspectionSnapshotRefreshState struct {
	appCtx      context.Context
	refresh     func(context.Context) error
	running     bool
	lastAttempt time.Time
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

// SetInspectionSnapshotRefresher connects smart supply with the Codex
// inspection service. The refresher runs asynchronously so a status request
// or automatic supply tick never waits for a full account scan.
func (s *Service) SetInspectionSnapshotRefresher(appCtx context.Context, refresh func(context.Context) error) {
	if s == nil {
		return
	}
	if appCtx == nil {
		appCtx = context.Background()
	}
	s.inspectionSnapshotRefreshMu.Lock()
	s.inspectionSnapshotRefresh.appCtx = appCtx
	s.inspectionSnapshotRefresh.refresh = refresh
	s.inspectionSnapshotRefreshMu.Unlock()
}

func (s *Service) publishSmartResource(resource SmartResource) SmartResource {
	if resource.Enabled && !resource.SnapshotFresh {
		s.requestStaleInspectionSnapshotRefresh()
	}
	resource = s.withInspectionSnapshotRefreshState(resource)
	s.setSmartResource(resource)
	return resource
}

func (s *Service) requestStaleInspectionSnapshotRefresh() {
	if s == nil {
		return
	}
	now := time.Now()
	s.inspectionSnapshotRefreshMu.Lock()
	state := &s.inspectionSnapshotRefresh
	if state.refresh == nil || state.running ||
		(!state.lastAttempt.IsZero() && now.Sub(state.lastAttempt) < staleInspectionSnapshotRefreshCooldown) {
		s.inspectionSnapshotRefreshMu.Unlock()
		return
	}
	state.running = true
	state.lastAttempt = now
	appCtx := state.appCtx
	refresh := state.refresh
	s.inspectionSnapshotRefreshMu.Unlock()

	go func() {
		if appCtx == nil {
			appCtx = context.Background()
		}
		refreshCtx, cancel := context.WithTimeout(appCtx, staleInspectionSnapshotRefreshTimeout)
		err := refresh(refreshCtx)
		cancel()

		// A completed inspection is durable in the store. Drop the previous
		// in-memory snapshot so the following 10-second UI poll reads the new
		// quota result immediately instead of waiting for its cache TTL.
		if err == nil {
			s.invalidateInspectionQuotaSnapshot()
		}
		s.inspectionSnapshotRefreshMu.Lock()
		s.inspectionSnapshotRefresh.running = false
		s.inspectionSnapshotRefreshMu.Unlock()
	}()
}

func (s *Service) withInspectionSnapshotRefreshState(resource SmartResource) SmartResource {
	if s == nil {
		return resource
	}
	s.inspectionSnapshotRefreshMu.Lock()
	resource.SnapshotRefreshInProgress = s.inspectionSnapshotRefresh.running
	if !s.inspectionSnapshotRefresh.lastAttempt.IsZero() {
		resource.SnapshotRefreshLastAttemptMS = s.inspectionSnapshotRefresh.lastAttempt.UnixMilli()
	}
	s.inspectionSnapshotRefreshMu.Unlock()
	return resource
}

func (s *Service) invalidateInspectionQuotaSnapshot() {
	if s == nil {
		return
	}
	s.quotaSnapshotMu.Lock()
	s.quotaSnapshot = inspectionQuotaSnapshot{}
	s.quotaSnapshotMu.Unlock()
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
		Recovery:      s.currentRecoverySummary(ctx, cfg.Supply),
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

func (s *Service) SyncRecoveriesIfDue(ctx context.Context) (RecoverySummary, error) {
	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return RecoverySummary{}, err
	}
	summary := s.currentRecoverySummary(ctx, cfg.Supply)
	if !summary.Enabled || !supplyCredentialsConfigured(cfg.Supply) {
		return summary, nil
	}
	now := time.Now()
	s.recoveryMu.Lock()
	nextSyncAtMS := s.recoveryState.NextSyncAtMS
	running := s.recoveryState.Running
	s.recoveryMu.Unlock()
	if running {
		return summary, nil
	}
	if nextSyncAtMS > 0 && now.Before(time.UnixMilli(nextSyncAtMS)) {
		return summary, nil
	}
	return s.SyncRecoveries(ctx, RecoverySyncRequest{})
}

func (s *Service) SyncRecoveries(ctx context.Context, req RecoverySyncRequest) (RecoverySummary, error) {
	if s == nil || s.store == nil || s.managerConfig == nil || s.supplyClient == nil {
		return RecoverySummary{}, ErrNotConfigured
	}
	s.recoveryMu.Lock()
	if s.recoveryState.Running {
		state := s.recoveryState
		s.recoveryMu.Unlock()
		return state, nil
	}
	s.recoveryState.Running = true
	s.recoveryMu.Unlock()
	defer func() {
		s.recoveryMu.Lock()
		s.recoveryState.Running = false
		s.recoveryMu.Unlock()
	}()

	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		s.recordRecoveryError(ctx, cfg.Supply, err)
		return RecoverySummary{}, err
	}
	if !recoverySyncEnabled(cfg.Supply) || !supplyCredentialsConfigured(cfg.Supply) {
		summary := s.currentRecoverySummary(ctx, cfg.Supply)
		s.recoveryMu.Lock()
		s.recoveryState = summary
		s.recoveryMu.Unlock()
		return summary, nil
	}
	if err := s.requireCredentials(cfg.Supply); err != nil {
		s.recordRecoveryError(ctx, cfg.Supply, err)
		return RecoverySummary{}, err
	}

	limit := req.Limit
	if limit <= 0 {
		limit = recoveryClaimBatchSize(cfg.Supply)
	}
	if limit <= 0 {
		limit = 20
	}
	autoClaim := recoveryAutoClaimEnabled(cfg.Supply)
	if req.AutoClaim != nil {
		autoClaim = *req.AutoClaim
	}
	recoveryID := strings.TrimSpace(req.RecoveryID)
	if autoClaim && !cpaManagementConfigured(cfg) {
		if recoveryID != "" {
			err := errors.New("CPA connection is not configured")
			s.recordRecoveryError(ctx, cfg.Supply, err)
			return RecoverySummary{}, err
		}
		autoClaim = false
	}
	result, err := s.syncRecoveriesOnce(ctx, cfg, autoClaim, limit, recoveryID)
	summary := s.currentRecoverySummary(ctx, cfg.Supply)
	summary.Seen = result.Seen
	summary.Claimable = result.Claimable
	summary.Claimed = result.Claimed
	summary.Imported = result.Imported
	summary.Refunded = result.Refunded
	summary.Failed = result.Failed
	now := time.Now()
	summary.LastSyncAtMS = now.UnixMilli()
	summary.NextSyncAtMS = now.Add(recoverySyncInterval(cfg.Supply, err)).UnixMilli()
	if err != nil {
		summary.LastResult = "failed"
		summary.LastError = safeError(err)
	} else {
		summary.LastResult = "completed"
		summary.LastError = ""
	}
	s.recoveryMu.Lock()
	s.recoveryState = summary
	s.recoveryMu.Unlock()
	return summary, err
}

func (s *Service) ListRecoveries(ctx context.Context, limit int, status string) ([]store.SupplyRecovery, error) {
	if s == nil || s.store == nil {
		return nil, ErrNotConfigured
	}
	return s.store.ListSupplyRecoveries(ctx, limit, status)
}

func (s *Service) Report(ctx context.Context, req ReportRequest) (Report, error) {
	if s == nil || s.store == nil {
		return Report{}, ErrNotConfigured
	}
	req = normalizeReportRequest(req)
	orders, err := s.store.ListSupplyOrdersBetween(ctx, req.FromMS, req.ToMS, req.Limit)
	if err != nil {
		return Report{}, err
	}
	recoveries, err := s.store.ListSupplyRecoveriesBetween(ctx, req.FromMS, req.ToMS, req.Limit)
	if err != nil {
		return Report{}, err
	}
	items, err := s.store.ListSupplyImportItemsBetween(ctx, req.FromMS, req.ToMS, req.Limit)
	if err != nil {
		return Report{}, err
	}
	usageItems, err := s.store.ListImportedSupplyItemsOverlapping(ctx, req.FromMS, req.ToMS, req.Limit*2)
	if err != nil {
		return Report{}, err
	}
	modelStats, usageTimeline, err := s.supplyUsageStats(ctx, req, supplyUsageAuthFiles(usageItems))
	if err != nil {
		return Report{}, err
	}
	prices, err := s.store.LoadModelPrices(ctx)
	if err != nil {
		return Report{}, err
	}
	report := buildSupplyReport(req, orders, recoveries, items, time.Now())
	applyUsageRevenueToReport(&report, modelStats, usageTimeline, prices)
	report.Range.Truncated = len(orders) >= req.Limit || len(recoveries) >= req.Limit ||
		len(items) >= req.Limit || len(usageItems) >= req.Limit*2
	return report, nil
}

func (s *Service) withRecoveryInterval(base time.Duration, cfg store.ManagerSupplyConfig) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if !recoverySyncEnabled(cfg) || !supplyCredentialsConfigured(cfg) {
		return base
	}
	s.recoveryMu.Lock()
	nextSyncAtMS := s.recoveryState.NextSyncAtMS
	running := s.recoveryState.Running
	s.recoveryMu.Unlock()
	if running || nextSyncAtMS <= 0 {
		return base
	}
	wait := time.Until(time.UnixMilli(nextSyncAtMS))
	if wait <= 0 {
		return time.Second
	}
	if wait < base {
		return wait
	}
	return base
}

func (s *Service) NextInterval(ctx context.Context) time.Duration {
	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return 30 * time.Second
	}
	if order, found, err := s.store.GetOpenSupplyOrder(ctx); err == nil && found {
		if order.Status == "creating" || order.Status == "create_uncertain" {
			return s.withRecoveryInterval(time.Minute, cfg.Supply)
		}
		if wait := time.Until(time.UnixMilli(order.SupplierRetryUntilMS)); wait > 0 {
			if wait > time.Minute {
				return s.withRecoveryInterval(time.Minute, cfg.Supply)
			}
			return s.withRecoveryInterval(wait, cfg.Supply)
		}
		resource := s.currentSmartResource(cfg.Supply)
		if s.emergencyOrderProcessingAllowed(cfg.Supply, order, resource) {
			// The supplier has not requested a retry delay. Keep emergency order
			// reconciliation responsive without spinning the worker.
			return s.withRecoveryInterval(3*time.Second, cfg.Supply)
		}
		if wait := time.Until(time.UnixMilli(order.NextPollAtMS)); wait > 0 {
			if wait > time.Minute {
				return s.withRecoveryInterval(time.Minute, cfg.Supply)
			}
			return s.withRecoveryInterval(wait, cfg.Supply)
		}
		seconds := cfg.Supply.PollIntervalSeconds
		if seconds <= 0 {
			seconds = 3
		}
		return s.withRecoveryInterval(time.Duration(seconds)*time.Second, cfg.Supply)
	}
	if !managerconfigsvc.SupplyEnabled(cfg.Supply) {
		return s.withRecoveryInterval(time.Minute, cfg.Supply)
	}
	resource := s.currentSmartResource(cfg.Supply)
	if smartEmergencyShortage(resource) {
		return s.withRecoveryInterval(time.Second, cfg.Supply)
	}
	seconds := smartAutomaticCheckIntervalSeconds(cfg.Supply, resource)
	return s.withRecoveryInterval(time.Duration(seconds)*time.Second, cfg.Supply)
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
		nowMS := time.Now().UnixMilli()
		// retry_after_seconds is a supplier contract, not a local cooldown. A
		// manual check may refresh the dashboard but must not poll the order
		// ahead of this deadline either.
		if active.SupplierRetryUntilMS > nowMS {
			return nil
		}
		if !force && active.NextPollAtMS > nowMS && !s.emergencyOrderProcessingAllowed(cfg.Supply, active, s.currentSmartResource(cfg.Supply)) {
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
		} else if recentFound && !smartEmergencyShortage(resource) && time.Since(time.UnixMilli(recent.CompletedAtMS)) < automaticSettleWindow(supplyCfg) {
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
			if !smartEmergencyShortage(resource) && s.automaticCreateCooldownActive(supplyCfg, resource) {
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
		OrderID:              remote.ID,
		Product:              supplyCfg.Product,
		RequestedQuantity:    quantity,
		Automatic:            manualQuantity == 0,
		Status:               localOrderStatus(remote.Status),
		RemoteStatus:         remote.Status,
		ReadyQuantity:        remote.ReadyQuantity,
		Progress:             remote.Progress,
		StatusURL:            remote.StatusURL,
		TakeURL:              remote.TakeURL,
		ChargedFen:           remote.ChargedFen,
		ReleasedFen:          remote.ReleasedFen,
		NextPollAtMS:         nextPollAt(supplyCfg, remote.RetryAfterSeconds),
		SupplierRetryUntilMS: supplierRetryUntilMS(remote.RetryAfterSeconds),
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
			if smartEmergencyShortage(resource) {
				resource.EmergencyShortage = true
				resource.SuggestedAction = smartActionEmergencyReplenish
				resource.DecisionReason = "emergency_capacity_shortage"
				s.setSmartResource(resource)
				return false, nil
			}
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
		if strings.HasPrefix(order.OrderID, "recovery-") {
			order.Status = "recovery_partial"
		} else {
			order.Status = "partial"
		}
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

func cpaManagementConfigured(cfg store.ManagerConfig) bool {
	return strings.TrimSpace(cfg.CPAConnection.CPABaseURL) != "" &&
		strings.TrimSpace(cfg.CPAConnection.ManagementKey) != ""
}

func normalizeReportRequest(req ReportRequest) ReportRequest {
	now := time.Now()
	if req.ToMS <= 0 {
		req.ToMS = now.UnixMilli()
	}
	if req.FromMS <= 0 {
		req.FromMS = time.UnixMilli(req.ToMS).AddDate(0, 0, -30).UnixMilli()
	}
	if req.ToMS <= req.FromMS {
		req.ToMS = time.UnixMilli(req.FromMS).Add(24 * time.Hour).UnixMilli()
	}
	maxRange := time.UnixMilli(req.ToMS).AddDate(-1, 0, 0).UnixMilli()
	if req.FromMS < maxRange {
		req.FromMS = maxRange
	}
	if req.Limit <= 0 || req.Limit > 10000 {
		req.Limit = 5000
	}
	return req
}

func (s *Service) supplyUsageStats(ctx context.Context, req ReportRequest, authFiles []string) ([]store.ModelStat, []store.TimelinePoint, error) {
	if len(authFiles) == 0 {
		return nil, nil, nil
	}
	const chunkSize = 200
	statsByKey := make(map[string]*store.ModelStat)
	modelStats := make([]store.ModelStat, 0)
	timeline := make([]store.TimelinePoint, 0)
	for start := 0; start < len(authFiles); start += chunkSize {
		end := start + chunkSize
		if end > len(authFiles) {
			end = len(authFiles)
		}
		filter := store.AnalyticsFilter{
			FromMS:    req.FromMS,
			ToMS:      req.ToMS,
			AuthFiles: authFiles[start:end],
		}
		chunkStats, err := s.store.ModelStatsWithFilter(ctx, filter, 0)
		if err != nil {
			return nil, nil, err
		}
		for _, stat := range chunkStats {
			key := strings.Join([]string{stat.Model, stat.BillingModel, stat.ServiceTier}, "\x00")
			existing := statsByKey[key]
			if existing == nil {
				statCopy := stat
				statsByKey[key] = &statCopy
				modelStats = append(modelStats, statCopy)
				continue
			}
			addReportModelStat(existing, stat)
		}
		chunkTimeline, err := s.store.TimelineWithFilter(ctx, filter, "day", time.Local)
		if err != nil {
			return nil, nil, err
		}
		timeline = append(timeline, chunkTimeline...)
	}
	for index := range modelStats {
		key := strings.Join([]string{modelStats[index].Model, modelStats[index].BillingModel, modelStats[index].ServiceTier}, "\x00")
		if merged := statsByKey[key]; merged != nil {
			modelStats[index] = *merged
		}
	}
	return modelStats, timeline, nil
}

func supplyUsageAuthFiles(items []store.SupplyImportItem) []string {
	seen := make(map[string]struct{}, len(items))
	authFiles := make([]string, 0, len(items))
	for _, item := range items {
		fileName := strings.TrimSpace(item.FileName)
		if fileName == "" {
			continue
		}
		if _, ok := seen[fileName]; ok {
			continue
		}
		seen[fileName] = struct{}{}
		authFiles = append(authFiles, fileName)
	}
	sort.Strings(authFiles)
	return authFiles
}

func addReportModelStat(target *store.ModelStat, stat store.ModelStat) {
	target.Calls += stat.Calls
	target.SuccessCalls += stat.SuccessCalls
	target.InputTokens += stat.InputTokens
	target.OutputTokens += stat.OutputTokens
	target.ReasoningTokens += stat.ReasoningTokens
	target.CachedTokens += stat.CachedTokens
	target.CacheReadTokens += stat.CacheReadTokens
	target.CacheCreationTokens += stat.CacheCreationTokens
	target.LongInputTokens += stat.LongInputTokens
	target.LongOutputTokens += stat.LongOutputTokens
	target.LongCachedTokens += stat.LongCachedTokens
	target.LongCacheReadTokens += stat.LongCacheReadTokens
	target.LongCacheCreationTokens += stat.LongCacheCreationTokens
	target.TotalTokens += stat.TotalTokens
}

func buildSupplyReport(req ReportRequest, orders []store.SupplyOrder, recoveries []store.SupplyRecovery, items []store.SupplyImportItem, now time.Time) Report {
	report := Report{
		Range: ReportRange{
			FromMS:        req.FromMS,
			ToMS:          req.ToMS,
			GeneratedAtMS: now.UnixMilli(),
			Days:          max(1, int(math.Ceil(float64(req.ToMS-req.FromMS)/float64(24*time.Hour/time.Millisecond)))),
		},
		Risk: ReportRisk{ClaimableAgeBuckets: []ReportRiskBucket{
			{Key: "lt_1h", Label: "<1h"},
			{Key: "1_6h", Label: "1-6h"},
			{Key: "6_24h", Label: "6-24h"},
			{Key: "gt_24h", Label: ">24h"},
		}},
	}
	report.Executive.UsageRevenueCurrency = "USD"
	timeline := make(map[int64]*ReportTimelinePoint)
	for bucket := reportDayBucketMS(req.FromMS); bucket < req.ToMS; bucket = reportNextDayBucketMS(bucket) {
		ensureReportTimelinePoint(timeline, bucket)
		if len(timeline) > 370 {
			break
		}
	}
	productStats := make(map[string]*ReportDimensionStat)
	orderStatusStats := make(map[string]*ReportDimensionStat)
	recoveryStatusStats := make(map[string]*ReportDimensionStat)
	deliveryStatusStats := make(map[string]*ReportDimensionStat)
	sourceStats := make(map[string]*ReportDimensionStat)

	var orderFulfillmentTotal int64
	var orderFulfillmentSamples int
	for _, order := range orders {
		source := reportOrderSource(order)
		product := reportKey(order.Product)
		status := reportKey(order.Status)
		quantity := order.RequestedQuantity
		if quantity <= 0 {
			quantity = order.ItemCount
		}
		report.Executive.Orders++
		report.Executive.RequestedAccounts += quantity
		report.Executive.ImportedAccounts += order.ImportedCount
		report.Executive.ChargedFen += order.ChargedFen
		report.Executive.ReleasedFen += order.ReleasedFen
		switch source {
		case "manual":
			report.Executive.ManualOrders++
		case "recovery":
			report.Executive.RecoveryOrders++
		default:
			report.Executive.AutomaticOrders++
		}
		if reportOpenOrderStatus(order.Status) {
			report.Risk.OpenOrders++
		}
		if order.CompletedAtMS > 0 && order.CreatedAtMS > 0 && order.CompletedAtMS >= order.CreatedAtMS {
			orderFulfillmentTotal += (order.CompletedAtMS - order.CreatedAtMS) / 1000
			orderFulfillmentSamples++
		}
		for _, stat := range []*ReportDimensionStat{
			reportDimension(productStats, product),
			reportDimension(orderStatusStats, status),
			reportDimension(sourceStats, source),
		} {
			stat.Count++
			stat.Orders++
			stat.Quantity += quantity
			stat.Imported += order.ImportedCount
			stat.ChargedFen += order.ChargedFen
			stat.ReleasedFen += order.ReleasedFen
		}
		point := ensureReportTimelinePoint(timeline, reportDayBucketMS(order.CreatedAtMS))
		point.Orders++
		point.Requested += quantity
		point.ChargedFen += order.ChargedFen
	}

	var recoveryClaimTotal int64
	var recoveryClaimSamples int
	var recoveryImportTotal int64
	var recoveryImportSamples int
	for _, recovery := range recoveries {
		status := reportKey(recovery.Status)
		product := reportKey(recovery.Product)
		deliveryStatus := reportKey(recovery.DeliveryStatus)
		report.Executive.Recoveries++
		report.Executive.RefundedFen += recovery.RefundedFen
		switch status {
		case "claimable":
			report.Executive.ClaimableRecoveries++
			report.Risk.UnclaimedRecoveries++
			reportAddClaimableAge(&report.Risk, recovery, now)
		case "claiming", "importing", "partial", "imported", "claimed":
			report.Executive.ClaimedRecoveries++
		case "refunded":
			report.Executive.RefundedRecoveries++
		case "failed":
			report.Executive.FailedRecoveries++
		}
		if status == "imported" {
			report.Executive.ImportedRecoveries++
		}
		if status == "partial" {
			report.Risk.PartialRecoveries++
		}
		if recovery.ClaimedAtMS > 0 {
			start := reportFirstPositiveMS(recovery.CreatedAtMS, recovery.LastSeenAtMS)
			if start > 0 && recovery.ClaimedAtMS >= start {
				recoveryClaimTotal += (recovery.ClaimedAtMS - start) / 1000
				recoveryClaimSamples++
			}
		}
		if status == "imported" && recovery.UpdatedAtMS > 0 {
			start := reportFirstPositiveMS(recovery.ClaimedAtMS, recovery.CreatedAtMS, recovery.LastSeenAtMS)
			if start > 0 && recovery.UpdatedAtMS >= start {
				recoveryImportTotal += (recovery.UpdatedAtMS - start) / 1000
				recoveryImportSamples++
			}
		}
		for _, stat := range []*ReportDimensionStat{
			reportDimension(productStats, product),
			reportDimension(recoveryStatusStats, status),
			reportDimension(deliveryStatusStats, deliveryStatus),
		} {
			stat.Count++
			stat.Recoveries++
			stat.Imported += recovery.ImportedCount
			stat.RefundedFen += recovery.RefundedFen
		}
		point := ensureReportTimelinePoint(timeline, reportDayBucketMS(reportFirstPositiveMS(recovery.CreatedAtMS, recovery.LastSeenAtMS, recovery.UpdatedAtMS)))
		point.Recoveries++
		if recovery.ClaimedAtMS > 0 || reportRecoveryClaimedStatus(status) {
			point.RecoveryClaimed++
		}
		if status == "imported" {
			point.RecoveryImported++
		}
		if status == "refunded" {
			point.RecoveryRefunded++
		}
	}

	var attempts int
	var importRegistrationTotal int64
	var importRegistrationSamples int
	for _, item := range items {
		status := strings.ToLower(strings.TrimSpace(item.Status))
		report.ImportHealth.Items++
		attempts += item.AttemptCount
		switch status {
		case "imported":
			report.ImportHealth.ImportedItems++
			if item.ImportedAtMS > 0 && item.CreatedAtMS > 0 && item.ImportedAtMS >= item.CreatedAtMS {
				importRegistrationTotal += (item.ImportedAtMS - item.CreatedAtMS) / 1000
				importRegistrationSamples++
			}
			if item.LeaseExpiresAtMS > 0 {
				if item.LeaseExpiresAtMS <= now.UnixMilli() {
					report.ImportHealth.ExpiredItems++
				} else if item.LeaseExpiresAtMS <= now.Add(15*time.Minute).UnixMilli() {
					report.ImportHealth.ExpiringSoonItems++
				}
			}
		case "failed":
			report.ImportHealth.FailedItems++
			report.Risk.FailedImportItems++
			report.Risk.ImportBacklogItems++
			if item.NextRetryAtMS > now.UnixMilli() {
				report.ImportHealth.RetryingItems++
			}
		default:
			report.ImportHealth.PendingItems++
			report.Risk.ImportBacklogItems++
		}
		point := ensureReportTimelinePoint(timeline, reportDayBucketMS(reportFirstPositiveMS(item.ImportedAtMS, item.UpdatedAtMS, item.CreatedAtMS)))
		if status == "imported" {
			point.Imported++
		}
		if status == "failed" {
			point.ImportFailures++
		}
	}

	report.Executive.NetFen = report.Executive.ChargedFen - report.Executive.ReleasedFen - report.Executive.RefundedFen
	report.Executive.SupplySpendFen = report.Executive.ChargedFen
	report.Executive.SupplyNetSpendFen = report.Executive.NetFen
	report.Executive.AverageUnitFen = reportRatioFloat(float64(report.Executive.ChargedFen), float64(report.Executive.ImportedAccounts))
	claimBase := report.Executive.ClaimableRecoveries + report.Executive.ClaimedRecoveries + report.Executive.FailedRecoveries
	report.Executive.RecoveryClaimRate = reportRatio(float64(report.Executive.ClaimedRecoveries), float64(claimBase))
	report.Executive.RecoveryImportRate = reportRatio(float64(report.Executive.ImportedRecoveries), float64(report.Executive.ClaimedRecoveries))
	report.Executive.RecoveryRefundRate = reportRatio(float64(report.Executive.RefundedRecoveries), float64(report.Executive.Recoveries))
	report.Executive.ImportSuccessRate = reportRatio(float64(report.ImportHealth.ImportedItems), float64(report.ImportHealth.Items))
	report.ImportHealth.SuccessRate = report.Executive.ImportSuccessRate
	report.ImportHealth.AverageAttempts = reportRatioFloat(float64(attempts), float64(report.ImportHealth.Items))
	report.Timing.AverageOrderFulfillmentSeconds = reportRatioFloat(float64(orderFulfillmentTotal), float64(orderFulfillmentSamples))
	report.Timing.AverageRecoveryClaimSeconds = reportRatioFloat(float64(recoveryClaimTotal), float64(recoveryClaimSamples))
	report.Timing.AverageRecoveryImportSeconds = reportRatioFloat(float64(recoveryImportTotal), float64(recoveryImportSamples))
	report.Timing.AverageImportRegistrationSeconds = reportRatioFloat(float64(importRegistrationTotal), float64(importRegistrationSamples))

	report.Timeline = reportTimelinePoints(timeline)
	report.Products = reportDimensionStats(productStats)
	report.OrderStatuses = reportDimensionStats(orderStatusStats)
	report.RecoveryStatuses = reportDimensionStats(recoveryStatusStats)
	report.DeliveryStatuses = reportDimensionStats(deliveryStatusStats)
	report.Sources = reportDimensionStats(sourceStats)
	return report
}

func applyUsageRevenueToReport(report *Report, stats []store.ModelStat, timeline []store.TimelinePoint, prices map[string]store.ModelPrice) {
	if report == nil {
		return
	}
	models := make([]ReportUsageModelStat, 0, len(stats))
	for _, stat := range stats {
		revenue := reportCostForStat(stat, prices)
		report.Executive.UsageCalls += stat.Calls
		report.Executive.UsageTokens += stat.TotalTokens
		report.Executive.UsageRevenue += revenue
		models = append(models, ReportUsageModelStat{
			Model:        stat.Model,
			BillingModel: stat.BillingModel,
			ServiceTier:  stat.ServiceTier,
			Calls:        stat.Calls,
			SuccessCalls: stat.SuccessCalls,
			Tokens:       stat.TotalTokens,
			Revenue:      reportRatioFloat(revenue, 1),
		})
	}
	report.Executive.UsageRevenue = reportRatioFloat(report.Executive.UsageRevenue, 1)
	report.Executive.AverageRevenuePerCall = reportRatioFloat(report.Executive.UsageRevenue, float64(report.Executive.UsageCalls))
	sort.Slice(models, func(i, j int) bool {
		if models[i].Revenue == models[j].Revenue {
			if models[i].Calls == models[j].Calls {
				return models[i].Model < models[j].Model
			}
			return models[i].Calls > models[j].Calls
		}
		return models[i].Revenue > models[j].Revenue
	})
	if len(models) > 20 {
		models = models[:20]
	}
	report.UsageModels = models

	timelineIndex := make(map[int64]int, len(report.Timeline))
	for i := range report.Timeline {
		timelineIndex[report.Timeline[i].BucketMS] = i
	}
	for _, point := range timeline {
		bucket := reportDayBucketMS(point.BucketMS)
		if bucket <= 0 {
			continue
		}
		index, ok := timelineIndex[bucket]
		if !ok {
			report.Timeline = append(report.Timeline, ReportTimelinePoint{
				BucketMS: bucket,
				Label:    time.UnixMilli(bucket).Format("2006-01-02"),
			})
			index = len(report.Timeline) - 1
			timelineIndex[bucket] = index
		}
		report.Timeline[index].UsageCalls += point.Calls
		report.Timeline[index].UsageTokens += point.Tokens
		report.Timeline[index].UsageRevenue += reportCostForTimelinePoint(point, prices)
	}
	for i := range report.Timeline {
		report.Timeline[i].UsageRevenue = reportRatioFloat(report.Timeline[i].UsageRevenue, 1)
	}
	sort.Slice(report.Timeline, func(i, j int) bool { return report.Timeline[i].BucketMS < report.Timeline[j].BucketMS })
}

func reportCostForStat(stat store.ModelStat, prices map[string]store.ModelPrice) float64 {
	return pricing.CostForModelCandidatesWithServiceTier([]string{stat.BillingModel, stat.Model}, stat.ServiceTier, pricing.ModelTokens{
		InputTokens:             stat.InputTokens,
		OutputTokens:            stat.OutputTokens,
		CachedTokens:            stat.CachedTokens,
		CacheReadTokens:         stat.CacheReadTokens,
		CacheCreationTokens:     stat.CacheCreationTokens,
		LongInputTokens:         stat.LongInputTokens,
		LongOutputTokens:        stat.LongOutputTokens,
		LongCachedTokens:        stat.LongCachedTokens,
		LongCacheReadTokens:     stat.LongCacheReadTokens,
		LongCacheCreationTokens: stat.LongCacheCreationTokens,
	}, prices)
}

func reportCostForTimelinePoint(point store.TimelinePoint, prices map[string]store.ModelPrice) float64 {
	return pricing.CostForModelCandidatesWithServiceTier([]string{point.BillingModel, point.Model}, point.ServiceTier, pricing.ModelTokens{
		InputTokens:             point.InputTokens,
		OutputTokens:            point.OutputTokens,
		CachedTokens:            point.CachedTokens,
		CacheReadTokens:         point.CacheReadTokens,
		CacheCreationTokens:     point.CacheCreationTokens,
		LongInputTokens:         point.LongInputTokens,
		LongOutputTokens:        point.LongOutputTokens,
		LongCachedTokens:        point.LongCachedTokens,
		LongCacheReadTokens:     point.LongCacheReadTokens,
		LongCacheCreationTokens: point.LongCacheCreationTokens,
	}, prices)
}

func reportDimension(values map[string]*ReportDimensionStat, key string) *ReportDimensionStat {
	key = reportKey(key)
	if stat, ok := values[key]; ok {
		return stat
	}
	stat := &ReportDimensionStat{Key: key, Label: key}
	values[key] = stat
	return stat
}

func reportDimensionStats(values map[string]*ReportDimensionStat) []ReportDimensionStat {
	stats := make([]ReportDimensionStat, 0, len(values))
	for _, stat := range values {
		if stat.Quantity > 0 && stat.Recoveries > 0 {
			stat.SuccessRate = reportRatio(float64(stat.Imported), float64(stat.Quantity+stat.Recoveries))
		} else if stat.Quantity > 0 {
			stat.SuccessRate = reportRatio(float64(stat.Imported), float64(stat.Quantity))
		} else if stat.Recoveries > 0 {
			stat.SuccessRate = reportRatio(float64(stat.Imported), float64(stat.Recoveries))
		}
		stats = append(stats, *stat)
	}
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].Count == stats[j].Count {
			return stats[i].Key < stats[j].Key
		}
		return stats[i].Count > stats[j].Count
	})
	return stats
}

func reportTimelinePoints(values map[int64]*ReportTimelinePoint) []ReportTimelinePoint {
	points := make([]ReportTimelinePoint, 0, len(values))
	for _, point := range values {
		points = append(points, *point)
	}
	sort.Slice(points, func(i, j int) bool { return points[i].BucketMS < points[j].BucketMS })
	return points
}

func ensureReportTimelinePoint(values map[int64]*ReportTimelinePoint, bucketMS int64) *ReportTimelinePoint {
	if bucketMS <= 0 {
		bucketMS = reportDayBucketMS(time.Now().UnixMilli())
	}
	if point, ok := values[bucketMS]; ok {
		return point
	}
	point := &ReportTimelinePoint{BucketMS: bucketMS, Label: time.UnixMilli(bucketMS).Format("2006-01-02")}
	values[bucketMS] = point
	return point
}

func reportDayBucketMS(value int64) int64 {
	if value <= 0 {
		return 0
	}
	t := time.UnixMilli(value)
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location()).UnixMilli()
}

func reportNextDayBucketMS(value int64) int64 {
	if value <= 0 {
		return reportDayBucketMS(time.Now().UnixMilli())
	}
	return time.UnixMilli(value).AddDate(0, 0, 1).UnixMilli()
}

func reportKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "unknown"
	}
	return value
}

func reportOrderSource(order store.SupplyOrder) string {
	if strings.HasPrefix(order.OrderID, "recovery-") || order.RemoteStatus == "recovery_claimed" {
		return "recovery"
	}
	if order.Automatic {
		return "automatic"
	}
	return "manual"
}

func reportOpenOrderStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case "creating", "create_uncertain", "created", "waiting_inventory", "ready", "taking", "importing", "partial", "recovery_importing", "recovery_partial":
		return true
	default:
		return false
	}
}

func reportRecoveryClaimedStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case "claimed", "claiming", "importing", "partial", "imported":
		return true
	default:
		return false
	}
}

func reportAddClaimableAge(risk *ReportRisk, recovery store.SupplyRecovery, now time.Time) {
	start := reportFirstPositiveMS(recovery.CreatedAtMS, recovery.LastSeenAtMS, recovery.UpdatedAtMS)
	if start <= 0 {
		return
	}
	age := now.Sub(time.UnixMilli(start))
	switch {
	case age < time.Hour:
		risk.ClaimableAgeBuckets[0].Count++
	case age < 6*time.Hour:
		risk.ClaimableAgeBuckets[1].Count++
		risk.StaleClaimableRecoveries++
	case age < 24*time.Hour:
		risk.ClaimableAgeBuckets[2].Count++
		risk.StaleClaimableRecoveries++
	default:
		risk.ClaimableAgeBuckets[3].Count++
		risk.StaleClaimableRecoveries++
	}
}

func reportFirstPositiveMS(values ...int64) int64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func reportRatio(numerator float64, denominator float64) float64 {
	if denominator <= 0 {
		return 0
	}
	return math.Round((numerator/denominator)*10000) / 10000
}

func reportRatioFloat(numerator float64, denominator float64) float64 {
	if denominator <= 0 {
		return 0
	}
	return math.Round((numerator/denominator)*100) / 100
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

func (s *Service) syncRecoveriesOnce(ctx context.Context, cfg store.ManagerConfig, autoClaim bool, limit int, recoveryID string) (RecoverySyncResult, error) {
	var result RecoverySyncResult
	var firstErr error
	mergePendingResult := func(imported int, failed int, err error) {
		result.Imported += imported
		result.Failed += failed
		if err != nil {
			if failed == 0 {
				result.Failed++
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if imported, failed, err := s.processPendingRecoveryImports(ctx, cfg, limit); err != nil {
		mergePendingResult(imported, failed, err)
	} else {
		mergePendingResult(imported, failed, nil)
	}
	credentials := credentialsFromConfig(cfg.Supply)
	remoteRecoveries, err := s.supplyClient.Recoveries(ctx, credentials)
	if err != nil {
		if firstErr != nil {
			return result, errors.Join(firstErr, err)
		}
		return result, err
	}
	localRecoveries := make([]store.SupplyRecovery, 0, len(remoteRecoveries))
	for _, remote := range remoteRecoveries {
		local := supplyRecoveryFromClient(remote)
		if local.RecoveryID == "" {
			continue
		}
		result.Seen++
		if local.Status == "claimable" {
			result.Claimable++
		}
		if local.Status == "refunded" {
			result.Refunded++
		}
		localRecoveries = append(localRecoveries, local)
	}
	if _, err := s.store.UpsertSupplyRecoveries(ctx, localRecoveries); err != nil {
		if firstErr != nil {
			return result, errors.Join(firstErr, err)
		}
		return result, err
	}
	if !autoClaim {
		if firstErr != nil {
			return result, firstErr
		}
		return result, nil
	}
	var claimable []store.SupplyRecovery
	if strings.TrimSpace(recoveryID) != "" {
		if recovery, found, err := s.store.GetSupplyRecovery(ctx, recoveryID); err != nil {
			if firstErr != nil {
				return result, errors.Join(firstErr, err)
			}
			return result, err
		} else if found && recovery.Status == "claimable" {
			claimable = []store.SupplyRecovery{recovery}
		}
	} else {
		var err error
		claimable, err = s.store.ListClaimableSupplyRecoveries(ctx, limit)
		if err != nil {
			if firstErr != nil {
				return result, errors.Join(firstErr, err)
			}
			return result, err
		}
	}
	for _, candidate := range claimable {
		recovery, claimed, err := s.store.ClaimSupplyRecoveryForProcessing(ctx, candidate.RecoveryID, time.Now().UnixMilli())
		if err != nil {
			result.Failed++
			continue
		}
		if !claimed {
			continue
		}
		if err := s.claimRecovery(ctx, cfg, recovery); err != nil {
			result.Failed++
			_ = s.store.MarkSupplyRecoveryFailed(ctx, recovery.RecoveryID, safeError(err))
			continue
		}
		result.Claimed++
	}
	if imported, failed, err := s.processPendingRecoveryImports(ctx, cfg, limit); err != nil {
		mergePendingResult(imported, failed, err)
	} else {
		mergePendingResult(imported, failed, nil)
	}
	if firstErr != nil {
		return result, firstErr
	}
	return result, nil
}

func (s *Service) claimRecovery(ctx context.Context, cfg store.ManagerConfig, recovery store.SupplyRecovery) error {
	claimed, err := s.supplyClient.ClaimRecovery(ctx, credentialsFromConfig(cfg.Supply), recovery.RecoveryID, recovery.ClaimURL)
	if err != nil {
		return err
	}
	normalized := make([]normalizedSupplyAccount, 0, len(claimed.Accounts))
	for index, raw := range claimed.Accounts {
		accounts, err := normalizeAccountPayloads(raw)
		if err != nil {
			return fmt.Errorf("recovery account %d format is unsupported: %w", index+1, err)
		}
		normalized = append(normalized, accounts...)
	}
	if len(normalized) == 0 {
		return errors.New("recovery claim response did not include importable accounts")
	}
	orderID := recoveryOrderID(recovery.RecoveryID)
	if _, found, err := s.store.GetSupplyOrder(ctx, orderID); err != nil {
		return err
	} else if !found {
		product := firstNonEmptyString(recovery.Product, cfg.Supply.Product)
		if product == "" {
			product = "oauth_30d"
		}
		if _, err := s.store.CreateSupplyOrder(ctx, store.SupplyOrder{
			OrderID:           orderID,
			Product:           product,
			RequestedQuantity: len(normalized),
			Automatic:         true,
			Status:            "recovery_importing",
			RemoteStatus:      "recovery_claimed",
			ItemCount:         len(normalized),
		}); err != nil {
			return err
		}
	}
	items := make([]store.SupplyImportItem, 0, len(normalized))
	seen := make(map[string]struct{}, len(normalized))
	for _, account := range normalized {
		if _, duplicate := seen[account.itemKey]; duplicate {
			continue
		}
		seen[account.itemKey] = struct{}{}
		items = append(items, store.SupplyImportItem{
			OrderID:          orderID,
			ItemKey:          account.itemKey,
			FileName:         account.fileName,
			PayloadJSON:      string(account.payload),
			LeaseExpiresAtMS: account.leaseExpiresAtMS,
		})
	}
	inserted, err := s.store.InsertSupplyImportItems(ctx, orderID, items)
	if err != nil {
		return err
	}
	if inserted == 0 && len(items) > 0 {
		inserted = len(items)
	}
	if err := s.store.MarkSupplyRecoveryClaimed(ctx, recovery.RecoveryID, orderID, inserted, time.Now().UnixMilli()); err != nil {
		return err
	}
	return nil
}

func (s *Service) processPendingRecoveryImports(ctx context.Context, cfg store.ManagerConfig, limit int) (int, int, error) {
	recoveries, err := s.store.ListImportPendingSupplyRecoveries(ctx, limit)
	if err != nil {
		return 0, 0, err
	}
	importedRecoveries := 0
	failedRecoveries := 0
	var firstErr error
	for _, recovery := range recoveries {
		if strings.TrimSpace(recovery.ClaimOrderID) == "" {
			continue
		}
		order, found, err := s.store.GetSupplyOrder(ctx, recovery.ClaimOrderID)
		if err != nil {
			failedRecoveries++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !found {
			err := fmt.Errorf("recovery import order %s was not found", recovery.ClaimOrderID)
			_ = s.store.MarkSupplyRecoveryFailed(ctx, recovery.RecoveryID, safeError(err))
			failedRecoveries++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		err = s.importItems(ctx, cfg, &order)
		total, imported, countsErr := s.store.SupplyImportCounts(ctx, recovery.ClaimOrderID)
		if countsErr != nil {
			failedRecoveries++
			if firstErr == nil {
				firstErr = countsErr
			}
			continue
		}
		if total > 0 && imported == total {
			if markErr := s.store.MarkSupplyRecoveryImported(ctx, recovery.RecoveryID, imported); markErr != nil {
				failedRecoveries++
				if firstErr == nil {
					firstErr = markErr
				}
				continue
			}
			importedRecoveries++
			if disableErr := s.disableRecoveredOriginal(ctx, cfg, recovery); disableErr != nil {
				message := "original account disable failed: " + safeError(disableErr)
				_ = s.store.SetSupplyRecoveryLastError(ctx, recovery.RecoveryID, message)
				failedRecoveries++
				if firstErr == nil {
					firstErr = errors.New(message)
				}
			}
			continue
		}
		message := ""
		if err != nil {
			message = safeError(err)
		}
		if markErr := s.store.MarkSupplyRecoveryImportProgress(ctx, recovery.RecoveryID, total, imported, message); markErr != nil {
			failedRecoveries++
			if firstErr == nil {
				firstErr = markErr
			}
			continue
		}
		if err != nil {
			failedRecoveries++
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return importedRecoveries, failedRecoveries, firstErr
}

func (s *Service) disableRecoveredOriginal(ctx context.Context, cfg store.ManagerConfig, recovery store.SupplyRecovery) error {
	if !recoveryDisableOriginalEnabled(cfg.Supply) || strings.TrimSpace(recovery.OriginalFileName) == "" ||
		strings.TrimSpace(cfg.CPAConnection.CPABaseURL) == "" || strings.TrimSpace(cfg.CPAConnection.ManagementKey) == "" {
		return nil
	}
	return s.authFiles.PatchDisabled(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey,
		recovery.OriginalFileName, true, recovery.OriginalAuthIndex)
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
	if smartEmergencyShortage(resource) {
		limit := smartAutomaticOrderQuantityLimit(cfg, resource)
		minimum := min(smartPrelockMinQuantity(cfg), limit)
		return clampInt(quantity, minimum, limit), "emergency_capacity_shortage"
	}
	if resource.DemandTrend == smartDemandTrendFalling && !smartEmergencyShortage(resource) {
		return 0, "demand_falling_observe"
	}
	if resource.DemandTrend == smartDemandTrendRising && !smartEmergencyShortage(resource) {
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
	if resource.DemandTrend == smartDemandTrendFalling && !smartEmergencyShortage(resource) {
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
	if resource.DemandTrend == smartDemandTrendRising && !smartEmergencyShortage(resource) {
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
	if smartEmergencyShortage(resource) {
		return 1
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
	if smartEmergencyShortage(resource) {
		return true
	}
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

func (s *Service) currentRecoverySummary(ctx context.Context, cfg store.ManagerSupplyConfig) RecoverySummary {
	enabled := recoverySyncEnabled(cfg) && supplyCredentialsConfigured(cfg)
	s.recoveryMu.Lock()
	summary := s.recoveryState
	s.recoveryMu.Unlock()
	summary.Enabled = enabled
	summary.AutoClaim = recoveryAutoClaimEnabled(cfg)
	if s != nil && s.store != nil {
		if stored, err := s.store.SupplyRecoverySummary(ctx); err == nil {
			summary.Total = stored.Total
			summary.Claimable = stored.Claimable
			summary.Importing = stored.Importing
			summary.StoredImported = stored.Imported
			summary.StoredRefunded = stored.Refunded
			summary.StoredFailed = stored.Failed
		}
	}
	return summary
}

func (s *Service) recordRecoveryError(ctx context.Context, cfg store.ManagerSupplyConfig, err error) {
	if err == nil {
		return
	}
	summary := s.currentRecoverySummary(ctx, cfg)
	now := time.Now()
	summary.LastSyncAtMS = now.UnixMilli()
	summary.NextSyncAtMS = now.Add(recoverySyncInterval(cfg, err)).UnixMilli()
	summary.LastResult = "failed"
	summary.LastError = safeError(err)
	s.recoveryMu.Lock()
	s.recoveryState = summary
	s.recoveryMu.Unlock()
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

func recoverySyncEnabled(cfg store.ManagerSupplyConfig) bool {
	return cfg.RecoverySyncEnabled == nil || *cfg.RecoverySyncEnabled
}

func recoveryAutoClaimEnabled(cfg store.ManagerSupplyConfig) bool {
	return cfg.RecoveryAutoClaim == nil || *cfg.RecoveryAutoClaim
}

func recoveryDisableOriginalEnabled(cfg store.ManagerSupplyConfig) bool {
	return cfg.RecoveryDisableOriginal == nil || *cfg.RecoveryDisableOriginal
}

func recoveryClaimBatchSize(cfg store.ManagerSupplyConfig) int {
	if cfg.RecoveryClaimBatchSize > 0 {
		return min(cfg.RecoveryClaimBatchSize, 100)
	}
	return 20
}

func recoverySyncInterval(cfg store.ManagerSupplyConfig, err error) time.Duration {
	seconds := cfg.RecoverySyncIntervalSeconds
	if seconds <= 0 {
		seconds = 60
	}
	if err != nil {
		if seconds < 60 {
			seconds = 60
		}
		if seconds > 300 {
			seconds = 300
		}
		return time.Duration(seconds) * time.Second
	}
	return time.Duration(seconds) * time.Second
}

func supplyRecoveryFromClient(remote supplyclient.Recovery) store.SupplyRecovery {
	status := supplyRecoveryStatus(remote)
	originalFileName := strings.TrimSpace(remote.OriginalAccount)
	if !strings.HasSuffix(strings.ToLower(originalFileName), ".json") {
		originalFileName = ""
	}
	return store.SupplyRecovery{
		RecoveryID:        strings.TrimSpace(remote.ID),
		Product:           strings.TrimSpace(remote.Product),
		DeliveryStatus:    strings.ToLower(strings.TrimSpace(remote.DeliveryStatus)),
		Status:            status,
		OriginalFileName:  originalFileName,
		OriginalAuthIndex: strings.TrimSpace(remote.OriginalAuthIndex),
		OriginalEmail:     strings.TrimSpace(remote.OriginalEmail),
		ClaimURL:          strings.TrimSpace(remote.ClaimURL),
		RefundedFen:       remote.RefundedFen,
		RawJSON:           string(remote.Raw),
		LastSeenAtMS:      time.Now().UnixMilli(),
	}
}

func supplyRecoveryStatus(remote supplyclient.Recovery) string {
	status := strings.ToLower(strings.TrimSpace(remote.DeliveryStatus))
	switch status {
	case "claimable", "ready", "available":
		if strings.TrimSpace(remote.ClaimURL) != "" {
			return "claimable"
		}
	case "refunded", "refund", "failed_refunded":
		return "refunded"
	case "claimed", "completed", "done":
		return "claimed"
	}
	if remote.RefundedFen > 0 {
		return "refunded"
	}
	return "seen"
}

func recoveryOrderID(recoveryID string) string {
	recoveryID = strings.TrimSpace(recoveryID)
	if recoveryID == "" {
		return "recovery-unknown"
	}
	var builder strings.Builder
	for _, r := range recoveryID {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '-' || r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
	}
	value := strings.Trim(builder.String(), "-_")
	if value == "" {
		value = "unknown"
	}
	return "recovery-" + value
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
	order.SupplierRetryUntilMS = supplierRetryUntilMS(remote.RetryAfterSeconds)
	order.LastError = ""
}

func supplierRetryUntilMS(retryAfterSeconds int) int64 {
	if retryAfterSeconds <= 0 {
		return 0
	}
	return time.Now().Add(time.Duration(retryAfterSeconds) * time.Second).UnixMilli()
}

func (s *Service) emergencyOrderProcessingAllowed(cfg store.ManagerSupplyConfig, order store.SupplyOrder, resource SmartResource) bool {
	if !order.Automatic || !smartSupplyEnabled(cfg) || order.Status == "taking" ||
		order.Status == "creating" || order.Status == "create_uncertain" || order.SupplierRetryUntilMS > time.Now().UnixMilli() {
		return false
	}
	return smartEmergencyShortage(resource)
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
