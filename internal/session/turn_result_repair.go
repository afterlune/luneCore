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

// RepairFailedInterruptedTurn corrects one legacy failed result and its exact
// terminal event. Both rows are rechecked and updated in one transaction.
func (s *Store) RepairFailedInterruptedTurn(ctx context.Context, sessionID, executionID string, eventCursor int64, expectedEvent, correctedEvent json.RawMessage) (bool, error) {
	if s == nil || s.db == nil || ctx == nil || sessionID == "" || executionID == "" || eventCursor <= 0 || !json.Valid(expectedEvent) || !json.Valid(correctedEvent) {
		return false, errors.New("session, execution, event cursor, and valid event payloads are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin interrupted turn repair: %w", err)
	}
	defer tx.Rollback()

	var status string
	var response []byte
	err = tx.QueryRowContext(ctx, `SELECT status, response FROM turn_results WHERE session_id = ? AND execution_id = ?`, sessionID, executionID).Scan(&status, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect interrupted turn result: %w", err)
	}
	if status != "failed" || response != nil {
		return false, nil
	}
	var storedEvent []byte
	err = tx.QueryRowContext(ctx, `SELECT payload FROM runtime_events WHERE id = ? AND session_id = ?`, eventCursor, sessionID).Scan(&storedEvent)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect interrupted turn event: %w", err)
	}
	if !bytes.Equal(storedEvent, expectedEvent) {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runtime_events SET payload = ? WHERE id = ? AND session_id = ?`, []byte(correctedEvent), eventCursor, sessionID); err != nil {
		return false, fmt.Errorf("correct interrupted turn event: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE turn_results SET status = 'cancelled', response = NULL, model_failure = NULL, budget_reason = '', updated_at = ?
		WHERE session_id = ? AND execution_id = ? AND status = 'failed' AND response IS NULL`, time.Now().UnixNano(), sessionID, executionID)
	if err != nil {
		return false, fmt.Errorf("correct interrupted turn result: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check interrupted turn result correction: %w", err)
	}
	if updated != 1 {
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit interrupted turn repair: %w", err)
	}
	return true, nil
}
