package containeropsagent

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/response"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
)

type ServerOptions struct {
	ServiceID  string
	Version    string
	DockerHost string
	Token      string
	StackRoot  string
	BackupRoot string
}

type Server struct {
	serviceID      string
	version        string
	token          string
	stackRoot      string
	backupRoot     string
	upgradeJobRoot string
	docker         *DockerClient
	jobMu          sync.Mutex
	jobSeq         int64
	upgradeJobs    map[string]model.ContainerOpsUpgradeJob
}

func NewServer(options ServerOptions) (*Server, error) {
	docker, err := NewDockerClient(options.DockerHost)
	if err != nil {
		return nil, err
	}
	serviceID := strings.TrimSpace(options.ServiceID)
	if serviceID == "" {
		serviceID = "cpamp-agent"
	}
	version := strings.TrimSpace(options.Version)
	if version == "" {
		version = "dev"
	}
	backupRoot := cleanBackupRoot(options.BackupRoot)
	server := &Server{
		serviceID:      serviceID,
		version:        version,
		token:          strings.TrimSpace(options.Token),
		stackRoot:      cleanStackRoot(options.StackRoot),
		backupRoot:     backupRoot,
		upgradeJobRoot: filepath.Join(backupRoot, "upgrade-jobs"),
		docker:         docker,
		upgradeJobs:    make(map[string]model.ContainerOpsUpgradeJob),
	}
	if err := server.loadUpgradeJobs(); err != nil {
		return nil, err
	}
	return server, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/agent/info", s.withAuth(s.agentInfo))
	mux.HandleFunc("/docker/overview", s.withAuth(s.dockerOverview))
	mux.HandleFunc("/deploys/cpa/render", s.withAuth(s.renderCPADeployFiles))
	mux.HandleFunc("/deploys/cpa/pull-images", s.withAuth(s.pullCPADeployImages))
	mux.HandleFunc("/deploys/cpa/start", s.withAuth(s.startCPADeployServices))
	mux.HandleFunc("/backups/cpa", s.withAuth(s.backupCPA))
	mux.HandleFunc("/restores/cpa/plan", s.withAuth(s.restoreCPAPlan))
	mux.HandleFunc("/restores/cpa/apply", s.withAuth(s.restoreCPAApply))
	mux.HandleFunc("/rollbacks/cpa/apply", s.withAuth(s.rollbackCPAApply))
	mux.HandleFunc("/networks/cpa/standardize", s.withAuth(s.standardizeCPANetwork))
	mux.HandleFunc("/egress/ips", s.withAuth(s.listEgressIPs))
	mux.HandleFunc("/egress/source-ip/ensure", s.withAuth(s.ensureSourceIP))
	mux.HandleFunc("/egress/source-ip/remove", s.withAuth(s.removeSourceIP))
	mux.HandleFunc("/egress/source-ip/check", s.withAuth(s.checkSourceIP))
	mux.HandleFunc("/upgrades/cpa/plan", s.withAuth(s.upgradeCPAPlan))
	mux.HandleFunc("/upgrades/cpa/prepare", s.withAuth(s.upgradeCPAPrepare))
	mux.HandleFunc("/upgrades/cpa/recreate", s.withAuth(s.upgradeCPARecreate))
	mux.HandleFunc("/upgrades/cpa/jobs", s.withAuth(s.upgradeCPAJobs))
	mux.HandleFunc("/upgrades/cpa/jobs/", s.withAuth(s.upgradeCPAJob))
	return mux
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		response.MethodNotAllowed(w)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": s.serviceID,
		"version": s.version,
	})
}

func (s *Server) agentInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		response.MethodNotAllowed(w)
		return
	}
	response.JSON(w, http.StatusOK, s.info(true))
}

func (s *Server) dockerOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		response.MethodNotAllowed(w)
		return
	}
	overview, err := s.docker.Overview(r.Context())
	if err != nil {
		response.Error(w, http.StatusBadGateway, err)
		return
	}
	response.JSON(w, http.StatusOK, overview)
}

func (s *Server) backupCPA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.MethodNotAllowed(w)
		return
	}
	result, err := s.docker.BackupCPAStack(r.Context(), BackupOptions{BackupRoot: s.backupRoot})
	if err != nil {
		response.Error(w, http.StatusBadGateway, err)
		return
	}
	response.JSON(w, http.StatusOK, result)
}

func (s *Server) restoreCPAPlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.MethodNotAllowed(w)
		return
	}
	var request model.ContainerOpsRestoreRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		response.Error(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.docker.RestoreCPAPlan(r.Context(), RestorePlanOptions{
		BackupRoot: s.backupRoot,
		BackupID:   request.BackupID,
	})
	if err != nil {
		response.Error(w, http.StatusBadGateway, err)
		return
	}
	response.JSON(w, http.StatusOK, result)
}

func (s *Server) restoreCPAApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.MethodNotAllowed(w)
		return
	}
	var request model.ContainerOpsRestoreRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		response.Error(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.docker.RestoreCPA(r.Context(), RestoreApplyOptions{
		BackupRoot: s.backupRoot,
		BackupID:   request.BackupID,
	})
	if err != nil {
		response.Error(w, http.StatusBadGateway, err)
		return
	}
	response.JSON(w, http.StatusOK, result)
}

func (s *Server) rollbackCPAApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.MethodNotAllowed(w)
		return
	}
	var request model.ContainerOpsRollbackRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		response.Error(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.docker.RollbackCPA(r.Context(), RestoreApplyOptions{
		BackupRoot: s.backupRoot,
		BackupID:   request.BackupID,
	})
	if err != nil {
		response.Error(w, http.StatusBadGateway, err)
		return
	}
	response.JSON(w, http.StatusOK, result)
}

func (s *Server) standardizeCPANetwork(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		response.MethodNotAllowed(w)
		return
	}
	var request model.ContainerOpsNetworkStandardizeRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		response.Error(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.docker.StandardizeCPANetwork(r.Context(), NetworkStandardizeOptions{
		BackupRoot: s.backupRoot,
		BackupID:   request.BackupID,
		Apply:      request.Apply,
	})
	if err != nil {
		response.Error(w, http.StatusBadGateway, err)
		return
	}
	response.JSON(w, http.StatusOK, result)
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" {
			next(w, r)
			return
		}
		if bearerToken(r.Header.Get("Authorization")) != s.token {
			response.Error(w, http.StatusUnauthorized, errors.New("invalid agent token"))
			return
		}
		next(w, r)
	}
}

func (s *Server) info(reachable bool) model.ContainerOpsAgentInfo {
	return model.ContainerOpsAgentInfo{
		Configured: true,
		Reachable:  reachable,
		Service:    s.serviceID,
		Version:    s.version,
		Mode:       "agent",
		DockerHost: s.docker.Host(),
		ReadOnly:   false,
	}
}

func bearerToken(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	const prefix = "bearer "
	if strings.HasPrefix(strings.ToLower(header), prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return ""
}
