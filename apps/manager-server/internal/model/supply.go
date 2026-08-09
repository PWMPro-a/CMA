package model

type SupplyOrder struct {
	ID                int64  `json:"id"`
	OrderID           string `json:"orderId"`
	Product           string `json:"product"`
	RequestedQuantity int    `json:"requestedQuantity"`
	Automatic         bool   `json:"automatic"`
	Status            string `json:"status"`
	RemoteStatus      string `json:"remoteStatus,omitempty"`
	ReadyQuantity     int    `json:"readyQuantity"`
	Progress          int    `json:"progress"`
	StatusURL         string `json:"statusUrl,omitempty"`
	TakeURL           string `json:"takeUrl,omitempty"`
	ChargedFen        int64  `json:"chargedFen"`
	ReleasedFen       int64  `json:"releasedFen"`
	ItemCount         int    `json:"itemCount"`
	ImportedCount     int    `json:"importedCount"`
	LastError         string `json:"lastError,omitempty"`
	NextPollAtMS      int64  `json:"nextPollAtMs,omitempty"`
	// SupplierRetryUntilMS is distinct from the regular poll deadline so an
	// emergency cycle skips only local pacing, never retry_after_seconds.
	SupplierRetryUntilMS int64 `json:"supplierRetryUntilMs,omitempty"`
	CompletedAtMS        int64 `json:"completedAtMs,omitempty"`
	CreatedAtMS          int64 `json:"createdAtMs"`
	UpdatedAtMS          int64 `json:"updatedAtMs"`
}

type SupplyImportItem struct {
	ID               int64  `json:"id"`
	OrderID          string `json:"orderId"`
	ItemKey          string `json:"itemKey"`
	FileName         string `json:"fileName"`
	Status           string `json:"status"`
	PayloadJSON      string `json:"-"`
	LastError        string `json:"lastError,omitempty"`
	AttemptCount     int    `json:"attemptCount"`
	NextRetryAtMS    int64  `json:"nextRetryAtMs,omitempty"`
	ImportedAtMS     int64  `json:"importedAtMs,omitempty"`
	LeaseExpiresAtMS int64  `json:"leaseExpiresAtMs,omitempty"`
	BasePriceFen     int64  `json:"basePriceFen,omitempty"`
	ChargedFen       int64  `json:"chargedFen,omitempty"`
	CreatedAtMS      int64  `json:"createdAtMs"`
	UpdatedAtMS      int64  `json:"updatedAtMs"`
}

type SupplyRecovery struct {
	ID                int64  `json:"id"`
	RecoveryID        string `json:"recoveryId"`
	Product           string `json:"product,omitempty"`
	DeliveryStatus    string `json:"deliveryStatus"`
	Status            string `json:"status"`
	OriginalFileName  string `json:"originalFileName,omitempty"`
	OriginalAuthIndex string `json:"originalAuthIndex,omitempty"`
	OriginalEmail     string `json:"originalEmail,omitempty"`
	ClaimURL          string `json:"-"`
	ClaimOrderID      string `json:"claimOrderId,omitempty"`
	ItemCount         int    `json:"itemCount"`
	ImportedCount     int    `json:"importedCount"`
	RefundedFen       int64  `json:"refundedFen,omitempty"`
	LastError         string `json:"lastError,omitempty"`
	RawJSON           string `json:"-"`
	LastSeenAtMS      int64  `json:"lastSeenAtMs"`
	ClaimedAtMS       int64  `json:"claimedAtMs,omitempty"`
	CreatedAtMS       int64  `json:"createdAtMs"`
	UpdatedAtMS       int64  `json:"updatedAtMs"`
}
