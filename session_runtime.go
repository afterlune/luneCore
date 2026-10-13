package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/agentruntime"
	"github.com/afterlune/luneCore/internal/session"
	publictool "github.com/afterlune/luneCore/tool"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint"
)

var (
	ErrSessionBusy  = errors.New("session already has active or queued work")
	ErrTaskPaused   = agentruntime.ErrTaskPaused
	ErrTurnNotFound = errors.New("execution id does not exist in this session")
)

// TaskPausedError identifies a resumable task pause and its optional budget cause.
type TaskPausedError = agentruntime.TaskPausedError

func budgetReasonFromError(err error) BudgetReason {
	var paused *TaskPausedError
	if errors.As(err, &paused) {
		return paused.BudgetReason
	}
	return ""
}

type SubmissionMode string

const (
	StartIfIdle SubmissionMode = "start_if_idle"
	QueueInput  SubmissionMode = "queue_input"
)

type SubmissionStatus string

const (
	SubmissionAccepted SubmissionStatus = "accepted"
	SubmissionQueued   SubmissionStatus = "queued"
)

type TurnRequest struct {
	Input                  TaskInput
	Model                  string
	MaxToolRounds          int
	Execution              ExecutionOptions
	PlanAuthorization      publictool.PlanAuthorization
	NoInteractiveDecisions bool
}

type TurnSubmission struct {
	Status      SubmissionStatus
	QueueID     string
	ExecutionID string
	Position    int64
}

type QueuedTurnState struct {
	QueueID     string
	Position    int64
	ExecutionID string
	Status      string
	CreatedAt   time.Time
}

type RuntimeState struct {
	Task  TaskState
	Queue []QueuedTurnState
}

// TaskResult is the shared synchronous and asynchronous outcome shape.
type TaskResult struct {
	ExecutionID string
	Status      TaskStatus
	Response    json.RawMessage
	// ProposedPlan is set only for a completed response containing one complete
	// proposed-plan block. Hosts can offer handoff choices when it is non-nil.
	ProposedPlan       *ProposedPlan
	BudgetReason       BudgetReason
	AcceptanceCriteria []AcceptanceCriterion
	AcceptanceEvidence []AcceptanceEvidence
	// AcceptanceReports contains every candidate judgment for this execution.
	AcceptanceReports []AcceptanceReport
}

type TurnResult = TaskResult

type PendingRequestKind string

const (
	PendingApproval PendingRequestKind = "approval"
	// PendingUserQuestion is a lead-agent clarification waiting for the host.
	PendingUserQuestion      PendingRequestKind = "user_question"
	PendingUserInteraction   PendingRequestKind = "user_interaction"
	PendingToolRecovery      PendingRequestKind = "tool_recovery"
	PendingAcceptanceRequest PendingRequestKind = "acceptance"
	PendingExecutionBlocker  PendingRequestKind = "execution_blocker"
)

type PendingRequest struct {
	Kind         PendingRequestKind
	Approval     *ApprovalRequest
	UserQuestion *UserQuestionRequest
	Interaction  *UserInteractionRequest
	ToolRecovery *ToolRecoveryRequest
	Acceptance   *PendingAcceptance
	Blocker      *ExecutionBlocker
}

// SessionRuntime is a handle to one session's turns, event log and pending
// requests. Accepted turns outlive the context used to submit them.
type SessionRuntime struct {
	core      *Core
	sessionID string
}

type storedTurnRequest struct {
	Input                  json.RawMessage              `json:"input"`
	Model                  string                       `json:"model,omitempty"`
	MaxToolRounds          int                          `json:"max_tool_rounds,omitempty"`
	Execution              ExecutionOptions             `json:"execution,omitempty"`
	ExecutionOverride      bool                         `json:"execution_override,omitempty"`
	PermissionOverride     string                       `json:"permission_override,omitempty"`
	PlanAuthorization      publictool.PlanAuthorization `json:"plan_authorization,omitempty"`
	NoInteractiveDecisions bool                         `json:"no_interactive_decisions,omitempty"`
	Resume                 bool                         `json:"resume,omitempty"`
}

type turnResult struct {
	response json.RawMessage
	err      error
}

func (c *Core) OpenSessionRuntime(ctx context.Context, sessionID string) (*SessionRuntime, error) {
	if c == nil || ctx == nil || sessionID == "" {
		return nil, errors.New("core, context, and session id are required")
	}
	if _, err := c.GetSession(ctx, sessionID); err != nil {
		return nil, err
	}
	hub := c.eventHub(sessionID)
	c.mu.Lock()
	if c.closed {
		if c.eventHubs[sessionID] == hub {
			delete(c.eventHubs, sessionID)
		}
		c.mu.Unlock()
		return nil, errors.New("core is closed")
	}
	if _, err := c.sessions.Get(ctx, sessionID, c.workspace); err != nil {
		if c.eventHubs[sessionID] == hub {
			delete(c.eventHubs, sessionID)
		}
		c.mu.Unlock()
		return nil, err
	}
	reconcile := !c.sessionIsRunningLocked(sessionID) && !c.queueDraining[sessionID]
	c.mu.Unlock()
	if reconcile {
		if err := c.reconcileQueuedTurns(ctx, sessionID); err != nil {
			return nil, err
		}
	}
	c.scheduleQueueDrain(sessionID)
	return &SessionRuntime{core: c, sessionID: sessionID}, nil
}

