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
	Enabled                     *bool   `json:"enabled,omitempty"`
	BaseURL                     string  `json:"baseUrl"`
	Username                    string  `json:"username"`
	Password                    string  `json:"password,omitempty"`
	PasswordConfigured          bool    `json:"passwordConfigured,omitempty"`
	Product                     string  `json:"product"`
	TargetAvailableAccounts     int     `json:"targetAvailableAccounts"`
	ReplenishBatchSize          int     `json:"replenishBatchSize"`
	CheckIntervalSeconds        int     `json:"checkIntervalSeconds"`
	PollIntervalSeconds         int     `json:"pollIntervalSeconds"`
	DefaultWebsockets           bool    `json:"defaultWebsockets"`
	SmartEnabled                *bool   `json:"smartEnabled,omitempty"`
	HealthyMinutesTarget        int     `json:"healthyMinutesTarget"`
	WarningMinutes              int     `json:"warningMinutes"`
	CriticalMinutes             int     `json:"criticalMinutes"`
	PrelockEnabled              *bool   `json:"prelockEnabled,omitempty"`
	PrelockMinQuantity          int     `json:"prelockMinQuantity"`
	PrelockMaxQuantity          int     `json:"prelockMaxQuantity"`
	CriticalTakeConfirmRounds   int     `json:"criticalTakeConfirmRounds"`
	CreateCooldownSeconds       int     `json:"createCooldownSeconds"`
	ReleaseCooldownSeconds      int     `json:"releaseCooldownSeconds"`
	AuthFilesCacheTTLSeconds    int     `json:"authFilesCacheTTLSeconds"`
	MinHoldSeconds              int     `json:"minHoldSeconds"`
	NewAccountConfidence        float64 `json:"newAccountConfidence"`
	MinBalanceReserveFen        int64   `json:"minBalanceReserveFen,omitempty"`
	DailyMaxHoldFen             int64   `json:"dailyMaxHoldFen,omitempty"`
	DailyMaxReplenishQuantity   int     `json:"dailyMaxReplenishQuantity,omitempty"`
	RevenueMultiplier           float64 `json:"revenueMultiplier,omitempty"`
	RecoverySyncEnabled         *bool   `json:"recoverySyncEnabled,omitempty"`
	RecoveryAutoClaim           *bool   `json:"recoveryAutoClaim,omitempty"`
	RecoverySyncIntervalSeconds int     `json:"recoverySyncIntervalSeconds,omitempty"`
	RecoveryClaimBatchSize      int     `json:"recoveryClaimBatchSize,omitempty"`
	RecoveryDisableOriginal     *bool   `json:"recoveryDisableOriginal,omitempty"`
}
