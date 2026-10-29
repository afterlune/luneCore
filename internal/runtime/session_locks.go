package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/afterlune/luneCore/internal/session"
)

type sessionLockEntry struct {
	lock           *session.FileLock
	runtimeRefs    int
	operationRefs  int
	releasePending bool
}

func (c *Core) beginSessionOpen(sessionID string) error {
	if c == nil || sessionID == "" {
		return errors.New("core and session id are required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.closing {
		return errors.New("core is closed")
	}
	if entry := c.sessionLocks[sessionID]; entry != nil {
		entry.runtimeRefs++
		entry.releasePending = false
		c.sessionOps.Add(1)
		return nil
	}
	lock, err := session.AcquireSessionLock(c.stateDir, sessionID)
	if err != nil {
		return fmt.Errorf("acquire session lock: %w", err)
	}
	c.sessionLocks[sessionID] = &sessionLockEntry{lock: lock, runtimeRefs: 1}
	c.sessionOps.Add(1)
	return nil
}

func (c *Core) releaseSessionLock(ctx context.Context, sessionID string) error {
	if c == nil || sessionID == "" {
		return nil
	}
	c.mu.Lock()
	entry := c.sessionLocks[sessionID]
	if entry == nil {
		c.mu.Unlock()
		return nil
	}
	if entry.runtimeRefs > 1 {
		entry.runtimeRefs--
		c.mu.Unlock()
		return nil
	}
	if entry.runtimeRefs == 0 {
		c.mu.Unlock()
		return nil
	}
	if c.closing {
		entry.runtimeRefs--
		if entry.runtimeRefs > 0 || entry.operationRefs > 0 {
			c.mu.Unlock()
			return nil
		}
		if c.sessionIsRunningLocked(sessionID) || c.queueDraining[sessionID] {
			entry.releasePending = true
			c.mu.Unlock()
			return nil
		}
		delete(c.sessionLocks, sessionID)
		lock := entry.lock
		c.mu.Unlock()
		return lock.Close()
	}
	if entry.operationRefs > 0 || c.sessionIsRunningLocked(sessionID) || c.queueDraining[sessionID] {
		entry.runtimeRefs--
		entry.releasePending = true
		c.mu.Unlock()
		return nil
	}
	c.sessionOps.Add(1)
	c.mu.Unlock()
	defer c.sessionOps.Done()

	queued, err := c.sessions.QueuedTurns(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("check queued session work before releasing lock: %w", err)
	}
	if hasExecutableQueuedTurn(queued) {
		c.mu.Lock()
		if current := c.sessionLocks[sessionID]; current == entry {
			if current.runtimeRefs > 1 {
				current.runtimeRefs--
			} else if current.runtimeRefs == 1 {
				current.runtimeRefs--
				current.releasePending = true
			}
		}
		c.mu.Unlock()
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	entry = c.sessionLocks[sessionID]
	if entry == nil {
		return nil
	}
	if entry.runtimeRefs > 1 {
		entry.runtimeRefs--
		return nil
	}
	if entry.runtimeRefs == 0 && entry.operationRefs == 0 {
		delete(c.sessionLocks, sessionID)
		return entry.lock.Close()
	}
	if entry.operationRefs > 0 {
		entry.runtimeRefs--
		entry.releasePending = true
		return nil
	}
	if c.sessionIsRunningLocked(sessionID) || c.queueDraining[sessionID] {
		entry.runtimeRefs--
		entry.releasePending = true
		return nil
	}
	delete(c.sessionLocks, sessionID)
	return entry.lock.Close()
}

func (c *Core) closeSessionLocksLocked() error {
	var closeErr error
	for sessionID, entry := range c.sessionLocks {
		delete(c.sessionLocks, sessionID)
		closeErr = errors.Join(closeErr, entry.lock.Close())
	}
	return closeErr
}

// withSessionLock serializes a short session-scoped mutation with any runtime
// that owns the session. The lock is released after operation completes unless
// another runtime in this Core still owns it.
func (c *Core) withSessionLock(sessionID string, operation func() error) error {
	if c == nil || sessionID == "" || operation == nil {
		return errors.New("core, session id, and operation are required")
	}
	c.mu.Lock()
	if c.closed || c.closing {
		c.mu.Unlock()
		return errors.New("core is closed")
	}
	entry := c.sessionLocks[sessionID]
	if entry == nil {
		lock, err := session.AcquireSessionLock(c.stateDir, sessionID)
		if err != nil {
			c.mu.Unlock()
			return fmt.Errorf("acquire session lock: %w", err)
		}
		entry = &sessionLockEntry{lock: lock}
		c.sessionLocks[sessionID] = entry
	}
	entry.operationRefs++
	entry.releasePending = false
	c.sessionOps.Add(1)
	c.mu.Unlock()

	operationErr := operation()
	releaseErr := c.releaseSessionOperation(sessionID, entry)
	c.sessionOps.Done()
	return errors.Join(operationErr, releaseErr)
}

func (c *Core) releaseSessionOperation(sessionID string, expected *sessionLockEntry) error {
	c.mu.Lock()
	entry := c.sessionLocks[sessionID]
	if entry != expected || entry == nil {
		c.mu.Unlock()
		return nil
	}
	entry.operationRefs--
	if entry.operationRefs < 0 {
		c.mu.Unlock()
		return errors.New("session lock operation reference underflow")
	}
	if entry.operationRefs > 0 || entry.runtimeRefs > 0 {
		c.mu.Unlock()
		return nil
	}
	entry.releasePending = true
	c.mu.Unlock()
	return c.releaseIdleSessionLock(sessionID, entry)
}

func (c *Core) releaseIdleSessionLock(sessionID string, expected *sessionLockEntry) error {
	c.mu.Lock()
	entry := c.sessionLocks[sessionID]
	if entry != expected || entry == nil || !entry.releasePending || entry.runtimeRefs > 0 || entry.operationRefs > 0 || c.sessionIsRunningLocked(sessionID) || c.queueDraining[sessionID] {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()
	queued, err := c.sessions.QueuedTurns(context.Background(), sessionID)
	if err != nil || hasExecutableQueuedTurn(queued) {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry = c.sessionLocks[sessionID]
	if entry != expected || entry == nil || !entry.releasePending || entry.runtimeRefs > 0 || entry.operationRefs > 0 || c.sessionIsRunningLocked(sessionID) || c.queueDraining[sessionID] {
		return nil
	}
	delete(c.sessionLocks, sessionID)
	return entry.lock.Close()
}

func hasExecutableQueuedTurn(queued []session.QueuedTurn) bool {
	for _, item := range queued {
		if item.Status == session.QueuedTurnQueued || item.Status == session.QueuedTurnActive {
			return true
		}
	}
	return false
}

func (c *Core) releasePendingSessionLock(sessionID string) error {
	c.mu.RLock()
	entry := c.sessionLocks[sessionID]
	c.mu.RUnlock()
	return c.releaseIdleSessionLock(sessionID, entry)
}
