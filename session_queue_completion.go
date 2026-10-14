package core

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/afterlune/luneCore/internal/agentruntime"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
)

func (c *Core) completeQueuedTurnFromRuntime(ctx context.Context, queued session.QueuedTurn, status TaskStatus, response json.RawMessage, runtime *agentruntime.Responder) error {
	var history, modelContext, acceptanceReports, acceptanceCriteria, acceptanceEvidence json.RawMessage
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
		if state, found := runtime.LastState(); found {
			if len(state.AcceptanceReports) > 0 {
				acceptanceReports, err = json.Marshal(state.AcceptanceReports)
				if err != nil {
					return fmt.Errorf("encode acceptance reports: %w", err)
				}
			}
			if len(state.AcceptanceCriteria) > 0 {
				acceptanceCriteria, err = json.Marshal(state.AcceptanceCriteria)
				if err != nil {
					return fmt.Errorf("encode acceptance criteria: %w", err)
				}
			}
			if len(state.AcceptanceEvidence) > 0 {
				acceptanceEvidence, err = json.Marshal(state.AcceptanceEvidence)
				if err != nil {
					return fmt.Errorf("encode acceptance evidence: %w", err)
				}
			}
		}
	}
	if status != TaskCompleted {
		response = nil
	}
	return c.commitQueuedTurn(ctx, queued, status, response, history, modelContext, contextKey, acceptanceReports, acceptanceCriteria, acceptanceEvidence)
}

func (c *Core) commitQueuedTurn(ctx context.Context, queued session.QueuedTurn, status TaskStatus, response, history, modelContext json.RawMessage, contextKey string, artifacts ...json.RawMessage) error {
	taskState, ok := c.StateForSession(queued.SessionID)
	var taskID string
	if ok && taskState.ExecutionID == queued.ExecutionID {
		taskID = taskState.TaskID
	}
	event := Event{Kind: EventTaskFinished, SessionID: queued.SessionID, TaskID: taskID, QueueID: queued.QueueID, ExecutionID: queued.ExecutionID, Status: status}
	var acceptanceReports, acceptanceCriteria, acceptanceEvidence json.RawMessage
	if len(artifacts) > 0 {
		acceptanceReports = artifacts[0]
	}
	if len(artifacts) > 1 {
		acceptanceCriteria = artifacts[1]
	}
	if len(artifacts) > 2 {
		acceptanceEvidence = artifacts[2]
	}
	if _, err := c.sessions.CompleteQueuedTurnWithAcceptance(ctx, queued.SessionID, queued.QueueID, queued.ExecutionID, string(status), response, history, modelContext, contextKey, acceptanceReports, acceptanceCriteria, acceptanceEvidence, mustMarshalEvent(event)); err != nil {
		return err
	}
	c.rememberQueuedTaskStatus(queued.SessionID, queued.ExecutionID, queued.QueueID, status)
	if hub := c.eventHubs[queued.SessionID]; hub != nil {
		hub.notifyDurable()
	}
	if err := c.runtime.DeleteCompletedRunForSession(queued.SessionID); err != nil {
		c.logger.Warn("delete completed graph checkpoint", "error", err)
	}
	if err := c.sessions.DeleteToolInvocationsForSession(context.Background(), queued.SessionID); err != nil {
		c.logger.Warn("delete completed tool invocation records", "error", err)
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
