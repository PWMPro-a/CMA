package codexquotaoperation_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	quotaoperationrepo "github.com/seakee/cpa-manager-plus/apps/manager-server/internal/repository/codexquotaoperation"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

func TestRepositoryEnforcesOperationAndActiveAccountIdempotency(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "quota-operations.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	first, created, err := st.CodexQuotaOperations.Create(ctx, model.CodexQuotaOperation{
		OperationID: "operation-1",
		AccountKey:  "codex:account-id:hash-1",
		AuthIndex:   "auth-1",
		State:       model.CodexQuotaOperationStateCreated,
	})
	if err != nil || !created {
		t.Fatalf("create operation: created=%v err=%v", created, err)
	}
	duplicate, created, err := st.CodexQuotaOperations.Create(ctx, first)
	if err != nil || created || duplicate.OperationID != first.OperationID {
		t.Fatalf("duplicate operation: operation=%#v created=%v err=%v", duplicate, created, err)
	}
	_, _, err = st.CodexQuotaOperations.Create(ctx, model.CodexQuotaOperation{
		OperationID: "operation-2",
		AccountKey:  first.AccountKey,
		AuthIndex:   "auth-2",
		State:       model.CodexQuotaOperationStateCreated,
	})
	if !errors.Is(err, quotaoperationrepo.ErrAccountBusy) {
		t.Fatalf("second active account operation error = %v", err)
	}
	first.State = model.CodexQuotaOperationStateCompleted
	if _, err := st.CodexQuotaOperations.Update(ctx, first); err != nil {
		t.Fatalf("complete first operation: %v", err)
	}
	_, created, err = st.CodexQuotaOperations.Create(ctx, model.CodexQuotaOperation{
		OperationID: "operation-2",
		AccountKey:  first.AccountKey,
		AuthIndex:   "auth-2",
		State:       model.CodexQuotaOperationStateCreated,
	})
	if err != nil || !created {
		t.Fatalf("create next operation after completion: created=%v err=%v", created, err)
	}
}

func TestCountCompletedByAccountCountsOnlyConsumedResets(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "quota-reset-count.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	accountKey := "codex:account-id:counted"
	for index, consumed := range []bool{true, false, true} {
		value := consumed
		_, created, err := st.CodexQuotaOperations.Create(ctx, model.CodexQuotaOperation{
			OperationID: fmt.Sprintf("operation-%d", index),
			AccountKey:  accountKey,
			AuthIndex:   "auth-1",
			State:       model.CodexQuotaOperationStateCompleted,
			Consumed:    &value,
		})
		if err != nil || !created {
			t.Fatalf("create operation %d: created=%v err=%v", index, created, err)
		}
	}
	count, err := st.CodexQuotaOperations.CountCompletedByAccount(ctx, accountKey)
	if err != nil || count != 2 {
		t.Fatalf("reset count=%d err=%v", count, err)
	}
}

func TestCountCompletedByCredentialIncludesReimportedAuthIndex(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "quota-reset-reimport.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	consumed := true
	for index, operation := range []model.CodexQuotaOperation{
		{
			OperationID:  "old-generation",
			AccountKey:   "codex:account-id:old",
			AuthIndex:    "auth-stable",
			AuthFileName: "codex.json",
			State:        model.CodexQuotaOperationStateCompleted,
			Consumed:     &consumed,
		},
		{
			OperationID:  "other-file",
			AccountKey:   "codex:account-id:other",
			AuthIndex:    "auth-stable",
			AuthFileName: "other.json",
			State:        model.CodexQuotaOperationStateCompleted,
			Consumed:     &consumed,
		},
	} {
		if _, created, createErr := st.CodexQuotaOperations.Create(ctx, operation); createErr != nil || !created {
			t.Fatalf("create operation %d: created=%v err=%v", index, created, createErr)
		}
	}
	count, err := st.CodexQuotaOperations.CountCompletedByCredential(
		ctx,
		"codex:account-id:new",
		"auth-stable",
	)
	if err != nil || count != 2 {
		t.Fatalf("reimport reset count=%d err=%v, want 2", count, err)
	}
}
