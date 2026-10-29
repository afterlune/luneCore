package runtime

import (
	"context"
	"sync"
	"time"
)

type contextLocker interface{ TryLock() bool }

// lockContext waits without spawning an operation that outlives its caller.
func lockContext(ctx context.Context, lock contextLocker) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if lock.TryLock() {
		return nil
	}
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if lock.TryLock() {
				return nil
			}
		}
	}
}

// The waiter only observes completion; ownership and resource cleanup stay with
// the caller. Accepted work is always joined before any storage is closed.
func waitContext(ctx context.Context, group *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() { group.Wait(); close(done) }()
	select {
	case <-done:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}
