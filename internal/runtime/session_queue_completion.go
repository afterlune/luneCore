package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/afterlune/luneCore/internal/agentruntime"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
)

func (c *Core) completeQueuedTurnFromRuntime(ctx context.Context, queued session.QueuedTurn, status TaskStatus, response json.RawMessage, runtime *agentruntime.Responder, taskErr error) error {
	var history, modelContext, agentContext json.RawMessage
	contextKey := ""
	var err error
	if runtime != nil {
		transcript := runtime.SessionHistory()
		if transcript == nil {
			transcript = model.NewResponseInputItems()
		}
		history, err = json.Marshal(transcript)
		if err != nil {
			return fmt.Errorf("encode completed transcript: %w", err)
		}
		context := runtime.ModelContextHistory()
		if context != nil {
			modelContext, err = json.Marshal(context)
			if err != nil {
				return fmt.Errorf("encode completed model context: %w", err)
			}
		}
		contextKey = runtime.ContextKey()
		agentContext = runtime.ConversationState()
	}
	if status != TaskCompleted {
		response = nil
	}
	return c.commitQueuedTurn(ctx, queued, status, response, history, modelContext, contextKey, agentContext, errorString(taskErr))
}

func (c *Core) commitQueuedTurn(ctx context.Context, queued session.QueuedTurn, status TaskStatus, response, history, modelContext json.RawMessage, contextKey string, agentContext json.RawMessage, taskError string) error {
	taskState, ok := c.StateForSession(queued.SessionID)
	var taskID string
	if ok && taskState.ExecutionID == queued.ExecutionID {
		taskID = taskState.TaskID
	}
	event := Event{Kind: EventTaskFinished, SessionID: queued.SessionID, TaskID: taskID, QueueID: queued.QueueID, ExecutionID: queued.ExecutionID, Status: status, Err: taskError}
	if _, err := c.sessions.CompleteQueuedTurn(ctx, queued.SessionID, queued.QueueID, queued.ExecutionID, string(status), response, history, modelContext, contextKey, mustMarshalEvent(event), agentContext); err != nil {
		return err
	}
	c.rememberQueuedTaskStatus(queued.SessionID, queued.ExecutionID, queued.QueueID, status)
	c.notifyEventHub(queued.SessionID)
	if err := c.runtime.DeleteCompletedRunForSession(queued.SessionID); err != nil {
		c.logger.Warn("delete completed graph checkpoint", "error", err)
	}
	if err := c.sessions.DeleteToolInvocationsForSession(context.Background(), queued.SessionID); err != nil {
		c.logger.Warn("delete completed tool invocation records", "error", err)
	}
	if err := c.sessions.DeleteExecutionTokenBudget(context.Background(), queued.ExecutionID); err != nil {
		c.logger.Warn("delete completed execution token budget", "error", err)
	}
	return nil
}

func (c *Core) rememberQueuedTaskStatus(sessionID, executionID, queueID string, status TaskStatus) {
	c.mu.Lock()
	state := c.activeTasks[sessionID]
	if state.ExecutionID != executionID {
		state = TaskState{SessionID: sessionID, ExecutionID: executionID, QueueID: queueID}
	}
	state.Status = status
	c.activeTasks[sessionID] = state
	c.active = state
	c.mu.Unlock()
}
