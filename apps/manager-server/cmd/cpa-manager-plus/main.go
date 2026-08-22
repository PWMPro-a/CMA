package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	httppprof "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/buildinfo"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/collector"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/command/adminreset"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/config"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/httpapi"
	sqliterepo "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/sqlite"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/security"
	bootstrapservice "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/bootstrap"
	collectorservice "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/collector"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/worker"
)

func main() {
	log.Printf("CPA Manager Plus version=%s commit=%s builtAt=%s", buildinfo.Version, buildinfo.Commit, buildinfo.BuildDate)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "reset-admin-key", "reset-admin-password":
			if err := adminreset.Run(context.Background(), os.Args[2:], os.Stdout, os.Stderr); err != nil {
				log.Printf("reset admin key: %v", err)
				os.Exit(1)
			}
			return
		}
	}
	runServer()
}

func runServer() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	dataKey, dataKeyCreated, err := security.LoadOrCreateDataKey(cfg.DataKey, cfg.DataKeyPath)
	if err != nil {
		log.Fatalf("load data key: %v", err)
	}
	protector, err := security.NewProtector(dataKey)
	if err != nil {
		log.Fatalf("initialize secret protector: %v", err)
	}
	db, err := store.OpenConfig(cfg, protector)
	if err != nil {
		log.Fatalf("open %s database: %v", cfg.DBDriver, err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Printf("close %s database: %v", cfg.DBDriver, err)
		}
	}()

	bootstrapResult, err := bootstrapservice.Run(context.Background(), cfg, db, dataKeyCreated)
	if err != nil {
		log.Fatalf("bootstrap manager server: %v", err)
	}
	if bootstrapResult.GeneratedAdminKey != "" {
		log.Printf("CPA Manager Plus admin key generated: %s", bootstrapResult.GeneratedAdminKey)
	} else {
		log.Printf("CPA Manager Plus admin credential initialized")
	}
	if bootstrapResult.DataKeyCreated {
		log.Printf("CPA Manager Plus data key created at %s", cfg.DataKeyPath)
	}
	if bootstrapResult.MigratedLegacy {
		log.Printf("CPA Manager Plus legacy data migrated")
	}

	manager := collector.NewManager(cfg, db)
	collectorService := collectorservice.New(manager)
	collectorWorker := worker.NewCollectorWorker(cfg, db, collectorService)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var walMaintenance *sqliterepo.WALMaintenance
	if db.Driver() == "sqlite" {
		walMaintenance, err = sqliterepo.NewWALMaintenance(cfg.DBPath)
		if err != nil {
			log.Printf("configure SQLite WAL maintenance: %v", err)
		} else {
			walMaintenance.Start(ctx)
			defer func() {
				if err := walMaintenance.Close(); err != nil {
					log.Printf("close SQLite WAL maintenance: %v", err)
				}
			}()
		}
	}

	serverApp := httpapi.New(cfg, db, manager)
	serverApp.AppContext().DatabaseMaintenance = walMaintenance
	recoveryCtx, cancelRecovery := context.WithTimeout(context.Background(), 10*time.Second)
	if err := serverApp.AppContext().CodexInspectionService.Recover(recoveryCtx); err != nil {
		log.Printf("recover codex inspection runs: %v", err)
	}
	cancelRecovery()
	if err := serverApp.AppContext().UsageService.StartImportSessionCleanup(ctx); err != nil {
		log.Fatalf("start usage import session cleanup: %v", err)
	}
	serverApp.AppContext().SupplyService.SetInspectionSnapshotRefresher(
		ctx,
		serverApp.AppContext().CodexInspectionService.RefreshSupplySnapshot,
	)
	warmCtx, warmCancel := context.WithTimeout(ctx, 10*time.Second)
	if err := serverApp.AppContext().SupplyService.WarmSmartUsage(warmCtx); err != nil {
		log.Printf("smart supply usage warm-up: %v", err)
	}
	warmCancel()
	automationSettingsService := serverApp.AppContext().AccountProcessingPolicyService
	runtimeSettings := automationSettingsService.RuntimeSettings(ctx)
	rateLimitAutoDisableWorker := worker.NewRateLimitAutoDisableWorkerWithMutationCoordinator(
		db,
		serverApp.AppContext().AuthFileMutationCoordinator,
		collector.RuntimeConfig{
			CPAUpstreamURL: cfg.CPAUpstreamURL,
			ManagementKey:  cfg.ManagementKey,
		},
	)
	rateLimitAutoDisableWorker.SetAutoResetter(serverApp.AppContext().CodexQuotaService)
	accountActionWorker := worker.NewAccountActionCandidateWorkerWithMutationCoordinator(
		db,
		serverApp.AppContext().AuthFileMutationCoordinator,
		runtimeSettings.AccountActionsAutoDisable,
	)
	accountHistoryRollupWorker := worker.NewAccountHistoryRollupWorker(db)
	var dashboardHourlyRollupWorker *worker.DashboardHourlyRollupWorker
	if cfg.DashboardHourlyRollupEnabled {
		dashboardHourlyRollupWorker = worker.NewDashboardHourlyRollupWorker(db)
	}
	usageDerivedRollupWorker := worker.NewUsagePricingRollupWorker(db)
	serverApp.AppContext().ModelPriceService.SetPricesChangedNotifier(usageDerivedRollupWorker.Wake)
	var usageHourlyAggregateWorker *worker.UsageHourlyAggregateWorker
	if cfg.DashboardHourlyRollupEnabled {
		usageHourlyAggregateWorker = worker.NewUsageHourlyAggregateWorker(db)
	}
	var startRollupsOnce sync.Once
	startRollups := func() {
		startRollupsOnce.Do(func() {
			log.Printf("usage rollup workers starting after cache-accounting maintenance")
			accountHistoryRollupWorker.Start(ctx)
			usageDerivedRollupWorker.Start(ctx)
			if usageHourlyAggregateWorker != nil {
				usageHourlyAggregateWorker.Start(ctx)
			}
			if dashboardHourlyRollupWorker != nil {
				dashboardHourlyRollupWorker.Start(ctx)
			}
		})
	}
	serverApp.AppContext().UsageService.SetEventsInsertedNotifier(func() {
		accountHistoryRollupWorker.Wake()
		usageDerivedRollupWorker.Wake()
		if usageHourlyAggregateWorker != nil {
			usageHourlyAggregateWorker.Wake()
		}
	})
	automationRuntime := worker.NewAutomationRuntime(
		automationSettingsService,
		manager,
		rateLimitAutoDisableWorker,
		accountActionWorker,
	)
	serverApp.AppContext().AutomationRuntimeService = automationRuntime
	automationRuntime.Start(ctx)
	manager.SetUsageEventHandler(worker.NewUsageEventFanout(
		automationRuntime.UsageEventHandler(),
		accountHistoryRollupWorker,
		usageDerivedRollupWorker,
		usageHourlyAggregateWorker,
		dashboardHourlyRollupWorker,
		serverApp.AppContext().SupplyService,
	))

	collectorWorker.Start(ctx)
	supplyReplenishmentWorker := worker.NewSupplyReplenishmentWorker(serverApp.AppContext().SupplyService)
	supplyReplenishmentWorker.Start(ctx)
	supplyPurchaseTaskWorker := worker.NewSupplyPurchaseTaskWorker(serverApp.AppContext().SupplyService)
	supplyPurchaseTaskWorker.Start(ctx)
	supplyLowPriceReserveWorker := worker.NewSupplyLowPriceReserveWorker(serverApp.AppContext().SupplyService)
	supplyLowPriceReserveWorker.Start(ctx)

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           serverApp.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	pprofServer, err := newPprofServer(cfg.PprofAddr)
	if err != nil {
		log.Fatalf("configure pprof: %v", err)
	}
	if pprofServer != nil {
		go func() {
			log.Printf("cpa-manager-plus pprof listening on %s", pprofServer.Addr)
			if err := pprofServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("pprof server: %v", err)
			}
		}()
	}

	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		log.Fatalf("http listen: %v", err)
	}
	codexInspectionWorker := worker.NewCodexInspectionWorker(serverApp.AppContext().Store, serverApp.AppContext().CodexInspectionService)
	codexInspectionWorker.Start(ctx)
	serverResult := make(chan error, 1)
	go func() {
		log.Printf("cpa-manager-plus listening on %s", listener.Addr())
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serverResult <- err
	}()

	usageCacheAccountingMigrationWorker := worker.NewUsageCacheAccountingMigrationWorker(db, func() {
		go func() {
			log.Printf("usage response metadata backfill starting before rollup catch-up")
			runUsageResponseMetadataBackfill(ctx, db)
			startRollups()
			accountHistoryRollupWorker.Wake()
			usageDerivedRollupWorker.Wake()
			if usageHourlyAggregateWorker != nil {
				usageHourlyAggregateWorker.Wake()
			}
			if dashboardHourlyRollupWorker != nil {
				dashboardHourlyRollupWorker.Wake()
			}
			log.Printf("usage routing diagnostics backfill starting")
			runUsageRoutingDiagnosticsBackfill(ctx, db)
		}()
	})
	usageCacheAccountingMigrationWorker.Start(ctx)

	select {
	case <-ctx.Done():
	case err := <-serverResult:
		if err != nil {
			log.Printf("http server stopped unexpectedly: %v", err)
		}
		// Ensure every worker using the shared process context observes the same
		// shutdown even when the HTTP listener, rather than a signal, initiated it.
		stop()
	}
	stopCodexInspectionWorker(codexInspectionWorker, 20*time.Second)
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	collectorWorker.Stop(context.Background())
	if pprofServer != nil {
		if err := pprofServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown pprof: %v", err)
		}
	}
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

