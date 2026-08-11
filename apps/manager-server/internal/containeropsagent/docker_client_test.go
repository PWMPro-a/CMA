package containeropsagent

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

func TestBuildOverviewDetectsCPAStackResources(t *testing.T) {
	overview := buildOverview(
		[]dockerContainer{
			{
				ID:     "1234567890abcdef",
				Names:  []string{"/new-api"},
				Image:  "calciumion/new-api:latest",
				State:  "running",
				Status: "Up 1 minute",
				Labels: map[string]string{},
				NetworkSettings: dockerContainerNetworkSettings{
					Networks: map[string]dockerEndpoint{
						"cpamp-cpa_default": {NetworkID: "network-newapi", IPAddress: "172.20.0.3", Gateway: "172.20.0.1"},
					},
				},
			},
			{
				ID:     "abcdef1234567890",
				Names:  []string{"/cli-proxy-api"},
				Image:  "seakee/cli-proxy-api:latest",
				State:  "running",
				Status: "Up 2 minutes",
				Labels: map[string]string{"com.cpamp.managed": "true"},
				Ports: []dockerPort{
					{PrivatePort: 8317, PublicPort: 8317, Type: "tcp", IP: "0.0.0.0"},
				},
				Mounts: []dockerMount{
					{Type: "volume", Name: "cpa-data", Destination: "/app/data", RW: true},
				},
				NetworkSettings: dockerContainerNetworkSettings{
					Networks: map[string]dockerEndpoint{
						"cpamp-cpa_default": {NetworkID: "network-cpa", IPAddress: "172.20.0.2", Gateway: "172.20.0.1"},
					},
				},
			},
			{
				ID:     "fedcba9876543210",
				Names:  []string{"/cpa-manager-plus"},
				Image:  "seakee/cpa-manager-plus:latest",
				State:  "exited",
				Status: "Exited",
				Labels: map[string]string{"com.cpamp.role": "cpamp"},
			},
		},
		[]dockerNetwork{
			{
				ID:         "network-cpamp-cpa",
				Name:       "cpamp-cpa_default",
				Driver:     "bridge",
				Scope:      "local",
				Attachable: true,
				Labels:     map[string]string{"com.cpamp.managed": "true"},
				Containers: map[string]dockerEndpoint{"a": {}, "b": {}},
			},
		},
		[]dockerImage{
			{ID: "sha256:image-cpa", RepoTags: []string{"seakee/cli-proxy-api:latest"}, Size: 1024, Created: 1770000000},
		},
	)

	if overview.Summary.ContainerCount != 3 || overview.Summary.RunningCount != 2 {
		t.Fatalf("summary = %#v", overview.Summary)
	}
	if overview.Summary.CPACount != 1 || overview.Summary.CPAMPCount != 1 || overview.Summary.NewAPICount != 1 {
		t.Fatalf("role summary = %#v", overview.Summary)
	}
	if overview.Summary.ManagedCount != 1 || overview.Summary.NetworkCount != 1 || overview.Summary.ImageCount != 1 {
		t.Fatalf("resource summary = %#v", overview.Summary)
	}
	if overview.Containers[0].Name != "cli-proxy-api" || overview.Containers[0].Role != "cpa" {
		t.Fatalf("first container = %#v", overview.Containers[0])
	}
	if overview.Containers[0].ID != "abcdef123456" || overview.Containers[0].ImageID != "" {
		t.Fatalf("short ids = %#v", overview.Containers[0])
	}
	if len(overview.Containers[0].Ports) != 1 || overview.Containers[0].Ports[0].PrivatePort != 8317 {
		t.Fatalf("ports = %#v", overview.Containers[0].Ports)
	}
	if len(overview.Containers[0].Mounts) != 1 || overview.Containers[0].Mounts[0].Destination != "/app/data" {
		t.Fatalf("mounts = %#v", overview.Containers[0].Mounts)
	}
	if len(overview.Containers[0].Networks) != 1 || overview.Containers[0].Networks[0].Name != "cpamp-cpa_default" {
		t.Fatalf("networks = %#v", overview.Containers[0].Networks)
	}
	if !overview.Networks[0].Managed || overview.Networks[0].Containers != 2 {
		t.Fatalf("network = %#v", overview.Networks[0])
	}
}

func TestBuildOverviewDetectsAgentBeforeSharedCPAMPImage(t *testing.T) {
	overview := buildOverview(
		[]dockerContainer{
			{
				ID:     "agent-full-id",
				Names:  []string{"/cpamp-agent"},
				Image:  "seakee/cpa-manager-plus:latest",
				State:  "running",
				Status: "Up 1 minute",
			},
		},
		[]dockerNetwork{},
		[]dockerImage{},
	)

	if len(overview.Containers) != 1 || overview.Containers[0].Role != "agent" {
		t.Fatalf("container role = %#v", overview.Containers)
	}
	if overview.Summary.CPAMPCount != 0 {
		t.Fatalf("agent should not be counted as CPAMP: %#v", overview.Summary)
	}
}

func TestBackupCPAStackArchivesCPAAndCPAMPData(t *testing.T) {
	client := &DockerClient{
		host: "unix:///fake/docker.sock",
		client: &http.Client{Transport: backupRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/containers/json":
				return backupJSONResponse(http.StatusOK, []dockerContainer{
					{
						ID:     "cpa-full-id",
						Names:  []string{"/cli-proxy-api"},
						Image:  "seakee/cli-proxy-api:latest",
						State:  "running",
						Labels: map[string]string{"com.cpamp.managed": "true"},
					},
					{
						ID:     "cpamp-full-id",
						Names:  []string{"/cpa-manager-plus"},
						Image:  "seakee/cpa-manager-plus:latest",
						State:  "running",
						Labels: map[string]string{"com.cpamp.role": "cpamp"},
					},
				})
			case "/networks":
				return backupJSONResponse(http.StatusOK, []dockerNetwork{})
			case "/images/json":
				return backupJSONResponse(http.StatusOK, []dockerImage{})
			case "/containers/cli-proxy-api/archive":
				if req.URL.Query().Get("path") != "/app/data" {
					t.Fatalf("cpa archive path = %q", req.URL.RawQuery)
				}
				return backupTextResponse(http.StatusOK, "cpa archive"), nil
			case "/containers/cpa-manager-plus/archive":
				if req.URL.Query().Get("path") != "/data" {
					t.Fatalf("cpamp archive path = %q", req.URL.RawQuery)
				}
				return backupTextResponse(http.StatusOK, "cpamp archive"), nil
			default:
				t.Fatalf("unexpected docker path %s", req.URL.String())
			}
			return nil, nil
		})},
	}

	now := time.Date(2026, 6, 10, 1, 2, 3, 0, time.UTC)
	result, err := client.BackupCPAStack(context.Background(), BackupOptions{
		BackupRoot: t.TempDir(),
		Now:        now,
	})
	if err != nil {
		t.Fatalf("backup cpa stack: %v", err)
	}
	if result.BackupID != "cpa-20260610T010203Z" || result.Status != "completed" {
		t.Fatalf("result = %#v", result)
	}
	if len(result.Archives) != 2 || len(result.Warnings) != 0 {
		t.Fatalf("archives=%#v warnings=%#v", result.Archives, result.Warnings)
	}

	backupDir := filepath.Join(result.BackupRoot, result.BackupID)
	cpaData, err := os.ReadFile(filepath.Join(backupDir, "cpa-cli-proxy-api.tar"))
	if err != nil {
		t.Fatalf("read cpa archive: %v", err)
	}
	if string(cpaData) != "cpa archive" {
		t.Fatalf("cpa archive = %q", cpaData)
	}
	cpampData, err := os.ReadFile(filepath.Join(backupDir, "cpamp-cpa-manager-plus.tar"))
	if err != nil {
		t.Fatalf("read cpamp archive: %v", err)
	}
	if string(cpampData) != "cpamp archive" {
		t.Fatalf("cpamp archive = %q", cpampData)
	}
	var manifest model.ContainerOpsBackupResult
	manifestData, err := os.ReadFile(filepath.Join(backupDir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if manifest.BackupID != result.BackupID || len(manifest.Archives) != 2 {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestBackupCPAStackRequiresCPAContainer(t *testing.T) {
	client := &DockerClient{
		host: "unix:///fake/docker.sock",
		client: &http.Client{Transport: backupRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/containers/json":
				return backupJSONResponse(http.StatusOK, []dockerContainer{
					{ID: "newapi-id", Names: []string{"/new-api"}, Image: "calciumion/new-api:latest", State: "running"},
				})
			case "/networks":
				return backupJSONResponse(http.StatusOK, []dockerNetwork{})
			case "/images/json":
				return backupJSONResponse(http.StatusOK, []dockerImage{})
			default:
				t.Fatalf("unexpected docker path %s", req.URL.String())
			}
			return nil, nil
		})},
	}

	if _, err := client.BackupCPAStack(context.Background(), BackupOptions{BackupRoot: t.TempDir()}); err == nil {
		t.Fatalf("expected missing CPA error")
	}
}

