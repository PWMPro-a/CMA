package codexquota

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
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
	}
}

func (s *Service) ResetCredit(ctx context.Context, request ResetRequest) (OperationResponse, error) {
	authIndex := strings.TrimSpace(request.AuthIndex)
	operationID := strings.TrimSpace(request.OperationID)
	if authIndex == "" || !isUUIDV4(operationID) {
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

func (s *Service) GetOperation(ctx context.Context, operationID string) (OperationResponse, error) {
	operationID = strings.TrimSpace(operationID)
	if !isUUIDV4(operationID) {
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