func (c *Core) pendingState(ctx context.Context, sessionID string) (agentgraph.State, bool, error) {
	state, found, err := c.runtime.PendingStateFor(ctx, sessionID)
	if errors.Is(err, checkpoint.ErrNotFound) {
		return agentgraph.State{}, false, nil
	}
	return state, found, err
}

func (r *SessionRuntime) SessionID() string {
	if r == nil {
		return ""
	}
	return r.sessionID
}

func (r *SessionRuntime) SubmitTurn(ctx context.Context, request TurnRequest, mode SubmissionMode) (TurnSubmission, error) {
	if r == nil || r.core == nil {
		return TurnSubmission{}, errors.New("session runtime is nil")
	}
	submission, _, err := r.core.submitTurn(ctx, r.sessionID, request, mode, false)
	return submission, err
}

// WaitTurn returns the durable outcome for an accepted execution. Canceling
// ctx stops this wait only; it does not interrupt the turn.
func (r *SessionRuntime) WaitTurn(ctx context.Context, executionID string) (TurnResult, error) {
	if r == nil || r.core == nil || ctx == nil || executionID == "" {
		return TurnResult{}, errors.New("session runtime, context, and execution id are required")
	}
	if _, err := r.core.GetSession(ctx, r.sessionID); err != nil {
		return TurnResult{}, err
	}
	hub := r.core.eventHub(r.sessionID)
	r.core.scheduleQueueDrain(r.sessionID)
	for {
		changed := hub.changeChannel()
		stored, found, err := r.core.sessions.TurnResult(ctx, r.sessionID, executionID)
		if err != nil {
			return TurnResult{}, err
		}
		if found {
			r.core.mu.RLock()
			runDone := r.core.executionRuns[executionID]
			r.core.mu.RUnlock()
			if runDone != nil {
				select {
				case <-runDone:
					continue
				case <-ctx.Done():
					return TurnResult{}, ctx.Err()
				}
			}
			response := append(json.RawMessage(nil), stored.Response...)
			var reports []AcceptanceReport
			if len(stored.AcceptanceReports) > 0 {
				if err := json.Unmarshal(stored.AcceptanceReports, &reports); err != nil {
					return TurnResult{}, fmt.Errorf("decode acceptance reports: %w", err)
				}
			}
			var criteria []AcceptanceCriterion
			if len(stored.AcceptanceCriteria) > 0 {
				if err := json.Unmarshal(stored.AcceptanceCriteria, &criteria); err != nil {
					return TurnResult{}, fmt.Errorf("decode acceptance criteria: %w", err)
				}
			}
			var evidence []AcceptanceEvidence
			if len(stored.AcceptanceEvidence) > 0 {
				if err := json.Unmarshal(stored.AcceptanceEvidence, &evidence); err != nil {
					return TurnResult{}, fmt.Errorf("decode acceptance evidence: %w", err)
				}
			}
			var plan *ProposedPlan
			if TaskStatus(stored.Status) == TaskCompleted {
				plan = extractProposedPlan(response)
			}
			return TurnResult{
				ExecutionID:        stored.ExecutionID,
				Status:             TaskStatus(stored.Status),
				Response:           response,
				ProposedPlan:       plan,
				BudgetReason:       BudgetReason(stored.BudgetReason),
				AcceptanceReports:  reports,
				AcceptanceCriteria: criteria,
				AcceptanceEvidence: evidence,
			}, nil
		}
		_, queued, err := r.core.sessions.QueuedTurnByExecutionID(ctx, r.sessionID, executionID)
		if err != nil {
			return TurnResult{}, err
		}
		if !queued {
			if _, err := r.core.GetSession(ctx, r.sessionID); err != nil {
				return TurnResult{}, err
			}
			return TurnResult{}, ErrTurnNotFound
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return TurnResult{}, ctx.Err()
		}
	}
}

// SubscribeEvents replays durable events after afterCursor, then follows new
// events. Model stream deltas are live-only and may be dropped if a consumer is slow.
func (r *SessionRuntime) SubscribeEvents(ctx context.Context, afterCursor int64) (<-chan Event, error) {
	return r.subscribeEvents(ctx, afterCursor, true)
}

