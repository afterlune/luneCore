package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/afterlune/luneCore/internal/agentgraph"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint"
)

// repairLegacyInterruptedTurn recognizes only the old empty-error failed
// result paired with its incomplete execution checkpoint. It leaves session
// history untouched and removes only recovery state for that interrupted run.
func (c *Core) repairLegacyInterruptedTurn(ctx context.Context, sessionID string) error {
	if c == nil || ctx == nil || sessionID == "" {
		return errors.New("core, context, and session id are required")
	}
	if state, ok := c.StateForSession(sessionID); ok && (state.Status == TaskRunning || state.Status == TaskPaused) {
		return nil
	}
	queued, err := c.sessions.QueuedTurns(ctx, sessionID)
	if err != nil {
		return err
	}
	if len(queued) != 0 {
		return nil
	}
	checkpointID := c.runtime.CheckpointID(sessionID)
	cp, checkpointErr := c.runtime.CheckpointForSession(ctx, sessionID)
	if errors.Is(checkpointErr, checkpoint.ErrNotFound) {
		return nil
	}
	if checkpointErr != nil {
		return checkpointErr
	}
	executionIDs := checkpointExecutionIDs(cp)
	if len(executionIDs) == 0 {
		return nil
	}
	terminal, found, err := c.latestTaskFinishedEvent(ctx, sessionID, executionIDs)
	if err != nil || !found {
		return err
	}
	result, found, err := c.sessions.TurnResult(ctx, sessionID, terminal.event.ExecutionID)
	if err != nil || !found || len(result.Response) != 0 {
		return err
	}
	if result.Status != string(TaskFailed) && result.Status != string(TaskCancelled) {
		return nil
	}
	legacyFailed := result.Status == string(TaskFailed) && terminal.event.Status == TaskFailed && terminal.event.TaskID == "" && terminal.event.Err == ""
	previouslyRepaired := result.Status == string(TaskCancelled) && terminal.event.Status == TaskCancelled
	if !previouslyRepaired {
		if !legacyFailed || cp.Completed {
			return nil
		}
		corrected := terminal.event
		corrected.Status = TaskCancelled
		corrected.Err = "cancelled after interrupted execution"
		payload, marshalErr := json.Marshal(safeEventProjection(corrected))
		if marshalErr != nil {
			return marshalErr
		}
		repaired, repairErr := c.sessions.RepairFailedInterruptedTurn(ctx, sessionID, terminal.event.ExecutionID, terminal.cursor, terminal.payload, payload)
		if repairErr != nil {
			return repairErr
		}
		if !repaired {
			return nil
		}
		terminal.event.Status = TaskCancelled
		c.mu.Lock()
		if state, ok := c.activeTasks[sessionID]; ok && state.ExecutionID == terminal.event.ExecutionID {
			state.Status = TaskCancelled
			c.activeTasks[sessionID] = state
			c.active = state
		}
		c.mu.Unlock()
	}
	if err := c.sessions.DeleteGraphResumeInputs(ctx, sessionID, checkpointID); err != nil {
		return fmt.Errorf("delete interrupted task resume inputs: %w", err)
	}
	if err := c.sessions.DeleteToolInvocationsForSession(ctx, sessionID); err != nil {
		return fmt.Errorf("delete interrupted task tool recovery records: %w", err)
	}
	if err := c.sessions.DeleteExecutionTokenBudget(ctx, terminal.event.ExecutionID); err != nil {
		return fmt.Errorf("delete interrupted task token budget: %w", err)
	}
	if err := c.runtime.DeleteCompletedRunForSession(sessionID); err != nil {
		return fmt.Errorf("delete interrupted task checkpoint: %w", err)
	}
	c.notifyEventHub(sessionID)
	return nil
}

type taskFinishedEvent struct {
	cursor  int64
	payload json.RawMessage
	event   Event
}

func (c *Core) latestTaskFinishedEvent(ctx context.Context, sessionID string, executionIDs map[string]struct{}) (taskFinishedEvent, bool, error) {
	var latest taskFinishedEvent
	var cursor int64
	for {
		events, err := c.sessions.RuntimeEventsAfter(ctx, sessionID, cursor, 1000)
		if err != nil {
			return taskFinishedEvent{}, false, err
		}
		if len(events) == 0 {
			return latest, latest.cursor != 0, nil
		}
		for _, stored := range events {
			cursor = stored.Cursor
			var event Event
			if err := json.Unmarshal(stored.Payload, &event); err != nil {
				return taskFinishedEvent{}, false, fmt.Errorf("decode runtime event %d: %w", stored.Cursor, err)
			}
			if event.Kind == EventTaskFinished {
				if _, ok := executionIDs[event.ExecutionID]; !ok {
					continue
				}
				latest = taskFinishedEvent{cursor: stored.Cursor, payload: stored.Payload, event: event}
			}
		}
		if len(events) < 1000 {
			return latest, latest.cursor != 0, nil
		}
	}
}

func checkpointExecutionIDs(cp graph.Checkpoint[agentgraph.State]) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, invocation := range cp.Invocations {
		if invocation.State.ExecutionID != "" {
			ids[invocation.State.ExecutionID] = struct{}{}
		}
	}
	return ids
}
