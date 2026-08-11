package containeropsagent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/response"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

const (
	deployStatusBlocked = "blocked"
	deployStatusStarted = "started"
)

type DeployStartOptions struct {
	StackRoot  string
	BackupRoot string
	Request    model.ContainerOpsDeployRenderRequest
}

type DeployStartResult struct {
	Status   string                            `json:"status"`
	Checks   []model.ContainerOpsDeployCheck   `json:"checks"`
	Actions  []model.ContainerOpsDeployAction  `json:"actions"`
	Overview *model.ContainerOpsDockerOverview `json:"overview,omitempty"`
}

type deployServiceSpec struct {
	Role         string
	Name         string
	Image        string
	Env          []string
	Entrypoint   []string
	Cmd          []string
	Ports        map[string]string
	VolumeMounts map[string]string
	Binds        []string
	ExtraHosts   []string
	CapAdd       []string
	HostNetwork  bool
	StartOrder   int
}

func (s *Server) startCPADeployServices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.MethodNotAllowed(w)
		return
	}
	var request model.ContainerOpsDeployRenderRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		response.Error(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.docker.StartCPADeployServices(r.Context(), DeployStartOptions{
		StackRoot:  s.stackRoot,
		BackupRoot: s.backupRoot,
		Request:    request,
	})
	if err != nil {
		response.Error(w, http.StatusBadGateway, err)
		return
	}
	response.JSON(w, http.StatusOK, result)
}

func (c *DockerClient) StartCPADeployServices(ctx context.Context, options DeployStartOptions) (DeployStartResult, error) {
	request := options.Request
	if err := validateDeployRenderRequest(request); err != nil {
		return DeployStartResult{}, err
	}
	if _, err := deployPullImages(request.Manifest); err != nil {
		return DeployStartResult{}, err
	}

	root := cleanStackRoot(options.StackRoot)
	backupRoot := cleanBackupRoot(firstNonEmptyValue(options.BackupRoot, request.Manifest.BackupRoot))
	result := newDeployStartResult(request.Manifest)
	env, envChecks := readDeployEnv(root)
	result.Checks = append(result.Checks, validateDeployStackFiles(root)...)
	result.Checks = append(result.Checks, envChecks...)
	if deployChecksBlocking(result.Checks) {
		result.Status = deployStatusBlocked
		return result, nil
	}

	overview, err := c.Overview(ctx)
	if err != nil {
		return DeployStartResult{}, fmt.Errorf("discover docker resources: %w", err)
	}
	result.Checks = append(result.Checks, validateDeployStartOverview(overview, request.Manifest)...)
	if deployChecksBlocking(result.Checks) {
		result.Status = deployStatusBlocked
		result.Overview = &overview
		return result, nil
	}

	specs := deployStartSpecs(request.Manifest, env, root, backupRoot)
	if err := c.applyDeployStart(ctx, &result, overview, specs); err != nil {
		return DeployStartResult{}, err
	}
	nextOverview, err := c.Overview(ctx)
	if err != nil {
		return DeployStartResult{}, fmt.Errorf("verify started services: %w", err)
	}
	result.Overview = &nextOverview
	result.Checks = append(result.Checks, healthcheckDeployServices(nextOverview, specs)...)
	if deployChecksBlocking(result.Checks) {
		deployMarkAction(result.Actions, "healthcheck_services", "failed", "One or more standard CPA stack containers failed the running-state health check.")
		result.Status = "start_failed"
		return result, nil
	}
	deployMarkAction(result.Actions, "healthcheck_services", "applied", "CPA, CPAMP, and Agent containers are running.")
	result.Status = deployStatusStarted
	return result, nil
}

