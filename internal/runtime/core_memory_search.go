package runtime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/afterlune/luneCore/memory"
)

const maxMemoryQueryRunes = 500

// SearchMemories searches workspace memory using SQLite full-text search and,
// when configured, an optional host-provided embedder.
func (c *Core) SearchMemories(ctx context.Context, query string, limit int) ([]memory.SearchResult, error) {
	if err := c.lockMemoryAPI(ctx); err != nil {
		return nil, err
	}
	defer c.runtimeConfigMu.RUnlock()
	return c.searchMemories(ctx, query, limit)
}

func (c *Core) searchMemories(ctx context.Context, query string, limit int) ([]memory.SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("memory search query must not be empty")
	}
	if utf8.RuneCountInString(query) > maxMemoryQueryRunes {
		return nil, fmt.Errorf("memory search query must be at most %d characters", maxMemoryQueryRunes)
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	lexicalLimit := min(limit*4, 100)
	lexical, err := c.sessions.SearchMemoriesText(ctx, c.workspace, query, lexicalLimit)
	if err != nil {
		return nil, err
	}
	if c.memoryConfig.Embedder == nil {
		if len(lexical) > limit {
			lexical = lexical[:limit]
		}
		return lexical, nil
	}
	_, vectors, embedErr := c.embedTexts(ctx, []string{query})
	if embedErr != nil {
		c.logger.Warn("memory vector search unavailable; using full-text results", "error_type", fmt.Sprintf("%T", embedErr))
		if len(lexical) > limit {
			lexical = lexical[:limit]
		}
		return lexical, nil
	}
	stored, err := c.sessions.MemoryVectors(ctx, c.workspace, c.memoryConfig.Embedder.ID())
	if err != nil {
		return nil, err
	}
	type ranked struct {
		item  memory.Item
		score float64
	}
	ranks := make(map[string]ranked, len(lexical)+len(stored))
	for index, result := range lexical {
		value := ranks[result.Item.ID]
		value.item = result.Item
		value.score += 1 / float64(60+index+1)
		ranks[result.Item.ID] = value
	}
	vectorRanks := make([]ranked, 0, len(stored))
	for _, value := range stored {
		if len(value.Embedding) != len(vectors[0]) {
			continue
		}
		similarity := cosineSimilarity(vectors[0], value.Embedding)
		if similarity <= 0 {
			continue
		}
		vectorRanks = append(vectorRanks, ranked{item: value.Item, score: similarity})
	}
	sort.SliceStable(vectorRanks, func(i, j int) bool { return vectorRanks[i].score > vectorRanks[j].score })
	for index, value := range vectorRanks {
		if index >= lexicalLimit {
			break
		}
		entry := ranks[value.item.ID]
		entry.item = value.item
		entry.score += 1 / float64(60+index+1)
		ranks[value.item.ID] = entry
	}
	ordered := make([]ranked, 0, len(ranks))
	for _, value := range ranks {
		ordered = append(ordered, value)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].score == ordered[j].score {
			if ordered[i].item.Importance == ordered[j].item.Importance {
				if ordered[i].item.UpdatedAt.Equal(ordered[j].item.UpdatedAt) {
					return ordered[i].item.ID < ordered[j].item.ID
				}
				return ordered[i].item.UpdatedAt.After(ordered[j].item.UpdatedAt)
			}
			return ordered[i].item.Importance > ordered[j].item.Importance
		}
		return ordered[i].score > ordered[j].score
	})
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	results := make([]memory.SearchResult, len(ordered))
	for index, value := range ordered {
		results[index] = memory.SearchResult{Item: value.item, Score: value.score}
	}
	return results, nil
}

func (c *Core) embedTexts(ctx context.Context, texts []string) (string, [][]float32, error) {
	embedder := c.memoryConfig.Embedder
	if embedder == nil {
		return "", nil, nil
	}
	embedderID := strings.TrimSpace(embedder.ID())
	if embedderID == "" {
		return "", nil, errors.New("memory embedder ID must not be empty")
	}
	if len(texts) == 0 {
		return embedderID, nil, nil
	}
	vectors, err := embedder.Embed(ctx, texts)
	if err != nil {
		return "", nil, err
	}
	if err := validateEmbeddingBatch(texts, vectors); err != nil {
		return "", nil, err
	}
	return embedderID, vectors, nil
}

func validateEmbeddingBatch(texts []string, vectors [][]float32) error {
	if len(vectors) != len(texts) {
		return errors.New("memory embedder must return one vector per input text")
	}
	dimension := 0
	for _, vector := range vectors {
		if len(vector) == 0 {
			return errors.New("memory embedder returned an empty vector")
		}
		if dimension == 0 {
			dimension = len(vector)
		} else if len(vector) != dimension {
			return errors.New("memory embedder returned inconsistent vector dimensions")
		}
		var norm float64
		for _, component := range vector {
			value := float64(component)
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return errors.New("memory embedder returned a non-finite vector value")
			}
			norm += value * value
		}
		if norm == 0 {
			return errors.New("memory embedder returned a zero vector")
		}
	}
	return nil
}

func cosineSimilarity(left, right []float32) float64 {
	if len(left) == 0 || len(left) != len(right) {
		return 0
	}
	var dot, leftNorm, rightNorm float64
	for i, component := range left {
		a, b := float64(component), float64(right[i])
		dot += a * b
		leftNorm += a * a
		rightNorm += b * b
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0
	}
	return dot / math.Sqrt(leftNorm*rightNorm)
}
