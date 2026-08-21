package router

import (
	"net/http"
	"strings"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/app"
	accountactioncontroller "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/controller/accountaction"
	apikeyaliascontroller "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/controller/apikeyalias"
	automationcontroller "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/controller/automation"
	codexinspectioncontroller "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/controller/codexinspection"
	codexquotacontroller "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/controller/codexquota"
	containeropscontroller "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/controller/containerops"
	dashboardcontroller "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/controller/dashboard"
	databasecontroller "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/controller/database"
	healthcontroller "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/controller/health"
	managerconfigcontroller "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/controller/managerconfig"
	modelpricecontroller "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/controller/modelprice"
	monitoringcontroller "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/controller/monitoring"
	panelcontroller "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/controller/panel"
	proxycontroller "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/controller/proxy"
	quotacooldowncontroller "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/controller/quotacooldown"
	quotasnapshotcontroller "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/controller/quotasnapshot"
	setupcontroller "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/controller/setup"
	supplycontroller "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/controller/supply"
	systemcontroller "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/controller/system"
	usagecontroller "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/controller/usage"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/http/middleware"
	proxysvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/proxy"
)

func New(appCtx *app.Context) http.Handler {
	healthHandler := &healthcontroller.Handler{ServiceID: appCtx.ServiceID}
	systemHandler := &systemcontroller.Handler{App: appCtx}
	setupHandler := &setupcontroller.Handler{App: appCtx}
	managerConfigHandler := &managerconfigcontroller.Handler{App: appCtx}
	usageHandler := &usagecontroller.Handler{App: appCtx}
	modelPriceHandler := &modelpricecontroller.Handler{App: appCtx}
	apiKeyAliasHandler := &apikeyaliascontroller.Handler{App: appCtx}
	accountActionHandler := &accountactioncontroller.Handler{App: appCtx}
	automationHandler := automationcontroller.New(appCtx)
	quotaCooldownHandler := &quotacooldowncontroller.Handler{App: appCtx}
	codexInspectionHandler := &codexinspectioncontroller.Handler{App: appCtx}
	codexQuotaHandler := &codexquotacontroller.Handler{App: appCtx}
	containerOpsHandler := &containeropscontroller.Handler{App: appCtx}
	dashboardHandler := &dashboardcontroller.Handler{App: appCtx}
	databaseHandler := &databasecontroller.Handler{App: appCtx}
	monitoringHandler := &monitoringcontroller.Handler{App: appCtx}
	quotaSnapshotHandler := &quotasnapshotcontroller.Handler{App: appCtx}
	proxyHandler := &proxycontroller.Handler{App: appCtx}
	panelHandler := &panelcontroller.Handler{App: appCtx}
	supplyHandler := &supplycontroller.Handler{App: appCtx}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", middleware.WithCORS(appCtx.Config, healthHandler.Health))
	mux.HandleFunc("/status", middleware.WithCORS(appCtx.Config, systemHandler.Status))
	mux.HandleFunc("/usage-service/info", middleware.WithCORS(appCtx.Config, systemHandler.Info))
	mux.HandleFunc("/usage-service/config", middleware.WithCORS(appCtx.Config, managerConfigHandler.Handle))
	mux.HandleFunc("/usage-service/account-processing-policy", middleware.WithCORS(appCtx.Config, automationHandler.Handle))
	mux.HandleFunc("/usage-service/quota-cooldowns", middleware.WithCORS(appCtx.Config, quotaCooldownHandler.Handle))
	mux.HandleFunc("/setup", middleware.WithCORS(appCtx.Config, setupHandler.Setup))
	mux.HandleFunc("/management.html", panelHandler.ManagementHTML)
	mux.HandleFunc("/", rootHandler(appCtx, usageHandler, modelPriceHandler, apiKeyAliasHandler, accountActionHandler, codexInspectionHandler, codexQuotaHandler, containerOpsHandler, dashboardHandler, databaseHandler, monitoringHandler, quotaSnapshotHandler, supplyHandler, proxyHandler))

	return middleware.Recovery(middleware.RequestLogger(middleware.CompressLargeResponses(mux)))
}

