package runtime

import (
	"context"
	"errors"
	"time"
)

func (c *Core) Close() error {
	if c == nil {
		return nil
	}
	runtimeConfigLocked := c.runtimeConfigMu.TryLock()
	closingAfterQueueDrain := false
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		if runtimeConfigLocked {
			c.runtimeConfigMu.Unlock()
		}
		return nil
	}
	if c.closing {
		c.mu.Unlock()
		if runtimeConfigLocked {
			c.runtimeConfigMu.Unlock()
		}
		return errors.New("core is already closing")
	}
	if c.hasRunningTasksLocked() {
		c.mu.Unlock()
		if runtimeConfigLocked {
			c.runtimeConfigMu.Unlock()
		}
		return errors.New("cannot close core while a task is running")
	}
	if !runtimeConfigLocked {
		if len(c.queueDraining) == 0 {
			c.mu.Unlock()
			return errors.New("cannot close core while runtime configuration changes are active")
		}
		c.closing = true
		c.mu.Unlock()
		c.queueDrainWG.Wait()
		for !c.runtimeConfigMu.TryLock() {
			time.Sleep(time.Millisecond)
		}
		runtimeConfigLocked = true
		closingAfterQueueDrain = true
		c.mu.Lock()
	} else {
		c.closing = true
	}
	defer func() {
		if runtimeConfigLocked {
			c.runtimeConfigMu.Unlock()
		}
	}()
	if !closingAfterQueueDrain && c.hasRunningTasksLocked() {
		c.closing = false
		c.mu.Unlock()
		return errors.New("cannot close core while a task is running")
	}
	if !closingAfterQueueDrain && len(c.queueDraining) > 0 {
		c.mu.Unlock()
		c.runtimeConfigMu.Unlock()
		runtimeConfigLocked = false
		c.queueDrainWG.Wait()
		for !c.runtimeConfigMu.TryLock() {
			time.Sleep(time.Millisecond)
		}
		runtimeConfigLocked = true
		closingAfterQueueDrain = true
		c.mu.Lock()
	}
	for executionID, waiter := range c.turnWaiters {
		delete(c.turnWaiters, executionID)
		waiter <- turnResult{err: errors.New("core is closing")}
	}
	for id, pending := range c.approvals {
		delete(c.approvals, id)
		select {
		case pending.response <- false:
		default:
		}
	}
	// Terminal command observers append events through Core and may need c.mu.
	// Keep new work out via c.closing, but release the lock while joining them.
	c.mu.Unlock()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 35*time.Second)
	cleanupErr := c.commands.Close(cleanupCtx)
	cleanupCancel()
	if cleanupErr != nil {
		c.mu.Lock()
		c.closing = false
		c.mu.Unlock()
		return cleanupErr
	}
	var closeMCP error
	if c.mcpManager != nil {
		closeMCP = c.mcpManager.Close()
	}
	c.mu.Lock()
	for id, pending := range c.interactions {
		delete(c.interactions, id)
		select {
		case pending.response <- userInteractionResult{Err: errors.New("core is closing")}:
		default:
		}
	}
	c.mu.Unlock()
	closeErr := errors.Join(closeMCP, c.checkpoints.Close(), c.sessions.Close(), c.registry.Close(), c.stateDirLock.Close())
	c.mu.Lock()
	c.closed = true
	c.closing = false
	c.mu.Unlock()
	return closeErr
}
