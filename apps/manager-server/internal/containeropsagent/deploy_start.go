package containeropsagent

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
	Role  string
	Name  string
	Image string
	Env   []string
	// ConfigYAML is deployment bootstrap data for the CPA container. It is
	// written into the mounted data volume before the container is started and
	// is intentionally not exposed in API responses or lifecycle logs.
	ConfigYAML   []byte
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
	if _, err := deployPullImages(request.Manifest, request.AllowCustomImages); err != nil {
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
	actions := make([]model.ContainerOpsDeployAction, 0, 11)
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
	add("seed_cpa_config", "cli-proxy-api", "Initialize config.yaml in the CPA data volume before the first start.")
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
	checks := make([]model.ContainerOpsDeployCheck, 0, 8)
	if err := scanner.Err(); err != nil {
		checks = append(checks, deployAgentCheck("error", "deploy_env_unreadable", "Deploy .env could not be read.", envPath, true))
		return values, checks
	}
	// The release checker also accepts the public license settings from the
	// customer CPA config. Keep the agent consistent with that path: generated
	// stacks may have a real public key in cliproxyapi/config.yaml while an old
	// .env file still has the key commented out or left empty. Environment
	// values remain authoritative; only missing/placeholder values are filled
	// from the local config, and secret material is never read from YAML.
	licenseConfigValues := readDeployLicenseConfigValues(root)
	for key, value := range licenseConfigValues {
		if deployEnvValueMissing(values[key]) && strings.TrimSpace(value) != "" {
			values[key] = value
		}
	}
	required := []string{"CPA_MANAGER_ADMIN_KEY", "CPA_MANAGEMENT_KEY", "CPAMP_AGENT_TOKEN"}
	for _, key := range required {
		value := strings.TrimSpace(values[key])
		if deployEnvValueMissing(value) {
			checks = append(checks, deployAgentCheck("error", "deploy_env_secret_missing", fmt.Sprintf("%s must be set in deploy .env before services can be started.", key), key, true))
		}
	}
	// CPA-CLI verifies signed licenses and server-issued grace leases with the
	// publisher public key. Keep this value in the deployment .env (or the local
	// CPA license section, which is accepted as a compatibility fallback) so the
	// agent can inject it into the CPA container without placing it in a
	// manifest or API response.
	if deployEnvValueMissing(values["CPA_LICENSE_PUBLIC_KEY"]) {
		checks = append(checks, deployAgentCheck("error", "deploy_env_license_public_key_missing", "CPA_LICENSE_PUBLIC_KEY must be set to the storefront Ed25519 public key before CPA can start.", "CPA_LICENSE_PUBLIC_KEY", true))
	} else if !validDeployLicensePublicKey(values["CPA_LICENSE_PUBLIC_KEY"]) {
		checks = append(checks, deployAgentCheck("error", "deploy_env_license_public_key_invalid", "CPA_LICENSE_PUBLIC_KEY must be a base64/base64url or hex Ed25519 public key (32 decoded bytes).", "CPA_LICENSE_PUBLIC_KEY", true))
	}
	if value := strings.TrimSpace(values["CPA_LICENSE_PLUGIN_PUBLIC_KEY"]); value != "" && deployEnvValueMissing(value) {
		checks = append(checks, deployAgentCheck("error", "deploy_env_license_plugin_public_key_invalid", "CPA_LICENSE_PLUGIN_PUBLIC_KEY is configured but still contains a placeholder.", "CPA_LICENSE_PLUGIN_PUBLIC_KEY", true))
	}
	if value := strings.TrimSpace(values["CPA_LICENSE_CLIENT_SECRET"]); value != "" && deployEnvValueMissing(value) {
		checks = append(checks, deployAgentCheck("error", "deploy_env_license_client_secret_invalid", "CPA_LICENSE_CLIENT_SECRET is configured but still contains a placeholder.", "CPA_LICENSE_CLIENT_SECRET", true))
	}
	secretFileKey := "CPA_LICENSE_CLIENT_SECRET_HOST_PATH"
	secretFile := strings.TrimSpace(values[secretFileKey])
	if secretFile == "" {
		// Keep accepting the original variable name for existing stacks. New
		// compose drafts use *_HOST_PATH to distinguish it from the stable
		// in-container file path.
		secretFileKey = "CPA_LICENSE_CLIENT_SECRET_FILE"
		secretFile = strings.TrimSpace(values[secretFileKey])
	}
	if secretFile != "" {
		if deployEnvValueMissing(secretFile) {
			checks = append(checks, deployAgentCheck("error", "deploy_env_license_client_secret_file_invalid", fmt.Sprintf("%s is configured but still contains a placeholder.", secretFileKey), secretFileKey, true))
		} else {
			resolvedSecretFile := secretFile
			if !filepath.IsAbs(resolvedSecretFile) {
				resolvedSecretFile = filepath.Join(root, strings.TrimPrefix(resolvedSecretFile, "./"))
			}
			if info, err := os.Stat(resolvedSecretFile); err != nil || info.IsDir() {
				checks = append(checks, deployAgentCheck("error", "deploy_env_license_client_secret_file_missing", fmt.Sprintf("%s must point to a readable host file before CPA can start.", secretFileKey), resolvedSecretFile, true))
			} else {
				values["CPA_LICENSE_CLIENT_SECRET_HOST_PATH"] = resolvedSecretFile
				values["CPA_LICENSE_CLIENT_SECRET_FILE"] = resolvedSecretFile
			}
		}
	}
	if len(checks) == 0 {
		checks = append(checks, deployAgentCheck("info", "deploy_env_ready", "Deploy .env contains the required CPA, license, and Agent settings.", envPath, false))
	}
	return values, checks
}

