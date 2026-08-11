package containerops

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/app"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/middleware"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/response"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	containeropssvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/containerops"
)

type Handler struct {
	App *app.Context
}

func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) {
	if !middleware.AuthorizePanel(w, r, h.App.AdminAuthService) {
		return
	}

	path := strings.TrimRight(r.URL.Path, "/")
	switch {
	case path == "/v0/management/container-ops/audits" && r.Method == http.MethodGet:
		limit := 20
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				limit = parsed
			}
		}
		audits, err := h.App.ContainerOpsService.Audits(r.Context(), limit)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, err)
			return
		}
		response.JSON(w, http.StatusOK, map[string]any{"items": audits})
	case path == "/v0/management/container-ops/upgrade-tasks" && r.Method == http.MethodGet:
		limit := 20
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				limit = parsed
			}
		}
		tasks, err := h.App.ContainerOpsService.UpgradeTasks(r.Context(), limit)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, err)
			return
		}
		response.JSON(w, http.StatusOK, map[string]any{"items": tasks})
	case path == "/v0/management/container-ops/upgrade-tasks/start" && r.Method == http.MethodPost:
		var request model.ContainerOpsUpgradeTaskStartRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			response.Error(w, http.StatusBadRequest, err)
			return
		}
		task, err := h.App.ContainerOpsService.StartUpgradeTask(r.Context(), request)
		if err != nil {
			response.Error(w, containerOpsErrorStatus(http.StatusBadRequest, err), err)
			return
		}
		response.JSON(w, http.StatusOK, task)
	case path == "/v0/management/container-ops/info" && r.Method == http.MethodGet:
		info, err := h.App.ContainerOpsService.Info(r.Context())
		if err != nil {
			response.Error(w, http.StatusInternalServerError, err)
			return
		}
		response.JSON(w, http.StatusOK, info)
	case path == "/v0/management/container-ops/agent" && r.Method == http.MethodGet:
		agent, err := h.App.ContainerOpsService.Agent(r.Context())
		if err != nil {
			response.Error(w, http.StatusInternalServerError, err)
			return
		}
		response.JSON(w, http.StatusOK, agent)
	case path == "/v0/management/container-ops/discover" && r.Method == http.MethodGet:
		discovery, err := h.App.ContainerOpsService.Discover(r.Context())
		if err != nil {
			response.Error(w, http.StatusBadGateway, err)
			return
		}
		response.JSON(w, http.StatusOK, discovery)
	case path == "/v0/management/container-ops/import" && r.Method == http.MethodPost:
		plan, err := h.App.ContainerOpsService.ImportPlan(r.Context())
		if err != nil {
			response.Error(w, http.StatusBadGateway, err)
			return
		}
		response.JSON(w, http.StatusOK, plan)
	case path == "/v0/management/container-ops/deploy" && r.Method == http.MethodPost:
		var request model.ContainerOpsDeployRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			response.Error(w, http.StatusBadRequest, err)
			return
		}
		plan, err := h.App.ContainerOpsService.DeployPlan(r.Context(), request)
		if err != nil {
			response.Error(w, containerOpsErrorStatus(http.StatusBadRequest, err), err)
			return
		}
		response.JSON(w, http.StatusOK, plan)
	case path == "/v0/management/container-ops/backup" && r.Method == http.MethodPost:
		backup, err := h.App.ContainerOpsService.Backup(r.Context())
		if err != nil {
			response.Error(w, containerOpsErrorStatus(http.StatusBadGateway, err), err)
			return
		}
		response.JSON(w, http.StatusOK, backup)
	case path == "/v0/management/container-ops/restore" && r.Method == http.MethodPost:
		var request model.ContainerOpsRestoreRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			response.Error(w, http.StatusBadRequest, err)
			return
		}
		plan, err := h.App.ContainerOpsService.RestorePlan(r.Context(), request)
		if err != nil {
			response.Error(w, containerOpsErrorStatus(http.StatusBadGateway, err), err)
			return
		}
		response.JSON(w, http.StatusOK, plan)
	case path == "/v0/management/container-ops/rollback" && r.Method == http.MethodPost:
		var request model.ContainerOpsRollbackRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			response.Error(w, http.StatusBadRequest, err)
			return
		}
		result, err := h.App.ContainerOpsService.Rollback(r.Context(), request)
		if err != nil {
			response.Error(w, containerOpsErrorStatus(http.StatusBadGateway, err), err)
			return
		}
		response.JSON(w, http.StatusOK, result)
	case path == "/v0/management/container-ops/network-standardize" && r.Method == http.MethodPost:
		var request model.ContainerOpsNetworkStandardizeRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			response.Error(w, http.StatusBadRequest, err)
			return
		}
		result, err := h.App.ContainerOpsService.StandardizeNetwork(r.Context(), request)
		if err != nil {
			response.Error(w, containerOpsErrorStatus(http.StatusBadGateway, err), err)
			return
		}
		response.JSON(w, http.StatusOK, result)
	case path == "/v0/management/container-ops/egress-ips" && r.Method == http.MethodGet:
		result, err := h.App.ContainerOpsService.EgressIPs(r.Context())
		if err != nil {
			response.Error(w, containerOpsErrorStatus(http.StatusBadGateway, err), err)
			return
		}
		response.JSON(w, http.StatusOK, result)
	case path == "/v0/management/container-ops/source-ip/ensure" && r.Method == http.MethodPost:
		var request model.ContainerOpsSourceIPRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			response.Error(w, http.StatusBadRequest, err)
			return
		}
		result, err := h.App.ContainerOpsService.EnsureSourceIP(r.Context(), request)
		if err != nil {
			response.Error(w, containerOpsErrorStatus(http.StatusBadGateway, err), err)
			return
		}
		response.JSON(w, http.StatusOK, result)
	case path == "/v0/management/container-ops/source-ip/check" && r.Method == http.MethodPost:
		var request model.ContainerOpsSourceIPRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			response.Error(w, http.StatusBadRequest, err)
			return
		}
		result, err := h.App.ContainerOpsService.CheckSourceIP(r.Context(), request)
		if err != nil {
			response.Error(w, containerOpsErrorStatus(http.StatusBadGateway, err), err)
			return
		}
		response.JSON(w, http.StatusOK, result)
	case path == "/v0/management/container-ops/source-ip/remove" && r.Method == http.MethodPost:
		var request model.ContainerOpsSourceIPRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			response.Error(w, http.StatusBadRequest, err)
			return
		}
		result, err := h.App.ContainerOpsService.RemoveSourceIP(r.Context(), request)
		if err != nil {
			response.Error(w, containerOpsErrorStatus(http.StatusBadGateway, err), err)
			return
		}
		response.JSON(w, http.StatusOK, result)
	case path == "/v0/management/container-ops/upgrade" && r.Method == http.MethodPost:
		var request model.ContainerOpsUpgradeRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			response.Error(w, http.StatusBadRequest, err)
			return
		}
		result, err := h.App.ContainerOpsService.Upgrade(r.Context(), request)
		if err != nil {
			response.Error(w, containerOpsErrorStatus(http.StatusBadGateway, err), err)
			return
		}
		response.JSON(w, http.StatusOK, result)
	default:
		response.MethodNotAllowed(w)
	}
}

func containerOpsErrorStatus(defaultStatus int, err error) int {
	if containeropssvc.IsLifecycleBusy(err) {
		return http.StatusConflict
	}
	return defaultStatus
}
