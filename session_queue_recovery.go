package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/session"
	"github.com/afterlune/luneGraph/checkpoint"
)

func (c *Core) recoverSession(ctx context.Context, sessionID string, wait bool, executionOverride ...ExecutionOptions) (TurnSubmission, <-chan turnResult, error) {
	if c == nil || ctx == nil || sessionID == "" {
		return TurnSubmission{}, nil, errors.New("core, context, and session id are required")
	}
	if len(executionOverride) > 1 {
		return TurnSubmission{}, nil, errors.New("at most one execution options override may be supplied")
	}
	if len(executionOverride) == 1 {
		if err := validateExecutionOptions(executionOverride[0]); err != nil {
			return TurnSubmission{}, nil, err
		}
	}
	if _, err := c.GetSession(ctx, sessionID); err != nil {
		return TurnSubmission{}, nil, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return TurnSubmission{}, nil, errors.New("core is closed")
	}
	if _, err := c.sessions.Get(ctx, sessionID, c.workspace); err != nil {
		c.mu.Unlock()
		return TurnSubmission{}, nil, err
	}
	if c.sessionIsRunningLocked(sessionID) {
		c.mu.Unlock()
		return TurnSubmission{}, nil, ErrSessionBusy
	}
	pending, found, err := c.pendingState(ctx, sessionID)
	if err != nil {
		c.mu.Unlock()
		return TurnSubmission{}, nil, err
	}
	if !found {
		c.mu.Unlock()
		return TurnSubmission{}, nil, errors.New("no paused task exists in this session")
	}
	if pending.ExecutionID == "" {
		if pending.TaskSettingsVersion != agentgraph.CurrentTaskSettingsVersion {
			c.mu.Unlock()
			return TurnSubmission{}, nil, checkpointSettingsVersionError(pending.TaskSettingsVersion)
		}
		c.mu.Unlock()
		return TurnSubmission{}, nil, errors.New("paused task checkpoint is missing its execution identity; discard it and start a new task")
	}
	if pending.TaskSettingsVersion != agentgraph.CurrentTaskSettingsVersion {
		c.mu.Unlock()
		return TurnSubmission{}, nil, checkpointSettingsVersionError(pending.TaskSettingsVersion)
	}
	if pending.ModelFailure != nil && time.Now().Before(pending.ModelFailure.RetryNotBefore) {
		c.mu.Unlock()
		return TurnSubmission{}, nil, ErrModelRetryNotReady
	}
	if pending.ModelFailure != nil {
		if err := c.admitModelRecovery(ctx, sessionID); err != nil {
			c.mu.Unlock()
			return TurnSubmission{}, nil, err
		}
	}
	if agentgraph.IsCommandCompletionWait(pending) {
		if err := c.admitCommandCompletionRecovery(ctx, sessionID); err != nil {
			c.mu.Unlock()
			return TurnSubmission{}, nil, err
		}
	}
	if pending.AcceptancePaused {
		accepted, decisionErr := c.hasAcceptanceResolutionInput(ctx, sessionID)
		if decisionErr != nil {
			c.mu.Unlock()
			return TurnSubmission{}, nil, fmt.Errorf("inspect acceptance resolution: %w", decisionErr)
		}
		if !accepted {
			c.mu.Unlock()
			return TurnSubmission{}, nil, errors.New("paused acceptance requires an explicit continue or confirm decision")
		}
	}
	queued, err := c.sessions.QueuedTurns(ctx, sessionID)
	if err != nil {
		c.mu.Unlock()
		return TurnSubmission{}, nil, err
	}
	var item *session.QueuedTurn
	for i := range queued {
		if queued[i].ExecutionID == pending.ExecutionID {
			item = &queued[i]
			break
		}
	}
	if item == nil {
		queueID, idErr := newID()
		if idErr != nil {
			c.mu.Unlock()
			return TurnSubmission{}, nil, idErr
		}
		resumeRequest := storedTurnRequest{Input: json.RawMessage("null"), Resume: true}
		if len(executionOverride) == 1 {
			resumeRequest.Execution, resumeRequest.ExecutionOverride = executionOverride[0], true
		}
		request, _ := json.Marshal(resumeRequest)
		created, _, enqueueErr := c.sessions.EnqueueTurn(ctx, session.QueuedTurn{SessionID: sessionID, QueueID: queueID, Position: -1, ExecutionID: pending.ExecutionID, Request: request, Status: session.QueuedTurnQueued},
			mustMarshalEvent(Event{Kind: EventTaskQueued, SessionID: sessionID, ExecutionID: pending.ExecutionID, QueueID: queueID, Status: TaskQueued}))
		if enqueueErr != nil {
			c.mu.Unlock()
			return TurnSubmission{}, nil, enqueueErr
		}
		item = &created
		if hub := c.eventHubs[sessionID]; hub != nil {
			hub.notifyDurable()
		}
	} else {
		var request storedTurnRequest
		if err := json.Unmarshal(item.Request, &request); err != nil {
			c.mu.Unlock()
			return TurnSubmission{}, nil, fmt.Errorf("decode paused turn request: %w", err)
		}
		request.Resume = true
		if len(executionOverride) == 1 {
			request.Execution = executionOverride[0]
			request.ExecutionOverride = true
		}
		encoded, err := json.Marshal(request)
		if err != nil {
			c.mu.Unlock()
			return TurnSubmission{}, nil, err
		}
		event := Event{Kind: EventTaskQueued, SessionID: sessionID, QueueID: item.QueueID, ExecutionID: item.ExecutionID, Status: TaskQueued}
		if _, err := c.sessions.SetQueuedTurnWithEvent(ctx, sessionID, item.QueueID, session.QueuedTurnQueued, encoded, mustMarshalEvent(event)); err != nil {
			c.mu.Unlock()
			return TurnSubmission{}, nil, err
		}
		if hub := c.eventHubs[sessionID]; hub != nil {
			hub.notifyDurable()
		}
		item.Status, item.Request = session.QueuedTurnQueued, encoded
	}
	var waiter chan turnResult
	if wait {
		waiter = make(chan turnResult, 1)
		c.turnWaiters[pending.ExecutionID] = waiter
	}
	c.mu.Unlock()
	c.scheduleQueueDrain(sessionID)
	return TurnSubmission{Status: SubmissionAccepted, QueueID: item.QueueID, ExecutionID: item.ExecutionID, Position: item.Position}, waiter, nil
}

