package model

type ManagerConfig struct {
	CPAConnection        ManagerCPAConnectionConfig        `json:"cpaConnection"`
	Collector            ManagerCollectorConfig            `json:"collector"`
	CodexInspection      ManagerCodexInspectionConfig      `json:"codexInspection"`
	Supply               ManagerSupplyConfig               `json:"supply"`
	ExternalUsageService ManagerExternalUsageServiceConfig `json:"externalUsageService"`
	UpdatedAtMS          int64                             `json:"updatedAtMs,omitempty"`
}

type ManagerCPAConnectionConfig struct {
	CPABaseURL    string `json:"cpaBaseUrl"`
	ManagementKey string `json:"managementKey,omitempty"`
}

type ManagerCollectorConfig struct {
	Enabled        *bool  `json:"enabled,omitempty"`
	CollectorMode  string `json:"collectorMode,omitempty"`
	Queue          string `json:"queue,omitempty"`
	PopSide        string `json:"popSide,omitempty"`
	BatchSize      int    `json:"batchSize,omitempty"`
	PollIntervalMS int    `json:"pollIntervalMs,omitempty"`
	QueryLimit     int    `json:"queryLimit,omitempty"`
	TLSSkipVerify  bool   `json:"tlsSkipVerify,omitempty"`
}

type ManagerExternalUsageServiceConfig struct {
	Enabled     bool   `json:"enabled"`
	ServiceBase string `json:"serviceBase,omitempty"`
}

type ManagerSupplyConfig struct {
	Enabled                 *bool  `json:"enabled,omitempty"`
	BaseURL                 string `json:"baseUrl"`
	Username                string `json:"username"`
	ClearUsername           bool   `json:"clearUsername,omitempty"`
	Password                string `json:"password,omitempty"`
	PasswordConfigured      bool   `json:"passwordConfigured,omitempty"`
	Product                 string `json:"product"`
	Strategy                string `json:"strategy,omitempty"`
	TargetAvailableAccounts int    `json:"targetAvailableAccounts"`
	ReplenishBatchSize      int    `json:"replenishBatchSize"`
	// LowPriceReserve builds a separately attributed standby pool when a
	// supplier has real inventory at or below the configured unit price. Reserve
	// accounts remain normal usable capacity; only procurement counting and the
	// staged price-watcher decision are separate from normal replenishment.
	LowPriceReserveEnabled         *bool  `json:"lowPriceReserveEnabled,omitempty"`
	LowPriceReserveMaxUnitPriceFen *int64 `json:"lowPriceReserveMaxUnitPriceFen,omitempty"`
	LowPriceReserveTargetAccounts  int    `json:"lowPriceReserveTargetAccounts,omitempty"`
	// LowPriceReserveCheckIntervalMilliseconds is independent from the normal
	// capacity-planning cadence. The dedicated price watcher only quotes cheap
	// inventory and enqueues one bounded ladder stage at a time.
	LowPriceReserveCheckIntervalMilliseconds int `json:"lowPriceReserveCheckIntervalMilliseconds,omitempty"`
	// MaxConcurrentOrders bounds parallel automatic reservations. A zero value
	// keeps the legacy single-order behavior for existing configurations.
	MaxConcurrentOrders         int                                           `json:"maxConcurrentOrders,omitempty"`
	CheckIntervalSeconds        int                                           `json:"checkIntervalSeconds"`
	PollIntervalSeconds         int                                           `json:"pollIntervalSeconds"`
	DefaultWebsockets           bool                                          `json:"defaultWebsockets"`
	SmartEnabled                *bool                                         `json:"smartEnabled,omitempty"`
	HealthyMinutesTarget        int                                           `json:"healthyMinutesTarget"`
	WarningMinutes              int                                           `json:"warningMinutes"`
	CriticalMinutes             int                                           `json:"criticalMinutes"`
	PrelockEnabled              *bool                                         `json:"prelockEnabled,omitempty"`
	PrelockMinQuantity          int                                           `json:"prelockMinQuantity"`
	PrelockMaxQuantity          int                                           `json:"prelockMaxQuantity"`
	CriticalTakeConfirmRounds   int                                           `json:"criticalTakeConfirmRounds"`
	CreateCooldownSeconds       int                                           `json:"createCooldownSeconds"`
	ReleaseCooldownSeconds      int                                           `json:"releaseCooldownSeconds"`
	AuthFilesCacheTTLSeconds    int                                           `json:"authFilesCacheTTLSeconds"`
	MinHoldSeconds              int                                           `json:"minHoldSeconds"`
	NewAccountConfidence        float64                                       `json:"newAccountConfidence"`
	MinBalanceReserveFen        int64                                         `json:"minBalanceReserveFen,omitempty"`
	RevenueMultiplier           float64                                       `json:"revenueMultiplier,omitempty"`
	CriticalAvailableAccounts   int                                           `json:"criticalAvailableAccounts,omitempty"`
	HealthyAvailableAccounts    int                                           `json:"healthyAvailableAccounts,omitempty"`
	StartupAvailableAccounts    *int                                          `json:"startupAvailableAccounts,omitempty"`
	DefaultEmergencyMinAccounts int                                           `json:"defaultEmergencyMinAccounts,omitempty"`
	VirtualDemandTTLMinutes     int                                           `json:"virtualDemandTtlMinutes,omitempty"`
	AccountMaxRequestsBefore401 int                                           `json:"accountMaxRequestsBefore401,omitempty"`
	AccountMaxUsefulSeconds401  int                                           `json:"accountMaxUsefulSecondsBefore401,omitempty"`
	EmergencyBypassUsageRate    *bool                                         `json:"emergencyBypassUsageRate,omitempty"`
	RecoveryTriggerOn401        *bool                                         `json:"recoveryTriggerOn401,omitempty"`
	RecoverySyncEnabled         *bool                                         `json:"recoverySyncEnabled,omitempty"`
	RecoveryAutoClaim           *bool                                         `json:"recoveryAutoClaim,omitempty"`
	RecoverySyncIntervalSeconds int                                           `json:"recoverySyncIntervalSeconds,omitempty"`
	RecoveryClaimBatchSize      int                                           `json:"recoveryClaimBatchSize,omitempty"`
	RecoveryDisableOriginal     *bool                                         `json:"recoveryDisableOriginal,omitempty"`
	QuotaEstimationPolicies     map[string]ManagerSupplyQuotaEstimationPolicy `json:"quotaEstimationPolicies,omitempty"`
	// Platforms is the multi-supplier configuration. The legacy top-level
	// connection fields remain available for backward compatibility and are
	// synthesized as one platform when this slice is absent.
	Platforms                 []ManagerSupplyPlatformConfig `json:"platforms,omitempty"`
	PlatformSelectionStrategy string                        `json:"platformSelectionStrategy,omitempty"`
}

