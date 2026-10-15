package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
	"github.com/afterlune/luneGraph/checkpoint"
)

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
	if c.closed || c.closing {
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
	discardedExecutionID := ""
	if paused != nil {
		discardedExecutionID = paused.ExecutionID
	} else if state, ok := c.activeTasks[sessionID]; ok && state.Status == TaskPaused {
		discardedExecutionID = state.ExecutionID
	}
	if discardedExecutionID == "" {
		pending, found, err := c.pendingState(ctx, sessionID)
		if err != nil {
			return false, err
		}
		if found {
			discardedExecutionID = pending.ExecutionID
		}
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 35*time.Second)
	cleanupErr := c.commands.StopSession(cleanupCtx, sessionID)
	cleanupCancel()
	if cleanupErr != nil {
		return false, cleanupErr
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
			c.notifyEventHub(sessionID)
		}
	} else if deletedCheckpoint {
		event := Event{Kind: EventTaskDiscarded, SessionID: sessionID, Status: TaskIdle}
		if _, err := c.sessions.AppendRuntimeEvent(ctx, sessionID, mustMarshalEvent(event)); err != nil {
			return false, err
		}
		c.notifyEventHub(sessionID)
	}
	if discardedExecutionID != "" && (deletedCheckpoint || deletedQueue) {
		if err := c.sessions.DeleteExecutionTokenBudget(ctx, discardedExecutionID); err != nil {
			return false, err
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

func (c *Core) waitForTurn(ctx context.Context, runtime *SessionRuntime, executionID string, waiter <-chan turnResult) (TaskResult, error) {
	if waiter == nil {
		return TaskResult{}, errors.New("turn completion channel is unavailable")
	}
	select {
	case result := <-waiter:
		stored, err := runtime.WaitTurn(context.Background(), executionID)
		if err != nil {
			return TaskResult{}, errors.Join(result.err, err)
		}
		return stored, result.err
	case <-ctx.Done():
		runtime.Interrupt(executionID)
		result := <-waiter
		stored, storedErr := runtime.WaitTurn(context.Background(), executionID)
		if storedErr != nil {
			return TaskResult{}, errors.Join(result.err, storedErr)
		}
		if result.err != nil {
			return stored, result.err
		}
		return stored, ctx.Err()
	}
}

func (c *Core) runSynchronous(ctx context.Context, request TaskRequest) (TaskResult, error) {
	if len(request.Input.Raw) == 0 || !json.Valid(request.Input.Raw) {
		return TaskResult{}, errors.New("task input must be valid JSON")
	}
	input, err := model.NewResponseInput(request.Input.Raw)
	if err != nil {
		return TaskResult{}, err
	}
	if err := validateTaskInputAttachments(input, c.attachmentLimits); err != nil {
		return TaskResult{}, err
	}
	runtime, err := c.OpenSessionRuntime(ctx, request.SessionID)
	if err != nil {
		return TaskResult{}, err
	}
	cursor, err := c.sessions.LatestRuntimeEventCursor(ctx, request.SessionID)
	if err != nil {
		return TaskResult{}, err
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	events, err := runtime.subscribeEvents(streamCtx, cursor, false)
	if err != nil {
		return TaskResult{}, err
	}
	go func() {
		for range events {
		}
	}()
	submission, waiter, err := c.submitTurn(ctx, request.SessionID, TurnRequest{
		Input: request.Input, Model: request.Model, MaxToolRounds: request.MaxToolRounds,
		Execution: request.Execution, PlanMode: request.PlanMode, PlanAuthorization: request.PlanAuthorization,
		NoInteractiveDecisions: request.NoInteractiveDecisions,
	}, StartIfIdle, true)
	if err != nil {
		return TaskResult{}, err
	}
	c.scheduleQueueDrain(request.SessionID)
	return c.waitForTurn(ctx, runtime, submission.ExecutionID, waiter)
}
