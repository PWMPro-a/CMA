package sqlite

import (
	"context"
	"errors"
	"testing"
)

func TestIsBusyError(t *testing.T) {
	for _, message := range []string{
		"database is locked (5)",
		"database is locked (517)",
		"database table is locked",
		"SQLITE_BUSY: writer pending",
	} {
		if !IsBusyError(errors.New(message)) {
			t.Fatalf("IsBusyError(%q) = false", message)
		}
	}
	if IsBusyError(errors.New("constraint failed")) {
		t.Fatal("constraint error must not be treated as busy")
	}
}

func TestWithBusyRetryEventuallySucceeds(t *testing.T) {
	attempts := 0
	err := WithBusyRetry(context.Background(), func() error {
		attempts++
		if attempts < 3 {
			return errors.New("database is locked (517)")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithBusyRetry: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestWithBusyRetryHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempts := 0
	err := WithBusyRetry(ctx, func() error {
		attempts++
		return errors.New("database is locked")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}
