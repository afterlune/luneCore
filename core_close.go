package core

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
	if !runtimeConfigLocked {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil
		}
		if c.hasRunningTasksLocked() {
			c.mu.Unlock()
			return errors.New("cannot close core while a task is running")
		}
		if len(c.queueDraining) == 0 {
			c.mu.Unlock()
			return errors.New("cannot close core while runtime configuration changes are active")
		}
		c.closed = true
		c.mu.Unlock()
		c.queueDrainWG.Wait()
		for !c.runtimeConfigMu.TryLock() {
			time.Sleep(time.Millisecond)
		}
		runtimeConfigLocked = true
		closingAfterQueueDrain = true
	}
	defer func() {
		if runtimeConfigLocked {
			c.runtimeConfigMu.Unlock()
		}
	}()
	c.mu.Lock()
	if c.closed && !closingAfterQueueDrain {
		c.mu.Unlock()
		return nil
	}
	if !closingAfterQueueDrain && c.hasRunningTasksLocked() {
		c.mu.Unlock()
		return errors.New("cannot close core while a task is running")
	}
	if !closingAfterQueueDrain && len(c.queueDraining) > 0 {
		c.closed = true
		c.mu.Unlock()
		c.runtimeConfigMu.Unlock()
		runtimeConfigLocked = false
		c.queueDrainWG.Wait()
		for !c.runtimeConfigMu.TryLock() {
			time.Sleep(time.Millisecond)
		}
		runtimeConfigLocked = true
		c.mu.Lock()
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 35*time.Second)
	cleanupErr := c.commands.Close(cleanupCtx)
	cleanupCancel()
	if cleanupErr != nil {
		c.mu.Unlock()
		return cleanupErr
	}
	c.closed = true
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
	c.mu.Unlock()
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
	return errors.Join(closeMCP, c.checkpoints.Close(), c.sessions.Close(), c.registry.Close(), c.stateDirLock.Close())
}
