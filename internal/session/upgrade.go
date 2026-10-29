package session

import (
	"context"
	"encoding/json"
	"time"
)

// UpgradeExecutions is used only by the offline upgrader after backup and lock
// acquisition. History and accounting transformations share its transaction.
func (s *Store) UpgradeExecutions(ctx context.Context, transform func(json.RawMessage) (json.RawMessage, error)) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,data FROM sessions`)
	if err != nil {
		return 0, err
	}
	type payload struct {
		id   string
		data []byte
	}
	var snapshots []payload
	for rows.Next() {
		var p payload
		if err := rows.Scan(&p.id, &p.data); err != nil {
			rows.Close()
			return 0, err
		}
		snapshots = append(snapshots, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, p := range snapshots {
		data, err := transform(p.data)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET data=? WHERE id=?`, []byte(data), p.id); err != nil {
			return 0, err
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT session_id,queue_id,execution_id FROM queued_turn_inputs`)
	if err != nil {
		return 0, err
	}
	type identity struct{ session, queue, execution string }
	var pending []identity
	for rows.Next() {
		var id identity
		if err := rows.Scan(&id.session, &id.queue, &id.execution); err != nil {
			rows.Close()
			return 0, err
		}
		pending = append(pending, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, id := range pending {
		if err := upsertTurnResultTx(ctx, tx, id.session, id.execution, "discarded", nil, time.Now()); err != nil {
			return 0, err
		}
		event, _ := json.Marshal(map[string]any{"Kind": "task_discarded", "SessionID": id.session, "QueueID": id.queue, "ExecutionID": id.execution, "Status": "discarded"})
		if _, err := appendRuntimeEventTx(ctx, tx, id.session, event, time.Now()); err != nil {
			return 0, err
		}
	}
	for _, table := range []string{"queued_turn_inputs", "tool_invocations", "graph_resume_inputs", "user_question_drafts", "execution_token_reservations", "execution_token_budgets", "commands"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return 0, err
		}
	}
	// Never restart an old active goal automatically. Its objective and usage stay.
	if _, err := tx.ExecContext(ctx, `UPDATE session_goals SET data=CAST(json_set(CAST(data AS TEXT),'$.status','paused','$.stop_reason','Execution engine upgraded; resume explicitly') AS BLOB) WHERE json_extract(CAST(data AS TEXT),'$.status')='active'`); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(pending), nil
}
