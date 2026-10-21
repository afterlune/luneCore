package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/session"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint"
)

func (c *Core) ForkSession(ctx context.Context, sessionID string) (Session, error) {
	if c == nil || ctx == nil || sessionID == "" {
		return Session{}, errors.New("core, context, and session id are required")
	}
	var forked Session
	err := c.withSessionLock(sessionID, func() error {
		var err error
		forked, err = c.forkSessionOwned(ctx, sessionID)
		return err
	})
	return forked, err
}

func (c *Core) forkSessionOwned(ctx context.Context, sessionID string) (Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.closing {
		return Session{}, errors.New("core is closed")
	}
	if c.sessionIsRunningLocked(sessionID) {
		return Session{}, errors.New("cannot fork a session while its task is running")
	}
	c.usageMu.Lock()
	defer c.usageMu.Unlock()
	saved, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return Session{}, err
	}
	return c.createSession(ctx, saved.History, saved.Settings)
}

// ForkPausedTask clones a paused, nonterminal task checkpoint into a sibling
// session. The returned session remains paused until Recover is called.
func (c *Core) ForkPausedTask(ctx context.Context, sessionID string) (Session, error) {
	if c == nil || ctx == nil || sessionID == "" {
		return Session{}, errors.New("core, context, and session id are required")
	}
	var forked Session
	err := c.withSessionLock(sessionID, func() error {
		var err error
		forked, err = c.forkPausedTaskOwned(ctx, sessionID)
		return err
	})
	return forked, err
}

