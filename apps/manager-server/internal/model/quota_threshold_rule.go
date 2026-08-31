package model

// QuotaThresholdRule disables a CPA credential when a live Codex response
// reports remaining quota at or below ThresholdPercent.
type QuotaThresholdRule struct {
	ID                           int64    `json:"id"`
	FileName                     string   `json:"fileName"`
	AuthIndex                    string   `json:"authIndex,omitempty"`
	Provider                     string   `json:"provider,omitempty"`
	AccountSnapshot              string   `json:"accountSnapshot,omitempty"`
	AccountID                    string   `json:"accountId,omitempty"`
	ThresholdPercent             float64  `json:"thresholdPercent"`
	Enabled                      bool     `json:"enabled"`
	LastObservedRemainingPercent *float64 `json:"lastObservedRemainingPercent,omitempty"`
	LastDisabled                 bool     `json:"lastDisabled"`
	LastTriggeredAtMS            int64    `json:"lastTriggeredAtMs,omitempty"`
	LastInspectionAtMS           int64    `json:"lastInspectionAtMs,omitempty"`
	LastError                    string   `json:"lastError,omitempty"`
	CreatedAtMS                  int64    `json:"createdAtMs"`
	UpdatedAtMS                  int64    `json:"updatedAtMs"`
}
