package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

var busyRetryDelays = [...]time.Duration{
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
	2 * time.Second,
	4 * time.Second,
}

const busyRetryBudget = 10 * time.Second

// WithTxBusyRetry starts a fresh transaction for every busy retry.
func WithTxBusyRetry(ctx context.Context, db *sql.DB, operation func(*sql.Tx) error) error {
	return WithBusyRetry(ctx, func() error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := operation(tx); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// IsBusyError identifies SQLite writer-contention errors returned by both the
// driver and SQLite itself. Extended error code 517 is SQLITE_BUSY_SNAPSHOT.
func IsBusyError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") ||
		strings.Contains(message, "database table is locked") ||
		strings.Contains(message, "sqlite_busy") ||
		strings.Contains(message, "locked (517)")
}

// WithBusyRetry re-runs a complete SQLite operation after transient writer
// contention. Callers wrapping a transaction must create a new transaction in
// operation on every attempt; retrying statements on an aborted transaction is
// not valid SQLite behavior.
func WithBusyRetry(ctx context.Context, operation func() error) error {
	startedAt := time.Now()
	for attempt := 0; ; attempt++ {
		err := operation()
		if !IsBusyError(err) || attempt >= len(busyRetryDelays) {
			return err
		}
		delay := busyRetryDelays[attempt]
		if time.Since(startedAt)+delay > busyRetryBudget {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}
