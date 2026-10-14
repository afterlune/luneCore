package session

import (
	"bytes"
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
	if status == QueuedTurnQueued {
		if _, err := tx.ExecContext(ctx, `DELETE FROM turn_results WHERE session_id = ? AND execution_id = (
			SELECT execution_id FROM queued_turn_inputs WHERE session_id = ? AND queue_id = ?
		)`, sessionID, sessionID, queueID); err != nil {
			return 0, fmt.Errorf("clear paused turn result: %w", err)
		}
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

// PauseQueuedTurn stores the resumable state and the observable paused result
// together. Recovering the turn clears this result before it is queued again.
func (s *Store) PauseQueuedTurn(ctx context.Context, sessionID, queueID, executionID, status string, event json.RawMessage) (int64, error) {
	return s.PauseQueuedTurnWithReason(ctx, sessionID, queueID, executionID, status, "", event)
}

func (s *Store) PauseQueuedTurnWithReason(ctx context.Context, sessionID, queueID, executionID, status, budgetReason string, event json.RawMessage, artifacts ...json.RawMessage) (int64, error) {
	if s == nil || s.db == nil || sessionID == "" || queueID == "" || executionID == "" || status == "" || !json.Valid(event) {
		return 0, errors.New("session store, turn identity, status, and valid event JSON are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin paused turn update: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE queued_turn_inputs SET status = ?, updated_at = ?
		WHERE session_id = ? AND queue_id = ? AND execution_id = ?`, QueuedTurnPaused, time.Now().UnixNano(), sessionID, queueID, executionID)
	if err != nil {
		return 0, fmt.Errorf("pause queued turn: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("check paused turn update: %w", err)
	}
	if updated == 0 {
		return 0, fmt.Errorf("queued turn %q does not exist in session %q", queueID, sessionID)
	}
	var acceptanceReports, acceptanceCriteria, acceptanceEvidence json.RawMessage
	if len(artifacts) > 0 {
		acceptanceReports = artifacts[0]
	}
	if len(artifacts) > 1 {
		acceptanceCriteria = artifacts[1]
	}
	if len(artifacts) > 2 {
		acceptanceEvidence = artifacts[2]
	}
	if err := upsertTurnResultWithAcceptanceTx(ctx, tx, sessionID, executionID, status, nil, time.Now(), acceptanceReports, acceptanceCriteria, acceptanceEvidence, budgetReason); err != nil {
		return 0, err
	}
	if len(artifacts) > 3 && len(artifacts[3]) > 0 {
		if !json.Valid(artifacts[3]) {
			return 0, errors.New("invalid model failure JSON")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE turn_results SET model_failure = ? WHERE session_id = ? AND execution_id = ?`, []byte(artifacts[3]), sessionID, executionID); err != nil {
			return 0, err
		}
	}
	cursor, err := appendRuntimeEventTx(ctx, tx, sessionID, event, time.Now())
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit paused turn update: %w", err)
	}
	return cursor, nil
}

// CompleteQueuedTurn atomically saves conversation state, the durable result,
// the terminal event, and queue removal.
func (s *Store) CompleteQueuedTurn(ctx context.Context, sessionID, queueID, executionID, status string, response, history, modelContext json.RawMessage, contextKey string, event json.RawMessage) (int64, error) {
	return s.CompleteQueuedTurnWithAcceptance(ctx, sessionID, queueID, executionID, status, response, history, modelContext, contextKey, nil, nil, nil, event)
}

// CompleteQueuedTurnWithAcceptance atomically saves acceptance details alongside
// its conversation, terminal event, and queue removal.
func (s *Store) CompleteQueuedTurnWithAcceptance(ctx context.Context, sessionID, queueID, executionID, status string, response, history, modelContext json.RawMessage, contextKey string, acceptanceReports, acceptanceCriteria, acceptanceEvidence, event json.RawMessage) (int64, error) {
	if s == nil || s.db == nil || sessionID == "" || queueID == "" || executionID == "" || status == "" || !json.Valid(event) {
		return 0, errors.New("session store, turn identity, status, and valid event JSON are required")
	}
	if len(response) > 0 && !json.Valid(response) || len(history) > 0 && !json.Valid(history) || len(modelContext) > 0 && !json.Valid(modelContext) || len(acceptanceReports) > 0 && !json.Valid(acceptanceReports) || len(acceptanceCriteria) > 0 && !json.Valid(acceptanceCriteria) || len(acceptanceEvidence) > 0 && !json.Valid(acceptanceEvidence) {
		return 0, errors.New("turn response and conversation must be valid JSON")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin turn completion: %w", err)
	}
	defer tx.Rollback()
	if len(history) > 0 {
		if len(modelContext) == 0 {
			modelContext = json.RawMessage("null")
		}
		if bytes.Equal(history, modelContext) {
			modelContext = json.RawMessage("null")
			contextKey = ""
		}
		result, err := tx.ExecContext(ctx, `UPDATE sessions
			SET data = CAST(json_set(
				json_set(
					json_set(CAST(data AS TEXT), '$.history', json(?)),
					'$.model_context', json(?)),
				'$.model_context_key', ?) AS BLOB), updated_at = ?
			WHERE id = ?`, string(history), string(modelContext), contextKey, time.Now().UnixNano(), sessionID)
		if err != nil {
			return 0, fmt.Errorf("persist completed conversation for session %q: %w", sessionID, err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("check completed conversation update: %w", err)
		}
		if updated == 0 {
			return 0, fmt.Errorf("session %q does not exist", sessionID)
		}
	}
	if err := upsertTurnResultWithAcceptanceTx(ctx, tx, sessionID, executionID, status, response, time.Now(), acceptanceReports, acceptanceCriteria, acceptanceEvidence); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM queued_turn_inputs WHERE session_id = ? AND queue_id = ? AND execution_id = ?`, sessionID, queueID, executionID)
	if err != nil {
		return 0, fmt.Errorf("remove completed turn from queue: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("check completed turn removal: %w", err)
	}
	if deleted == 0 {
		return 0, fmt.Errorf("queued turn %q does not exist in session %q", queueID, sessionID)
	}
	cursor, err := appendRuntimeEventTx(ctx, tx, sessionID, event, time.Now())
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit turn completion: %w", err)
	}
	return cursor, nil
}

func (s *Store) DiscardQueuedTurn(ctx context.Context, sessionID, queueID, executionID, status string, event json.RawMessage) (bool, int64, error) {
	if s == nil || s.db == nil || sessionID == "" || queueID == "" || executionID == "" || status == "" || !json.Valid(event) {
		return false, 0, errors.New("session store, turn identity, status, and valid event JSON are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, fmt.Errorf("begin queued turn discard: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM queued_turn_inputs WHERE session_id = ? AND queue_id = ? AND execution_id = ? AND status = ?`, sessionID, queueID, executionID, QueuedTurnPaused)
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
	if err := upsertTurnResultTx(ctx, tx, sessionID, executionID, status, nil, time.Now()); err != nil {
		return false, 0, err
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

func (s *Store) CancelQueuedTurn(ctx context.Context, sessionID, queueID, executionID, status string, event json.RawMessage) (bool, int64, error) {
	if s == nil || s.db == nil || sessionID == "" || queueID == "" || executionID == "" || status == "" || !json.Valid(event) {
		return false, 0, errors.New("session store, turn identity, status, and valid event JSON are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, fmt.Errorf("begin queued turn cancellation: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM queued_turn_inputs WHERE session_id = ? AND queue_id = ? AND execution_id = ? AND status = ?`, sessionID, queueID, executionID, QueuedTurnQueued)
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
	if err := upsertTurnResultTx(ctx, tx, sessionID, executionID, status, nil, time.Now()); err != nil {
		return false, 0, err
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