func newDeployStartResult(manifest model.ContainerOpsStackManifest) DeployStartResult {
	actions := make([]model.ContainerOpsDeployAction, 0, 10)
	add := func(code string, target string, message string) {
		actions = append(actions, model.ContainerOpsDeployAction{
			Order:   len(actions) + 1,
			Code:    code,
			Target:  target,
			Status:  "planned",
			Message: message,
		})
	}
	add("create_standard_network", manifest.Network, "Create the standard CPAMP CPA bridge network if it is missing.")
	add("create_cpa_volume", deployVolumeName(manifest.ComposeProject, "cpa-data"), "Create the standard CPA data volume.")
	add("create_cpamp_volume", deployVolumeName(manifest.ComposeProject, "cpa-manager-plus-data"), "Create the standard CPAMP data volume.")
	add("create_cpa_container", "cli-proxy-api", "Create the standard CPA container.")
	add("create_cpamp_container", "cpa-manager-plus", "Create the standard CPAMP container.")
	add("create_agent_container", "cpamp-agent", "Create the standard cpamp-agent container.")
	add("start_cpa_container", "cli-proxy-api", "Start the CPA container.")
	add("start_agent_container", "cpamp-agent", "Start the cpamp-agent container.")
	add("start_cpamp_container", "cpa-manager-plus", "Start the CPAMP container.")
	add("healthcheck_services", manifest.Network, "Verify CPA, CPAMP, and Agent containers are running.")
	return DeployStartResult{Status: "planned", Actions: actions}
}

func validateDeployStackFiles(root string) []model.ContainerOpsDeployCheck {
	checks := make([]model.ContainerOpsDeployCheck, 0, 3)
	for _, name := range []string{"compose.yml", "stack.manifest.json"} {
		path := filepath.Join(root, name)
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			checks = append(checks, deployAgentCheck("error", "deploy_stack_file_missing", fmt.Sprintf("Required deploy stack file %s is missing.", name), path, true))
		} else {
			checks = append(checks, deployAgentCheck("info", "deploy_stack_file_ready", fmt.Sprintf("Required deploy stack file %s exists.", name), path, false))
		}
	}
	return checks
}

func readDeployEnv(root string) (map[string]string, []model.ContainerOpsDeployCheck) {
	envPath := filepath.Join(root, ".env")
	values := make(map[string]string)
	file, err := os.Open(envPath)
	if err != nil {
		return values, []model.ContainerOpsDeployCheck{
			deployAgentCheck("error", "deploy_env_missing", "Deploy .env is required before services can be started.", envPath, true),
		}
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if key != "" {
			values[key] = value
		}
	}
	checks := make([]model.ContainerOpsDeployCheck, 0, 4)
	if err := scanner.Err(); err != nil {
		checks = append(checks, deployAgentCheck("error", "deploy_env_unreadable", "Deploy .env could not be read.", envPath, true))
		return values, checks
	}
	required := []string{"CPA_MANAGER_ADMIN_KEY", "CPA_MANAGEMENT_KEY", "CPAMP_AGENT_TOKEN"}
	for _, key := range required {
		value := strings.TrimSpace(values[key])
		if value == "" || strings.HasPrefix(value, "replace-with") || strings.Contains(value, "?set ") {
			checks = append(checks, deployAgentCheck("error", "deploy_env_secret_missing", fmt.Sprintf("%s must be set in deploy .env before services can be started.", key), key, true))
		}
	}
	if len(checks) == 0 {
		checks = append(checks, deployAgentCheck("info", "deploy_env_ready", "Deploy .env contains the required CPA and Agent secrets.", envPath, false))
	}
	return values, checks
}

func validateDeployStartOverview(overview model.ContainerOpsDockerOverview, manifest model.ContainerOpsStackManifest) []model.ContainerOpsDeployCheck {
	checks := make([]model.ContainerOpsDeployCheck, 0, 4)
	if network, ok := findDockerNetwork(overview, manifest.Network); ok {
		if network.Driver != "bridge" {
			checks = append(checks, deployAgentCheck("error", "standard_network_driver_mismatch", "The standard network exists but is not a bridge network.", manifest.Network, true))
		} else if !network.Managed {
			checks = append(checks, deployAgentCheck("error", "standard_network_conflict", "The standard network exists but is not CPAMP-managed.", manifest.Network, true))
		} else {
			checks = append(checks, deployAgentCheck("info", "standard_network_reusable", "The standard CPAMP network already exists and can be reused.", manifest.Network, false))
		}
	}
	for _, service := range manifest.Services {
		if !service.IncludeInCompose {
			continue
		}
		if existing, ok := findContainerByName(overview, service.Service); ok && !deployAgentReusableContainer(existing, service) {
			checks = append(checks, deployAgentCheck("error", "deploy_container_conflict", "A container with the standard service name already exists but is not CPAMP-managed for this role.", service.Service, true))
		}
	}
	return checks
}

