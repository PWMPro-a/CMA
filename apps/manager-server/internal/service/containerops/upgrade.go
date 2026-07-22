package containerops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

const (
	upgradeAgentJobPollInterval = 100 * time.Millisecond
	upgradeAgentJobMaxPolls     = 6000
)

func (s *Service) Upgrade(ctx context.Context, request model.ContainerOpsUpgradeRequest) (model.ContainerOpsUpgradePlan, error) {
	agent := s.agentInfo(ctx)
	if !agent.Configured {
		return model.ContainerOpsUpgradePlan{}, errors.New("container ops agent is not configured")
	}
	if !agent.Reachable {
		return model.ContainerOpsUpgradePlan{}, fmt.Errorf("container ops agent is not reachable: %s", firstNonEmpty(agent.Error, "unknown error"))
	}

	request.CPAImage = strings.TrimSpace(request.CPAImage)
	request.CPAMPImage = strings.TrimSpace(request.CPAMPImage)

	endpoint := "/upgrades/cpa/plan"
	action := "create CPA stack upgrade plan"
	var task *model.ContainerOpsUpgradeTask
	var finish func(status string, phase string, message string, result any) model.ContainerOpsLifecycleState
	if request.Apply {
		state, finishLifecycle, err := s.beginLifecycle(ctx, "upgrade_prepare", "prepare", agent, request)
		if err != nil {
			return model.ContainerOpsUpgradePlan{}, err
		}
		finish = finishLifecycle
		task, err = s.createUpgradeTask(ctx, state, agent, request)
		if err != nil {
			finish(lifecycleStatusFailed, "task_create_failed", err.Error(), map[string]any{"error": err.Error()})
			return model.ContainerOpsUpgradePlan{}, err
		}
		endpoint = "/upgrades/cpa/prepare"
		action = "prepare CPA stack upgrade"
	}

	var plan model.ContainerOpsUpgradePlan
	if err := s.postAgentJSON(ctx, endpoint, request, &plan); err != nil {
		if finish != nil {
			if task != nil {
				failed := s.failUpgradeTask(ctx, *task, agent, err.Error())
				task = &failed
			}
			finish(lifecycleStatusFailed, "agent_request_failed", err.Error(), map[string]any{"error": err.Error()})
		}
		return model.ContainerOpsUpgradePlan{}, fmt.Errorf("%s: %w", action, err)
	}
	plan.Agent = agent
	if finish != nil {
		plan.Lifecycle = attachLifecycle(finish(lifecycleStatusForResult(plan.Status), plan.Status, "CPA stack upgrade preparation finished with status "+plan.Status+".", plan))
	}
	if task != nil {
		next := s.finishUpgradeTask(ctx, *task, agent, plan)
		plan.Task = &next
	}
	return plan, nil
}

