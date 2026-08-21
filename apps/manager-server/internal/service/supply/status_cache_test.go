package supply

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

func TestSupplyStatusCacheCoalescesConcurrentBuilds(t *testing.T) {
	var cache supplyStatusCache
	var builds atomic.Int32
	buildStarted := make(chan struct{})
	releaseBuild := make(chan struct{})
	build := func(context.Context, int) (Status, error) {
		if builds.Add(1) == 1 {
			close(buildStarted)
		}
		<-releaseBuild
		return cachedStatusFixture("shared"), nil
	}

	const callers = 24
	start := make(chan struct{})
	var ready sync.WaitGroup
	var done sync.WaitGroup
	ready.Add(callers)
	done.Add(callers)
	errs := make(chan error, callers)
	for index := 0; index < callers; index++ {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			status, err := cache.get(context.Background(), 50, build)
			if err != nil {
				errs <- err
				return
			}
			if len(status.Orders) != 1 || status.Orders[0].OrderID != "shared" {
				errs <- fmt.Errorf("unexpected status: %#v", status.Orders)
			}
		}()
	}
	ready.Wait()
	close(start)
	<-buildStarted
	time.Sleep(25 * time.Millisecond)
	close(releaseBuild)
	done.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("status builds = %d, want 1", got)
	}
}

func TestSupplyStatusCacheReturnsIndependentSnapshots(t *testing.T) {
	var cache supplyStatusCache
	first, err := cache.get(context.Background(), 50, func(context.Context, int) (Status, error) {
		return cachedStatusFixture("original"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	first.Orders[0].OrderID = "mutated"
	first.ActiveOrder.OrderID = "mutated"

	second, err := cache.get(context.Background(), 50, func(context.Context, int) (Status, error) {
		t.Fatal("fresh cache unexpectedly rebuilt")
		return Status{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Orders[0].OrderID != "original" || second.ActiveOrder.OrderID != "original" {
		t.Fatalf("cached snapshot was mutated: %#v", second)
	}
}

func TestSupplyStatusCacheServesLastGoodOnSQLiteBusy(t *testing.T) {
	var cache supplyStatusCache
	if _, err := cache.get(context.Background(), 50, func(context.Context, int) (Status, error) {
		return cachedStatusFixture("last-good"), nil
	}); err != nil {
		t.Fatal(err)
	}
	cache.invalidate()

	status, err := cache.get(context.Background(), 50, func(context.Context, int) (Status, error) {
		return Status{}, errors.New("database is locked (5) (SQLITE_BUSY)")
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Orders) != 1 || status.Orders[0].OrderID != "last-good" {
		t.Fatalf("stale status = %#v", status)
	}
}

func TestSupplyStatusCacheDoesNotHideFirstOrNonTransientFailure(t *testing.T) {
	var empty supplyStatusCache
	if _, err := empty.get(context.Background(), 50, func(context.Context, int) (Status, error) {
		return Status{}, errors.New("database is locked (SQLITE_BUSY)")
	}); err == nil {
		t.Fatal("first busy build unexpectedly succeeded")
	}

	var populated supplyStatusCache
	if _, err := populated.get(context.Background(), 50, func(context.Context, int) (Status, error) {
		return cachedStatusFixture("last-good"), nil
	}); err != nil {
		t.Fatal(err)
	}
	populated.invalidate()
	wantErr := errors.New("invalid supply configuration")
	if _, err := populated.get(context.Background(), 50, func(context.Context, int) (Status, error) {
		return Status{}, wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("non-transient error = %v, want %v", err, wantErr)
	}
}

func TestSupplyStatusCacheInvalidationForcesSuccessfulRefresh(t *testing.T) {
	var cache supplyStatusCache
	if _, err := cache.get(context.Background(), 50, func(context.Context, int) (Status, error) {
		return cachedStatusFixture("before"), nil
	}); err != nil {
		t.Fatal(err)
	}
	cache.invalidate()

	var builds int
	status, err := cache.get(context.Background(), 50, func(context.Context, int) (Status, error) {
		builds++
		return cachedStatusFixture("after"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if builds != 1 || status.Orders[0].OrderID != "after" {
		t.Fatalf("builds=%d status=%#v", builds, status)
	}
}

func TestSupplyStatusCacheDashboardServesStaleWhileRefreshing(t *testing.T) {
	var cache supplyStatusCache
	if _, err := cache.get(context.Background(), 50, func(context.Context, int) (Status, error) {
		return cachedStatusFixture("before"), nil
	}); err != nil {
		t.Fatal(err)
	}
	cache.invalidate()
	started := make(chan struct{})
	release := make(chan struct{})
	var builds atomic.Int32
	build := func(context.Context, int) (Status, error) {
		if builds.Add(1) == 1 {
			close(started)
		}
		<-release
		return cachedStatusFixture("after"), nil
	}

	status, err := cache.getStaleWhileRefresh(context.Background(), 50, build)
	if err != nil || status.Orders[0].OrderID != "before" {
		t.Fatalf("stale dashboard status=%#v err=%v", status, err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background dashboard refresh did not start")
	}
	status, err = cache.getStaleWhileRefresh(context.Background(), 50, build)
	if err != nil || status.Orders[0].OrderID != "before" || builds.Load() != 1 {
		t.Fatalf("coalesced stale dashboard status=%#v builds=%d err=%v", status, builds.Load(), err)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		status, err = cache.get(context.Background(), 50, build)
		if err == nil && status.Orders[0].OrderID == "after" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background dashboard refresh did not publish: status=%#v err=%v", status, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func cachedStatusFixture(orderID string) Status {
	order := store.SupplyOrder{OrderID: orderID, Status: "waiting_inventory"}
	return Status{
		Overview:    Overview{CPAAvailable: 3, CPATarget: 5, CPADeficit: 2},
		ActiveOrder: &order,
		ActiveOrders: []store.SupplyOrder{
			order,
		},
		Orders: []store.SupplyOrder{order},
	}
}