func deployStartSpecs(manifest model.ContainerOpsStackManifest, env map[string]string, stackRoot string, backupRoot string) []deployServiceSpec {
	networkBaseURL := strings.TrimSuffix(manifest.NewAPIBaseURL, "/v1")
	return []deployServiceSpec{
		{
			Role:         "cpa",
			Name:         "cli-proxy-api",
			Image:        deployManifestImage(manifest, "cpa"),
			VolumeMounts: map[string]string{deployVolumeName(manifest.ComposeProject, "cpa-data"): "/app/data"},
			HostNetwork:  true,
			StartOrder:   1,
		},
		{
			Role:  "cpamp",
			Name:  "cpa-manager-plus",
			Image: deployManifestImage(manifest, "cpamp"),
			Env: []string{
				"HTTP_ADDR=0.0.0.0:18317",
				"CPA_UPSTREAM_URL=" + networkBaseURL,
				"CPA_MANAGEMENT_KEY=" + env["CPA_MANAGEMENT_KEY"],
				"CPA_MANAGER_ADMIN_KEY=" + env["CPA_MANAGER_ADMIN_KEY"],
				"CPAMP_AGENT_URL=http://host.docker.internal:18417",
				"CPAMP_AGENT_TOKEN=" + env["CPAMP_AGENT_TOKEN"],
			},
			Ports:        map[string]string{"18317/tcp": "18317"},
			VolumeMounts: map[string]string{deployVolumeName(manifest.ComposeProject, "cpa-manager-plus-data"): "/data"},
			ExtraHosts:   []string{"host.docker.internal:host-gateway"},
			StartOrder:   3,
		},
		{
			Role:  "agent",
			Name:  "cpamp-agent",
			Image: deployManifestImage(manifest, "agent"),
			Entrypoint: []string{
				"cpamp-agent",
			},
			Env: []string{
				"CPAMP_AGENT_ADDR=0.0.0.0:18417",
				"CPAMP_STACK_ROOT=" + stackRoot,
				"CPAMP_BACKUP_ROOT=" + backupRoot,
				"CPAMP_AGENT_TOKEN=" + env["CPAMP_AGENT_TOKEN"],
				"DOCKER_HOST=unix:///var/run/docker.sock",
			},
			Binds: []string{
				"/var/run/docker.sock:/var/run/docker.sock",
				stackRoot + ":" + stackRoot,
				backupRoot + ":" + backupRoot,
			},
			CapAdd:      []string{"NET_ADMIN", "NET_RAW"},
			HostNetwork: true,
			StartOrder:  2,
		},
	}
}

