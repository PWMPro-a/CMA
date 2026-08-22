package codexquota

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	quotaoperationrepo "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/codexquotaoperation"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpaauthfiles"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

const persistenceTimeout = 3 * time.Second

type authFileFinder interface {
	Find(ctx context.Context, baseURL string, managementKey string, fileName string, authIndex string) (cpaauthfiles.File, bool, error)
}

type authFileFetcher interface {
	Fetch(ctx context.Context, baseURL string, managementKey string) ([]cpaauthfiles.File, error)
}

type authFileStatusMutator interface {
	ResolveVerifiedStatusMutationTarget(ctx context.Context, baseURL string, managementKey string, identity cpaauthfiles.Identity) (cpaauthfiles.StatusMutationTarget, error)
	PatchDisabledTarget(ctx context.Context, baseURL string, managementKey string, target cpaauthfiles.StatusMutationTarget, disabled bool) error
}

type quotaCooldownRepository interface {
	ListActive(ctx context.Context) ([]model.QuotaCooldown, error)
	MarkRecovered(ctx context.Context, id int64, recoveredAtMS int64) error
}

type quotaGateway interface {
	usage(ctx context.Context, setup store.Setup, authIndex string, accountID string) (apiCallResult, error)
	resetCredits(ctx context.Context, setup store.Setup, authIndex string, accountID string) (apiCallResult, error)
	consumeResetCredit(ctx context.Context, setup store.Setup, authIndex string, accountID string, operationID string) (apiCallResult, error)
	resetLocalQuota(ctx context.Context, setup store.Setup, authIndex string) (json.RawMessage, int, error)
}

type credentialHistoryRepository interface {
	DeleteCredentialHistory(ctx context.Context, authFileSnapshot, authIndex string) (int64, error)
}

type setupResolver interface {
	ResolveSetup(ctx context.Context) (store.Setup, bool, error)
}

type Service struct {
	operations        quotaoperationrepo.Repository
	setupService      setupResolver
	authFiles         authFileFinder
	authStatuses      authFileStatusMutator
	gateway           quotaGateway
	quotaCooldowns    quotaCooldownRepository
	authFileMutations *cpaauthfiles.MutationCoordinator
	locks             *accountLocks
	history           credentialHistoryRepository
}

func New(st *store.Store, setupService *managerconfig.Service, clients ...*http.Client) *Service {
	return NewWithMutationCoordinator(st, setupService, nil, clients...)
}

func NewWithMutationCoordinator(
	st *store.Store,
	setupService *managerconfig.Service,
	coordinator *cpaauthfiles.MutationCoordinator,
	clients ...*http.Client,
) *Service {
	if st == nil {
		if coordinator == nil {
			coordinator = cpaauthfiles.NewMutationCoordinator()
		}
		return &Service{
			setupService:      setupService,
			authFileMutations: coordinator,
			locks:             newAccountLocks(),
		}
	}
	var client *http.Client
	if len(clients) > 0 {
		client = clients[0]
	}
	if coordinator == nil {
		coordinator = cpaauthfiles.NewMutationCoordinator()
	}
	authFiles := cpaauthfiles.New(client, defaultOperationTimeout)
	return &Service{
		operations:        st.CodexQuotaOperations,
		setupService:      setupService,
		authFiles:         authFiles,
		authStatuses:      authFiles,
		gateway:           newCPAAdapter(client),
		quotaCooldowns:    st.QuotaCooldowns,
		authFileMutations: coordinator,
		locks:             newAccountLocks(),
		history:           st.UsageEvents,
	}
}

