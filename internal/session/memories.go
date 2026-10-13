package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/afterlune/luneCore/memory"
)

type MemoryVector struct {
	Item      memory.Item
	Embedding []float32
}

func (s *Store) CreateMemory(ctx context.Context, workspace string, item memory.Item, embedderID string, embedding []float32) error {
	if s == nil || s.db == nil || ctx == nil {
		return errors.New("session store and context are required")
	}
	if err := validateMemoryRow(workspace, item); err != nil {
		return err
	}
	encoded, err := encodeEmbedding(embedding)
	if err != nil {
		return err
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now()
	}
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = item.CreatedAt
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO memories(
		id, workspace, category, content, importance, source_session_id, source_execution_id, source_ordinal,
		embedder_id, embedding, created_at, updated_at
	) VALUES(?, ?, ?, ?, ?, ?, ?, -1, ?, ?, ?, ?)`,
		item.ID, workspace, string(item.Category), item.Content, item.Importance, item.SourceSessionID,
		item.SourceExecutionID, embedderID, nullableEmbedding(encoded), item.CreatedAt.UnixNano(), item.UpdatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("create memory %q: %w", item.ID, err)
	}
	return nil
}

func (s *Store) UpdateMemory(ctx context.Context, workspace string, item memory.Item, embedderID string, embedding []float32) error {
	if s == nil || s.db == nil || ctx == nil {
		return errors.New("session store and context are required")
	}
	if err := validateMemoryRow(workspace, item); err != nil {
		return err
	}
	encoded, err := encodeEmbedding(embedding)
	if err != nil {
		return err
	}
	updatedAt := item.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE memories SET
		category = ?, content = ?, importance = ?, embedder_id = ?, embedding = ?, updated_at = ?
		WHERE id = ? AND workspace = ?`,
		string(item.Category), item.Content, item.Importance, embedderID, nullableEmbedding(encoded), updatedAt.UnixNano(), item.ID, workspace)
	if err != nil {
		return fmt.Errorf("update memory %q: %w", item.ID, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check memory update: %w", err)
	}
	if updated == 0 {
		return fmt.Errorf("memory %q does not exist in workspace", item.ID)
	}
	return nil
}