func (c *Core) forkPausedTaskOwned(ctx context.Context, sessionID string) (Session, error) {
	c.runtimeConfigMu.RLock()
	defer c.runtimeConfigMu.RUnlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.closing {
		return Session{}, errors.New("core is closed")
	}
	if c.sessionIsRunningLocked(sessionID) || c.queueDraining[sessionID] {
		return Session{}, errors.New("cannot fork a task while its session is running")
	}
	c.usageMu.Lock()
	defer c.usageMu.Unlock()

	saved, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return Session{}, err
	}
	sourceRunID := c.runtime.CheckpointID(sessionID)
	sourceCheckpoint, err := c.checkpoints.Load(ctx, sourceRunID)
	if err != nil {
		return Session{}, fmt.Errorf("load paused task checkpoint: %w", err)
	}
	if sourceCheckpoint.Completed {
		return Session{}, graph.ErrRunCompleted
	}
	var pending agentgraph.State
	foundPending := false
	for _, invocation := range sourceCheckpoint.Invocations {
		switch invocation.Status {
		case graph.InvocationEnded, graph.InvocationFailed, graph.InvocationJoined:
			continue
		default:
			pending, foundPending = invocation.State, true
			break
		}
		if foundPending {
			break
		}
	}
	if !foundPending || pending.ExecutionID == "" || pending.TaskSettingsVersion != agentgraph.CurrentTaskSettingsVersion {
		return Session{}, errors.New("session does not contain a forkable paused task")
	}
	queuedTurns, err := c.sessions.QueuedTurns(ctx, sessionID)
	if err != nil {
		return Session{}, err
	}
	var sourceTurn *session.QueuedTurn
	for i := range queuedTurns {
		if queuedTurns[i].ExecutionID == pending.ExecutionID && queuedTurns[i].Status == session.QueuedTurnPaused {
			sourceTurn = &queuedTurns[i]
			break
		}
	}
	if sourceTurn == nil {
		return Session{}, errors.New("paused task is missing its durable turn record")
	}
	var turnRequest storedTurnRequest
	if err := json.Unmarshal(sourceTurn.Request, &turnRequest); err != nil {
		return Session{}, fmt.Errorf("decode paused task request: %w", err)
	}
	if turnRequest.ExecutionOverride {
		pending.Execution = agentExecutionOptions(turnRequest.Execution)
	}
	turnRequest.Resume = true
	requestJSON, err := json.Marshal(turnRequest)
	if err != nil {
		return Session{}, fmt.Errorf("encode forked task request: %w", err)
	}
	newExecutionID, err := newID()
	if err != nil {
		return Session{}, err
	}
	newQueueID, err := newID()
	if err != nil {
		return Session{}, err
	}
	forkedSession, err := c.createSession(ctx, saved.History, saved.Settings)
	if err != nil {
		return Session{}, err
	}
	newRunID := c.runtime.CheckpointID(forkedSession.ID)
	rollback := func(cause error) (Session, error) {
		cause = errors.Join(cause, c.sessions.DeleteExecutionTokenBudget(context.Background(), newExecutionID))
		if deleteErr := c.checkpoints.Delete(context.Background(), newRunID); deleteErr != nil && !errors.Is(deleteErr, checkpoint.ErrNotFound) {
			cause = errors.Join(cause, deleteErr)
		}
		cause = errors.Join(cause, c.sessions.DeleteToolInvocationsForSession(context.Background(), forkedSession.ID))
		cause = errors.Join(cause, c.sessions.DeleteGraphResumeInputs(context.Background(), forkedSession.ID, newRunID))
		if _, deleteErr := c.sessions.Delete(context.Background(), forkedSession.ID, c.workspace); deleteErr != nil {
			cause = errors.Join(cause, deleteErr)
		}
		return Session{}, cause
	}
	profileID := pending.ModelProfile
	if profileID == "" {
		profileID = saved.Settings.ModelProfile
	}
	forkRuntime, _, err := c.configureRuntime(profileID, pending.ReasoningEffort, TaskState{SessionID: forkedSession.ID, Execution: coreExecutionOptions(pending.Execution)})
	if err != nil {
		return rollback(fmt.Errorf("prepare paused task fork: %w", err))
	}
	forkedCheckpoint, err := forkRuntime.Fork(ctx, newRunID, sourceCheckpoint)
	if err != nil {
		return rollback(fmt.Errorf("clone paused task checkpoint: %w", err))
	}
	for i := range forkedCheckpoint.Invocations {
		state := agentgraph.RekeyForkState(forkedCheckpoint.Invocations[i].State, newRunID, newExecutionID)
		state.Execution = pending.Execution
		forkedCheckpoint.Invocations[i].State = state
	}
	if forkedCheckpoint.Final != nil {
		state := agentgraph.RekeyForkState(*forkedCheckpoint.Final, newRunID, newExecutionID)
		state.Execution = pending.Execution
		forkedCheckpoint.Final = &state
	}
	if err := c.checkpoints.Create(ctx, forkedCheckpoint); err != nil {
		return rollback(fmt.Errorf("save forked task checkpoint: %w", err))
	}
	if err := c.sessions.SetExecutionTokenBudget(ctx, forkedSession.ID, newExecutionID, pending.Execution.TokenBudget); err != nil {
		return rollback(fmt.Errorf("initialize forked task token budget: %w", err))
	}
	var budgetReason string
	result, found, resultErr := c.sessions.TurnResult(ctx, sessionID, pending.ExecutionID)
	if resultErr != nil {
		return rollback(fmt.Errorf("read paused turn result: %w", resultErr))
	}
	if found {
		budgetReason = result.BudgetReason
	}
	event := Event{Kind: EventTaskFinished, SessionID: forkedSession.ID, QueueID: newQueueID, ExecutionID: newExecutionID,
		Status: TaskPaused, BudgetReason: BudgetReason(budgetReason)}
	if err := c.sessions.ForkPausedTask(ctx, sessionID, sourceRunID, pending.ExecutionID, forkedSession.ID, newRunID,
		newExecutionID, newQueueID, requestJSON, mustMarshalEvent(event)); err != nil {
		return rollback(fmt.Errorf("copy paused task receipts: %w", err))
	}
	return forkedSession, nil
}
