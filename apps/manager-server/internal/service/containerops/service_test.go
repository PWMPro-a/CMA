package containerops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/testutil"
)

func TestInfoDefaultsToReadOnlyWithoutAgent(t *testing.T) {
	service := New(Options{})
	info, err := service.Info(context.Background())
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if !info.Enabled {
		t.Fatalf("enabled = false")
	}
	if info.Mode != "read_only" {
		t.Fatalf("mode = %q", info.Mode)
	}
	if info.Agent.Configured || info.Agent.Reachable {
		t.Fatalf("agent = %#v", info.Agent)
	}
	if info.DestructiveActions {
		t.Fatalf("destructive actions should be disabled without agent")
	}
	if info.NewAPI.RecommendedBaseURL != "http://host.docker.internal:8317/v1" {
		t.Fatalf("recommended base url = %q", info.NewAPI.RecommendedBaseURL)
	}
	if info.Lifecycle.Status != lifecycleStatusIdle || info.Lifecycle.Active {
		t.Fatalf("lifecycle = %#v", info.Lifecycle)
	}
}

func TestInfoReportsIdleLifecycleInitially(t *testing.T) {
	service := New(Options{})
	info, err := service.Info(context.Background())
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Lifecycle.Status != lifecycleStatusIdle {
		t.Fatalf("lifecycle status = %q", info.Lifecycle.Status)
	}
	if info.Lifecycle.Active || info.Lifecycle.OperationID != "" || info.Lifecycle.Operation != "" {
		t.Fatalf("lifecycle = %#v", info.Lifecycle)
	}
}

func TestInfoUsesAgentModeAndSanitizesURL(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/info" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(model.ContainerOpsAgentInfo{
			Service:    "cpamp-agent",
			Version:    "test",
			DockerHost: "unix:///var/run/docker.sock",
			ReadOnly:   true,
		})
	}))
	t.Cleanup(agent.Close)

	agentURL := strings.Replace(agent.URL, "http://", "http://user:secret@", 1)
	service := New(Options{AgentURL: agentURL})
	info, err := service.Info(context.Background())
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Mode != "agent" {
		t.Fatalf("mode = %q", info.Mode)
	}
	if !info.Agent.Configured || !info.Agent.Reachable {
		t.Fatalf("agent = %#v", info.Agent)
	}
	if info.Agent.BaseURL != agent.URL {
		t.Fatalf("agent base url = %q", info.Agent.BaseURL)
	}
	if !info.Agent.ReadOnly {
		t.Fatalf("agent read only = false")
	}
	if info.DestructiveActions {
		t.Fatalf("destructive actions should stay disabled while the agent is read-only")
	}
}

func TestInfoEnablesDestructiveActionsForWritableAgent(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/info" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(model.ContainerOpsAgentInfo{
			Service:  "cpamp-agent",
			Version:  "test",
			Mode:     "agent",
			ReadOnly: false,
		})
	}))
	t.Cleanup(agent.Close)

	service := New(Options{AgentURL: agent.URL})
	info, err := service.Info(context.Background())
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if !info.DestructiveActions {
		t.Fatalf("destructive actions should be enabled for a reachable writable agent")
	}
}

