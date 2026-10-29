package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	publictool "github.com/afterlune/luneCore/tool"

	"github.com/afterlune/luneCore/internal/agentgraph"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint"
)

func (r *SessionRuntime) PendingRequests(ctx context.Context) ([]PendingRequest, error) {
	if r == nil || r.core == nil || ctx == nil {
		return nil, errors.New("session runtime and context are required")
	}
	c := r.core
	var requests []PendingRequest
	c.mu.RLock()
	for _, pending := range c.approvals {
		if pending.sessionID == r.sessionID {
			copy := cloneApproval(pending.request)
			requests = append(requests, PendingRequest{Kind: PendingApproval, Approval: &copy})
		}
	}
	for _, pending := range c.interactions {
		if pending.sessionID == r.sessionID {
			copy := cloneInteraction(pending.request)
			requests = append(requests, PendingRequest{Kind: PendingUserInteraction, Interaction: &copy})
		}
	}
	c.mu.RUnlock()
	question, hasQuestion, err := c.runtime.PendingUserQuestion(ctx, r.sessionID)
	if err != nil {
		return nil, err
	}
	if hasQuestion {
		publicQuestion := publicUserQuestionRequest(question)
		draft, draftErr := c.sessions.UserQuestionDraft(ctx, r.sessionID, question.RunID, question.InvocationID, question.CallID)
		if draftErr != nil {
			return nil, draftErr
		}
		if len(draft) > 0 {
			publicQuestion.DraftPayload = append(json.RawMessage(nil), draft...)
			var parsed UserQuestionDraft
			if err := json.Unmarshal(draft, &parsed); err == nil {
				publicQuestion.Draft = &parsed
			}
		}
		requests = append(requests, PendingRequest{Kind: PendingUserQuestion, UserQuestion: &publicQuestion})
	}
	modelRecovery, hasModelRecovery, err := c.pendingModelRecovery(ctx, r.sessionID)
	if err != nil {
		return nil, err
	}
	if hasModelRecovery {
		requests = append(requests, PendingRequest{Kind: PendingModelRecoveryRequest, ModelRecovery: &modelRecovery})
	}
	blocker, hasBlocker, err := c.pendingExecutionBlocker(ctx, r.sessionID)
	if err != nil {
		return nil, err
	}
	if hasBlocker {
		requests = append(requests, PendingRequest{Kind: PendingExecutionBlocker, Blocker: &blocker})
	}
	recoveries, err := c.pendingToolRecoveries(ctx, r.sessionID)
	if err != nil {
		return nil, err
	}
	for i := range recoveries {
		copy := recoveries[i]
		requests = append(requests, PendingRequest{Kind: PendingToolRecovery, ToolRecovery: &copy})
	}
	sort.Slice(requests, func(i, j int) bool {
		if requests[i].Kind != requests[j].Kind {
			return requests[i].Kind < requests[j].Kind
		}
		return pendingRequestID(requests[i]) < pendingRequestID(requests[j])
	})
	return requests, nil
}

func (c *Core) pendingExecutionBlocker(ctx context.Context, sessionID string) (ExecutionBlocker, bool, error) {
	cp, err := c.runtime.CheckpointForSession(ctx, sessionID)
	if errors.Is(err, checkpoint.ErrNotFound) {
		return ExecutionBlocker{}, false, nil
	}
	if err != nil {
		return ExecutionBlocker{}, false, err
	}
	for _, invocation := range cp.Invocations {
		if invocation.Status != graph.InvocationWaiting || invocation.Continuation != agentgraph.ExecutionBlockerContinuation || invocation.State.BlockerID == "" {
			continue
		}
		return ExecutionBlocker{
			ID: invocation.State.BlockerID, ExecutionID: invocation.State.ExecutionID,
			Tool: invocation.State.BlockerTool, Reason: invocation.State.BlockerReason,
		}, true, nil
	}
	return ExecutionBlocker{}, false, nil
}

func pendingRequestID(request PendingRequest) string {
	switch request.Kind {
	case PendingApproval:
		if request.Approval != nil {
			return request.Approval.ID
		}
	case PendingUserQuestion:
		if request.UserQuestion != nil {
			return request.UserQuestion.ID
		}
	case PendingUserInteraction:
		if request.Interaction != nil {
			return request.Interaction.ID
		}
	case PendingToolRecovery:
		if request.ToolRecovery != nil {
			return request.ToolRecovery.InvocationID
		}
	case PendingModelRecoveryRequest:
		if request.ModelRecovery != nil {
			return request.ModelRecovery.CallID
		}
	case PendingExecutionBlocker:
		if request.Blocker != nil {
			return request.Blocker.ID
		}
	}
	return ""
}

func (r *SessionRuntime) ResolveApprovalDecision(ctx context.Context, id string, decision publictool.ApprovalDecision) error {
	if r == nil || r.core == nil || ctx == nil || id == "" {
		return errors.New("session runtime, context, and approval id are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.core.resolveApprovalDecision(ctx, r.sessionID, id, decision)
}

// ResolveUserQuestion stores an answer and resumes the exact graph continuation.
func (r *SessionRuntime) ResolveUserQuestion(ctx context.Context, answer UserQuestionAnswer) error {
	if r == nil || r.core == nil || ctx == nil {
		return errors.New("session runtime and context are required")
	}
	return r.core.resolveUserQuestion(ctx, r.sessionID, answer)
}

func (r *SessionRuntime) ResolveUserInteraction(id string, response json.RawMessage) error {
	if r == nil || r.core == nil || id == "" {
		return errors.New("session runtime and interaction id are required")
	}
	if len(response) == 0 || !json.Valid(response) {
		return errors.New("user interaction response must be valid JSON")
	}
	return r.core.resolveUserInteraction(r.sessionID, id, response)
}

func (r *SessionRuntime) ResolveToolRecovery(ctx context.Context, invocationID string, decision ToolRecoveryDecision) error {
	if r == nil || r.core == nil {
		return errors.New("session runtime is nil")
	}
	return r.core.resolveToolRecovery(ctx, r.sessionID, invocationID, decision)
}
