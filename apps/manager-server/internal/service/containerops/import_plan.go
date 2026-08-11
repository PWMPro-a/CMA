package containerops

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

const (
	roleCPA    = "cpa"
	roleCPAMP  = "cpamp"
	roleAgent  = "agent"
	roleNewAPI = "newapi"
)

var importRoleRank = map[string]int{
	roleCPA:    1,
	roleCPAMP:  2,
	roleAgent:  3,
	roleNewAPI: 4,
}

func (s *Service) ImportPlan(ctx context.Context) (model.ContainerOpsImportPlan, error) {
	discovery, err := s.Discover(ctx)
	if err != nil {
		return model.ContainerOpsImportPlan{}, err
	}
	return buildImportPlan(discovery.Agent, discovery.Docker, standardResources(), discovery.NewAPI), nil
}

func buildImportPlan(
	agent model.ContainerOpsAgentInfo,
	overview model.ContainerOpsDockerOverview,
	resources model.ContainerOpsStandardResource,
	newAPI model.ContainerOpsNewAPIInfo,
) model.ContainerOpsImportPlan {
	candidates := buildImportCandidates(overview, resources)
	cpa, cpaFound := selectRoleContainer(overview, roleCPA, resources.CPAService)
	cpamp, cpampFound := selectRoleContainer(overview, roleCPAMP, resources.CPAMPService)
	agentContainer, agentFound := selectRoleContainer(overview, roleAgent, resources.AgentService)
	newAPIContainer, newAPIFound := selectRoleContainer(overview, roleNewAPI, "new-api")

	risks := buildImportRisks(overview, resources, cpa, cpaFound, cpampFound, agentFound, newAPIContainer, newAPIFound)
	manifest := buildManifest(resources, newAPI, cpa, cpaFound, cpamp, cpampFound, agentContainer, agentFound, newAPIContainer, newAPIFound)
	compose := buildComposeDraft(resources, cpa, cpaFound, cpamp, cpampFound)
	blocking := blockingRiskCount(risks)

	return model.ContainerOpsImportPlan{
		Agent:       agent,
		Summary:     buildImportSummary(cpaFound, cpampFound, agentFound, newAPIFound, len(risks), blocking),
		Manifest:    manifest,
		Compose:     compose,
		Candidates:  candidates,
		Risks:       risks,
		NextActions: buildNextActions(risks, cpaFound),
		NewAPI:      newAPI,
		ReadOnly:    true,
	}
}

