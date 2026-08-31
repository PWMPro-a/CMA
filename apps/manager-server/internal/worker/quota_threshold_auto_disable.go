package worker

import (
	"context"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpaauthfiles"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

type QuotaThresholdAutoDisableWorker struct {
	store         *store.Store
	cpaURL        string
	managementKey string
	mutations     *cpaauthfiles.MutationCoordinator
	client        *cpaauthfiles.Client
	mu            sync.Mutex
	cancel        context.CancelFunc
	done          chan struct{}
	started       bool
}

func NewQuotaThresholdAutoDisableWorker(st *store.Store, cpaURL, managementKey string, mutations *cpaauthfiles.MutationCoordinator) *QuotaThresholdAutoDisableWorker {
	return &QuotaThresholdAutoDisableWorker{store: st, cpaURL: strings.TrimSpace(cpaURL), managementKey: strings.TrimSpace(managementKey), mutations: mutations, client: cpaauthfiles.New(nil, 20*time.Second)}
}
func (w *QuotaThresholdAutoDisableWorker) Start(ctx context.Context) {
	if w == nil || w.store == nil {
		return
	}
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return
	}
	workerCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.done = make(chan struct{})
	w.started = true
	done := w.done
	w.mu.Unlock()
	go func() { defer close(done); w.run(workerCtx) }()
}
func (w *QuotaThresholdAutoDisableWorker) StopAndWait(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	cancel, done := w.cancel, w.done
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (w *QuotaThresholdAutoDisableWorker) run(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}
func (w *QuotaThresholdAutoDisableWorker) tick(ctx context.Context) {
	rules, err := w.store.QuotaThresholdRules.List(ctx)
	if err != nil {
		log.Printf("quota threshold rules: %v", err)
		return
	}
	active := make([]model.QuotaThresholdRule, 0, len(rules))
	for _, rule := range rules {
		if rule.Enabled {
			active = append(active, rule)
		}
	}
	if len(active) == 0 {
		return
	}
	run, found, err := w.store.GetLatestCompletedCodexInspectionRun(ctx)
	if err != nil {
		log.Printf("quota threshold latest inspection: %v", err)
		return
	}
	if !found {
		return
	}
	results, err := w.store.ListCodexInspectionResults(ctx, run.ID)
	if err != nil {
		log.Printf("quota threshold inspection results: %v", err)
		return
	}
	for _, rule := range active {
		w.processRule(ctx, rule, run, results)
	}
}
func (w *QuotaThresholdAutoDisableWorker) processRule(ctx context.Context, rule model.QuotaThresholdRule, run model.CodexInspectionRun, results []model.CodexInspectionResult) {
	result, ok := findResult(rule, results)
	if !ok || result.UsedPercent == nil {
		return
	}
	remaining := math.Max(0, math.Min(100, 100-*result.UsedPercent))
	disabled := result.Disabled
	if err := w.store.UpdateQuotaThresholdObservation(ctx, rule.ID, &remaining, disabled, result.CreatedAtMS, 0, ""); err != nil {
		log.Printf("quota threshold observation %d: %v", rule.ID, err)
		return
	}
	cpaURL, managementKey := w.connection(ctx)
	if remaining > rule.ThresholdPercent || disabled || cpaURL == "" || managementKey == "" {
		return
	}
	identity := cpaauthfiles.Identity{AuthFileName: rule.FileName, AuthIndex: rule.AuthIndex, Provider: rule.Provider, AccountSnapshot: rule.AccountSnapshot, AccountIDSnapshot: rule.AccountID}
	if w.mutations == nil {
		return
	}
	release, err := w.mutations.Acquire(ctx, rule.FileName)
	if err != nil {
		log.Printf("quota threshold mutation %s: %v", rule.FileName, err)
		return
	}
	defer release()
	target, err := w.client.ResolveVerifiedStatusMutationTarget(ctx, cpaURL, managementKey, identity)
	if err != nil {
		_ = w.store.UpdateQuotaThresholdObservation(ctx, rule.ID, &remaining, false, result.CreatedAtMS, 0, err.Error())
		return
	}
	if target.File.Disabled {
		_ = w.store.UpdateQuotaThresholdObservation(ctx, rule.ID, &remaining, true, result.CreatedAtMS, 0, "")
		return
	}
	if err := w.client.PatchDisabledTarget(ctx, cpaURL, managementKey, target, true); err != nil {
		_ = w.store.UpdateQuotaThresholdObservation(ctx, rule.ID, &remaining, false, result.CreatedAtMS, 0, err.Error())
		return
	}
	now := time.Now().UnixMilli()
	if err := w.store.UpdateQuotaThresholdObservation(ctx, rule.ID, &remaining, true, result.CreatedAtMS, now, ""); err != nil {
		log.Printf("quota threshold trigger %d: %v", rule.ID, err)
	}
	log.Printf("quota threshold disabled %s at %.2f%% remaining (threshold %.2f%%)", rule.FileName, remaining, rule.ThresholdPercent)
}

func (w *QuotaThresholdAutoDisableWorker) connection(ctx context.Context) (string, string) {
	cpaURL, managementKey := strings.TrimSpace(w.cpaURL), strings.TrimSpace(w.managementKey)
	if cpaURL != "" && managementKey != "" {
		return cpaURL, managementKey
	}
	setup, ok, err := w.store.LoadSetup(ctx)
	if err == nil && ok {
		if cpaURL == "" {
			cpaURL = strings.TrimSpace(setup.CPAUpstreamURL)
		}
		if managementKey == "" {
			managementKey = strings.TrimSpace(setup.ManagementKey)
		}
	}
	return cpaURL, managementKey
}
func findResult(rule model.QuotaThresholdRule, results []model.CodexInspectionResult) (model.CodexInspectionResult, bool) {
	for _, result := range results {
		if !strings.EqualFold(strings.TrimSpace(result.FileName), strings.TrimSpace(rule.FileName)) {
			continue
		}
		if strings.TrimSpace(rule.AuthIndex) != "" && strings.TrimSpace(result.AuthIndex) != strings.TrimSpace(rule.AuthIndex) {
			continue
		}
		if rule.AuthIndex == "" && rule.AccountID != "" && !strings.EqualFold(strings.TrimSpace(result.AccountID), strings.TrimSpace(rule.AccountID)) {
			continue
		}
		if rule.AuthIndex == "" && rule.AccountID == "" && rule.AccountSnapshot != "" && !strings.EqualFold(strings.TrimSpace(result.AccountSnapshot), strings.TrimSpace(rule.AccountSnapshot)) {
			continue
		}
		return result, true
	}
	return model.CodexInspectionResult{}, false
}
