package runtime

import (
	"context"
	"errors"

	"github.com/afterlune/luneCore/memory"
)

// ListMemories returns editable workspace memories, ordered by importance and
// recency. A non-positive limit uses the store default.
func (c *Core) ListMemories(ctx context.Context, limit int) ([]memory.Item, error) {
	if err := c.lockMemoryAPI(ctx); err != nil {
		return nil, err
	}
	defer c.runtimeConfigMu.RUnlock()
	return c.memory.List(ctx, limit)
}

func (c *Core) AddMemory(ctx context.Context, input memory.Input) (memory.Item, error) {
	if err := c.lockMemoryAPI(ctx); err != nil {
		return memory.Item{}, err
	}
	defer c.runtimeConfigMu.RUnlock()
	return c.memory.Add(ctx, input)
}

func (c *Core) UpdateMemory(ctx context.Context, id string, input memory.Input) (memory.Item, error) {
	if err := c.lockMemoryAPI(ctx); err != nil {
		return memory.Item{}, err
	}
	defer c.runtimeConfigMu.RUnlock()
	return c.memory.Update(ctx, id, input)
}

func (c *Core) DeleteMemory(ctx context.Context, id string) error {
	if err := c.lockMemoryAPI(ctx); err != nil {
		return err
	}
	defer c.runtimeConfigMu.RUnlock()
	return c.memory.Delete(ctx, id)
}

func (c *Core) ClearMemories(ctx context.Context) error {
	if err := c.lockMemoryAPI(ctx); err != nil {
		return err
	}
	defer c.runtimeConfigMu.RUnlock()
	return c.memory.Clear(ctx)
}

// ReindexMemories regenerates vectors after the host changes its embedder ID.
// Full-text search remains available if a batch cannot be embedded.
func (c *Core) ReindexMemories(ctx context.Context) error {
	if err := c.lockMemoryAPI(ctx); err != nil {
		return err
	}
	defer c.runtimeConfigMu.RUnlock()
	return c.memory.Reindex(ctx)
}

// SearchMemories searches workspace memory using SQLite full-text search and,
// when configured, an optional host-provided embedder.
func (c *Core) SearchMemories(ctx context.Context, query string, limit int) ([]memory.SearchResult, error) {
	if err := c.lockMemoryAPI(ctx); err != nil {
		return nil, err
	}
	defer c.runtimeConfigMu.RUnlock()
	return c.memory.Search(ctx, query, limit)
}

func (c *Core) lockMemoryAPI(ctx context.Context) error {
	if c == nil || ctx == nil {
		return errors.New("core and context are required")
	}
	c.runtimeConfigMu.RLock()
	c.mu.RLock()
	closed := c.closed || c.closing
	c.mu.RUnlock()
	if closed {
		c.runtimeConfigMu.RUnlock()
		return errors.New("core is closed")
	}
	if c.memory == nil {
		c.runtimeConfigMu.RUnlock()
		return errors.New("memory service is unavailable")
	}
	return nil
}
