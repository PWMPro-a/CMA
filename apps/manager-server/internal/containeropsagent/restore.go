package containeropsagent

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

type RestorePlanOptions struct {
	BackupRoot string
	BackupID   string
}

type RestoreApplyOptions struct {
	BackupRoot string
	BackupID   string
	Now        time.Time
}

type restoreApplyMode struct {
	SafetyBackupPrefix  string
	SafetyBackupTitle   string
	SafetyBackupMessage string
	ArchiveActionPrefix string
	ArchiveActionVerb   string
	SuccessStatus       string
	FailureStatus       string
	CompletedCheckCode  string
	CompletedMessage    string
	ActionFailedCode    string
	ActionFailedMessage string
	CommitActionCode    string
	CommitMessage       string
	PreflightCheckCode  string
	PreflightMessage    string
}

var restoreApplyModeRestore = restoreApplyMode{
	SafetyBackupPrefix:  "rollback-cpa",
	SafetyBackupTitle:   "Create a fresh rollback backup before restore",
	SafetyBackupMessage: "Rollback backup created before restore.",
	ArchiveActionPrefix: "restore_",
	ArchiveActionVerb:   "Restore",
	SuccessStatus:       "restored",
	FailureStatus:       "restore_failed",
	CompletedCheckCode:  "restore_completed",
	CompletedMessage:    "CPA stack restore completed and target containers are running.",
	ActionFailedCode:    "restore_action_failed",
	ActionFailedMessage: "Restore execution action failed.",
	CommitActionCode:    "commit_restore",
	CommitMessage:       "Restore completed after health checks.",
	PreflightCheckCode:  "readonly_restore_plan",
	PreflightMessage:    "This endpoint only builds a restore preflight plan and does not modify Docker resources.",
}

var restoreApplyModeRollback = restoreApplyMode{
	SafetyBackupPrefix:  "pre-rollback-cpa",
	SafetyBackupTitle:   "Create a fresh safety backup before rollback",
	SafetyBackupMessage: "Safety backup created before rollback.",
	ArchiveActionPrefix: "rollback_",
	ArchiveActionVerb:   "Rollback",
	SuccessStatus:       "rolled_back",
	FailureStatus:       "rollback_failed",
	CompletedCheckCode:  "rollback_completed",
	CompletedMessage:    "CPA stack rollback completed and target containers are running.",
	ActionFailedCode:    "rollback_action_failed",
	ActionFailedMessage: "Rollback execution action failed.",
	CommitActionCode:    "commit_rollback",
	CommitMessage:       "Rollback completed after health checks.",
	PreflightCheckCode:  "rollback_preflight_ready",
	PreflightMessage:    "Rollback preflight passed; execution will create a safety backup before applying the requested rollback backup.",
}

func (c *DockerClient) RestoreCPAPlan(ctx context.Context, options RestorePlanOptions) (model.ContainerOpsRestorePlan, error) {
	backupRoot := cleanBackupRoot(options.BackupRoot)
	backupID, err := cleanBackupID(options.BackupID)
	if err != nil {
		return model.ContainerOpsRestorePlan{}, err
	}
	backupDir := filepath.Join(backupRoot, backupID)
	manifest, err := readBackupManifest(backupDir)
	if err != nil {
		return model.ContainerOpsRestorePlan{}, err
	}

	overview, err := c.Overview(ctx)
	if err != nil {
		return model.ContainerOpsRestorePlan{}, fmt.Errorf("discover docker resources: %w", err)
	}

	checks := buildRestoreChecks(backupDir, backupID, manifest, overview)
	plan := model.ContainerOpsRestorePlan{
		BackupID:   backupID,
		Status:     restoreStatus(checks),
		BackupRoot: backupRoot,
		CreatedAt:  manifest.CreatedAt,
		Archives:   manifest.Archives,
		Checks:     checks,
		Steps:      buildRestoreSteps(manifest.Archives),
		ReadOnly:   true,
	}
	return plan, nil
}

func (c *DockerClient) RestoreCPA(ctx context.Context, options RestoreApplyOptions) (model.ContainerOpsRestorePlan, error) {
	return c.applyCPAArchiveBackup(ctx, options, restoreApplyModeRestore)
}

