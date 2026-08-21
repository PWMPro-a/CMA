package supply

import (
	"encoding/json"
	"errors"
	"io"
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
	if platformID, ok := refreshNvtokensPlatformID(path); ok {
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		result, err := h.App.SupplyService.RefreshNvtokensSession(r.Context(), platformID)
		h.writeResult(w, result, err)
		return
	}
	if taskID, ok := cancelPurchaseTaskID(path); ok {
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		result, err := h.App.SupplyService.CancelPurchaseTask(r.Context(), taskID)
		h.writeResult(w, result, err)
		return
	}
	if orderID, ok := dismissUncertainOrderID(path); ok {
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		result, err := h.App.SupplyService.DismissCreateUncertain(r.Context(), orderID)
		h.writeResult(w, result, err)
		return
	}
	if recoveryID, ok := retryRecoveryImportID(path); ok {
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		result, err := h.App.SupplyService.RetryRecoveryImport(r.Context(), recoveryID)
		h.writeResult(w, result, err)
		return
	}
	if recoveryID, ok := claimRecoveryID(path); ok {
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		autoClaim := true
		result, err := h.App.SupplyService.SyncRecoveries(r.Context(), supplysvc.RecoverySyncRequest{
			Force:      true,
			AutoClaim:  &autoClaim,
			Limit:      1,
			RecoveryID: recoveryID,
		})
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
		result, err := h.App.SupplyService.GetDashboardStatus(r.Context(), limit)
		h.writeResult(w, result, err)
	case "/v0/management/supply/account-pool":
		if r.Method != http.MethodGet {
			response.MethodNotAllowed(w)
			return
		}
		result, err := h.App.SupplyService.GetAccountPoolSummary(r.Context())
		h.writeResult(w, result, err)
	case "/v0/management/supply/active":
		if r.Method != http.MethodGet {
			response.MethodNotAllowed(w)
			return
		}
		result, err := h.App.SupplyService.GetActiveOrderStatus(r.Context())
		h.writeResult(w, result, err)
	case "/v0/management/supply/tasks":
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
		result, err := h.App.SupplyService.ListPurchaseTasks(r.Context(), limit)
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
	case "/v0/management/supply/platform-catalog":
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		var req struct {
			Platform store.ManagerSupplyPlatformConfig `json:"platform"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, http.StatusBadRequest, err)
			return
		}
		result, err := h.App.SupplyService.GetPlatformProductCatalog(r.Context(), req.Platform)
		h.writeResult(w, result, err)
	case "/v0/management/supply/quote":
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		var req struct {
			Quantity   int    `json:"quantity"`
			SupplierID string `json:"supplierId"`
			Product    string `json:"product"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, http.StatusBadRequest, err)
			return
		}
		result, err := h.App.SupplyService.QuotePlatformProduct(r.Context(), req.Quantity, req.SupplierID, req.Product)
		h.writeResult(w, result, err)
	case "/v0/management/supply/recoveries":
		if r.Method != http.MethodGet {
			response.MethodNotAllowed(w)
			return
		}
		limit := 100
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				limit = parsed
			}
		}
		result, err := h.App.SupplyService.ListRecoveries(r.Context(), limit, r.URL.Query().Get("status"))
		h.writeResult(w, result, err)
	case "/v0/management/supply/accounts":
		if r.Method != http.MethodGet {
			response.MethodNotAllowed(w)
			return
		}
		req := supplysvc.SupplyAccountsRequest{
			Status: r.URL.Query().Get("status"),
		}
		if raw := strings.TrimSpace(r.URL.Query().Get("fromMs")); raw != "" {
			if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
				req.FromMS = parsed
			}
		}
		if raw := strings.TrimSpace(r.URL.Query().Get("toMs")); raw != "" {
			if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
				req.ToMS = parsed
			}
		}
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				req.Limit = parsed
			}
		}
		result, err := h.App.SupplyService.ListAccounts(r.Context(), req)
		h.writeResult(w, result, err)
	case "/v0/management/supply/account-leases":
		if r.Method != http.MethodGet {
			response.MethodNotAllowed(w)
			return
		}
		result, err := h.App.SupplyService.ListAccountLeases(r.Context())
		h.writeResult(w, result, err)
	case "/v0/management/supply/reports":
		if r.Method != http.MethodGet {
			response.MethodNotAllowed(w)
			return
		}
		req := supplysvc.ReportRequest{}
		if raw := strings.TrimSpace(r.URL.Query().Get("fromMs")); raw != "" {
			if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
				req.FromMS = parsed
			}
		}
		if raw := strings.TrimSpace(r.URL.Query().Get("toMs")); raw != "" {
			if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
				req.ToMS = parsed
			}
		}
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				req.Limit = parsed
			}
		}
		result, err := h.App.SupplyService.Report(r.Context(), req)
		h.writeResult(w, result, err)
	case "/v0/management/supply/recoveries/sync":
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		var req supplysvc.RecoverySyncRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			response.Error(w, http.StatusBadRequest, err)
			return
		}
		req.Force = true
		result, err := h.App.SupplyService.SyncRecoveries(r.Context(), req)
		h.writeResult(w, result, err)
	case "/v0/management/supply/replenish":
		if r.Method != http.MethodPost {
			response.MethodNotAllowed(w)
			return
		}
		var req struct {
			Quantity   int    `json:"quantity"`
			SupplierID string `json:"supplierId"`
			Product    string `json:"product"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, http.StatusBadRequest, err)
			return
		}
		result, err := h.App.SupplyService.ReplenishProduct(r.Context(), req.Quantity, req.SupplierID, req.Product)
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
	case errors.Is(err, supplysvc.ErrPurchaseTaskNotFound):
		status = http.StatusNotFound
	case errors.Is(err, supplysvc.ErrNotCreateUncertain):
		status = http.StatusConflict
	case errors.Is(err, supplysvc.ErrRecoveryImportNotReady):
		status = http.StatusConflict
	case errors.Is(err, supplysvc.ErrInsufficientBalance):
		status = http.StatusPaymentRequired
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

func cancelPurchaseTaskID(path string) (string, bool) {
	const prefix = "/v0/management/supply/tasks/"
	const suffix = "/cancel"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	taskID := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix))
	return taskID, taskID != "" && !strings.Contains(taskID, "/")
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

func claimRecoveryID(path string) (string, bool) {
	const prefix = "/v0/management/supply/recoveries/"
	const suffix = "/claim"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	recoveryID := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix))
	return recoveryID, recoveryID != "" && !strings.Contains(recoveryID, "/")
}

func retryRecoveryImportID(path string) (string, bool) {
	const prefix = "/v0/management/supply/recoveries/"
	const suffix = "/retry-import"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	recoveryID := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix))
	return recoveryID, recoveryID != "" && !strings.Contains(recoveryID, "/")
}

func refreshNvtokensPlatformID(path string) (string, bool) {
	const prefix = "/v0/management/supply/platforms/"
	const suffix = "/refresh-session"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	platformID := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix))
	return platformID, platformID != "" && !strings.Contains(platformID, "/")
}
