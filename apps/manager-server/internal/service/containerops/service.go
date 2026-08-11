package containerops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

const (
	modeReadOnly = "read_only"
	modeAgent    = "agent"
)

type Options struct {
	AgentURL   string
	AgentToken string
	AuditStore auditStore
	Now        func() time.Time
}

type auditStore interface {
	CreateContainerOpsAudit(ctx context.Context, entry model.ContainerOpsAuditEntry) (model.ContainerOpsAuditEntry, error)
	UpdateContainerOpsAudit(ctx context.Context, entry model.ContainerOpsAuditEntry) error
	ListContainerOpsAudits(ctx context.Context, limit int) ([]model.ContainerOpsAuditEntry, error)
	CreateContainerOpsUpgradeTask(ctx context.Context, task model.ContainerOpsUpgradeTask) (model.ContainerOpsUpgradeTask, error)
	GetContainerOpsUpgradeTask(ctx context.Context, taskID string) (model.ContainerOpsUpgradeTask, bool, error)
	UpdateContainerOpsUpgradeTask(ctx context.Context, task model.ContainerOpsUpgradeTask) error
	ListContainerOpsUpgradeTasks(ctx context.Context, limit int) ([]model.ContainerOpsUpgradeTask, error)
}

type Service struct {
	agentURL     string
	agentToken   string
	client       *http.Client
	auditStore   auditStore
	now          func() time.Time
	lifecycleMu  sync.Mutex
	lifecycle    model.ContainerOpsLifecycleState
	lifecycleSeq int64
}

func New(options Options) *Service {
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		agentURL:   strings.TrimSpace(options.AgentURL),
		agentToken: strings.TrimSpace(options.AgentToken),
		client:     &http.Client{Timeout: 5 * time.Second},
		auditStore: options.AuditStore,
		now:        now,
	}
}

func (s *Service) Info(ctx context.Context) (model.ContainerOpsInfo, error) {
	agent := s.agentInfo(ctx)
	return model.ContainerOpsInfo{
		Enabled:           true,
		Mode:              resolveMode(agent.Configured),
		Agent:             agent,
		StandardResources: standardResources(),
		NewAPI:            newAPIInfo(),
		ManagedStack:      "cpa",
		SupportedScopes: []string{
			"single-host-docker",
			"cpa-stack-lifecycle",
			"newapi-network-check",
			"cpa-stack-deploy-plan",
			"cpa-stack-backup",
			"cpa-stack-restore-plan",
			"cpa-stack-restore-apply",
			"cpa-stack-rollback",
			"cpa-network-standardize",
			"cpa-stack-upgrade-prepare",
			"cpa-stack-upgrade-state",
			"cpa-stack-upgrade-runner",
			"source-ip-egress-one-click",
			"source-ip-account-binding",
		},
		UnsupportedScopes: []string{
			"multi-host-scheduling",
			"kubernetes",
			"generic-container-management",
			"newapi-data-backup",
		},
		DestructiveActions: agent.Configured && agent.Reachable && !agent.ReadOnly,
		Lifecycle:          s.currentLifecycle(),
	}, nil
}

func (s *Service) Agent(ctx context.Context) (model.ContainerOpsAgentInfo, error) {
	return s.agentInfo(ctx), nil
}

func (s *Service) Audits(ctx context.Context, limit int) ([]model.ContainerOpsAuditEntry, error) {
	if s.auditStore == nil {
		return []model.ContainerOpsAuditEntry{}, nil
	}
	return s.auditStore.ListContainerOpsAudits(ctx, limit)
}

func (s *Service) UpgradeTasks(ctx context.Context, limit int) ([]model.ContainerOpsUpgradeTask, error) {
	if s.auditStore == nil {
		return []model.ContainerOpsUpgradeTask{}, nil
	}
	return s.auditStore.ListContainerOpsUpgradeTasks(ctx, limit)
}

func (s *Service) Discover(ctx context.Context) (model.ContainerOpsDiscovery, error) {
	agent := s.agentInfo(ctx)
	if !agent.Configured {
		return model.ContainerOpsDiscovery{}, errors.New("container ops agent is not configured")
	}
	if !agent.Reachable {
		return model.ContainerOpsDiscovery{}, fmt.Errorf("container ops agent is not reachable: %s", firstNonEmpty(agent.Error, "unknown error"))
	}
	var overview model.ContainerOpsDockerOverview
	if err := s.getAgentJSON(ctx, "/docker/overview", &overview); err != nil {
		return model.ContainerOpsDiscovery{}, fmt.Errorf("discover docker resources: %w", err)
	}
	return model.ContainerOpsDiscovery{
		Agent:             agent,
		Docker:            overview,
		NewAPI:            newAPIInfo(),
		RecommendedAction: recommendedAction(overview),
	}, nil
}

