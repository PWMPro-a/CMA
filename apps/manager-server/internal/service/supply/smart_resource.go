package supply

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	collectorpkg "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/collector"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpaauthfiles"
	managerconfigsvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/usage"
)

const (
	smartActionHealthy            = "healthy"
	smartActionPrelock            = "prelock"
	smartActionWaitLocked         = "wait_locked"
	smartActionReleaseLocked      = "release_locked"
	smartActionTakeLocked         = "take_locked"
	smartActionBalanceBlocked     = "balance_blocked"
	smartActionInventoryBlocked   = "inventory_blocked"
	smartActionConfigError        = "config_error"
	smartActionSnapshotStale      = "snapshot_stale"
	smartActionManualReview       = "manual_review"
	smartActionObserveDemand      = "observe_demand"
	smartActionEmergencyReplenish = "emergency_replenish"
	smartActionPriceWait          = "price_wait"
	smartActionSupplierGateWait   = "supplier_gate_wait"

	smartHealthHealthy  = "healthy"
	smartHealthWarning  = "warning"
	smartHealthCritical = "critical"
	smartHealthUnknown  = "unknown"

	smartConfidenceHigh   = "high"
	smartConfidenceMedium = "medium"
	smartConfidenceLow    = "low"

	smartSupplyPressurePlenty  = "plenty"
	smartSupplyPressureNormal  = "normal"
	smartSupplyPressureTight   = "tight"
	smartSupplyPressureScarce  = "scarce"
	smartSupplyPressureUnknown = "unknown"

	smartCapacitySourceInspection  = "inspection_snapshot"
	smartCapacitySourceUnavailable = "unavailable"
	smartTokenCapacityMode         = "million_tokens"

	smartDemandTrendUnknown = "unknown"
	smartDemandTrendStable  = "stable"
	smartDemandTrendRising  = "rising"
	smartDemandTrendFalling = "falling"
	smartDemandTrendVirtual = "virtual"

	// Quota headers expose remaining percentages but not an absolute Token limit.
	// Keep a conservative generic fallback, while team credentials use the
	// verified 7-day baseline until an account has at least 10% valid usage data.
	smartDefaultAccountQuotaMillionTokens     = 10.0
	smartDefaultTeamAccountQuotaMillionTokens = 60.0
	smartDefaultPlusAccountQuotaMillionTokens = 160.0
	smartDefaultProAccountQuotaMillionTokens  = 3000.0
	// Only a short, recent gap between the last successful request and an empty
	// pool is treated as outage demand for emergency order sizing. Older demand
	// memory still protects the low-water state, but must not create a fresh
	// short-lived batch after traffic has genuinely gone idle.
	smartEmergencyDemandMemoryMaxAge = 5 * time.Minute
	// Repeated interrupted refresh attempts must not hide the most recent
	// completed capacity baseline. A short 20-run window can be exhausted in a
	// few minutes by a degraded SQLite/inspection loop, leaving smart supply
	// unable to recover even though a valid completed snapshot still exists.
	latestSmartInspectionSearchLimit = 100
)

type SmartResource struct {
	Enabled                      bool    `json:"enabled"`
	HealthLevel                  string  `json:"healthLevel"`
	SuggestedAction              string  `json:"suggestedAction"`
	SuggestedQuantity            int     `json:"suggestedQuantity"`
	DecisionReason               string  `json:"decisionReason"`
	Confidence                   string  `json:"confidence"`
	SnapshotFresh                bool    `json:"snapshotFresh"`
	SnapshotEvidencePartial      bool    `json:"snapshotEvidencePartial,omitempty"`
	SnapshotRefreshInProgress    bool    `json:"snapshotRefreshInProgress,omitempty"`
	SnapshotRefreshLastAttemptMS int64   `json:"snapshotRefreshLastAttemptMs,omitempty"`
	GeneratedAtMS                int64   `json:"generatedAtMs"`
	CapacitySource               string  `json:"capacitySource"`
	CapacityCoverage             float64 `json:"capacityCoverage"`
	CapacityLifetimeCoverage     float64 `json:"capacityLifetimeCoverage"`
	CapacitySnapshotAtMS         int64   `json:"capacitySnapshotAtMs"`
	CapacitySnapshotAgeSeconds   int     `json:"capacitySnapshotAgeSeconds"`
	CapacitySnapshotRunID        int64   `json:"capacitySnapshotRunId,omitempty"`
	// CapacityDeltaAtMS/CapacityDeltaAccounts describe the request-driven,
	// account-scoped quota updates layered over the last completed inspection.
	// The completed run remains the durable pool baseline; only credentials with
	// newer response headers are recalculated on the hot path.
	CapacityDeltaAtMS     int64 `json:"capacityDeltaAtMs,omitempty"`
	CapacityDeltaAccounts int   `json:"capacityDeltaAccounts,omitempty"`
	// These counts drive only the strategy waterline guard. Normal smart
	// replenishment remains capacity- and burn-rate based.
	AvailableAccounts   int `json:"availableAccounts"`
	SchedulableAccounts int `json:"schedulableAccounts"`
	HealthyAccounts     int `json:"healthyAccounts"`
	// EnabledAccounts and the four operator buckets below mirror the credential
	// page's exclusive account summary. They are reconciled from the current CPA
	// files plus the latest completed inspection and never drive capacity math.
	EnabledAccounts               int  `json:"enabledAccounts"`
	AccountClassificationObserved bool `json:"accountClassificationObserved"`
	NormalAccounts                int  `json:"normalAccounts"`
	NeedsAttentionAccounts        int  `json:"needsAttentionAccounts"`
	QuotaRiskAccounts             int  `json:"quotaRiskAccounts"`
	UnconfirmedAccounts           int  `json:"unconfirmedAccounts"`
	// AtRiskAccounts is the compatibility aggregate of the three non-normal,
	// non-disabled operator buckets.
	AtRiskAccounts   int `json:"atRiskAccounts"`
	WeakAccounts     int `json:"weakAccounts"`
	TotalAccounts    int `json:"totalAccounts"`
	DisabledAccounts int `json:"disabledAccounts"`
	FrozenAccounts   int `json:"frozenAccounts"`
	// Concurrency is an instantaneous serving constraint and remains separate
	// from quota/expiry RCU. A zero or missing per-account limit is treated as
	// unlimited; missing values stay visible so operators can distinguish an
	// explicit unlimited setting from absent metadata.
	ConcurrencyLimitedAccounts      int     `json:"concurrencyLimitedAccounts"`
	ConcurrencyUnlimitedAccounts    int     `json:"concurrencyUnlimitedAccounts"`
	ConcurrencyMissingAccounts      int     `json:"concurrencyMissingAccounts"`
	ConcurrencyFiniteSlots          int     `json:"concurrencyFiniteSlots"`
	RequiredConcurrencySlots        int     `json:"requiredConcurrencySlots"`
	ConcurrencyHeadroomSlots        int     `json:"concurrencyHeadroomSlots"`
	ConcurrencyAccountDeficit       int     `json:"concurrencyAccountDeficit"`
	ConcurrencyCoverage             float64 `json:"concurrencyCoverage"`
	ConcurrencyEffectiveCapacityRCU float64 `json:"concurrencyEffectiveCapacityRcu"`
	AverageRequestLatencyMS         float64 `json:"averageRequestLatencyMs"`
	ConcurrencyUnlimited            bool    `json:"concurrencyUnlimited"`
	ConcurrencyLimited              bool    `json:"concurrencyLimited"`
	ConcurrencyDemandObserved       bool    `json:"concurrencyDemandObserved"`
	// Newly delivered credentials are only added as a conservative provisional
	// capacity overlay until the next completed usability inspection verifies
	// them. They are deliberately separate from HealthyAccounts.
	PendingInspectionAccounts    int     `json:"pendingInspectionAccounts,omitempty"`
	PendingInspectionCapacityRCU float64 `json:"pendingInspectionCapacityRcu,omitempty"`
	EstimatedRequiredAccounts    int     `json:"estimatedRequiredAccounts,omitempty"`
	ProjectedAvailableAccounts   int     `json:"projectedAvailableAccounts,omitempty"`
	AccountQuantityDeficit       int     `json:"accountQuantityDeficit,omitempty"`
	// A supplier-managed credential can have a verified usable probe but expose
	// only its monthly quota window. The quota selector always prefers a shorter
	// window; monthly data is used only as the fallback when no short window is
	// present. The delivery lease still bounds its usable lifetime.
	LeaseEstimatedAccounts    int     `json:"leaseEstimatedAccounts,omitempty"`
	LeaseEstimatedCapacityRCU float64 `json:"leaseEstimatedCapacityRcu,omitempty"`
	TargetAvailableAccounts   int     `json:"-"`
	ConfiguredHealthyMinutes  int     `json:"configuredHealthyMinutesTarget,omitempty"`
	EffectiveHealthyMinutes   int     `json:"effectiveHealthyMinutesTarget"`
	AccountLifetimeMinutes    int     `json:"accountLifetimeMinutes"`
	EstimatedSustainMinutes   float64 `json:"estimatedSustainMinutes"`
	// EmergencyShortage marks a runway shortfall that takes precedence over
	// normal demand-trend observation.
	EmergencyShortage           bool    `json:"emergencyShortage"`
	HealthyMinutesTarget        int     `json:"healthyMinutesTarget"`
	WarningMinutes              int     `json:"warningMinutes"`
	CriticalMinutes             int     `json:"criticalMinutes"`
	RPM30M                      float64 `json:"rpm30m"`
	RPM5MPeak                   float64 `json:"rpm5mPeak"`
	TPM30M                      float64 `json:"tpm30m"`
	RPM1M                       float64 `json:"rpm1m"`
	RPM5M                       float64 `json:"rpm5m"`
	RPM10M                      float64 `json:"rpm10m"`
	TPM1M                       float64 `json:"tpm1m"`
	TPM5M                       float64 `json:"tpm5m"`
	TPM10M                      float64 `json:"tpm10m"`
	RequestDemandRCUPerMinute   float64 `json:"requestDemandRcuPerMinute"`
	TokenDemandRCUPerMinute     float64 `json:"tokenDemandRcuPerMinute"`
	DemandDriver                string  `json:"demandDriver,omitempty"`
	ConsumeRCU1M                float64 `json:"consumeRcu1m"`
	ConsumeRCU5M                float64 `json:"consumeRcu5m"`
	ConsumeRCU10M               float64 `json:"consumeRcu10m"`
	DemandTrend                 string  `json:"demandTrend"`
	DemandPlanningRCUPerMinute  float64 `json:"demandPlanningRcuPerMinute"`
	ConsumeRCUPerMinute         float64 `json:"consumeRcuPerMinute"`
	CurrentCapacityRCU          float64 `json:"currentCapacityRcu"`
	AvailableCapacityRCU        float64 `json:"availableCapacityRcu"`
	FrozenCapacityRCU           float64 `json:"frozenCapacityRcu"`
	TotalCapacityRCU            float64 `json:"totalCapacityRcu"`
	AvailableSustainMinutes     float64 `json:"availableSustainMinutes"`
	FrozenSustainMinutes        float64 `json:"frozenSustainMinutes"`
	RawCapacityRCU              float64 `json:"rawCapacityRcu,omitempty"`
	TimeLimitedCapacityRCU      float64 `json:"timeLimitedCapacityRcu,omitempty"`
	ExpiryWasteRiskRCU          float64 `json:"expiryWasteRiskRcu,omitempty"`
	RawSustainMinutes           float64 `json:"rawSustainMinutes,omitempty"`
	ExpiryLimitedSustainMinutes float64 `json:"expiryLimitedSustainMinutes,omitempty"`
	NearestExpiryAtMS           int64   `json:"nearestExpiryAtMs,omitempty"`
	NearestExpiryMinutes        float64 `json:"nearestExpiryMinutes,omitempty"`
	NextCapacityDeficitAtMS     int64   `json:"nextCapacityDeficitAtMs,omitempty"`
	// ExpiringAccounts counts currently schedulable credentials whose routing
	// timestamp or runtime validity is inside the warning window. Supplier
	// timestamps are informational and do not cap a healthy account's capacity.
	ExpiringAccounts                int     `json:"expiringAccounts"`
	ExpiringWithinMinutes           int     `json:"expiringWithinMinutes"`
	ExpiringCapacityRCU             float64 `json:"expiringCapacityRcu,omitempty"`
	TargetCapacityRCU               float64 `json:"targetCapacityRcu"`
	CapacityGapRCU                  float64 `json:"capacityGapRcu"`
	UnitCapacityRCU                 float64 `json:"unitCapacityRcu"`
	RecommendedCapacityRCU          float64 `json:"recommendedCapacityRcu"`
	PrelockedCapacityRCU            float64 `json:"prelockedCapacityRcu,omitempty"`
	ProjectedCapacityAfterRefillRCU float64 `json:"projectedCapacityAfterRefillRcu,omitempty"`
	ProjectedSustainAfterRefillMin  float64 `json:"projectedSustainAfterRefillMinutes,omitempty"`
	SupplyPressureLevel             string  `json:"supplyPressureLevel,omitempty"`
	SupplyPressureReason            string  `json:"supplyPressureReason,omitempty"`
	SupplyInventoryAvailable        int     `json:"supplyInventoryAvailable,omitempty"`
	SupplyInventoryMissing          int     `json:"supplyInventoryMissing,omitempty"`
	SupplyInventoryMinRemainMinutes float64 `json:"supplyInventoryMinRemainingMinutes,omitempty"`
	SupplyInventoryMaxRemainMinutes float64 `json:"supplyInventoryMaxRemainingMinutes,omitempty"`
	SupplyNeedsProduction           bool    `json:"supplyNeedsProduction,omitempty"`
	SupplyAvgFulfillSeconds         int     `json:"supplyAvgFulfillSeconds,omitempty"`
	SupplyRecentWaiting             int     `json:"supplyRecentWaiting,omitempty"`
	SupplyRecentOrders              int     `json:"supplyRecentOrders,omitempty"`
	SupplyRecentCancelled           int     `json:"supplyRecentCancelled,omitempty"`
	SupplyRecentZeroDelivery        int     `json:"supplyRecentZeroDelivery,omitempty"`
	SupplyRecentRequestedQuantity   int     `json:"supplyRecentRequestedQuantity,omitempty"`
	SupplyRecentDeliveredQuantity   int     `json:"supplyRecentDeliveredQuantity,omitempty"`
	SupplyFulfillmentRate           float64 `json:"supplyFulfillmentRate,omitempty"`
	SupplyReliable                  bool    `json:"supplyReliable,omitempty"`
	SupplyRecovering                bool    `json:"supplyRecovering,omitempty"`
	SupplyRecentSuccessStreak       int     `json:"supplyRecentSuccessStreak,omitempty"`
	SupplyShortWindowOrders         int     `json:"supplyShortWindowOrders,omitempty"`
	SupplyShortWindowFulfillment    float64 `json:"supplyShortWindowFulfillmentRate,omitempty"`
	PurchaseLeadMinutes             float64 `json:"purchaseLeadMinutes,omitempty"`
	PurchaseTimingTriggerMinutes    float64 `json:"purchaseTimingTriggerMinutes,omitempty"`
	PurchaseTimingWaitMinutes       float64 `json:"purchaseTimingWaitMinutes,omitempty"`
	PurchaseTimingEligibleQuantity  int     `json:"purchaseTimingEligibleQuantity,omitempty"`
	PurchaseSupplyLifetimeMinutes   float64 `json:"purchaseSupplyLifetimeMinutes,omitempty"`
	PurchaseLifetimeQuantityLimit   int     `json:"purchaseLifetimeQuantityLimit,omitempty"`
	PurchaseLifetimeLimited         bool    `json:"purchaseLifetimeLimited,omitempty"`
	UsageSampleMinutes              int     `json:"usageSampleMinutes"`
	AccountCacheAgeSeconds          int     `json:"accountCacheAgeSeconds"`
	LockedOrderID                   string  `json:"lockedOrderId,omitempty"`
	LockedOrderAgeSeconds           int     `json:"lockedOrderAgeSeconds,omitempty"`
	LockedConfirmRounds             int     `json:"lockedConfirmRounds,omitempty"`
	Strategy                        string  `json:"strategy,omitempty"`
	CriticalAvailableAccounts       int     `json:"criticalAvailableAccounts,omitempty"`
	HealthyAvailableAccounts        int     `json:"healthyAvailableAccounts,omitempty"`
	StartupAvailableAccounts        int     `json:"startupAvailableAccounts,omitempty"`
	EmergencyMinAccounts            int     `json:"emergencyMinAccounts,omitempty"`
	EmergencyReason                 string  `json:"emergencyReason,omitempty"`
	PoolVacuumActive                bool    `json:"poolVacuumActive,omitempty"`
	PoolVacuumStartedAtMS           int64   `json:"poolVacuumStartedAtMs,omitempty"`
	PoolVacuumDurationSeconds       int     `json:"poolVacuumDurationSeconds,omitempty"`
	DemandMemoryRCUPerMinute        float64 `json:"demandMemoryRcuPerMinute,omitempty"`
	DemandMemoryLastSeenMS          int64   `json:"demandMemoryLastSeenMs,omitempty"`
	DemandMemoryAgeSeconds          int     `json:"demandMemoryAgeSeconds,omitempty"`
	VirtualDemandRCUPerMinute       float64 `json:"virtualDemandRcuPerMinute,omitempty"`
	VirtualDemandTTLMinutes         int     `json:"virtualDemandTtlMinutes,omitempty"`
	AccountMaxRequestsBefore401     int     `json:"accountMaxRequestsBefore401,omitempty"`
	AccountMaxUsefulSeconds401      int     `json:"accountMaxUsefulSecondsBefore401,omitempty"`
	EstimatedNewAccountCapacityRCU  float64 `json:"estimatedNewAccountCapacityRcu,omitempty"`
	// RiskAdjustedUnitCapacityRCU is retained for API compatibility. It is the
	// conservative quota estimate for one newly supplied credential. 401
	// thresholds are operational risk signals and do not change this value.
	RiskAdjustedUnitCapacityRCU float64 `json:"riskAdjustedUnitCapacityRcu,omitempty"`
	// Token-capacity fields expose the replenishment dimension directly in
	// million tokens. RCU fields remain for API compatibility, but are derived
	// from the same token budget so runway and order sizing stay dimensionally
	// consistent.
	TokenCapacityMode                     string                   `json:"tokenCapacityMode,omitempty"`
	AccountQuotaEstimateM                 float64                  `json:"accountQuotaEstimateM,omitempty"`
	AccountQuotaEstimateSource            string                   `json:"accountQuotaEstimateSource,omitempty"`
	AccountQuotaCalibrationConfidence     string                   `json:"accountQuotaCalibrationConfidence,omitempty"`
	AccountQuotaCalibrationSamples        int                      `json:"accountQuotaCalibrationSamples,omitempty"`
	AccountQuotaCalibrationObservedPct    float64                  `json:"accountQuotaCalibrationObservedPercent,omitempty"`
	AccountQuotaCalibrationUniqueAccounts int                      `json:"accountQuotaCalibrationUniqueAccounts,omitempty"`
	AccountQuotaCurrentEstimateM          float64                  `json:"accountQuotaCurrentEstimateM,omitempty"`
	AccountQuotaRecentEstimateM           float64                  `json:"accountQuotaRecentEstimateM,omitempty"`
	AccountQuotaHistoricalEstimateM       float64                  `json:"accountQuotaHistoricalEstimateM,omitempty"`
	AccountQuotaDivergencePercent         float64                  `json:"accountQuotaDivergencePercent,omitempty"`
	AccountQuotaPlanEstimates             []SmartQuotaPlanEstimate `json:"accountQuotaPlanEstimates,omitempty"`
	QuotaEstimateOrderingBlocked          bool                     `json:"quotaEstimateOrderingBlocked,omitempty"`
	QuotaEstimatePendingPlans             int                      `json:"quotaEstimatePendingPlans,omitempty"`
	RawCapacityTokenM                     float64                  `json:"rawCapacityTokenM,omitempty"`
	CurrentCapacityTokenM                 float64                  `json:"currentCapacityTokenM,omitempty"`
	AvailableCapacityTokenM               float64                  `json:"availableCapacityTokenM,omitempty"`
	FrozenCapacityTokenM                  float64                  `json:"frozenCapacityTokenM,omitempty"`
	TotalCapacityTokenM                   float64                  `json:"totalCapacityTokenM,omitempty"`
	TimeLimitedCapacityTokenM             float64                  `json:"timeLimitedCapacityTokenM,omitempty"`
	ExpiryWasteRiskTokenM                 float64                  `json:"expiryWasteRiskTokenM,omitempty"`
	ObservedTokenM1M                      float64                  `json:"observedTokenM1m,omitempty"`
	ObservedTokenM5M                      float64                  `json:"observedTokenM5m,omitempty"`
	ObservedTokenM10M                     float64                  `json:"observedTokenM10m,omitempty"`
	ObservedTokenM30M                     float64                  `json:"observedTokenM30m,omitempty"`
	ConsumeTokenM1M                       float64                  `json:"consumeTokenM1m,omitempty"`
	ConsumeTokenM5M                       float64                  `json:"consumeTokenM5m,omitempty"`
	ConsumeTokenM10M                      float64                  `json:"consumeTokenM10m,omitempty"`
	ConsumeTokenMPerMinute                float64                  `json:"consumeTokenMPerMinute,omitempty"`
	DemandPlanningTokenMPerMinute         float64                  `json:"demandPlanningTokenMPerMinute,omitempty"`
	ForecastSustainMinutes                float64                  `json:"forecastSustainMinutes,omitempty"`
	TargetCapacityTokenM                  float64                  `json:"targetCapacityTokenM,omitempty"`
	CapacityGapTokenM                     float64                  `json:"capacityGapTokenM,omitempty"`
	EstimatedNewAccountCapacityTokenM     float64                  `json:"estimatedNewAccountCapacityTokenM,omitempty"`
	PrelockedCapacityTokenM               float64                  `json:"prelockedCapacityTokenM,omitempty"`
	ProjectedCapacityAfterRefillTokenM    float64                  `json:"projectedCapacityAfterRefillTokenM,omitempty"`
	operatorClassificationObserved        bool
	capacityItems                         []smartCapacityItem
	quotaSupplierByFile                   map[string]string
}

