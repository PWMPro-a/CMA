package model

type SupplyOrder struct {
	ID                        int64  `json:"id"`
	OrderID                   string `json:"orderId"`
	TaskID                    string `json:"taskId,omitempty"`
	SupplierID                string `json:"supplierId,omitempty"`
	MarketplaceSellerID       string `json:"marketplaceSellerId,omitempty"`
	MarketplaceSellerName     string `json:"marketplaceSellerName,omitempty"`
	MarketplaceChannelID      string `json:"marketplaceChannelId,omitempty"`
	MarketplaceSelectionToken string `json:"marketplaceSelectionToken,omitempty"`
	Product                   string `json:"product"`
	RequestedQuantity         int    `json:"requestedQuantity"`
	Automatic                 bool   `json:"automatic"`
	Strategy                  string `json:"strategy,omitempty"`
	TriggerReason             string `json:"triggerReason,omitempty"`
	Status                    string `json:"status"`
	RemoteStatus              string `json:"remoteStatus,omitempty"`
	ReadyQuantity             int    `json:"readyQuantity"`
	Progress                  int    `json:"progress"`
	StatusURL                 string `json:"statusUrl,omitempty"`
	TakeURL                   string `json:"takeUrl,omitempty"`
	ChargedFen                int64  `json:"chargedFen"`
	ReleasedFen               int64  `json:"releasedFen"`
	ItemCount                 int    `json:"itemCount"`
	ImportedCount             int    `json:"importedCount"`
	LastError                 string `json:"lastError,omitempty"`
	NextPollAtMS              int64  `json:"nextPollAtMs,omitempty"`
	// SupplierRetryUntilMS is distinct from the regular poll deadline so an
	// emergency cycle skips only local pacing, never retry_after_seconds.
	SupplierRetryUntilMS int64 `json:"supplierRetryUntilMs,omitempty"`
	CompletedAtMS        int64 `json:"completedAtMs,omitempty"`
	CreatedAtMS          int64 `json:"createdAtMs"`
	UpdatedAtMS          int64 `json:"updatedAtMs"`
}

// SupplyPurchaseTask is the durable intent to acquire a target number of
// usable accounts. Manual and automatic planners only create/update this
// intent; the dedicated purchase-task worker owns supplier order creation,
// retries and completion accounting.
type SupplyPurchaseTask struct {
	ID                  int64  `json:"id"`
	TaskID              string `json:"taskId"`
	Source              string `json:"source"`
	SupplierID          string `json:"supplierId,omitempty"`
	Product             string `json:"product,omitempty"`
	TargetQuantity      int    `json:"targetQuantity"`
	FulfilledQuantity   int    `json:"fulfilledQuantity"`
	Status              string `json:"status"`
	Strategy            string `json:"strategy,omitempty"`
	TriggerReason       string `json:"triggerReason,omitempty"`
	MaxConcurrentOrders int    `json:"maxConcurrentOrders"`
	AttemptCount        int    `json:"attemptCount"`
	OrderCount          int    `json:"orderCount"`
	ActiveOrderCount    int    `json:"activeOrderCount"`
	NextAttemptAtMS     int64  `json:"nextAttemptAtMs,omitempty"`
	LastError           string `json:"lastError,omitempty"`
	CancelledAtMS       int64  `json:"cancelledAtMs,omitempty"`
	CompletedAtMS       int64  `json:"completedAtMs,omitempty"`
	CreatedAtMS         int64  `json:"createdAtMs"`
	UpdatedAtMS         int64  `json:"updatedAtMs"`
}

