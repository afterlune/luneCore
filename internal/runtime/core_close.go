package runtime

import (
	"context"
	"errors"

	publictool "github.com/afterlune/luneCore/tool"
)

func (c *Core) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
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
		if err := waitContext(ctx, &c.queueDrainWG); err != nil {
			c.mu.Lock()
			c.closing = false
			c.mu.Unlock()
			return err
		}
		if err := lockContext(ctx, &c.runtimeConfigMu); err != nil {
			c.mu.Lock()
			c.closing = false
			c.mu.Unlock()
			return err
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
		if err := waitContext(ctx, &c.queueDrainWG); err != nil {
			c.mu.Lock()
			c.closing = false
			c.mu.Unlock()
			return err
		}
		if err := lockContext(ctx, &c.runtimeConfigMu); err != nil {
			c.mu.Lock()
			c.closing = false
			c.mu.Unlock()
			return err
		}
		runtimeConfigLocked = true
		closingAfterQueueDrain = true
		c.mu.Lock()
	}
	waiters := c.turnWaiters
	approvals := c.approvals
	interactions := c.interactions
	c.turnWaiters = make(map[string]chan turnResult)
	c.approvals = make(map[string]pendingApproval)
	c.interactions = make(map[string]pendingInteraction)
	// Terminal command observers append events through Core and may need c.mu.
	// Keep new work out via c.closing, but release the lock while joining them.
	c.mu.Unlock()
	for _, waiter := range waiters {
		waiter <- turnResult{err: errors.New("core is closing")}
	}
	for _, pending := range approvals {
		select {
		case pending.response <- publictool.ApprovalDeny:
		default:
		}
	}
	for _, pending := range interactions {
		select {
		case pending.response <- userInteractionResult{Err: ErrMCPServerLifecycleChanged}:
		default:
		}
	}
	if err := waitContext(ctx, &c.sessionOps); err != nil {
		c.mu.Lock()
		c.closing = false
		c.mu.Unlock()
		return err
	}
	cleanupErr := c.commands.Close(ctx)
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
	closeErr := errors.Join(closeMCP, c.checkpoints.Close(), c.sessions.Close(), c.registry.Close(), c.closeSessionLocksLocked(), c.stateLock.Close())
	c.closed = true
	c.closing = false
	c.mu.Unlock()
	return closeErr
}