// ReplaceExtractedMemories replaces all memory candidates emitted by one
// accepted execution in one transaction, so graph recovery can safely retry.
func (s *Store) ReplaceExtractedMemories(ctx context.Context, workspace, sessionID, executionID string, items []memory.Item, embedderID string, embeddings [][]float32) error {
	if s == nil || s.db == nil || ctx == nil || workspace == "" || sessionID == "" || executionID == "" {
		return errors.New("session store, context, workspace, session ID, and execution ID are required")
	}
	if len(embeddings) != 0 && len(embeddings) != len(items) {
		return errors.New("memory embeddings must match the number of items")
	}
	encoded := make([][]byte, len(items))
	for index, item := range items {
		if err := validateMemoryRow(workspace, item); err != nil {
			return err
		}
		if item.SourceSessionID != sessionID || item.SourceExecutionID != executionID {
			return errors.New("extracted memory source does not match its execution")
		}
		if len(embeddings) > 0 {
			value, err := encodeEmbedding(embeddings[index])
			if err != nil {
				return err
			}
			encoded[index] = value
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin extracted memory replacement: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM memories WHERE workspace = ? AND source_execution_id = ?`, workspace, executionID); err != nil {
		return fmt.Errorf("replace extracted memories: %w", err)
	}
	now := time.Now()
	for index, item := range items {
		createdAt := item.CreatedAt
		if createdAt.IsZero() {
			createdAt = now
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO memories(
			id, workspace, category, content, importance, source_session_id, source_execution_id, source_ordinal,
			embedder_id, embedding, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			item.ID, workspace, string(item.Category), item.Content, item.Importance, sessionID, executionID, index,
			embedderID, nullableEmbedding(encoded[index]), createdAt.UnixNano(), now.UnixNano())
		if err != nil {
			return fmt.Errorf("save extracted memory %d: %w", index, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit extracted memories: %w", err)
	}
	return nil
}

func (s *Store) GetMemory(ctx context.Context, workspace, id string) (memory.Item, error) {
	if s == nil || s.db == nil || ctx == nil || workspace == "" || id == "" {
		return memory.Item{}, errors.New("session store, context, workspace, and memory ID are required")
	}
	var item memory.Item
	var category string
	var createdAt, updatedAt int64
	err := s.db.QueryRowContext(ctx, `SELECT id, category, content, importance, source_session_id,
		source_execution_id, created_at, updated_at FROM memories WHERE id = ? AND workspace = ?`, id, workspace).
		Scan(&item.ID, &category, &item.Content, &item.Importance, &item.SourceSessionID, &item.SourceExecutionID, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return memory.Item{}, fmt.Errorf("memory %q does not exist in workspace", id)
	}
	if err != nil {
		return memory.Item{}, fmt.Errorf("read memory %q: %w", id, err)
	}
	item.Category = memory.Category(category)
	item.CreatedAt, item.UpdatedAt = time.Unix(0, createdAt), time.Unix(0, updatedAt)
	return item, nil
}

func (s *Store) ListMemories(ctx context.Context, workspace string, limit int) ([]memory.Item, error) {
	if s == nil || s.db == nil || ctx == nil || workspace == "" {
		return nil, errors.New("session store, context, and workspace are required")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, category, content, importance, source_session_id,
		source_execution_id, created_at, updated_at FROM memories
		WHERE workspace = ? ORDER BY importance DESC, updated_at DESC, id ASC LIMIT ?`, workspace, limit)
	if err != nil {
		return nil, fmt.Errorf("list memories: %w", err)
	}
	defer rows.Close()
	items := make([]memory.Item, 0)
	for rows.Next() {
		var item memory.Item
		var category string
		var createdAt, updatedAt int64
		if err := rows.Scan(&item.ID, &category, &item.Content, &item.Importance, &item.SourceSessionID, &item.SourceExecutionID, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("read memory: %w", err)
		}
		item.Category = memory.Category(category)
		item.CreatedAt, item.UpdatedAt = time.Unix(0, createdAt), time.Unix(0, updatedAt)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read memories: %w", err)
	}
	return items, nil
}

func (s *Store) AllMemories(ctx context.Context, workspace string) ([]memory.Item, error) {
	if s == nil || s.db == nil || ctx == nil || workspace == "" {
		return nil, errors.New("session store, context, and workspace are required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, category, content, importance, source_session_id,
		source_execution_id, created_at, updated_at FROM memories
		WHERE workspace = ? ORDER BY importance DESC, updated_at DESC, id ASC`, workspace)
	if err != nil {
		return nil, fmt.Errorf("list all memories: %w", err)
	}
	defer rows.Close()
	items := make([]memory.Item, 0)
	for rows.Next() {
		var item memory.Item
		var category string
		var createdAt, updatedAt int64
		if err := rows.Scan(&item.ID, &category, &item.Content, &item.Importance, &item.SourceSessionID, &item.SourceExecutionID, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("read memory: %w", err)
		}
		item.Category = memory.Category(category)
		item.CreatedAt, item.UpdatedAt = time.Unix(0, createdAt), time.Unix(0, updatedAt)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read all memories: %w", err)
	}
	return items, nil
}

func (s *Store) SearchMemoriesText(ctx context.Context, workspace, query string, limit int) ([]memory.SearchResult, error) {
	if s == nil || s.db == nil || ctx == nil || workspace == "" {
		return nil, errors.New("session store, context, and workspace are required")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	if utf8.RuneCountInString(query) >= 3 {
		if results, err := s.searchMemoryFTS(ctx, workspace, query, limit); err == nil && len(results) > 0 {
			return results, nil
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, category, content, importance, source_session_id,
		source_execution_id, created_at, updated_at FROM memories
		WHERE workspace = ? AND instr(lower(content), lower(?)) > 0
		ORDER BY importance DESC, updated_at DESC, id ASC LIMIT ?`, workspace, query, limit)
	if err != nil {
		return nil, fmt.Errorf("search memories: %w", err)
	}
	defer rows.Close()
	return scanMemorySearchRows(rows)
}

func (s *Store) searchMemoryFTS(ctx context.Context, workspace, query string, limit int) ([]memory.SearchResult, error) {
	parts := strings.Fields(query)
	if len(parts) == 0 {
		return nil, nil
	}
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		if utf8.RuneCountInString(part) < 3 {
			continue
		}
		quoted = append(quoted, `"`+strings.ReplaceAll(part, `"`, `""`)+`"`)
	}
	if len(quoted) == 0 {
		return nil, nil
	}
	match := strings.Join(quoted, " OR ")
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.category, m.content, m.importance,
		m.source_session_id, m.source_execution_id, m.created_at, m.updated_at
		FROM memory_fts JOIN memories AS m ON m.rowid = memory_fts.rowid
		WHERE memory_fts MATCH ? AND m.workspace = ?
		ORDER BY bm25(memory_fts), m.importance DESC, m.updated_at DESC, m.id ASC LIMIT ?`, match, workspace, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results, err := scanMemorySearchRows(rows)
	if err != nil {
		return nil, err
	}
	for index := range results {
		results[index].Score = 1 / float64(index+1)
	}
	return results, nil
}

func (s *Store) MemoryVectors(ctx context.Context, workspace, embedderID string) ([]MemoryVector, error) {
	if s == nil || s.db == nil || ctx == nil || workspace == "" || embedderID == "" {
		return nil, errors.New("session store, context, workspace, and embedder ID are required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, category, content, importance, source_session_id,
		source_execution_id, created_at, updated_at, embedding FROM memories
		WHERE workspace = ? AND embedder_id = ? AND embedding IS NOT NULL ORDER BY updated_at DESC, id ASC`, workspace, embedderID)
	if err != nil {
		return nil, fmt.Errorf("list memory vectors: %w", err)
	}
	defer rows.Close()
	values := make([]MemoryVector, 0)
	for rows.Next() {
		var item memory.Item
		var category string
		var createdAt, updatedAt int64
		var encoded []byte
		if err := rows.Scan(&item.ID, &category, &item.Content, &item.Importance, &item.SourceSessionID, &item.SourceExecutionID, &createdAt, &updatedAt, &encoded); err != nil {
			return nil, fmt.Errorf("read memory vector: %w", err)
		}
		var vector []float32
		if err := json.Unmarshal(encoded, &vector); err != nil {
			return nil, fmt.Errorf("decode memory vector %q: %w", item.ID, err)
		}
		item.Category = memory.Category(category)
		item.CreatedAt, item.UpdatedAt = time.Unix(0, createdAt), time.Unix(0, updatedAt)
		values = append(values, MemoryVector{Item: item, Embedding: vector})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read memory vectors: %w", err)
	}
	return values, nil
}

