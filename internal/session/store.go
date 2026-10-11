package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const currentSchemaVersion = 1

// Snapshot is the persisted state for one workspace session. Data is owned by
// the caller so the session package does not depend on the TUI or model layer.
type Snapshot struct {
	ID        string
	Workspace string
	UpdatedAt time.Time
	Data      json.RawMessage
}

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve session database path: %w", err)
	}
	filePath := filepath.ToSlash(absolute)
	if filepath.VolumeName(absolute) != "" {
		filePath = "/" + filePath
	}
	params := url.Values{}
	params.Set("_busy_timeout", "5000")
	params.Set("_journal_mode", "WAL")
	params.Set("_synchronous", "FULL")
	fileURL := url.URL{Scheme: "file", Path: filePath, RawQuery: params.Encode()}
	db, err := sql.Open("sqlite", fileURL.String())
	if err != nil {
		return nil, fmt.Errorf("open session database: %w", err)
	}
	db.SetMaxOpenConns(1)
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read session database version: %w", err)
	}
	if version > currentSchemaVersion {
		_ = db.Close()
		return nil, fmt.Errorf("session database version %d is newer than supported version %d", version, currentSchemaVersion)
	}
	tx, err := db.Begin()
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("begin session database migration: %w", err)
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			workspace TEXT NOT NULL,
			updated_at INTEGER NOT NULL,
			data BLOB NOT NULL
		)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize session database: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS sessions_workspace_updated ON sessions(workspace, updated_at DESC)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize session index: %w", err)
	}
	if version < 1 {
		if _, err := tx.Exec(`UPDATE sessions
			SET data = CAST(json_set(
				CAST(data AS TEXT),
				'$.title', coalesce(json_extract(CAST(data AS TEXT), '$.title'), ''),
			'$.archived', json('false')
			) AS BLOB)`); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			return nil, fmt.Errorf("migrate session metadata: %w", err)
		}
		if _, err := tx.Exec(`PRAGMA user_version = 1`); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			return nil, fmt.Errorf("set session database version: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("commit session database migration: %w", err)
	}
	if err := os.Chmod(absolute, 0600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secure session database permissions: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Save(ctx context.Context, snapshot Snapshot) error {
	if s == nil || s.db == nil {
		return errors.New("session store is not open")
	}
	if snapshot.ID == "" || snapshot.Workspace == "" {
		return errors.New("session id and workspace are required")
	}
	if !json.Valid(snapshot.Data) {
		return errors.New("session data must be valid JSON")
	}
	updatedAt := snapshot.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions(id, workspace, updated_at, data) VALUES(?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			workspace = excluded.workspace,
			updated_at = excluded.updated_at,
			data = CAST(json_set(
				CAST(excluded.data AS TEXT),
				'$.history',
				json(coalesce(json_extract(CAST(sessions.data AS TEXT), '$.history'), 'null')),
				'$.title', coalesce(json_extract(CAST(sessions.data AS TEXT), '$.title'), ''),
				'$.archived', json(CASE json_type(CAST(sessions.data AS TEXT), '$.archived')
					WHEN 'true' THEN 'true' WHEN 'false' THEN 'false' ELSE 'false' END)
			) AS BLOB)
	`, snapshot.ID, snapshot.Workspace, updatedAt.UnixNano(), []byte(snapshot.Data))
	if err != nil {
		return fmt.Errorf("save session %q: %w", snapshot.ID, err)
	}
	return nil
}

// ReplaceDataIfUnchanged replaces a payload only when it still matches the
// version read by the caller. It leaves updated_at untouched so schema
// migrations do not reorder sessions.
func (s *Store) ReplaceDataIfUnchanged(ctx context.Context, id, workspace string, expected, replacement json.RawMessage) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("session store is not open")
	}
	if id == "" || workspace == "" || !json.Valid(expected) || !json.Valid(replacement) {
		return false, errors.New("session id, workspace, and valid data are required")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET data = ?
		WHERE id = ? AND workspace = ? AND data = ?
	`, []byte(replacement), id, workspace, []byte(expected))
	if err != nil {
		return false, fmt.Errorf("replace session data for %q: %w", id, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check session data replacement: %w", err)
	}
	return updated > 0, nil
}

func (s *Store) UpdateTitle(ctx context.Context, id, workspace, title string) error {
	return s.updateMetadata(ctx, id, workspace, "title", title)
}

func (s *Store) SetArchived(ctx context.Context, id, workspace string, archived bool) error {
	return s.updateMetadata(ctx, id, workspace, "archived", archived)
}

func (s *Store) updateMetadata(ctx context.Context, id, workspace, key string, value any) error {
	if s == nil || s.db == nil {
		return errors.New("session store is not open")
	}
	if id == "" || workspace == "" {
		return errors.New("session id and workspace are required")
	}
	if key != "title" && key != "archived" {
		return fmt.Errorf("unsupported session metadata field %q", key)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode session %s: %w", key, err)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET data = CAST(json_set(CAST(data AS TEXT), ?, json(?)) AS BLOB), updated_at = ?
		WHERE id = ? AND workspace = ?
	`, "$."+key, string(encoded), time.Now().UnixNano(), id, workspace)
	if err != nil {
		return fmt.Errorf("update session %s for %q: %w", key, id, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check session metadata update: %w", err)
	}
	if updated == 0 {
		return fmt.Errorf("session %q does not exist in workspace", id)
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, id, workspace string) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("session store is not open")
	}
	if id == "" || workspace == "" {
		return false, errors.New("session id and workspace are required")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ? AND workspace = ?`, id, workspace)
	if err != nil {
		return false, fmt.Errorf("delete session %q: %w", id, err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check session deletion: %w", err)
	}
	return deleted > 0, nil
}

// UpdateHistory replaces the canonical conversation and discards any cached
// provider context because it no longer corresponds to that transcript.
func (s *Store) UpdateHistory(ctx context.Context, id, workspace string, history json.RawMessage) error {
	return s.UpdateConversation(ctx, id, workspace, history, json.RawMessage("null"), "")
}

// UpdateConversation atomically replaces the canonical transcript and its
// active provider context. A single SQL update keeps them consistent with the
// graph checkpoint lifecycle.
func (s *Store) UpdateConversation(ctx context.Context, id, workspace string, history, modelContext json.RawMessage, contextKey string) error {
	if s == nil || s.db == nil {
		return errors.New("session store is not open")
	}
	if id == "" || workspace == "" || !json.Valid(history) || len(modelContext) > 0 && !json.Valid(modelContext) {
		return errors.New("session id, workspace, valid history, and valid model context are required")
	}
	if len(modelContext) == 0 {
		modelContext = json.RawMessage("null")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET data = CAST(json_set(
			json_set(
				json_set(CAST(data AS TEXT), '$.history', json(?)),
				'$.model_context', json(?)),
			'$.model_context_key', ?) AS BLOB), updated_at = ?
		WHERE id = ? AND workspace = ?
	`, string(history), string(modelContext), contextKey, time.Now().UnixNano(), id, workspace)
	if err != nil {
		return fmt.Errorf("persist conversation for session %q: %w", id, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check session conversation update: %w", err)
	}
	if updated == 0 {
		return fmt.Errorf("session %q does not exist in workspace", id)
	}
	return nil
}

func (s *Store) Latest(ctx context.Context, workspace string) (*Snapshot, error) {
	rows, err := s.query(ctx, `
		SELECT id, workspace, updated_at, data FROM sessions
		WHERE workspace = ? AND coalesce(json_extract(CAST(data AS TEXT), '$.archived'), 0) = 0
		ORDER BY updated_at DESC LIMIT 1
	`, workspace)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

func (s *Store) Get(ctx context.Context, id, workspace string) (*Snapshot, error) {
	if id == "" || workspace == "" {
		return nil, errors.New("session id and workspace are required")
	}
	rows, err := s.query(ctx, `
		SELECT id, workspace, updated_at, data FROM sessions
		WHERE id = ? AND workspace = ? LIMIT 1
	`, id, workspace)
	if err != nil || len(rows) == 0 {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("session %q not found in workspace", id)
	}
	return &rows[0], nil
}

func (s *Store) List(ctx context.Context, workspace string, limit int, includeArchived bool) ([]Snapshot, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	return s.query(ctx, `
		SELECT id, workspace, updated_at, data FROM sessions
		WHERE workspace = ? AND (? OR coalesce(json_extract(CAST(data AS TEXT), '$.archived'), 0) = 0)
		ORDER BY updated_at DESC LIMIT ?
	`, workspace, includeArchived, limit)
}

func (s *Store) query(ctx context.Context, statement string, args ...any) ([]Snapshot, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("session store is not open")
	}
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	var snapshots []Snapshot
	for rows.Next() {
		var snapshot Snapshot
		var updatedAt int64
		var data []byte
		if err := rows.Scan(&snapshot.ID, &snapshot.Workspace, &updatedAt, &data); err != nil {
			return nil, fmt.Errorf("read session: %w", err)
		}
		snapshot.UpdatedAt = time.Unix(0, updatedAt)
		snapshot.Data = append(json.RawMessage(nil), data...)
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read sessions: %w", err)
	}
	return snapshots, nil
}