func (c *DockerClient) applyDeployStart(ctx context.Context, result *DeployStartResult, overview model.ContainerOpsDockerOverview, specs []deployServiceSpec) error {
	networkReady := false
	if network, ok := findDockerNetwork(overview, standardCPANetworkName); ok && network.Driver == "bridge" && network.Managed {
		networkReady = true
	}
	if networkReady {
		deployMarkAction(result.Actions, "create_standard_network", "skipped", "Standard network already exists.")
	} else {
		if err := c.createStandardNetwork(ctx); err != nil {
			return fmt.Errorf("create standard network: %w", err)
		}
		deployMarkAction(result.Actions, "create_standard_network", "applied", "Standard network created.")
	}

	for _, volume := range []string{deployVolumeName("cpamp-cpa", "cpa-data"), deployVolumeName("cpamp-cpa", "cpa-manager-plus-data")} {
		if err := c.createDeployVolume(ctx, volume); err != nil {
			return fmt.Errorf("create deploy volume %s: %w", volume, err)
		}
		if strings.Contains(volume, "cpa-manager-plus") {
			deployMarkAction(result.Actions, "create_cpamp_volume", "applied", "Standard CPAMP data volume is ready.")
		} else {
			deployMarkAction(result.Actions, "create_cpa_volume", "applied", "Standard CPA data volume is ready.")
		}
	}

	for _, spec := range specs {
		existing, ok := findContainerByName(overview, spec.Name)
		if ok && deployAgentReusableContainer(existing, model.ContainerOpsManifestService{Role: spec.Role, Service: spec.Name, Managed: true}) {
			deployMarkAction(result.Actions, "create_"+spec.Role+"_container", "skipped", "Standard managed container already exists.")
			continue
		}
		if err := c.createDeployContainer(ctx, spec); err != nil {
			return fmt.Errorf("create %s container: %w", spec.Name, err)
		}
		deployMarkAction(result.Actions, "create_"+spec.Role+"_container", "applied", "Standard managed container created.")
	}

	startSpecs := append([]deployServiceSpec{}, specs...)
	sortDeploySpecsByStartOrder(startSpecs)
	current, err := c.Overview(ctx)
	if err != nil {
		return fmt.Errorf("refresh containers before start: %w", err)
	}
	for _, spec := range startSpecs {
		existing, ok := findContainerByName(current, spec.Name)
		if ok && existing.State == "running" {
			deployMarkAction(result.Actions, "start_"+spec.Role+"_container", "skipped", "Container is already running.")
			continue
		}
		if err := c.startDeployContainer(ctx, spec.Name); err != nil {
			return fmt.Errorf("start %s container: %w", spec.Name, err)
		}
		deployMarkAction(result.Actions, "start_"+spec.Role+"_container", "applied", "Container start requested.")
	}
	return nil
}

func (c *DockerClient) createDeployVolume(ctx context.Context, name string) error {
	return c.post(ctx, "/volumes/create", map[string]any{
		"Name": name,
		"Labels": map[string]string{
			"com.cpamp.managed": "true",
			"com.cpamp.stack":   "cpa",
		},
	}, nil)
}

func (c *DockerClient) createDeployContainer(ctx context.Context, spec deployServiceSpec) error {
	exposedPorts := make(map[string]any)
	portBindings := make(map[string][]map[string]string)
	for containerPort, hostPort := range spec.Ports {
		exposedPorts[containerPort] = map[string]any{}
		portBindings[containerPort] = []map[string]string{{"HostPort": hostPort}}
	}
	mounts := make([]map[string]string, 0, len(spec.VolumeMounts))
	for source, target := range spec.VolumeMounts {
		mounts = append(mounts, map[string]string{"Type": "volume", "Source": source, "Target": target})
	}
	hostConfig := map[string]any{
		"RestartPolicy": map[string]string{"Name": "unless-stopped"},
	}
	if len(portBindings) > 0 {
		hostConfig["PortBindings"] = portBindings
	}
	if len(mounts) > 0 {
		hostConfig["Mounts"] = mounts
	}
	if len(spec.Binds) > 0 {
		hostConfig["Binds"] = spec.Binds
	}
	if len(spec.ExtraHosts) > 0 {
		hostConfig["ExtraHosts"] = spec.ExtraHosts
	}
	if len(spec.CapAdd) > 0 {
		hostConfig["CapAdd"] = spec.CapAdd
	}
	if spec.HostNetwork {
		hostConfig["NetworkMode"] = "host"
	}
	payload := map[string]any{
		"Image":        spec.Image,
		"Labels":       deployServiceLabels(spec),
		"Env":          spec.Env,
		"ExposedPorts": exposedPorts,
		"HostConfig":   hostConfig,
	}
	if !spec.HostNetwork {
		payload["NetworkingConfig"] = map[string]any{
			"EndpointsConfig": map[string]any{
				standardCPANetworkName: map[string]any{
					"Aliases": []string{spec.Name},
				},
			},
		}
	}
	if len(spec.Cmd) > 0 {
		payload["Cmd"] = spec.Cmd
	}
	if len(spec.Entrypoint) > 0 {
		payload["Entrypoint"] = spec.Entrypoint
	}
	endpoint := "/containers/create?name=" + url.QueryEscape(spec.Name)
	return c.post(ctx, endpoint, payload, nil)
}

