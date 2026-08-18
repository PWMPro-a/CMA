package supplytask_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	sqliterepo "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/sqlite"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/supplytask"
)

func TestRepositoryCreatesListsAndCancelsPurchaseTask(t *testing.T) {
	db, err := sqliterepo.Open(filepath.Join(t.TempDir(), "purchase-task.sqlite"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository := supplytask.New(db)
	ctx := context.Background()
	created, err := repository.Create(ctx, model.SupplyPurchaseTask{
		TaskID: "purchase-test", Source: "manual", SupplierID: "supplier-a",
		Product: "oauth_7d", TargetQuantity: 10,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if created.Status != "pending" || created.MaxConcurrentOrders != 1 {
		t.Fatalf("created task = %#v", created)
	}
	active, err := repository.ListActive(ctx, 10)
	if err != nil || len(active) != 1 || active[0].TaskID != created.TaskID {
		t.Fatalf("active tasks = %#v err=%v", active, err)
	}
	cancelled, changed, err := repository.Cancel(ctx, created.TaskID, 1234)
	if err != nil || !changed {
		t.Fatalf("cancel task changed=%v err=%v", changed, err)
	}
	if cancelled.Status != "cancelled" || cancelled.CancelledAtMS != 1234 {
		t.Fatalf("cancelled task = %#v", cancelled)
	}
	active, err = repository.ListActive(ctx, 10)
	if err != nil || len(active) != 0 {
		t.Fatalf("active tasks after cancel = %#v err=%v", active, err)
	}
}