func buildImportCandidates(overview model.ContainerOpsDockerOverview, resources model.ContainerOpsStandardResource) []model.ContainerOpsImportCandidate {
	candidates := make([]model.ContainerOpsImportCandidate, 0)
	for _, container := range overview.Containers {
		role := strings.TrimSpace(container.Role)
		if _, ok := importRoleRank[role]; !ok {
			continue
		}
		candidates = append(candidates, model.ContainerOpsImportCandidate{
			Role:             role,
			ContainerID:      container.ID,
			Name:             container.Name,
			Image:            container.Image,
			State:            container.State,
			Managed:          container.Managed,
			TargetService:    targetServiceForRole(role, resources),
			IncludeInCompose: role != roleNewAPI,
			Networks:         networkNames(container),
			Ports:            container.Ports,
			Mounts:           container.Mounts,
			Reasons:          candidateReasons(container, targetServiceForRole(role, resources)),
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		leftRank := importRoleRank[candidates[i].Role]
		rightRank := importRoleRank[candidates[j].Role]
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return candidates[i].Name < candidates[j].Name
	})
	return candidates
}

func candidateReasons(container model.ContainerOpsDockerContainer, targetService string) []string {
	reasons := make([]string, 0, 3)
	if strings.EqualFold(container.Name, targetService) {
		reasons = append(reasons, "name_matches_target_service")
	}
	if container.Managed {
		reasons = append(reasons, "already_cpamp_managed")
	}
	if container.State == "running" {
		reasons = append(reasons, "container_is_running")
	}
	return reasons
}

func buildImportSummary(cpaFound bool, cpampFound bool, agentFound bool, newAPIFound bool, riskCount int, blocking int) model.ContainerOpsImportSummary {
	return model.ContainerOpsImportSummary{
		Ready:             cpaFound && blocking == 0,
		CPAFound:          cpaFound,
		CPAMPFound:        cpampFound,
		AgentFound:        agentFound,
		NewAPIFound:       newAPIFound,
		RiskCount:         riskCount,
		BlockingRiskCount: blocking,
	}
}

func selectRoleContainer(overview model.ContainerOpsDockerOverview, role string, targetService string) (model.ContainerOpsDockerContainer, bool) {
	matches := make([]model.ContainerOpsDockerContainer, 0)
	for _, container := range overview.Containers {
		if container.Role == role {
			matches = append(matches, container)
		}
	}
	if len(matches) == 0 {
		return model.ContainerOpsDockerContainer{}, false
	}
	sort.Slice(matches, func(i, j int) bool {
		leftScore := importCandidateScore(matches[i], targetService)
		rightScore := importCandidateScore(matches[j], targetService)
		if leftScore != rightScore {
			return leftScore > rightScore
		}
		return matches[i].Name < matches[j].Name
	})
	return matches[0], true
}

func importCandidateScore(container model.ContainerOpsDockerContainer, targetService string) int {
	score := 0
	if strings.EqualFold(container.Name, targetService) {
		score += 100
	}
	if container.State == "running" {
		score += 20
	}
	if container.Managed {
		score += 10
	}
	return score
}

func buildImportRisks(
	overview model.ContainerOpsDockerOverview,
	resources model.ContainerOpsStandardResource,
	cpa model.ContainerOpsDockerContainer,
	cpaFound bool,
	cpampFound bool,
	agentFound bool,
	newAPI model.ContainerOpsDockerContainer,
	newAPIFound bool,
) []model.ContainerOpsImportRisk {
	risks := make([]model.ContainerOpsImportRisk, 0)
	addRisk := func(severity string, code string, message string, resource string, blocking bool) {
		risks = append(risks, model.ContainerOpsImportRisk{
			Severity: severity,
			Code:     code,
			Message:  message,
			Resource: resource,
			Blocking: blocking,
		})
	}

	if !cpaFound {
		addRisk("error", "cpa_not_found", "No CPA container was detected. Deploy or start CPA before importing an existing stack.", "", true)
	} else {
		if countRole(overview, roleCPA) > 1 {
			addRisk("warning", "multiple_cpa_candidates", "Multiple CPA containers were detected. The import plan selected the best match, but the apply step must confirm ownership.", cpa.Name, false)
		}
		if cpa.State != "running" {
			addRisk("warning", "cpa_not_running", "The selected CPA container is not running. Backups and health checks may be incomplete.", cpa.Name, false)
		}
		if !cpa.Managed {
			addRisk("warning", "managed_label_missing", "The selected CPA container is missing com.cpamp.managed=true. Lifecycle actions must add CPAMP labels before taking ownership.", cpa.Name, false)
		}
		if !strings.EqualFold(cpa.Name, resources.CPAService) {
			addRisk("warning", "service_name_mismatch", "The selected CPA container name differs from the standard service name. The import should add the standard DNS alias before switching NewAPI traffic.", cpa.Name, false)
		}
		if len(cpa.Mounts) == 0 {
			addRisk("warning", "cpa_data_mount_unknown", "No CPA data mount was detected. Confirm the data directory before enabling backup or restore.", cpa.Name, false)
		}
		if hasPublicPort(cpa) {
			addRisk("info", "cpa_public_port", "CPA exposes a host port. Keep this only if direct host access is still required.", cpa.Name, false)
		}
	}

	if !cpampFound {
		addRisk("info", "cpamp_not_found", "No CPAMP container was detected. The current manager may be running outside Docker or under a non-standard name.", "", false)
	}
	if !agentFound {
		addRisk("info", "agent_not_found", "No cpamp-agent container was detected. Future write operations require the agent service in the managed network.", resources.AgentService, false)
	}

	network, networkFound := findNetwork(overview, resources.Network)
	if !networkFound {
		addRisk("warning", "standard_network_missing", "The standard CPAMP network was not detected. The Compose draft will create it before ownership is applied.", resources.Network, false)
	} else if !network.Managed {
		addRisk("warning", "network_label_missing", "The standard network is missing com.cpamp.managed=true. The apply step must label it before lifecycle actions.", resources.Network, false)
	}

	if newAPIFound && cpaFound {
		if !containersShareNetwork(cpa, newAPI) {
			addRisk("warning", "newapi_network_mismatch", "NewAPI and CPA do not share a Docker network. NewAPI cannot use the recommended internal Base URL until they are attached to the same network.", newAPI.Name, false)
		}
	} else if !newAPIFound {
		addRisk("info", "newapi_not_found", "No NewAPI container was detected. NewAPI data will not be backed up by CPAMP; only the route can be checked when it appears.", "", false)
	}

	addRisk("info", "readonly_preview", "This endpoint only generates an import plan and Compose draft. It does not modify Docker resources.", "", false)
	return risks
}

func buildManifest(
	resources model.ContainerOpsStandardResource,
	newAPI model.ContainerOpsNewAPIInfo,
	cpa model.ContainerOpsDockerContainer,
	cpaFound bool,
	cpamp model.ContainerOpsDockerContainer,
	cpampFound bool,
	agent model.ContainerOpsDockerContainer,
	agentFound bool,
	newAPIContainer model.ContainerOpsDockerContainer,
	newAPIFound bool,
) model.ContainerOpsStackManifest {
	services := []model.ContainerOpsManifestService{
		buildManifestService(roleCPA, resources.CPAService, true, cpa, cpaFound),
		buildManifestService(roleCPAMP, resources.CPAMPService, true, cpamp, cpampFound),
		buildManifestService(roleAgent, resources.AgentService, true, agent, agentFound),
	}
	if newAPIFound {
		services = append(services, buildManifestService(roleNewAPI, "new-api", false, newAPIContainer, true))
	}
	services[0].InternalBaseURL = newAPI.RecommendedBaseURL
	return model.ContainerOpsStackManifest{
		Stack:          "cpa",
		ComposeProject: resources.ComposeProject,
		Network:        resources.Network,
		StackRoot:      resources.StackRoot,
		BackupRoot:     resources.BackupRoot,
		NewAPIBaseURL:  newAPI.RecommendedBaseURL,
		Services:       services,
		Volumes:        manifestVolumes(cpa, cpaFound),
	}
}

func buildManifestService(role string, serviceName string, includeInCompose bool, container model.ContainerOpsDockerContainer, found bool) model.ContainerOpsManifestService {
	if !found {
		return model.ContainerOpsManifestService{
			Role:             role,
			Service:          serviceName,
			Image:            defaultImageForRole(role),
			State:            "planned",
			IncludeInCompose: includeInCompose,
		}
	}
	return model.ContainerOpsManifestService{
		Role:             role,
		Service:          serviceName,
		SourceContainer:  container.Name,
		Image:            container.Image,
		State:            container.State,
		Managed:          container.Managed,
		IncludeInCompose: includeInCompose,
		Networks:         networkNames(container),
		Mounts:           formatMounts(container.Mounts),
		Ports:            formatPorts(container.Ports),
	}
}

func manifestVolumes(cpa model.ContainerOpsDockerContainer, found bool) []model.ContainerOpsManifestVolume {
	if !found || len(cpa.Mounts) == 0 {
		return []model.ContainerOpsManifestVolume{
			{Name: "cpa-data", Type: "volume", Destination: "/app/data", External: false},
		}
	}
	volumes := make([]model.ContainerOpsManifestVolume, 0, len(cpa.Mounts))
	for _, mount := range cpa.Mounts {
		name := firstNonEmpty(mount.Name, mount.Source)
		if name == "" {
			name = mount.Destination
		}
		volumes = append(volumes, model.ContainerOpsManifestVolume{
			Name:        name,
			Type:        mount.Type,
			Source:      mount.Source,
			Destination: mount.Destination,
			External:    mount.Type == "volume" || mount.Type == "bind",
		})
	}
	return volumes
}

func buildComposeDraft(
	resources model.ContainerOpsStandardResource,
	cpa model.ContainerOpsDockerContainer,
	cpaFound bool,
	cpamp model.ContainerOpsDockerContainer,
	cpampFound bool,
) model.ContainerOpsComposeDraft {
	cpaImage := defaultImageForRole(roleCPA)
	cpaMounts := []string{quoteYAML("cpa-data:/app/data")}
	cpaPorts := []string{quoteYAML("8317:8317")}
	if cpaFound {
		cpaImage = firstNonEmpty(cpa.Image, cpaImage)
		if mounts := composeMounts(cpa.Mounts); len(mounts) > 0 {
			cpaMounts = mounts
		}
		if ports := composePorts(cpa.Ports); len(ports) > 0 {
			cpaPorts = ports
		}
	}
	cpampImage := defaultImageForRole(roleCPAMP)
	if cpampFound {
		cpampImage = firstNonEmpty(cpamp.Image, cpampImage)
	}

	var builder strings.Builder
	line := func(format string, args ...any) {
		builder.WriteString(fmt.Sprintf(format, args...))
		builder.WriteByte('\n')
	}
	line("name: %s", resources.ComposeProject)
	line("services:")
	line("  %s:", resources.CPAService)
	line("    image: %s", cpaImage)
	line("    container_name: %s", resources.CPAService)
	line("    restart: unless-stopped")
	writeComposeLabels(&builder, roleCPA)
	line("    networks:")
	line("      - %s", resources.Network)
	line("    ports:")
	for _, port := range cpaPorts {
		line("      - %s", port)
	}
	line("    volumes:")
	for _, mount := range cpaMounts {
		line("      - %s", mount)
	}
	line("")
	line("  %s:", resources.CPAMPService)
	line("    image: %s", cpampImage)
	line("    container_name: %s", resources.CPAMPService)
	line("    restart: unless-stopped")
	writeComposeLabels(&builder, roleCPAMP)
	line("    environment:")
	line("      HTTP_ADDR: %s", quoteYAML("0.0.0.0:18317"))
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
	for _, volume := range composeNamedVolumes(cpa.Mounts) {
		line("  %s:", volume)
		line("    external: true")
	}
	line("  cpa-manager-plus-data:")

	return model.ContainerOpsComposeDraft{
		FileName:    "compose.import-preview.yml",
		ProjectName: resources.ComposeProject,
		NetworkName: resources.Network,
		Services:    []string{resources.CPAService, resources.CPAMPService, resources.AgentService},
		Content:     strings.TrimSpace(builder.String()) + "\n",
	}
}

func writeComposeLabels(builder *strings.Builder, role string) {
	builder.WriteString("    labels:\n")
	builder.WriteString("      com.cpamp.managed: \"true\"\n")
	builder.WriteString("      com.cpamp.stack: \"cpa\"\n")
	builder.WriteString(fmt.Sprintf("      com.cpamp.role: %q\n", role))
}

func buildNextActions(risks []model.ContainerOpsImportRisk, cpaFound bool) []string {
	if !cpaFound {
		return []string{"deploy_cpa_stack"}
	}
	actions := []string{"review_import_plan", "backup_cpa_data"}
	if hasRisk(risks, "managed_label_missing") || hasRisk(risks, "network_label_missing") {
		actions = append(actions, "apply_cpamp_labels")
	}
	if hasRisk(risks, "standard_network_missing") || hasRisk(risks, "newapi_network_mismatch") {
		actions = append(actions, "standardize_network")
	}
	actions = append(actions, "run_health_checks", "enable_write_operations_after_backup")
	return actions
}

func targetServiceForRole(role string, resources model.ContainerOpsStandardResource) string {
	switch role {
	case roleCPA:
		return resources.CPAService
	case roleCPAMP:
		return resources.CPAMPService
	case roleAgent:
		return resources.AgentService
	case roleNewAPI:
		return "new-api"
	default:
		return role
	}
}

func defaultImageForRole(role string) string {
	switch role {
	case roleCPA:
		return "seakee/cli-proxy-api:latest"
	case roleCPAMP, roleAgent:
		return "seakee/cpa-manager-plus:latest"
	default:
		return ""
	}
}

func countRole(overview model.ContainerOpsDockerOverview, role string) int {
	count := 0
	for _, container := range overview.Containers {
		if container.Role == role {
			count++
		}
	}
	return count
}

func findNetwork(overview model.ContainerOpsDockerOverview, name string) (model.ContainerOpsDockerNetwork, bool) {
	for _, network := range overview.Networks {
		if network.Name == name {
			return network, true
		}
	}
	return model.ContainerOpsDockerNetwork{}, false
}

func containersShareNetwork(left model.ContainerOpsDockerContainer, right model.ContainerOpsDockerContainer) bool {
	leftNetworks := make(map[string]struct{}, len(left.Networks))
	for _, network := range left.Networks {
		leftNetworks[network.Name] = struct{}{}
	}
	for _, network := range right.Networks {
		if _, ok := leftNetworks[network.Name]; ok {
			return true
		}
	}
	return false
}

func hasPublicPort(container model.ContainerOpsDockerContainer) bool {
	for _, port := range container.Ports {
		if port.PublicPort > 0 {
			return true
		}
	}
	return false
}

func networkNames(container model.ContainerOpsDockerContainer) []string {
	names := make([]string, 0, len(container.Networks))
	for _, network := range container.Networks {
		names = append(names, network.Name)
	}
	sort.Strings(names)
	return names
}

func formatPorts(ports []model.ContainerOpsDockerPort) []string {
	result := make([]string, 0, len(ports))
	for _, port := range ports {
		target := strconv.Itoa(port.PrivatePort)
		if port.Type != "" {
			target += "/" + port.Type
		}
		if port.PublicPort > 0 {
			result = append(result, fmt.Sprintf("%d->%s", port.PublicPort, target))
		} else {
			result = append(result, target)
		}
	}
	return result
}

func formatMounts(mounts []model.ContainerOpsDockerMount) []string {
	result := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		source := firstNonEmpty(mount.Source, mount.Name)
		if source == "" {
			result = append(result, mount.Destination)
			continue
		}
		result = append(result, fmt.Sprintf("%s:%s", source, mount.Destination))
	}
	return result
}