func (c *DockerClient) startDeployContainer(ctx context.Context, name string) error {
	return c.post(ctx, "/containers/"+url.PathEscape(name)+"/start", nil, nil)
}

func healthcheckDeployServices(overview model.ContainerOpsDockerOverview, specs []deployServiceSpec) []model.ContainerOpsDeployCheck {
	checks := make([]model.ContainerOpsDeployCheck, 0, len(specs)+1)
	for _, spec := range specs {
		container, ok := findContainerByName(overview, spec.Name)
		if !ok {
			checks = append(checks, deployAgentCheck("error", "deploy_container_missing_after_start", "Expected deployed container was not found after start.", spec.Name, true))
			continue
		}
		if container.State != "running" {
			checks = append(checks, deployAgentCheck("error", "deploy_container_not_running", "Expected deployed container is not running after start.", spec.Name, true))
			continue
		}
		checks = append(checks, deployAgentCheck("info", "deploy_container_running", "Expected deployed container is running.", spec.Name, false))
	}
	if !deployChecksBlocking(checks) {
		checks = append(checks, deployAgentCheck("info", "deploy_services_started", "CPA, CPAMP, and Agent containers are running.", "cpamp-cpa", false))
	}
	return checks
}

func deployServiceLabels(spec deployServiceSpec) map[string]string {
	return map[string]string{
		"com.cpamp.managed":                   "true",
		"com.cpamp.stack":                     "cpa",
		"com.cpamp.role":                      spec.Role,
		"com.docker.compose.project":          "cpamp-cpa",
		"com.docker.compose.service":          spec.Name,
		"com.docker.compose.oneoff":           "False",
		"com.docker.compose.container-number": "1",
	}
}

func deployManifestImage(manifest model.ContainerOpsStackManifest, role string) string {
	for _, service := range manifest.Services {
		if service.Role == role && service.IncludeInCompose {
			return service.Image
		}
	}
	return ""
}

func deployVolumeName(project string, volume string) string {
	return project + "_" + volume
}

func deployAgentReusableContainer(container model.ContainerOpsDockerContainer, service model.ContainerOpsManifestService) bool {
	return container.Managed && container.Role == service.Role && strings.EqualFold(container.Name, service.Service)
}

func findContainerByName(overview model.ContainerOpsDockerOverview, name string) (model.ContainerOpsDockerContainer, bool) {
	for _, container := range overview.Containers {
		if strings.EqualFold(container.Name, name) {
			return container, true
		}
	}
	return model.ContainerOpsDockerContainer{}, false
}

func deployMarkAction(actions []model.ContainerOpsDeployAction, code string, status string, message string) {
	for index := range actions {
		if actions[index].Code == code {
			actions[index].Status = status
			actions[index].Message = message
			return
		}
	}
}

func deployChecksBlocking(checks []model.ContainerOpsDeployCheck) bool {
	for _, check := range checks {
		if check.Blocking {
			return true
		}
	}
	return false
}

func deployAgentCheck(severity string, code string, message string, resource string, blocking bool) model.ContainerOpsDeployCheck {
	return model.ContainerOpsDeployCheck{
		Severity: severity,
		Code:     code,
		Message:  message,
		Resource: resource,
		Blocking: blocking,
	}
}

func sortDeploySpecsByStartOrder(specs []deployServiceSpec) {
	for i := 0; i < len(specs); i++ {
		for j := i + 1; j < len(specs); j++ {
			if specs[j].StartOrder < specs[i].StartOrder {
				specs[i], specs[j] = specs[j], specs[i]
			}
		}
	}
}