func (s *Service) StartUpgradeTask(ctx context.Context, request model.ContainerOpsUpgradeTaskStartRequest) (model.ContainerOpsUpgradeTask, error) {
	agent := s.agentInfo(ctx)
	if !agent.Configured {
		return model.ContainerOpsUpgradeTask{}, errors.New("container ops agent is not configured")
	}
	if !agent.Reachable {
		return model.ContainerOpsUpgradeTask{}, fmt.Errorf("container ops agent is not reachable: %s", firstNonEmpty(agent.Error, "unknown error"))
	}
	if s.auditStore == nil {
		return model.ContainerOpsUpgradeTask{}, errors.New("container ops upgrade task store is not configured")
	}

	request.TaskID = strings.TrimSpace(request.TaskID)
	if request.TaskID == "" {
		return model.ContainerOpsUpgradeTask{}, errors.New("taskId is required")
	}
	task, ok, err := s.auditStore.GetContainerOpsUpgradeTask(ctx, request.TaskID)
	if err != nil {
		return model.ContainerOpsUpgradeTask{}, fmt.Errorf("load container ops upgrade task: %w", err)
	}
	if !ok {
		return model.ContainerOpsUpgradeTask{}, errors.New("container ops upgrade task not found")
	}
	if !upgradeTaskStartable(task) {
		return model.ContainerOpsUpgradeTask{}, fmt.Errorf("container ops upgrade task %s is not ready to start async recreate", task.TaskID)
	}

	state, finishLifecycle, err := s.beginLifecycle(ctx, "upgrade_async", "async_recreate", agent, request)
	if err != nil {
		return model.ContainerOpsUpgradeTask{}, err
	}
	var job model.ContainerOpsUpgradeJob
	if err := s.postAgentJSON(ctx, "/upgrades/cpa/jobs", model.ContainerOpsUpgradeJobStartRequest{
		TaskID:           task.TaskID,
		CPAImage:         task.CPAImage,
		CPAMPImage:       task.CPAMPImage,
		RollbackBackupID: task.RollbackBackupID,
	}, &job); err != nil {
		finishLifecycle(lifecycleStatusFailed, "agent_job_start_failed", err.Error(), map[string]any{"error": err.Error()})
		return model.ContainerOpsUpgradeTask{}, fmt.Errorf("start agent upgrade job: %w", err)
	}
	if strings.TrimSpace(job.JobID) == "" {
		err := errors.New("agent upgrade job id is required")
		finishLifecycle(lifecycleStatusFailed, "agent_job_start_failed", err.Error(), map[string]any{"error": err.Error()})
		return model.ContainerOpsUpgradeTask{}, err
	}
	task.OperationID = state.OperationID
	task.Status = "running"
	task.Phase = "async_recreate"
	task.AgentBaseURL = agent.BaseURL
	task.Message = "Asynchronous upgrade task started."
	task.Error = ""
	task.NextAction = "wait_for_async_result"
	task.Result = map[string]any{
		"status":           task.Status,
		"agentJobId":       job.JobID,
		"agentJobStatus":   job.Status,
		"rollbackBackupId": task.RollbackBackupID,
	}
	task.FinishedAtMS = 0
	if err := s.auditStore.UpdateContainerOpsUpgradeTask(context.WithoutCancel(ctx), task); err != nil {
		finishLifecycle(lifecycleStatusFailed, "task_start_failed", err.Error(), map[string]any{"error": err.Error()})
		return model.ContainerOpsUpgradeTask{}, fmt.Errorf("start container ops upgrade task: %w", err)
	}

	go s.runAsyncUpgradeTask(context.Background(), task, agent, job, finishLifecycle)
	return task, nil
}

func (s *Service) runAsyncUpgradeTask(ctx context.Context, task model.ContainerOpsUpgradeTask, agent model.ContainerOpsAgentInfo, job model.ContainerOpsUpgradeJob, finish func(status string, phase string, message string, result any) model.ContainerOpsLifecycleState) {
	finalJob, err := s.waitForUpgradeJob(ctx, job)
	if err != nil {
		failed := s.failAsyncUpgradeTask(ctx, task, agent, err.Error(), &finalJob)
		finish(lifecycleStatusFailed, failed.Phase, failed.Message, failed)
		return
	}
	plan, ok := upgradeJobPlan(finalJob)
	if !ok {
		message := firstNonEmpty(finalJob.Error, finalJob.Message, "agent upgrade job finished without a plan")
		failed := s.failAsyncUpgradeTask(ctx, task, agent, message, &finalJob)
		finish(lifecycleStatusFailed, failed.Phase, failed.Message, failed)
		return
	}
	plan.Agent = agent
	next := s.finishAsyncUpgradeTask(ctx, task, agent, finalJob, plan)
	finish(lifecycleStatusForAsyncUpgradeTask(next.Status), next.Phase, next.Message, finalJob)
}

