package containeropsagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/response"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

const (
	defaultCPAUpgradeImage   = "seakee/cli-proxy-api:latest"
	defaultCPAMPUpgradeImage = "seakee/cpa-manager-plus:latest"
)

type UpgradeOptions struct {
	BackupRoot       string
	RollbackBackupID string
	Request          model.ContainerOpsUpgradeRequest
	Now              time.Time
}

func (s *Server) upgradeCPAPlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.MethodNotAllowed(w)
		return
	}
	var request model.ContainerOpsUpgradeRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		response.Error(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.docker.UpgradeCPAPlan(r.Context(), UpgradeOptions{
		BackupRoot: s.backupRoot,
		Request:    request,
	})
	if err != nil {
		response.Error(w, http.StatusBadGateway, err)
		return
	}
	response.JSON(w, http.StatusOK, result)
}

func (s *Server) upgradeCPAPrepare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.MethodNotAllowed(w)
		return
	}
	var request model.ContainerOpsUpgradeRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		response.Error(w, http.StatusBadRequest, err)
		return
	}
	request.Apply = true
	result, err := s.docker.PrepareCPAUpgrade(r.Context(), UpgradeOptions{
		BackupRoot: s.backupRoot,
		Request:    request,
	})
	if err != nil {
		response.Error(w, http.StatusBadGateway, err)
		return
	}
	response.JSON(w, http.StatusOK, result)
}

func (s *Server) upgradeCPARecreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.MethodNotAllowed(w)
		return
	}
	var request model.ContainerOpsUpgradeRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		response.Error(w, http.StatusBadRequest, err)
		return
	}
	request.Apply = true
	result, err := s.docker.RecreateCPAUpgrade(r.Context(), UpgradeOptions{
		BackupRoot: s.backupRoot,
		Request:    request,
	})
	if err != nil {
		response.Error(w, http.StatusBadGateway, err)
		return
	}
	response.JSON(w, http.StatusOK, result)
}

func (s *Server) upgradeCPAJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.MethodNotAllowed(w)
		return
	}
	var request model.ContainerOpsUpgradeJobStartRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		response.Error(w, http.StatusBadRequest, err)
		return
	}
	request.TaskID = strings.TrimSpace(request.TaskID)
	if request.TaskID == "" {
		response.Error(w, http.StatusBadRequest, fmt.Errorf("taskId is required"))
		return
	}
	job, err := s.createUpgradeJob(request)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, err)
		return
	}
	go s.runUpgradeJob(context.Background(), job.JobID)
	response.JSON(w, http.StatusAccepted, job)
}

func (s *Server) upgradeCPAJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		response.MethodNotAllowed(w)
		return
	}
	jobID := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/upgrades/cpa/jobs/"))
	if jobID == "" || strings.Contains(jobID, "/") {
		http.NotFound(w, r)
		return
	}
	job, ok := s.getUpgradeJob(jobID)
	if !ok {
		response.Error(w, http.StatusNotFound, fmt.Errorf("upgrade job not found"))
		return
	}
	response.JSON(w, http.StatusOK, job)
}

func (s *Server) createUpgradeJob(request model.ContainerOpsUpgradeJobStartRequest) (model.ContainerOpsUpgradeJob, error) {
	upgradeRequest := normalizeUpgradeRequest(model.ContainerOpsUpgradeRequest{
		CPAImage:   request.CPAImage,
		CPAMPImage: request.CPAMPImage,
		Apply:      true,
	})
	nowMS := time.Now().UTC().UnixMilli()

	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	jobID := ""
	for {
		s.jobSeq++
		jobID = fmt.Sprintf("upgrade-cpa-job-%d-%d", nowMS, s.jobSeq)
		if _, exists := s.upgradeJobs[jobID]; !exists {
			break
		}
	}
	job := model.ContainerOpsUpgradeJob{
		JobID:            jobID,
		TaskID:           strings.TrimSpace(request.TaskID),
		Status:           "queued",
		Phase:            "queued",
		CPAImage:         upgradeRequest.CPAImage,
		CPAMPImage:       upgradeRequest.CPAMPImage,
		RollbackBackupID: strings.TrimSpace(request.RollbackBackupID),
		Message:          "Upgrade job queued on cpamp-agent.",
		NextAction:       "wait_for_agent_job",
		Actions:          buildUpgradeRecreateActions(),
		StartedAtMS:      nowMS,
		CreatedAtMS:      nowMS,
		UpdatedAtMS:      nowMS,
	}
	s.upgradeJobs[jobID] = job
	if err := s.saveUpgradeJobLocked(job); err != nil {
		delete(s.upgradeJobs, jobID)
		return model.ContainerOpsUpgradeJob{}, fmt.Errorf("persist upgrade job: %w", err)
	}
	return job, nil
}

