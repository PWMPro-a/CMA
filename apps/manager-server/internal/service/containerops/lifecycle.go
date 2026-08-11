package containerops

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

const (
	lifecycleStatusIdle       = "idle"
	lifecycleStatusInProgress = "in_progress"
	lifecycleStatusCompleted  = "completed"
	lifecycleStatusFailed     = "failed"
	lifecycleStatusBlocked    = "blocked"
)

type LifecycleBusyError struct {
	Active model.ContainerOpsLifecycleState
}

func (err *LifecycleBusyError) Error() string {
	return fmt.Sprintf("container ops lifecycle operation %s is already %s", err.Active.OperationID, err.Active.Status)
}

func IsLifecycleBusy(err error) bool {
	var busy *LifecycleBusyError
	return errors.As(err, &busy)
}

func (s *Service) currentLifecycle() model.ContainerOpsLifecycleState {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return normalizeLifecycleState(s.lifecycle)
}

func (s *Service) beginLifecycle(ctx context.Context, operation string, phase string, agent model.ContainerOpsAgentInfo, request any) (model.ContainerOpsLifecycleState, func(status string, phase string, message string, result any) model.ContainerOpsLifecycleState, error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.lifecycle.Active {
		return model.ContainerOpsLifecycleState{}, nil, &LifecycleBusyError{Active: normalizeLifecycleState(s.lifecycle)}
	}

	s.lifecycleSeq++
	now := s.now().UTC()
	state := model.ContainerOpsLifecycleState{
		OperationID: fmt.Sprintf("cpa-%s-%d", safeLifecycleToken(operation), s.lifecycleSeq),
		Operation:   operation,
		Phase:       phase,
		Status:      lifecycleStatusInProgress,
		Active:      true,
		StartedAt:   now.Unix(),
		Message:     "Operation started.",
	}
	s.lifecycle = state
	if err := s.createLifecycleAudit(ctx, state, agent, request, now.UnixMilli()); err != nil {
		state.Status = lifecycleStatusFailed
		state.Active = false
		state.FinishedAt = s.now().UTC().Unix()
		state.Message = err.Error()
		s.lifecycle = state
		return model.ContainerOpsLifecycleState{}, nil, err
	}

	finish := func(status string, nextPhase string, message string, result any) model.ContainerOpsLifecycleState {
		return s.finishLifecycle(ctx, state.OperationID, status, nextPhase, message, agent, result)
	}
	return state, finish, nil
}

func (s *Service) finishLifecycle(ctx context.Context, operationID string, status string, phase string, message string, agent model.ContainerOpsAgentInfo, result any) model.ContainerOpsLifecycleState {
	s.lifecycleMu.Lock()

	state := normalizeLifecycleState(s.lifecycle)
	if state.OperationID != operationID {
		s.lifecycleMu.Unlock()
		return state
	}
	if strings.TrimSpace(status) == "" {
		status = lifecycleStatusCompleted
	}
	if strings.TrimSpace(phase) != "" {
		state.Phase = phase
	}
	state.Status = status
	state.Active = false
	state.FinishedAt = s.now().UTC().Unix()
	state.Message = strings.TrimSpace(message)
	s.lifecycle = state
	s.lifecycleMu.Unlock()
	s.updateLifecycleAudit(ctx, state, agent, result)
	return state
}

func normalizeLifecycleState(state model.ContainerOpsLifecycleState) model.ContainerOpsLifecycleState {
	if strings.TrimSpace(state.Status) == "" {
		state.Status = lifecycleStatusIdle
	}
	return state
}

func lifecycleStatusForResult(status string) string {
	value := strings.ToLower(strings.TrimSpace(status))
	switch {
	case value == "":
		return lifecycleStatusCompleted
	case value == "blocked":
		return lifecycleStatusBlocked
	case strings.Contains(value, "failed"):
		return lifecycleStatusFailed
	default:
		return lifecycleStatusCompleted
	}
}

func safeLifecycleToken(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return "operation"
	}
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			builder.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "operation"
	}
	return result
}

func attachLifecycle(state model.ContainerOpsLifecycleState) *model.ContainerOpsLifecycleState {
	state = normalizeLifecycleState(state)
	return &state
}

