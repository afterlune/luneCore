package core

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
	"github.com/afterlune/luneGraph/checkpoint"
)

func (c *Core) scheduleQueueDrain(sessionID string) {
	c.mu.Lock()
	if c.closed || c.queueDraining[sessionID] {
		c.mu.Unlock()
		return
	}
	c.queueDraining[sessionID] = true
	c.queueDrainWG.Add(1)
	c.mu.Unlock()
	go c.drainQueue(sessionID)
}

func (c *Core) drainQueue(sessionID string) {
	defer c.queueDrainWG.Done()
	defer func() {
		c.mu.Lock()
		delete(c.queueDraining, sessionID)
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return
		}
		queued, err := c.sessions.QueuedTurns(context.Background(), sessionID)
		if err != nil {
			return
		}
		for _, item := range queued {
			if item.Status == session.QueuedTurnPaused || item.Status == session.QueuedTurnActive {
				return
			}
			if item.Status == session.QueuedTurnQueued {
				c.scheduleQueueDrain(sessionID)
				return
			}
		}
	}()
	ctx := context.Background()
	if err := c.reconcileQueuedTurns(ctx, sessionID); err != nil {
		slog.Warn("reconcile queued turns before draining", "error", err)
		return
	}
	for {
		c.mu.RLock()
		closed := c.closed
		c.mu.RUnlock()
		if closed {
			return
		}
		if state, ok := c.StateForSession(sessionID); ok && state.Status == TaskRunning {
			return
		}
		pending, found, err := c.pendingState(ctx, sessionID)
		if err != nil {
			slog.Warn("inspect checkpoint before queued turn", "error", err)
			return
		}
		if found {
			queued, listErr := c.sessions.QueuedTurns(ctx, sessionID)
			if listErr != nil || len(queued) == 0 || queued[0].ExecutionID != pending.ExecutionID {
				return
			}
			var request storedTurnRequest
			if json.Unmarshal(queued[0].Request, &request) != nil || !request.Resume {
				return
			}
		}
		queued, claimed, err := c.sessions.ClaimNextQueuedTurn(ctx, sessionID)
		if err != nil {
			slog.Warn("claim queued turn", "error", err)
			return
		}
		if !claimed {
			return
		}
		c.mu.RLock()
		closing := c.closed
		c.mu.RUnlock()
		if closing {
			if err := c.sessions.SetQueuedTurnStatus(ctx, sessionID, queued.QueueID, session.QueuedTurnQueued); err != nil {
				slog.Warn("return claimed turn to queue during close", "error", err)
			}
			c.resolveTurnWaiter(queued.ExecutionID, turnResult{err: errors.New("core is closing")})
			return
		}
		var request storedTurnRequest
		if err := json.Unmarshal(queued.Request, &request); err != nil {
			c.failQueuedTurn(ctx, queued, err)
			continue
		}
		input, err := model.NewResponseInput(request.Input)
		if err != nil {
			c.failQueuedTurn(ctx, queued, err)
			continue
		}
		runDone := make(chan struct{})
		c.mu.Lock()
		c.executionRuns[queued.ExecutionID] = runDone
		c.mu.Unlock()
		response, runErr := c.run(ctx, sessionID, request.PlanMode, request.MaxToolRounds, request.Model, request.Execution, request.ExecutionOverride, request.Resume, input, queued.QueueID, queued.ExecutionID)
		c.mu.Lock()
		if c.executionRuns[queued.ExecutionID] == runDone {
			delete(c.executionRuns, queued.ExecutionID)
		}
		close(runDone)
		c.mu.Unlock()
		c.eventHub(sessionID).notifyDurable()
		c.mu.RLock()
		closing = c.closed
		c.mu.RUnlock()
		if closing {
			if _, exists, lookupErr := c.sessions.QueuedTurnByExecutionID(ctx, sessionID, queued.ExecutionID); lookupErr == nil && exists {
				if err := c.sessions.SetQueuedTurnStatus(ctx, sessionID, queued.QueueID, session.QueuedTurnQueued); err != nil {
					slog.Warn("preserve queued turn during close", "error", err)
				}
			}
			c.resolveTurnWaiter(queued.ExecutionID, turnResult{err: errors.New("core is closing")})
			return
		}
		resumeQueued := false
		if current, exists, lookupErr := c.sessions.QueuedTurnByExecutionID(ctx, sessionID, queued.ExecutionID); lookupErr == nil && exists {
			state, ok := c.StateForSession(sessionID)
			if ok && state.Status == TaskPaused {
				if current.Status != session.QueuedTurnPaused && current.Status != session.QueuedTurnQueued {
					event := Event{Kind: EventTaskFinished, SessionID: sessionID, TaskID: state.TaskID, QueueID: queued.QueueID, ExecutionID: queued.ExecutionID, Status: TaskPaused, BudgetReason: budgetReasonFromError(runErr), Err: errorString(runErr)}
					if persistErr := c.persistQueuedEvent(ctx, event, func(payload json.RawMessage) (int64, error) {
						return c.sessions.PauseQueuedTurnWithReason(ctx, sessionID, queued.QueueID, queued.ExecutionID, string(TaskPaused), string(event.BudgetReason), payload)
					}); persistErr != nil {
						slog.Warn("restore paused queued turn", "error", persistErr)
					}
				}
				if current.Status == session.QueuedTurnQueued {
					var request storedTurnRequest
					resumeQueued = json.Unmarshal(current.Request, &request) == nil && request.Resume
				} else {
					// The host may answer as soon as the wait event is emitted, while
					// this drain still owns the run. Queue that answer once the wait
					// checkpoint has committed, then continue this drain in place.
					inputs, inputErr := c.graphResumeInputs(ctx, sessionID, c.runtime.CheckpointID(sessionID))
					if inputErr != nil {
						slog.Warn("inspect answered graph wait", "error", inputErr)
					} else if len(inputs) > 0 {
						if _, _, recoverErr := c.recoverSession(ctx, sessionID, false); recoverErr == nil {
							resumeQueued = true
						} else if !errors.Is(recoverErr, ErrSessionBusy) {
							slog.Warn("queue answer received while graph was pausing", "error", recoverErr)
						}
					}
				}
			} else {
				completed, finalResponse, finalizeErr := c.completeQueuedTurnFromCheckpoint(ctx, current)
				if finalizeErr != nil {
					slog.Warn("finalize completed queued turn", "error", finalizeErr)
					c.resolveTurnWaiter(queued.ExecutionID, turnResult{response: response, err: finalizeErr})
					return
				}
				if completed {
					if finalResponse != nil {
						response = finalResponse
					}
				} else {
					c.failQueuedTurn(ctx, queued, runErr)
				}
			}
		}
		c.resolveTurnWaiter(queued.ExecutionID, turnResult{response: response, err: runErr})
		if state, ok := c.StateForSession(sessionID); ok && state.Status == TaskPaused {
			if resumeQueued {
				continue
			}
			return
		}
	}
}