func (c *DockerClient) RollbackCPA(ctx context.Context, options RestoreApplyOptions) (model.ContainerOpsRestorePlan, error) {
	return c.applyCPAArchiveBackup(ctx, options, restoreApplyModeRollback)
}

func (c *DockerClient) applyCPAArchiveBackup(ctx context.Context, options RestoreApplyOptions, mode restoreApplyMode) (model.ContainerOpsRestorePlan, error) {
	backupRoot := cleanBackupRoot(options.BackupRoot)
	backupID, err := cleanBackupID(options.BackupID)
	if err != nil {
		return model.ContainerOpsRestorePlan{}, err
	}
	plan, err := c.RestoreCPAPlan(ctx, RestorePlanOptions{BackupRoot: backupRoot, BackupID: backupID})
	if err != nil {
		return model.ContainerOpsRestorePlan{}, err
	}
	adaptRestorePlanForApplyMode(&plan, mode)
	plan.ReadOnly = false
	plan.Destructive = true
	plan.Actions = buildRestoreActionsWithMode(plan.Archives, mode)
	if plan.Status == "blocked" {
		return plan, nil
	}

	rollback, err := c.BackupCPAStack(ctx, BackupOptions{
		BackupRoot:     backupRoot,
		BackupIDPrefix: mode.SafetyBackupPrefix,
		Now:            options.Now,
	})
	if err != nil {
		return restoreApplyFailure(plan, mode, "create_rollback_backup", "Create safety backup failed: "+err.Error(), false), nil
	}
	plan.RollbackBackup = &rollback
	restoreMarkAction(plan.Actions, "create_rollback_backup", "applied", mode.SafetyBackupMessage)

	overview, err := c.Overview(ctx)
	if err != nil {
		return restoreApplyFailure(plan, mode, "stop_cpa", "Refresh restore targets failed: "+err.Error(), false), nil
	}
	targets := restoreTargetsByRole(overview, plan.Archives)
	if target, ok := targets["cpamp"]; ok {
		if target.State == "running" {
			if err := c.stopRestoreContainer(ctx, target); err != nil {
				return restoreApplyFailure(plan, mode, "stop_cpamp", "Stop CPAMP container failed: "+err.Error(), true), nil
			}
			restoreMarkAction(plan.Actions, "stop_cpamp", "applied", "CPAMP container stopped for restore.")
		} else {
			restoreMarkAction(plan.Actions, "stop_cpamp", "skipped", "CPAMP container was not running.")
		}
	} else if hasRestoreArchiveRole(plan.Archives, "cpamp") {
		restoreMarkAction(plan.Actions, "stop_cpamp", "skipped", "No CPAMP target container was detected.")
	}

	cpa, ok := targets["cpa"]
	if !ok {
		return restoreApplyFailure(plan, mode, "stop_cpa", "CPA restore target disappeared before restore.", false), nil
	}
	if cpa.State == "running" {
		if err := c.stopRestoreContainer(ctx, cpa); err != nil {
			return restoreApplyFailure(plan, mode, "stop_cpa", "Stop CPA container failed: "+err.Error(), true), nil
		}
		restoreMarkAction(plan.Actions, "stop_cpa", "applied", "CPA container stopped for restore.")
	} else {
		restoreMarkAction(plan.Actions, "stop_cpa", "skipped", "CPA container was not running.")
	}

	backupDir := filepath.Join(backupRoot, backupID)
	for _, archive := range plan.Archives {
		if archive.Role != "cpa" && archive.Role != "cpamp" {
			continue
		}
		actionCode := mode.archiveActionCode(archive.Role)
		target, ok := targets[archive.Role]
		if !ok {
			restoreMarkAction(plan.Actions, actionCode, "skipped", "No restore target container was detected.")
			continue
		}
		archivePath, err := backupFilePath(backupDir, archive.FileName)
		if err != nil {
			return restoreApplyFailure(plan, mode, actionCode, "Resolve restore archive failed: "+err.Error(), true), nil
		}
		if err := c.restoreContainerArchive(ctx, target.Name, archivePath, archive.Path); err != nil {
			return restoreApplyFailure(plan, mode, actionCode, mode.ArchiveActionVerb+" archive failed: "+err.Error(), true), nil
		}
		restoreMarkAction(plan.Actions, actionCode, "applied", mode.ArchiveActionVerb+" archive applied to the target container.")
	}

	if err := c.startRestoreContainer(ctx, cpa); err != nil {
		return restoreApplyFailure(plan, mode, "start_cpa", "Start CPA container failed: "+err.Error(), true), nil
	}
	restoreMarkAction(plan.Actions, "start_cpa", "applied", "CPA container start requested.")
	if target, ok := targets["cpamp"]; ok {
		if err := c.startRestoreContainer(ctx, target); err != nil {
			return restoreApplyFailure(plan, mode, "start_cpamp", "Start CPAMP container failed: "+err.Error(), true), nil
		}
		restoreMarkAction(plan.Actions, "start_cpamp", "applied", "CPAMP container start requested.")
	} else if hasRestoreArchiveRole(plan.Archives, "cpamp") {
		restoreMarkAction(plan.Actions, "start_cpamp", "skipped", "No CPAMP target container was detected.")
	}

	nextOverview, err := c.Overview(ctx)
	if err != nil {
		return restoreApplyFailure(plan, mode, "healthcheck", "Verify restored services failed: "+err.Error(), true), nil
	}
	plan.Overview = &nextOverview
	plan.Checks = append(plan.Checks, restoreHealthChecks(nextOverview, targets, mode)...)
	if restoreChecksBlocking(plan.Checks) {
		restoreMarkAction(plan.Actions, "healthcheck", "failed", "One or more restored containers failed the running-state health check.")
		plan.Status = mode.FailureStatus
		plan.Applied = true
		return plan, nil
	}
	restoreMarkAction(plan.Actions, "healthcheck", "applied", "Restored containers are running.")
	restoreMarkAction(plan.Actions, mode.CommitActionCode, "applied", mode.CommitMessage)
	plan.Status = mode.SuccessStatus
	plan.Applied = true
	return plan, nil
}