func (s *Service) ResetCredit(ctx context.Context, request ResetRequest) (OperationResponse, error) {
	authIndex := strings.TrimSpace(request.AuthIndex)
	operationID := strings.TrimSpace(request.OperationID)
	if authIndex == "" || !isOperationUUID(operationID) {
		return OperationResponse{}, ErrInvalidRequest
	}
	setup, ok, err := s.resolveSetup(ctx)
	if err != nil {
		return OperationResponse{}, err
	}
	if !ok {
		return OperationResponse{}, ErrNotConfigured
	}
	file, found, err := s.authFiles.Find(ctx, setup.CPAUpstreamURL, setup.ManagementKey, "", authIndex)
	if err != nil {
		return OperationResponse{}, err
	}
	if !found || !strings.EqualFold(strings.TrimSpace(file.Provider), "codex") {
		return OperationResponse{}, ErrAuthNotFound
	}
	accountKey := stableAccountKey(file)
	release, err := s.locks.acquire(ctx, accountKey)
	if err != nil {
		return OperationResponse{}, err
	}
	defer release()

	operation, found, err := s.operations.Get(ctx, operationID)
	if err != nil {
		return OperationResponse{}, err
	}
	if found {
		if operation.AccountKey != accountKey || operation.AuthIndex != authIndex {
			return OperationResponse{}, ErrOperationConflict
		}
		return s.resume(ctx, setup, file, operation)
	}
	operation, created, err := s.operations.Create(ctx, model.CodexQuotaOperation{
		OperationID:  operationID,
		AccountKey:   accountKey,
		AuthIndex:    authIndex,
		AuthFileName: strings.TrimSpace(file.Name),
		State:        model.CodexQuotaOperationStateCreated,
	})
	if errors.Is(err, quotaoperationrepo.ErrAccountBusy) {
		if operation.AccountKey == accountKey && operation.AuthIndex == authIndex {
			return s.resume(ctx, setup, file, operation)
		}
		return OperationResponse{}, ErrAccountBusy
	}
	if err != nil {
		return OperationResponse{}, err
	}
	if !created && (operation.AccountKey != accountKey || operation.AuthIndex != authIndex) {
		return OperationResponse{}, ErrOperationConflict
	}
	return s.resume(ctx, setup, file, operation)
}

// AutoResetCredit verifies that the account is currently exhausted and has a
// reset credit before entering the existing idempotent reset saga. The caller
// supplies a stable operation id derived from the cooldown row.
func (s *Service) AutoResetCredit(ctx context.Context, request ResetRequest) (OperationResponse, AutoResetResult, error) {
	authIndex := strings.TrimSpace(request.AuthIndex)
	operationID := strings.TrimSpace(request.OperationID)
	if authIndex == "" || !isOperationUUID(operationID) {
		return OperationResponse{}, AutoResetResult{Reason: "invalid_request"}, ErrInvalidRequest
	}
	setup, ok, err := s.resolveSetup(ctx)
	if err != nil {
		return OperationResponse{}, AutoResetResult{}, err
	}
	if !ok {
		return OperationResponse{}, AutoResetResult{}, ErrNotConfigured
	}
	file, found, err := s.authFiles.Find(ctx, setup.CPAUpstreamURL, setup.ManagementKey, "", authIndex)
	if err != nil {
		return OperationResponse{}, AutoResetResult{}, err
	}
	if !found || !strings.EqualFold(strings.TrimSpace(file.Provider), "codex") {
		return OperationResponse{}, AutoResetResult{Reason: "auth_not_found"}, ErrAuthNotFound
	}
	if currentRequests, ok := currentRequestCount(file.Raw); !ok || currentRequests != 0 {
		return OperationResponse{}, AutoResetResult{Reason: "active_requests"}, nil
	}
	usage, credits := s.fetchBefore(ctx, setup, file)
	if usage.err != nil || !successfulStatus(usage.response.StatusCode) {
		return OperationResponse{}, AutoResetResult{Reason: "usage_unavailable"}, nil
	}
	observed, exhausted := codexUsageLimitState(usage.response.Body, codexQuotaRecoveryThreshold)
	if !observed {
		return OperationResponse{}, AutoResetResult{Reason: "usage_unavailable"}, nil
	}
	if !exhausted {
		// A recovered quota needs no credit consumption. Still clear a stale
		// CPAMP quota-preempt disable so the worker can finish the cooldown
		// recovery path immediately.
		if err := s.recoverRuntimeQuotaPreempt(ctx, setup, file); err != nil {
			return OperationResponse{}, AutoResetResult{Eligible: true, Reason: "recovery_failed"}, err
		}
		return OperationResponse{
			OperationID: operationID,
			AuthIndex:   authIndex,
			State:       model.CodexQuotaOperationStateCompleted,
		}, AutoResetResult{Eligible: true, Reason: "quota_already_recovered"}, nil
	}
	if credits.err != nil || !successfulStatus(credits.response.StatusCode) || !hasAvailableResetCredit(credits.response.Body) {
		return OperationResponse{}, AutoResetResult{Reason: "no_reset_credit"}, nil
	}
	response, err := s.ResetCredit(ctx, request)
	if err != nil {
		return OperationResponse{}, AutoResetResult{Eligible: true, Reason: "reset_failed"}, err
	}
	return response, AutoResetResult{Eligible: true, Reason: "reset_started"}, nil
}

