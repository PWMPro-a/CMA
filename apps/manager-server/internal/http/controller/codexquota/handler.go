package codexquota

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/app"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/middleware"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/response"
	codexquotasvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/codexquota"
)

const resetCreditPath = "/v0/management/cpamp/codex-quota/reset-credit"
const resetCreditInspectionPath = "/v0/management/cpamp/codex-quota/reset-credit-inspection"
const resetCountPath = "/v0/management/cpamp/codex-quota/reset-counts"

type Handler struct {
	App *app.Context
}

func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) {
	if !middleware.AuthorizePanel(w, r, h.App.AdminAuthService) {
		return
	}
	path := strings.TrimRight(strings.TrimSpace(r.URL.Path), "/")
	switch {
	case path == resetCountPath:
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		items, err := h.App.CodexQuotaService.ListResetCounts(r.Context())
		if err != nil {
			response.Error(w, errorStatus(err), err)
			return
		}
		response.JSON(w, http.StatusOK, map[string]any{"items": items})
	case path == resetCreditInspectionPath:
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		items, err := h.App.CodexQuotaService.InspectResetCredits(r.Context())
		if err != nil {
			response.Error(w, errorStatus(err), err)
			return
		}
		response.JSON(w, http.StatusOK, map[string]any{"items": items})
	case path == resetCreditInspectionPath+"/batch-reset":
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		var request codexquotasvc.BatchResetRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			response.Error(w, http.StatusBadRequest, err)
			return
		}
		outcomes, err := h.App.CodexQuotaService.BatchResetCredits(r.Context(), request)
		if err != nil {
			response.Error(w, errorStatus(err), err)
			return
		}
		response.JSON(w, http.StatusOK, map[string]any{"items": outcomes})
	case path == resetCreditPath:
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		var request codexquotasvc.ResetRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			response.Error(w, http.StatusBadRequest, err)
			return
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err == nil {
				err = errors.New("request body contains multiple JSON values")
			}
			response.Error(w, http.StatusBadRequest, err)
			return
		}
		result, err := h.App.CodexQuotaService.ResetCredit(r.Context(), request)
		if err != nil {
			response.Error(w, errorStatus(err), err)
			return
		}
		response.JSON(w, http.StatusOK, result)
	case strings.HasPrefix(path, resetCreditPath+"/operations/"):
		if r.Method != http.MethodGet {
			response.MethodNotAllowed(w)
			return
		}
		operationID := strings.TrimPrefix(path, resetCreditPath+"/operations/")
		result, err := h.App.CodexQuotaService.GetOperation(r.Context(), operationID)
		if err != nil {
			response.Error(w, errorStatus(err), err)
			return
		}
		response.JSON(w, http.StatusOK, result)
	default:
		response.MethodNotAllowed(w)
	}
}

func errorStatus(err error) int {
	switch {
	case errors.Is(err, codexquotasvc.ErrInvalidRequest):
		return http.StatusBadRequest
	case errors.Is(err, codexquotasvc.ErrAuthNotFound),
		errors.Is(err, codexquotasvc.ErrOperationNotFound):
		return http.StatusNotFound
	case errors.Is(err, codexquotasvc.ErrOperationConflict),
		errors.Is(err, codexquotasvc.ErrAccountBusy):
		return http.StatusConflict
	case errors.Is(err, codexquotasvc.ErrNotConfigured):
		return http.StatusPreconditionFailed
	default:
		return http.StatusBadGateway
	}
}
