package containeropsagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/response"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

const defaultStackRoot = "/opt/cpamp/stacks/cpa"

func (s *Server) renderCPADeployFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.MethodNotAllowed(w)
		return
	}
	var request model.ContainerOpsDeployRenderRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		response.Error(w, http.StatusBadRequest, err)
		return
	}
	files, err := RenderCPADeployFiles(r.Context(), s.stackRoot, request)
	if err != nil {
		response.Error(w, http.StatusBadGateway, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"status": "rendered",
		"files":  files,
	})
}

func RenderCPADeployFiles(ctx context.Context, stackRoot string, request model.ContainerOpsDeployRenderRequest) ([]model.ContainerOpsDeployFile, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if err := validateDeployRenderRequest(request); err != nil {
		return nil, err
	}
	root := cleanStackRoot(stackRoot)
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("create stack directory: %w", err)
	}

	manifestData, err := json.MarshalIndent(request.Manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal stack manifest: %w", err)
	}
	manifestData = append(manifestData, '\n')

	files := make([]model.ContainerOpsDeployFile, 0, 3)
	written, err := writeDeployFile(root, "compose.yml", []byte(request.Compose.Content), "compose")
	if err != nil {
		return nil, err
	}
	files = append(files, written)
	written, err = writeDeployFile(root, "stack.manifest.json", manifestData, "manifest")
	if err != nil {
		return nil, err
	}
	files = append(files, written)
	written, err = writeDeployFile(root, ".env.example", []byte(deployEnvExample()), "env_example")
	if err != nil {
		return nil, err
	}
	files = append(files, written)
	return files, nil
}

func validateDeployRenderRequest(request model.ContainerOpsDeployRenderRequest) error {
	if request.Manifest.ComposeProject != "cpamp-cpa" {
		return fmt.Errorf("unsupported compose project %q", request.Manifest.ComposeProject)
	}
	if request.Manifest.Network != "cpamp-cpa_default" || request.Compose.NetworkName != "cpamp-cpa_default" {
		return errors.New("unsupported CPA network")
	}
	if request.Compose.ProjectName != "cpamp-cpa" {
		return fmt.Errorf("unsupported compose draft project %q", request.Compose.ProjectName)
	}
	if !strings.Contains(request.Compose.Content, "name: cpamp-cpa") ||
		!strings.Contains(request.Compose.Content, "cli-proxy-api") ||
		!strings.Contains(request.Compose.Content, "cpa-manager-plus") ||
		!strings.Contains(request.Compose.Content, "cpamp-agent") {
		return errors.New("compose content does not look like a CPAMP CPA stack")
	}
	return nil
}

func writeDeployFile(root string, name string, data []byte, kind string) (model.ContainerOpsDeployFile, error) {
	if filepath.Base(name) != name {
		return model.ContainerOpsDeployFile{}, fmt.Errorf("unsafe deploy file name %q", name)
	}
	path := filepath.Join(root, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return model.ContainerOpsDeployFile{}, fmt.Errorf("write %s: %w", name, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return model.ContainerOpsDeployFile{}, fmt.Errorf("commit %s: %w", name, err)
	}
	return model.ContainerOpsDeployFile{
		Path: path,
		Kind: kind,
		Size: int64(len(data)),
	}, nil
}

func cleanStackRoot(raw string) string {
	root := strings.TrimSpace(raw)
	if root == "" {
		root = defaultStackRoot
	}
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		root = filepath.Join(defaultStackRoot, root)
	}
	return root
}

func deployEnvExample() string {
	return strings.Join([]string{
		"CPA_MANAGER_ADMIN_KEY=replace-with-a-long-random-admin-key",
		"CPA_MANAGEMENT_KEY=replace-with-cpa-management-key",
		"CPAMP_AGENT_TOKEN=replace-with-a-long-random-agent-token",
		"",
	}, "\n")
}