func TestRestoreCPAPlanValidatesManifestArchivesAndTargets(t *testing.T) {
	backupRoot := t.TempDir()
	backupID := "cpa-20260610T010203Z"
	backupDir := filepath.Join(backupRoot, backupID)
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		t.Fatalf("create backup dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "cpa-cli-proxy-api.tar"), []byte("cpa archive"), 0o640); err != nil {
		t.Fatalf("write cpa archive: %v", err)
	}
	cpampArchivePath := filepath.Join(backupDir, "cpamp-cpa-manager-plus.tar")
	if err := writeTestTar(cpampArchivePath, map[string]string{
		"data/usage.sqlite": "usage db",
		"data/data.key":     "data key",
	}); err != nil {
		t.Fatalf("write cpamp archive: %v", err)
	}
	cpampArchiveStat, err := os.Stat(cpampArchivePath)
	if err != nil {
		t.Fatalf("stat cpamp archive: %v", err)
	}
	manifest := model.ContainerOpsBackupResult{
		BackupID:   backupID,
		Status:     "completed",
		BackupRoot: backupRoot,
		CreatedAt:  1781053323,
		Archives: []model.ContainerOpsBackupArchive{
			{Role: "cpa", Service: "cli-proxy-api", Container: "cli-proxy-api", Path: "/app/data", FileName: "cpa-cli-proxy-api.tar", Size: int64(len("cpa archive"))},
			{Role: "cpamp", Service: "cpa-manager-plus", Container: "cpa-manager-plus", Path: "/data", FileName: "cpamp-cpa-manager-plus.tar", Size: cpampArchiveStat.Size()},
		},
		ReadOnly: true,
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "manifest.json"), manifestData, 0o640); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	client := &DockerClient{
		host: "unix:///fake/docker.sock",
		client: &http.Client{Transport: backupRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/containers/json":
				return backupJSONResponse(http.StatusOK, []dockerContainer{
					{ID: "cpa-full-id", Names: []string{"/cli-proxy-api"}, Image: "seakee/cli-proxy-api:latest", State: "running"},
					{ID: "cpamp-full-id", Names: []string{"/cpa-manager-plus"}, Image: "seakee/cpa-manager-plus:latest", State: "running", Labels: map[string]string{"com.cpamp.role": "cpamp"}},
				})
			case "/networks":
				return backupJSONResponse(http.StatusOK, []dockerNetwork{})
			case "/images/json":
				return backupJSONResponse(http.StatusOK, []dockerImage{})
			default:
				t.Fatalf("unexpected docker path %s", req.URL.String())
			}
			return nil, nil
		})},
	}

	plan, err := client.RestoreCPAPlan(context.Background(), RestorePlanOptions{
		BackupRoot: backupRoot,
		BackupID:   backupID,
	})
	if err != nil {
		t.Fatalf("restore plan: %v", err)
	}
	if plan.Status != "ready" || !plan.ReadOnly {
		t.Fatalf("plan = %#v", plan)
	}
	if plan.BackupID != backupID || len(plan.Archives) != 2 || len(plan.Steps) < 6 {
		t.Fatalf("plan = %#v", plan)
	}
	if !hasRestoreCheck(plan.Checks, "manifest_loaded") ||
		!hasRestoreCheck(plan.Checks, "archive_ready") ||
		!hasRestoreCheck(plan.Checks, "cpamp_required_files_ready") ||
		!hasRestoreCheck(plan.Checks, "cpa_target_ready") {
		t.Fatalf("checks = %#v", plan.Checks)
	}
}

func TestRestoreCPAPlanBlocksUnsafeOrIncompleteBackups(t *testing.T) {
	backupRoot := t.TempDir()
	if _, err := (&DockerClient{}).RestoreCPAPlan(context.Background(), RestorePlanOptions{
		BackupRoot: backupRoot,
		BackupID:   "../escape",
	}); err == nil {
		t.Fatalf("expected unsafe backup id error")
	}

	backupID := "cpa-20260610T010203Z"
	backupDir := filepath.Join(backupRoot, backupID)
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		t.Fatalf("create backup dir: %v", err)
	}
	manifest := model.ContainerOpsBackupResult{
		BackupID: backupID,
		Archives: []model.ContainerOpsBackupArchive{
			{Role: "cpa", Service: "cli-proxy-api", Container: "cli-proxy-api", Path: "/app/data", FileName: "missing.tar", Size: 128},
		},
		ReadOnly: true,
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "manifest.json"), manifestData, 0o640); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	client := &DockerClient{
		host: "unix:///fake/docker.sock",
		client: &http.Client{Transport: backupRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/containers/json":
				return backupJSONResponse(http.StatusOK, []dockerContainer{})
			case "/networks":
				return backupJSONResponse(http.StatusOK, []dockerNetwork{})
			case "/images/json":
				return backupJSONResponse(http.StatusOK, []dockerImage{})
			default:
				t.Fatalf("unexpected docker path %s", req.URL.String())
			}
			return nil, nil
		})},
	}

	plan, err := client.RestoreCPAPlan(context.Background(), RestorePlanOptions{
		BackupRoot: backupRoot,
		BackupID:   backupID,
	})
	if err != nil {
		t.Fatalf("restore plan: %v", err)
	}
	if plan.Status != "blocked" {
		t.Fatalf("status = %q checks=%#v", plan.Status, plan.Checks)
	}
	if !hasRestoreCheck(plan.Checks, "archive_missing") || !hasRestoreCheck(plan.Checks, "cpa_target_missing") {
		t.Fatalf("checks = %#v", plan.Checks)
	}
}