func (s *Server) getUpgradeJob(jobID string) (model.ContainerOpsUpgradeJob, bool) {
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	job, ok := s.upgradeJobs[jobID]
	return job, ok
}

func (s *Server) updateUpgradeJob(jobID string, mutate func(*model.ContainerOpsUpgradeJob)) (model.ContainerOpsUpgradeJob, bool, error) {
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	job, ok := s.upgradeJobs[jobID]
	if !ok {
		return model.ContainerOpsUpgradeJob{}, false, nil
	}
	mutate(&job)
	job.UpdatedAtMS = time.Now().UTC().UnixMilli()
	s.upgradeJobs[jobID] = job
	if err := s.saveUpgradeJobLocked(job); err != nil {
		return job, true, fmt.Errorf("persist upgrade job: %w", err)
	}
	return job, true, nil
}

func (s *Server) runUpgradeJob(ctx context.Context, jobID string) {
	job, ok, _ := s.updateUpgradeJob(jobID, func(job *model.ContainerOpsUpgradeJob) {
		job.Status = "running"
		job.Phase = "async_recreate"
		job.Message = "Upgrade job is running on cpamp-agent."
		job.NextAction = "wait_for_async_result"
	})
	if !ok {
		return
	}

	plan, err := s.docker.RecreateCPAUpgrade(ctx, UpgradeOptions{
		BackupRoot:       s.backupRoot,
		RollbackBackupID: job.RollbackBackupID,
		Request: model.ContainerOpsUpgradeRequest{
			CPAImage:   job.CPAImage,
			CPAMPImage: job.CPAMPImage,
			Apply:      true,
		},
	})
	if err != nil {
		s.updateUpgradeJob(jobID, func(job *model.ContainerOpsUpgradeJob) {
			job.Status = "failed"
			job.Phase = "async_recreate_failed"
			job.Message = "Upgrade job failed before the agent returned a plan."
			job.Error = err.Error()
			job.NextAction = "review_failure"
			job.FinishedAtMS = time.Now().UTC().UnixMilli()
		})
		return
	}

	status := upgradeJobStatus(plan.Status)
	s.updateUpgradeJob(jobID, func(job *model.ContainerOpsUpgradeJob) {
		job.Status = status
		job.Phase = upgradeJobPhase(status)
		job.CPAImage = plan.CPAImage
		job.CPAMPImage = plan.CPAMPImage
		job.Message = upgradeJobMessage(status, plan.Status)
		job.Error = upgradeJobError(status, plan.Status)
		job.NextAction = upgradeJobNextAction(status)
		job.Checks = plan.Checks
		job.Actions = plan.Actions
		job.Plan = &plan
		job.FinishedAtMS = time.Now().UTC().UnixMilli()
	})
}

