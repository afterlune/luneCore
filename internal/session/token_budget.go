package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TokenReservation is one model request's durable share of an execution budget.
// A nil actual token count remains reserved and is charged at its full estimate.
type TokenReservation struct {
	ID               int64
	OutputLimit      int64
	OutputWasLimited bool
}

// SetExecutionTokenBudget creates or updates the absolute limit without
// changing reservations already recorded for this execution.
func (s *Store) SetExecutionTokenBudget(ctx context.Context, sessionID, executionID string, limit int64) error {
	if s == nil || s.db == nil {
		return errors.New("session store is not open")
	}
	if sessionID == "" || executionID == "" || limit < 0 {
		return errors.New("session id, execution id, and a non-negative token limit are required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO execution_token_budgets(execution_id, session_id, token_limit, updated_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(execution_id) DO UPDATE SET session_id=excluded.session_id, token_limit=excluded.token_limit, updated_at=excluded.updated_at`,
		executionID, sessionID, limit, time.Now().UnixNano())
	if err != nil {
		return fmt.Errorf("save execution token budget: %w", err)
	}
	return nil
}

// ReserveExecutionTokens atomically reserves input plus output capacity. The
// bool result is false when the request cannot fit under the current limit.
func (s *Store) ReserveExecutionTokens(ctx context.Context, executionID string, inputEstimate, requestedOutput int64) (TokenReservation, bool, error) {
	if s == nil || s.db == nil {
		return TokenReservation{}, false, errors.New("session store is not open")
	}
	if executionID == "" || inputEstimate < 0 || requestedOutput < 0 {
		return TokenReservation{}, false, errors.New("execution id and non-negative token estimates are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TokenReservation{}, false, fmt.Errorf("begin token reservation: %w", err)
	}
	defer tx.Rollback()
	var limit int64
	if err := tx.QueryRowContext(ctx, `SELECT token_limit FROM execution_token_budgets WHERE execution_id = ?`, executionID).Scan(&limit); err != nil {
		return TokenReservation{}, false, fmt.Errorf("load execution token budget: %w", err)
	}
	var spent, reserved int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(coalesce(actual_tokens, 0)), 0),
		coalesce(sum(CASE WHEN actual_tokens IS NULL THEN reserved_tokens ELSE 0 END), 0)
		FROM execution_token_reservations WHERE execution_id = ?`, executionID).Scan(&spent, &reserved); err != nil {
		return TokenReservation{}, false, fmt.Errorf("read execution token reservations: %w", err)
	}
	outputLimit := requestedOutput
	outputWasLimited := false
	if limit > 0 {
		available := limit - spent - reserved
		if inputEstimate >= available {
			return TokenReservation{}, false, nil
		}
		available -= inputEstimate
		if outputLimit <= 0 || outputLimit > available {
			outputLimit = available
			outputWasLimited = requestedOutput <= 0 || outputLimit < requestedOutput
		}
		if outputLimit <= 0 {
			return TokenReservation{}, false, nil
		}
	}
	reservedTokens := inputEstimate + outputLimit
	if _, err := tx.ExecContext(ctx, `INSERT INTO execution_token_reservations(execution_id, reserved_tokens, created_at) VALUES(?, ?, ?)`,
		executionID, reservedTokens, time.Now().UnixNano()); err != nil {
		return TokenReservation{}, false, fmt.Errorf("save execution token reservation: %w", err)
	}
	var reservationID int64
	if err := tx.QueryRowContext(ctx, `SELECT last_insert_rowid()`).Scan(&reservationID); err != nil {
		return TokenReservation{}, false, fmt.Errorf("read token reservation id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return TokenReservation{}, false, fmt.Errorf("commit token reservation: %w", err)
	}
	return TokenReservation{ID: reservationID, OutputLimit: outputLimit, OutputWasLimited: outputWasLimited}, true, nil
}

// SettleExecutionTokens replaces a request's reservation with measured usage.
// Calling it repeatedly with the same value is safe.
func (s *Store) SettleExecutionTokens(ctx context.Context, executionID string, reservationID, actualTokens int64) error {
	if s == nil || s.db == nil {
		return errors.New("session store is not open")
	}
	if executionID == "" || reservationID <= 0 || actualTokens < 0 {
		return errors.New("execution id, reservation id, and non-negative actual usage are required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE execution_token_reservations SET actual_tokens = ?
		WHERE id = ? AND execution_id = ? AND actual_tokens IS NULL`, actualTokens, reservationID, executionID)
	if err != nil {
		return fmt.Errorf("settle execution token reservation: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check token reservation settlement: %w", err)
	}
	if updated > 0 {
		return nil
	}
	var existing sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT actual_tokens FROM execution_token_reservations WHERE id = ? AND execution_id = ?`, reservationID, executionID).Scan(&existing); err != nil {
		return fmt.Errorf("read settled token reservation: %w", err)
	}
	if existing.Valid && existing.Int64 == actualTokens {
		return nil
	}
	return errors.New("token reservation was already settled with a different usage value")
}

// DeleteExecutionTokenBudget removes the budget and its request ledger rows.
func (s *Store) DeleteExecutionTokenBudget(ctx context.Context, executionID string) error {
	if s == nil || s.db == nil {
		return errors.New("session store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM execution_token_reservations WHERE execution_id = ?`, executionID); err != nil {
		return fmt.Errorf("delete execution token reservations: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM execution_token_budgets WHERE execution_id = ?`, executionID); err != nil {
		return fmt.Errorf("delete execution token budget: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit execution token budget cleanup: %w", err)
	}
	return nil
}