func TestRestoreCPAApplyCreatesRollbackAndRestoresStandardTargets(t *testing.T) {
	backupRoot := t.TempDir()
	backupID := "cpa-20260610T010203Z"
	writeRestoreBackupFixture(t, backupRoot, backupID)

	stopped := make([]string, 0)
	restored := make([]string, 0)
	started := make([]string, 0)
	client := &DockerClient{
		host: "unix:///fake/docker.sock",
		client: &http.Client{Transport: backupRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/containers/json":
				return backupJSONResponse(http.StatusOK, []dockerContainer{
					{ID: "cpa-full-id", Names: []string{"/cli-proxy-api"}, Image: "seakee/cli-proxy-api:latest", State: "running", Labels: map[string]string{"com.cpamp.managed": "true"}},
					{ID: "cpamp-full-id", Names: []string{"/cpa-manager-plus"}, Image: "seakee/cpa-manager-plus:latest", State: "running", Labels: map[string]string{"com.cpamp.role": "cpamp", "com.cpamp.managed": "true"}},
				})
			case "/networks":
				return backupJSONResponse(http.StatusOK, []dockerNetwork{{Name: standardCPANetworkName, Driver: "bridge", Labels: map[string]string{"com.cpamp.managed": "true"}}})
			case "/images/json":
				return backupJSONResponse(http.StatusOK, []dockerImage{})
			case "/containers/cli-proxy-api/archive":
				switch req.Method {
				case http.MethodGet:
					if req.URL.Query().Get("path") != "/app/data" {
						t.Fatalf("rollback cpa archive path = %q", req.URL.RawQuery)
					}
					return backupTextResponse(http.StatusOK, "rollback cpa archive"), nil
				case http.MethodPut:
					if req.URL.Query().Get("path") != "/app" {
						t.Fatalf("restore cpa archive path = %q", req.URL.RawQuery)
					}
					restored = append(restored, "cli-proxy-api")
					return backupJSONResponse(http.StatusOK, map[string]any{})
				}
			case "/containers/cpa-manager-plus/archive":
				switch req.Method {
				case http.MethodGet:
					if req.URL.Query().Get("path") != "/data" {
						t.Fatalf("rollback cpamp archive path = %q", req.URL.RawQuery)
					}
					return backupTextResponse(http.StatusOK, "rollback cpamp archive"), nil
				case http.MethodPut:
					if req.URL.Query().Get("path") != "/" {
						t.Fatalf("restore cpamp archive path = %q", req.URL.RawQuery)
					}
					restored = append(restored, "cpa-manager-plus")
					return backupJSONResponse(http.StatusOK, map[string]any{})
				}
			case "/containers/cpa-manager-plus/stop", "/containers/cli-proxy-api/stop":
				stopped = append(stopped, strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/containers/"), "/stop"))
				return backupJSONResponse(http.StatusNoContent, map[string]any{})
			case "/containers/cli-proxy-api/start", "/containers/cpa-manager-plus/start":
				started = append(started, strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/containers/"), "/start"))
				return backupJSONResponse(http.StatusNoContent, map[string]any{})
			default:
				t.Fatalf("unexpected docker path %s %s", req.Method, req.URL.String())
			}
			return nil, nil
		})},
	}

	result, err := client.RestoreCPA(context.Background(), RestoreApplyOptions{
		BackupRoot: backupRoot,
		BackupID:   backupID,
		Now:        time.Date(2026, 6, 10, 1, 2, 3, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("restore apply: %v", err)
	}
	if result.Status != "restored" || !result.Applied || !result.Destructive || result.ReadOnly {
		t.Fatalf("result = %#v", result)
	}
	if result.RollbackBackup == nil || result.RollbackBackup.BackupID != "rollback-cpa-20260610T010203Z" {
		t.Fatalf("rollback backup = %#v", result.RollbackBackup)
	}
	if strings.Join(stopped, ",") != "cpa-manager-plus,cli-proxy-api" {
		t.Fatalf("stopped = %#v", stopped)
	}
	if strings.Join(restored, ",") != "cli-proxy-api,cpa-manager-plus" {
		t.Fatalf("restored = %#v", restored)
	}
	if strings.Join(started, ",") != "cli-proxy-api,cpa-manager-plus" {
		t.Fatalf("started = %#v", started)
	}
	if !hasRestoreAction(result.Actions, "create_rollback_backup", "applied") ||
		!hasRestoreAction(result.Actions, "restore_cpa_archive", "applied") ||
		!hasRestoreAction(result.Actions, "restore_cpamp_archive", "applied") ||
		!hasRestoreAction(result.Actions, "commit_restore", "applied") ||
		!hasRestoreCheck(result.Checks, "restore_completed") {
		t.Fatalf("actions=%#v checks=%#v", result.Actions, result.Checks)
	}
	if _, err := os.Stat(filepath.Join(backupRoot, result.RollbackBackup.BackupID, "manifest.json")); err != nil {
		t.Fatalf("rollback manifest not written: %v", err)
	}
}

func TestRollbackCPAApplyCreatesSafetyBackupAndRestoresRollbackArchive(t *testing.T) {
	backupRoot := t.TempDir()
	backupID := "rollback-cpa-20260610T010203Z"
	writeRestoreBackupFixture(t, backupRoot, backupID)

	restored := make([]string, 0)
	client := &DockerClient{
		host: "unix:///fake/docker.sock",
		client: &http.Client{Transport: backupRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/containers/json":
				return backupJSONResponse(http.StatusOK, []dockerContainer{
					{ID: "cpa-full-id", Names: []string{"/cli-proxy-api"}, Image: "seakee/cli-proxy-api:latest", State: "running", Labels: map[string]string{"com.cpamp.managed": "true"}},
					{ID: "cpamp-full-id", Names: []string{"/cpa-manager-plus"}, Image: "seakee/cpa-manager-plus:latest", State: "running", Labels: map[string]string{"com.cpamp.role": "cpamp", "com.cpamp.managed": "true"}},
				})
			case "/networks":
				return backupJSONResponse(http.StatusOK, []dockerNetwork{{Name: standardCPANetworkName, Driver: "bridge", Labels: map[string]string{"com.cpamp.managed": "true"}}})
			case "/images/json":
				return backupJSONResponse(http.StatusOK, []dockerImage{})
			case "/containers/cli-proxy-api/archive":
				switch req.Method {
				case http.MethodGet:
					return backupTextResponse(http.StatusOK, "pre rollback cpa archive"), nil
				case http.MethodPut:
					if req.URL.Query().Get("path") != "/app" {
						t.Fatalf("rollback cpa archive path = %q", req.URL.RawQuery)
					}
					restored = append(restored, "cli-proxy-api")
					return backupJSONResponse(http.StatusOK, map[string]any{})
				}
			case "/containers/cpa-manager-plus/archive":
				switch req.Method {
				case http.MethodGet:
					return backupTextResponse(http.StatusOK, "pre rollback cpamp archive"), nil
				case http.MethodPut:
					if req.URL.Query().Get("path") != "/" {
						t.Fatalf("rollback cpamp archive path = %q", req.URL.RawQuery)
					}
					restored = append(restored, "cpa-manager-plus")
					return backupJSONResponse(http.StatusOK, map[string]any{})
				}
			case "/containers/cpa-manager-plus/stop", "/containers/cli-proxy-api/stop":
				return backupJSONResponse(http.StatusNoContent, map[string]any{})
			case "/containers/cli-proxy-api/start", "/containers/cpa-manager-plus/start":
				return backupJSONResponse(http.StatusNoContent, map[string]any{})
			default:
				t.Fatalf("unexpected docker path %s %s", req.Method, req.URL.String())
			}
			return nil, nil
		})},
	}

	result, err := client.RollbackCPA(context.Background(), RestoreApplyOptions{
		BackupRoot: backupRoot,
		BackupID:   backupID,
		Now:        time.Date(2026, 6, 10, 1, 2, 4, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("rollback apply: %v", err)
	}
	if result.Status != "rolled_back" || !result.Applied || !result.Destructive || result.ReadOnly {
		t.Fatalf("result = %#v", result)
	}
	if result.RollbackBackup == nil || result.RollbackBackup.BackupID != "pre-rollback-cpa-20260610T010204Z" {
		t.Fatalf("safety backup = %#v", result.RollbackBackup)
	}
	if strings.Join(restored, ",") != "cli-proxy-api,cpa-manager-plus" {
		t.Fatalf("restored = %#v", restored)
	}
	if !hasRestoreAction(result.Actions, "rollback_cpa_archive", "applied") ||
		!hasRestoreAction(result.Actions, "rollback_cpamp_archive", "applied") ||
		!hasRestoreAction(result.Actions, "commit_rollback", "applied") ||
		!hasRestoreCheck(result.Checks, "rollback_completed") ||
		!hasRestoreCheck(result.Checks, "rollback_preflight_ready") {
		t.Fatalf("actions=%#v checks=%#v", result.Actions, result.Checks)
	}
	if _, err := os.Stat(filepath.Join(backupRoot, result.RollbackBackup.BackupID, "manifest.json")); err != nil {
		t.Fatalf("safety backup manifest not written: %v", err)
	}
}

func TestUpgradeCPAPlanValidatesStandardTargetsAndImages(t *testing.T) {
	client := &DockerClient{
		host: "unix:///fake/docker.sock",
		client: &http.Client{Transport: backupRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/containers/json":
				return backupJSONResponse(http.StatusOK, []dockerContainer{
					{ID: "cpa-full-id", Names: []string{"/cli-proxy-api"}, Image: "seakee/cli-proxy-api:old", State: "running", Labels: map[string]string{"com.cpamp.managed": "true"}},
					{ID: "cpamp-full-id", Names: []string{"/cpa-manager-plus"}, Image: "seakee/cpa-manager-plus:old", State: "running", Labels: map[string]string{"com.cpamp.managed": "true", "com.cpamp.role": "cpamp"}},
				})
			case "/networks":
				return backupJSONResponse(http.StatusOK, []dockerNetwork{{Name: standardCPANetworkName, Driver: "bridge", Labels: map[string]string{"com.cpamp.managed": "true"}}})
			case "/images/json":
				return backupJSONResponse(http.StatusOK, []dockerImage{})
			default:
				t.Fatalf("unexpected docker path %s %s", req.Method, req.URL.String())
			}
			return nil, nil
		})},
	}

	plan, err := client.UpgradeCPAPlan(context.Background(), UpgradeOptions{})
	if err != nil {
		t.Fatalf("upgrade plan: %v", err)
	}
	if plan.Status != "ready" || plan.CPAImage != defaultCPAUpgradeImage || plan.CPAMPImage != defaultCPAMPUpgradeImage || !plan.ReadOnly || !plan.Destructive {
		t.Fatalf("plan = %#v", plan)
	}
	if !hasUpgradeCheck(plan.Checks, "upgrade_cpa_image_allowed") ||
		!hasUpgradeCheck(plan.Checks, "upgrade_cpamp_image_allowed") ||
		!hasUpgradeCheck(plan.Checks, "upgrade_standard_network_ready") ||
		!hasUpgradeCheck(plan.Checks, "upgrade_target_ready") {
		t.Fatalf("checks = %#v", plan.Checks)
	}
	if len(plan.Steps) != 4 || plan.Overview == nil {
		t.Fatalf("steps=%#v overview=%#v", plan.Steps, plan.Overview)
	}
}

