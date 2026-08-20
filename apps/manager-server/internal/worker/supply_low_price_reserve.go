package worker

import (
	"context"
	"log"
	"sync"
	"time"

	supplysvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/supply"
)

// SupplyLowPriceReserveWorker independently watches bargain inventory. It only
// creates one durable ladder-stage intent; the purchase-task worker remains the
// sole path that creates and reconciles supplier orders.
type SupplyLowPriceReserveWorker struct {
	service *supplysvc.Service
	once    sync.Once
}

func NewSupplyLowPriceReserveWorker(service *supplysvc.Service) *SupplyLowPriceReserveWorker {
	return &SupplyLowPriceReserveWorker{service: service}
}

func (w *SupplyLowPriceReserveWorker) Start(ctx context.Context) {
	if w == nil || w.service == nil {
		return
	}
	w.once.Do(func() {
		go w.run(ctx)
	})
}

func (w *SupplyLowPriceReserveWorker) run(ctx context.Context) {
	initialDelay := time.Second
	w.service.ScheduleLowPriceReserveExecution(time.Now().Add(initialDelay))
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			w.service.SetLowPriceReserveRunning(true)
			execution, err := w.service.RunLowPriceReserve(ctx)
			finishedAt := time.Now()
			nextInterval := w.service.NextLowPriceReserveInterval(ctx)
			w.service.RecordLowPriceReserveExecution(execution, finishedAt, finishedAt.Add(nextInterval), err)
			if err != nil {
				log.Printf("[supply-low-price] watcher failed: %v", err)
			}
			timer.Reset(nextInterval)
		}
	}
}