func (mode restoreApplyMode) archiveActionCode(role string) string {
	return mode.ArchiveActionPrefix + role + "_archive"
}

func readBackupManifest(backupDir string) (model.ContainerOpsBackupResult, error) {
	data, err := os.ReadFile(filepath.Join(backupDir, "manifest.json"))
	if err != nil {
		return model.ContainerOpsBackupResult{}, fmt.Errorf("read backup manifest: %w", err)
	}
	var manifest model.ContainerOpsBackupResult
	if err := json.Unmarshal(data, &manifest); err != nil {
		return model.ContainerOpsBackupResult{}, fmt.Errorf("parse backup manifest: %w", err)
	}
	return manifest, nil
}

func buildRestoreChecks(
	backupDir string,
	backupID string,
	manifest model.ContainerOpsBackupResult,
	overview model.ContainerOpsDockerOverview,
) []model.ContainerOpsRestoreCheck {
	checks := make([]model.ContainerOpsRestoreCheck, 0, len(manifest.Archives)+6)
	add := func(severity string, code string, message string, resource string, blocking bool) {
		checks = append(checks, model.ContainerOpsRestoreCheck{
			Severity: severity,
			Code:     code,
			Message:  message,
			Resource: resource,
			Blocking: blocking,
		})
	}

	if manifest.BackupID == backupID {
		add("info", "manifest_loaded", "Backup manifest was loaded and matches the requested backup ID.", backupID, false)
	} else {
		add("error", "backup_id_mismatch", "Backup manifest ID does not match the requested backup ID.", manifest.BackupID, true)
	}

	if len(manifest.Archives) == 0 {
		add("error", "archive_manifest_empty", "Backup manifest does not include any archives.", backupID, true)
	}

	hasCPAArchive := false
	hasCPAMPArchive := false
	for _, archive := range manifest.Archives {
		if archive.Role == "cpa" {
			hasCPAArchive = true
		}
		if archive.Role == "cpamp" {
			hasCPAMPArchive = true
		}
		filePath, err := backupFilePath(backupDir, archive.FileName)
		if err != nil {
			add("error", "archive_path_invalid", err.Error(), archive.FileName, true)
			continue
		}
		stat, err := os.Stat(filePath)
		if err != nil {
			add("error", "archive_missing", "Archive file is missing from the backup directory.", archive.FileName, true)
			continue
		}
		if stat.IsDir() {
			add("error", "archive_is_directory", "Archive path points to a directory, not a tar file.", archive.FileName, true)
			continue
		}
		if archive.Size > 0 && stat.Size() != archive.Size {
			add("error", "archive_size_mismatch", "Archive file size does not match the backup manifest.", archive.FileName, true)
			continue
		}
		add("info", "archive_ready", "Archive file is present and matches the backup manifest.", archive.FileName, false)
		if archive.Role == "cpamp" {
			foundUsageDB, foundDataKey, err := scanTarForCPAMPData(filePath)
			if err != nil {
				add("error", "cpamp_archive_unreadable", "CPAMP archive could not be inspected as a tar file.", archive.FileName, true)
			} else if !foundUsageDB || !foundDataKey {
				add("error", "cpamp_required_files_missing", "CPAMP archive must include usage.sqlite and data.key before automatic Manager data restore.", archive.FileName, true)
			} else {
				add("info", "cpamp_required_files_ready", "CPAMP archive includes usage.sqlite and data.key.", archive.FileName, false)
			}
		}
	}
	if !hasCPAArchive {
		add("error", "cpa_archive_missing", "CPA data archive is required before restore can proceed.", backupID, true)
	}

	cpa, cpaFound := selectBackupContainer(overview, "cpa", "cli-proxy-api")
	if !cpaFound {
		add("error", "cpa_target_missing", "No current CPA container was detected as a restore target.", "cli-proxy-api", true)
	} else if cpa.State != "running" {
		add("warning", "cpa_target_not_running", "The CPA restore target exists but is not running.", cpa.Name, false)
	} else {
		add("info", "cpa_target_ready", "Current CPA container was detected as the restore target.", cpa.Name, false)
	}

	if hasCPAMPArchive {
		cpamp, cpampFound := selectBackupContainer(overview, "cpamp", "cpa-manager-plus")
		if !cpampFound {
			add("warning", "cpamp_target_missing", "The backup contains CPAMP data, but no current CPAMP container was detected.", "cpa-manager-plus", false)
		} else {
			add("info", "cpamp_target_ready", "Current CPAMP container was detected as an optional restore target.", cpamp.Name, false)
		}
	}

	add("info", "readonly_restore_plan", "This endpoint only builds a restore preflight plan and does not modify Docker resources.", backupID, false)
	return checks
}