func TestPrepareCPAUpgradeCreatesRollbackBackupAndPullsAllowedImages(t *testing.T) {
	backupRoot := t.TempDir()
	pulled := make([]string, 0)
	client := &DockerClient{
		host: "unix:///fake/docker.sock",
		client: &http.Client{Transport: backupRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/containers/json":
				return backupJSONResponse(http.StatusOK, []dockerContainer{
					{ID: "cpa-full-id", Names: []string{"/cli-proxy-api"}, Image: "seakee/cli-proxy-api:old", State: "running", Labels: map[string]string{"com.cpamp.managed": "true"}},
					{ID: "cpamp-full-id", Names: []string{"/cpa-manager-plus"}, Image: "seakee/cpa-manager-plus:old", State: "running", Labels: map[string]string{"com.cpamp.managed": "true", "com.cpamp.role": "cpamp"}},
				})
			case "/networks":
				return backupJSONResponse(http.StatusOK, []dockerNetwork{{Name: standardCPANetworkName, Driver: "bridge", Labels: map[string]string{"com.cpamp.managed": "true"}}})
			case "/images/json":
				return backupJSONResponse(http.StatusOK, []dockerImage{})
			case "/containers/cli-proxy-api/archive":
				if req.URL.Query().Get("path") != "/app/data" {
					t.Fatalf("cpa archive path = %q", req.URL.RawQuery)
				}
				return backupTextResponse(http.StatusOK, "upgrade cpa archive"), nil
			case "/containers/cpa-manager-plus/archive":
				if req.URL.Query().Get("path") != "/data" {
					t.Fatalf("cpamp archive path = %q", req.URL.RawQuery)
				}
				return backupTextResponse(http.StatusOK, "upgrade cpamp archive"), nil
			case "/images/create":
				pulled = append(pulled, req.URL.Query().Get("fromImage")+":"+req.URL.Query().Get("tag"))
				return backupJSONResponse(http.StatusOK, map[string]any{})
			default:
				t.Fatalf("unexpected docker path %s %s", req.Method, req.URL.String())
			}
			return nil, nil
		})},
	}

	result, err := client.PrepareCPAUpgrade(context.Background(), UpgradeOptions{
		BackupRoot: backupRoot,
		Request: model.ContainerOpsUpgradeRequest{
			CPAImage:   "seakee/cli-proxy-api:v2",
			CPAMPImage: "seakee/cpa-manager-plus:v2",
		},
		Now: time.Date(2026, 6, 10, 1, 2, 3, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("prepare upgrade: %v", err)
	}
	if result.Status != "prepared" || !result.Applied || result.ReadOnly {
		t.Fatalf("result = %#v", result)
	}
	if result.RollbackBackup == nil || result.RollbackBackup.BackupID != "upgrade-cpa-20260610T010203Z" {
		t.Fatalf("rollback backup = %#v", result.RollbackBackup)
	}
	if strings.Join(pulled, ",") != "seakee/cli-proxy-api:v2,seakee/cpa-manager-plus:v2" || len(result.ImagePulls) != 2 {
		t.Fatalf("pulled=%#v imagePulls=%#v", pulled, result.ImagePulls)
	}
	if !hasUpgradeAction(result.Actions, "create_upgrade_backup", "applied") ||
		!hasUpgradeAction(result.Actions, "pull_upgrade_images", "applied") ||
		!hasUpgradeAction(result.Actions, "prepare_recreate", "skipped") {
		t.Fatalf("actions = %#v", result.Actions)
	}
	if _, err := os.Stat(filepath.Join(backupRoot, result.RollbackBackup.BackupID, "manifest.json")); err != nil {
		t.Fatalf("upgrade rollback manifest not written: %v", err)
	}
}

func TestPrepareCPAUpgradeBlocksNonStandardImageWithoutWrites(t *testing.T) {
	var writeRequests int
	client := &DockerClient{
		host: "unix:///fake/docker.sock",
		client: &http.Client{Transport: backupRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/containers/json":
				return backupJSONResponse(http.StatusOK, []dockerContainer{
					{ID: "cpa-full-id", Names: []string{"/cli-proxy-api"}, Image: "seakee/cli-proxy-api:old", State: "running", Labels: map[string]string{"com.cpamp.managed": "true"}},
					{ID: "cpamp-full-id", Names: []string{"/cpa-manager-plus"}, Image: "seakee/cpa-manager-plus:old", State: "running", Labels: map[string]string{"com.cpamp.managed": "true", "com.cpamp.role": "cpamp"}},
				})
			case "/networks":
				return backupJSONResponse(http.StatusOK, []dockerNetwork{{Name: standardCPANetworkName, Driver: "bridge", Labels: map[string]string{"com.cpamp.managed": "true"}}})
			case "/images/json":
				return backupJSONResponse(http.StatusOK, []dockerImage{})
			default:
				writeRequests++
				t.Fatalf("unexpected docker write path %s %s", req.Method, req.URL.String())
			}
			return nil, nil
		})},
	}

	result, err := client.PrepareCPAUpgrade(context.Background(), UpgradeOptions{
		BackupRoot: t.TempDir(),
		Request: model.ContainerOpsUpgradeRequest{
			CPAImage:   "example.com/other/cpa:latest",
			CPAMPImage: "seakee/cpa-manager-plus:latest",
		},
	})
	if err != nil {
		t.Fatalf("prepare upgrade: %v", err)
	}
	if result.Status != "blocked" || result.Applied || result.RollbackBackup != nil || len(result.ImagePulls) != 0 {
		t.Fatalf("result = %#v", result)
	}
	if !hasUpgradeCheck(result.Checks, "upgrade_cpa_image_unsupported") {
		t.Fatalf("checks = %#v", result.Checks)
	}
	if writeRequests != 0 {
		t.Fatalf("write requests = %d", writeRequests)
	}
}

