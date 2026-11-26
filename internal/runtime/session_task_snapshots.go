package runtime

import (
	"context"
	"errors"

	"github.com/afterlune/luneCore/internal/session"
)

// SessionTaskSnapshot summarizes durable and in-memory task state for a
// session without opening a runtime or scheduling queued work.
type SessionTaskSnapshot struct {
	SessionID   string     `json:"sessionId"`
	Status      TaskStatus `json:"status"`
	ExecutionID string     `json:"executionId,omitempty"`
	QueueCount  int        `json:"queueCount,omitempty"`
}

// ListSessionTaskSnapshots returns task status for sessions in this Core's
// workspace. It is suitable for dashboards and does not start work.
func (c *Core) ListSessionTaskSnapshots(ctx context.Context) ([]SessionTaskSnapshot, error) {
	if c == nil || ctx == nil {
		return nil, errors.New("core and context are required")
	}
	const pageSize = 100
	var snapshots []SessionTaskSnapshot
	for offset := 0; ; {
		sessionIDs, err := c.sessions.ListIDsPage(ctx, c.workspace, offset, pageSize, true)
		if err != nil {
			return nil, err
		}
		for _, sessionID := range sessionIDs {
			queued, err := c.sessions.QueuedTurns(ctx, sessionID)
			if err != nil {
				return nil, err
			}
			state, found := c.StateForSession(sessionID)
			if !found {
				state = TaskState{SessionID: sessionID, Status: TaskIdle}
			}
			selected := state.Status == TaskRunning
			for _, item := range queued {
				if selected {
					continue
				}
				switch item.Status {
				case session.QueuedTurnPaused:
					state = TaskState{SessionID: sessionID, QueueID: item.QueueID, ExecutionID: item.ExecutionID, Status: TaskPaused}
					selected = true
				case session.QueuedTurnQueued, session.QueuedTurnActive:
					state = TaskState{SessionID: sessionID, QueueID: item.QueueID, ExecutionID: item.ExecutionID, Status: TaskQueued}
					selected = true
				}
			}
			if state.Status != TaskRunning {
				pending, pendingErr := c.hasPending(ctx, sessionID)
				if pendingErr != nil {
					return nil, pendingErr
				}
				recoveryQueued := false
				if pending {
					for _, item := range queued {
						if item.ExecutionID == state.ExecutionID && (item.Status == session.QueuedTurnQueued || item.Status == session.QueuedTurnActive) {
							recoveryQueued = true
							break
						}
					}
				}
				if pending && !recoveryQueued {
					state = TaskState{SessionID: sessionID, ExecutionID: state.ExecutionID, Status: TaskPaused}
				}
			}
			snapshots = append(snapshots, SessionTaskSnapshot{SessionID: sessionID, Status: state.Status, ExecutionID: state.ExecutionID, QueueCount: len(queued)})
		}
		offset += len(sessionIDs)
		if len(sessionIDs) < pageSize {
			return snapshots, nil
		}
	}
}
