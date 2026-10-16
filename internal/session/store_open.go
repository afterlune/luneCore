package session

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const currentSchemaVersion = 13

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
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS tool_invocations (
		run_id TEXT NOT NULL,
		invocation_id TEXT NOT NULL,
		session_id TEXT NOT NULL,
		execution_id TEXT NOT NULL DEFAULT '',
		agent_id TEXT NOT NULL,
		call_id TEXT NOT NULL,
		name TEXT NOT NULL,
		arguments BLOB NOT NULL,
		status TEXT NOT NULL,
		output BLOB,
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (run_id, invocation_id)
	)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize tool invocation store: %w", err)
	}
	if version >= 2 && version < 3 {
		if _, err := tx.Exec(`ALTER TABLE tool_invocations ADD COLUMN execution_id TEXT NOT NULL DEFAULT ''`); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			return nil, fmt.Errorf("migrate tool invocation execution IDs: %w", err)
		}
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS tool_invocations_session ON tool_invocations(session_id, run_id)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize tool invocation index: %w", err)
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS queued_turn_inputs (
		session_id TEXT NOT NULL,
		queue_id TEXT NOT NULL,
		position INTEGER NOT NULL,
		execution_id TEXT NOT NULL,
		request BLOB NOT NULL,
		status TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (session_id, queue_id),
		UNIQUE (session_id, position),
		CHECK (status IN ('queued', 'active', 'paused'))
	)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize queued turn input store: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS queued_turn_inputs_session_status_position
		ON queued_turn_inputs(session_id, status, position)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize queued turn input index: %w", err)
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS runtime_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		payload BLOB NOT NULL,
		created_at INTEGER NOT NULL
	)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize runtime event log: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS runtime_events_session_id
		ON runtime_events(session_id, id)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize runtime event index: %w", err)
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS turn_results (
		session_id TEXT NOT NULL,
		execution_id TEXT NOT NULL,
		status TEXT NOT NULL,
		response BLOB,
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (session_id, execution_id)
	)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize turn result store: %w", err)
	}
	if version < 7 {
		if _, err := tx.Exec(`ALTER TABLE turn_results ADD COLUMN budget_reason TEXT NOT NULL DEFAULT ''`); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			return nil, fmt.Errorf("migrate turn result budget reason: %w", err)
		}
	}
	if version < 10 {
		if _, err := tx.Exec(`ALTER TABLE turn_results ADD COLUMN model_failure BLOB`); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			return nil, fmt.Errorf("migrate model failure: %w", err)
		}
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS user_question_drafts (
 session_id TEXT NOT NULL, run_id TEXT NOT NULL, invocation_id TEXT NOT NULL, call_id TEXT NOT NULL,
 payload BLOB NOT NULL, PRIMARY KEY(session_id,run_id,invocation_id,call_id)
 )`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize question drafts: %w", err)
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS graph_resume_inputs (
		session_id TEXT NOT NULL,
		run_id TEXT NOT NULL,
		invocation_id TEXT NOT NULL,
		call_id TEXT NOT NULL,
		payload BLOB NOT NULL,
		digest BLOB NOT NULL,
		consumed INTEGER NOT NULL DEFAULT 0 CHECK (consumed IN (0, 1)),
		created_at INTEGER NOT NULL,
		PRIMARY KEY (session_id, run_id, invocation_id, call_id)
	)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize graph resume input store: %w", err)
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS memories (
		id TEXT PRIMARY KEY,
		workspace TEXT NOT NULL,
		category TEXT NOT NULL,
		content TEXT NOT NULL,
		importance INTEGER NOT NULL,
		source_session_id TEXT NOT NULL DEFAULT '',
		source_execution_id TEXT NOT NULL DEFAULT '',
		source_ordinal INTEGER NOT NULL DEFAULT -1,
		embedder_id TEXT NOT NULL DEFAULT '',
		embedding BLOB,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize memory store: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS memories_workspace_updated
		ON memories(workspace, updated_at DESC)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize memory workspace index: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS memories_workspace_source_session
		ON memories(workspace, source_session_id)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize memory source index: %w", err)
	}
	if _, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS memories_execution_ordinal
		ON memories(workspace, source_execution_id, source_ordinal)
		WHERE source_execution_id <> ''`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize memory execution index: %w", err)
	}
	if _, err := tx.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
		content, content='memories', content_rowid='rowid', tokenize='trigram'
	)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize memory full-text index: %w", err)
	}
	for _, statement := range []string{
		`CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
			INSERT INTO memory_fts(rowid, content) VALUES (new.rowid, new.content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
			INSERT INTO memory_fts(memory_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE OF content ON memories BEGIN
			INSERT INTO memory_fts(memory_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
			INSERT INTO memory_fts(rowid, content) VALUES (new.rowid, new.content);
		END`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			return nil, fmt.Errorf("initialize memory full-text trigger: %w", err)
		}
	}
	if version < 9 {
		if _, err := tx.Exec(`INSERT INTO memory_fts(memory_fts) VALUES ('rebuild')`); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			return nil, fmt.Errorf("rebuild memory full-text index: %w", err)
		}
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS commands (
        session_id TEXT NOT NULL, id TEXT NOT NULL, execution_id TEXT NOT NULL,
        data BLOB NOT NULL, PRIMARY KEY (session_id, id)
    )`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize commands: %w", err)
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS execution_token_budgets (
		execution_id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		token_limit INTEGER NOT NULL CHECK (token_limit >= 0),
		updated_at INTEGER NOT NULL
	)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize execution token budgets: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS execution_token_budgets_session ON execution_token_budgets(session_id)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize execution token budget index: %w", err)
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS execution_token_reservations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		execution_id TEXT NOT NULL,
		reserved_tokens INTEGER NOT NULL CHECK (reserved_tokens >= 0),
		actual_tokens INTEGER CHECK (actual_tokens >= 0),
		created_at INTEGER NOT NULL
	)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize execution token reservations: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS execution_token_reservations_execution ON execution_token_reservations(execution_id)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("initialize execution token reservation index: %w", err)
	}
	if version < currentSchemaVersion {
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, currentSchemaVersion)); err != nil {
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
