package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type GoalStatus string

const (
	GoalActive GoalStatus = "active"
	GoalPaused GoalStatus = "paused"
	GoalBlocked GoalStatus = "blocked"
	GoalBudgetLimited GoalStatus = "budget_limited"
	GoalComplete GoalStatus = "complete"
)

// Goal is the durable objective and accounting for one session. TokenBudget
// counts uncached input plus output, rather than money or context-window size.
type Goal struct {
	ID string `json:"id"`
	Revision int64 `json:"revision"`
	Objective string `json:"objective"`
	Status GoalStatus `json:"status"`
	TokenBudget *int64 `json:"token_budget,omitempty"`
	TokensUsed int64 `json:"tokens_used"`
	CachedTokens int64 `json:"cached_tokens"`
	Estimated bool `json:"estimated,omitempty"`
	RunMilliseconds int64 `json:"run_milliseconds"`
	StopReason string `json:"stop_reason,omitempty"`
	EmptyTurns int `json:"empty_turns,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Store) Goal(ctx context.Context, sessionID string) (*Goal, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM session_goals WHERE session_id = ?`, sessionID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) { return nil, nil }
	if err != nil { return nil, fmt.Errorf("read goal: %w", err) }
	var goal Goal
	if err := json.Unmarshal(data, &goal); err != nil { return nil, err }
	return &goal, nil
}

// ModifyGoal commits the mutation, optional usage receipt, and public event in
// one transaction. Usage receipts survive completed execution cleanup.
func (s *Store) ModifyGoal(ctx context.Context, sessionID, requestKey string, mutate func(*Goal) (*Goal, error), event func(*Goal) json.RawMessage) (*Goal, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil { return nil, false, err }
	defer tx.Rollback()
	var data []byte
	var previous *Goal
	err = tx.QueryRowContext(ctx, `SELECT data FROM session_goals WHERE session_id = ?`, sessionID).Scan(&data)
	if err == nil {
		previous = &Goal{}
		if err := json.Unmarshal(data, previous); err != nil { return nil, false, err }
	} else if !errors.Is(err, sql.ErrNoRows) { return nil, false, err }
	if requestKey != "" {
		if previous == nil { return nil, false, nil }
		result, err := tx.ExecContext(ctx, `INSERT INTO goal_usage(goal_id, request_key) VALUES(?, ?) ON CONFLICT DO NOTHING`, previous.ID, requestKey)
		if err != nil { return nil, false, err }
		count, _ := result.RowsAffected()
		if count == 0 { return previous, false, nil }
	}
	next, err := mutate(previous)
	if err != nil { return nil, false, err }
	if next == nil {
		if previous != nil {
			if _, err = tx.ExecContext(ctx, `DELETE FROM goal_usage WHERE goal_id = ?`, previous.ID); err != nil { return nil, false, err }
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM session_goals WHERE session_id = ?`, sessionID)
	} else {
		if previous != nil && previous.ID != next.ID {
			if _, err = tx.ExecContext(ctx, `DELETE FROM goal_usage WHERE goal_id = ?`, previous.ID); err != nil { return nil, false, err }
		}
		data, err = json.Marshal(next)
		if err != nil { return nil, false, err }
		_, err = tx.ExecContext(ctx, `INSERT INTO session_goals(session_id, goal_id, data) VALUES(?, ?, ?)
			ON CONFLICT(session_id) DO UPDATE SET goal_id=excluded.goal_id, data=excluded.data`, sessionID, next.ID, data)
	}
	if err != nil { return nil, false, err }
	if _, err = tx.ExecContext(ctx, `INSERT INTO runtime_events(session_id, payload, created_at) VALUES(?, ?, ?)`, sessionID, []byte(event(next)), time.Now().UnixNano()); err != nil { return nil, false, err }
	if err := tx.Commit(); err != nil { return nil, false, err }
	return next, true, nil
}