type SmartQuotaPlanEstimate struct {
	Key                    string                    `json:"key"`
	SupplierID             string                    `json:"supplierId,omitempty"`
	SupplierName           string                    `json:"supplierName,omitempty"`
	PlanType               string                    `json:"planType"`
	Mode                   string                    `json:"mode"`
	AccountCount           int                       `json:"accountCount"`
	FallbackM              float64                   `json:"fallbackM"`
	FixedM                 float64                   `json:"fixedM,omitempty"`
	ObservedM              float64                   `json:"observedM,omitempty"`
	AdoptedM               float64                   `json:"adoptedM"`
	Source                 string                    `json:"source"`
	SampleCount            int                       `json:"sampleCount"`
	UniqueAccounts         int                       `json:"uniqueAccounts"`
	CompleteWindowAccounts int                       `json:"completeWindowAccounts,omitempty"`
	MinimumUniqueAccounts  int                       `json:"minimumUniqueAccounts,omitempty"`
	DivergencePercent      float64                   `json:"divergencePercent,omitempty"`
	PendingConfirmation    bool                      `json:"pendingConfirmation,omitempty"`
	ConfirmationRounds     int                       `json:"confirmationRounds,omitempty"`
	RequiredRounds         int                       `json:"requiredRounds,omitempty"`
	ValidationState        string                    `json:"validationState,omitempty"`
	UsingFallback          bool                      `json:"usingFallback,omitempty"`
	RejectedAccounts       int                       `json:"rejectedAccounts,omitempty"`
	OrderingBlocked        bool                      `json:"orderingBlocked,omitempty"`
	LastInspectionRunID    int64                     `json:"lastInspectionRunId,omitempty"`
	QuotaClasses           []SmartQuotaClassEstimate `json:"quotaClasses,omitempty"`
}

type SmartQuotaClassEstimate struct {
	ID                  string  `json:"id"`
	CenterM             float64 `json:"centerM"`
	MinimumM            float64 `json:"minimumM"`
	MaximumM            float64 `json:"maximumM"`
	AccountCount        int     `json:"accountCount"`
	TrustedAccounts     int     `json:"trustedAccounts"`
	ProvisionalAccounts int     `json:"provisionalAccounts"`
	SharePercent        float64 `json:"sharePercent"`
	Confidence          string  `json:"confidence"`
}

type smartUsageBucket struct {
	minuteMS       int64
	requests       int64
	success        int64
	failed         int64
	zeroTokens     int64
	totalTokens    int64
	latencySumMS   int64
	latencySamples int64
}

type authFileSnapshot struct {
	files       []cpaauthfiles.File
	generatedAt time.Time
	attemptedAt time.Time
	lastErr     error
}

// inspectionQuotaSnapshot is derived exclusively from a completed credential
// inspection. It deliberately does not read CPA's live auth-file list: that
// list contains transient scheduler state and is too volatile to drive a
// purchasing decision.
type inspectionQuotaSnapshot struct {
	run                  store.CodexInspectionRun
	results              []store.CodexInspectionResult
	quotaWindowUsage     []smartQuotaWindowBaseline
	leaseExpiresByFile   map[string]int64
	accountExpiresByFile map[string]int64
	supplierByFile       map[string]string
	activeImportItems    []store.SupplyImportItem
	generatedAt          time.Time
	attemptedAt          time.Time
	lastErr              error
}

// smartLiveQuotaObservation is the local delta layer above a completed
// inspection snapshot. Usage collection already receives account-scoped quota
// response headers, so rebuilding the whole pool to learn that one account
// moved from 40% to 41% used is unnecessary. Observations are kept in memory,
// keyed by the same credential identities used by quota calibration, and are
// discarded naturally after a newer completed inspection supersedes them.
type smartLiveQuotaObservation struct {
	updatedAtMS int64
	planType    string
	windows     []model.CodexInspectionQuotaWindow
}

type smartLiveQuotaDelta struct {
	updatedAtMS int64
	accounts    int
}

type smartCapacityItem struct {
	credentialKey     string
	fileKey           string
	capacityRCU       float64
	usableCapacityRCU float64
	remainingMinutes  float64
	expiresAtMS       int64
}

func defaultSmartResource(cfg store.ManagerSupplyConfig) SmartResource {
	configuredTarget := smartHealthyMinutesTarget(cfg)
	effectiveTarget := smartEffectiveHealthyMinutesTarget(cfg)
	strategy := managerconfigsvc.NormalizeSupplyStrategy(cfg.Strategy)
	resource := SmartResource{
		Enabled:                     smartSupplyEnabled(cfg),
		HealthLevel:                 smartHealthUnknown,
		SuggestedAction:             smartActionSnapshotStale,
		DecisionReason:              "snapshot_not_ready",
		Confidence:                  smartConfidenceLow,
		SnapshotFresh:               false,
		GeneratedAtMS:               time.Now().UnixMilli(),
		CapacitySource:              smartCapacitySourceUnavailable,
		DemandTrend:                 smartDemandTrendUnknown,
		CapacityLifetimeCoverage:    100,
		TargetAvailableAccounts:     cfg.TargetAvailableAccounts,
		ConfiguredHealthyMinutes:    configuredTarget,
		EffectiveHealthyMinutes:     effectiveTarget,
		AccountLifetimeMinutes:      smartCapacityPlanningHorizonMinutes(cfg),
		HealthyMinutesTarget:        effectiveTarget,
		WarningMinutes:              smartWarningMinutes(cfg),
		CriticalMinutes:             smartCriticalMinutes(cfg),
		UnitCapacityRCU:             smartProductUnitCapacity(cfg.Product),
		Strategy:                    strategy,
		CriticalAvailableAccounts:   smartCriticalAvailableAccounts(cfg),
		HealthyAvailableAccounts:    smartHealthyAvailableAccounts(cfg),
		StartupAvailableAccounts:    smartStartupAvailableAccounts(cfg),
		EmergencyMinAccounts:        smartEmergencyMinAccounts(cfg),
		VirtualDemandTTLMinutes:     smartVirtualDemandTTLMinutes(cfg),
		AccountMaxRequestsBefore401: smartAccountMaxRequestsBefore401(cfg),
		AccountMaxUsefulSeconds401:  smartAccountMaxUsefulSecondsBefore401(cfg),
	}
	applySmartTokenCapacityDefaults(cfg, &resource)
	applySmartTokenMetrics(&resource)
	return resource
}

func (s *Service) HandleUsageEvents(ctx context.Context, runtimeCfg collectorpkg.RuntimeConfig, events []usage.Event) {
	if s == nil || len(events) == 0 || ctx.Err() != nil {
		return
	}
	s.recordSmartUsageEvents(events, time.Now())
	s.handleSupplyAuth401Events(ctx, runtimeCfg, events)
}

// WarmSmartUsage restores the longest configurable demand-memory window after
// a Manager restart. Runtime collectors keep smartBuckets current afterwards.
func (s *Service) WarmSmartUsage(ctx context.Context) error {
	if s == nil || s.store == nil {
		return nil
	}
	now := time.Now()
	from := now.Add(-180 * time.Minute).UnixMilli()
	minutes, err := s.store.ListSupplyUsageMinutes(ctx, from)
	if err != nil {
		return err
	}
	quotaEvents, err := s.store.ListSupplyQuotaCalibrationEvents(
		ctx,
		now.Add(-smartQuotaCalibrationWarmWindow).UnixMilli(),
		100_000,
	)
	quotaWarmErr := err
	oldestMinute := now.Add(-180*time.Minute).UnixMilli() / 60000 * 60000
	s.smartMu.Lock()
	defer s.smartMu.Unlock()
	if s.smartBuckets == nil {
		s.smartBuckets = make(map[int64]*smartUsageBucket)
	}
	for _, minute := range minutes {
		if minute.MinuteMS < oldestMinute {
			continue
		}
		s.smartBuckets[minute.MinuteMS] = &smartUsageBucket{
			minuteMS:       minute.MinuteMS,
			requests:       minute.Requests,
			success:        minute.Successful,
			failed:         minute.Failed,
			totalTokens:    minute.TotalTokens,
			latencySumMS:   minute.LatencySumMS,
			latencySamples: minute.LatencySamples,
		}
	}
	for minute := range s.smartBuckets {
		if minute < oldestMinute {
			delete(s.smartBuckets, minute)
		}
	}
	if quotaWarmErr == nil {
		_ = s.recordSmartLiveQuotaObservationsLocked(quotaEvents, now)
		_ = s.recordSmartQuotaCalibrationEventsLocked(quotaEvents, now)
	}
	return quotaWarmErr
}

func (s *Service) recordSmartUsageEvents(events []usage.Event, now time.Time) {
	s.smartMu.Lock()
	if s.smartBuckets == nil {
		s.smartBuckets = make(map[int64]*smartUsageBucket)
	}
	oldestMinute := now.Add(-180*time.Minute).UnixMilli() / 60000 * 60000
	for _, event := range events {
		if !isSupplyCapacityEvent(event) {
			continue
		}
		ts := event.TimestampMS
		if ts <= 0 {
			ts = now.UnixMilli()
		}
		minute := ts / 60000 * 60000
		if minute < oldestMinute {
			continue
		}
		bucket := s.smartBuckets[minute]
		if bucket == nil {
			bucket = &smartUsageBucket{minuteMS: minute}
			s.smartBuckets[minute] = bucket
		}
		bucket.requests++
		if event.Failed {
			bucket.failed++
		} else {
			bucket.success++
			if event.LatencyMS != nil && *event.LatencyMS > 0 {
				bucket.latencySumMS += *event.LatencyMS
				bucket.latencySamples++
			}
		}
		if event.TotalTokens <= 0 && event.InputTokens <= 0 && event.OutputTokens <= 0 {
			bucket.zeroTokens++
		}
		bucket.totalTokens += maxInt64(event.TotalTokens, event.InputTokens+event.OutputTokens+event.ReasoningTokens)
	}
	for minute := range s.smartBuckets {
		if minute < oldestMinute {
			delete(s.smartBuckets, minute)
		}
	}
	liveQuotaChanged := s.recordSmartLiveQuotaObservationsLocked(events, now)
	supplierScoreChanged := s.recordSmartQuotaCalibrationEventsLocked(events, now)
	s.smartMu.Unlock()
	if supplierScoreChanged {
		s.invalidateAllMarketplaceSupplierQuotaScores()
	}
	if liveQuotaChanged {
		// The next dashboard read and automatic decision can rebuild from the
		// durable historical baseline plus only the changed credentials. No full
		// inspection or database-wide quota aggregation is required here.
		s.invalidateStatusCache()
		s.signalAutomaticWorker()
	}
}

func (s *Service) smartResource(ctx context.Context, cfg store.ManagerConfig, forceAuthRefresh bool) (SmartResource, error) {
	if !smartSupplyEnabled(cfg.Supply) {
		resource := defaultSmartResource(cfg.Supply)
		resource.Enabled = false
		resource.HealthLevel = smartHealthUnknown
		resource.SuggestedAction = smartActionHealthy
		resource.DecisionReason = "smart_disabled"
		return s.publishSmartResource(resource), nil
	}
	quotaSnapshot, err := s.cachedInspectionQuotaSnapshot(ctx, cfg.Supply, forceAuthRefresh)
	if err != nil && len(quotaSnapshot.results) == 0 {
		resource := defaultSmartResource(cfg.Supply)
		resource.SuggestedAction = smartActionSnapshotStale
		resource.DecisionReason = "inspection_snapshot_unavailable"
		resource = s.publishSmartResource(resource)
		// Missing quota evidence is an expected cold-start state. The automatic
		// loop must pause quietly rather than repeatedly recording an operational
		// error or falling back to credential counts.
		return resource, nil
	}
	now := time.Now()
	previous := s.currentSmartResource(cfg.Supply)
	resource := s.buildSmartResourceFromInspectionSnapshot(cfg.Supply, quotaSnapshot, now)
	applyHistoricalCapacityStartupObservation(cfg.Supply, &resource, previous, now)
	if err != nil {
		resource.SnapshotFresh = false
		resource.DecisionReason = "using_stale_inspection_snapshot"
	}
	return s.publishSmartResource(resource), nil
}

func (s *Service) buildSmartResourceFromInspectionSnapshot(cfg store.ManagerSupplyConfig, snapshot inspectionQuotaSnapshot, now time.Time) (resource SmartResource) {
	var liveDelta smartLiveQuotaDelta
	snapshot, liveDelta = s.applySmartLiveQuotaDelta(snapshot, now)
	resource = defaultSmartResource(cfg)
	resource.quotaSupplierByFile = snapshot.supplierByFile
	defer func() {
		applySmartTokenMetrics(&resource)
	}()
	resource.GeneratedAtMS = now.UnixMilli()
	resource.CapacitySource = smartCapacitySourceInspection
	resource.CapacitySnapshotRunID = snapshot.run.ID
	resource.CapacityDeltaAtMS = liveDelta.updatedAtMS
	resource.CapacityDeltaAccounts = liveDelta.accounts
	if !snapshot.generatedAt.IsZero() {
		resource.CapacitySnapshotAtMS = snapshot.generatedAt.UnixMilli()
		resource.CapacitySnapshotAgeSeconds = max(0, int(now.Sub(snapshot.generatedAt).Seconds()))
		resource.AccountCacheAgeSeconds = resource.CapacitySnapshotAgeSeconds
	}
	resource.SnapshotFresh = smartInspectionSnapshotFresh(snapshot, now)

	usageStats := s.smartUsageSnapshot(now)
	resource.UnitCapacityRCU = smartProductUnitCapacity(cfg.Product)
	quotaInspectionRunID := snapshot.run.ID
	if quotaInspectionRunID <= 0 {
		quotaInspectionRunID = snapshot.run.FinishedAtMS
	}
	if quotaInspectionRunID <= 0 && !snapshot.generatedAt.IsZero() {
		quotaInspectionRunID = snapshot.generatedAt.UnixMilli()
	}
	planQuotaEstimates, planningByPlan := s.smartQuotaPlanEstimatesForInspection(
		cfg,
		snapshot.results,
		quotaInspectionRunID,
		now,
		snapshot.supplierByFile,
	)
	resource.AccountQuotaPlanEstimates = planQuotaEstimates
	blockedSuppliers := make(map[string]struct{})
	for _, estimate := range planQuotaEstimates {
		if estimate.PendingConfirmation {
			resource.QuotaEstimatePendingPlans++
		}
		if estimate.OrderingBlocked && strings.EqualFold(estimate.PlanType, "team") {
			blockedSuppliers[normalizeSmartQuotaSupplierID(estimate.SupplierID)] = struct{}{}
		}
	}
	platforms := supplyPlatforms(cfg)
	if len(platforms) == 0 {
		_, resource.QuotaEstimateOrderingBlocked = blockedSuppliers[""]
	} else {
		resource.QuotaEstimateOrderingBlocked = true
		for _, platform := range platforms {
			if _, blocked := blockedSuppliers[normalizeSmartQuotaSupplierID(platform.ID)]; !blocked {
				resource.QuotaEstimateOrderingBlocked = false
				break
			}
		}
	}
	dominantSupplier, dominantPlan := dominantSmartQuotaContext(snapshot.results, snapshot.supplierByFile, platforms)
	poolQuotaEstimate := smartQuotaPlanningEstimateForPlan(planningByPlan, dominantSupplier, dominantPlan)
	s.applySmartQuotaEstimate(cfg, &resource, poolQuotaEstimate)
	consumeRCUPerMinute := applySmartUsage(&resource, usageStats, resource.UnitCapacityRCU)

	if !smartInspectionSnapshotComplete(snapshot) {
		resource.SnapshotFresh = false
		resource.Confidence = smartConfidenceLow
		resource.HealthLevel = smartHealthUnknown
		resource.SuggestedAction = smartActionSnapshotStale
		resource.DecisionReason = "inspection_snapshot_incomplete"
		return resource
	}

	capacityItems := make([]smartCapacityItem, 0, len(snapshot.results))
	inspectedFiles := make(map[string]struct{}, len(snapshot.results))
	eligible := 0
	withQuotaEvidence := 0
	usabilityRequired := 0
	withVerifiedUsability := 0
	for _, result := range snapshot.results {
		if !isSmartCapacityInspectionResult(result) {
			continue
		}
		fileName := strings.TrimSpace(result.FileName)
		leaseExpiresAtMS, suppliedAccount := snapshot.leaseExpiresByFile[fileName]
		resource.TotalAccounts++
		if !result.Disabled {
			resource.EnabledAccounts++
		}
		if fileName != "" {
			inspectedFiles[fileName] = struct{}{}
		}
		if inspectionResultCapacityExcluded(result) {
			continue
		}
		eligible++
		usabilityRequired++
		remaining, hasCapacityQuota := inspectionResultRemainingQuotaFraction(result)
		if hasCapacityQuota {
			withQuotaEvidence++
		}
		// A completed inspection with status=error has quota headers but did
		// not prove that the credential can serve a request. Keep it out of
		// verified capacity and pause automation rather than purchasing against
		// a possibly unavailable account.
		if inspectionResultUsabilityUnverified(result) {
			continue
		}
		withVerifiedUsability++
		resource.SchedulableAccounts++
		resource.AvailableAccounts++
		remainingMinutes := float64(smartCapacityPlanningHorizonMinutes(cfg))
		accountExpiresAtMS := snapshot.accountExpiresByFile[fileName]
		if accountExpiresAtMS > 0 {
			remainingMinutes = math.Max(0, time.UnixMilli(accountExpiresAtMS).Sub(now).Minutes())
		}
		if !hasCapacityQuota && suppliedAccount {
			withQuotaEvidence++
		}
		capacity := 0.0
		planType := strings.ToLower(strings.TrimSpace(result.PlanType))
		if planType == "" {
			planType = "unknown"
		}
		supplierID := normalizeSmartQuotaSupplierID(snapshot.supplierByFile[fileName])
		if supplierID == "" && len(platforms) == 1 {
			supplierID = normalizeSmartQuotaSupplierID(platforms[0].ID)
		}
		planQuotaEstimate := smartQuotaPlanningEstimateForPlan(planningByPlan, supplierID, planType)
		accountQuotaEstimate := s.smartQuotaEstimateForInspectionResult(result, planQuotaEstimate, now)
		switch {
		case hasCapacityQuota:
			capacity = smartAccountQuotaCapacityRCU(resource.UnitCapacityRCU, accountQuotaEstimate.CapacityM, remaining)
		case suppliedAccount:
			// The completed probe proves current usability. Supplier timestamps
			// remain routing-priority hints and do not cap usable capacity.
			capacity = smartEstimatedNewAccountTokenCapacityRCU(cfg, accountQuotaEstimate.CapacityM)
			resource.LeaseEstimatedAccounts++
			resource.LeaseEstimatedCapacityRCU += capacity
		default:
			continue
		}
		if capacity <= 0 {
			continue
		}
		expiryHintMinutes := remainingMinutes
		if accountExpiresAtMS > 0 {
			expiryHintMinutes = time.UnixMilli(accountExpiresAtMS).Sub(now).Minutes()
		} else if suppliedAccount {
			expiryHintMinutes = time.UnixMilli(leaseExpiresAtMS).Sub(now).Minutes()
		}
		resource.recordExpiringAccount(expiryHintMinutes, capacity)
		// A successful current quota inspection is direct evidence that the
		// credential is alive. 401 thresholds do not reduce its actual quota.
		resource.HealthyAccounts++
		if hasCapacityQuota && remaining >= smartNormalAccountMinimumRemainingFraction && !inspectionResultInCooldown(result) {
			resource.NormalAccounts++
		}
		capacityItem := smartCapacityItem{
			credentialKey:    operatorCredentialKey(result.FileName, result.AuthIndex),
			fileKey:          operatorFileCredentialKey(result.FileName),
			capacityRCU:      capacity,
			remainingMinutes: remainingMinutes,
		}
		if accountExpiresAtMS > 0 {
			capacityItem.expiresAtMS = accountExpiresAtMS
		} else if suppliedAccount {
			capacityItem.expiresAtMS = leaseExpiresAtMS
		}
		capacityItems = append(capacityItems, capacityItem)
	}
	// A completed inspection is intentionally snapshot based, so an account
	// delivered just after that run used to contribute zero capacity until the
	// next full scan completed. That made a manual purchase appear to have no
	// effect for several minutes. Overlay current imports that were added
	// after this snapshot, use the configured new-account confidence discount,
	// and keep them visibly separate from verified healthy credentials.
	overlaidFiles := make(map[string]struct{}, len(inspectedFiles)+len(snapshot.activeImportItems))
	for fileName := range inspectedFiles {
		overlaidFiles[fileName] = struct{}{}
	}
	for _, item := range snapshot.activeImportItems {
		fileName := strings.TrimSpace(item.FileName)
		inspectionStartedAtMS := snapshot.run.StartedAtMS
		if inspectionStartedAtMS <= 0 {
			inspectionStartedAtMS = snapshot.generatedAt.UnixMilli()
		}
		// The inspection's file set is captured at its start, not when its
		// results finish writing. Accounts imported while a long inspection is
		// running are absent from that completed result and need this overlay.
		if fileName == "" || item.ImportedAtMS <= inspectionStartedAtMS {
			continue
		}
		if _, alreadyCounted := overlaidFiles[fileName]; alreadyCounted {
			continue
		}
		overlaidFiles[fileName] = struct{}{}
		remainingMinutes := float64(smartCapacityPlanningHorizonMinutes(cfg))
		accountExpiresAtMS := snapshot.accountExpiresByFile[fileName]
		if accountExpiresAtMS > 0 {
			remainingMinutes = math.Max(0, time.UnixMilli(accountExpiresAtMS).Sub(now).Minutes())
		}
		supplierID := normalizeSmartQuotaSupplierID(snapshot.supplierByFile[fileName])
		if supplierID == "" && len(platforms) == 1 {
			supplierID = normalizeSmartQuotaSupplierID(platforms[0].ID)
		}
		accountQuotaEstimate := smartQuotaPlanningEstimateForPlan(planningByPlan, supplierID, "team")
		capacity := smartEstimatedNewAccountTokenCapacityRCU(cfg, accountQuotaEstimate.CapacityM)
		if capacity <= 0 {
			continue
		}
		expiresAtMS := accountExpiresAtMS
		if expiresAtMS <= 0 {
			expiresAtMS = item.LeaseExpiresAtMS
		}
		capacityItems = append(capacityItems, smartCapacityItem{
			fileKey:          operatorFileCredentialKey(fileName),
			capacityRCU:      capacity,
			remainingMinutes: remainingMinutes,
			expiresAtMS:      expiresAtMS,
		})
		resource.TotalAccounts++
		resource.EnabledAccounts++
		resource.UnconfirmedAccounts++
		resource.SchedulableAccounts++
		resource.AvailableAccounts++
		resource.PendingInspectionAccounts++
		resource.PendingInspectionCapacityRCU += capacity
		expiryHintMinutes := remainingMinutes
		if expiresAtMS > 0 {
			expiryHintMinutes = time.UnixMilli(expiresAtMS).Sub(now).Minutes()
		}
		resource.recordExpiringAccount(expiryHintMinutes, capacity)
	}
	resource.DisabledAccounts = max(0, resource.TotalAccounts-resource.AvailableAccounts)
	applySmartAccountCountBreakdown(&resource)
	resource.PendingInspectionCapacityRCU = round2(resource.PendingInspectionCapacityRCU)
	resource.LeaseEstimatedCapacityRCU = round2(resource.LeaseEstimatedCapacityRCU)
	if eligible > 0 {
		resource.CapacityCoverage = round2(float64(withQuotaEvidence) / float64(eligible) * 100)
	} else {
		// A complete inspection which has no usable credential is a trusted zero
		// capacity state, not a missing data state.
		resource.CapacityCoverage = 100
	}
	// Supplier timestamps are provenance and routing-priority signals. Current
	// health/quota evidence, rather than the timestamp, determines capacity.
	resource.CapacityLifetimeCoverage = 100
	quotaEvidenceIncomplete := eligible > 0 && withQuotaEvidence != eligible
	usabilityEvidenceIncomplete := usabilityRequired > 0 && withVerifiedUsability != usabilityRequired
	// Live usability and quota evidence decide whether a credential contributes
	// capacity; supplier timestamps remain informational.
	for _, item := range capacityItems {
		resource.RawCapacityRCU += item.capacityRCU
	}
	resource.RawCapacityRCU = round2(resource.RawCapacityRCU)
	resource.capacityItems = capacityItems
	applySmartExpiryCapacity(&resource, resource.capacityItems, consumeRCUPerMinute, now)
	resource.TargetCapacityRCU = round2(consumeRCUPerMinute * float64(resource.EffectiveHealthyMinutes))
	resource.RecommendedCapacityRCU = resource.TargetCapacityRCU
	consumeRCUPerMinute = s.applySmartDemandMemory(cfg, &resource, now, consumeRCUPerMinute)
	applySmartEmergencyAvailability(cfg, &resource, now)

	// Keep the verified portion visible even while one or more credentials did
	// not return quota evidence. Returning early here used to turn the entire
	// dashboard into 0 RCU, despite most capacity being known. The resulting
	// figure is a lower bound: a bounded purchase may use its capacity deficit
	// while it remains recent, without counting unverified credentials as usable.
	if quotaEvidenceIncomplete || usabilityEvidenceIncomplete {
		if usageStats.successful30 > 0 && resource.ConsumeRCUPerMinute > 0 {
			recalculateSmartResourceCapacityPlan(cfg, &resource)
		}
		resource.SnapshotFresh = false
		resource.SnapshotEvidencePartial = true
		resource.Confidence = smartConfidenceLow
		incompleteReason := "inspection_usability_incomplete"
		if quotaEvidenceIncomplete {
			incompleteReason = "inspection_quota_incomplete"
		}
		resource.DecisionReason = incompleteReason
		if smartPartialInspectionCapacityDeficitAllowed(resource) {
			// Keep the capacity plan derived exclusively from verified credentials.
			// The incomplete result never increases available capacity; it only
			// permits a bounded order against the recent verified lower bound.
			resource.SuggestedQuantity = min(resource.SuggestedQuantity, 3)
			applySmartRefillProjection(cfg, &resource)
			resource.DecisionReason = incompleteReason + "_capacity_deficit"
		} else {
			resource.HealthLevel = smartHealthUnknown
			resource.SuggestedAction = smartActionSnapshotStale
			resource.SuggestedQuantity = 0
		}
		return resource
	}
	if !smartResourceEmergency(resource) &&
		((usageStats.successful30 <= 0 && resource.VirtualDemandRCUPerMinute <= 0) ||
			(resource.ConsumeRCUPerMinute <= 0 && resource.DemandTrend != smartDemandTrendFalling)) {
		resource.Confidence = smartConfidenceLow
		resource.HealthLevel = smartHealthUnknown
		resource.SuggestedAction = smartActionSnapshotStale
		resource.DecisionReason = "usage_rate_not_ready"
		return resource
	}
	recalculateSmartResourceCapacityPlan(cfg, &resource)
	if usageStats.sampleMinutes >= 20 && resource.SnapshotFresh {
		resource.Confidence = smartConfidenceHigh
	} else if usageStats.sampleMinutes >= 5 {
		resource.Confidence = smartConfidenceMedium
	} else {
		resource.Confidence = smartConfidenceLow
	}
	return resource
}

