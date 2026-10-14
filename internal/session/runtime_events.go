package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type RuntimeEvent struct {
	Cursor    int64
	SessionID string
	Payload   json.RawMessage
	CreatedAt time.Time
}

func (s *Store) AppendRuntimeEvent(ctx context.Context, sessionID string, payload json.RawMessage) (int64, error) {
	if s == nil || s.db == nil || sessionID == "" || !json.Valid(payload) {
		return 0, errors.New("session store, session id, and valid event JSON are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin runtime event append: %w", err)
	}
	defer tx.Rollback()
	cursor, err := appendRuntimeEventTx(ctx, tx, sessionID, payload, time.Now())
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit runtime event: %w", err)
	}
	return cursor, nil
}

func appendRuntimeEventTx(ctx context.Context, tx *sql.Tx, sessionID string, payload json.RawMessage, createdAt time.Time) (int64, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO runtime_events(session_id, payload, created_at) VALUES(?, ?, ?)`, sessionID, []byte(payload), createdAt.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("append runtime event: %w", err)
	}
	cursor, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read runtime event cursor: %w", err)
	}
	return cursor, nil
}

func (s *Store) RuntimeEventsAfter(ctx context.Context, sessionID string, cursor int64, limit int) ([]RuntimeEvent, error) {
	if s == nil || s.db == nil || sessionID == "" {
		return nil, errors.New("session store and session id are required")
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, session_id, payload, created_at FROM runtime_events
		WHERE session_id = ? AND id > ? ORDER BY id LIMIT ?`, sessionID, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("read runtime events: %w", err)
	}
	defer rows.Close()
	var events []RuntimeEvent
	for rows.Next() {
		var event RuntimeEvent
		var payload []byte
		var createdAt int64
		if err := rows.Scan(&event.Cursor, &event.SessionID, &payload, &createdAt); err != nil {
			return nil, fmt.Errorf("read runtime event: %w", err)
		}
		event.Payload = append(json.RawMessage(nil), payload...)
		event.CreatedAt = time.Unix(0, createdAt)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read runtime events: %w", err)
	}
	return events, nil
}

func (s *Store) LatestRuntimeEventCursor(ctx context.Context, sessionID string) (int64, error) {
	if s == nil || s.db == nil || sessionID == "" {
		return 0, errors.New("session store and session id are required")
	}
	var cursor int64
	if err := s.db.QueryRowContext(ctx, `SELECT coalesce(MAX(id), 0) FROM runtime_events WHERE session_id = ?`, sessionID).Scan(&cursor); err != nil {
		return 0, fmt.Errorf("read latest runtime event cursor: %w", err)
	}
	return cursor, nil
}
