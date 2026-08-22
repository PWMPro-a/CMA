package codexquota

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpaauthfiles"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

var verificationBackoff = []time.Duration{0, 300 * time.Millisecond, time.Second, 2 * time.Second}

const codexQuotaRecoveryThreshold = 90

func (s *Service) consume(
	ctx context.Context,
	setup store.Setup,
	file cpaauthfiles.File,
	operation model.CodexQuotaOperation,
	result ResetResult,
	warnings []string,
) (OperationResponse, error) {
	if len(result.UsageBefore) == 0 || len(result.ResetCreditsBefore) == 0 {
		usage, credits := s.fetchBefore(ctx, setup, file)
		if len(result.UsageBefore) == 0 {
			if usage.err != nil || !successfulStatus(usage.response.StatusCode) {
				warnings = addWarning(warnings, "usage_before_unavailable")
			} else {
				result.UsageBefore = usage.response.Body
				observed, needsReset := codexUsageLimitState(usage.response.Body, codexQuotaRecoveryThreshold)
				if observed && !needsReset {
					return s.finishAlreadyAvailable(
						ctx,
						setup,
						file,
						operation,
						result,
						addWarning(warnings, "quota_already_available"),
					)
				}
			}
		}
		if len(result.ResetCreditsBefore) == 0 {
			if credits.err != nil || !successfulStatus(credits.response.StatusCode) {
				warnings = addWarning(warnings, "reset_credits_before_unavailable")
			} else {
				result.ResetCreditsBefore = credits.response.Body
			}
		}
	}
	operation.State = model.CodexQuotaOperationStateConsuming
	operation.LastError = ""
	var err error
	operation, claimed, err := s.persistIfState(ctx, operation, result, warnings, model.CodexQuotaOperationStateCreated)
	if err != nil {
		return OperationResponse{}, err
	}
	if !claimed {
		return operationResponse(operation), nil
	}

	consumeResponse, err := s.gateway.consumeResetCredit(
		ctx,
		setup,
		operation.AuthIndex,
		file.AccountID,
		operation.OperationID,
	)
	if err != nil {
		operation.State = model.CodexQuotaOperationStateConsumeStatusUnknown
		operation.Consumed = nil
		operation.LastError = truncate(err.Error(), 2048)
		operation, err = s.persist(ctx, operation, result, addWarning(warnings, "consume_status_unknown"))
		if err != nil {
			return OperationResponse{}, err
		}
		return operationResponse(operation), nil
	}
	status := consumeResponse.StatusCode
	operation.UpstreamStatus = &status
	result.ConsumeResponse = consumeResponse.Body
	if !successfulStatus(status) {
		if status >= 500 || status == 408 {
			operation.State = model.CodexQuotaOperationStateConsumeStatusUnknown
			operation.Consumed = nil
			operation.LastError = fmt.Sprintf("reset-credit consume returned HTTP %d", status)
			operation, err = s.persist(ctx, operation, result, addWarning(warnings, "consume_status_unknown"))
			if err != nil {
				return OperationResponse{}, err
			}
			return operationResponse(operation), nil
		}
		consumed := false
		operation.Consumed = &consumed
		operation.State = model.CodexQuotaOperationStateFailed
		operation.LastError = fmt.Sprintf("reset-credit consume returned HTTP %d", status)
		operation, err = s.persist(ctx, operation, result, warnings)
		if err != nil {
			return OperationResponse{}, err
		}
		return operationResponse(operation), nil
	}
	consumed := true
	operation.Consumed = &consumed
	operation.State = model.CodexQuotaOperationStateUpstreamAccepted
	operation.LastError = ""
	operation, err = s.persist(ctx, operation, result, warnings)
	if err != nil {
		return OperationResponse{}, err
	}
	return s.completePostConsume(ctx, setup, file, operation, result, warnings)
}

