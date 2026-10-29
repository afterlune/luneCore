package runtime

import (
	"context"
	"encoding/json"

	"github.com/afterlune/luneCore/internal/session"
)

func (c *Core) commitRunOutcome(ctx context.Context, queued session.QueuedTurn, outcome runOutcome) error {
	if outcome.Status != TaskPaused {
		return c.commitQueuedTurn(ctx, queued, outcome.Status, outcome.Response, outcome.History, outcome.ModelContext, outcome.ContextKey, outcome.AgentContext, errorString(outcome.Err))
	}
	state, _ := c.StateForSession(queued.SessionID)
	event := Event{Kind: EventTaskFinished, SessionID: queued.SessionID, TaskID: state.TaskID, ExecutionID: queued.ExecutionID, QueueID: queued.QueueID, Status: TaskPaused, BudgetReason: outcome.BudgetReason, Err: errorString(outcome.Err), ContextWindow: state.ContextWindow, ModelProfile: state.ModelProfile, Provider: state.Provider, Model: state.Model}
	failure, err := c.modelFailureArtifact(ctx, queued.SessionID)
	if err != nil {
		return err
	}
	if _, err := c.sessions.PauseQueuedTurnWithReason(ctx, queued.SessionID, queued.QueueID, queued.ExecutionID, string(TaskPaused), string(outcome.BudgetReason), mustMarshalEvent(event), failure); err != nil {
		return err
	}
	c.rememberQueuedTaskStatus(queued.SessionID, queued.ExecutionID, queued.QueueID, TaskPaused)
	c.notifyEventHub(queued.SessionID)
	if outcome.Question != nil {
		c.emit(Event{Kind: EventUserQuestionRequired, SessionID: queued.SessionID, TaskID: state.TaskID, ExecutionID: queued.ExecutionID, UserQuestion: outcome.Question})
	}
	return nil
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
	if err := c.sessions.DeleteToolInvocationsForSession(ctx, queued.SessionID); err != nil {
		c.logger.Warn("delete completed tool invocation records", "error", err)
	}
	if err := c.sessions.DeleteExecutionTokenBudget(ctx, queued.ExecutionID); err != nil {
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
