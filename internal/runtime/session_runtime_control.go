package runtime

import (
	"context"
	"errors"

	"github.com/afterlune/luneCore/internal/session"
)

func (r *SessionRuntime) State(ctx context.Context) (RuntimeState, error) {
	if r == nil || r.core == nil || ctx == nil {
		return RuntimeState{}, errors.New("session runtime and context are required")
	}
	state, ok := r.core.StateForSession(r.sessionID)
	if !ok {
		state = TaskState{SessionID: r.sessionID, Status: TaskIdle}
	}
	queued, err := r.core.sessions.QueuedTurns(ctx, r.sessionID)
	if err != nil {
		return RuntimeState{}, err
	}
	goal, err := r.GetGoal(ctx)
	if err != nil {
		return RuntimeState{}, err
	}
	result := RuntimeState{Task: state, Goal: goal}
	taskSelected := result.Task.Status == TaskRunning
	for _, item := range queued {
		result.Queue = append(result.Queue, QueuedTurnState{QueueID: item.QueueID, Position: item.Position, ExecutionID: item.ExecutionID, Status: item.Status, CreatedAt: item.CreatedAt})
		if taskSelected {
			continue
		}
		if item.Status == session.QueuedTurnPaused {
			result.Task = TaskState{SessionID: r.sessionID, ExecutionID: item.ExecutionID, QueueID: item.QueueID, Status: TaskPaused}
			taskSelected = true
		} else if item.Status == session.QueuedTurnQueued || item.Status == session.QueuedTurnActive {
			result.Task = TaskState{SessionID: r.sessionID, ExecutionID: item.ExecutionID, QueueID: item.QueueID, Status: TaskQueued}
			taskSelected = true
		}
	}
	if result.Task.Status != TaskRunning {
		pending, found, err := r.core.pendingState(ctx, r.sessionID)
		if err != nil {
			return RuntimeState{}, err
		}
		if found {
			recoveryQueued := false
			for _, item := range queued {
				if item.ExecutionID == pending.ExecutionID && (item.Status == session.QueuedTurnQueued || item.Status == session.QueuedTurnActive) {
					recoveryQueued = true
					break
				}
			}
			if !recoveryQueued {
				result.Task = TaskState{SessionID: r.sessionID, ExecutionID: pending.ExecutionID, Status: TaskPaused, GoalID: pending.GoalID, GoalRevision: pending.GoalRevision, AutomaticGoal: pending.AutomaticGoal}
			}
		}
	}
	return result, nil
}

// Interrupt accepts a cancellation request; it does not wait for task cleanup.
func (r *SessionRuntime) Interrupt(ctx context.Context, executionID string) (bool, error) {
	if r == nil || r.core == nil || ctx == nil || executionID == "" {
		return false, errors.New("session runtime, context, and execution id are required")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if r.core.goalRun(executionID).ID != "" {
		_, err := r.pauseGoal(ctx, false, executionID)
		if errors.Is(err, errStaleGoal) {
			return false, nil
		}
		return err == nil, err
	}
	return r.core.interruptExecution(ctx, r.sessionID, executionID)
}

func (r *SessionRuntime) Recover(ctx context.Context, options ...ExecutionOptions) (TurnSubmission, error) {
	if r == nil || r.core == nil {
		return TurnSubmission{}, errors.New("session runtime is nil")
	}
	submission, _, err := r.core.recoverSession(ctx, r.sessionID, false, options...)
	return submission, err
}

func (r *SessionRuntime) DiscardPaused(ctx context.Context) (bool, error) {
	if r == nil || r.core == nil || ctx == nil {
		return false, errors.New("session runtime and context are required")
	}
	deleted, err := r.core.discardPaused(ctx, r.sessionID)
	if err == nil && deleted {
		r.core.scheduleQueueDrain(r.sessionID)
	}
	return deleted, err
}

func (r *SessionRuntime) SetUserInteractionAvailable(available bool) {
	if r == nil || r.core == nil {
		return
	}
	r.core.mu.Lock()
	if available {
		r.core.interactiveSessions[r.sessionID] = true
	} else {
		delete(r.core.interactiveSessions, r.sessionID)
	}
	r.core.mu.Unlock()
}
