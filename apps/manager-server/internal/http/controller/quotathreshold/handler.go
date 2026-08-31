package quotathreshold

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/app"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/middleware"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/response"
	quotasvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/quotathreshold"
)

type Handler struct{ App *app.Context }

func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) {
	if !middleware.AuthorizePanel(w, r, h.App.AdminAuthService) {
		return
	}
	path := strings.TrimRight(strings.TrimSpace(r.URL.Path), "/")
	if path == "/v0/management/quota-threshold-rules" {
		switch r.Method {
		case http.MethodGet:
			result, err := h.App.QuotaThresholdService.List(r.Context())
			if err != nil {
				response.Error(w, http.StatusInternalServerError, err)
				return
			}
			response.JSON(w, http.StatusOK, result)
		case http.MethodPut:
			var req quotasvc.UpsertRequest
			defer r.Body.Close()
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				response.Error(w, http.StatusBadRequest, err)
				return
			}
			result, err := h.App.QuotaThresholdService.Upsert(r.Context(), req.Rules)
			if err != nil {
				response.Error(w, http.StatusBadRequest, err)
				return
			}
			response.JSON(w, http.StatusOK, result)
		default:
			response.MethodNotAllowed(w)
		}
		return
	}
	prefix := "/v0/management/quota-threshold-rules/"
	if !strings.HasPrefix(path, prefix) {
		response.MethodNotAllowed(w)
		return
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(path, prefix), 10, 64)
	if err != nil || id <= 0 {
		response.Error(w, http.StatusBadRequest, errors.New("rule id is required"))
		return
	}
	if r.Method != http.MethodDelete {
		response.MethodNotAllowed(w)
		return
	}
	if err := h.App.QuotaThresholdService.Delete(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			response.Error(w, http.StatusNotFound, err)
			return
		}
		response.Error(w, http.StatusInternalServerError, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
}
