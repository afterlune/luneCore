package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	QueuedTurnQueued = "queued"
	QueuedTurnActive = "active"
	QueuedTurnPaused = "paused"
)

type QueuedTurn struct {
	SessionID   string
	QueueID     string
	Position    int64
	ExecutionID string
	Request     json.RawMessage
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type RuntimeEvent struct {
	Cursor    int64
	SessionID string
	Payload   json.RawMessage
	CreatedAt time.Time
}

func (s *Store) EnqueueTurn(ctx context.Context, queued QueuedTurn, event json.RawMessage) (QueuedTurn, int64, error) {
	if s == nil || s.db == nil {
		return QueuedTurn{}, 0, errors.New("session store is not open")
	}
	if queued.SessionID == "" || queued.QueueID == "" || queued.ExecutionID == "" || !json.Valid(queued.Request) || !json.Valid(event) {
		return QueuedTurn{}, 0, errors.New("session id, queue id, execution id, and valid input/event JSON are required")
	}
	if queued.Status == "" {
		queued.Status = QueuedTurnQueued
	}
	if queued.Status != QueuedTurnQueued {
		return QueuedTurn{}, 0, fmt.Errorf("new queued turn status must be %q", QueuedTurnQueued)
	}
	now := time.Now()
	queued.CreatedAt = now
	queued.UpdatedAt = now
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return QueuedTurn{}, 0, fmt.Errorf("begin queued turn transaction: %w", err)
	}
	defer tx.Rollback()
	if queued.Position < 0 {
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(MIN(position), 0) - 1 FROM queued_turn_inputs WHERE session_id = ?`, queued.SessionID).Scan(&queued.Position); err != nil {
			return QueuedTurn{}, 0, fmt.Errorf("allocate queue position: %w", err)
		}
	} else if err := tx.QueryRowContext(ctx, `SELECT coalesce(MAX(position), 0) + 1 FROM queued_turn_inputs WHERE session_id = ?`, queued.SessionID).Scan(&queued.Position); err != nil {
		return QueuedTurn{}, 0, fmt.Errorf("allocate queue position: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO queued_turn_inputs(session_id, queue_id, position, execution_id, request, status, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, queued.SessionID, queued.QueueID, queued.Position, queued.ExecutionID, []byte(queued.Request), queued.Status, now.UnixNano(), now.UnixNano()); err != nil {
		return QueuedTurn{}, 0, fmt.Errorf("enqueue turn: %w", err)
	}
	cursor, err := appendRuntimeEventTx(ctx, tx, queued.SessionID, event, now)
	if err != nil {
		return QueuedTurn{}, 0, err
	}
	if err := tx.Commit(); err != nil {
		return QueuedTurn{}, 0, fmt.Errorf("commit queued turn: %w", err)
	}
	return queued, cursor, nil
}

func (s *Store) ClaimNextQueuedTurn(ctx context.Context, sessionID string) (QueuedTurn, bool, error) {
	if s == nil || s.db == nil || sessionID == "" {
		return QueuedTurn{}, false, errors.New("session store and session id are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return QueuedTurn{}, false, fmt.Errorf("begin queued turn claim: %w", err)
	}
	defer tx.Rollback()
	var queued QueuedTurn
	var request []byte
	var createdAt, updatedAt int64
	err = tx.QueryRowContext(ctx, `SELECT q.session_id, q.queue_id, q.position, q.execution_id, q.request, q.status, q.created_at, q.updated_at
		FROM queued_turn_inputs q WHERE q.session_id = ? AND q.status = ?
		AND NOT EXISTS (SELECT 1 FROM queued_turn_inputs x WHERE x.session_id = q.session_id AND x.status IN (?, ?))
		ORDER BY position LIMIT 1`, sessionID, QueuedTurnQueued, QueuedTurnActive, QueuedTurnPaused).
		Scan(&queued.SessionID, &queued.QueueID, &queued.Position, &queued.ExecutionID, &request, &queued.Status, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return QueuedTurn{}, false, nil
	}
	if err != nil {
		return QueuedTurn{}, false, fmt.Errorf("load next queued turn: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE queued_turn_inputs SET status = ?, updated_at = ?
		WHERE session_id = ? AND queue_id = ? AND status = ?`, QueuedTurnActive, time.Now().UnixNano(), sessionID, queued.QueueID, QueuedTurnQueued)
	if err != nil {
		return QueuedTurn{}, false, fmt.Errorf("claim queued turn: %w", err)
	}
	claimed, err := result.RowsAffected()
	if err != nil {
		return QueuedTurn{}, false, fmt.Errorf("check queued turn claim: %w", err)
	}
	if claimed == 0 {
		return QueuedTurn{}, false, nil
	}
	if err := tx.Commit(); err != nil {
		return QueuedTurn{}, false, fmt.Errorf("commit queued turn claim: %w", err)
	}
	queued.Request = append(json.RawMessage(nil), request...)
	queued.Status = QueuedTurnActive
	queued.CreatedAt = time.Unix(0, createdAt)
	queued.UpdatedAt = time.Now()
	return queued, true, nil
}

