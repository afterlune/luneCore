package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/agentruntime"
	publictool "github.com/afterlune/luneCore/tool"
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
	ModelFailure *ModelFailure
	ExecutionID  string
	Status       TaskStatus
	Response     json.RawMessage
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
	PendingModelRecoveryRequest PendingRequestKind = "model_recovery"
	PendingApproval             PendingRequestKind = "approval"
	// PendingUserQuestion is a lead-agent clarification waiting for the host.
	PendingUserQuestion      PendingRequestKind = "user_question"
	PendingUserInteraction   PendingRequestKind = "user_interaction"
	PendingToolRecovery      PendingRequestKind = "tool_recovery"
	PendingAcceptanceRequest PendingRequestKind = "acceptance"
	PendingExecutionBlocker  PendingRequestKind = "execution_blocker"
)

type PendingRequest struct {
	Kind          PendingRequestKind
	ModelRecovery *PendingModelRecovery
	Approval      *ApprovalRequest
	UserQuestion  *UserQuestionRequest
	Interaction   *UserInteractionRequest
	ToolRecovery  *ToolRecoveryRequest
	Acceptance    *PendingAcceptance
	Blocker       *ExecutionBlocker
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
			var failure *ModelFailure
			if len(stored.ModelFailure) > 0 {
				if err := json.Unmarshal(stored.ModelFailure, &failure); err != nil {
					return TurnResult{}, fmt.Errorf("decode model failure: %w", err)
				}
			}
			var plan *ProposedPlan
			if TaskStatus(stored.Status) == TaskCompleted {
				plan = extractProposedPlan(response)
			}
			return TurnResult{
				ModelFailure: failure, ExecutionID: stored.ExecutionID,
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
