package supply

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/app"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/middleware"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/response"
	supplysvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/supply"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/supplyclient"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

type Handler struct {
	App *app.Context
}

func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) {
	if !middleware.AuthorizePanel(w, r, h.App.AdminAuthService) {
		return
	}
	path := strings.TrimRight(r.URL.Path, "/")
	if orderID, ok := dismissUncertainOrderID(path); ok {
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		result, err := h.App.SupplyService.DismissCreateUncertain(r.Context(), orderID)
		h.writeResult(w, result, err)
		return
	}
	if orderID, ok := orderActionID(path, "/cancel"); ok {
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		result, err := h.App.SupplyService.CancelOrder(r.Context(), orderID)
		h.writeResult(w, result, err)
		return
	}
	switch path {
	case "/v0/management/supply":
		if r.Method != http.MethodGet {
			response.MethodNotAllowed(w)
			return
		}
		limit := 50
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				limit = parsed
			}
		}
		result, err := h.App.SupplyService.GetStatus(r.Context(), limit)
		h.writeResult(w, result, err)
	case "/v0/management/supply/config":
		if r.Method != http.MethodPut {
			response.MethodNotAllowed(w)
			return
		}
		var req struct {
			Config store.ManagerSupplyConfig `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, http.StatusBadRequest, err)
			return
		}
		result, err := h.App.SupplyService.UpdateConfig(r.Context(), req.Config)
		h.writeResult(w, result, err)
	case "/v0/management/supply/check":
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		result, err := h.App.SupplyService.Check(r.Context())
		h.writeResult(w, result, err)
	case "/v0/management/supply/replenish":
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		var req struct {
			Quantity int `json:"quantity"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, http.StatusBadRequest, err)
			return
		}
		result, err := h.App.SupplyService.Replenish(r.Context(), req.Quantity)
		h.writeResult(w, result, err)
	default:
		response.MethodNotAllowed(w)
	}
}

func (h *Handler) writeResult(w http.ResponseWriter, result any, err error) {
	if err == nil {
		response.JSON(w, http.StatusOK, result)
		return
	}
	status := http.StatusInternalServerError
	var upstreamErr *supplyclient.HTTPError
	switch {
	case errors.Is(err, supplysvc.ErrNotConfigured):
		status = http.StatusPreconditionFailed
	case errors.Is(err, supplysvc.ErrInvalidQuantity):
		status = http.StatusBadRequest
	case errors.Is(err, supplysvc.ErrOrderInProgress):
		status = http.StatusConflict
	case errors.Is(err, supplysvc.ErrCreateUncertain):
		status = http.StatusConflict
	case errors.Is(err, supplysvc.ErrOrderNotFound):
		status = http.StatusNotFound
	case errors.Is(err, supplysvc.ErrNotCreateUncertain):
		status = http.StatusConflict
	case errors.Is(err, supplysvc.ErrOrderNotCancellable):
		status = http.StatusConflict
	case errors.Is(err, supplysvc.ErrInsufficientBalance):
		status = http.StatusPaymentRequired
	case errors.Is(err, supplyclient.ErrReleaseUnsupported):
		status = http.StatusBadGateway
	case errors.As(err, &upstreamErr):
		switch upstreamErr.StatusCode {
		case http.StatusBadRequest:
			status = http.StatusBadRequest
		case http.StatusPaymentRequired:
			status = http.StatusPaymentRequired
		case http.StatusConflict:
			status = http.StatusConflict
		default:
			status = http.StatusBadGateway
		}
	}
	response.Error(w, status, err)
}

func dismissUncertainOrderID(path string) (string, bool) {
	const prefix = "/v0/management/supply/orders/"
	const suffix = "/dismiss-uncertain"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	orderID := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix))
	return orderID, orderID != "" && !strings.Contains(orderID, "/")
}

func orderActionID(path string, suffix string) (string, bool) {
	const prefix = "/v0/management/supply/orders/"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	orderID := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix))
	return orderID, orderID != "" && !strings.Contains(orderID, "/")
}