func buildRestoreSteps(archives []model.ContainerOpsBackupArchive) []model.ContainerOpsRestoreStep {
	return buildRestoreStepsWithMode(archives, restoreApplyModeRestore)
}

func buildRestoreStepsWithMode(archives []model.ContainerOpsBackupArchive, mode restoreApplyMode) []model.ContainerOpsRestoreStep {
	steps := []model.ContainerOpsRestoreStep{
		{Order: 1, Code: "create_rollback_backup", Title: mode.SafetyBackupTitle, Target: "cpa", Destructive: false},
	}
	order := 2
	if hasRestoreArchiveRole(archives, "cpamp") {
		steps = append(steps, model.ContainerOpsRestoreStep{Order: order, Code: "stop_cpamp", Title: "Stop CPAMP service", Target: "cpa-manager-plus", Destructive: true})
		order++
	}
	steps = append(steps, model.ContainerOpsRestoreStep{Order: order, Code: "stop_cpa", Title: "Stop CPA service", Target: "cli-proxy-api", Destructive: true})
	order++
	for _, archive := range archives {
		if archive.Role != "cpa" && archive.Role != "cpamp" {
			continue
		}
		steps = append(steps, model.ContainerOpsRestoreStep{
			Order:       order,
			Code:        mode.archiveActionCode(archive.Role),
			Title:       fmt.Sprintf("%s %s archive into %s", mode.ArchiveActionVerb, strings.ToUpper(archive.Role), archive.Path),
			Target:      firstNonEmptyValue(archive.Container, archive.Service),
			Destructive: true,
		})
		order++
	}
	steps = append(steps,
		model.ContainerOpsRestoreStep{Order: order, Code: "start_services", Title: "Start CPA stack services", Target: "cpa", Destructive: true},
		model.ContainerOpsRestoreStep{Order: order + 1, Code: "healthcheck", Title: "Run CPA and NewAPI route health checks", Target: "cpa", Destructive: false},
		model.ContainerOpsRestoreStep{Order: order + 2, Code: mode.CommitActionCode, Title: mode.CommitMessage, Target: "cpa", Destructive: false},
	)
	return steps
}

