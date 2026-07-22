package containeropsagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

const defaultBackupRoot = "/opt/cpamp/backups"

type BackupOptions struct {
	BackupRoot     string
	BackupIDPrefix string
	Now            time.Time
}

func (c *DockerClient) BackupCPAStack(ctx context.Context, options BackupOptions) (model.ContainerOpsBackupResult, error) {
	now := options.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	backupRoot := cleanBackupRoot(options.BackupRoot)
	prefix := strings.TrimSpace(options.BackupIDPrefix)
	if prefix == "" {
		prefix = "cpa"
	}
	backupID := prefix + "-" + now.UTC().Format("20060102T150405Z")
	backupDir := filepath.Join(backupRoot, backupID)
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		return model.ContainerOpsBackupResult{}, fmt.Errorf("create backup directory: %w", err)
	}

	overview, err := c.Overview(ctx)
	if err != nil {
		return model.ContainerOpsBackupResult{}, err
	}
	cpa, ok := selectBackupContainer(overview, "cpa", "cli-proxy-api")
	if !ok {
		return model.ContainerOpsBackupResult{}, fmt.Errorf("no CPA container detected for backup")
	}

	result := model.ContainerOpsBackupResult{
		BackupID:   backupID,
		Status:     "completed",
		BackupRoot: backupRoot,
		CreatedAt:  now.Unix(),
		Archives:   make([]model.ContainerOpsBackupArchive, 0, 2),
		ReadOnly:   true,
	}
	archive, err := c.writeContainerArchive(ctx, backupDir, cpa, "cpa", "cli-proxy-api", "/app/data")
	if err != nil {
		return model.ContainerOpsBackupResult{}, err
	}
	result.Archives = append(result.Archives, archive)

	if cpamp, ok := selectBackupContainer(overview, "cpamp", "cpa-manager-plus"); ok {
		archive, err := c.writeContainerArchive(ctx, backupDir, cpamp, "cpamp", "cpa-manager-plus", "/data")
		if err != nil {
			result.Warnings = append(result.Warnings, model.ContainerOpsBackupWarning{
				Code:     "cpamp_archive_failed",
				Message:  err.Error(),
				Resource: cpamp.Name,
			})
		} else {
			result.Archives = append(result.Archives, archive)
		}
	} else {
		result.Warnings = append(result.Warnings, model.ContainerOpsBackupWarning{
			Code:    "cpamp_not_found",
			Message: "No CPAMP container was detected; manager data was not archived.",
		})
	}

	if len(result.Warnings) > 0 {
		result.Status = "completed_with_warnings"
	}
	if err := writeBackupManifest(backupDir, result); err != nil {
		return model.ContainerOpsBackupResult{}, err
	}
	return result, nil
}

func (c *DockerClient) writeContainerArchive(
	ctx context.Context,
	backupDir string,
	container model.ContainerOpsDockerContainer,
	role string,
	service string,
	containerPath string,
) (model.ContainerOpsBackupArchive, error) {
	reader, err := c.archiveContainerPath(ctx, container.Name, containerPath)
	if err != nil {
		return model.ContainerOpsBackupArchive{}, fmt.Errorf("archive %s %s: %w", container.Name, containerPath, err)
	}
	defer reader.Close()

	fileName := fmt.Sprintf("%s-%s.tar", role, safeFileToken(container.Name))
	target := filepath.Join(backupDir, fileName)
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return model.ContainerOpsBackupArchive{}, fmt.Errorf("create archive file: %w", err)
	}
	bytesWritten, copyErr := io.Copy(file, reader)
	closeErr := file.Close()
	if copyErr != nil {
		return model.ContainerOpsBackupArchive{}, copyErr
	}
	if closeErr != nil {
		return model.ContainerOpsBackupArchive{}, closeErr
	}
	return model.ContainerOpsBackupArchive{
		Role:      role,
		Service:   service,
		Container: container.Name,
		Path:      containerPath,
		FileName:  fileName,
		Size:      bytesWritten,
	}, nil
}

func (c *DockerClient) archiveContainerPath(ctx context.Context, container string, containerPath string) (io.ReadCloser, error) {
	endpoint := fmt.Sprintf(
		"http://docker/containers/%s/archive?path=%s",
		url.PathEscape(container),
		url.QueryEscape(containerPath),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, fmt.Errorf("docker api status %d", resp.StatusCode)
	}
	return resp.Body, nil
}

func writeBackupManifest(backupDir string, result model.ContainerOpsBackupResult) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(backupDir, "manifest.json"), data, 0o640)
}

func cleanBackupRoot(raw string) string {
	root := strings.TrimSpace(raw)
	if root == "" {
		root = defaultBackupRoot
	}
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		root = filepath.Join(defaultBackupRoot, root)
	}
	return root
}

func selectBackupContainer(overview model.ContainerOpsDockerOverview, role string, standardName string) (model.ContainerOpsDockerContainer, bool) {
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
		leftScore := backupContainerScore(matches[i], standardName)
		rightScore := backupContainerScore(matches[j], standardName)
		if leftScore != rightScore {
			return leftScore > rightScore
		}
		return matches[i].Name < matches[j].Name
	})
	return matches[0], true
}

func backupContainerScore(container model.ContainerOpsDockerContainer, standardName string) int {
	score := 0
	if strings.EqualFold(container.Name, standardName) {
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

func safeFileToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "container"
	}
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		allowed := r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' ||
			r == '.' ||
			r == '_' ||
			r == '-'
		if allowed {
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
		return "container"
	}
	return result
}
