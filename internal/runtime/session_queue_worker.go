package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
)

func (c *Core) scheduleQueueDrain(sessionID string) {
	c.mu.Lock()
	if c.closed || c.closing || c.queueDraining[sessionID] {
		c.mu.Unlock()
		return
	}
	c.eventHub(sessionID).setDrainError(nil)
	c.queueDraining[sessionID] = true
	c.queueDrainWG.Add(1)
	c.mu.Unlock()
	go c.drainQueue(sessionID)
}

func (c *Core) drainQueue(sessionID string) (drainErr error) {
	defer c.queueDrainWG.Done()
	defer func() {
		c.mu.Lock()
		c.eventHub(sessionID).setDrainError(drainErr)
		delete(c.queueDraining, sessionID)
		closed := c.closed || c.closing
		c.mu.Unlock()
		if closed || drainErr != nil {
			return
		}
		if err := c.releasePendingSessionLock(sessionID); err != nil {
			c.logger.Warn("release idle session lock", "session_id", sessionID, "error", err)
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
		if c.goalMayContinue(context.Background(), sessionID) {
			c.scheduleQueueDrain(sessionID)
		}
	}()
	ctx := context.Background()
	if err := c.reconcileQueuedTurns(ctx, sessionID); err != nil {
		c.logger.Warn("reconcile queued turns before draining", "error", err)
		return err
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
			return err
		}
		if found {
			queued, listErr := c.sessions.QueuedTurns(ctx, sessionID)
			if listErr != nil {
				return listErr
			}
			if len(queued) == 0 || queued[0].ExecutionID != pending.ExecutionID {
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
			return err
		}
		if !claimed {
			enqueued, err := c.enqueueGoalContinuation(ctx, sessionID)
			if err != nil {
				return err
			}
			if enqueued {
				continue
			}
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
			if persistErr := c.failQueuedTurn(ctx, queued, err); persistErr != nil {
				return persistErr
			}
			continue
		}
		if err := c.admitGoalExecution(ctx, sessionID, queued.ExecutionID, request); err != nil {
			if errors.Is(err, errStaleGoal) {
				goal, _ := c.sessions.Goal(ctx, sessionID)
				if goal != nil && goal.ID == request.GoalID && goal.Status != GoalActive {
					if err := c.sessions.SetQueuedTurnStatus(ctx, sessionID, queued.QueueID, session.QueuedTurnPaused); err != nil {
						return err
					}
					return
				}
			}
			if persistErr := c.failQueuedTurn(ctx, queued, err); persistErr != nil {
				return persistErr
			}
			continue
		}
		input, err := model.NewResponseInput(request.Input)
		if err != nil {
			if persistErr := c.failQueuedTurn(ctx, queued, err); persistErr != nil {
				return persistErr
			}
			continue
		}
		runDone := make(chan struct{})
		c.mu.Lock()
		c.executionRuns[queued.ExecutionID] = runDone
		c.mu.Unlock()
		goalStarted := time.Now()
		outcome := c.run(ctx, sessionID, request.MaxToolRounds, request.Model, request.Execution, request.ExecutionOverride, request.PermissionOverride, request.PlanMode, request.PlanAuthorization, request.NoInteractiveDecisions, request.Resume, input, queued.QueueID, queued.ExecutionID)
		interrupted := c.beginTaskFinish(queued.ExecutionID)
		if interrupted {
			outcome.Status = TaskCancelled
			outcome.Response = nil
		}
		c.mu.RLock()
		closing = c.closed || c.closing
		c.mu.RUnlock()
		var persistErr error
		if closing && len(outcome.History) == 0 {
			persistErr = c.sessions.SetQueuedTurnStatus(ctx, sessionID, queued.QueueID, session.QueuedTurnQueued)
		} else {
			persistCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			persistErr = c.commitRunOutcome(persistCtx, queued, outcome)
			cancel()
			if persistErr == nil {
				c.finishGoalExecution(sessionID, queued.ExecutionID, outcome.Response, outcome.Err, time.Since(goalStarted))
			}
		}
		c.goalMu.Lock()
		delete(c.goalRuns, queued.ExecutionID)
		c.goalMu.Unlock()
		c.endTaskFinish(queued.ExecutionID)
		c.mu.Lock()
		delete(c.executionRuns, queued.ExecutionID)
		close(runDone)
		c.mu.Unlock()
		c.notifyEventHub(sessionID)
		if persistErr != nil {
			c.rememberQueuedTaskStatus(sessionID, queued.ExecutionID, queued.QueueID, TaskFailed)
			c.logger.Warn("commit execution outcome", "execution_id", queued.ExecutionID, "error", persistErr)
			c.resolveTurnWaiter(queued.ExecutionID, turnResult{err: persistErr})
			return persistErr
		}
		if closing {
			c.resolveTurnWaiter(queued.ExecutionID, turnResult{err: errors.New("core is closing")})
			return
		}
		resultErr := outcome.Err
		if interrupted {
			resultErr = context.Canceled
		}
		c.resolveTurnWaiter(queued.ExecutionID, turnResult{response: outcome.Response, err: resultErr})
		if outcome.Status == TaskPaused {
			// An answer can arrive while the graph commits its wait. Admit it once;
			// the drain exit handoff picks up the queued resume without guessing a result.
			inputs, inputErr := c.graphResumeInputs(ctx, sessionID, c.runtime.CheckpointID(sessionID), false)
			if inputErr != nil {
				c.logger.Warn("inspect answered graph wait", "error", inputErr)
			} else if len(inputs) > 0 {
				if _, _, err := c.recoverSession(ctx, sessionID, false); err != nil && !errors.Is(err, ErrSessionBusy) {
					c.logger.Warn("queue answered graph wait", "error", err)
				}
			}
			return
		}
	}
}

func (c *Core) failQueuedTurn(ctx context.Context, queued session.QueuedTurn, err error) error {
	if err == nil {
		err = errors.New("queued task failed without an error")
	}
	event := Event{Kind: EventTaskFinished, SessionID: queued.SessionID, QueueID: queued.QueueID, ExecutionID: queued.ExecutionID, Status: TaskFailed, Err: errorString(err)}
	if _, persistErr := c.sessions.CompleteQueuedTurn(ctx, queued.SessionID, queued.QueueID, queued.ExecutionID, string(TaskFailed), nil, nil, nil, "", mustMarshalEvent(event), nil); persistErr != nil {
		c.logger.Warn("finish rejected queued turn", "error", persistErr)
		c.resolveTurnWaiter(queued.ExecutionID, turnResult{err: errors.Join(err, persistErr)})
		return persistErr
	} else {
		c.notifyEventHub(queued.SessionID)
		if cleanupErr := c.sessions.DeleteExecutionTokenBudget(context.Background(), queued.ExecutionID); cleanupErr != nil {
			c.logger.Warn("delete failed execution token budget", "error", cleanupErr)
		}
	}
	c.resolveTurnWaiter(queued.ExecutionID, turnResult{err: err})
	return nil
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