// ManagerSupplyPlatformConfig includes an optional automatic-purchase quota
// gate for marketplace sellers. Unknown sellers receive a bounded trial; low
// observed quota excludes them from later automatic purchases.
type ManagerSupplyPlatformConfig struct {
	ID                         string                                        `json:"id"`
	Name                       string                                        `json:"name,omitempty"`
	Type                       string                                        `json:"type"`
	Enabled                    *bool                                         `json:"enabled,omitempty"`
	BaseURL                    string                                        `json:"baseUrl"`
	Username                   string                                        `json:"username,omitempty"`
	ClearUsername              bool                                          `json:"clearUsername,omitempty"`
	Password                   string                                        `json:"password,omitempty"`
	PasswordConfigured         bool                                          `json:"passwordConfigured,omitempty"`
	Token                      string                                        `json:"token,omitempty"`
	TokenConfigured            bool                                          `json:"tokenConfigured,omitempty"`
	Product                    string                                        `json:"product"`
	PurchaseAccountType        string                                        `json:"purchaseAccountType,omitempty"`
	MaxUnitPriceFen            *int64                                        `json:"maxUnitPriceFen,omitempty"`
	SupplierQuotaGateEnabled   *bool                                         `json:"supplierQuotaGateEnabled,omitempty"`
	SupplierQuotaMinimumM      float64                                       `json:"supplierQuotaMinimumM,omitempty"`
	SupplierQuotaTrialQuantity int                                           `json:"supplierQuotaTrialQuantity,omitempty"`
	SessionRefreshEnabled      *bool                                         `json:"sessionRefreshEnabled,omitempty"`
	ChallengeProvider          string                                        `json:"challengeProvider,omitempty"`
	ChallengeAPIBase           string                                        `json:"challengeApiBase,omitempty"`
	ChallengeAPIKey            string                                        `json:"challengeApiKey,omitempty"`
	ChallengeAPIKeyConfigured  bool                                          `json:"challengeApiKeyConfigured,omitempty"`
	ClearChallengeAPIKey       bool                                          `json:"clearChallengeApiKey,omitempty"`
	RefreshCooldownSeconds     int                                           `json:"refreshCooldownSeconds,omitempty"`
	Priority                   int                                           `json:"priority,omitempty"`
	EmergencyOnly              bool                                          `json:"emergencyOnly,omitempty"`
	QuotaEstimationPolicies    map[string]ManagerSupplyQuotaEstimationPolicy `json:"quotaEstimationPolicies,omitempty"`
}

type ManagerSupplyQuotaEstimationPolicy struct {
	Mode      string  `json:"mode,omitempty"`
	FallbackM float64 `json:"fallbackM,omitempty"`
	FixedM    float64 `json:"fixedM,omitempty"`
}