func (s *Server) loadUpgradeJobs() error {
	if s.upgradeJobs == nil {
		s.upgradeJobs = make(map[string]model.ContainerOpsUpgradeJob)
	}
	root := strings.TrimSpace(s.upgradeJobRoot)
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read upgrade jobs: %w", err)
	}
	nowMS := time.Now().UTC().UnixMilli()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read upgrade job %s: %w", entry.Name(), err)
		}
		var job model.ContainerOpsUpgradeJob
		if err := json.Unmarshal(data, &job); err != nil {
			return fmt.Errorf("decode upgrade job %s: %w", entry.Name(), err)
		}
		job.JobID = strings.TrimSpace(job.JobID)
		if job.JobID == "" {
			return fmt.Errorf("upgrade job %s is missing jobId", entry.Name())
		}
		if !upgradeJobTerminal(job.Status) {
			job.Status = "failed"
			job.Phase = "async_recreate_failed"
			job.Message = "Upgrade job was recovered after cpamp-agent restart without an active runner."
			job.Error = "agent restarted before the upgrade job reached a terminal state"
			job.NextAction = "restart_upgrade_task"
			job.FinishedAtMS = nowMS
			job.UpdatedAtMS = nowMS
		}
		s.upgradeJobs[job.JobID] = job
		if seq, ok := upgradeJobSequence(job.JobID); ok && seq > s.jobSeq {
			s.jobSeq = seq
		}
		if job.Status == "failed" && job.NextAction == "restart_upgrade_task" {
			if err := s.saveUpgradeJobLocked(job); err != nil {
				return fmt.Errorf("persist recovered upgrade job %s: %w", job.JobID, err)
			}
		}
	}
	return nil
}

