package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	ToolInvocationRunning   = "running"
	ToolInvocationUnknown   = "unknown"
	ToolInvocationRetry     = "retry"
	ToolInvocationCompleted = "completed"
	ToolInvocationSkipped   = "skipped"
)

type ToolInvocation struct {
	RunID        string
	InvocationID string
	SessionID    string
	ExecutionID  string
	AgentID      string
	CallID       string
	Name         string
	Arguments    json.RawMessage
	Status       string
	Output       json.RawMessage
	UpdatedAt    time.Time
}

// StartToolInvocation records the invocation before it can cause a side effect.
// A false created result means a prior attempt already has a durable record.
func (s *Store) StartToolInvocation(ctx context.Context, invocation ToolInvocation) (ToolInvocation, bool, error) {
	if s == nil || s.db == nil {
		return ToolInvocation{}, false, errors.New("session store is not open")
	}
	if invocation.RunID == "" || invocation.InvocationID == "" || invocation.SessionID == "" || invocation.ExecutionID == "" || invocation.CallID == "" || invocation.Name == "" || !json.Valid(invocation.Arguments) {
		return ToolInvocation{}, false, errors.New("tool invocation identity, name, and valid arguments are required")
	}
	now := time.Now()
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO tool_invocations(
		run_id, invocation_id, session_id, execution_id, agent_id, call_id, name, arguments, status, output, updated_at
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`, invocation.RunID, invocation.InvocationID, invocation.SessionID,
		invocation.ExecutionID, invocation.AgentID, invocation.CallID, invocation.Name, []byte(invocation.Arguments), ToolInvocationRunning, now.UnixNano())
	if err != nil {
		return ToolInvocation{}, false, fmt.Errorf("record tool invocation: %w", err)
	}
	createdCount, err := result.RowsAffected()
	if err != nil {
		return ToolInvocation{}, false, fmt.Errorf("check tool invocation insert: %w", err)
	}
	stored, err := s.GetToolInvocation(ctx, invocation.RunID, invocation.InvocationID)
	if err != nil {
		return ToolInvocation{}, false, err
	}
	created := createdCount > 0
	if !created && (stored.SessionID != invocation.SessionID || stored.ExecutionID != invocation.ExecutionID || stored.AgentID != invocation.AgentID || stored.CallID != invocation.CallID || stored.Name != invocation.Name || !bytes.Equal(stored.Arguments, invocation.Arguments)) {
		return ToolInvocation{}, false, errors.New("tool invocation ID conflicts with a previously recorded operation")
	}
	return stored, created, nil
}

func (s *Store) GetToolInvocation(ctx context.Context, runID, invocationID string) (ToolInvocation, error) {
	if s == nil || s.db == nil {
		return ToolInvocation{}, errors.New("session store is not open")
	}
	var invocation ToolInvocation
	var arguments, output []byte
	var updatedAt int64
	err := s.db.QueryRowContext(ctx, `SELECT run_id, invocation_id, session_id, execution_id, agent_id, call_id, name, arguments, status, output, updated_at
		FROM tool_invocations WHERE run_id = ? AND invocation_id = ?`, runID, invocationID).
		Scan(&invocation.RunID, &invocation.InvocationID, &invocation.SessionID, &invocation.ExecutionID, &invocation.AgentID, &invocation.CallID,
			&invocation.Name, &arguments, &invocation.Status, &output, &updatedAt)
	if err != nil {
		return ToolInvocation{}, fmt.Errorf("load tool invocation: %w", err)
	}
	invocation.Arguments = append(json.RawMessage(nil), arguments...)
	invocation.Output = append(json.RawMessage(nil), output...)
	invocation.UpdatedAt = time.Unix(0, updatedAt)
	return invocation, nil
}

func (s *Store) GetToolInvocationForSession(ctx context.Context, sessionID, invocationID string) (ToolInvocation, error) {
	if s == nil || s.db == nil {
		return ToolInvocation{}, errors.New("session store is not open")
	}
	var invocation ToolInvocation
	var arguments, output []byte
	var updatedAt int64
	err := s.db.QueryRowContext(ctx, `SELECT run_id, invocation_id, session_id, execution_id, agent_id, call_id, name, arguments, status, output, updated_at
		FROM tool_invocations WHERE session_id = ? AND invocation_id = ? ORDER BY updated_at DESC LIMIT 1`, sessionID, invocationID).
		Scan(&invocation.RunID, &invocation.InvocationID, &invocation.SessionID, &invocation.ExecutionID, &invocation.AgentID, &invocation.CallID,
			&invocation.Name, &arguments, &invocation.Status, &output, &updatedAt)
	if err != nil {
		return ToolInvocation{}, fmt.Errorf("load tool invocation for session: %w", err)
	}
	invocation.Arguments = append(json.RawMessage(nil), arguments...)
	invocation.Output = append(json.RawMessage(nil), output...)
	invocation.UpdatedAt = time.Unix(0, updatedAt)
	return invocation, nil
}

func (s *Store) PendingToolInvocations(ctx context.Context, sessionID, executionID string) ([]ToolInvocation, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("session store is not open")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, invocation_id, session_id, execution_id, agent_id, call_id, name, arguments, status, output, updated_at
		FROM tool_invocations WHERE session_id = ? AND execution_id = ? AND status IN (?, ?) ORDER BY updated_at, run_id, invocation_id`,
		sessionID, executionID, ToolInvocationRunning, ToolInvocationUnknown)
	if err != nil {
		return nil, fmt.Errorf("list pending tool invocations: %w", err)
	}
	defer rows.Close()
	var invocations []ToolInvocation
	for rows.Next() {
		var invocation ToolInvocation
		var arguments, output []byte
		var updatedAt int64
		if err := rows.Scan(&invocation.RunID, &invocation.InvocationID, &invocation.SessionID, &invocation.ExecutionID, &invocation.AgentID,
			&invocation.CallID, &invocation.Name, &arguments, &invocation.Status, &output, &updatedAt); err != nil {
			return nil, fmt.Errorf("read pending tool invocation: %w", err)
		}
		invocation.Arguments = append(json.RawMessage(nil), arguments...)
		invocation.Output = append(json.RawMessage(nil), output...)
		invocation.UpdatedAt = time.Unix(0, updatedAt)
		invocations = append(invocations, invocation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pending tool invocations: %w", err)
	}
	return invocations, nil
}

func (s *Store) MarkToolInvocationUnknown(ctx context.Context, runID, invocationID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE tool_invocations SET status = ?, updated_at = ?
		WHERE run_id = ? AND invocation_id = ? AND status = ?`, ToolInvocationUnknown, time.Now().UnixNano(),
		runID, invocationID, ToolInvocationRunning)
	if err != nil {
		return fmt.Errorf("mark tool invocation outcome unknown: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check unknown tool invocation update: %w", err)
	}
	if updated == 0 {
		invocation, loadErr := s.GetToolInvocation(ctx, runID, invocationID)
		if loadErr != nil {
			return loadErr
		}
		if invocation.Status != ToolInvocationUnknown {
			return fmt.Errorf("tool invocation is %q, not running", invocation.Status)
		}
	}
	return nil
}

func (s *Store) ClaimToolInvocationRetry(ctx context.Context, runID, invocationID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE tool_invocations SET status = ?, updated_at = ?
		WHERE run_id = ? AND invocation_id = ? AND status = ?`, ToolInvocationRunning, time.Now().UnixNano(),
		runID, invocationID, ToolInvocationRetry)
	if err != nil {
		return fmt.Errorf("claim tool invocation retry: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check tool invocation retry claim: %w", err)
	}
	if updated == 0 {
		return errors.New("tool invocation retry is no longer available")
	}
	return nil
}

