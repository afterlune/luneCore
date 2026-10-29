package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *Store) UpdateSettings(ctx context.Context, id, workspace string, settings json.RawMessage) error {
	if !json.Valid(settings) {
		return errors.New("settings must be valid JSON")
	}
	return s.modifyFields(ctx, id, workspace, []string{"settings"}, func(json.RawMessage) (json.RawMessage, error) {
		return json.Marshal(map[string]json.RawMessage{"settings": settings})
	})
}

func (s *Store) ModifyUsage(ctx context.Context, id, workspace string, mutate func(json.RawMessage) (json.RawMessage, error)) error {
	return s.modifyFields(ctx, id, workspace, []string{"usage_requests", "agent_usage", "task_usage", "session_usage"}, mutate)
}

func (s *Store) ResetTaskUsage(ctx context.Context, id, workspace string) error {
	return s.modifyFields(ctx, id, workspace, []string{"task_usage"}, func(json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{"task_usage":{}}`), nil })
}

func (s *Store) ModifyApprovals(ctx context.Context, id, workspace string, mutate func(json.RawMessage) (json.RawMessage, error)) error {
	return s.modifyFields(ctx, id, workspace, []string{"approvals"}, mutate)
}

func (s *Store) ModifyPathGrants(ctx context.Context, id, workspace string, mutate func(json.RawMessage) (json.RawMessage, error)) error {
	return s.modifyFields(ctx, id, workspace, []string{"path_grants"}, mutate)
}

// modifyFields reads and writes only the named ownership group in one write
// transaction. Mutators are pure: they must not call back into the Store.
func (s *Store) modifyFields(ctx context.Context, id, workspace string, fields []string, mutate func(json.RawMessage) (json.RawMessage, error)) error {
	if s == nil || s.db == nil || ctx == nil || id == "" || workspace == "" || mutate == nil {
		return errors.New("store, context, session identity, and mutation are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var selectParts []string
	var selectArgs []any
	for _, field := range fields {
		selectParts = append(selectParts, "?, json_extract(CAST(data AS TEXT), ?)")
		selectArgs = append(selectArgs, field, "$."+field)
	}
	selectArgs = append(selectArgs, id, workspace)
	var previous []byte
	err = tx.QueryRowContext(ctx, "SELECT json_object("+strings.Join(selectParts, ",")+") FROM sessions WHERE id=? AND workspace=?", selectArgs...).Scan(&previous)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("session %q does not exist in workspace", id)
	}
	if err != nil {
		return err
	}
	updated, err := mutate(previous)
	if err != nil {
		return err
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(updated, &values); err != nil {
		return err
	}
	var assignments []string
	var args []any
	for _, field := range fields {
		value := values[field]
		if len(value) == 0 {
			value = json.RawMessage(`null`)
		}
		assignments = append(assignments, "?, json(?)")
		args = append(args, "$."+field, string(value))
	}
	args = append(args, time.Now().UnixNano(), id, workspace)
	if _, err := tx.ExecContext(ctx, "UPDATE sessions SET data=CAST(json_set(CAST(data AS TEXT),"+strings.Join(assignments, ",")+") AS BLOB),updated_at=? WHERE id=? AND workspace=?", args...); err != nil {
		return err
	}
	return tx.Commit()
}
