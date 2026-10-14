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
	SessionID          string
	ExecutionID        string
	Status             string
	Response           json.RawMessage
	ModelFailure       json.RawMessage
	AcceptanceReports  json.RawMessage
	AcceptanceCriteria json.RawMessage
	AcceptanceEvidence json.RawMessage
	BudgetReason       string
	UpdatedAt          time.Time
}

func upsertTurnResultTx(ctx context.Context, tx *sql.Tx, sessionID, executionID, status string, response json.RawMessage, now time.Time, budgetReasons ...string) error {
	return upsertTurnResultWithAcceptanceTx(ctx, tx, sessionID, executionID, status, response, now, nil, nil, nil, budgetReasons...)
}

func upsertTurnResultWithAcceptanceTx(ctx context.Context, tx *sql.Tx, sessionID, executionID, status string, response json.RawMessage, now time.Time, acceptanceReports, acceptanceCriteria, acceptanceEvidence json.RawMessage, budgetReasons ...string) error {
	var payload any
	if len(response) > 0 {
		payload = []byte(response)
	}
	budgetReason := ""
	if len(budgetReasons) > 0 {
		budgetReason = budgetReasons[0]
	}
	var reports any
	var criteria any
	var evidence any
	if len(acceptanceReports) > 0 {
		reports = []byte(acceptanceReports)
	}
	if len(acceptanceCriteria) > 0 {
		criteria = []byte(acceptanceCriteria)
	}
	if len(acceptanceEvidence) > 0 {
		evidence = []byte(acceptanceEvidence)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO turn_results(session_id, execution_id, status, response, budget_reason, acceptance_reports, acceptance_criteria, acceptance_evidence, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, execution_id) DO UPDATE SET model_failure = NULL, status = excluded.status, response = excluded.response, budget_reason = excluded.budget_reason,
		acceptance_reports = CASE WHEN excluded.acceptance_reports IS NULL THEN turn_results.acceptance_reports ELSE excluded.acceptance_reports END,
		acceptance_criteria = CASE WHEN excluded.acceptance_criteria IS NULL THEN turn_results.acceptance_criteria ELSE excluded.acceptance_criteria END,
		acceptance_evidence = CASE WHEN excluded.acceptance_evidence IS NULL THEN turn_results.acceptance_evidence ELSE excluded.acceptance_evidence END,
		updated_at = excluded.updated_at`,
		sessionID, executionID, status, payload, budgetReason, reports, criteria, evidence, now.UnixNano()); err != nil {
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
	var acceptanceReports, acceptanceCriteria, acceptanceEvidence, modelFailure []byte
	var updatedAt int64
	err := s.db.QueryRowContext(ctx, `SELECT model_failure, session_id, execution_id, status, response, budget_reason, acceptance_reports, acceptance_criteria, acceptance_evidence, updated_at
		FROM turn_results WHERE session_id = ? AND execution_id = ?`, sessionID, executionID).
		Scan(&modelFailure, &result.SessionID, &result.ExecutionID, &result.Status, &response, &result.BudgetReason, &acceptanceReports, &acceptanceCriteria, &acceptanceEvidence, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TurnResult{}, false, nil
	}
	if err != nil {
		return TurnResult{}, false, fmt.Errorf("load turn result: %w", err)
	}
	result.ModelFailure = append(json.RawMessage(nil), modelFailure...)
	result.Response = append(json.RawMessage(nil), response...)
	result.AcceptanceReports = append(json.RawMessage(nil), acceptanceReports...)
	result.AcceptanceCriteria = append(json.RawMessage(nil), acceptanceCriteria...)
	result.AcceptanceEvidence = append(json.RawMessage(nil), acceptanceEvidence...)
	result.UpdatedAt = time.Unix(0, updatedAt)
	return result, true, nil
}