func (c *Core) failQueuedTurn(ctx context.Context, queued session.QueuedTurn, err error) {
	event := Event{Kind: EventTaskFinished, SessionID: queued.SessionID, QueueID: queued.QueueID, ExecutionID: queued.ExecutionID, Status: TaskFailed, Err: errorString(err)}
	if _, persistErr := c.sessions.CompleteQueuedTurn(ctx, queued.SessionID, queued.QueueID, queued.ExecutionID, string(TaskFailed), nil, nil, nil, "", mustMarshalEvent(event)); persistErr != nil {
		slog.Warn("finish rejected queued turn", "error", persistErr)
	} else if hub := c.eventHubs[queued.SessionID]; hub != nil {
		hub.notifyDurable()
	}
	c.resolveTurnWaiter(queued.ExecutionID, turnResult{err: err})
}

func (c *Core) resolveTurnWaiter(executionID string, result turnResult) {
	c.mu.Lock()
	waiter := c.turnWaiters[executionID]
	delete(c.turnWaiters, executionID)
	c.mu.Unlock()
	if waiter != nil {
		waiter <- result
	}
}

func (c *Core) interruptExecution(sessionID, executionID string) bool {
	c.mu.Lock()
	if state, ok := c.activeTasks[sessionID]; ok && state.ExecutionID == executionID && state.Status == TaskRunning {
		cancel := c.taskCancels[sessionID]
		c.mu.Unlock()
		if cancel != nil {
			cancel()
			return true
		}
		return false
	}
	queued, found, err := c.sessions.QueuedTurnByExecutionID(context.Background(), sessionID, executionID)
	if err != nil || !found {
		c.mu.Unlock()
		return false
	}
	if queued.Status == session.QueuedTurnActive {
		c.interruptRequests[executionID] = true
		c.mu.Unlock()
		return true
	}
	if queued.Status != session.QueuedTurnQueued {
		c.mu.Unlock()
		return false
	}
	c.interruptRequests[executionID] = true
	c.mu.Unlock()
	event := Event{Kind: EventTaskFinished, SessionID: sessionID, QueueID: queued.QueueID, ExecutionID: executionID, Status: TaskCancelled, Err: "interrupted before the turn started"}
	deleted, _, err := c.sessions.CancelQueuedTurn(context.Background(), sessionID, queued.QueueID, executionID, string(TaskCancelled), mustMarshalEvent(event))
	if err == nil && deleted {
		c.mu.Lock()
		delete(c.interruptRequests, executionID)
		c.mu.Unlock()
		c.eventHub(sessionID).notifyDurable()
		c.resolveTurnWaiter(executionID, turnResult{err: context.Canceled})
		return true
	}
	c.mu.Lock()
	state, running := c.activeTasks[sessionID]
	if running && state.ExecutionID == executionID && state.Status == TaskRunning {
		cancel := c.taskCancels[sessionID]
		c.mu.Unlock()
		if cancel != nil {
			cancel()
			return true
		}
		return false
	}
	current, exists, lookupErr := c.sessions.QueuedTurnByExecutionID(context.Background(), sessionID, executionID)
	if lookupErr == nil && exists && current.Status == session.QueuedTurnActive {
		c.mu.Unlock()
		return true
	}
	delete(c.interruptRequests, executionID)
	c.mu.Unlock()
	return false
}

