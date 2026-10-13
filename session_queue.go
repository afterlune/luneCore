package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/agentruntime"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
	"github.com/afterlune/luneGraph/checkpoint"
)

func (c *Core) submitTurn(ctx context.Context, sessionID string, request TurnRequest, mode SubmissionMode, wait bool) (TurnSubmission, <-chan turnResult, error) {
	if c == nil || ctx == nil || sessionID == "" {
		return TurnSubmission{}, nil, errors.New("core, context, and session id are required")
	}
	if mode != StartIfIdle && mode != QueueInput {
		return TurnSubmission{}, nil, fmt.Errorf("unsupported submission mode %q", mode)
	}
	if len(request.Input.Raw) == 0 || !json.Valid(request.Input.Raw) {
		return TurnSubmission{}, nil, errors.New("task input must be valid JSON")
	}
	input, err := model.NewResponseInput(request.Input.Raw)
	if err != nil {
		return TurnSubmission{}, nil, err
	}
	if err := validateTaskInputAttachments(input, c.attachmentLimits); err != nil {
		return TurnSubmission{}, nil, err
	}
	if request.MaxToolRounds < 0 {
		return TurnSubmission{}, nil, errors.New("maximum tool rounds must not be negative")
	}
	if err := validateExecutionOptions(request.Execution); err != nil {
		return TurnSubmission{}, nil, err
	}
	if _, err := c.GetSession(ctx, sessionID); err != nil {
		return TurnSubmission{}, nil, err
	}
	executionID, err := newID()
	if err != nil {
		return TurnSubmission{}, nil, err
	}
	queueID, err := newID()
	if err != nil {
		return TurnSubmission{}, nil, err
	}
	storedRequest, err := json.Marshal(storedTurnRequest{
		Input: append(json.RawMessage(nil), request.Input.Raw...),
		Model: request.Model, MaxToolRounds: request.MaxToolRounds, Execution: request.Execution,
	})
	if err != nil {
		return TurnSubmission{}, nil, fmt.Errorf("encode queued turn: %w", err)
	}
	status := SubmissionQueued
	if mode == StartIfIdle {
		status = SubmissionAccepted
	}
	var waiter chan turnResult
	if wait {
		waiter = make(chan turnResult, 1)
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
	if mode == StartIfIdle {
		if c.sessionIsRunningLocked(sessionID) {
			c.mu.Unlock()
			return TurnSubmission{}, nil, ErrSessionBusy
		}
		pending, pendingErr := c.runtime.HasPendingFor(sessionID, ctx)
		if pendingErr != nil {
			c.mu.Unlock()
			return TurnSubmission{}, nil, pendingErr
		}
		if pending {
			c.mu.Unlock()
			return TurnSubmission{}, nil, ErrTaskPaused
		}
		queued, queueErr := c.sessions.QueuedTurns(ctx, sessionID)
		if queueErr != nil {
			c.mu.Unlock()
			return TurnSubmission{}, nil, queueErr
		}
		if len(queued) > 0 {
			c.mu.Unlock()
			for _, item := range queued {
				if item.Status == session.QueuedTurnPaused {
					return TurnSubmission{}, nil, ErrTaskPaused
				}
			}
			return TurnSubmission{}, nil, ErrSessionBusy
		}
	}
	queued, _, err := c.sessions.EnqueueTurn(ctx, session.QueuedTurn{
		SessionID: sessionID, QueueID: queueID, ExecutionID: executionID, Request: storedRequest, Status: session.QueuedTurnQueued,
	}, mustMarshalEvent(Event{Kind: EventTaskQueued, SessionID: sessionID, ExecutionID: executionID, QueueID: queueID, Status: TaskQueued}))
	if err != nil {
		c.mu.Unlock()
		return TurnSubmission{}, nil, err
	}
	if waiter != nil {
		c.turnWaiters[executionID] = waiter
	}
	c.mu.Unlock()
	c.eventHub(sessionID).notifyDurable()
	c.scheduleQueueDrain(sessionID)
	return TurnSubmission{Status: status, QueueID: queued.QueueID, ExecutionID: executionID, Position: queued.Position}, waiter, nil
}

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
	var response, history, modelContext json.RawMessage
	contextKey := ""
	if state != nil {
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
	if err := c.commitQueuedTurn(ctx, queued, status, response, history, modelContext, contextKey); err != nil {
		return true, nil, err
	}
	return true, response, nil
}

func (c *Core) completeQueuedTurnFromRuntime(ctx context.Context, queued session.QueuedTurn, status TaskStatus, response json.RawMessage, runtime *agentruntime.Responder) error {
	var history, modelContext json.RawMessage
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
	}
	if status != TaskCompleted {
		response = nil
	}
	return c.commitQueuedTurn(ctx, queued, status, response, history, modelContext, contextKey)
}

func (c *Core) commitQueuedTurn(ctx context.Context, queued session.QueuedTurn, status TaskStatus, response, history, modelContext json.RawMessage, contextKey string) error {
	taskState, ok := c.StateForSession(queued.SessionID)
	var taskID string
	if ok && taskState.ExecutionID == queued.ExecutionID {
		taskID = taskState.TaskID
	}
	event := Event{Kind: EventTaskFinished, SessionID: queued.SessionID, TaskID: taskID, QueueID: queued.QueueID, ExecutionID: queued.ExecutionID, Status: status}
	if _, err := c.sessions.CompleteQueuedTurn(ctx, queued.SessionID, queued.QueueID, queued.ExecutionID, string(status), response, history, modelContext, contextKey, mustMarshalEvent(event)); err != nil {
		return err
	}
	c.rememberQueuedTaskStatus(queued.SessionID, queued.ExecutionID, queued.QueueID, status)
	if hub := c.eventHubs[queued.SessionID]; hub != nil {
		hub.notifyDurable()
	}
	if err := c.runtime.DeleteCompletedRunForSession(queued.SessionID); err != nil {
		slog.Warn("delete completed graph checkpoint", "error", err)
	}
	if err := c.sessions.DeleteToolInvocationsForSession(context.Background(), queued.SessionID); err != nil {
		slog.Warn("delete completed tool invocation records", "error", err)
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
