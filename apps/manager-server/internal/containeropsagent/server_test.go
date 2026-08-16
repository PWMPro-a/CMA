package containeropsagent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

func TestServerRequiresBearerTokenForAgentInfo(t *testing.T) {
	serverApp, err := NewServer(ServerOptions{
		ServiceID:  "cpamp-agent",
		Version:    "test",
		DockerHost: "unix:///tmp/cpamp-test-docker.sock",
		Token:      "agent-secret",
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	handler := serverApp.Handler()
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/agent/info", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/agent/info", nil)
	req.Header.Set("Authorization", "Bearer agent-secret")
	authorized := httptest.NewRecorder()
	handler.ServeHTTP(authorized, req)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d body = %s", authorized.Code, authorized.Body.String())
	}
	var info model.ContainerOpsAgentInfo
	if err := json.Unmarshal(authorized.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode info: %v", err)
	}
	if !info.Configured || !info.Reachable || info.ReadOnly {
		t.Fatalf("info = %#v", info)
	}
	if info.Service != "cpamp-agent" || info.Version != "test" || info.Mode != "agent" {
		t.Fatalf("info = %#v", info)
	}

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d", health.Code)
	}
	var healthInfo struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(health.Body.Bytes(), &healthInfo); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if healthInfo.Version != "test" {
		t.Fatalf("health version = %q", healthInfo.Version)
	}
}

