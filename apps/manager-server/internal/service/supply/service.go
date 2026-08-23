package supply

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
	collectorpkg "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/collector"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	sqliterepo "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/sqlite"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpaauthfiles"
	managerconfigsvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/pricing"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/supplyclient"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/usage"
)

var (
	ErrNotConfigured               = errors.New("account supply is not configured")
	ErrOrderInProgress             = errors.New("a supply order is already in progress")
	ErrInvalidQuantity             = errors.New("replenishment quantity must be between 1 and 10000")
	ErrInsufficientBalance         = errors.New("supply account available balance is insufficient")
	ErrCreateUncertain             = errors.New("supply order creation result is uncertain")
	ErrOrderNotFound               = errors.New("supply order was not found")
	ErrPurchaseTaskNotFound        = errors.New("supply purchase task was not found")
	ErrNotCreateUncertain          = errors.New("supply order is not waiting for create-result confirmation")
	ErrRecoveryImportNotReady      = errors.New("recovery has no local CPA import task to retry")
	ErrCapacitySnapshotUnavailable = errors.New("quota inspection snapshot is unavailable")
)

const (
	// The supplier has no cancel/release endpoint. This local marker closes an
	// automatic reservation that is no longer needed; the supplier releases it
	// by its own order-expiry policy without any outbound cancellation request.
	remoteStatusAutomaticReleasePending = "auto_release_pending"
	automaticReleasePendingMessage      = "automatically released locally; supplier reservation will expire automatically"
	defaultSupplyRevenueMultiplier      = 0.06
	automaticTransientRetryBackoff      = 30 * time.Second
	recoverySyncBackgroundTimeout       = 10 * time.Minute
	automaticRunSlowLogThreshold        = 500 * time.Millisecond
	lowPriceReserveTriggerReason        = "low_price_reserve"
)

type automaticRunTiming struct {
	startedAt time.Time
	stageAt   time.Time
	stage     string
	durations []string
}

func newAutomaticRunTiming() *automaticRunTiming {
	now := time.Now()
	return &automaticRunTiming{startedAt: now, stageAt: now, stage: "run-lock"}
}

func (t *automaticRunTiming) next(stage string) {
	if t == nil {
		return
	}
	now := time.Now()
	if t.stage != "" {
		t.durations = append(t.durations, fmt.Sprintf("%s=%s", t.stage, now.Sub(t.stageAt).Round(time.Millisecond)))
	}
	t.stage = stage
	t.stageAt = now
}

func (t *automaticRunTiming) finish(err error) {
	if t == nil {
		return
	}
	t.next("")
	total := time.Since(t.startedAt)
	if err == nil && total < automaticRunSlowLogThreshold {
		return
	}
	log.Printf("[supply] automatic check completed duration=%s stages=[%s] err=%v",
		total.Round(time.Millisecond), strings.Join(t.durations, " "), err)
}

type Overview struct {
	CheckedAtMS int64 `json:"checkedAtMs,omitempty"`
	// CPAAvailable is the operator-facing normal account count. Capacity
	// planning has its own SmartResource.AvailableAccounts field; exposing
	// that stricter capacity count here made the legacy overview disagree with
	// the credential-management metrics (for example 9 normal vs 14 verified
	// capacity accounts).
	CPAAvailable       int                     `json:"cpaAvailable"`
	CPATarget          int                     `json:"cpaTarget"`
	CPADeficit         int                     `json:"cpaDeficit"`
	Inventory          *supplyclient.Inventory `json:"inventory,omitempty"`
	Balance            *supplyclient.Balance   `json:"balance,omitempty"`
	SelectedPlatformID string                  `json:"selectedPlatformId,omitempty"`
	Platforms          []PlatformOverview      `json:"platforms,omitempty"`
	LastError          string                  `json:"lastError,omitempty"`
}

type Status struct {
	Config          store.ManagerSupplyConfig      `json:"config"`
	Running         bool                           `json:"running"`
	Overview        Overview                       `json:"overview"`
	AccountPool     *AccountPoolSummary            `json:"accountPool,omitempty"`
	SmartResource   SmartResource                  `json:"smartResource"`
	Automation      AutomationExecution            `json:"automation"`
	LowPriceReserve LowPriceReserveExecution       `json:"lowPriceReserve"`
	Recovery        RecoverySummary                `json:"recovery"`
	ActiveOrder     *store.SupplyOrder             `json:"activeOrder,omitempty"`
	ActiveOrders    []store.SupplyOrder            `json:"activeOrders,omitempty"`
	PurchaseTasks   []store.SupplyPurchaseTask     `json:"purchaseTasks,omitempty"`
	SessionRefresh  []NvtokensSessionRefreshStatus `json:"sessionRefresh,omitempty"`
	Orders          []store.SupplyOrder            `json:"orders"`
}

// AccountPoolSummary is the lightweight, operator-facing account split shared
// by the credential and supply pages. Capacity planning intentionally remains
// on SmartResource: a read-only supply inspection can be conservative enough
// for purchasing without being authoritative for whether a live CPA account
// should be shown as broken.
type AccountPoolSummary struct {
	CheckedAtMS            int64                          `json:"checkedAtMs"`
	Total                  int                            `json:"total"`
	Normal                 int                            `json:"normal"`
	NeedsAttention         int                            `json:"needsAttention"`
	QuotaRisk              int                            `json:"quotaRisk"`
	Disabled               int                            `json:"disabled"`
	Unconfirmed            int                            `json:"unconfirmed"`
	ClassificationObserved bool                           `json:"classificationObserved"`
	Plans                  []AccountPoolPlanSummary       `json:"plans,omitempty"`
	Credentials            []AccountPoolCredentialSummary `json:"credentials,omitempty"`
}

// AccountPoolPlanSummary is the live schedulable account split used only for
// dashboard counts. SmartQuotaPlanEstimate.AccountCount remains tied to the
// completed capacity inspection so changing this view cannot change ordering
// or quota planning.
type AccountPoolPlanSummary struct {
	Key          string `json:"key"`
	SupplierID   string `json:"supplierId,omitempty"`
	SupplierName string `json:"supplierName,omitempty"`
	PlanType     string `json:"planType"`
	AccountCount int    `json:"accountCount"`
}

// AccountPoolCredentialSummary publishes the exact credential-level bucket
// used to build AccountPoolSummary. The credential page consumes this list so
// its status cards and filters cannot drift from the supply page's live pool
// classification while local quota requests are still catching up.
type AccountPoolCredentialSummary struct {
	AuthFileName              string `json:"authFileName"`
	RuntimeID                 string `json:"runtimeId,omitempty"`
	Provider                  string `json:"provider,omitempty"`
	AuthIndex                 string `json:"authIndex,omitempty"`
	AccountID                 string `json:"accountId,omitempty"`
	AccountSnapshot           string `json:"accountSnapshot,omitempty"`
	Bucket                    string `json:"bucket"`
	Schedulable               bool   `json:"schedulable"`
	TemporaryLimited          bool   `json:"temporaryLimited,omitempty"`
	TemporaryLimitKind        string `json:"temporaryLimitKind,omitempty"`
	TemporaryLimitCode        string `json:"temporaryLimitCode,omitempty"`
	TemporaryLimitRecoverAtMS int64  `json:"temporaryLimitRecoverAtMs,omitempty"`
}

// ActiveOrderStatus is deliberately smaller than Status. The management page
// can refresh an active reservation frequently without rebuilding the whole
// account pool, quota snapshot and report summaries on every tick.
type ActiveOrderStatus struct {
	CheckedAtMS    int64               `json:"checkedAtMs"`
	ActiveOrder    *store.SupplyOrder  `json:"activeOrder,omitempty"`
	ActiveOrders   []store.SupplyOrder `json:"activeOrders,omitempty"`
	PollAttempted  bool                `json:"pollAttempted"`
	PollInProgress bool                `json:"pollInProgress"`
	PollError      string              `json:"pollError,omitempty"`
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
	External       int    `json:"external"`
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

type smartEmergencyRetryPlan struct {
	active        bool
	reason        string
	quantityLimit int
	cooldown      time.Duration
}

type ReportRequest struct {
	FromMS int64 `json:"fromMs,omitempty"`
	ToMS   int64 `json:"toMs,omitempty"`
	Limit  int   `json:"limit,omitempty"`
}

type SupplyAccountsRequest struct {
	FromMS int64  `json:"fromMs,omitempty"`
	ToMS   int64  `json:"toMs,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	Status string `json:"status,omitempty"`
}

type ReportRange struct {
	FromMS        int64 `json:"fromMs"`
	ToMS          int64 `json:"toMs"`
	GeneratedAtMS int64 `json:"generatedAtMs"`
	Days          int   `json:"days"`
	Truncated     bool  `json:"truncated"`
}

type SupplyAccountSummary struct {
	Total                 int     `json:"total"`
	Imported              int     `json:"imported"`
	Pending               int     `json:"pending"`
	Failed                int     `json:"failed"`
	Active                int     `json:"active"`
	Disabled              int     `json:"disabled"`
	Expired               int     `json:"expired"`
	Missing               int     `json:"missing"`
	Unknown               int     `json:"unknown"`
	ExpiringSoon          int     `json:"expiringSoon"`
	UsageCalls            int64   `json:"usageCalls"`
	UsageSuccessCalls     int64   `json:"usageSuccessCalls"`
	UsageFailureCalls     int64   `json:"usageFailureCalls"`
	UsageTokens           int64   `json:"usageTokens"`
	UsageRevenue          float64 `json:"usageRevenue"`
	UsageRevenueCurrency  string  `json:"usageRevenueCurrency"`
	RevenueMultiplier     float64 `json:"revenueMultiplier"`
	AverageRevenuePerCall float64 `json:"averageRevenuePerCall"`
	LastUsedAtMS          int64   `json:"lastUsedAtMs,omitempty"`
	CPAStatusError        string  `json:"cpaStatusError,omitempty"`
	Auth401Accounts       int     `json:"auth401Accounts"`
	AutoQuarantined       int     `json:"autoQuarantined"`
}

type SupplyAccountItem struct {
	ID                   int64   `json:"id"`
	FileName             string  `json:"fileName"`
	OrderID              string  `json:"orderId"`
	Source               string  `json:"source"`
	Product              string  `json:"product,omitempty"`
	OrderStatus          string  `json:"orderStatus,omitempty"`
	Status               string  `json:"status"`
	AccountStatus        string  `json:"accountStatus"`
	AccountStatusReason  string  `json:"accountStatusReason,omitempty"`
	CPAProvider          string  `json:"cpaProvider,omitempty"`
	CPAAccount           string  `json:"cpaAccount,omitempty"`
	CPAAccountID         string  `json:"cpaAccountId,omitempty"`
	CPAAuthIndex         string  `json:"cpaAuthIndex,omitempty"`
	CPAStatus            string  `json:"cpaStatus,omitempty"`
	CPADisabled          bool    `json:"cpaDisabled,omitempty"`
	UsageCalls           int64   `json:"usageCalls"`
	UsageSuccessCalls    int64   `json:"usageSuccessCalls"`
	UsageFailureCalls    int64   `json:"usageFailureCalls"`
	UsageTokens          int64   `json:"usageTokens"`
	UsageRevenue         float64 `json:"usageRevenue"`
	UsageRevenueCurrency string  `json:"usageRevenueCurrency"`
	SupplierBasePriceFen int64   `json:"supplierBasePriceFen,omitempty"`
	SupplierChargedFen   int64   `json:"supplierChargedFen,omitempty"`
	SupplierReleasedFen  int64   `json:"supplierReleasedFen,omitempty"`
	LastUsedAtMS         int64   `json:"lastUsedAtMs,omitempty"`
	ImportedAtMS         int64   `json:"importedAtMs,omitempty"`
	ExpiresAtMS          int64   `json:"expiresAtMs,omitempty"`
	LeaseExpiresAtMS     int64   `json:"leaseExpiresAtMs,omitempty"`
	WarrantyExpiresAtMS  int64   `json:"warrantyExpiresAtMs,omitempty"`
	RemainingSeconds     int64   `json:"remainingSeconds,omitempty"`
	Auth401AtMS          int64   `json:"auth401AtMs,omitempty"`
	Auth401BeforeCalls   int64   `json:"auth401BeforeCalls,omitempty"`
	Auth401Reason        string  `json:"auth401Reason,omitempty"`
	AutoDisabledAtMS     int64   `json:"autoDisabledAtMs,omitempty"`
	RecoveryID           string  `json:"recoveryId,omitempty"`
	RecoveryStatus       string  `json:"recoveryStatus,omitempty"`
	AttemptCount         int     `json:"attemptCount"`
	LastError            string  `json:"lastError,omitempty"`
	CreatedAtMS          int64   `json:"createdAtMs"`
	UpdatedAtMS          int64   `json:"updatedAtMs"`
}

type SupplyAccountList struct {
	Range   ReportRange          `json:"range"`
	Summary SupplyAccountSummary `json:"summary"`
	Items   []SupplyAccountItem  `json:"items"`
}

type SupplyAccountLeaseItem struct {
	FileName            string `json:"fileName"`
	OrderID             string `json:"orderId,omitempty"`
	SupplierID          string `json:"supplierId,omitempty"`
	PlatformName        string `json:"platformName,omitempty"`
	Product             string `json:"product,omitempty"`
	Source              string `json:"source,omitempty"`
	ImportMethod        string `json:"importMethod,omitempty"`
	ImportAction        string `json:"importAction,omitempty"`
	ReplacedFileName    string `json:"replacedFileName,omitempty"`
	RecoveryID          string `json:"recoveryId,omitempty"`
	RecoveryStatus      string `json:"recoveryStatus,omitempty"`
	ImportedAtMS        int64  `json:"importedAtMs,omitempty"`
	LeaseExpiresAtMS    int64  `json:"leaseExpiresAtMs,omitempty"`
	WarrantyExpiresAtMS int64  `json:"warrantyExpiresAtMs,omitempty"`
}

type ReportExecutive struct {
	Orders                       int     `json:"orders"`
	ManualOrders                 int     `json:"manualOrders"`
	AutomaticOrders              int     `json:"automaticOrders"`
	RecoveryOrders               int     `json:"recoveryOrders"`
	RequestedAccounts            int     `json:"requestedAccounts"`
	ImportedAccounts             int     `json:"importedAccounts"`
	ChargedFen                   int64   `json:"chargedFen"`
	ReleasedFen                  int64   `json:"releasedFen"`
	NetFen                       int64   `json:"netFen"`
	SupplySpendFen               int64   `json:"supplySpendFen"`
	SupplyNetSpendFen            int64   `json:"supplyNetSpendFen"`
	AverageUnitFen               float64 `json:"averageUnitFen"`
	UsageCalls                   int64   `json:"usageCalls"`
	UsageTokens                  int64   `json:"usageTokens"`
	UsageRevenue                 float64 `json:"usageRevenue"`
	UsageRevenueCurrency         string  `json:"usageRevenueCurrency"`
	RevenueMultiplier            float64 `json:"revenueMultiplier"`
	AverageRevenuePerCall        float64 `json:"averageRevenuePerCall"`
	Recoveries                   int     `json:"recoveries"`
	ClaimableRecoveries          int     `json:"claimableRecoveries"`
	ClaimedRecoveries            int     `json:"claimedRecoveries"`
	ImportedRecoveries           int     `json:"importedRecoveries"`
	RefundedRecoveries           int     `json:"refundedRecoveries"`
	FailedRecoveries             int     `json:"failedRecoveries"`
	RefundedFen                  int64   `json:"refundedFen"`
	RecoveryClaimRate            float64 `json:"recoveryClaimRate"`
	RecoveryImportRate           float64 `json:"recoveryImportRate"`
	RecoveryRefundRate           float64 `json:"recoveryRefundRate"`
	ImportSuccessRate            float64 `json:"importSuccessRate"`
	Auth401Accounts              int     `json:"auth401Accounts"`
	Auth401Events                int     `json:"auth401Events"`
	Auth401Rate                  float64 `json:"auth401Rate"`
	AutoQuarantined              int     `json:"autoQuarantined"`
	EmergencyReplenishments      int     `json:"emergencyReplenishments"`
	VirtualDemandReplenishments  int     `json:"virtualDemandReplenishments"`
	VacuumReplenishments         int     `json:"vacuumReplenishments"`
	VacuumTotalSeconds           int64   `json:"vacuumTotalSeconds"`
	AverageVacuumRecoverySeconds float64 `json:"averageVacuumRecoverySeconds"`
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

type ReportReconciliationSummary struct {
	OrderRows                   int     `json:"orderRows"`
	AccountRows                 int     `json:"accountRows"`
	RecoveryRows                int     `json:"recoveryRows"`
	OrderChargedFen             int64   `json:"orderChargedFen"`
	OrderReleasedFen            int64   `json:"orderReleasedFen"`
	OrderNetFen                 int64   `json:"orderNetFen"`
	AccountAllocatedChargedFen  int64   `json:"accountAllocatedChargedFen"`
	AccountAllocatedReleasedFen int64   `json:"accountAllocatedReleasedFen"`
	AccountAllocatedNetFen      int64   `json:"accountAllocatedNetFen"`
	AccountUsageCalls           int64   `json:"accountUsageCalls"`
	AccountUsageTokens          int64   `json:"accountUsageTokens"`
	AccountUsageRevenue         float64 `json:"accountUsageRevenue"`
	RefundedFen                 int64   `json:"refundedFen"`
	UsageRevenueCurrency        string  `json:"usageRevenueCurrency"`
	AllocationMethod            string  `json:"allocationMethod"`
}

type ReportOrderLedgerRow struct {
	OrderID           string `json:"orderId"`
	Source            string `json:"source"`
	Strategy          string `json:"strategy,omitempty"`
	TriggerReason     string `json:"triggerReason,omitempty"`
	Product           string `json:"product"`
	Status            string `json:"status"`
	RequestedQuantity int    `json:"requestedQuantity"`
	ItemCount         int    `json:"itemCount"`
	ImportedCount     int    `json:"importedCount"`
	ChargedFen        int64  `json:"chargedFen"`
	ReleasedFen       int64  `json:"releasedFen"`
	NetFen            int64  `json:"netFen"`
	CreatedAtMS       int64  `json:"createdAtMs"`
	CompletedAtMS     int64  `json:"completedAtMs,omitempty"`
}

type ReportAccountLedgerRow struct {
	FileName             string  `json:"fileName"`
	OrderID              string  `json:"orderId"`
	Source               string  `json:"source"`
	Product              string  `json:"product,omitempty"`
	Status               string  `json:"status"`
	AccountStatus        string  `json:"accountStatus"`
	ImportedAtMS         int64   `json:"importedAtMs,omitempty"`
	ExpiresAtMS          int64   `json:"expiresAtMs,omitempty"`
	LeaseExpiresAtMS     int64   `json:"leaseExpiresAtMs,omitempty"`
	WarrantyExpiresAtMS  int64   `json:"warrantyExpiresAtMs,omitempty"`
	AllocatedChargedFen  int64   `json:"allocatedChargedFen"`
	AllocatedReleasedFen int64   `json:"allocatedReleasedFen"`
	AllocatedNetFen      int64   `json:"allocatedNetFen"`
	SupplierBasePriceFen int64   `json:"supplierBasePriceFen,omitempty"`
	SupplierChargedFen   int64   `json:"supplierChargedFen,omitempty"`
	SupplierReleasedFen  int64   `json:"supplierReleasedFen,omitempty"`
	UsageCalls           int64   `json:"usageCalls"`
	UsageSuccessCalls    int64   `json:"usageSuccessCalls"`
	UsageFailureCalls    int64   `json:"usageFailureCalls"`
	UsageTokens          int64   `json:"usageTokens"`
	UsageRevenue         float64 `json:"usageRevenue"`
	LastUsedAtMS         int64   `json:"lastUsedAtMs,omitempty"`
	Auth401AtMS          int64   `json:"auth401AtMs,omitempty"`
	AutoDisabledAtMS     int64   `json:"autoDisabledAtMs,omitempty"`
}

type ReportRecoveryLedgerRow struct {
	RecoveryID       string `json:"recoveryId"`
	Product          string `json:"product,omitempty"`
	DeliveryStatus   string `json:"deliveryStatus"`
	Status           string `json:"status"`
	OriginalFileName string `json:"originalFileName,omitempty"`
	ClaimOrderID     string `json:"claimOrderId,omitempty"`
	ItemCount        int    `json:"itemCount"`
	ImportedCount    int    `json:"importedCount"`
	RefundedFen      int64  `json:"refundedFen"`
	LastSeenAtMS     int64  `json:"lastSeenAtMs,omitempty"`
	ClaimedAtMS      int64  `json:"claimedAtMs,omitempty"`
	UpdatedAtMS      int64  `json:"updatedAtMs"`
}

type ReportReconciliation struct {
	Summary    ReportReconciliationSummary `json:"summary"`
	Orders     []ReportOrderLedgerRow      `json:"orders"`
	Accounts   []ReportAccountLedgerRow    `json:"accounts"`
	Recoveries []ReportRecoveryLedgerRow   `json:"recoveries"`
}

type Report struct {
	Range            ReportRange            `json:"range"`
	Executive        ReportExecutive        `json:"executive"`
	ImportHealth     ReportImportHealth     `json:"importHealth"`
	Timing           ReportTiming           `json:"timing"`
	Risk             ReportRisk             `json:"risk"`
	Reconciliation   ReportReconciliation   `json:"reconciliation"`
	Timeline         []ReportTimelinePoint  `json:"timeline"`
	Products         []ReportDimensionStat  `json:"products"`
	Strategies       []ReportDimensionStat  `json:"strategies"`
	TriggerReasons   []ReportDimensionStat  `json:"triggerReasons"`
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

	smartMu                  sync.RWMutex
	smartBuckets             map[int64]*smartUsageBucket
	smartQuotaState          smartQuotaCalibrationState
	smartLiveQuota           map[string]smartLiveQuotaObservation
	quotaPolicyMu            sync.Mutex
	quotaPolicyState         map[string]smartQuotaPlanAdoptionState
	authCacheMu              sync.Mutex
	authRefreshMu            sync.Mutex
	authCache                authFileSnapshot
	quotaSnapshotMu          sync.Mutex
	quotaRefreshMu           sync.Mutex
	quotaSnapshot            inspectionQuotaSnapshot
	operatorHeadersMu        sync.Mutex
	operatorHeaders          operatorHeaderSnapshotCache
	operatorPoolMu           sync.Mutex
	operatorPoolRefreshMu    sync.Mutex
	operatorPoolAsyncMu      sync.Mutex
	operatorPoolGeneration   uint64
	operatorPool             operatorAccountPoolCache
	smartResourceState       SmartResource
	automation               AutomationExecution
	lowPriceReserve          LowPriceReserveExecution
	automaticDecisionSet     bool
	automaticDecision        SmartResource
	recoveryMu               sync.Mutex
	recoveryState            RecoverySummary
	recoveryAsyncMu          sync.Mutex
	recoveryAsyncRunning     bool
	recoverySyncIfDue        func(context.Context) (RecoverySummary, error)
	importMu                 sync.Mutex
	warrantyMigrationMu      sync.Mutex
	warrantyMigrationRunning bool
	warrantyMigrationDone    bool
	poolVacuumMu             sync.Mutex
	poolVacuumStarted        int64

	inspectionSnapshotRefreshMu sync.Mutex
	inspectionSnapshotRefresh   inspectionSnapshotRefreshState

	criticalConfirmMu       sync.Mutex
	criticalConfirmRounds   map[string]int
	lastAutomaticCreateAtMS int64
	automaticGuardMu        sync.Mutex
	automaticEnabled        bool
	automaticBaselineAtMS   int64
	automaticAccountAtMS    int64
	accountListCacheMu      sync.Mutex
	accountListCache        supplyAccountListCache
	supplyOrdersCacheMu     sync.Mutex
	supplyOrdersCache       supplyOrdersCache
	statusCache             supplyStatusCache
	automaticWake           chan struct{}
	purchaseTaskWake        chan struct{}
	challengeClient         *http.Client
	nvtokensRefreshMu       sync.Mutex
	nvtokensRefreshState    map[string]*nvtokensRefreshState
	supplierQuotaScoreMu    sync.Mutex
	supplierQuotaScores     map[string]supplierQuotaScoreCacheEntry
}

type supplyAccountListCache struct {
	key       string
	generated time.Time
	result    SupplyAccountList
}

type supplyOrdersCache struct {
	generated time.Time
	orders    []store.SupplyOrder
}

type supplyStatusCacheKey struct {
	limit      int
	generation uint64
}

type supplyStatusCacheEntry struct {
	generated  time.Time
	generation uint64
	payload    []byte
}

type supplyStatusRefresh struct {
	done    chan struct{}
	payload []byte
	err     error
}

// supplyStatusCache coalesces the expensive supply-dashboard read model. The
// cached representation is JSON rather than a shared Status value so every
// caller receives its own slices, maps and pointers and cannot race with the
// next response encoder or another request.
type supplyStatusCache struct {
	mu         sync.Mutex
	generation uint64
	entries    map[int]supplyStatusCacheEntry
	refreshes  map[supplyStatusCacheKey]*supplyStatusRefresh
	retryAfter map[supplyStatusCacheKey]time.Time
}

type operatorHeaderSnapshotCache struct {
	generated time.Time
	limit     int
	items     []store.HeaderSnapshot
}

type operatorAccountPoolCache struct {
	generation uint64
	key        string
	generated  time.Time
	stats      accountPoolStats
	err        error
}

const (
	supplyAccountListCacheTTL  = 15 * time.Second
	supplyOrdersCacheTTL       = 5 * time.Second
	supplyStatusCacheTTL       = 10 * time.Second
	supplyStatusStaleTTL       = 30 * time.Minute
	supplyStatusRetryCooldown  = 15 * time.Second
	supplyStatusRefreshTimeout = 8 * time.Second
	smartStatusRefreshTimeout  = 15 * time.Second
	supplyOverviewQuoteTTL     = 10 * time.Second
	operatorHeaderCacheTTL     = 60 * time.Second
	operatorAccountPoolTTL     = 30 * time.Second
	operatorAccountPoolTimeout = 12 * time.Second
)

const (
	staleInspectionSnapshotRefreshCooldown  = 30 * time.Second
	quotaEstimateCalibrationRefreshCooldown = 2 * time.Minute
	staleInspectionSnapshotRefreshTimeout   = 15 * time.Minute
	inspectionSnapshotRefreshFailureBackoff = 15 * time.Minute
	smartInspectionSnapshotFreshTTL         = 20 * time.Minute
)

type inspectionSnapshotRefreshState struct {
	appCtx      context.Context
	refresh     func(context.Context) error
	running     bool
	lastAttempt time.Time
	retryAfter  time.Time
}

func New(st *store.Store, managerConfig *managerconfigsvc.Service, httpClient ...*http.Client) *Service {
	var client *http.Client
	if len(httpClient) > 0 {
		client = httpClient[0]
	}
	service := &Service{
		store:                 st,
		managerConfig:         managerConfig,
		supplyClient:          supplyclient.New(client),
		challengeClient:       client,
		authFiles:             cpaauthfiles.New(client),
		smartBuckets:          make(map[int64]*smartUsageBucket),
		smartQuotaState:       newSmartQuotaCalibrationState(),
		smartLiveQuota:        make(map[string]smartLiveQuotaObservation),
		criticalConfirmRounds: make(map[string]int),
		automaticWake:         make(chan struct{}, 1),
		purchaseTaskWake:      make(chan struct{}, 1),
		nvtokensRefreshState:  make(map[string]*nvtokensRefreshState),
		supplierQuotaScores:   make(map[string]supplierQuotaScoreCacheEntry),
	}
	service.supplyClient.SetNvtokensSessionRefresher(service.refreshNvtokensSession)
	return service
}

// signalAutomaticWorker coalesces account-scoped quota changes. A quota header
// can arrive between regular replenishment ticks; waking the worker lets the
// capacity decision react immediately without shortening every idle poll or
// launching a full credential inspection.
func (s *Service) signalAutomaticWorker() {
	if s == nil || s.automaticWake == nil {
		return
	}
	select {
	case s.automaticWake <- struct{}{}:
	default:
	}
}

func (s *Service) AutomaticWake() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.automaticWake
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
	if smartInspectionSnapshotRefreshNeeded(resource) {
		s.requestStaleInspectionSnapshotRefresh()
	}
	resource = s.withInspectionSnapshotRefreshState(resource)
	s.setSmartResource(resource)
	return resource
}

// smartInspectionSnapshotRefreshNeeded separates a stale snapshot from a
// recent-but-partial snapshot. A partial inspection is still intentionally
// excluded from normal automatic purchasing, but immediately repeating the
// same full-pool scan cannot make credentials that consistently omit quota or
// usability evidence complete. Let the regular inspection cadence (or the
// normal 20-minute freshness expiry) retry it instead of keeping the Manager
// in a permanent refresh loop.
func smartInspectionSnapshotRefreshNeeded(resource SmartResource) bool {
	if !resource.Enabled || resource.SnapshotFresh {
		return false
	}
	reason := strings.TrimSuffix(resource.DecisionReason, "_capacity_deficit")
	partialEvidence := resource.SnapshotEvidencePartial ||
		reason == "inspection_quota_incomplete" || reason == "inspection_usability_incomplete"
	if partialEvidence {
		return resource.CapacitySnapshotAtMS <= 0 ||
			resource.CapacitySnapshotAgeSeconds > int(smartInspectionSnapshotFreshTTL/time.Second)
	}
	return true
}

func (s *Service) requestStaleInspectionSnapshotRefresh() {
	s.requestInspectionSnapshotRefresh(staleInspectionSnapshotRefreshCooldown)
}

func (s *Service) requestQuotaEstimateCalibrationRefresh() {
	s.requestInspectionSnapshotRefresh(quotaEstimateCalibrationRefreshCooldown)
}

func (s *Service) requestInspectionSnapshotRefresh(cooldown time.Duration) {
	if s == nil {
		return
	}
	if cooldown <= 0 {
		cooldown = staleInspectionSnapshotRefreshCooldown
	}
	now := time.Now()
	s.inspectionSnapshotRefreshMu.Lock()
	state := &s.inspectionSnapshotRefresh
	if state.refresh == nil || state.running || now.Before(state.retryAfter) ||
		(!state.lastAttempt.IsZero() && now.Sub(state.lastAttempt) < cooldown) {
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

		finishedAt := time.Now()
		s.inspectionSnapshotRefreshMu.Lock()
		s.inspectionSnapshotRefresh.running = false
		s.inspectionSnapshotRefresh.lastAttempt = finishedAt
		if err != nil {
			s.inspectionSnapshotRefresh.retryAfter = finishedAt.Add(inspectionSnapshotRefreshFailureBackoff)
		} else {
			s.inspectionSnapshotRefresh.retryAfter = time.Time{}
		}
		s.inspectionSnapshotRefreshMu.Unlock()
		// A completed inspection is durable in the store. Publish the completed
		// runtime state before dropping read caches so the next status rebuild
		// cannot preserve a short-lived "refresh in progress" snapshot.
		if err == nil {
			s.invalidateInspectionQuotaSnapshot()
			s.invalidateStatusCache()
		} else {
			log.Printf("[supply] inspection snapshot refresh failed; retry after %s: %v",
				finishedAt.Add(inspectionSnapshotRefreshFailureBackoff).Format(time.RFC3339), err)
		}
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
	if s == nil {
		return Status{}, ErrNotConfigured
	}
	if limit <= 0 {
		limit = 50
	}
	return s.statusCache.get(ctx, limit, s.buildStatus)
}

// GetDashboardStatus keeps the operator console responsive while a refreshed
// status snapshot is waiting on CPA or SQLite. Mutating service methods still
// call GetStatus and synchronously observe their post-write state.
func (s *Service) GetDashboardStatus(ctx context.Context, limit int) (Status, error) {
	if s == nil {
		return Status{}, ErrNotConfigured
	}
	if limit <= 0 {
		limit = 50
	}
	return s.statusCache.getStaleWhileRefresh(ctx, limit, s.buildStatus)
}

func (c *supplyStatusCache) get(
	ctx context.Context,
	limit int,
	build func(context.Context, int) (Status, error),
) (Status, error) {
	return c.getWithMode(ctx, limit, build, false)
}

func (c *supplyStatusCache) getStaleWhileRefresh(
	ctx context.Context,
	limit int,
	build func(context.Context, int) (Status, error),
) (Status, error) {
	return c.getWithMode(ctx, limit, build, true)
}

func (c *supplyStatusCache) getWithMode(
	ctx context.Context,
	limit int,
	build func(context.Context, int) (Status, error),
	staleWhileRefresh bool,
) (Status, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now()
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[int]supplyStatusCacheEntry)
	}
	if c.refreshes == nil {
		c.refreshes = make(map[supplyStatusCacheKey]*supplyStatusRefresh)
	}
	if c.retryAfter == nil {
		c.retryAfter = make(map[supplyStatusCacheKey]time.Time)
	}
	generation := c.generation
	key := supplyStatusCacheKey{limit: limit, generation: generation}
	entry, cached := c.entries[limit]
	if cached && entry.generation == generation && now.Sub(entry.generated) <= supplyStatusCacheTTL {
		payload := append([]byte(nil), entry.payload...)
		c.mu.Unlock()
		return decodeSupplyStatus(payload)
	}
	stalePayload, hasStale := staleSupplyStatusPayload(entry, cached, now)
	if retryAt := c.retryAfter[key]; hasStale && now.Before(retryAt) {
		c.mu.Unlock()
		return decodeSupplyStatus(stalePayload)
	}
	if staleWhileRefresh && hasStale {
		// Invalidation advances the generation. Do not start one expensive build
		// per mutation while an older generation for the same response limit is
		// still refreshing; the next poll will catch up after it finishes.
		for activeKey := range c.refreshes {
			if activeKey.limit == limit {
				c.mu.Unlock()
				return decodeSupplyStatus(stalePayload)
			}
		}
	}
	if refresh := c.refreshes[key]; refresh != nil {
		c.mu.Unlock()
		if staleWhileRefresh && hasStale {
			return decodeSupplyStatus(stalePayload)
		}
		select {
		case <-ctx.Done():
			return Status{}, ctx.Err()
		case <-refresh.done:
		}
		if refresh.err != nil {
			if hasStale && transientSupplyStatusError(refresh.err) {
				return decodeSupplyStatus(stalePayload)
			}
			return Status{}, refresh.err
		}
		return decodeSupplyStatus(refresh.payload)
	}
	refresh := &supplyStatusRefresh{done: make(chan struct{})}
	c.refreshes[key] = refresh
	c.mu.Unlock()
	if staleWhileRefresh && hasStale {
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), supplyStatusRefreshTimeout)
		go func() {
			defer cancel()
			_, refreshErr := c.executeRefresh(refreshCtx, limit, generation, key, refresh, true, build)
			if refreshErr != nil {
				log.Printf("[supply] background status refresh failed: %v", refreshErr)
			}
		}()
		return decodeSupplyStatus(stalePayload)
	}

	status, buildErr := c.executeRefresh(ctx, limit, generation, key, refresh, hasStale, build)
	if buildErr != nil {
		if hasStale && transientSupplyStatusError(buildErr) {
			log.Printf("[supply] serving last good status after transient refresh failure: %v", buildErr)
			return decodeSupplyStatus(stalePayload)
		}
		return Status{}, buildErr
	}
	return status, nil
}

func (c *supplyStatusCache) executeRefresh(
	ctx context.Context,
	limit int,
	generation uint64,
	key supplyStatusCacheKey,
	refresh *supplyStatusRefresh,
	hasStale bool,
	build func(context.Context, int) (Status, error),
) (Status, error) {
	buildCtx := ctx
	cancel := func() {}
	if hasStale {
		buildCtx, cancel = context.WithTimeout(ctx, supplyStatusRefreshTimeout)
	}
	status, buildErr := build(buildCtx, limit)
	cancel()
	var payload []byte
	if buildErr == nil {
		payload, buildErr = json.Marshal(status)
		if buildErr != nil {
			buildErr = fmt.Errorf("encode supply status cache: %w", buildErr)
		}
	}

	c.mu.Lock()
	if buildErr == nil {
		generated := time.Now()
		_, exists := c.entries[limit]
		if c.generation == generation || !exists {
			c.entries[limit] = supplyStatusCacheEntry{
				generated:  generated,
				generation: generation,
				payload:    append([]byte(nil), payload...),
			}
		}
		delete(c.retryAfter, key)
	} else if c.generation == generation && hasStale && transientSupplyStatusError(buildErr) {
		c.retryAfter[key] = time.Now().Add(supplyStatusRetryCooldown)
	}
	refresh.payload = append([]byte(nil), payload...)
	refresh.err = buildErr
	delete(c.refreshes, key)
	close(refresh.done)
	c.mu.Unlock()
	return status, buildErr
}

func staleSupplyStatusPayload(entry supplyStatusCacheEntry, cached bool, now time.Time) ([]byte, bool) {
	if !cached || entry.generated.IsZero() || now.Sub(entry.generated) > supplyStatusStaleTTL || len(entry.payload) == 0 {
		return nil, false
	}
	return append([]byte(nil), entry.payload...), true
}

func decodeSupplyStatus(payload []byte) (Status, error) {
	var status Status
	if err := json.Unmarshal(payload, &status); err != nil {
		return Status{}, fmt.Errorf("decode supply status cache: %w", err)
	}
	return status, nil
}

func transientSupplyStatusError(err error) bool {
	return sqliterepo.IsBusyError(err) || errors.Is(err, context.DeadlineExceeded)
}

func (c *supplyStatusCache) invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.generation++
	clear(c.retryAfter)
	c.mu.Unlock()
}

func (s *Service) invalidateStatusCache() {
	if s == nil {
		return
	}
	s.statusCache.invalidate()
}

func (s *Service) buildStatus(ctx context.Context, limit int) (Status, error) {
	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("build supply status config: %w", err)
	}
	resource := s.currentSmartResource(cfg.Supply)
	if resource.Enabled && s.newerCompletedSmartInspectionAvailable(ctx, resource.CapacitySnapshotRunID) {
		s.invalidateInspectionQuotaSnapshot()
	}
	if resource.Enabled {
		// Automatic replenishment may be disabled while operators still need a
		// current capacity view. Rebuild from the cached inspection snapshot on
		// every status poll so the usage window, account lease expiry and demand
		// rate naturally move forward even when no new request arrives.
		// A large production usage database may need several seconds to aggregate
		// the per-account quota windows behind a completed inspection. Five
		// seconds was below the measured healthy-query time once the event table
		// grew, which discarded an otherwise valid snapshot as unavailable and
		// started another full inspection. Keep the read bounded, but allow the
		// indexed baseline query to finish once and populate the short status
		// cache; subsequent dashboard polls remain fast.
		refreshCtx, cancel := context.WithTimeout(ctx, smartStatusRefreshTimeout)
		refreshed, refreshErr := s.smartResource(refreshCtx, cfg, false)
		cancel()
		if refreshErr == nil {
			preserveSmartResourceRuntimeState(&refreshed, resource)
			resource = refreshed
		} else {
			resource = s.currentSmartResource(cfg.Supply)
		}
	}
	// Capacity and credential counts have different consistency requirements.
	// The latest completed inspection remains the source of truth for quota and
	// health, while the live CPA auth-file list is the source of truth for how
	// many Codex credentials currently exist and are enabled. Reuse the short
	// auth-file cache so the ten-second dashboard poll does not scan the whole
	// pool on every request.
	overviewAvailable := -1
	var accountPool *AccountPoolSummary
	if cpaManagementConfigured(cfg) {
		poolStats, poolStatsErr := s.countAccountPoolStatsWithInspection(ctx, cfg, resource)
		if poolStatsErr == nil || poolStats.liveObserved {
			inspectedEnabled := resource.EnabledAccounts
			applyAccountPoolStats(&resource, poolStats)
			capacityChanged := s.reconcileSmartNormalCapacityFloor(cfg.Supply, &resource, poolStats, time.Now())
			if reconcileSmartCapacityWithAccountPool(&resource, inspectedEnabled) {
				capacityChanged = true
			}
			if capacityChanged && resource.ConsumeRCUPerMinute > 0 {
				recalculateSmartResourceCapacityPlan(cfg.Supply, &resource)
			}
			s.reconcileSmartAccountPoolGuard(cfg.Supply, &resource)
			applySmartAccountQuantityEstimate(cfg.Supply, &resource)
			overviewAvailable = poolStats.operatorAvailable(resource.SchedulableAccounts)
			summary := accountPoolSummaryFromStats(poolStats, time.Now())
			summary.Plans = accountPoolPlanSummaries(poolStats, cfg.Supply, resource.quotaSupplierByFile)
			accountPool = &summary
		}
	}
	retryPlan, err := s.applySmartEmergencyRetryPlan(ctx, cfg.Supply, &resource)
	if err != nil {
		return Status{}, fmt.Errorf("build supply status emergency retry: %w", err)
	}
	if retryPlan.active {
		cooldownActive, cooldownErr := s.automaticCreateCooldownActive(ctx, cfg.Supply, resource)
		if cooldownErr != nil {
			return Status{}, fmt.Errorf("build supply status create cooldown: %w", cooldownErr)
		}
		if cooldownActive {
			resource.SuggestedAction = smartActionObserveDemand
			resource.DecisionReason = "emergency_retry_cooldown"
		}
	}
	// smartResource() publishes the inspection-derived state before the live CPA
	// account list is reconciled above. Persist the final reconciled result too;
	// otherwise dashboard polling can leave the worker scheduled from stale
	// inspection account counts even while the page correctly shows a critical
	// live pool.
	s.setSmartResource(resource)
	// Overview used to live only in process memory. Recreating the Manager
	// therefore made inventory and balance look empty until a later automation
	// branch happened to refresh them; an open order can keep that branch from
	// running for a long time. Hydrate the read-only supplier snapshot on cold
	// start and refresh it after a short TTL so an order reservation never turns
	// the dashboard's current price into a historical quote.
	s.hydrateOverviewIfNeeded(ctx, cfg.Supply)
	orders, err := s.store.ListSupplyOrders(ctx, limit)
	if err != nil {
		return Status{}, fmt.Errorf("build supply status orders: %w", err)
	}
	purchaseTasks, err := s.listPurchaseTasks(ctx, limit)
	if err != nil {
		return Status{}, fmt.Errorf("build supply status purchase tasks: %w", err)
	}
	activeOrders, err := s.store.ListOpenSupplyOrders(ctx, maxTrackedOpenSupplyOrders)
	if err != nil {
		return Status{}, fmt.Errorf("build supply status open orders: %w", err)
	}
	if len(activeOrders) > 0 {
		resource.PrelockedCapacityRCU = totalSupplyOrderCapacityRCU(cfg.Supply, resource, activeOrders)
		resource.LockedOrderID = activeOrders[0].OrderID
	} else {
		resource.PrelockedCapacityRCU = 0
		resource.LockedOrderID = ""
		resource.LockedOrderAgeSeconds = 0
		resource.LockedConfirmRounds = 0
	}
	applySmartRefillProjection(cfg.Supply, &resource)
	s.setSmartResource(resource)
	s.stateMu.RLock()
	overview := s.overview
	running := s.running
	s.stateMu.RUnlock()
	if overview.Inventory != nil {
		pressure := smartSupplyPressureFromOrders(cfg.Supply, *overview.Inventory, max(1, resource.SuggestedQuantity), orders)
		applySmartSupplyPressure(&resource, pressure)
		purchaseTiming := smartJustInTimePurchase(cfg.Supply, resource, pressure, resource.SuggestedQuantity)
		applySmartPurchaseTiming(&resource, purchaseTiming)
		if len(activeOrders) == 0 && !smartResourceEmergency(resource) &&
			purchaseTiming.eligibleQuantity <= 0 &&
			(purchaseTiming.waitMinutes > 0 || purchaseTiming.lifetimeLimited) {
			// Keep the read model aligned with automatic execution. Operators should
			// see that this cycle is intentionally waiting rather than a stale
			// positive suggestion that the worker has already rejected.
			resource.SuggestedAction = smartActionObserveDemand
			resource.SuggestedQuantity = 0
			resource.DecisionReason = "purchase_timing_wait"
			if purchaseTiming.lifetimeLimited {
				resource.DecisionReason = "supply_lifetime_capacity_wait"
			}
			applySmartRefillProjection(cfg.Supply, &resource)
		}
		s.setSmartResource(resource)
	}
	if overviewAvailable >= 0 {
		overview.CPAAvailable = overviewAvailable
	}
	overview.CPATarget = cfg.Supply.TargetAvailableAccounts
	if overview.CPATarget > overview.CPAAvailable {
		overview.CPADeficit = overview.CPATarget - overview.CPAAvailable
	} else {
		overview.CPADeficit = 0
	}
	if applyOrdinaryAccountTargetGate(cfg.Supply, &resource, overview.CPAAvailable) {
		s.setSmartResource(resource)
	}
	applySmartTokenMetrics(&resource)
	status := Status{
		Config:          sanitizeConfig(cfg.Supply),
		Running:         running,
		Overview:        overview,
		AccountPool:     accountPool,
		SmartResource:   resource,
		Automation:      s.currentAutomationExecution(managerconfigsvc.SupplyEnabled(cfg.Supply)),
		LowPriceReserve: s.currentLowPriceReserveExecution(cfg.Supply, purchaseTasks),
		Recovery:        s.currentRecoverySummary(ctx, cfg.Supply),
		PurchaseTasks:   purchaseTasks,
		SessionRefresh:  s.nvtokensRefreshStatuses(cfg.Supply),
		Orders:          orders,
	}
	if len(activeOrders) > 0 {
		status.ActiveOrder = &activeOrders[0]
		status.ActiveOrders = activeOrders
	}
	return status, nil
}

func (s *Service) GetAccountPoolSummary(ctx context.Context) (AccountPoolSummary, error) {
	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return AccountPoolSummary{}, err
	}
	stats, statsErr := s.countOperatorAccountPoolStatsForDashboard(ctx, cfg)
	if statsErr != nil && !stats.liveObserved {
		return AccountPoolSummary{}, statsErr
	}
	summary := accountPoolSummaryFromStats(stats, time.Now())
	summary.Plans = accountPoolPlanSummaries(stats, cfg.Supply, nil)
	summary.Credentials = accountPoolCredentialSummaries(stats)
	return summary, nil
}

func accountPoolSummaryFromStats(stats accountPoolStats, checkedAt time.Time) AccountPoolSummary {
	available := stats.schedulable
	if stats.classificationObserved {
		available = stats.operatorUsable
	}
	return AccountPoolSummary{
		CheckedAtMS: checkedAt.UnixMilli(),
		Total:       max(0, stats.enabled),
		// Availability requires current positive evidence. Quota-risk credentials
		// remain included when they are still schedulable, while unconfirmed and
		// actionable credentials stay out of the available list.
		Normal:                 max(0, available),
		NeedsAttention:         max(0, stats.needsAttention),
		QuotaRisk:              max(0, stats.quotaRisk),
		Disabled:               max(0, stats.total-stats.enabled),
		Unconfirmed:            max(0, stats.unconfirmed),
		ClassificationObserved: stats.classificationObserved,
	}
}

func accountPoolPlanSummaries(
	stats accountPoolStats,
	cfg store.ManagerSupplyConfig,
	supplierByFile map[string]string,
) []AccountPoolPlanSummary {
	if len(stats.files) == 0 {
		return nil
	}
	type planKey struct {
		supplierID string
		planType   string
	}
	platforms := supplyPlatforms(cfg)
	platformByID := make(map[string]store.ManagerSupplyPlatformConfig, len(platforms))
	for _, platform := range platforms {
		platformByID[normalizeSmartQuotaSupplierID(platform.ID)] = platform
	}
	counts := make(map[planKey]int)
	names := make(map[planKey]string)
	for index, file := range stats.files {
		if !isCodexAuthFile(file) || file.Disabled || !isAvailableCodexFile(file) ||
			index >= len(stats.operatorUsableByFile) || !stats.operatorUsableByFile[index] {
			continue
		}
		planType := resolveSupplyPlanType(file.Raw)
		if planType == "" {
			planType = "unknown"
		}
		planType = strings.ToLower(strings.TrimSpace(planType))
		marker := mapFromMap(file.Raw, "cpamp_import")
		supplierID := normalizeSmartQuotaSupplierID(supplierByFile[strings.TrimSpace(file.Name)])
		if supplierID == "" {
			supplierID = normalizeSmartQuotaSupplierID(firstNonEmptyString(
				stringFromMap(marker, "platform_id", "platformId", "supplier_id", "supplierId"),
				stringFromMap(file.Raw, "platform_id", "platformId", "supplier_id", "supplierId"),
			))
		}
		if supplierID == "" && len(platforms) == 1 {
			supplierID = normalizeSmartQuotaSupplierID(platforms[0].ID)
		}
		key := planKey{supplierID: supplierID, planType: planType}
		counts[key]++
		name := stringFromMap(marker, "platform_name", "platformName", "supplier_name", "supplierName")
		if platform, ok := platformByID[supplierID]; ok {
			name = firstNonEmptyString(platform.Name, platform.ID, name)
		}
		if strings.TrimSpace(name) != "" {
			names[key] = strings.TrimSpace(name)
		}
	}
	if len(counts) == 0 {
		return nil
	}
	items := make([]AccountPoolPlanSummary, 0, len(counts))
	for key, count := range counts {
		items = append(items, AccountPoolPlanSummary{
			Key:          smartQuotaPublicContextKey(key.supplierID, key.planType),
			SupplierID:   key.supplierID,
			SupplierName: names[key],
			PlanType:     key.planType,
			AccountCount: count,
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		leftSupplier := normalizeSmartQuotaSupplierID(items[i].SupplierID)
		rightSupplier := normalizeSmartQuotaSupplierID(items[j].SupplierID)
		if leftSupplier != rightSupplier {
			return leftSupplier < rightSupplier
		}
		planRank := func(planType string) int {
			switch strings.ToLower(strings.TrimSpace(planType)) {
			case "team":
				return 0
			case "plus":
				return 1
			case "free":
				return 2
			default:
				return 3
			}
		}
		leftRank := planRank(items[i].PlanType)
		rightRank := planRank(items[j].PlanType)
		return leftRank < rightRank || (leftRank == rightRank && items[i].PlanType < items[j].PlanType)
	})
	return items
}

func accountPoolCredentialSummaries(stats accountPoolStats) []AccountPoolCredentialSummary {
	if len(stats.files) == 0 {
		return nil
	}
	filesByName := make(map[string]int, len(stats.files))
	for _, file := range stats.files {
		if isCodexAuthFile(file) {
			filesByName[strings.TrimSpace(file.Name)]++
		}
	}
	items := make([]AccountPoolCredentialSummary, 0, stats.total)
	for _, file := range stats.files {
		if !isCodexAuthFile(file) {
			continue
		}
		bucket := "disabled"
		temporaryLimit := operatorAccountTemporaryLimit{}
		if !file.Disabled {
			credentialKey := operatorCredentialKey(file.Name, file.AuthIndex)
			fileKey := operatorFileCredentialKey(file.Name)
			classified, matched := stats.bucketByCredential[credentialKey]
			if !matched && filesByName[strings.TrimSpace(file.Name)] == 1 {
				classified, matched = stats.bucketByCredential[fileKey]
			}
			if !matched {
				classified = operatorAccountUnconfirmed
			}
			bucket = operatorAccountBucketName(classified)
			temporaryLimit = stats.temporaryLimitByCredential[credentialKey]
			if !temporaryLimit.observed && filesByName[strings.TrimSpace(file.Name)] == 1 {
				temporaryLimit = stats.temporaryLimitByCredential[fileKey]
			}
		}
		items = append(items, AccountPoolCredentialSummary{
			AuthFileName:              strings.TrimSpace(file.Name),
			RuntimeID:                 strings.TrimSpace(file.ID),
			Provider:                  strings.TrimSpace(file.Provider),
			AuthIndex:                 strings.TrimSpace(file.AuthIndex),
			AccountID:                 strings.TrimSpace(file.AccountID),
			AccountSnapshot:           strings.TrimSpace(file.AccountSnapshot),
			Bucket:                    bucket,
			Schedulable:               isAvailableCodexFile(file),
			TemporaryLimited:          temporaryLimit.observed,
			TemporaryLimitKind:        temporaryLimit.kind,
			TemporaryLimitCode:        temporaryLimit.code,
			TemporaryLimitRecoverAtMS: temporaryLimit.recoverAtMS,
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		leftName := strings.ToLower(items[i].AuthFileName)
		rightName := strings.ToLower(items[j].AuthFileName)
		if leftName != rightName {
			return leftName < rightName
		}
		return strings.ToLower(items[i].AuthIndex) < strings.ToLower(items[j].AuthIndex)
	})
	return items
}

func operatorAccountBucketName(bucket operatorAccountBucket) string {
	switch bucket {
	case operatorAccountNormal:
		return "normal"
	case operatorAccountNeedsAttention:
		return "needs_attention"
	case operatorAccountQuotaRisk:
		return "quota_risk"
	default:
		return "unconfirmed"
	}
}

// GetActiveOrderStatus refreshes only the currently open supplier order when
// its local poll deadline has elapsed. It is used by the operations page as a
// fast path while an order is waiting for inventory. The method never bypasses
// a supplier-provided retry-after deadline and never performs the potentially
// long take/import side effects from processOrder.
func (s *Service) GetActiveOrderStatus(ctx context.Context) (ActiveOrderStatus, error) {
	result := ActiveOrderStatus{CheckedAtMS: time.Now().UnixMilli()}
	if s == nil || s.store == nil {
		return result, nil
	}
	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return ActiveOrderStatus{}, err
	}
	orders, err := s.store.ListOpenSupplyOrders(ctx, maxTrackedOpenSupplyOrders)
	if err != nil {
		return ActiveOrderStatus{}, err
	}
	if len(orders) == 0 {
		return result, nil
	}
	order := selectSupplyOrderToProcess(orders, time.Now().UnixMilli())
	result.ActiveOrder = &order
	result.ActiveOrders = orders
	if !s.activeOrderStatusPollDue(order, time.Now().UnixMilli()) {
		return result, nil
	}

	// The worker and an operator-triggered status read share the same mutex.
	// A busy worker remains the source of truth; the fast endpoint reports that
	// fact instead of starting a second supplier request.
	if !s.runMu.TryLock() {
		result.PollInProgress = true
		return result, nil
	}
	defer s.runMu.Unlock()

	orders, err = s.store.ListOpenSupplyOrders(ctx, maxTrackedOpenSupplyOrders)
	if err != nil {
		return ActiveOrderStatus{}, err
	}
	if len(orders) == 0 {
		result.ActiveOrder = nil
		result.ActiveOrders = nil
		return result, nil
	}
	order = selectSupplyOrderToProcess(orders, time.Now().UnixMilli())
	result.ActiveOrder = &order
	result.ActiveOrders = orders
	if !s.activeOrderStatusPollDue(order, time.Now().UnixMilli()) {
		return result, nil
	}

	result.PollAttempted = true
	if err := s.refreshActiveOrderRemoteStatus(ctx, cfg, &order); err != nil {
		result.PollError = safeError(err)
	}
	latestOrders, latestErr := s.store.ListOpenSupplyOrders(ctx, maxTrackedOpenSupplyOrders)
	if latestErr != nil {
		return ActiveOrderStatus{}, latestErr
	}
	if len(latestOrders) > 0 {
		latest := selectSupplyOrderToProcess(latestOrders, time.Now().UnixMilli())
		result.ActiveOrder = &latest
		result.ActiveOrders = latestOrders
	} else {
		result.ActiveOrder = nil
		result.ActiveOrders = nil
	}
	result.CheckedAtMS = time.Now().UnixMilli()
	return result, nil
}

func (s *Service) activeOrderStatusPollDue(order store.SupplyOrder, nowMS int64) bool {
	if order.SupplierRetryUntilMS > nowMS {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(order.Status)) {
	case "creating", "create_uncertain", "importing", "partial", "recovery_importing", "recovery_partial":
		// These states are progressed by their dedicated local workflows. The
		// fast status endpoint must not duplicate create or import side effects.
		return false
	}
	deadline := supplyOrderPollDeadline(order)
	return deadline <= 0 || deadline <= nowMS
}

const maxActiveOrderPollInterval = 3 * time.Second

func supplyPollIntervalSeconds(cfg store.ManagerSupplyConfig) int {
	seconds := cfg.PollIntervalSeconds
	if seconds <= 0 {
		seconds = int(maxActiveOrderPollInterval / time.Second)
	}
	// A local poll interval larger than this value makes the management page
	// show a stale reservation even when the supplier has already prepared it.
	// Supplier retry-after remains authoritative and is handled separately.
	if seconds > int(maxActiveOrderPollInterval/time.Second) {
		seconds = int(maxActiveOrderPollInterval / time.Second)
	}
	return seconds
}

func isFastSupplyOrderStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "created", "waiting_inventory", "ready":
		return true
	default:
		return false
	}
}

func supplyOrderPollDeadline(order store.SupplyOrder) int64 {
	deadline := order.NextPollAtMS
	if deadline <= 0 {
		return 0
	}
	if !isFastSupplyOrderStatus(order.Status) || order.UpdatedAtMS <= 0 {
		return deadline
	}
	// Older orders may have been persisted with a 60-second local deadline.
	// Once the supplier retry-after window is over, use a three-second local
	// cadence from the last persisted remote observation instead of waiting for
	// that stale configuration value.
	fastDeadline := order.UpdatedAtMS + maxActiveOrderPollInterval.Milliseconds()
	if fastDeadline < deadline {
		return fastDeadline
	}
	return deadline
}

func (s *Service) refreshActiveOrderRemoteStatus(ctx context.Context, cfg store.ManagerConfig, order *store.SupplyOrder) error {
	if s == nil || order == nil {
		return nil
	}
	platform, err := resolveSupplyPlatform(cfg.Supply, order.SupplierID, order.Product)
	if err != nil {
		return err
	}
	credentials := marketplaceSellerCredentials(platform, marketplaceSellerSelectionFromOrder(*order))
	remote, err := s.supplyClient.GetOrder(ctx, credentials, order.OrderID, order.StatusURL)
	if err != nil {
		if isHTTPStatus(err, http.StatusConflict) ||
			(isHTTPStatus(err, http.StatusNotFound) && !nvtokensPaidOrderLookupUncertain(platform, *order)) {
			return s.cancelOrder(ctx, order, err)
		}
		return s.updateOrderError(ctx, order, err, cfg.Supply)
	}
	applyRemoteOrder(order, remote, cfg.Supply)
	if isTerminalRemoteStatus(remote.Status) && !isSuccessfulRemoteStatus(remote.Status) {
		order.Status = localOrderStatus(remote.Status)
		order.CompletedAtMS = time.Now().UnixMilli()
		return s.store.UpdateSupplyOrder(ctx, *order)
	}
	if isReadyForTake(remote.Status) {
		order.Status = "ready"
	} else if strings.TrimSpace(remote.Status) != "" {
		order.Status = "waiting_inventory"
	}
	return s.store.UpdateSupplyOrder(ctx, *order)
}

func (s *Service) newerCompletedSmartInspectionAvailable(ctx context.Context, currentRunID int64) bool {
	if s == nil || s.store == nil {
		return false
	}
	runs, err := s.store.ListCodexInspectionRuns(ctx, latestSmartInspectionSearchLimit)
	if err != nil {
		return false
	}
	for _, run := range runs {
		if run.ID <= currentRunID || run.Status != model.CodexInspectionStatusCompleted ||
			!run.Settings.HasTargetProvider(model.CodexInspectionTargetCodex) {
			continue
		}
		if run.ProbeSetCount > 0 || smartInspectionRunIsTrustedEmptySupplySnapshot(run) {
			return true
		}
	}
	return false
}

func (s *Service) UpdateConfig(ctx context.Context, config store.ManagerSupplyConfig) (Status, error) {
	current, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return Status{}, err
	}
	updated, err := s.managerConfig.UpdateSupply(ctx, config)
	if err != nil {
		return Status{}, err
	}
	s.invalidateAllMarketplaceSupplierQuotaScores()
	s.invalidateStatusCache()
	wasEnabled := managerconfigsvc.SupplyEnabled(current.Supply)
	isEnabled := managerconfigsvc.SupplyEnabled(updated)
	if isEnabled && !wasEnabled {
		s.armAutomaticBaseline()
		s.invalidateInspectionQuotaSnapshot()
		s.requestStaleInspectionSnapshotRefresh()
	} else if !isEnabled {
		s.observeAutomaticEnabled(false)
	}
	if resolved, _, _, resolveErr := s.managerConfig.ResolveManagerConfigWithSource(ctx); resolveErr == nil {
		s.resetNvtokensRefreshStates(resolved.Supply)
		if openOrders, listErr := s.store.ListOpenSupplyOrders(ctx, maxTrackedOpenSupplyOrders); listErr == nil {
			_, _ = s.reconcileUnavailableSupplyOrders(ctx, resolved.Supply, openOrders)
		}
	}
	// Platform identity, product, filters, and credentials all affect quotes.
	// Drop the previous runtime quote so the response is hydrated from the newly
	// saved platform graph rather than displaying a stale supplier/error pair.
	s.stateMu.Lock()
	s.overview.Inventory = nil
	s.overview.Balance = nil
	s.overview.Platforms = nil
	s.overview.SelectedPlatformID = ""
	s.overview.LastError = ""
	s.overview.CheckedAtMS = 0
	s.stateMu.Unlock()
	return s.GetStatus(ctx, 50)
}

func (s *Service) Check(ctx context.Context) (Status, error) {
	if err := s.run(ctx, false, 0, true); err != nil {
		s.recordError(err)
		return Status{}, err
	}
	s.invalidateStatusCache()
	return s.GetStatus(ctx, 50)
}

func (s *Service) Replenish(ctx context.Context, quantity int, supplierID ...string) (Status, error) {
	requestedSupplierID := ""
	if len(supplierID) > 0 {
		requestedSupplierID = strings.TrimSpace(supplierID[0])
	}
	return s.ReplenishProduct(ctx, quantity, requestedSupplierID, "")
}

func (s *Service) ReplenishProduct(ctx context.Context, quantity int, supplierID string, product string) (Status, error) {
	if quantity <= 0 || quantity > 10000 {
		return Status{}, ErrInvalidQuantity
	}
	if _, err := s.createManualPurchaseTask(ctx, quantity, strings.TrimSpace(supplierID), strings.TrimSpace(product)); err != nil {
		s.recordError(err)
		return Status{}, err
	}
	s.invalidateStatusCache()
	s.signalPurchaseTaskWorker()
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
	s.invalidateSupplyOrdersCache()
	return s.GetStatus(ctx, 50)
}

func (s *Service) RunAutomatic(ctx context.Context) error {
	s.beginAutomaticRunDecision()
	err := s.run(ctx, true, 0, false)
	if err == nil {
		if reconcileErr := s.reconcileAutomaticPurchaseTaskCancellation(ctx); reconcileErr != nil {
			err = reconcileErr
		}
	}
	if err == nil {
		// Execute one durable task step immediately so an urgent decision does not
		// wait for the next worker tick. Supplier creation still lives entirely in
		// RunPurchaseTasks and is equally used by manual and automatic requests.
		err = s.RunPurchaseTasks(ctx)
	}
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
	if !summary.Enabled || !recoverySupplyPlatformConfigured(cfg.Supply) {
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

// ScheduleRecoverySyncIfDue keeps the potentially long recovery scan outside
// the automatic replenishment critical path. The extra guard covers the short
// window before SyncRecoveries marks its public state as running, so rapid
// worker ticks still create at most one background scan.
func (s *Service) ScheduleRecoverySyncIfDue(ctx context.Context) bool {
	if s == nil {
		return false
	}
	s.recoveryAsyncMu.Lock()
	if s.recoveryAsyncRunning {
		s.recoveryAsyncMu.Unlock()
		return false
	}
	s.recoveryAsyncRunning = true
	run := s.recoverySyncIfDue
	s.recoveryAsyncMu.Unlock()

	go func() {
		startedAt := time.Now()
		defer func() {
			s.recoveryAsyncMu.Lock()
			s.recoveryAsyncRunning = false
			s.recoveryAsyncMu.Unlock()
		}()
		if ctx == nil {
			ctx = context.Background()
		}
		syncCtx, cancel := context.WithTimeout(ctx, recoverySyncBackgroundTimeout)
		defer cancel()
		if run == nil {
			run = s.SyncRecoveriesIfDue
		}
		_, err := run(syncCtx)
		duration := time.Since(startedAt)
		if err != nil {
			log.Printf("[supply] background recovery sync failed duration=%s: %v", duration.Round(time.Millisecond), err)
		} else if duration >= automaticRunSlowLogThreshold {
			log.Printf("[supply] background recovery sync completed duration=%s", duration.Round(time.Millisecond))
		}
	}()
	return true
}

// ScheduleWarrantyMetadataMigration separates legacy nvtokens supplier
// warranty timestamps from credential expiry without delaying replenishment.
// Failed runs remain retryable on the next worker tick.
func (s *Service) ScheduleWarrantyMetadataMigration(ctx context.Context) bool {
	if s == nil || s.store == nil || s.managerConfig == nil {
		return false
	}
	s.warrantyMigrationMu.Lock()
	if s.warrantyMigrationRunning || s.warrantyMigrationDone {
		s.warrantyMigrationMu.Unlock()
		return false
	}
	s.warrantyMigrationRunning = true
	s.warrantyMigrationMu.Unlock()

	go func(parent context.Context) {
		if parent == nil {
			parent = context.Background()
		}
		migrationCtx, cancel := context.WithTimeout(parent, 10*time.Minute)
		defer cancel()
		cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(migrationCtx)
		if err == nil {
			var items []store.SupplyImportItem
			items, err = s.store.ListSupplyImportItems(migrationCtx, 5000, "imported")
			if err == nil {
				err = s.migrateNvtokensWarrantyMetadata(migrationCtx, cfg, items)
			}
		}
		s.warrantyMigrationMu.Lock()
		s.warrantyMigrationRunning = false
		s.warrantyMigrationDone = err == nil
		s.warrantyMigrationMu.Unlock()
		if err != nil {
			log.Printf("[supply] nvtokens warranty metadata migration failed: %v", err)
		}
	}(ctx)
	return true
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
	s.invalidateStatusCache()
	defer func() {
		s.recoveryMu.Lock()
		s.recoveryState.Running = false
		s.recoveryMu.Unlock()
		s.invalidateStatusCache()
	}()

	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		s.recordRecoveryError(ctx, cfg.Supply, err)
		return RecoverySummary{}, err
	}
	if !recoverySyncEnabled(cfg.Supply) || !recoverySupplyPlatformConfigured(cfg.Supply) {
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
	remainingClaimable := summary.Claimable
	summary.Seen = result.Seen
	summary.Claimable = result.Claimable
	summary.Claimed = result.Claimed
	summary.Imported = result.Imported
	summary.Refunded = result.Refunded
	summary.Failed = result.Failed
	now := time.Now()
	summary.LastSyncAtMS = now.UnixMilli()
	summary.NextSyncAtMS = now.Add(recoveryNextSyncInterval(cfg.Supply, err, autoClaim && recoveryID == "", remainingClaimable)).UnixMilli()
	if err != nil {
		if sqliterepo.IsBusyError(err) {
			summary.LastResult = "scheduled"
			summary.LastError = ""
		} else {
			summary.LastResult = "failed"
			summary.LastError = safeError(err)
		}
	} else {
		summary.LastResult = "completed"
		summary.LastError = ""
	}
	s.recoveryMu.Lock()
	s.recoveryState = summary
	s.recoveryMu.Unlock()
	s.invalidateStatusCache()
	return summary, err
}

func (s *Service) ListRecoveries(ctx context.Context, limit int, status string) ([]store.SupplyRecovery, error) {
	if s == nil || s.store == nil {
		return nil, ErrNotConfigured
	}
	recoveries, err := s.store.ListSupplyRecoveries(ctx, limit, status)
	if err != nil || len(recoveries) == 0 {
		return recoveries, err
	}
	orderIDs := make([]string, 0, len(recoveries))
	for _, recovery := range recoveries {
		if orderID := strings.TrimSpace(recovery.ClaimOrderID); orderID != "" {
			orderIDs = append(orderIDs, orderID)
		}
	}
	items, err := s.store.ListSupplyImportItemsByOrderIDs(ctx, orderIDs)
	if err != nil {
		return nil, err
	}
	byOrder := make(map[string][]store.SupplyImportItem)
	for _, item := range items {
		if strings.TrimSpace(item.OrderID) == "" {
			continue
		}
		byOrder[item.OrderID] = append(byOrder[item.OrderID], item)
	}
	for index := range recoveries {
		ownership, ownershipErr := s.recoveryOwnership(ctx, recoveries[index])
		if ownershipErr != nil {
			return nil, ownershipErr
		}
		recoveries[index].Ownership = ownership
		for _, item := range byOrder[recoveries[index].ClaimOrderID] {
			recoveries[index].ImportItems = append(recoveries[index].ImportItems, store.SupplyRecoveryImportItem{
				AccountName:      item.AccountName,
				FileName:         item.FileName,
				ImportAction:     item.ImportAction,
				ReplacedFileName: item.ReplacedFileName,
				Status:           strings.ToLower(strings.TrimSpace(item.Status)),
				LastError:        item.LastError,
				AttemptCount:     item.AttemptCount,
				NextRetryAtMS:    item.NextRetryAtMS,
				ImportedAtMS:     item.ImportedAtMS,
				UpdatedAtMS:      item.UpdatedAtMS,
			})
			switch strings.ToLower(strings.TrimSpace(item.Status)) {
			case "imported":
				recoveries[index].ImportedFileNames = append(recoveries[index].ImportedFileNames, item.FileName)
				if item.ImportedAtMS > recoveries[index].LastImportedAtMS {
					recoveries[index].LastImportedAtMS = item.ImportedAtMS
				}
			case "failed":
				recoveries[index].ImportFailedCount++
				if item.NextRetryAtMS > 0 && (recoveries[index].ImportNextRetryAtMS == 0 || item.NextRetryAtMS < recoveries[index].ImportNextRetryAtMS) {
					recoveries[index].ImportNextRetryAtMS = item.NextRetryAtMS
				}
			default:
				recoveries[index].ImportPendingCount++
			}
		}
		applyRecoveryImportStage(&recoveries[index])
	}
	return recoveries, nil
}

func (s *Service) recoveryOwnership(ctx context.Context, recovery store.SupplyRecovery) (string, error) {
	if strings.TrimSpace(recovery.ClaimOrderID) != "" {
		return "local", nil
	}
	sourceOrderID := strings.TrimSpace(recovery.SourceOrderID)
	if sourceOrderID == "" {
		return "unknown", nil
	}
	_, found, err := s.store.GetSupplyOrder(ctx, sourceOrderID)
	if err != nil {
		return "", err
	}
	if found {
		return "local", nil
	}
	return "external", nil
}

func (s *Service) RetryRecoveryImport(ctx context.Context, recoveryID string) (store.SupplyRecovery, error) {
	if s == nil || s.store == nil || s.managerConfig == nil {
		return store.SupplyRecovery{}, ErrNotConfigured
	}
	recoveryID = strings.TrimSpace(recoveryID)
	recovery, found, err := s.store.GetSupplyRecovery(ctx, recoveryID)
	if err != nil {
		return store.SupplyRecovery{}, err
	}
	if !found {
		return store.SupplyRecovery{}, ErrOrderNotFound
	}
	if strings.TrimSpace(recovery.ClaimOrderID) == "" {
		return recovery, ErrRecoveryImportNotReady
	}
	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return recovery, err
	}
	if !cpaManagementConfigured(cfg) {
		return recovery, ErrNotConfigured
	}
	reset, err := s.store.ResetSupplyRecoveryImport(ctx, recoveryID, time.Now().UnixMilli())
	if err != nil {
		return recovery, err
	}
	if !reset {
		if recovery.ItemCount > 0 && recovery.ImportedCount >= recovery.ItemCount {
			return recovery, nil
		}
		return recovery, ErrRecoveryImportNotReady
	}
	recovery, found, err = s.store.GetSupplyRecovery(ctx, recoveryID)
	if err != nil || !found {
		return recovery, err
	}
	_, _, processErr := s.processRecoveryImport(ctx, cfg, recovery)
	latest, _, getErr := s.store.GetSupplyRecovery(ctx, recoveryID)
	if getErr != nil {
		return recovery, getErr
	}
	s.invalidateStatusCache()
	return latest, processErr
}

func applyRecoveryImportStage(recovery *store.SupplyRecovery) {
	if recovery == nil {
		return
	}
	status := strings.ToLower(strings.TrimSpace(recovery.Status))
	deliveryStatus := strings.ToLower(strings.TrimSpace(recovery.DeliveryStatus))
	switch {
	case status == "refunded" || deliveryStatus == "refunded":
		recovery.ImportStatus = "refunded"
		recovery.ImportMessage = "supplier refunded this recovery; no CPA import is expected"
	case recovery.ItemCount > 0 && recovery.ImportedCount >= recovery.ItemCount:
		recovery.ImportStatus = "imported"
		recovery.ImportMessage = "all replacement files are registered in the CPA account pool"
	case status == "imported":
		recovery.ImportStatus = "imported"
		recovery.ImportMessage = "replacement files are registered in the CPA account pool"
	case recovery.Ownership == "external":
		recovery.ImportStatus = "not_this_pool"
		recovery.ImportMessage = "the source pickup order belongs to another CPAM pool; this instance only mirrors supplier status and does not claim the one-time ticket"
	case recovery.Ownership == "unknown" && strings.TrimSpace(recovery.ClaimOrderID) == "":
		recovery.ImportStatus = "ownership_unknown"
		recovery.ImportMessage = "the supplier did not include enough source identity to safely assign this one-time ticket to the current CPAM pool"
	case strings.TrimSpace(recovery.ClaimOrderID) == "" && (status == "claimable" || status == "seen"):
		recovery.ImportStatus = "waiting_claim"
		recovery.ImportMessage = "waiting for automatic claim before a local import task can be created"
	case strings.TrimSpace(recovery.ClaimOrderID) == "" && status == "claiming":
		recovery.ImportStatus = "claiming"
		recovery.ImportMessage = "the supplier claim is running; the local import task will be created next"
	case strings.TrimSpace(recovery.ClaimOrderID) == "":
		recovery.ImportStatus = "claimed_without_local_payload"
		recovery.ImportMessage = "the supplier ticket was claimed, but this CPAM instance did not receive or persist the replacement payload"
	case recovery.ItemCount == 0 || len(recovery.ImportItems) == 0:
		recovery.ImportStatus = "claimed_waiting_task"
		recovery.ImportMessage = "the claim is stored, but no local import item is available yet"
	case recovery.ImportFailedCount > 0 && recovery.ImportNextRetryAtMS > 0:
		recovery.ImportStatus = "retry_scheduled"
		recovery.ImportMessage = "a CPA import attempt failed and automatic retry is scheduled"
	case recovery.ImportFailedCount > 0:
		recovery.ImportStatus = "failed"
		recovery.ImportMessage = "the latest CPA import attempt failed"
	case recovery.ImportedCount > 0:
		recovery.ImportStatus = "partial"
		recovery.ImportMessage = "some replacement files are imported and the remainder is still processing"
	case recovery.ImportPendingCount > 0:
		recovery.ImportStatus = "task_pending"
		recovery.ImportMessage = "the local import task is ready and waiting for execution"
	default:
		recovery.ImportStatus = "importing"
		recovery.ImportMessage = "the replacement is being written to the CPA account pool"
	}
}

func (s *Service) ListAccounts(ctx context.Context, req SupplyAccountsRequest) (SupplyAccountList, error) {
	if s == nil || s.store == nil {
		return SupplyAccountList{}, ErrNotConfigured
	}
	req = normalizeSupplyAccountsRequest(req)
	statusFilter := strings.ToLower(strings.TrimSpace(req.Status))
	var managerCfg store.ManagerConfig
	var managerCfgErr error
	managerCfgKnown := false
	revenueMultiplier := defaultSupplyRevenueMultiplier
	if s.managerConfig != nil {
		managerCfg, _, _, managerCfgErr = s.managerConfig.ResolveManagerConfigWithSource(ctx)
		if managerCfgErr == nil {
			managerCfgKnown = true
			revenueMultiplier = supplyRevenueMultiplier(managerCfg.Supply)
		}
	}
	cacheKey := supplyAccountListCacheKey(req, statusFilter, revenueMultiplier)
	s.accountListCacheMu.Lock()
	if cached, ok := s.accountListCache.get(cacheKey, time.Now()); ok {
		s.accountListCacheMu.Unlock()
		return cached, nil
	}
	// Hold the short-lived read lock for the complete build. Concurrent page
	// refreshes then collapse into one analytics query instead of multiplying
	// the expensive pricing-band aggregation under SQLite.
	defer s.accountListCacheMu.Unlock()
	items, err := s.store.ListSupplyImportItems(ctx, supplyAccountListLimit(req, statusFilter), supplyImportStatusFilter(statusFilter))
	if err != nil {
		return SupplyAccountList{}, err
	}
	authFiles := supplyUsageAuthFiles(items)
	var prices map[string]store.ModelPrice
	var usageByFile map[string]supplyAccountUsage
	var issuesByFile map[string]supplyAccountIssue
	var orders map[string]store.SupplyOrder
	var recoveriesByFile map[string]supplyAccountRecoveryStatus
	tasksCtx, cancelTasks := context.WithCancel(ctx)
	defer cancelTasks()
	var taskGroup sync.WaitGroup
	var taskError error
	var taskErrorMu sync.Mutex
	runTask := func(task func() error) {
		taskGroup.Add(1)
		go func() {
			defer taskGroup.Done()
			if taskErr := task(); taskErr != nil {
				taskErrorMu.Lock()
				if taskError == nil {
					taskError = taskErr
					cancelTasks()
				}
				taskErrorMu.Unlock()
			}
		}()
	}
	runTask(func() error {
		var loadErr error
		prices, loadErr = s.store.LoadModelPrices(tasksCtx)
		if loadErr != nil {
			return loadErr
		}
		usageByFile, loadErr = s.supplyAccountUsageByFile(tasksCtx, ReportRequest{FromMS: req.FromMS, ToMS: req.ToMS}, authFiles, prices, revenueMultiplier)
		return loadErr
	})
	runTask(func() error {
		var loadErr error
		issuesByFile, loadErr = s.supplyAccountIssuesByFile(tasksCtx, authFiles)
		return loadErr
	})
	runTask(func() error {
		var loadErr error
		orders, loadErr = s.supplyOrdersForItems(tasksCtx, nil, items)
		return loadErr
	})
	runTask(func() error {
		var loadErr error
		recoveriesByFile, loadErr = s.supplyRecoveriesByOriginalFile(tasksCtx)
		return loadErr
	})
	taskGroup.Wait()
	if taskError != nil {
		return SupplyAccountList{}, taskError
	}

	var cpaStatusErr string
	cpaFiles := map[string]cpaauthfiles.File{}
	cpaLookupKnown := false
	if s.managerConfig != nil {
		if managerCfgKnown && cpaManagementConfigured(managerCfg) {
			snapshot, snapshotErr := s.cachedAuthFiles(ctx, managerCfg, false)
			if snapshotErr != nil {
				cpaStatusErr = safeError(snapshotErr)
			}
			cpaLookupKnown = snapshotErr == nil || len(snapshot.files) > 0
			cpaFiles = supplyCPAFileMap(snapshot.files)
		} else if managerCfgErr != nil {
			cpaStatusErr = safeError(managerCfgErr)
		}
	}

	now := time.Now()
	result := SupplyAccountList{
		Range: ReportRange{
			FromMS:        req.FromMS,
			ToMS:          req.ToMS,
			GeneratedAtMS: now.UnixMilli(),
			Days:          max(1, int(math.Ceil(float64(req.ToMS-req.FromMS)/float64(24*time.Hour/time.Millisecond)))),
			Truncated:     len(items) >= supplyAccountListLimit(req, statusFilter),
		},
		Summary: SupplyAccountSummary{
			UsageRevenueCurrency: "USD",
			RevenueMultiplier:    revenueMultiplier,
			CPAStatusError:       cpaStatusErr,
		},
		Items: make([]SupplyAccountItem, 0, min(len(items), req.Limit)),
	}
	for _, item := range items {
		if item.SupersededAtMS > 0 {
			continue
		}
		fileName := strings.TrimSpace(item.FileName)
		file, found := cpaFiles[fileName]
		issue := issuesByFile[fileName]
		if effectiveFrom := max(item.EffectiveFromMS, item.ImportedAtMS); effectiveFrom > 0 && issue.Auth401AtMS < effectiveFrom {
			issue = supplyAccountIssue{}
		}
		account := supplyAccountItemFromStore(item, orders[item.OrderID], usageByFile[fileName], file, cpaLookupKnown, found, now, issue, recoveriesByFile[fileName])
		if !supplyAccountStatusMatches(statusFilter, account) {
			continue
		}
		result.Items = append(result.Items, account)
		supplyAccountSummaryAdd(&result.Summary, account, now)
		if len(result.Items) >= req.Limit {
			result.Range.Truncated = result.Range.Truncated || len(items) > len(result.Items)
			break
		}
	}
	result.Summary.UsageRevenue = reportRatioFloat(result.Summary.UsageRevenue, 1)
	result.Summary.AverageRevenuePerCall = reportRatioFloat(result.Summary.UsageRevenue, float64(result.Summary.UsageCalls))
	s.accountListCache.put(cacheKey, result, now)
	return result, nil
}

func supplyAccountListCacheKey(req SupplyAccountsRequest, status string, revenueMultiplier float64) string {
	return fmt.Sprintf("%d:%d:%d:%s:%.8f", req.FromMS, req.ToMS, req.Limit, strings.ToLower(strings.TrimSpace(status)), revenueMultiplier)
}

func (cache *supplyAccountListCache) get(key string, now time.Time) (SupplyAccountList, bool) {
	if cache == nil || cache.key != key || cache.generated.IsZero() || now.Sub(cache.generated) > supplyAccountListCacheTTL {
		return SupplyAccountList{}, false
	}
	return cloneSupplyAccountList(cache.result), true
}

func (cache *supplyAccountListCache) put(key string, result SupplyAccountList, generated time.Time) {
	if cache == nil {
		return
	}
	cache.key = key
	cache.generated = generated
	cache.result = cloneSupplyAccountList(result)
}

func cloneSupplyAccountList(result SupplyAccountList) SupplyAccountList {
	result.Items = append([]SupplyAccountItem(nil), result.Items...)
	return result
}

// ListAccountLeases is intentionally lightweight for the credential list. It
// avoids analytics, pricing and CPA status joins while exposing the durable
// import provenance needed to recover metadata after a credential replacement.
func (s *Service) ListAccountLeases(ctx context.Context) ([]SupplyAccountLeaseItem, error) {
	if s == nil || s.store == nil {
		return nil, ErrNotConfigured
	}
	items, err := s.store.ListSupplyImportItems(ctx, 5000, "imported")
	if err != nil {
		return nil, err
	}
	orders, err := s.supplyOrdersForItems(ctx, nil, items)
	if err != nil {
		return nil, err
	}
	recoveries, err := s.store.ListSupplyRecoveries(ctx, 1000, "")
	if err != nil {
		return nil, err
	}
	recoveryByClaimOrder := make(map[string]store.SupplyRecovery, len(recoveries))
	for _, recovery := range recoveries {
		orderID := strings.TrimSpace(recovery.ClaimOrderID)
		if orderID == "" {
			continue
		}
		current, found := recoveryByClaimOrder[orderID]
		if !found || recovery.UpdatedAtMS >= current.UpdatedAtMS {
			recoveryByClaimOrder[orderID] = recovery
		}
	}
	var supplyCfg store.ManagerSupplyConfig
	if s.managerConfig != nil {
		if managerCfg, _, _, resolveErr := s.managerConfig.ResolveManagerConfigWithSource(ctx); resolveErr == nil {
			supplyCfg = managerCfg.Supply
		}
	}
	latestByFile := make(map[string]store.SupplyImportItem, len(items))
	for _, item := range items {
		fileName := strings.TrimSpace(item.FileName)
		if fileName == "" || item.SupersededAtMS > 0 {
			continue
		}
		current, found := latestByFile[fileName]
		if !found || item.UpdatedAtMS > current.UpdatedAtMS {
			latestByFile[fileName] = item
		}
	}
	result := make([]SupplyAccountLeaseItem, 0, len(latestByFile))
	for fileName, item := range latestByFile {
		order := orders[item.OrderID]
		source := "unknown"
		if strings.TrimSpace(order.OrderID) != "" {
			source = reportOrderSource(order)
		} else if strings.HasPrefix(strings.TrimSpace(item.OrderID), "recovery-") {
			source = "recovery"
		}
		method := "unknown"
		if source == "manual" {
			method = "manual_supply"
		} else if source == "automatic" {
			method = "automatic_supply"
		} else if source == "recovery" {
			method = "reauth_replacement"
		}
		platformID := strings.TrimSpace(order.SupplierID)
		platformName := platformID
		if platform, resolveErr := resolveSupplyPlatform(supplyCfg, platformID, order.Product); resolveErr == nil {
			platformID = firstNonEmptyString(platform.ID, platformID)
			platformName = firstNonEmptyString(platform.Name, platform.ID, platformName)
		}
		recovery := recoveryByClaimOrder[item.OrderID]
		result = append(result, SupplyAccountLeaseItem{
			FileName:            fileName,
			OrderID:             item.OrderID,
			SupplierID:          platformID,
			PlatformName:        platformName,
			Product:             order.Product,
			Source:              source,
			ImportMethod:        method,
			ImportAction:        item.ImportAction,
			ReplacedFileName:    item.ReplacedFileName,
			RecoveryID:          recovery.RecoveryID,
			RecoveryStatus:      recovery.Status,
			ImportedAtMS:        item.ImportedAtMS,
			LeaseExpiresAtMS:    item.LeaseExpiresAtMS,
			WarrantyExpiresAtMS: item.WarrantyExpiresAtMS,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].FileName < result[j].FileName })
	return result, nil
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
	actionCandidates, err := s.store.ListAccountActionCandidatesBetween(ctx, req.FromMS, req.ToMS, req.Limit)
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
	revenueMultiplier := s.currentSupplyRevenueMultiplier(ctx)
	reconciliationItems := mergeSupplyImportItems(items, usageItems)
	accountUsage, err := s.supplyAccountUsageByFile(ctx, req, supplyUsageAuthFiles(reconciliationItems), prices, revenueMultiplier)
	if err != nil {
		return Report{}, err
	}
	orderLookup, err := s.supplyOrdersForItems(ctx, orders, reconciliationItems)
	if err != nil {
		return Report{}, err
	}
	report := buildSupplyReport(req, orders, recoveries, items, actionCandidates, time.Now())
	report.Executive.RevenueMultiplier = revenueMultiplier
	applyUsageRevenueToReport(&report, modelStats, usageTimeline, prices, revenueMultiplier)
	report.Executive.Auth401Rate = math.Min(1, reportRatio(float64(report.Executive.Auth401Events), float64(report.Executive.UsageCalls)))
	report.Reconciliation = buildReportReconciliation(req, orders, recoveries, reconciliationItems, orderLookup, accountUsage, supplyAccountIssuesByFileFromCandidates(actionCandidates), time.Now())
	report.Range.Truncated = len(orders) >= req.Limit || len(recoveries) >= req.Limit ||
		len(items) >= req.Limit || len(actionCandidates) >= req.Limit || len(usageItems) >= req.Limit*2
	return report, nil
}

func (s *Service) withRecoveryInterval(base time.Duration, cfg store.ManagerSupplyConfig) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if !recoverySyncEnabled(cfg) || !supplyCredentialsConfigured(cfg) {
		return base
	}
	s.recoveryAsyncMu.Lock()
	asyncRunning := s.recoveryAsyncRunning
	s.recoveryAsyncMu.Unlock()
	if asyncRunning {
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
	if orders, err := s.store.ListOpenSupplyOrders(ctx, maxTrackedOpenSupplyOrders); err == nil && len(orders) > 0 {
		if eligible, eligibilityErr := s.automaticParallelCreateEligible(ctx, cfg.Supply, orders); eligibilityErr == nil && eligible {
			// Build the full-size emergency competition immediately. Supplier retry-after
			// delays polling of an existing reservation, not creation of the next
			// bounded competing quantity.
			return s.withRecoveryInterval(time.Second, cfg.Supply)
		}
		order := selectSupplyOrderToProcess(orders, time.Now().UnixMilli())
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
		if deadline := supplyOrderPollDeadline(order); deadline > 0 {
			wait := time.Until(time.UnixMilli(deadline))
			if wait <= 0 {
				return s.withRecoveryInterval(time.Second, cfg.Supply)
			}
			if wait > time.Minute {
				return s.withRecoveryInterval(time.Minute, cfg.Supply)
			}
			return s.withRecoveryInterval(wait, cfg.Supply)
		}
		seconds := supplyPollIntervalSeconds(cfg.Supply)
		return s.withRecoveryInterval(time.Duration(seconds)*time.Second, cfg.Supply)
	}
	if !managerconfigsvc.SupplyEnabled(cfg.Supply) {
		return s.withRecoveryInterval(time.Minute, cfg.Supply)
	}
	resource := s.currentSmartResource(cfg.Supply)
	if plan, err := s.applySmartEmergencyRetryPlan(ctx, cfg.Supply, &resource); err == nil {
		s.setSmartResource(resource)
		if plan.active {
			latest, found, latestErr := s.store.GetLatestAutomaticSupplyOrder(ctx)
			if latestErr == nil && found {
				// A zero-delivery cancellation is a failed reservation, not a
				// successful replenishment. Wake the worker immediately so the
				// next ladder rung can compete for stock instead of waiting for
				// the normal create cooldown.
				if automaticImmediateRetryEligible(cfg.Supply, latest) {
					state, stateErr := s.automaticRetryLadderState(ctx, cfg.Supply, latest)
					if stateErr == nil {
						if state.nextQuantity > 0 {
							return s.withRecoveryInterval(time.Second, cfg.Supply)
						}
						retryAt := time.UnixMilli(smartSupplyOrderTerminalAtMS(latest)).Add(
							automaticRetryCycleCooldown(cfg.Supply, resource),
						)
						if wait := time.Until(retryAt); wait > 0 {
							return s.withRecoveryInterval(wait, cfg.Supply)
						}
						return s.withRecoveryInterval(time.Second, cfg.Supply)
					}
				}
				retryAt := time.UnixMilli(smartSupplyOrderTerminalAtMS(latest)).Add(plan.cooldown)
				if wait := time.Until(retryAt); wait > 0 {
					return s.withRecoveryInterval(wait, cfg.Supply)
				}
				return s.withRecoveryInterval(time.Second, cfg.Supply)
			}
		}
	}
	seconds := smartAutomaticCheckIntervalSeconds(cfg.Supply, resource)
	return s.withRecoveryInterval(time.Duration(seconds)*time.Second, cfg.Supply)
}

func maxConcurrentSupplyOrders(cfg store.ManagerSupplyConfig) int {
	if cfg.MaxConcurrentOrders <= 0 {
		return 3
	}
	return clampInt(cfg.MaxConcurrentOrders, 1, 3)
}

const maxTrackedOpenSupplyOrders = 16

func parallelSupplyTriggerReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "automatic"
	}
	if strings.HasPrefix(strings.ToLower(reason), "parallel_") {
		return reason
	}
	return "parallel_" + reason
}

func unwrappedSupplyTriggerReason(reason string) string {
	reason = strings.TrimSpace(reason)
	for strings.HasPrefix(strings.ToLower(reason), "parallel_") {
		reason = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(reason), "parallel_"))
	}
	return reason
}

func supplyOrderStatusPriority(status string) int {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "importing", "partial":
		return 0
	case "ready":
		return 1
	case "taking":
		return 2
	case "waiting_inventory", "created":
		return 3
	case "creating", "create_uncertain":
		return 4
	default:
		return 5
	}
}

func selectSupplyOrderToProcess(orders []store.SupplyOrder, nowMS int64) store.SupplyOrder {
	if len(orders) == 0 {
		return store.SupplyOrder{}
	}
	selected := orders[0]
	selectedDue := selected.SupplierRetryUntilMS <= nowMS && supplyOrderPollDeadline(selected) <= nowMS
	for _, candidate := range orders[1:] {
		candidateDue := candidate.SupplierRetryUntilMS <= nowMS && supplyOrderPollDeadline(candidate) <= nowMS
		if candidateDue && !selectedDue {
			selected, selectedDue = candidate, true
			continue
		}
		if candidateDue != selectedDue {
			continue
		}
		selectedRank := supplyOrderStatusPriority(selected.Status)
		candidateRank := supplyOrderStatusPriority(candidate.Status)
		selectedDeadline := supplyOrderPollDeadline(selected)
		candidateDeadline := supplyOrderPollDeadline(candidate)
		if candidateRank < selectedRank ||
			(candidateRank == selectedRank && (candidateDeadline < selectedDeadline ||
				(candidateDeadline == selectedDeadline && candidate.CreatedAtMS < selected.CreatedAtMS))) {
			selected = candidate
		}
	}
	return selected
}

func openOrdersAllowParallelCreate(orders []store.SupplyOrder) bool {
	return openOrdersAllowParallelCreateAt(orders, time.Now())
}

func openOrdersAllowParallelCreateAt(orders []store.SupplyOrder, now time.Time) bool {
	if len(orders) == 0 {
		return false
	}
	for _, order := range orders {
		if purchaseTaskOrderReservationStale(order, now) {
			continue
		}
		if isSupplyOrderCapacityCommitted(order) {
			// Stock has already been secured for this order. Stop expanding the
			// competition window and let the aggregate take budget settle it first.
			return false
		}
		switch strings.ToLower(strings.TrimSpace(order.Status)) {
		case "created", "waiting_inventory":
			// These orders are waiting for supplier inventory and are the only
			// states where another reservation can improve the抢货 success rate.
		default:
			return false
		}
	}
	return true
}

type parallelSupplyCompetition struct {
	anchor   store.SupplyOrder
	attempts int
}

func isParallelSupplyOrder(order store.SupplyOrder) bool {
	return order.Automatic && strings.HasPrefix(strings.ToLower(strings.TrimSpace(order.TriggerReason)), "parallel_")
}

// parallelSupplyCompetitionForOrders keeps a bounded full-size competition
// attached to the oldest non-parallel reservation that is still waiting for
// stock. Terminal parallel attempts remain part of the same bounded wave, so a
// cancelled competitor still consumes one of the two competition slots while
// the anchor reservation is active.
func parallelSupplyCompetitionForOrders(
	cfg store.ManagerSupplyConfig,
	openOrders []store.SupplyOrder,
	history []store.SupplyOrder,
) (parallelSupplyCompetition, bool) {
	var anchor store.SupplyOrder
	found := false
	for _, order := range openOrders {
		if !order.Automatic || isParallelSupplyOrder(order) || !isSupplierPurchaseHistoryOrder(order) ||
			!supplyProductConfigured(cfg, order.Product) {
			continue
		}
		if !found || order.CreatedAtMS < anchor.CreatedAtMS ||
			(order.CreatedAtMS == anchor.CreatedAtMS && order.ID < anchor.ID) {
			anchor = order
			found = true
		}
	}
	if !found {
		return parallelSupplyCompetition{}, false
	}
	competition := parallelSupplyCompetition{anchor: anchor}
	seen := make(map[string]struct{}, len(openOrders)+len(history))
	add := func(order store.SupplyOrder) {
		if !isParallelSupplyOrder(order) || !isSupplierPurchaseHistoryOrder(order) ||
			!supplyProductConfigured(cfg, order.Product) ||
			(anchor.CreatedAtMS > 0 && order.CreatedAtMS > 0 && order.CreatedAtMS < anchor.CreatedAtMS) {
			return
		}
		key := strings.TrimSpace(order.OrderID)
		if key == "" {
			key = fmt.Sprintf("id:%d", order.ID)
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		competition.attempts++
	}
	for _, order := range history {
		add(order)
	}
	for _, order := range openOrders {
		add(order)
	}
	return competition, true
}

func (s *Service) parallelSupplyCompetition(
	ctx context.Context,
	cfg store.ManagerSupplyConfig,
	openOrders []store.SupplyOrder,
) (parallelSupplyCompetition, bool, error) {
	history, err := s.recentSupplyOrders(ctx)
	if err != nil {
		return parallelSupplyCompetition{}, false, err
	}
	competition, found := parallelSupplyCompetitionForOrders(cfg, openOrders, history)
	return competition, found, nil
}

func (s *Service) automaticParallelCreateEligible(
	ctx context.Context,
	cfg store.ManagerSupplyConfig,
	orders []store.SupplyOrder,
) (bool, error) {
	admissionOrderCount := purchaseTaskAdmissionOrderCount(orders, time.Now())
	if len(orders) == 0 || admissionOrderCount >= maxConcurrentSupplyOrders(cfg) ||
		!openOrdersAllowParallelCreateAt(orders, time.Now()) || !smartSupplyEnabled(cfg) {
		return false, nil
	}
	resource := s.currentSmartResource(cfg)
	if smartProgressiveStartupFloorRecovery(resource) {
		return false, nil
	}
	if eligible, err := s.activePurchaseTaskParallelContinuationEligible(ctx, cfg, orders); err != nil {
		return false, err
	} else if eligible {
		// A durable task can outlive its original shortage snapshot. Re-enter the
		// planner while a waiting child still owns only one slot so a newly enlarged
		// emergency deficit can expand the task and use the remaining slots. The
		// fresh smart-resource pass later in run() remains the creation authority.
		return true, nil
	}
	if !smartResourceEmergency(resource) && !isSmartEmergencyRetryReason(resource.DecisionReason) {
		return false, nil
	}
	competition, found, err := s.parallelSupplyCompetition(ctx, cfg, orders)
	if err != nil {
		return false, err
	}
	if !found || competition.attempts >= max(0, maxConcurrentSupplyOrders(cfg)-1) {
		return false, nil
	}
	blocked, err := s.automaticParallelCreateBlocked(ctx, cfg)
	if err != nil {
		return false, err
	}
	return !blocked, nil
}

func (s *Service) activePurchaseTaskParallelContinuationEligible(
	ctx context.Context,
	cfg store.ManagerSupplyConfig,
	orders []store.SupplyOrder,
) (bool, error) {
	if s == nil || s.store == nil || maxConcurrentSupplyOrders(cfg) <= 1 ||
		len(orders) == 0 || purchaseTaskAdmissionOrderCount(orders, time.Now()) >= maxConcurrentSupplyOrders(cfg) {
		return false, nil
	}
	task, found, err := s.store.GetActiveAutomaticSupplyPurchaseTask(ctx)
	if err != nil || !found {
		return false, err
	}
	for _, order := range orders {
		if strings.TrimSpace(order.TaskID) == task.TaskID {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) run(ctx context.Context, allowCreate bool, manualQuantity int, force bool, manualSupplierID ...string) (err error) {
	timing := newAutomaticRunTiming()
	defer func() { timing.finish(err) }()
	s.runMu.Lock()
	defer s.runMu.Unlock()
	timing.next("config")
	s.setRunning(true)
	defer s.setRunning(false)
	var resource SmartResource
	useSmart := false
	defer func() {
		if useSmart && resource.GeneratedAtMS > 0 {
			s.recordAutomaticRunDecision(resource)
		}
	}()

	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return err
	}
	supplyCfg := cfg.Supply
	requestedSupplierID := ""
	if manualQuantity > 0 && len(manualSupplierID) > 0 {
		requestedSupplierID = strings.TrimSpace(manualSupplierID[0])
	}
	if requestedSupplierID != "" {
		if _, err := resolveSupplyPlatform(supplyCfg, requestedSupplierID, ""); err != nil {
			return err
		}
	}
	_, baselineStarted := s.observeAutomaticEnabled(managerconfigsvc.SupplyEnabled(supplyCfg))
	if baselineStarted && smartSupplyEnabled(supplyCfg) {
		s.invalidateInspectionQuotaSnapshot()
		s.requestStaleInspectionSnapshotRefresh()
	}
	timing.next("order-reconcile")
	if restored, restoredFound, err := s.store.ActivateNextUnsupportedSupplyRelease(ctx); err != nil {
		return err
	} else if restoredFound {
		return s.processOrder(ctx, cfg, restored)
	}
	openOrders, err := s.store.ListOpenSupplyOrders(ctx, maxTrackedOpenSupplyOrders)
	if err != nil {
		return err
	}
	parallelEligible := false
	var immediateRetryOrder *store.SupplyOrder
	manualParallel := manualQuantity > 0 && len(openOrders) > 0
	if manualParallel {
		if len(openOrders) >= maxConcurrentSupplyOrders(supplyCfg) {
			return ErrOrderInProgress
		}
	} else if len(openOrders) > 0 {
		active := selectSupplyOrderToProcess(openOrders, time.Now().UnixMilli())
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
		// ahead of this deadline either. It only delays polling this reservation;
		// another waiting-inventory reservation may still be created to compete
		// for stock when the configured parallel window has room.
		pollAllowed := active.SupplierRetryUntilMS <= nowMS
		if pollAllowed && !force && supplyOrderPollDeadline(active) > nowMS &&
			!s.emergencyOrderProcessingAllowed(cfg.Supply, active, s.currentSmartResource(cfg.Supply)) {
			pollAllowed = false
		}
		if pollAllowed {
			if err := s.processOrder(ctx, cfg, active); err != nil {
				return err
			}
			if !allowCreate {
				return nil
			}
			// Processing may turn waiting_inventory into ready, complete/release an
			// order, or change its supplier retry deadline. Re-read the open set so
			// creation decisions never use the stale pre-processing snapshot.
			openOrders, err = s.store.ListOpenSupplyOrders(ctx, maxTrackedOpenSupplyOrders)
			if err != nil {
				return err
			}
			if len(openOrders) == 0 {
				// A cancelled zero-delivery automatic reservation did not change
				// capacity. Re-enter the shortage calculation in this same worker
				// turn and submit the next, smaller ladder rung immediately. The
				// one-create-per-turn boundary remains in place, so this cannot spin
				// through multiple supplier reservations in one execution.
				processed, processedFound, processedErr := s.store.GetSupplyOrder(ctx, active.OrderID)
				if processedErr != nil {
					return processedErr
				}
				if processedFound && automaticImmediateRetryEligible(supplyCfg, processed) {
					immediateRetryOrder = &processed
				} else {
					// Keep one supplier lifecycle action per worker turn for all
					// successful or uncertain terminal transitions.
					return nil
				}
			}
		}
		if allowCreate && manualQuantity == 0 {
			parallelEligible, err = s.automaticParallelCreateEligible(ctx, supplyCfg, openOrders)
			if err != nil {
				return err
			}
		}
		if len(openOrders) > 0 && !parallelEligible && !lowPriceReserveOpenOrdersReversible(openOrders) {
			return nil
		}
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
	useSmart = manualQuantity == 0 && smartSupplyEnabled(supplyCfg)
	if useSmart {
		timing.next("capacity-snapshot")
		resource, err = s.smartResource(ctx, cfg, force)
		if err != nil {
			return err
		}
		if len(openOrders) > 0 {
			resource.PrelockedCapacityRCU = totalSupplyOrderCapacityRCU(supplyCfg, resource, openOrders)
			resource.LockedOrderID = openOrders[0].OrderID
			applySmartRefillProjection(supplyCfg, &resource)
		}
		timing.next("account-pool")
		if poolAvailable, emergencyQuantity, emergencyReason, accountLoaded, err := s.smartEmergencyAvailability(ctx, cfg, &resource); err != nil {
			return err
		} else {
			available = poolAvailable
			if accountLoaded {
				s.markAutomaticAccountSnapshot()
			}
			if emergencyQuantity > 0 {
				resource.SuggestedQuantity = max(resource.SuggestedQuantity, emergencyQuantity)
				resource.DecisionReason = emergencyReason
				resource.EmergencyReason = emergencyReason
			}
			applySmartAccountQuantityEstimate(supplyCfg, &resource)
			applySmartRefillProjection(supplyCfg, &resource)
			s.setSmartResource(resource)
		}
	} else {
		timing.next("account-pool")
		available, err = s.countAvailableAccounts(ctx, cfg)
		if err != nil {
			return err
		}
	}
	timing.next("decision")
	if manualQuantity == 0 {
		if useSmart {
			if reason := s.automaticSupplyGuardReason(resource); reason != "" {
				resource.EmergencyShortage = false
				resource.EmergencyReason = ""
				resource.SuggestedAction = smartActionSnapshotStale
				resource.SuggestedQuantity = 0
				resource.DecisionReason = reason
				s.setSmartResource(resource)
				s.updateCPAOverview(available, supplyCfg.TargetAvailableAccounts)
				return nil
			}
			if applyOrdinaryAccountTargetGate(supplyCfg, &resource, available) {
				if _, cancelErr := s.cancelSatisfiedOrdinaryAutomaticTask(ctx, cfg, resource, available); cancelErr != nil {
					return cancelErr
				}
				s.setSmartResource(resource)
				s.updateCPAOverview(available, supplyCfg.TargetAvailableAccounts)
				return nil
			}
		}
		if recent, recentFound, err := s.store.GetLatestCompletedAutomaticSupplyOrder(ctx); err != nil {
			return err
		} else if settlementPending := recentFound &&
			time.Since(time.UnixMilli(recent.CompletedAtMS)) < automaticSettleWindow(supplyCfg) &&
			!parallelEligible && (!useSmart || recentAutomaticOrderCoversCurrentShortage(supplyCfg, resource, recent)); settlementPending {
			if useSmart {
				resource.EmergencyShortage = false
				resource.SuggestedAction = smartActionObserveDemand
				resource.SuggestedQuantity = 0
				resource.DecisionReason = "automatic_settlement_pending"
				s.setSmartResource(resource)
			}
			s.updateCPAOverview(available, supplyCfg.TargetAvailableAccounts)
			return nil
		}
		if useSmart {
			retryPlan, err := s.applySmartEmergencyRetryPlan(ctx, supplyCfg, &resource)
			if err != nil {
				return err
			}
			immediateRetryQuantityLimit := 0
			retryLadderExhausted := false
			if immediateRetryOrder == nil && retryPlan.active {
				latest, found, latestErr := s.store.GetLatestAutomaticSupplyOrder(ctx)
				if latestErr != nil {
					return latestErr
				}
				if found && automaticImmediateRetryEligible(supplyCfg, latest) {
					immediateRetryOrder = &latest
				}
			}
			if immediateRetryOrder != nil {
				state, stateErr := s.automaticRetryLadderState(ctx, supplyCfg, *immediateRetryOrder)
				if stateErr != nil {
					return stateErr
				}
				if state.nextQuantity > 0 {
					immediateRetryQuantityLimit = state.nextQuantity
				} else {
					retryLadderExhausted = true
					immediateRetryOrder = nil
				}
			}
			if immediateRetryOrder == nil {
				cooldownActive, err := s.automaticCreateCooldownActive(ctx, supplyCfg, resource)
				if err != nil {
					return err
				}
				if cooldownActive && !parallelEligible {
					resource.SuggestedAction = smartActionObserveDemand
					if isSmartEmergencyRetryReason(resource.DecisionReason) {
						resource.DecisionReason = "emergency_retry_cooldown"
					} else {
						resource.EmergencyShortage = false
						resource.SuggestedQuantity = 0
						resource.DecisionReason = "automatic_create_cooldown"
					}
					applySmartRefillProjection(supplyCfg, &resource)
					s.setSmartResource(resource)
					s.updateCPAOverview(available, supplyCfg.TargetAvailableAccounts)
					return nil
				}
				if retryLadderExhausted {
					// The previous base/half/fifth cycle has completed. Once its
					// short cooldown expires, the next order starts a fresh cycle
					// from the current verified shortage rather than inheriting the
					// old retry reason and quantities.
					resource.DecisionReason = firstNonEmptyString(resource.EmergencyReason, "emergency_refill_to_healthy")
				}
			} else {
				resource.DecisionReason = "emergency_retry_immediate_after_cancelled"
			}
			if immediateRetryQuantityLimit > 0 {
				resource.SuggestedQuantity = min(resource.SuggestedQuantity, immediateRetryQuantityLimit)
			}
		}
	}
	quantity := manualQuantity
	if quantity == 0 {
		if useSmart {
			planningResource := supplyCreatePlanningResource(supplyCfg, resource, openOrders, parallelEligible)
			if smartResourceEmergency(resource) {
				quantity = s.smartSuggestedCreateQuantity(supplyCfg, planningResource)
			} else if !resource.SnapshotFresh && !smartPartialInspectionCapacityDeficitAllowed(resource) {
				return nil
			} else if resource.DecisionReason == "usage_rate_not_ready" || resource.ConsumeRCUPerMinute <= 0 {
				return s.refreshSupplyOverview(ctx, supplyCfg, available, max(1, supplyCfg.ReplenishBatchSize))
			} else if resource.CapacityGapRCU <= 0 {
				return s.refreshSupplyOverview(ctx, supplyCfg, available, max(1, supplyCfg.ReplenishBatchSize))
			} else if resource.SuggestedAction == smartActionHealthy || resource.HealthLevel == smartHealthHealthy {
				return s.refreshSupplyOverview(ctx, supplyCfg, available, max(1, supplyCfg.ReplenishBatchSize))
			} else {
				quantity = s.smartSuggestedCreateQuantity(supplyCfg, planningResource)
			}
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
	if manualQuantity == 0 {
		activeTask, found, taskErr := s.store.GetActiveAutomaticSupplyPurchaseTask(ctx)
		if taskErr != nil {
			return taskErr
		}
		if found && isLowPriceReserveTrigger(activeTask.TriggerReason) {
			if taskErr := s.cancelLowPriceReservePurchaseTask(ctx, &activeTask, "superseded by live replenishment demand"); taskErr != nil {
				return taskErr
			}
			openOrders, err = s.store.ListOpenSupplyOrders(ctx, maxTrackedOpenSupplyOrders)
			if err != nil {
				return err
			}
		}
	}
	if manualQuantity == 0 && immediateRetryOrder != nil {
		retryQuantity, retryErr := s.automaticImmediateRetryQuantity(ctx, supplyCfg, *immediateRetryOrder, quantity)
		if retryErr != nil {
			return retryErr
		}
		if retryQuantity <= 0 {
			resource.SuggestedAction = smartActionObserveDemand
			resource.SuggestedQuantity = 0
			resource.DecisionReason = "emergency_retry_cooldown"
			applySmartRefillProjection(supplyCfg, &resource)
			s.setSmartResource(resource)
			s.updateCPAOverview(available, supplyCfg.TargetAvailableAccounts)
			return nil
		}
		quantity = min(quantity, retryQuantity)
	}

	timing.next("supplier-quote")
	selection, err := s.selectSupplyPlatform(ctx, supplyCfg, quantity, openOrders, requestedSupplierID)
	if err != nil {
		if manualQuantity == 0 {
			if action, reason, ok := automaticSupplyWaitDecision(err); ok {
				resource.SuggestedAction = action
				resource.SuggestedQuantity = 0
				resource.DecisionReason = reason
				if useSmart {
					s.setSmartResource(resource)
				}
				overview := Overview{
					CheckedAtMS:  time.Now().UnixMilli(),
					CPAAvailable: available,
					CPATarget:    supplyCfg.TargetAvailableAccounts,
					CPADeficit:   max(0, supplyCfg.TargetAvailableAccounts-available),
					Platforms:    selection.all,
				}
				if selection.status.Inventory != nil {
					overview.Inventory = selection.status.Inventory
					overview.SelectedPlatformID = selection.platform.ID
				}
				s.setOverview(overview)
				s.updateCPAOverview(available, supplyCfg.TargetAvailableAccounts)
				return nil
			}
		}
		return err
	}
	platform := selection.platform
	inventory := *selection.status.Inventory
	balance := *selection.status.Balance
	if useSmart {
		pressure := s.smartSupplyPressure(ctx, supplyCfg, inventory, quantity)
		applySmartSupplyPressure(&resource, pressure)
		adjustedQuantity, pressureReason, timing := smartPrelockQuantityForSupplyPressureWithTiming(supplyCfg, resource, pressure, quantity)
		applySmartPurchaseTiming(&resource, timing)
		if manualQuantity == 0 && immediateRetryOrder != nil {
			// Retry ladder quantities are a final upper bound. In particular,
			// account-vacuum pressure must not lift a 10/5/2 retry back to the
			// emergency minimum and create a row of identical reservations.
			retryQuantity, retryErr := s.automaticImmediateRetryQuantity(ctx, supplyCfg, *immediateRetryOrder, quantity)
			if retryErr != nil {
				return retryErr
			}
			if retryQuantity <= 0 {
				adjustedQuantity = 0
			} else if adjustedQuantity <= 0 || adjustedQuantity > retryQuantity {
				adjustedQuantity = retryQuantity
			}
		}
		if adjustedQuantity <= 0 {
			// A zero adjusted quantity is an explicit just-in-time admission
			// decision, not a missing override. Preserve it through the execution
			// layer so purchase_timing_wait never falls back to the original smart
			// suggestion and creates a real supplier order early.
			quantity = 0
			resource.SuggestedQuantity = 0
			resource.SuggestedAction = smartActionObserveDemand
		} else if adjustedQuantity != quantity {
			quantity = adjustedQuantity
			selection, err = s.selectSupplyPlatform(ctx, supplyCfg, quantity, openOrders)
			if err != nil {
				return err
			}
			platform = selection.platform
			inventory = *selection.status.Inventory
			balance = *selection.status.Balance
			pressure = s.smartSupplyPressure(ctx, supplyCfg, inventory, quantity)
			applySmartSupplyPressure(&resource, pressure)
		}
		if pressureReason != "" {
			resource.DecisionReason = pressureReason
		}
		if smartResourceEmergency(resource) && !smartAccountAvailabilityEmergency(resource) &&
			!isSmartEmergencyRetryReason(resource.DecisionReason) {
			resource.DecisionReason = "emergency_refill_to_healthy"
		}
		// Preserve the full verified shortage as the durable task target while the
		// emergency competition window is open. The purchase-task worker shards
		// this target across its remaining slots (for example 20 -> 7 + 7 + 6),
		// and ready/take admission selects the best live-deficit combination before
		// any reservation is paid.
		if parallelEligible {
			competition, found, competitionErr := s.parallelSupplyCompetition(ctx, supplyCfg, openOrders)
			if competitionErr != nil {
				return competitionErr
			}
			stagedQuantity := emergencyParallelOrderQuantity(resource, quantity, competition, found)
			switch {
			case stagedQuantity <= 0:
				quantity = 0
				resource.SuggestedAction = smartActionWaitLocked
				resource.DecisionReason = "parallel_ladder_exhausted"
				resource.SuggestedQuantity = 0
				if found {
					resource.LockedOrderID = competition.anchor.OrderID
				}
			case stagedQuantity != quantity:
				quantity = stagedQuantity
				selection, err = s.selectSupplyPlatform(ctx, supplyCfg, quantity, openOrders)
				if err != nil {
					return err
				}
				platform = selection.platform
				inventory = *selection.status.Inventory
				balance = *selection.status.Balance
				applySmartSupplyPressure(&resource, s.smartSupplyPressure(ctx, supplyCfg, inventory, quantity))
			}
		}
	}
	s.setOverview(Overview{
		CheckedAtMS:        time.Now().UnixMilli(),
		CPAAvailable:       available,
		CPATarget:          supplyCfg.TargetAvailableAccounts,
		CPADeficit:         max(0, supplyCfg.TargetAvailableAccounts-available),
		Inventory:          &inventory,
		Balance:            &balance,
		SelectedPlatformID: platform.ID,
		Platforms:          selection.all,
	})
	if quantity <= 0 {
		s.setSmartResource(resource)
		s.updateCPAOverview(available, supplyCfg.TargetAvailableAccounts)
		return nil
	}
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
		if inventory.Available <= 0 && !inventory.NeedsProduction && resource.HealthLevel != smartHealthCritical {
			resource.SuggestedAction = smartActionInventoryBlocked
			resource.DecisionReason = "inventory_unavailable"
			s.setSmartResource(resource)
			return nil
		}
		resource.SuggestedQuantity = quantity
		resource.PrelockedCapacityRCU = round2(resource.PrelockedCapacityRCU + estimatedSupplyOrderCapacityRCU(supplyCfg, resource, quantity))
		applySmartRefillProjection(supplyCfg, &resource)
		s.setSmartResource(resource)
	}

	triggerReason := supplyOrderTriggerReason(resource, manualQuantity == 0)
	if len(openOrders) > 0 && maxConcurrentSupplyOrders(supplyCfg) > 1 {
		triggerReason = parallelSupplyTriggerReason(triggerReason)
	}
	timing.next("task-enqueue")
	maxConcurrent := 1
	if manualQuantity == 0 && smartResourceEmergency(resource) && !smartProgressiveStartupFloorRecovery(resource) {
		maxConcurrent = maxConcurrentSupplyOrders(supplyCfg)
	}
	_, err = s.upsertAutomaticPurchaseTask(ctx, store.SupplyPurchaseTask{
		Source:              "automatic",
		Product:             platform.Product,
		TargetQuantity:      quantity,
		Status:              "pending",
		Strategy:            supplyOrderStrategy(supplyCfg, true),
		TriggerReason:       triggerReason,
		MaxConcurrentOrders: maxConcurrent,
	})
	if err == nil {
		s.signalPurchaseTaskWorker()
	}
	return err
}

func (s *Service) processOrder(ctx context.Context, cfg store.ManagerConfig, order store.SupplyOrder) error {
	// Every successful processOrder path may advance or finish an order. Drop
	// the short history cache once on exit rather than relying on each status
	// branch to remember the invalidation.
	defer s.invalidateSupplyOrdersCache()
	if stopped, err := s.stopPurchaseTaskOrderIfNeeded(ctx, &order); stopped || err != nil {
		return err
	}
	if purchaseTaskOrderRequiresOperatorReview(order) {
		return nil
	}
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
		return s.retryUncertainCreate(ctx, cfg, order)
	}
	total, imported, err := s.store.SupplyImportCounts(ctx, order.OrderID)
	if err != nil {
		return err
	}
	if total > imported {
		importCtx, cancelImport := supplyImportCompletionContext(ctx)
		defer cancelImport()
		return s.importItems(importCtx, cfg, &order)
	}
	if released, err := s.autoReleaseAutomaticOrderIfNotNeeded(ctx, cfg, &order, true); released || err != nil {
		return err
	}
	platform, err := resolveSupplyPlatform(cfg.Supply, order.SupplierID, order.Product)
	if err != nil {
		return err
	}
	credentials := marketplaceSellerCredentials(platform, marketplaceSellerSelectionFromOrder(order))
	remote, err := s.supplyClient.GetOrder(ctx, credentials, order.OrderID, order.StatusURL)
	if err != nil {
		if isHTTPStatus(err, http.StatusConflict) ||
			(isHTTPStatus(err, http.StatusNotFound) && !nvtokensPaidOrderLookupUncertain(platform, order)) {
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
	takeAllowed := s.smartTakeAllowed(cfg.Supply, order.OrderID)
	if !takeAllowed && order.Automatic && supplyOrderHasPaymentEvidence(order) {
		// The task ledger is the durable admission record for a paid delivery.
		// Prefer its remaining target over a moving dashboard snapshot so a ready
		// order cannot be kept in ready forever by repeated status refreshes.
		readyAllowed, readyErr := s.purchaseTaskReadyTakeAllowed(ctx, order)
		if readyErr != nil {
			return readyErr
		}
		if readyAllowed {
			resource := s.currentSmartResource(cfg.Supply)
			resource.LockedOrderID = order.OrderID
			resource.LockedOrderAgeSeconds = max(0, int(time.Since(time.UnixMilli(order.CreatedAtMS)).Seconds()))
			resource.SuggestedAction = smartActionTakeLocked
			resource.DecisionReason = "purchase_task_ready_target_remaining"
			s.setSmartResource(resource)
			takeAllowed = true
		}
	}
	if !retryingTake && order.Automatic && smartSupplyEnabled(cfg.Supply) && !takeAllowed {
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
	if order.ChargedFen <= 0 {
		order.ChargedFen = supplyOrderItemsChargedFen(taken.OrderItems)
	}
	replacementSyncErr := s.syncTakeReplacementFiles(ctx, cfg, order.OrderID, taken.ReplacementFiles)
	if taken.Pending {
		order.Status = "waiting_inventory"
		if err := s.store.UpdateSupplyOrder(ctx, order); err != nil {
			return err
		}
		return replacementSyncErr
	}
	normalized := make([]normalizedSupplyAccount, 0, len(taken.Accounts))
	for index, raw := range taken.Accounts {
		normalizedAccounts, err := normalizeAccountPayloads(raw)
		if err != nil {
			return s.failUndeliverableOrder(ctx, &order, fmt.Errorf("supply account %d format is unsupported: %w", index+1, err))
		}
		normalized = append(normalized, normalizedAccounts...)
	}
	// A supplier item describes delivery validity, whereas a returned payload may
	// be a Sub2API bundle that expands into several CPA files. Apply per-item
	// timing only when that expansion is exactly one-to-one. nvtokens timing is
	// supplier warranty, never credential expiry, so clear the normalization
	// fallback even when a bundled delivery cannot be mapped exactly.
	isNvtokensDelivery := strings.EqualFold(strings.TrimSpace(platform.Type), managerconfigsvc.SupplyPlatformNvtokens)
	if isNvtokensDelivery {
		for index := range normalized {
			normalized[index].leaseExpiresAtMS = 0
		}
	}
	applySupplyOrderItemDetails(normalized, taken.OrderItems, time.Now(), isNvtokensDelivery)
	items := make([]store.SupplyImportItem, 0, len(normalized))
	seenItemKeys := make(map[string]struct{}, len(normalized))
	for _, account := range normalized {
		if _, duplicate := seenItemKeys[account.itemKey]; duplicate {
			continue
		}
		seenItemKeys[account.itemKey] = struct{}{}
		items = append(items, store.SupplyImportItem{
			OrderID:                   order.OrderID,
			ItemKey:                   account.itemKey,
			AccountName:               account.accountName,
			NameKey:                   account.nameKey,
			FileName:                  account.fileName,
			PayloadJSON:               string(account.payload),
			LeaseExpiresAtMS:          account.leaseExpiresAtMS,
			WarrantyExpiresAtMS:       account.warrantyExpiresAtMS,
			MarketplaceSellerID:       order.MarketplaceSellerID,
			MarketplaceSellerName:     order.MarketplaceSellerName,
			MarketplaceChannelID:      order.MarketplaceChannelID,
			MarketplaceSelectionToken: order.MarketplaceSelectionToken,
			BasePriceFen:              account.basePriceFen,
			ChargedFen:                account.chargedFen,
		})
	}
	if len(items) == 0 {
		err := errors.New("supply take response did not include importable accounts")
		return s.failUndeliverableOrder(ctx, &order, err)
	}
	if _, err := s.store.InsertSupplyImportItems(ctx, order.OrderID, items); err != nil {
		return s.updateOrderError(ctx, &order, err, cfg.Supply)
	}
	s.invalidateMarketplaceSupplierQuotaScores(order.SupplierID, order.Product)
	s.resetCriticalConfirm(order.OrderID)
	order.Status = "importing"
	order.LastError = ""
	if err := s.store.UpdateSupplyOrder(ctx, order); err != nil {
		return err
	}
	// The supplier delivery and its import tasks are durable at this point.
	// Finish registering them even when the dashboard request disconnects or
	// reaches its client-side timeout; otherwise a paid batch can be recorded as
	// partially failed while CPA is still completing normal initialization.
	importCtx, cancelImport := supplyImportCompletionContext(ctx)
	defer cancelImport()
	if err := s.importItems(importCtx, cfg, &order); err != nil {
		return err
	}
	return replacementSyncErr
}

const supplyImportCompletionTimeout = 15 * time.Minute

func supplyImportCompletionContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	} else {
		parent = context.WithoutCancel(parent)
	}
	return context.WithTimeout(parent, supplyImportCompletionTimeout)
}

func (s *Service) syncTakeReplacementFiles(ctx context.Context, cfg store.ManagerConfig, sourceOrderID string, files []supplyclient.ReplacementFile) error {
	if len(files) == 0 {
		return nil
	}
	order, found, err := s.store.GetSupplyOrder(ctx, sourceOrderID)
	if err != nil {
		return err
	}
	platform, err := resolveSupplyPlatform(cfg.Supply, order.SupplierID, order.Product)
	if err != nil || !found {
		platform, err = recoverySupplyPlatform(cfg.Supply)
	}
	if err != nil {
		return err
	}
	credentials := marketplaceSellerCredentials(platform, marketplaceSellerSelectionFromOrder(order))
	recoveries := make([]store.SupplyRecovery, 0, len(files))
	for _, file := range files {
		file.SourceOrderID = firstNonEmptyString(file.SourceOrderID, sourceOrderID)
		remote := supplyclient.Recovery{
			ID:                strings.TrimSpace(file.RecoveryID),
			DeliveryStatus:    "pending",
			Product:           file.Product,
			SourceOrderID:     file.SourceOrderID,
			OriginalEmail:     file.OriginalEmail,
			OriginalAccount:   file.OriginalAccount,
			OriginalAuthIndex: file.OriginalAuthIndex,
			ClaimURL:          file.ClaimURL,
			ClaimTicket:       file.ClaimTicket,
			StatusURL:         file.StatusURL,
			CredentialVersion: file.CredentialVersion,
			Raw:               file.Raw,
		}
		if file.Ready {
			remote.DeliveryStatus = "ready"
			if strings.TrimSpace(file.StatusURL) != "" {
				latest, err := s.supplyClient.GetRecovery(ctx, credentials, file.RecoveryID, file.StatusURL)
				if err != nil {
					local := supplyRecoveryFromClient(remote, platform.ID)
					local.LastError = "replacement status refresh deferred: " + safeError(err)
					recoveries = append(recoveries, local)
					continue
				}
				mergeSupplyRecovery(&remote, latest)
			}
		}
		recoveries = append(recoveries, supplyRecoveryFromClient(remote, platform.ID))
	}
	_, err = s.store.UpsertSupplyRecoveries(ctx, recoveries)
	return err
}

func mergeSupplyRecovery(target *supplyclient.Recovery, source supplyclient.Recovery) {
	if target == nil {
		return
	}
	if source.ID != "" {
		target.ID = source.ID
	}
	if source.DeliveryStatus != "" {
		target.DeliveryStatus = source.DeliveryStatus
	}
	if source.Product != "" {
		target.Product = source.Product
	}
	if source.SourceOrderID != "" {
		target.SourceOrderID = source.SourceOrderID
	}
	if source.OriginalEmail != "" {
		target.OriginalEmail = source.OriginalEmail
	}
	if source.OriginalAccount != "" {
		target.OriginalAccount = source.OriginalAccount
	}
	if source.OriginalAuthIndex != "" {
		target.OriginalAuthIndex = source.OriginalAuthIndex
	}
	if source.ClaimURL != "" {
		target.ClaimURL = source.ClaimURL
	}
	if source.ClaimTicket != "" {
		target.ClaimTicket = source.ClaimTicket
	}
	if source.StatusURL != "" {
		target.StatusURL = source.StatusURL
	}
	if source.CredentialVersion > target.CredentialVersion {
		target.CredentialVersion = source.CredentialVersion
	}
	if source.RefundedFen > 0 {
		target.RefundedFen = source.RefundedFen
	}
	if len(source.Raw) > 0 {
		target.Raw = source.Raw
	}
}

func (s *Service) retryUncertainCreate(ctx context.Context, cfg store.ManagerConfig, attempt store.SupplyOrder) error {
	platform, err := resolveSupplyPlatform(cfg.Supply, attempt.SupplierID, attempt.Product)
	if err != nil {
		return err
	}
	credentials := marketplaceSellerCredentials(platform, marketplaceSellerSelectionFromOrder(attempt))
	remote, err := s.supplyClient.CreateOrder(ctx, credentials, attempt.Product, attempt.RequestedQuantity, attempt.OrderID)
	if err != nil {
		attempt.LastError = safeError(err)
		attempt.NextPollAtMS = nextSupplierRetryAt(cfg.Supply, err)
		if isDefiniteCreateFailure(err) {
			attempt.Status = "failed"
			attempt.CompletedAtMS = time.Now().UnixMilli()
		}
		if updateErr := s.store.UpdateSupplyOrder(ctx, attempt); updateErr != nil {
			return updateErr
		}
		return err
	}
	order := supplyOrderFromCreateResponse(attempt, remote, cfg.Supply)
	if err := s.store.PromoteSupplyCreateAttempt(ctx, attempt.OrderID, order); err != nil {
		return err
	}
	if order.Status == "ready" || order.Status == "taking" {
		return s.processOrder(ctx, cfg, order)
	}
	return nil
}

func supplyOrderFromCreateResponse(attempt store.SupplyOrder, remote supplyclient.Order, cfg store.ManagerSupplyConfig) store.SupplyOrder {
	order := store.SupplyOrder{
		ID:                        attempt.ID,
		OrderID:                   remote.ID,
		TaskID:                    attempt.TaskID,
		SupplierID:                attempt.SupplierID,
		MarketplaceSellerID:       attempt.MarketplaceSellerID,
		MarketplaceSellerName:     attempt.MarketplaceSellerName,
		MarketplaceChannelID:      attempt.MarketplaceChannelID,
		MarketplaceSelectionToken: attempt.MarketplaceSelectionToken,
		Product:                   attempt.Product,
		RequestedQuantity:         attempt.RequestedQuantity,
		Automatic:                 attempt.Automatic,
		Strategy:                  attempt.Strategy,
		TriggerReason:             attempt.TriggerReason,
		Status:                    localOrderStatus(remote.Status),
		RemoteStatus:              remote.Status,
		ReadyQuantity:             remote.ReadyQuantity,
		Progress:                  remote.Progress,
		StatusURL:                 remote.StatusURL,
		TakeURL:                   remote.TakeURL,
		ChargedFen:                remote.ChargedFen,
		ReleasedFen:               remote.ReleasedFen,
		NextPollAtMS:              nextPollAt(cfg, remote.RetryAfterSeconds),
		SupplierRetryUntilMS:      supplierRetryUntilMS(remote.RetryAfterSeconds),
		CreatedAtMS:               attempt.CreatedAtMS,
	}
	if isTerminalRemoteStatus(remote.Status) && !isSuccessfulRemoteStatus(remote.Status) {
		order.CompletedAtMS = time.Now().UnixMilli()
	}
	return order
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
	if isLowPriceReserveTrigger(order.TriggerReason) && supplyLowPriceReserveEnabled(cfg.Supply) {
		handled, released, err := s.lowPriceReserveOrderAdmission(ctx, cfg, order)
		if handled || err != nil {
			return released, err
		}
	}
	if platform, platformErr := resolveSupplyPlatform(cfg.Supply, order.SupplierID, order.Product); platformErr == nil && strings.EqualFold(strings.TrimSpace(platform.Type), managerconfigsvc.SupplyPlatformNvtokens) &&
		supplyOrderHasPaymentEvidence(*order) &&
		(isReadyForTake(order.Status) || isReadyForTake(order.RemoteStatus)) {
		// NV does not expose an order release/refund operation. Once the order is
		// ready, the account already exists and the balance has been charged; a
		// local "released" flag only hides paid inventory. Force the idempotent
		// Take/import path regardless of a recovered capacity snapshot.
		resource := s.currentSmartResource(cfg.Supply)
		resource.LockedOrderID = order.OrderID
		resource.LockedOrderAgeSeconds = max(0, int(time.Since(time.UnixMilli(order.CreatedAtMS)).Seconds()))
		resource.SuggestedAction = smartActionTakeLocked
		resource.DecisionReason = "paid_ready_order_take"
		s.setSmartResource(resource)
		return false, nil
	}
	if smartSupplyEnabled(cfg.Supply) {
		resource, err := s.smartResource(ctx, cfg, forceSmartRefresh)
		if err != nil || !resource.SnapshotFresh {
			// A durable purchase task is the source of truth for an order that has
			// already secured supplier stock. A long-running inspection refresh must
			// not strand a paid ready order when its quantity still fits the task's
			// unfulfilled target. This path never creates another order; it only lets
			// the existing idempotent Take continue.
			allowed, taskErr := s.purchaseTaskReadyTakeAllowed(ctx, *order)
			if taskErr != nil {
				return false, taskErr
			}
			if allowed {
				resource.LockedOrderID = order.OrderID
				resource.LockedOrderAgeSeconds = max(0, int(time.Since(time.UnixMilli(order.CreatedAtMS)).Seconds()))
				resource.SuggestedAction = smartActionTakeLocked
				resource.DecisionReason = "purchase_task_ready_stale_snapshot"
				s.setSmartResource(resource)
				return false, nil
			}
			// A stale/unknown snapshot is not enough evidence to abandon a paid
			// reservation. Continue the normal status polling path instead.
			return false, nil
		}
		// A quota snapshot can remain healthy while the live CPA pool has just
		// lost several credentials to 401/quarantine. Refresh the live account
		// waterline before releasing a waiting order; otherwise the worker can
		// release the emergency reservation and submit another one on the next
		// cooldown tick, creating a vacuum and a chain of held reservations.
		_, emergencyQuantity, emergencyReason, accountLoaded, err := s.smartEmergencyAvailability(ctx, cfg, &resource)
		if err != nil {
			return false, err
		}
		// A fresh planner snapshot may recover after the first child accounts are
		// imported even though the durable purchase task still has an unmet target.
		// Keep an already-paid ready child moving within that task budget instead of
		// discarding it locally and making the task reserve another supplier order.
		allowedByTask, taskErr := s.purchaseTaskReadyTakeAllowed(ctx, *order)
		if taskErr != nil {
			return false, taskErr
		}
		if allowedByTask {
			resource.LockedOrderID = order.OrderID
			resource.LockedOrderAgeSeconds = max(0, int(time.Since(time.UnixMilli(order.CreatedAtMS)).Seconds()))
			resource.SuggestedAction = smartActionTakeLocked
			resource.DecisionReason = "purchase_task_ready_target_remaining"
			s.setSmartResource(resource)
			return false, nil
		}
		if release, accepted, reason, err := s.shouldReleaseOversizedOpenOrder(ctx, cfg.Supply, resource, order); err != nil {
			return false, err
		} else if accepted {
			resource.LockedOrderID = order.OrderID
			resource.LockedOrderAgeSeconds = max(0, int(time.Since(time.UnixMilli(order.CreatedAtMS)).Seconds()))
			resource.SuggestedAction = smartActionTakeLocked
			resource.DecisionReason = reason
			s.setSmartResource(resource)
			return false, nil
		} else if release {
			resource.LockedOrderID = order.OrderID
			resource.LockedOrderAgeSeconds = max(0, int(time.Since(time.UnixMilli(order.CreatedAtMS)).Seconds()))
			resource.EmergencyShortage = false
			resource.EmergencyReason = ""
			resource.SuggestedAction = smartActionReleaseLocked
			resource.DecisionReason = reason
			s.setSmartResource(resource)
			return true, s.markAutomaticOrderReleasedLocally(ctx, order)
		}
		if accountLoaded && emergencyQuantity > 0 {
			resource.LockedOrderID = order.OrderID
			resource.LockedOrderAgeSeconds = max(0, int(time.Since(time.UnixMilli(order.CreatedAtMS)).Seconds()))
			resource.EmergencyShortage = true
			resource.EmergencyReason = emergencyReason
			resource.SuggestedAction = smartActionEmergencyReplenish
			resource.DecisionReason = emergencyReason
			resource.SuggestedQuantity = emergencyQuantity
			s.setSmartResource(resource)
			return false, nil
		}
		if !isSupplyOrderCapacityCommitted(*order) {
			// A waiting supplier reservation is still useful even when the latest
			// pool snapshot temporarily recovers. The supplier has no cancellation
			// endpoint; marking it released locally forgets the reservation and can
			// create another identical order a few seconds later. Keep polling this
			// order until it becomes ready or reaches a supplier terminal state.
			resource.LockedOrderID = order.OrderID
			resource.LockedOrderAgeSeconds = max(0, int(time.Since(time.UnixMilli(order.CreatedAtMS)).Seconds()))
			resource.SuggestedAction = smartActionWaitLocked
			resource.SuggestedQuantity = 0
			resource.DecisionReason = "waiting_inventory_reservation"
			s.setSmartResource(resource)
			return false, nil
		}
		if resource.ConsumeRCUPerMinute <= 0 && resource.DemandTrend != smartDemandTrendFalling {
			// The live-account gate above already retained this reservation for an
			// empty or sub-critical pool. With no current traffic and the minimum
			// floor satisfied, taking more short-lived credentials would only
			// create expiry waste, so release the local reservation immediately.
			resource.LockedOrderID = order.OrderID
			resource.LockedOrderAgeSeconds = max(0, int(time.Since(time.UnixMilli(order.CreatedAtMS)).Seconds()))
			resource.EmergencyShortage = false
			resource.EmergencyReason = ""
			resource.SuggestedAction = smartActionReleaseLocked
			resource.SuggestedQuantity = 0
			resource.DecisionReason = "no_traffic_minimum_pool"
			s.setSmartResource(resource)
			return true, s.markAutomaticOrderReleasedLocally(ctx, order)
		}
		resource.LockedOrderID = order.OrderID
		resource.LockedOrderAgeSeconds = max(0, int(time.Since(time.UnixMilli(order.CreatedAtMS)).Seconds()))
		if resource.HealthLevel != smartHealthHealthy && resource.CapacityGapRCU > 0 {
			if smartResourceEmergency(resource) {
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
	if !isSupplyOrderCapacityCommitted(*order) {
		return false, nil
	}
	if cfg.Supply.TargetAvailableAccounts <= 0 {
		return false, nil
	}
	available, err := s.countAvailableAccounts(ctx, cfg)
	if err != nil {
		return false, err
	}
	s.updateCPAOverview(available, cfg.Supply.TargetAvailableAccounts)
	deficit := max(0, cfg.Supply.TargetAvailableAccounts-available)
	if deficit <= 0 {
		return true, s.markAutomaticOrderReleasedLocally(ctx, order)
	}
	orders, err := s.store.ListOpenSupplyOrders(ctx, maxTrackedOpenSupplyOrders)
	if err != nil {
		return false, err
	}
	unit := smartEstimatedNewAccountCapacityForResource(cfg.Supply, SmartResource{})
	need := float64(deficit) * unit
	allowance := math.Max(unit, need*0.15)
	if readySupplyOrderAccepted(cfg.Supply, SmartResource{}, orders, order, need, allowance) {
		return false, nil
	}
	return true, s.markAutomaticOrderReleasedLocally(ctx, order)
}

// lowPriceReserveOrderAdmission keeps a cheap reservation useful even while
// the capacity planner is healthy or idle. Once the configured reserve
// waterline has already been reached, the normal smart/legacy admission logic
// takes over so a real shortage can still retain the same order.
func (s *Service) lowPriceReserveOrderAdmission(
	ctx context.Context,
	cfg store.ManagerConfig,
	order *store.SupplyOrder,
) (handled bool, released bool, err error) {
	if order == nil || !isLowPriceReserveTrigger(order.TriggerReason) || !supplyLowPriceReserveEnabled(cfg.Supply) {
		return false, false, nil
	}
	available, err := s.countLowPriceReserveAccounts(ctx, cfg)
	if err != nil {
		return true, false, err
	}
	remaining := cfg.Supply.LowPriceReserveTargetAccounts - available
	if remaining <= 0 {
		return false, false, nil
	}
	if !isSupplyOrderCapacityCommitted(*order) {
		return true, false, nil
	}
	actual := max(order.ReadyQuantity, max(order.ItemCount, order.RequestedQuantity))
	allowance := max(1, int(math.Ceil(float64(remaining)*0.15)))
	if actual <= remaining+allowance {
		return true, false, nil
	}
	return false, false, nil
}

func (s *Service) shouldReleaseOversizedOpenOrder(
	ctx context.Context,
	cfg store.ManagerSupplyConfig,
	resource SmartResource,
	order *store.SupplyOrder,
) (bool, bool, string, error) {
	if s == nil || order == nil || !order.Automatic {
		return false, false, "", nil
	}
	if order.ReadyQuantity <= 0 && !isReadyForTake(order.RemoteStatus) &&
		!isReadyForTake(order.Status) {
		// Keep parallel reservations alive while they are still competing for
		// supplier inventory. The aggregate cap is enforced when an order is
		// actually ready to take, where the total ready/in-flight capacity is
		// known and a surplus reservation can be left to expire.
		return false, false, "", nil
	}
	orders, err := s.store.ListOpenSupplyOrders(ctx, maxTrackedOpenSupplyOrders)
	if err != nil {
		return false, false, "", err
	}
	if resource.ConsumeRCUPerMinute <= 0 || resource.HealthLevel == smartHealthHealthy {
		// Idle traffic and genuinely healthy verified capacity retain the existing
		// release behavior. Overage tolerance protects an active shortage, not a
		// minimum account floor that has no current demand behind it.
		return false, false, "", nil
	}
	// CapacityGapRCU already subtracts prelocked orders. Use the live shortage
	// before reservations here, otherwise an order can erase its own deficit as
	// soon as stock becomes ready and then be released instead of taken.
	need := math.Max(0, resource.TargetCapacityRCU-resource.CurrentCapacityRCU)
	need = math.Max(need, math.Max(0, resource.CapacityGapRCU))
	unit := smartEstimatedNewAccountCapacityForResource(cfg, resource)
	accountFloor := max(resource.TargetAvailableAccounts, resource.HealthyAvailableAccounts)
	accountDeficit := max(0, accountFloor-resource.AvailableAccounts)
	accountDeficit = max(accountDeficit, max(0, resource.AccountQuantityDeficit))
	accountDeficit = max(accountDeficit, max(0, resource.ConcurrencyAccountDeficit))
	if unit > 0 && accountDeficit > 0 {
		need = math.Max(need, float64(accountDeficit)*unit)
	}
	if need <= 0 {
		return false, false, "", nil
	}
	// One account-equivalent or 15% of the required capacity is a small
	// tolerance for live deficit drift while an order is competing for stock.
	// A ready order inside this range is taken immediately; releasing it would
	// discard scarce inventory only to recreate nearly the same order later.
	allowance := math.Max(unit, need*0.15)
	if readySupplyOrderAccepted(cfg, resource, orders, order, need, allowance) {
		return false, true, "low_water_take_ready", nil
	}
	return true, false, "parallel_capacity_overage", nil
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
	s.importMu.Lock()
	defer s.importMu.Unlock()
	items, err := s.store.ListPendingSupplyImportItems(ctx, order.OrderID, time.Now().UnixMilli(), 100)
	if err != nil {
		return err
	}
	primaryRecoveryItemID := int64(0)
	if strings.EqualFold(strings.TrimSpace(order.Strategy), "recovery") {
		allItems, listErr := s.store.ListSupplyImportItemsByOrderIDs(ctx, []string{order.OrderID})
		if listErr != nil {
			return listErr
		}
		for _, candidate := range allItems {
			if primaryRecoveryItemID == 0 || candidate.ID < primaryRecoveryItemID {
				primaryRecoveryItemID = candidate.ID
			}
		}
	}
	var firstErr error
	for _, item := range items {
		account, err := normalizeAccountForImport(item.PayloadJSON)
		if err == nil {
			account = withSupplyAccountLeaseMetadata(account, item.LeaseExpiresAtMS)
			account = withSupplyAccountWarrantyMetadata(account, item.WarrantyExpiresAtMS)
			account = withSupplyAccountImportMetadata(account, cfg.Supply, *order, time.Now())
		}
		fileName := item.FileName
		importAction := strings.ToLower(strings.TrimSpace(item.ImportAction))
		if err == nil && (importAction != "add" && importAction != "replace") {
			plan, planErr := s.resolveSupplyImportPlan(ctx, cfg, *order, item, account, primaryRecoveryItemID == 0 || item.ID == primaryRecoveryItemID)
			if planErr != nil {
				err = planErr
			} else {
				fileName = plan.fileName
				importAction = plan.action
				item.AccountName = account.accountName
				item.NameKey = account.nameKey
				item.FileName = fileName
				item.ImportAction = importAction
				item.ReplacedFileName = plan.replacedFileName
				err = s.store.UpdateSupplyImportItemPlan(ctx, item.ID, item.AccountName, item.NameKey, fileName, importAction, plan.replacedFileName)
			}
		}
		if err == nil {
			err = s.ensureCPAAccountImported(ctx, cfg, fileName, account.payload, importAction, account)
		}
		if err == nil {
			if markErr := s.store.MarkSupplyImportItemImported(ctx, item.ID, time.Now().UnixMilli()); markErr != nil {
				return markErr
			}
			s.invalidateAuthAndCapacityCaches()
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
	} else if settled, settlementError, settleErr := s.supplyImportFailuresSettled(ctx, *order, time.Now()); settleErr != nil {
		return settleErr
	} else if settled {
		if imported > 0 {
			order.Status = "completed_partial"
		} else {
			order.Status = "failed"
		}
		order.CompletedAtMS = time.Now().UnixMilli()
		order.NextPollAtMS = 0
		order.LastError = settlementError
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
	if (order.Status == "completed" || order.Status == "completed_partial") && order.Automatic {
		s.invalidateInspectionQuotaSnapshot()
		s.requestStaleInspectionSnapshotRefresh()
	}
	if order.Status == "completed_partial" || order.Status == "failed" {
		// The supplier delivery is already terminal and every remaining item has
		// exhausted its useful import path. Closing the order returns that failed
		// quantity to the live deficit instead of retaining an active-order lock.
		return nil
	}
	return firstErr
}

const minimumUsefulSupplyImportLease = 5 * time.Minute

// supplyImportFailuresSettled reports whether every non-imported item in a
// terminal supplier order has become unusable. Recovery claims retain their
// existing retry workflow; only paid supplier orders are closed here.
func (s *Service) supplyImportFailuresSettled(ctx context.Context, order store.SupplyOrder, now time.Time) (bool, string, error) {
	if s == nil || s.store == nil || strings.HasPrefix(order.OrderID, "recovery-") ||
		strings.EqualFold(strings.TrimSpace(order.Strategy), "recovery") ||
		!supplyDeliveryFinishedForImportSettlement(order) {
		return false, "", nil
	}
	items, err := s.store.ListSupplyImportItemsByOrderIDs(ctx, []string{order.OrderID})
	if err != nil {
		return false, "", err
	}
	terminalFailures := 0
	lastError := ""
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item.Status), "imported") {
			continue
		}
		if !terminalSupplyImportItem(item, now) {
			return false, "", nil
		}
		terminalFailures++
		if strings.TrimSpace(item.LastError) != "" {
			lastError = strings.TrimSpace(item.LastError)
		}
	}
	if terminalFailures == 0 {
		return false, "", nil
	}
	message := fmt.Sprintf("supplier delivery settled with %d unusable account(s)", terminalFailures)
	if lastError != "" {
		message += ": " + lastError
	}
	return true, message, nil
}

func supplyDeliveryFinishedForImportSettlement(order store.SupplyOrder) bool {
	if isSuccessfulRemoteStatus(order.RemoteStatus) {
		return true
	}
	status := strings.ToLower(strings.TrimSpace(order.RemoteStatus))
	switch status {
	case "partial", "completed_partial", "partially_completed":
		// Some suppliers finalize a short delivery as partial and release the
		// unused hold. Progress below 100 still means more inventory may arrive.
		return order.Progress >= 100 &&
			(order.ItemCount > 0 || order.ReadyQuantity > 0 || order.ReleasedFen > 0)
	default:
		return false
	}
}

func terminalSupplyImportItem(item store.SupplyImportItem, now time.Time) bool {
	if item.LeaseExpiresAtMS > 0 && item.LeaseExpiresAtMS <= now.UnixMilli() {
		return true
	}
	if item.AttemptCount > 0 && permanentCPAAuthLifecycleFailure(item.LastError) {
		return true
	}
	return item.AttemptCount >= 3 && item.LeaseExpiresAtMS > 0 &&
		time.UnixMilli(item.LeaseExpiresAtMS).Sub(now) <= minimumUsefulSupplyImportLease
}

func withSupplyAccountLeaseMetadata(account normalizedSupplyAccount, leaseExpiresAtMS int64) normalizedSupplyAccount {
	if leaseExpiresAtMS <= 0 || len(account.payload) == 0 {
		return account
	}
	var metadata map[string]any
	if json.Unmarshal(account.payload, &metadata) != nil {
		return account
	}
	metadata["supply_lease_expires_at_ms"] = leaseExpiresAtMS
	metadata["supply_lease_expires_at"] = time.UnixMilli(leaseExpiresAtMS).UTC().Format(time.RFC3339)
	if normalized, err := json.Marshal(metadata); err == nil {
		account.payload = normalized
	}
	return account
}

func withSupplyAccountWarrantyMetadata(account normalizedSupplyAccount, warrantyExpiresAtMS int64) normalizedSupplyAccount {
	if warrantyExpiresAtMS <= 0 || len(account.payload) == 0 {
		return account
	}
	var metadata map[string]any
	if json.Unmarshal(account.payload, &metadata) != nil {
		return account
	}
	deleteSupplyLeaseMetadata(metadata)
	metadata["supply_warranty_expires_at_ms"] = warrantyExpiresAtMS
	metadata["supply_warranty_expires_at"] = time.UnixMilli(warrantyExpiresAtMS).UTC().Format(time.RFC3339)
	if normalized, err := json.Marshal(metadata); err == nil {
		account.payload = normalized
	}
	return account
}

func deleteSupplyLeaseMetadata(metadata map[string]any) {
	if metadata == nil {
		return
	}
	for _, key := range []string{
		"supply_lease_expires_at_ms", "supplyLeaseExpiresAtMs",
		"supply_lease_expires_at", "supplyLeaseExpiresAt",
	} {
		delete(metadata, key)
	}
}

func withSupplyAccountImportMetadata(account normalizedSupplyAccount, cfg store.ManagerSupplyConfig, order store.SupplyOrder, importedAt time.Time) normalizedSupplyAccount {
	if len(account.payload) == 0 {
		return account
	}
	var metadata map[string]any
	if json.Unmarshal(account.payload, &metadata) != nil {
		return account
	}
	platformID := strings.TrimSpace(order.SupplierID)
	platformName := platformID
	if platform, err := resolveSupplyPlatform(cfg, platformID, order.Product); err == nil {
		platformID = firstNonEmptyString(platform.ID, platformID)
		platformName = firstNonEmptyString(platform.Name, platform.ID, platformName)
	}
	if platformID == "" {
		platformID = "supply"
	}
	if platformName == "" {
		platformName = platformID
	}
	method := "manual_supply"
	if isLowPriceReserveTrigger(order.TriggerReason) {
		method = lowPriceReserveTriggerReason
	} else if order.Automatic {
		method = "automatic_supply"
	}
	provenance := map[string]any{
		"version":       1,
		"source":        "supply",
		"method":        method,
		"platform_id":   platformID,
		"platform_name": platformName,
		"imported_by":   "cpa-manager-plus",
		"imported_at":   importedAt.UTC().Format(time.RFC3339Nano),
	}
	if sellerID := strings.TrimSpace(order.MarketplaceSellerID); sellerID != "" {
		provenance["marketplace_seller_id"] = sellerID
		if sellerName := strings.TrimSpace(order.MarketplaceSellerName); sellerName != "" {
			provenance["marketplace_seller_name"] = sellerName
		}
		if channelID := strings.TrimSpace(order.MarketplaceChannelID); channelID != "" {
			provenance["marketplace_channel_id"] = channelID
		}
	}
	metadata["cpamp_import"] = provenance
	if normalized, err := json.Marshal(metadata); err == nil {
		account.payload = normalized
	}
	return account
}

func (s *Service) refreshOverview(ctx context.Context, cfg store.ManagerConfig, quantity int) error {
	available := 0
	if smartSupplyEnabled(cfg.Supply) {
		resource, err := s.smartResource(ctx, cfg, true)
		if err != nil {
			return err
		}
		poolStats, err := s.countAccountPoolStatsWithInspection(ctx, cfg, resource)
		if err != nil {
			return err
		}
		inspectedEnabled := resource.EnabledAccounts
		applyAccountPoolStats(&resource, poolStats)
		capacityChanged := s.reconcileSmartNormalCapacityFloor(cfg.Supply, &resource, poolStats, time.Now())
		if reconcileSmartCapacityWithAccountPool(&resource, inspectedEnabled) {
			capacityChanged = true
		}
		if capacityChanged && resource.ConsumeRCUPerMinute > 0 {
			recalculateSmartResourceCapacityPlan(cfg.Supply, &resource)
		}
		applySmartAccountQuantityEstimate(cfg.Supply, &resource)
		s.setSmartResource(resource)
		// The overview is an operator-facing account summary. Keep it aligned
		// with the credential page instead of exposing the stricter inspection
		// capacity count used by smart replenishment.
		available = poolStats.operatorAvailable(resource.SchedulableAccounts)
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
	selection, err := s.selectSupplyPlatform(ctx, cfg, quantity, nil)
	overview := Overview{
		CheckedAtMS:  time.Now().UnixMilli(),
		CPAAvailable: available,
		CPATarget:    cfg.TargetAvailableAccounts,
		CPADeficit:   max(0, cfg.TargetAvailableAccounts-available),
		Platforms:    selection.all,
	}
	if err != nil {
		if _, _, ok := automaticSupplyWaitDecision(err); ok {
			overview.Inventory = selection.status.Inventory
			overview.SelectedPlatformID = selection.platform.ID
			s.setOverview(overview)
			return nil
		}
		overview.LastError = safeError(err)
		s.setOverview(overview)
		return err
	}
	overview.Inventory = selection.status.Inventory
	overview.Balance = selection.status.Balance
	overview.SelectedPlatformID = selection.platform.ID
	s.setOverview(overview)
	return nil
}

func (s *Service) hydrateOverviewIfNeeded(ctx context.Context, cfg store.ManagerSupplyConfig) {
	if s == nil || !managerconfigsvc.SupplyEnabled(cfg) || !supplyCredentialsConfigured(cfg) {
		return
	}

	s.overviewRefreshMu.Lock()
	defer s.overviewRefreshMu.Unlock()

	s.stateMu.RLock()
	current := s.overview
	s.stateMu.RUnlock()
	hasCompleteQuote := current.Inventory != nil && current.Balance != nil
	if hasCompleteQuote && current.CheckedAtMS > 0 &&
		time.Since(time.UnixMilli(current.CheckedAtMS)) < supplyOverviewQuoteTTL {
		return
	}
	// Keep a failed supplier request from being retried by every 10-second UI
	// refresh while still recovering promptly from a short upstream outage.
	if !hasCompleteQuote && current.CheckedAtMS > 0 && time.Since(time.UnixMilli(current.CheckedAtMS)) < 20*time.Second {
		return
	}
	refreshStartedAtMS := time.Now().UnixMilli()

	refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	selection, err := s.selectSupplyPlatform(refreshCtx, cfg, max(1, cfg.ReplenishBatchSize), nil)
	cancel()

	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	// Automation may have refreshed the overview while the supplier request was
	// in flight. Do not replace fresher complete data with this copy.
	if s.overview.CheckedAtMS > refreshStartedAtMS && s.overview.Inventory != nil && s.overview.Balance != nil {
		return
	}
	if err != nil && hasCompleteQuote {
		// Keep the last valid price and balance visible during a transient supplier
		// outage. The next TTL window retries the live quote.
		s.overview.Platforms = selection.all
		s.overview.LastError = safeError(err)
		return
	}
	s.overview.CheckedAtMS = time.Now().UnixMilli()
	s.overview.CPATarget = cfg.TargetAvailableAccounts
	s.overview.CPADeficit = max(0, cfg.TargetAvailableAccounts-s.overview.CPAAvailable)
	if err != nil {
		s.overview.Platforms = selection.all
		s.overview.LastError = safeError(err)
		return
	}
	s.overview.Inventory = selection.status.Inventory
	s.overview.Balance = selection.status.Balance
	s.overview.SelectedPlatformID = selection.platform.ID
	s.overview.Platforms = selection.all
	s.overview.LastError = ""
}

func supplyCredentialsConfigured(cfg store.ManagerSupplyConfig) bool {
	for _, platform := range supplyPlatforms(cfg) {
		if supplyPlatformConfigured(platform) {
			return true
		}
	}
	return false
}

func cpaManagementConfigured(cfg store.ManagerConfig) bool {
	return strings.TrimSpace(cfg.CPAConnection.CPABaseURL) != "" &&
		strings.TrimSpace(cfg.CPAConnection.ManagementKey) != ""
}

func (s *Service) currentSupplyRevenueMultiplier(ctx context.Context) float64 {
	if s == nil || s.managerConfig == nil {
		return defaultSupplyRevenueMultiplier
	}
	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return defaultSupplyRevenueMultiplier
	}
	return supplyRevenueMultiplier(cfg.Supply)
}

func supplyRevenueMultiplier(cfg store.ManagerSupplyConfig) float64 {
	value := cfg.RevenueMultiplier
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return defaultSupplyRevenueMultiplier
	}
	if value > 100 {
		return 100
	}
	return value
}

func normalizeSupplyAccountsRequest(req SupplyAccountsRequest) SupplyAccountsRequest {
	now := time.Now()
	if req.ToMS <= 0 {
		req.ToMS = now.UnixMilli()
	}
	if req.FromMS <= 0 {
		to := time.UnixMilli(req.ToMS).In(time.Local)
		req.FromMS = time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.Local).UnixMilli()
	}
	if req.ToMS <= req.FromMS {
		req.ToMS = time.UnixMilli(req.FromMS).Add(24 * time.Hour).UnixMilli()
	}
	maxRange := time.UnixMilli(req.ToMS).AddDate(-1, 0, 0).UnixMilli()
	if req.FromMS < maxRange {
		req.FromMS = maxRange
	}
	if req.Limit <= 0 {
		req.Limit = 200
	}
	if req.Limit > 1000 {
		req.Limit = 1000
	}
	req.Status = strings.ToLower(strings.TrimSpace(req.Status))
	return req
}

func normalizeReportRequest(req ReportRequest) ReportRequest {
	now := time.Now()
	if req.ToMS <= 0 {
		req.ToMS = now.UnixMilli()
	}
	if req.FromMS <= 0 {
		to := time.UnixMilli(req.ToMS).In(time.Local)
		req.FromMS = time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.Local).UnixMilli()
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

func supplyAccountListLimit(req SupplyAccountsRequest, statusFilter string) int {
	limit := req.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	if supplyImportStatusFilter(statusFilter) == "" && statusFilter != "" && statusFilter != "all" {
		limit = min(1000, limit*4)
	}
	return limit
}

func supplyImportStatusFilter(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "imported", "pending", "failed":
		return strings.ToLower(strings.TrimSpace(status))
	default:
		return ""
	}
}

func supplyAccountStatusMatches(status string, account SupplyAccountItem) bool {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" || status == "all" {
		return true
	}
	if supplyImportStatusFilter(status) != "" {
		return strings.EqualFold(account.Status, status)
	}
	return strings.EqualFold(account.AccountStatus, status)
}

func supplyCPAFileMap(files []cpaauthfiles.File) map[string]cpaauthfiles.File {
	mapped := make(map[string]cpaauthfiles.File, len(files))
	for _, file := range files {
		name := strings.TrimSpace(file.Name)
		if name == "" {
			continue
		}
		if _, exists := mapped[name]; exists {
			continue
		}
		mapped[name] = file
	}
	return mapped
}

type supplyAccountUsage struct {
	Calls        int64
	SuccessCalls int64
	FailureCalls int64
	Tokens       int64
	Revenue      float64
	LastUsedAtMS int64
}

type supplyAccountIssue struct {
	Auth401AtMS      int64
	Auth401Reason    string
	AutoDisabledAtMS int64
	HitCount         int
}

type supplyAccountRecoveryStatus struct {
	RecoveryID string
	Status     string
}

func (s *Service) supplyAccountUsageByFile(ctx context.Context, req ReportRequest, authFiles []string, prices map[string]store.ModelPrice, revenueMultiplier float64) (map[string]supplyAccountUsage, error) {
	usageByFile := make(map[string]supplyAccountUsage)
	if len(authFiles) == 0 {
		return usageByFile, nil
	}
	req = normalizeReportRequest(req)
	const chunkSize = 200
	for start := 0; start < len(authFiles); start += chunkSize {
		end := start + chunkSize
		if end > len(authFiles) {
			end = len(authFiles)
		}
		stats, err := s.store.CredentialModelStatsWithFilter(ctx, store.AnalyticsFilter{
			FromMS:        req.FromMS,
			ToMS:          req.ToMS,
			AuthFiles:     authFiles[start:end],
			IncludeFailed: true,
		})
		if err != nil {
			return nil, err
		}
		for _, stat := range stats {
			fileName := strings.TrimSpace(stat.AuthFileSnapshot)
			if fileName == "" {
				continue
			}
			current := usageByFile[fileName]
			current.Calls += stat.Calls
			current.SuccessCalls += stat.SuccessCalls
			current.FailureCalls += stat.FailureCalls
			current.Tokens += stat.TotalTokens
			current.Revenue += reportCostForCredentialStat(stat, prices, revenueMultiplier)
			if stat.LastSeenMS > current.LastUsedAtMS {
				current.LastUsedAtMS = stat.LastSeenMS
			}
			usageByFile[fileName] = current
		}
	}
	for fileName, usage := range usageByFile {
		usage.Revenue = reportRatioFloat(usage.Revenue, 1)
		usageByFile[fileName] = usage
	}
	return usageByFile, nil
}

func (s *Service) supplyAccountIssuesByFile(ctx context.Context, authFiles []string) (map[string]supplyAccountIssue, error) {
	result := make(map[string]supplyAccountIssue)
	if s == nil || s.store == nil || len(authFiles) == 0 {
		return result, nil
	}
	candidates, err := s.store.ListAccountActionCandidatesByAuthFiles(ctx, authFiles, len(authFiles)*5)
	if err != nil {
		return nil, err
	}
	for _, candidate := range candidates {
		if !supplyAccountActionIs401(candidate) {
			continue
		}
		fileName := strings.TrimSpace(candidate.AuthFileName)
		if fileName == "" {
			continue
		}
		current := result[fileName]
		if candidate.LastSeenAtMS >= current.Auth401AtMS {
			current.Auth401AtMS = candidate.LastSeenAtMS
			current.Auth401Reason = firstNonEmptyString(candidate.Reason, candidate.ReasonCode)
			current.AutoDisabledAtMS = candidate.AutoDisabledAtMS
			current.HitCount = candidate.HitCount
			result[fileName] = current
		}
	}
	return result, nil
}

func supplyAccountIssuesByFileFromCandidates(candidates []model.AccountActionCandidate) map[string]supplyAccountIssue {
	result := make(map[string]supplyAccountIssue)
	for _, candidate := range candidates {
		if !supplyAccountActionIs401(candidate) {
			continue
		}
		fileName := strings.TrimSpace(candidate.AuthFileName)
		if fileName == "" {
			continue
		}
		current := result[fileName]
		if candidate.LastSeenAtMS >= current.Auth401AtMS {
			current.Auth401AtMS = candidate.LastSeenAtMS
			current.Auth401Reason = firstNonEmptyString(candidate.Reason, candidate.ReasonCode)
			current.AutoDisabledAtMS = candidate.AutoDisabledAtMS
			current.HitCount = candidate.HitCount
			result[fileName] = current
		}
	}
	return result
}

func (s *Service) supplyRecoveriesByOriginalFile(ctx context.Context) (map[string]supplyAccountRecoveryStatus, error) {
	result := make(map[string]supplyAccountRecoveryStatus)
	if s == nil || s.store == nil {
		return result, nil
	}
	recoveries, err := s.store.ListSupplyRecoveries(ctx, 1000, "")
	if err != nil {
		return nil, err
	}
	updatedByFile := map[string]int64{}
	for _, recovery := range recoveries {
		fileName := strings.TrimSpace(recovery.OriginalFileName)
		if fileName == "" {
			continue
		}
		if currentUpdated, ok := updatedByFile[fileName]; !ok || recovery.UpdatedAtMS >= currentUpdated {
			result[fileName] = supplyAccountRecoveryStatus{RecoveryID: recovery.RecoveryID, Status: recovery.Status}
			updatedByFile[fileName] = recovery.UpdatedAtMS
		}
	}
	return result, nil
}

func supplyAccountActionIs401(candidate model.AccountActionCandidate) bool {
	text := strings.ToLower(strings.Join([]string{candidate.ReasonCode, candidate.Reason, candidate.EvidenceJSON}, " "))
	return strings.Contains(text, "401") ||
		strings.Contains(text, "invalid_401") ||
		strings.Contains(text, "invalid_credentials") ||
		strings.Contains(text, "token_revoked")
}

func (s *Service) supplyOrdersForItems(ctx context.Context, existing []store.SupplyOrder, items []store.SupplyImportItem) (map[string]store.SupplyOrder, error) {
	orders := make(map[string]store.SupplyOrder, len(existing))
	for _, order := range existing {
		if strings.TrimSpace(order.OrderID) == "" {
			continue
		}
		orders[order.OrderID] = order
	}
	if s == nil || s.store == nil {
		return orders, nil
	}
	missingIDs := make([]string, 0)
	seenMissing := make(map[string]struct{})
	for _, item := range items {
		orderID := strings.TrimSpace(item.OrderID)
		if orderID == "" {
			continue
		}
		if _, ok := orders[orderID]; ok {
			continue
		}
		if _, skipped := seenMissing[orderID]; skipped {
			continue
		}
		seenMissing[orderID] = struct{}{}
		missingIDs = append(missingIDs, orderID)
	}
	if len(missingIDs) > 0 {
		loaded, err := s.store.ListSupplyOrdersByIDs(ctx, missingIDs)
		if err != nil {
			return nil, err
		}
		for _, order := range loaded {
			if orderID := strings.TrimSpace(order.OrderID); orderID != "" {
				orders[orderID] = order
			}
		}
	}
	return orders, nil
}

func supplyAccountItemFromStore(item store.SupplyImportItem, order store.SupplyOrder, usage supplyAccountUsage, file cpaauthfiles.File, cpaLookupKnown bool, cpaFound bool, now time.Time, issue supplyAccountIssue, recovery supplyAccountRecoveryStatus) SupplyAccountItem {
	source := "unknown"
	product := ""
	orderStatus := ""
	if strings.TrimSpace(order.OrderID) != "" {
		source = reportOrderSource(order)
		product = order.Product
		orderStatus = order.Status
	} else if strings.HasPrefix(item.OrderID, "recovery-") {
		source = "recovery"
	}
	expiresAtMS := supplyAccountExpiryAtMS(file.Raw, item.PayloadJSON, now)
	remainingSeconds := int64(0)
	if expiresAtMS > 0 {
		remainingSeconds = max(0, (expiresAtMS-now.UnixMilli())/1000)
	}
	accountStatus := supplyAccountStatus(item, file, cpaLookupKnown, cpaFound, now)
	accountStatusReason := supplyAccountStatusReason(accountStatus, item, file, cpaLookupKnown, cpaFound, now)
	if issue.Auth401AtMS > 0 {
		accountStatusReason = firstNonEmptyString(issue.Auth401Reason, accountStatusReason, "账号触发 401，已进入隔离/修复流程")
	}
	return SupplyAccountItem{
		ID:                   item.ID,
		FileName:             item.FileName,
		OrderID:              item.OrderID,
		Source:               source,
		Product:              product,
		OrderStatus:          orderStatus,
		Status:               reportKey(item.Status),
		AccountStatus:        accountStatus,
		AccountStatusReason:  accountStatusReason,
		CPAProvider:          file.Provider,
		CPAAccount:           firstNonEmptyString(file.AccountSnapshot, textField(file.Raw, "account", "email", "username", "auth_label")),
		CPAAccountID:         file.AccountID,
		CPAAuthIndex:         file.AuthIndex,
		CPAStatus:            textField(file.Raw, "status", "state"),
		CPADisabled:          file.Disabled,
		UsageCalls:           usage.Calls,
		UsageSuccessCalls:    usage.SuccessCalls,
		UsageFailureCalls:    usage.FailureCalls,
		UsageTokens:          usage.Tokens,
		UsageRevenue:         reportRatioFloat(usage.Revenue, 1),
		UsageRevenueCurrency: "USD",
		SupplierBasePriceFen: item.BasePriceFen,
		SupplierChargedFen:   item.ChargedFen,
		SupplierReleasedFen:  supplyItemReleasedFen(item.BasePriceFen, item.ChargedFen),
		LastUsedAtMS:         usage.LastUsedAtMS,
		ImportedAtMS:         item.ImportedAtMS,
		ExpiresAtMS:          expiresAtMS,
		LeaseExpiresAtMS:     item.LeaseExpiresAtMS,
		WarrantyExpiresAtMS:  item.WarrantyExpiresAtMS,
		RemainingSeconds:     remainingSeconds,
		Auth401AtMS:          issue.Auth401AtMS,
		Auth401BeforeCalls:   usage.SuccessCalls,
		Auth401Reason:        issue.Auth401Reason,
		AutoDisabledAtMS:     issue.AutoDisabledAtMS,
		RecoveryID:           recovery.RecoveryID,
		RecoveryStatus:       recovery.Status,
		AttemptCount:         item.AttemptCount,
		LastError:            item.LastError,
		CreatedAtMS:          item.CreatedAtMS,
		UpdatedAtMS:          item.UpdatedAtMS,
	}
}

func supplyAccountExpiryAtMS(raw map[string]any, payloadJSON string, now time.Time) int64 {
	for _, values := range []map[string]any{raw, supplyAccountPayloadMetadata(payloadJSON)} {
		if len(values) == 0 {
			continue
		}
		for _, key := range []string{"expired", "expires_at", "expiresAt", "valid_until", "validUntil"} {
			value, ok := values[key]
			if !ok || value == nil {
				continue
			}
			if expiry, parsed := parseSmartExpiryTime(value, now); parsed {
				return expiry.UnixMilli()
			}
		}
	}
	return 0
}

func supplyAccountPayloadMetadata(payloadJSON string) map[string]any {
	payloadJSON = strings.TrimSpace(payloadJSON)
	if payloadJSON == "" {
		return nil
	}
	var metadata map[string]any
	if json.Unmarshal([]byte(payloadJSON), &metadata) != nil {
		return nil
	}
	return metadata
}

func supplyAccountStatus(item store.SupplyImportItem, file cpaauthfiles.File, cpaLookupKnown bool, cpaFound bool, now time.Time) string {
	status := reportKey(item.Status)
	if status != "imported" {
		return status
	}
	if expiresAtMS := supplyAccountExpiryAtMS(file.Raw, item.PayloadJSON, now); expiresAtMS > 0 && expiresAtMS <= now.UnixMilli() {
		return "expired"
	}
	if !cpaLookupKnown {
		return "unknown"
	}
	if !cpaFound {
		return "missing"
	}
	if isCPAAuthLifecyclePending(file) {
		return "cooldown"
	}
	if !isAvailableCodexFile(file) {
		return "disabled"
	}
	return "active"
}

func supplyAccountStatusReason(accountStatus string, item store.SupplyImportItem, file cpaauthfiles.File, cpaLookupKnown bool, cpaFound bool, now time.Time) string {
	accountStatus = reportKey(accountStatus)
	switch accountStatus {
	case "active":
		return ""
	case "failed":
		return compactAccountStatusReason(firstNonEmptyString(item.LastError, "账号导入失败"))
	case "pending":
		return "等待导入到 CPA 认证文件"
	case "expired":
		if expiresAtMS := supplyAccountExpiryAtMS(file.Raw, item.PayloadJSON, now); expiresAtMS > 0 {
			return fmt.Sprintf("账号有效期已于 %s 过期", time.UnixMilli(expiresAtMS).In(time.Local).Format("2006-01-02 15:04:05"))
		}
		return "账号有效期已过期"
	case "missing":
		return "CPA 认证文件未找到，可能已被删除或导入未完成"
	case "unknown":
		if !cpaLookupKnown {
			return "CPA 管理连接未配置或认证文件状态暂未获取"
		}
		if !cpaFound {
			return "CPA 认证文件状态未知"
		}
		return "账号状态未知"
	case "disabled":
		if reason := supplyCPAUnavailableReason(file); reason != "" {
			return reason
		}
		return "CPA 账号已停用或当前不可用"
	case "cooldown":
		if reason := supplyCPAUnavailableReason(file); reason != "" {
			return reason
		}
		return "CPA 账号正在初始化或自动恢复"
	default:
		if status := reportKey(item.Status); status != "" && status != "imported" {
			if item.LastError != "" {
				return compactAccountStatusReason(item.LastError)
			}
			return fmt.Sprintf("导入状态为 %s", status)
		}
	}
	return ""
}

func supplyCPAUnavailableReason(file cpaauthfiles.File) string {
	if isCPAAuthLifecyclePending(file) {
		status := strings.ToLower(textField(file.Raw, "status", "state"))
		if reason := cpaStatusReasonField(file.Raw, "status_message", "statusMessage", "last_error", "lastError"); reason != "" {
			return compactAccountStatusReason(reason)
		}
		return fmt.Sprintf("CPA 账号正在自动恢复（%s）", status)
	}
	if reason := cpaStatusReasonField(file.Raw,
		"reason", "disabled_reason", "disabledReason", "disable_reason", "disableReason",
		"status_reason", "statusReason", "last_error", "lastError", "error",
		"error_message", "errorMessage", "status_message", "statusMessage", "message",
		"quota_reason", "quotaReason", "invalid_reason", "invalidReason",
		"error_kind", "errorKind", "header_error_kind", "headerErrorKind",
	); reason != "" {
		return compactAccountStatusReason(reason)
	}
	for _, key := range []string{"disabled", "expired", "revoked", "deleted"} {
		if boolField(file.Raw, key) {
			return fmt.Sprintf("CPA 标记 %s=true", key)
		}
	}
	status := strings.ToLower(textField(file.Raw, "status", "state"))
	switch status {
	case "disabled", "inactive", "invalid", "expired", "revoked", "deleted":
		return fmt.Sprintf("CPA 状态为 %s", status)
	}
	if smartAccountCapacityHardBlocked(file.Raw) {
		if reason := cpaStatusReasonField(file.Raw, "runtime_status", "runtimeStatus", "status", "state", "last_error", "lastError"); reason != "" {
			return compactAccountStatusReason("CPA 硬限制或鉴权失败：" + reason)
		}
		return "CPA 硬限制或鉴权失败"
	}
	if file.Disabled {
		return "CPA 认证文件 disabled=true"
	}
	provider := strings.TrimSpace(file.Provider)
	if provider != "" && !strings.EqualFold(provider, "codex") && !strings.EqualFold(provider, "openai-codex") {
		return fmt.Sprintf("CPA provider=%s，非 codex 账号", provider)
	}
	return ""
}

func cpaStatusReasonField(values map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := values[key]
		if !ok || value == nil {
			continue
		}
		if text := cpaStatusReasonText(value); text != "" {
			return text
		}
	}
	return ""
}

func cpaStatusReasonText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		text := strings.TrimSpace(typed)
		switch strings.ToLower(text) {
		case "", "<nil>", "nil", "null", "none", "false":
			return ""
		default:
			return text
		}
	case json.Number:
		return strings.TrimSpace(typed.String())
	case bool:
		return ""
	case map[string]any:
		return cpaStatusReasonField(typed,
			"reason", "message", "error", "detail", "details", "description",
			"status_reason", "statusReason", "status_message", "statusMessage",
			"error_message", "errorMessage", "last_error", "lastError",
		)
	case []any:
		for _, item := range typed {
			if text := cpaStatusReasonText(item); text != "" {
				return text
			}
		}
		return ""
	default:
		text := strings.TrimSpace(fmt.Sprint(value))
		if text == "<nil>" {
			return ""
		}
		return text
	}
}

func compactAccountStatusReason(reason string) string {
	reason = strings.Join(strings.Fields(strings.TrimSpace(reason)), " ")
	runes := []rune(reason)
	if len(runes) <= 240 {
		return reason
	}
	return string(runes[:240]) + "..."
}

func supplyAccountStatusFromItem(item store.SupplyImportItem, now time.Time) string {
	status := reportKey(item.Status)
	if status != "imported" {
		return status
	}
	if expiresAtMS := supplyAccountExpiryAtMS(nil, item.PayloadJSON, now); expiresAtMS > 0 && expiresAtMS <= now.UnixMilli() {
		return "expired"
	}
	return "imported"
}

func supplyAccountSummaryAdd(summary *SupplyAccountSummary, account SupplyAccountItem, now time.Time) {
	if summary == nil {
		return
	}
	summary.Total++
	switch reportKey(account.Status) {
	case "imported":
		summary.Imported++
	case "pending":
		summary.Pending++
	case "failed":
		summary.Failed++
	}
	switch reportKey(account.AccountStatus) {
	case "active":
		summary.Active++
	case "disabled":
		summary.Disabled++
	case "expired":
		summary.Expired++
	case "missing":
		summary.Missing++
	case "unknown":
		summary.Unknown++
	}
	if account.AccountStatus == "active" && account.ExpiresAtMS > 0 &&
		account.ExpiresAtMS <= now.Add(15*time.Minute).UnixMilli() {
		summary.ExpiringSoon++
	}
	summary.UsageCalls += account.UsageCalls
	summary.UsageSuccessCalls += account.UsageSuccessCalls
	summary.UsageFailureCalls += account.UsageFailureCalls
	summary.UsageTokens += account.UsageTokens
	summary.UsageRevenue += account.UsageRevenue
	if account.LastUsedAtMS > summary.LastUsedAtMS {
		summary.LastUsedAtMS = account.LastUsedAtMS
	}
	if account.Auth401AtMS > 0 {
		summary.Auth401Accounts++
	}
	if account.AutoDisabledAtMS > 0 {
		summary.AutoQuarantined++
	}
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

func buildSupplyReport(req ReportRequest, orders []store.SupplyOrder, recoveries []store.SupplyRecovery, items []store.SupplyImportItem, actionCandidates []model.AccountActionCandidate, now time.Time) Report {
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
	strategyStats := make(map[string]*ReportDimensionStat)
	triggerReasonStats := make(map[string]*ReportDimensionStat)
	orderStatusStats := make(map[string]*ReportDimensionStat)
	recoveryStatusStats := make(map[string]*ReportDimensionStat)
	deliveryStatusStats := make(map[string]*ReportDimensionStat)
	sourceStats := make(map[string]*ReportDimensionStat)

	var orderFulfillmentTotal int64
	var orderFulfillmentSamples int
	var vacuumRecoveryTotal int64
	var vacuumRecoverySamples int
	for _, order := range orders {
		source := reportOrderSource(order)
		strategy := reportKey(firstNonEmptyString(order.Strategy, source))
		triggerReason := reportKey(unwrappedSupplyTriggerReason(order.TriggerReason))
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
		if order.Automatic {
			if supplyTriggerReasonEmergency(triggerReason) {
				report.Executive.EmergencyReplenishments++
			}
			if triggerReason == "virtual_demand_memory" {
				report.Executive.VirtualDemandReplenishments++
			}
			if triggerReason == "emergency_pool_vacuum" {
				report.Executive.VacuumReplenishments++
				vacuumEndMS := order.CompletedAtMS
				if vacuumEndMS <= 0 {
					vacuumEndMS = min(now.UnixMilli(), req.ToMS)
				}
				if order.CreatedAtMS > 0 && vacuumEndMS >= order.CreatedAtMS {
					durationSeconds := (vacuumEndMS - order.CreatedAtMS) / 1000
					report.Executive.VacuumTotalSeconds += durationSeconds
					if order.CompletedAtMS > 0 {
						vacuumRecoveryTotal += durationSeconds
						vacuumRecoverySamples++
					}
				}
			}
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
			reportDimension(strategyStats, strategy),
			reportDimension(triggerReasonStats, triggerReason),
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

	auth401Files := map[string]struct{}{}
	for _, candidate := range actionCandidates {
		if !supplyAccountActionIs401(candidate) {
			continue
		}
		report.Executive.Auth401Events += max(1, candidate.HitCount)
		fileName := strings.TrimSpace(candidate.AuthFileName)
		if fileName != "" {
			auth401Files[fileName] = struct{}{}
		}
		if candidate.AutoDisabledAtMS > 0 {
			report.Executive.AutoQuarantined++
		}
	}
	report.Executive.Auth401Accounts = len(auth401Files)

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
			expiresAtMS := supplyAccountExpiryAtMS(nil, item.PayloadJSON, now)
			if expiresAtMS <= 0 {
				expiresAtMS = item.LeaseExpiresAtMS
			}
			if expiresAtMS > 0 {
				if expiresAtMS <= now.UnixMilli() {
					report.ImportHealth.ExpiredItems++
				} else if expiresAtMS <= now.Add(15*time.Minute).UnixMilli() {
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

	report.Executive.NetFen = report.Executive.ChargedFen - report.Executive.RefundedFen
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
	report.Executive.AverageVacuumRecoverySeconds = reportRatioFloat(float64(vacuumRecoveryTotal), float64(vacuumRecoverySamples))
	report.Timing.AverageRecoveryClaimSeconds = reportRatioFloat(float64(recoveryClaimTotal), float64(recoveryClaimSamples))
	report.Timing.AverageRecoveryImportSeconds = reportRatioFloat(float64(recoveryImportTotal), float64(recoveryImportSamples))
	report.Timing.AverageImportRegistrationSeconds = reportRatioFloat(float64(importRegistrationTotal), float64(importRegistrationSamples))

	report.Timeline = reportTimelinePoints(timeline)
	report.Products = reportDimensionStats(productStats)
	report.Strategies = reportDimensionStats(strategyStats)
	report.TriggerReasons = reportDimensionStats(triggerReasonStats)
	report.OrderStatuses = reportDimensionStats(orderStatusStats)
	report.RecoveryStatuses = reportDimensionStats(recoveryStatusStats)
	report.DeliveryStatuses = reportDimensionStats(deliveryStatusStats)
	report.Sources = reportDimensionStats(sourceStats)
	return report
}

func applyUsageRevenueToReport(report *Report, stats []store.ModelStat, timeline []store.TimelinePoint, prices map[string]store.ModelPrice, revenueMultiplier float64) {
	if report == nil {
		return
	}
	models := make([]ReportUsageModelStat, 0, len(stats))
	for _, stat := range stats {
		revenue := reportCostForStat(stat, prices, revenueMultiplier)
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
		report.Timeline[index].UsageRevenue += reportCostForTimelinePoint(point, prices, revenueMultiplier)
	}
	for i := range report.Timeline {
		report.Timeline[i].UsageRevenue = reportRatioFloat(report.Timeline[i].UsageRevenue, 1)
	}
	sort.Slice(report.Timeline, func(i, j int) bool { return report.Timeline[i].BucketMS < report.Timeline[j].BucketMS })
}

func buildReportReconciliation(req ReportRequest, orders []store.SupplyOrder, recoveries []store.SupplyRecovery, items []store.SupplyImportItem, orderLookup map[string]store.SupplyOrder, usageByFile map[string]supplyAccountUsage, issuesByFile map[string]supplyAccountIssue, now time.Time) ReportReconciliation {
	reconciliation := ReportReconciliation{
		Summary: ReportReconciliationSummary{
			UsageRevenueCurrency: "USD",
			AllocationMethod:     "order_even_split_by_visible_accounts",
		},
		Orders:     make([]ReportOrderLedgerRow, 0, len(orders)),
		Accounts:   make([]ReportAccountLedgerRow, 0, len(items)),
		Recoveries: make([]ReportRecoveryLedgerRow, 0, len(recoveries)),
	}
	for _, order := range orders {
		row := ReportOrderLedgerRow{
			OrderID:           order.OrderID,
			Source:            reportOrderSource(order),
			Strategy:          order.Strategy,
			TriggerReason:     order.TriggerReason,
			Product:           order.Product,
			Status:            reportKey(order.Status),
			RequestedQuantity: order.RequestedQuantity,
			ItemCount:         order.ItemCount,
			ImportedCount:     order.ImportedCount,
			ChargedFen:        order.ChargedFen,
			ReleasedFen:       order.ReleasedFen,
			NetFen:            order.ChargedFen,
			CreatedAtMS:       order.CreatedAtMS,
			CompletedAtMS:     order.CompletedAtMS,
		}
		reconciliation.Orders = append(reconciliation.Orders, row)
		reconciliation.Summary.OrderChargedFen += row.ChargedFen
		reconciliation.Summary.OrderReleasedFen += row.ReleasedFen
		reconciliation.Summary.OrderNetFen += row.NetFen
	}

	accountIndexesByOrder := make(map[string][]int)
	type usageVersion struct {
		id              int64
		effectiveFromMS int64
	}
	usageVersionByFile := make(map[string]usageVersion)
	for _, item := range items {
		if !strings.EqualFold(strings.TrimSpace(item.Status), "imported") {
			continue
		}
		fileName := strings.TrimSpace(item.FileName)
		if fileName == "" {
			continue
		}
		effectiveFromMS := reportFirstPositiveMS(item.EffectiveFromMS, item.ImportedAtMS, item.CreatedAtMS)
		if effectiveFromMS >= req.ToMS || (item.SupersededAtMS > 0 && item.SupersededAtMS <= req.FromMS) {
			continue
		}
		current, found := usageVersionByFile[fileName]
		if !found || effectiveFromMS > current.effectiveFromMS || (effectiveFromMS == current.effectiveFromMS && item.ID > current.id) {
			usageVersionByFile[fileName] = usageVersion{id: item.ID, effectiveFromMS: effectiveFromMS}
		}
	}
	for _, item := range items {
		order := orderLookup[item.OrderID]
		source := "unknown"
		product := ""
		if strings.TrimSpace(order.OrderID) != "" {
			source = reportOrderSource(order)
			product = order.Product
		} else if strings.HasPrefix(item.OrderID, "recovery-") {
			source = "recovery"
		}
		fileName := strings.TrimSpace(item.FileName)
		usage := supplyAccountUsage{}
		if usageVersionByFile[fileName].id == item.ID {
			usage = usageByFile[fileName]
		}
		issue := issuesByFile[strings.TrimSpace(item.FileName)]
		if effectiveFrom := max(item.EffectiveFromMS, item.ImportedAtMS); effectiveFrom > 0 && issue.Auth401AtMS < effectiveFrom {
			issue = supplyAccountIssue{}
		}
		row := ReportAccountLedgerRow{
			FileName:             item.FileName,
			OrderID:              item.OrderID,
			Source:               source,
			Product:              product,
			Status:               reportKey(item.Status),
			AccountStatus:        supplyAccountStatusFromItem(item, now),
			ImportedAtMS:         item.ImportedAtMS,
			ExpiresAtMS:          supplyAccountExpiryAtMS(nil, item.PayloadJSON, now),
			LeaseExpiresAtMS:     item.LeaseExpiresAtMS,
			WarrantyExpiresAtMS:  item.WarrantyExpiresAtMS,
			SupplierBasePriceFen: item.BasePriceFen,
			SupplierChargedFen:   item.ChargedFen,
			SupplierReleasedFen:  supplyItemReleasedFen(item.BasePriceFen, item.ChargedFen),
			UsageCalls:           usage.Calls,
			UsageSuccessCalls:    usage.SuccessCalls,
			UsageFailureCalls:    usage.FailureCalls,
			UsageTokens:          usage.Tokens,
			UsageRevenue:         reportRatioFloat(usage.Revenue, 1),
			LastUsedAtMS:         usage.LastUsedAtMS,
			Auth401AtMS:          issue.Auth401AtMS,
			AutoDisabledAtMS:     issue.AutoDisabledAtMS,
		}
		reconciliation.Accounts = append(reconciliation.Accounts, row)
		index := len(reconciliation.Accounts) - 1
		if strings.TrimSpace(item.OrderID) != "" {
			accountIndexesByOrder[item.OrderID] = append(accountIndexesByOrder[item.OrderID], index)
		}
		reconciliation.Summary.AccountUsageCalls += row.UsageCalls
		reconciliation.Summary.AccountUsageTokens += row.UsageTokens
		reconciliation.Summary.AccountUsageRevenue += row.UsageRevenue
	}
	for orderID, indexes := range accountIndexesByOrder {
		order, ok := orderLookup[orderID]
		if !ok || len(indexes) == 0 {
			continue
		}
		if applyExactSupplierAccountCosts(&reconciliation, indexes) {
			continue
		}
		chargedParts := splitFenEvenly(order.ChargedFen, len(indexes))
		releasedParts := splitFenEvenly(order.ReleasedFen, len(indexes))
		for i, index := range indexes {
			reconciliation.Accounts[index].AllocatedChargedFen = chargedParts[i]
			reconciliation.Accounts[index].AllocatedReleasedFen = releasedParts[i]
			reconciliation.Accounts[index].AllocatedNetFen = chargedParts[i]
			reconciliation.Summary.AccountAllocatedChargedFen += chargedParts[i]
			reconciliation.Summary.AccountAllocatedReleasedFen += releasedParts[i]
			reconciliation.Summary.AccountAllocatedNetFen += chargedParts[i]
		}
	}
	reconciliation.Summary.AccountUsageRevenue = reportRatioFloat(reconciliation.Summary.AccountUsageRevenue, 1)

	for _, recovery := range recoveries {
		row := ReportRecoveryLedgerRow{
			RecoveryID:       recovery.RecoveryID,
			Product:          recovery.Product,
			DeliveryStatus:   reportKey(recovery.DeliveryStatus),
			Status:           reportKey(recovery.Status),
			OriginalFileName: recovery.OriginalFileName,
			ClaimOrderID:     recovery.ClaimOrderID,
			ItemCount:        recovery.ItemCount,
			ImportedCount:    recovery.ImportedCount,
			RefundedFen:      recovery.RefundedFen,
			LastSeenAtMS:     recovery.LastSeenAtMS,
			ClaimedAtMS:      recovery.ClaimedAtMS,
			UpdatedAtMS:      recovery.UpdatedAtMS,
		}
		reconciliation.Recoveries = append(reconciliation.Recoveries, row)
		reconciliation.Summary.RefundedFen += row.RefundedFen
	}
	reconciliation.Summary.OrderRows = len(reconciliation.Orders)
	reconciliation.Summary.AccountRows = len(reconciliation.Accounts)
	reconciliation.Summary.RecoveryRows = len(reconciliation.Recoveries)
	return reconciliation
}

func applyExactSupplierAccountCosts(reconciliation *ReportReconciliation, indexes []int) bool {
	if reconciliation == nil || len(indexes) == 0 {
		return false
	}
	for _, index := range indexes {
		if index < 0 || index >= len(reconciliation.Accounts) {
			return false
		}
		row := reconciliation.Accounts[index]
		if row.SupplierBasePriceFen <= 0 && row.SupplierChargedFen <= 0 {
			return false
		}
	}
	for _, index := range indexes {
		row := &reconciliation.Accounts[index]
		row.AllocatedChargedFen = row.SupplierChargedFen
		row.AllocatedReleasedFen = row.SupplierReleasedFen
		row.AllocatedNetFen = row.SupplierChargedFen
		reconciliation.Summary.AccountAllocatedChargedFen += row.AllocatedChargedFen
		reconciliation.Summary.AccountAllocatedReleasedFen += row.AllocatedReleasedFen
		reconciliation.Summary.AccountAllocatedNetFen += row.AllocatedNetFen
	}
	if reconciliation.Summary.AllocationMethod == "order_even_split_by_visible_accounts" {
		reconciliation.Summary.AllocationMethod = "supplier_item_exact_else_order_even_split"
	}
	return true
}

func supplyItemReleasedFen(basePriceFen int64, chargedFen int64) int64 {
	if basePriceFen <= chargedFen {
		return 0
	}
	return basePriceFen - chargedFen
}

func splitFenEvenly(total int64, parts int) []int64 {
	if parts <= 0 {
		return nil
	}
	values := make([]int64, parts)
	base := total / int64(parts)
	remainder := total % int64(parts)
	for i := range values {
		values[i] = base
		if remainder > 0 {
			values[i]++
			remainder--
		} else if remainder < 0 {
			values[i]--
			remainder++
		}
	}
	return values
}

func mergeSupplyImportItems(groups ...[]store.SupplyImportItem) []store.SupplyImportItem {
	merged := make([]store.SupplyImportItem, 0)
	seen := make(map[string]int)
	for _, group := range groups {
		for _, item := range group {
			key := supplyImportItemMergeKey(item)
			if key == "" {
				continue
			}
			if index, ok := seen[key]; ok {
				merged[index] = richerSupplyImportItem(merged[index], item)
				continue
			}
			seen[key] = len(merged)
			merged = append(merged, item)
		}
	}
	return merged
}

func supplyImportItemMergeKey(item store.SupplyImportItem) string {
	if item.ID > 0 {
		return "id:" + strconv.FormatInt(item.ID, 10)
	}
	if strings.TrimSpace(item.OrderID) != "" && strings.TrimSpace(item.ItemKey) != "" {
		return "order:" + strings.TrimSpace(item.OrderID) + ":" + strings.TrimSpace(item.ItemKey)
	}
	if strings.TrimSpace(item.FileName) != "" {
		return "file:" + strings.TrimSpace(item.FileName)
	}
	return ""
}

func richerSupplyImportItem(current store.SupplyImportItem, candidate store.SupplyImportItem) store.SupplyImportItem {
	if strings.TrimSpace(current.OrderID) == "" && strings.TrimSpace(candidate.OrderID) != "" {
		current.OrderID = candidate.OrderID
	}
	if strings.TrimSpace(current.ItemKey) == "" && strings.TrimSpace(candidate.ItemKey) != "" {
		current.ItemKey = candidate.ItemKey
	}
	if strings.TrimSpace(current.FileName) == "" && strings.TrimSpace(candidate.FileName) != "" {
		current.FileName = candidate.FileName
	}
	if strings.TrimSpace(current.Status) == "" && strings.TrimSpace(candidate.Status) != "" {
		current.Status = candidate.Status
	}
	if current.ImportedAtMS <= 0 && candidate.ImportedAtMS > 0 {
		current.ImportedAtMS = candidate.ImportedAtMS
	}
	if current.LeaseExpiresAtMS <= 0 && candidate.LeaseExpiresAtMS > 0 {
		current.LeaseExpiresAtMS = candidate.LeaseExpiresAtMS
	}
	if current.WarrantyExpiresAtMS <= 0 && candidate.WarrantyExpiresAtMS > 0 {
		current.WarrantyExpiresAtMS = candidate.WarrantyExpiresAtMS
	}
	if current.CreatedAtMS <= 0 && candidate.CreatedAtMS > 0 {
		current.CreatedAtMS = candidate.CreatedAtMS
	}
	if candidate.UpdatedAtMS > current.UpdatedAtMS {
		current.UpdatedAtMS = candidate.UpdatedAtMS
	}
	return current
}

func reportCostForStat(stat store.ModelStat, prices map[string]store.ModelPrice, revenueMultiplier float64) float64 {
	return reportApplyRevenueMultiplier(pricing.CostForModelCandidatesWithServiceTier([]string{stat.BillingModel, stat.Model}, stat.ServiceTier, pricing.ModelTokens{
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
	}, prices), revenueMultiplier)
}

func reportCostForCredentialStat(stat store.CredentialModelStat, prices map[string]store.ModelPrice, revenueMultiplier float64) float64 {
	return reportApplyRevenueMultiplier(pricing.CostForModelCandidatesWithServiceTier([]string{stat.BillingModel, stat.Model}, stat.ServiceTier, pricing.ModelTokens{
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
	}, prices), revenueMultiplier)
}

func reportCostForTimelinePoint(point store.TimelinePoint, prices map[string]store.ModelPrice, revenueMultiplier float64) float64 {
	return reportApplyRevenueMultiplier(pricing.CostForModelCandidatesWithServiceTier([]string{point.BillingModel, point.Model}, point.ServiceTier, pricing.ModelTokens{
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
	}, prices), revenueMultiplier)
}

func reportApplyRevenueMultiplier(value float64, revenueMultiplier float64) float64 {
	return value * supplyRevenueMultiplier(store.ManagerSupplyConfig{RevenueMultiplier: revenueMultiplier})
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

func supplyTriggerReasonEmergency(reason string) bool {
	reason = reportKey(reason)
	return strings.Contains(reason, "emergency") ||
		strings.Contains(reason, "critical") ||
		reason == "virtual_demand_memory"
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
	selection, err := s.selectSupplyPlatform(ctx, cfg, quantity, nil)
	if err != nil {
		return supplyclient.Inventory{}, supplyclient.Balance{}, err
	}
	return *selection.status.Inventory, *selection.status.Balance, nil
}

func (s *Service) syncRecoveriesOnce(ctx context.Context, cfg store.ManagerConfig, autoClaim bool, limit int, recoveryID string) (RecoverySyncResult, error) {
	var result RecoverySyncResult
	var firstErr error
	if err := s.backfillSupplyAccountMetadata(ctx, cfg); err != nil {
		firstErr = err
	}
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
	platforms := recoverySupplyPlatforms(cfg.Supply)
	if len(platforms) == 0 {
		err := errors.New("no recovery-capable supply platform is configured")
		if firstErr != nil {
			return result, errors.Join(firstErr, err)
		}
		return result, err
	}
	localRecoveries := make([]store.SupplyRecovery, 0)
	for _, platform := range platforms {
		remoteRecoveries, err := s.supplyClient.Recoveries(ctx, supplyPlatformCredentials(platform))
		if err != nil {
			platformErr := fmt.Errorf("recovery platform %s: %w", firstNonEmptyString(platform.Name, platform.ID), err)
			if firstErr == nil {
				firstErr = platformErr
			} else {
				firstErr = errors.Join(firstErr, platformErr)
			}
			continue
		}
		for _, remote := range remoteRecoveries {
			local := supplyRecoveryFromClient(remote, platform.ID)
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
			if retryErr := s.markRecoveryClaimRetry(ctx, recovery, err); retryErr != nil && firstErr == nil {
				firstErr = retryErr
			} else if firstErr == nil {
				firstErr = err
			}
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
	platform, err := recoverySupplyPlatform(cfg.Supply, recovery.SupplierID)
	if err != nil {
		return err
	}
	claimed, err := s.supplyClient.ClaimRecovery(
		ctx,
		supplyPlatformCredentials(platform),
		recovery.RecoveryID,
		recovery.ClaimURL,
		recovery.ClaimTicket,
	)
	if err != nil {
		return err
	}
	if claimed.CredentialVersion <= 1 {
		return fmt.Errorf("recovery claim returned invalid credential_version %d", claimed.CredentialVersion)
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
	product := firstNonEmptyString(recovery.Product, cfg.Supply.Product)
	if product == "" {
		product = "oauth_30d"
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
			AccountName:      account.accountName,
			NameKey:          account.nameKey,
			FileName:         account.fileName,
			PayloadJSON:      string(account.payload),
			LeaseExpiresAtMS: account.leaseExpiresAtMS,
		})
	}
	claimedAtMS := time.Now().UnixMilli()
	recovery.CredentialVersion = max(recovery.CredentialVersion, claimed.CredentialVersion)
	recovery.Status = "importing"
	recovery.DeliveryStatus = firstNonEmptyString(claimed.Recovery.DeliveryStatus, "claimed")
	recovery.ClaimOrderID = orderID
	recovery.ItemCount = len(items)
	recovery.LastSeenAtMS = claimedAtMS
	order := store.SupplyOrder{
		OrderID:           orderID,
		SupplierID:        firstNonEmptyString(recovery.SupplierID, platform.ID),
		Product:           product,
		RequestedQuantity: len(items),
		Automatic:         true,
		Strategy:          "recovery",
		TriggerReason:     "recovery_claimed",
		Status:            "recovery_importing",
		RemoteStatus:      "recovery_claimed",
		ItemCount:         len(items),
	}
	// The supplier ticket is consumed before local persistence. Keep the local
	// order, encrypted payloads, and recovery state in one retried transaction
	// so SQLite writer contention cannot leave a claimed replacement without an
	// import task.
	if err := s.store.PersistSupplyRecoveryClaim(ctx, recovery, order, items, claimedAtMS); err != nil {
		return err
	}
	return nil
}

func (s *Service) markRecoveryClaimRetry(ctx context.Context, recovery store.SupplyRecovery, claimErr error) error {
	recovery.Status = "claimable"
	if isHTTPStatus(claimErr, http.StatusConflict) {
		// A 409 means the signed URL is stale or already consumed. Keep the
		// record visible but wait for the next recovery listing to issue a new URL.
		recovery.Status = "seen"
	}
	recovery.LastError = safeError(claimErr)
	recovery.LastSeenAtMS = time.Now().UnixMilli()
	_, err := s.store.UpsertSupplyRecoveries(ctx, []store.SupplyRecovery{recovery})
	return err
}

func recoveryFileComponent(value string) string {
	var builder strings.Builder
	for _, r := range strings.TrimSpace(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	result := strings.Trim(builder.String(), "-_")
	if result == "" {
		return "unknown"
	}
	if len(result) > 40 {
		return result[:40]
	}
	return result
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
		imported, failed, err := s.processRecoveryImport(ctx, cfg, recovery)
		if imported {
			importedRecoveries++
		}
		if failed {
			failedRecoveries++
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return importedRecoveries, failedRecoveries, firstErr
}

func (s *Service) processRecoveryImport(ctx context.Context, cfg store.ManagerConfig, recovery store.SupplyRecovery) (bool, bool, error) {
	orderID := strings.TrimSpace(recovery.ClaimOrderID)
	if orderID == "" {
		// Older builds could persist the local order and encrypted import item,
		// then lose only the final recovery link to a SQLite writer conflict.
		// Recover that deterministic link without claiming the supplier ticket a
		// second time.
		orderID = recoveryOrderID(recovery.RecoveryID)
	}
	order, found, err := s.store.GetSupplyOrder(ctx, orderID)
	if err != nil {
		return false, true, err
	}
	if !found {
		if strings.TrimSpace(recovery.ClaimOrderID) == "" {
			return false, false, nil
		}
		err := fmt.Errorf("recovery import order %s was not found", orderID)
		_ = s.store.MarkSupplyRecoveryFailed(ctx, recovery.RecoveryID, safeError(err))
		return false, true, err
	}
	if strings.TrimSpace(recovery.ClaimOrderID) == "" {
		total, _, countsErr := s.store.SupplyImportCounts(ctx, orderID)
		if countsErr != nil {
			return false, true, countsErr
		}
		if total == 0 {
			return false, false, nil
		}
		if err := s.store.MarkSupplyRecoveryClaimed(ctx, recovery.RecoveryID, orderID, total, time.Now().UnixMilli()); err != nil {
			return false, true, err
		}
		recovery.ClaimOrderID = orderID
		recovery.ItemCount = total
	}
	err = s.importItems(ctx, cfg, &order)
	total, imported, countsErr := s.store.SupplyImportCounts(ctx, orderID)
	if countsErr != nil {
		return false, true, countsErr
	}
	if total > 0 && imported == total {
		if markErr := s.store.MarkSupplyRecoveryImported(ctx, recovery.RecoveryID, imported); markErr != nil {
			return false, true, markErr
		}
		if disableErr := s.disableRecoveredOriginal(ctx, cfg, recovery); disableErr != nil {
			message := "original account disable failed: " + safeError(disableErr)
			_ = s.store.SetSupplyRecoveryLastError(ctx, recovery.RecoveryID, message)
			return true, true, errors.New(message)
		}
		return true, false, nil
	}
	if settled, settlementError, settleErr := s.recoveryImportFailuresSettled(ctx, cfg, order, time.Now()); settleErr != nil {
		return false, true, settleErr
	} else if settled {
		order.Status = "failed"
		order.CompletedAtMS = time.Now().UnixMilli()
		order.NextPollAtMS = 0
		order.LastError = settlementError
		if updateErr := s.store.UpdateSupplyOrder(ctx, order); updateErr != nil {
			return false, true, updateErr
		}
		if markErr := s.store.MarkSupplyRecoveryFailed(ctx, recovery.RecoveryID, settlementError); markErr != nil {
			return false, true, markErr
		}
		return false, true, nil
	}
	message := ""
	if err != nil {
		message = safeError(err)
	}
	if markErr := s.store.MarkSupplyRecoveryImportProgress(ctx, recovery.RecoveryID, total, imported, message); markErr != nil {
		return false, true, markErr
	}
	if err != nil {
		return false, true, err
	}
	return false, false, nil
}

// recoveryImportFailuresSettled closes a consumed recovery ticket once every
// remaining replacement credential has reached a terminal lifecycle failure.
// Without this gate, quota-exhausted replacement accounts are uploaded and
// initialized forever, keeping the recovery worker busy and repeatedly
// disturbing the live account snapshot.
func (s *Service) recoveryImportFailuresSettled(ctx context.Context, cfg store.ManagerConfig, order store.SupplyOrder, now time.Time) (bool, string, error) {
	if s == nil || s.store == nil || !strings.HasPrefix(order.OrderID, "recovery-") {
		return false, "", nil
	}
	items, err := s.store.ListSupplyImportItemsByOrderIDs(ctx, []string{order.OrderID})
	if err != nil {
		return false, "", err
	}
	terminalFailures := 0
	lastError := ""
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item.Status), "imported") {
			continue
		}
		if !terminalSupplyImportItem(item, now) {
			return false, "", nil
		}
		terminalFailures++
		if strings.TrimSpace(item.LastError) != "" {
			lastError = strings.TrimSpace(item.LastError)
		}
		if strings.TrimSpace(cfg.CPAConnection.CPABaseURL) != "" && strings.TrimSpace(cfg.CPAConnection.ManagementKey) != "" && safeSupplyAuthFileName(item.FileName) {
			if disableErr := s.authFiles.PatchDisabled(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey, item.FileName, true, ""); disableErr != nil && !errors.Is(disableErr, cpaauthfiles.ErrAuthFileNotFound) {
				return false, "", fmt.Errorf("disable terminal recovery auth file %q: %w", item.FileName, disableErr)
			}
		}
	}
	if terminalFailures == 0 {
		return false, "", nil
	}
	message := fmt.Sprintf("recovery delivery settled with %d unusable account(s)", terminalFailures)
	if lastError != "" {
		message += ": " + lastError
	}
	return true, message, nil
}

func (s *Service) disableRecoveredOriginal(ctx context.Context, cfg store.ManagerConfig, recovery store.SupplyRecovery) error {
	if !recoveryDisableOriginalEnabled(cfg.Supply) || strings.TrimSpace(recovery.OriginalFileName) == "" ||
		strings.TrimSpace(cfg.CPAConnection.CPABaseURL) == "" || strings.TrimSpace(cfg.CPAConnection.ManagementKey) == "" {
		return nil
	}
	claimOrderID := firstNonEmptyString(recovery.ClaimOrderID, recoveryOrderID(recovery.RecoveryID))
	items, err := s.store.ListSupplyImportItemsByOrderIDs(ctx, []string{claimOrderID})
	if err != nil {
		return err
	}
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item.FileName), strings.TrimSpace(recovery.OriginalFileName)) {
			return nil
		}
	}
	return s.authFiles.PatchDisabled(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey,
		recovery.OriginalFileName, true, recovery.OriginalAuthIndex)
}

func (s *Service) countAvailableAccounts(ctx context.Context, cfg store.ManagerConfig) (int, error) {
	stats, err := s.countAccountPoolStats(ctx, cfg)
	return stats.schedulable, err
}

type accountPoolStats struct {
	files                       []cpaauthfiles.File
	total                       int
	enabled                     int
	schedulable                 int
	verifiedAvailable           int
	operatorUsable              int
	normal                      int
	needsAttention              int
	quotaRisk                   int
	unconfirmed                 int
	concurrencyLimited          int
	concurrencyUnlimited        int
	concurrencyMissing          int
	concurrencyFiniteSlots      int
	classificationObserved      bool
	liveObserved                bool
	inspectionObserved          bool
	bucketByCredential          map[string]operatorAccountBucket
	temporaryLimitByCredential  map[string]operatorAccountTemporaryLimit
	normalRemainingByCredential map[string]float64
	operatorUsableByFile        []bool
}

type operatorAccountTemporaryLimit struct {
	observed    bool
	kind        string
	code        string
	recoverAtMS int64
}

func (s *Service) countAccountPoolStats(ctx context.Context, cfg store.ManagerConfig) (accountPoolStats, error) {
	if strings.TrimSpace(cfg.CPAConnection.CPABaseURL) == "" || strings.TrimSpace(cfg.CPAConnection.ManagementKey) == "" {
		return accountPoolStats{}, errors.New("CPA connection is not configured")
	}
	files := make([]cpaauthfiles.File, 0)
	err := s.authFiles.Visit(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey, func(file cpaauthfiles.File) (bool, error) {
		files = append(files, file)
		return false, nil
	})
	return accountPoolStatsFromFiles(files), err
}

// countAccountPoolStatsWithInspection deliberately keeps capacity and operator
// classification separate. The current CPA auth-file set defines the live
// population, countOperatorAccountPoolStats defines the status cards, and the
// matching SmartResource inspection remains the conservative capacity source.
func (s *Service) countAccountPoolStatsWithInspection(
	ctx context.Context,
	cfg store.ManagerConfig,
	resource SmartResource,
) (accountPoolStats, error) {
	stats, liveErr := s.countOperatorAccountPoolStats(ctx, cfg)
	return reconcileAccountPoolStatsWithInspection(stats, resource), liveErr
}

func (s *Service) countOperatorAccountPoolStats(
	ctx context.Context,
	cfg store.ManagerConfig,
) (accountPoolStats, error) {
	if s == nil {
		return accountPoolStats{}, errors.New("supply service is unavailable")
	}
	now := time.Now()
	cacheKey := strings.TrimSpace(cfg.CPAConnection.CPABaseURL) + "\x00" + strings.TrimSpace(cfg.CPAConnection.ManagementKey)
	s.operatorPoolMu.Lock()
	cached := s.operatorPool
	generation := s.operatorPoolGeneration
	s.operatorPoolMu.Unlock()
	cacheMatches := cached.key == cacheKey && !cached.generated.IsZero()
	cacheCurrent := cacheMatches && cached.generation == generation
	if cacheCurrent && now.Sub(cached.generated) <= operatorAccountPoolTTL {
		return cached.stats, cached.err
	}
	// Once a complete snapshot exists, only one caller pays for refreshing it.
	// Polling requests arriving behind that refresh get the last complete view
	// immediately instead of waiting on CPA and SQLite reads until their HTTP
	// deadlines expire.
	if cacheCurrent {
		if !s.operatorPoolRefreshMu.TryLock() {
			return cached.stats, cached.err
		}
	} else {
		s.operatorPoolRefreshMu.Lock()
	}
	defer s.operatorPoolRefreshMu.Unlock()

	// A caller may have completed the refresh while this request was waiting for
	// the single-flight lock. Re-read both the cache and its invalidation epoch.
	now = time.Now()
	s.operatorPoolMu.Lock()
	cached = s.operatorPool
	refreshGeneration := s.operatorPoolGeneration
	s.operatorPoolMu.Unlock()
	cacheMatches = cached.key == cacheKey && !cached.generated.IsZero()
	if cacheMatches && cached.generation == refreshGeneration && now.Sub(cached.generated) <= operatorAccountPoolTTL {
		return cached.stats, cached.err
	}

	stats, err := s.loadOperatorAccountPoolStats(ctx, cfg)
	if err == nil || stats.liveObserved {
		s.operatorPoolMu.Lock()
		if s.operatorPoolGeneration == refreshGeneration {
			s.operatorPool = operatorAccountPoolCache{
				generation: refreshGeneration,
				key:        cacheKey,
				generated:  time.Now(),
				stats:      stats,
				err:        err,
			}
		}
		s.operatorPoolMu.Unlock()
		return stats, err
	}
	if cacheMatches {
		return cached.stats, err
	}
	return stats, err
}

func (s *Service) countOperatorAccountPoolStatsForDashboard(
	ctx context.Context,
	cfg store.ManagerConfig,
) (accountPoolStats, error) {
	if s == nil {
		return accountPoolStats{}, errors.New("supply service is unavailable")
	}
	now := time.Now()
	cacheKey := strings.TrimSpace(cfg.CPAConnection.CPABaseURL) + "\x00" + strings.TrimSpace(cfg.CPAConnection.ManagementKey)
	s.operatorPoolMu.Lock()
	cached := s.operatorPool
	generation := s.operatorPoolGeneration
	s.operatorPoolMu.Unlock()
	cacheMatches := cached.key == cacheKey && !cached.generated.IsZero()
	if !cacheMatches {
		return s.countOperatorAccountPoolStats(ctx, cfg)
	}
	if cached.generation == generation && now.Sub(cached.generated) <= operatorAccountPoolTTL {
		return cached.stats, cached.err
	}
	if s.operatorPoolAsyncMu.TryLock() {
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), operatorAccountPoolTimeout)
		go func() {
			defer s.operatorPoolAsyncMu.Unlock()
			defer cancel()
			if _, err := s.countOperatorAccountPoolStats(refreshCtx, cfg); err != nil {
				log.Printf("[supply] background account-pool refresh failed: %v", err)
			}
		}()
	}
	return cached.stats, cached.err
}

func (s *Service) loadOperatorAccountPoolStats(
	ctx context.Context,
	cfg store.ManagerConfig,
) (accountPoolStats, error) {
	if strings.TrimSpace(cfg.CPAConnection.CPABaseURL) == "" || strings.TrimSpace(cfg.CPAConnection.ManagementKey) == "" {
		return accountPoolStats{}, errors.New("CPA connection is not configured")
	}
	authCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	snapshot, liveErr := s.cachedAuthFiles(authCtx, cfg, false)
	cancel()
	if liveErr != nil && len(snapshot.files) == 0 {
		return accountPoolStats{}, liveErr
	}
	stats := accountPoolStatsFromFiles(snapshot.files)
	if !stats.liveObserved {
		return stats, liveErr
	}

	inspectionCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	inspectionSnapshot, inspectionErr := s.cachedInspectionQuotaSnapshot(inspectionCtx, cfg.Supply, false)
	cancel()
	now := time.Now()
	headerCtx, cancelHeaders := context.WithTimeout(ctx, 2*time.Second)
	headers, _ := s.cachedOperatorHeaderSnapshots(headerCtx, len(stats.files), now)
	cancelHeaders()
	// SQLite evidence reads can briefly lose the writer race on a busy pool.
	// The live CPA auth-file snapshot is still authoritative for whether a
	// credential is enabled and schedulable, so continue through the shared
	// classifier even when both persisted evidence sources are temporarily
	// unavailable. That classifier keeps raw auth errors actionable, preserves
	// genuinely unschedulable unknowns as unconfirmed, and treats live
	// schedulable credentials missing from the selected evidence as available.
	// Returning the bare file stats here made every enabled account flash as
	// unconfirmed during a transient SQLITE_BUSY window.
	results := inspectionSnapshot.results
	triggerType := inspectionSnapshot.run.TriggerType
	if inspectionErr != nil {
		results = nil
		triggerType = ""
	} else if !operatorInspectionAuthoritative(triggerType) {
		// Supply snapshots are optimized for capacity planning and can be newer
		// than the scheduled/manual health run. Using them as the sole operator
		// truth lets an old quota header hide a later 401/402 finding. Prefer the
		// latest completed authoritative run for list classification; live headers
		// may still supersede it when they carry newer evidence.
		authoritativeCtx, cancelAuthoritative := context.WithTimeout(ctx, 2*time.Second)
		authoritativeResults, authoritativeTrigger, authoritativeErr := s.loadLatestOperatorInspectionEvidence(authoritativeCtx)
		cancelAuthoritative()
		if authoritativeErr == nil {
			results = authoritativeResults
			triggerType = authoritativeTrigger
		}
	}
	stats = accountPoolStatsFromFilesAndCurrentEvidence(
		stats.files,
		results,
		headers,
		triggerType,
		now,
	)
	return stats, liveErr
}

func (s *Service) loadLatestOperatorInspectionEvidence(ctx context.Context) ([]store.CodexInspectionResult, string, error) {
	if s == nil || s.store == nil {
		return nil, "", ErrCapacitySnapshotUnavailable
	}
	runs, err := s.store.ListCodexInspectionRuns(ctx, 50)
	if err != nil {
		return nil, "", err
	}
	for _, run := range runs {
		if run.Status != model.CodexInspectionStatusCompleted || !operatorInspectionAuthoritative(run.TriggerType) {
			continue
		}
		results, err := s.store.ListCodexInspectionResults(ctx, run.ID)
		if err != nil {
			return nil, "", err
		}
		filtered := make([]store.CodexInspectionResult, 0, len(results))
		for _, result := range results {
			if isSmartCapacityInspectionResult(result) {
				filtered = append(filtered, result)
			}
		}
		if len(filtered) == 0 {
			continue
		}
		return filtered, run.TriggerType, nil
	}
	return nil, "", ErrCapacitySnapshotUnavailable
}

func (s *Service) cachedOperatorHeaderSnapshots(
	ctx context.Context,
	fileCount int,
	now time.Time,
) ([]store.HeaderSnapshot, error) {
	if s == nil || s.store == nil {
		return nil, ErrCapacitySnapshotUnavailable
	}
	limit := max(1000, fileCount*2)
	s.operatorHeadersMu.Lock()
	defer s.operatorHeadersMu.Unlock()
	if len(s.operatorHeaders.items) > 0 && s.operatorHeaders.limit >= limit &&
		now.Sub(s.operatorHeaders.generated) <= operatorHeaderCacheTTL {
		return append([]store.HeaderSnapshot(nil), s.operatorHeaders.items...), nil
	}
	items, err := s.store.LatestHeaderSnapshots(
		ctx,
		now.Add(-30*24*time.Hour).UnixMilli(),
		limit,
	)
	if err != nil {
		if len(s.operatorHeaders.items) > 0 {
			return append([]store.HeaderSnapshot(nil), s.operatorHeaders.items...), err
		}
		return nil, err
	}
	s.operatorHeaders = operatorHeaderSnapshotCache{
		generated: now,
		limit:     limit,
		items:     append([]store.HeaderSnapshot(nil), items...),
	}
	return items, nil
}

func accountPoolStatsFromFiles(files []cpaauthfiles.File) accountPoolStats {
	stats := accountPoolStats{
		files:                       files,
		liveObserved:                true,
		bucketByCredential:          make(map[string]operatorAccountBucket),
		temporaryLimitByCredential:  make(map[string]operatorAccountTemporaryLimit),
		normalRemainingByCredential: make(map[string]float64),
		operatorUsableByFile:        make([]bool, len(files)),
	}
	filesByName := make(map[string]int, len(files))
	for _, file := range files {
		if isCodexAuthFile(file) {
			filesByName[strings.TrimSpace(file.Name)]++
		}
	}
	for index, file := range files {
		if !isCodexAuthFile(file) {
			continue
		}
		stats.total++
		if !file.Disabled {
			stats.enabled++
			if smartAccountNeedsAttention(file.Raw) {
				stats.needsAttention++
				stats.recordCredentialBucket(file, operatorAccountNeedsAttention, filesByName[strings.TrimSpace(file.Name)] == 1)
			} else {
				stats.unconfirmed++
				stats.recordCredentialBucket(file, operatorAccountUnconfirmed, filesByName[strings.TrimSpace(file.Name)] == 1)
			}
		}
		if isAvailableCodexFile(file) {
			stats.schedulable++
			stats.operatorUsableByFile[index] = true
			limit, observed := smartAccountConcurrencyLimit(file.Raw)
			switch {
			case !observed:
				// CPA versions predating the concurrency field have no per-account
				// limit. Runtime scheduling treats that as unlimited; preserve that
				// behavior while reporting the missing metadata separately.
				stats.concurrencyUnlimited++
				stats.concurrencyMissing++
			case limit <= 0:
				stats.concurrencyUnlimited++
			default:
				stats.concurrencyLimited++
				stats.concurrencyFiniteSlots += limit
			}
		}
	}
	return stats
}

func operatorCredentialKey(fileName, authIndex string) string {
	fileName = strings.ToLower(strings.TrimSpace(fileName))
	authIndex = strings.ToLower(strings.TrimSpace(authIndex))
	if fileName == "" || authIndex == "" {
		return ""
	}
	return "auth\x00" + fileName + "\x00" + authIndex
}

func operatorFileCredentialKey(fileName string) string {
	fileName = strings.ToLower(strings.TrimSpace(fileName))
	if fileName == "" {
		return ""
	}
	return "file\x00" + fileName
}

func (stats *accountPoolStats) recordCredentialBucket(file cpaauthfiles.File, bucket operatorAccountBucket, uniqueFileName bool) {
	if stats == nil || file.Disabled {
		return
	}
	if stats.bucketByCredential == nil {
		stats.bucketByCredential = make(map[string]operatorAccountBucket)
	}
	if key := operatorCredentialKey(file.Name, file.AuthIndex); key != "" {
		stats.bucketByCredential[key] = bucket
	}
	if uniqueFileName {
		if key := operatorFileCredentialKey(file.Name); key != "" {
			stats.bucketByCredential[key] = bucket
		}
	}
}

func (stats *accountPoolStats) recordCredentialTemporaryLimit(file cpaauthfiles.File, limit operatorAccountTemporaryLimit, uniqueFileName bool) {
	if stats == nil || file.Disabled || !limit.observed {
		return
	}
	if stats.temporaryLimitByCredential == nil {
		stats.temporaryLimitByCredential = make(map[string]operatorAccountTemporaryLimit)
	}
	if key := operatorCredentialKey(file.Name, file.AuthIndex); key != "" {
		stats.temporaryLimitByCredential[key] = limit
	}
	if uniqueFileName {
		if key := operatorFileCredentialKey(file.Name); key != "" {
			stats.temporaryLimitByCredential[key] = limit
		}
	}
}

func (stats *accountPoolStats) recordNormalCapacityEvidence(file cpaauthfiles.File, remaining float64, uniqueFileName bool) {
	if stats == nil || file.Disabled {
		return
	}
	if stats.normalRemainingByCredential == nil {
		stats.normalRemainingByCredential = make(map[string]float64)
	}
	remaining = clampFloat(remaining, 0, 1)
	if key := operatorCredentialKey(file.Name, file.AuthIndex); key != "" {
		stats.normalRemainingByCredential[key] = remaining
	}
	if uniqueFileName {
		if key := operatorFileCredentialKey(file.Name); key != "" {
			stats.normalRemainingByCredential[key] = remaining
		}
	}
}

type operatorAccountBucket int

const (
	operatorAccountUnconfirmed operatorAccountBucket = iota
	operatorAccountNormal
	operatorAccountNeedsAttention
	operatorAccountQuotaRisk
)

// accountPoolStatsFromFilesAndInspection uses the current CPA file set as the
// population and the latest completed inspection only as per-account evidence.
// This avoids combining counts from different snapshots while matching the
// credential page's exclusive precedence: disabled, action/error, quota risk,
// unconfirmed, then normally available.
func accountPoolStatsFromFilesAndInspection(files []cpaauthfiles.File, results []store.CodexInspectionResult) accountPoolStats {
	return accountPoolStatsFromFilesAndCurrentEvidence(
		files,
		results,
		nil,
		model.CodexInspectionTriggerManual,
		time.Now(),
	)
}

func accountPoolStatsFromFilesAndCurrentEvidence(
	files []cpaauthfiles.File,
	results []store.CodexInspectionResult,
	headerSnapshots []store.HeaderSnapshot,
	inspectionTriggerType string,
	now time.Time,
) accountPoolStats {
	stats := accountPoolStatsFromFiles(files)
	stats.normal = 0
	stats.needsAttention = 0
	stats.quotaRisk = 0
	stats.unconfirmed = 0
	stats.classificationObserved = true
	stats.liveObserved = true
	stats.bucketByCredential = make(map[string]operatorAccountBucket, len(files)*2)
	stats.temporaryLimitByCredential = make(map[string]operatorAccountTemporaryLimit, len(files)*2)
	stats.normalRemainingByCredential = make(map[string]float64, len(files)*2)
	stats.operatorUsableByFile = make([]bool, len(files))

	resultsByFile := make(map[string][]store.CodexInspectionResult, len(results))
	filesByName := make(map[string]int, len(files))
	for _, file := range files {
		if isCodexAuthFile(file) {
			filesByName[strings.TrimSpace(file.Name)]++
		}
	}
	for _, result := range results {
		if !isSmartCapacityInspectionResult(result) {
			continue
		}
		name := strings.TrimSpace(result.FileName)
		resultsByFile[name] = append(resultsByFile[name], result)
	}
	headerByCredential := make(map[string]store.HeaderSnapshot, len(headerSnapshots))
	for _, snapshot := range headerSnapshots {
		key := operatorHeaderCredentialKey(snapshot.AuthFileSnapshot, snapshot.AuthIndex)
		if key == "" {
			continue
		}
		current, exists := headerByCredential[key]
		if !exists || snapshot.TimestampMS > current.TimestampMS ||
			(snapshot.TimestampMS == current.TimestampMS && snapshot.ID > current.ID) {
			headerByCredential[key] = snapshot
		}
	}
	inspectionAuthoritative := operatorInspectionAuthoritative(inspectionTriggerType)

	for index, file := range files {
		if !isCodexAuthFile(file) || file.Disabled {
			continue
		}
		result, matched := matchInspectionResultForAuthFile(
			file,
			resultsByFile[strings.TrimSpace(file.Name)],
			filesByName[strings.TrimSpace(file.Name)],
		)
		header, headerMatched := headerByCredential[operatorHeaderCredentialKey(file.Name, file.AuthIndex)]
		if headerMatched && operatorHeaderSnapshotExpired(header, now) {
			headerMatched = false
		}
		bucket := operatorAccountUnconfirmed
		temporaryLimit := operatorAccountTemporaryLimit{}
		remainingFraction := 1.0
		if isAvailableCodexFile(file) && len(resultsByFile[strings.TrimSpace(file.Name)]) == 0 {
			// Preserve live capacity behavior when the selected inspection does not
			// contain this file. The credential summary still uses authoritative
			// inspection evidence whenever a matching row exists.
			bucket = operatorAccountNormal
		}
		if smartAccountNeedsAttention(file.Raw) {
			bucket = operatorAccountNeedsAttention
		} else if headerMatched && (!inspectionAuthoritative || !matched || header.TimestampMS > result.CreatedAtMS) {
			bucket = classifyOperatorAccountFromHeader(header)
			temporaryLimit, _ = operatorAccountTemporaryLimitFromHeader(header)
			if usedPercent, hasQuota := operatorHeaderSnapshotUsedPercent(header); hasQuota {
				remainingFraction = 1 - clampFloat(usedPercent/100, 0, 1)
			}
		} else if matched && (inspectionAuthoritative || operatorSupplyInspectionUsabilityConfirmed(result)) {
			bucket = classifyOperatorAccount(file, result)
			if remaining, hasQuota := inspectionResultRemainingQuotaFraction(result); hasQuota {
				remainingFraction = remaining
			}
		}
		switch bucket {
		case operatorAccountNormal:
			stats.normal++
		case operatorAccountNeedsAttention:
			stats.needsAttention++
		case operatorAccountQuotaRisk:
			stats.quotaRisk++
		default:
			stats.unconfirmed++
		}
		if isAvailableCodexFile(file) && (bucket == operatorAccountNormal || bucket == operatorAccountQuotaRisk) {
			stats.operatorUsable++
			stats.operatorUsableByFile[index] = true
		}
		uniqueFileName := filesByName[strings.TrimSpace(file.Name)] == 1
		stats.recordCredentialBucket(file, bucket, uniqueFileName)
		stats.recordCredentialTemporaryLimit(file, temporaryLimit, uniqueFileName)
		if bucket == operatorAccountNormal {
			stats.recordNormalCapacityEvidence(file, remainingFraction, uniqueFileName)
		}
	}
	return stats
}

func operatorSupplyInspectionUsabilityConfirmed(result store.CodexInspectionResult) bool {
	if inspectionResultUsabilityUnverified(result) {
		return false
	}
	action := strings.ToLower(strings.TrimSpace(result.Action))
	if action != "" && action != "keep" {
		return false
	}
	if _, hasQuota := inspectionResultRemainingQuotaFraction(result); hasQuota {
		return true
	}
	status := strings.ToLower(strings.TrimSpace(result.Status))
	state := strings.ToLower(strings.TrimSpace(result.State))
	return status == "active" || status == "ready" || state == "active" || state == "ready"
}

func operatorInspectionAuthoritative(triggerType string) bool {
	return !strings.EqualFold(strings.TrimSpace(triggerType), model.CodexInspectionTriggerSupplySnapshot)
}

func operatorHeaderCredentialKey(fileName, authIndex string) string {
	fileName = strings.ToLower(strings.TrimSpace(fileName))
	authIndex = strings.ToLower(strings.TrimSpace(authIndex))
	if fileName == "" || authIndex == "" {
		return ""
	}
	return fileName + "\x00" + authIndex
}

func classifyOperatorAccountFromHeader(snapshot store.HeaderSnapshot) operatorAccountBucket {
	errorKind := strings.ToLower(strings.TrimSpace(snapshot.HeaderErrorKind))
	errorCode := strings.ToLower(strings.TrimSpace(snapshot.HeaderErrorCode))
	usedPercent, hasQuota := operatorHeaderSnapshotUsedPercent(snapshot)
	if operatorHeaderSnapshotQuotaLimited(snapshot, errorKind, errorCode) {
		return operatorAccountQuotaRisk
	}
	if operatorHeaderSnapshotTemporarilyLimited(errorKind, errorCode) {
		// A short provider backoff confirms that the credential is accepted; it
		// does not turn an enabled, schedulable account into an account requiring
		// operator intervention. Preserve the independent low-quota signal when
		// the same response also publishes a nearly exhausted quota window.
		if hasQuota && usedPercent >= (1-smartNormalAccountMinimumRemainingFraction)*100 {
			return operatorAccountQuotaRisk
		}
		return operatorAccountNormal
	}
	if errorKind != "" || errorCode != "" {
		return operatorAccountNeedsAttention
	}
	if hasQuota && usedPercent >= (1-smartNormalAccountMinimumRemainingFraction)*100 {
		return operatorAccountQuotaRisk
	}
	if hasQuota {
		return operatorAccountNormal
	}
	return operatorAccountUnconfirmed
}

func operatorHeaderSnapshotQuotaLimited(snapshot store.HeaderSnapshot, errorKind, errorCode string) bool {
	if quota := snapshot.ResponseMetadata; quota != nil && quota.Quota != nil &&
		strings.TrimSpace(quota.Quota.RateLimitReachedType) != "" {
		return true
	}
	errorText := strings.ToLower(strings.TrimSpace(errorKind + " " + errorCode))
	for _, marker := range []string{
		"usage_limit_reached",
		"quota_exceeded",
		"quota_exhausted",
		"quota_depleted",
		"insufficient_quota",
		"billing_hard_limit",
		"hard_limit_reached",
		"credit_grant_exhausted",
		"credits_depleted",
	} {
		if strings.Contains(errorText, marker) {
			return true
		}
	}
	return false
}

func operatorHeaderSnapshotTemporarilyLimited(errorKind, errorCode string) bool {
	errorText := strings.ToLower(strings.TrimSpace(errorKind + " " + errorCode))
	for _, marker := range []string{
		"rate_limit",
		"rate-limit",
		"retry_after",
		"retry after",
		"too_many_requests",
		"too many requests",
		"http 429",
		"status 429",
		"status_code:429",
		"status_code\":429",
		"cooldown",
		"cooling",
	} {
		if strings.Contains(errorText, marker) {
			return true
		}
	}
	return false
}

func operatorAccountTemporaryLimitFromHeader(snapshot store.HeaderSnapshot) (operatorAccountTemporaryLimit, bool) {
	errorKind := strings.ToLower(strings.TrimSpace(snapshot.HeaderErrorKind))
	errorCode := strings.ToLower(strings.TrimSpace(snapshot.HeaderErrorCode))
	if operatorHeaderSnapshotQuotaLimited(snapshot, errorKind, errorCode) ||
		!operatorHeaderSnapshotTemporarilyLimited(errorKind, errorCode) {
		return operatorAccountTemporaryLimit{}, false
	}
	limit := operatorAccountTemporaryLimit{
		observed: true,
		kind:     errorKind,
		code:     errorCode,
	}
	if metadata := snapshot.ResponseMetadata; metadata != nil && metadata.Errors != nil {
		limit.recoverAtMS = metadata.Errors.RetryAfterRecoverAtMS
	}
	return limit, true
}

func operatorHeaderSnapshotUsedPercent(snapshot store.HeaderSnapshot) (float64, bool) {
	values := make([]float64, 0, 3)
	if snapshot.HeaderQuotaUsedPercent.Valid {
		values = append(values, snapshot.HeaderQuotaUsedPercent.Float64)
	}
	if quota := snapshot.ResponseMetadata; quota != nil && quota.Quota != nil {
		if quota.Quota.UsedPercent != nil {
			values = append(values, *quota.Quota.UsedPercent)
		}
		for _, window := range []*usage.HeaderQuotaWindow{quota.Quota.Primary, quota.Quota.Secondary} {
			if window != nil && window.UsedPercent != nil {
				values = append(values, *window.UsedPercent)
			}
		}
	}
	if len(values) == 0 {
		return 0, false
	}
	usedPercent := values[0]
	for _, value := range values[1:] {
		if value > usedPercent {
			usedPercent = value
		}
	}
	return usedPercent, true
}

func operatorHeaderSnapshotExpired(snapshot store.HeaderSnapshot, now time.Time) bool {
	resetStates := make([]int64, 0, 2)
	if metadata := snapshot.ResponseMetadata; metadata != nil && metadata.Quota != nil {
		for _, window := range []*usage.HeaderQuotaWindow{metadata.Quota.Primary, metadata.Quota.Secondary} {
			if window == nil || (window.UsedPercent == nil && window.ResetAtMS <= 0 &&
				(window.ResetAfterSeconds == nil || *window.ResetAfterSeconds <= 0)) {
				continue
			}
			resetAtMS := window.ResetAtMS
			if resetAtMS <= 0 && window.ResetAfterSeconds != nil && *window.ResetAfterSeconds > 0 {
				resetAtMS = snapshot.TimestampMS + int64(*window.ResetAfterSeconds*1000)
			}
			resetStates = append(resetStates, resetAtMS)
		}
	}
	if len(resetStates) > 0 {
		for _, resetAtMS := range resetStates {
			if resetAtMS <= 0 || resetAtMS > now.UnixMilli() {
				return false
			}
		}
		return true
	}
	if snapshot.HeaderQuotaRecoverAtMS.Valid {
		return snapshot.HeaderQuotaRecoverAtMS.Int64 <= now.UnixMilli()
	}
	if metadata := snapshot.ResponseMetadata; metadata != nil && metadata.Errors != nil &&
		metadata.Errors.RetryAfterRecoverAtMS > 0 {
		return metadata.Errors.RetryAfterRecoverAtMS <= now.UnixMilli()
	}
	return false
}

func matchInspectionResultForAuthFile(file cpaauthfiles.File, candidates []store.CodexInspectionResult, sameNameFiles int) (store.CodexInspectionResult, bool) {
	matches := make([]store.CodexInspectionResult, 0, 1)
	for _, result := range candidates {
		if !inspectionResultMatchesAuthFile(result, file, sameNameFiles == 1 && len(candidates) == 1) {
			continue
		}
		matches = append(matches, result)
	}
	if len(matches) != 1 {
		return store.CodexInspectionResult{}, false
	}
	return matches[0], true
}

func inspectionResultMatchesAuthFile(result store.CodexInspectionResult, file cpaauthfiles.File, allowFileOnly bool) bool {
	if strings.TrimSpace(result.FileName) != strings.TrimSpace(file.Name) ||
		normalizeOperatorAccountProvider(result.Provider) != normalizeOperatorAccountProvider(file.Provider) {
		return false
	}
	resultAuthIndex := strings.TrimSpace(result.AuthIndex)
	fileAuthIndex := strings.TrimSpace(file.AuthIndex)
	// CPA's auth-files response does not expose account_id for every Codex
	// credential. auth_index is the stable credential identity in that case,
	// so an account-id value present only in the inspection row must not make an
	// otherwise exact auth-index match look unconfirmed.
	if resultAuthIndex != "" {
		if fileAuthIndex == "" || resultAuthIndex != fileAuthIndex {
			return false
		}
		if resultAccountID := strings.TrimSpace(result.AccountID); resultAccountID != "" {
			if fileAccountID := strings.TrimSpace(file.AccountID); fileAccountID != "" && fileAccountID != resultAccountID {
				return false
			}
		}
		if resultSnapshot := directOperatorAccountSnapshot(result.FileName, result.AccountSnapshot); resultSnapshot != "" {
			if fileSnapshot := directOperatorAccountSnapshot(file.Name, file.AccountSnapshot); fileSnapshot != "" && fileSnapshot != resultSnapshot {
				return false
			}
		}
		return true
	}
	resultAccountID := strings.TrimSpace(result.AccountID)
	if resultAccountID != "" {
		fileAccountID := strings.TrimSpace(file.AccountID)
		if fileAccountID != "" {
			return resultAccountID == fileAccountID
		}
	}
	resultSnapshot := directOperatorAccountSnapshot(result.FileName, result.AccountSnapshot)
	if resultSnapshot != "" {
		fileSnapshot := directOperatorAccountSnapshot(file.Name, file.AccountSnapshot)
		if fileSnapshot != "" {
			return resultSnapshot == fileSnapshot
		}
	}
	// An identity that exists only in the inspection database cannot be safely
	// assigned to a live file unless the filename is unique in the caller.
	return allowFileOnly
}

func normalizeOperatorAccountProvider(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "-")
	if value == "openai" {
		return "codex"
	}
	if value == "openai-codex" {
		return "codex"
	}
	return value
}

func directOperatorAccountSnapshot(fileName, snapshot string) string {
	snapshot = strings.TrimSpace(snapshot)
	if snapshot == "" || snapshot == strings.TrimSpace(fileName) {
		return ""
	}
	return snapshot
}

func classifyOperatorAccount(file cpaauthfiles.File, result store.CodexInspectionResult) operatorAccountBucket {
	if smartAccountNeedsAttention(file.Raw) {
		return operatorAccountNeedsAttention
	}
	action := strings.ToLower(strings.TrimSpace(result.Action))
	// The credential page normalizes a missing historical action to keep.
	// Identity uncertainty, rather than an omitted legacy action, is what
	// belongs in the unconfirmed bucket.
	// Quota-limited results can carry an error/status message describing the
	// exhausted window. The credential page puts those in quota risk before
	// diagnostic exceptions, so preserve the same precedence here.
	if inspectionResultInCooldown(result) || result.IsQuota {
		return operatorAccountQuotaRisk
	}
	if action != "" && action != "keep" {
		return operatorAccountNeedsAttention
	}
	if strings.TrimSpace(result.ErrorKind) != "" || strings.TrimSpace(result.Error) != "" || strings.TrimSpace(result.ErrorDetail) != "" {
		return operatorAccountNeedsAttention
	}
	if remaining, hasQuota := inspectionResultRemainingQuotaFraction(result); hasQuota && remaining < smartNormalAccountMinimumRemainingFraction {
		return operatorAccountQuotaRisk
	}
	return operatorAccountNormal
}

func (stats accountPoolStats) capacityAvailable(fallback int) int {
	if stats.inspectionObserved {
		return max(0, stats.verifiedAvailable)
	}
	return max(0, stats.schedulable)
}

// operatorAvailable is deliberately separate from capacityAvailable. The
// former mirrors the credential-management "normally available" bucket; the
// latter is the conservative inspection-backed population used by the supply
// planner. A missing classification snapshot falls back to the live
// schedulable count rather than pretending that capacity health is an
// operator classification.
func (stats accountPoolStats) operatorAvailable(fallback int) int {
	if stats.classificationObserved {
		return max(0, stats.normal)
	}
	if stats.liveObserved {
		return max(0, stats.schedulable)
	}
	return max(0, fallback)
}

func reconcileAccountPoolStatsWithInspection(stats accountPoolStats, resource SmartResource) accountPoolStats {
	if !stats.liveObserved {
		if resource.CapacitySource != smartCapacitySourceInspection || resource.CapacitySnapshotAtMS <= 0 ||
			resource.TotalAccounts < stats.total {
			return stats
		}
		stats.inspectionObserved = true
		stats.verifiedAvailable = min(max(0, resource.AvailableAccounts), max(0, stats.schedulable))
		return stats
	}
	inspectedEnabled := resource.EnabledAccounts
	if inspectedEnabled <= 0 {
		inspectedEnabled = resource.TotalAccounts
	}
	if resource.CapacitySource != smartCapacitySourceInspection || resource.CapacitySnapshotAtMS <= 0 ||
		inspectedEnabled < stats.enabled {
		return stats
	}
	stats.inspectionObserved = true
	stats.verifiedAvailable = min(max(0, resource.AvailableAccounts), max(0, stats.schedulable))
	return stats
}

func applyAccountPoolStats(resource *SmartResource, stats accountPoolStats) {
	if resource == nil {
		return
	}
	if stats.liveObserved {
		resource.TotalAccounts = max(0, stats.enabled)
	} else {
		resource.TotalAccounts = max(0, stats.total)
	}
	resource.AvailableAccounts = stats.capacityAvailable(resource.AvailableAccounts)
	resource.SchedulableAccounts = max(0, stats.schedulable)
	if stats.liveObserved {
		resource.EnabledAccounts = max(0, stats.enabled)
		resource.DisabledAccounts = max(0, stats.total-stats.enabled)
	} else {
		resource.DisabledAccounts = max(0, resource.TotalAccounts-resource.SchedulableAccounts)
	}
	if stats.classificationObserved {
		resource.operatorClassificationObserved = true
		resource.AccountClassificationObserved = true
		resource.NormalAccounts = max(0, stats.normal)
		resource.NeedsAttentionAccounts = max(0, stats.needsAttention)
		resource.QuotaRiskAccounts = max(0, stats.quotaRisk)
		resource.UnconfirmedAccounts = max(0, stats.unconfirmed)
		resource.AvailableAccounts = max(0, stats.normal)
	} else if stats.liveObserved {
		// Do not retain the previous inspection split when the current response
		// has no matching evidence. HealthyAccounts/AvailableAccounts are
		// capacity-planning values, not an operator classification; carrying
		// them into the summary makes the pool appear fully normal after an
		// inspection read failure or while smart mode is disabled.
		resource.operatorClassificationObserved = false
		resource.AccountClassificationObserved = false
		resource.NormalAccounts = 0
		resource.NeedsAttentionAccounts = 0
		resource.QuotaRiskAccounts = 0
		resource.UnconfirmedAccounts = 0
	}
	if resource.HealthyAccounts > resource.AvailableAccounts {
		resource.HealthyAccounts = resource.AvailableAccounts
	}
	applySmartAccountCountBreakdown(resource)
	applySmartCapacityBreakdown(resource, stats)
	applyAccountPoolConcurrency(resource, stats)
	applySmartTokenMetrics(resource)
}

func applySmartCapacityBreakdown(resource *SmartResource, stats accountPoolStats) {
	if resource == nil {
		return
	}
	total := math.Max(0, resource.CurrentCapacityRCU)
	if !stats.classificationObserved {
		resource.AvailableCapacityRCU = total
		resource.FrozenCapacityRCU = 0
		resource.TotalCapacityRCU = total
		return
	}
	available := 0.0
	frozen := 0.0
	unmatched := 0.0
	for _, item := range resource.capacityItems {
		capacity := math.Max(0, item.usableCapacityRCU)
		bucket, matched := stats.bucketByCredential[item.credentialKey]
		if !matched {
			bucket, matched = stats.bucketByCredential[item.fileKey]
		}
		if !matched {
			unmatched += capacity
		} else if bucket == operatorAccountNormal {
			available += capacity
		} else {
			frozen += capacity
		}
	}
	if unmatched > 0 {
		normalRatio := 0.0
		if stats.enabled > 0 {
			normalRatio = clampFloat(float64(max(0, stats.normal))/float64(stats.enabled), 0, 1)
		}
		available += unmatched * normalRatio
		frozen += unmatched * (1 - normalRatio)
	}
	classified := available + frozen
	if classified <= 0 && total > 0 {
		// Old in-memory snapshots do not contain credential-level capacity items.
		// Preserve the total and use the enabled-pool ratio only as a compatibility
		// fallback until the next snapshot rebuild supplies exact identities.
		if stats.enabled > 0 {
			available = total * float64(max(0, stats.normal)) / float64(stats.enabled)
		}
		frozen = math.Max(0, total-available)
	} else if classified > 0 && classified != total {
		scale := total / classified
		available *= scale
		frozen *= scale
	}
	resource.AvailableCapacityRCU = round2(clampFloat(available, 0, total))
	resource.FrozenCapacityRCU = round2(math.Max(0, total-resource.AvailableCapacityRCU))
	resource.TotalCapacityRCU = round2(total)
}

// reconcileSmartNormalCapacityFloor adds live credentials that have current
// normal evidence but are absent from the completed inspection's capacity
// items. This commonly happens when a stale inspection recorded 401/402 while
// a newer real request has already proved the refreshed credential usable.
// Missing account-scoped >10% evidence uses the configured plan fallback; a
// provisional 2-10% estimate never reduces the account below that fallback.
func (s *Service) reconcileSmartNormalCapacityFloor(
	cfg store.ManagerSupplyConfig,
	resource *SmartResource,
	stats accountPoolStats,
	now time.Time,
) bool {
	if s == nil || resource == nil || resource.CapacitySource != smartCapacitySourceInspection ||
		resource.CapacitySnapshotAtMS <= 0 || !stats.classificationObserved || len(stats.files) == 0 {
		return false
	}
	existing := make(map[string]struct{}, len(resource.capacityItems)*2)
	for _, item := range resource.capacityItems {
		if item.credentialKey != "" {
			existing[item.credentialKey] = struct{}{}
		}
		if item.fileKey != "" {
			existing[item.fileKey] = struct{}{}
		}
	}
	filesByName := make(map[string]int, len(stats.files))
	for _, file := range stats.files {
		if isCodexAuthFile(file) {
			filesByName[strings.TrimSpace(file.Name)]++
		}
	}
	addedCapacity := 0.0
	addedAccounts := 0
	addedByPlan := make(map[string]int)
	platforms := supplyPlatforms(cfg)
	for _, file := range stats.files {
		if file.Disabled || !isAvailableCodexFile(file) {
			continue
		}
		credentialKey := operatorCredentialKey(file.Name, file.AuthIndex)
		fileKey := operatorFileCredentialKey(file.Name)
		bucket, matched := stats.bucketByCredential[credentialKey]
		if !matched && filesByName[strings.TrimSpace(file.Name)] == 1 {
			bucket, matched = stats.bucketByCredential[fileKey]
		}
		if !matched || bucket != operatorAccountNormal {
			continue
		}
		if _, found := existing[credentialKey]; found {
			continue
		}
		uniqueFileName := filesByName[strings.TrimSpace(file.Name)] == 1
		if uniqueFileName {
			if _, found := existing[fileKey]; found {
				continue
			}
		}

		planType := strings.ToLower(strings.TrimSpace(textField(
			file.Raw,
			"plan_type", "planType", "chatgpt_plan_type", "chatgptPlanType",
		)))
		if planType == "" {
			planType = "unknown"
		}
		supplierID := normalizeSmartQuotaSupplierID(resource.quotaSupplierByFile[strings.TrimSpace(file.Name)])
		if supplierID == "" && len(platforms) == 1 {
			supplierID = normalizeSmartQuotaSupplierID(platforms[0].ID)
		}
		policy := smartQuotaPolicyForSupplier(cfg, supplierID, planType)
		estimate := smartQuotaEstimate{
			CapacityM:  policy.FallbackM,
			Source:     smartQuotaEstimateSourceDefault,
			Confidence: smartConfidenceLow,
		}
		if policy.Mode == smartQuotaPolicyModeFixed {
			estimate.CapacityM = policy.FixedM
			estimate.Source = smartQuotaPolicyModeFixed
			estimate.Confidence = smartConfidenceHigh
		} else {
			for _, plan := range resource.AccountQuotaPlanEstimates {
				if normalizeSmartQuotaSupplierID(plan.SupplierID) == supplierID &&
					strings.EqualFold(strings.TrimSpace(plan.PlanType), planType) && plan.AdoptedM > 0 {
					estimate.CapacityM = plan.AdoptedM
					estimate.Source = plan.Source
					break
				}
			}
			identities := smartQuotaCalibrationResultIdentities(
				file.Name,
				file.AuthIndex,
				textField(file.Raw, "account_key", "accountKey"),
				firstNonEmptyString(file.AccountID, textField(file.Raw, "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId")),
			)
			if current, ok := s.smartQuotaCurrentEstimateForAt(now, identities...); ok && !current.Provisional && current.CapacityM > 0 {
				estimate = current
			}
		}
		remaining := 1.0
		if value, ok := stats.normalRemainingByCredential[credentialKey]; ok {
			remaining = value
		} else if value, ok := stats.normalRemainingByCredential[fileKey]; ok {
			remaining = value
		}
		capacity := smartAccountQuotaCapacityRCU(resource.UnitCapacityRCU, estimate.CapacityM, remaining)
		if capacity <= 0 {
			continue
		}
		remainingMinutes := smartAccountRemainingMinutes(file.Raw, now, smartCapacityPlanningHorizonMinutes(cfg))
		item := smartCapacityItem{
			credentialKey:    credentialKey,
			fileKey:          fileKey,
			capacityRCU:      capacity,
			remainingMinutes: remainingMinutes,
		}
		expiryHintMinutes := remainingMinutes
		if expiry, supplied := smartSupplyLeaseExpiry(file.Raw, now); supplied {
			item.expiresAtMS = expiry.UnixMilli()
			expiryHintMinutes = expiry.Sub(now).Minutes()
		}
		resource.capacityItems = append(resource.capacityItems, item)
		existing[credentialKey] = struct{}{}
		if uniqueFileName {
			existing[fileKey] = struct{}{}
		}
		addedCapacity += capacity
		addedAccounts++
		addedByPlan[smartQuotaContextKey(supplierID, planType)]++
		resource.recordExpiringAccount(expiryHintMinutes, capacity)
	}
	if addedAccounts == 0 || addedCapacity <= 0 {
		return false
	}
	resource.RawCapacityRCU = round2(resource.RawCapacityRCU + addedCapacity)
	resource.HealthyAccounts = min(resource.AvailableAccounts, resource.HealthyAccounts+addedAccounts)
	applySmartAccountCountBreakdown(resource)
	for planKey, count := range addedByPlan {
		for index := range resource.AccountQuotaPlanEstimates {
			plan := &resource.AccountQuotaPlanEstimates[index]
			if smartQuotaContextKey(plan.SupplierID, plan.PlanType) == planKey {
				plan.AccountCount += count
				break
			}
		}
	}
	applySmartExpiryCapacity(resource, resource.capacityItems, resource.ConsumeRCUPerMinute, now)
	applySmartCapacityBreakdown(resource, stats)
	applySmartTokenMetrics(resource)
	return true
}

func applyAccountPoolConcurrency(resource *SmartResource, stats accountPoolStats) {
	if resource == nil || !stats.liveObserved {
		return
	}
	resource.ConcurrencyLimitedAccounts = max(0, stats.concurrencyLimited)
	resource.ConcurrencyUnlimitedAccounts = max(0, stats.concurrencyUnlimited)
	resource.ConcurrencyMissingAccounts = max(0, stats.concurrencyMissing)
	resource.ConcurrencyFiniteSlots = max(0, stats.concurrencyFiniteSlots)
	resource.ConcurrencyUnlimited = resource.ConcurrencyUnlimitedAccounts > 0
	recalculateAccountPoolConcurrency(resource)
}

func recalculateAccountPoolConcurrency(resource *SmartResource) {
	if resource == nil {
		return
	}
	resource.ConcurrencyLimited = false
	resource.ConcurrencyAccountDeficit = 0
	resource.ConcurrencyEffectiveCapacityRCU = round2(resource.CurrentCapacityRCU)

	required := max(0, resource.RequiredConcurrencySlots)
	if required <= 0 {
		resource.ConcurrencyCoverage = 100
		if !resource.ConcurrencyUnlimited {
			resource.ConcurrencyHeadroomSlots = resource.ConcurrencyFiniteSlots
		} else {
			resource.ConcurrencyHeadroomSlots = 0
		}
		return
	}
	if resource.ConcurrencyUnlimited {
		resource.ConcurrencyCoverage = 100
		resource.ConcurrencyHeadroomSlots = 0
		return
	}

	resource.ConcurrencyHeadroomSlots = resource.ConcurrencyFiniteSlots - required
	coverage := clampFloat(float64(resource.ConcurrencyFiniteSlots)/float64(required), 0, 1)
	resource.ConcurrencyCoverage = round2(coverage * 100)
	resource.ConcurrencyEffectiveCapacityRCU = round2(resource.CurrentCapacityRCU * coverage)
	resource.ConcurrencyLimited = resource.ConcurrencyFiniteSlots < required
	if !resource.ConcurrencyLimited {
		return
	}

	unitSlots := 1
	if resource.ConcurrencyLimitedAccounts > 0 && resource.ConcurrencyFiniteSlots > 0 {
		unitSlots = max(1, int(math.Ceil(float64(resource.ConcurrencyFiniteSlots)/float64(resource.ConcurrencyLimitedAccounts))))
	}
	resource.ConcurrencyAccountDeficit = int(math.Ceil(
		float64(required-resource.ConcurrencyFiniteSlots) / float64(unitSlots),
	))
}

// applySmartAccountCountBreakdown keeps the two account-counting dimensions
// explicit. AvailableAccounts/WeakAccounts are inspection-backed capacity
// counts used by the replenishment strategy, while SchedulableAccounts and
// AtRiskAccounts describe the live CPA pool shown to operators.
func applySmartAccountCountBreakdown(resource *SmartResource) {
	if resource == nil {
		return
	}
	resource.WeakAccounts = max(0, resource.AvailableAccounts-resource.HealthyAccounts)
	resource.FrozenAccounts = max(0, resource.TotalAccounts-resource.AvailableAccounts)
	if resource.operatorClassificationObserved {
		resource.AtRiskAccounts = max(0, resource.NeedsAttentionAccounts+resource.QuotaRiskAccounts+resource.UnconfirmedAccounts)
		return
	}
	resource.AtRiskAccounts = max(0, resource.SchedulableAccounts-resource.NormalAccounts)
}

// reconcileSmartCapacityWithAccountPool prevents a completed inspection from
// retaining capacity for credentials that have already disappeared from the
// live CPA schedulable pool. The live list is intentionally only a downward
// bound: newly visible files still need an inspection or import overlay before
// they can add capacity, while a rapid 401/disable proportionally removes stale
// inspected quota. Request-count risk thresholds are intentionally absent from
// this calculation.
func reconcileSmartCapacityWithAccountPool(resource *SmartResource, inspectedEnabled int) bool {
	if resource == nil {
		return false
	}
	liveEnabled := max(0, resource.TotalAccounts)
	inspectedEnabled = max(0, inspectedEnabled)
	if inspectedEnabled <= 0 {
		if liveEnabled == 0 && resource.CurrentCapacityRCU > 0 {
			resource.CurrentCapacityRCU = 0
			resource.TimeLimitedCapacityRCU = 0
			resource.AvailableCapacityRCU = 0
			resource.FrozenCapacityRCU = 0
			resource.TotalCapacityRCU = 0
			resource.PendingInspectionCapacityRCU = 0
			resource.PendingInspectionAccounts = 0
			recalculateAccountPoolConcurrency(resource)
			return true
		}
		return false
	}
	if liveEnabled >= inspectedEnabled {
		return false
	}
	ratio := float64(liveEnabled) / float64(inspectedEnabled)
	adjust := func(value float64) float64 { return round2(math.Max(0, value) * ratio) }
	changed := false
	currentCapacity := adjust(resource.CurrentCapacityRCU)
	if resource.CurrentCapacityRCU != currentCapacity {
		resource.CurrentCapacityRCU = currentCapacity
		changed = true
	}
	timeLimitedCapacity := adjust(resource.TimeLimitedCapacityRCU)
	if resource.TimeLimitedCapacityRCU != timeLimitedCapacity {
		resource.TimeLimitedCapacityRCU = timeLimitedCapacity
		changed = true
	}
	resource.AvailableCapacityRCU = adjust(resource.AvailableCapacityRCU)
	resource.FrozenCapacityRCU = adjust(resource.FrozenCapacityRCU)
	resource.TotalCapacityRCU = resource.CurrentCapacityRCU
	for index := range resource.capacityItems {
		resource.capacityItems[index].usableCapacityRCU = adjust(resource.capacityItems[index].usableCapacityRCU)
	}
	maximumPending := max(0, liveEnabled-resource.HealthyAccounts)
	if resource.PendingInspectionAccounts > maximumPending {
		previousPending := resource.PendingInspectionAccounts
		resource.PendingInspectionAccounts = maximumPending
		if maximumPending == 0 {
			resource.PendingInspectionCapacityRCU = 0
		} else if previousPending > 0 {
			resource.PendingInspectionCapacityRCU = round2(resource.PendingInspectionCapacityRCU * float64(maximumPending) / float64(previousPending))
		}
		changed = true
	}
	if changed {
		recalculateAccountPoolConcurrency(resource)
	}
	return changed
}

func preserveSmartResourceRuntimeState(resource *SmartResource, previous SmartResource) {
	if resource == nil {
		return
	}
	resource.PrelockedCapacityRCU = previous.PrelockedCapacityRCU
	resource.LockedOrderID = previous.LockedOrderID
	resource.LockedOrderAgeSeconds = previous.LockedOrderAgeSeconds
	resource.LockedConfirmRounds = previous.LockedConfirmRounds
	if previous.SupplyPressureLevel != "" {
		resource.SupplyPressureLevel = previous.SupplyPressureLevel
		resource.SupplyPressureReason = previous.SupplyPressureReason
		resource.SupplyInventoryAvailable = previous.SupplyInventoryAvailable
		resource.SupplyInventoryMissing = previous.SupplyInventoryMissing
		resource.SupplyInventoryMinRemainMinutes = previous.SupplyInventoryMinRemainMinutes
		resource.SupplyInventoryMaxRemainMinutes = previous.SupplyInventoryMaxRemainMinutes
		resource.SupplyNeedsProduction = previous.SupplyNeedsProduction
		resource.SupplyAvgFulfillSeconds = previous.SupplyAvgFulfillSeconds
		resource.SupplyRecentWaiting = previous.SupplyRecentWaiting
		resource.SupplyRecentOrders = previous.SupplyRecentOrders
		resource.SupplyRecentCancelled = previous.SupplyRecentCancelled
		resource.SupplyRecentZeroDelivery = previous.SupplyRecentZeroDelivery
		resource.SupplyRecentRequestedQuantity = previous.SupplyRecentRequestedQuantity
		resource.SupplyRecentDeliveredQuantity = previous.SupplyRecentDeliveredQuantity
		resource.SupplyFulfillmentRate = previous.SupplyFulfillmentRate
		resource.SupplyReliable = previous.SupplyReliable
		resource.SupplyRecovering = previous.SupplyRecovering
		resource.SupplyRecentSuccessStreak = previous.SupplyRecentSuccessStreak
		resource.SupplyShortWindowOrders = previous.SupplyShortWindowOrders
		resource.SupplyShortWindowFulfillment = previous.SupplyShortWindowFulfillment
	}
}

func (s *Service) reconcileSmartAccountPoolGuard(cfg store.ManagerSupplyConfig, resource *SmartResource) {
	if resource == nil || !smartSupplyStrategyConfigured(cfg) || !smartEmergencyBypassUsageRate(cfg) {
		return
	}
	if smartAvailableCapacityEmergency(cfg, *resource) {
		applySmartEmergencyAvailability(cfg, resource, time.Now())
		// At exactly the configured critical account floor, an idle pool is
		// intentionally sufficient: applySmartEmergencyAvailability clears the
		// stale emergency state instead of buying short-lived credentials. Finish
		// the zero-traffic recalculation here so a previous
		// usage_rate_not_ready decision does not survive the live-pool refresh.
		if !smartResourceEmergency(*resource) {
			recalculateSmartResourceCapacityPlan(cfg, resource)
			return
		}
		if resource.AvailableAccounts <= 0 {
			startedAtMS := s.beginPoolVacuum()
			resource.PoolVacuumActive = true
			resource.PoolVacuumStartedAtMS = startedAtMS
			resource.PoolVacuumDurationSeconds = max(0, int(time.Since(time.UnixMilli(startedAtMS)).Seconds()))
		} else {
			s.clearPoolVacuum()
			resource.PoolVacuumActive = false
			resource.PoolVacuumStartedAtMS = 0
			resource.PoolVacuumDurationSeconds = 0
		}
		return
	}
	s.clearPoolVacuum()
	if !smartAccountAvailabilityEmergencyReason(resource.EmergencyReason) &&
		resource.EmergencyReason != "healthy_available_accounts" {
		return
	}
	resource.EmergencyReason = ""
	resource.PoolVacuumActive = false
	resource.PoolVacuumStartedAtMS = 0
	resource.PoolVacuumDurationSeconds = 0
	if smartAccountAvailabilityEmergencyReason(resource.DecisionReason) ||
		resource.DecisionReason == "healthy_available_accounts" {
		resource.EmergencyShortage = false
		if resource.ConsumeRCUPerMinute <= 0 && resource.DemandTrend != smartDemandTrendFalling {
			resource.HealthLevel = smartHealthUnknown
			resource.SuggestedAction = smartActionSnapshotStale
			resource.SuggestedQuantity = 0
			resource.DecisionReason = "usage_rate_not_ready"
			return
		}
		recalculateSmartResourceCapacityPlan(cfg, resource)
	}
}

func (s *Service) smartEmergencyAvailability(ctx context.Context, cfg store.ManagerConfig, resource *SmartResource) (int, int, string, bool, error) {
	if resource == nil {
		return 0, 0, "", false, nil
	}
	poolStats, err := s.countAccountPoolStatsWithInspection(ctx, cfg, *resource)
	if err != nil {
		return 0, 0, "", false, err
	}
	inspectedEnabled := resource.EnabledAccounts
	applyAccountPoolStats(resource, poolStats)
	capacityChanged := s.reconcileSmartNormalCapacityFloor(cfg.Supply, resource, poolStats, time.Now())
	if reconcileSmartCapacityWithAccountPool(resource, inspectedEnabled) {
		capacityChanged = true
	}
	if capacityChanged && resource.ConsumeRCUPerMinute > 0 {
		recalculateSmartResourceCapacityPlan(cfg.Supply, resource)
	}
	// The first return value only updates the operator overview; every guard and
	// replenishment decision below remains based on verified capacity.
	operatorAvailable := poolStats.operatorAvailable(resource.SchedulableAccounts)
	if !smartSupplyStrategyConfigured(cfg.Supply) || !smartEmergencyBypassUsageRate(cfg.Supply) {
		return operatorAvailable, 0, "", true, nil
	}
	if !smartAvailableCapacityEmergency(cfg.Supply, *resource) {
		s.clearPoolVacuum()
		return operatorAvailable, 0, "", true, nil
	}
	applySmartEmergencyAvailability(cfg.Supply, resource, time.Now())
	if resource.AvailableAccounts <= 0 {
		startedAtMS := s.beginPoolVacuum()
		resource.PoolVacuumActive = true
		resource.PoolVacuumStartedAtMS = startedAtMS
		resource.PoolVacuumDurationSeconds = max(0, int(time.Since(time.UnixMilli(startedAtMS)).Seconds()))
	} else {
		s.clearPoolVacuum()
		resource.PoolVacuumActive = false
		resource.PoolVacuumStartedAtMS = 0
		resource.PoolVacuumDurationSeconds = 0
	}
	quantity := resource.SuggestedQuantity
	reason := resource.DecisionReason
	if reason == "" {
		reason = "available_capacity_critical"
	}
	return operatorAvailable, quantity, reason, true, nil
}

func (s *Service) beginPoolVacuum() int64 {
	if s == nil {
		return time.Now().UnixMilli()
	}
	s.poolVacuumMu.Lock()
	defer s.poolVacuumMu.Unlock()
	if s.poolVacuumStarted <= 0 {
		s.poolVacuumStarted = time.Now().UnixMilli()
	}
	return s.poolVacuumStarted
}

func (s *Service) clearPoolVacuum() {
	if s == nil {
		return
	}
	s.poolVacuumMu.Lock()
	s.poolVacuumStarted = 0
	s.poolVacuumMu.Unlock()
}

type supplyAuth401Candidate struct {
	FileName       string
	AuthIndex      string
	Account        string
	AccountID      string
	AuthLabel      string
	Provider       string
	EventHash      string
	SeenAtMS       int64
	EvidenceJSON   string
	FailureSummary string
}

func (s *Service) handleSupplyAuth401Events(ctx context.Context, runtimeCfg collectorpkg.RuntimeConfig, events []usage.Event) {
	if s == nil || len(events) == 0 {
		return
	}
	seen := map[string]struct{}{}
	candidates := make([]supplyAuth401Candidate, 0)
	nowMS := time.Now().UnixMilli()
	for _, event := range events {
		candidate, ok := supplyAuth401CandidateFromEvent(event, nowMS)
		if !ok {
			continue
		}
		key := candidate.FileName + "\x00" + candidate.AuthIndex
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		return
	}
	go s.processSupplyAuth401Candidates(context.WithoutCancel(ctx), runtimeCfg, candidates)
}

func supplyAuth401CandidateFromEvent(event usage.Event, nowMS int64) (supplyAuth401Candidate, bool) {
	if !event.Failed || event.FailStatusCode != http.StatusUnauthorized {
		return supplyAuth401Candidate{}, false
	}
	fileName := strings.TrimSpace(event.AuthFileSnapshot)
	if fileName == "" || !smartSupplyManagedFileName(fileName) {
		return supplyAuth401Candidate{}, false
	}
	seenAtMS := event.TimestampMS
	if seenAtMS <= 0 {
		seenAtMS = nowMS
	}
	provider := strings.TrimSpace(firstNonEmptyString(event.AuthProviderSnapshot, event.Provider))
	return supplyAuth401Candidate{
		FileName:       fileName,
		AuthIndex:      strings.TrimSpace(event.AuthIndex),
		Account:        firstNonEmptyString(event.AccountSnapshot, event.AuthLabelSnapshot, event.Source, fileName),
		AccountID:      strings.TrimSpace(event.AuthProjectIDSnapshot),
		AuthLabel:      event.AuthLabelSnapshot,
		Provider:       provider,
		EventHash:      event.EventHash,
		SeenAtMS:       seenAtMS,
		EvidenceJSON:   supplyAuth401EvidenceJSON(event),
		FailureSummary: event.FailSummary,
	}, true
}

func (s *Service) processSupplyAuth401Candidates(ctx context.Context, runtimeCfg collectorpkg.RuntimeConfig, candidates []supplyAuth401Candidate) {
	if s == nil || len(candidates) == 0 || s.store == nil || s.managerConfig == nil {
		return
	}
	cfg, _, _, err := s.managerConfig.ResolveManagerConfigWithSource(ctx)
	if err != nil {
		return
	}
	baseURL := strings.TrimSpace(firstNonEmptyString(runtimeCfg.CPAUpstreamURL, cfg.CPAConnection.CPABaseURL))
	managementKey := strings.TrimSpace(firstNonEmptyString(runtimeCfg.ManagementKey, cfg.CPAConnection.ManagementKey))
	patchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, candidate := range candidates {
		// Record the 401 as a reauth candidate, but keep the credential visible
		// until an operator or a separate recovery flow handles it.
		item, err := s.store.UpsertAccountActionCandidate(patchCtx, model.AccountActionCandidateUpsert{
			ActionType:          model.AccountActionTypeReauth,
			Provider:            candidate.Provider,
			AuthFileName:        candidate.FileName,
			AuthIndex:           candidate.AuthIndex,
			AccountSnapshot:     candidate.Account,
			AccountIDSnapshot:   candidate.AccountID,
			AuthLabel:           candidate.AuthLabel,
			ReasonCode:          "invalid_401",
			Reason:              firstNonEmptyString(candidate.FailureSummary, "OAuth token returned 401 and was quarantined"),
			AutoDisableEligible: false,
			EvidenceJSON:        candidate.EvidenceJSON,
			SeenAtMS:            candidate.SeenAtMS,
		})
		if err == nil && baseURL != "" && managementKey != "" && item.AutoDisableEligible {
			if patchErr := s.authFiles.PatchDisabled(patchCtx, baseURL, managementKey, candidate.FileName, true, candidate.AuthIndex); patchErr == nil {
				_ = s.store.MarkAccountActionCandidateAutoDisabled(patchCtx, item.ID, time.Now().UnixMilli())
			} else {
				_ = s.store.RecordAccountActionCandidateFailure(patchCtx, item.ID, patchErr.Error())
			}
		}
	}
	s.invalidateAuthAndCapacityCaches()
	if smartRecoveryTriggerOn401(cfg.Supply) {
		autoClaim := true
		_, _ = s.SyncRecoveries(ctx, RecoverySyncRequest{
			Force:     true,
			AutoClaim: &autoClaim,
			Limit:     max(1, recoveryClaimBatchSize(cfg.Supply)),
		})
	}
	if managerconfigsvc.SupplyEnabled(cfg.Supply) {
		_ = s.RunAutomatic(ctx)
	}
}

func supplyAuth401EvidenceJSON(event usage.Event) string {
	evidence := map[string]any{
		"eventHash":         event.EventHash,
		"requestId":         event.RequestID,
		"timestamp":         event.Timestamp,
		"timestampMs":       event.TimestampMS,
		"statusCode":        event.FailStatusCode,
		"failSummary":       event.FailSummary,
		"headerErrorKind":   event.HeaderErrorKind,
		"headerErrorCode":   event.HeaderErrorCode,
		"headerTraceId":     event.HeaderTraceID,
		"authIndex":         event.AuthIndex,
		"authFileName":      event.AuthFileSnapshot,
		"accountSnapshot":   event.AccountSnapshot,
		"accountIdSnapshot": event.AuthProjectIDSnapshot,
		"authLabel":         event.AuthLabelSnapshot,
		"provider":          firstNonEmptyString(event.AuthProviderSnapshot, event.Provider),
		"model":             event.Model,
		"endpoint":          event.Endpoint,
		"actionType":        model.AccountActionTypeReauth,
		"reasonCode":        "invalid_401",
		"quarantineSource":  "supply_pool",
	}
	data, err := json.Marshal(evidence)
	if err != nil {
		return ""
	}
	return string(data)
}

func (s *Service) invalidateAuthAndCapacityCaches() {
	if s == nil {
		return
	}
	s.invalidateAuthFilesCache()
	s.invalidateInspectionQuotaSnapshot()
	s.invalidateStatusCache()
}

type smartSupplyPressure struct {
	level                       string
	reason                      string
	inventoryAvailable          int
	inventoryMissing            int
	inventoryMinRemainSeconds   int64
	inventoryMaxRemainSeconds   int64
	needsProduction             bool
	avgFulfillSeconds           int
	recentWaiting               int
	recentOrders                int
	recentCancelled             int
	recentZeroDelivery          int
	requestedQuantity           int
	deliveredQuantity           int
	fulfillmentRate             float64
	shortWindowOrders           int
	shortWindowRequested        int
	shortWindowDelivered        int
	shortWindowFulfillmentRate  float64
	shortWindowAvgFulfillSecond int
	recentSuccessStreak         int
	reliablyAvailable           bool
	recoveringAvailable         bool
}

const (
	smartSupplyReliabilityWindowOrders   = 8
	smartSupplyReliabilityMinOrders      = 3
	smartSupplyReliabilityMinFulfillment = 85.0
	smartSupplyReliabilityMaxLeadSeconds = 90
)

// recentSupplyOrders amortizes the two operator calculations that inspect the
// same recent order history (supplier pressure and daily quantity usage). The
// cache is intentionally very short and is invalidated on every local
// supplier-purchase mutation, so it removes repeated SQLite scans from
// dashboard/worker bursts without delaying a replenishment decision.
func (s *Service) recentSupplyOrders(ctx context.Context) ([]store.SupplyOrder, error) {
	if s == nil || s.store == nil {
		return nil, nil
	}
	now := time.Now()
	s.supplyOrdersCacheMu.Lock()
	defer s.supplyOrdersCacheMu.Unlock()
	if !s.supplyOrdersCache.generated.IsZero() && now.Sub(s.supplyOrdersCache.generated) <= supplyOrdersCacheTTL {
		return append([]store.SupplyOrder(nil), s.supplyOrdersCache.orders...), nil
	}

	orders, err := s.store.ListSupplyOrders(ctx, 200)
	if err != nil {
		return nil, err
	}
	s.supplyOrdersCache = supplyOrdersCache{
		generated: time.Now(),
		orders:    append([]store.SupplyOrder(nil), orders...),
	}
	return orders, nil
}

func (s *Service) invalidateSupplyOrdersCache() {
	if s == nil {
		return
	}
	s.supplyOrdersCacheMu.Lock()
	s.supplyOrdersCache = supplyOrdersCache{}
	s.supplyOrdersCacheMu.Unlock()
	s.invalidateStatusCache()
}

func (s *Service) smartSupplyPressure(ctx context.Context, cfg store.ManagerSupplyConfig, inventory supplyclient.Inventory, requestedQuantity int) smartSupplyPressure {
	if s == nil || s.store == nil {
		return smartSupplyPressureFromOrders(cfg, inventory, requestedQuantity, nil)
	}
	orders, err := s.recentSupplyOrders(ctx)
	if err != nil {
		return smartSupplyPressureFromOrders(cfg, inventory, requestedQuantity, nil)
	}
	return smartSupplyPressureFromOrders(cfg, inventory, requestedQuantity, orders)
}

func smartSupplyPressureFromOrders(cfg store.ManagerSupplyConfig, inventory supplyclient.Inventory, requestedQuantity int, orders []store.SupplyOrder) smartSupplyPressure {
	quantity := max(1, requestedQuantity)
	pressure := smartSupplyPressure{
		level:                     smartSupplyPressureUnknown,
		reason:                    "supply_pressure_unknown",
		inventoryAvailable:        max(0, inventory.Available),
		inventoryMissing:          max(0, inventory.Missing),
		inventoryMinRemainSeconds: max(0, inventory.MinimumRemainingSeconds),
		inventoryMaxRemainSeconds: max(0, inventory.MaximumRemainingSeconds),
		needsProduction:           inventory.NeedsProduction,
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

	nowMS := time.Now().UnixMilli()
	cutoffMS := nowMS - int64((24*time.Hour)/time.Millisecond)
	orderedOrders := append([]store.SupplyOrder(nil), orders...)
	sort.SliceStable(orderedOrders, func(i, j int) bool {
		if orderedOrders[i].CreatedAtMS != orderedOrders[j].CreatedAtMS {
			return orderedOrders[i].CreatedAtMS > orderedOrders[j].CreatedAtMS
		}
		return orderedOrders[i].ID > orderedOrders[j].ID
	})
	fulfillSamples := 0
	totalFulfillMS := int64(0)
	shortFulfillSamples := 0
	shortTotalFulfillMS := int64(0)
	streakOpen := true
	shortWindowClosed := false
	for _, order := range orderedOrders {
		if order.CreatedAtMS > 0 && order.CreatedAtMS < cutoffMS {
			break
		}
		if !isSupplierPurchaseHistoryOrder(order) || !order.Automatic || !supplyProductConfigured(cfg, order.Product) {
			continue
		}
		delivered := automaticOrderDeliveredQuantity(order)
		status := strings.ToLower(strings.TrimSpace(order.Status))
		shortTerminal := false
		switch status {
		case "completed":
			shortTerminal = true
			pressure.recentOrders++
			pressure.requestedQuantity += max(0, order.RequestedQuantity)
			pressure.deliveredQuantity += delivered
			if order.CompletedAtMS > order.CreatedAtMS {
				totalFulfillMS += order.CompletedAtMS - order.CreatedAtMS
				fulfillSamples++
			}
		case "cancelled", "failed", "dismissed":
			shortTerminal = true
			pressure.recentOrders++
			pressure.requestedQuantity += max(0, order.RequestedQuantity)
			pressure.deliveredQuantity += delivered
			if order.Status == "cancelled" {
				pressure.recentCancelled++
			}
			if delivered <= 0 {
				pressure.recentZeroDelivery++
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
		if shortTerminal && !shortWindowClosed && pressure.shortWindowOrders < smartSupplyReliabilityWindowOrders {
			requested := max(0, order.RequestedQuantity)
			successful := status == "completed" && requested > 0 &&
				float64(delivered)/float64(requested)*100 >= smartSupplyReliabilityMinFulfillment
			if !successful && pressure.recentSuccessStreak >= smartSupplyReliabilityMinOrders {
				// Once the latest three orders all succeeded, older failures belong to
				// the stale regime and must not suppress the newly proven supply state.
				shortWindowClosed = true
				continue
			}
			pressure.shortWindowOrders++
			pressure.shortWindowRequested += requested
			pressure.shortWindowDelivered += delivered
			if streakOpen {
				if successful {
					pressure.recentSuccessStreak++
				} else {
					streakOpen = false
				}
			}
			if successful && order.CompletedAtMS > order.CreatedAtMS {
				shortTotalFulfillMS += order.CompletedAtMS - order.CreatedAtMS
				shortFulfillSamples++
			}
		}
	}
	if fulfillSamples > 0 {
		pressure.avgFulfillSeconds = int(math.Round(float64(totalFulfillMS) / float64(fulfillSamples) / 1000))
	}
	if pressure.requestedQuantity > 0 {
		pressure.fulfillmentRate = round1(clampFloat(
			float64(pressure.deliveredQuantity)/float64(pressure.requestedQuantity)*100,
			0,
			100,
		))
	}
	if pressure.shortWindowRequested > 0 {
		pressure.shortWindowFulfillmentRate = round1(clampFloat(
			float64(pressure.shortWindowDelivered)/float64(pressure.shortWindowRequested)*100,
			0,
			100,
		))
	}
	if shortFulfillSamples > 0 {
		pressure.shortWindowAvgFulfillSecond = int(math.Round(
			float64(shortTotalFulfillMS) / float64(shortFulfillSamples) / 1000,
		))
	}
	recentDeliveryProvesSupply := pressure.recentSuccessStreak >= smartSupplyReliabilityMinOrders &&
		pressure.shortWindowAvgFulfillSecond > 0 && pressure.shortWindowAvgFulfillSecond <= 45
	inventorySnapshotReady := inventory.Available > 0 && !inventory.NeedsProduction
	pressure.reliablyAvailable = pressure.shortWindowOrders >= smartSupplyReliabilityMinOrders &&
		pressure.recentSuccessStreak >= smartSupplyReliabilityMinOrders &&
		pressure.shortWindowFulfillmentRate >= smartSupplyReliabilityMinFulfillment &&
		pressure.shortWindowAvgFulfillSecond > 0 &&
		pressure.shortWindowAvgFulfillSecond <= smartSupplyReliabilityMaxLeadSeconds &&
		(inventorySnapshotReady || recentDeliveryProvesSupply)
	if pressure.reliablyAvailable {
		// A short streak must recover quickly from stale failures in both the
		// 24-hour aggregate and a pessimistic momentary inventory snapshot. Three
		// fast, complete deliveries are stronger evidence than needsProduction or
		// missing counters captured before the supplier finished its latest batch.
		// Stable fulfillment means there is no need to reserve a batch early;
		// purchase one consumption-paced step near the lower line.
		pressure.level = smartSupplyPressurePlenty
		pressure.reason = "supply_history_reliably_available"
		return pressure
	}
	recentRecoveryProvesSupply := inventorySnapshotReady &&
		pressure.recentSuccessStreak >= 2 &&
		pressure.shortWindowAvgFulfillSecond > 0 &&
		pressure.shortWindowAvgFulfillSecond <= 45
	if recentRecoveryProvesSupply {
		// Two consecutive fast, complete deliveries plus live stock are enough
		// to leave the historical failure regime, but not enough to declare the
		// supplier fully reliable. Admit only the normal one-step JIT probe and
		// observe it before increasing the batch.
		pressure.level = smartSupplyPressurePlenty
		pressure.reason = "supply_history_recovering"
		pressure.recoveringAvailable = true
		return pressure
	}
	// Supplier inventory snapshots are momentary and can report available while
	// competing orders are repeatedly cancelled. Treat the recent realized
	// delivery rate as the stronger signal so the next attempt uses the full
	// shortage-sized batch instead of repeating ineffective 1/2/3 probes.
	if pressure.recentOrders >= 3 &&
		(pressure.recentCancelled >= 2 || pressure.recentZeroDelivery >= 2 || pressure.fulfillmentRate < 50) {
		pressure.level = smartSupplyPressureScarce
		pressure.reason = "supply_history_low_fulfillment"
		return pressure
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
	resource.SupplyInventoryMinRemainMinutes = round1(float64(pressure.inventoryMinRemainSeconds) / 60)
	resource.SupplyInventoryMaxRemainMinutes = round1(float64(pressure.inventoryMaxRemainSeconds) / 60)
	resource.SupplyNeedsProduction = pressure.needsProduction
	resource.SupplyAvgFulfillSeconds = pressure.avgFulfillSeconds
	resource.SupplyRecentWaiting = pressure.recentWaiting
	resource.SupplyRecentOrders = pressure.recentOrders
	resource.SupplyRecentCancelled = pressure.recentCancelled
	resource.SupplyRecentZeroDelivery = pressure.recentZeroDelivery
	resource.SupplyRecentRequestedQuantity = pressure.requestedQuantity
	resource.SupplyRecentDeliveredQuantity = pressure.deliveredQuantity
	resource.SupplyFulfillmentRate = pressure.fulfillmentRate
	resource.SupplyReliable = pressure.reliablyAvailable
	resource.SupplyRecovering = pressure.recoveringAvailable
	resource.SupplyRecentSuccessStreak = pressure.recentSuccessStreak
	resource.SupplyShortWindowOrders = pressure.shortWindowOrders
	resource.SupplyShortWindowFulfillment = pressure.shortWindowFulfillmentRate
}

func smartPrelockQuantityForSupplyPressure(cfg store.ManagerSupplyConfig, resource SmartResource, pressure smartSupplyPressure, quantity int) (int, string) {
	quantity, reason, _ := smartPrelockQuantityForSupplyPressureWithTiming(cfg, resource, pressure, quantity)
	return quantity, reason
}

type smartPurchaseTiming struct {
	leadMinutes           float64
	triggerMinutes        float64
	waitMinutes           float64
	eligibleQuantity      int
	supplyLifetimeMinutes float64
	lifetimeQuantityLimit int
	lifetimeKnown         bool
	lifetimeLimited       bool
}

func applySmartPurchaseTiming(resource *SmartResource, timing smartPurchaseTiming) {
	if resource == nil {
		return
	}
	resource.PurchaseLeadMinutes = timing.leadMinutes
	resource.PurchaseTimingTriggerMinutes = timing.triggerMinutes
	resource.PurchaseTimingWaitMinutes = timing.waitMinutes
	resource.PurchaseTimingEligibleQuantity = timing.eligibleQuantity
	resource.PurchaseSupplyLifetimeMinutes = timing.supplyLifetimeMinutes
	resource.PurchaseLifetimeQuantityLimit = timing.lifetimeQuantityLimit
	resource.PurchaseLifetimeLimited = timing.lifetimeLimited
}

func smartPrelockQuantityForSupplyPressureWithTiming(cfg store.ManagerSupplyConfig, resource SmartResource, pressure smartSupplyPressure, quantity int) (int, string, smartPurchaseTiming) {
	if quantity <= 0 {
		return quantity, "", smartPurchaseTiming{}
	}
	if smartProgressiveStartupFloorRecovery(resource) {
		return min(quantity, 1), "startup_account_floor", smartPurchaseTiming{eligibleQuantity: min(quantity, 1)}
	}
	if isSmartEmergencyRetryReason(resource.DecisionReason) {
		// A retry quantity is already a descending ladder rung. Keep the normal
		// configured minimum here even when the account pool is empty; applying
		// the larger emergency minimum would turn 10/5/2 back into 10/5/5.
		limit := smartAutomaticOrderQuantityLimit(cfg, resource)
		minimum := min(smartPrelockMinQuantity(cfg), limit)
		return clampInt(quantity, minimum, limit), resource.DecisionReason, smartPurchaseTiming{}
	}
	if smartResourceEmergency(resource) {
		limit := smartAutomaticOrderQuantityLimit(cfg, resource)
		minimum := min(smartPrelockMinQuantity(cfg), limit)
		if smartAccountAvailabilityEmergency(resource) {
			minimum = 1
		}
		reason := firstNonEmptyString(resource.EmergencyReason, resource.DecisionReason)
		if reason == "" {
			reason = "emergency_refill_to_healthy"
		}
		if !smartAccountAvailabilityEmergency(resource) {
			reason = "emergency_refill_to_healthy"
		}
		return clampInt(quantity, minimum, limit), reason, smartPurchaseTiming{}
	}
	if resource.DemandTrend == smartDemandTrendFalling && !smartResourceEmergency(resource) {
		return 0, "demand_falling_observe", smartPurchaseTiming{}
	}
	rising := resource.DemandTrend == smartDemandTrendRising && !smartResourceEmergency(resource)
	if rising {
		quantity = min(quantity, smartRisingObservationQuantity(cfg, resource))
	}
	timing := smartJustInTimePurchase(cfg, resource, pressure, quantity)
	quantity = timing.eligibleQuantity
	if smartExtendedWaterlineProgressiveMode(resource) &&
		smartResourceAtOrBelowWarning(resource) && !smartResourceEmergency(resource) {
		// When an old 55-minute implicit ceiling is removed, the configured
		// 120/100/80 reserve can expose several accounts of backlog at once.
		// Fill that reserve one verified account per successful observation
		// cycle; low-price reserve purchasing remains independent.
		quantity = min(quantity, 1)
		timing.eligibleQuantity = quantity
	}
	if quantity <= 0 {
		if timing.lifetimeLimited {
			return 0, "supply_lifetime_capacity_wait", timing
		}
		return 0, "purchase_timing_wait", timing
	}
	if rising {
		return quantity, "demand_rising_observe", timing
	}
	maxQuantity := smartAutomaticOrderQuantityLimit(cfg, resource)
	minimumQuantity := min(smartPrelockMinQuantity(cfg), maxQuantity)
	quantity = clampInt(quantity, minimumQuantity, maxQuantity)
	if smartResourceAtOrBelowWarning(resource) {
		if smartAccountAvailabilityEmergency(resource) {
			return quantity, firstNonEmptyString(resource.EmergencyReason, "critical_available_accounts"), timing
		}
		return quantity, "low_water_staged_batch", timing
	}
	if resource.HealthLevel == smartHealthHealthy {
		// Pool health takes precedence over momentary supplier pressure. While
		// capacity is comfortably above the warning line, reserve only a small
		// batch and let the normal create cooldown spread subsequent purchases.
		// This staggers lease expiries without weakening warning/emergency refill.
		progressiveBatch := smartPlentySmallBatchQuantity(cfg, quantity)
		if quantity > progressiveBatch {
			return progressiveBatch, "pool_healthy_progressive_batch", timing
		}
		return quantity, "pool_healthy_progressive_batch", timing
	}
	if !smartPrelockEnabled(cfg) {
		return quantity, "", timing
	}
	minQuantity := minimumQuantity
	fallbackBatch := smartFallbackBatchQuantity(cfg)
	switch pressure.level {
	case smartSupplyPressurePlenty:
		// 货源充足时仍然少量多次，但批量跟随容量缺口分档：1/2/3。
		// quantity 已由消耗速率、账号剩余额度、有效期和健康水位共同计算。
		smallBatch := smartPlentySmallBatchQuantity(cfg, quantity)
		if quantity > smallBatch {
			return smallBatch, "supply_plenty_small_batch", timing
		}
		return quantity, "supply_plenty_small_batch", timing
	case smartSupplyPressureNormal:
		moderateBatch := clampInt(int(math.Ceil(float64(quantity)/2)), minQuantity, maxQuantity)
		moderateBatch = min(moderateBatch, fallbackBatch)
		if quantity > moderateBatch {
			return moderateBatch, "supply_normal_moderate_batch", timing
		}
		return quantity, "supply_normal_moderate_batch", timing
	case smartSupplyPressureTight:
		// 货源紧张时，健康度不足意味着需要尽快补足容量。不要再按
		// fallbackBatch 固定拆成 5 个，避免补货速度落后于消耗速度。
		return quantity, "supply_tight_full_batch", timing
	case smartSupplyPressureScarce:
		// 货源稀缺时同样按智能计算出的缺口一次锁定，数量仍已受
		// PrelockMaxQuantity、ReplenishBatchSize 和日限额约束。
		return quantity, "supply_scarce_full_batch", timing
	default:
		if resource.HealthLevel == smartHealthCritical {
			return quantity, "", timing
		}
		conservativeBatch := min(clampInt(2, minQuantity, maxQuantity), fallbackBatch)
		if quantity > conservativeBatch {
			return conservativeBatch, "supply_unknown_conservative_batch", timing
		}
		return quantity, "", timing
	}
}

func smartJustInTimePurchase(cfg store.ManagerSupplyConfig, resource SmartResource, pressure smartSupplyPressure, requested int) smartPurchaseTiming {
	result := smartPurchaseTiming{eligibleQuantity: max(0, requested)}
	demand := math.Max(resource.ConsumeRCUPerMinute, resource.DemandPlanningRCUPerMinute)
	unit := smartEstimatedNewAccountCapacityForResource(cfg, resource)
	if demand <= 0 || unit <= 0 {
		return result
	}
	leadMinutes := smartSupplyDeliveryLeadMinutes(cfg, pressure)
	currentMinutes := resource.EstimatedSustainMinutes
	if currentMinutes <= 0 && resource.CurrentCapacityRCU > 0 {
		currentMinutes = resource.CurrentCapacityRCU / demand
	}
	currentCapacity := resource.CurrentCapacityRCU
	if currentCapacity <= 0 && currentMinutes > 0 {
		currentCapacity = currentMinutes * demand
	}
	if lifetimeMinutes, known := smartSupplyInventoryLifetimeMinutes(pressure); known {
		result.lifetimeKnown = true
		result.supplyLifetimeMinutes = round1(lifetimeMinutes)
		lifetimeCapacityRoom := math.Max(0, demand*lifetimeMinutes-currentCapacity-resource.PrelockedCapacityRCU)
		result.lifetimeQuantityLimit = max(0, int(math.Floor((lifetimeCapacityRoom+1e-9)/unit)))
		if !smartAccountAvailabilityEmergency(resource) && result.eligibleQuantity > result.lifetimeQuantityLimit {
			result.eligibleQuantity = result.lifetimeQuantityLimit
			result.lifetimeLimited = true
		}
	}
	requested = result.eligibleQuantity
	unitMinutes := unit / demand
	targetMinutes := float64(max(1, resource.EffectiveHealthyMinutes))
	triggerMinutes := math.Max(float64(max(1, resource.WarningMinutes)), targetMinutes+leadMinutes-unitMinutes)
	if pressure.reliablyAvailable {
		// Stable stock and a fast recent success streak allow the pool to enter
		// later. Target the warning runway rather than refilling to the healthy
		// line, so each order replaces roughly the consumption until the next
		// useful account instead of creating another same-expiry batch.
		targetMinutes = float64(max(1, resource.WarningMinutes))
		triggerMinutes = math.Max(float64(max(1, resource.CriticalMinutes)), targetMinutes+leadMinutes-unitMinutes)
	}
	result.leadMinutes = round1(leadMinutes)
	result.triggerMinutes = round1(triggerMinutes)
	if requested <= 0 {
		if currentMinutes > triggerMinutes {
			result.waitMinutes = round1(currentMinutes - triggerMinutes)
		}
		return result
	}
	if smartResourceEmergency(resource) || resource.CapacityGapRCU <= 0 {
		return result
	}
	atOrBelowWarning := smartResourceAtOrBelowWarning(resource)
	if atOrBelowWarning && !pressure.reliablyAvailable &&
		(pressure.level == smartSupplyPressureTight || pressure.level == smartSupplyPressureScarce) {
		return result
	}
	targetCapacity := demand * targetMinutes
	arrivalGap := math.Max(0, targetCapacity-currentCapacity-resource.PrelockedCapacityRCU) + demand*leadMinutes
	eligible := int(math.Floor((arrivalGap + 1e-9) / unit))
	eligible = min(requested, max(0, eligible))
	if eligible <= 0 && atOrBelowWarning && !pressure.reliablyAvailable &&
		(pressure.level == smartSupplyPressurePlenty || pressure.level == smartSupplyPressureNormal) {
		// With no proven reliability streak, crossing warning water must still
		// reserve one account. Keep it to one instead of filling the healthy gap.
		eligible = min(requested, 1)
	}
	result.eligibleQuantity = eligible
	if eligible <= 0 && currentMinutes > triggerMinutes {
		result.waitMinutes = round1(currentMinutes - triggerMinutes)
	}
	return result
}

func smartSupplyInventoryLifetimeMinutes(pressure smartSupplyPressure) (float64, bool) {
	remainingSeconds := pressure.inventoryMinRemainSeconds
	if remainingSeconds <= 0 {
		remainingSeconds = pressure.inventoryMaxRemainSeconds
	}
	if remainingSeconds <= 0 {
		return 0, false
	}
	minutes := float64(remainingSeconds) / 60
	return math.Max(0, minutes), true
}

func smartSupplyDeliveryLeadMinutes(cfg store.ManagerSupplyConfig, pressure smartSupplyPressure) float64 {
	lead := 5.0
	switch pressure.level {
	case smartSupplyPressurePlenty:
		lead = 1
	case smartSupplyPressureNormal:
		lead = 3
	case smartSupplyPressureTight:
		lead = 6
	case smartSupplyPressureScarce:
		lead = 12
	}
	avgFulfillSeconds := pressure.avgFulfillSeconds
	fulfillmentRate := pressure.fulfillmentRate
	recentCancelled := pressure.recentCancelled
	if pressure.reliablyAvailable {
		avgFulfillSeconds = pressure.shortWindowAvgFulfillSecond
		fulfillmentRate = pressure.shortWindowFulfillmentRate
		recentCancelled = 0
	}
	if avgFulfillSeconds > 0 {
		lead = math.Max(lead, float64(avgFulfillSeconds)/60)
	}
	if fulfillmentRate > 0 && fulfillmentRate < 100 {
		lead *= math.Min(3, 100/fulfillmentRate)
	}
	lead += math.Min(5, float64(max(0, recentCancelled)))
	// Polling, download, import, token refresh and the first quota probe happen
	// after remote fulfillment. Reserve at least one minute for that local path.
	localReadyMinutes := math.Max(1, float64(max(1, cfg.PollIntervalSeconds))/60)
	return clampFloat(lead+localReadyMinutes, 2, 30)
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
	if resource.DemandTrend == smartDemandTrendFalling && !smartResourceEmergency(resource) {
		return 0
	}
	if smartAccountAvailabilityEmergency(resource) {
		quantity := smartMinimumAvailableRefillQuantity(cfg, resource)
		if quantity <= 0 {
			return 0
		}
		return clampInt(quantity, 1, 100)
	}
	quantity := resource.SuggestedQuantity
	if quantity <= 0 && resource.CapacityGapRCU > 0 && resource.UnitCapacityRCU > 0 {
		unit := smartEstimatedNewAccountCapacityForResource(cfg, resource)
		if unit <= 0 {
			unit = smartEstimatedAccountCapacityRCU(resource.UnitCapacityRCU, float64(smartCapacityPlanningHorizonMinutes(cfg)))
		}
		quantity = int(math.Ceil(resource.CapacityGapRCU / unit))
	}
	if quantity <= 0 {
		return 0
	}
	if resource.PrelockedCapacityRCU > 0 && resource.CapacityGapRCU > 0 {
		unit := smartEstimatedNewAccountCapacityForResource(cfg, resource)
		if unit <= 0 {
			unit = resource.UnitCapacityRCU
		}
		if unit > 0 {
			pendingCapacityAccounts := int(math.Ceil(resource.PrelockedCapacityRCU / unit))
			remainingAccountDeficit := max(0, resource.AccountQuantityDeficit-pendingCapacityAccounts)
			remainingCapacity := math.Max(0, resource.CapacityGapRCU-resource.PrelockedCapacityRCU)
			if remainingCapacity <= 0 && remainingAccountDeficit <= 0 {
				return 0
			}
			if remainingCapacity > 0 {
				quantity = min(quantity, int(math.Ceil(remainingCapacity/unit)))
			}
		}
	}
	if quantity <= 0 {
		return 0
	}
	accountQuantityLimit := 0
	applySmartAccountQuantityEstimate(cfg, &resource)
	if resource.EstimatedRequiredAccounts > 0 {
		if resource.AccountQuantityDeficit <= 0 {
			return 0
		}
		accountQuantityLimit = resource.AccountQuantityDeficit
		quantity = min(quantity, accountQuantityLimit)
	}
	limit := smartAutomaticOrderQuantityLimit(cfg, resource)
	quantity = min(quantity, limit)
	if smartPrelockEnabled(cfg) {
		minimumQuantity := min(smartPrelockMinQuantity(cfg), limit)
		if accountQuantityLimit > 0 {
			minimumQuantity = min(minimumQuantity, accountQuantityLimit)
		}
		quantity = max(quantity, minimumQuantity)
	}
	if resource.DemandTrend == smartDemandTrendRising && !smartResourceEmergency(resource) {
		quantity = min(quantity, smartRisingObservationQuantity(cfg, resource))
	}
	return clampInt(quantity, 1, 100)
}

func estimatedSupplyOrderCapacityRCU(cfg store.ManagerSupplyConfig, resource SmartResource, quantity int) float64 {
	if quantity <= 0 {
		return 0
	}
	unit := smartEstimatedNewAccountCapacityForResource(cfg, resource)
	return round2(float64(quantity) * unit)
}

func supplyOrderCapacityRCU(cfg store.ManagerSupplyConfig, resource SmartResource, order store.SupplyOrder) float64 {
	quantity := max(0, order.RequestedQuantity)
	if isSupplyOrderCapacityCommitted(order) {
		// Once stock is secured, use the quantity the supplier has actually made
		// ready (or that the importer has materialized), not the original request.
		// A 30-account order with six ready accounts contributes six accounts to
		// the final take budget and must not hide another real capacity deficit.
		actual := max(order.ReadyQuantity, max(order.ItemCount, order.ImportedCount))
		if actual > 0 {
			quantity = actual
		}
	}
	return estimatedSupplyOrderCapacityRCU(cfg, resource, quantity)
}

func totalSupplyOrderCapacityRCU(cfg store.ManagerSupplyConfig, resource SmartResource, orders []store.SupplyOrder) float64 {
	total := 0.0
	for _, order := range orders {
		total += supplyOrderCapacityRCU(cfg, resource, order)
	}
	return round2(total)
}

func totalCommittedSupplyOrderCapacityRCU(cfg store.ManagerSupplyConfig, resource SmartResource, orders []store.SupplyOrder) float64 {
	total := 0.0
	for _, order := range orders {
		if isSupplyOrderCapacityCommitted(order) {
			total += supplyOrderCapacityRCU(cfg, resource, order)
		}
	}
	return round2(total)
}

// supplyCreatePlanningResource separates reservations that are merely
// competing for supplier stock from capacity that is already secured. Waiting
// orders remain visible in the aggregate prelock/financial view, but they do
// not erase the shortage used to size a parallel抢货 order. Once any order is
// ready or being imported, its committed capacity is deducted normally.
func supplyCreatePlanningResource(
	cfg store.ManagerSupplyConfig,
	resource SmartResource,
	orders []store.SupplyOrder,
	parallel bool,
) SmartResource {
	if !parallel {
		return resource
	}
	resource.PrelockedCapacityRCU = totalCommittedSupplyOrderCapacityRCU(cfg, resource, orders)
	applySmartAccountQuantityEstimate(cfg, &resource)
	applySmartRefillProjection(cfg, &resource)
	return resource
}

// emergencyParallelOrderQuantity keeps the current calculated shortage as the
// durable task target while the bounded emergency competition remains active.
// The purchase-task worker is responsible for splitting that target into small
// child reservations; the attempt stage still includes cancelled rows so the
// three-order ceiling is respected.
func emergencyParallelOrderQuantity(
	resource SmartResource,
	quantity int,
	competition parallelSupplyCompetition,
	parallel bool,
) int {
	if quantity <= 0 || !parallel ||
		(!smartResourceEmergency(resource) && !isSmartEmergencyRetryReason(resource.DecisionReason)) {
		return quantity
	}
	if competition.attempts < 0 || competition.attempts >= 2 {
		return 0
	}
	return quantity
}

// readySupplyOrderAccepted applies a deterministic aggregate take budget to
// all orders that have actually secured stock. Taking/importing orders are
// irreversible commitments. The remaining ready orders are selected as a
// small subset whose combined capacity covers the current deficit with the
// least overage; if no subset can cover it, the largest useful underfill wins.
// This prevents an older large order from displacing a newer, better-fitting
// order while still counting every in-progress import before another take.
func readySupplyOrderAccepted(
	cfg store.ManagerSupplyConfig,
	resource SmartResource,
	orders []store.SupplyOrder,
	current *store.SupplyOrder,
	need float64,
	allowance float64,
) bool {
	committed := make([]store.SupplyOrder, 0, len(orders)+1)
	currentIncluded := false
	for _, order := range orders {
		if current != nil && order.OrderID == current.OrderID {
			order = *current
			currentIncluded = true
		}
		if !isSupplyOrderCapacityCommitted(order) {
			continue
		}
		committed = append(committed, order)
	}
	if current != nil && !currentIncluded && isSupplyOrderCapacityCommitted(*current) {
		committed = append(committed, *current)
	}
	if current == nil || len(committed) == 0 {
		return false
	}

	fixedCapacity := 0.0
	ready := make([]store.SupplyOrder, 0, len(committed))
	for _, order := range committed {
		if isSupplyOrderCapacityIrreversible(order) {
			fixedCapacity += supplyOrderCapacityRCU(cfg, resource, order)
			if order.OrderID == current.OrderID {
				return true
			}
			continue
		}
		ready = append(ready, order)
	}
	sort.SliceStable(ready, func(i, j int) bool {
		if ready[i].CreatedAtMS != ready[j].CreatedAtMS {
			return ready[i].CreatedAtMS < ready[j].CreatedAtMS
		}
		if ready[i].ID != ready[j].ID {
			return ready[i].ID < ready[j].ID
		}
		return ready[i].OrderID < ready[j].OrderID
	})
	need = math.Max(0, need)
	if fixedCapacity >= need || len(ready) == 0 {
		return false
	}
	// The open purchase window is normally three orders. Keep a defensive cap
	// for corrupted/legacy state so subset enumeration remains bounded.
	if len(ready) > maxTrackedOpenSupplyOrders {
		ready = ready[:maxTrackedOpenSupplyOrders]
	}
	capWithAllowance := need + math.Max(0, allowance)
	// When the first secured order alone covers the current shortage within the
	// overage allowance, take it instead of releasing scarce stock in favor of a
	// slightly closer combination that may disappear before the next worker run.
	// Later ready orders are reconsidered after this one becomes irreversible.
	if firstTotal := fixedCapacity + supplyOrderCapacityRCU(cfg, resource, ready[0]); firstTotal >= need && firstTotal <= capWithAllowance {
		return ready[0].OrderID == current.OrderID
	}
	bestMask := 0
	bestCategory := 3
	bestTotal := 0.0
	bestCount := 0
	maskLimit := 1 << len(ready)
	for mask := 1; mask < maskLimit; mask++ {
		total := fixedCapacity
		count := 0
		for index, order := range ready {
			if mask&(1<<index) == 0 {
				continue
			}
			total += supplyOrderCapacityRCU(cfg, resource, order)
			count++
		}
		category := 1
		switch {
		case total >= need && total <= capWithAllowance:
			category = 0
		case total >= need:
			// Prefer a useful underfill over an unnecessarily large take when a
			// closer order is still available. If every candidate is oversized,
			// the minimum-overage subset is a fallback only for a real emergency;
			// normal/warning capacity may wait for a better-sized reservation.
			category = 2
		}
		if category == 2 && !smartResourceEmergency(resource) {
			continue
		}
		better := false
		switch {
		case category < bestCategory:
			better = true
		case category > bestCategory:
			better = false
		case category == 0 || category == 2:
			overage := total - need
			bestOverage := bestTotal - need
			better = overage < bestOverage ||
				(overage == bestOverage && (count < bestCount || (count == bestCount && mask < bestMask)))
		default:
			better = total > bestTotal ||
				(total == bestTotal && (count < bestCount || (count == bestCount && mask < bestMask)))
		}
		if better {
			bestMask = mask
			bestCategory = category
			bestTotal = total
			bestCount = count
		}
	}
	for index, order := range ready {
		if order.OrderID == current.OrderID {
			return bestMask&(1<<index) != 0
		}
	}
	return false
}

func isSupplyOrderCapacityCommitted(order store.SupplyOrder) bool {
	if order.ReadyQuantity > 0 || isReadyForTake(order.RemoteStatus) {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(order.Status)) {
	case "ready", "taking", "importing", "partial":
		return true
	default:
		return false
	}
}

func isSupplyOrderCapacityIrreversible(order store.SupplyOrder) bool {
	switch strings.ToLower(strings.TrimSpace(order.Status)) {
	case "taking", "importing", "partial":
		return true
	default:
		return false
	}
}

func supplyOrderStrategy(cfg store.ManagerSupplyConfig, automatic bool) string {
	if !automatic {
		return "manual"
	}
	return managerconfigsvc.NormalizeSupplyStrategy(cfg.Strategy)
}

func supplyLowPriceReserveEnabled(cfg store.ManagerSupplyConfig) bool {
	return cfg.LowPriceReserveEnabled != nil && *cfg.LowPriceReserveEnabled &&
		cfg.LowPriceReserveMaxUnitPriceFen != nil && *cfg.LowPriceReserveMaxUnitPriceFen > 0 &&
		cfg.LowPriceReserveTargetAccounts > 0
}

func isLowPriceReserveTrigger(reason string) bool {
	return unwrappedSupplyTriggerReason(reason) == lowPriceReserveTriggerReason
}

func lowPriceReserveOpenOrdersReversible(orders []store.SupplyOrder) bool {
	if len(orders) == 0 {
		return false
	}
	for _, order := range orders {
		if !isLowPriceReserveTrigger(order.TriggerReason) ||
			isSupplyOrderCapacityCommitted(order) ||
			isSupplyOrderCapacityIrreversible(order) ||
			purchaseTaskOrderRequiresOperatorReview(order) {
			return false
		}
	}
	return true
}

func supplyOrderTriggerReason(resource SmartResource, automatic bool) string {
	if !automatic {
		return "manual"
	}
	if isSmartEmergencyRetryReason(resource.DecisionReason) {
		return resource.DecisionReason
	}
	if resource.EmergencyReason == "critical_available_accounts" && resource.CurrentCapacityRCU > 0 &&
		resource.CapacityGapRCU > 0 && resource.ConsumeRCUPerMinute > 0 {
		return "emergency_refill_to_healthy"
	}
	if resource.EmergencyReason != "" {
		return resource.EmergencyReason
	}
	if resource.VirtualDemandRCUPerMinute > 0 || resource.DemandTrend == smartDemandTrendVirtual {
		return "virtual_demand_memory"
	}
	return firstNonEmptyString(resource.DecisionReason, "automatic")
}

func isSupplierPurchaseHistoryOrder(order store.SupplyOrder) bool {
	return !strings.EqualFold(strings.TrimSpace(order.Strategy), "recovery") &&
		!strings.EqualFold(strings.TrimSpace(order.RemoteStatus), "recovery_claimed") &&
		!strings.HasPrefix(strings.ToLower(strings.TrimSpace(order.OrderID)), "recovery-")
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

func (s *Service) automaticCreateCooldownActive(ctx context.Context, cfg store.ManagerSupplyConfig, resource SmartResource) (bool, error) {
	seconds := smartCreateCooldownForResource(cfg, resource)
	s.stateMu.RLock()
	last := s.lastAutomaticCreateAtMS
	s.stateMu.RUnlock()
	latest, found, err := s.store.GetLatestAutomaticSupplyOrder(ctx)
	if err != nil {
		return false, err
	}
	if found {
		last = maxInt64(last, latest.CreatedAtMS)
		if retry, retryErr := s.automaticRetryPlan(ctx, cfg, resource, latest, time.Now()); retryErr != nil {
			return false, retryErr
		} else if retry.active {
			if automaticImmediateRetryEligible(cfg, latest) {
				state, stateErr := s.automaticRetryLadderState(ctx, cfg, latest)
				if stateErr != nil {
					return false, stateErr
				}
				// Only an untried rung bypasses cooldown. Once base/half/fifth
				// have all been confirmed cancelled, pause the whole cycle before
				// recalculating a fresh base from current demand.
				if state.nextQuantity > 0 {
					return false, nil
				}
				seconds = int(automaticRetryCycleCooldown(cfg, resource) / time.Second)
				last = maxInt64(last, smartSupplyOrderTerminalAtMS(latest))
			} else {
				seconds = max(1, int(retry.cooldown/time.Second))
				last = maxInt64(last, smartSupplyOrderTerminalAtMS(latest))
			}
		} else if automaticOrderHasNoDeliveredCapacity(latest) {
			// A failed/cancelled zero-delivery attempt still needs a short
			// duplicate guard. It does not represent usable capacity, so do not
			// let the normal "recent order covers the gap" branch hide it.
			last = maxInt64(last, smartSupplyOrderTerminalAtMS(latest))
		} else {
			// Start successful-order observation after fulfillment/import, not
			// order creation. Supplier or inspection latency must not consume the
			// interval intended to measure the capacity actually delivered.
			last = maxInt64(last, smartSupplyOrderTerminalAtMS(latest))
			seconds = smartSuccessfulOrderCooldownForDelivery(cfg, resource, automaticOrderDeliveredQuantity(latest))
		}
	}
	if seconds <= 0 || last <= 0 {
		return false, nil
	}
	return time.Since(time.UnixMilli(last)) < time.Duration(seconds)*time.Second, nil
}

const (
	automaticRetryBurstWindow         = 10 * time.Minute
	automaticUrgentRetryCycleCooldown = 10 * time.Second
)

// automaticRetryCycleCooldown starts a fresh segmented抢货 wave on the
// operator-configured check cadence. The base/half/fifth ladder already bounds
// one wave; adding a hidden 90-second pause after it is exhausted sharply
// reduces the total quantity attempted exactly when realized fulfillment is
// low. Urgent pool-vacuum cases may run faster, but a shortage cycle must never
// run slower than the configured automatic check interval.
func automaticRetryCycleCooldown(cfg store.ManagerSupplyConfig, resource SmartResource) time.Duration {
	configured := time.Duration(smartAutomaticCheckIntervalSeconds(cfg, resource)) * time.Second
	if configured <= 0 {
		configured = 30 * time.Second
	}
	if smartSupplyUrgentRetryRequired(resource) && configured > automaticUrgentRetryCycleCooldown {
		return automaticUrgentRetryCycleCooldown
	}
	return configured
}

func smartSupplyShortageRetryCadenceRequired(resource SmartResource) bool {
	if !smartShortageFastRetryAllowed(resource) {
		return false
	}
	if smartResourceEmergency(resource) || smartResourceAtOrBelowWarning(resource) {
		return true
	}
	switch resource.SupplyPressureLevel {
	case smartSupplyPressureTight, smartSupplyPressureScarce:
		return true
	default:
		return false
	}
}

func smartSupplyUrgentRetryRequired(resource SmartResource) bool {
	if !smartShortageFastRetryAllowed(resource) || resource.ConsumeRCUPerMinute <= 0 {
		return false
	}
	if resource.PoolVacuumActive || (resource.AccountClassificationObserved &&
		resource.AvailableAccounts <= max(1, resource.CriticalAvailableAccounts)) {
		return true
	}
	return resource.AvailableSustainMinutes > 0 && resource.AvailableSustainMinutes <= 5
}

// automaticZeroDeliveryBurst summarizes consecutive supplier attempts that
// failed to deliver any usable capacity. It is intentionally based on local
// terminal order history rather than a momentary inventory snapshot: a
// supplier can report stock briefly while rejecting every competing order.
func (s *Service) automaticZeroDeliveryBurst(
	ctx context.Context,
	cfg store.ManagerSupplyConfig,
	latest store.SupplyOrder,
) (failures int, cancellations int, err error) {
	if s == nil || s.store == nil || !latest.Automatic || !automaticOrderHasNoDeliveredCapacity(latest) {
		return 0, 0, nil
	}
	latestTerminalAtMS := smartSupplyOrderTerminalAtMS(latest)
	if latestTerminalAtMS <= 0 {
		return 0, 0, nil
	}
	orders, err := s.recentSupplyOrders(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, order := range orders {
		if !order.Automatic || !supplyProductConfigured(cfg, order.Product) ||
			!isSupplierPurchaseHistoryOrder(order) {
			continue
		}
		terminalAtMS := smartSupplyOrderTerminalAtMS(order)
		if terminalAtMS <= 0 || terminalAtMS > latestTerminalAtMS ||
			latestTerminalAtMS-terminalAtMS > automaticRetryBurstWindow.Milliseconds() {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(order.Status)) {
		case "completed", "released":
			// The burst is consecutive. A realized delivery resets the failure
			// streak even when older cancelled rows are still inside the window.
			if automaticOrderDeliveredQuantity(order) > 0 {
				return failures, cancellations, nil
			}
		case "cancelled", "failed", "dismissed":
			if automaticOrderDeliveredQuantity(order) > 0 {
				continue
			}
			failures++
			if strings.EqualFold(strings.TrimSpace(order.Status), "cancelled") {
				cancellations++
			}
		}
	}
	return failures, cancellations, nil
}

// automaticRetryPlan applies an escalating local backoff after a supplier
// failure burst. The pure planner remains at the normal 10-second retry for a
// first failure; repeated zero-delivery attempts progressively back off to
// avoid flooding the supplier and creating a long cancelled-order tail.
func (s *Service) automaticRetryPlan(
	ctx context.Context,
	cfg store.ManagerSupplyConfig,
	resource SmartResource,
	order store.SupplyOrder,
	now time.Time,
) (smartEmergencyRetryPlan, error) {
	plan := smartEmergencyRetryPlanForOrder(cfg, resource, order, now)
	if !plan.active {
		return plan, nil
	}
	failures, cancellations, err := s.automaticZeroDeliveryBurst(ctx, cfg, order)
	if err != nil {
		return smartEmergencyRetryPlan{}, err
	}
	if smartSupplyUrgentRetryRequired(resource) {
		plan.cooldown = automaticRetryCycleCooldown(cfg, resource)
		return plan, nil
	}
	switch {
	case failures >= 5 || cancellations >= 4:
		plan.cooldown = 5 * time.Minute
	case failures >= 3 || cancellations >= 2:
		plan.cooldown = 2 * time.Minute
	case failures >= 2:
		plan.cooldown = time.Minute
	}
	if smartSupplyShortageRetryCadenceRequired(resource) {
		// Failure history changes quantity selection and keeps the segmented
		// ladder active; it must not introduce a 1/2/5-minute local pause while
		// the pool is critical or the supplier is tight/scarce. Repeating the
		// bounded wave at the configured cadence increases total抢号 coverage
		// without turning every individual order into one oversized request.
		plan.cooldown = min(plan.cooldown, automaticRetryCycleCooldown(cfg, resource))
	}
	return plan, nil
}

// automaticParallelCreateBlocked is retained as a policy hook for callers and
// metrics. The bounded open-order window is the actual competition limit; a
// zero-delivery cancellation should keep that window full rather than closing
// it behind a long local backoff.
func (s *Service) automaticParallelCreateBlocked(ctx context.Context, cfg store.ManagerSupplyConfig) (bool, error) {
	// Keep the competition window full after cancellations. The hard
	// maxConcurrentSupplyOrders limit and aggregate take admission are the
	// over-capacity guards. A historical zero-delivery burst is a reason to keep
	// competing for stock, not a reason to stop trying altogether.
	return false, nil
}

// automaticImmediateRetryEligible identifies a supplier reservation that was
// cancelled without securing any capacity. It deliberately excludes paid or
// partially delivered orders: those still require normal reconciliation and
// aggregate capacity admission before another purchase is considered.
func automaticImmediateRetryEligible(cfg store.ManagerSupplyConfig, order store.SupplyOrder) bool {
	if !order.Automatic || !supplyProductConfigured(cfg, order.Product) ||
		strings.EqualFold(strings.TrimSpace(order.Strategy), "recovery") ||
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(order.OrderID)), "recovery-") ||
		order.ChargedFen > 0 || order.ReadyQuantity > 0 || order.ItemCount > 0 || order.ImportedCount > 0 {
		return false
	}
	return automaticOrderCancellationConfirmed(order)
}

// automaticOrderCancellationConfirmed is stricter than the local status. A
// transport conflict or uncertain create may close a local row, but only an
// explicit supplier cancelled/canceled terminal response with no payment,
// account or import evidence is allowed to unlock the next reservation.
func automaticOrderCancellationConfirmed(order store.SupplyOrder) bool {
	if !strings.EqualFold(strings.TrimSpace(order.Status), "cancelled") ||
		localOrderStatus(order.RemoteStatus) != "cancelled" ||
		smartSupplyOrderTerminalAtMS(order) <= 0 || strings.TrimSpace(order.LastError) != "" {
		return false
	}
	return order.ChargedFen <= 0 && order.ReadyQuantity <= 0 && order.ItemCount <= 0 && order.ImportedCount <= 0
}

// automaticRetryLadderQuantities returns one bounded competition cycle. The
// original requirement remains the base and the only retry quantities are half
// and one fifth: 10 -> 5 -> 2. Rounded duplicates are removed for small orders.
func automaticRetryLadderQuantities(baseQuantity int) []int {
	if baseQuantity <= 0 {
		return nil
	}
	raw := []int{
		baseQuantity,
		int(math.Ceil(float64(baseQuantity) / 2)),
		int(math.Ceil(float64(baseQuantity) / 5)),
	}
	quantities := make([]int, 0, len(raw))
	seen := make(map[int]struct{}, len(raw))
	for _, quantity := range raw {
		quantity = clampInt(quantity, 1, baseQuantity)
		if _, ok := seen[quantity]; ok {
			continue
		}
		seen[quantity] = struct{}{}
		quantities = append(quantities, quantity)
	}
	return quantities
}

// automaticRetryLadderQuantity returns the rung following failureCount
// confirmed cancellations. Zero means the cycle is exhausted.
func automaticRetryLadderQuantity(baseQuantity, failureCount int) int {
	quantities := automaticRetryLadderQuantities(baseQuantity)
	if failureCount < 0 || failureCount >= len(quantities) {
		return 0
	}
	return quantities[failureCount]
}

type automaticRetryLadderState struct {
	baseQuantity        int
	nextQuantity        int
	attemptedQuantities map[int]struct{}
}

func automaticRetryContinuationOrder(order store.SupplyOrder) bool {
	return isParallelSupplyOrder(order) || isSmartEmergencyRetryReason(order.TriggerReason)
}

// automaticRetryLadderStateForOrders attaches retries to the most recent
// non-retry anchor. A new full order after cooldown therefore starts a fresh
// cycle even while older cancellations remain in the ten-minute history.
func automaticRetryLadderStateForOrders(
	cfg store.ManagerSupplyConfig,
	latest store.SupplyOrder,
	orders []store.SupplyOrder,
) automaticRetryLadderState {
	state := automaticRetryLadderState{attemptedQuantities: make(map[int]struct{}, 3)}
	if !automaticImmediateRetryEligible(cfg, latest) {
		return state
	}
	latestTerminalAtMS := smartSupplyOrderTerminalAtMS(latest)
	started := false
	add := func(order store.SupplyOrder) bool {
		if !order.Automatic || !supplyProductConfigured(cfg, order.Product) ||
			!isSupplierPurchaseHistoryOrder(order) {
			return true
		}
		terminalAtMS := smartSupplyOrderTerminalAtMS(order)
		if terminalAtMS <= 0 || terminalAtMS > latestTerminalAtMS ||
			latestTerminalAtMS-terminalAtMS > automaticRetryBurstWindow.Milliseconds() {
			return false
		}
		if automaticOrderDeliveredQuantity(order) > 0 || !automaticOrderCancellationConfirmed(order) {
			return false
		}
		if order.RequestedQuantity > 0 {
			state.baseQuantity = max(state.baseQuantity, order.RequestedQuantity)
			state.attemptedQuantities[order.RequestedQuantity] = struct{}{}
		}
		return automaticRetryContinuationOrder(order)
	}
	for _, order := range orders {
		if !started {
			if order.OrderID != latest.OrderID {
				continue
			}
			started = true
		}
		if !add(order) {
			break
		}
	}
	if !started {
		_ = add(latest)
	}
	for _, quantity := range automaticRetryLadderQuantities(state.baseQuantity) {
		if _, attempted := state.attemptedQuantities[quantity]; !attempted {
			state.nextQuantity = quantity
			break
		}
	}
	return state
}

func (s *Service) automaticRetryLadderState(
	ctx context.Context,
	cfg store.ManagerSupplyConfig,
	latest store.SupplyOrder,
) (automaticRetryLadderState, error) {
	orders, err := s.recentSupplyOrders(ctx)
	if err != nil {
		return automaticRetryLadderState{}, err
	}
	return automaticRetryLadderStateForOrders(cfg, latest, orders), nil
}

func (s *Service) automaticImmediateRetryQuantity(
	ctx context.Context,
	cfg store.ManagerSupplyConfig,
	latest store.SupplyOrder,
	calculatedQuantity int,
) (int, error) {
	if calculatedQuantity <= 0 || !automaticImmediateRetryEligible(cfg, latest) {
		return calculatedQuantity, nil
	}
	state, err := s.automaticRetryLadderState(ctx, cfg, latest)
	if err != nil {
		return 0, err
	}
	if state.nextQuantity <= 0 {
		return 0, nil
	}
	return min(calculatedQuantity, state.nextQuantity), nil
}

func automaticOrderHasNoDeliveredCapacity(order store.SupplyOrder) bool {
	switch strings.ToLower(strings.TrimSpace(order.Status)) {
	case "failed", "cancelled", "dismissed", "released":
		return true
	}
	return automaticOrderDeliveredQuantity(order) <= 0
}

func automaticOrderDeliveredQuantity(order store.SupplyOrder) int {
	if order.ImportedCount > 0 {
		return order.ImportedCount
	}
	if order.ItemCount > 0 {
		return order.ItemCount
	}
	if order.ReadyQuantity > 0 {
		return order.ReadyQuantity
	}
	if strings.EqualFold(strings.TrimSpace(order.Status), "completed") && order.RequestedQuantity > 0 {
		return order.RequestedQuantity
	}
	return 0
}

func recentAutomaticOrderCoversCurrentShortage(
	cfg store.ManagerSupplyConfig,
	resource SmartResource,
	order store.SupplyOrder,
) bool {
	if !order.Automatic || strings.EqualFold(strings.TrimSpace(order.Strategy), "recovery") ||
		!isSupplierPurchaseHistoryOrder(order) || automaticOrderHasNoDeliveredCapacity(order) {
		return false
	}

	required := math.Max(0, resource.CapacityGapRCU)
	unit := smartEstimatedNewAccountCapacityForResource(cfg, resource)
	if unit <= 0 && resource.UnitCapacityRCU > 0 {
		unit = smartEstimatedAccountCapacityRCU(resource.UnitCapacityRCU, float64(smartCapacityPlanningHorizonMinutes(cfg)))
	}
	accountDeficit := max(0, resource.AccountQuantityDeficit)
	accountDeficit = max(accountDeficit, max(0, resource.ConcurrencyAccountDeficit))
	if unit > 0 && accountDeficit > 0 {
		required = math.Max(required, float64(accountDeficit)*unit)
	}
	if required <= 0 {
		return true
	}
	if resource.PrelockedCapacityRCU >= required {
		return true
	}
	deliveredCapacity := estimatedSupplyOrderCapacityRCU(cfg, resource, automaticOrderDeliveredQuantity(order))
	return deliveredCapacity >= required
}

func (s *Service) applySmartEmergencyRetryPlan(ctx context.Context, cfg store.ManagerSupplyConfig, resource *SmartResource) (smartEmergencyRetryPlan, error) {
	if s == nil || s.store == nil || resource == nil || !smartShortageFastRetryAllowed(*resource) {
		return smartEmergencyRetryPlan{}, nil
	}
	latest, found, err := s.store.GetLatestAutomaticSupplyOrder(ctx)
	if err != nil || !found {
		return smartEmergencyRetryPlan{}, err
	}
	plan, err := s.automaticRetryPlan(ctx, cfg, *resource, latest, time.Now())
	if err != nil {
		return smartEmergencyRetryPlan{}, err
	}
	if !plan.active {
		return plan, nil
	}
	quantity := s.smartSuggestedCreateQuantity(cfg, *resource)
	if quantity <= 0 {
		quantity = resource.SuggestedQuantity
	}
	resource.SuggestedQuantity = clampInt(quantity, 1, plan.quantityLimit)
	resource.DecisionReason = plan.reason
	applySmartRefillProjection(cfg, resource)
	return plan, nil
}

func smartEmergencyRetryPlanForOrder(cfg store.ManagerSupplyConfig, resource SmartResource, order store.SupplyOrder, now time.Time) smartEmergencyRetryPlan {
	if !smartShortageFastRetryAllowed(resource) || !order.Automatic ||
		!supplyProductConfigured(cfg, order.Product) ||
		strings.EqualFold(strings.TrimSpace(order.Strategy), "recovery") ||
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(order.OrderID)), "recovery-") ||
		order.ChargedFen > 0 || order.ItemCount > 0 || order.ImportedCount > 0 || order.ReadyQuantity > 0 {
		return smartEmergencyRetryPlan{}
	}
	terminalAtMS := smartSupplyOrderTerminalAtMS(order)
	if terminalAtMS <= 0 {
		return smartEmergencyRetryPlan{}
	}
	age := now.Sub(time.UnixMilli(terminalAtMS))
	if age < 0 || age > 10*time.Minute {
		return smartEmergencyRetryPlan{}
	}

	if !automaticOrderCancellationConfirmed(order) {
		return smartEmergencyRetryPlan{}
	}
	reason := "emergency_retry_after_cancelled"

	limit := smartAutomaticOrderQuantityLimit(cfg, resource)
	return smartEmergencyRetryPlan{active: true, reason: reason, quantityLimit: limit, cooldown: 10 * time.Second}
}

func smartShortageFastRetryAllowed(resource SmartResource) bool {
	if smartResourceEmergency(resource) {
		return true
	}
	if !resource.SnapshotFresh || resource.DemandTrend == smartDemandTrendFalling {
		return false
	}
	return resource.CapacityGapRCU > 0 || resource.AccountQuantityDeficit > 0 ||
		resource.ConcurrencyAccountDeficit > 0 || smartResourceAtOrBelowWarning(resource)
}

func smartAutomaticFailureBlocksFastRetry(lastError string) bool {
	value := strings.ToLower(strings.TrimSpace(lastError))
	if value == "" {
		return false
	}
	for _, marker := range []string{
		"http 400", "http 401", "http 402", "http 403", "http 404", "http 409", "http 422",
		"balance", "余额", "unauthorized", "forbidden", "password", "username", "credential",
		"invalid product", "invalid quantity", "token missing", "token expired",
	} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func smartSupplyOrderTerminalAtMS(order store.SupplyOrder) int64 {
	if order.CompletedAtMS > 0 {
		return order.CompletedAtMS
	}
	return maxInt64(order.UpdatedAtMS, order.CreatedAtMS)
}

func isSmartEmergencyRetryReason(reason string) bool {
	reason = unwrappedSupplyTriggerReason(reason)
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(reason)), "emergency_retry_") &&
		!strings.EqualFold(strings.TrimSpace(reason), "emergency_retry_cooldown")
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
	if smartResourceEmergency(resource) {
		return max(1, min(seconds, smartCreateCooldownForResource(cfg, resource)))
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

func (s *Service) armAutomaticBaseline() int64 {
	if s == nil {
		return 0
	}
	s.automaticGuardMu.Lock()
	defer s.automaticGuardMu.Unlock()
	s.automaticEnabled = true
	s.automaticBaselineAtMS = time.Now().UnixMilli()
	s.automaticAccountAtMS = 0
	return s.automaticBaselineAtMS
}

func (s *Service) observeAutomaticEnabled(enabled bool) (int64, bool) {
	if s == nil {
		return 0, false
	}
	s.automaticGuardMu.Lock()
	defer s.automaticGuardMu.Unlock()
	if !enabled {
		s.automaticEnabled = false
		s.automaticBaselineAtMS = 0
		s.automaticAccountAtMS = 0
		return 0, false
	}
	if !s.automaticEnabled || s.automaticBaselineAtMS <= 0 {
		s.automaticEnabled = true
		s.automaticBaselineAtMS = time.Now().UnixMilli()
		s.automaticAccountAtMS = 0
		return s.automaticBaselineAtMS, true
	}
	return s.automaticBaselineAtMS, false
}

func (s *Service) markAutomaticAccountSnapshot() {
	if s == nil {
		return
	}
	s.automaticGuardMu.Lock()
	if s.automaticEnabled && s.automaticBaselineAtMS > 0 {
		s.automaticAccountAtMS = time.Now().UnixMilli()
	}
	s.automaticGuardMu.Unlock()
}

func (s *Service) automaticBaselineBlockReason(resource SmartResource) string {
	if s == nil {
		return ""
	}
	s.automaticGuardMu.Lock()
	baselineAtMS := s.automaticBaselineAtMS
	accountAtMS := s.automaticAccountAtMS
	enabled := s.automaticEnabled
	s.automaticGuardMu.Unlock()
	if !enabled || baselineAtMS <= 0 {
		return ""
	}
	if accountAtMS < baselineAtMS {
		return "initial_account_snapshot_pending"
	}
	// A process restart must not invalidate a completed, still-fresh quota
	// snapshot. Requiring CapacitySnapshotAtMS >= baselineAtMS made every deploy
	// wait for another full-pool inspection even though live account counts were
	// already refreshed and the existing quota evidence was valid. That can
	// strand a critical pool for many minutes. Missing/stale evidence still
	// blocks, and imported accounts remain protected by the separate pending
	// inspection guard below.
	if !resource.SnapshotFresh || resource.CapacitySnapshotAtMS <= 0 {
		if smartInspectionSnapshotRefreshNeeded(resource) {
			s.requestStaleInspectionSnapshotRefresh()
		}
		// A recent incomplete inspection still provides a verified capacity lower
		// bound. Do not strand a live deficit merely because the remainder of the
		// pool is still being inspected after a Manager restart. The partial path
		// stays bounded by smartAutomaticOrderQuantityLimit, while a genuine live
		// account emergency is sized by the independently refreshed account pool.
		if smartPartialInspectionCapacityDeficitAllowed(resource) ||
			smartResourceEmergency(resource) || smartAccountAvailabilityEmergency(resource) {
			return ""
		}
		return "initial_capacity_snapshot_pending"
	}
	return ""
}

func (s *Service) automaticSupplyGuardReason(resource SmartResource) string {
	if resource.QuotaEstimateOrderingBlocked {
		// Quota calibration is advisory for ordering. A rejected or still-pending
		// observation is already replaced by the configured no-data fallback in
		// the resource calculation, so keep inspecting without stranding supply.
		s.requestQuotaEstimateCalibrationRefresh()
	}
	// Quota divergence remains a visible staged calibration but does not pause
	// ordering. Keep collecting independent inspection rounds in the background
	// so the adopted value can converge without operator action.
	if resource.QuotaEstimatePendingPlans > 0 {
		s.requestQuotaEstimateCalibrationRefresh()
	}
	if reason := s.automaticBaselineBlockReason(resource); reason != "" {
		return reason
	}
	if resource.PendingInspectionAccounts > 0 {
		s.requestStaleInspectionSnapshotRefresh()
		// The latest completed inspection remains the verified historical base,
		// while buildSmartResourceFromInspectionSnapshot overlays newly imported
		// files with the persisted supplier/plan capacity estimate. That local
		// delta is enough to make the next replenishment decision immediately;
		// the background inspection only upgrades confidence and must not turn one
		// imported account into a full-pool stop-the-world barrier.
		if resource.SnapshotFresh &&
			resource.CapacitySource == smartCapacitySourceInspection &&
			resource.PendingInspectionCapacityRCU > 0 {
			return ""
		}
		// Pending inspection must not strand a critical pool. The emergency
		// quantity remains bounded by the verified live deficit and all persisted
		// cooldown/in-flight-order guards still apply.
		if smartResourceEmergency(resource) || smartAccountAvailabilityEmergency(resource) {
			return ""
		}
		return "pending_account_inspection"
	}
	return ""
}

func (s *Service) hasInspectionSnapshotRefresher() bool {
	if s == nil {
		return false
	}
	s.inspectionSnapshotRefreshMu.Lock()
	configured := s.inspectionSnapshotRefresh.refresh != nil
	s.inspectionSnapshotRefreshMu.Unlock()
	return configured
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
	if smartResourceEmergency(resource) {
		return true
	}
	if resource.SuggestedAction == smartActionTakeLocked {
		switch resource.DecisionReason {
		case "critical_take_confirmed", "critical_take_confirmed_stale_lower_bound", "supply_plenty_small_take", "low_water_take_ready", "low_water_take_ready_stale_lower_bound", "purchase_task_ready_stale_snapshot", "purchase_task_ready_target_remaining", "paid_ready_order_take":
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
	configured := 0
	for _, platform := range supplyPlatforms(cfg) {
		if !supplyPlatformConfigured(platform) {
			continue
		}
		parsed, err := url.Parse(platform.BaseURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("account supply platform %s base URL is invalid", platform.ID)
		}
		if !supplyProductSupportedByPlatform(platform, platform.Product) {
			return fmt.Errorf("account supply platform %s product is invalid", platform.ID)
		}
		configured++
	}
	if configured == 0 {
		return ErrNotConfigured
	}
	return nil
}

func (s *Service) updateOrderError(ctx context.Context, order *store.SupplyOrder, err error, cfg store.ManagerSupplyConfig) error {
	order.LastError = safeError(err)
	if retryAtMS := supplierRetryAtMS(err); retryAtMS > 0 {
		order.NextPollAtMS = retryAtMS
		order.SupplierRetryUntilMS = retryAtMS
	} else if order.Status == "taking" {
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
	s.invalidateStatusCache()
}

func (s *Service) beginAutomaticRunDecision() {
	if s == nil {
		return
	}
	s.stateMu.Lock()
	s.automaticDecisionSet = false
	s.automaticDecision = SmartResource{}
	s.stateMu.Unlock()
}

func (s *Service) recordAutomaticRunDecision(resource SmartResource) {
	if s == nil {
		return
	}
	s.stateMu.Lock()
	s.automaticDecisionSet = true
	s.automaticDecision = resource
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
	s.automation.LastStartedAtMS = startedAt.UnixMilli()
	s.automation.LastFinishedAtMS = finishedAt.UnixMilli()
	s.automation.NextExecutionAtMS = nextAt.UnixMilli()
	s.automation.IntervalSeconds = max(0, int(math.Ceil(nextAt.Sub(finishedAt).Seconds())))
	// Dashboard/status refreshes rebuild smartResourceState asynchronously. Use
	// the decision captured from this exact automatic run so a later read-only
	// refresh cannot make a blocked cycle look like it created an order (or hide
	// an order that the cycle actually enqueued).
	decision := s.smartResourceState
	if s.automaticDecisionSet {
		decision = s.automaticDecision
	}
	s.automation.LastAction = decision.SuggestedAction
	s.automation.LastReason = decision.DecisionReason
	if err != nil {
		if sqliterepo.IsBusyError(err) {
			s.automation.LastResult = "scheduled"
			s.automation.LastError = ""
			s.stateMu.Unlock()
			s.invalidateStatusCache()
			return
		}
		if isTransientSupplyAPIError(err) {
			s.automation.LastResult = "retrying"
			s.automation.LastError = safeError(err)
			s.stateMu.Unlock()
			s.invalidateStatusCache()
			return
		}
		s.automation.LastResult = "failed"
		s.automation.LastError = safeError(err)
		s.stateMu.Unlock()
		s.invalidateStatusCache()
		return
	}
	switch decision.DecisionReason {
	case "supplier_price_above_ceiling":
		s.automation.LastResult = "price_wait"
		s.automation.LastError = ""
		s.stateMu.Unlock()
		s.invalidateStatusCache()
		return
	case "supplier_quota_gate_wait":
		s.automation.LastResult = "quota_wait"
		s.automation.LastError = ""
		s.stateMu.Unlock()
		s.invalidateStatusCache()
		return
	}
	s.automation.LastResult = "completed"
	s.automation.LastError = ""
	s.stateMu.Unlock()
	s.invalidateStatusCache()
}

func automaticSupplyWaitDecision(err error) (action string, reason string, ok bool) {
	if _, priceWait := marketplacePriceWaitError(err); priceWait {
		return smartActionPriceWait, "supplier_price_above_ceiling", true
	}
	if errors.Is(err, ErrSupplierQuotaGateNoEligibleSeller) {
		return smartActionSupplierGateWait, "supplier_quota_gate_wait", true
	}
	return "", "", false
}

func (s *Service) NextAutomaticInterval(ctx context.Context, runErr error) time.Duration {
	return automaticIntervalWithRunError(s.NextInterval(ctx), runErr)
}

func automaticIntervalWithRunError(interval time.Duration, runErr error) time.Duration {
	if isTransientSupplyAPIError(runErr) && interval < automaticTransientRetryBackoff {
		return automaticTransientRetryBackoff
	}
	return interval
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
	enabled := recoverySyncEnabled(cfg) && recoverySupplyPlatformConfigured(cfg)
	s.recoveryMu.Lock()
	summary := s.recoveryState
	s.recoveryMu.Unlock()
	summary.Enabled = enabled
	summary.AutoClaim = recoveryAutoClaimEnabled(cfg)
	if s != nil && s.store != nil {
		if stored, err := s.store.SupplyRecoverySummary(ctx); err == nil {
			summary.Total = stored.Total
			summary.External = stored.External
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
	if sqliterepo.IsBusyError(err) {
		summary.LastResult = "scheduled"
		summary.LastError = ""
	} else {
		summary.LastResult = "failed"
		summary.LastError = safeError(err)
	}
	s.recoveryMu.Lock()
	s.recoveryState = summary
	s.recoveryMu.Unlock()
	s.invalidateStatusCache()
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

func (s *Service) ensureCPAAccountImported(ctx context.Context, cfg store.ManagerConfig, fileName string, payload []byte, importAction string, account normalizedSupplyAccount) error {
	find := func(findCtx context.Context) (cpaauthfiles.File, error) {
		file, found, err := s.authFiles.Find(findCtx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey, fileName, "")
		if err != nil {
			return cpaauthfiles.File{}, err
		}
		if !found {
			return cpaauthfiles.File{}, errors.New("CPA did not register the imported auth file")
		}
		provider := strings.ToLower(strings.TrimSpace(file.Provider))
		if provider != "codex" && provider != "openai-codex" {
			return file, fmt.Errorf("CPA registered imported auth file with unsupported provider %q", provider)
		}
		if !isAvailableCodexFile(file) {
			message := textField(file.Raw, "status_message", "statusMessage", "last_error", "lastError", "error")
			if message != "" {
				return file, fmt.Errorf("CPA registered imported auth file but it is not available: %s", message)
			}
			return file, errors.New("CPA registered imported auth file but it is not available")
		}
		return file, nil
	}
	existingFound := false
	if existing, found, err := s.authFiles.Find(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey, fileName, ""); err != nil {
		return err
	} else if found {
		existingFound = true
		matchesAccount := supplyCPAFileMatchesAccount(existing, account)
		if strings.EqualFold(strings.TrimSpace(importAction), "add") && !matchesAccount {
			return fmt.Errorf("CPA auth file %q already belongs to another account", fileName)
		}
		if matchesAccount {
			if _, err := find(ctx); err == nil {
				return nil
			}
			if isCPAAuthLifecyclePending(existing) {
				return s.waitForCPAAuthLifecycle(ctx, find)
			}
		}
	}
	if existingFound || strings.EqualFold(strings.TrimSpace(importAction), "replace") {
		existingPayload, errDownload := s.authFiles.Download(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey, fileName)
		if errDownload == nil {
			payload = preserveCodexSupplyMetadata(payload, existingPayload)
		} else if !errors.Is(errDownload, cpaauthfiles.ErrAuthFileNotFound) {
			return fmt.Errorf("preserve CPA auth file metadata %q: %w", fileName, errDownload)
		}
	}
	if err := s.authFiles.Upload(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey,
		fileName, payload, cfg.Supply.DefaultWebsockets); err != nil {
		return err
	}
	return s.waitForCPAAuthLifecycle(ctx, find)
}

const cpaAuthLifecycleWaitTimeout = 90 * time.Second

const terminalCPAAuthUnavailableMessage = "CPA auth file is terminally unavailable"

func (s *Service) waitForCPAAuthLifecycle(ctx context.Context, find func(context.Context) (cpaauthfiles.File, error)) error {
	if find == nil {
		return errors.New("CPA auth lifecycle lookup is unavailable")
	}
	waitCtx, cancel := context.WithTimeout(ctx, cpaAuthLifecycleWaitTimeout)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastErr error
	for {
		file, err := find(waitCtx)
		if err == nil {
			return nil
		}
		lastErr = err
		if file.Name != "" {
			if lifecycleErr := terminalCPAAuthLifecycleError(file); lifecycleErr != nil {
				return lifecycleErr
			}
			if !isCPAAuthLifecyclePending(file) {
				return fmt.Errorf("%s: name=%q status=%q disabled=%t: %w", terminalCPAAuthUnavailableMessage,
					file.Name, textField(file.Raw, "status", "state"), file.Disabled, err)
			}
		}
		select {
		case <-waitCtx.Done():
			if lastErr != nil {
				return fmt.Errorf("CPA auth initialization did not become ready: %w", lastErr)
			}
			return waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func terminalCPAAuthLifecycleError(file cpaauthfiles.File) error {
	message := textField(file.Raw,
		"status_message", "statusMessage",
		"initialization_error", "initializationError",
		"recovery_error", "recoveryError",
		"last_error", "lastError", "error",
	)
	if !permanentCPAAuthLifecycleFailure(message) {
		return nil
	}
	return errors.New(message)
}

func permanentCPAAuthLifecycleFailure(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, marker := range []string{
		"refresh_token_invalidated",
		"invalid_grant",
		"session has ended",
		"log in again",
		"oauth token was revoked",
		"token was revoked or invalidated",
		"workspace is deactivated",
		"workspace_deactivated",
		"deactivated_workspace",
		"usage endpoint returned 402",
		"quota endpoint returned 402",
		"usage_limit_reached",
		"quota_exhausted",
		"insufficient_quota",
		"billing_hard_limit",
		"hard_limit_reached",
		"credit_grant_exhausted",
		"exceeded your current quota",
		strings.ToLower(terminalCPAAuthUnavailableMessage),
	} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func isCPAAuthLifecyclePending(file cpaauthfiles.File) bool {
	for _, status := range []string{
		strings.ToLower(textField(file.Raw, "status", "state")),
		strings.ToLower(textField(file.Raw, "initialization_state", "initializationState")),
		strings.ToLower(textField(file.Raw, "recovery_state", "recoveryState")),
	} {
		switch status {
		case "initializing", "refreshing_token", "refreshing_quota", "initialization_failed",
			"recovering_token", "recovering_quota", "recovery_failed":
			return true
		}
	}
	if file.Name == "" || file.Disabled || !isCodexAuthFile(file) || terminalCPAAuthLifecycleError(file) != nil {
		return false
	}
	status := strings.ToLower(textField(file.Raw, "status", "state"))
	switch status {
	case "disabled", "inactive", "invalid", "expired", "revoked", "deleted":
		return false
	}
	return !isAvailableCodexFile(file)
}

type supplyImportPlan struct {
	fileName         string
	action           string
	replacedFileName string
}

func (s *Service) resolveSupplyImportPlan(ctx context.Context, cfg store.ManagerConfig, order store.SupplyOrder, item store.SupplyImportItem, account normalizedSupplyAccount, allowRecoveryOriginal bool) (supplyImportPlan, error) {
	if strings.EqualFold(strings.TrimSpace(order.Strategy), "recovery") {
		if recovery, found, err := s.store.GetSupplyRecoveryByClaimOrder(ctx, order.OrderID); err != nil {
			return supplyImportPlan{}, err
		} else if found {
			if allowRecoveryOriginal {
				if plan, ok, err := s.resolveRecoveryOriginalPlan(ctx, cfg, recovery); err != nil || ok {
					return plan, err
				}
			}
			bindings, err := s.store.ListCurrentSupplyImportItemsByNameKey(ctx, account.nameKey)
			if err != nil {
				return supplyImportPlan{}, err
			}
			if binding, ok := matchRecoveryBinding(bindings, recovery, account); ok {
				return s.planForBoundFile(ctx, cfg, binding.FileName)
			}
		}
	}

	bindings, err := s.store.ListCurrentSupplyImportItemsByItemKey(ctx, account.itemKey)
	if err != nil {
		return supplyImportPlan{}, err
	}
	if len(bindings) > 0 {
		return s.planForBoundFile(ctx, cfg, bindings[0].FileName)
	}

	bindings, err = s.store.ListCurrentSupplyImportItemsByNameKey(ctx, account.nameKey)
	if err != nil {
		return supplyImportPlan{}, err
	}
	for _, binding := range bindings {
		if binding.ItemKey == account.itemKey {
			return s.planForBoundFile(ctx, cfg, binding.FileName)
		}
	}
	return s.planForCandidateFile(ctx, cfg, account.fileName, account)
}

func (s *Service) resolveRecoveryOriginalPlan(ctx context.Context, cfg store.ManagerConfig, recovery store.SupplyRecovery) (supplyImportPlan, bool, error) {
	originalFileName := strings.TrimSpace(recovery.OriginalFileName)
	if safeSupplyAuthFileName(originalFileName) {
		file, found, err := s.findCPAAuthFile(ctx, cfg, originalFileName, "")
		if err != nil {
			return supplyImportPlan{}, false, err
		}
		if found {
			return supplyImportPlan{fileName: file.Name, action: "replace", replacedFileName: file.Name}, true, nil
		}
	}
	if authIndex := strings.TrimSpace(recovery.OriginalAuthIndex); authIndex != "" {
		file, found, err := s.findCPAAuthFile(ctx, cfg, "", authIndex)
		if err != nil {
			return supplyImportPlan{}, false, err
		}
		if found && safeSupplyAuthFileName(file.Name) {
			return supplyImportPlan{fileName: file.Name, action: "replace", replacedFileName: file.Name}, true, nil
		}
	}
	if safeSupplyAuthFileName(originalFileName) {
		return supplyImportPlan{fileName: originalFileName, action: "add"}, true, nil
	}
	return supplyImportPlan{}, false, nil
}

func (s *Service) planForBoundFile(ctx context.Context, cfg store.ManagerConfig, fileName string) (supplyImportPlan, error) {
	fileName = strings.TrimSpace(fileName)
	file, found, err := s.findCPAAuthFile(ctx, cfg, fileName, "")
	if err != nil {
		return supplyImportPlan{}, err
	}
	if found {
		return supplyImportPlan{fileName: file.Name, action: "replace", replacedFileName: file.Name}, nil
	}
	return supplyImportPlan{fileName: fileName, action: "add"}, nil
}

func (s *Service) planForCandidateFile(ctx context.Context, cfg store.ManagerConfig, candidate string, account normalizedSupplyAccount) (supplyImportPlan, error) {
	for attempt := 0; attempt < 100; attempt++ {
		fileName := candidate
		if attempt == 1 {
			fileName = supplyAccountFileNameWithIdentity(account.accountName, account.itemKey)
		} else if attempt > 1 {
			base := strings.TrimSuffix(supplyAccountFileNameWithIdentity(account.accountName, account.itemKey), ".json")
			fileName = fmt.Sprintf("%s-%d.json", base, attempt)
		}
		file, found, err := s.findCPAAuthFile(ctx, cfg, fileName, "")
		if err != nil {
			return supplyImportPlan{}, err
		}
		if !found {
			return supplyImportPlan{fileName: fileName, action: "add"}, nil
		}
		if supplyCPAFileMatchesAccount(file, account) {
			return supplyImportPlan{fileName: file.Name, action: "replace", replacedFileName: file.Name}, nil
		}
	}
	return supplyImportPlan{}, errors.New("could not allocate a unique CPA auth file name")
}

func (s *Service) findCPAAuthFile(ctx context.Context, cfg store.ManagerConfig, fileName string, authIndex string) (cpaauthfiles.File, bool, error) {
	file, found, err := s.authFiles.Find(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey, fileName, authIndex)
	if err != nil || found {
		return file, found, err
	}
	var matched cpaauthfiles.File
	err = s.authFiles.Visit(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey, func(candidate cpaauthfiles.File) (bool, error) {
		if fileName != "" && candidate.Name != strings.TrimSpace(fileName) {
			return false, nil
		}
		if authIndex != "" && candidate.AuthIndex != strings.TrimSpace(authIndex) {
			return false, nil
		}
		matched = candidate
		return true, nil
	})
	if err != nil {
		return cpaauthfiles.File{}, false, err
	}
	return matched, strings.TrimSpace(matched.Name) != "", nil
}

func matchRecoveryBinding(bindings []store.SupplyImportItem, recovery store.SupplyRecovery, account normalizedSupplyAccount) (store.SupplyImportItem, bool) {
	if len(bindings) == 1 {
		return bindings[0], true
	}
	originalEmail := strings.ToLower(strings.TrimSpace(recovery.OriginalEmail))
	for _, binding := range bindings {
		if binding.ItemKey == account.itemKey {
			return binding, true
		}
		if originalEmail == "" {
			continue
		}
		normalized, err := normalizeAccountForImport(binding.PayloadJSON)
		if err != nil {
			continue
		}
		var metadata map[string]any
		if json.Unmarshal(normalized.payload, &metadata) == nil && strings.EqualFold(stringFromMap(metadata, "email"), originalEmail) {
			return binding, true
		}
	}
	return store.SupplyImportItem{}, false
}

func supplyCPAFileMatchesAccount(file cpaauthfiles.File, account normalizedSupplyAccount) bool {
	var metadata map[string]any
	if json.Unmarshal(account.payload, &metadata) != nil {
		return false
	}
	expected := supplyAccountIdentityPartsFromMetadata(metadata)
	actualMetadata := cloneMap(file.Raw)
	setStringIfEmpty(actualMetadata, "account_id", file.AccountID)
	setStringIfEmpty(actualMetadata, "email", file.AccountSnapshot)
	normalizeCodexPayloadAliases(actualMetadata)
	actual := supplyAccountIdentityPartsFromMetadata(actualMetadata)
	return supplyAccountIdentityPartsMatch(expected, actual)
}

func safeSupplyAuthFileName(fileName string) bool {
	fileName = strings.TrimSpace(fileName)
	return fileName != "" && strings.HasSuffix(strings.ToLower(fileName), ".json") &&
		!strings.ContainsAny(fileName, `/\\`) && fileName != "." && fileName != ".."
}

const supplyFileNameReconcileBatchSize = 200

func (s *Service) backfillSupplyAccountMetadata(ctx context.Context, cfg store.ManagerConfig) error {
	items, err := s.store.ListSupplyImportItems(ctx, 5000, "imported")
	if err != nil {
		return err
	}
	if err := s.migrateNvtokensWarrantyMetadata(ctx, cfg, items); err != nil {
		return err
	}
	currentFileUsers := make(map[string]int)
	for _, item := range items {
		if item.SupersededAtMS > 0 {
			continue
		}
		if fileName := strings.TrimSpace(item.FileName); fileName != "" {
			currentFileUsers[fileName]++
		}
	}

	inspected := 0
	changed := false
	var reconcileErrors []error
	for _, item := range items {
		if item.SupersededAtMS > 0 {
			continue
		}
		account, err := normalizeAccountForImport(item.PayloadJSON)
		if err != nil {
			continue
		}
		if looksLikeSupplyAccountEmail(item.AccountName) && !looksLikeSupplyAccountEmail(account.accountName) {
			account = withSupplyAccountName(account, item.AccountName)
		}

		fileName := strings.TrimSpace(item.FileName)
		legacyFileName := ""
		if (fileName != account.fileName || !looksLikeSupplyAccountEmail(account.accountName)) && inspected < supplyFileNameReconcileBatchSize {
			inspected++
			migratedFileName, reconciledAccount, migrated, migrateErr := s.reconcileSupplyAuthFileName(ctx, cfg, item, account, currentFileUsers)
			account = reconciledAccount
			if migrateErr != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("reconcile supply import %d: %w", item.ID, migrateErr))
			} else if migrated {
				legacyFileName = fileName
				fileName = migratedFileName
				changed = true
			}
		}

		if account.accountName == item.AccountName && account.nameKey == item.NameKey && fileName == item.FileName {
			continue
		}
		importAction := strings.ToLower(strings.TrimSpace(item.ImportAction))
		if importAction != "add" && importAction != "replace" {
			importAction = "add"
		}
		if err := s.store.UpdateSupplyImportItemPlan(ctx, item.ID, account.accountName, account.nameKey, fileName, importAction, item.ReplacedFileName); err != nil {
			return err
		}
		if legacyFileName != "" {
			if err := s.authFiles.Delete(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey, legacyFileName); err != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("delete migrated supply auth file %q: %w", legacyFileName, err))
			}
		}
		changed = true
	}
	if changed {
		s.invalidateAuthAndCapacityCaches()
	}
	if len(reconcileErrors) > 0 {
		return errors.Join(reconcileErrors...)
	}
	return nil
}

func (s *Service) migrateNvtokensWarrantyMetadata(ctx context.Context, cfg store.ManagerConfig, items []store.SupplyImportItem) error {
	if s == nil || s.store == nil || len(items) == 0 {
		return nil
	}
	orders, err := s.supplyOrdersForItems(ctx, nil, items)
	if err != nil {
		return err
	}
	changed := false
	for index := range items {
		item := &items[index]
		if item.SupersededAtMS > 0 || item.LeaseExpiresAtMS <= 0 || item.WarrantyExpiresAtMS > 0 {
			continue
		}
		order := orders[item.OrderID]
		if !supplyOrderUsesNvtokensPlatform(cfg.Supply, order) {
			continue
		}
		warrantyExpiresAtMS := item.LeaseExpiresAtMS
		if err := s.rewriteNvtokensWarrantyAuthFile(ctx, cfg, *item, warrantyExpiresAtMS); err != nil {
			return fmt.Errorf("migrate nvtokens warranty for %q: %w", item.FileName, err)
		}
		if err := s.store.UpdateSupplyImportItemWarrantyMetadata(ctx, item.ID, 0, warrantyExpiresAtMS); err != nil {
			return err
		}
		item.LeaseExpiresAtMS = 0
		item.WarrantyExpiresAtMS = warrantyExpiresAtMS
		changed = true
	}
	if changed {
		s.invalidateAuthAndCapacityCaches()
	}
	return nil
}

func supplyOrderUsesNvtokensPlatform(cfg store.ManagerSupplyConfig, order store.SupplyOrder) bool {
	if strings.TrimSpace(order.OrderID) == "" {
		return false
	}
	supplierID := strings.TrimSpace(order.SupplierID)
	for _, platform := range managerconfigsvc.SupplyPlatforms(cfg) {
		if strings.EqualFold(strings.TrimSpace(platform.ID), supplierID) {
			return strings.EqualFold(strings.TrimSpace(platform.Type), managerconfigsvc.SupplyPlatformNvtokens)
		}
	}
	return false
}

func (s *Service) rewriteNvtokensWarrantyAuthFile(ctx context.Context, cfg store.ManagerConfig, item store.SupplyImportItem, warrantyExpiresAtMS int64) error {
	fileName := strings.TrimSpace(item.FileName)
	if warrantyExpiresAtMS <= 0 || !safeSupplyAuthFileName(fileName) ||
		strings.TrimSpace(cfg.CPAConnection.CPABaseURL) == "" || strings.TrimSpace(cfg.CPAConnection.ManagementKey) == "" {
		return nil
	}
	payload, err := s.authFiles.Download(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey, fileName)
	if errors.Is(err, cpaauthfiles.ErrAuthFileNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var metadata map[string]any
	if json.Unmarshal(payload, &metadata) != nil {
		return errors.New("CPA auth file is not a JSON object")
	}
	currentWarranty := int64(numberField(metadata, "supply_warranty_expires_at_ms", "supplyWarrantyExpiresAtMs"))
	currentLease := int64(numberField(metadata, "supply_lease_expires_at_ms", "supplyLeaseExpiresAtMs"))
	if currentWarranty == warrantyExpiresAtMS && currentLease <= 0 &&
		stringFromMap(metadata, "supply_lease_expires_at", "supplyLeaseExpiresAt") == "" {
		return nil
	}
	deleteSupplyLeaseMetadata(metadata)
	metadata["supply_warranty_expires_at_ms"] = warrantyExpiresAtMS
	metadata["supply_warranty_expires_at"] = time.UnixMilli(warrantyExpiresAtMS).UTC().Format(time.RFC3339)
	normalized, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	return s.authFiles.Upload(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey, fileName, normalized, cfg.Supply.DefaultWebsockets)
}

// reconcileSupplyAuthFileName migrates a legacy supplier-labelled auth file to
// the canonical account-labelled filename. The old file is deleted only after
// the new file is uploaded and verified against the account identity.
func (s *Service) reconcileSupplyAuthFileName(ctx context.Context, cfg store.ManagerConfig, item store.SupplyImportItem, account normalizedSupplyAccount, currentFileUsers map[string]int) (string, normalizedSupplyAccount, bool, error) {
	oldFileName := strings.TrimSpace(item.FileName)
	if oldFileName == "" || !safeSupplyAuthFileName(oldFileName) || !strings.HasPrefix(strings.ToLower(oldFileName), "codex-") {
		return oldFileName, account, false, nil
	}
	if strings.TrimSpace(cfg.CPAConnection.CPABaseURL) == "" || strings.TrimSpace(cfg.CPAConnection.ManagementKey) == "" {
		return oldFileName, account, false, nil
	}
	oldFile, found, err := s.findCPAAuthFile(ctx, cfg, oldFileName, "")
	if err != nil {
		return oldFileName, account, false, err
	}
	if !found {
		return oldFileName, account, false, nil
	}
	account = supplyAccountWithCPAIdentity(account, oldFile)
	if !supplyCPAFileMatchesAccount(oldFile, account) {
		return oldFileName, account, false, nil
	}
	if currentFileUsers[oldFileName] > 1 {
		return oldFileName, account, false, fmt.Errorf("legacy auth file %q is referenced by %d current imports", oldFileName, currentFileUsers[oldFileName])
	}

	targetFileName, targetFile, targetFound, err := s.findCanonicalSupplyAuthFile(ctx, cfg, account)
	if err != nil {
		return oldFileName, account, false, err
	}
	if targetFileName == oldFileName {
		return oldFileName, account, false, nil
	}
	if !targetFound {
		if err := s.authFiles.Upload(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey,
			targetFileName, []byte(account.payload), cfg.Supply.DefaultWebsockets); err != nil {
			return oldFileName, account, false, err
		}
		verified, verifiedFound, verifyErr := s.findCPAAuthFile(ctx, cfg, targetFileName, "")
		if verifyErr != nil {
			return oldFileName, account, false, verifyErr
		}
		if !verifiedFound || !supplyCPAFileMatchesAccount(verified, account) {
			return oldFileName, account, false, fmt.Errorf("uploaded auth file %q failed identity verification", targetFileName)
		}
		targetFile = verified
	}
	if oldFile.Disabled && !targetFile.Disabled {
		if err := s.authFiles.PatchDisabled(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey, targetFileName, true, targetFile.AuthIndex); err != nil {
			return oldFileName, account, false, err
		}
	}
	return targetFileName, account, true, nil
}

func supplyAccountWithCPAIdentity(account normalizedSupplyAccount, file cpaauthfiles.File) normalizedSupplyAccount {
	identity := firstNonEmptyString(
		textField(file.Raw, "email", "account", "username", "display_account", "displayAccount"),
		file.AccountSnapshot,
	)
	if !looksLikeSupplyAccountEmail(identity) {
		return account
	}
	account = withSupplyAccountName(account, identity)
	var metadata map[string]any
	if json.Unmarshal(account.payload, &metadata) == nil {
		setString(metadata, "email", identity)
		setString(metadata, "account", identity)
		if normalized, err := json.Marshal(metadata); err == nil {
			account.payload = normalized
		}
	}
	return account
}

func withSupplyAccountName(account normalizedSupplyAccount, accountName string) normalizedSupplyAccount {
	account.accountName = strings.TrimSpace(accountName)
	account.nameKey = supplyAccountNameKey(account.accountName, account.workspaceKey)
	account.fileName = stableSupplyAccountFileName(account.accountName, account.workspaceKey)
	return account
}

func looksLikeSupplyAccountEmail(value string) bool {
	value = strings.TrimSpace(value)
	at := strings.LastIndexByte(value, '@')
	return at > 0 && at < len(value)-1 && strings.Contains(value[at+1:], ".")
}

func (s *Service) findCanonicalSupplyAuthFile(ctx context.Context, cfg store.ManagerConfig, account normalizedSupplyAccount) (string, cpaauthfiles.File, bool, error) {
	baseName := stableSupplyAccountFileName(account.accountName, account.workspaceKey)
	candidates := []string{baseName, supplyAccountFileNameWithIdentity(account.accountName, account.itemKey)}
	for attempt := 2; attempt < 100; attempt++ {
		base := strings.TrimSuffix(candidates[1], ".json")
		candidates = append(candidates, fmt.Sprintf("%s-%d.json", base, attempt))
	}
	for _, candidate := range candidates {
		// Canonical candidates are exact filenames. Avoid the compatibility
		// fallback that scans the entire auth-file collection for every missing
		// candidate during a bulk migration.
		file, found, err := s.authFiles.Find(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey, candidate, "")
		if err != nil {
			return "", cpaauthfiles.File{}, false, err
		}
		if !found {
			return candidate, cpaauthfiles.File{}, false, nil
		}
		if supplyCPAFileMatchesAccount(file, account) {
			return file.Name, file, true, nil
		}
	}
	return "", cpaauthfiles.File{}, false, errors.New("could not allocate a canonical CPA auth file name")
}

func (s *Service) invalidateAuthFilesCache() {
	if s == nil {
		return
	}
	// Keep the lock order aligned with ListAccounts (account-list cache before
	// auth-file cache); import completion can happen while an account query is
	// reading CPA state, so reversing this order would deadlock the two paths.
	s.accountListCacheMu.Lock()
	s.accountListCache = supplyAccountListCache{}
	s.accountListCacheMu.Unlock()
	s.authCacheMu.Lock()
	s.authCache = authFileSnapshot{}
	s.authCacheMu.Unlock()
	s.operatorPoolMu.Lock()
	s.operatorPoolGeneration++
	s.operatorPoolMu.Unlock()
}

func (s *Service) recordError(err error) {
	if err == nil {
		return
	}
	// Price ceilings and supplier-evidence pauses are expected automatic wait
	// decisions, not operator-actionable failures. The automatic execution
	// snapshot already exposes their typed result/reason; keeping the same text
	// in overview.lastError makes a healthy price_wait cycle look broken.
	if _, _, waiting := automaticSupplyWaitDecision(err); waiting {
		s.stateMu.Lock()
		s.overview.LastError = ""
		s.stateMu.Unlock()
		s.invalidateStatusCache()
		return
	}
	if sqliterepo.IsBusyError(err) {
		log.Printf("[supply] automatic check deferred while SQLite writer is busy")
		return
	}
	s.stateMu.Lock()
	s.overview.LastError = safeError(err)
	s.stateMu.Unlock()
	s.invalidateStatusCache()
}

func sanitizeConfig(cfg store.ManagerSupplyConfig) store.ManagerSupplyConfig {
	cfg.PasswordConfigured = strings.TrimSpace(cfg.Password) != ""
	cfg.Password = ""
	for index := range cfg.Platforms {
		cfg.Platforms[index].PasswordConfigured = strings.TrimSpace(cfg.Platforms[index].Password) != ""
		cfg.Platforms[index].TokenConfigured = strings.TrimSpace(cfg.Platforms[index].Token) != ""
		cfg.Platforms[index].ChallengeAPIKeyConfigured = strings.TrimSpace(cfg.Platforms[index].ChallengeAPIKey) != ""
		cfg.Platforms[index].Password = ""
		cfg.Platforms[index].Token = ""
		cfg.Platforms[index].ChallengeAPIKey = ""
		cfg.Platforms[index].ClearChallengeAPIKey = false
	}
	return cfg
}

func credentialsFromConfig(cfg store.ManagerSupplyConfig) supplyclient.Credentials {
	platform, err := resolveSupplyPlatform(cfg, "", cfg.Product)
	if err != nil {
		return supplyclient.Credentials{BaseURL: cfg.BaseURL, Username: cfg.Username, Password: cfg.Password}
	}
	return supplyPlatformCredentials(platform)
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
	var upstreamErr *supplyclient.HTTPError
	if errors.As(err, &upstreamErr) && upstreamErr.RetryAfterSeconds > 0 {
		seconds := clampInt(upstreamErr.RetryAfterSeconds, 1, 3600)
		return time.Duration(seconds) * time.Second
	}
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

func recoveryNextSyncInterval(cfg store.ManagerSupplyConfig, err error, autoClaim bool, remainingClaimable int) time.Duration {
	interval := recoverySyncInterval(cfg, err)
	if err != nil || !autoClaim || remainingClaimable <= 0 {
		return interval
	}
	const backlogInterval = 3 * time.Second
	if interval > backlogInterval {
		return backlogInterval
	}
	return interval
}

func supplyRecoveryFromClient(remote supplyclient.Recovery, supplierID ...string) store.SupplyRecovery {
	status := supplyRecoveryStatus(remote)
	originalFileName := strings.TrimSpace(remote.OriginalAccount)
	if !strings.HasSuffix(strings.ToLower(originalFileName), ".json") {
		originalFileName = ""
	}
	resolvedSupplierID := ""
	if len(supplierID) > 0 {
		resolvedSupplierID = strings.TrimSpace(supplierID[0])
	}
	return store.SupplyRecovery{
		RecoveryID:        strings.TrimSpace(remote.ID),
		SupplierID:        resolvedSupplierID,
		Product:           strings.TrimSpace(remote.Product),
		DeliveryStatus:    strings.ToLower(strings.TrimSpace(remote.DeliveryStatus)),
		Status:            status,
		SourceOrderID:     strings.TrimSpace(remote.SourceOrderID),
		OriginalFileName:  originalFileName,
		OriginalAuthIndex: strings.TrimSpace(remote.OriginalAuthIndex),
		OriginalEmail:     strings.TrimSpace(remote.OriginalEmail),
		CredentialVersion: remote.CredentialVersion,
		ClaimURL:          strings.TrimSpace(remote.ClaimURL),
		ClaimTicket:       strings.TrimSpace(remote.ClaimTicket),
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
	return smartResourceEmergency(resource)
}

func (s *Service) cancelOrder(ctx context.Context, order *store.SupplyOrder, err error) error {
	order.Status = "cancelled"
	order.RemoteStatus = "cancelled"
	order.LastError = safeError(err)
	order.NextPollAtMS = 0
	order.CompletedAtMS = time.Now().UnixMilli()
	return s.store.UpdateSupplyOrder(ctx, *order)
}

// NV's paid purchase ledger is authoritative even when an order-shaped route
// returns 404. A charged order with ready quantity must remain open for ledger
// reconciliation; marking it cancelled would allow another purchase while the
// original account is still sitting in the supplier workspace.
func nvtokensPaidOrderLookupUncertain(platform store.ManagerSupplyPlatformConfig, order store.SupplyOrder) bool {
	return strings.EqualFold(strings.TrimSpace(platform.Type), managerconfigsvc.SupplyPlatformNvtokens) &&
		supplyOrderHasPaymentEvidence(order) &&
		(order.ReadyQuantity > 0 || order.Progress >= 100 || isReadyForTake(order.RemoteStatus)) &&
		order.ReleasedFen < order.ChargedFen && order.ItemCount == 0 && order.ImportedCount == 0
}

// failUndeliverableOrder keeps a paid or fulfilled supplier delivery open for
// operator reconciliation. Once the supplier has returned stock, reported
// success, or exposed a charge, the safe assumption is that value left the
// wallet. Releasing the purchase-task slot here would submit another paid order
// while the original credential merely needs a parser/import repair.
func (s *Service) failUndeliverableOrder(ctx context.Context, order *store.SupplyOrder, err error) error {
	if order == nil {
		return err
	}
	if supplyOrderHasPaymentEvidence(*order) {
		order.Status = "partial"
		order.RemoteStatus = "paid_delivery_unparsed"
		order.LastError = safeError(err)
		order.SupplierRetryUntilMS = 0
		order.NextPollAtMS = time.Now().Add(24 * time.Hour).UnixMilli()
		order.CompletedAtMS = 0
		if updateErr := s.store.UpdateSupplyOrder(ctx, *order); updateErr != nil {
			return updateErr
		}
		return err
	}
	order.Status = "failed"
	order.RemoteStatus = "invalid_payload"
	order.LastError = safeError(err)
	order.NextPollAtMS = 0
	order.SupplierRetryUntilMS = 0
	order.CompletedAtMS = time.Now().UnixMilli()
	if updateErr := s.store.UpdateSupplyOrder(ctx, *order); updateErr != nil {
		return updateErr
	}
	return err
}

func supplyOrderHasPaymentEvidence(order store.SupplyOrder) bool {
	return order.ChargedFen > 0 || order.ReadyQuantity > 0 || order.Progress >= 100 ||
		order.ItemCount > 0 || isSuccessfulRemoteStatus(order.RemoteStatus) || isReadyForTake(order.RemoteStatus)
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
		http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

func supplierRetryAtMS(err error) int64 {
	var upstreamErr *supplyclient.HTTPError
	if !errors.As(err, &upstreamErr) || upstreamErr.RetryAfterSeconds <= 0 {
		return 0
	}
	return time.Now().Add(time.Duration(upstreamErr.RetryAfterSeconds) * time.Second).UnixMilli()
}

func nextSupplierRetryAt(cfg store.ManagerSupplyConfig, err error) int64 {
	if retryAtMS := supplierRetryAtMS(err); retryAtMS > 0 {
		return retryAtMS
	}
	return nextPollAt(cfg, 0)
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
	case "ready", "ready_for_pickup", "ready_partial", "partial_ready", "partially_ready":
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
	return status == "ready" || status == "ready_for_pickup" || status == "ready_partial" ||
		status == "partial_ready" || status == "partially_ready" || isSuccessfulRemoteStatus(status)
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
		seconds = supplyPollIntervalSeconds(cfg)
	}
	if seconds <= 0 {
		seconds = int(maxActiveOrderPollInterval / time.Second)
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
	payload             []byte
	itemKey             string
	accountName         string
	nameKey             string
	workspaceKey        string
	fileName            string
	leaseExpiresAtMS    int64
	warrantyExpiresAtMS int64
	basePriceFen        int64
	chargedFen          int64
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
	case string:
		decoded, err := decodeSupplyAccountPayload(json.RawMessage(strconv.Quote(strings.TrimSpace(typed))))
		if err != nil {
			return nil, err
		}
		return normalizeSupplyAccountValue(decoded, inheritedExportedAt)
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
	for _, key := range []string{
		"account_json", "accountJson",
		"sub2api_account", "sub2apiAccount",
		"codex_account", "codexAccount",
	} {
		if child, exists := object[key]; exists && child != nil {
			return []any{child}, exportedAt, true
		}
	}
	for _, key := range []string{"card_payload", "cardPayload", "payload", "data", "result"} {
		if child, ok := object[key].(map[string]any); ok {
			if list, childExportedAt, found := nestedSupplyAccountList(child, exportedAt); found {
				return list, childExportedAt, true
			}
			if key == "card_payload" || key == "cardPayload" {
				return []any{child}, exportedAt, true
			}
		} else if child, exists := object[key]; exists && child != nil && (key == "card_payload" || key == "cardPayload") {
			return []any{child}, exportedAt, true
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
	// Supplier payloads may advertise a conservative per-account concurrency
	// value (for example `concurrency: 1`). That value describes the supplier's
	// own pool, not CPA's runtime scheduler. Never persist it into the imported
	// auth file, otherwise it becomes a hard max_concurrency and overrides the
	// configured cache-affinity limit.
	removeSupplierConcurrencyFields(metadata)
	// Supplier-side priority tiers describe how the marketplace routed its own
	// inventory. CLIProxyAPI always selects the highest credential priority
	// bucket first, so preserving that value can leave healthy purchased
	// accounts completely idle while a few higher-priority accounts absorb all
	// traffic. CPA-managed imports must enter one shared scheduling tier.
	delete(metadata, "priority")
	resetSupplyImportRuntimeState(metadata)
	enrichCodexIdentityFromTokens(metadata)
	pinSupplyCodexPlanType(metadata)
	// Supplier-managed pool accounts must remain immediately selectable after
	// account-level or model-selection errors. An explicit zero is required:
	// CPA otherwise applies its implicit 30-second freeze whenever another
	// runtime-limit field (for example max_concurrency) is present.
	metadata["selection_error_freeze_seconds"] = 0
	metadata["codex_cli_only"] = true
	// Supplier-managed Team credentials serve both official Codex clients and
	// CPA's already-authenticated gateway path. CLIProxyAPI requires an internal
	// post-authentication proof before this per-account exception is considered,
	// so a caller cannot activate it with copied HTTP headers.
	metadata["codex_cli_only_allow_app_server"] = true

	identity := supplyAccountIdentity(metadata)
	if identity == "" {
		return normalizedSupplyAccount{}, errors.New("stable account identity is missing")
	}
	if strings.TrimSpace(stringFromMap(metadata, "codex_identity_fingerprint")) == "" {
		metadata["codex_identity_fingerprint"] = stableCodexIdentityFingerprint(identity)
	}

	normalized, err := json.Marshal(metadata)
	if err != nil {
		return normalizedSupplyAccount{}, err
	}
	sum := sha256.Sum256([]byte(identity))
	digest := hex.EncodeToString(sum[:])
	// The supplier's `name` is a product label such as "普通 Team · 7D ·
	// 有效期 51 分钟", not the account identity. Prefer the stable customer
	// identity shown by CPA so filenames remain useful to operators and stable
	// when the supplier changes its product/TTL description.
	accountName := firstNonEmptyString(
		stringFromMap(metadata, "email", "email_address", "emailAddress"),
		stringFromMap(metadata, "account_id", "chatgpt_account_id"),
		stringFromMap(metadata, "name"),
		"OpenAI OAuth Account",
	)
	workspaceKey := supplyAccountWorkspaceKey(metadata)
	nameKey := supplyAccountNameKey(accountName, workspaceKey)
	return normalizedSupplyAccount{
		payload:          normalized,
		itemKey:          digest,
		accountName:      accountName,
		nameKey:          nameKey,
		workspaceKey:     workspaceKey,
		fileName:         stableSupplyAccountFileName(accountName, workspaceKey),
		leaseExpiresAtMS: supplyDeliveryLeaseExpiresAtMS(object, time.Now()),
	}, nil
}

func stableSupplyAccountFileName(accountName string, workspaceKeys ...string) string {
	base := "codex-" + safeSupplyFileComponent(accountName)
	workspaceKey := ""
	if len(workspaceKeys) > 0 {
		workspaceKey = strings.TrimSpace(workspaceKeys[0])
	}
	if workspaceKey == "" {
		return base + ".json"
	}
	sum := sha256.Sum256([]byte(strings.ToLower(workspaceKey)))
	return base + "-space-" + hex.EncodeToString(sum[:4]) + ".json"
}

func supplyAccountFileNameWithIdentity(accountName string, itemKey string) string {
	digest := strings.ToLower(strings.TrimSpace(itemKey))
	if len(digest) > 12 {
		digest = digest[:12]
	}
	if digest == "" {
		sum := sha256.Sum256([]byte(strings.TrimSpace(accountName)))
		digest = hex.EncodeToString(sum[:6])
	}
	return "codex-" + safeSupplyFileComponent(accountName) + "-" + digest + ".json"
}

func supplyAccountNameKey(accountName string, workspaceKeys ...string) string {
	nameKey := strings.ToLower(safeSupplyFileComponent(accountName))
	if len(workspaceKeys) == 0 || strings.TrimSpace(workspaceKeys[0]) == "" {
		return nameKey
	}
	return nameKey + "|" + strings.ToLower(strings.TrimSpace(workspaceKeys[0]))
}

func supplyAccountWorkspaceKey(metadata map[string]any) string {
	return firstNonEmptyString(
		stringFromMap(metadata, "workspace_id", "workspaceId", "chatgpt_workspace_id", "chatgptWorkspaceId", "workspace"),
		stringFromMap(metadata, "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId"),
		stringFromMap(metadata, "organization_id", "organizationId", "org_id", "orgId", "poid"),
	)
}

func safeSupplyFileComponent(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasSuffix(strings.ToLower(value), ".json") {
		value = strings.TrimSpace(value[:len(value)-len(".json")])
	}
	var builder strings.Builder
	lastSeparator := false
	for _, r := range strings.ToLower(value) {
		allowed := unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("@._+-", r)
		if allowed {
			if (r == '.' || r == '-' || r == '_') && builder.Len() == 0 {
				continue
			}
			builder.WriteRune(r)
			lastSeparator = r == '-'
		} else if builder.Len() > 0 && !lastSeparator {
			builder.WriteByte('-')
			lastSeparator = true
		}
		if len([]rune(builder.String())) >= 80 {
			break
		}
	}
	result := strings.Trim(builder.String(), ".-_ ")
	if result == "" {
		result = "account"
	}
	switch result {
	case "con", "prn", "aux", "nul", "com1", "com2", "com3", "com4", "com5", "com6", "com7", "com8", "com9", "lpt1", "lpt2", "lpt3", "lpt4", "lpt5", "lpt6", "lpt7", "lpt8", "lpt9":
		result += "-account"
	}
	return result
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

// applySupplyOrderItemDetails returns true only for an exact, ordered mapping
// between supplier order items and CPA import files. A bundled delivery may
// expand one supplied payload into many files, so a partial mapping would make
// a valid account appear expired or receive another account's cost.
func applySupplyOrderItemDetails(accounts []normalizedSupplyAccount, items []supplyclient.OrderItem, now time.Time, nvtokens ...bool) bool {
	if len(accounts) == 0 || len(accounts) != len(items) {
		return false
	}
	warrantyOnly := len(nvtokens) > 0 && nvtokens[0]
	for index := range accounts {
		if warrantyOnly {
			accounts[index].leaseExpiresAtMS = 0
		}
		if items[index].HasRemaining {
			expiresAtMS := supplyDeliveryLeaseExpiresAtMSFromSeconds(items[index].RemainingSeconds, now)
			if warrantyOnly {
				accounts[index].warrantyExpiresAtMS = expiresAtMS
			} else {
				accounts[index].leaseExpiresAtMS = expiresAtMS
			}
		}
		accounts[index].basePriceFen = items[index].BasePriceFen
		accounts[index].chargedFen = items[index].ChargedFen
	}
	return true
}

func supplyOrderItemsChargedFen(items []supplyclient.OrderItem) int64 {
	var total int64
	for _, item := range items {
		if item.ChargedFen > 0 {
			total += item.ChargedFen
		}
	}
	return total
}

// applySupplyOrderItemLeases returns true only for an exact, ordered mapping
// between supplier order items and CPA import files. A bundled delivery may
// expand one supplied payload into many files, so a partial mapping would make
// a valid account appear expired (or vice versa).
func applySupplyOrderItemLeases(accounts []normalizedSupplyAccount, remainingSeconds []int64, now time.Time) bool {
	if len(accounts) == 0 || len(accounts) != len(remainingSeconds) {
		return false
	}
	items := make([]supplyclient.OrderItem, 0, len(remainingSeconds))
	for _, seconds := range remainingSeconds {
		items = append(items, supplyclient.OrderItem{RemainingSeconds: seconds, HasRemaining: true})
	}
	return applySupplyOrderItemDetails(accounts, items, now)
}

func normalizeAccountPayloadForImport(payloadJSON string) ([]byte, string, string, error) {
	account, err := normalizeAccountForImport(payloadJSON)
	if err != nil {
		return nil, "", "", err
	}
	return account.payload, account.itemKey, account.fileName, nil
}

func normalizeAccountForImport(payloadJSON string) (normalizedSupplyAccount, error) {
	accounts, err := normalizeAccountPayloads(json.RawMessage(strings.TrimSpace(payloadJSON)))
	if err != nil {
		return normalizedSupplyAccount{}, err
	}
	if len(accounts) != 1 {
		return normalizedSupplyAccount{}, fmt.Errorf("expected one supply account payload, got %d", len(accounts))
	}
	return accounts[0], nil
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

	identitySources := []map[string]any{metadata, extra, account}
	email := stringFromMaps(identitySources, "email", "email_address", "emailAddress")
	accountID := stringFromMaps(identitySources, "chatgpt_account_id", "chatgptAccountId", "account_id", "accountId")
	userID := stringFromMaps(identitySources, "chatgpt_user_id", "chatgptUserId", "user_id", "userId")
	accountUserID := stringFromMaps(identitySources, "chatgpt_account_user_id", "chatgptAccountUserId")
	organizationID := stringFromMaps(identitySources, "organization_id", "organizationId", "org_id", "orgId", "poid")
	workspaceID := stringFromMaps(identitySources, "workspace_id", "workspaceId", "chatgpt_workspace_id", "chatgptWorkspaceId", "workspace")
	workspaceName := stringFromMaps(identitySources, "workspace_name", "workspaceName", "organization_name", "organizationName", "team_name", "teamName")
	planType := resolveSupplyPlanType(metadata, extra, account)
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
	setString(metadata, "chatgpt_account_user_id", accountUserID)
	setString(metadata, "organization_id", organizationID)
	setString(metadata, "workspace_id", workspaceID)
	setString(metadata, "workspace_name", workspaceName)
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
	copyOptionalSupplyField(metadata, account, "proxy_url", "proxy_url")
	copyOptionalSupplyField(metadata, account, "proxy_url", "proxyUrl")
	copyOptionalSupplyField(metadata, extra, "proxy_url", "proxy_url")
	copyOptionalSupplyField(metadata, extra, "proxy_url", "proxyUrl")
	copyOptionalSupplyField(metadata, extra, "websockets", "websockets")
	copyOptionalSupplyField(metadata, extra, "openai_oauth_responses_websockets_v2_enabled", "openai_oauth_responses_websockets_v2_enabled")
	enrichCodexIdentityFromTokens(metadata)
	return stripEmptyValues(metadata)
}

func resetSupplyImportRuntimeState(metadata map[string]any) {
	if metadata == nil {
		return
	}
	for _, key := range []string{
		"status", "state", "status_message", "statusMessage",
		"runtime_status", "runtimeStatus",
		"last_error", "lastError", "error",
		"error_kind", "errorKind", "header_error_kind", "headerErrorKind",
		"initialization_state", "initializationState",
		"initialization_error", "initializationError",
		"recovery_state", "recoveryState", "recovery_error", "recoveryError",
		"unavailable", "revoked", "deleted",
	} {
		delete(metadata, key)
	}
	// Supplier runtime state belongs to the export source. Every delivered OAuth
	// credential must enter CPA enabled so CPA can initialize and evaluate it.
	metadata["disabled"] = false
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
	if userID := stringFromMap(metadata, "chatgpt_user_id", "chatgptUserId", "user_id", "userId"); userID != "" {
		metadata["chatgpt_user_id"] = userID
	}
	if accountUserID := stringFromMap(metadata, "chatgpt_account_user_id", "chatgptAccountUserId"); accountUserID != "" {
		metadata["chatgpt_account_user_id"] = accountUserID
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

func pinSupplyCodexPlanType(metadata map[string]any) {
	planType := resolveSupplyPlanType(metadata)
	if planType == "" {
		return
	}
	metadata["plan_type"] = planType
	metadata["chatgpt_plan_type"] = planType
	// Supplier metadata describes the purchased workspace entitlement. A newly
	// issued ID token can temporarily report `free` while Team membership is
	// propagating, so CPA keeps this stable value as the effective plan.
	if planType != "free" {
		metadata["codex_plan_type_pinned"] = true
	}
}

func supplyAccountIdentity(metadata map[string]any) string {
	parts := supplyAccountIdentityPartsFromMetadata(metadata)
	if parts.workspaceID != "" && parts.memberID != "" {
		return "workspace:" + parts.workspaceID + "|member:" + parts.memberID
	}
	if parts.workspaceID != "" && parts.email != "" {
		return "workspace:" + parts.workspaceID + "|email:" + parts.email
	}
	if parts.organizationID != "" && parts.memberID != "" {
		return "organization:" + parts.organizationID + "|member:" + parts.memberID
	}
	if parts.memberID != "" {
		return "member:" + parts.memberID
	}
	if parts.email != "" {
		return "email:" + parts.email
	}
	if parts.accountID != "" {
		return "account:" + parts.accountID
	}
	return strings.TrimSpace(stringFromMap(metadata, "refresh_token", "access_token", "id_token"))
}

type supplyAccountIdentityParts struct {
	workspaceID    string
	accountID      string
	memberID       string
	email          string
	organizationID string
}

func supplyAccountIdentityPartsFromMetadata(metadata map[string]any) supplyAccountIdentityParts {
	accountID := normalizedSupplyIdentityValue(stringFromMap(metadata,
		"account_id", "accountId", "chatgpt_account_id", "chatgptAccountId"))
	workspaceID := normalizedSupplyIdentityValue(firstNonEmptyString(
		stringFromMap(metadata, "workspace_id", "workspaceId", "chatgpt_workspace_id", "chatgptWorkspaceId", "workspace"),
		accountID,
	))
	return supplyAccountIdentityParts{
		workspaceID: workspaceID,
		accountID:   accountID,
		memberID: normalizedSupplyIdentityValue(firstNonEmptyString(
			stringFromMap(metadata, "chatgpt_user_id", "chatgptUserId", "user_id", "userId"),
			stringFromMap(metadata, "chatgpt_account_user_id", "chatgptAccountUserId"),
		)),
		email: normalizedSupplyIdentityValue(stringFromMap(metadata,
			"email", "email_address", "emailAddress", "account")),
		organizationID: normalizedSupplyIdentityValue(stringFromMap(metadata,
			"organization_id", "organizationId", "org_id", "orgId", "poid")),
	}
}

func supplyAccountIdentityPartsMatch(expected supplyAccountIdentityParts, actual supplyAccountIdentityParts) bool {
	if expected.workspaceID != "" && expected.memberID != "" && actual.workspaceID != "" && actual.memberID != "" {
		return expected.workspaceID == actual.workspaceID && expected.memberID == actual.memberID
	}
	if expected.workspaceID != "" && expected.email != "" && actual.workspaceID != "" && actual.email != "" {
		return expected.workspaceID == actual.workspaceID && expected.email == actual.email
	}
	if expected.accountID != "" && expected.email != "" && actual.accountID != "" && actual.email != "" {
		return expected.accountID == actual.accountID && expected.email == actual.email
	}
	if expected.memberID != "" && actual.memberID != "" {
		return expected.memberID == actual.memberID
	}
	return expected.email != "" && actual.email != "" && expected.email == actual.email
}

func normalizedSupplyIdentityValue(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

const maxSupplyJWTPayloadSegmentBytes = 64 * 1024

func enrichCodexIdentityFromTokens(metadata map[string]any) {
	if len(metadata) == 0 {
		return
	}
	claims := make([]map[string]any, 0, 2)
	for _, token := range []string{
		stringFromMap(metadata, "access_token", "accessToken", "session_access_token", "sessionAccessToken"),
		stringFromMap(metadata, "id_token", "idToken"),
	} {
		if payload := decodeSupplyJWTPayload(token); payload != nil {
			claims = append(claims, payload)
		}
	}
	if len(claims) == 0 {
		return
	}
	authClaims := make([]map[string]any, 0, len(claims))
	for _, claim := range claims {
		if auth, ok := claim["https://api.openai.com/auth"].(map[string]any); ok {
			authClaims = append(authClaims, auth)
		}
	}
	accountID := firstNonEmptyString(
		stringFromMap(metadata, "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId"),
		stringFromMaps(authClaims, "chatgpt_account_id", "chatgptAccountId", "account_id", "accountId"),
	)
	memberID := firstNonEmptyString(
		stringFromMap(metadata, "chatgpt_user_id", "chatgptUserId", "user_id", "userId"),
		stringFromMaps(authClaims, "chatgpt_user_id", "chatgptUserId", "user_id", "userId"),
	)
	accountUserID := firstNonEmptyString(
		stringFromMap(metadata, "chatgpt_account_user_id", "chatgptAccountUserId"),
		stringFromMaps(authClaims, "chatgpt_account_user_id", "chatgptAccountUserId"),
	)
	organizationID := firstNonEmptyString(
		stringFromMap(metadata, "organization_id", "organizationId", "org_id", "orgId", "poid"),
		stringFromMaps(authClaims, "poid", "organization_id", "organizationId", "org_id", "orgId"),
		defaultOrganizationIDFromJWTClaims(claims),
	)
	workspaceID := firstNonEmptyString(
		stringFromMap(metadata, "workspace_id", "workspaceId", "chatgpt_workspace_id", "chatgptWorkspaceId", "workspace"),
		accountID,
	)
	email := firstNonEmptyString(
		stringFromMap(metadata, "email", "email_address", "emailAddress", "account"),
		stringFromMaps(claims, "email"),
	)
	if accountID != "" {
		metadata["account_id"] = accountID
		metadata["chatgpt_account_id"] = accountID
	}
	setStringIfEmpty(metadata, "chatgpt_user_id", memberID)
	setStringIfEmpty(metadata, "chatgpt_account_user_id", accountUserID)
	setStringIfEmpty(metadata, "organization_id", organizationID)
	setStringIfEmpty(metadata, "workspace_id", workspaceID)
	setStringIfEmpty(metadata, "email", email)
}

func decodeSupplyJWTPayload(token string) map[string]any {
	token = strings.TrimSpace(token)
	parts := strings.Split(token, ".")
	if len(parts) < 2 || parts[1] == "" || len(parts[1]) > maxSupplyJWTPayloadSegmentBytes {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil || len(payload) == 0 || len(payload) > maxSupplyJWTPayloadSegmentBytes {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return nil
	}
	return claims
}

func defaultOrganizationIDFromJWTClaims(claims []map[string]any) string {
	for _, claim := range claims {
		auth, _ := claim["https://api.openai.com/auth"].(map[string]any)
		organizations, _ := auth["organizations"].([]any)
		for _, raw := range organizations {
			organization, _ := raw.(map[string]any)
			if boolField(organization, "is_default", "isDefault") {
				if id := stringFromMap(organization, "id", "organization_id", "organizationId"); id != "" {
					return id
				}
			}
		}
		for _, raw := range organizations {
			if organization, ok := raw.(map[string]any); ok {
				if id := stringFromMap(organization, "id", "organization_id", "organizationId"); id != "" {
					return id
				}
			}
		}
	}
	return ""
}

func setStringIfEmpty(values map[string]any, key string, value string) {
	if strings.TrimSpace(stringFromMap(values, key)) == "" {
		setString(values, key, value)
	}
}

func stableCodexIdentityFingerprint(identity string) string {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return ""
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("cpa-manager-plus:codex:account-fingerprint:"+identity)).String()
}

func preserveCodexSupplyMetadata(payload []byte, existingPayload []byte) []byte {
	if len(payload) == 0 || len(existingPayload) == 0 {
		return payload
	}
	var next map[string]any
	var existing map[string]any
	if json.Unmarshal(payload, &next) != nil || json.Unmarshal(existingPayload, &existing) != nil {
		return payload
	}
	changed := false
	fingerprint := strings.TrimSpace(stringFromMap(existing,
		"codex_identity_fingerprint",
		"codex-identity-fingerprint",
		"codexIdentityFingerprint",
	))
	if fingerprint == "" {
		fingerprint = stableCodexIdentityFingerprint(supplyAccountIdentity(existing))
	}
	if fingerprint != "" {
		next["codex_identity_fingerprint"] = fingerprint
		changed = true
	}
	if supplyCodexPlanTypePinned(existing) {
		existingPlan := resolveSupplyPlanType(existing)
		nextPlan := resolveSupplyPlanType(next)
		if existingPlan != "" && existingPlan != "free" && (nextPlan == "" || nextPlan == "free") {
			next["plan_type"] = existingPlan
			next["chatgpt_plan_type"] = existingPlan
			next["codex_plan_type_pinned"] = true
			changed = true
		}
	}
	nextWarrantyExpiresAtMS := int64(numberField(next, "supply_warranty_expires_at_ms", "supplyWarrantyExpiresAtMs"))
	if nextWarrantyExpiresAtMS > 0 {
		// A supplier warranty is informational only. Never let an older scheduling
		// lease return while preserving metadata for a freshly imported NV payload.
		if int64(numberField(next, "supply_lease_expires_at_ms", "supplyLeaseExpiresAtMs")) > 0 ||
			stringFromMap(next, "supply_lease_expires_at", "supplyLeaseExpiresAt") != "" {
			deleteSupplyLeaseMetadata(next)
			changed = true
		}
	} else if nextLeaseExpiresAtMS := int64(numberField(next, "supply_lease_expires_at_ms", "supplyLeaseExpiresAtMs")); nextLeaseExpiresAtMS <= 0 {
		leaseExpiresAtMS := int64(numberField(existing, "supply_lease_expires_at_ms", "supplyLeaseExpiresAtMs"))
		if leaseExpiresAtMS > 0 {
			next["supply_lease_expires_at_ms"] = leaseExpiresAtMS
			if leaseExpiresAt := stringFromMap(existing, "supply_lease_expires_at", "supplyLeaseExpiresAt"); leaseExpiresAt != "" {
				next["supply_lease_expires_at"] = leaseExpiresAt
			}
			changed = true
		}
	}
	if nextWarrantyExpiresAtMS <= 0 {
		warrantyExpiresAtMS := int64(numberField(existing, "supply_warranty_expires_at_ms", "supplyWarrantyExpiresAtMs"))
		if warrantyExpiresAtMS > 0 {
			deleteSupplyLeaseMetadata(next)
			next["supply_warranty_expires_at_ms"] = warrantyExpiresAtMS
			if warrantyExpiresAt := stringFromMap(existing, "supply_warranty_expires_at", "supplyWarrantyExpiresAt"); warrantyExpiresAt != "" {
				next["supply_warranty_expires_at"] = warrantyExpiresAt
			}
			changed = true
		}
	}
	if !changed {
		return payload
	}
	normalized, err := json.Marshal(next)
	if err != nil {
		return payload
	}
	return normalized
}

func supplyCodexPlanTypePinned(metadata map[string]any) bool {
	if boolField(metadata, "codex_plan_type_pinned", "codexPlanTypePinned") {
		return true
	}
	// Compatibility for supplier files imported before the explicit marker was
	// introduced. Their normalized paid plan already came from the Sub2
	// workspace payload and has the same authority as newly marked files.
	planType := resolveSupplyPlanType(metadata)
	return strings.EqualFold(stringFromMap(metadata, "import_format"), "sub2api") &&
		planType != "" && planType != "free"
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

func removeSupplierConcurrencyFields(values map[string]any) {
	if len(values) == 0 {
		return
	}
	for _, key := range []string{
		"concurrency",
		"concurrency_limit",
		"concurrencyLimit",
		"max_concurrency",
		"max-concurrency",
		"maxConcurrency",
	} {
		delete(values, key)
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
	if credentialType != "" && credentialType != "oauth" && credentialType != "codex" && credentialType != "openai" {
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

func isCodexAuthFile(file cpaauthfiles.File) bool {
	provider := strings.ToLower(strings.TrimSpace(file.Provider))
	return provider == "codex" || provider == "openai-codex"
}

func isSmartCapacityCodexFile(file cpaauthfiles.File) bool {
	if file.Disabled {
		return false
	}
	if !isCodexAuthFile(file) {
		return false
	}
	if boolField(file.Raw, "disabled", "expired", "revoked", "deleted") {
		return false
	}
	status := strings.ToLower(textField(file.Raw, "status", "state"))
	switch status {
	case "disabled", "inactive", "invalid", "expired", "revoked", "deleted",
		"initializing", "refreshing_token", "refreshing_quota", "initialization_failed",
		"recovering_token", "recovering_quota", "recovery_failed":
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

func isTransientSupplyAPIError(err error) bool {
	var httpErr *supplyclient.HTTPError
	if !errors.As(err, &httpErr) || httpErr == nil {
		return false
	}
	return httpErr.StatusCode == http.StatusRequestTimeout ||
		httpErr.StatusCode == http.StatusTooEarly ||
		httpErr.StatusCode == http.StatusTooManyRequests ||
		httpErr.StatusCode >= http.StatusInternalServerError
}
