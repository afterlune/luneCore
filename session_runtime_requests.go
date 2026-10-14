package core

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/afterlune/luneCore/internal/agentgraph"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint"
)

func (r *SessionRuntime) PendingRequests(ctx context.Context) ([]PendingRequest, error) {
	if r == nil || r.core == nil || ctx == nil {
		return nil, errors.New("session runtime and context are required")
	}
	c := r.core
	c.mu.RLock()
	var requests []PendingRequest
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
		requests = append(requests, PendingRequest{Kind: PendingUserQuestion, UserQuestion: &publicQuestion})
	}
	pendingAcceptance, hasAcceptance, err := c.pendingAcceptance(ctx, r.sessionID)
	if err != nil {
		return nil, err
	}
	if hasAcceptance {
		requests = append(requests, PendingRequest{Kind: PendingAcceptanceRequest, Acceptance: &pendingAcceptance})
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

func (r *SessionRuntime) PendingAcceptance(ctx context.Context) (PendingAcceptance, bool, error) {
	if r == nil || r.core == nil || ctx == nil {
		return PendingAcceptance{}, false, errors.New("session runtime and context are required")
	}
	return r.core.pendingAcceptance(ctx, r.sessionID)
}

func (c *Core) pendingAcceptance(ctx context.Context, sessionID string) (PendingAcceptance, bool, error) {
	cp, err := c.runtime.CheckpointForSession(ctx, sessionID)
	if errors.Is(err, checkpoint.ErrNotFound) {
		return PendingAcceptance{}, false, nil
	}
	if err != nil {
		return PendingAcceptance{}, false, err
	}
	for _, invocation := range cp.Invocations {
		if invocation.Status != graph.InvocationWaiting || invocation.Continuation != agentgraph.AcceptanceContinuation || !invocation.State.AcceptancePaused {
			continue
		}
		state := invocation.State
		return PendingAcceptance{
			RunID: cp.RunID, InvocationID: invocation.ID, CallID: invocation.CallID, ExecutionID: state.ExecutionID,
			CandidateID: state.AcceptanceCandidateID, Candidate: state.AcceptanceCandidate, Kind: state.AcceptanceKind,
			Criteria:             append([]AcceptanceCriterion(nil), state.AcceptanceCriteria...),
			ClarificationAnswers: append([]string(nil), state.AcceptanceClarifications...),
			Evidence:             append([]AcceptanceEvidence(nil), state.AcceptanceEvidence...),
			Reports:              cloneAcceptanceReports(state.AcceptanceReports), Reason: state.AcceptancePauseReason, Repairs: state.AcceptanceRepairs,
		}, true, nil
	}
	return PendingAcceptance{}, false, nil
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

func cloneAcceptanceReports(reports []AcceptanceReport) []AcceptanceReport {
	cloned := make([]AcceptanceReport, len(reports))
	for i, report := range reports {
		cloned[i] = report
		cloned[i].Results = make([]AcceptanceCriterionResult, len(report.Results))
		for j, result := range report.Results {
			cloned[i].Results[j] = result
			cloned[i].Results[j].Evidence = append([]string(nil), result.Evidence...)
		}
	}
	return cloned
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
	case PendingAcceptanceRequest:
		if request.Acceptance != nil {
			return request.Acceptance.CallID
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

func (r *SessionRuntime) ResolveApproval(id string, allowed bool) error {
	if r == nil || r.core == nil || id == "" {
		return errors.New("session runtime and approval id are required")
	}
	return r.core.resolveApproval(r.sessionID, id, allowed)
}

// ResolveUserQuestion stores an answer and resumes the exact graph continuation.
func (r *SessionRuntime) ResolveUserQuestion(ctx context.Context, answer UserQuestionAnswer) error {
	if r == nil || r.core == nil || ctx == nil {
		return errors.New("session runtime and context are required")
	}
	return r.core.resolveUserQuestion(ctx, r.sessionID, answer)
}

// ResolveAcceptance explicitly confirms the candidate or grants a fresh pair
// of correction attempts for a paused acceptance.
func (r *SessionRuntime) ResolveAcceptance(ctx context.Context, resolution AcceptanceResolution) error {
	if r == nil || r.core == nil || ctx == nil {
		return errors.New("session runtime and context are required")
	}
	return r.core.resolveAcceptance(ctx, r.sessionID, resolution)
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
