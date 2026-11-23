package runtime

import (
	"context"
	"errors"

	publictool "github.com/afterlune/luneCore/tool"
)

func (c *Core) Close(ctx context.Context) error {
	return c.close(ctx, false)
}

// BeginShutdown closes Core admission and cancels active turns. Hosts should
// call it as soon as they receive a shutdown signal, before joining RPC calls.
func (c *Core) BeginShutdown() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.closeDone != nil || c.preserveOnClose {
		return nil
	}
	if c.closing {
		return errors.New("core is already closing")
	}
	c.closing = true
	c.preserveOnClose = true
	for _, cancel := range c.taskCancels {
		cancel()
	}
	return nil
}

// Shutdown stops queue admission and pauses active turns before closing Core.
// Standalone hosts call it after stopping and joining their RPC handlers.
func (c *Core) Shutdown(ctx context.Context) error {
	return c.close(ctx, true)
}

func (c *Core) close(ctx context.Context, preserve bool) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	if preserve {
		if err := c.BeginShutdown(); err != nil {
			return err
		}
	}
	var attempt chan struct{}
	for {
		c.mu.Lock()
		if c.closeDone != nil {
			done := c.closeDone
			c.mu.Unlock()
			return c.waitClosed(ctx, done)
		}
		if c.closed {
			c.mu.Unlock()
			return nil
		}
		if waiting := c.closeAttempt; waiting != nil {
			c.mu.Unlock()
			select {
			case <-waiting:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		attempt = make(chan struct{})
		c.closeAttempt = attempt
		c.mu.Unlock()
		break
	}
	defer func() {
		c.mu.Lock()
		if c.closeAttempt == attempt {
			c.closeAttempt = nil
			close(attempt)
		}
		c.mu.Unlock()
	}()
	runtimeConfigLocked := c.runtimeConfigMu.TryLock()
	closingAfterQueueDrain := false
	c.mu.Lock()
	if c.closeDone != nil {
		done := c.closeDone
		c.mu.Unlock()
		if runtimeConfigLocked {
			c.runtimeConfigMu.Unlock()
		}
		return c.waitClosed(ctx, done)
	}
	if c.closed {
		c.mu.Unlock()
		if runtimeConfigLocked {
			c.runtimeConfigMu.Unlock()
		}
		return nil
	}
	if c.closing && !(preserve && c.preserveOnClose) {
		c.mu.Unlock()
		if runtimeConfigLocked {
			c.runtimeConfigMu.Unlock()
		}
		return errors.New("core is already closing")
	}
	if !preserve && c.hasRunningTasksLocked() {
		c.mu.Unlock()
		if runtimeConfigLocked {
			c.runtimeConfigMu.Unlock()
		}
		return errors.New("cannot close core while a task is running")
	}
	if preserve {
		c.mu.Unlock()
		if runtimeConfigLocked {
			c.runtimeConfigMu.Unlock()
		}
		if err := waitContext(ctx, &c.queueDrainWG); err != nil {
			if !preserve {
				c.abortClose()
			}
			return err
		}
		if err := lockContext(ctx, &c.runtimeConfigMu); err != nil {
			if !preserve {
				c.abortClose()
			}
			return err
		}
		runtimeConfigLocked = true
		closingAfterQueueDrain = true
		c.mu.Lock()
	} else if !runtimeConfigLocked {
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
		if !preserve {
			c.abortClose()
		}
		return err
	}
	cleanupErr := c.commands.Close(ctx)
	if cleanupErr != nil {
		if !preserve {
			c.abortClose()
		}
		return cleanupErr
	}
	c.mu.Lock()
	c.closeDone = make(chan struct{})
	done := c.closeDone
	c.mu.Unlock()
	go c.finishClose()
	return c.waitClosed(ctx, done)
}

func (c *Core) abortClose() {
	c.mu.Lock()
	c.closing = false
	c.preserveOnClose = false
	c.mu.Unlock()
}

func (c *Core) waitClosed(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return c.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Once resource shutdown starts, keep admission closed even if a waiter leaves.
// Storage remains open until every MCP operation has released its ownership.
func (c *Core) finishClose() {
	var closeMCP error
	if c.mcpManager != nil {
		closeMCP = c.mcpManager.Close(context.Background())
	}
	c.mu.Lock()
	c.closeErr = errors.Join(c.shutdownDrainErr, closeMCP, c.checkpoints.Close(), c.sessions.Close(), c.registry.Close(), c.closeSessionLocksLocked(), c.stateLock.Close())
	c.closed = true
	c.closing = false
	close(c.closeDone)
	c.mu.Unlock()
}
