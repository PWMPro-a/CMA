package worker

import (
	"context"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	collectorpkg "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/collector"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/cpaauthfiles"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/usage"
)

const quotaThresholdJobQueueSize = 256

type quotaThresholdJob struct {
	rule         model.QuotaThresholdRule
	remaining    float64
	observedAtMS int64
}

// QuotaThresholdAutoDisableWorker reacts to newly persisted CPA usage events.
// It deliberately does not inspect historical inspection runs: the quota
// percentage in a live response is the freshest signal available and lets an
// account be disabled before another request is routed to it.
type QuotaThresholdAutoDisableWorker struct {
	store         *store.Store
	cpaURL        string
	managementKey string
	mutations     *cpaauthfiles.MutationCoordinator
	client        *cpaauthfiles.Client
	jobs          chan quotaThresholdJob
	mu            sync.Mutex
	cancel        context.CancelFunc
	done          chan struct{}
	started       bool
}

func NewQuotaThresholdAutoDisableWorker(st *store.Store, cpaURL, managementKey string, mutations *cpaauthfiles.MutationCoordinator) *QuotaThresholdAutoDisableWorker {
	return &QuotaThresholdAutoDisableWorker{
		store:         st,
		cpaURL:        strings.TrimSpace(cpaURL),
		managementKey: strings.TrimSpace(managementKey),
		mutations:     mutations,
		client:        cpaauthfiles.New(nil, 20*time.Second),
		jobs:          make(chan quotaThresholdJob, quotaThresholdJobQueueSize),
	}
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
	go func() {
		defer close(done)
		w.run(workerCtx)
	}()
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

// HandleUsageEvents is called by the collector after a batch has been inserted.
// Only new events are supplied by the collector, so this path avoids the old
// 30-second inspection ticker and reacts as soon as CPA returns quota headers.
func (w *QuotaThresholdAutoDisableWorker) HandleUsageEvents(ctx context.Context, cfg collectorpkg.RuntimeConfig, events []usage.Event) {
	if w == nil || w.store == nil || len(events) == 0 || ctx.Err() != nil {
		return
	}
	if baseURL, managementKey := strings.TrimSpace(cfg.CPAUpstreamURL), strings.TrimSpace(cfg.ManagementKey); baseURL != "" && managementKey != "" {
		w.mu.Lock()
		w.cpaURL, w.managementKey = baseURL, managementKey
		w.mu.Unlock()
	}
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
	for _, event := range events {
		if !isCodexQuotaEvent(event) || event.HeaderQuotaUsedPercent == nil {
			continue
		}
		used := *event.HeaderQuotaUsedPercent
		if math.IsNaN(used) || math.IsInf(used, 0) {
			continue
		}
		remaining := math.Max(0, math.Min(100, 100-used))
		observedAtMS := quotaEventObservedAtMS(event, time.Now())
		for _, rule := range active {
			if !quotaThresholdRuleMatchesEvent(rule, event) {
				continue
			}
			job := quotaThresholdJob{rule: rule, remaining: remaining, observedAtMS: observedAtMS}
			select {
			case w.jobs <- job:
			case <-ctx.Done():
				return
			default:
				log.Printf("quota threshold job queue full, dropped event=%q file=%q", event.EventHash, rule.FileName)
			}
		}
	}
}

func (w *QuotaThresholdAutoDisableWorker) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-w.jobs:
			w.processJob(ctx, job)
		}
	}
}

