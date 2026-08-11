package containerops

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

const (
	deployActionRenderFiles = "render_files"
	deployActionPullImages  = "pull_images"
	deployActionStart       = "start_services"
)

func (s *Service) DeployPlan(ctx context.Context, request model.ContainerOpsDeployRequest) (model.ContainerOpsDeployPlan, error) {
	agent := s.agentInfo(ctx)
	baseChecks := make([]model.ContainerOpsDeployCheck, 0)
	var overview *model.ContainerOpsDockerOverview
	if !agent.Configured {
		baseChecks = append(baseChecks, deployCheck("warning", "agent_not_configured", "No cpamp-agent is configured. The compose draft can be rendered, but Docker conflicts cannot be verified.", "", false))
	} else if !agent.Reachable {
		baseChecks = append(baseChecks, deployCheck("warning", "agent_unreachable", "cpamp-agent is configured but unreachable. The compose draft can be rendered, but Docker conflicts cannot be verified.", firstNonEmpty(agent.BaseURL, agent.Error), false))
	} else {
		var next model.ContainerOpsDockerOverview
		if err := s.getAgentJSON(ctx, "/docker/overview", &next); err != nil {
			baseChecks = append(baseChecks, deployCheck("warning", "docker_precheck_failed", "Docker resources could not be inspected through cpamp-agent. Review the compose draft before applying it manually.", err.Error(), false))
		} else {
			overview = &next
		}
	}

	plan := buildDeployPlan(agent, overview, standardResources(), newAPIInfo(), baseChecks)
	if !request.Apply {
		return plan, nil
	}
	if !agent.Configured {
		return model.ContainerOpsDeployPlan{}, errors.New("container ops agent is not configured")
	}
	if !agent.Reachable {
		return model.ContainerOpsDeployPlan{}, fmt.Errorf("container ops agent is not reachable: %s", firstNonEmpty(agent.Error, "unknown error"))
	}
	if plan.Status == "blocked" {
		return plan, nil
	}

	action := strings.TrimSpace(request.Action)
	if action == "" {
		action = deployActionRenderFiles
	}
	operation := "deploy_" + action
	_, finish, err := s.beginLifecycle(ctx, operation, action, agent, request)
	if err != nil {
		return model.ContainerOpsDeployPlan{}, err
	}
	var result model.ContainerOpsDeployPlan
	switch action {
	case deployActionRenderFiles:
		result, err = s.renderDeployFiles(ctx, plan)
	case deployActionPullImages:
		result, err = s.pullDeployImages(ctx, plan)
	case deployActionStart:
		result, err = s.startDeployServices(ctx, plan)
	default:
		finish(lifecycleStatusFailed, "unsupported_action", fmt.Sprintf("unsupported deploy action %q", request.Action), map[string]any{"error": "unsupported_action"})
		return model.ContainerOpsDeployPlan{}, fmt.Errorf("unsupported deploy action %q", request.Action)
	}
	if err != nil {
		finish(lifecycleStatusFailed, "agent_request_failed", err.Error(), map[string]any{"error": err.Error()})
		return model.ContainerOpsDeployPlan{}, err
	}
	result.Lifecycle = attachLifecycle(finish(lifecycleStatusForResult(result.Status), result.Status, "CPA deploy action finished with status "+result.Status+".", result))
	return result, nil
}

func (s *Service) renderDeployFiles(ctx context.Context, plan model.ContainerOpsDeployPlan) (model.ContainerOpsDeployPlan, error) {
	var rendered struct {
		Status string                         `json:"status"`
		Files  []model.ContainerOpsDeployFile `json:"files"`
	}
	if err := s.postAgentJSON(ctx, "/deploys/cpa/render", model.ContainerOpsDeployRenderRequest{
		Manifest: plan.Manifest,
		Compose:  plan.Compose,
	}, &rendered); err != nil {
		return model.ContainerOpsDeployPlan{}, fmt.Errorf("render CPA stack deploy files: %w", err)
	}
	plan.Status = firstNonEmpty(rendered.Status, "rendered")
	plan.Files = rendered.Files
	plan.Applied = true
	plan.ReadOnly = false
	plan.Checks = append(plan.Checks, deployCheck("info", "deploy_files_rendered", "The standard CPA stack compose, manifest, and environment example were written by cpamp-agent.", "", false))
	return plan, nil
}

