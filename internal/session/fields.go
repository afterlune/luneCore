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

// These five fixed ownership groups build their SQL once. They are not caller
// supplied projections; adding a session field does not change existing groups.
var (
	settingsFields  = fieldStatements("settings")
	usageFields     = fieldStatements("usage_requests", "agent_usage", "task_usage", "session_usage")
	taskUsageFields = fieldStatements("task_usage")
	approvalFields  = fieldStatements("approvals")
	pathGrantFields = fieldStatements("path_grants")
)

type fieldSQL struct{ selectSQL, updateSQL string }

func fieldStatements(fields ...string) fieldSQL {
	var projections, assignments []string
	for _, field := range fields {
		projections = append(projections, "'"+field+"', json_extract(CAST(data AS TEXT), '$."+field+"')")
		assignments = append(assignments, "'$."+field+"', updated.value -> '$."+field+"'")
	}
	return fieldSQL{
		selectSQL: "SELECT json_object(" + strings.Join(projections, ",") + ") FROM sessions WHERE id=? AND workspace=?",
		updateSQL: "WITH updated(value) AS MATERIALIZED (VALUES(jsonb(?))) UPDATE sessions SET data=CAST((SELECT json_set(CAST(sessions.data AS TEXT)," + strings.Join(assignments, ",") + ") FROM updated) AS BLOB),updated_at=? WHERE id=? AND workspace=?",
	}
}

func (s *Store) UpdateSettings(ctx context.Context, id, workspace string, settings json.RawMessage) error {
	if !json.Valid(settings) {
		return errors.New("settings must be valid JSON")
	}
	return s.modifyFields(ctx, id, workspace, settingsFields, func(json.RawMessage) (json.RawMessage, error) {
		return json.Marshal(map[string]json.RawMessage{"settings": settings})
	})
}

func (s *Store) ModifyUsage(ctx context.Context, id, workspace string, mutate func(json.RawMessage) (json.RawMessage, error)) error {
	return s.modifyFields(ctx, id, workspace, usageFields, mutate)
}

func (s *Store) ResetTaskUsage(ctx context.Context, id, workspace string) error {
	return s.modifyFields(ctx, id, workspace, taskUsageFields, func(json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{"task_usage":{}}`), nil })
}

func (s *Store) ModifyApprovals(ctx context.Context, id, workspace string, mutate func(json.RawMessage) (json.RawMessage, error)) error {
	return s.modifyFields(ctx, id, workspace, approvalFields, mutate)
}

func (s *Store) ModifyPathGrants(ctx context.Context, id, workspace string, mutate func(json.RawMessage) (json.RawMessage, error)) error {
	return s.modifyFields(ctx, id, workspace, pathGrantFields, mutate)
}

// modifyFields reads and writes only the named ownership group in one write
// transaction. Mutators are pure: they must not call back into the Store.
func (s *Store) modifyFields(ctx context.Context, id, workspace string, statements fieldSQL, mutate func(json.RawMessage) (json.RawMessage, error)) error {
	if s == nil || s.db == nil || ctx == nil || id == "" || workspace == "" || mutate == nil {
		return errors.New("store, context, session identity, and mutation are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previous []byte
	err = tx.QueryRowContext(ctx, statements.selectSQL, id, workspace).Scan(&previous)
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
	// Keep JSON interpretation in SQLite. The JSON arrow preserves scalar,
	// object, array, and null values without copying each field through a Go map.
	if _, err := tx.ExecContext(ctx, statements.updateSQL, string(updated), time.Now().UnixNano(), id, workspace); err != nil {
		return err
	}
	return tx.Commit()
}