func (w *QuotaThresholdAutoDisableWorker) processJob(ctx context.Context, job quotaThresholdJob) {
	rule := job.rule
	if job.remaining > rule.ThresholdPercent {
		if err := w.store.UpdateQuotaThresholdObservation(ctx, rule.ID, &job.remaining, rule.LastDisabled, job.observedAtMS, 0, ""); err != nil {
			log.Printf("quota threshold observation %d: %v", rule.ID, err)
		}
		return
	}

	cpaURL, managementKey := w.connection(ctx)
	if cpaURL == "" || managementKey == "" || w.mutations == nil {
		_ = w.store.UpdateQuotaThresholdObservation(ctx, rule.ID, &job.remaining, rule.LastDisabled, job.observedAtMS, 0, "CPA connection is not configured")
		return
	}
	identity := cpaauthfiles.Identity{
		AuthFileName:      rule.FileName,
		AuthIndex:         rule.AuthIndex,
		Provider:          rule.Provider,
		AccountSnapshot:   rule.AccountSnapshot,
		AccountIDSnapshot: rule.AccountID,
	}
	release, err := w.mutations.Acquire(ctx, rule.FileName)
	if err != nil {
		log.Printf("quota threshold mutation %s: %v", rule.FileName, err)
		return
	}
	defer release()
	target, err := w.client.ResolveVerifiedStatusMutationTarget(ctx, cpaURL, managementKey, identity)
	if err != nil {
		_ = w.store.UpdateQuotaThresholdObservation(ctx, rule.ID, &job.remaining, false, job.observedAtMS, 0, err.Error())
		return
	}
	if credentialImportedAfter(target.File.Raw, job.observedAtMS) {
		// A re-import replaces the credential state. An event emitted before that
		// replacement must not disable the newly imported account.
		return
	}
	if target.File.Disabled {
		_ = w.store.UpdateQuotaThresholdObservation(ctx, rule.ID, &job.remaining, true, job.observedAtMS, 0, "")
		return
	}
	if err := w.client.PatchDisabledTarget(ctx, cpaURL, managementKey, target, true); err != nil {
		_ = w.store.UpdateQuotaThresholdObservation(ctx, rule.ID, &job.remaining, false, job.observedAtMS, 0, err.Error())
		return
	}
	now := time.Now().UnixMilli()
	if err := w.store.UpdateQuotaThresholdObservation(ctx, rule.ID, &job.remaining, true, job.observedAtMS, now, ""); err != nil {
		log.Printf("quota threshold trigger %d: %v", rule.ID, err)
	}
	log.Printf("quota threshold disabled %s at %.2f%% remaining (threshold %.2f%%)", rule.FileName, job.remaining, rule.ThresholdPercent)
}

func (w *QuotaThresholdAutoDisableWorker) connection(ctx context.Context) (string, string) {
	w.mu.Lock()
	cpaURL, managementKey := strings.TrimSpace(w.cpaURL), strings.TrimSpace(w.managementKey)
	w.mu.Unlock()
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

func isCodexQuotaEvent(event usage.Event) bool {
	for _, identity := range []string{event.Provider, event.AuthProviderSnapshot, event.ExecutorType, event.AuthType} {
		if strings.Contains(strings.ToLower(strings.TrimSpace(identity)), "codex") {
			return true
		}
	}
	return false
}

func quotaThresholdRuleMatchesEvent(rule model.QuotaThresholdRule, event usage.Event) bool {
	if !strings.EqualFold(strings.TrimSpace(rule.FileName), strings.TrimSpace(event.AuthFileSnapshot)) {
		return false
	}
	if authIndex := strings.TrimSpace(rule.AuthIndex); authIndex != "" {
		return strings.EqualFold(authIndex, strings.TrimSpace(event.AuthIndex))
	}
	if accountID := strings.TrimSpace(rule.AccountID); accountID != "" {
		return strings.EqualFold(accountID, strings.TrimSpace(event.AuthProjectIDSnapshot)) || strings.EqualFold(accountID, strings.TrimSpace(event.AccountSnapshot))
	}
	if provider := normalizeQuotaProvider(rule.Provider); provider != "" && provider != normalizeQuotaProvider(event.Provider) && provider != normalizeQuotaProvider(event.AuthProviderSnapshot) {
		return false
	}
	snapshot := strings.TrimSpace(rule.AccountSnapshot)
	return snapshot == "" || strings.EqualFold(snapshot, strings.TrimSpace(event.AccountSnapshot))
}