func (s *Service) pullDeployImages(ctx context.Context, plan model.ContainerOpsDeployPlan) (model.ContainerOpsDeployPlan, error) {
	var pulled struct {
		Status     string                        `json:"status"`
		ImagePulls []model.ContainerOpsImagePull `json:"imagePulls"`
	}
	if err := s.postAgentJSON(ctx, "/deploys/cpa/pull-images", model.ContainerOpsDeployRenderRequest{
		Manifest: plan.Manifest,
		Compose:  plan.Compose,
	}, &pulled); err != nil {
		return model.ContainerOpsDeployPlan{}, fmt.Errorf("pull CPA stack deploy images: %w", err)
	}
	plan.Status = firstNonEmpty(pulled.Status, "images_pulled")
	plan.ImagePulls = pulled.ImagePulls
	plan.Applied = true
	plan.ReadOnly = false
	plan.Checks = append(plan.Checks, deployCheck("info", "deploy_images_pulled", "cpamp-agent pulled the standard CPA and CPAMP images. No containers were started.", "", false))
	return plan, nil
}

func (s *Service) startDeployServices(ctx context.Context, plan model.ContainerOpsDeployPlan) (model.ContainerOpsDeployPlan, error) {
	var started struct {
		Status   string                            `json:"status"`
		Checks   []model.ContainerOpsDeployCheck   `json:"checks"`
		Actions  []model.ContainerOpsDeployAction  `json:"actions"`
		Overview *model.ContainerOpsDockerOverview `json:"overview,omitempty"`
	}
	if err := s.postAgentJSON(ctx, "/deploys/cpa/start", model.ContainerOpsDeployRenderRequest{
		Manifest: plan.Manifest,
		Compose:  plan.Compose,
	}, &started); err != nil {
		return model.ContainerOpsDeployPlan{}, fmt.Errorf("start CPA stack deploy services: %w", err)
	}
	plan.Status = firstNonEmpty(started.Status, "started")
	plan.Checks = append(plan.Checks, started.Checks...)
	plan.Actions = started.Actions
	plan.Overview = started.Overview
	plan.Applied = plan.Status != "blocked"
	plan.ReadOnly = false
	plan.Destructive = false
	return plan, nil
}

func buildDeployPlan(
	agent model.ContainerOpsAgentInfo,
	overview *model.ContainerOpsDockerOverview,
	resources model.ContainerOpsStandardResource,
	newAPI model.ContainerOpsNewAPIInfo,
	baseChecks []model.ContainerOpsDeployCheck,
) model.ContainerOpsDeployPlan {
	checks := append([]model.ContainerOpsDeployCheck{}, baseChecks...)
	addCheck := func(severity string, code string, message string, resource string, blocking bool) {
		checks = append(checks, deployCheck(severity, code, message, resource, blocking))
	}

	var newAPIContainer model.ContainerOpsDockerContainer
	newAPIFound := false
	if overview != nil {
		addCheck("info", "docker_precheck_ready", "Docker resources were inspected through cpamp-agent.", "", false)
		addExistingRoleDeployChecks(*overview, resources, addCheck)
		if candidate, ok := selectRoleContainer(*overview, roleNewAPI, "new-api"); ok {
			newAPIContainer = candidate
			newAPIFound = true
			addCheck("info", "newapi_detected", "NewAPI was detected. CPAMP will only provide the recommended internal route; it will not back up or mutate NewAPI data.", candidate.Name, false)
		}
		if network, ok := findNetwork(*overview, resources.Network); ok {
			if network.Driver != "bridge" {
				addCheck("error", "standard_network_driver_mismatch", "The standard network name already exists with a non-bridge driver.", resources.Network, true)
			} else if !network.Managed {
				addCheck("error", "standard_network_conflict", "The standard network name already exists but is not CPAMP-managed. Resolve ownership before deploying a clean stack.", resources.Network, true)
			} else {
				addCheck("info", "standard_network_reusable", "The standard CPAMP network already exists and can be reused.", resources.Network, false)
			}
		} else {
			addCheck("info", "standard_network_planned", "The compose draft will create the standard CPAMP bridge network.", resources.Network, false)
		}
	}
	addCheck("info", "readonly_deploy_plan", "The deploy preflight starts in plan mode. apply=true requires an explicit CPA deploy action: render files, pull images, or start services after the stack .env is ready.", "", false)

	status := "ready"
	if deployBlockingCount(checks) > 0 {
		status = "blocked"
	} else if deployWarningCount(checks) > 0 {
		status = "ready_with_warnings"
	}

	manifest := buildManifest(
		resources,
		newAPI,
		model.ContainerOpsDockerContainer{},
		false,
		model.ContainerOpsDockerContainer{},
		false,
		model.ContainerOpsDockerContainer{},
		false,
		newAPIContainer,
		newAPIFound,
	)

	return model.ContainerOpsDeployPlan{
		Agent:       agent,
		Status:      status,
		Manifest:    manifest,
		Compose:     buildDeployComposeDraft(resources, newAPI),
		Checks:      checks,
		Steps:       buildDeploySteps(resources, newAPI),
		Applied:     false,
		Destructive: false,
		ReadOnly:    true,
		Overview:    overview,
	}
}