func (s *Service) finishAlreadyAvailable(
	ctx context.Context,
	setup store.Setup,
	file cpaauthfiles.File,
	operation model.CodexQuotaOperation,
	result ResetResult,
	warnings []string,
) (OperationResponse, error) {
	consumed := false
	operation.Consumed = &consumed
	result.Verified = true
	result.UsageAfter = result.UsageBefore
	operation.State = model.CodexQuotaOperationStateUpstreamAccepted
	operation.LastError = ""
	var err error
	operation, err = s.persist(ctx, operation, result, warnings)
	if err != nil {
		return OperationResponse{}, err
	}
	return s.completePostConsume(ctx, setup, file, operation, result, warnings)
}

func (s *Service) resumeUnknownConsume(
	ctx context.Context,
	setup store.Setup,
	file cpaauthfiles.File,
	operation model.CodexQuotaOperation,
	result ResetResult,
	warnings []string,
) (OperationResponse, error) {
	verified, attempts, usageAfter, verificationWarning := s.verifyUsage(ctx, setup, file)
	result.VerificationAttempts += attempts
	if len(usageAfter) > 0 {
		result.UsageAfter = usageAfter
	}
	if !verified {
		operation.State = model.CodexQuotaOperationStateConsumeStatusUnknown
		operation.Consumed = nil
		operation.LastError = "reset-credit consumption remains unconfirmed"
		warnings = addWarning(warnings, "consume_status_unknown")
		if verificationWarning != "" {
			warnings = addWarning(warnings, verificationWarning)
		}
		var err error
		operation, err = s.persist(ctx, operation, result, warnings)
		if err != nil {
			return OperationResponse{}, err
		}
		return operationResponse(operation), nil
	}
	consumed := true
	operation.Consumed = &consumed
	operation.State = model.CodexQuotaOperationStateUpstreamAccepted
	operation.LastError = ""
	warnings = addWarning(warnings, "consume_confirmed_by_usage")
	var err error
	operation, err = s.persist(ctx, operation, result, warnings)
	if err != nil {
		return OperationResponse{}, err
	}
	return s.completePostConsume(ctx, setup, file, operation, result, warnings)
}

func (s *Service) completePostConsume(
	ctx context.Context,
	setup store.Setup,
	file cpaauthfiles.File,
	operation model.CodexQuotaOperation,
	result ResetResult,
	warnings []string,
) (OperationResponse, error) {
	operation.State = model.CodexQuotaOperationStateVerifying
	operation.LastError = ""
	var err error
	operation, err = s.persist(ctx, operation, result, warnings)
	if err != nil {
		return OperationResponse{}, err
	}
	verified, attempts, usageAfter, verificationWarning := s.verifyUsage(ctx, setup, file)
	result.VerificationAttempts += attempts
	if len(usageAfter) > 0 {
		result.UsageAfter = usageAfter
	}
	result.Verified = verified
	if !verified {
		operation.State = model.CodexQuotaOperationStatePartialSuccess
		operation.LastError = "upstream quota recovery was not confirmed"
		warnings = addWarning(warnings, verificationWarning)
		operation, err = s.persist(ctx, operation, result, warnings)
		if err != nil {
			return OperationResponse{}, err
		}
		return operationResponse(operation), nil
	}
	localResponse, _, err := s.gateway.resetLocalQuota(ctx, setup, operation.AuthIndex)
	if err != nil {
		operation.State = model.CodexQuotaOperationStatePartialSuccess
		operation.LastError = truncate(err.Error(), 2048)
		operation, err = s.persist(ctx, operation, result, addWarning(warnings, "local_quota_reset_failed"))
		if err != nil {
			return OperationResponse{}, err
		}
		return operationResponse(operation), nil
	}
	result.LocalResetResponse = localResponse
	if err := s.recoverRuntimeQuotaPreempt(ctx, setup, file); err != nil {
		operation.State = model.CodexQuotaOperationStateLocallyRecovered
		operation.LastError = truncate(err.Error(), 2048)
		operation, persistErr := s.persist(ctx, operation, result, addWarning(warnings, "runtime_quota_preempt_recovery_failed"))
		if persistErr != nil {
			return OperationResponse{}, persistErr
		}
		return operationResponse(operation), nil
	}
	operation.State = model.CodexQuotaOperationStateLocallyRecovered
	operation.LastError = ""
	operation, err = s.persist(ctx, operation, result, warnings)
	if err != nil {
		return OperationResponse{}, err
	}
	return s.finishAfterLocalReset(ctx, setup, file, operation, result, warnings)
}