type codexInspectionStopper interface {
	StopAndWait(context.Context) error
}

func stopCodexInspectionWorker(stopper codexInspectionStopper, timeout time.Duration) {
	if stopper == nil {
		return
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), timeout)
	err := stopper.StopAndWait(shutdownCtx)
	cancelShutdown()
	if err == nil {
		return
	}
	// Shutdown must remain bounded. The worker has already fenced new starts and
	// cancelled owned work; if it still cannot finish within the grace period,
	// leave an explicit error for the supervisor and let startup recovery reclaim
	// the expired lease instead of hanging the process indefinitely.
	log.Printf("shutdown codex inspection worker: %v; continuing bounded process shutdown", err)
}

func newPprofServer(addr string) (*http.Server, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid address %q: %w", addr, err)
	}
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("pprof address must use a loopback host: %q", addr)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", httppprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", httppprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", httppprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", httppprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", httppprof.Trace)
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}, nil
}

func runUsageResponseMetadataBackfill(ctx context.Context, db *store.Store) {
	const batchLimit = 100
	const maxStartupBatches = 20
	const batchTimeout = 3 * time.Second
	const batchDelay = 2 * time.Second
	total := 0
	for batch := 0; batch < maxStartupBatches; batch++ {
		batchCtx, cancel := context.WithTimeout(ctx, batchTimeout)
		updated, err := db.BackfillUsageResponseMetadata(batchCtx, batchLimit)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				if errors.Is(err, context.DeadlineExceeded) {
					log.Printf("usage response metadata backfill paused after timeout: updated=%d batch_limit=%d timeout=%s", total, batchLimit, batchTimeout)
					return
				}
				log.Printf("usage response metadata backfill: %v", err)
			}
			return
		}
		if updated == 0 {
			if total > 0 {
				log.Printf("usage response metadata backfill completed: updated=%d", total)
			} else {
				log.Printf("usage response metadata backfill no pending rows")
			}
			return
		}
		total += updated
		select {
		case <-ctx.Done():
			return
		case <-time.After(batchDelay):
		}
	}
	log.Printf("usage response metadata backfill paused after startup slice: updated=%d batch_limit=%d max_batches=%d", total, batchLimit, maxStartupBatches)
}

func runUsageRoutingDiagnosticsBackfill(ctx context.Context, db *store.Store) {
	const batchLimit = 250
	const maxStartupBatches = 12
	const batchTimeout = 5 * time.Second
	const batchDelay = 250 * time.Millisecond
	total := 0
	for batch := 0; batch < maxStartupBatches; batch++ {
		batchCtx, cancel := context.WithTimeout(ctx, batchTimeout)
		updated, err := db.BackfillUsageRoutingDiagnostics(batchCtx, batchLimit)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, context.DeadlineExceeded) {
				log.Printf("usage routing diagnostics backfill paused after timeout: updated=%d batch_limit=%d timeout=%s", total, batchLimit, batchTimeout)
				return
			}
			log.Printf("usage routing diagnostics backfill: %v", err)
			return
		}
		if updated == 0 {
			log.Printf("usage routing diagnostics backfill completed: updated=%d", total)
			return
		}
		total += updated
		select {
		case <-ctx.Done():
			return
		case <-time.After(batchDelay):
		}
	}
	log.Printf("usage routing diagnostics backfill paused after startup slice: updated=%d batch_limit=%d max_batches=%d", total, batchLimit, maxStartupBatches)
}