// readDeployLicenseConfigValues reads only scalar values under the top-level
// license section from known customer config locations. It deliberately uses a
// small, non-evaluating parser instead of unmarshalling arbitrary YAML: this
// file is deployment input and may contain provider-specific structures that
// are irrelevant to the agent's preflight checks.
func readDeployLicenseConfigValues(root string) map[string]string {
	// Only publisher metadata and the non-secret client identifier may fall
	// back to YAML. Runtime endpoints, durations, storage keys, and all secret
	// values remain environment-owned so an old config cannot silently change a
	// deployment's policy.
	keys := map[string]string{
		"public-key":        "CPA_LICENSE_PUBLIC_KEY",
		"plugin-public-key": "CPA_LICENSE_PLUGIN_PUBLIC_KEY",
		"client-id":         "CPA_LICENSE_CLIENT_ID",
	}
	result := make(map[string]string, len(keys))
	root = cleanStackRoot(root)
	paths := []string{
		filepath.Join(root, "cliproxyapi", "config.yaml"),
		filepath.Join(root, "config.yaml"),
		filepath.Join(root, "data", "cpa", "config.yaml"),
	}
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		parsed := parseDeployLicenseYAML(file, keys)
		_ = file.Close()
		for envKey, value := range parsed {
			if strings.TrimSpace(result[envKey]) == "" {
				result[envKey] = value
			}
		}
	}
	return result
}

func parseDeployLicenseYAML(reader io.Reader, keys map[string]string) map[string]string {
	result := make(map[string]string, len(keys))
	scanner := bufio.NewScanner(reader)
	inLicense := false
	licenseIndent := -1
	for scanner.Scan() {
		raw := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " \t"))
		if key, value, ok := strings.Cut(trimmed, ":"); ok && indent == 0 && strings.TrimSpace(key) == "license" && strings.TrimSpace(value) == "" {
			inLicense = true
			licenseIndent = indent
			continue
		}
		if !inLicense {
			continue
		}
		if indent <= licenseIndent {
			inLicense = false
			continue
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		envKey, ok := keys[strings.TrimSpace(key)]
		if !ok {
			continue
		}
		value = normalizeDeployYAMLScalar(value)
		if value != "" {
			result[envKey] = value
		}
	}
	return result
}

func normalizeDeployYAMLScalar(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	// Strip a YAML inline comment before unquoting. A comment is recognized
	// only when preceded by whitespace, so URL fragments and base64 material
	// containing a literal '#' are left intact.
	if comment := strings.Index(value, " #"); comment >= 0 {
		value = strings.TrimSpace(value[:comment])
	}
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		if unquoted, err := strconv.Unquote(value); err == nil {
			return strings.TrimSpace(unquoted)
		}
		return strings.TrimSpace(value[1 : len(value)-1])
	}
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return strings.TrimSpace(value[1 : len(value)-1])
	}
	return strings.TrimSpace(value)
}

func deployEnvValueMissing(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "?set ") {
		return true
	}
	lower := strings.ToLower(value)
	return strings.HasPrefix(lower, "replace-with") || strings.HasPrefix(lower, "changeme")
}

