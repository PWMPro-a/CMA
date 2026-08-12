package codexquota

import (
	"context"
	"strings"
	"sync"
)

type accountLocks struct {
	mu     sync.Mutex
	active map[string]struct{}
	wait   chan struct{}
}

func newAccountLocks() *accountLocks {
	return &accountLocks{active: make(map[string]struct{}), wait: make(chan struct{})}
}

func (l *accountLocks) acquire(ctx context.Context, accountKey string) (func(), error) {
	key := strings.ToLower(strings.TrimSpace(accountKey))
	if key == "" {
		return func() {}, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		l.mu.Lock()
		if _, exists := l.active[key]; !exists {
			l.active[key] = struct{}{}
			l.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					l.mu.Lock()
					delete(l.active, key)
					close(l.wait)
					l.wait = make(chan struct{})
					l.mu.Unlock()
				})
			}, nil
		}
		wait := l.wait
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wait:
		}
	}
}