func buildDeployComposeDraft(resources model.ContainerOpsStandardResource, newAPI model.ContainerOpsNewAPIInfo) model.ContainerOpsComposeDraft {
	var builder strings.Builder
	line := func(format string, args ...any) {
		builder.WriteString(fmt.Sprintf(format, args...))
		builder.WriteByte('\n')
	}

	cpampImage := defaultImageForRole(roleCPAMP)
	line("name: %s", resources.ComposeProject)
	line("services:")
	line("  %s:", resources.CPAService)
	line("    image: %s", defaultImageForRole(roleCPA))
	line("    container_name: %s", resources.CPAService)
	line("    restart: unless-stopped")
	line("    network_mode: host")
	writeComposeLabels(&builder, roleCPA)
	line("    volumes:")
	line("      - %s", quoteYAML("cpa-data:/app/data"))
	line("")
	line("  %s:", resources.CPAMPService)
	line("    image: %s", cpampImage)
	line("    container_name: %s", resources.CPAMPService)
	line("    restart: unless-stopped")
	writeComposeLabels(&builder, roleCPAMP)
	line("    environment:")
	line("      HTTP_ADDR: %s", quoteYAML("0.0.0.0:18317"))
	line("      CPA_UPSTREAM_URL: %s", quoteYAML(strings.TrimSuffix(newAPI.RecommendedBaseURL, "/v1")))
	line("      CPA_MANAGEMENT_KEY: %s", quoteYAML("${CPA_MANAGEMENT_KEY:?set CPA_MANAGEMENT_KEY}"))
	line("      CPA_MANAGER_ADMIN_KEY: %s", quoteYAML("${CPA_MANAGER_ADMIN_KEY:?set CPA_MANAGER_ADMIN_KEY}"))
	line("      CPAMP_AGENT_URL: %s", quoteYAML("http://host.docker.internal:18417"))
	line("      CPAMP_AGENT_TOKEN: %s", quoteYAML("${CPAMP_AGENT_TOKEN:?set CPAMP_AGENT_TOKEN}"))
	line("    extra_hosts:")
	line("      - %s", quoteYAML("host.docker.internal:host-gateway"))
	line("    networks:")
	line("      - %s", resources.Network)
	line("    ports:")
	line("      - %s", quoteYAML("18317:18317"))
	line("    volumes:")
	line("      - %s", quoteYAML("cpa-manager-plus-data:/data"))
	line("    depends_on:")
	line("      - %s", resources.CPAService)
	line("      - %s", resources.AgentService)
	line("")
	line("  %s:", resources.AgentService)
	line("    image: %s", cpampImage)
	line("    container_name: %s", resources.AgentService)
	line("    entrypoint: [%s]", quoteYAML("cpamp-agent"))
	line("    restart: unless-stopped")
	line("    network_mode: host")
	line("    cap_add:")
	line("      - NET_ADMIN")
	line("      - NET_RAW")
	writeComposeLabels(&builder, roleAgent)
	line("    environment:")
	line("      CPAMP_AGENT_ADDR: %s", quoteYAML("0.0.0.0:18417"))
	line("      CPAMP_STACK_ROOT: %s", quoteYAML(resources.StackRoot))
	line("      CPAMP_BACKUP_ROOT: %s", quoteYAML(resources.BackupRoot))
	line("      CPAMP_AGENT_TOKEN: %s", quoteYAML("${CPAMP_AGENT_TOKEN:?set CPAMP_AGENT_TOKEN}"))
	line("      DOCKER_HOST: %s", quoteYAML("unix:///var/run/docker.sock"))
	line("    volumes:")
	line("      - %s", quoteYAML("/var/run/docker.sock:/var/run/docker.sock"))
	line("      - %s", quoteYAML(resources.StackRoot+":"+resources.StackRoot))
	line("      - %s", quoteYAML(resources.BackupRoot+":"+resources.BackupRoot))
	line("")
	line("networks:")
	line("  %s:", resources.Network)
	line("    name: %s", resources.Network)
	line("    driver: bridge")
	line("    labels:")
	line("      com.cpamp.managed: %s", quoteYAML("true"))
	line("      com.cpamp.stack: %s", quoteYAML("cpa"))
	line("")
	line("volumes:")
	line("  cpa-data:")
	line("  cpa-manager-plus-data:")

	return model.ContainerOpsComposeDraft{
		FileName:    "compose.deploy-preview.yml",
		ProjectName: resources.ComposeProject,
		NetworkName: resources.Network,
		Services:    []string{resources.CPAService, resources.CPAMPService, resources.AgentService},
		Content:     strings.TrimSpace(builder.String()) + "\n",
	}
}