func (s *Store) DeleteMemory(ctx context.Context, workspace, id string) error {
	if s == nil || s.db == nil || ctx == nil || workspace == "" || id == "" {
		return errors.New("session store, context, workspace, and memory ID are required")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE id = ? AND workspace = ?`, id, workspace)
	if err != nil {
		return fmt.Errorf("delete memory %q: %w", id, err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check memory deletion: %w", err)
	}
	if deleted == 0 {
		return fmt.Errorf("memory %q does not exist in workspace", id)
	}
	return nil
}

func (s *Store) ClearMemories(ctx context.Context, workspace string) error {
	if s == nil || s.db == nil || ctx == nil || workspace == "" {
		return errors.New("session store, context, and workspace are required")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE workspace = ?`, workspace); err != nil {
		return fmt.Errorf("clear workspace memories: %w", err)
	}
	return nil
}

func validateMemoryRow(workspace string, item memory.Item) error {
	if workspace == "" || item.ID == "" || item.Category == "" || strings.TrimSpace(item.Content) == "" {
		return errors.New("workspace, memory ID, category, and content are required")
	}
	return nil
}

func encodeEmbedding(value []float32) ([]byte, error) {
	if len(value) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode memory embedding: %w", err)
	}
	return encoded, nil
}

func nullableEmbedding(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func scanMemorySearchRows(rows *sql.Rows) ([]memory.SearchResult, error) {
	results := make([]memory.SearchResult, 0)
	for rows.Next() {
		var item memory.Item
		var category string
		var createdAt, updatedAt int64
		if err := rows.Scan(&item.ID, &category, &item.Content, &item.Importance, &item.SourceSessionID, &item.SourceExecutionID, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("read memory search result: %w", err)
		}
		item.Category = memory.Category(category)
		item.CreatedAt, item.UpdatedAt = time.Unix(0, createdAt), time.Unix(0, updatedAt)
		results = append(results, memory.SearchResult{Item: item})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read memory search results: %w", err)
	}
	return results, nil
}