func validDeployLicensePublicKey(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, decoder := range []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.StdEncoding,
	} {
		decoded, err := decoder.DecodeString(value)
		if err == nil && len(decoded) == ed25519.PublicKeySize {
			return true
		}
	}
	if decoded, err := hex.DecodeString(strings.TrimPrefix(value, "0x")); err == nil && len(decoded) == ed25519.PublicKeySize {
		return true
	}
	return false
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
	cpaLicenseEnv, cpaLicenseBinds := deployCPALicenseEnv(env)
	return []deployServiceSpec{
		{
			Role:  "cpa",
			Name:  "cli-proxy-api",
			Image: deployManifestImage(manifest, "cpa"),
			Env:   cpaLicenseEnv,
			// Keep the runtime command explicit. The CPA image intentionally
			// has no default config path because customer installs use a named
			// volume mounted at /app/data.
			Cmd:          []string{"./CLIProxyAPI", "-config", "/app/data/config.yaml"},
			ConfigYAML:   renderCPADeployConfig(env),
			VolumeMounts: map[string]string{deployVolumeName(manifest.ComposeProject, "cpa-data"): "/app/data"},
			Binds:        cpaLicenseBinds,
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

// renderCPADeployConfig returns the smallest valid CPA config needed for a
// customer-host deployment. License values are also passed as environment
// overrides, but keeping the public settings in config.yaml makes the
// installation self-describing and gives remote-management a durable key.
// The client secret is deliberately excluded; it is supplied through the
// environment/Docker secret mount by deployCPALicenseEnv.
func renderCPADeployConfig(env map[string]string) []byte {
	valueOr := func(key string, fallback string) string {
		if value := strings.TrimSpace(env[key]); value != "" {
			return value
		}
		return fallback
	}
	quote := func(value string) string { return strconv.Quote(value) }

	lines := []string{
		"host: \"\"",
		"port: 8317",
		"remote-management:",
		"  allow-remote: true",
		"  secret-key: " + quote(strings.TrimSpace(env["CPA_MANAGEMENT_KEY"])),
		"  disable-control-panel: true",
		"license:",
		"  provider: " + quote(valueOr("CPA_LICENSE_PROVIDER", "shop666")),
		"  product-code: " + quote(valueOr("CPA_LICENSE_PRODUCT_CODE", "CPA")),
		"  api-base-url: " + quote(valueOr("CPA_LICENSE_API_BASE_URL", "https://p.666ttt.net/api/storefront")),
		"  public-key: " + quote(strings.TrimSpace(env["CPA_LICENSE_PUBLIC_KEY"])),
		"  plugin-public-key: " + quote(strings.TrimSpace(env["CPA_LICENSE_PLUGIN_PUBLIC_KEY"])),
		"  client-id: " + quote(strings.TrimSpace(env["CPA_LICENSE_CLIENT_ID"])),
		"  state-dir: " + quote(valueOr("CPA_LICENSE_STATE_DIR", "/app/data/license")),
		"  shop-auth-url: " + quote(valueOr("CPA_LICENSE_SHOP_AUTH_URL", "https://p.666ttt.net/shop/?authorize=cpa")),
		"  shop-exchange-path: " + quote(valueOr("CPA_LICENSE_SHOP_EXCHANGE_PATH", "/licenses/exchange")),
		"  activate-path: " + quote(valueOr("CPA_LICENSE_ACTIVATE_PATH", "/licenses/activate")),
		"  refresh-path: " + quote(valueOr("CPA_LICENSE_REFRESH_PATH", "/licenses/refresh")),
		"  verify-path: " + quote(valueOr("CPA_LICENSE_VERIFY_PATH", "/licenses/verify")),
		"  grace-path: " + quote(valueOr("CPA_LICENSE_GRACE_PATH", "/licenses/grace")),
		"  refresh-interval: " + quote(valueOr("CPA_LICENSE_REFRESH_INTERVAL", "10m")),
		"  grace-period: " + quote(valueOr("CPA_LICENSE_GRACE_PERIOD", "6h")),
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
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// ensureCPAConfig writes config.yaml into the container's mounted /app/data
// volume only when it is missing. Docker's archive endpoint works while the
// container is stopped, so no helper image or host-specific volume path is
// needed. The function returns true when a file was seeded.
func (c *DockerClient) ensureCPAConfig(ctx context.Context, container string, config []byte) (bool, error) {
	container = strings.TrimSpace(container)
	if container == "" {
		return false, fmt.Errorf("CPA container name is empty")
	}
	if len(bytes.TrimSpace(config)) == 0 {
		return false, fmt.Errorf("CPA bootstrap config is empty")
	}
	exists, err := c.containerPathExists(ctx, container, "/app/data/config.yaml")
	if err != nil {
		return false, fmt.Errorf("check /app/data/config.yaml: %w", err)
	}
	if exists {
		return false, nil
	}
	archive, err := tarArchive("config.yaml", config)
	if err != nil {
		return false, fmt.Errorf("build CPA config archive: %w", err)
	}
	if err := c.putContainerArchive(ctx, container, "/app/data", archive); err != nil {
		return false, fmt.Errorf("write /app/data/config.yaml: %w", err)
	}
	return true, nil
}

func tarArchive(name string, data []byte) ([]byte, error) {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return nil, fmt.Errorf("invalid archive file name")
	}
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	if err := writer.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o600,
		Size: int64(len(data)),
	}); err != nil {
		return nil, err
	}
	if _, err := writer.Write(data); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func (c *DockerClient) containerPathExists(ctx context.Context, container string, path string) (bool, error) {
	endpoint := fmt.Sprintf(
		"http://docker/containers/%s/archive?path=%s",
		url.PathEscape(strings.TrimSpace(container)),
		url.QueryEscape(path),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("docker api status %d", resp.StatusCode)
	}
	return true, nil
}

func (c *DockerClient) putContainerArchive(ctx context.Context, container string, path string, archive []byte) error {
	endpoint := fmt.Sprintf(
		"http://docker/containers/%s/archive?path=%s",
		url.PathEscape(strings.TrimSpace(container)),
		url.QueryEscape(path),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(archive))
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
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// deployCPALicenseEnv maps deployment-only values into the CPA container.
// The public key is required; optional client credentials and endpoint
// overrides are included only when present. A host secret file is mounted at
// a stable container path so the CPA process can read it without exposing its
// contents in a compose draft or stack manifest.
func deployCPALicenseEnv(env map[string]string) ([]string, []string) {
	valueOr := func(key string, fallback string) string {
		if value := strings.TrimSpace(env[key]); value != "" {
			return value
		}
		return fallback
	}
	result := []string{
		"CPA_LICENSE_PROVIDER=" + valueOr("CPA_LICENSE_PROVIDER", "shop666"),
		"CPA_LICENSE_PRODUCT_CODE=" + valueOr("CPA_LICENSE_PRODUCT_CODE", "CPA"),
		"CPA_LICENSE_PUBLIC_KEY=" + strings.TrimSpace(env["CPA_LICENSE_PUBLIC_KEY"]),
		"CPA_LICENSE_CLIENT_SECRET_FILE=/run/secrets/cpa-license-client-secret",
	}
	for _, item := range []struct {
		key      string
		fallback string
	}{
		{"CPA_LICENSE_PLUGIN_PUBLIC_KEY", ""},
		{"CPA_LICENSE_CLIENT_ID", ""},
		{"CPA_LICENSE_API_BASE_URL", "https://p.666ttt.net/api/storefront"},
		{"CPA_LICENSE_CLIENT_SECRET", ""},
		{"CPA_LICENSE_STATE_DIR", "/app/data/license"},
		{"CPA_LICENSE_SHOP_AUTH_URL", "https://p.666ttt.net/shop/?authorize=cpa"},
		{"CPA_LICENSE_SHOP_EXCHANGE_PATH", "/licenses/exchange"},
		{"CPA_LICENSE_ACTIVATE_PATH", "/licenses/activate"},
		{"CPA_LICENSE_REFRESH_PATH", "/licenses/refresh"},
		{"CPA_LICENSE_VERIFY_PATH", "/licenses/verify"},
		{"CPA_LICENSE_GRACE_PATH", "/licenses/grace"},
		{"CPA_LICENSE_REFRESH_INTERVAL", "10m"},
		{"CPA_LICENSE_GRACE_PERIOD", "6h"},
		{"CPA_LICENSE_STORAGE_KEY", ""},
		{"CPA_LICENSE_EXECUTABLE_SHA256", ""},
		{"CPA_LICENSE_CLAIM_PATH", ""},
	} {
		result = append(result, item.key+"="+valueOr(item.key, item.fallback))
	}
	binds := make([]string, 0, 1)
	secretFile := strings.TrimSpace(env["CPA_LICENSE_CLIENT_SECRET_HOST_PATH"])
	if secretFile == "" {
		secretFile = strings.TrimSpace(env["CPA_LICENSE_CLIENT_SECRET_FILE"])
	}
	if secretFile != "" {
		binds = append(binds, secretFile+":/run/secrets/cpa-license-client-secret:ro")
	} else {
		// Keep the optional file readable inside the container without making
		// key-only installations depend on a host secret.
		binds = append(binds, "/dev/null:/run/secrets/cpa-license-client-secret:ro")
	}
	return result, binds
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

	// A named volume starts empty on a clean customer host. Seed the CPA
	// config after the container has been created (so the volume is mounted)
	// but before any service is started. Existing config.yaml files are kept
	// intact so a retry or an imported deployment cannot overwrite settings.
	for _, spec := range specs {
		if spec.Role != "cpa" {
			continue
		}
		seeded, err := c.ensureCPAConfig(ctx, spec.Name, spec.ConfigYAML)
		if err != nil {
			return fmt.Errorf("initialize CPA config: %w", err)
		}
		if seeded {
			deployMarkAction(result.Actions, "seed_cpa_config", "applied", "CPA config.yaml was written to the mounted data volume.")
		} else {
			deployMarkAction(result.Actions, "seed_cpa_config", "skipped", "Existing CPA config.yaml was preserved.")
		}
		break
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