type SupplyImportItem struct {
	ID                        int64  `json:"id"`
	OrderID                   string `json:"orderId"`
	ItemKey                   string `json:"itemKey"`
	AccountName               string `json:"accountName,omitempty"`
	NameKey                   string `json:"-"`
	FileName                  string `json:"fileName"`
	ImportAction              string `json:"importAction,omitempty"`
	ReplacedFileName          string `json:"replacedFileName,omitempty"`
	SupersedesItemID          int64  `json:"supersedesItemId,omitempty"`
	Status                    string `json:"status"`
	PayloadJSON               string `json:"-"`
	LastError                 string `json:"lastError,omitempty"`
	AttemptCount              int    `json:"attemptCount"`
	NextRetryAtMS             int64  `json:"nextRetryAtMs,omitempty"`
	ImportedAtMS              int64  `json:"importedAtMs,omitempty"`
	EffectiveFromMS           int64  `json:"effectiveFromMs,omitempty"`
	SupersededAtMS            int64  `json:"supersededAtMs,omitempty"`
	LeaseExpiresAtMS          int64  `json:"leaseExpiresAtMs,omitempty"`
	WarrantyExpiresAtMS       int64  `json:"warrantyExpiresAtMs,omitempty"`
	MarketplaceSellerID       string `json:"marketplaceSellerId,omitempty"`
	MarketplaceSellerName     string `json:"marketplaceSellerName,omitempty"`
	MarketplaceChannelID      string `json:"marketplaceChannelId,omitempty"`
	MarketplaceSelectionToken string `json:"marketplaceSelectionToken,omitempty"`
	BasePriceFen              int64  `json:"basePriceFen,omitempty"`
	ChargedFen                int64  `json:"chargedFen,omitempty"`
	// QuotaCapacityM is the durable per-account capacity sample used for
	// supplier cost-ratio ranking. It is populated from the first 5% estimate
	// and replaced only when the same account reaches an exhausted final window.
	QuotaCapacityM            float64 `json:"-"`
	QuotaCapacityObservedAtMS int64   `json:"-"`
	QuotaCapacityComplete     bool    `json:"-"`
	CreatedAtMS               int64   `json:"createdAtMs"`
	UpdatedAtMS               int64   `json:"updatedAtMs"`
}

type SupplyRecovery struct {
	ID                  int64                      `json:"id"`
	RecoveryID          string                     `json:"recoveryId"`
	SupplierID          string                     `json:"supplierId,omitempty"`
	Product             string                     `json:"product,omitempty"`
	DeliveryStatus      string                     `json:"deliveryStatus"`
	Status              string                     `json:"status"`
	CredentialVersion   int                        `json:"credentialVersion,omitempty"`
	SourceOrderID       string                     `json:"sourceOrderId,omitempty"`
	OriginalFileName    string                     `json:"originalFileName,omitempty"`
	OriginalAuthIndex   string                     `json:"originalAuthIndex,omitempty"`
	OriginalEmail       string                     `json:"originalEmail,omitempty"`
	ClaimURL            string                     `json:"-"`
	ClaimTicket         string                     `json:"-"`
	ClaimOrderID        string                     `json:"claimOrderId,omitempty"`
	ItemCount           int                        `json:"itemCount"`
	ImportedCount       int                        `json:"importedCount"`
	RefundedFen         int64                      `json:"refundedFen,omitempty"`
	LastError           string                     `json:"lastError,omitempty"`
	RawJSON             string                     `json:"-"`
	LastSeenAtMS        int64                      `json:"lastSeenAtMs"`
	ClaimedAtMS         int64                      `json:"claimedAtMs,omitempty"`
	CreatedAtMS         int64                      `json:"createdAtMs"`
	UpdatedAtMS         int64                      `json:"updatedAtMs"`
	ImportedFileNames   []string                   `json:"importedFileNames,omitempty"`
	ImportPendingCount  int                        `json:"importPendingCount,omitempty"`
	ImportFailedCount   int                        `json:"importFailedCount,omitempty"`
	LastImportedAtMS    int64                      `json:"lastImportedAtMs,omitempty"`
	ImportStatus        string                     `json:"importStatus,omitempty"`
	ImportMessage       string                     `json:"importMessage,omitempty"`
	Ownership           string                     `json:"ownership,omitempty"`
	ImportNextRetryAtMS int64                      `json:"importNextRetryAtMs,omitempty"`
	ImportItems         []SupplyRecoveryImportItem `json:"importItems,omitempty"`
}

// SupplyRecoveryImportItem is the display-safe import state for one
// replacement credential. It deliberately excludes PayloadJSON so the
// recovery list can show a useful audit trail without returning credentials.
type SupplyRecoveryImportItem struct {
	AccountName      string `json:"accountName,omitempty"`
	FileName         string `json:"fileName,omitempty"`
	ImportAction     string `json:"importAction,omitempty"`
	ReplacedFileName string `json:"replacedFileName,omitempty"`
	Status           string `json:"status"`
	LastError        string `json:"lastError,omitempty"`
	AttemptCount     int    `json:"attemptCount"`
	NextRetryAtMS    int64  `json:"nextRetryAtMs,omitempty"`
	ImportedAtMS     int64  `json:"importedAtMs,omitempty"`
	UpdatedAtMS      int64  `json:"updatedAtMs"`
}