func (s *Service) InspectResetCredits(ctx context.Context) ([]ResetCreditInspectionItem, error) {
	setup, ok, err := s.resolveSetup(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotConfigured
	}
	fetcher, ok := s.authFiles.(authFileFetcher)
	if !ok {
		return nil, ErrNotConfigured
	}
	files, err := fetcher.Fetch(ctx, setup.CPAUpstreamURL, setup.ManagementKey)
	if err != nil {
		return nil, err
	}
	candidates := make([]cpaauthfiles.File, 0, len(files))
	for _, file := range files {
		if !strings.EqualFold(strings.TrimSpace(file.Provider), "codex") || strings.TrimSpace(file.AuthIndex) == "" {
			continue
		}
		candidates = append(candidates, file)
	}
	items := make([]ResetCreditInspectionItem, len(candidates))
	if len(candidates) == 0 {
		return items, nil
	}
	workerCount := 8
	if len(candidates) < workerCount {
		workerCount = len(candidates)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				items[index] = s.inspectResetCreditItem(ctx, setup, candidates[index])
			}
		}()
	}
	for index := range candidates {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	return items, nil
}

// ListResetCounts returns durable reset totals without making provider quota
// requests. It is used by the account list and is intentionally lightweight.
func (s *Service) ListResetCounts(ctx context.Context) ([]ResetCountItem, error) {
	setup, ok, err := s.resolveSetup(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotConfigured
	}
	fetcher, ok := s.authFiles.(authFileFetcher)
	if !ok {
		return nil, ErrNotConfigured
	}
	files, err := fetcher.Fetch(ctx, setup.CPAUpstreamURL, setup.ManagementKey)
	if err != nil {
		return nil, err
	}
	items := make([]ResetCountItem, 0, len(files))
	for _, file := range files {
		if !strings.EqualFold(strings.TrimSpace(file.Provider), "codex") || strings.TrimSpace(file.AuthIndex) == "" {
			continue
		}
		count, err := s.operations.CountCompletedByCredential(
			ctx,
			stableAccountKey(file),
			file.AuthIndex,
		)
		if err != nil {
			return nil, err
		}
		items = append(items, ResetCountItem{AuthFileName: file.Name, AuthIndex: file.AuthIndex, ResetCount: count})
	}
	return items, nil
}

func (s *Service) inspectResetCreditItem(ctx context.Context, setup store.Setup, file cpaauthfiles.File) ResetCreditInspectionItem {
	usage, credits := s.fetchBefore(ctx, setup, file)
	item := ResetCreditInspectionItem{
		AuthIndex: file.AuthIndex, AuthFileName: file.Name, AccountID: file.AccountID,
		Account: file.AccountSnapshot, Disabled: file.Disabled,
	}
	if s.operations != nil {
		if count, err := s.operations.CountCompletedByCredential(
			ctx,
			stableAccountKey(file),
			file.AuthIndex,
		); err == nil {
			item.ResetCount = count
		}
	}
	if usage.err != nil || !successfulStatus(usage.response.StatusCode) {
		item.Reason = "usage_unavailable"
	} else {
		_, item.Exhausted = codexUsageLimitState(usage.response.Body, codexQuotaRecoveryThreshold)
	}
	if credits.err == nil && successfulStatus(credits.response.StatusCode) {
		item.AvailableCount = availableResetCreditCount(credits.response.Body)
	} else {
		item.Reason = "reset_credits_unavailable"
	}
	if currentRequests, ok := currentRequestCount(file.Raw); ok {
		item.CurrentRequests = &currentRequests
		item.Eligible = item.Exhausted && item.AvailableCount > 0 && currentRequests == 0
	} else {
		item.Eligible = false
	}
	if !item.Eligible && item.Reason == "" {
		switch {
		case !item.Exhausted:
			item.Reason = "quota_not_exhausted"
		case item.AvailableCount == 0:
			item.Reason = "no_reset_credit"
		default:
			item.Reason = "active_requests"
		}
	}
	return item
}