func (s *Service) buildSmartResourceFromSnapshots(cfg store.ManagerSupplyConfig, authSnapshot authFileSnapshot, now time.Time) (resource SmartResource) {
	resource = defaultSmartResource(cfg)
	defer func() {
		applySmartTokenMetrics(&resource)
	}()
	resource.GeneratedAtMS = now.UnixMilli()
	resource.SnapshotFresh = !authSnapshot.generatedAt.IsZero() && now.Sub(authSnapshot.generatedAt) <= time.Duration(smartAuthFilesCacheTTLSeconds(cfg))*time.Second*2
	if !authSnapshot.generatedAt.IsZero() {
		resource.AccountCacheAgeSeconds = max(0, int(now.Sub(authSnapshot.generatedAt).Seconds()))
	}
	usageStats := s.smartUsageSnapshot(now)

	unit := smartProductUnitCapacity(cfg.Product)
	resource.UnitCapacityRCU = unit
	poolPlan, poolIdentities := smartQuotaAuthSnapshotContext(authSnapshot.files)
	poolQuotaEstimate := s.smartQuotaEstimateFor(poolPlan, poolIdentities...)
	poolPolicy := smartQuotaPolicyForPlan(cfg, poolPlan)
	if poolPolicy.Mode == smartQuotaPolicyModeFixed {
		poolQuotaEstimate = smartQuotaEstimate{
			CapacityM:  poolPolicy.FixedM,
			Source:     smartQuotaPolicyModeFixed,
			Confidence: smartConfidenceHigh,
		}
	} else if !smartQuotaEstimateHasValidData(poolQuotaEstimate) {
		poolQuotaEstimate = smartQuotaEstimate{
			CapacityM:  poolPolicy.FallbackM,
			Source:     smartQuotaEstimateSourceDefault,
			Confidence: smartConfidenceLow,
		}
	}
	s.applySmartQuotaEstimate(cfg, &resource, poolQuotaEstimate)
	consumeRCUPerMinute := applySmartUsage(&resource, usageStats, unit)
	var weightedCapacity float64
	var effectiveAvailable float64
	capacityItems := make([]smartCapacityItem, 0, len(authSnapshot.files))
	for _, file := range authSnapshot.files {
		if !isCodexAuthFile(file) {
			continue
		}
		resource.TotalAccounts++
		leaseExpiresAt, suppliedAccount := smartSupplyLeaseExpiry(file.Raw, now)
		if !file.Disabled {
			resource.EnabledAccounts++
			if smartAccountNeedsAttention(file.Raw) {
				resource.NeedsAttentionAccounts++
			} else {
				resource.UnconfirmedAccounts++
			}
		}
		if !isSmartCapacityCodexFile(file) {
			resource.DisabledAccounts++
			continue
		}
		resource.SchedulableAccounts++
		if !isAvailableCodexFile(file) {
			resource.DisabledAccounts++
			continue
		}
		// 临时运行态（包括冷却与上游异常）不是凭证健康信号。请求历史
		// 只用于全局消耗速度，不能折减单凭证余额或可用数量。
		resource.AvailableAccounts++
		resource.HealthyAccounts++
		resource.NormalAccounts++
		remainingMinutes := smartAccountRemainingMinutes(file.Raw, now, smartCapacityPlanningHorizonMinutes(cfg))
		planType := strings.ToLower(strings.TrimSpace(textField(
			file.Raw,
			"plan_type",
			"planType",
			"chatgpt_plan_type",
			"chatgptPlanType",
		)))
		planPolicy := smartQuotaPolicyForPlan(cfg, planType)
		planQuotaEstimate := poolQuotaEstimate
		if planType != "" && planType != poolPlan {
			planQuotaEstimate = s.smartQuotaEstimateForAt(now, planType)
			if !smartQuotaEstimateHasValidData(planQuotaEstimate) {
				planQuotaEstimate = smartQuotaEstimate{
					CapacityM:  planPolicy.FallbackM,
					Source:     smartQuotaEstimateSourceDefault,
					Confidence: smartConfidenceLow,
				}
			}
		}
		identities := smartQuotaCalibrationResultIdentities(
			file.Name,
			textField(file.Raw, "auth_index", "authIndex"),
			textField(file.Raw, "account_key", "accountKey"),
			textField(file.Raw, "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId"),
		)
		accountQuotaEstimate, hasCurrentEstimate := s.smartQuotaCurrentEstimateForAt(now, identities...)
		switch {
		case planPolicy.Mode == smartQuotaPolicyModeFixed:
			accountQuotaEstimate = smartQuotaEstimate{
				CapacityM:  planPolicy.FixedM,
				Source:     smartQuotaPolicyModeFixed,
				Confidence: smartConfidenceHigh,
			}
		case hasCurrentEstimate:
			accountQuotaEstimate.CurrentEstimateM = accountQuotaEstimate.CapacityM
		default:
			accountQuotaEstimate = planQuotaEstimate
		}
		rawCapacity, ok := smartAccountCapacityRCU(file.Raw, unit, accountQuotaEstimate.CapacityM)
		if !ok {
			rawCapacity = smartTokenMillionToRCU(accountQuotaEstimate.CapacityM, unit)
		}
		weightedCapacity += rawCapacity
		if rawCapacity > 0 {
			capacityItem := smartCapacityItem{
				credentialKey:    operatorCredentialKey(file.Name, file.AuthIndex),
				fileKey:          operatorFileCredentialKey(file.Name),
				capacityRCU:      rawCapacity,
				remainingMinutes: remainingMinutes,
			}
			if suppliedAccount {
				capacityItem.expiresAtMS = leaseExpiresAt.UnixMilli()
			}
			capacityItems = append(capacityItems, capacityItem)
		}
		expiryHintMinutes := remainingMinutes
		if suppliedAccount {
			expiryHintMinutes = leaseExpiresAt.Sub(now).Minutes()
		}
		resource.recordExpiringAccount(expiryHintMinutes, rawCapacity)
		effectiveAvailable++
	}
	resource.AvailableAccounts = int(effectiveAvailable)
	applySmartAccountCountBreakdown(&resource)
	resource.RawCapacityRCU = round2(weightedCapacity)
	resource.capacityItems = capacityItems
	applySmartExpiryCapacity(&resource, resource.capacityItems, consumeRCUPerMinute, now)
	applyAccountPoolConcurrency(&resource, accountPoolStatsFromFiles(authSnapshot.files))
	resource.TargetCapacityRCU = round2(consumeRCUPerMinute * float64(resource.EffectiveHealthyMinutes))
	resource.RecommendedCapacityRCU = resource.TargetCapacityRCU
	consumeRCUPerMinute = s.applySmartDemandMemory(cfg, &resource, now, consumeRCUPerMinute)
	applySmartEmergencyAvailability(cfg, &resource, now)

	if !smartResourceEmergency(resource) &&
		((usageStats.successful30 <= 0 && resource.VirtualDemandRCUPerMinute <= 0) ||
			(resource.ConsumeRCUPerMinute <= 0 && resource.DemandTrend != smartDemandTrendFalling)) {
		resource.Confidence = smartConfidenceLow
		resource.HealthLevel = smartHealthUnknown
		resource.SuggestedAction = smartActionSnapshotStale
		resource.DecisionReason = "usage_rate_not_ready"
		resource.EstimatedSustainMinutes = 0
		return resource
	}

	if usageStats.sampleMinutes >= 20 && resource.SnapshotFresh {
		resource.Confidence = smartConfidenceHigh
	} else if usageStats.sampleMinutes >= 5 {
		resource.Confidence = smartConfidenceMedium
	} else {
		resource.Confidence = smartConfidenceLow
	}
	recalculateSmartResourceCapacityPlan(cfg, &resource)
	return resource
}

func smartQuotaAuthSnapshotContext(files []cpaauthfiles.File) (string, []string) {
	planCounts := make(map[string]int)
	identitySeen := make(map[string]struct{})
	identities := make([]string, 0, len(files))
	for _, file := range files {
		if !isSmartCapacityCodexFile(file) || !isAvailableCodexFile(file) {
			continue
		}
		planType := strings.ToLower(strings.TrimSpace(textField(
			file.Raw,
			"plan_type",
			"planType",
			"chatgpt_plan_type",
			"chatgptPlanType",
		)))
		if planType != "" {
			planCounts[planType]++
		}
		for _, identity := range smartQuotaCalibrationResultIdentities(
			file.Name,
			textField(file.Raw, "auth_index", "authIndex"),
			textField(file.Raw, "account_key", "accountKey"),
			textField(file.Raw, "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId"),
		) {
			if _, ok := identitySeen[identity]; ok {
				continue
			}
			identitySeen[identity] = struct{}{}
			identities = append(identities, identity)
		}
	}
	bestPlan := ""
	bestCount := 0
	for planType, count := range planCounts {
		if count > bestCount || (count == bestCount && planType < bestPlan) {
			bestPlan = planType
			bestCount = count
		}
	}
	return bestPlan, identities
}

func (resource *SmartResource) recordExpiringAccount(remainingMinutes, capacity float64) {
	if resource == nil || remainingMinutes <= 0 || remainingMinutes > float64(resource.WarningMinutes) {
		return
	}
	resource.ExpiringAccounts++
	resource.ExpiringCapacityRCU += math.Max(0, capacity)
	if resource.ExpiringWithinMinutes <= 0 || int(math.Ceil(remainingMinutes)) < resource.ExpiringWithinMinutes {
		resource.ExpiringWithinMinutes = max(1, int(math.Ceil(remainingMinutes)))
	}
}

// recalculateSmartResourceCapacityPlan applies the active configuration to an
// existing capacity snapshot. GetStatus uses it too, so a changed health-water
// level never keeps an obsolete health decision until the next CPA refresh.
func recalculateSmartResourceCapacityPlan(cfg store.ManagerSupplyConfig, resource *SmartResource) {
	if resource == nil {
		return
	}
	applySmartTokenCapacityDefaults(cfg, resource)
	defer func() {
		applySmartAccountQuantityEstimate(cfg, resource)
		applySmartRefillProjection(cfg, resource)
		applySmartTokenMetrics(resource)
	}()
	if resource.ConsumeRCUPerMinute <= 0 {
		// An in-flight reservation has its own state machine. A config refresh
		// must not replace a take/wait instruction merely because the current
		// traffic sample is empty.
		if resource.SuggestedAction == smartActionTakeLocked || resource.SuggestedAction == smartActionWaitLocked {
			return
		}
		// With no observed or remembered traffic, capacity demand is zero. Keep
		// only the configured emergency account floor: applySmartEmergencyAvailability
		// remains responsible for an empty/critical pool, while ordinary accounts
		// are left untouched to avoid buying short-lived inventory that cannot be
		// consumed.
		resource.TargetCapacityRCU = 0
		resource.RecommendedCapacityRCU = 0
		resource.EstimatedSustainMinutes = 0
		resource.CapacityGapRCU = 0
		resource.SuggestedQuantity = 0
		resource.HealthLevel = smartHealthHealthy
		resource.SuggestedAction = smartActionObserveDemand
		if resource.DemandTrend == smartDemandTrendFalling {
			resource.DecisionReason = "demand_falling_observe"
		} else {
			resource.DecisionReason = "no_traffic_minimum_pool"
		}
		return
	}
	resource.TargetCapacityRCU = round2(resource.ConsumeRCUPerMinute * float64(resource.EffectiveHealthyMinutes))
	resource.RecommendedCapacityRCU = resource.TargetCapacityRCU
	resource.EstimatedSustainMinutes = round1(resource.CurrentCapacityRCU / resource.ConsumeRCUPerMinute)
	resource.CapacityGapRCU = round2(math.Max(0, resource.TargetCapacityRCU-resource.CurrentCapacityRCU-resource.PrelockedCapacityRCU))
	resource.SuggestedQuantity = 0

	if resource.EstimatedSustainMinutes >= float64(resource.EffectiveHealthyMinutes) {
		resource.HealthLevel = smartHealthHealthy
		resource.SuggestedAction = smartActionHealthy
		resource.DecisionReason = "capacity_healthy"
	} else if resource.EstimatedSustainMinutes < float64(resource.CriticalMinutes) {
		resource.HealthLevel = smartHealthCritical
		resource.SuggestedAction = smartActionTakeLocked
		resource.DecisionReason = "capacity_critical"
	} else if resource.EstimatedSustainMinutes < float64(resource.WarningMinutes) {
		resource.HealthLevel = smartHealthWarning
		resource.SuggestedAction = smartActionPrelock
		resource.DecisionReason = "capacity_below_warning"
	} else {
		resource.HealthLevel = smartHealthWarning
		resource.SuggestedAction = smartActionPrelock
		resource.DecisionReason = "capacity_below_target"
	}
	emergency := smartEmergencyShortage(*resource)
	resource.EmergencyShortage = emergency
	if emergency {
		// A critically short runway must not be hidden by a transient demand
		// drop. Observation batching may be shortened, but the persisted create
		// cooldown and in-flight supply guards remain mandatory.
		resource.HealthLevel = smartHealthCritical
		resource.SuggestedAction = smartActionEmergencyReplenish
		resource.DecisionReason = "emergency_capacity_shortage"
	}
	if resource.DemandTrend == smartDemandTrendFalling && !emergency {
		// One completed low-minute sample is sufficient to stop creating more
		// short-lived credentials while the buffer remains above the emergency
		// runway. Keep the target deficit visible instead of reporting healthy.
		resource.SuggestedAction = smartActionObserveDemand
		resource.DecisionReason = "capacity_below_target_falling_observe"
		return
	}

	if resource.EstimatedSustainMinutes >= float64(resource.EffectiveHealthyMinutes) {
		return
	}

	unitForNew := smartEstimatedNewAccountCapacityForResource(cfg, *resource)
	if unitForNew <= 0 {
		unitForNew = smartEstimatedAccountCapacityRCU(resource.UnitCapacityRCU, float64(smartCapacityPlanningHorizonMinutes(cfg)))
	}
	maxUsefulNewCapacity := math.Max(0, resource.ConsumeRCUPerMinute*float64(smartCapacityPlanningHorizonMinutes(cfg))-resource.CurrentCapacityRCU-resource.PrelockedCapacityRCU)
	gapForOrder := math.Min(resource.CapacityGapRCU, maxUsefulNewCapacity)
	if gapForOrder <= 0 {
		resource.HealthLevel = smartHealthWarning
		resource.SuggestedAction = smartActionHealthy
		resource.DecisionReason = "expiry_limited_capacity"
		return
	}
	batchLimit := smartAutomaticOrderQuantityLimit(cfg, *resource)
	minimumQuantity := min(smartPrelockMinQuantity(cfg), batchLimit)
	resource.SuggestedQuantity = clampInt(int(math.Ceil(gapForOrder/unitForNew)), minimumQuantity, batchLimit)
	if resource.DemandTrend == smartDemandTrendRising && !emergency {
		resource.SuggestedQuantity = min(resource.SuggestedQuantity, smartRisingObservationQuantity(cfg, *resource))
		resource.DecisionReason = "demand_rising_observe"
	}
}

