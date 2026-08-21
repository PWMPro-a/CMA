package database

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/app"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/middleware"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/response"
	databasesvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/database"
)

type Handler struct {
	App *app.Context
}

type connectionRequest struct {
	Target databasesvc.ConnectionConfig `json:"target"`
}

type startMigrationRequest struct {
	Target             databasesvc.ConnectionConfig `json:"target"`
	RequireEmptyTarget bool                         `json:"requireEmptyTarget"`
}

type switchRequest struct {
	MigrationID string                       `json:"migrationId"`
	Target      databasesvc.ConnectionConfig `json:"target"`
}

func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) {
	if !middleware.AuthorizePanel(w, r, h.App.AdminAuthService) {
		return
	}
	path := strings.TrimRight(r.URL.Path, "/")
	switch {
	case path == "/v0/management/database":
		if r.Method != http.MethodGet {
			response.MethodNotAllowed(w)
			return
		}
		response.JSON(w, http.StatusOK, h.App.DatabaseService.ManagementStatus(r.Context()))
	case path == "/v0/management/database/test":
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		var req connectionRequest
		if !decode(w, r, &req) {
			return
		}
		result, err := h.App.DatabaseService.Probe(r.Context(), req.Target)
		h.writeResult(w, result, err)
	case path == "/v0/management/database/migrations/plan":
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		var req connectionRequest
		if !decode(w, r, &req) {
			return
		}
		result, err := h.App.DatabaseService.Plan(r.Context(), req.Target)
		h.writeResult(w, result, err)
	case path == "/v0/management/database/migrations":
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		var req startMigrationRequest
		if !decode(w, r, &req) {
			return
		}
		result, err := h.App.DatabaseService.StartMigration(req.Target, req.RequireEmptyTarget)
		if err != nil {
			response.Error(w, http.StatusConflict, err)
			return
		}
		response.JSON(w, http.StatusAccepted, result)
	case path == "/v0/management/database/switch":
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		var req switchRequest
		if !decode(w, r, &req) {
			return
		}
		result, err := h.App.DatabaseService.PrepareSwitch(req.MigrationID, req.Target)
		h.writeResult(w, result, err)
	default:
		h.handleMigrationPath(w, r, path)
	}
}

func (h *Handler) handleMigrationPath(w http.ResponseWriter, r *http.Request, path string) {
	const prefix = "/v0/management/database/migrations/"
	if !strings.HasPrefix(path, prefix) {
		http.NotFound(w, r)
		return
	}
	remainder := strings.TrimPrefix(path, prefix)
	if strings.HasSuffix(remainder, "/cancel") {
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		id := strings.TrimSuffix(remainder, "/cancel")
		job, err := h.App.DatabaseService.CancelMigration(id)
		if err != nil {
			response.Error(w, http.StatusNotFound, err)
			return
		}
		response.JSON(w, http.StatusOK, job)
		return
	}
	if r.Method != http.MethodGet {
		response.MethodNotAllowed(w)
		return
	}
	job, ok := h.App.DatabaseService.GetMigration(remainder)
	if !ok {
		response.Error(w, http.StatusNotFound, errors.New("migration not found"))
		return
	}
	response.JSON(w, http.StatusOK, job)
}

func (h *Handler) writeResult(w http.ResponseWriter, result any, err error) {
	if err != nil {
		response.Error(w, http.StatusBadRequest, err)
		return
	}
	response.JSON(w, http.StatusOK, result)
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		response.Error(w, http.StatusBadRequest, err)
		return false
	}
	return true
}