func (s *Service) BatchResetCredits(ctx context.Context, request BatchResetRequest) ([]BatchResetOutcome, error) {
	seen := make(map[string]struct{}, len(request.AuthIndexes))
	outcomes := make([]BatchResetOutcome, 0, len(request.AuthIndexes))
	for _, raw := range request.AuthIndexes {
		authIndex := strings.TrimSpace(raw)
		if authIndex == "" {
			continue
		}
		if _, ok := seen[authIndex]; ok {
			continue
		}
		seen[authIndex] = struct{}{}
		opID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("manual-batch-reset:"+authIndex+":"+strconv.FormatInt(time.Now().Unix()/60, 10))).String()
		response, eligible, err := s.AutoResetCredit(ctx, ResetRequest{AuthIndex: authIndex, OperationID: opID})
		outcome := BatchResetOutcome{AuthIndex: authIndex, Eligible: eligible.Eligible, Reason: eligible.Reason}
		if err != nil {
			outcome.Error = err.Error()
		} else {
			outcome.State = response.State
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

func hasAvailableResetCredit(body json.RawMessage) bool {
	return availableResetCreditCount(body) > 0
}

func availableResetCreditCount(body json.RawMessage) int64 {
	var payload any
	if len(body) == 0 || json.Unmarshal(body, &payload) != nil {
		return 0
	}
	return availableResetCreditValue(payload)
}

func availableResetCreditValue(value any) int64 {
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range []string{"available_count", "availableCount", "available", "count"} {
			if raw, ok := typed[key]; ok {
				if count, ok := numberValue(raw); ok {
					return maxNonNegativeInt64(count)
				}
			}
		}
		for _, key := range []string{
			"rate_limit_reset_credits",
			"rateLimitResetCredits",
			"reset_credits",
			"resetCredits",
			"credits",
			"data",
		} {
			if nested, ok := typed[key]; ok {
				if count := availableResetCreditValue(nested); count > 0 {
					return count
				}
			}
		}
	case []any:
		available := int64(0)
		for _, item := range typed {
			record, ok := item.(map[string]any)
			if !ok {
				continue
			}
			status, _ := record["status"].(string)
			if strings.EqualFold(strings.TrimSpace(status), "available") || strings.TrimSpace(status) == "" {
				available++
			}
		}
		return available
	}
	return 0
}

func maxNonNegativeInt64(value float64) int64 {
	if value <= 0 {
		return 0
	}
	return int64(value)
}

func currentRequestCountZero(raw map[string]any) bool {
	count, ok := currentRequestCount(raw)
	return ok && count == 0
}

func currentRequestCount(raw map[string]any) (int64, bool) {
	var maxCount int64
	found := false
	for _, key := range []string{"runtime_current_concurrency", "runtimeCurrentConcurrency", "current_concurrency", "currentConcurrency", "active_requests", "activeRequests", "in_flight_requests", "inFlightRequests"} {
		value, ok := raw[key]
		if !ok {
			continue
		}
		count, ok := numberValue(value)
		if !ok {
			continue
		}
		if count < 0 {
			count = 0
		}
		if !found || int64(count) > maxCount {
			maxCount = int64(count)
		}
		found = true
	}
	return maxCount, found
}

func (s *Service) GetOperation(ctx context.Context, operationID string) (OperationResponse, error) {
	operationID = strings.TrimSpace(operationID)
	if !isOperationUUID(operationID) {
		return OperationResponse{}, ErrInvalidRequest
	}
	operation, found, err := s.operations.Get(ctx, operationID)
	if err != nil {
		return OperationResponse{}, err
	}
	if !found {
		return OperationResponse{}, ErrOperationNotFound
	}
	return operationResponse(operation), nil
}

func (s *Service) resolveSetup(ctx context.Context) (store.Setup, bool, error) {
	if s == nil || s.setupService == nil || s.operations == nil || s.authFiles == nil || s.gateway == nil {
		return store.Setup{}, false, ErrNotConfigured
	}
	return s.setupService.ResolveSetup(ctx)
}

func (s *Service) resume(
	ctx context.Context,
	setup store.Setup,
	file cpaauthfiles.File,
	operation model.CodexQuotaOperation,
) (OperationResponse, error) {
	result := decodeResult(operation.ResultJSON)
	warnings := decodeWarnings(operation.WarningCodesJSON)
	switch operation.State {
	case model.CodexQuotaOperationStateCompleted, model.CodexQuotaOperationStateFailed:
		return operationResponse(operation), nil
	case model.CodexQuotaOperationStateConsumeStatusUnknown, model.CodexQuotaOperationStateConsuming:
		return s.resumeUnknownConsume(ctx, setup, file, operation, result, warnings)
	case model.CodexQuotaOperationStateUpstreamAccepted, model.CodexQuotaOperationStateVerifying:
		return s.completePostConsume(ctx, setup, file, operation, result, warnings)
	case model.CodexQuotaOperationStateLocallyRecovered:
		return s.finishAfterLocalReset(ctx, setup, file, operation, result, warnings)
	case model.CodexQuotaOperationStatePartialSuccess:
		if operation.Consumed != nil && *operation.Consumed {
			return s.completePostConsume(ctx, setup, file, operation, result, warnings)
		}
		return operationResponse(operation), nil
	default:
		return s.consume(ctx, setup, file, operation, result, warnings)
	}
}

func stableAccountKey(file cpaauthfiles.File) string {
	if value := strings.ToLower(strings.TrimSpace(file.AccountID)); value != "" {
		return "codex:account-id:" + identityHash(value)
	}
	if value := strings.ToLower(strings.TrimSpace(file.AccountSnapshot)); value != "" {
		return "codex:account:" + identityHash(value)
	}
	return "codex:auth-index:" + strings.ToLower(strings.TrimSpace(file.AuthIndex))
}

func identityHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:16])
}

