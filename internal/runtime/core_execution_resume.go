package runtime

import (
	"context"

	"github.com/afterlune/luneCore/internal/agentgraph"
	graph "github.com/afterlune/luneGraph"
)

func (c *Core) graphResumeInputs(ctx context.Context, sessionID, runID string) ([]graph.ResumeInput, error) {
	stored, err := c.sessions.GraphResumeInputs(ctx, sessionID, runID)
	if err != nil || len(stored) == 0 {
		return nil, err
	}
	cp, err := c.runtime.CheckpointForSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var inputs []graph.ResumeInput
	for _, input := range stored {
		for _, invocation := range cp.Invocations {
			if invocation.Status == graph.InvocationWaiting && (invocation.Continuation == agentgraph.UserQuestionContinuation || invocation.Continuation == agentgraph.AcceptanceContinuation || invocation.Continuation == agentgraph.ModelRecoveryContinuation || invocation.Continuation == agentgraph.ExecutionBlockerContinuation) && invocation.ID == input.InvocationID && invocation.CallID == input.CallID {
				inputs = append(inputs, graph.ResumeInput{InvocationID: input.InvocationID, CallID: input.CallID, Payload: append([]byte(nil), input.Payload...)})
				break
			}
		}
	}
	return inputs, nil
}

func (c *Core) reconcileGraphResumeInputs(ctx context.Context, sessionID, runID string) error {
	stored, err := c.sessions.GraphResumeInputs(ctx, sessionID, runID)
	if err != nil || len(stored) == 0 {
		return err
	}
	cp, err := c.runtime.CheckpointForSession(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, input := range stored {
		stillPending := false
		for _, invocation := range cp.Invocations {
			if invocation.Status == graph.InvocationWaiting && (invocation.Continuation == agentgraph.UserQuestionContinuation || invocation.Continuation == agentgraph.AcceptanceContinuation || invocation.Continuation == agentgraph.ModelRecoveryContinuation || invocation.Continuation == agentgraph.ExecutionBlockerContinuation) && invocation.ID == input.InvocationID && invocation.CallID == input.CallID {
				stillPending = true
				break
			}
		}
		if !stillPending {
			if err := c.sessions.MarkGraphResumeInputConsumed(ctx, sessionID, input); err != nil {
				return err
			}
		}
	}
	return nil
}
