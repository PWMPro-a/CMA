package containeropsagent

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

const standardCPANetworkName = "cpamp-cpa_default"

type NetworkStandardizeOptions struct {
	BackupRoot string
	BackupID   string
	Apply      bool
}

func (c *DockerClient) StandardizeCPANetwork(ctx context.Context, options NetworkStandardizeOptions) (model.ContainerOpsNetworkStandardizeResult, error) {
	backupRoot := cleanBackupRoot(options.BackupRoot)
	backupID, err := cleanBackupID(options.BackupID)
	if err != nil {
		return model.ContainerOpsNetworkStandardizeResult{}, err
	}
	manifest, err := readBackupManifest(filepathForBackupID(backupRoot, backupID))
	if err != nil {
		return model.ContainerOpsNetworkStandardizeResult{}, err
	}
	if manifest.BackupID != backupID {
		return model.ContainerOpsNetworkStandardizeResult{}, fmt.Errorf("backup manifest ID %q does not match requested backupId %q", manifest.BackupID, backupID)
	}

	overview, err := c.Overview(ctx)
	if err != nil {
		return model.ContainerOpsNetworkStandardizeResult{}, fmt.Errorf("discover docker resources: %w", err)
	}
	result := buildNetworkStandardizePlan(backupRoot, backupID, manifest, overview)
	if options.Apply && result.Status != "blocked" {
		if err := c.applyNetworkStandardizePlan(ctx, &result); err != nil {
			return model.ContainerOpsNetworkStandardizeResult{}, err
		}
		refreshed, err := c.Overview(ctx)
		if err == nil {
			result.Overview = &refreshed
		}
		result.Applied = true
		result.Status = networkStatus(result.Checks, true)
	}
	return result, nil
}

func buildNetworkStandardizePlan(
	backupRoot string,
	backupID string,
	manifest model.ContainerOpsBackupResult,
	overview model.ContainerOpsDockerOverview,
) model.ContainerOpsNetworkStandardizeResult {
	checks := make([]model.ContainerOpsNetworkCheck, 0, 8)
	actions := make([]model.ContainerOpsNetworkAction, 0, 6)
	addCheck := func(severity string, code string, message string, resource string, blocking bool) {
		checks = append(checks, model.ContainerOpsNetworkCheck{
			Severity: severity,
			Code:     code,
			Message:  message,
			Resource: resource,
			Blocking: blocking,
		})
	}
	addAction := func(code string, target string, status string, message string) {
		actions = append(actions, model.ContainerOpsNetworkAction{
			Order:   len(actions) + 1,
			Code:    code,
			Target:  target,
			Status:  status,
			Message: message,
		})
	}

	if backupHasCPAArchive(backupRoot, backupID, manifest) {
		addCheck("info", "backup_ready", "Backup manifest and CPA archive were found for this network operation.", backupID, false)
	} else {
		addCheck("error", "backup_missing_cpa_archive", "A valid CPA backup archive is required before network standardization.", backupID, true)
	}

	network, networkFound := findDockerNetwork(overview, standardCPANetworkName)
	if !networkFound {
		addCheck("warning", "standard_network_missing", "The standard CPA network is missing and will be created with CPAMP labels.", standardCPANetworkName, false)
		addAction("create_standard_network", standardCPANetworkName, "planned", "Create the standard CPAMP CPA bridge network.")
	} else {
		if network.Driver != "" && network.Driver != "bridge" {
			addCheck("error", "standard_network_driver_mismatch", "The standard network exists but is not a bridge network.", standardCPANetworkName, true)
		} else {
			addCheck("info", "standard_network_ready", "The standard CPA network already exists.", standardCPANetworkName, false)
		}
		if !network.Managed {
			addCheck("warning", "standard_network_unmanaged", "Existing Docker networks cannot be relabeled safely by this step; confirm ownership before destructive lifecycle actions.", standardCPANetworkName, false)
		}
	}

	targets := networkTargets(overview)
	if len(targets) == 0 {
		addCheck("error", "cpa_target_missing", "No CPA container was detected for network standardization.", "cli-proxy-api", true)
	} else if _, ok := targets["cpa"]; !ok {
		addCheck("error", "cpa_target_missing", "No CPA container was detected for network standardization.", "cli-proxy-api", true)
	} else {
		addCheck("info", "cpa_target_ready", "CPA container was detected for network standardization.", targets["cpa"].Name, false)
	}

	roles := make([]string, 0, len(targets))
	for role := range targets {
		roles = append(roles, role)
	}
	sort.Slice(roles, func(i, j int) bool {
		return networkRoleRank(roles[i]) < networkRoleRank(roles[j])
	})
	for _, role := range roles {
		container := targets[role]
		if containerHasNetwork(container, standardCPANetworkName) {
			addAction("connect_"+role+"_to_standard_network", container.Name, "skipped", "Container is already attached to the standard network.")
			continue
		}
		addAction("connect_"+role+"_to_standard_network", container.Name, "planned", "Attach the detected container to the standard network.")
	}

	return model.ContainerOpsNetworkStandardizeResult{
		BackupID:    backupID,
		Status:      networkStatus(checks, false),
		Network:     standardCPANetworkName,
		Checks:      checks,
		Actions:     actions,
		Applied:     false,
		Destructive: false,
	}
}

