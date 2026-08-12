package model

const (
	CodexQuotaOperationStateCreated              = "created"
	CodexQuotaOperationStateConsuming            = "consuming"
	CodexQuotaOperationStateUpstreamAccepted     = "upstream_accepted"
	CodexQuotaOperationStateVerifying            = "verifying"
	CodexQuotaOperationStateLocallyRecovered     = "locally_recovered"
	CodexQuotaOperationStateCompleted            = "completed"
	CodexQuotaOperationStateConsumeStatusUnknown = "consume_status_unknown"
	CodexQuotaOperationStatePartialSuccess       = "partial_success"
	CodexQuotaOperationStateFailed               = "failed"
)

type CodexQuotaOperation struct {
	OperationID      string
	AccountKey       string
	AuthIndex        string
	AuthFileName     string
	State            string
	Consumed         *bool
	UpstreamStatus   *int
	WarningCodesJSON string
	ResultJSON       string
	LastError        string
	CreatedAtMS      int64
	UpdatedAtMS      int64
}