func restoreStatus(checks []model.ContainerOpsRestoreCheck) string {
	hasWarning := false
	for _, check := range checks {
		if check.Blocking {
			return "blocked"
		}
		if check.Severity == "warning" {
			hasWarning = true
		}
	}
	if hasWarning {
		return "ready_with_warnings"
	}
	return "ready"
}

func buildRestoreActionsWithMode(archives []model.ContainerOpsBackupArchive, mode restoreApplyMode) []model.ContainerOpsRestoreAction {
	actions := make([]model.ContainerOpsRestoreAction, 0, len(archives)+6)
	add := func(code string, target string, message string) {
		actions = append(actions, model.ContainerOpsRestoreAction{
			Order:   len(actions) + 1,
			Code:    code,
			Target:  target,
			Status:  "planned",
			Message: message,
		})
	}
	add("create_rollback_backup", "cpa", mode.SafetyBackupTitle+".")
	if hasRestoreArchiveRole(archives, "cpamp") {
		add("stop_cpamp", "cpa-manager-plus", "Stop CPAMP before restoring Manager data.")
	}
	add("stop_cpa", "cli-proxy-api", "Stop CPA before restoring data.")
	for _, archive := range archives {
		if archive.Role != "cpa" && archive.Role != "cpamp" {
			continue
		}
		add(mode.archiveActionCode(archive.Role), firstNonEmptyValue(archive.Container, archive.Service), mode.ArchiveActionVerb+" archive into the standard target container.")
	}
	add("start_cpa", "cli-proxy-api", "Start CPA after restore.")
	if hasRestoreArchiveRole(archives, "cpamp") {
		add("start_cpamp", "cpa-manager-plus", "Start CPAMP after restore.")
	}
	add("healthcheck", "cpa", "Verify restored containers are running.")
	add(mode.CommitActionCode, "cpa", mode.CommitMessage)
	return actions
}

func adaptRestorePlanForApplyMode(plan *model.ContainerOpsRestorePlan, mode restoreApplyMode) {
	plan.Steps = buildRestoreStepsWithMode(plan.Archives, mode)
	if mode.PreflightCheckCode == "readonly_restore_plan" {
		return
	}
	for index := range plan.Checks {
		if plan.Checks[index].Code == "readonly_restore_plan" {
			plan.Checks[index].Code = mode.PreflightCheckCode
			plan.Checks[index].Message = mode.PreflightMessage
			return
		}
	}
}

func restoreTargetsByRole(overview model.ContainerOpsDockerOverview, archives []model.ContainerOpsBackupArchive) map[string]model.ContainerOpsDockerContainer {
	targets := make(map[string]model.ContainerOpsDockerContainer, 2)
	if cpa, ok := selectBackupContainer(overview, "cpa", "cli-proxy-api"); ok {
		targets["cpa"] = cpa
	}
	if hasRestoreArchiveRole(archives, "cpamp") {
		if cpamp, ok := selectBackupContainer(overview, "cpamp", "cpa-manager-plus"); ok {
			targets["cpamp"] = cpamp
		}
	}
	return targets
}

func hasRestoreArchiveRole(archives []model.ContainerOpsBackupArchive, role string) bool {
	for _, archive := range archives {
		if archive.Role == role {
			return true
		}
	}
	return false
}

func (c *DockerClient) stopRestoreContainer(ctx context.Context, container model.ContainerOpsDockerContainer) error {
	return c.post(ctx, "/containers/"+url.PathEscape(container.Name)+"/stop", nil, nil)
}

func (c *DockerClient) startRestoreContainer(ctx context.Context, container model.ContainerOpsDockerContainer) error {
	return c.post(ctx, "/containers/"+url.PathEscape(container.Name)+"/start", nil, nil)
}