func applySmartRefillProjection(cfg store.ManagerSupplyConfig, resource *SmartResource) {
	if resource == nil {
		return
	}
	unit := smartEstimatedNewAccountCapacityForResource(cfg, *resource)
	if unit <= 0 && resource.UnitCapacityRCU > 0 {
		unit = smartEstimatedAccountCapacityRCU(resource.UnitCapacityRCU, float64(smartCapacityPlanningHorizonMinutes(cfg)))
	}
	projectedSupply := math.Max(0, resource.PrelockedCapacityRCU)
	if resource.SuggestedQuantity > 0 && unit > 0 {
		// SuggestedQuantity describes the pending order while one exists and the
		// proposed next order otherwise. Use the larger projection rather than
		// double-counting the same in-flight credentials.
		projectedSupply = math.Max(projectedSupply, float64(resource.SuggestedQuantity)*unit)
	}
	projected := math.Max(0, resource.CurrentCapacityRCU) + projectedSupply
	resource.ProjectedCapacityAfterRefillRCU = round2(projected)
	resource.ProjectedSustainAfterRefillMin = 0
	if resource.ConsumeRCUPerMinute > 0 {
		resource.ProjectedSustainAfterRefillMin = round1(projected / resource.ConsumeRCUPerMinute)
	}
	applySmartTokenMetrics(resource)
}

func applySmartAccountQuantityEstimate(cfg store.ManagerSupplyConfig, resource *SmartResource) {
	if resource == nil {
		return
	}
	// Account replenishment is based on credentials that can actually serve
	// traffic. Enabled-but-frozen credentials remain part of TotalAccounts for
	// operator accounting, but treating them as projected availability masks a
	// real pool shortage and can leave the gateway returning auth_unavailable.
	projected := max(0, resource.AvailableAccounts)
	healthyFloor := smartHealthyAvailableAccounts(cfg)
	// Required account count uses the conservative quota estimate for a new
	// credential. 401 request/time thresholds are churn warnings only: they do
	// not become an RCU conversion factor or cap the pool-wide waterline.
	unit := smartEstimatedNewAccountCapacityForResource(cfg, *resource)
	if unit <= 0 && resource.UnitCapacityRCU > 0 {
		unit = smartEstimatedAccountCapacityRCU(resource.UnitCapacityRCU, float64(smartCapacityPlanningHorizonMinutes(cfg)))
	}
	if unit > 0 && resource.PrelockedCapacityRCU > 0 {
		projected += int(math.Ceil(resource.PrelockedCapacityRCU / unit))
	}
	countFloorDeficit := max(0, healthyFloor-projected)
	capacityDeficit := 0
	if unit > 0 && resource.CapacityGapRCU > 0 {
		// CapacityGapRCU already subtracts verified current quota and in-flight
		// capacity. Derive the additional account need from that real deficit;
		// do not subtract every enabled-but-nearly-empty account a second time.
		capacityDeficit = int(math.Ceil(resource.CapacityGapRCU / unit))
	}
	additional := max(countFloorDeficit, max(capacityDeficit, max(0, resource.ConcurrencyAccountDeficit)))
	resource.EstimatedRequiredAccounts = projected + additional
	resource.ProjectedAvailableAccounts = projected
	resource.AccountQuantityDeficit = additional
	applySmartConcurrencyShortagePlan(cfg, resource)
}

func applySmartConcurrencyShortagePlan(cfg store.ManagerSupplyConfig, resource *SmartResource) {
	if resource == nil || !resource.ConcurrencyDemandObserved || !resource.ConcurrencyLimited ||
		resource.ConcurrencyAccountDeficit <= 0 || !resource.SnapshotFresh {
		return
	}
	limit := smartAutomaticOrderQuantityLimit(cfg, *resource)
	quantity := min(resource.ConcurrencyAccountDeficit, limit)
	if resource.ConcurrencyFiniteSlots > 0 {
		// A finite pool with some headroom left is a warning dimension, not a
		// reason for a large one-shot order. Re-evaluate after at most three new
		// credentials have been inspected.
		quantity = min(quantity, 3)
	} else {
		// Zero finite slots with observed demand is a real serving emergency,
		// not an ordinary observation shortage. Use the same half-waterline
		// minimum as the account-vacuum path instead of staging one credential.
		quantity = max(quantity, smartEmergencyMinimumOrderQuantity(cfg))
	}
	resource.SuggestedQuantity = max(resource.SuggestedQuantity, max(1, quantity))
	if resource.ConcurrencyFiniteSlots <= 0 {
		resource.EmergencyShortage = true
		resource.EmergencyReason = "concurrency_capacity_shortage"
		resource.HealthLevel = smartHealthCritical
		resource.SuggestedAction = smartActionEmergencyReplenish
		resource.DecisionReason = "concurrency_capacity_shortage"
		return
	}
	if !smartResourceEmergency(*resource) {
		resource.HealthLevel = smartHealthWarning
		resource.SuggestedAction = smartActionPrelock
		resource.DecisionReason = "concurrency_capacity_shortage"
	}
}

type smartUsageAggregate struct {
	requests30     int64
	successful30   int64
	tokens30       int64
	rpm30          float64
	rpm5Peak       float64
	tpm30          float64
	successful1    int64
	successful5    int64
	successful10   int64
	tokens1        int64
	tokens5        int64
	tokens10       int64
	rpm1           float64
	rpm5           float64
	rpm10          float64
	tpm1           float64
	tpm5           float64
	tpm10          float64
	latencySumMS   int64
	latencySamples int64
	avgLatencyMS   float64
	oneMinuteReady bool
	sampleMinutes  int
}

type smartDemandPlan struct {
	consumeRCU  float64
	planningRCU float64
	rcu1        float64
	rcu5        float64
	rcu10       float64
	requestRCU  float64
	tokenRCU    float64
	trend       string
}

func (s *Service) smartUsageSnapshot(now time.Time) smartUsageAggregate {
	s.smartMu.RLock()
	defer s.smartMu.RUnlock()
	result := smartUsageAggregate{}
	if len(s.smartBuckets) == 0 {
		return result
	}
	from30 := now.Add(-30*time.Minute).UnixMilli() / 60000 * 60000
	from5 := now.Add(-5*time.Minute).UnixMilli() / 60000 * 60000
	currentMinute := now.UnixMilli() / 60000 * 60000
	lastCompletedMinute := currentMinute - int64(time.Minute/time.Millisecond)
	from5Completed := lastCompletedMinute - 4*int64(time.Minute/time.Millisecond)
	from10Completed := lastCompletedMinute - 9*int64(time.Minute/time.Millisecond)
	firstMinute := int64(0)
	perMinute5 := map[int64]int64{}
	for minute, bucket := range s.smartBuckets {
		if bucket == nil || minute < from30 {
			continue
		}
		result.requests30 += bucket.requests
		result.successful30 += bucket.success
		result.tokens30 += bucket.totalTokens
		result.latencySumMS += bucket.latencySumMS
		result.latencySamples += bucket.latencySamples
		if bucket.success > 0 && (firstMinute == 0 || minute < firstMinute) {
			firstMinute = minute
		}
		if minute >= from5 {
			// Failed calls do not consume output/input tokens and should not make
			// the replenishment planner buy capacity for malformed/retried traffic.
			perMinute5[minute] += bucket.success
		}
		// Capacity planning uses whole, completed minute buckets only. The
		// in-progress minute is intentionally excluded so a partial bucket never
		// looks like a sudden demand collapse at the beginning of each minute.
		if minute == lastCompletedMinute {
			result.successful1 += bucket.success
			result.tokens1 += bucket.totalTokens
		}
		if minute >= from5Completed && minute <= lastCompletedMinute {
			result.successful5 += bucket.success
			result.tokens5 += bucket.totalTokens
		}
		if minute >= from10Completed && minute <= lastCompletedMinute {
			result.successful10 += bucket.success
			result.tokens10 += bucket.totalTokens
		}
	}
	if firstMinute > 0 {
		observedMinutes := int(now.Sub(time.UnixMilli(firstMinute)).Minutes()) + 1
		result.sampleMinutes = clampInt(observedMinutes, 1, 30)
	}
	if result.successful30 > 0 {
		// A warm Manager has a persisted 30-minute baseline. A cold Manager
		// divides by its actual observed span instead of pretending the partial
		// in-memory history already covers 30 minutes.
		denominator := math.Max(1, float64(result.sampleMinutes))
		result.rpm30 = float64(result.successful30) / denominator
		result.tpm30 = float64(result.tokens30) / denominator
	}
	// A zero in the most recently completed bucket is a meaningful demand
	// signal once at least two minutes have been observed: it should stop new
	// purchases quickly instead of leaving an old 30-minute average in charge.
	result.oneMinuteReady = result.sampleMinutes >= 2
	if result.oneMinuteReady {
		result.rpm1 = float64(result.successful1)
		result.tpm1 = float64(result.tokens1)
		result.rpm5 = float64(result.successful5) / 5
		result.tpm5 = float64(result.tokens5) / 5
		result.rpm10 = float64(result.successful10) / 10
		result.tpm10 = float64(result.tokens10) / 10
	}
	for _, count := range perMinute5 {
		if float64(count) > result.rpm5Peak {
			result.rpm5Peak = float64(count)
		}
	}
	if result.latencySamples > 0 {
		result.avgLatencyMS = float64(result.latencySumMS) / float64(result.latencySamples)
	}
	return result
}

func smartDemandPlanForUsage(usage smartUsageAggregate, unit float64) smartDemandPlan {
	historicalRequestRCU := math.Max(usage.rpm30, usage.rpm5Peak*0.7)
	historicalTokenRCU := smartTokenDemandRCU(usage.tpm30, unit)
	result := smartDemandPlan{
		rcu1:        smartWindowConsumeRCU(usage.rpm1, usage.tpm1, unit),
		rcu5:        smartWindowConsumeRCU(usage.rpm5, usage.tpm5, unit),
		rcu10:       smartWindowConsumeRCU(usage.rpm10, usage.tpm10, unit),
		requestRCU:  historicalRequestRCU,
		tokenRCU:    historicalTokenRCU,
		trend:       smartDemandTrendUnknown,
		consumeRCU:  math.Max(historicalRequestRCU, historicalTokenRCU),
		planningRCU: math.Max(historicalRequestRCU, historicalTokenRCU),
	}
	if !usage.oneMinuteReady {
		return result
	}

	baseline := math.Max(result.rcu5, result.rcu10)
	result.consumeRCU = result.rcu1
	result.planningRCU = result.rcu1
	result.requestRCU = math.Max(0, usage.rpm1)
	result.tokenRCU = smartTokenDemandRCU(usage.tpm1, unit)
	result.trend = smartDemandTrendStable
	// A rise is confirmed by comparing the last complete minute with two
	// broader windows. Its immediate burn still drives the health calculation,
	// while purchase sizing is held to the 5/10 minute baseline and capped to a
	// small observation batch elsewhere.
	if result.rcu1 > 0 && (baseline <= 0 || result.rcu1 >= baseline*1.6) {
		result.trend = smartDemandTrendRising
		if baseline > 0 {
			result.planningRCU = baseline
		}
		return result
	}
	// Demand falls are intentionally asymmetric. A completed low one-minute
	// bucket immediately becomes the active rate to prevent buying credentials
	// that will expire within an hour. New orders are paused by the caller.
	if baseline > 0 && result.rcu1 <= baseline*0.55 {
		result.trend = smartDemandTrendFalling
	}
	return result
}

func smartWindowConsumeRCU(rpm, tpm, unit float64) float64 {
	requestRCU := math.Max(0, rpm)
	tokenRCU := smartTokenDemandRCU(tpm, unit)
	return math.Max(requestRCU, tokenRCU)
}

// smartRequiredConcurrencySlots applies Little's Law to the observed request
// rate and successful-request duration, then keeps 20% headroom for normal
// jitter. It estimates instantaneous slots only; quota RCU remains an
// independent capacity dimension.
func smartRequiredConcurrencySlots(requestsPerMinute, averageLatencyMS float64) int {
	if requestsPerMinute <= 0 || averageLatencyMS <= 0 {
		return 0
	}
	concurrent := requestsPerMinute / 60 * averageLatencyMS / 1000
	return max(1, int(math.Ceil(concurrent*1.2)))
}

func smartTokenDemandRCU(tpm, unit float64) float64 {
	if unit <= 0 || tpm <= 0 {
		return 0
	}
	return tpm / 1000 / unit
}

func smartDemandDriver(requestRCU, tokenRCU float64) string {
	switch {
	case requestRCU <= 0 && tokenRCU <= 0:
		return "none"
	case tokenRCU > requestRCU:
		return "tokens"
	default:
		return "requests"
	}
}

func applySmartUsage(resource *SmartResource, usage smartUsageAggregate, unit float64) float64 {
	if resource == nil {
		return 0
	}
	demand := smartDemandPlanForUsage(usage, unit)
	resource.RPM30M = usage.rpm30
	resource.RPM5MPeak = usage.rpm5Peak
	resource.TPM30M = usage.tpm30
	resource.RPM1M = usage.rpm1
	resource.RPM5M = usage.rpm5
	resource.RPM10M = usage.rpm10
	resource.TPM1M = usage.tpm1
	resource.TPM5M = usage.tpm5
	resource.TPM10M = usage.tpm10
	resource.AverageRequestLatencyMS = round2(usage.avgLatencyMS)
	resource.ConcurrencyDemandObserved = usage.avgLatencyMS > 0 && demand.requestRCU > 0
	resource.RequiredConcurrencySlots = smartRequiredConcurrencySlots(demand.requestRCU, usage.avgLatencyMS)
	resource.RequestDemandRCUPerMinute = round2(demand.requestRCU)
	resource.TokenDemandRCUPerMinute = round2(demand.tokenRCU)
	resource.DemandDriver = smartDemandDriver(demand.requestRCU, demand.tokenRCU)
	resource.ConsumeRCU1M = round2(demand.rcu1)
	resource.ConsumeRCU5M = round2(demand.rcu5)
	resource.ConsumeRCU10M = round2(demand.rcu10)
	resource.DemandTrend = demand.trend
	resource.DemandPlanningRCUPerMinute = round2(demand.planningRCU)
	resource.ConsumeRCUPerMinute = round2(demand.consumeRCU)
	resource.UsageSampleMinutes = usage.sampleMinutes
	applySmartTokenMetrics(resource)
	return demand.consumeRCU
}

func applySmartTokenMetrics(resource *SmartResource) {
	if resource == nil {
		return
	}
	unit := resource.UnitCapacityRCU
	if unit <= 0 {
		unit = 1
	}
	resource.TokenCapacityMode = smartTokenCapacityMode
	if resource.AccountQuotaEstimateM <= 0 {
		resource.AccountQuotaEstimateM = smartDefaultAccountQuotaMillionTokens
	}
	resource.RawCapacityTokenM = round2(smartRCUToTokenMillion(resource.RawCapacityRCU, unit))
	if resource.AvailableCapacityRCU <= 0 && resource.FrozenCapacityRCU <= 0 && resource.TotalCapacityRCU <= 0 && resource.CurrentCapacityRCU > 0 {
		// Compatibility for resources created before capacity splitting existed.
		// A missing split means unknown metadata, not that every account is frozen.
		resource.AvailableCapacityRCU = resource.CurrentCapacityRCU
	}
	resource.TotalCapacityRCU = round2(math.Max(0, resource.CurrentCapacityRCU))
	resource.AvailableCapacityRCU = round2(clampFloat(resource.AvailableCapacityRCU, 0, resource.TotalCapacityRCU))
	resource.FrozenCapacityRCU = round2(math.Max(0, resource.TotalCapacityRCU-resource.AvailableCapacityRCU))
	resource.CurrentCapacityTokenM = round2(smartRCUToTokenMillion(resource.CurrentCapacityRCU, unit))
	resource.AvailableCapacityTokenM = round2(smartRCUToTokenMillion(resource.AvailableCapacityRCU, unit))
	resource.FrozenCapacityTokenM = round2(smartRCUToTokenMillion(resource.FrozenCapacityRCU, unit))
	resource.TotalCapacityTokenM = resource.CurrentCapacityTokenM
	resource.TimeLimitedCapacityTokenM = round2(smartRCUToTokenMillion(resource.TimeLimitedCapacityRCU, unit))
	resource.ExpiryWasteRiskTokenM = round2(smartRCUToTokenMillion(resource.ExpiryWasteRiskRCU, unit))
	resource.ObservedTokenM1M = round2(math.Max(0, resource.TPM1M) / 1_000_000)
	resource.ObservedTokenM5M = round2(math.Max(0, resource.TPM5M) / 1_000_000)
	resource.ObservedTokenM10M = round2(math.Max(0, resource.TPM10M) / 1_000_000)
	resource.ObservedTokenM30M = round2(math.Max(0, resource.TPM30M) / 1_000_000)
	resource.ConsumeTokenM1M = round2(smartRCUToTokenMillion(resource.ConsumeRCU1M, unit))
	resource.ConsumeTokenM5M = round2(smartRCUToTokenMillion(resource.ConsumeRCU5M, unit))
	resource.ConsumeTokenM10M = round2(smartRCUToTokenMillion(resource.ConsumeRCU10M, unit))
	resource.ConsumeTokenMPerMinute = round2(smartRCUToTokenMillion(resource.ConsumeRCUPerMinute, unit))
	resource.DemandPlanningTokenMPerMinute = round2(smartRCUToTokenMillion(resource.DemandPlanningRCUPerMinute, unit))
	forecastRCUPerMinute := math.Max(resource.ConsumeRCUPerMinute, resource.DemandPlanningRCUPerMinute)
	resource.ForecastSustainMinutes = 0
	resource.RawSustainMinutes = 0
	resource.ExpiryLimitedSustainMinutes = 0
	resource.AvailableSustainMinutes = 0
	resource.FrozenSustainMinutes = 0
	resource.NextCapacityDeficitAtMS = 0
	if forecastRCUPerMinute > 0 {
		resource.RawSustainMinutes = round1(resource.RawCapacityRCU / forecastRCUPerMinute)
		resource.ExpiryLimitedSustainMinutes = round1(resource.CurrentCapacityRCU / forecastRCUPerMinute)
		resource.ForecastSustainMinutes = round1(resource.CurrentCapacityRCU / forecastRCUPerMinute)
		resource.AvailableSustainMinutes = round1(resource.AvailableCapacityRCU / forecastRCUPerMinute)
		resource.FrozenSustainMinutes = round1(resource.FrozenCapacityRCU / forecastRCUPerMinute)
		if resource.GeneratedAtMS > 0 && resource.CurrentCapacityRCU > 0 {
			resource.NextCapacityDeficitAtMS = resource.GeneratedAtMS + int64(resource.CurrentCapacityRCU/forecastRCUPerMinute*float64(time.Minute/time.Millisecond))
		}
	}
	if resource.NearestExpiryAtMS > resource.GeneratedAtMS && resource.GeneratedAtMS > 0 {
		resource.NearestExpiryMinutes = round1(float64(resource.NearestExpiryAtMS-resource.GeneratedAtMS) / float64(time.Minute/time.Millisecond))
	} else if resource.NearestExpiryAtMS > 0 {
		resource.NearestExpiryMinutes = 0
	}
	resource.TargetCapacityTokenM = round2(smartRCUToTokenMillion(resource.TargetCapacityRCU, unit))
	resource.CapacityGapTokenM = round2(smartRCUToTokenMillion(resource.CapacityGapRCU, unit))
	resource.EstimatedNewAccountCapacityTokenM = round2(smartRCUToTokenMillion(resource.EstimatedNewAccountCapacityRCU, unit))
	resource.PrelockedCapacityTokenM = round2(smartRCUToTokenMillion(resource.PrelockedCapacityRCU, unit))
	resource.ProjectedCapacityAfterRefillTokenM = round2(smartRCUToTokenMillion(resource.ProjectedCapacityAfterRefillRCU, unit))
}