func (s *Service) finishAfterLocalReset(
	ctx context.Context,
	setup store.Setup,
	file cpaauthfiles.File,
	operation model.CodexQuotaOperation,
	result ResetResult,
	warnings []string,
) (OperationResponse, error) {
	// A previous completion attempt may have finished the quota reset but lost
	// the runtime status PATCH. Retry that narrow recovery before finalizing the
	// operation so a transient control-plane error cannot leave the credential
	// disabled indefinitely.
	if err := s.recoverRuntimeQuotaPreempt(ctx, setup, file); err != nil {
		operation.State = model.CodexQuotaOperationStateLocallyRecovered
		operation.LastError = truncate(err.Error(), 2048)
		operation, persistErr := s.persist(ctx, operation, result, addWarning(warnings, "runtime_quota_preempt_recovery_failed"))
		if persistErr != nil {
			return OperationResponse{}, persistErr
		}
		return operationResponse(operation), nil
	}
	credits, err := s.gateway.resetCredits(ctx, setup, operation.AuthIndex, file.AccountID)
	if err != nil || !successfulStatus(credits.StatusCode) {
		warnings = addWarning(warnings, "reset_credits_after_unavailable")
	} else {
		result.ResetCreditsAfter = credits.Body
	}
	if err := s.releaseOwnedQuotaCooldown(ctx, setup, file); err != nil {
		operation.State = model.CodexQuotaOperationStateLocallyRecovered
		operation.LastError = truncate(err.Error(), 2048)
		operation, err = s.persist(ctx, operation, result, addWarning(warnings, "quota_cooldown_release_failed"))
		if err != nil {
			return OperationResponse{}, err
		}
		return operationResponse(operation), nil
	}
	if s.history != nil && operation.Consumed != nil && *operation.Consumed {
		deleted, cleanupErr := s.history.DeleteCredentialHistory(ctx, file.Name, file.AuthIndex)
		if cleanupErr != nil {
			operation.State = model.CodexQuotaOperationStateLocallyRecovered
			operation.LastError = truncate(cleanupErr.Error(), 2048)
			operation, err = s.persist(ctx, operation, result, addWarning(warnings, "history_cleanup_failed"))
			if err != nil {
				return OperationResponse{}, err
			}
			return operationResponse(operation), nil
		}
		result.HistoryDeleted = deleted
	}
	operation.State = model.CodexQuotaOperationStateCompleted
	operation.LastError = ""
	operation, err = s.persist(ctx, operation, result, warnings)
	if err != nil {
		return OperationResponse{}, err
	}
	return operationResponse(operation), nil
}

type beforeFetchResult struct {
	response apiCallResult
	err      error
}

func (s *Service) fetchBefore(ctx context.Context, setup store.Setup, file cpaauthfiles.File) (beforeFetchResult, beforeFetchResult) {
	usageChannel := make(chan beforeFetchResult, 1)
	creditsChannel := make(chan beforeFetchResult, 1)
	go func() {
		response, err := s.gateway.usage(ctx, setup, file.AuthIndex, file.AccountID)
		usageChannel <- beforeFetchResult{response: response, err: err}
	}()
	go func() {
		response, err := s.gateway.resetCredits(ctx, setup, file.AuthIndex, file.AccountID)
		creditsChannel <- beforeFetchResult{response: response, err: err}
	}()
	return <-usageChannel, <-creditsChannel
}

func (s *Service) verifyUsage(ctx context.Context, setup store.Setup, file cpaauthfiles.File) (bool, int, json.RawMessage, string) {
	attempts := 0
	var latest json.RawMessage
	hadResponse := false
	for _, delay := range verificationBackoff {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return false, attempts, latest, "usage_verification_failed"
			case <-timer.C:
			}
		}
		response, err := s.gateway.usage(ctx, setup, file.AuthIndex, file.AccountID)
		attempts++
		if err != nil || !successfulStatus(response.StatusCode) {
			continue
		}
		hadResponse = true
		latest = response.Body
		observed, limited := codexUsageLimitState(response.Body, codexQuotaRecoveryThreshold)
		if observed && !limited {
			return true, attempts, latest, ""
		}
	}
	if hadResponse {
		return false, attempts, latest, "usage_recovery_unconfirmed"
	}
	return false, attempts, latest, "usage_verification_failed"
}