func TestRecreateCPAUpgradeBlocksWithoutRollbackBackup(t *testing.T) {
	var writeRequests int
	client := &DockerClient{
		host: "unix:///fake/docker.sock",
		client: &http.Client{Transport: backupRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/containers/json":
				return backupJSONResponse(http.StatusOK, []dockerContainer{
					{ID: "cpa-full-id", Names: []string{"/cli-proxy-api"}, Image: "seakee/cli-proxy-api:old", State: "running", Labels: map[string]string{"com.cpamp.managed": "true"}},
					{ID: "cpamp-full-id", Names: []string{"/cpa-manager-plus"}, Image: "seakee/cpa-manager-plus:old", State: "running", Labels: map[string]string{"com.cpamp.managed": "true", "com.cpamp.role": "cpamp"}},
				})
			case "/networks":
				return backupJSONResponse(http.StatusOK, []dockerNetwork{{Name: standardCPANetworkName, Driver: "bridge", Labels: map[string]string{"com.cpamp.managed": "true"}}})
			case "/images/json":
				return backupJSONResponse(http.StatusOK, []dockerImage{})
			default:
				writeRequests++
				t.Fatalf("unexpected docker write path %s %s", req.Method, req.URL.String())
			}
			return nil, nil
		})},
	}

	result, err := client.RecreateCPAUpgrade(context.Background(), UpgradeOptions{
		BackupRoot: t.TempDir(),
		Request: model.ContainerOpsUpgradeRequest{
			CPAImage:   "seakee/cli-proxy-api:v2",
			CPAMPImage: "seakee/cpa-manager-plus:v2",
		},
	})
	if err != nil {
		t.Fatalf("recreate upgrade: %v", err)
	}
	if result.Status != "blocked" || result.Applied || result.ReadOnly {
		t.Fatalf("result = %#v", result)
	}
	if !hasUpgradeAction(result.Actions, "verify_rollback_backup", "failed") {
		t.Fatalf("actions = %#v", result.Actions)
	}
	if writeRequests != 0 {
		t.Fatalf("write requests = %d", writeRequests)
	}
}