func (c *DockerClient) restoreContainerArchive(ctx context.Context, container string, archivePath string, destination string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()

	targetPath := filepath.Dir(strings.TrimSpace(destination))
	if targetPath == "." || targetPath == "" {
		targetPath = "/"
	}
	endpoint := fmt.Sprintf(
		"http://docker/containers/%s/archive?path=%s",
		url.PathEscape(container),
		url.QueryEscape(targetPath),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, file)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("docker api status %d", resp.StatusCode)
	}
	return nil
}

func restoreHealthChecks(overview model.ContainerOpsDockerOverview, targets map[string]model.ContainerOpsDockerContainer, mode restoreApplyMode) []model.ContainerOpsRestoreCheck {
	checks := make([]model.ContainerOpsRestoreCheck, 0, len(targets)+1)
	for _, role := range []string{"cpa", "cpamp"} {
		target, ok := targets[role]
		if !ok {
			continue
		}
		container, ok := findContainerByName(overview, target.Name)
		if !ok {
			checks = append(checks, restoreCheck("error", "restore_container_missing_after_start", "Expected restored container was not found after start.", target.Name, true))
			continue
		}
		if container.State != "running" {
			checks = append(checks, restoreCheck("error", "restore_container_not_running", "Expected restored container is not running after start.", target.Name, true))
			continue
		}
		checks = append(checks, restoreCheck("info", "restore_container_running", "Expected restored container is running.", target.Name, false))
	}
	if !restoreChecksBlocking(checks) {
		checks = append(checks, restoreCheck("info", mode.CompletedCheckCode, mode.CompletedMessage, "cpa", false))
	}
	return checks
}

func restoreApplyFailure(plan model.ContainerOpsRestorePlan, mode restoreApplyMode, actionCode string, message string, applied bool) model.ContainerOpsRestorePlan {
	restoreMarkAction(plan.Actions, actionCode, "failed", message)
	plan.Checks = append(plan.Checks, restoreCheck("error", mode.ActionFailedCode, message, actionCode, true))
	plan.Status = mode.FailureStatus
	plan.Applied = applied
	plan.ReadOnly = false
	plan.Destructive = true
	return plan
}

func restoreMarkAction(actions []model.ContainerOpsRestoreAction, code string, status string, message string) {
	for index := range actions {
		if actions[index].Code == code {
			actions[index].Status = status
			actions[index].Message = message
			return
		}
	}
}

func restoreChecksBlocking(checks []model.ContainerOpsRestoreCheck) bool {
	for _, check := range checks {
		if check.Blocking {
			return true
		}
	}
	return false
}

func restoreCheck(severity string, code string, message string, resource string, blocking bool) model.ContainerOpsRestoreCheck {
	return model.ContainerOpsRestoreCheck{
		Severity: severity,
		Code:     code,
		Message:  message,
		Resource: resource,
		Blocking: blocking,
	}
}

func cleanBackupID(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("backupId is required")
	}
	if value == "." || value == ".." {
		return "", fmt.Errorf("invalid backupId %q", value)
	}
	for _, r := range value {
		allowed := r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' ||
			r == '.' ||
			r == '_' ||
			r == '-'
		if !allowed {
			return "", fmt.Errorf("invalid backupId %q", value)
		}
	}
	if filepath.Base(value) != value {
		return "", fmt.Errorf("invalid backupId %q", value)
	}
	return value, nil
}

func backupFilePath(backupDir string, fileName string) (string, error) {
	fileName = strings.TrimSpace(fileName)
	if fileName == "" || filepath.Base(fileName) != fileName {
		return "", fmt.Errorf("invalid archive file name %q", fileName)
	}
	target := filepath.Clean(filepath.Join(backupDir, fileName))
	rel, err := filepath.Rel(backupDir, target)
	if err != nil {
		return "", err
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", fmt.Errorf("archive path escapes backup directory: %s", fileName)
	}
	return target, nil
}

func scanTarForCPAMPData(filePath string) (bool, bool, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return false, false, err
	}
	defer file.Close()

	var foundUsageDB bool
	var foundDataKey bool
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, false, err
		}
		if header.FileInfo().IsDir() {
			continue
		}
		switch filepath.Base(header.Name) {
		case "usage.sqlite":
			foundUsageDB = true
		case "data.key":
			foundDataKey = true
		}
	}
	return foundUsageDB, foundDataKey, nil
}

func firstNonEmptyValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