func (s *Service) waitForUpgradeJob(ctx context.Context, job model.ContainerOpsUpgradeJob) (model.ContainerOpsUpgradeJob, error) {
	if upgradeJobTerminal(job.Status) {
		return job, nil
	}
	for attempt := 0; attempt < upgradeAgentJobMaxPolls; attempt++ {
		var next model.ContainerOpsUpgradeJob
		if err := s.getAgentJSON(ctx, "/upgrades/cpa/jobs/"+strings.TrimSpace(job.JobID), &next); err != nil {
			return job, fmt.Errorf("poll agent upgrade job %s: %w", job.JobID, err)
		}
		if strings.TrimSpace(next.JobID) == "" {
			next.JobID = job.JobID
		}
		job = next
		if upgradeJobTerminal(job.Status) {
			return job, nil
		}
		time.Sleep(upgradeAgentJobPollInterval)
	}
	return job, fmt.Errorf("agent upgrade job %s did not finish within poll budget", job.JobID)
}

func upgradeJobTerminal(status string) bool {
	value := strings.ToLower(strings.TrimSpace(status))
	return value == "completed" ||
		value == "recreate_deferred" ||
		value == "blocked" ||
		value == "failed" ||
		strings.Contains(value, "failed")
}

func upgradeJobPlan(job model.ContainerOpsUpgradeJob) (model.ContainerOpsUpgradePlan, bool) {
	if job.Plan != nil {
		return *job.Plan, true
	}
	if strings.EqualFold(strings.TrimSpace(job.Status), "failed") {
		return model.ContainerOpsUpgradePlan{}, false
	}
	status := strings.TrimSpace(job.Status)
	if status == "" {
		return model.ContainerOpsUpgradePlan{}, false
	}
	return model.ContainerOpsUpgradePlan{
		Status:      status,
		CPAImage:    job.CPAImage,
		CPAMPImage:  job.CPAMPImage,
		Checks:      job.Checks,
		Actions:     job.Actions,
		Applied:     false,
		Destructive: true,
		ReadOnly:    false,
	}, true
}

func (s *Service) finishAsyncUpgradeTask(ctx context.Context, task model.ContainerOpsUpgradeTask, agent model.ContainerOpsAgentInfo, job model.ContainerOpsUpgradeJob, plan model.ContainerOpsUpgradePlan) model.ContainerOpsUpgradeTask {
	status := asyncUpgradeTaskStatus(plan.Status)
	task.Status = status
	task.Phase = asyncUpgradeTaskPhase(status)
	task.CPAImage = plan.CPAImage
	task.CPAMPImage = plan.CPAMPImage
	task.AgentBaseURL = agent.BaseURL
	task.Message = asyncUpgradeTaskMessage(status, plan.Status)
	task.Error = asyncUpgradeTaskError(status, plan.Status)
	task.NextAction = asyncUpgradeTaskNextAction(status)
	task.Result = asyncUpgradeTaskResult(plan, job, task.RollbackBackupID)
	task.FinishedAtMS = s.now().UTC().UnixMilli()
	if plan.RollbackBackup != nil && task.RollbackBackupID == "" {
		task.RollbackBackupID = plan.RollbackBackup.BackupID
	}
	if s.auditStore != nil {
		_ = s.auditStore.UpdateContainerOpsUpgradeTask(context.WithoutCancel(ctx), task)
	}
	return task
}

func asyncUpgradeTaskResult(plan model.ContainerOpsUpgradePlan, job model.ContainerOpsUpgradeJob, rollbackBackupID string) map[string]any {
	result, ok := auditResultSummary(plan).(map[string]any)
	if !ok {
		result = map[string]any{"status": plan.Status}
	}
	result["agentJobId"] = job.JobID
	result["agentJobStatus"] = job.Status
	result["agentJobPhase"] = job.Phase
	if result["rollbackBackupId"] == "" && rollbackBackupID != "" {
		result["rollbackBackupId"] = rollbackBackupID
	}
	return result
}