func TestRecreateCPAUpgradeRecreatesCPAOnlyWithRollbackBackup(t *testing.T) {
	backupRoot := t.TempDir()
	backupID := "upgrade-cpa-20260610T010203Z"
	writeNetworkBackupFixture(t, backupRoot, backupID)

	var stoppedOld bool
	var preservedOld bool
	var createdNew bool
	var startedNew bool
	client := &DockerClient{
		host: "unix:///fake/docker.sock",
		client: &http.Client{Transport: backupRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/containers/json":
				containers := []dockerContainer{
					{
						ID:     "cpamp-full-id",
						Names:  []string{"/cpa-manager-plus"},
						Image:  "seakee/cpa-manager-plus:old",
						State:  "running",
						Labels: map[string]string{"com.cpamp.managed": "true", "com.cpamp.role": "cpamp"},
					},
				}
				if startedNew {
					containers = append(containers,
						dockerContainer{
							ID:     "new-cpa-full-id",
							Names:  []string{"/cli-proxy-api"},
							Image:  "seakee/cli-proxy-api:v2",
							State:  "running",
							Labels: map[string]string{"com.cpamp.managed": "true", "com.cpamp.role": "cpa"},
							Mounts: []dockerMount{{Type: "volume", Name: "cpamp-cpa_cpa-data", Destination: "/app/data", RW: true}},
							Ports:  []dockerPort{{PrivatePort: 8317, PublicPort: 8317, Type: "tcp"}},
						},
						dockerContainer{
							ID:     "old-cpa-full-id",
							Names:  []string{"/cli-proxy-api-upgrade-old-20260610T010203Z"},
							Image:  "seakee/cli-proxy-api:old",
							State:  "exited",
							Labels: map[string]string{"com.cpamp.managed": "true", "com.cpamp.role": "cpa"},
							Mounts: []dockerMount{{Type: "volume", Name: "cpamp-cpa_cpa-data", Destination: "/app/data", RW: true}},
						},
					)
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
				if req.Method != http.MethodPost {
					t.Fatalf("stop method = %s", req.Method)
				}
				stoppedOld = true
				return backupJSONResponse(http.StatusNoContent, map[string]any{})
			case "/containers/cli-proxy-api/rename":
				if req.Method != http.MethodPost {
					t.Fatalf("rename method = %s", req.Method)
				}
				if req.URL.Query().Get("name") != "cli-proxy-api-upgrade-old-20260610T010203Z" {
					t.Fatalf("preserved name = %q", req.URL.RawQuery)
				}
				preservedOld = true
				return backupJSONResponse(http.StatusNoContent, map[string]any{})
			case "/containers/create":
				if req.Method != http.MethodPost || req.URL.Query().Get("name") != "cli-proxy-api" {
					t.Fatalf("create request = %s %s", req.Method, req.URL.String())
				}
				var payload struct {
					Image      string            `json:"Image"`
					Labels     map[string]string `json:"Labels"`
					HostConfig struct {
						Mounts []struct {
							Type   string `json:"Type"`
							Source string `json:"Source"`
							Target string `json:"Target"`
						} `json:"Mounts"`
					} `json:"HostConfig"`
				}
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Fatalf("decode create payload: %v", err)
				}
				hasCPAMount := false
				for _, mount := range payload.HostConfig.Mounts {
					if mount.Type == "volume" && mount.Source == "cpamp-cpa_cpa-data" && mount.Target == "/app/data" {
						hasCPAMount = true
					}
				}
				if payload.Image != "seakee/cli-proxy-api:v2" ||
					payload.Labels["com.cpamp.role"] != "cpa" ||
					!hasCPAMount {
					t.Fatalf("create payload = %#v", payload)
				}
				createdNew = true
				return backupJSONResponse(http.StatusCreated, map[string]any{"Id": "new-cpa-full-id"})
			case "/containers/cli-proxy-api/start":
				if req.Method != http.MethodPost {
					t.Fatalf("start method = %s", req.Method)
				}
				startedNew = true
				return backupJSONResponse(http.StatusNoContent, map[string]any{})
			default:
				t.Fatalf("unexpected docker path %s %s", req.Method, req.URL.String())
			}
			return nil, nil
		})},
	}

	result, err := client.RecreateCPAUpgrade(context.Background(), UpgradeOptions{
		BackupRoot:       backupRoot,
		RollbackBackupID: backupID,
		Request: model.ContainerOpsUpgradeRequest{
			CPAImage:   "seakee/cli-proxy-api:v2",
			CPAMPImage: "seakee/cpa-manager-plus:v2",
		},
		Now: time.Date(2026, 6, 10, 1, 2, 3, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("recreate upgrade: %v", err)
	}
	if result.Status != "completed" || !result.Applied || result.ReadOnly {
		t.Fatalf("result = %#v", result)
	}
	if !stoppedOld || !preservedOld || !createdNew || !startedNew {
		t.Fatalf("stopped=%v preserved=%v created=%v started=%v", stoppedOld, preservedOld, createdNew, startedNew)
	}
	if !hasUpgradeAction(result.Actions, "verify_rollback_backup", "applied") ||
		!hasUpgradeAction(result.Actions, "stop_cpa_container", "applied") ||
		!hasUpgradeAction(result.Actions, "preserve_old_cpa_container", "applied") ||
		!hasUpgradeAction(result.Actions, "recreate_cpa_container", "applied") ||
		!hasUpgradeAction(result.Actions, "start_cpa_container", "applied") ||
		!hasUpgradeAction(result.Actions, "healthcheck_after_recreate", "applied") ||
		!hasUpgradeAction(result.Actions, "recreate_cpamp_container", "skipped") {
		t.Fatalf("actions = %#v", result.Actions)
	}
	if result.RollbackBackup == nil || result.RollbackBackup.BackupID != backupID {
		t.Fatalf("rollback backup = %#v", result.RollbackBackup)
	}
}

func TestStandardizeCPANetworkPlansAndAppliesGuardedNetworkWrites(t *testing.T) {
	backupRoot := t.TempDir()
	backupID := "cpa-20260610T010203Z"
	writeNetworkBackupFixture(t, backupRoot, backupID)

	var networkCreated bool
	connectedContainers := make([]string, 0)
	client := &DockerClient{
		host: "unix:///fake/docker.sock",
		client: &http.Client{Transport: backupRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/containers/json":
				return backupJSONResponse(http.StatusOK, []dockerContainer{
					{
						ID:     "cpa-full-id",
						Names:  []string{"/legacy-cpa"},
						Image:  "seakee/cli-proxy-api:latest",
						State:  "running",
						Labels: map[string]string{},
						NetworkSettings: dockerContainerNetworkSettings{
							Networks: map[string]dockerEndpoint{"legacy_net": {NetworkID: "legacy-network"}},
						},
					},
					{
						ID:     "newapi-full-id",
						Names:  []string{"/new-api"},
						Image:  "calciumion/new-api:latest",
						State:  "running",
						Labels: map[string]string{},
						NetworkSettings: dockerContainerNetworkSettings{
							Networks: map[string]dockerEndpoint{"legacy_net": {NetworkID: "legacy-network"}},
						},
					},
				})
			case "/networks":
				if networkCreated {
					return backupJSONResponse(http.StatusOK, []dockerNetwork{
						{
							ID:         "network-cpamp-cpa",
							Name:       standardCPANetworkName,
							Driver:     "bridge",
							Scope:      "local",
							Labels:     map[string]string{"com.cpamp.managed": "true"},
							Containers: map[string]dockerEndpoint{},
						},
					})
				}
				return backupJSONResponse(http.StatusOK, []dockerNetwork{})
			case "/images/json":
				return backupJSONResponse(http.StatusOK, []dockerImage{})
			case "/networks/create":
				if req.Method != http.MethodPost {
					t.Fatalf("network create method = %s", req.Method)
				}
				var payload struct {
					Name   string            `json:"Name"`
					Driver string            `json:"Driver"`
					Labels map[string]string `json:"Labels"`
				}
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Fatalf("decode network create: %v", err)
				}
				if payload.Name != standardCPANetworkName || payload.Driver != "bridge" || payload.Labels["com.cpamp.stack"] != "cpa" {
					t.Fatalf("network create payload = %#v", payload)
				}
				networkCreated = true
				return backupJSONResponse(http.StatusCreated, map[string]string{"Id": "network-cpamp-cpa"})
			case "/networks/cpamp-cpa_default/connect":
				if req.Method != http.MethodPost {
					t.Fatalf("network connect method = %s", req.Method)
				}
				var payload struct {
					Container string `json:"Container"`
				}
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Fatalf("decode network connect: %v", err)
				}
				connectedContainers = append(connectedContainers, payload.Container)
				return backupJSONResponse(http.StatusOK, map[string]any{})
			default:
				t.Fatalf("unexpected docker path %s", req.URL.String())
			}
			return nil, nil
		})},
	}

	plan, err := client.StandardizeCPANetwork(context.Background(), NetworkStandardizeOptions{
		BackupRoot: backupRoot,
		BackupID:   backupID,
	})
	if err != nil {
		t.Fatalf("network plan: %v", err)
	}
	if plan.Status != "planned_with_warnings" || plan.Applied || plan.Destructive {
		t.Fatalf("plan = %#v", plan)
	}
	if networkCreated || len(connectedContainers) != 0 {
		t.Fatalf("dry run wrote docker resources created=%v connected=%#v", networkCreated, connectedContainers)
	}
	if !hasNetworkAction(plan.Actions, "create_standard_network", "planned") ||
		!hasNetworkAction(plan.Actions, "connect_cpa_to_standard_network", "planned") ||
		!hasNetworkAction(plan.Actions, "connect_newapi_to_standard_network", "planned") {
		t.Fatalf("actions = %#v", plan.Actions)
	}

	result, err := client.StandardizeCPANetwork(context.Background(), NetworkStandardizeOptions{
		BackupRoot: backupRoot,
		BackupID:   backupID,
		Apply:      true,
	})
	if err != nil {
		t.Fatalf("network apply: %v", err)
	}
	if result.Status != "applied_with_warnings" || !result.Applied || result.Destructive {
		t.Fatalf("result = %#v", result)
	}
	if !networkCreated {
		t.Fatalf("network was not created")
	}
	if len(connectedContainers) != 2 || connectedContainers[0] != "legacy-cpa" || connectedContainers[1] != "new-api" {
		t.Fatalf("connected containers = %#v", connectedContainers)
	}
	if !hasNetworkAction(result.Actions, "create_standard_network", "applied") ||
		!hasNetworkAction(result.Actions, "connect_cpa_to_standard_network", "applied") ||
		!hasNetworkAction(result.Actions, "connect_newapi_to_standard_network", "applied") {
		t.Fatalf("result actions = %#v", result.Actions)
	}
}

func TestStandardizeCPANetworkBlocksWithoutCPATarget(t *testing.T) {
	backupRoot := t.TempDir()
	backupID := "cpa-20260610T010203Z"
	writeNetworkBackupFixture(t, backupRoot, backupID)

	client := &DockerClient{
		host: "unix:///fake/docker.sock",
		client: &http.Client{Transport: backupRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/containers/json":
				return backupJSONResponse(http.StatusOK, []dockerContainer{
					{ID: "newapi-full-id", Names: []string{"/new-api"}, Image: "calciumion/new-api:latest", State: "running"},
				})
			case "/networks":
				return backupJSONResponse(http.StatusOK, []dockerNetwork{})
			case "/images/json":
				return backupJSONResponse(http.StatusOK, []dockerImage{})
			default:
				t.Fatalf("unexpected docker path %s", req.URL.String())
			}
			return nil, nil
		})},
	}

	result, err := client.StandardizeCPANetwork(context.Background(), NetworkStandardizeOptions{
		BackupRoot: backupRoot,
		BackupID:   backupID,
		Apply:      true,
	})
	if err != nil {
		t.Fatalf("network standardize: %v", err)
	}
	if result.Status != "blocked" || result.Applied {
		t.Fatalf("result = %#v", result)
	}
	if !hasNetworkCheck(result.Checks, "cpa_target_missing") {
		t.Fatalf("checks = %#v", result.Checks)
	}
}