func (s *Store) SaveToolInvocationOutput(ctx context.Context, runID, invocationID, status string, output json.RawMessage) error {
	if status != ToolInvocationCompleted && status != ToolInvocationSkipped {
		return fmt.Errorf("unsupported completed tool invocation status %q", status)
	}
	if !json.Valid(output) {
		return errors.New("tool invocation output must be valid JSON")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE tool_invocations SET status = ?, output = ?, updated_at = ?
		WHERE run_id = ? AND invocation_id = ? AND status IN (?, ? , ?)
	`, status, []byte(output), time.Now().UnixNano(), runID, invocationID,
		ToolInvocationRunning, ToolInvocationRetry, ToolInvocationUnknown)
	if err != nil {
		return fmt.Errorf("save tool invocation output: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check tool invocation output update: %w", err)
	}
	if updated == 0 {
		return errors.New("tool invocation is no longer in a writable state")
	}
	return nil
}

func (s *Store) ResolveToolInvocation(ctx context.Context, sessionID, runID, invocationID, resolution string, output json.RawMessage) error {
	if resolution != ToolInvocationRetry && resolution != ToolInvocationSkipped {
		return fmt.Errorf("unsupported tool invocation resolution %q", resolution)
	}
	if resolution == ToolInvocationSkipped && !json.Valid(output) {
		return errors.New("skipped tool invocation output must be valid JSON")
	}
	var savedOutput any
	if len(output) > 0 {
		savedOutput = []byte(output)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE tool_invocations SET status = ?, output = ?, updated_at = ?
		WHERE session_id = ? AND run_id = ? AND invocation_id = ? AND status = ?`, resolution, savedOutput, time.Now().UnixNano(),
		sessionID, runID, invocationID, ToolInvocationUnknown)
	if err != nil {
		return fmt.Errorf("resolve tool invocation: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check tool invocation resolution: %w", err)
	}
	if updated == 0 {
		return errors.New("tool invocation is not awaiting recovery")
	}
	return nil
}

func (s *Store) DeleteToolInvocationsForSession(ctx context.Context, sessionID string) error {
	if s == nil || s.db == nil || sessionID == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM tool_invocations WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("delete tool invocations for session %q: %w", sessionID, err)
	}
	return nil
}