func (s *Service) failAsyncUpgradeTask(ctx context.Context, task model.ContainerOpsUpgradeTask, agent model.ContainerOpsAgentInfo, message string, job *model.ContainerOpsUpgradeJob) model.ContainerOpsUpgradeTask {
	task.Status = "failed"
	task.Phase = "async_recreate_failed"
	task.AgentBaseURL = agent.BaseURL
	task.Message = "Asynchronous upgrade task failed before the agent returned a plan."
	task.Error = strings.TrimSpace(message)
	task.NextAction = "review_failure"
	result := map[string]any{"error": task.Error}
	if job != nil {
		result["agentJobId"] = job.JobID
		result["agentJobStatus"] = job.Status
	}
	task.Result = result
	task.FinishedAtMS = s.now().UTC().UnixMilli()
	if s.auditStore != nil {
		_ = s.auditStore.UpdateContainerOpsUpgradeTask(context.WithoutCancel(ctx), task)
	}
	return task
}

func upgradeBlockingCount(checks []model.ContainerOpsUpgradeCheck) int {
	count := 0
	for _, check := range checks {
		if check.Blocking {
			count++
		}
	}
	return count
}

func (s *Service) createUpgradeTask(ctx context.Context, state model.ContainerOpsLifecycleState, agent model.ContainerOpsAgentInfo, request model.ContainerOpsUpgradeRequest) (*model.ContainerOpsUpgradeTask, error) {
	if s.auditStore == nil {
		return nil, nil
	}
	startedAtMS := state.StartedAt * 1000
	task, err := s.auditStore.CreateContainerOpsUpgradeTask(context.WithoutCancel(ctx), model.ContainerOpsUpgradeTask{
		TaskID:       state.OperationID,
		OperationID:  state.OperationID,
		Status:       "preparing",
		Phase:        "prepare",
		CPAImage:     request.CPAImage,
		CPAMPImage:   request.CPAMPImage,
		AgentBaseURL: agent.BaseURL,
		Message:      "Upgrade preparation started.",
		NextAction:   "wait_for_prepare",
		Request:      auditRequestSummary(request),
		StartedAtMS:  startedAtMS,
		CreatedAtMS:  startedAtMS,
		UpdatedAtMS:  startedAtMS,
	})
	if err != nil {
		return nil, fmt.Errorf("create container ops upgrade task: %w", err)
	}
	return &task, nil
}

func (s *Service) finishUpgradeTask(ctx context.Context, task model.ContainerOpsUpgradeTask, agent model.ContainerOpsAgentInfo, plan model.ContainerOpsUpgradePlan) model.ContainerOpsUpgradeTask {
	status := upgradeTaskStatus(plan.Status)
	task.Status = status
	task.Phase = upgradeTaskPhase(status)
	task.CPAImage = plan.CPAImage
	task.CPAMPImage = plan.CPAMPImage
	task.AgentBaseURL = agent.BaseURL
	task.Message = upgradeTaskMessage(status, plan.Status)
	task.Error = upgradeTaskError(status, plan.Status)
	task.NextAction = upgradeTaskNextAction(status)
	task.Result = auditResultSummary(plan)
	task.FinishedAtMS = s.now().UTC().UnixMilli()
	if plan.RollbackBackup != nil {
		task.RollbackBackupID = plan.RollbackBackup.BackupID
	}
	if s.auditStore != nil {
		_ = s.auditStore.UpdateContainerOpsUpgradeTask(context.WithoutCancel(ctx), task)
	}
	return task
}

func (s *Service) failUpgradeTask(ctx context.Context, task model.ContainerOpsUpgradeTask, agent model.ContainerOpsAgentInfo, message string) model.ContainerOpsUpgradeTask {
	task.Status = "failed"
	task.Phase = "prepare_failed"
	task.AgentBaseURL = agent.BaseURL
	task.Message = "Upgrade preparation failed before the agent returned a plan."
	task.Error = strings.TrimSpace(message)
	task.NextAction = "review_failure"
	task.Result = map[string]any{"error": task.Error}
	task.FinishedAtMS = s.now().UTC().UnixMilli()
	if s.auditStore != nil {
		_ = s.auditStore.UpdateContainerOpsUpgradeTask(context.WithoutCancel(ctx), task)
	}
	return task
}