func TestPullCPADeployImagesUsesOnlyStandardManifestImages(t *testing.T) {
	pulled := make([]string, 0)
	client := &DockerClient{
		host: "unix:///fake/docker.sock",
		client: &http.Client{Transport: backupRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost || req.URL.Path != "/images/create" {
				t.Fatalf("unexpected docker request %s %s", req.Method, req.URL.String())
			}
			pulled = append(pulled, req.URL.Query().Get("fromImage")+":"+req.URL.Query().Get("tag"))
			return backupJSONResponse(http.StatusOK, map[string]any{})
		})},
	}

	result, err := client.PullCPADeployImages(context.Background(), standardDeployRenderRequest())
	if err != nil {
		t.Fatalf("pull deploy images: %v", err)
	}
	if len(result) != 2 || result[0].Image != "seakee/cli-proxy-api:latest" || result[1].Image != "seakee/cpa-manager-plus:latest" {
		t.Fatalf("result = %#v", result)
	}
	if len(pulled) != 2 || pulled[0] != "seakee/cli-proxy-api:latest" || pulled[1] != "seakee/cpa-manager-plus:latest" {
		t.Fatalf("pulled = %#v", pulled)
	}
}

func TestPullCPADeployImagesRejectsNonStandardImage(t *testing.T) {
	request := standardDeployRenderRequest()
	request.Manifest.Services[0].Image = "library/nginx:latest"

	client := &DockerClient{
		host: "unix:///fake/docker.sock",
		client: &http.Client{Transport: backupRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected docker request %s", req.URL.String())
			return nil, nil
		})},
	}

	if _, err := client.PullCPADeployImages(context.Background(), request); err == nil {
		t.Fatalf("expected non-standard image error")
	}
}

func TestStartCPADeployServicesBlocksWithoutDeployEnvAndDoesNotWriteDocker(t *testing.T) {
	stackRoot := t.TempDir()
	writeDeployStartStackFiles(t, stackRoot)

	var dockerRequests int
	client := &DockerClient{
		host: "unix:///fake/docker.sock",
		client: &http.Client{Transport: backupRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			dockerRequests++
			t.Fatalf("unexpected docker request %s %s", req.Method, req.URL.String())
			return nil, nil
		})},
	}

	result, err := client.StartCPADeployServices(context.Background(), DeployStartOptions{
		StackRoot:  stackRoot,
		BackupRoot: t.TempDir(),
		Request:    standardDeployRenderRequest(),
	})
	if err != nil {
		t.Fatalf("start services: %v", err)
	}
	if result.Status != "blocked" {
		t.Fatalf("result = %#v", result)
	}
	if !hasAgentDeployCheck(result.Checks, "deploy_env_missing") {
		t.Fatalf("checks = %#v", result.Checks)
	}
	if dockerRequests != 0 {
		t.Fatalf("docker requests = %d", dockerRequests)
	}
	if hasAgentDeployActionStatus(result.Actions, "create_standard_network", "applied") {
		t.Fatalf("actions should remain planned when env is missing: %#v", result.Actions)
	}
}

func TestStartCPADeployServicesCreatesAndStartsStandardStack(t *testing.T) {
	stackRoot := t.TempDir()
	backupRoot := t.TempDir()
	writeDeployStartStackFiles(t, stackRoot)
	writeDeployStartEnv(t, stackRoot)

	var overviewCalls int
	var networkCreated bool
	volumesCreated := make([]string, 0)
	containersCreated := make([]string, 0)
	started := make([]string, 0)
	client := &DockerClient{
		host: "unix:///fake/docker.sock",
		client: &http.Client{Transport: backupRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/containers/json":
				overviewCalls++
				switch overviewCalls {
				case 1:
					return backupJSONResponse(http.StatusOK, []dockerContainer{})
				case 2:
					return backupJSONResponse(http.StatusOK, []dockerContainer{
						deployTestContainer("cli-proxy-api", "seakee/cli-proxy-api:latest", "created", "cpa"),
						deployTestContainer("cpamp-agent", "seakee/cpa-manager-plus:latest", "created", "agent"),
						deployTestContainer("cpa-manager-plus", "seakee/cpa-manager-plus:latest", "created", "cpamp"),
					})
				default:
					return backupJSONResponse(http.StatusOK, []dockerContainer{
						deployTestContainer("cli-proxy-api", "seakee/cli-proxy-api:latest", "running", "cpa"),
						deployTestContainer("cpamp-agent", "seakee/cpa-manager-plus:latest", "running", "agent"),
						deployTestContainer("cpa-manager-plus", "seakee/cpa-manager-plus:latest", "running", "cpamp"),
					})
				}
			case "/networks":
				if networkCreated {
					return backupJSONResponse(http.StatusOK, []dockerNetwork{
						{Name: standardCPANetworkName, Driver: "bridge", Labels: map[string]string{"com.cpamp.managed": "true"}},
					})
				}
				return backupJSONResponse(http.StatusOK, []dockerNetwork{})
			case "/images/json":
				return backupJSONResponse(http.StatusOK, []dockerImage{})
			case "/networks/create":
				if req.Method != http.MethodPost {
					t.Fatalf("network method = %s", req.Method)
				}
				var payload struct {
					Name   string            `json:"Name"`
					Driver string            `json:"Driver"`
					Labels map[string]string `json:"Labels"`
				}
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Fatalf("decode network create: %v", err)
				}
				if payload.Name != standardCPANetworkName || payload.Driver != "bridge" || payload.Labels["com.cpamp.stack"] != "cpa" {
					t.Fatalf("network payload = %#v", payload)
				}
				networkCreated = true
				return backupJSONResponse(http.StatusCreated, map[string]string{"Id": "network-cpamp-cpa"})
			case "/volumes/create":
				var payload struct {
					Name   string            `json:"Name"`
					Labels map[string]string `json:"Labels"`
				}
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Fatalf("decode volume create: %v", err)
				}
				if payload.Labels["com.cpamp.managed"] != "true" || payload.Labels["com.cpamp.stack"] != "cpa" {
					t.Fatalf("volume payload = %#v", payload)
				}
				volumesCreated = append(volumesCreated, payload.Name)
				return backupJSONResponse(http.StatusCreated, map[string]string{"Name": payload.Name})
			case "/containers/create":
				name := req.URL.Query().Get("name")
				var payload struct {
					Image      string            `json:"Image"`
					Labels     map[string]string `json:"Labels"`
					Env        []string          `json:"Env"`
					Entrypoint []string          `json:"Entrypoint"`
					HostConfig struct {
						Binds       []string `json:"Binds"`
						CapAdd      []string `json:"CapAdd"`
						NetworkMode string   `json:"NetworkMode"`
					} `json:"HostConfig"`
				}
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Fatalf("decode container create: %v", err)
				}
				if payload.Labels["com.cpamp.managed"] != "true" || payload.Labels["com.cpamp.stack"] != "cpa" {
					t.Fatalf("container labels for %s = %#v", name, payload.Labels)
				}
				switch name {
				case "cli-proxy-api":
					if payload.Labels["com.cpamp.role"] != "cpa" ||
						payload.Image != "seakee/cli-proxy-api:latest" ||
						payload.HostConfig.NetworkMode != "host" {
						t.Fatalf("cpa payload = %#v", payload)
					}
				case "cpa-manager-plus":
					if payload.Labels["com.cpamp.role"] != "cpamp" ||
						!containsString(payload.Env, "CPA_MANAGER_ADMIN_KEY=admin-secret") ||
						!containsString(payload.Env, "CPAMP_AGENT_URL=http://host.docker.internal:18417") {
						t.Fatalf("cpamp payload = %#v", payload)
					}
				case "cpamp-agent":
					if payload.Labels["com.cpamp.role"] != "agent" ||
						!containsString(payload.Entrypoint, "cpamp-agent") ||
						payload.HostConfig.NetworkMode != "host" ||
						!containsString(payload.HostConfig.CapAdd, "NET_ADMIN") ||
						!containsString(payload.Env, "CPAMP_STACK_ROOT="+stackRoot) ||
						!containsString(payload.Env, "CPAMP_BACKUP_ROOT="+backupRoot) ||
						!containsString(payload.HostConfig.Binds, stackRoot+":"+stackRoot) ||
						!containsString(payload.HostConfig.Binds, backupRoot+":"+backupRoot) {
						t.Fatalf("agent payload = %#v", payload)
					}
				default:
					t.Fatalf("unexpected container create name %q", name)
				}
				containersCreated = append(containersCreated, name)
				return backupJSONResponse(http.StatusCreated, map[string]string{"Id": name + "-id"})
			case "/containers/cli-proxy-api/start", "/containers/cpamp-agent/start", "/containers/cpa-manager-plus/start":
				started = append(started, strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/containers/"), "/start"))
				return backupJSONResponse(http.StatusNoContent, map[string]any{})
			default:
				t.Fatalf("unexpected docker path %s", req.URL.String())
			}
			return nil, nil
		})},
	}

	result, err := client.StartCPADeployServices(context.Background(), DeployStartOptions{
		StackRoot:  stackRoot,
		BackupRoot: backupRoot,
		Request:    standardDeployRenderRequest(),
	})
	if err != nil {
		t.Fatalf("start services: %v", err)
	}
	if result.Status != "started" {
		t.Fatalf("result = %#v", result)
	}
	if !networkCreated {
		t.Fatalf("network was not created")
	}
	if strings.Join(volumesCreated, ",") != "cpamp-cpa_cpa-data,cpamp-cpa_cpa-manager-plus-data" {
		t.Fatalf("volumes = %#v", volumesCreated)
	}
	if strings.Join(containersCreated, ",") != "cli-proxy-api,cpa-manager-plus,cpamp-agent" {
		t.Fatalf("containers = %#v", containersCreated)
	}
	if strings.Join(started, ",") != "cli-proxy-api,cpamp-agent,cpa-manager-plus" {
		t.Fatalf("started order = %#v", started)
	}
	if !hasAgentDeployActionStatus(result.Actions, "healthcheck_services", "applied") ||
		!hasAgentDeployCheck(result.Checks, "deploy_services_started") {
		t.Fatalf("actions=%#v checks=%#v", result.Actions, result.Checks)
	}
}