func composePorts(ports []model.ContainerOpsDockerPort) []string {
	result := make([]string, 0, len(ports))
	for _, port := range ports {
		if port.PublicPort == 0 {
			continue
		}
		result = append(result, quoteYAML(fmt.Sprintf("%d:%d", port.PublicPort, port.PrivatePort)))
	}
	return result
}

func composeMounts(mounts []model.ContainerOpsDockerMount) []string {
	result := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		source := firstNonEmpty(mount.Source, mount.Name)
		if source == "" || mount.Destination == "" {
			continue
		}
		result = append(result, quoteYAML(fmt.Sprintf("%s:%s", source, mount.Destination)))
	}
	return result
}

func composeNamedVolumes(mounts []model.ContainerOpsDockerMount) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0)
	for _, mount := range mounts {
		if mount.Type != "volume" || mount.Name == "" {
			continue
		}
		if _, ok := seen[mount.Name]; ok {
			continue
		}
		seen[mount.Name] = struct{}{}
		result = append(result, mount.Name)
	}
	sort.Strings(result)
	if len(result) == 0 {
		return []string{"cpa-data"}
	}
	return result
}

func quoteYAML(value string) string {
	return strconv.Quote(value)
}

func blockingRiskCount(risks []model.ContainerOpsImportRisk) int {
	count := 0
	for _, risk := range risks {
		if risk.Blocking {
			count++
		}
	}
	return count
}

func hasRisk(risks []model.ContainerOpsImportRisk, code string) bool {
	for _, risk := range risks {
		if risk.Code == code {
			return true
		}
	}
	return false
}