func (s *Service) createLifecycleAudit(ctx context.Context, state model.ContainerOpsLifecycleState, agent model.ContainerOpsAgentInfo, request any, startedAtMS int64) error {
	if s.auditStore == nil {
		return nil
	}
	_, err := s.auditStore.CreateContainerOpsAudit(context.WithoutCancel(ctx), model.ContainerOpsAuditEntry{
		OperationID:  state.OperationID,
		Operation:    state.Operation,
		Phase:        state.Phase,
		Status:       state.Status,
		BackupID:     auditBackupID(request),
		AgentBaseURL: agent.BaseURL,
		Message:      state.Message,
		Request:      auditRequestSummary(request),
		StartedAtMS:  startedAtMS,
		CreatedAtMS:  startedAtMS,
		UpdatedAtMS:  startedAtMS,
	})
	if err != nil {
		return fmt.Errorf("create container ops audit: %w", err)
	}
	return nil
}

func (s *Service) updateLifecycleAudit(ctx context.Context, state model.ContainerOpsLifecycleState, agent model.ContainerOpsAgentInfo, result any) {
	if s.auditStore == nil {
		return
	}
	startedAtMS := state.StartedAt * 1000
	finishedAtMS := state.FinishedAt * 1000
	_ = s.auditStore.UpdateContainerOpsAudit(context.WithoutCancel(ctx), model.ContainerOpsAuditEntry{
		OperationID:  state.OperationID,
		Operation:    state.Operation,
		Phase:        state.Phase,
		Status:       state.Status,
		BackupID:     firstNonEmpty(auditBackupID(result), auditBackupID(state)),
		AgentBaseURL: agent.BaseURL,
		Message:      state.Message,
		Error:        auditError(state, result),
		Result:       auditResultSummary(result),
		StartedAtMS:  startedAtMS,
		FinishedAtMS: finishedAtMS,
		DurationMS:   finishedAtMS - startedAtMS,
	})
}

func auditRequestSummary(request any) any {
	switch value := request.(type) {
	case model.ContainerOpsDeployRequest:
		return map[string]any{"apply": value.Apply, "action": value.Action}
	case model.ContainerOpsRestoreRequest:
		return map[string]any{"backupId": value.BackupID, "apply": value.Apply}
	case model.ContainerOpsRollbackRequest:
		return map[string]any{"backupId": value.BackupID}
	case model.ContainerOpsNetworkStandardizeRequest:
		return map[string]any{"backupId": value.BackupID, "apply": value.Apply}
	case model.ContainerOpsUpgradeRequest:
		return map[string]any{"apply": value.Apply, "cpaImage": value.CPAImage, "cpampImage": value.CPAMPImage}
	case model.ContainerOpsUpgradeTaskStartRequest:
		return map[string]any{"taskId": value.TaskID}
	case model.ContainerOpsSourceIPRequest:
		return map[string]any{"sourceIp": value.SourceIP, "interface": value.Interface}
	case map[string]any:
		return value
	default:
		return nil
	}
}

