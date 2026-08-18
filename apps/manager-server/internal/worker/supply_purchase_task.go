package worker

import (
	"context"
	"log"
	"sync"
	"time"

	supplysvc "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/supply"
)

// SupplyPurchaseTaskWorker is the single execution path for durable purchase
// intents. Capacity automation and manual API calls only enqueue tasks; this
// worker reconciles child orders and keeps retrying the remaining quantity.
type SupplyPurchaseTaskWorker struct {
	service *supplysvc.Service
	once    sync.Once
}

func NewSupplyPurchaseTaskWorker(service *supplysvc.Service) *SupplyPurchaseTaskWorker {
	return &SupplyPurchaseTaskWorker{service: service}
}

func (w *SupplyPurchaseTaskWorker) Start(ctx context.Context) {
	if w == nil || w.service == nil {
		return
	}
	w.once.Do(func() {
		go w.run(ctx)
	})
}

func (w *SupplyPurchaseTaskWorker) run(ctx context.Context) {
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	wake := w.service.PurchaseTaskWake()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
		if err := w.service.RunPurchaseTasks(ctx); err != nil {
			log.Printf("[supply-task] execution step failed: %v", err)
		}
		timer.Reset(w.service.NextPurchaseTaskInterval(ctx))
	}
}