func (s *Service) persist(
	ctx context.Context,
	operation model.CodexQuotaOperation,
	result ResetResult,
	warnings []string,
) (model.CodexQuotaOperation, error) {
	resultJSON, _ := json.Marshal(result)
	warningsJSON, _ := json.Marshal(warnings)
	operation.ResultJSON = string(resultJSON)
	operation.WarningCodesJSON = string(warningsJSON)
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
	defer cancel()
	updated, err := s.operations.Update(persistCtx, operation)
	if err != nil {
		return operation, err
	}
	return updated, nil
}

func (s *Service) persistIfState(
	ctx context.Context,
	operation model.CodexQuotaOperation,
	result ResetResult,
	warnings []string,
	expectedState string,
) (model.CodexQuotaOperation, bool, error) {
	resultJSON, _ := json.Marshal(result)
	warningsJSON, _ := json.Marshal(warnings)
	operation.ResultJSON = string(resultJSON)
	operation.WarningCodesJSON = string(warningsJSON)
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
	defer cancel()
	return s.operations.UpdateIfState(persistCtx, operation, expectedState)
}

func successfulStatus(status int) bool {
	return status >= 200 && status < 300
}

func codexUsageLimitState(body json.RawMessage, recoveryThreshold float64) (bool, bool) {
	var payload map[string]any
	if len(body) == 0 || json.Unmarshal(body, &payload) != nil {
		return false, false
	}
	observed := false
	limited := false
	for _, key := range []string{"rate_limit", "rateLimit", "code_review_rate_limit", "codeReviewRateLimit"} {
		if value, ok := payload[key].(map[string]any); ok {
			observed = true
			limited = limited || rateLimitReached(value, recoveryThreshold)
		}
	}
	for _, key := range []string{"additional_rate_limits", "additionalRateLimits"} {
		items, _ := payload[key].([]any)
		for _, item := range items {
			record, _ := item.(map[string]any)
			for _, childKey := range []string{"rate_limit", "rateLimit"} {
				if child, ok := record[childKey].(map[string]any); ok {
					observed = true
					limited = limited || rateLimitReached(child, recoveryThreshold)
				}
			}
		}
	}
	return observed, limited
}

func rateLimitReached(limit map[string]any, recoveryThreshold float64) bool {
	allowed, hasAllowed := boolValue(limit["allowed"])
	if hasAllowed && !allowed {
		return true
	}
	var limitReached bool
	var hasLimitReached bool
	for _, key := range []string{"limit_reached", "limitReached"} {
		if reached, ok := boolValue(limit[key]); ok {
			limitReached = reached
			hasLimitReached = true
			if reached {
				return true
			}
		}
	}
	// The usage endpoint can report a rounded 100% window while explicitly
	// confirming that requests are still allowed and the limit is not reached.
	// Trust those explicit recovery signals over the percentage fallback.
	if hasAllowed && allowed && hasLimitReached && !limitReached {
		return false
	}
	for _, key := range []string{"primary_window", "primaryWindow", "secondary_window", "secondaryWindow"} {
		window, _ := limit[key].(map[string]any)
		for _, usedKey := range []string{"used_percent", "usedPercent"} {
			if used, ok := numberValue(window[usedKey]); ok && used >= recoveryThreshold {
				return true
			}
		}
	}
	return false
}

func boolValue(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(typed))
		return parsed, err == nil
	case float64:
		return typed != 0, true
	default:
		return false, false
	}
}

func numberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case json.Number:
		trimmed := strings.TrimSuffix(strings.TrimSpace(typed.String()), "%")
		parsed, err := strconv.ParseFloat(trimmed, 64)
		return parsed, err == nil
	case float64:
		return typed, true
	case string:
		trimmed := strings.TrimSuffix(strings.TrimSpace(typed), "%")
		parsed, err := strconv.ParseFloat(trimmed, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}