type backupRoundTripFunc func(req *http.Request) (*http.Response, error)

func (f backupRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func backupJSONResponse(status int, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(data)),
	}, nil
}

func backupTextResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
	}
}

func writeTestTar(path string, files map[string]string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	writer := tar.NewWriter(file)
	for name, content := range files {
		data := []byte(content)
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data))}); err != nil {
			_ = file.Close()
			return err
		}
		if _, err := writer.Write(data); err != nil {
			_ = file.Close()
			return err
		}
	}
	closeWriterErr := writer.Close()
	closeFileErr := file.Close()
	if closeWriterErr != nil {
		return closeWriterErr
	}
	return closeFileErr
}

func writeNetworkBackupFixture(t *testing.T, backupRoot string, backupID string) {
	t.Helper()
	backupDir := filepath.Join(backupRoot, backupID)
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		t.Fatalf("create backup dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "cpa-cli-proxy-api.tar"), []byte("cpa archive"), 0o640); err != nil {
		t.Fatalf("write cpa archive: %v", err)
	}
	manifest := model.ContainerOpsBackupResult{
		BackupID: backupID,
		Archives: []model.ContainerOpsBackupArchive{
			{Role: "cpa", Service: "cli-proxy-api", Container: "cli-proxy-api", Path: "/app/data", FileName: "cpa-cli-proxy-api.tar", Size: int64(len("cpa archive"))},
		},
		ReadOnly: true,
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal network backup manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "manifest.json"), data, 0o640); err != nil {
		t.Fatalf("write network backup manifest: %v", err)
	}
}

func writeRestoreBackupFixture(t *testing.T, backupRoot string, backupID string) {
	t.Helper()
	backupDir := filepath.Join(backupRoot, backupID)
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		t.Fatalf("create restore backup dir: %v", err)
	}
	cpaArchivePath := filepath.Join(backupDir, "cpa-cli-proxy-api.tar")
	if err := os.WriteFile(cpaArchivePath, []byte("cpa archive"), 0o640); err != nil {
		t.Fatalf("write cpa archive: %v", err)
	}
	cpampArchivePath := filepath.Join(backupDir, "cpamp-cpa-manager-plus.tar")
	if err := writeTestTar(cpampArchivePath, map[string]string{
		"data/usage.sqlite": "usage db",
		"data/data.key":     "data key",
	}); err != nil {
		t.Fatalf("write cpamp archive: %v", err)
	}
	cpaStat, err := os.Stat(cpaArchivePath)
	if err != nil {
		t.Fatalf("stat cpa archive: %v", err)
	}
	cpampStat, err := os.Stat(cpampArchivePath)
	if err != nil {
		t.Fatalf("stat cpamp archive: %v", err)
	}
	manifest := model.ContainerOpsBackupResult{
		BackupID:   backupID,
		Status:     "completed",
		BackupRoot: backupRoot,
		CreatedAt:  1781053323,
		Archives: []model.ContainerOpsBackupArchive{
			{Role: "cpa", Service: "cli-proxy-api", Container: "cli-proxy-api", Path: "/app/data", FileName: "cpa-cli-proxy-api.tar", Size: cpaStat.Size()},
			{Role: "cpamp", Service: "cpa-manager-plus", Container: "cpa-manager-plus", Path: "/data", FileName: "cpamp-cpa-manager-plus.tar", Size: cpampStat.Size()},
		},
		ReadOnly: true,
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal restore manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "manifest.json"), data, 0o640); err != nil {
		t.Fatalf("write restore manifest: %v", err)
	}
}

func writeDeployStartStackFiles(t *testing.T, stackRoot string) {
	t.Helper()
	request := standardDeployRenderRequest()
	manifestData, err := json.Marshal(request.Manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stackRoot, "compose.yml"), []byte(request.Compose.Content), 0o640); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stackRoot, "stack.manifest.json"), manifestData, 0o640); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func writeDeployStartEnv(t *testing.T, stackRoot string) {
	t.Helper()
	data := strings.Join([]string{
		"CPA_MANAGER_ADMIN_KEY=admin-secret",
		"CPA_MANAGEMENT_KEY=management-secret",
		"CPAMP_AGENT_TOKEN=agent-token",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(stackRoot, ".env"), []byte(data), 0o640); err != nil {
		t.Fatalf("write env: %v", err)
	}
}

func deployTestContainer(name string, image string, state string, role string) dockerContainer {
	return dockerContainer{
		ID:     name + "-full-id",
		Names:  []string{"/" + name},
		Image:  image,
		State:  state,
		Status: state,
		Labels: map[string]string{
			"com.cpamp.managed": "true",
			"com.cpamp.role":    role,
			"com.cpamp.stack":   "cpa",
		},
	}
}

func hasRestoreCheck(checks []model.ContainerOpsRestoreCheck, code string) bool {
	for _, check := range checks {
		if check.Code == code {
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

func hasNetworkCheck(checks []model.ContainerOpsNetworkCheck, code string) bool {
	for _, check := range checks {
		if check.Code == code {
			return true
		}
	}
	return false
}

func hasAgentDeployCheck(checks []model.ContainerOpsDeployCheck, code string) bool {
	for _, check := range checks {
		if check.Code == code {
			return true
		}
	}
	return false
}

func hasAgentDeployActionStatus(actions []model.ContainerOpsDeployAction, code string, status string) bool {
	for _, action := range actions {
		if action.Code == code && action.Status == status {
			return true
		}
	}
	return false
}

func hasUpgradeCheck(checks []model.ContainerOpsUpgradeCheck, code string) bool {
	for _, check := range checks {
		if check.Code == code {
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

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func hasNetworkAction(actions []model.ContainerOpsNetworkAction, code string, status string) bool {
	for _, action := range actions {
		if action.Code == code && action.Status == status {
			return true
		}
	}
	return false
}