func (s *Store) QueuedTurns(ctx context.Context, sessionID string) ([]QueuedTurn, error) {
	if s == nil || s.db == nil || sessionID == "" {
		return nil, errors.New("session store and session id are required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, queue_id, position, execution_id, request, status, created_at, updated_at
		FROM queued_turn_inputs WHERE session_id = ? ORDER BY position`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list queued turns: %w", err)
	}
	defer rows.Close()
	var queued []QueuedTurn
	for rows.Next() {
		var item QueuedTurn
		var request []byte
		var createdAt, updatedAt int64
		if err := rows.Scan(&item.SessionID, &item.QueueID, &item.Position, &item.ExecutionID, &request, &item.Status, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("read queued turn: %w", err)
		}
		item.Request = append(json.RawMessage(nil), request...)
		item.CreatedAt = time.Unix(0, createdAt)
		item.UpdatedAt = time.Unix(0, updatedAt)
		queued = append(queued, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read queued turns: %w", err)
	}
	return queued, nil
}

func (s *Store) QueuedTurnByExecutionID(ctx context.Context, sessionID, executionID string) (QueuedTurn, bool, error) {
	if s == nil || s.db == nil || sessionID == "" || executionID == "" {
		return QueuedTurn{}, false, errors.New("session store, session id, and execution id are required")
	}
	var queued QueuedTurn
	var request []byte
	var createdAt, updatedAt int64
	err := s.db.QueryRowContext(ctx, `SELECT session_id, queue_id, position, execution_id, request, status, created_at, updated_at
		FROM queued_turn_inputs WHERE session_id = ? AND execution_id = ? LIMIT 1`, sessionID, executionID).
		Scan(&queued.SessionID, &queued.QueueID, &queued.Position, &queued.ExecutionID, &request, &queued.Status, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return QueuedTurn{}, false, nil
	}
	if err != nil {
		return QueuedTurn{}, false, fmt.Errorf("load queued turn by execution id: %w", err)
	}
	queued.Request = append(json.RawMessage(nil), request...)
	queued.CreatedAt = time.Unix(0, createdAt)
	queued.UpdatedAt = time.Unix(0, updatedAt)
	return queued, true, nil
}

func (s *Store) SetQueuedTurnStatus(ctx context.Context, sessionID, queueID, status string) error {
	if s == nil || s.db == nil || sessionID == "" || queueID == "" {
		return errors.New("session store, session id, and queue id are required")
	}
	if status != QueuedTurnQueued && status != QueuedTurnActive && status != QueuedTurnPaused {
		return fmt.Errorf("unsupported queued turn status %q", status)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE queued_turn_inputs SET status = ?, updated_at = ? WHERE session_id = ? AND queue_id = ?`,
		status, time.Now().UnixNano(), sessionID, queueID)
	if err != nil {
		return fmt.Errorf("update queued turn status: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check queued turn status update: %w", err)
	}
	if updated == 0 {
		return fmt.Errorf("queued turn %q does not exist in session %q", queueID, sessionID)
	}
	return nil
}

func (s *Store) SetQueuedTurnWithEvent(ctx context.Context, sessionID, queueID, status string, request, event json.RawMessage) (int64, error) {
	if s == nil || s.db == nil || sessionID == "" || queueID == "" || !json.Valid(request) || !json.Valid(event) {
		return 0, errors.New("session store, session id, queue id, and valid request/event JSON are required")
	}
	if status != QueuedTurnQueued && status != QueuedTurnActive && status != QueuedTurnPaused {
		return 0, fmt.Errorf("unsupported queued turn status %q", status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin queued turn update: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE queued_turn_inputs SET status = ?, request = ?, updated_at = ?
		WHERE session_id = ? AND queue_id = ?`, status, []byte(request), time.Now().UnixNano(), sessionID, queueID)
	if err != nil {
		return 0, fmt.Errorf("update queued turn: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("check queued turn update: %w", err)
	}
	if updated == 0 {
		return 0, fmt.Errorf("queued turn %q does not exist in session %q", queueID, sessionID)
	}
	cursor, err := appendRuntimeEventTx(ctx, tx, sessionID, event, time.Now())
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit queued turn update: %w", err)
	}
	return cursor, nil
}

func (s *Store) DiscardQueuedTurn(ctx context.Context, sessionID, queueID string, event json.RawMessage) (bool, int64, error) {
	if s == nil || s.db == nil || sessionID == "" || queueID == "" || !json.Valid(event) {
		return false, 0, errors.New("session store, session id, queue id, and valid event JSON are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, fmt.Errorf("begin queued turn discard: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM queued_turn_inputs WHERE session_id = ? AND queue_id = ? AND status = ?`, sessionID, queueID, QueuedTurnPaused)
	if err != nil {
		return false, 0, fmt.Errorf("discard paused turn: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return false, 0, fmt.Errorf("check paused turn discard: %w", err)
	}
	if deleted == 0 {
		return false, 0, nil
	}
	cursor, err := appendRuntimeEventTx(ctx, tx, sessionID, event, time.Now())
	if err != nil {
		return false, 0, err
	}
	if err := tx.Commit(); err != nil {
		return false, 0, fmt.Errorf("commit paused turn discard: %w", err)
	}
	return true, cursor, nil
}

func (s *Store) SetQueuedTurnStatusWithEvent(ctx context.Context, sessionID, queueID, status string, event json.RawMessage) (int64, error) {
	if s == nil || s.db == nil || sessionID == "" || queueID == "" || !json.Valid(event) {
		return 0, errors.New("session store, session id, queue id, and valid event JSON are required")
	}
	if status != QueuedTurnQueued && status != QueuedTurnActive && status != QueuedTurnPaused {
		return 0, fmt.Errorf("unsupported queued turn status %q", status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin queued turn status event: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE queued_turn_inputs SET status = ?, updated_at = ? WHERE session_id = ? AND queue_id = ?`,
		status, time.Now().UnixNano(), sessionID, queueID)
	if err != nil {
		return 0, fmt.Errorf("update queued turn status: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("check queued turn status update: %w", err)
	}
	if updated == 0 {
		return 0, fmt.Errorf("queued turn %q does not exist in session %q", queueID, sessionID)
	}
	cursor, err := appendRuntimeEventTx(ctx, tx, sessionID, event, time.Now())
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit queued turn status event: %w", err)
	}
	return cursor, nil
}

func (s *Store) FinishQueuedTurn(ctx context.Context, sessionID, queueID string, event json.RawMessage) (int64, error) {
	if s == nil || s.db == nil || sessionID == "" || queueID == "" || !json.Valid(event) {
		return 0, errors.New("session store, session id, queue id, and valid event JSON are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin queued turn completion: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM queued_turn_inputs WHERE session_id = ? AND queue_id = ?`, sessionID, queueID)
	if err != nil {
		return 0, fmt.Errorf("finish queued turn: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("check queued turn completion: %w", err)
	}
	if deleted == 0 {
		return 0, fmt.Errorf("queued turn %q does not exist in session %q", queueID, sessionID)
	}
	cursor, err := appendRuntimeEventTx(ctx, tx, sessionID, event, time.Now())
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit queued turn completion: %w", err)
	}
	return cursor, nil
}

func (s *Store) CancelQueuedTurn(ctx context.Context, sessionID, queueID string, event json.RawMessage) (bool, int64, error) {
	if s == nil || s.db == nil || sessionID == "" || queueID == "" || !json.Valid(event) {
		return false, 0, errors.New("session store, session id, queue id, and valid event JSON are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, fmt.Errorf("begin queued turn cancellation: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM queued_turn_inputs WHERE session_id = ? AND queue_id = ? AND status = ?`, sessionID, queueID, QueuedTurnQueued)
	if err != nil {
		return false, 0, fmt.Errorf("cancel queued turn: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return false, 0, fmt.Errorf("check queued turn cancellation: %w", err)
	}
	if deleted == 0 {
		return false, 0, nil
	}
	cursor, err := appendRuntimeEventTx(ctx, tx, sessionID, event, time.Now())
	if err != nil {
		return false, 0, err
	}
	if err := tx.Commit(); err != nil {
		return false, 0, fmt.Errorf("commit queued turn cancellation: %w", err)
	}
	return true, cursor, nil
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