func TestUpgradeJobRoutesRecreateCPAOnlyAndPersistJob(t *testing.T) {
	backupRoot := t.TempDir()
	backupID := "backup-1"
	writeNetworkBackupFixture(t, backupRoot, backupID)
	var startedNew bool
	var cpaWrites []string
	serverApp := &Server{
		serviceID:      "cpamp-agent",
		version:        "test",
		backupRoot:     backupRoot,
		upgradeJobRoot: filepath.Join(backupRoot, "upgrade-jobs"),
		docker: &DockerClient{
			host: "unix:///fake/docker.sock",
			client: &http.Client{Transport: backupRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/containers/json":
					containers := []dockerContainer{
						{ID: "cpamp-full-id", Names: []string{"/cpa-manager-plus"}, Image: "seakee/cpa-manager-plus:old", State: "running", Labels: map[string]string{"com.cpamp.managed": "true", "com.cpamp.role": "cpamp"}},
					}
					if startedNew {
						containers = append(containers, dockerContainer{
							ID:     "new-cpa-full-id",
							Names:  []string{"/cli-proxy-api"},
							Image:  "seakee/cli-proxy-api:v2",
							State:  "running",
							Labels: map[string]string{"com.cpamp.managed": "true", "com.cpamp.role": "cpa"},
							Mounts: []dockerMount{{Type: "volume", Name: "cpamp-cpa_cpa-data", Destination: "/app/data", RW: true}},
							Ports:  []dockerPort{{PrivatePort: 8317, PublicPort: 8317, Type: "tcp"}},
						})
					} else {
						containers = append(containers, dockerContainer{
							ID:     "old-cpa-full-id",
							Names:  []string{"/cli-proxy-api"},
							Image:  "seakee/cli-proxy-api:old",
							State:  "running",
							Labels: map[string]string{"com.cpamp.managed": "true", "com.cpamp.role": "cpa"},
							Mounts: []dockerMount{{Type: "volume", Name: "cpamp-cpa_cpa-data", Destination: "/app/data", RW: true}},
							Ports:  []dockerPort{{PrivatePort: 8317, PublicPort: 8317, Type: "tcp"}},
						})
					}
					return backupJSONResponse(http.StatusOK, containers)
				case "/networks":
					return backupJSONResponse(http.StatusOK, []dockerNetwork{{Name: standardCPANetworkName, Driver: "bridge", Labels: map[string]string{"com.cpamp.managed": "true"}}})
				case "/images/json":
					return backupJSONResponse(http.StatusOK, []dockerImage{})
				case "/containers/cli-proxy-api/stop":
					cpaWrites = append(cpaWrites, "stop")
					return backupJSONResponse(http.StatusNoContent, map[string]any{})
				case "/containers/cli-proxy-api/rename":
					cpaWrites = append(cpaWrites, "rename:"+req.URL.Query().Get("name"))
					return backupJSONResponse(http.StatusNoContent, map[string]any{})
				case "/containers/create":
					if req.URL.Query().Get("name") != "cli-proxy-api" {
						t.Fatalf("unexpected create path %s", req.URL.String())
					}
					cpaWrites = append(cpaWrites, "create")
					return backupJSONResponse(http.StatusCreated, map[string]string{"Id": "new-cpa-full-id"})
				case "/containers/cli-proxy-api/start":
					cpaWrites = append(cpaWrites, "start")
					startedNew = true
					return backupJSONResponse(http.StatusNoContent, map[string]any{})
				default:
					if strings.Contains(req.URL.Path, "cpa-manager-plus") || strings.Contains(req.URL.Path, "cpamp-agent") {
						t.Fatalf("upgrade job must not write CPAMP/Agent path %s %s", req.Method, req.URL.String())
					}
					t.Fatalf("unexpected docker write path %s %s", req.Method, req.URL.String())
				}
				return nil, nil
			})},
		},
		upgradeJobs: make(map[string]model.ContainerOpsUpgradeJob),
	}

	handler := serverApp.Handler()
	start := httptest.NewRecorder()
	handler.ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/upgrades/cpa/jobs", strings.NewReader(`{"taskId":"task-1","cpaImage":"seakee/cli-proxy-api:v2","cpampImage":"seakee/cpa-manager-plus:v2","rollbackBackupId":"backup-1"}`)))
	if start.Code != http.StatusAccepted {
		t.Fatalf("start status = %d body = %s", start.Code, start.Body.String())
	}
	var started model.ContainerOpsUpgradeJob
	if err := json.Unmarshal(start.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode started job: %v", err)
	}
	if started.JobID == "" || started.Status != "queued" || started.NextAction != "wait_for_agent_job" {
		t.Fatalf("started job = %#v", started)
	}

	var job model.ContainerOpsUpgradeJob
	for attempt := 0; attempt < 20; attempt++ {
		get := httptest.NewRecorder()
		handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/upgrades/cpa/jobs/"+started.JobID, nil))
		if get.Code != http.StatusOK {
			t.Fatalf("get status = %d body = %s", get.Code, get.Body.String())
		}
		if err := json.Unmarshal(get.Body.Bytes(), &job); err != nil {
			t.Fatalf("decode job: %v", err)
		}
		if job.Status == "completed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.Status != "completed" ||
		job.Phase != "healthcheck_completed" ||
		job.NextAction != "review_upgrade_result" ||
		job.RollbackBackupID != "backup-1" ||
		job.Plan == nil {
		t.Fatalf("job = %#v", job)
	}
	if len(cpaWrites) != 4 || cpaWrites[0] != "stop" || !strings.HasPrefix(cpaWrites[1], "rename:cli-proxy-api-upgrade-old-") || cpaWrites[2] != "create" || cpaWrites[3] != "start" {
		t.Fatalf("cpa writes = %#v", cpaWrites)
	}
	if !hasUpgradeAction(job.Actions, "recreate_cpa_container", "applied") ||
		!hasUpgradeAction(job.Actions, "recreate_cpamp_container", "skipped") {
		t.Fatalf("actions = %#v", job.Actions)
	}
	if _, err := os.Stat(serverApp.upgradeJobPath(started.JobID)); err != nil {
		t.Fatalf("upgrade job was not persisted: %v", err)
	}

	recovered := &Server{
		serviceID:      "cpamp-agent",
		version:        "test",
		backupRoot:     backupRoot,
		upgradeJobRoot: serverApp.upgradeJobRoot,
		docker:         serverApp.docker,
		upgradeJobs:    make(map[string]model.ContainerOpsUpgradeJob),
	}
	if err := recovered.loadUpgradeJobs(); err != nil {
		t.Fatalf("load upgrade jobs: %v", err)
	}
	loaded, ok := recovered.getUpgradeJob(started.JobID)
	if !ok || loaded.Status != "completed" || loaded.Plan == nil {
		t.Fatalf("loaded job ok=%v job=%#v", ok, loaded)
	}
}