func (c *Core) takeInterruptRequest(executionID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	requested := c.interruptRequests[executionID]
	delete(c.interruptRequests, executionID)
	return requested
}

func (c *Core) discardPaused(ctx context.Context, sessionID string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false, errors.New("core is closed")
	}
	if c.sessionIsRunningLocked(sessionID) {
		return false, errors.New("cannot discard a task while another task is running")
	}
	queued, err := c.sessions.QueuedTurns(ctx, sessionID)
	if err != nil {
		return false, err
	}
	var paused *session.QueuedTurn
	for i := range queued {
		if queued[i].Status == session.QueuedTurnPaused {
			paused = &queued[i]
			break
		}
	}
	parentID := c.runtime.CheckpointID(sessionID)
	delete(c.liveExecutions, parentID)
	deletedCheckpoint := false
	if err := c.checkpoints.Delete(ctx, parentID); err == nil {
		deletedCheckpoint = true
	} else if !errors.Is(err, checkpoint.ErrNotFound) {
		return false, err
	}
	if err := c.sessions.DeleteToolInvocationsForSession(ctx, sessionID); err != nil {
		return false, err
	}
	if err := c.sessions.DeleteGraphResumeInputs(ctx, sessionID, parentID); err != nil {
		return false, err
	}
	deletedQueue := false
	if paused != nil {
		event := Event{Kind: EventTaskDiscarded, SessionID: sessionID, QueueID: paused.QueueID, ExecutionID: paused.ExecutionID, Status: TaskDiscarded}
		deletedQueue, _, err = c.sessions.DiscardQueuedTurn(ctx, sessionID, paused.QueueID, paused.ExecutionID, string(TaskDiscarded), mustMarshalEvent(event))
		if err != nil {
			return false, err
		}
		if deletedQueue {
			if hub := c.eventHubs[sessionID]; hub != nil {
				hub.notifyDurable()
			}
		}
	} else if deletedCheckpoint {
		event := Event{Kind: EventTaskDiscarded, SessionID: sessionID, Status: TaskIdle}
		if _, err := c.sessions.AppendRuntimeEvent(ctx, sessionID, mustMarshalEvent(event)); err != nil {
			return false, err
		}
		if hub := c.eventHubs[sessionID]; hub != nil {
			hub.notifyDurable()
		}
	}
	if state, ok := c.activeTasks[sessionID]; ok && state.Status == TaskPaused && (deletedCheckpoint || deletedQueue) {
		state.Status, state.QueueID = TaskIdle, ""
		c.activeTasks[sessionID] = state
		if c.active.SessionID == sessionID {
			c.active = state
		}
	}
	return deletedCheckpoint || deletedQueue, nil
}

func (c *Core) waitForTurn(ctx context.Context, runtime *SessionRuntime, executionID string, waiter <-chan turnResult) (json.RawMessage, error) {
	if waiter == nil {
		return nil, errors.New("turn completion channel is unavailable")
	}
	select {
	case result := <-waiter:
		return result.response, result.err
	case <-ctx.Done():
		runtime.Interrupt(executionID)
		result := <-waiter
		if result.err != nil {
			return result.response, result.err
		}
		return result.response, ctx.Err()
	}
}

func (c *Core) runSynchronous(ctx context.Context, request TaskRequest) (json.RawMessage, error) {
	if len(request.Input.Raw) == 0 || !json.Valid(request.Input.Raw) {
		return nil, errors.New("task input must be valid JSON")
	}
	input, err := model.NewResponseInput(request.Input.Raw)
	if err != nil {
		return nil, err
	}
	if err := validateTaskInputAttachments(input, c.attachmentLimits); err != nil {
		return nil, err
	}
	runtime, err := c.OpenSessionRuntime(ctx, request.SessionID)
	if err != nil {
		return nil, err
	}
	cursor, err := c.sessions.LatestRuntimeEventCursor(ctx, request.SessionID)
	if err != nil {
		return nil, err
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	events, err := runtime.subscribeEvents(streamCtx, cursor, false)
	if err != nil {
		return nil, err
	}
	go func() {
		for range events {
		}
	}()
	submission, waiter, err := c.submitTurn(ctx, request.SessionID, TurnRequest{
		Input: request.Input, PlanMode: request.PlanMode, Model: request.Model, MaxToolRounds: request.MaxToolRounds,
		Execution: request.Execution,
	}, StartIfIdle, true)
	if err != nil {
		return nil, err
	}
	c.scheduleQueueDrain(request.SessionID)
	return c.waitForTurn(ctx, runtime, submission.ExecutionID, waiter)
}
