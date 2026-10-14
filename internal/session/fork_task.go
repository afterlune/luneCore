package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ForkPausedTask copies the durable side-effect and continuation receipts for
// a cloned checkpoint and creates its paused queue entry in one transaction.
func (s *Store) ForkPausedTask(ctx context.Context, sourceSessionID, sourceRunID, sourceExecutionID, forkSessionID, forkRunID, forkExecutionID, forkQueueID string, request, event json.RawMessage) error {
	if s == nil || s.db == nil || ctx == nil || sourceSessionID == "" || sourceRunID == "" || sourceExecutionID == "" ||
		forkSessionID == "" || forkRunID == "" || forkExecutionID == "" || forkQueueID == "" || !json.Valid(request) || !json.Valid(event) {
		return errors.New("session store, source and fork identities, and valid request/event JSON are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin paused task fork: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().UnixNano()
	if _, err := tx.ExecContext(ctx, `INSERT INTO commands(session_id,id,execution_id,data) SELECT ?,id,?,CAST(json_set(CAST(data AS TEXT),'$.info.session_id',?,'$.info.execution_id',?,'$.info.task_id','') AS BLOB) FROM commands WHERE session_id=? AND execution_id=?`, forkSessionID, forkExecutionID, forkSessionID, forkExecutionID, sourceSessionID, sourceExecutionID); err != nil {
		return fmt.Errorf("copy command records: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO tool_invocations(
		run_id, invocation_id, session_id, execution_id, agent_id, call_id, name, arguments, status, output, updated_at
	) SELECT ?, invocation_id, ?, ?, agent_id, call_id, name, arguments, status, output, updated_at
		FROM tool_invocations WHERE session_id = ? AND run_id = ? AND execution_id = ?`,
		forkRunID, forkSessionID, forkExecutionID, sourceSessionID, sourceRunID, sourceExecutionID); err != nil {
		return fmt.Errorf("copy forked task tool receipts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO graph_resume_inputs(
		session_id, run_id, invocation_id, call_id, payload, digest, consumed, created_at
	) SELECT ?, ?, invocation_id, call_id, payload, digest, consumed, created_at
		FROM graph_resume_inputs WHERE session_id = ? AND run_id = ?`,
		forkSessionID, forkRunID, sourceSessionID, sourceRunID); err != nil {
		return fmt.Errorf("copy forked task continuation receipts: %w", err)
	}

	var position int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(MAX(position), -1) + 1 FROM queued_turn_inputs WHERE session_id = ?`, forkSessionID).Scan(&position); err != nil {
		return fmt.Errorf("allocate forked paused turn position: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO queued_turn_inputs(
		session_id, queue_id, position, execution_id, request, status, created_at, updated_at
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, forkSessionID, forkQueueID, position, forkExecutionID, []byte(request), QueuedTurnPaused, now, now); err != nil {
		return fmt.Errorf("create forked paused turn: %w", err)
	}
	var budgetReason string
	var acceptanceReports, acceptanceCriteria, acceptanceEvidence, modelFailure []byte
	err = tx.QueryRowContext(ctx, `SELECT model_failure, budget_reason, acceptance_reports, acceptance_criteria, acceptance_evidence FROM turn_results WHERE session_id = ? AND execution_id = ?`, sourceSessionID, sourceExecutionID).Scan(&modelFailure, &budgetReason, &acceptanceReports, &acceptanceCriteria, &acceptanceEvidence)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read source paused turn result: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO turn_results(session_id, execution_id, status, response, model_failure, budget_reason, acceptance_reports, acceptance_criteria, acceptance_evidence, updated_at)
		VALUES(?, ?, ?, NULL, ?, ?, ?, ?, ?, ?)`, forkSessionID, forkExecutionID, "paused", modelFailure, budgetReason, acceptanceReports, acceptanceCriteria, acceptanceEvidence, now); err != nil {
		return fmt.Errorf("create forked paused turn result: %w", err)
	}
	if _, err := appendRuntimeEventTx(ctx, tx, forkSessionID, event, time.Now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit paused task fork: %w", err)
	}
	return nil
}