func upgradeTaskStatus(planStatus string) string {
	value := strings.ToLower(strings.TrimSpace(planStatus))
	switch {
	case value == "prepared":
		return "prepared"
	case value == "blocked":
		return "blocked"
	case strings.Contains(value, "failed"):
		return "failed"
	default:
		return "prepare_review"
	}
}

func upgradeTaskPhase(status string) string {
	switch status {
	case "prepared":
		return "prepare_completed"
	case "blocked":
		return "prepare_blocked"
	case "failed":
		return "prepare_failed"
	default:
		return "prepare_review"
	}
}

func upgradeTaskNextAction(status string) string {
	switch status {
	case "prepared":
		return "start_async_recreate"
	case "blocked":
		return "fix_blocking_checks"
	case "failed":
		return "review_failure"
	default:
		return "review_prepare_result"
	}
}

func upgradeTaskMessage(status string, planStatus string) string {
	switch status {
	case "prepared":
		return "Upgrade preparation completed. Container recreate is deferred to the asynchronous upgrade phase."
	case "blocked":
		return "Upgrade preparation was blocked by preflight checks."
	case "failed":
		return "Upgrade preparation failed."
	default:
		return "Upgrade preparation returned status " + planStatus + "."
	}
}

func upgradeTaskError(status string, planStatus string) string {
	if status != "failed" {
		return ""
	}
	return "upgrade preparation returned status " + planStatus
}

func upgradeTaskStartable(task model.ContainerOpsUpgradeTask) bool {
	return strings.EqualFold(strings.TrimSpace(task.Status), "prepared") &&
		strings.EqualFold(strings.TrimSpace(task.NextAction), "start_async_recreate")
}

func asyncUpgradeTaskStatus(planStatus string) string {
	value := strings.ToLower(strings.TrimSpace(planStatus))
	switch {
	case value == "upgraded" || value == "completed":
		return "completed"
	case value == "recreate_deferred":
		return "recreate_deferred"
	case value == "blocked":
		return "blocked"
	case strings.Contains(value, "failed"):
		return "failed"
	default:
		return "review_async_result"
	}
}

func asyncUpgradeTaskPhase(status string) string {
	switch status {
	case "completed":
		return "healthcheck_completed"
	case "recreate_deferred":
		return "async_recreate_deferred"
	case "blocked":
		return "async_recreate_blocked"
	case "failed":
		return "async_recreate_failed"
	default:
		return "async_recreate_review"
	}
}

func asyncUpgradeTaskNextAction(status string) string {
	switch status {
	case "completed":
		return "review_upgrade_result"
	case "recreate_deferred":
		return "implement_agent_recreate"
	case "blocked":
		return "fix_blocking_checks"
	case "failed":
		return "review_failure"
	default:
		return "review_async_result"
	}
}

func asyncUpgradeTaskMessage(status string, planStatus string) string {
	switch status {
	case "completed":
		return "Asynchronous upgrade task completed."
	case "recreate_deferred":
		return "Asynchronous upgrade control plane is ready; agent-side container recreate remains deferred."
	case "blocked":
		return "Asynchronous upgrade task was blocked by agent checks."
	case "failed":
		return "Asynchronous upgrade task failed."
	default:
		return "Asynchronous upgrade task returned status " + planStatus + "."
	}
}

func asyncUpgradeTaskError(status string, planStatus string) string {
	if status != "failed" {
		return ""
	}
	return "asynchronous upgrade returned status " + planStatus
}

func lifecycleStatusForAsyncUpgradeTask(status string) string {
	switch status {
	case "failed":
		return lifecycleStatusFailed
	case "blocked":
		return lifecycleStatusBlocked
	default:
		return lifecycleStatusCompleted
	}
}