func (s *Service) applySmartDemandMemory(cfg store.ManagerSupplyConfig, resource *SmartResource, now time.Time, currentRCU float64) float64 {
	if resource == nil {
		return currentRCU
	}
	if !smartSupplyStrategyConfigured(cfg) {
		return currentRCU
	}
	ttl := time.Duration(smartVirtualDemandTTLMinutes(cfg)) * time.Minute
	memoryRCU, lastSeenMS, ageSeconds := s.smartDemandMemory(now, ttl, resource.UnitCapacityRCU)
	resource.DemandMemoryRCUPerMinute = round2(memoryRCU)
	resource.DemandMemoryLastSeenMS = lastSeenMS
	resource.DemandMemoryAgeSeconds = ageSeconds
	resource.VirtualDemandTTLMinutes = smartVirtualDemandTTLMinutes(cfg)
	if currentRCU > 0 || memoryRCU <= 0 || !smartEmergencyBypassUsageRate(cfg) {
		return currentRCU
	}
	resource.VirtualDemandRCUPerMinute = round2(memoryRCU)
	resource.DemandPlanningRCUPerMinute = round2(memoryRCU)
	resource.DemandTrend = smartDemandTrendVirtual
	resource.TargetCapacityRCU = round2(memoryRCU * float64(resource.EffectiveHealthyMinutes))
	resource.RecommendedCapacityRCU = resource.TargetCapacityRCU
	resource.EstimatedSustainMinutes = 0
	resource.CapacityGapRCU = round2(math.Max(0, resource.TargetCapacityRCU-resource.CurrentCapacityRCU-resource.PrelockedCapacityRCU))
	resource.DecisionReason = "virtual_demand_memory"
	// Demand memory is a low-water guard, not current traffic. Keep the actual
	// rate at zero so an idle pool does not display stale consumption or size a
	// normal capacity order from historical load. The account waterline may
	// still request its small emergency batch when the pool is truly critical.
	return currentRCU
}

func (s *Service) smartDemandMemory(now time.Time, ttl time.Duration, unit float64) (float64, int64, int) {
	if s == nil || ttl <= 0 {
		return 0, 0, 0
	}
	from := now.Add(-ttl).UnixMilli() / 60000 * 60000
	s.smartMu.RLock()
	defer s.smartMu.RUnlock()
	if len(s.smartBuckets) == 0 {
		return 0, 0, 0
	}
	var last int64
	for minute, bucket := range s.smartBuckets {
		if bucket != nil && minute >= from && bucket.success > 0 && minute > last {
			last = minute
		}
	}
	if last <= 0 {
		return 0, 0, 0
	}
	// Preserve the rate from the final non-zero ten-minute window. Idle time
	// after that window is represented by ageSeconds and must not dilute the
	// remembered rate before the selected strategy TTL expires.
	windowStart := last - int64((9*time.Minute)/time.Millisecond)
	var success, tokens int64
	var first int64
	peak := 0.0
	for minute, bucket := range s.smartBuckets {
		if bucket == nil || minute < windowStart || minute > last {
			continue
		}
		if first == 0 || minute < first {
			first = minute
		}
		success += bucket.success
		tokens += bucket.totalTokens
		if float64(bucket.success) > peak {
			peak = float64(bucket.success)
		}
	}
	if success <= 0 || first <= 0 || last <= 0 {
		return 0, 0, 0
	}
	denominator := math.Max(1, float64((last-first)/int64(time.Minute/time.Millisecond)+1))
	rpm := float64(success) / denominator
	tpm := float64(tokens) / denominator
	rcu := smartConsumeRCUPerMinute(rpm, peak, tpm, unit)
	return rcu, last, max(0, int(now.Sub(time.UnixMilli(last)).Seconds()))
}

func applySmartEmergencyAvailability(cfg store.ManagerSupplyConfig, resource *SmartResource, now time.Time) {
	if resource == nil || !smartSupplyStrategyConfigured(cfg) {
		return
	}
	startup := smartStartupAvailableAccounts(cfg)
	startupFloorEmergency := smartStartupAccountFloorEmergency(cfg, *resource)
	if !smartAvailableCapacityEmergency(cfg, *resource) {
		return
	}
	// The startup account floor protects an idle pool. When the last usable
	// credential disappears, successful traffic also becomes zero; relying only
	// on burn-rate demand would therefore stop buying at exactly the moment the
	// pool needs credentials to start serving again. Once live demand is flowing,
	// the available runway and critical account line decide whether the pool is
	// actually in an emergency.
	//
	// A pool outage is different from genuine idleness: once the last account
	// disappears, the current success rate immediately becomes zero even while
	// recent real demand remains in DemandPlanningRCUPerMinute. Treat that
	// remembered rate as sizing evidence. Falling back to the fixed emergency
	// batch in this state creates several credentials at the same timestamp and
	// recreates the expiry-waste pattern that progressive replenishment avoids.
	sizingResource, demandObserved := smartEmergencySizingResource(*resource)
	if !demandObserved && resource.AvailableAccounts > 0 {
		if resource.AvailableAccounts >= startup {
			if smartAccountAvailabilityEmergencyReason(resource.EmergencyReason) {
				resource.EmergencyShortage = false
				resource.EmergencyReason = ""
				resource.PoolVacuumActive = false
				resource.PoolVacuumStartedAtMS = 0
				resource.PoolVacuumDurationSeconds = 0
			}
			return
		}
	}
	reason := "available_capacity_critical"
	if startupFloorEmergency && cfg.StartupAvailableAccounts != nil {
		reason = "startup_account_floor"
	}
	if resource.AvailableAccounts <= 0 {
		reason = "emergency_pool_vacuum"
		resource.PoolVacuumActive = true
		if resource.PoolVacuumStartedAtMS <= 0 {
			resource.PoolVacuumStartedAtMS = now.UnixMilli()
		}
		resource.PoolVacuumDurationSeconds = max(0, int(now.Sub(time.UnixMilli(resource.PoolVacuumStartedAtMS)).Seconds()))
	}
	resource.EmergencyShortage = true
	resource.EmergencyReason = reason
	resource.HealthLevel = smartHealthCritical
	resource.SuggestedAction = smartActionEmergencyReplenish
	resource.DecisionReason = reason
	refillQuantity := smartMinimumAvailableRefillQuantity(cfg, sizingResource)
	if !demandObserved {
		refillQuantity = smartIdleEmergencyRefillQuantity(cfg, resource.AvailableAccounts)
	}
	resource.SuggestedQuantity = refillQuantity
	if resource.CapacityGapRCU <= 0 && resource.ConsumeRCUPerMinute > 0 {
		resource.CapacityGapRCU = round2(math.Max(0, resource.TargetCapacityRCU-resource.CurrentCapacityRCU-resource.PrelockedCapacityRCU))
	}
	applySmartRefillProjection(cfg, resource)
}

// smartEmergencySizingResource keeps the displayed current burn rate honest
// while still allowing recent real demand to size an emergency refill after a
// pool outage. DemandPlanningRCUPerMinute is populated from the last completed
// non-zero window and bounded by the strategy TTL, so it is materially better
// evidence than the fixed emergency account minimum without pretending that
// requests are currently succeeding.
func smartEmergencySizingResource(resource SmartResource) (SmartResource, bool) {
	if resource.ConsumeRCUPerMinute > 0 {
		return resource, true
	}
	if resource.DemandMemoryAgeSeconds > int(smartEmergencyDemandMemoryMaxAge/time.Second) {
		return resource, false
	}
	planningRate := math.Max(resource.DemandPlanningRCUPerMinute, resource.VirtualDemandRCUPerMinute)
	if planningRate <= 0 {
		return resource, false
	}
	resource.ConsumeRCUPerMinute = planningRate
	return resource, true
}

func smartAvailableCapacity(resource SmartResource) float64 {
	if resource.AvailableCapacityRCU > 0 || resource.FrozenCapacityRCU > 0 || resource.TotalCapacityRCU > 0 {
		return math.Max(0, resource.AvailableCapacityRCU)
	}
	return math.Max(0, resource.CurrentCapacityRCU)
}

// smartStartupAccountFloorEmergency keeps a small pool ready while there is no
// live traffic. Once demand is flowing, the capacity runway and the critical
// account waterline own the purchase decision; treating the higher startup
// floor as an emergency would bypass just-in-time timing even when the pool has
// more than enough available runway.
func smartStartupAccountFloorEmergency(cfg store.ManagerSupplyConfig, resource SmartResource) bool {
	if resource.ConsumeRCUPerMinute > 0 {
		return false
	}
	_, recentDemandObserved := smartEmergencySizingResource(resource)
	if recentDemandObserved {
		return false
	}
	// A virtual/planning demand sample can outlive the short demand-memory
	// window.  It still represents usable runway, so an idle pool with a healthy
	// capacity baseline must not be escalated to emergency merely because its
	// account count is below the startup floor.  The startup floor is intended
	// for a genuinely empty/low-capacity pool; the capacity critical line remains
	// the emergency boundary when a stale demand estimate is all that remains.
	planningRate := math.Max(resource.DemandPlanningRCUPerMinute, resource.VirtualDemandRCUPerMinute)
	planningRate = math.Max(planningRate, resource.DemandMemoryRCUPerMinute)
	if planningRate > 0 {
		availableCapacity := smartAvailableCapacity(resource)
		if availableCapacity > 0 {
			runwayMinutes := availableCapacity / planningRate
			criticalMinutes := max(1, resource.CriticalMinutes)
			if runwayMinutes > float64(criticalMinutes) {
				return false
			}
		}
	}
	return resource.AvailableAccounts < smartStartupAvailableAccounts(cfg)
}

func smartAvailableCapacityEmergency(cfg store.ManagerSupplyConfig, resource SmartResource) bool {
	if smartStartupAccountFloorEmergency(cfg, resource) {
		return true
	}
	criticalAccounts := smartCriticalAvailableAccounts(cfg)
	if resource.AvailableAccounts <= criticalAccounts {
		return true
	}
	if resource.ConsumeRCUPerMinute <= 0 || resource.CriticalMinutes <= 0 {
		return false
	}
	criticalMinutes := resource.CriticalMinutes
	if smartExtendedWaterlineProgressiveMode(resource) {
		criticalMinutes = min(criticalMinutes, max(1, smartUsefulAccountLifetimeMinutes()/4))
	}
	return smartAvailableCapacity(resource)/resource.ConsumeRCUPerMinute <= float64(criticalMinutes)
}

// smartMinimumAvailableRefillQuantity buys only enough immediately usable
// capacity to cross the critical runway/account line. Frozen credentials still
// count toward ordinary pool health and can recover without being replaced.
func smartMinimumAvailableRefillQuantity(cfg store.ManagerSupplyConfig, resource SmartResource) int {
	if resource.ConsumeRCUPerMinute <= 0 {
		return smartIdleEmergencyRefillQuantity(cfg, resource.AvailableAccounts)
	}
	unit := smartEstimatedNewAccountCapacityForResource(cfg, resource)
	if unit <= 0 && resource.UnitCapacityRCU > 0 {
		unit = smartEstimatedAccountCapacityRCU(resource.UnitCapacityRCU, float64(smartCapacityPlanningHorizonMinutes(cfg)))
	}
	if unit <= 0 {
		return 1
	}
	criticalMinutes := max(1, resource.CriticalMinutes)
	if smartExtendedWaterlineProgressiveMode(resource) {
		criticalMinutes = min(criticalMinutes, max(1, smartUsefulAccountLifetimeMinutes()/4))
	}
	targetCapacity := resource.ConsumeRCUPerMinute * float64(criticalMinutes+1)
	capacityGap := math.Max(0, targetCapacity-smartAvailableCapacity(resource)-resource.PrelockedCapacityRCU)
	capacityQuantity := int(math.Ceil(capacityGap / unit))
	pendingAccounts := 0
	if resource.PrelockedCapacityRCU > 0 {
		pendingAccounts = int(math.Ceil(resource.PrelockedCapacityRCU / unit))
	}
	accountTarget := smartCriticalAvailableAccounts(cfg) + 1
	accountQuantity := max(0, accountTarget-resource.AvailableAccounts-pendingAccounts)
	quantity := max(capacityQuantity, accountQuantity)
	if quantity <= 0 {
		return 0
	}
	limit := smartReplenishBatchLimit(cfg)
	if smartPrelockEnabled(cfg) {
		limit = min(limit, smartPrelockMaxQuantity(cfg))
	}
	return clampInt(quantity, 1, max(1, limit))
}

func (s *Service) cachedAuthFiles(ctx context.Context, cfg store.ManagerConfig, force bool) (authFileSnapshot, error) {
	if strings.TrimSpace(cfg.CPAConnection.CPABaseURL) == "" || strings.TrimSpace(cfg.CPAConnection.ManagementKey) == "" {
		return authFileSnapshot{}, ErrNotConfigured
	}
	ttl := time.Duration(smartAuthFilesCacheTTLSeconds(cfg.Supply)) * time.Second
	now := time.Now()
	s.authCacheMu.Lock()
	if !force && authSnapshotCacheUsable(s.authCache, now, ttl) {
		snapshot := cloneAuthSnapshot(s.authCache)
		s.authCacheMu.Unlock()
		return snapshot, snapshot.lastErr
	}
	s.authCacheMu.Unlock()

	s.authRefreshMu.Lock()
	defer s.authRefreshMu.Unlock()
	s.authCacheMu.Lock()
	if !force && authSnapshotCacheUsable(s.authCache, now, ttl) {
		snapshot := cloneAuthSnapshot(s.authCache)
		s.authCacheMu.Unlock()
		return snapshot, snapshot.lastErr
	}
	s.authCacheMu.Unlock()

	files := make([]cpaauthfiles.File, 0)
	err := s.authFiles.Visit(ctx, cfg.CPAConnection.CPABaseURL, cfg.CPAConnection.ManagementKey, func(file cpaauthfiles.File) (bool, error) {
		files = append(files, file)
		return false, nil
	})
	s.authCacheMu.Lock()
	attemptedAt := time.Now()
	if err == nil {
		s.authCache = authFileSnapshot{files: files, generatedAt: attemptedAt, attemptedAt: attemptedAt}
	} else {
		s.authCache.attemptedAt = attemptedAt
		s.authCache.lastErr = err
	}
	snapshot := cloneAuthSnapshot(s.authCache)
	s.authCacheMu.Unlock()
	return snapshot, err
}

func (s *Service) cachedInspectionQuotaSnapshot(ctx context.Context, cfg store.ManagerSupplyConfig, force bool) (inspectionQuotaSnapshot, error) {
	if s == nil || s.store == nil {
		return inspectionQuotaSnapshot{}, ErrCapacitySnapshotUnavailable
	}
	// The completed inspection is a durable historical baseline. Runtime quota
	// headers are merged account-by-account in memory, so re-reading every
	// result and re-aggregating the usage table every auth-file TTL defeats the
	// local-update path. Keep the baseline for its freshness window; explicit
	// inspection/import invalidation and the lightweight newer-run check still
	// replace it sooner when needed.
	ttl := smartInspectionSnapshotFreshTTL
	now := time.Now()
	s.quotaSnapshotMu.Lock()
	if !force && inspectionSnapshotCacheUsable(s.quotaSnapshot, now, ttl) {
		snapshot := cloneInspectionQuotaSnapshot(s.quotaSnapshot)
		s.quotaSnapshotMu.Unlock()
		return snapshot, inspectionSnapshotReadError(snapshot, now)
	}
	s.quotaSnapshotMu.Unlock()

	s.quotaRefreshMu.Lock()
	defer s.quotaRefreshMu.Unlock()
	s.quotaSnapshotMu.Lock()
	if !force && inspectionSnapshotCacheUsable(s.quotaSnapshot, now, ttl) {
		snapshot := cloneInspectionQuotaSnapshot(s.quotaSnapshot)
		s.quotaSnapshotMu.Unlock()
		return snapshot, inspectionSnapshotReadError(snapshot, now)
	}
	s.quotaSnapshotMu.Unlock()

	refreshed, err := s.loadLatestInspectionQuotaSnapshot(ctx, cfg)
	attemptedAt := time.Now()
	if err == nil {
		s.recordSmartQuotaWindowBaselines(refreshed.quotaWindowUsage, attemptedAt)
	}
	s.quotaSnapshotMu.Lock()
	if err == nil {
		refreshed.attemptedAt = attemptedAt
		refreshed.lastErr = nil
		s.quotaSnapshot = refreshed
	} else {
		s.quotaSnapshot.attemptedAt = attemptedAt
		s.quotaSnapshot.lastErr = err
	}
	snapshot := cloneInspectionQuotaSnapshot(s.quotaSnapshot)
	s.quotaSnapshotMu.Unlock()
	return snapshot, inspectionSnapshotReadError(snapshot, attemptedAt)
}

// inspectionSnapshotReadError keeps a recent completed capacity baseline
// usable when only an optional refresh attempt times out. lastErr remains on
// the cached snapshot for diagnostics and retry pacing, but it must not turn a
// still-fresh historical baseline into a stale decision. Doing so can make one
// empty usage bucket fall through to startup_account_floor and purchase a
// full-price credential while the prior capacity decision is explicitly
// waiting for the just-in-time trigger.
func inspectionSnapshotReadError(snapshot inspectionQuotaSnapshot, now time.Time) error {
	if !snapshot.generatedAt.IsZero() && smartInspectionSnapshotFresh(snapshot, now) {
		return nil
	}
	return snapshot.lastErr
}

func (s *Service) loadLatestInspectionQuotaSnapshot(ctx context.Context, configs ...store.ManagerSupplyConfig) (inspectionQuotaSnapshot, error) {
	var cfg store.ManagerSupplyConfig
	if len(configs) > 0 {
		cfg = configs[0]
	}
	runs, err := s.store.ListCodexInspectionRuns(ctx, latestSmartInspectionSearchLimit)
	if err != nil {
		return inspectionQuotaSnapshot{}, err
	}
	for _, run := range runs {
		if run.Status != "completed" {
			continue
		}
		results, err := s.store.ListCodexInspectionResults(ctx, run.ID)
		if err != nil {
			return inspectionQuotaSnapshot{}, err
		}
		filtered := make([]store.CodexInspectionResult, 0, len(results))
		for _, result := range results {
			if isSmartCapacityInspectionResult(result) {
				filtered = append(filtered, result)
			}
		}
		// A read-only Codex supply snapshot can legitimately contain no
		// results when the pool is empty. Preserve that completed run as a
		// trusted zero-capacity baseline so first-enable automation does not
		// wait forever for a result row that cannot exist.
		if len(filtered) == 0 && !smartInspectionRunIsTrustedEmptySupplySnapshot(run) {
			continue
		}
		importItems, err := s.store.ListCurrentImportedSupplyItems(ctx)
		if err != nil {
			return inspectionQuotaSnapshot{}, err
		}
		currentImportItems := make([]store.SupplyImportItem, 0, len(importItems))
		orderIDs := make([]string, 0, len(importItems))
		orderSeen := make(map[string]struct{}, len(importItems))
		for _, item := range importItems {
			orderID := strings.TrimSpace(item.OrderID)
			if orderID == "" {
				continue
			}
			if _, exists := orderSeen[orderID]; exists {
				continue
			}
			orderSeen[orderID] = struct{}{}
			orderIDs = append(orderIDs, orderID)
		}
		orders, err := s.store.ListSupplyOrdersByIDs(ctx, orderIDs)
		if err != nil {
			return inspectionQuotaSnapshot{}, err
		}
		orderByID := make(map[string]store.SupplyOrder, len(orders))
		for _, order := range orders {
			orderByID[strings.TrimSpace(order.OrderID)] = order
		}
		supplierByFile := make(map[string]string, len(importItems))
		credentialEffectiveFromByFile := make(map[string]int64, len(importItems))
		for _, item := range importItems {
			fileName := strings.TrimSpace(item.FileName)
			if fileName == "" {
				continue
			}
			credentialEffectiveFromByFile[fileName] = maxInt64(
				credentialEffectiveFromByFile[fileName],
				maxInt64(item.EffectiveFromMS, item.ImportedAtMS),
			)
			order := orderByID[strings.TrimSpace(item.OrderID)]
			supplierID := normalizeSmartQuotaSupplierID(order.SupplierID)
			if supplierID == "" && strings.TrimSpace(order.Product) != "" {
				if platform, resolveErr := resolveSupplyPlatform(cfg, "", order.Product); resolveErr == nil {
					supplierID = normalizeSmartQuotaSupplierID(platform.ID)
				}
			}
			if supplierID != "" {
				supplierByFile[fileName] = supplierID
			}
		}
		platforms := supplyPlatforms(cfg)
		if len(platforms) == 1 {
			supplierID := normalizeSmartQuotaSupplierID(platforms[0].ID)
			if supplierID != "" {
				for _, result := range filtered {
					fileName := strings.TrimSpace(result.FileName)
					if fileName != "" && normalizeSmartQuotaSupplierID(supplierByFile[fileName]) == "" {
						supplierByFile[fileName] = supplierID
					}
				}
			}
		}
		quotaWindowUsage := []smartQuotaWindowBaseline{}
		// Absolute quota-window calibration is useful only for a current
		// inspection. When a degraded refresh burst forces recovery of an older
		// completed baseline, aggregating every historical account window from a
		// multi-million-row usage table can exceed the status deadline and hide
		// the otherwise valid capacity snapshot again. Capacity itself comes from
		// the persisted inspection results, so skip this optional calibration for
		// an already stale recovered run and use the configured/adopted estimate.
		if smartInspectionSnapshotFresh(inspectionQuotaSnapshot{generatedAt: time.UnixMilli(run.FinishedAtMS)}, time.Now()) {
			var quotaWindowTargets []store.SupplyQuotaWindowUsageQuery
			quotaWindowUsage, quotaWindowTargets = smartQuotaWindowBaselinesForInspection(
				filtered,
				run,
				supplierByFile,
				credentialEffectiveFromByFile,
			)
			if len(quotaWindowTargets) > 0 {
				usageRows, err := s.store.ListSupplyQuotaWindowUsage(ctx, quotaWindowTargets)
				if err != nil {
					return inspectionQuotaSnapshot{}, err
				}
				usageByRequest := make(map[int]store.SupplyQuotaWindowUsage, len(usageRows))
				for _, usageRow := range usageRows {
					usageByRequest[usageRow.RequestIndex] = usageRow
				}
				for index := range quotaWindowUsage {
					usageRow := usageByRequest[quotaWindowUsage[index].requestIndex]
					quotaWindowUsage[index].windowTokens = usageRow.TotalTokens
					quotaWindowUsage[index].firstSeenMS = usageRow.FirstSeenMS
					quotaWindowUsage[index].lastSeenMS = usageRow.LastSeenMS
				}
			}
		}
		leaseExpiresByFile := make(map[string]int64, len(importItems))
		accountExpiresByFile := make(map[string]int64, len(importItems))
		accountExpiryEffectiveByFile := make(map[string]int64, len(importItems))
		snapshotNow := time.Now()
		for _, item := range importItems {
			fileName := strings.TrimSpace(item.FileName)
			if fileName == "" {
				continue
			}
			currentImportItems = append(currentImportItems, item)
			if item.LeaseExpiresAtMS > 0 {
				leaseExpiresByFile[fileName] = maxInt64(leaseExpiresByFile[fileName], item.LeaseExpiresAtMS)
			}
			effectiveAtMS := maxInt64(item.EffectiveFromMS, item.ImportedAtMS)
			if previousEffectiveAtMS, exists := accountExpiryEffectiveByFile[fileName]; !exists || effectiveAtMS >= previousEffectiveAtMS {
				accountExpiryEffectiveByFile[fileName] = effectiveAtMS
				if expiresAtMS := supplyAccountExpiryAtMS(nil, item.PayloadJSON, snapshotNow); expiresAtMS > 0 {
					accountExpiresByFile[fileName] = expiresAtMS
				} else {
					delete(accountExpiresByFile, fileName)
				}
			}
		}
		generatedAt := time.UnixMilli(run.FinishedAtMS)
		if run.FinishedAtMS <= 0 {
			generatedAt = time.UnixMilli(run.UpdatedAtMS)
		}
		return inspectionQuotaSnapshot{
			run:                  run,
			results:              filtered,
			quotaWindowUsage:     quotaWindowUsage,
			leaseExpiresByFile:   leaseExpiresByFile,
			accountExpiresByFile: accountExpiresByFile,
			supplierByFile:       supplierByFile,
			activeImportItems:    currentImportItems,
			generatedAt:          generatedAt,
		}, nil
	}
	return inspectionQuotaSnapshot{}, ErrCapacitySnapshotUnavailable
}

