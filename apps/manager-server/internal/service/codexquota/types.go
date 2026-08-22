package codexquota

import (
	"encoding/json"
	"errors"
)

var (
	ErrNotConfigured     = errors.New("usage service is not configured")
	ErrInvalidRequest    = errors.New("codex quota reset request is invalid")
	ErrAuthNotFound      = errors.New("codex auth file not found")
	ErrOperationNotFound = errors.New("codex quota operation not found")
	ErrOperationConflict = errors.New("codex quota operation conflicts with the selected account")
	ErrAccountBusy       = errors.New("codex account already has an active quota operation")
)

type ResetRequest struct {
	AuthIndex   string `json:"auth_index"`
	OperationID string `json:"operation_id"`
}

type ResetResult struct {
	UsageBefore          json.RawMessage `json:"usage_before,omitempty"`
	UsageAfter           json.RawMessage `json:"usage_after,omitempty"`
	ResetCreditsBefore   json.RawMessage `json:"reset_credits_before,omitempty"`
	ResetCreditsAfter    json.RawMessage `json:"reset_credits_after,omitempty"`
	ConsumeResponse      json.RawMessage `json:"consume_response,omitempty"`
	LocalResetResponse   json.RawMessage `json:"local_reset_response,omitempty"`
	Verified             bool            `json:"verified"`
	VerificationAttempts int             `json:"verification_attempts"`
	HistoryDeleted       int64           `json:"history_deleted,omitempty"`
}

type OperationResponse struct {
	OperationID    string       `json:"operation_id"`
	AccountKey     string       `json:"account_key"`
	AuthIndex      string       `json:"auth_index"`
	AuthFileName   string       `json:"auth_file_name,omitempty"`
	State          string       `json:"state"`
	Consumed       *bool        `json:"consumed"`
	UpstreamStatus *int         `json:"upstream_status,omitempty"`
	WarningCodes   []string     `json:"warning_codes"`
	Result         *ResetResult `json:"result,omitempty"`
	LastError      string       `json:"last_error,omitempty"`
	CreatedAtMS    int64        `json:"created_at_ms"`
	UpdatedAtMS    int64        `json:"updated_at_ms"`
}

// AutoResetResult describes the eligibility check used by the cooldown worker.
// It is intentionally separate from OperationResponse so a poll that finds no
// remaining credit never creates an operation or consumes a credit.
type AutoResetResult struct {
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason,omitempty"`
}

type ResetCreditInspectionItem struct {
	AuthIndex       string `json:"auth_index"`
	AuthFileName    string `json:"auth_file_name"`
	AccountID       string `json:"account_id,omitempty"`
	Account         string `json:"account,omitempty"`
	Disabled        bool   `json:"disabled"`
	CurrentRequests *int64 `json:"current_requests,omitempty"`
	AvailableCount  int64  `json:"available_count"`
	ResetCount      int64  `json:"reset_count"`
	Exhausted       bool   `json:"exhausted"`
	Eligible        bool   `json:"eligible"`
	Reason          string `json:"reason,omitempty"`
}

type ResetCountItem struct {
	AuthFileName string `json:"auth_file_name"`
	AuthIndex    string `json:"auth_index"`
	ResetCount   int64  `json:"reset_count"`
}

type BatchResetRequest struct {
	AuthIndexes []string `json:"auth_indexes"`
}

type BatchResetOutcome struct {
	AuthIndex string `json:"auth_index"`
	State     string `json:"state,omitempty"`
	Eligible  bool   `json:"eligible"`
	Reason    string `json:"reason,omitempty"`
	Error     string `json:"error,omitempty"`
}