func (r *SessionRuntime) subscribeEvents(ctx context.Context, afterCursor int64, schedule bool) (<-chan Event, error) {
	if r == nil || r.core == nil || ctx == nil || afterCursor < 0 {
		return nil, errors.New("session runtime, context, and a non-negative event cursor are required")
	}
	if _, err := r.core.GetSession(ctx, r.sessionID); err != nil {
		return nil, err
	}
	hub := r.core.eventHub(r.sessionID)
	live, unsubscribe := hub.subscribeLive()
	if _, err := r.core.GetSession(ctx, r.sessionID); err != nil {
		unsubscribe()
		return nil, err
	}
	output := make(chan Event, 128)
	go func() {
		defer close(output)
		defer unsubscribe()
		cursor := afterCursor
		for {
			if hub.isClosed() {
				return
			}
			changed := hub.changeChannel()
			events, err := r.core.sessions.RuntimeEventsAfter(ctx, r.sessionID, cursor, 200)
			if err != nil {
				select {
				case output <- Event{Kind: EventRuntimeStreamFailed, SessionID: r.sessionID, Err: err.Error()}:
				case <-ctx.Done():
				}
				return
			}
			for _, stored := range events {
				var event Event
				if err := json.Unmarshal(stored.Payload, &event); err != nil {
					select {
					case output <- Event{Kind: EventRuntimeStreamFailed, SessionID: r.sessionID, Err: err.Error()}:
					case <-ctx.Done():
					}
					return
				}
				event.Cursor, event.OccurredAt = stored.Cursor, stored.CreatedAt
				select {
				case output <- event:
					cursor = stored.Cursor
				case <-ctx.Done():
					return
				}
			}
			if len(events) == 200 {
				continue
			}
			if hub.isClosed() {
				return
			}
			select {
			case event := <-live:
				select {
				case output <- event:
				case <-ctx.Done():
					return
				}
			case <-changed:
			case <-ctx.Done():
				return
			}
		}
	}()
	if schedule {
		r.core.scheduleQueueDrain(r.sessionID)
	}
	return output, nil
}

func (r *SessionRuntime) State(ctx context.Context) (RuntimeState, error) {
	if r == nil || r.core == nil || ctx == nil {
		return RuntimeState{}, errors.New("session runtime and context are required")
	}
	state, ok := r.core.StateForSession(r.sessionID)
	if !ok {
		state = TaskState{SessionID: r.sessionID, Status: TaskIdle}
	}
	queued, err := r.core.sessions.QueuedTurns(ctx, r.sessionID)
	if err != nil {
		return RuntimeState{}, err
	}
	result := RuntimeState{Task: state}
	for _, item := range queued {
		result.Queue = append(result.Queue, QueuedTurnState{QueueID: item.QueueID, Position: item.Position, ExecutionID: item.ExecutionID, Status: item.Status, CreatedAt: item.CreatedAt})
		if result.Task.Status != TaskRunning && item.Status == session.QueuedTurnPaused {
			result.Task = TaskState{SessionID: r.sessionID, ExecutionID: item.ExecutionID, QueueID: item.QueueID, Status: TaskPaused}
			break
		} else if result.Task.Status != TaskRunning && (item.Status == session.QueuedTurnQueued || item.Status == session.QueuedTurnActive) {
			result.Task = TaskState{SessionID: r.sessionID, ExecutionID: item.ExecutionID, QueueID: item.QueueID, Status: TaskQueued}
			break
		}
	}
	if result.Task.Status != TaskRunning {
		pending, found, err := r.core.pendingState(ctx, r.sessionID)
		if err != nil {
			return RuntimeState{}, err
		}
		if found {
			recoveryQueued := false
			for _, item := range queued {
				if item.ExecutionID == pending.ExecutionID && (item.Status == session.QueuedTurnQueued || item.Status == session.QueuedTurnActive) {
					recoveryQueued = true
					break
				}
			}
			if !recoveryQueued {
				result.Task = TaskState{SessionID: r.sessionID, ExecutionID: pending.ExecutionID, Status: TaskPaused}
			}
		}
	}
	return result, nil
}

func (r *SessionRuntime) Interrupt(executionID string) bool {
	return r != nil && r.core != nil && executionID != "" && r.core.interruptExecution(r.sessionID, executionID)
}

func (r *SessionRuntime) Recover(ctx context.Context, options ...ExecutionOptions) (TurnSubmission, error) {
	if r == nil || r.core == nil {
		return TurnSubmission{}, errors.New("session runtime is nil")
	}
	submission, _, err := r.core.recoverSession(ctx, r.sessionID, false, options...)
	return submission, err
}

func (r *SessionRuntime) DiscardPaused(ctx context.Context) (bool, error) {
	if r == nil || r.core == nil || ctx == nil {
		return false, errors.New("session runtime and context are required")
	}
	deleted, err := r.core.discardPaused(ctx, r.sessionID)
	if err == nil && deleted {
		r.core.scheduleQueueDrain(r.sessionID)
	}
	return deleted, err
}

func (r *SessionRuntime) SetUserInteractionAvailable(available bool) {
	if r == nil || r.core == nil {
		return
	}
	r.core.mu.Lock()
	if available {
		r.core.interactiveSessions[r.sessionID] = true
	} else {
		delete(r.core.interactiveSessions, r.sessionID)
	}
	r.core.mu.Unlock()
}

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
