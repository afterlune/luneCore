package session

import "context"

func (s *Store) SaveUserQuestionDraft(ctx context.Context, sessionID, runID, invocationID, callID string, payload []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO user_question_drafts(session_id,run_id,invocation_id,call_id,payload)
	 VALUES(?,?,?,?,?) ON CONFLICT(session_id,run_id,invocation_id,call_id) DO UPDATE SET payload=excluded.payload`, sessionID, runID, invocationID, callID, payload)
	return err
}

func (s *Store) UserQuestionDraft(ctx context.Context, sessionID, runID, invocationID, callID string) ([]byte, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM user_question_drafts WHERE session_id=? AND run_id=? AND invocation_id=? AND call_id=?`, sessionID, runID, invocationID, callID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var payload []byte
	if rows.Next() {
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
	}
	return payload, rows.Err()
}

func (s *Store) DeleteUserQuestionDraft(ctx context.Context, sessionID, runID, invocationID, callID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_question_drafts WHERE session_id=? AND run_id=? AND invocation_id=? AND call_id=?`, sessionID, runID, invocationID, callID)
	return err
}