func buildDeploySteps(resources model.ContainerOpsStandardResource, newAPI model.ContainerOpsNewAPIInfo) []model.ContainerOpsDeployStep {
	return []model.ContainerOpsDeployStep{
		{Order: 1, Code: "create_stack_root", Title: "Create the stack directory", Target: resources.StackRoot},
		{Order: 2, Code: "write_env_file", Title: "Write the environment file with admin and agent secrets", Target: resources.StackRoot + "/.env"},
		{Order: 3, Code: "write_compose_file", Title: "Write the rendered compose file", Target: resources.StackRoot + "/compose.yml"},
		{Order: 4, Code: "pull_images", Title: "Pull CPA and CPAMP images", Target: strings.Join([]string{defaultImageForRole(roleCPA), defaultImageForRole(roleCPAMP)}, ", ")},
		{Order: 5, Code: "start_services", Title: "Start the standard CPA stack", Target: resources.ComposeProject},
		{Order: 6, Code: "healthcheck_services", Title: "Verify CPA, CPAMP, and Agent health", Target: resources.Network},
		{Order: 7, Code: "configure_manager", Title: "Bind CPAMP to the CPA service using the management key", Target: strings.TrimSuffix(newAPI.RecommendedBaseURL, "/v1")},
		{Order: 8, Code: "update_newapi_route", Title: "Point NewAPI channels at the internal CPA base URL", Target: newAPI.RecommendedBaseURL},
	}
}

func addExistingRoleDeployChecks(
	overview model.ContainerOpsDockerOverview,
	resources model.ContainerOpsStandardResource,
	addCheck func(severity string, code string, message string, resource string, blocking bool),
) {
	for _, spec := range []struct {
		role         string
		target       string
		blockCode    string
		blockMessage string
	}{
		{
			role:         roleCPA,
			target:       resources.CPAService,
			blockCode:    "cpa_already_exists",
			blockMessage: "A non-standard or unmanaged CPA container already exists. Use import or network standardization before enabling deploy execution.",
		},
		{
			role:         roleCPAMP,
			target:       resources.CPAMPService,
			blockCode:    "cpamp_already_exists",
			blockMessage: "A non-standard or unmanaged CPAMP container already exists. The clean deploy flow must not overwrite it.",
		},
		{
			role:         roleAgent,
			target:       resources.AgentService,
			blockCode:    "agent_already_exists",
			blockMessage: "A non-standard or unmanaged cpamp-agent container already exists. The clean deploy flow must not overwrite it.",
		},
	} {
		blockingNames := make([]string, 0)
		reusableNames := make([]string, 0)
		for _, container := range overview.Containers {
			if container.Role != spec.role {
				continue
			}
			if deployReusableStandardContainer(container, spec.target) {
				reusableNames = append(reusableNames, container.Name)
			} else {
				blockingNames = append(blockingNames, container.Name)
			}
		}
		if len(reusableNames) > 0 {
			addCheck("info", "standard_service_reusable", "A standard CPAMP-managed service already exists and can be reused by the deploy state machine.", strings.Join(reusableNames, ", "), false)
		}
		if len(blockingNames) > 0 {
			addCheck("error", spec.blockCode, spec.blockMessage, strings.Join(blockingNames, ", "), true)
		}
	}
}

func deployReusableStandardContainer(container model.ContainerOpsDockerContainer, targetService string) bool {
	return container.Managed && strings.EqualFold(container.Name, targetService)
}

func roleNames(overview model.ContainerOpsDockerOverview, role string) []string {
	names := make([]string, 0)
	for _, container := range overview.Containers {
		if container.Role == role {
			names = append(names, container.Name)
		}
	}
	return names
}

func deployCheck(severity string, code string, message string, resource string, blocking bool) model.ContainerOpsDeployCheck {
	return model.ContainerOpsDeployCheck{
		Severity: severity,
		Code:     code,
		Message:  message,
		Resource: resource,
		Blocking: blocking,
	}
}

func deployBlockingCount(checks []model.ContainerOpsDeployCheck) int {
	count := 0
	for _, check := range checks {
		if check.Blocking {
			count++
		}
	}
	return count
}

func deployWarningCount(checks []model.ContainerOpsDeployCheck) int {
	count := 0
	for _, check := range checks {
		if check.Severity == "warning" {
			count++
		}
	}
	return count
}