func (s *Service) Backup(ctx context.Context) (model.ContainerOpsBackupResult, error) {
	agent := s.agentInfo(ctx)
	if !agent.Configured {
		return model.ContainerOpsBackupResult{}, errors.New("container ops agent is not configured")
	}
	if !agent.Reachable {
		return model.ContainerOpsBackupResult{}, fmt.Errorf("container ops agent is not reachable: %s", firstNonEmpty(agent.Error, "unknown error"))
	}
	_, finish, err := s.beginLifecycle(ctx, "backup", "archive", agent, map[string]any{})
	if err != nil {
		return model.ContainerOpsBackupResult{}, err
	}
	var result model.ContainerOpsBackupResult
	if err := s.postAgentJSON(ctx, "/backups/cpa", map[string]any{}, &result); err != nil {
		finish(lifecycleStatusFailed, "archive_failed", err.Error(), map[string]any{"error": err.Error()})
		return model.ContainerOpsBackupResult{}, fmt.Errorf("create CPA stack backup: %w", err)
	}
	result.Agent = agent
	result.Lifecycle = attachLifecycle(finish(lifecycleStatusForResult(result.Status), result.Status, "CPA stack backup finished with status "+result.Status+".", result))
	return result, nil
}

func (s *Service) RestorePlan(ctx context.Context, request model.ContainerOpsRestoreRequest) (model.ContainerOpsRestorePlan, error) {
	agent := s.agentInfo(ctx)
	if !agent.Configured {
		return model.ContainerOpsRestorePlan{}, errors.New("container ops agent is not configured")
	}
	if !agent.Reachable {
		return model.ContainerOpsRestorePlan{}, fmt.Errorf("container ops agent is not reachable: %s", firstNonEmpty(agent.Error, "unknown error"))
	}
	request.BackupID = strings.TrimSpace(request.BackupID)
	if request.BackupID == "" {
		return model.ContainerOpsRestorePlan{}, errors.New("backupId is required")
	}
	var plan model.ContainerOpsRestorePlan
	endpoint := "/restores/cpa/plan"
	action := "create CPA stack restore plan"
	var finish func(status string, phase string, message string, result any) model.ContainerOpsLifecycleState
	if request.Apply {
		_, finishLifecycle, err := s.beginLifecycle(ctx, "restore", "apply", agent, request)
		if err != nil {
			return model.ContainerOpsRestorePlan{}, err
		}
		finish = finishLifecycle
		endpoint = "/restores/cpa/apply"
		action = "apply CPA stack restore"
	}
	if err := s.postAgentJSON(ctx, endpoint, request, &plan); err != nil {
		if finish != nil {
			finish(lifecycleStatusFailed, "agent_request_failed", err.Error(), map[string]any{"error": err.Error()})
		}
		return model.ContainerOpsRestorePlan{}, fmt.Errorf("%s: %w", action, err)
	}
	plan.Agent = agent
	if finish != nil {
		plan.Lifecycle = attachLifecycle(finish(lifecycleStatusForResult(plan.Status), plan.Status, "CPA stack restore finished with status "+plan.Status+".", plan))
	}
	return plan, nil
}

func (s *Service) Rollback(ctx context.Context, request model.ContainerOpsRollbackRequest) (model.ContainerOpsRestorePlan, error) {
	agent := s.agentInfo(ctx)
	if !agent.Configured {
		return model.ContainerOpsRestorePlan{}, errors.New("container ops agent is not configured")
	}
	if !agent.Reachable {
		return model.ContainerOpsRestorePlan{}, fmt.Errorf("container ops agent is not reachable: %s", firstNonEmpty(agent.Error, "unknown error"))
	}
	request.BackupID = strings.TrimSpace(request.BackupID)
	if request.BackupID == "" {
		return model.ContainerOpsRestorePlan{}, errors.New("backupId is required")
	}
	_, finish, err := s.beginLifecycle(ctx, "rollback", "apply", agent, request)
	if err != nil {
		return model.ContainerOpsRestorePlan{}, err
	}
	var plan model.ContainerOpsRestorePlan
	if err := s.postAgentJSON(ctx, "/rollbacks/cpa/apply", request, &plan); err != nil {
		finish(lifecycleStatusFailed, "agent_request_failed", err.Error(), map[string]any{"error": err.Error()})
		return model.ContainerOpsRestorePlan{}, fmt.Errorf("apply CPA stack rollback: %w", err)
	}
	plan.Agent = agent
	plan.Lifecycle = attachLifecycle(finish(lifecycleStatusForResult(plan.Status), plan.Status, "CPA stack rollback finished with status "+plan.Status+".", plan))
	return plan, nil
}

