package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/afterlune/luneCore/memory"
)

const maxMemoryTextRunes = 1000

// ListMemories returns editable workspace memories, ordered by importance and
// recency. A non-positive limit uses the store default.
func (c *Core) ListMemories(ctx context.Context, limit int) ([]memory.Item, error) {
	if err := c.lockMemoryAPI(ctx); err != nil {
		return nil, err
	}
	defer c.runtimeConfigMu.RUnlock()
	return c.sessions.ListMemories(ctx, c.workspace, limit)
}

func (c *Core) AddMemory(ctx context.Context, input memory.Input) (memory.Item, error) {
	if err := c.lockMemoryAPI(ctx); err != nil {
		return memory.Item{}, err
	}
	defer c.runtimeConfigMu.RUnlock()
	item, err := normalizeMemoryInput(input)
	if err != nil {
		return memory.Item{}, err
	}
	item.ID, err = newID()
	if err != nil {
		return memory.Item{}, err
	}
	item.CreatedAt, item.UpdatedAt = time.Now(), time.Now()
	embedderID, vectors, embedErr := c.embedTexts(ctx, []string{item.Content})
	if embedErr != nil {
		c.logger.Warn("memory embedding unavailable; saving for full-text search", "error_type", fmt.Sprintf("%T", embedErr))
		embedderID, vectors = "", nil
	}
	var vector []float32
	if len(vectors) == 1 {
		vector = vectors[0]
	}
	if err := c.sessions.CreateMemory(ctx, c.workspace, item, embedderID, vector); err != nil {
		return memory.Item{}, err
	}
	return item, nil
}

func (c *Core) UpdateMemory(ctx context.Context, id string, input memory.Input) (memory.Item, error) {
	if err := c.lockMemoryAPI(ctx); err != nil {
		return memory.Item{}, err
	}
	defer c.runtimeConfigMu.RUnlock()
	item, err := c.sessions.GetMemory(ctx, c.workspace, id)
	if err != nil {
		return memory.Item{}, err
	}
	updated, err := normalizeMemoryInput(input)
	if err != nil {
		return memory.Item{}, err
	}
	item.Category, item.Content, item.Importance = updated.Category, updated.Content, updated.Importance
	item.UpdatedAt = time.Now()
	embedderID, vectors, embedErr := c.embedTexts(ctx, []string{item.Content})
	if embedErr != nil {
		c.logger.Warn("memory embedding unavailable; updating full-text memory", "error_type", fmt.Sprintf("%T", embedErr))
		embedderID, vectors = "", nil
	}
	var vector []float32
	if len(vectors) == 1 {
		vector = vectors[0]
	}
	if err := c.sessions.UpdateMemory(ctx, c.workspace, item, embedderID, vector); err != nil {
		return memory.Item{}, err
	}
	return item, nil
}

func (c *Core) DeleteMemory(ctx context.Context, id string) error {
	if err := c.lockMemoryAPI(ctx); err != nil {
		return err
	}
	defer c.runtimeConfigMu.RUnlock()
	return c.sessions.DeleteMemory(ctx, c.workspace, id)
}

func (c *Core) ClearMemories(ctx context.Context) error {
	if err := c.lockMemoryAPI(ctx); err != nil {
		return err
	}
	defer c.runtimeConfigMu.RUnlock()
	return c.sessions.ClearMemories(ctx, c.workspace)
}

// ReindexMemories regenerates vectors after the host changes its embedder ID.
// Full-text search remains available if a batch cannot be embedded.
func (c *Core) ReindexMemories(ctx context.Context) error {
	if err := c.lockMemoryAPI(ctx); err != nil {
		return err
	}
	defer c.runtimeConfigMu.RUnlock()
	if c.memoryConfig.Embedder == nil {
		return errors.New("memory embedder is not configured")
	}
	items, err := c.sessions.AllMemories(ctx, c.workspace)
	if err != nil {
		return err
	}
	const batchSize = 64
	for start := 0; start < len(items); start += batchSize {
		end := min(start+batchSize, len(items))
		batch := items[start:end]
		texts := make([]string, len(batch))
		for i, item := range batch {
			texts[i] = item.Content
		}
		embedderID, vectors, err := c.embedTexts(ctx, texts)
		if err != nil {
			return fmt.Errorf("reindex workspace memory: %w", err)
		}
		for i, item := range batch {
			if err := c.sessions.UpdateMemory(ctx, c.workspace, item, embedderID, vectors[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Core) lockMemoryAPI(ctx context.Context) error {
	if c == nil || ctx == nil {
		return errors.New("core and context are required")
	}
	c.runtimeConfigMu.RLock()
	c.mu.RLock()
	closed := c.closed
	c.mu.RUnlock()
	if closed {
		c.runtimeConfigMu.RUnlock()
		return errors.New("core is closed")
	}
	return nil
}

func normalizeMemoryInput(input memory.Input) (memory.Item, error) {
	input.Content = strings.TrimSpace(input.Content)
	if !validMemoryCategory(input.Category) {
		return memory.Item{}, fmt.Errorf("unsupported memory category %q", input.Category)
	}
	if input.Content == "" || utf8.RuneCountInString(input.Content) > maxMemoryTextRunes {
		return memory.Item{}, fmt.Errorf("memory content must contain 1 to %d characters", maxMemoryTextRunes)
	}
	if input.Importance == 0 {
		input.Importance = 2
	}
	if input.Importance < 1 || input.Importance > 3 {
		return memory.Item{}, errors.New("memory importance must be between 1 and 3")
	}
	return memory.Item{Category: input.Category, Content: input.Content, Importance: input.Importance}, nil
}

func validMemoryCategory(category memory.Category) bool {
	switch category {
	case memory.CategoryPreference, memory.CategoryCorrection, memory.CategoryConvention,
		memory.CategoryDecision, memory.CategoryVerifiedFact:
		return true
	default:
		return false
	}
}
