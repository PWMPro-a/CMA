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
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			_ = w.service.RunAutomatic(ctx)
			timer.Reset(w.service.NextInterval(ctx))
		}
	}
}