func rootHandler(
	appCtx *app.Context,
	usageHandler *usagecontroller.Handler,
	modelPriceHandler *modelpricecontroller.Handler,
	apiKeyAliasHandler *apikeyaliascontroller.Handler,
	accountActionHandler *accountactioncontroller.Handler,
	codexInspectionHandler *codexinspectioncontroller.Handler,
	codexQuotaHandler *codexquotacontroller.Handler,
	containerOpsHandler *containeropscontroller.Handler,
	dashboardHandler *dashboardcontroller.Handler,
	databaseHandler *databasecontroller.Handler,
	monitoringHandler *monitoringcontroller.Handler,
	quotaSnapshotHandler *quotasnapshotcontroller.Handler,
	supplyHandler *supplycontroller.Handler,
	proxyHandler *proxycontroller.Handler,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Older saved panel sessions and manually pasted panel URLs can retain
		// `/management.html` as the API base. Accept that legacy request shape so
		// an already-open browser recovers immediately, while the frontend
		// normalizer repairs the persisted base on its next session restore.
		const legacyPanelManagementPrefix = "/management.html/v0/management/"
		if strings.HasPrefix(r.URL.Path, legacyPanelManagementPrefix) {
			r.URL.Path = strings.TrimPrefix(r.URL.Path, "/management.html")
			if r.URL.RawPath != "" {
				r.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, "/management.html")
			}
		}
		// Management responses are live operational snapshots. The panel polls
		// these stable URLs repeatedly, so allowing a browser or intermediary to
		// reuse an older JSON response can make the account and supply pages show
		// different generations of the same pool after imports, cleanup, or a
		// manager rollout. Keep caching disabled for the complete management API;
		// individual services already provide their own short-lived in-process
		// caches where amortization is required.
		if strings.HasPrefix(r.URL.Path, "/v0/management/") {
			w.Header().Set("Cache-Control", "no-store, max-age=0")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
		}
		if r.Method == http.MethodOptions {
			middleware.WriteCORS(appCtx.Config, w, r)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v0/management/model-prices") {
			middleware.WithCORS(appCtx.Config, modelPriceHandler.Handle)(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v0/management/api-key-aliases") {
			middleware.WithCORS(appCtx.Config, apiKeyAliasHandler.Handle)(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v0/management/account-action-candidates") {
			middleware.WithCORS(appCtx.Config, accountActionHandler.Handle)(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v0/management/codex-inspection") {
			middleware.WithCORS(appCtx.Config, codexInspectionHandler.Handle)(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v0/management/cpamp/codex-quota/") {
			middleware.WithCORS(appCtx.Config, codexQuotaHandler.Handle)(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v0/management/container-ops") {
			middleware.WithCORS(appCtx.Config, containerOpsHandler.Handle)(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v0/management/dashboard/") {
			middleware.WithCORS(appCtx.Config, dashboardHandler.Handle)(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v0/management/database") {
			middleware.WithCORS(appCtx.Config, databaseHandler.Handle)(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v0/management/monitoring/") {
			middleware.WithCORS(appCtx.Config, monitoringHandler.Handle)(w, r)
			return
		}
		cleanQuotaSnapshotPath := strings.TrimRight(r.URL.Path, "/")
		if cleanQuotaSnapshotPath == "/v0/management/quota-snapshots" ||
			cleanQuotaSnapshotPath == "/v0/management/quota-snapshots/query" {
			middleware.WithCORS(appCtx.Config, quotaSnapshotHandler.Handle)(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v0/management/supply") {
			middleware.WithCORS(appCtx.Config, supplyHandler.Handle)(w, r)
			return
		}
		cleanUsagePath := strings.TrimRight(r.URL.Path, "/")
		if cleanUsagePath == "/v0/management/usage" || strings.HasPrefix(cleanUsagePath, "/v0/management/usage/") {
			middleware.WithCORS(appCtx.Config, usageHandler.Handle)(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v0/management/") {
			middleware.WithCORS(appCtx.Config, proxyHandler.Management)(w, r)
			return
		}
		if r.URL.Path == "/v1/models" || r.URL.Path == "/v1/models/" ||
			r.URL.Path == "/models" || r.URL.Path == "/models/" {
			middleware.WithCORS(appCtx.Config, proxyHandler.ModelList)(w, r)
			return
		}
		if proxysvc.IsCPAPluginResourcePath(r.URL.Path) {
			middleware.WithCORS(appCtx.Config, proxyHandler.CPAResource)(w, r)
			return
		}
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/management.html", http.StatusTemporaryRedirect)
			return
		}
		http.NotFound(w, r)
	}
}
