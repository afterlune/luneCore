package runtime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/session"
	publicmodel "github.com/afterlune/luneCore/model"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint"
)

var ErrModelRetryNotReady = errors.New("model retry is not ready; wait until RetryNotBefore")

type ModelFailure = publicmodel.Failure

// PendingModelRecovery identifies the saved request awaiting explicit recovery.
type PendingModelRecovery struct {
	RunID        string
	InvocationID string
	CallID       string
	ExecutionID  string
	Failure      ModelFailure
}

func (r *SessionRuntime) PendingModelRecovery(ctx context.Context) (PendingModelRecovery, bool, error) {
	if r == nil || r.core == nil || ctx == nil {
		return PendingModelRecovery{}, false, errors.New("session runtime and context are required")
	}
	return r.core.pendingModelRecovery(ctx, r.sessionID)
}

func (c *Core) pendingModelRecovery(ctx context.Context, sessionID string) (PendingModelRecovery, bool, error) {
	cp, err := c.runtime.CheckpointForSession(ctx, sessionID)
	if errors.Is(err, checkpoint.ErrNotFound) {
		return PendingModelRecovery{}, false, nil
	}
	if err != nil {
		return PendingModelRecovery{}, false, err
	}
	for _, invocation := range cp.Invocations {
		if invocation.Status == graph.InvocationWaiting && invocation.Continuation == agentgraph.ModelRecoveryContinuation && invocation.State.ModelFailure != nil {
			return PendingModelRecovery{RunID: cp.RunID, InvocationID: invocation.ID, CallID: invocation.CallID, ExecutionID: invocation.State.ExecutionID, Failure: *invocation.State.ModelFailure}, true, nil
		}
	}
	return PendingModelRecovery{}, false, nil
}

func (c *Core) admitModelRecovery(ctx context.Context, sessionID string) error {
	pending, found, err := c.pendingModelRecovery(ctx, sessionID)
	if err != nil || !found {
		return err
	}
	_, err = c.sessions.PutGraphResumeInput(ctx, sessionID, session.GraphResumeInput{RunID: pending.RunID, InvocationID: pending.InvocationID, CallID: pending.CallID, Payload: json.RawMessage(`""`)})
	return err
}

func (c *Core) modelFailureArtifact(ctx context.Context, sessionID string) (json.RawMessage, error) {
	state, found, err := c.runtime.PendingStateFor(ctx, sessionID)
	if err != nil || !found || state.ModelFailure == nil {
		return nil, err
	}
	return json.Marshal(state.ModelFailure)
}

func (c *Core) onModelRecoveryEvent(sessionID string, state agentgraph.State, stage string, failure publicmodel.Failure) {
	kind := EventModelRetry
	if stage == "required" {
		kind = EventModelRecoveryRequired
	}
	if stage == "resumed" {
		kind = EventModelRecoveryResumed
	}
	task, _ := c.StateForSession(sessionID)
	c.emit(Event{Kind: kind, SessionID: sessionID, TaskID: task.TaskID, ExecutionID: state.ExecutionID, AgentID: state.AgentID, Provider: state.Provider, ModelProfile: state.ModelProfile, Model: state.Model, ModelFailure: &failure})
}