func (s *Service) StandardizeNetwork(ctx context.Context, request model.ContainerOpsNetworkStandardizeRequest) (model.ContainerOpsNetworkStandardizeResult, error) {
	agent := s.agentInfo(ctx)
	if !agent.Configured {
		return model.ContainerOpsNetworkStandardizeResult{}, errors.New("container ops agent is not configured")
	}
	if !agent.Reachable {
		return model.ContainerOpsNetworkStandardizeResult{}, fmt.Errorf("container ops agent is not reachable: %s", firstNonEmpty(agent.Error, "unknown error"))
	}
	request.BackupID = strings.TrimSpace(request.BackupID)
	if request.BackupID == "" {
		return model.ContainerOpsNetworkStandardizeResult{}, errors.New("backupId is required")
	}
	var finish func(status string, phase string, message string, result any) model.ContainerOpsLifecycleState
	if request.Apply {
		_, finishLifecycle, err := s.beginLifecycle(ctx, "network_standardize", "apply", agent, request)
		if err != nil {
			return model.ContainerOpsNetworkStandardizeResult{}, err
		}
		finish = finishLifecycle
	}
	var result model.ContainerOpsNetworkStandardizeResult
	if err := s.postAgentJSON(ctx, "/networks/cpa/standardize", request, &result); err != nil {
		if finish != nil {
			finish(lifecycleStatusFailed, "agent_request_failed", err.Error(), map[string]any{"error": err.Error()})
		}
		return model.ContainerOpsNetworkStandardizeResult{}, fmt.Errorf("standardize CPA network: %w", err)
	}
	result.Agent = agent
	if finish != nil {
		result.Lifecycle = attachLifecycle(finish(lifecycleStatusForResult(result.Status), result.Status, "CPA network standardization finished with status "+result.Status+".", result))
	}
	return result, nil
}

func (s *Service) EgressIPs(ctx context.Context) (model.ContainerOpsEgressIPInventory, error) {
	agent := s.agentInfo(ctx)
	if !agent.Configured {
		return model.ContainerOpsEgressIPInventory{}, errors.New("container ops agent is not configured")
	}
	if !agent.Reachable {
		return model.ContainerOpsEgressIPInventory{}, fmt.Errorf("container ops agent is not reachable: %s", firstNonEmpty(agent.Error, "unknown error"))
	}
	var inventory model.ContainerOpsEgressIPInventory
	if err := s.getAgentJSON(ctx, "/egress/ips", &inventory); err != nil {
		return model.ContainerOpsEgressIPInventory{}, fmt.Errorf("inspect egress IPs: %w", err)
	}
	inventory.Agent = agent
	return inventory, nil
}

func (s *Service) EnsureSourceIP(ctx context.Context, request model.ContainerOpsSourceIPRequest) (model.ContainerOpsSourceIPResult, error) {
	return s.sourceIPAction(ctx, "source_ip_ensure", "ensure", "/egress/source-ip/ensure", request)
}

func (s *Service) CheckSourceIP(ctx context.Context, request model.ContainerOpsSourceIPRequest) (model.ContainerOpsSourceIPResult, error) {
	agent := s.agentInfo(ctx)
	if !agent.Configured {
		return model.ContainerOpsSourceIPResult{}, errors.New("container ops agent is not configured")
	}
	if !agent.Reachable {
		return model.ContainerOpsSourceIPResult{}, fmt.Errorf("container ops agent is not reachable: %s", firstNonEmpty(agent.Error, "unknown error"))
	}
	var result model.ContainerOpsSourceIPResult
	if err := s.postAgentJSON(ctx, "/egress/source-ip/check", request, &result); err != nil {
		return model.ContainerOpsSourceIPResult{}, fmt.Errorf("check source IP: %w", err)
	}
	result.Agent = agent
	return result, nil
}

