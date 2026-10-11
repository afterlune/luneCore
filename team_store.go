package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
	_ "modernc.org/sqlite"
)

type teamRunRecord struct {
	ID        string             `json:"id"`
	ParentID  string             `json:"parent_id"`
	CallID    string             `json:"call_id"`
	UpdatedAt time.Time          `json:"updated_at"`
	Members   []teamMemberRecord `json:"members"`
}

type teamMemberRecord struct {
	ID       string `json:"id"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
	ModelID  string `json:"model_id"`
	Task     string `json:"task"`
	Status   string `json:"status"`
	Text     string `json:"text,omitempty"`
	Error    string `json:"error,omitempty"`
}

type teamStore struct {
	mu sync.Mutex
	db *sql.DB
}

func openTeamStore(path string) (*teamStore, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve team store path: %w", err)
	}
	filePath := filepath.ToSlash(absolute)
	if filepath.VolumeName(absolute) != "" {
		filePath = "/" + filePath
	}
	query := url.Values{}
	query.Set("_busy_timeout", "5000")
	query.Set("_journal_mode", "WAL")
	fileURL := url.URL{Scheme: "file", Path: filePath, RawQuery: query.Encode()}
	db, err := sql.Open("sqlite", fileURL.String())
	if err != nil {
		return nil, fmt.Errorf("open team store: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS team_runs (
		run_id TEXT PRIMARY KEY,
		parent_id TEXT NOT NULL,
		updated_at INTEGER NOT NULL,
		data BLOB NOT NULL
	)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize team store: %w", err)
	}
	if err := os.Chmod(absolute, 0600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secure team store permissions: %w", err)
	}
	return &teamStore{db: db}, nil
}

func (s *teamStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *teamStore) GetOrCreate(parentID, callID string, assignments []agentgraph.TeamAssignment, profiles map[string]model.ModelProfile) (teamRunRecord, bool, error) {
	if s == nil || s.db == nil {
		return teamRunRecord{}, false, errors.New("team store is not open")
	}
	runID := teamRunKey(parentID, callID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if record, found, err := s.load(context.Background(), runID); err != nil || found {
		if err != nil {
			return teamRunRecord{}, false, err
		}
		if !sameTeamAssignments(record, assignments) {
			return teamRunRecord{}, false, errors.New("team replay assignments do not match the saved team run")
		}
		return record, true, nil
	}
	record := teamRunRecord{ID: runID, ParentID: parentID, CallID: callID, UpdatedAt: time.Now(), Members: make([]teamMemberRecord, 0, len(assignments))}
	for _, assignment := range assignments {
		profile, ok := profiles[assignment.Model]
		if !ok {
			return teamRunRecord{}, false, fmt.Errorf("model profile %q is no longer configured", assignment.Model)
		}
		record.Members = append(record.Members, teamMemberRecord{
			ID: assignment.ID, Model: assignment.Model, Provider: profile.Provider,
			ModelID: profile.Model, Task: assignment.Task, Status: "queued",
		})
	}
	if err := s.save(context.Background(), record); err != nil {
		return teamRunRecord{}, false, err
	}
	return record, false, nil
}

func (s *teamStore) Member(runID, memberID string) (teamMemberRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, found, err := s.load(context.Background(), runID)
	if err != nil {
		return teamMemberRecord{}, err
	}
	if !found {
		return teamMemberRecord{}, fmt.Errorf("team run %q not found", runID)
	}
	for _, member := range record.Members {
		if member.ID == memberID {
			return member, nil
		}
	}
	return teamMemberRecord{}, fmt.Errorf("team member %q not found", memberID)
}

func (s *teamStore) UpdateMember(runID string, member teamMemberRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, found, err := s.load(context.Background(), runID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("team run %q not found", runID)
	}
	updated := false
	for index := range record.Members {
		if record.Members[index].ID == member.ID {
			record.Members[index] = member
			updated = true
			break
		}
	}
	if !updated {
		return fmt.Errorf("team member %q not found", member.ID)
	}
	record.UpdatedAt = time.Now()
	return s.save(context.Background(), record)
}

func (s *teamStore) DeleteParent(parentID string) error {
	if s == nil || s.db == nil || parentID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM team_runs WHERE parent_id = ?`, parentID)
	if err != nil {
		return fmt.Errorf("delete completed team runs: %w", err)
	}
	return nil
}

func (s *teamStore) CheckpointIDs(parentID string) ([]string, error) {
	if s == nil || s.db == nil || parentID == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT data FROM team_runs WHERE parent_id = ?`, parentID)
	if err != nil {
		return nil, fmt.Errorf("list team runs for %q: %w", parentID, err)
	}
	defer rows.Close()
	var checkpoints []string
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("read team run for %q: %w", parentID, err)
		}
		var record teamRunRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, fmt.Errorf("decode team run for %q: %w", parentID, err)
		}
		for _, member := range record.Members {
			checkpoints = append(checkpoints, record.ID+":"+member.ID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read team runs for %q: %w", parentID, err)
	}
	return checkpoints, nil
}

func (s *teamStore) load(ctx context.Context, id string) (teamRunRecord, bool, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM team_runs WHERE run_id = ?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return teamRunRecord{}, false, nil
	}
	if err != nil {
		return teamRunRecord{}, false, fmt.Errorf("load team run: %w", err)
	}
	var record teamRunRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return teamRunRecord{}, false, fmt.Errorf("decode team run: %w", err)
	}
	return record, true, nil
}

func (s *teamStore) save(ctx context.Context, record teamRunRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode team run: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO team_runs(run_id, parent_id, updated_at, data) VALUES(?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET parent_id = excluded.parent_id, updated_at = excluded.updated_at, data = excluded.data`,
		record.ID, record.ParentID, record.UpdatedAt.UnixNano(), data)
	if err != nil {
		return fmt.Errorf("save team run: %w", err)
	}
	return nil
}

func sameTeamAssignments(record teamRunRecord, assignments []agentgraph.TeamAssignment) bool {
	if len(record.Members) != len(assignments) {
		return false
	}
	byID := make(map[string]agentgraph.TeamAssignment, len(assignments))
	for _, assignment := range assignments {
		byID[assignment.ID] = assignment
	}
	for _, saved := range record.Members {
		assignment, ok := byID[saved.ID]
		if !ok || assignment.Model != saved.Model || assignment.Task != saved.Task {
			return false
		}
	}
	return true
}

func teamRunKey(parentID, callID string) string {
	return parentID + ":" + callID
}
