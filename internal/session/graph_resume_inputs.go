package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GraphResumeInput is one durable payload addressed to an exact graph wait.
type GraphResumeInput struct {
	RunID        string
	InvocationID string
	CallID       string
	Payload      []byte
}

// PutGraphResumeInput stores one answer before its graph continuation runs.
// Identical retries are idempotent; a different payload for the same wait is rejected.
func (s *Store) PutGraphResumeInput(ctx context.Context, sessionID string, input GraphResumeInput) (bool, error) {
	if s == nil || s.db == nil || ctx == nil || sessionID == "" || input.RunID == "" || input.InvocationID == "" || input.CallID == "" || len(input.Payload) == 0 {
		return false, errors.New("session store, context, wait identity, and payload are required")
	}
	digest := sha256.Sum256(input.Payload)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin graph resume input: %w", err)
	}
	defer tx.Rollback()
	var storedDigest []byte
	err = tx.QueryRowContext(ctx, `SELECT digest FROM graph_resume_inputs
		WHERE session_id = ? AND run_id = ? AND invocation_id = ? AND call_id = ?`,
		sessionID, input.RunID, input.InvocationID, input.CallID).Scan(&storedDigest)
	if err == nil {
		if !bytes.Equal(storedDigest, digest[:]) {
			return false, errors.New("a different answer is already recorded for this user question")
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("commit graph resume input check: %w", err)
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("inspect graph resume input: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO graph_resume_inputs
		(session_id, run_id, invocation_id, call_id, payload, digest, consumed, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?)`, sessionID, input.RunID, input.InvocationID, input.CallID, input.Payload, digest[:], time.Now().UnixNano())
	if err != nil {
		return false, fmt.Errorf("save graph resume input: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit graph resume input: %w", err)
	}
	return true, nil
}

// GraphResumeInputs returns inputs not yet confirmed by the graph checkpoint.
func (s *Store) GraphResumeInputs(ctx context.Context, sessionID, runID string) ([]GraphResumeInput, error) {
	if s == nil || s.db == nil || ctx == nil || sessionID == "" || runID == "" {
		return nil, errors.New("session store, context, session id, and run id are required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT invocation_id, call_id, payload FROM graph_resume_inputs
		WHERE session_id = ? AND run_id = ? AND consumed = 0 ORDER BY created_at, invocation_id, call_id`, sessionID, runID)
	if err != nil {
		return nil, fmt.Errorf("load graph resume inputs: %w", err)
	}
	defer rows.Close()
	var inputs []GraphResumeInput
	for rows.Next() {
		var input GraphResumeInput
		input.RunID = runID
		if err := rows.Scan(&input.InvocationID, &input.CallID, &input.Payload); err != nil {
			return nil, fmt.Errorf("scan graph resume input: %w", err)
		}
		inputs = append(inputs, input)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read graph resume inputs: %w", err)
	}
	return inputs, nil
}

// MarkGraphResumeInputConsumed records that the checkpoint moved past a wait.
// The digest remains as a small receipt so a retried identical answer succeeds.
func (s *Store) MarkGraphResumeInputConsumed(ctx context.Context, sessionID string, input GraphResumeInput) error {
	if s == nil || s.db == nil || ctx == nil || sessionID == "" || input.RunID == "" || input.InvocationID == "" || input.CallID == "" {
		return errors.New("session store, context, and wait identity are required")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE graph_resume_inputs SET consumed = 1, payload = X''
		WHERE session_id = ? AND run_id = ? AND invocation_id = ? AND call_id = ?`,
		sessionID, input.RunID, input.InvocationID, input.CallID)
	if err != nil {
		return fmt.Errorf("mark graph resume input consumed: %w", err)
	}
	return nil
}

func (s *Store) DeleteGraphResumeInput(ctx context.Context, sessionID string, input GraphResumeInput) error {
	if s == nil || s.db == nil || ctx == nil || sessionID == "" || input.RunID == "" || input.InvocationID == "" || input.CallID == "" {
		return errors.New("session store, context, and wait identity are required")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM graph_resume_inputs
		WHERE session_id = ? AND run_id = ? AND invocation_id = ? AND call_id = ?`, sessionID, input.RunID, input.InvocationID, input.CallID); err != nil {
		return fmt.Errorf("delete graph resume input: %w", err)
	}
	return nil
}

func (s *Store) DeleteGraphResumeInputs(ctx context.Context, sessionID, runID string) error {
	if s == nil || s.db == nil || ctx == nil || sessionID == "" || runID == "" {
		return errors.New("session store, context, session id, and run id are required")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM graph_resume_inputs WHERE session_id = ? AND run_id = ?`, sessionID, runID); err != nil {
		return fmt.Errorf("delete graph resume inputs: %w", err)
	}
	return nil
}