func (s *Server) saveUpgradeJobLocked(job model.ContainerOpsUpgradeJob) error {
	root := strings.TrimSpace(s.upgradeJobRoot)
	if root == "" {
		return nil
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	target := s.upgradeJobPath(job.JobID)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *Server) upgradeJobPath(jobID string) string {
	return filepath.Join(s.upgradeJobRoot, safeFileToken(jobID)+".json")
}

func upgradeJobTerminal(status string) bool {
	value := strings.ToLower(strings.TrimSpace(status))
	return value == "completed" ||
		value == "recreate_deferred" ||
		value == "blocked" ||
		value == "failed" ||
		strings.Contains(value, "failed")
}

func upgradeJobSequence(jobID string) (int64, bool) {
	prefix := "upgrade-cpa-job-"
	if !strings.HasPrefix(jobID, prefix) {
		return 0, false
	}
	index := strings.LastIndex(jobID, "-")
	if index <= len(prefix) || index == len(jobID)-1 {
		return 0, false
	}
	var seq int64
	if _, err := fmt.Sscanf(jobID[index+1:], "%d", &seq); err != nil {
		return 0, false
	}
	return seq, true
}

func (c *DockerClient) UpgradeCPAPlan(ctx context.Context, options UpgradeOptions) (model.ContainerOpsUpgradePlan, error) {
	request := normalizeUpgradeRequest(options.Request)
	checks := validateUpgradeImages(request)
	overview, err := c.Overview(ctx)
	if err != nil {
		return model.ContainerOpsUpgradePlan{}, fmt.Errorf("discover docker resources: %w", err)
	}
	checks = append(checks, buildUpgradeTargetChecks(overview)...)
	status := upgradeStatus(checks, false)
	plan := model.ContainerOpsUpgradePlan{
		Status:      status,
		CPAImage:    request.CPAImage,
		CPAMPImage:  request.CPAMPImage,
		Checks:      checks,
		Steps:       buildUpgradeSteps(),
		ReadOnly:    true,
		Destructive: true,
		Overview:    &overview,
	}
	return plan, nil
}

func (c *DockerClient) PrepareCPAUpgrade(ctx context.Context, options UpgradeOptions) (model.ContainerOpsUpgradePlan, error) {
	backupRoot := cleanBackupRoot(options.BackupRoot)
	request := normalizeUpgradeRequest(options.Request)
	plan, err := c.UpgradeCPAPlan(ctx, UpgradeOptions{BackupRoot: backupRoot, Request: request})
	if err != nil {
		return model.ContainerOpsUpgradePlan{}, err
	}
	plan.ReadOnly = false
	plan.Actions = buildUpgradeActions()
	if plan.Status == "blocked" {
		return plan, nil
	}

	rollback, err := c.BackupCPAStack(ctx, BackupOptions{
		BackupRoot:     backupRoot,
		BackupIDPrefix: "upgrade-cpa",
		Now:            options.Now,
	})
	if err != nil {
		return upgradeFailure(plan, "create_upgrade_backup", "Create upgrade rollback backup failed: "+err.Error()), nil
	}
	plan.RollbackBackup = &rollback
	upgradeMarkAction(plan.Actions, "create_upgrade_backup", "applied", "Upgrade rollback backup created.")

	for _, image := range []string{request.CPAImage, request.CPAMPImage} {
		if err := c.pullImage(ctx, image); err != nil {
			return upgradeFailure(plan, "pull_upgrade_images", "Pull upgrade image failed: "+err.Error()), nil
		}
		plan.ImagePulls = append(plan.ImagePulls, model.ContainerOpsImagePull{
			Image:   image,
			Status:  "pulled",
			Message: "Upgrade image pull completed through cpamp-agent.",
		})
	}
	upgradeMarkAction(plan.Actions, "pull_upgrade_images", "applied", "Upgrade images pulled.")
	upgradeMarkAction(plan.Actions, "prepare_recreate", "skipped", "Synchronous recreate is deferred to a later asynchronous upgrade phase.")
	plan.Status = "prepared"
	plan.Applied = true
	return plan, nil
}

func (c *DockerClient) RecreateCPAUpgrade(ctx context.Context, options UpgradeOptions) (model.ContainerOpsUpgradePlan, error) {
	backupRoot := cleanBackupRoot(options.BackupRoot)
	request := normalizeUpgradeRequest(options.Request)
	plan, err := c.UpgradeCPAPlan(ctx, UpgradeOptions{BackupRoot: backupRoot, Request: request})
	if err != nil {
		return model.ContainerOpsUpgradePlan{}, err
	}
	plan.ReadOnly = false
	plan.Actions = buildUpgradeRecreateActions()
	if plan.Status == "blocked" {
		return plan, nil
	}

	rollbackBackupID, err := cleanBackupID(options.RollbackBackupID)
	if err != nil {
		return upgradeRecreateBlocked(plan, "verify_rollback_backup", "Rollback backup is required before CPA recreate: "+err.Error()), nil
	}
	rollbackBackup, err := verifyUpgradeRollbackBackup(backupRoot, rollbackBackupID)
	if err != nil {
		return upgradeRecreateBlocked(plan, "verify_rollback_backup", "Rollback backup is not usable before CPA recreate: "+err.Error()), nil
	}
	plan.RollbackBackup = &rollbackBackup
	upgradeMarkAction(plan.Actions, "verify_rollback_backup", "applied", "Upgrade rollback backup is available.")

	overview, err := c.Overview(ctx)
	if err != nil {
		return upgradeRecreateFailure(plan, "stop_cpa_container", "Refresh CPA recreate target failed: "+err.Error(), false), nil
	}
	cpa, ok := findContainerByName(overview, "cli-proxy-api")
	if !ok || !cpa.Managed || cpa.Role != "cpa" {
		return upgradeRecreateBlocked(plan, "stop_cpa_container", "Standard managed CPA container is required before CPA recreate."), nil
	}
	if !upgradeCPADataMountReady(cpa) {
		return upgradeRecreateBlocked(plan, "recreate_cpa_container", "CPA container must have a writable /app/data volume or bind mount before recreate."), nil
	}

	wasRunning := cpa.State == "running"
	if wasRunning {
		if err := c.stopUpgradeContainer(ctx, cpa.Name); err != nil {
			return upgradeRecreateFailure(plan, "stop_cpa_container", "Stop CPA container failed: "+err.Error(), true), nil
		}
		upgradeMarkAction(plan.Actions, "stop_cpa_container", "applied", "CPA container stopped for recreate.")
	} else {
		upgradeMarkAction(plan.Actions, "stop_cpa_container", "skipped", "CPA container was not running.")
	}

	now := options.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	preservedName := "cli-proxy-api-upgrade-old-" + now.UTC().Format("20060102T150405Z")
	if err := c.renameUpgradeContainer(ctx, cpa.Name, preservedName); err != nil {
		return upgradeRecreateFailure(plan, "preserve_old_cpa_container", "Preserve old CPA container failed: "+err.Error(), true), nil
	}
	upgradeMarkAction(plan.Actions, "preserve_old_cpa_container", "applied", "Old CPA container was renamed and preserved for rollback.")

	spec := upgradeCPAServiceSpec(request.CPAImage, cpa)
	if err := c.createDeployContainer(ctx, spec); err != nil {
		rollbackMessage := c.rollbackCPARecreate(ctx, preservedName, false, wasRunning, now)
		return upgradeRecreateFailure(plan, "recreate_cpa_container", "Create upgraded CPA container failed: "+err.Error()+rollbackMessage, true), nil
	}
	upgradeMarkAction(plan.Actions, "recreate_cpa_container", "applied", "Upgraded CPA container created with the prepared image.")

	if err := c.startDeployContainer(ctx, spec.Name); err != nil {
		rollbackMessage := c.rollbackCPARecreate(ctx, preservedName, true, wasRunning, now)
		return upgradeRecreateFailure(plan, "start_cpa_container", "Start upgraded CPA container failed: "+err.Error()+rollbackMessage, true), nil
	}
	upgradeMarkAction(plan.Actions, "start_cpa_container", "applied", "Upgraded CPA container start requested.")

	nextOverview, err := c.Overview(ctx)
	if err != nil {
		rollbackMessage := c.rollbackCPARecreate(ctx, preservedName, true, wasRunning, now)
		return upgradeRecreateFailure(plan, "healthcheck_after_recreate", "Verify upgraded CPA container failed: "+err.Error()+rollbackMessage, true), nil
	}
	plan.Overview = &nextOverview
	if upgraded, ok := findContainerByName(nextOverview, spec.Name); !ok || upgraded.State != "running" || !upgraded.Managed || upgraded.Role != "cpa" {
		rollbackMessage := c.rollbackCPARecreate(ctx, preservedName, true, wasRunning, now)
		return upgradeRecreateFailure(plan, "healthcheck_after_recreate", "Upgraded CPA container failed the running-state health check."+rollbackMessage, true), nil
	}
	upgradeMarkAction(plan.Actions, "healthcheck_after_recreate", "applied", "Upgraded CPA container is running.")
	upgradeMarkAction(plan.Actions, "recreate_cpamp_container", "skipped", "CPAMP and Agent recreate remain deferred to a later phase.")
	plan.Status = "completed"
	plan.Applied = true
	return plan, nil
}

func verifyUpgradeRollbackBackup(backupRoot string, backupID string) (model.ContainerOpsBackupResult, error) {
	backupDir := filepathForBackupID(backupRoot, backupID)
	manifest, err := readBackupManifest(backupDir)
	if err != nil {
		return model.ContainerOpsBackupResult{}, err
	}
	if manifest.BackupID != backupID {
		return model.ContainerOpsBackupResult{}, fmt.Errorf("backup manifest ID %q does not match %q", manifest.BackupID, backupID)
	}
	if !backupHasCPAArchive(backupRoot, backupID, manifest) {
		return model.ContainerOpsBackupResult{}, fmt.Errorf("backup %s does not include a usable CPA archive", backupID)
	}
	return manifest, nil
}

func upgradeCPADataMountReady(cpa model.ContainerOpsDockerContainer) bool {
	for _, mount := range cpa.Mounts {
		if mount.Destination != "/app/data" || !mount.RW {
			continue
		}
		if mount.Type == "volume" && firstNonEmptyValue(mount.Name, mount.Source) != "" {
			return true
		}
		if mount.Type == "bind" && strings.TrimSpace(mount.Source) != "" {
			return true
		}
	}
	return false
}

func upgradeCPAServiceSpec(image string, cpa model.ContainerOpsDockerContainer) deployServiceSpec {
	ports := make(map[string]string)
	for _, port := range cpa.Ports {
		if port.PrivatePort <= 0 || port.PublicPort <= 0 {
			continue
		}
		protocol := strings.TrimSpace(port.Type)
		if protocol == "" {
			protocol = "tcp"
		}
		ports[strconv.Itoa(port.PrivatePort)+"/"+protocol] = strconv.Itoa(port.PublicPort)
	}
	if len(ports) == 0 {
		ports["8317/tcp"] = "8317"
	}

	volumeMounts := make(map[string]string)
	binds := make([]string, 0)
	for _, mount := range cpa.Mounts {
		if strings.TrimSpace(mount.Destination) == "" {
			continue
		}
		switch mount.Type {
		case "volume":
			source := firstNonEmptyValue(mount.Name, mount.Source)
			if source != "" {
				volumeMounts[source] = mount.Destination
			}
		case "bind":
			source := strings.TrimSpace(mount.Source)
			if source == "" {
				continue
			}
			bind := source + ":" + mount.Destination
			if !mount.RW {
				bind += ":ro"
			}
			binds = append(binds, bind)
		}
	}

	return deployServiceSpec{
		Role:         "cpa",
		Name:         "cli-proxy-api",
		Image:        image,
		Ports:        ports,
		VolumeMounts: volumeMounts,
		Binds:        binds,
		StartOrder:   1,
	}
}

func (c *DockerClient) stopUpgradeContainer(ctx context.Context, name string) error {
	return c.post(ctx, "/containers/"+url.PathEscape(name)+"/stop", nil, nil)
}

func (c *DockerClient) renameUpgradeContainer(ctx context.Context, currentName string, nextName string) error {
	endpoint := "/containers/" + url.PathEscape(currentName) + "/rename?name=" + url.QueryEscape(nextName)
	return c.post(ctx, endpoint, nil, nil)
}

func (c *DockerClient) rollbackCPARecreate(ctx context.Context, preservedName string, upgradedContainerExists bool, originalWasRunning bool, now time.Time) string {
	messages := make([]string, 0, 3)
	failedName := "cli-proxy-api-upgrade-failed-" + now.UTC().Format("20060102T150405Z")
	if upgradedContainerExists {
		if err := c.renameUpgradeContainer(ctx, "cli-proxy-api", failedName); err != nil {
			messages = append(messages, "rename failed upgraded CPA container: "+err.Error())
		}
	}
	if err := c.renameUpgradeContainer(ctx, preservedName, "cli-proxy-api"); err != nil {
		messages = append(messages, "restore old CPA container name: "+err.Error())
	} else if originalWasRunning {
		if err := c.startDeployContainer(ctx, "cli-proxy-api"); err != nil {
			messages = append(messages, "restart old CPA container: "+err.Error())
		}
	}
	if len(messages) == 0 {
		return " Rollback to the preserved CPA container was requested."
	}
	return " Rollback encountered errors: " + strings.Join(messages, "; ") + "."
}

func normalizeUpgradeRequest(request model.ContainerOpsUpgradeRequest) model.ContainerOpsUpgradeRequest {
	request.CPAImage = strings.TrimSpace(request.CPAImage)
	if request.CPAImage == "" {
		request.CPAImage = defaultCPAUpgradeImage
	}
	request.CPAMPImage = strings.TrimSpace(request.CPAMPImage)
	if request.CPAMPImage == "" {
		request.CPAMPImage = defaultCPAMPUpgradeImage
	}
	return request
}

func validateUpgradeImages(request model.ContainerOpsUpgradeRequest) []model.ContainerOpsUpgradeCheck {
	checks := make([]model.ContainerOpsUpgradeCheck, 0, 3)
	add := func(severity string, code string, message string, resource string, blocking bool) {
		checks = append(checks, model.ContainerOpsUpgradeCheck{Severity: severity, Code: code, Message: message, Resource: resource, Blocking: blocking})
	}
	if !deployImageAllowed("cpa", request.CPAImage) {
		add("error", "upgrade_cpa_image_unsupported", "CPA upgrade image must use the standard seakee/cli-proxy-api repository.", request.CPAImage, true)
	} else {
		add("info", "upgrade_cpa_image_allowed", "CPA upgrade image uses the standard repository.", request.CPAImage, false)
	}
	if !deployImageAllowed("cpamp", request.CPAMPImage) {
		add("error", "upgrade_cpamp_image_unsupported", "CPAMP upgrade image must use the standard seakee/cpa-manager-plus repository.", request.CPAMPImage, true)
	} else {
		add("info", "upgrade_cpamp_image_allowed", "CPAMP upgrade image uses the standard repository.", request.CPAMPImage, false)
	}
	add("info", "upgrade_agent_recreate_deferred", "cpamp-agent uses the CPAMP image, but self-upgrade/recreate is deferred to an asynchronous phase.", "cpamp-agent", false)
	return checks
}

func buildUpgradeTargetChecks(overview model.ContainerOpsDockerOverview) []model.ContainerOpsUpgradeCheck {
	checks := make([]model.ContainerOpsUpgradeCheck, 0, 5)
	add := func(severity string, code string, message string, resource string, blocking bool) {
		checks = append(checks, model.ContainerOpsUpgradeCheck{Severity: severity, Code: code, Message: message, Resource: resource, Blocking: blocking})
	}
	if network, ok := findDockerNetwork(overview, standardCPANetworkName); !ok {
		add("error", "upgrade_standard_network_missing", "Standard CPA network must exist before upgrade preparation.", standardCPANetworkName, true)
	} else if network.Driver != "bridge" || !network.Managed {
		add("error", "upgrade_standard_network_unmanaged", "Standard CPA network must be a CPAMP-managed bridge network before upgrade preparation.", standardCPANetworkName, true)
	} else {
		add("info", "upgrade_standard_network_ready", "Standard CPA network is ready.", standardCPANetworkName, false)
	}
	for _, target := range []struct {
		role string
		name string
	}{
		{role: "cpa", name: "cli-proxy-api"},
		{role: "cpamp", name: "cpa-manager-plus"},
	} {
		container, ok := findContainerByName(overview, target.name)
		if !ok {
			add("error", "upgrade_target_missing", "Standard target container is required before upgrade preparation.", target.name, true)
			continue
		}
		if !container.Managed || container.Role != target.role {
			add("error", "upgrade_target_unmanaged", "Upgrade target must be CPAMP-managed for the expected role.", target.name, true)
			continue
		}
		if container.State != "running" {
			add("warning", "upgrade_target_not_running", "Upgrade target exists but is not running.", target.name, false)
			continue
		}
		add("info", "upgrade_target_ready", "Upgrade target is CPAMP-managed and running.", target.name, false)
	}
	return checks
}

func buildUpgradeSteps() []model.ContainerOpsUpgradeStep {
	return []model.ContainerOpsUpgradeStep{
		{Order: 1, Code: "precheck", Title: "Validate standard CPA stack ownership and target images", Target: "cpa", Destructive: false},
		{Order: 2, Code: "create_upgrade_backup", Title: "Create upgrade rollback backup", Target: "cpa", Destructive: false},
		{Order: 3, Code: "pull_upgrade_images", Title: "Pull standard CPA and CPAMP upgrade images", Target: "images", Destructive: false},
		{Order: 4, Code: "prepare_recreate", Title: "Defer container recreate to asynchronous upgrade phase", Target: "cpa", Destructive: true},
	}
}

func buildUpgradeActions() []model.ContainerOpsUpgradeAction {
	return []model.ContainerOpsUpgradeAction{
		{Order: 1, Code: "create_upgrade_backup", Target: "cpa", Status: "planned", Message: "Create upgrade rollback backup."},
		{Order: 2, Code: "pull_upgrade_images", Target: "images", Status: "planned", Message: "Pull standard upgrade images."},
		{Order: 3, Code: "prepare_recreate", Target: "cpa", Status: "planned", Message: "Prepare for a later asynchronous recreate phase."},
	}
}

func buildUpgradeRecreateActions() []model.ContainerOpsUpgradeAction {
	return []model.ContainerOpsUpgradeAction{
		{Order: 1, Code: "verify_rollback_backup", Target: "rollback", Status: "planned", Message: "Verify the upgrade rollback backup before any container change."},
		{Order: 2, Code: "stop_cpa_container", Target: "cli-proxy-api", Status: "planned", Message: "Stop the current CPA container."},
		{Order: 3, Code: "preserve_old_cpa_container", Target: "cli-proxy-api", Status: "planned", Message: "Rename and preserve the old CPA container for rollback."},
		{Order: 4, Code: "recreate_cpa_container", Target: "cli-proxy-api", Status: "planned", Message: "Create the CPA container with the prepared image."},
		{Order: 5, Code: "start_cpa_container", Target: "cli-proxy-api", Status: "planned", Message: "Start the upgraded CPA container."},
		{Order: 6, Code: "healthcheck_after_recreate", Target: "cli-proxy-api", Status: "planned", Message: "Verify the upgraded CPA container is running."},
		{Order: 7, Code: "recreate_cpamp_container", Target: "cpa-manager-plus", Status: "planned", Message: "Recreate CPAMP in a later phase."},
	}
}

func upgradeStatus(checks []model.ContainerOpsUpgradeCheck, applied bool) string {
	if upgradeChecksBlocking(checks) {
		return "blocked"
	}
	if applied {
		return "prepared"
	}
	hasWarning := false
	for _, check := range checks {
		if check.Severity == "warning" {
			hasWarning = true
			break
		}
	}
	if hasWarning {
		return "ready_with_warnings"
	}
	return "ready"
}

func upgradeJobStatus(planStatus string) string {
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

func upgradeJobPhase(status string) string {
	switch status {
	case "queued":
		return "queued"
	case "running":
		return "async_recreate"
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

func upgradeJobNextAction(status string) string {
	switch status {
	case "queued":
		return "wait_for_agent_job"
	case "running":
		return "wait_for_async_result"
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

func upgradeJobMessage(status string, planStatus string) string {
	switch status {
	case "completed":
		return "Upgrade job completed on cpamp-agent."
	case "recreate_deferred":
		return "Upgrade job reached the deferred recreate boundary on cpamp-agent."
	case "blocked":
		return "Upgrade job was blocked by agent checks."
	case "failed":
		return "Upgrade job failed on cpamp-agent."
	default:
		return "Upgrade job returned status " + planStatus + "."
	}
}

func upgradeJobError(status string, planStatus string) string {
	if status != "failed" {
		return ""
	}
	return "upgrade job returned status " + planStatus
}

func upgradeFailure(plan model.ContainerOpsUpgradePlan, actionCode string, message string) model.ContainerOpsUpgradePlan {
	upgradeMarkAction(plan.Actions, actionCode, "failed", message)
	plan.Status = "upgrade_prepare_failed"
	plan.Applied = true
	return plan
}

func upgradeRecreateBlocked(plan model.ContainerOpsUpgradePlan, actionCode string, message string) model.ContainerOpsUpgradePlan {
	upgradeMarkAction(plan.Actions, actionCode, "failed", message)
	plan.Checks = append(plan.Checks, model.ContainerOpsUpgradeCheck{
		Severity: "error",
		Code:     "upgrade_recreate_blocked",
		Message:  message,
		Resource: actionCode,
		Blocking: true,
	})
	plan.Status = "blocked"
	plan.Applied = false
	return plan
}

func upgradeRecreateFailure(plan model.ContainerOpsUpgradePlan, actionCode string, message string, applied bool) model.ContainerOpsUpgradePlan {
	upgradeMarkAction(plan.Actions, actionCode, "failed", message)
	plan.Checks = append(plan.Checks, model.ContainerOpsUpgradeCheck{
		Severity: "error",
		Code:     "upgrade_recreate_failed",
		Message:  message,
		Resource: actionCode,
		Blocking: true,
	})
	plan.Status = "upgrade_recreate_failed"
	plan.Applied = applied
	return plan
}

func upgradeChecksBlocking(checks []model.ContainerOpsUpgradeCheck) bool {
	for _, check := range checks {
		if check.Blocking {
			return true
		}
	}
	return false
}

func upgradeMarkAction(actions []model.ContainerOpsUpgradeAction, code string, status string, message string) {
	for index := range actions {
		if actions[index].Code == code {
			actions[index].Status = status
			actions[index].Message = message
			return
		}
	}
}