func isUUIDV4(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.Version() == 4
}

func isOperationUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	if err != nil {
		return false
	}
	return parsed.Version() == 4 || parsed.Version() == 5
}

func operationResponse(operation model.CodexQuotaOperation) OperationResponse {
	var result *ResetResult
	if strings.TrimSpace(operation.ResultJSON) != "" {
		decoded := decodeResult(operation.ResultJSON)
		result = &decoded
	}
	return OperationResponse{
		OperationID:    operation.OperationID,
		AccountKey:     operation.AccountKey,
		AuthIndex:      operation.AuthIndex,
		AuthFileName:   operation.AuthFileName,
		State:          operation.State,
		Consumed:       operation.Consumed,
		UpstreamStatus: operation.UpstreamStatus,
		WarningCodes:   decodeWarnings(operation.WarningCodesJSON),
		Result:         result,
		LastError:      operation.LastError,
		CreatedAtMS:    operation.CreatedAtMS,
		UpdatedAtMS:    operation.UpdatedAtMS,
	}
}

func decodeResult(value string) ResetResult {
	var result ResetResult
	_ = json.Unmarshal([]byte(value), &result)
	return result
}

func decodeWarnings(value string) []string {
	warnings := make([]string, 0)
	_ = json.Unmarshal([]byte(value), &warnings)
	return warnings
}

func addWarning(warnings []string, code string) []string {
	for _, existing := range warnings {
		if existing == code {
			return warnings
		}
	}
	return append(warnings, code)
}