func (c *Core) reconcileQueuedTurns(ctx context.Context, sessionID string) error {
	queued, err := c.sessions.QueuedTurns(ctx, sessionID)
	if err != nil {
		return err
	}
	pending, found, err := c.pendingState(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, item := range queued {
		if item.Status != session.QueuedTurnActive {
			continue
		}
		completed, _, finalizeErr := c.completeQueuedTurnFromCheckpoint(ctx, item)
		if finalizeErr != nil {
			return finalizeErr
		}
		if completed {
			continue
		}
		if _, checkpointErr := c.runtime.CheckpointForSession(ctx, sessionID); errors.Is(checkpointErr, checkpoint.ErrNotFound) {
			c.failQueuedTurn(ctx, item, errors.New("active task checkpoint is missing; refusing to replay a turn that may have completed tool side effects"))
			continue
		} else if checkpointErr != nil {
			return checkpointErr
		}
		status := session.QueuedTurnQueued
		event := Event{Kind: EventTaskQueued, SessionID: sessionID, QueueID: item.QueueID, ExecutionID: item.ExecutionID, Status: TaskQueued}
		if found && pending.ExecutionID == item.ExecutionID {
			status = session.QueuedTurnPaused
			event.Kind, event.Status = EventTaskFinished, TaskPaused
		}
		var updateErr error
		if status == session.QueuedTurnPaused {
			_, updateErr = c.sessions.PauseQueuedTurn(ctx, sessionID, item.QueueID, item.ExecutionID, string(TaskPaused), mustMarshalEvent(event))
		} else {
			_, updateErr = c.sessions.SetQueuedTurnStatusWithEvent(ctx, sessionID, item.QueueID, status, mustMarshalEvent(event))
		}
		if updateErr != nil {
			return updateErr
		}
		if hub := c.eventHubs[sessionID]; hub != nil {
			hub.notifyDurable()
		}
	}
	// A committed result may survive a crash that happened before checkpoint
	// deletion. It cannot be resumed and should not block the next turn.
	if completedCheckpoint, err := c.runtime.CheckpointForSession(ctx, sessionID); err == nil && completedCheckpoint.Completed {
		if err := c.runtime.DeleteCompletedRunForSession(sessionID); err != nil {
			return err
		}
	} else if err != nil && !errors.Is(err, checkpoint.ErrNotFound) {
		return err
	}
	return nil
}

func (c *Core) completeQueuedTurnFromCheckpoint(ctx context.Context, queued session.QueuedTurn) (bool, json.RawMessage, error) {
	cp, err := c.runtime.CheckpointForSession(ctx, queued.SessionID)
	if errors.Is(err, checkpoint.ErrNotFound) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	if !cp.Completed {
		return false, nil, nil
	}
	state := cp.Final
	if state != nil && state.ExecutionID != "" && state.ExecutionID != queued.ExecutionID {
		return false, nil, nil
	}
	status := TaskFailed
	var response, history, modelContext, acceptanceReports, acceptanceCriteria, acceptanceEvidence json.RawMessage
	contextKey := ""
	if state != nil {
		if len(state.AcceptanceReports) > 0 {
			acceptanceReports, err = json.Marshal(state.AcceptanceReports)
			if err != nil {
				return true, nil, fmt.Errorf("encode acceptance reports: %w", err)
			}
		}
		if len(state.AcceptanceCriteria) > 0 {
			acceptanceCriteria, err = json.Marshal(state.AcceptanceCriteria)
			if err != nil {
				return true, nil, fmt.Errorf("encode acceptance criteria: %w", err)
			}
		}
		if len(state.AcceptanceEvidence) > 0 {
			acceptanceEvidence, err = json.Marshal(state.AcceptanceEvidence)
			if err != nil {
				return true, nil, fmt.Errorf("encode acceptance evidence: %w", err)
			}
		}
		transcript := state.Transcript
		if transcript.Items == nil && transcript.Text == nil {
			transcript = state.History
		}
		history, err = json.Marshal(&transcript)
		if err != nil {
			return true, nil, fmt.Errorf("encode completed transcript: %w", err)
		}
		modelContext, err = json.Marshal(&state.History)
		if err != nil {
			return true, nil, fmt.Errorf("encode completed model context: %w", err)
		}
		if state.FinalResponse != nil && state.TaskError == "" && cp.Failure == nil {
			response, err = json.Marshal(state.FinalResponse)
			if err != nil {
				return true, nil, fmt.Errorf("encode completed turn response: %w", err)
			}
			status = TaskCompleted
		}
	}
	saved, err := c.GetSession(ctx, queued.SessionID)
	if err != nil {
		return true, nil, err
	}
	contextKey = saved.ModelContextKey
	if state != nil && state.Provider != "" {
		if providerConfig, ok := c.models.ProviderConfig(state.Provider); ok {
			effectiveModel := state.Model
			if state.ModelOverride != "" {
				effectiveModel = state.ModelOverride
			}
			contextKey = modelContextKey(state.Provider, effectiveModel, providerConfig)
		}
	}
	if state != nil && state.TaskError != "" {
		response = nil
	}
	if status != TaskCompleted {
		response = nil
	}
	if err := c.commitQueuedTurn(ctx, queued, status, response, history, modelContext, contextKey, acceptanceReports, acceptanceCriteria, acceptanceEvidence); err != nil {
		return true, nil, err
	}
	return true, response, nil
}
