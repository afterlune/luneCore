package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/afterlune/luneCore/internal/command"
	publictool "github.com/afterlune/luneCore/tool"
)

func (s *Store) SaveCommand(ctx context.Context, record command.Record) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO commands(session_id,id,execution_id,data)
        SELECT ?,?,?,? WHERE EXISTS(SELECT 1 FROM sessions WHERE id=?)
        ON CONFLICT(session_id,id) DO UPDATE SET data=excluded.data`, record.Info.SessionID, record.Info.ID, record.Info.ExecutionID, encoded, record.Info.SessionID)
	if err != nil {
		return fmt.Errorf("save command: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return errors.New("command session no longer exists")
	}
	return nil
}

func (s *Store) GetCommand(ctx context.Context, sessionID, id string) (command.Record, error) {
	var data []byte
	var record command.Record
	if err := s.db.QueryRowContext(ctx, `SELECT data FROM commands WHERE session_id=? AND id=?`, sessionID, id).Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return record, errors.New("command does not exist in this session")
		}
		return record, err
	}
	err := json.Unmarshal(data, &record)
	return record, err
}

func (s *Store) ListCommands(ctx context.Context, sessionID, executionID string) ([]command.Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT json_object('info',json_extract(data,'$.info')) FROM commands WHERE (?='' OR session_id=?) AND (?='' OR execution_id=?) ORDER BY rowid`, sessionID, sessionID, executionID, executionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []command.Record
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var record command.Record
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Store) InterruptCommands(ctx context.Context) error {
	records, err := s.ListCommands(ctx, "", "")
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.Info.Status.Active() {
			record, err = s.GetCommand(ctx, record.Info.SessionID, record.Info.ID)
			if err != nil {
				return err
			}
			record.Info.Status = publictool.CommandInterrupted
			record.Info.FinishedAt = time.Now()
			record.Info.ExitCode = nil
			record.Error = "The prior process ended without a confirmed result; it was not restarted."
			if err := s.SaveCommand(ctx, record); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) DeleteCommands(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM commands WHERE session_id=?`, sessionID)
	return err
}