func auditResultSummary(result any) any {
	switch value := result.(type) {
	case model.ContainerOpsBackupResult:
		return map[string]any{
			"backupId":     value.BackupID,
			"status":       value.Status,
			"archiveCount": len(value.Archives),
			"warningCount": len(value.Warnings),
			"readOnly":     value.ReadOnly,
		}
	case model.ContainerOpsDeployPlan:
		return map[string]any{
			"status":         value.Status,
			"applied":        value.Applied,
			"fileCount":      len(value.Files),
			"imagePullCount": len(value.ImagePulls),
			"actionCount":    len(value.Actions),
			"blockingChecks": deployBlockingCount(value.Checks),
		}
	case model.ContainerOpsRestorePlan:
		rollbackBackupID := ""
		if value.RollbackBackup != nil {
			rollbackBackupID = value.RollbackBackup.BackupID
		}
		return map[string]any{
			"backupId":         value.BackupID,
			"status":           value.Status,
			"applied":          value.Applied,
			"rollbackBackupId": rollbackBackupID,
			"actionCount":      len(value.Actions),
			"blockingChecks":   restoreBlockingCount(value.Checks),
		}
	case model.ContainerOpsNetworkStandardizeResult:
		return map[string]any{
			"backupId":       value.BackupID,
			"status":         value.Status,
			"network":        value.Network,
			"applied":        value.Applied,
			"actionCount":    len(value.Actions),
			"blockingChecks": networkBlockingCount(value.Checks),
		}
	case model.ContainerOpsSourceIPResult:
		return map[string]any{
			"sourceIp":       value.SourceIP,
			"interface":      value.Interface,
			"status":         value.Status,
			"mounted":        value.Mounted,
			"alreadyPresent": value.AlreadyPresent,
			"removed":        value.Removed,
			"outboundIp":     value.OutboundIP,
			"blockingChecks": egressBlockingCount(value.Checks),
		}
	case model.ContainerOpsUpgradePlan:
		rollbackBackupID := ""
		if value.RollbackBackup != nil {
			rollbackBackupID = value.RollbackBackup.BackupID
		}
		return map[string]any{
			"status":           value.Status,
			"cpaImage":         value.CPAImage,
			"cpampImage":       value.CPAMPImage,
			"applied":          value.Applied,
			"rollbackBackupId": rollbackBackupID,
			"imagePullCount":   len(value.ImagePulls),
			"actionCount":      len(value.Actions),
			"blockingChecks":   upgradeBlockingCount(value.Checks),
		}
	case model.ContainerOpsUpgradeJob:
		rollbackBackupID := value.RollbackBackupID
		if rollbackBackupID == "" && value.Plan != nil && value.Plan.RollbackBackup != nil {
			rollbackBackupID = value.Plan.RollbackBackup.BackupID
		}
		return map[string]any{
			"agentJobId":       value.JobID,
			"taskId":           value.TaskID,
			"status":           value.Status,
			"phase":            value.Phase,
			"cpaImage":         value.CPAImage,
			"cpampImage":       value.CPAMPImage,
			"rollbackBackupId": rollbackBackupID,
			"actionCount":      len(value.Actions),
			"blockingChecks":   upgradeBlockingCount(value.Checks),
		}
	case map[string]any:
		return value
	default:
		return nil
	}
}

func auditBackupID(value any) string {
	switch typed := value.(type) {
	case model.ContainerOpsRestoreRequest:
		return strings.TrimSpace(typed.BackupID)
	case model.ContainerOpsRollbackRequest:
		return strings.TrimSpace(typed.BackupID)
	case model.ContainerOpsNetworkStandardizeRequest:
		return strings.TrimSpace(typed.BackupID)
	case model.ContainerOpsBackupResult:
		return strings.TrimSpace(typed.BackupID)
	case model.ContainerOpsRestorePlan:
		return strings.TrimSpace(typed.BackupID)
	case model.ContainerOpsNetworkStandardizeResult:
		return strings.TrimSpace(typed.BackupID)
	case model.ContainerOpsUpgradePlan:
		if typed.RollbackBackup != nil {
			return strings.TrimSpace(typed.RollbackBackup.BackupID)
		}
		return ""
	case model.ContainerOpsUpgradeJob:
		if typed.RollbackBackupID != "" {
			return strings.TrimSpace(typed.RollbackBackupID)
		}
		if typed.Plan != nil && typed.Plan.RollbackBackup != nil {
			return strings.TrimSpace(typed.Plan.RollbackBackup.BackupID)
		}
		return ""
	case model.ContainerOpsLifecycleState:
		return ""
	default:
		return ""
	}
}

func auditError(state model.ContainerOpsLifecycleState, result any) string {
	if state.Status != lifecycleStatusFailed {
		return ""
	}
	if value, ok := result.(map[string]any); ok {
		if text, ok := value["error"].(string); ok {
			return text
		}
	}
	return state.Message
}

func restoreBlockingCount(checks []model.ContainerOpsRestoreCheck) int {
	count := 0
	for _, check := range checks {
		if check.Blocking {
			count++
		}
	}
	return count
}

func networkBlockingCount(checks []model.ContainerOpsNetworkCheck) int {
	count := 0
	for _, check := range checks {
		if check.Blocking {
			count++
		}
	}
	return count
}

func egressBlockingCount(checks []model.ContainerOpsEgressCheck) int {
	count := 0
	for _, check := range checks {
		if check.Blocking {
			count++
		}
	}
	return count
}