func (s *Service) RemoveSourceIP(ctx context.Context, request model.ContainerOpsSourceIPRequest) (model.ContainerOpsSourceIPResult, error) {
	return s.sourceIPAction(ctx, "source_ip_remove", "remove", "/egress/source-ip/remove", request)
}

func (s *Service) sourceIPAction(ctx context.Context, operation string, phase string, endpoint string, request model.ContainerOpsSourceIPRequest) (model.ContainerOpsSourceIPResult, error) {
	agent := s.agentInfo(ctx)
	if !agent.Configured {
		return model.ContainerOpsSourceIPResult{}, errors.New("container ops agent is not configured")
	}
	if !agent.Reachable {
		return model.ContainerOpsSourceIPResult{}, fmt.Errorf("container ops agent is not reachable: %s", firstNonEmpty(agent.Error, "unknown error"))
	}
	_, finish, err := s.beginLifecycle(ctx, operation, phase, agent, request)
	if err != nil {
		return model.ContainerOpsSourceIPResult{}, err
	}
	var result model.ContainerOpsSourceIPResult
	if err := s.postAgentJSON(ctx, endpoint, request, &result); err != nil {
		finish(lifecycleStatusFailed, "agent_request_failed", err.Error(), map[string]any{"error": err.Error()})
		return model.ContainerOpsSourceIPResult{}, fmt.Errorf("%s source IP: %w", phase, err)
	}
	result.Agent = agent
	result.Lifecycle = attachLifecycle(finish(lifecycleStatusForResult(result.Status), result.Status, "Source IP operation finished with status "+result.Status+".", result))
	return result, nil
}

func (s *Service) agentInfo(ctx context.Context) model.ContainerOpsAgentInfo {
	if s.agentURL == "" {
		return model.ContainerOpsAgentInfo{Configured: false, Reachable: false}
	}
	info := model.ContainerOpsAgentInfo{
		Configured: true,
		Reachable:  false,
		BaseURL:    sanitizeAgentURL(s.agentURL),
	}
	if _, err := url.ParseRequestURI(s.agentURL); err != nil {
		info.Error = "invalid agent url"
		return info
	}
	var remote model.ContainerOpsAgentInfo
	if err := s.getAgentJSON(ctx, "/agent/info", &remote); err != nil {
		info.Error = err.Error()
		return info
	}
	remote.Configured = true
	remote.Reachable = true
	remote.BaseURL = info.BaseURL
	if remote.Mode == "" {
		remote.Mode = modeAgent
	}
	return remote
}

func resolveMode(agentConfigured bool) string {
	if agentConfigured {
		return modeAgent
	}
	return modeReadOnly
}

func standardResources() model.ContainerOpsStandardResource {
	return model.ContainerOpsStandardResource{
		ComposeProject: "cpamp-cpa",
		Network:        "cpamp-cpa_default",
		CPAService:     "cli-proxy-api",
		CPAMPService:   "cpa-manager-plus",
		AgentService:   "cpamp-agent",
		StackRoot:      "/opt/cpamp/stacks/cpa",
		BackupRoot:     "/opt/cpamp/backups",
	}
}

func newAPIInfo() model.ContainerOpsNewAPIInfo {
	return model.ContainerOpsNewAPIInfo{
		RecommendedBaseURL: "http://host.docker.internal:8317/v1",
	}
}

func sanitizeAgentURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	parsed.User = nil
	return parsed.String()
}

func (s *Service) getAgentJSON(ctx context.Context, path string, target any) error {
	endpoint, err := joinURL(s.agentURL, path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if s.agentToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.agentToken)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("agent status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return err
	}
	return nil
}

func (s *Service) postAgentJSON(ctx context.Context, path string, body any, target any) error {
	endpoint, err := joinURL(s.agentURL, path)
	if err != nil {
		return err
	}
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &payload)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.agentToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.agentToken)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("agent status %d", resp.StatusCode)
	}
	if target == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return err
	}
	return nil
}

func joinURL(baseURL string, path string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + strings.TrimLeft(path, "/")
	return parsed.String(), nil
}

func recommendedAction(overview model.ContainerOpsDockerOverview) string {
	if overview.Summary.CPACount == 0 {
		return "deploy_cpa_stack"
	}
	if overview.Summary.NewAPICount > 0 {
		return "verify_newapi_internal_route"
	}
	return "import_existing_cpa_stack"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
