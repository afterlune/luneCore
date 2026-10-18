package runtime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
)

func (c *Core) scheduleQueueDrain(sessionID string) {
	c.mu.Lock()
	if c.closed || c.closing || c.queueDraining[sessionID] {
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
		closed := c.closed || c.closing
		c.mu.Unlock()
		if closed {
			return
		}
		queued, err := c.sessions.QueuedTurns(context.Background(), sessionID)
		if err != nil {
			return
		}
		for _, item := range queued {
			if item.Status == session.QueuedTurnPaused {
				return
			}
			if item.Status == session.QueuedTurnActive {
				if state, ok := c.StateForSession(sessionID); ok && state.Status == TaskRunning {
					return
				}
				// The drain exited while this turn was still marked active but
				// no run owns it (for example a paused checkpoint could not be
				// persisted). A fresh drain reconciles active rows before
				// claiming work, so reschedule instead of leaving it orphaned.
				c.scheduleQueueDrain(sessionID)
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
		c.logger.Warn("reconcile queued turns before draining", "error", err)
		return
	}
	for {
		c.mu.RLock()
		closed := c.closed || c.closing
		c.mu.RUnlock()
		if closed {
			return
		}
		if state, ok := c.StateForSession(sessionID); ok && state.Status == TaskRunning {
			return
		}
		pending, found, err := c.pendingState(ctx, sessionID)
		if err != nil {
			c.logger.Warn("inspect checkpoint before queued turn", "error", err)
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
		c.queueMutationMu.Lock()
		queued, claimed, err := c.sessions.ClaimNextQueuedTurn(ctx, sessionID)
		c.queueMutationMu.Unlock()
		if err != nil {
			c.logger.Warn("claim queued turn", "error", err)
			return
		}
		if !claimed {
			return
		}
		c.mu.RLock()
		closing := c.closed || c.closing
		c.mu.RUnlock()
		if closing {
			if err := c.sessions.SetQueuedTurnStatus(ctx, sessionID, queued.QueueID, session.QueuedTurnQueued); err != nil {
				c.logger.Warn("return claimed turn to queue during close", "error", err)
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
		response, runErr := c.run(ctx, sessionID, request.MaxToolRounds, request.Model, request.Execution, request.ExecutionOverride, request.PermissionOverride, request.PlanMode, request.PlanAuthorization, request.NoInteractiveDecisions, request.Resume, input, queued.QueueID, queued.ExecutionID)
		c.mu.Lock()
		if c.executionRuns[queued.ExecutionID] == runDone {
			delete(c.executionRuns, queued.ExecutionID)
		}
		close(runDone)
		c.mu.Unlock()
		c.eventHub(sessionID).notifyDurable()
		c.mu.RLock()
		closing = c.closed || c.closing
		c.mu.RUnlock()
		if closing {
			if _, exists, lookupErr := c.sessions.QueuedTurnByExecutionID(ctx, sessionID, queued.ExecutionID); lookupErr == nil && exists {
				if err := c.sessions.SetQueuedTurnStatus(ctx, sessionID, queued.QueueID, session.QueuedTurnQueued); err != nil {
					c.logger.Warn("preserve queued turn during close", "error", err)
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
						modelFailure, failureErr := c.modelFailureArtifact(ctx, sessionID)
						if failureErr != nil {
							return 0, failureErr
						}
						return c.sessions.PauseQueuedTurnWithReason(ctx, sessionID, queued.QueueID, queued.ExecutionID, string(TaskPaused), string(event.BudgetReason), payload, modelFailure)
					}); persistErr != nil {
						c.logger.Warn("restore paused queued turn", "error", persistErr)
					}
				}
				if current.Status == session.QueuedTurnQueued {
					var request storedTurnRequest
					resumeQueued = json.Unmarshal(current.Request, &request) == nil && request.Resume
				} else {
					// The host may answer as soon as the wait event is emitted, while
					// this drain still owns the run. Queue that answer once the wait
					// checkpoint has committed, then continue this drain in place.
					inputs, inputErr := c.graphResumeInputs(ctx, sessionID, c.runtime.CheckpointID(sessionID), false)
					if inputErr != nil {
						c.logger.Warn("inspect answered graph wait", "error", inputErr)
					} else if len(inputs) > 0 {
						if _, _, recoverErr := c.recoverSession(ctx, sessionID, false); recoverErr == nil {
							resumeQueued = true
						} else if !errors.Is(recoverErr, ErrSessionBusy) {
							c.logger.Warn("queue answer received while graph was pausing", "error", recoverErr)
						}
					}
				}
			} else {
				completed, finalResponse, finalizeErr := c.completeQueuedTurnFromCheckpoint(ctx, current)
				if finalizeErr != nil {
					c.logger.Warn("finalize completed queued turn", "error", finalizeErr)
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
		c.logger.Warn("finish rejected queued turn", "error", persistErr)
	} else {
		c.notifyEventHub(queued.SessionID)
		if cleanupErr := c.sessions.DeleteExecutionTokenBudget(context.Background(), queued.ExecutionID); cleanupErr != nil {
			c.logger.Warn("delete failed execution token budget", "error", cleanupErr)
		}
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
