package agentruntime

import (
	"context"
	"errors"
	"fmt"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint"
)

func (r *Responder) PendingHistoryFor(ctx context.Context, sessionID string) (*model.ResponseInput, error) {
	state, ok, err := r.PendingStateFor(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	if state.Transcript.Items != nil || state.Transcript.Text != nil {
		return model.NewResponseInputItems(responseHistoryItems(&state.Transcript)...), nil
	}
	return model.NewResponseInputItems(responseHistoryItems(&state.History)...), nil
}

func (r *Responder) PendingPlanModeFor(ctx context.Context, sessionID string) (bool, error) {
	state, ok, err := r.PendingStateFor(ctx, sessionID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	return state.PlanMode, nil
}

func (r *Responder) PendingStateFor(ctx context.Context, sessionID string) (agentgraph.State, bool, error) {
	return r.PendingStateForRunID(ctx, r.CheckpointID(sessionID))
}

func (r *Responder) PendingStateForRunID(ctx context.Context, runID string) (agentgraph.State, bool, error) {
	return pendingStateForRunID(ctx, r.store, runID)
}

func (r *Responder) PendingUserQuestion(ctx context.Context, sessionID string) (PendingQuestion, bool, error) {
	if ctx == nil {
		return PendingQuestion{}, false, errors.New("context is nil")
	}
	var cp graph.Checkpoint[agentgraph.State]
	var pending []graph.PendingInvocation
	if r.execution != nil && r.runID() == r.CheckpointID(sessionID) {
		pending = r.execution.Pending()
		var err error
		cp, err = r.execution.Checkpoint()
		if err != nil {
			return PendingQuestion{}, false, err
		}
	} else {
		var err error
		cp, err = r.store.Load(ctx, r.CheckpointID(sessionID))
		if err != nil {
			if errors.Is(err, checkpoint.ErrNotFound) {
				return PendingQuestion{}, false, nil
			}
			return PendingQuestion{}, false, err
		}
		for _, invocation := range cp.Invocations {
			if invocation.Status == graph.InvocationWaiting {
				pending = append(pending, graph.PendingInvocation{
					InvocationID: invocation.ID, CallID: invocation.CallID, Node: invocation.Node, Continuation: invocation.Continuation,
				})
			}
		}
	}
	for _, wait := range pending {
		if wait.Continuation != agentgraph.UserQuestionContinuation {
			continue
		}
		var state *agentgraph.State
		for index := range cp.Invocations {
			invocation := &cp.Invocations[index]
			if invocation.ID == wait.InvocationID && invocation.CallID == wait.CallID {
				state = &invocation.State
				break
			}
		}
		if state == nil || state.UserQuestion == "" {
			continue
		}
		return PendingQuestion{
			ID: wait.CallID, AgentID: state.AgentID, RunID: cp.RunID,
			InvocationID: wait.InvocationID, CallID: wait.CallID, ExecutionID: state.ExecutionID,
			Question: state.UserQuestion, Options: append([]string(nil), state.UserQuestionOptions...),
		}, true, nil
	}
	return PendingQuestion{}, false, nil
}

// CheckpointForSession uses luneGraph's durable completion contract: Store.Load
// returns Completed, Final, Failure, and HadLocalFailures, and Store.Delete
// removes the checkpoint after the session transaction commits.
func (r *Responder) CheckpointForSession(ctx context.Context, sessionID string) (graph.Checkpoint[agentgraph.State], error) {
	return r.store.Load(ctx, r.CheckpointID(sessionID))
}

func (r *Responder) DeleteCompletedRunForSession(sessionID string) error {
	err := r.store.Delete(context.Background(), r.CheckpointID(sessionID))
	if errors.Is(err, checkpoint.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete completed task checkpoint: %w", err)
	}
	return nil
}

func pendingStateForRunID(ctx context.Context, store graph.Store[agentgraph.State], runID string) (agentgraph.State, bool, error) {
	checkpoint, err := store.Load(ctx, runID)
	if err != nil {
		return agentgraph.State{}, false, err
	}
	if checkpoint.Completed {
		return agentgraph.State{}, false, nil
	}
	for _, invocation := range checkpoint.Invocations {
		if invocation.ID == "i1" && invocation.Status != graph.InvocationEnded && invocation.Status != graph.InvocationFailed && invocation.Status != graph.InvocationJoined {
			return invocation.State, true, nil
		}
	}
	for index := len(checkpoint.Invocations) - 1; index >= 0; index-- {
		invocation := checkpoint.Invocations[index]
		if invocation.Status == graph.InvocationEnded || invocation.Status == graph.InvocationFailed || invocation.Status == graph.InvocationJoined {
			continue
		}
		return invocation.State, true, nil
	}
	if checkpoint.Final != nil {
		return *checkpoint.Final, true, nil
	}
	return agentgraph.State{}, false, nil
}

func (r *Responder) runID() string {
	return r.CheckpointID(r.sessionID)
}

func (r *Responder) RunID() string {
	return r.runID()
}

func (r *Responder) CheckpointID(sessionID string) string {
	if sessionID == "" || sessionID == activeRunID {
		return activeRunID
	}
	return "lune-session-" + sessionID
}