func (c *DockerClient) applyNetworkStandardizePlan(ctx context.Context, result *model.ContainerOpsNetworkStandardizeResult) error {
	for index, action := range result.Actions {
		if action.Status != "planned" {
			continue
		}
		switch action.Code {
		case "create_standard_network":
			if err := c.createStandardNetwork(ctx); err != nil {
				return fmt.Errorf("create standard network: %w", err)
			}
		default:
			if strings.HasPrefix(action.Code, "connect_") {
				if err := c.connectContainerToNetwork(ctx, result.Network, action.Target); err != nil {
					return fmt.Errorf("connect %s to %s: %w", action.Target, result.Network, err)
				}
			}
		}
		result.Actions[index].Status = "applied"
	}
	return nil
}

func (c *DockerClient) createStandardNetwork(ctx context.Context) error {
	return c.post(ctx, "/networks/create", map[string]any{
		"Name":   standardCPANetworkName,
		"Driver": "bridge",
		"Labels": map[string]string{
			"com.cpamp.managed": "true",
			"com.cpamp.stack":   "cpa",
		},
	}, nil)
}

func (c *DockerClient) connectContainerToNetwork(ctx context.Context, networkName string, containerName string) error {
	endpoint := fmt.Sprintf("/networks/%s/connect", url.PathEscape(networkName))
	return c.post(ctx, endpoint, map[string]any{"Container": containerName}, nil)
}

func backupHasCPAArchive(backupRoot string, backupID string, manifest model.ContainerOpsBackupResult) bool {
	backupDir := filepathForBackupID(backupRoot, backupID)
	for _, archive := range manifest.Archives {
		if archive.Role != "cpa" {
			continue
		}
		filePath, err := backupFilePath(backupDir, archive.FileName)
		if err != nil {
			return false
		}
		if _, err := os.Stat(filePath); err != nil {
			return false
		}
		return true
	}
	return false
}

func networkTargets(overview model.ContainerOpsDockerOverview) map[string]model.ContainerOpsDockerContainer {
	targets := make(map[string]model.ContainerOpsDockerContainer)
	for _, spec := range []struct {
		role string
		name string
	}{
		{role: "cpa", name: "cli-proxy-api"},
		{role: "cpamp", name: "cpa-manager-plus"},
		{role: "agent", name: "cpamp-agent"},
		{role: "newapi", name: "new-api"},
	} {
		if container, ok := selectBackupContainer(overview, spec.role, spec.name); ok {
			targets[spec.role] = container
		}
	}
	return targets
}

func networkStatus(checks []model.ContainerOpsNetworkCheck, applied bool) string {
	hasWarning := false
	for _, check := range checks {
		if check.Blocking {
			return "blocked"
		}
		if check.Severity == "warning" {
			hasWarning = true
		}
	}
	if applied {
		if hasWarning {
			return "applied_with_warnings"
		}
		return "applied"
	}
	if hasWarning {
		return "planned_with_warnings"
	}
	return "planned"
}

func filepathForBackupID(backupRoot string, backupID string) string {
	return filepath.Join(backupRoot, backupID)
}

func networkRoleRank(role string) int {
	switch role {
	case "cpa":
		return 1
	case "cpamp":
		return 2
	case "agent":
		return 3
	case "newapi":
		return 4
	default:
		return 99
	}
}

func findDockerNetwork(overview model.ContainerOpsDockerOverview, name string) (model.ContainerOpsDockerNetwork, bool) {
	for _, network := range overview.Networks {
		if network.Name == name {
			return network, true
		}
	}
	return model.ContainerOpsDockerNetwork{}, false
}

func containerHasNetwork(container model.ContainerOpsDockerContainer, name string) bool {
	for _, network := range container.Networks {
		if network.Name == name {
			return true
		}
	}
	return false
}