func TestUpgradeJobRecoveryFailsNonTerminalJobs(t *testing.T) {
	backupRoot := t.TempDir()
	serverApp := &Server{
		serviceID:      "cpamp-agent",
		version:        "test",
		backupRoot:     backupRoot,
		upgradeJobRoot: filepath.Join(backupRoot, "upgrade-jobs"),
		docker:         &DockerClient{host: "unix:///fake/docker.sock"},
		upgradeJobs:    make(map[string]model.ContainerOpsUpgradeJob),
	}
	job := model.ContainerOpsUpgradeJob{
		JobID:       "upgrade-cpa-job-1781053323000-9",
		TaskID:      "task-running",
		Status:      "running",
		Phase:       "async_recreate",
		CPAImage:    "seakee/cli-proxy-api:v2",
		CPAMPImage:  "seakee/cpa-manager-plus:v2",
		NextAction:  "wait_for_async_result",
		StartedAtMS: 1781053323000,
		CreatedAtMS: 1781053323000,
		UpdatedAtMS: 1781053323000,
	}
	if err := serverApp.saveUpgradeJobLocked(job); err != nil {
		t.Fatalf("save upgrade job: %v", err)
	}

	recovered := &Server{
		serviceID:      "cpamp-agent",
		version:        "test",
		backupRoot:     backupRoot,
		upgradeJobRoot: serverApp.upgradeJobRoot,
		docker:         serverApp.docker,
		upgradeJobs:    make(map[string]model.ContainerOpsUpgradeJob),
	}
	if err := recovered.loadUpgradeJobs(); err != nil {
		t.Fatalf("load upgrade jobs: %v", err)
	}
	loaded, ok := recovered.getUpgradeJob(job.JobID)
	if !ok {
		t.Fatalf("recovered job not found")
	}
	if loaded.Status != "failed" ||
		loaded.Phase != "async_recreate_failed" ||
		loaded.NextAction != "restart_upgrade_task" ||
		loaded.Error == "" ||
		loaded.FinishedAtMS <= 0 {
		t.Fatalf("loaded job = %#v", loaded)
	}
}

func TestRenderCPADeployFilesWritesOnlyStandardStackFiles(t *testing.T) {
	stackRoot := t.TempDir()
	request := standardDeployRenderRequest()

	files, err := RenderCPADeployFiles(t.Context(), stackRoot, request)
	if err != nil {
		t.Fatalf("render files: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("files = %#v", files)
	}
	for _, name := range []string{"compose.yml", "stack.manifest.json", ".env.example"} {
		if _, err := os.Stat(filepath.Join(stackRoot, name)); err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
	}
	composeData, err := os.ReadFile(filepath.Join(stackRoot, "compose.yml"))
	if err != nil {
		t.Fatalf("read compose: %v", err)
	}
	if !strings.Contains(string(composeData), "name: cpamp-cpa") {
		t.Fatalf("compose = %s", composeData)
	}
	envData, err := os.ReadFile(filepath.Join(stackRoot, ".env.example"))
	if err != nil {
		t.Fatalf("read env example: %v", err)
	}
	if !strings.Contains(string(envData), "CPAMP_AGENT_TOKEN") {
		t.Fatalf("env example = %s", envData)
	}
}

func TestRenderCPADeployFilesRejectsNonStandardCompose(t *testing.T) {
	_, err := RenderCPADeployFiles(t.Context(), t.TempDir(), model.ContainerOpsDeployRenderRequest{
		Manifest: model.ContainerOpsStackManifest{
			ComposeProject: "other",
			Network:        "bridge",
		},
		Compose: model.ContainerOpsComposeDraft{
			ProjectName: "other",
			NetworkName: "bridge",
			Content:     "name: other\n",
		},
	})
	if err == nil {
		t.Fatalf("expected non-standard compose error")
	}
}

func standardDeployRenderRequest() model.ContainerOpsDeployRenderRequest {
	return model.ContainerOpsDeployRenderRequest{
		Manifest: model.ContainerOpsStackManifest{
			Stack:          "cpa",
			ComposeProject: "cpamp-cpa",
			Network:        "cpamp-cpa_default",
			StackRoot:      "/opt/cpamp/stacks/cpa",
			BackupRoot:     "/opt/cpamp/backups",
			NewAPIBaseURL:  "http://host.docker.internal:8317/v1",
			Services: []model.ContainerOpsManifestService{
				{Role: "cpa", Service: "cli-proxy-api", Image: "seakee/cli-proxy-api:latest", Managed: true, IncludeInCompose: true},
				{Role: "cpamp", Service: "cpa-manager-plus", Image: "seakee/cpa-manager-plus:latest", Managed: true, IncludeInCompose: true},
				{Role: "agent", Service: "cpamp-agent", Image: "seakee/cpa-manager-plus:latest", Managed: true, IncludeInCompose: true},
			},
		},
		Compose: model.ContainerOpsComposeDraft{
			FileName:    "compose.deploy-preview.yml",
			ProjectName: "cpamp-cpa",
			NetworkName: "cpamp-cpa_default",
			Services:    []string{"cli-proxy-api", "cpa-manager-plus", "cpamp-agent"},
			Content: strings.Join([]string{
				"name: cpamp-cpa",
				"services:",
				"  cli-proxy-api:",
				"    image: seakee/cli-proxy-api:latest",
				"  cpa-manager-plus:",
				"    image: seakee/cpa-manager-plus:latest",
				"  cpamp-agent:",
				"    entrypoint: [\"cpamp-agent\"]",
				"networks:",
				"  cpamp-cpa_default:",
				"    driver: bridge",
				"",
			}, "\n"),
		},
	}
}
