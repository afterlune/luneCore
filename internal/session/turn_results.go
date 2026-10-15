package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type TurnResult struct {
	SessionID    string
	ExecutionID  string
	Status       string
	Response     json.RawMessage
	ModelFailure json.RawMessage
	BudgetReason string
	UpdatedAt    time.Time
}

func upsertTurnResultTx(ctx context.Context, tx *sql.Tx, sessionID, executionID, status string, response json.RawMessage, now time.Time, budgetReasons ...string) error {
	var payload any
	if len(response) > 0 {
		payload = []byte(response)
	}
	budgetReason := ""
	if len(budgetReasons) > 0 {
		budgetReason = budgetReasons[0]
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO turn_results(session_id, execution_id, status, response, budget_reason, updated_at)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, execution_id) DO UPDATE SET model_failure = NULL, status = excluded.status, response = excluded.response, budget_reason = excluded.budget_reason,
		updated_at = excluded.updated_at`,
		sessionID, executionID, status, payload, budgetReason, now.UnixNano()); err != nil {
		return fmt.Errorf("persist turn result: %w", err)
	}
	return nil
}

func (s *Store) TurnResult(ctx context.Context, sessionID, executionID string) (TurnResult, bool, error) {
	if s == nil || s.db == nil || sessionID == "" || executionID == "" {
		return TurnResult{}, false, errors.New("session store, session id, and execution id are required")
	}
	var result TurnResult
	var response []byte
	var modelFailure []byte
	var updatedAt int64
	err := s.db.QueryRowContext(ctx, `SELECT model_failure, session_id, execution_id, status, response, budget_reason, updated_at
		FROM turn_results WHERE session_id = ? AND execution_id = ?`, sessionID, executionID).
		Scan(&modelFailure, &result.SessionID, &result.ExecutionID, &result.Status, &response, &result.BudgetReason, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TurnResult{}, false, nil
	}
	if err != nil {
		return TurnResult{}, false, fmt.Errorf("load turn result: %w", err)
	}
	result.ModelFailure = append(json.RawMessage(nil), modelFailure...)
	result.Response = append(json.RawMessage(nil), response...)
	result.UpdatedAt = time.Unix(0, updatedAt)
	return result, true, nil
}