func inspectionSnapshotCacheUsable(snapshot inspectionQuotaSnapshot, now time.Time, ttl time.Duration) bool {
	if !snapshot.generatedAt.IsZero() && now.Sub(snapshot.attemptedAt) <= ttl {
		return true
	}
	return !snapshot.attemptedAt.IsZero() && now.Sub(snapshot.attemptedAt) <= ttl
}

func cloneInspectionQuotaSnapshot(snapshot inspectionQuotaSnapshot) inspectionQuotaSnapshot {
	results := make([]store.CodexInspectionResult, len(snapshot.results))
	copy(results, snapshot.results)
	snapshot.results = results
	quotaWindowUsage := make([]smartQuotaWindowBaseline, len(snapshot.quotaWindowUsage))
	copy(quotaWindowUsage, snapshot.quotaWindowUsage)
	snapshot.quotaWindowUsage = quotaWindowUsage
	items := make([]store.SupplyImportItem, len(snapshot.activeImportItems))
	copy(items, snapshot.activeImportItems)
	snapshot.activeImportItems = items
	leases := make(map[string]int64, len(snapshot.leaseExpiresByFile))
	for fileName, expiresAtMS := range snapshot.leaseExpiresByFile {
		leases[fileName] = expiresAtMS
	}
	snapshot.leaseExpiresByFile = leases
	expires := make(map[string]int64, len(snapshot.accountExpiresByFile))
	for fileName, expiresAtMS := range snapshot.accountExpiresByFile {
		expires[fileName] = expiresAtMS
	}
	snapshot.accountExpiresByFile = expires
	suppliers := make(map[string]string, len(snapshot.supplierByFile))
	for fileName, supplierID := range snapshot.supplierByFile {
		suppliers[fileName] = supplierID
	}
	snapshot.supplierByFile = suppliers
	return snapshot
}

func smartInspectionSnapshotFresh(snapshot inspectionQuotaSnapshot, now time.Time) bool {
	if snapshot.generatedAt.IsZero() || now.Before(snapshot.generatedAt) {
		return false
	}
	return now.Sub(snapshot.generatedAt) <= smartInspectionSnapshotFreshTTL
}

func smartInspectionSnapshotComplete(snapshot inspectionQuotaSnapshot) bool {
	if snapshot.run.ProbeSetCount > 0 {
		return snapshot.run.SampledCount >= snapshot.run.ProbeSetCount
	}
	return smartInspectionRunIsTrustedEmptySupplySnapshot(snapshot.run)
}

func smartInspectionRunIsTrustedEmptySupplySnapshot(run store.CodexInspectionRun) bool {
	if run.Status != model.CodexInspectionStatusCompleted ||
		run.TriggerType != model.CodexInspectionTriggerSupplySnapshot ||
		run.ProbeSetCount != 0 || run.SampledCount != 0 {
		return false
	}
	targets := run.Settings.TargetProviders()
	return len(targets) == 1 && targets[0] == model.CodexInspectionTargetCodex
}

func isSmartCapacityInspectionResult(result store.CodexInspectionResult) bool {
	switch strings.ToLower(strings.TrimSpace(result.Provider)) {
	case "codex", "openai", "openai-codex":
		return true
	default:
		return false
	}
}

func inspectionResultCapacityExcluded(result store.CodexInspectionResult) bool {
	if result.IsQuota {
		return true
	}
	action := strings.ToLower(strings.TrimSpace(result.Action))
	switch action {
	case "reauth", "delete":
		return true
	}
	if result.StatusCode != nil {
		switch *result.StatusCode {
		case 401, 402, 403, 404, 410:
			return true
		}
	}
	message := strings.ToLower(strings.Join([]string{
		result.Status,
		result.State,
		result.ActionReason,
		result.ErrorKind,
		result.Error,
		result.ErrorDetail,
	}, " "))
	if strings.Contains(message, "invalid") ||
		strings.Contains(message, "deactivated") ||
		strings.Contains(message, "expired") ||
		strings.Contains(message, "revoked") ||
		strings.Contains(message, "unauthorized") ||
		strings.Contains(message, "forbidden") ||
		strings.Contains(message, "quota") ||
		strings.Contains(message, "usage_limit_reached") {
		return true
	}
	// A transient cooldown may temporarily mark a file disabled. It does not
	// erase a credential's verified remaining quota, so retain it in capacity.
	if inspectionResultInCooldown(result) {
		return false
	}
	return result.Disabled
}

func inspectionResultUsabilityUnverified(result store.CodexInspectionResult) bool {
	if inspectionResultInCooldown(result) {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(result.Status))
	state := strings.ToLower(strings.TrimSpace(result.State))
	if status != "error" && state != "error" {
		return false
	}
	// The file's runtime status may lag behind a completed inspection. A fresh
	// successful quota response with quota evidence proves the credential is
	// usable now, so a stale `status=error` must not remove it from capacity.
	// Failed probes still have no 2xx status (or no quota evidence) and remain
	// excluded by the conservative path below.
	if result.StatusCode != nil && *result.StatusCode >= 200 && *result.StatusCode < 300 &&
		strings.TrimSpace(result.Error) == "" && strings.TrimSpace(result.ErrorKind) == "" &&
		strings.TrimSpace(result.ErrorDetail) == "" {
		_, hasQuota := inspectionResultRemainingQuotaFraction(result)
		return !hasQuota
	}
	return true
}

func inspectionResultInCooldown(result store.CodexInspectionResult) bool {
	message := strings.ToLower(strings.Join([]string{result.Status, result.State, result.ActionReason, result.ErrorKind, result.Error, result.ErrorDetail}, " "))
	return strings.Contains(message, "cooldown") ||
		strings.Contains(message, "cooling") ||
		strings.Contains(message, "retry_after")
}

func smartSupplyManagedFileName(fileName string) bool {
	fileName = strings.ToLower(strings.TrimSpace(fileName))
	return strings.HasPrefix(fileName, "codex-supply-") || strings.HasPrefix(fileName, "supply-")
}

const (
	smartQuotaFiveHourSeconds                  = 5 * 60 * 60
	smartQuotaWeekSeconds                      = 7 * 24 * 60 * 60
	smartQuotaMonthSeconds                     = 30 * 24 * 60 * 60
	smartNormalAccountMinimumRemainingFraction = 0.20
)

// inspectionResultRemainingQuotaFraction returns the capacity fraction that
// can be used before the credential's next limiting quota window is
// exhausted. The shortest available window is always selected. Monthly quota
// therefore acts only as a fallback for providers that do not expose a 5-hour
// or weekly window; it is never added to a shorter-window allowance.
//
// A credential can expose both five-hour and weekly limits. The shortest
// window is the one that will block it first, so only that window contributes
// to the supply capacity calculation. If multiple windows have the same
// shortest duration, use the lowest remaining fraction as the conservative
// bound. Results written before quota windows were persisted retain the legacy
// UsedPercent fallback.
func inspectionResultRemainingQuotaFraction(result store.CodexInspectionResult) (float64, bool) {
	if len(result.QuotaWindows) == 0 {
		if result.UsedPercent == nil {
			return 0, false
		}
		used := *result.UsedPercent
		if used > 1 {
			used /= 100
		}
		return clampFloat(1-used, 0, 1), true
	}

	shortestSeconds := math.MaxFloat64
	remainingAtShortest := 1.0
	found := false
	for _, window := range result.QuotaWindows {
		if inspectionQuotaWindowExcludedFromCapacity(window) || window.UsedPercent == nil {
			continue
		}
		used := *window.UsedPercent
		if used > 1 {
			used /= 100
		}
		remaining := clampFloat(1-used, 0, 1)
		seconds := inspectionQuotaWindowDurationSeconds(window)
		switch {
		case !found || seconds < shortestSeconds:
			shortestSeconds = seconds
			remainingAtShortest = remaining
			found = true
		case seconds == shortestSeconds:
			remainingAtShortest = math.Min(remainingAtShortest, remaining)
		}
	}
	if found {
		return remainingAtShortest, true
	}
	return 0, false
}

func inspectionQuotaWindowExcludedFromCapacity(window model.CodexInspectionQuotaWindow) bool {
	metadata := strings.ToLower(strings.TrimSpace(window.ID + " " + window.LabelKey))
	if strings.Contains(metadata, "code-review") ||
		strings.Contains(metadata, "code_review") {
		return true
	}
	return false
}

func inspectionQuotaWindowDurationSeconds(window model.CodexInspectionQuotaWindow) float64 {
	if window.LimitWindowSeconds != nil && !math.IsNaN(*window.LimitWindowSeconds) && !math.IsInf(*window.LimitWindowSeconds, 0) && *window.LimitWindowSeconds > 0 {
		return *window.LimitWindowSeconds
	}
	metadata := strings.ToLower(strings.TrimSpace(window.ID + " " + window.LabelKey))
	switch {
	case strings.Contains(metadata, "five-hour"),
		strings.Contains(metadata, "five_hour"),
		strings.Contains(metadata, "5-hour"),
		strings.Contains(metadata, "5_hour"):
		return smartQuotaFiveHourSeconds
	case strings.Contains(metadata, "weekly"),
		strings.Contains(metadata, "seven-day"),
		strings.Contains(metadata, "seven_day"),
		strings.Contains(metadata, "7-day"),
		strings.Contains(metadata, "7_day"):
		return smartQuotaWeekSeconds
	case strings.Contains(metadata, "monthly"),
		strings.Contains(metadata, "month"):
		return smartQuotaMonthSeconds
	default:
		// Keep unclassified windows usable for backward compatibility, but only
		// after every duration that is known or can be inferred.
		return math.MaxFloat64
	}
}

func authSnapshotCacheUsable(snapshot authFileSnapshot, now time.Time, ttl time.Duration) bool {
	if !snapshot.generatedAt.IsZero() && now.Sub(snapshot.generatedAt) <= ttl {
		return true
	}
	return !snapshot.attemptedAt.IsZero() && now.Sub(snapshot.attemptedAt) <= ttl
}

func cloneAuthSnapshot(snapshot authFileSnapshot) authFileSnapshot {
	files := make([]cpaauthfiles.File, len(snapshot.files))
	copy(files, snapshot.files)
	snapshot.files = files
	return snapshot
}

func (s *Service) currentSmartResource(cfg store.ManagerSupplyConfig) SmartResource {
	s.stateMu.RLock()
	resource := s.smartResourceState
	s.stateMu.RUnlock()
	if resource.GeneratedAtMS == 0 {
		return defaultSmartResource(cfg)
	}
	previousEffectiveMinutes := resource.EffectiveHealthyMinutes
	previousWarningMinutes := resource.WarningMinutes
	previousCriticalMinutes := resource.CriticalMinutes
	previousUnitCapacity := resource.UnitCapacityRCU
	resource.Enabled = smartSupplyEnabled(cfg)
	resource.ConfiguredHealthyMinutes = smartHealthyMinutesTarget(cfg)
	resource.EffectiveHealthyMinutes = smartEffectiveHealthyMinutesTarget(cfg)
	resource.AccountLifetimeMinutes = smartCapacityPlanningHorizonMinutes(cfg)
	if resource.CapacitySource == smartCapacitySourceInspection {
		resource.CapacitySnapshotAgeSeconds = max(0, int(time.Since(time.UnixMilli(resource.CapacitySnapshotAtMS)).Seconds()))
		if resource.CapacitySnapshotAgeSeconds > 20*60 {
			resource.SnapshotFresh = false
		}
	}
	resource.HealthyMinutesTarget = resource.EffectiveHealthyMinutes
	resource.WarningMinutes = smartWarningMinutes(cfg)
	resource.CriticalMinutes = smartCriticalMinutes(cfg)
	resource.TargetAvailableAccounts = cfg.TargetAvailableAccounts
	resource.Strategy = managerconfigsvc.NormalizeSupplyStrategy(cfg.Strategy)
	resource.CriticalAvailableAccounts = smartCriticalAvailableAccounts(cfg)
	resource.HealthyAvailableAccounts = smartHealthyAvailableAccounts(cfg)
	resource.StartupAvailableAccounts = smartStartupAvailableAccounts(cfg)
	resource.EmergencyMinAccounts = smartEmergencyMinAccounts(cfg)
	resource.VirtualDemandTTLMinutes = smartVirtualDemandTTLMinutes(cfg)
	resource.AccountMaxRequestsBefore401 = smartAccountMaxRequestsBefore401(cfg)
	resource.AccountMaxUsefulSeconds401 = smartAccountMaxUsefulSecondsBefore401(cfg)
	applySmartTokenCapacityDefaults(cfg, &resource)
	configChanged := previousEffectiveMinutes != resource.EffectiveHealthyMinutes ||
		previousWarningMinutes != resource.WarningMinutes ||
		previousCriticalMinutes != resource.CriticalMinutes ||
		previousUnitCapacity != smartProductUnitCapacity(cfg.Product)
	capacityDecision := strings.HasPrefix(resource.DecisionReason, "capacity_") || resource.DecisionReason == "expiry_limited_capacity"
	if resource.Enabled && resource.SnapshotFresh && (configChanged || capacityDecision) {
		recalculateSmartResourceCapacityPlan(cfg, &resource)
	}
	applySmartTokenMetrics(&resource)
	return s.withInspectionSnapshotRefreshState(resource)
}

func (s *Service) setSmartResource(resource SmartResource) {
	s.stateMu.Lock()
	s.smartResourceState = resource
	s.stateMu.Unlock()
}

func smartSupplyEnabled(cfg store.ManagerSupplyConfig) bool {
	return cfg.SmartEnabled == nil || *cfg.SmartEnabled
}

func smartHealthyMinutesTarget(cfg store.ManagerSupplyConfig) int {
	return positiveOr(cfg.HealthyMinutesTarget, 120)
}

func smartAccountLifetimeMinutes() int {
	return 60
}

func smartUsefulAccountLifetimeMinutes() int {
	// Keep a conservative rolling planning horizon for accounts without stronger
	// runtime lifetime evidence. This is a forecast window, not a disable time.
	return 55
}

// smartCapacityPlanningHorizonMinutes is the forecast horizon for credentials
// that do not carry an upstream expiry. It must cover the configured pool
// waterline; otherwise a 120-minute target is silently reduced to the old
// single-account 55-minute estimate. Explicit credential expiry always wins.
func smartCapacityPlanningHorizonMinutes(cfg store.ManagerSupplyConfig) int {
	return max(smartHealthyMinutesTarget(cfg), smartUsefulAccountLifetimeMinutes())
}

func smartEffectiveHealthyMinutesTarget(cfg store.ManagerSupplyConfig) int {
	// The configured waterline describes how many minutes the whole pool should
	// sustain current demand. A single credential's fallback lifetime or
	// observed 401 age must not cap that pool-wide reserve. Credentials with a
	// real upstream expiry are still limited individually by that timestamp.
	return smartHealthyMinutesTarget(cfg)
}

func smartWarningMinutes(cfg store.ManagerSupplyConfig) int {
	target := smartEffectiveHealthyMinutesTarget(cfg)
	value := positiveOr(cfg.WarningMinutes, max(1, target/2))
	if value >= target {
		value = max(1, target/2)
	}
	return value
}

func smartCriticalMinutes(cfg store.ManagerSupplyConfig) int {
	value := positiveOr(cfg.CriticalMinutes, 30)
	if value >= smartWarningMinutes(cfg) {
		value = max(1, smartWarningMinutes(cfg)/2)
	}
	return value
}

func smartPrelockEnabled(cfg store.ManagerSupplyConfig) bool {
	return cfg.PrelockEnabled == nil || *cfg.PrelockEnabled
}

func smartPrelockMinQuantity(cfg store.ManagerSupplyConfig) int {
	return clampInt(positiveOr(cfg.PrelockMinQuantity, 1), 1, 100)
}

func smartPrelockMaxQuantity(cfg store.ManagerSupplyConfig) int {
	maxQuantity := clampInt(positiveOr(cfg.PrelockMaxQuantity, 10), 1, 100)
	if maxQuantity < smartPrelockMinQuantity(cfg) {
		maxQuantity = smartPrelockMinQuantity(cfg)
	}
	return maxQuantity
}

func smartCriticalAvailableAccounts(cfg store.ManagerSupplyConfig) int {
	return clampInt(cfg.CriticalAvailableAccounts, 0, 1000)
}

func smartSupplyStrategyConfigured(cfg store.ManagerSupplyConfig) bool {
	return strings.TrimSpace(cfg.Strategy) != "" ||
		cfg.CriticalAvailableAccounts > 0 || cfg.HealthyAvailableAccounts > 0 ||
		cfg.StartupAvailableAccounts != nil || cfg.DefaultEmergencyMinAccounts > 0 || cfg.VirtualDemandTTLMinutes > 0 ||
		cfg.AccountMaxRequestsBefore401 > 0 || cfg.AccountMaxUsefulSeconds401 > 0 ||
		cfg.EmergencyBypassUsageRate != nil || cfg.RecoveryTriggerOn401 != nil
}

func smartHealthyAvailableAccounts(cfg store.ManagerSupplyConfig) int {
	value := clampInt(cfg.HealthyAvailableAccounts, 0, 10000)
	if value < smartCriticalAvailableAccounts(cfg) {
		return smartCriticalAvailableAccounts(cfg)
	}
	return value
}

func smartStartupAvailableAccounts(cfg store.ManagerSupplyConfig) int {
	if cfg.StartupAvailableAccounts == nil {
		return smartCriticalAvailableAccounts(cfg)
	}
	return clampInt(*cfg.StartupAvailableAccounts, 1, 1000)
}

func smartHealthyFloorShortageEnabled(cfg store.ManagerSupplyConfig) bool {
	return managerconfigsvc.NormalizeSupplyStrategy(cfg.Strategy) == managerconfigsvc.SupplyStrategyCustom &&
		smartHealthyAvailableAccounts(cfg) > smartCriticalAvailableAccounts(cfg)
}

func smartEmergencyMinAccounts(cfg store.ManagerSupplyConfig) int {
	return clampInt(positiveOr(cfg.DefaultEmergencyMinAccounts, 5), 1, 100)
}

