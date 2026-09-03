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

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/containeropsimage"
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
	for _, dir := range []string{
		filepath.Join(root, "cliproxyapi", "auths"),
		filepath.Join(root, "cliproxyapi", "data"),
		filepath.Join(root, "cliproxyapi", "data", "license"),
		filepath.Join(root, "cliproxyapi", "logs"),
		filepath.Join(root, "secrets"),
	} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create deploy directory %s: %w", dir, err)
		}
	}

	manifestData, err := json.MarshalIndent(request.Manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal stack manifest: %w", err)
	}
	manifestData = append(manifestData, '\n')

	files := make([]model.ContainerOpsDeployFile, 0, 4)
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
	secretPath := filepath.Join(root, "secrets", "cpa-license-client-secret")
	if _, err := os.Stat(secretPath); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("inspect client secret placeholder: %w", err)
		}
		if err := os.WriteFile(secretPath, nil, 0o600); err != nil {
			return nil, fmt.Errorf("write client secret placeholder: %w", err)
		}
	}
	// Keep the CPA config in the stack checkout so a customer can start the
	// rendered Compose file immediately. Secrets are injected through Compose
	// environment/Docker secrets and are deliberately absent from this file.
	written, err = writeDeployFilePreserve(root, filepath.Join("cliproxyapi", "config.yaml"), []byte(deployCPAConfigExample()), "cpa_config")
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
	seen := make(map[string]bool, 3)
	for _, service := range request.Manifest.Services {
		if !service.IncludeInCompose {
			continue
		}
		role := strings.TrimSpace(service.Role)
		if role != "cpa" && role != "cpamp" && role != "agent" {
			return fmt.Errorf("unsupported deploy service role %q", role)
		}
		if service.Image == "" || !containeropsimage.Allowed(role, service.Image, request.AllowCustomImages) {
			return fmt.Errorf("unsupported %s deploy image %q", role, service.Image)
		}
		seen[role] = true
	}
	for _, role := range []string{"cpa", "cpamp", "agent"} {
		if !seen[role] {
			return fmt.Errorf("missing %s deploy service", role)
		}
	}
	return nil
}

func writeDeployFile(root string, name string, data []byte, kind string) (model.ContainerOpsDeployFile, error) {
	relative := filepath.Clean(name)
	if relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return model.ContainerOpsDeployFile{}, fmt.Errorf("unsafe deploy file name %q", name)
	}
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return model.ContainerOpsDeployFile{}, fmt.Errorf("create directory for %s: %w", name, err)
	}
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

func writeDeployFilePreserve(root string, name string, data []byte, kind string) (model.ContainerOpsDeployFile, error) {
	relative := filepath.Clean(name)
	if relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return model.ContainerOpsDeployFile{}, fmt.Errorf("unsafe deploy file name %q", name)
	}
	path := filepath.Join(root, relative)
	if info, err := os.Stat(path); err == nil {
		if info.IsDir() {
			return model.ContainerOpsDeployFile{}, fmt.Errorf("deploy file path is a directory: %s", path)
		}
		return model.ContainerOpsDeployFile{Path: path, Kind: kind, Size: info.Size()}, nil
	} else if !os.IsNotExist(err) {
		return model.ContainerOpsDeployFile{}, fmt.Errorf("inspect %s: %w", name, err)
	}
	return writeDeployFile(root, relative, data, kind)
}

func deployCPAConfigExample() string {
	return strings.Join([]string{
		"host: \"\"",
		"port: 8317",
		"remote-management:",
		"  # MANAGEMENT_PASSWORD is injected by Compose from CPA_MANAGEMENT_KEY.",
		"  secret-key: \"\"",
		"  allow-remote: true",
		"  disable-control-panel: true",
		"license:",
		"  provider: \"shop666\"",
		"  product-code: \"CPA\"",
		"  api-base-url: \"https://p.666ttt.net/api/storefront\"",
		"  # Publisher keys are public release metadata; client values are injected through Compose.",
		"  public-key: \"kJhDRBpfneFdURvPXwiGW3XAmPrd2HVVORfHzP-eYTg\"",
		"  plugin-public-key: \"OHRHVVIlFC34K-5AQUkOPcZLeiSpeX_n_VPbrH3agXQ\"",
		"  client-id: \"\"",
		"  state-dir: \"/app/data/license\"",
		"  shop-auth-url: \"https://p.666ttt.net/shop/?authorize=cpa\"",
		"  shop-exchange-path: \"/licenses/exchange\"",
		"  activate-path: \"/licenses/activate\"",
		"  refresh-path: \"/licenses/refresh\"",
		"  verify-path: \"/licenses/verify\"",
		"  grace-path: \"/licenses/grace\"",
		"  refresh-interval: \"10m\"",
		"  grace-period: \"6h\"",
		"  instance-binding: \"strict\"",
		"auth-dir: \"/app/data/auths\"",
		"api-keys: []",
		"debug: false",
		"commercial-mode: false",
		"logging-to-file: false",
		"usage-statistics-enabled: true",
		"request-retry: 3",
		"max-retry-credentials: 0",
		"max-retry-interval: 30",
		"routing:",
		"  strategy: \"round-robin\"",
		"",
	}, "\n")
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
		"# CPA storefront license settings (the public key is required)",
		"CPA_LICENSE_PROVIDER=shop666",
		"CPA_LICENSE_PRODUCT_CODE=CPA",
		"CPA_LICENSE_PUBLIC_KEY=kJhDRBpfneFdURvPXwiGW3XAmPrd2HVVORfHzP-eYTg",
		"CPA_LICENSE_PLUGIN_PUBLIC_KEY=OHRHVVIlFC34K-5AQUkOPcZLeiSpeX_n_VPbrH3agXQ",
		"CPA_LICENSE_CLIENT_ID=",
		"# Set either CPA_LICENSE_CLIENT_SECRET or a host-readable secret file path.",
		"CPA_LICENSE_CLIENT_SECRET=",
		"CPA_LICENSE_CLIENT_SECRET_HOST_PATH=",
		"# CPA_LICENSE_CLIENT_SECRET_FILE is accepted as a legacy host path;",
		"# generated containers receive the stable /run/secrets path.",
		"CPA_LICENSE_CLIENT_SECRET_FILE=",
		"CPA_LICENSE_API_BASE_URL=https://p.666ttt.net/api/storefront",
		"CPA_LICENSE_STATE_DIR=/app/data/license",
		"CPA_LICENSE_SHOP_AUTH_URL=https://p.666ttt.net/shop/?authorize=cpa",
		"CPA_LICENSE_SHOP_EXCHANGE_PATH=/licenses/exchange",
		"CPA_LICENSE_ACTIVATE_PATH=/licenses/activate",
		"CPA_LICENSE_REFRESH_PATH=/licenses/refresh",
		"CPA_LICENSE_VERIFY_PATH=/licenses/verify",
		"CPA_LICENSE_GRACE_PATH=/licenses/grace",
		"CPA_LICENSE_REFRESH_INTERVAL=10m",
		"CPA_LICENSE_GRACE_PERIOD=6h",
		"CPA_LICENSE_STORAGE_KEY=",
		"CPA_LICENSE_EXECUTABLE_SHA256=",
		"CPA_LICENSE_CLAIM_PATH=",
		"",
	}, "\n")
}