func TestDiscoverUsesAgentOverviewAndToken(t *testing.T) {
	var overviewAuthorized bool
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/agent/info":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsAgentInfo{
				Service:  "cpamp-agent",
				Version:  "test",
				Mode:     "agent",
				ReadOnly: true,
			})
		case "/docker/overview":
			overviewAuthorized = true
			_ = json.NewEncoder(w).Encode(model.ContainerOpsDockerOverview{
				Summary: model.ContainerOpsDockerSummary{
					ContainerCount: 2,
					RunningCount:   2,
					CPACount:       1,
					NewAPICount:    1,
				},
				Containers: []model.ContainerOpsDockerContainer{
					{Name: "cli-proxy-api", Role: "cpa", State: "running"},
					{Name: "new-api", Role: "newapi", State: "running"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(agent.Close)

	service := New(Options{AgentURL: agent.URL, AgentToken: "agent-token"})
	discovery, err := service.Discover(context.Background())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if !overviewAuthorized {
		t.Fatalf("docker overview was not requested")
	}
	if !discovery.Agent.Configured || !discovery.Agent.Reachable || !discovery.Agent.ReadOnly {
		t.Fatalf("agent = %#v", discovery.Agent)
	}
	if discovery.Docker.Summary.CPACount != 1 || discovery.Docker.Summary.NewAPICount != 1 {
		t.Fatalf("summary = %#v", discovery.Docker.Summary)
	}
	if discovery.RecommendedAction != "verify_newapi_internal_route" {
		t.Fatalf("recommended action = %q", discovery.RecommendedAction)
	}
	if discovery.NewAPI.RecommendedBaseURL != "http://host.docker.internal:8317/v1" {
		t.Fatalf("recommended base url = %q", discovery.NewAPI.RecommendedBaseURL)
	}
}

func TestImportPlanBuildsManifestRisksAndComposeDraft(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent/info":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsAgentInfo{
				Service:  "cpamp-agent",
				Version:  "test",
				Mode:     "agent",
				ReadOnly: true,
			})
		case "/docker/overview":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsDockerOverview{
				Summary: model.ContainerOpsDockerSummary{
					ContainerCount: 3,
					RunningCount:   3,
					CPACount:       1,
					CPAMPCount:     1,
					NewAPICount:    1,
				},
				Containers: []model.ContainerOpsDockerContainer{
					{
						ID:      "cpa123",
						Name:    "legacy-cpa",
						Image:   "seakee/cli-proxy-api:v1",
						State:   "running",
						Role:    "cpa",
						Managed: false,
						Ports: []model.ContainerOpsDockerPort{
							{PrivatePort: 8317, PublicPort: 8317, Type: "tcp"},
						},
						Mounts: []model.ContainerOpsDockerMount{
							{Type: "volume", Name: "cpa-data", Destination: "/app/data", RW: true},
						},
						Networks: []model.ContainerOpsDockerAttachment{{Name: "legacy_net"}},
					},
					{
						ID:       "cpamp123",
						Name:     "cpa-manager-plus",
						Image:    "seakee/cpa-manager-plus:v1",
						State:    "running",
						Role:     "cpamp",
						Managed:  true,
						Networks: []model.ContainerOpsDockerAttachment{{Name: "cpamp-cpa_default"}},
					},
					{
						ID:       "newapi123",
						Name:     "new-api",
						Image:    "calciumion/new-api:latest",
						State:    "running",
						Role:     "newapi",
						Networks: []model.ContainerOpsDockerAttachment{{Name: "legacy_net"}},
					},
				},
				Networks: []model.ContainerOpsDockerNetwork{
					{Name: "cpamp-cpa_default", Driver: "bridge", Managed: false, Containers: 1},
					{Name: "legacy_net", Driver: "bridge", Managed: false, Containers: 2},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(agent.Close)

	service := New(Options{AgentURL: agent.URL})
	plan, err := service.ImportPlan(context.Background())
	if err != nil {
		t.Fatalf("import plan: %v", err)
	}
	if !plan.ReadOnly || !plan.Agent.ReadOnly {
		t.Fatalf("plan should be read-only: %#v", plan.Agent)
	}
	if !plan.Summary.Ready || !plan.Summary.CPAFound || !plan.Summary.NewAPIFound {
		t.Fatalf("summary = %#v", plan.Summary)
	}
	if plan.Manifest.ComposeProject != "cpamp-cpa" || plan.Manifest.Network != "cpamp-cpa_default" {
		t.Fatalf("manifest = %#v", plan.Manifest)
	}
	if len(plan.Manifest.Services) != 4 || plan.Manifest.Services[0].InternalBaseURL != "http://host.docker.internal:8317/v1" {
		t.Fatalf("services = %#v", plan.Manifest.Services)
	}
	if !strings.Contains(plan.Compose.Content, "image: seakee/cli-proxy-api:v1") ||
		!strings.Contains(plan.Compose.Content, "entrypoint: [\"cpamp-agent\"]") ||
		!strings.Contains(plan.Compose.Content, "/opt/cpamp/stacks/cpa:/opt/cpamp/stacks/cpa") ||
		!strings.Contains(plan.Compose.Content, "cpa-data:") {
		t.Fatalf("compose draft =\n%s", plan.Compose.Content)
	}
	if !hasImportRisk(plan.Risks, "managed_label_missing") ||
		!hasImportRisk(plan.Risks, "service_name_mismatch") ||
		!hasImportRisk(plan.Risks, "network_label_missing") {
		t.Fatalf("risks = %#v", plan.Risks)
	}
	if !hasImportAction(plan.NextActions, "apply_cpamp_labels") {
		t.Fatalf("next actions = %#v", plan.NextActions)
	}
}

func TestImportPlanBlocksWhenCPAIsMissing(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent/info":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsAgentInfo{
				Service:  "cpamp-agent",
				Version:  "test",
				Mode:     "agent",
				ReadOnly: true,
			})
		case "/docker/overview":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsDockerOverview{
				Summary: model.ContainerOpsDockerSummary{ContainerCount: 1, NewAPICount: 1},
				Containers: []model.ContainerOpsDockerContainer{
					{Name: "new-api", Image: "calciumion/new-api:latest", State: "running", Role: "newapi"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(agent.Close)

	service := New(Options{AgentURL: agent.URL})
	plan, err := service.ImportPlan(context.Background())
	if err != nil {
		t.Fatalf("import plan: %v", err)
	}
	if plan.Summary.Ready || plan.Summary.CPAFound || plan.Summary.BlockingRiskCount != 1 {
		t.Fatalf("summary = %#v", plan.Summary)
	}
	if !hasImportRisk(plan.Risks, "cpa_not_found") {
		t.Fatalf("risks = %#v", plan.Risks)
	}
	if len(plan.NextActions) != 1 || plan.NextActions[0] != "deploy_cpa_stack" {
		t.Fatalf("next actions = %#v", plan.NextActions)
	}
}

func TestDeployPlanRendersCleanStackComposeDraft(t *testing.T) {
	var overviewRequested bool
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent/info":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsAgentInfo{
				Service:  "cpamp-agent",
				Version:  "test",
				Mode:     "agent",
				ReadOnly: false,
			})
		case "/docker/overview":
			overviewRequested = true
			_ = json.NewEncoder(w).Encode(model.ContainerOpsDockerOverview{
				Summary:    model.ContainerOpsDockerSummary{},
				Containers: []model.ContainerOpsDockerContainer{},
				Networks:   []model.ContainerOpsDockerNetwork{},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(agent.Close)

	service := New(Options{AgentURL: agent.URL})
	plan, err := service.DeployPlan(context.Background(), model.ContainerOpsDeployRequest{})
	if err != nil {
		t.Fatalf("deploy plan: %v", err)
	}
	if !overviewRequested {
		t.Fatalf("docker overview was not requested")
	}
	if plan.Status != "ready" || !plan.ReadOnly || plan.Applied || plan.Destructive {
		t.Fatalf("plan = %#v", plan)
	}
	if plan.Compose.FileName != "compose.deploy-preview.yml" ||
		!strings.Contains(plan.Compose.Content, "CPA_MANAGER_ADMIN_KEY") ||
		!strings.Contains(plan.Compose.Content, "CPAMP_AGENT_TOKEN") ||
		!strings.Contains(plan.Compose.Content, "/opt/cpamp/stacks/cpa:/opt/cpamp/stacks/cpa") ||
		!strings.Contains(plan.Compose.Content, "cpa-data:") ||
		strings.Contains(plan.Compose.Content, "external: true") {
		t.Fatalf("compose draft =\n%s", plan.Compose.Content)
	}
	if !hasDeployCheck(plan.Checks, "docker_precheck_ready") ||
		!hasDeployCheck(plan.Checks, "standard_network_planned") ||
		!hasDeployCheck(plan.Checks, "readonly_deploy_plan") {
		t.Fatalf("checks = %#v", plan.Checks)
	}
	if len(plan.Steps) == 0 || plan.Steps[len(plan.Steps)-1].Code != "update_newapi_route" {
		t.Fatalf("steps = %#v", plan.Steps)
	}
}

func TestDeployPlanBlocksWhenCPAAlreadyExists(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent/info":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsAgentInfo{
				Service:  "cpamp-agent",
				Version:  "test",
				Mode:     "agent",
				ReadOnly: false,
			})
		case "/docker/overview":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsDockerOverview{
				Summary: model.ContainerOpsDockerSummary{ContainerCount: 1, CPACount: 1},
				Containers: []model.ContainerOpsDockerContainer{
					{Name: "cli-proxy-api", Image: "seakee/cli-proxy-api:latest", State: "running", Role: "cpa"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(agent.Close)

	service := New(Options{AgentURL: agent.URL})
	plan, err := service.DeployPlan(context.Background(), model.ContainerOpsDeployRequest{})
	if err != nil {
		t.Fatalf("deploy plan: %v", err)
	}
	if plan.Status != "blocked" {
		t.Fatalf("status = %q checks=%#v", plan.Status, plan.Checks)
	}
	if !hasDeployCheck(plan.Checks, "cpa_already_exists") {
		t.Fatalf("checks = %#v", plan.Checks)
	}
}

func TestDeployRenderUsesAgentEndpointAndToken(t *testing.T) {
	var renderRequested bool
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/agent/info":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsAgentInfo{
				Service:  "cpamp-agent",
				Version:  "test",
				Mode:     "agent",
				ReadOnly: false,
			})
		case "/docker/overview":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsDockerOverview{
				Summary:    model.ContainerOpsDockerSummary{},
				Containers: []model.ContainerOpsDockerContainer{},
				Networks:   []model.ContainerOpsDockerNetwork{},
			})
		case "/deploys/cpa/render":
			renderRequested = true
			if r.Method != http.MethodPost {
				t.Fatalf("render method = %s", r.Method)
			}
			var request model.ContainerOpsDeployRenderRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode render request: %v", err)
			}
			if request.Manifest.ComposeProject != "cpamp-cpa" ||
				!strings.Contains(request.Compose.Content, "CPAMP_AGENT_TOKEN") {
				t.Fatalf("render request = %#v", request)
			}
			_ = json.NewEncoder(w).Encode(struct {
				Status string                         `json:"status"`
				Files  []model.ContainerOpsDeployFile `json:"files"`
			}{
				Status: "rendered",
				Files: []model.ContainerOpsDeployFile{
					{Path: "/opt/cpamp/stacks/cpa/compose.yml", Kind: "compose", Size: 128},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(agent.Close)

	service := New(Options{AgentURL: agent.URL, AgentToken: "agent-token"})
	plan, err := service.DeployPlan(context.Background(), model.ContainerOpsDeployRequest{Apply: true})
	if err != nil {
		t.Fatalf("deploy render: %v", err)
	}
	if !renderRequested {
		t.Fatalf("render endpoint was not requested")
	}
	if plan.Status != "rendered" || !plan.Applied || plan.ReadOnly || len(plan.Files) != 1 {
		t.Fatalf("plan = %#v", plan)
	}
	if !hasDeployCheck(plan.Checks, "deploy_files_rendered") {
		t.Fatalf("checks = %#v", plan.Checks)
	}
}

func TestDeployPullImagesUsesAgentEndpointAndToken(t *testing.T) {
	var pullRequested bool
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/agent/info":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsAgentInfo{
				Service:  "cpamp-agent",
				Version:  "test",
				Mode:     "agent",
				ReadOnly: false,
			})
		case "/docker/overview":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsDockerOverview{
				Summary:    model.ContainerOpsDockerSummary{},
				Containers: []model.ContainerOpsDockerContainer{},
				Networks:   []model.ContainerOpsDockerNetwork{},
			})
		case "/deploys/cpa/pull-images":
			pullRequested = true
			if r.Method != http.MethodPost {
				t.Fatalf("pull method = %s", r.Method)
			}
			var request model.ContainerOpsDeployRenderRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode pull request: %v", err)
			}
			if request.Manifest.ComposeProject != "cpamp-cpa" ||
				!strings.Contains(request.Compose.Content, "seakee/cli-proxy-api:latest") {
				t.Fatalf("pull request = %#v", request)
			}
			_ = json.NewEncoder(w).Encode(struct {
				Status     string                        `json:"status"`
				ImagePulls []model.ContainerOpsImagePull `json:"imagePulls"`
			}{
				Status: "images_pulled",
				ImagePulls: []model.ContainerOpsImagePull{
					{Image: "seakee/cli-proxy-api:latest", Status: "pulled"},
					{Image: "seakee/cpa-manager-plus:latest", Status: "pulled"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(agent.Close)

	service := New(Options{AgentURL: agent.URL, AgentToken: "agent-token"})
	plan, err := service.DeployPlan(context.Background(), model.ContainerOpsDeployRequest{Apply: true, Action: "pull_images"})
	if err != nil {
		t.Fatalf("deploy pull images: %v", err)
	}
	if !pullRequested {
		t.Fatalf("pull endpoint was not requested")
	}
	if plan.Status != "images_pulled" || !plan.Applied || plan.ReadOnly || len(plan.ImagePulls) != 2 {
		t.Fatalf("plan = %#v", plan)
	}
	if !hasDeployCheck(plan.Checks, "deploy_images_pulled") {
		t.Fatalf("checks = %#v", plan.Checks)
	}
}

func TestDeployStartUsesAgentEndpointAndToken(t *testing.T) {
	var startRequested bool
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/agent/info":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsAgentInfo{
				Service:  "cpamp-agent",
				Version:  "test",
				Mode:     "agent",
				ReadOnly: false,
			})
		case "/docker/overview":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsDockerOverview{
				Summary:    model.ContainerOpsDockerSummary{},
				Containers: []model.ContainerOpsDockerContainer{},
				Networks:   []model.ContainerOpsDockerNetwork{},
			})
		case "/deploys/cpa/start":
			startRequested = true
			if r.Method != http.MethodPost {
				t.Fatalf("start method = %s", r.Method)
			}
			var request model.ContainerOpsDeployRenderRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode start request: %v", err)
			}
			if request.Manifest.ComposeProject != "cpamp-cpa" ||
				!strings.Contains(request.Compose.Content, "cpamp-agent") {
				t.Fatalf("start request = %#v", request)
			}
			_ = json.NewEncoder(w).Encode(struct {
				Status   string                            `json:"status"`
				Checks   []model.ContainerOpsDeployCheck   `json:"checks"`
				Actions  []model.ContainerOpsDeployAction  `json:"actions"`
				Overview *model.ContainerOpsDockerOverview `json:"overview,omitempty"`
			}{
				Status: "started",
				Checks: []model.ContainerOpsDeployCheck{
					{Severity: "info", Code: "deploy_services_started", Message: "ok"},
				},
				Actions: []model.ContainerOpsDeployAction{
					{Order: 1, Code: "create_standard_network", Target: "cpamp-cpa_default", Status: "applied"},
					{Order: 10, Code: "healthcheck_services", Target: "cpamp-cpa_default", Status: "applied"},
				},
				Overview: &model.ContainerOpsDockerOverview{
					Summary: model.ContainerOpsDockerSummary{ContainerCount: 3, RunningCount: 3, CPACount: 1, CPAMPCount: 1},
					Containers: []model.ContainerOpsDockerContainer{
						{Name: "cli-proxy-api", Role: "cpa", State: "running", Managed: true},
						{Name: "cpamp-agent", Role: "agent", State: "running", Managed: true},
						{Name: "cpa-manager-plus", Role: "cpamp", State: "running", Managed: true},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(agent.Close)

	service := New(Options{AgentURL: agent.URL, AgentToken: "agent-token"})
	plan, err := service.DeployPlan(context.Background(), model.ContainerOpsDeployRequest{Apply: true, Action: "start_services"})
	if err != nil {
		t.Fatalf("deploy start: %v", err)
	}
	if !startRequested {
		t.Fatalf("start endpoint was not requested")
	}
	if plan.Status != "started" || !plan.Applied || plan.ReadOnly || len(plan.Actions) != 2 || plan.Overview == nil {
		t.Fatalf("plan = %#v", plan)
	}
	if !hasDeployCheck(plan.Checks, "deploy_services_started") {
		t.Fatalf("checks = %#v", plan.Checks)
	}
	if !hasDeployAction(plan.Actions, "healthcheck_services", "applied") {
		t.Fatalf("actions = %#v", plan.Actions)
	}
}

func TestBackupUsesAgentEndpointAndToken(t *testing.T) {
	var backupRequested bool
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/agent/info":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsAgentInfo{
				Service:  "cpamp-agent",
				Version:  "test",
				Mode:     "agent",
				ReadOnly: true,
			})
		case "/backups/cpa":
			backupRequested = true
			if r.Method != http.MethodPost {
				t.Fatalf("backup method = %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(model.ContainerOpsBackupResult{
				BackupID:   "cpa-20260610T010203Z",
				Status:     "completed",
				BackupRoot: "/opt/cpamp/backups",
				CreatedAt:  1781053323,
				Archives: []model.ContainerOpsBackupArchive{
					{Role: "cpa", Service: "cli-proxy-api", Container: "cli-proxy-api", Path: "/app/data", FileName: "cpa-cli-proxy-api.tar", Size: 128},
				},
				ReadOnly: true,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(agent.Close)

	service := New(Options{AgentURL: agent.URL, AgentToken: "agent-token"})
	result, err := service.Backup(context.Background())
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if !backupRequested {
		t.Fatalf("backup endpoint was not requested")
	}
	if result.BackupID != "cpa-20260610T010203Z" || len(result.Archives) != 1 {
		t.Fatalf("result = %#v", result)
	}
	if !result.Agent.Configured || !result.Agent.Reachable || !result.Agent.ReadOnly {
		t.Fatalf("agent = %#v", result.Agent)
	}
	if result.Lifecycle == nil ||
		result.Lifecycle.Operation != "backup" ||
		result.Lifecycle.Status != lifecycleStatusCompleted ||
		result.Lifecycle.Active {
		t.Fatalf("lifecycle = %#v", result.Lifecycle)
	}
	info, err := service.Info(context.Background())
	if err != nil {
		t.Fatalf("info after backup: %v", err)
	}
	if info.Lifecycle.Operation != "backup" || info.Lifecycle.Status != lifecycleStatusCompleted || info.Lifecycle.Active {
		t.Fatalf("info lifecycle = %#v", info.Lifecycle)
	}
}

func TestBackupPersistsAuditEntry(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent/info":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsAgentInfo{
				Service:  "cpamp-agent",
				Version:  "test",
				Mode:     "agent",
				ReadOnly: false,
			})
		case "/backups/cpa":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsBackupResult{
				BackupID:   "cpa-20260610T010203Z",
				Status:     "completed",
				BackupRoot: "/opt/cpamp/backups",
				CreatedAt:  1781053323,
				Archives: []model.ContainerOpsBackupArchive{
					{Role: "cpa", Service: "cli-proxy-api", Container: "cli-proxy-api", Path: "/app/data", FileName: "cpa-cli-proxy-api.tar", Size: 128},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(agent.Close)

	db := testutil.NewStore(t, testutil.NewConfig(t))
	service := New(Options{AgentURL: agent.URL, AuditStore: db})
	if _, err := service.Backup(context.Background()); err != nil {
		t.Fatalf("backup: %v", err)
	}
	audits, err := service.Audits(context.Background(), 10)
	if err != nil {
		t.Fatalf("audits: %v", err)
	}
	if len(audits) != 1 {
		t.Fatalf("len(audits) = %d, want 1: %#v", len(audits), audits)
	}
	audit := audits[0]
	if audit.Operation != "backup" || audit.Status != lifecycleStatusCompleted || audit.BackupID != "cpa-20260610T010203Z" {
		t.Fatalf("audit = %#v", audit)
	}
	if audit.AgentBaseURL != agent.URL || audit.StartedAtMS <= 0 || audit.FinishedAtMS <= 0 {
		t.Fatalf("audit metadata = %#v", audit)
	}
	result, ok := audit.Result.(map[string]any)
	if !ok || result["backupId"] != "cpa-20260610T010203Z" {
		t.Fatalf("audit result = %#v", audit.Result)
	}
}

func TestConcurrentLifecycleWriteOperationReturnsBusy(t *testing.T) {
	backupStarted := make(chan struct{})
	releaseBackup := make(chan struct{})
	var closeStarted sync.Once

	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent/info":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsAgentInfo{
				Service:  "cpamp-agent",
				Version:  "test",
				Mode:     "agent",
				ReadOnly: false,
			})
		case "/backups/cpa":
			closeStarted.Do(func() { close(backupStarted) })
			<-releaseBackup
			_ = json.NewEncoder(w).Encode(model.ContainerOpsBackupResult{
				BackupID:   "cpa-20260610T010203Z",
				Status:     "completed",
				BackupRoot: "/opt/cpamp/backups",
				CreatedAt:  1781053323,
				Archives: []model.ContainerOpsBackupArchive{
					{Role: "cpa", Service: "cli-proxy-api", Container: "cli-proxy-api", Path: "/app/data", FileName: "cpa-cli-proxy-api.tar", Size: 128},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(agent.Close)

	service := New(Options{AgentURL: agent.URL})
	backupErr := make(chan error, 1)
	go func() {
		_, err := service.Backup(context.Background())
		backupErr <- err
	}()

	select {
	case <-backupStarted:
	case <-time.After(2 * time.Second):
		close(releaseBackup)
		t.Fatal("backup did not start")
	}

	info, err := service.Info(context.Background())
	if err != nil {
		close(releaseBackup)
		t.Fatalf("info during backup: %v", err)
	}
	if !info.Lifecycle.Active || info.Lifecycle.Operation != "backup" || info.Lifecycle.Status != lifecycleStatusInProgress {
		close(releaseBackup)
		t.Fatalf("active lifecycle = %#v", info.Lifecycle)
	}

	_, err = service.RestorePlan(context.Background(), model.ContainerOpsRestoreRequest{
		BackupID: "cpa-20260610T010203Z",
		Apply:    true,
	})
	if !IsLifecycleBusy(err) {
		close(releaseBackup)
		t.Fatalf("restore apply err = %v, want lifecycle busy", err)
	}

	close(releaseBackup)
	select {
	case err := <-backupErr:
		if err != nil {
			t.Fatalf("backup: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backup did not finish")
	}
}

func TestRestorePlanUsesAgentEndpointAndToken(t *testing.T) {
	var restoreRequested bool
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/agent/info":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsAgentInfo{
				Service:  "cpamp-agent",
				Version:  "test",
				Mode:     "agent",
				ReadOnly: true,
			})
		case "/restores/cpa/plan":
			restoreRequested = true
			if r.Method != http.MethodPost {
				t.Fatalf("restore method = %s", r.Method)
			}
			var request model.ContainerOpsRestoreRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if request.BackupID != "cpa-20260610T010203Z" {
				t.Fatalf("backup id = %q", request.BackupID)
			}
			_ = json.NewEncoder(w).Encode(model.ContainerOpsRestorePlan{
				BackupID:   request.BackupID,
				Status:     "ready",
				BackupRoot: "/opt/cpamp/backups",
				CreatedAt:  1781053323,
				Archives: []model.ContainerOpsBackupArchive{
					{Role: "cpa", Service: "cli-proxy-api", Container: "cli-proxy-api", Path: "/app/data", FileName: "cpa-cli-proxy-api.tar", Size: 128},
				},
				Checks: []model.ContainerOpsRestoreCheck{
					{Severity: "info", Code: "manifest_loaded", Message: "ok"},
				},
				Steps: []model.ContainerOpsRestoreStep{
					{Order: 1, Code: "create_rollback_backup", Title: "Create rollback backup"},
				},
				ReadOnly: true,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(agent.Close)

	service := New(Options{AgentURL: agent.URL, AgentToken: "agent-token"})
	plan, err := service.RestorePlan(context.Background(), model.ContainerOpsRestoreRequest{BackupID: " cpa-20260610T010203Z "})
	if err != nil {
		t.Fatalf("restore plan: %v", err)
	}
	if !restoreRequested {
		t.Fatalf("restore endpoint was not requested")
	}
	if plan.BackupID != "cpa-20260610T010203Z" || plan.Status != "ready" || len(plan.Steps) != 1 {
		t.Fatalf("plan = %#v", plan)
	}
	if !plan.Agent.Configured || !plan.Agent.Reachable || !plan.Agent.ReadOnly {
		t.Fatalf("agent = %#v", plan.Agent)
	}
}

func TestRestoreApplyUsesAgentEndpointAndToken(t *testing.T) {
	var applyRequested bool
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/agent/info":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsAgentInfo{
				Service:  "cpamp-agent",
				Version:  "test",
				Mode:     "agent",
				ReadOnly: false,
			})
		case "/restores/cpa/apply":
			applyRequested = true
			if r.Method != http.MethodPost {
				t.Fatalf("restore apply method = %s", r.Method)
			}
			var request model.ContainerOpsRestoreRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if request.BackupID != "cpa-20260610T010203Z" || !request.Apply {
				t.Fatalf("request = %#v", request)
			}
			_ = json.NewEncoder(w).Encode(model.ContainerOpsRestorePlan{
				BackupID:   request.BackupID,
				Status:     "restored",
				BackupRoot: "/opt/cpamp/backups",
				CreatedAt:  1781053323,
				Archives: []model.ContainerOpsBackupArchive{
					{Role: "cpa", Service: "cli-proxy-api", Container: "cli-proxy-api", Path: "/app/data", FileName: "cpa-cli-proxy-api.tar", Size: 128},
				},
				Checks: []model.ContainerOpsRestoreCheck{
					{Severity: "info", Code: "restore_completed", Message: "ok"},
				},
				Steps: []model.ContainerOpsRestoreStep{
					{Order: 1, Code: "create_rollback_backup", Title: "Create rollback backup"},
				},
				Actions: []model.ContainerOpsRestoreAction{
					{Order: 1, Code: "create_rollback_backup", Target: "cpa", Status: "applied"},
					{Order: 2, Code: "commit_restore", Target: "cpa", Status: "applied"},
				},
				RollbackBackup: &model.ContainerOpsBackupResult{
					BackupID: "rollback-cpa-20260610T010203Z",
					Status:   "completed",
				},
				Applied:     true,
				Destructive: true,
				ReadOnly:    false,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(agent.Close)

	service := New(Options{AgentURL: agent.URL, AgentToken: "agent-token"})
	plan, err := service.RestorePlan(context.Background(), model.ContainerOpsRestoreRequest{
		BackupID: " cpa-20260610T010203Z ",
		Apply:    true,
	})
	if err != nil {
		t.Fatalf("restore apply: %v", err)
	}
	if !applyRequested {
		t.Fatalf("restore apply endpoint was not requested")
	}
	if plan.BackupID != "cpa-20260610T010203Z" || plan.Status != "restored" || !plan.Applied || !plan.Destructive || plan.ReadOnly {
		t.Fatalf("plan = %#v", plan)
	}
	if plan.RollbackBackup == nil || plan.RollbackBackup.BackupID != "rollback-cpa-20260610T010203Z" {
		t.Fatalf("rollback = %#v", plan.RollbackBackup)
	}
	if !plan.Agent.Configured || !plan.Agent.Reachable || plan.Agent.ReadOnly {
		t.Fatalf("agent = %#v", plan.Agent)
	}
	if !hasRestoreAction(plan.Actions, "commit_restore", "applied") {
		t.Fatalf("actions = %#v", plan.Actions)
	}
}

func TestRollbackUsesAgentEndpointAndToken(t *testing.T) {
	var rollbackRequested bool
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/agent/info":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsAgentInfo{
				Service:  "cpamp-agent",
				Version:  "test",
				Mode:     "agent",
				ReadOnly: false,
			})
		case "/rollbacks/cpa/apply":
			rollbackRequested = true
			if r.Method != http.MethodPost {
				t.Fatalf("rollback method = %s", r.Method)
			}
			var request model.ContainerOpsRollbackRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if request.BackupID != "rollback-cpa-20260610T010203Z" {
				t.Fatalf("request = %#v", request)
			}
			_ = json.NewEncoder(w).Encode(model.ContainerOpsRestorePlan{
				BackupID:   request.BackupID,
				Status:     "rolled_back",
				BackupRoot: "/opt/cpamp/backups",
				CreatedAt:  1781053323,
				Archives: []model.ContainerOpsBackupArchive{
					{Role: "cpa", Service: "cli-proxy-api", Container: "cli-proxy-api", Path: "/app/data", FileName: "cpa-cli-proxy-api.tar", Size: 128},
				},
				Checks: []model.ContainerOpsRestoreCheck{
					{Severity: "info", Code: "rollback_completed", Message: "ok"},
				},
				Steps: []model.ContainerOpsRestoreStep{
					{Order: 1, Code: "create_rollback_backup", Title: "Create safety backup"},
				},
				Actions: []model.ContainerOpsRestoreAction{
					{Order: 1, Code: "create_rollback_backup", Target: "cpa", Status: "applied"},
					{Order: 2, Code: "commit_rollback", Target: "cpa", Status: "applied"},
				},
				RollbackBackup: &model.ContainerOpsBackupResult{
					BackupID: "pre-rollback-cpa-20260610T010204Z",
					Status:   "completed",
				},
				Applied:     true,
				Destructive: true,
				ReadOnly:    false,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(agent.Close)

	service := New(Options{AgentURL: agent.URL, AgentToken: "agent-token"})
	plan, err := service.Rollback(context.Background(), model.ContainerOpsRollbackRequest{
		BackupID: " rollback-cpa-20260610T010203Z ",
	})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !rollbackRequested {
		t.Fatalf("rollback endpoint was not requested")
	}
	if plan.BackupID != "rollback-cpa-20260610T010203Z" || plan.Status != "rolled_back" || !plan.Applied || !plan.Destructive || plan.ReadOnly {
		t.Fatalf("plan = %#v", plan)
	}
	if plan.RollbackBackup == nil || plan.RollbackBackup.BackupID != "pre-rollback-cpa-20260610T010204Z" {
		t.Fatalf("safety backup = %#v", plan.RollbackBackup)
	}
	if !plan.Agent.Configured || !plan.Agent.Reachable || plan.Agent.ReadOnly {
		t.Fatalf("agent = %#v", plan.Agent)
	}
	if !hasRestoreAction(plan.Actions, "commit_rollback", "applied") {
		t.Fatalf("actions = %#v", plan.Actions)
	}
}

func TestStandardizeNetworkUsesAgentEndpointAndToken(t *testing.T) {
	var networkRequested bool
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/agent/info":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsAgentInfo{
				Service:  "cpamp-agent",
				Version:  "test",
				Mode:     "agent",
				ReadOnly: false,
			})
		case "/networks/cpa/standardize":
			networkRequested = true
			if r.Method != http.MethodPost {
				t.Fatalf("network method = %s", r.Method)
			}
			var request model.ContainerOpsNetworkStandardizeRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if request.BackupID != "cpa-20260610T010203Z" || !request.Apply {
				t.Fatalf("request = %#v", request)
			}
			_ = json.NewEncoder(w).Encode(model.ContainerOpsNetworkStandardizeResult{
				BackupID:    request.BackupID,
				Status:      "applied",
				Network:     "cpamp-cpa_default",
				Applied:     true,
				Destructive: false,
				Checks: []model.ContainerOpsNetworkCheck{
					{Severity: "info", Code: "backup_ready", Message: "ok"},
				},
				Actions: []model.ContainerOpsNetworkAction{
					{Order: 1, Code: "create_standard_network", Target: "cpamp-cpa_default", Status: "applied"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(agent.Close)

	service := New(Options{AgentURL: agent.URL, AgentToken: "agent-token"})
	result, err := service.StandardizeNetwork(context.Background(), model.ContainerOpsNetworkStandardizeRequest{
		BackupID: " cpa-20260610T010203Z ",
		Apply:    true,
	})
	if err != nil {
		t.Fatalf("standardize network: %v", err)
	}
	if !networkRequested {
		t.Fatalf("network endpoint was not requested")
	}
	if result.BackupID != "cpa-20260610T010203Z" || result.Status != "applied" || !result.Applied {
		t.Fatalf("result = %#v", result)
	}
	if !result.Agent.Configured || !result.Agent.Reachable || result.Agent.ReadOnly {
		t.Fatalf("agent = %#v", result.Agent)
	}
}

func TestUpgradePlanUsesAgentEndpointAndToken(t *testing.T) {
	var planRequested bool
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/agent/info":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsAgentInfo{
				Service:  "cpamp-agent",
				Version:  "test",
				Mode:     "agent",
				ReadOnly: false,
			})
		case "/upgrades/cpa/plan":
			planRequested = true
			if r.Method != http.MethodPost {
				t.Fatalf("upgrade plan method = %s", r.Method)
			}
			var request model.ContainerOpsUpgradeRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if request.Apply || request.CPAImage != "seakee/cli-proxy-api:v2" || request.CPAMPImage != "" {
				t.Fatalf("request = %#v", request)
			}
			_ = json.NewEncoder(w).Encode(model.ContainerOpsUpgradePlan{
				Status:     "ready",
				CPAImage:   request.CPAImage,
				CPAMPImage: "seakee/cpa-manager-plus:latest",
				Checks: []model.ContainerOpsUpgradeCheck{
					{Severity: "info", Code: "upgrade_target_ready", Message: "ok"},
				},
				Steps: []model.ContainerOpsUpgradeStep{
					{Order: 1, Code: "precheck", Title: "Precheck"},
				},
				ReadOnly:    true,
				Destructive: true,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(agent.Close)

	service := New(Options{AgentURL: agent.URL, AgentToken: "agent-token"})
	plan, err := service.Upgrade(context.Background(), model.ContainerOpsUpgradeRequest{
		CPAImage: " seakee/cli-proxy-api:v2 ",
	})
	if err != nil {
		t.Fatalf("upgrade plan: %v", err)
	}
	if !planRequested {
		t.Fatalf("upgrade plan endpoint was not requested")
	}
	if plan.Status != "ready" || !plan.ReadOnly || !plan.Destructive || plan.CPAImage != "seakee/cli-proxy-api:v2" {
		t.Fatalf("plan = %#v", plan)
	}
	if plan.Lifecycle != nil {
		t.Fatalf("plan lifecycle should stay nil: %#v", plan.Lifecycle)
	}
	if !plan.Agent.Configured || !plan.Agent.Reachable || plan.Agent.ReadOnly {
		t.Fatalf("agent = %#v", plan.Agent)
	}
}

func TestUpgradePrepareUsesLifecycleAndAudit(t *testing.T) {
	var prepareRequested bool
	var upgradeJobStarted bool
	var upgradeJobPolled bool
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent/info":
			_ = json.NewEncoder(w).Encode(model.ContainerOpsAgentInfo{
				Service:  "cpamp-agent",
				Version:  "test",
				Mode:     "agent",
				ReadOnly: false,
			})
		case "/upgrades/cpa/prepare":
			prepareRequested = true
			if r.Method != http.MethodPost {
				t.Fatalf("upgrade prepare method = %s", r.Method)
			}
			var request model.ContainerOpsUpgradeRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if !request.Apply || request.CPAImage != "seakee/cli-proxy-api:v2" || request.CPAMPImage != "seakee/cpa-manager-plus:v2" {
				t.Fatalf("request = %#v", request)
			}
			_ = json.NewEncoder(w).Encode(model.ContainerOpsUpgradePlan{
				Status:     "prepared",
				CPAImage:   request.CPAImage,
				CPAMPImage: request.CPAMPImage,
				Checks: []model.ContainerOpsUpgradeCheck{
					{Severity: "info", Code: "upgrade_target_ready", Message: "ok"},
				},
				Steps: []model.ContainerOpsUpgradeStep{
					{Order: 1, Code: "precheck", Title: "Precheck"},
				},
				Actions: []model.ContainerOpsUpgradeAction{
					{Order: 1, Code: "create_upgrade_backup", Target: "cpa", Status: "applied"},
					{Order: 2, Code: "pull_upgrade_images", Target: "images", Status: "applied"},
					{Order: 3, Code: "prepare_recreate", Target: "cpa", Status: "skipped"},
				},
				ImagePulls: []model.ContainerOpsImagePull{
					{Image: request.CPAImage, Status: "pulled"},
					{Image: request.CPAMPImage, Status: "pulled"},
				},
				RollbackBackup: &model.ContainerOpsBackupResult{
					BackupID: "upgrade-cpa-20260610T010203Z",
					Status:   "completed",
				},
				Applied:     true,
				Destructive: true,
				ReadOnly:    false,
			})
		case "/upgrades/cpa/jobs":
			if r.Method != http.MethodPost {
				t.Fatalf("upgrade job method = %s", r.Method)
			}
			var request model.ContainerOpsUpgradeJobStartRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode job request: %v", err)
			}
			if request.TaskID == "" ||
				request.CPAImage != "seakee/cli-proxy-api:v2" ||
				request.CPAMPImage != "seakee/cpa-manager-plus:v2" ||
				request.RollbackBackupID != "upgrade-cpa-20260610T010203Z" {
				t.Fatalf("job request = %#v", request)
			}
			upgradeJobStarted = true
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(model.ContainerOpsUpgradeJob{
				JobID:            "agent-job-1",
				TaskID:           request.TaskID,
				Status:           "queued",
				Phase:            "queued",
				CPAImage:         request.CPAImage,
				CPAMPImage:       request.CPAMPImage,
				RollbackBackupID: request.RollbackBackupID,
				NextAction:       "wait_for_agent_job",
			})
		case "/upgrades/cpa/jobs/agent-job-1":
			if r.Method != http.MethodGet {
				t.Fatalf("upgrade job poll method = %s", r.Method)
			}
			upgradeJobPolled = true
			plan := model.ContainerOpsUpgradePlan{
				Status:     "recreate_deferred",
				CPAImage:   "seakee/cli-proxy-api:v2",
				CPAMPImage: "seakee/cpa-manager-plus:v2",
				Checks: []model.ContainerOpsUpgradeCheck{
					{Severity: "info", Code: "upgrade_target_ready", Message: "ok"},
				},
				Actions: []model.ContainerOpsUpgradeAction{
					{Order: 1, Code: "recreate_cpa_container", Target: "cli-proxy-api", Status: "skipped"},
					{Order: 2, Code: "recreate_cpamp_container", Target: "cpa-manager-plus", Status: "skipped"},
					{Order: 3, Code: "healthcheck_after_recreate", Target: "cpamp-cpa", Status: "skipped"},
				},
				Applied:     false,
				Destructive: true,
				ReadOnly:    false,
			}
			_ = json.NewEncoder(w).Encode(model.ContainerOpsUpgradeJob{
				JobID:            "agent-job-1",
				TaskID:           "upgrade-cpa-prepare",
				Status:           "recreate_deferred",
				Phase:            "async_recreate_deferred",
				CPAImage:         plan.CPAImage,
				CPAMPImage:       plan.CPAMPImage,
				RollbackBackupID: "upgrade-cpa-20260610T010203Z",
				NextAction:       "implement_agent_recreate",
				Checks:           plan.Checks,
				Actions:          plan.Actions,
				Plan:             &plan,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(agent.Close)

	db := testutil.NewStore(t, testutil.NewConfig(t))
	service := New(Options{AgentURL: agent.URL, AuditStore: db})
	plan, err := service.Upgrade(context.Background(), model.ContainerOpsUpgradeRequest{
		CPAImage:   "seakee/cli-proxy-api:v2",
		CPAMPImage: "seakee/cpa-manager-plus:v2",
		Apply:      true,
	})
	if err != nil {
		t.Fatalf("upgrade prepare: %v", err)
	}
	if !prepareRequested {
		t.Fatalf("upgrade prepare endpoint was not requested")
	}
	if plan.Status != "prepared" || !plan.Applied || plan.ReadOnly || len(plan.ImagePulls) != 2 {
		t.Fatalf("plan = %#v", plan)
	}
	if plan.RollbackBackup == nil || plan.RollbackBackup.BackupID != "upgrade-cpa-20260610T010203Z" {
		t.Fatalf("rollback backup = %#v", plan.RollbackBackup)
	}
	if plan.Lifecycle == nil ||
		plan.Lifecycle.Operation != "upgrade_prepare" ||
		plan.Lifecycle.Status != lifecycleStatusCompleted ||
		plan.Lifecycle.Active {
		t.Fatalf("lifecycle = %#v", plan.Lifecycle)
	}
	if plan.Task == nil ||
		plan.Task.TaskID != plan.Lifecycle.OperationID ||
		plan.Task.OperationID != plan.Lifecycle.OperationID ||
		plan.Task.Status != "prepared" ||
		plan.Task.Phase != "prepare_completed" ||
		plan.Task.RollbackBackupID != "upgrade-cpa-20260610T010203Z" ||
		plan.Task.NextAction != "start_async_recreate" ||
		plan.Task.FinishedAtMS <= 0 {
		t.Fatalf("upgrade task = %#v lifecycle = %#v", plan.Task, plan.Lifecycle)
	}
	if !hasUpgradeAction(plan.Actions, "prepare_recreate", "skipped") {
		t.Fatalf("actions = %#v", plan.Actions)
	}

	audits, err := service.Audits(context.Background(), 10)
	if err != nil {
		t.Fatalf("audits: %v", err)
	}
	if len(audits) != 1 {
		t.Fatalf("len(audits) = %d, want 1: %#v", len(audits), audits)
	}
	audit := audits[0]
	if audit.Operation != "upgrade_prepare" || audit.Status != lifecycleStatusCompleted || audit.BackupID != "upgrade-cpa-20260610T010203Z" {
		t.Fatalf("audit = %#v", audit)
	}
	result, ok := audit.Result.(map[string]any)
	if !ok || result["rollbackBackupId"] != "upgrade-cpa-20260610T010203Z" || result["imagePullCount"].(float64) != 2 {
		t.Fatalf("audit result = %#v", audit.Result)
	}

	tasks, err := service.UpgradeTasks(context.Background(), 10)
	if err != nil {
		t.Fatalf("upgrade tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1: %#v", len(tasks), tasks)
	}
	task := tasks[0]
	if task.TaskID != plan.Task.TaskID ||
		task.Status != "prepared" ||
		task.Phase != "prepare_completed" ||
		task.CPAImage != "seakee/cli-proxy-api:v2" ||
		task.CPAMPImage != "seakee/cpa-manager-plus:v2" ||
		task.RollbackBackupID != "upgrade-cpa-20260610T010203Z" ||
		task.NextAction != "start_async_recreate" {
		t.Fatalf("persisted task = %#v", task)
	}
	taskResult, ok := task.Result.(map[string]any)
	if !ok || taskResult["rollbackBackupId"] != "upgrade-cpa-20260610T010203Z" || taskResult["imagePullCount"].(float64) != 2 {
		t.Fatalf("task result = %#v", task.Result)
	}

	started, err := service.StartUpgradeTask(context.Background(), model.ContainerOpsUpgradeTaskStartRequest{TaskID: task.TaskID})
	if err != nil {
		t.Fatalf("start upgrade task: %v", err)
	}
	if started.Status != "running" || started.Phase != "async_recreate" || started.NextAction != "wait_for_async_result" {
		t.Fatalf("started task = %#v", started)
	}
	if !upgradeJobStarted {
		t.Fatalf("agent upgrade job was not started")
	}
	var deferred model.ContainerOpsUpgradeTask
	for attempt := 0; attempt < 20; attempt++ {
		tasks, err := service.UpgradeTasks(context.Background(), 10)
		if err != nil {
			t.Fatalf("upgrade tasks after start: %v", err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len(tasks after start) = %d, want 1: %#v", len(tasks), tasks)
		}
		deferred = tasks[0]
		if deferred.Status == "recreate_deferred" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if deferred.Status != "recreate_deferred" ||
		deferred.Phase != "async_recreate_deferred" ||
		deferred.NextAction != "implement_agent_recreate" ||
		deferred.FinishedAtMS <= 0 {
		t.Fatalf("deferred task = %#v", deferred)
	}
	if !upgradeJobPolled {
		t.Fatalf("agent upgrade job was not polled")
	}
	deferredResult, ok := deferred.Result.(map[string]any)
	if !ok ||
		deferredResult["agentJobId"] != "agent-job-1" ||
		deferredResult["rollbackBackupId"] != "upgrade-cpa-20260610T010203Z" {
		t.Fatalf("deferred result = %#v", deferred.Result)
	}
}

func hasImportRisk(risks []model.ContainerOpsImportRisk, code string) bool {
	for _, risk := range risks {
		if risk.Code == code {
			return true
		}
	}
	return false
}

func hasImportAction(actions []string, action string) bool {
	for _, item := range actions {
		if item == action {
			return true
		}
	}
	return false
}

func hasDeployCheck(checks []model.ContainerOpsDeployCheck, code string) bool {
	for _, check := range checks {
		if check.Code == code {
			return true
		}
	}
	return false
}

func hasDeployAction(actions []model.ContainerOpsDeployAction, code string, status string) bool {
	for _, action := range actions {
		if action.Code == code && action.Status == status {
			return true
		}
	}
	return false
}

func hasRestoreAction(actions []model.ContainerOpsRestoreAction, code string, status string) bool {
	for _, action := range actions {
		if action.Code == code && action.Status == status {
			return true
		}
	}
	return false
}

func hasUpgradeAction(actions []model.ContainerOpsUpgradeAction, code string, status string) bool {
	for _, action := range actions {
		if action.Code == code && action.Status == status {
			return true
		}
	}
	return false
}