// smartEmergencyMinimumOrderQuantity is the lower bound for a genuine pool
// emergency. The old path used the configured emergency minimum (or the
// remaining critical deficit), which produced 1/2/3-account orders even when
// the healthy account waterline was materially higher. A half-waterline batch
// restores useful serving headroom in one guarded order without purchasing the
// whole healthy floor when the pool is empty and traffic is still uncertain.
func smartEmergencyMinimumOrderQuantity(cfg store.ManagerSupplyConfig) int {
	healthy := smartHealthyAvailableAccounts(cfg)
	if healthy <= 0 {
		// Direct unit tests and legacy configurations can omit the new account
		// waterline. Keep the historical emergency minimum as the fallback
		// instead of turning a missing setting into a one-account order.
		healthy = max(2, smartEmergencyMinAccounts(cfg)*2)
	}
	halfHealthy := int(math.Ceil(float64(healthy) / 2))
	return clampInt(max(smartEmergencyMinAccounts(cfg), halfHealthy), 1, 100)
}

func smartEmergencyRefillQuantity(cfg store.ManagerSupplyConfig, available int) int {
	toHealthy := max(0, smartHealthyAvailableAccounts(cfg)-max(0, available))
	minimum := smartEmergencyMinAccounts(cfg)
	if max(0, available) <= smartCriticalAvailableAccounts(cfg) {
		minimum = smartEmergencyMinimumOrderQuantity(cfg)
	}
	return clampInt(max(minimum, toHealthy), 1, 100)
}

// smartIdleEmergencyRefillQuantity keeps the configured startup account floor
// independent of traffic, so a completely exhausted pool continues supplier
// retries until enough schedulable credentials exist to restart requests.
func smartIdleEmergencyRefillQuantity(cfg store.ManagerSupplyConfig, available int) int {
	if cfg.StartupAvailableAccounts == nil {
		if available <= 0 || available <= smartCriticalAvailableAccounts(cfg) {
			return smartEmergencyMinimumOrderQuantity(cfg)
		}
		return 0
	}
	return clampInt(max(0, smartStartupAvailableAccounts(cfg)-max(0, available)), 0, 100)
}

func smartVirtualDemandTTLMinutes(cfg store.ManagerSupplyConfig) int {
	return clampInt(positiveOr(cfg.VirtualDemandTTLMinutes, 60), 1, 180)
}

func smartAccountMaxRequestsBefore401(cfg store.ManagerSupplyConfig) int {
	if cfg.AccountMaxRequestsBefore401 <= 0 && !smartSupplyStrategyConfigured(cfg) {
		return 0
	}
	return clampInt(positiveOr(cfg.AccountMaxRequestsBefore401, 30), 1, 100000)
}

func smartAccountMaxUsefulSecondsBefore401(cfg store.ManagerSupplyConfig) int {
	if cfg.AccountMaxUsefulSeconds401 <= 0 && !smartSupplyStrategyConfigured(cfg) {
		return 0
	}
	return clampInt(positiveOr(cfg.AccountMaxUsefulSeconds401, 120), 1, 3600)
}

func smartEmergencyBypassUsageRate(cfg store.ManagerSupplyConfig) bool {
	return cfg.EmergencyBypassUsageRate == nil || *cfg.EmergencyBypassUsageRate
}

func smartRecoveryTriggerOn401(cfg store.ManagerSupplyConfig) bool {
	return cfg.RecoveryTriggerOn401 == nil || *cfg.RecoveryTriggerOn401
}

// smartResourceAtOrBelowWarning distinguishes a true warning-water event from
// the ordinary "below healthy target" prelock state. Only the former may use
// the larger ReplenishBatchSize cap and the shorter retry cooldown.
func smartResourceAtOrBelowWarning(resource SmartResource) bool {
	return resource.ConsumeRCUPerMinute > 0 && resource.WarningMinutes > 0 &&
		resource.EstimatedSustainMinutes <= float64(resource.WarningMinutes)
}

// smartExtendedWaterlineProgressiveMode identifies configurations whose
// warning/critical lines extend beyond the old no-expiry forecast horizon.
// Those configured lines remain the exact runtime health thresholds, but the
// newly exposed reserve must be accumulated progressively rather than turning
// one rollout/config refresh into an emergency burst.
func smartExtendedWaterlineProgressiveMode(resource SmartResource) bool {
	useful := smartUsefulAccountLifetimeMinutes()
	return resource.EffectiveHealthyMinutes > useful &&
		resource.WarningMinutes > useful &&
		resource.CriticalMinutes > max(1, useful/2)
}

// smartProgressiveStartupFloorRecovery distinguishes a warm pool rebuilding its
// configured startup account floor from a real account-vacuum rescue. A fresh
// completed capacity snapshot is the durable baseline; newly imported accounts
// are already overlaid locally on that baseline. When several usable accounts
// remain, the startup floor therefore needs only one incremental credential and
// one observation cycle, not a concurrent batch created from an instantaneous
// zero-traffic sample.
func smartProgressiveStartupFloorRecovery(resource SmartResource) bool {
	if resource.EmergencyReason != "startup_account_floor" && resource.DecisionReason != "startup_account_floor" {
		return false
	}
	rescueFloor := max(2, resource.CriticalAvailableAccounts)
	if resource.AvailableAccounts <= rescueFloor {
		return false
	}
	runway := math.Max(resource.AvailableSustainMinutes, resource.EstimatedSustainMinutes)
	if runway <= 0 {
		demand := math.Max(resource.ConsumeRCUPerMinute, resource.DemandPlanningRCUPerMinute)
		demand = math.Max(demand, math.Max(resource.DemandMemoryRCUPerMinute, resource.VirtualDemandRCUPerMinute))
		if demand > 0 {
			runway = smartAvailableCapacity(resource) / demand
		}
	}
	shortRescueMinutes := float64(max(1, smartUsefulAccountLifetimeMinutes()/4))
	if runway > 0 {
		return runway > shortRescueMinutes
	}
	// Missing demand telemetry is not evidence of a short rescue. With several
	// verified accounts still serving, default to the one-account path and let
	// the next local usage/capacity update decide whether the pool has crossed
	// the explicit short-runway boundary.
	return true
}

// applyHistoricalCapacityStartupObservation prevents one transient empty usage
// bucket from overriding the immediately preceding verified capacity decision.
// The full inspection snapshot remains the durable capacity history; this
// process-local record only supplies its recent demand denominator. Imported
// credentials have already been overlaid on resource, so the calculation is a
// local delta and does not wait for another full-pool inspection.
func applyHistoricalCapacityStartupObservation(cfg store.ManagerSupplyConfig, resource *SmartResource, previous SmartResource, now time.Time) bool {
	if resource == nil || !smartProgressiveStartupFloorRecovery(*resource) ||
		!resource.SnapshotFresh || resource.CurrentCapacityRCU <= 0 ||
		!previous.SnapshotFresh || previous.GeneratedAtMS <= 0 {
		return false
	}
	maxAge := max(120, positiveOr(cfg.CheckIntervalSeconds, 60)*3)
	if age := int(now.Sub(time.UnixMilli(previous.GeneratedAtMS)).Seconds()); age < 0 || age > maxAge {
		return false
	}
	demand := math.Max(previous.ConsumeRCUPerMinute, previous.DemandPlanningRCUPerMinute)
	demand = math.Max(demand, math.Max(previous.DemandMemoryRCUPerMinute, previous.VirtualDemandRCUPerMinute))
	if demand <= 0 {
		return false
	}
	availableRunway := smartAvailableCapacity(*resource) / demand
	if availableRunway <= float64(max(1, smartUsefulAccountLifetimeMinutes()/4)) {
		return false
	}
	resource.DemandPlanningRCUPerMinute = round2(math.Max(resource.DemandPlanningRCUPerMinute, demand))
	resource.DemandMemoryRCUPerMinute = round2(math.Max(resource.DemandMemoryRCUPerMinute, previous.DemandMemoryRCUPerMinute))
	resource.DemandMemoryLastSeenMS = max(resource.DemandMemoryLastSeenMS, previous.DemandMemoryLastSeenMS)
	if resource.DemandMemoryLastSeenMS > 0 {
		resource.DemandMemoryAgeSeconds = max(0, int(now.Sub(time.UnixMilli(resource.DemandMemoryLastSeenMS)).Seconds()))
	}
	resource.VirtualDemandRCUPerMinute = round2(math.Max(resource.VirtualDemandRCUPerMinute, previous.VirtualDemandRCUPerMinute))
	resource.EmergencyShortage = false
	resource.EmergencyReason = ""
	resource.PoolVacuumActive = false
	resource.PoolVacuumStartedAtMS = 0
	resource.PoolVacuumDurationSeconds = 0
	resource.TargetCapacityRCU = round2(demand * float64(resource.EffectiveHealthyMinutes))
	resource.RecommendedCapacityRCU = resource.TargetCapacityRCU
	resource.EstimatedSustainMinutes = round1(resource.CurrentCapacityRCU / demand)
	resource.AvailableSustainMinutes = round1(availableRunway)
	resource.CapacityGapRCU = round2(math.Max(0, resource.TargetCapacityRCU-resource.CurrentCapacityRCU-resource.PrelockedCapacityRCU))
	resource.HealthLevel = smartHealthHealthy
	resource.SuggestedAction = smartActionObserveDemand
	resource.SuggestedQuantity = 0
	resource.DecisionReason = "historical_capacity_observe"
	applySmartRefillProjection(cfg, resource)
	applySmartTokenMetrics(resource)
	return true
}

// smartEmergencyShortage is narrower than merely being below the healthy
// target. New credentials are short-lived, so a normal target deficit may
// observe a falling one-minute sample. Extended waterlines keep their exact
// configured health classification, while only a much shorter rescue runway
// bypasses progressive order pacing.
func smartEmergencyShortage(resource SmartResource) bool {
	if resource.ConsumeRCUPerMinute <= 0 || resource.CapacityGapRCU <= 0 ||
		resource.EffectiveHealthyMinutes <= 0 || resource.EstimatedSustainMinutes < 0 {
		return false
	}
	threshold := max(resource.CriticalMinutes, max(1, resource.EffectiveHealthyMinutes/2))
	if smartExtendedWaterlineProgressiveMode(resource) {
		threshold = min(threshold, max(1, smartUsefulAccountLifetimeMinutes()/4))
	}
	return resource.EstimatedSustainMinutes <= float64(threshold)
}

func smartResourceEmergency(resource SmartResource) bool {
	return resource.EmergencyShortage || smartEmergencyShortage(resource) || resource.SuggestedAction == smartActionEmergencyReplenish
}

// ordinaryAccountTargetReached keeps the regular capacity planner from buying
// another full-price credential for a small healthy-waterline drift after the
// configured account pool has already been filled. True warning/emergency
// shortages still bypass this gate. Low-price reserve purchasing is evaluated
// by its dedicated worker and does not use this decision.
func ordinaryAccountTargetReached(cfg store.ManagerSupplyConfig, resource SmartResource, available int) bool {
	return cfg.TargetAvailableAccounts > 0 &&
		available >= cfg.TargetAvailableAccounts &&
		resource.SnapshotFresh &&
		!smartResourceEmergency(resource)
}

func applyOrdinaryAccountTargetGate(cfg store.ManagerSupplyConfig, resource *SmartResource, available int) bool {
	if resource == nil || !ordinaryAccountTargetReached(cfg, *resource, available) {
		return false
	}
	resource.AvailableAccounts = max(resource.AvailableAccounts, available)
	resource.EmergencyShortage = false
	resource.EmergencyReason = ""
	resource.SuggestedAction = smartActionObserveDemand
	resource.SuggestedQuantity = 0
	resource.DecisionReason = "account_target_reached_reserve_only"
	applySmartRefillProjection(cfg, resource)
	return true
}

// smartPartialInspectionCapacityDeficitAllowed permits a bounded purchase
// from the verified capacity lower bound when usability/quota evidence is
// incomplete. It deliberately excludes missing snapshots and old evidence;
// the 45-minute upper bound accommodates the regular full-pool inspection
// duration while avoiding decisions from a stale historic snapshot.
func smartPartialInspectionCapacityDeficitAllowed(resource SmartResource) bool {
	if resource.CurrentCapacityRCU <= 0 || resource.CapacityGapRCU <= 0 ||
		resource.CapacitySnapshotAtMS <= 0 || resource.CapacitySnapshotAgeSeconds > 45*60 {
		return false
	}
	baseReason := strings.TrimSuffix(resource.DecisionReason, "_capacity_deficit")
	switch baseReason {
	case "inspection_quota_incomplete", "inspection_usability_incomplete":
		return true
	default:
		return false
	}
}

// smartStaleVerifiedLowWaterReadyTakeAllowed is intentionally narrower than
// normal automation: it never creates another order from stale data. It only
// permits taking an already-reserved ready order when the last completed
// inspection has complete quota/usability coverage and its verified lower
// bound remains at or below warning water. This prevents a long inspection of
// a large pool from stranding needed capacity after the normal freshness
// window, without acting on an incomplete inspection.
func smartStaleVerifiedLowWaterReadyTakeAllowed(resource SmartResource) bool {
	if resource.SnapshotFresh || !smartResourceAtOrBelowWarning(resource) ||
		resource.CapacitySource != smartCapacitySourceInspection || resource.CapacitySnapshotRunID <= 0 ||
		resource.CapacityCoverage < 100 || resource.CurrentCapacityRCU < 0 ||
		resource.ConsumeRCUPerMinute <= 0 || resource.CapacitySnapshotAtMS <= 0 ||
		resource.CapacitySnapshotAgeSeconds > 90*60 {
		return false
	}
	return resource.Confidence == smartConfidenceMedium || resource.Confidence == smartConfidenceHigh
}

func smartReplenishBatchLimit(cfg store.ManagerSupplyConfig) int {
	return clampInt(positiveOr(cfg.ReplenishBatchSize, smartPrelockMaxQuantity(cfg)), 1, 100)
}

// smartAutomaticOrderQuantityLimit lets a verified capacity deficit recover
// toward the healthy waterline in one guarded order. Only incomplete
// inspection evidence remains staged at three credentials or fewer; a fresh
// stable snapshot must not force a real five-account shortage back to 1/2/3.
func smartAutomaticOrderQuantityLimit(cfg store.ManagerSupplyConfig, resource SmartResource) int {
	limit := smartReplenishBatchLimit(cfg)
	if smartPrelockEnabled(cfg) {
		limit = min(limit, smartPrelockMaxQuantity(cfg))
	}
	if smartResourceEmergency(resource) {
		// A severe account-pool shortage must not be reduced back to a 1/2/3
		// order merely because an old prelock cap was smaller. Normal orders and
		// manual orders retain their configured limits; this expansion applies
		// only to the automatic emergency path and is still bounded by 100.
		limit = max(limit, smartEmergencyMinimumOrderQuantity(cfg))
		return max(1, limit)
	}
	if smartPartialInspectionCapacityDeficitAllowed(resource) {
		limit = min(limit, 3)
	}
	return max(1, limit)
}

func smartAccountAvailabilityEmergencyReason(reason string) bool {
	switch reason {
	case "available_capacity_critical", "critical_available_accounts", "emergency_pool_vacuum", "startup_account_floor":
		return true
	default:
		return false
	}
}

func smartAccountAvailabilityEmergency(resource SmartResource) bool {
	if resource.PoolVacuumActive {
		return true
	}
	return smartAccountAvailabilityEmergencyReason(resource.EmergencyReason)
}

func smartRisingObservationQuantity(cfg store.ManagerSupplyConfig, resource SmartResource) int {
	limit := smartAutomaticOrderQuantityLimit(cfg, resource)
	if limit <= 0 {
		return 1
	}
	// A 1-minute spike is real enough to lower the capacity health immediately,
	// but it is not enough evidence to buy a full one-hour batch. Cap the first
	// reservation to 1/2/3 credentials according to the currently observed
	// shortfall, then let the next complete minute and 5/10-minute windows
	// decide whether another batch is justified.
	unit := smartEstimatedNewAccountCapacityForResource(cfg, resource)
	if unit <= 0 {
		unit = smartEstimatedAccountCapacityRCU(resource.UnitCapacityRCU, float64(smartCapacityPlanningHorizonMinutes(cfg)))
	}
	needed := 1
	if unit > 0 && resource.CapacityGapRCU > 0 {
		needed = int(math.Ceil(resource.CapacityGapRCU / unit))
	}
	return clampInt(needed, 1, min(limit, 3))
}

func smartCriticalTakeConfirmRounds(cfg store.ManagerSupplyConfig) int {
	return clampInt(positiveOr(cfg.CriticalTakeConfirmRounds, 2), 1, 5)
}

func smartCreateCooldownSeconds(cfg store.ManagerSupplyConfig) int {
	return clampInt(positiveOr(cfg.CreateCooldownSeconds, 120), 0, 3600)
}

const (
	// Successful progressive purchases use a strategy-specific observation
	// window. These windows apply only while the pool still has runway; a true
	// emergency keeps the existing short recovery cadence.
	smartStrongSupplyProgressiveCooldownSeconds = 5 * 60
	smartStrongSupplyWarningCooldownSeconds     = 2 * 60
	smartBalancedProgressiveCooldownSeconds     = 10 * 60
	smartBalancedWarningCooldownSeconds         = 3 * 60
	smartCostFirstProgressiveCooldownSeconds    = 15 * 60
	smartCostFirstWarningCooldownSeconds        = 5 * 60
)

// smartSuccessfulOrderCooldownForResource keeps the procurement cadence aligned
// with the selected strategy after an order has delivered usable accounts.
// Healthy/buffered pools observe longer so lease expiries spread out. Warning
// pools shorten the observation, while emergency pools retain the existing
// fast cadence.
func smartSuccessfulOrderCooldownForResource(cfg store.ManagerSupplyConfig, resource SmartResource) int {
	if smartProgressiveStartupFloorRecovery(resource) {
		return max(smartCreateCooldownSeconds(cfg), 120)
	}
	if smartResourceEmergency(resource) {
		return smartCreateCooldownForResource(cfg, resource)
	}
	base := smartCreateCooldownSeconds(cfg)
	normalMinimum := 0
	warningMinimum := 0
	switch managerconfigsvc.NormalizeSupplyStrategy(cfg.Strategy) {
	case managerconfigsvc.SupplyStrategyBalanced:
		normalMinimum = smartBalancedProgressiveCooldownSeconds
		warningMinimum = smartBalancedWarningCooldownSeconds
	case managerconfigsvc.SupplyStrategyCostFirst:
		normalMinimum = smartCostFirstProgressiveCooldownSeconds
		warningMinimum = smartCostFirstWarningCooldownSeconds
	case managerconfigsvc.SupplyStrategyCustom:
		return smartCreateCooldownForResource(cfg, resource)
	default:
		normalMinimum = smartStrongSupplyProgressiveCooldownSeconds
		warningMinimum = smartStrongSupplyWarningCooldownSeconds
	}
	if smartResourceAtOrBelowWarning(resource) {
		return max(smartCreateCooldownForResource(cfg, resource), warningMinimum)
	}
	return max(base, normalMinimum)
}

// smartSuccessfulOrderCooldownForDelivery observes a larger successful batch
// for longer when the pool still has runway. Half of the delivered capacity's
// estimated consumption time is enough to reveal the real burn rate while the
// critical-waterline budget prevents the observation from delaying recovery.
func smartSuccessfulOrderCooldownForDelivery(cfg store.ManagerSupplyConfig, resource SmartResource, delivered int) int {
	base := smartSuccessfulOrderCooldownForResource(cfg, resource)
	if delivered <= 0 {
		return base
	}
	if smartResourceEmergency(resource) {
		// A delivered small rung still gets one configured emergency observation
		// cycle, but it must not silently turn a 30-second shortage cadence into a
		// fixed 90-second pause. The next capacity snapshot decides whether the
		// segmented ladder should continue.
		return base
	}
	demand := math.Max(resource.ConsumeRCUPerMinute, resource.DemandPlanningRCUPerMinute)
	unit := smartEstimatedNewAccountCapacityForResource(cfg, resource)
	if demand <= 0 || unit <= 0 {
		return base
	}
	deliveredRunwayMinutes := float64(delivered) * unit / demand
	criticalBudgetMinutes := math.Max(0, resource.EstimatedSustainMinutes-float64(max(1, resource.CriticalMinutes)))
	observationMinutes := math.Min(deliveredRunwayMinutes/2, criticalBudgetMinutes/2)
	if observationMinutes <= 0 {
		return base
	}
	return max(base, int(math.Ceil(observationMinutes*60)))
}

// smartCreateCooldownForResource shortens recovery retries without ever
// removing the safety delay. The delay is also checked against the latest
// persisted automatic order, so a process restart cannot reset it.
func smartCreateCooldownForResource(cfg store.ManagerSupplyConfig, resource SmartResource) int {
	cooldown := smartCreateCooldownSeconds(cfg)
	if smartProgressiveStartupFloorRecovery(resource) {
		return max(cooldown, 120)
	}
	if smartResourceEmergency(resource) {
		checkInterval := positiveOr(cfg.CheckIntervalSeconds, 60)
		// During an emergency the automatic check cadence is also the maximum
		// create cooldown. A configured 30-second loop therefore remains a real
		// 30-second抢号 cadence instead of being stretched to a hidden minute.
		return clampInt(min(cooldown, min(checkInterval, 60)), 1, 60)
	}
	if !smartResourceAtOrBelowWarning(resource) {
		return cooldown
	}
	if resource.HealthLevel == smartHealthCritical {
		return min(cooldown, 15)
	}
	return min(cooldown, 45)
}

