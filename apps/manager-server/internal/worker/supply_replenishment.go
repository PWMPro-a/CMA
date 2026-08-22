package worker

import (
	"context"
	"sync"
	"time"

	supplysvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/supply"
)

type SupplyReplenishmentWorker struct {
	service *supplysvc.Service
	once    sync.Once
}

func NewSupplyReplenishmentWorker(service *supplysvc.Service) *SupplyReplenishmentWorker {
	return &SupplyReplenishmentWorker{service: service}
}

func (w *SupplyReplenishmentWorker) Start(ctx context.Context) {
	if w == nil || w.service == nil {
		return
	}
	w.once.Do(func() {
		go w.run(ctx)
	})
}

func (w *SupplyReplenishmentWorker) run(ctx context.Context) {
	initialDelay := 2 * time.Second
	w.service.ScheduleAutomaticExecution(time.Now().Add(initialDelay))
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-w.service.AutomaticWake():
		}
		startedAt := time.Now()
		err := w.service.RunAutomatic(ctx)
		finishedAt := time.Now()
		w.service.ScheduleWarrantyMetadataMigration(ctx)
		// Recovery can scan and reconcile hundreds of records. Schedule it
		// after the replenishment decision so it never extends the dashboard's
		// "checking" state or delays an urgent order.
		w.service.ScheduleRecoverySyncIfDue(ctx)
		nextInterval := w.service.NextAutomaticInterval(ctx, err)
		w.service.RecordAutomaticExecution(startedAt, finishedAt, finishedAt.Add(nextInterval), err)
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(nextInterval)
	}
}
