package command

import (
	"context"
	"errors"
	"strings"
	"time"

	publictool "github.com/afterlune/luneCore/tool"
)

func cloneInfo(info publictool.CommandInfo) publictool.CommandInfo {
	if info.ExitCode != nil {
		code := *info.ExitCode
		info.ExitCode = &code
	}
	return info
}

func owned(record Record, sessionID, executionID, agentID string) bool {
	if record.Info.SessionID != sessionID {
		return false
	}
	if record.Info.Background {
		return true
	}
	return (executionID == "" || record.Info.ExecutionID == executionID) && (agentID == "" || record.Info.AgentID == agentID)
}

func (m *Manager) Read(ctx context.Context, sessionID, id, executionID, agentID string, options publictool.CommandReadOptions) (publictool.CommandOutput, error) {
	if ctx == nil || options.Cursor < 0 || options.Wait < 0 || options.Wait > MaxWait {
		return publictool.CommandOutput{}, errors.New("invalid command read options")
	}
	m.mu.Lock()
	j := m.jobs[id]
	m.mu.Unlock()
	if j != nil {
		j.mu.Lock()
		if !owned(j.record, sessionID, executionID, agentID) {
			j.mu.Unlock()
			return publictool.CommandOutput{}, errors.New("command does not belong to this owner")
		}
		if options.Cursor > j.record.End {
			j.mu.Unlock()
			return publictool.CommandOutput{}, errors.New("command cursor exceeds output")
		}
		changed := j.changed
		wait := j.record.Info.Status.Active() && j.record.End <= options.Cursor && options.Wait > 0
		j.mu.Unlock()
		if wait {
			timer := time.NewTimer(options.Wait)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return publictool.CommandOutput{}, ctx.Err()
			case <-changed:
			case <-timer.C:
			}
		}
		j.mu.Lock()
		defer j.mu.Unlock()
		if err := m.store.SaveCommand(ctx, j.record); err != nil {
			return publictool.CommandOutput{}, err
		}
		return output(j.record, options.Cursor)
	}
	record, err := m.store.GetCommand(ctx, sessionID, id)
	if err != nil {
		return publictool.CommandOutput{}, err
	}
	if !owned(record, sessionID, executionID, agentID) {
		return publictool.CommandOutput{}, errors.New("command does not belong to this owner")
	}
	return output(record, options.Cursor)
}

func output(record Record, cursor int64) (publictool.CommandOutput, error) {
	if cursor > record.End {
		return publictool.CommandOutput{}, errors.New("command cursor exceeds output")
	}
	base := record.End - int64(len(record.Output))
	truncated := cursor < base
	if truncated {
		cursor = base
	}
	end := cursor + ReadLimit
	if end > record.End {
		end = record.End
	}
	return publictool.CommandOutput{Command: cloneInfo(record.Info), Output: strings.ToValidUTF8(string(record.Output[cursor-base:end-base]), "\uFFFD"), Cursor: cursor, NextCursor: end, EndCursor: record.End, Truncated: truncated, Error: record.Error}, nil
}

func (m *Manager) List(ctx context.Context, sessionID, executionID string) ([]publictool.CommandInfo, error) {
	records, err := m.store.ListCommands(ctx, sessionID, executionID)
	if err != nil {
		return nil, err
	}
	result := make([]publictool.CommandInfo, 0, len(records))
	for _, record := range records {
		m.mu.Lock()
		j := m.jobs[record.Info.ID]
		m.mu.Unlock()
		if j != nil {
			j.mu.Lock()
			record.Info = cloneInfo(j.record.Info)
			j.mu.Unlock()
		}
		result = append(result, record.Info)
	}
	return result, nil
}