func smartReleaseCooldownSeconds(cfg store.ManagerSupplyConfig) int {
	return clampInt(positiveOr(cfg.ReleaseCooldownSeconds, 60), 0, 3600)
}

func smartAuthFilesCacheTTLSeconds(cfg store.ManagerSupplyConfig) int {
	return clampInt(positiveOr(cfg.AuthFilesCacheTTLSeconds, 60), 10, 600)
}

func smartMinHoldSeconds(cfg store.ManagerSupplyConfig) int {
	return clampInt(positiveOr(cfg.MinHoldSeconds, 30), 0, 3600)
}

func smartNewAccountConfidence(cfg store.ManagerSupplyConfig) float64 {
	if cfg.NewAccountConfidence <= 0 {
		return 0.7
	}
	if cfg.NewAccountConfidence > 1 {
		return 1
	}
	return cfg.NewAccountConfidence
}

func smartProductUnitCapacity(product string) float64 {
	switch strings.ToLower(strings.TrimSpace(product)) {
	case "team_1h":
		return 60
	case "oauth_7d":
		return 40
	case "oauth_30d":
		return 80
	default:
		return 50
	}
}

func smartEstimatedAccountCapacityRCU(unitPerMinute float64, remainingMinutes float64) float64 {
	if unitPerMinute <= 0 {
		unitPerMinute = 1
	}
	remainingMinutes = math.Max(0, remainingMinutes)
	return unitPerMinute * remainingMinutes
}

// smartTokenMillionToRCU converts an absolute token budget into the existing
// request-capacity unit. One RCU represents unit*1000 effective tokens, which
// is also the conversion used by smartTokenDemandRCU. Keeping both sides on
// the same scale removes the previous unit^2 runway inflation.
func smartTokenMillionToRCU(tokenM, unit float64) float64 {
	if tokenM <= 0 {
		return 0
	}
	if unit <= 0 {
		unit = 1
	}
	return tokenM * 1000 / unit
}

func smartRCUToTokenMillion(rcu, unit float64) float64 {
	if rcu <= 0 {
		return 0
	}
	if unit <= 0 {
		unit = 1
	}
	return rcu * unit / 1000
}

func smartAccountQuotaCapacityRCU(unit, accountQuotaM, remainingFraction float64) float64 {
	remainingFraction = clampFloat(remainingFraction, 0, 1)
	if accountQuotaM <= 0 {
		accountQuotaM = smartDefaultAccountQuotaMillionTokens
	}
	return smartTokenMillionToRCU(accountQuotaM*remainingFraction, unit)
}

func smartEstimatedNewAccountCapacityRCU(cfg store.ManagerSupplyConfig) float64 {
	unit := smartProductUnitCapacity(cfg.Product)
	capacity := smartEstimatedAccountCapacityRCU(unit, float64(smartCapacityPlanningHorizonMinutes(cfg)))
	confidence := smartNewAccountConfidence(cfg)
	if confidence <= 0 {
		confidence = 1
	}
	return capacity * confidence
}

func smartEstimatedNewAccountTokenCapacityRCU(cfg store.ManagerSupplyConfig, accountQuotaM float64) float64 {
	unit := smartProductUnitCapacity(cfg.Product)
	if accountQuotaM <= 0 {
		accountQuotaM = smartDefaultAccountQuotaMillionTokens
	}
	capacity := smartTokenMillionToRCU(accountQuotaM, unit)
	confidence := smartNewAccountConfidence(cfg)
	if confidence <= 0 {
		confidence = 1
	}
	return capacity * confidence
}

func applySmartTokenCapacityDefaults(cfg store.ManagerSupplyConfig, resource *SmartResource) {
	if resource == nil {
		return
	}
	resource.TokenCapacityMode = smartTokenCapacityMode
	if resource.AccountQuotaEstimateM <= 0 {
		resource.AccountQuotaEstimateM = smartDefaultAccountQuotaMillionTokens
		resource.AccountQuotaEstimateSource = smartQuotaEstimateSourceDefault
		resource.AccountQuotaCalibrationConfidence = smartConfidenceLow
	}
	resource.EstimatedNewAccountCapacityRCU = round2(smartEstimatedNewAccountTokenCapacityRCU(cfg, resource.AccountQuotaEstimateM))
	resource.RiskAdjustedUnitCapacityRCU = resource.EstimatedNewAccountCapacityRCU
}

func smartEstimatedNewAccountCapacityForResource(cfg store.ManagerSupplyConfig, resource SmartResource) float64 {
	if resource.TokenCapacityMode == smartTokenCapacityMode && resource.EstimatedNewAccountCapacityRCU > 0 {
		return resource.EstimatedNewAccountCapacityRCU
	}
	return smartEstimatedNewAccountCapacityRCU(cfg)
}

func smartConsumeRCUPerMinute(rpm30 float64, rpm5Peak float64, tpm30 float64, unit float64) float64 {
	requestRate := math.Max(rpm30, rpm5Peak*0.7)
	// RCU demand uses the larger of request pressure and token pressure. Both
	// components are published so a low RPM cannot hide token-driven demand.
	return math.Max(requestRate, smartTokenDemandRCU(tpm30, unit))
}

func smartAccountCapacityHardBlocked(values map[string]any) bool {
	status := strings.ToLower(textField(values, "status", "state", "runtime_status", "runtimeStatus"))
	message := strings.ToLower(textField(values, "status_message", "statusMessage", "error_kind", "errorKind", "header_error_kind", "headerErrorKind", "last_error", "lastError"))
	combined := strings.TrimSpace(status + " " + message)
	if combined == "" {
		return false
	}
	switch {
	case strings.Contains(combined, "usage_limit_reached"),
		strings.Contains(combined, "quota_exhausted"),
		strings.Contains(combined, "insufficient_quota"),
		strings.Contains(combined, "billing_hard_limit"),
		strings.Contains(combined, "hard_limit_reached"),
		strings.Contains(combined, "credit_grant_exhausted"),
		strings.Contains(combined, "exceeded your current quota"):
		return true
	case strings.Contains(combined, "credential invalidated"),
		strings.Contains(combined, "token_invalidated"),
		strings.Contains(combined, "invalid_grant"),
		strings.Contains(combined, "invalid token"),
		strings.Contains(combined, "invalid_token"),
		strings.Contains(combined, "login_required"),
		strings.Contains(combined, "reauth"),
		strings.Contains(combined, "unauthorized"),
		strings.Contains(combined, "forbidden"),
		strings.Contains(combined, "revoked"),
		strings.Contains(combined, "expired"),
		strings.Contains(combined, " 401 "):
		return true
	default:
		return false
	}
}

func smartAccountNeedsAttention(values map[string]any) bool {
	if smartAccountCapacityHardBlocked(values) {
		return true
	}
	message := strings.ToLower(textField(
		values,
		"status_message", "statusMessage",
		"error_kind", "errorKind",
		"header_error_kind", "headerErrorKind",
		"last_error", "lastError",
	))
	if message == "" || smartAccountHealthyStatusMessage(message) {
		return false
	}
	return !smartAccountRuntimeCooling(values, message)
}

func smartAccountHealthyStatusMessage(message string) bool {
	message = strings.TrimSpace(strings.ToLower(message))
	switch message {
	case "ok", "healthy", "ready", "success", "available", "active":
		return true
	default:
		return false
	}
}

func smartAccountRuntimeCooling(values map[string]any, message string) bool {
	combined := strings.ToLower(strings.TrimSpace(strings.Join([]string{
		textField(values, "status", "state", "runtime_status", "runtimeStatus"),
		message,
		textField(values, "status_code", "statusCode", "http_status", "httpStatus", "last_status_code", "lastStatusCode"),
	}, " ")))
	for _, marker := range []string{
		"rate limit exceeded",
		"rate_limit",
		"rate-limit",
		"too many requests",
		"http 429",
		"status 429",
		"status_code:429",
		"status_code\":429",
		"cooldown",
		"cooling",
		"retry_after",
		"retry after",
		"quota cooldown",
		"quota window",
		"waiting for quota",
		"quota reset",
	} {
		if strings.Contains(combined, marker) {
			return true
		}
	}
	for _, marker := range []string{
		"temporarily unavailable",
		"temporary unavailable",
		"service unavailable",
		"server overloaded",
		"upstream unavailable",
	} {
		if strings.Contains(combined, marker) {
			return smartAccountHasRecentSuccess(values)
		}
	}
	return false
}

func smartAccountHasRecentSuccess(values map[string]any) bool {
	raw := values["recent_requests"]
	if raw == nil {
		raw = values["recentRequests"]
	}
	switch buckets := raw.(type) {
	case []any:
		for _, bucket := range buckets {
			if item, ok := bucket.(map[string]any); ok && numberField(item, "success", "successful") > 0 {
				return true
			}
		}
	case []map[string]any:
		for _, bucket := range buckets {
			if numberField(bucket, "success", "successful") > 0 {
				return true
			}
		}
	}
	return false
}

func smartAccountCapacityRCU(values map[string]any, unit float64, accountQuotaM float64) (float64, bool) {
	if unit <= 0 {
		unit = 1
	}
	if smartAccountCapacityHardBlocked(values) {
		return 0, true
	}
	if capacity := numberField(
		values,
		"remaining_rcu", "remainingRcu", "quota_remaining_rcu", "quotaRemainingRcu",
		"available_rcu", "availableRcu", "quota_balance_rcu", "quotaBalanceRcu",
	); capacity > 0 {
		return capacity, true
	}
	if capacity := numberField(
		values,
		"quota_remaining", "quotaRemaining", "remaining_quota", "remainingQuota",
		"available_quota", "availableQuota", "quota_balance", "quotaBalance",
		"remaining_budget", "remainingBudget",
	); capacity > 0 {
		return capacity, true
	}
	if tokens := numberField(
		values,
		"remaining_tokens", "remainingTokens", "quota_remaining_tokens", "quotaRemainingTokens",
		"available_tokens", "availableTokens",
	); tokens > 0 {
		return tokens / 1000 / unit, true
	}
	if usedPercent := numberField(values, "header_quota_used_percent", "quota_used_percent", "quotaUsedPercent", "used_percent", "usage_percent"); usedPercent > 0 {
		if usedPercent > 1 {
			usedPercent = usedPercent / 100
		}
		remaining := 1 - usedPercent
		if remaining < 0 {
			remaining = 0
		}
		return smartAccountQuotaCapacityRCU(unit, accountQuotaM, remaining), true
	}
	return 0, false
}

func smartExpiryLimitedCapacity(items []smartCapacityItem, consumeRCUPerMinute float64) (float64, float64) {
	if consumeRCUPerMinute <= 0 || len(items) == 0 {
		total := 0.0
		for _, item := range items {
			total += math.Max(0, item.capacityRCU)
		}
		return total, 0
	}
	ordered := make([]smartCapacityItem, 0, len(items))
	for _, item := range items {
		if item.capacityRCU <= 0 {
			continue
		}
		item.remainingMinutes = math.Max(0, item.remainingMinutes)
		ordered = append(ordered, item)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].remainingMinutes < ordered[j].remainingMinutes
	})
	usable := 0.0
	wasteRisk := 0.0
	for _, item := range ordered {
		maxConsumableBeforeExpiry := consumeRCUPerMinute * item.remainingMinutes
		remainingDemandBeforeExpiry := maxConsumableBeforeExpiry - usable
		if remainingDemandBeforeExpiry <= 0 {
			wasteRisk += item.capacityRCU
			continue
		}
		use := math.Min(item.capacityRCU, remainingDemandBeforeExpiry)
		usable += use
		wasteRisk += math.Max(0, item.capacityRCU-use)
	}
	return usable, wasteRisk
}

// applySmartExpiryCapacity turns per-account quota and actual runtime lifetime
// evidence into one capacity timeline. Supplier lease timestamps are retained
// only for nearest-expiry observability and routing priority; they do not set
// remainingMinutes or cap a still-healthy credential's usable capacity.
func applySmartExpiryCapacity(resource *SmartResource, items []smartCapacityItem, consumeRCUPerMinute float64, now time.Time) {
	if resource == nil {
		return
	}
	for index := range items {
		items[index].usableCapacityRCU = math.Max(0, items[index].capacityRCU)
	}
	resource.CurrentCapacityRCU = resource.RawCapacityRCU
	resource.TimeLimitedCapacityRCU = resource.RawCapacityRCU
	resource.AvailableCapacityRCU = resource.RawCapacityRCU
	resource.FrozenCapacityRCU = 0
	resource.TotalCapacityRCU = resource.RawCapacityRCU
	resource.ExpiryWasteRiskRCU = 0
	resource.NearestExpiryAtMS = 0
	resource.NearestExpiryMinutes = 0
	for _, item := range items {
		if item.expiresAtMS <= now.UnixMilli() {
			continue
		}
		if resource.NearestExpiryAtMS == 0 || item.expiresAtMS < resource.NearestExpiryAtMS {
			resource.NearestExpiryAtMS = item.expiresAtMS
		}
	}
	if resource.NearestExpiryAtMS > 0 {
		resource.NearestExpiryMinutes = round1(math.Max(0, time.UnixMilli(resource.NearestExpiryAtMS).Sub(now).Minutes()))
	}
	if consumeRCUPerMinute <= 0 {
		return
	}
	ordered := make([]int, 0, len(items))
	for index, item := range items {
		if item.capacityRCU <= 0 || item.remainingMinutes <= 0 {
			items[index].usableCapacityRCU = 0
			continue
		}
		items[index].remainingMinutes = math.Max(0, item.remainingMinutes)
		ordered = append(ordered, index)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return items[ordered[i]].remainingMinutes < items[ordered[j]].remainingMinutes
	})
	usableCapacity := 0.0
	wasteRisk := 0.0
	for start := 0; start < len(ordered); {
		end := start + 1
		remainingMinutes := items[ordered[start]].remainingMinutes
		groupCapacity := items[ordered[start]].capacityRCU
		for end < len(ordered) && math.Abs(items[ordered[end]].remainingMinutes-remainingMinutes) < 1e-9 {
			groupCapacity += items[ordered[end]].capacityRCU
			end++
		}
		maxConsumableBeforeExpiry := consumeRCUPerMinute * remainingMinutes
		remainingDemandBeforeExpiry := maxConsumableBeforeExpiry - usableCapacity
		if remainingDemandBeforeExpiry <= 0 {
			for _, index := range ordered[start:end] {
				items[index].usableCapacityRCU = 0
				wasteRisk += items[index].capacityRCU
			}
			start = end
			continue
		}
		groupUsable := math.Min(groupCapacity, remainingDemandBeforeExpiry)
		usableFraction := 0.0
		if groupCapacity > 0 {
			usableFraction = groupUsable / groupCapacity
		}
		for _, index := range ordered[start:end] {
			item := &items[index]
			item.usableCapacityRCU = item.capacityRCU * usableFraction
			wasteRisk += math.Max(0, item.capacityRCU-item.usableCapacityRCU)
		}
		usableCapacity += groupUsable
		start = end
	}
	resource.TimeLimitedCapacityRCU = round2(usableCapacity)
	resource.ExpiryWasteRiskRCU = round2(wasteRisk)
	resource.CurrentCapacityRCU = resource.TimeLimitedCapacityRCU
	resource.AvailableCapacityRCU = resource.CurrentCapacityRCU
	resource.TotalCapacityRCU = resource.CurrentCapacityRCU
}

func smartAccountRemainingMinutes(values map[string]any, now time.Time, fallbackMinutes int) float64 {
	if fallbackMinutes <= 0 {
		fallbackMinutes = smartAccountLifetimeMinutes()
	}
	if seconds, ok := numberFieldOK(values,
		"remaining_seconds", "remainingSeconds", "remaining_valid_seconds", "remainingValidSeconds",
		"minimum_remaining_seconds", "minimumRemainingSeconds", "ttl_seconds", "ttlSeconds",
	); ok {
		return math.Max(0, seconds/60)
	}
	if minutes, ok := numberFieldOK(values, "remaining_minutes", "remainingMinutes", "ttl_minutes", "ttlMinutes"); ok {
		return math.Max(0, minutes)
	}
	if seconds, ok := numberFieldOK(values, "expires_in", "expiresIn", "expire_in", "expireIn"); ok {
		return math.Max(0, seconds/60)
	}
	for _, key := range []string{"expired", "expires_at", "expiresAt", "expire_at", "expireAt", "valid_until", "validUntil"} {
		raw, ok := values[key]
		if !ok || raw == nil {
			continue
		}
		if expiry, ok := parseSmartExpiryTime(raw, now); ok {
			return math.Max(0, expiry.Sub(now).Minutes())
		}
	}
	return float64(fallbackMinutes)
}

func smartSupplyLeaseExpiry(values map[string]any, now time.Time) (time.Time, bool) {
	for _, key := range []string{"supply_lease_expires_at_ms", "supplyLeaseExpiresAtMs", "supply_lease_expires_at", "supplyLeaseExpiresAt"} {
		if raw, ok := values[key]; ok && raw != nil {
			if expiry, parsed := parseSmartExpiryTime(raw, now); parsed {
				return expiry, true
			}
		}
	}
	return time.Time{}, false
}

func parseSmartExpiryTime(value any, now time.Time) (time.Time, bool) {
	switch typed := value.(type) {
	case int:
		return unixLikeToTime(float64(typed), now)
	case int64:
		return unixLikeToTime(float64(typed), now)
	case float64:
		return unixLikeToTime(typed, now)
	case jsonNumber:
		parsed, err := typed.Float64()
		if err != nil {
			return time.Time{}, false
		}
		return unixLikeToTime(parsed, now)
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return time.Time{}, false
		}
		if parsed, ok := parseFloat(text); ok {
			return unixLikeToTime(parsed, now)
		}
		for _, layout := range []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02 15:04:05",
			"2006-01-02T15:04:05",
			"2006-01-02",
		} {
			if parsed, err := time.Parse(layout, text); err == nil {
				return parsed, true
			}
		}
	}
	return time.Time{}, false
}

func unixLikeToTime(value float64, now time.Time) (time.Time, bool) {
	if value <= 0 {
		return time.Time{}, false
	}
	// Values smaller than 10 years are durations from now rather than absolute unix timestamps.
	if value < 10*365*24*60*60 {
		return now.Add(time.Duration(value) * time.Second), true
	}
	if value > 1e12 {
		return time.UnixMilli(int64(value)), true
	}
	return time.Unix(int64(value), 0), true
}

func isSupplyCapacityEvent(event usage.Event) bool {
	identity := strings.ToLower(strings.Join([]string{event.Provider, event.ExecutorType, event.AuthType, event.AuthProviderSnapshot}, " "))
	if strings.Contains(identity, "codex") || strings.Contains(identity, "openai") {
		return true
	}
	return event.AuthIndex != "" || event.AccountSnapshot != "" || event.AuthFileSnapshot != ""
}

func numberField(values map[string]any, keys ...string) float64 {
	value, _ := numberFieldOK(values, keys...)
	return value
}

func smartAccountConcurrencyLimit(values map[string]any) (int, bool) {
	value, observed := numberFieldOK(
		values,
		"max_concurrency",
		"maxConcurrency",
		"concurrency",
		"concurrency_limit",
		"concurrencyLimit",
	)
	if !observed || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false
	}
	return int(math.Floor(value)), true
}

func numberFieldOK(values map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		value, ok := values[key]
		if !ok || value == nil {
			continue
		}
		switch typed := value.(type) {
		case int:
			return float64(typed), true
		case int64:
			return float64(typed), true
		case float64:
			return typed, true
		case jsonNumber:
			parsed, _ := typed.Float64()
			return parsed, true
		case string:
			if parsed, ok := parseFloat(typed); ok {
				return parsed, true
			}
		}
	}
	return 0, false
}

type jsonNumber interface{ Float64() (float64, error) }

func parseFloat(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	result, err := strconv.ParseFloat(value, 64)
	return result, err == nil
}

func positiveOr(value int, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func clampInt(value, minValue, maxValue int) int {
	if maxValue < minValue {
		maxValue = minValue
	}
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func clampFloat(value, minValue, maxValue float64) float64 {
	if maxValue < minValue {
		maxValue = minValue
	}
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func round1(value float64) float64 {
	return math.Round(value*10) / 10
}

func round2(value float64) float64 {
	return math.Round(value*100) / 100
}

func supplySmartActionPriority(action string) int {
	order := []string{smartActionHealthy, smartActionObserveDemand, smartActionPriceWait, smartActionSupplierGateWait, smartActionPrelock, smartActionWaitLocked, smartActionReleaseLocked, smartActionTakeLocked, smartActionEmergencyReplenish, smartActionBalanceBlocked, smartActionInventoryBlocked, smartActionConfigError, smartActionSnapshotStale, smartActionManualReview}
	for index, item := range order {
		if item == action {
			return index
		}
	}
	return len(order)
}

func sortSmartResources(resources []SmartResource) {
	sort.Slice(resources, func(i, j int) bool {
		return supplySmartActionPriority(resources[i].SuggestedAction) < supplySmartActionPriority(resources[j].SuggestedAction)
	})
}
