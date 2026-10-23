package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/agentruntime"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
	publictool "github.com/afterlune/luneCore/tool"
	"github.com/afterlune/luneGraph/checkpoint"
)

var (
	ErrSessionBusy       = errors.New("session already has active or queued work")
	ErrTaskPaused        = agentruntime.ErrTaskPaused
	ErrTurnNotFound      = errors.New("execution id does not exist in this session")
	ErrQueuedTurnStarted = errors.New("queued turn has already started")
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
	// PermissionMode pins this turn to a host-selected mode. Empty uses Core defaults.
	PermissionMode         PermissionMode
	Input                  TaskInput
	Model                  string
	MaxToolRounds          int
	Execution              ExecutionOptions
	PlanMode               bool
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
	ProposedPlan *ProposedPlan
	BudgetReason BudgetReason
}

type TurnResult = TaskResult

type PendingRequestKind string

const (
	PendingModelRecoveryRequest PendingRequestKind = "model_recovery"
	PendingApproval             PendingRequestKind = "approval"
	// PendingUserQuestion is a lead-agent clarification waiting for the host.
	PendingUserQuestion     PendingRequestKind = "user_question"
	PendingUserInteraction  PendingRequestKind = "user_interaction"
	PendingToolRecovery     PendingRequestKind = "tool_recovery"
	PendingExecutionBlocker PendingRequestKind = "execution_blocker"
)

type PendingRequest struct {
	Kind          PendingRequestKind
	ModelRecovery *PendingModelRecovery
	Approval      *ApprovalRequest
	UserQuestion  *UserQuestionRequest
	Interaction   *UserInteractionRequest
	ToolRecovery  *ToolRecoveryRequest
	Blocker       *ExecutionBlocker
}

// SessionRuntime is a handle to one session's turns, event log and pending
// requests. Accepted turns outlive the context used to submit them.
type SessionRuntime struct {
	core      *Core
	sessionID string
	closeMu   sync.Mutex
	closed    bool
}

type storedTurnRequest struct {
	Input                  json.RawMessage              `json:"input"`
	Model                  string                       `json:"model,omitempty"`
	MaxToolRounds          int                          `json:"max_tool_rounds,omitempty"`
	Execution              ExecutionOptions             `json:"execution,omitempty"`
	ExecutionOverride      bool                         `json:"execution_override,omitempty"`
	PermissionOverride     string                       `json:"permission_override,omitempty"`
	PlanMode               bool                         `json:"plan_mode,omitempty"`
	PlanAuthorization      publictool.PlanAuthorization `json:"plan_authorization,omitempty"`
	NoInteractiveDecisions bool                         `json:"no_interactive_decisions,omitempty"`
	Resume                 bool                         `json:"resume,omitempty"`
}

type turnResult struct {
	response json.RawMessage
	err      error
}

const (
	legacyAcceptanceMachineID           = "lune-agent-v3"
	legacyAcceptanceTaskSettingsVersion = 9
)

func (c *Core) OpenSessionRuntime(ctx context.Context, sessionID string) (_ *SessionRuntime, err error) {
	if c == nil || ctx == nil || sessionID == "" {
		return nil, errors.New("core, context, and session id are required")
	}
	if err := c.beginSessionOpen(sessionID); err != nil {
		return nil, err
	}
	defer c.sessionOps.Done()
	keepSessionLock := false
	defer func() {
		if !keepSessionLock {
			if releaseErr := c.releaseSessionLock(sessionID); releaseErr != nil {
				err = errors.Join(err, fmt.Errorf("release session lock after open failure: %w", releaseErr))
			}
		}
	}()
	if _, err := c.GetSession(ctx, sessionID); err != nil {
		return nil, err
	}
	hub := c.eventHub(sessionID)
	c.mu.Lock()
	if c.closed || c.closing {
		c.mu.Unlock()
		c.removeEventHub(sessionID, hub)
		return nil, errors.New("core is closed")
	}
	if _, err := c.sessions.Get(ctx, sessionID, c.workspace); err != nil {
		c.mu.Unlock()
		c.removeEventHub(sessionID, hub)
		return nil, err
	}
	reconcile := !c.sessionIsRunningLocked(sessionID) && !c.queueDraining[sessionID]
	c.mu.Unlock()
	if reconcile {
		if err := c.reconcileQueuedTurns(ctx, sessionID); err != nil {
			return nil, err
		}
	}
	if err := c.discardObsoletePausedTask(ctx, sessionID); err != nil {
		c.removeEventHub(sessionID, hub)
		return nil, err
	}
	c.mu.RLock()
	closing := c.closed || c.closing
	c.mu.RUnlock()
	if closing {
		c.removeEventHub(sessionID, hub)
		return nil, errors.New("core is closed")
	}
	c.scheduleQueueDrain(sessionID)
	keepSessionLock = true
	return &SessionRuntime{core: c, sessionID: sessionID}, nil
}

// Close releases this runtime's session ownership. Do not use the runtime
// after Close. Active or queued work keeps the session locked until it drains.
func (r *SessionRuntime) Close() error {
	if r == nil || r.core == nil {
		return nil
	}
	r.closeMu.Lock()
	defer r.closeMu.Unlock()
	if r.closed {
		return nil
	}
	if err := r.core.releaseSessionLock(r.sessionID); err != nil {
		return err
	}
	r.closed = true
	return nil
}

func (c *Core) discardObsoletePausedTask(ctx context.Context, sessionID string) error {
	cp, err := c.runtime.CheckpointForSession(ctx, sessionID)
	if errors.Is(err, checkpoint.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if cp.MachineID != legacyAcceptanceMachineID {
		return nil
	}
	state, found, err := c.pendingState(ctx, sessionID)
	// The immediately preceding Core version used graph v3 and task settings 9
	// for the removed acceptance workflow. Preserve other old checkpoint paths.
	if err != nil || !found || state.TaskSettingsVersion != legacyAcceptanceTaskSettingsVersion {
		return err
	}
	if _, err := c.discardPaused(ctx, sessionID); err != nil {
		return fmt.Errorf("discard obsolete paused task: %w", err)
	}
	return nil
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

// Model returns the configured model for this session.
func (r *SessionRuntime) Model(ctx context.Context) string {
	if r == nil || r.core == nil {
		return ""
	}
	return r.core.ModelForSession(ctx, r.sessionID)
}

func (r *SessionRuntime) SubmitTurn(ctx context.Context, request TurnRequest, mode SubmissionMode) (TurnSubmission, error) {
	if r == nil || r.core == nil {
		return TurnSubmission{}, errors.New("session runtime is nil")
	}
	submission, _, err := r.core.submitTurn(ctx, r.sessionID, request, mode, false)
	return submission, err
}

// CancelPendingTurns interrupts the active turn and atomically cancels every
// turn still waiting in this session's queue. It waits for active cleanup
// before returning so callers can safely rewind and submit a replacement.
func (r *SessionRuntime) CancelPendingTurns(ctx context.Context) ([]string, error) {
	if r == nil || r.core == nil || ctx == nil {
		return nil, errors.New("session runtime, core, and context are required")
	}
	c := r.core
	c.queueMutationMu.Lock()
	locked := true
	defer func() {
		if locked {
			c.queueMutationMu.Unlock()
		}
	}()

	state, err := r.State(ctx)
	if err != nil {
		return nil, err
	}
	if state.Task.Status == TaskPaused {
		return nil, ErrTaskPaused
	}
	queued, err := c.sessions.QueuedTurns(ctx, r.sessionID)
	if err != nil {
		return nil, err
	}
	for _, item := range queued {
		if item.Status == session.QueuedTurnPaused {
			return nil, ErrTaskPaused
		}
	}
	activeExecutionID := ""
	if state.Task.Status == TaskRunning {
		activeExecutionID = state.Task.ExecutionID
	}
	for _, item := range queued {
		if item.Status == session.QueuedTurnActive {
			activeExecutionID = item.ExecutionID
			break
		}
	}
	if state.Task.Status == TaskRunning && activeExecutionID == "" {
		return nil, errors.New("running task has no execution id")
	}

	cancelled, err := c.sessions.CancelQueuedTurns(ctx, r.sessionID, string(TaskCancelled), func(item session.QueuedTurn) json.RawMessage {
		return mustMarshalEvent(Event{
			Kind: EventTaskFinished, SessionID: r.sessionID, QueueID: item.QueueID,
			ExecutionID: item.ExecutionID, Status: TaskCancelled, Err: "cancelled because the conversation was edited",
		})
	})
	if err != nil {
		return nil, err
	}
	if activeExecutionID != "" {
		c.interruptExecution(r.sessionID, activeExecutionID)
	}
	c.queueMutationMu.Unlock()
	locked = false

	affected := make([]string, 0, len(cancelled)+1)
	for _, item := range cancelled {
		affected = append(affected, item.ExecutionID)
		c.resolveTurnWaiter(item.ExecutionID, turnResult{err: context.Canceled})
	}
	if len(cancelled) > 0 {
		c.eventHub(r.sessionID).notifyDurable()
	}
	if activeExecutionID != "" {
		affected = append(affected, activeExecutionID)
		result, err := r.WaitTurn(ctx, activeExecutionID)
		if err != nil {
			return affected, fmt.Errorf("wait for interrupted turn cleanup: %w", err)
		}
		if result.Status == TaskPaused {
			discarded, err := r.DiscardPaused(ctx)
			if err != nil {
				return affected, fmt.Errorf("discard interrupted task checkpoint: %w", err)
			}
			if !discarded {
				return affected, errors.New("interrupted task checkpoint could not be discarded")
			}
		}
	}
	return affected, nil
}

// CancelQueuedTurn removes one turn that has not started without interrupting
// the active turn or changing the order of the remaining queue.
func (r *SessionRuntime) CancelQueuedTurn(ctx context.Context, executionID string) error {
	if r == nil || r.core == nil || ctx == nil || executionID == "" {
		return errors.New("session runtime, core, context, and execution id are required")
	}
	c := r.core
	c.queueMutationMu.Lock()
	locked := true
	defer func() {
		if locked {
			c.queueMutationMu.Unlock()
		}
	}()

	item, found, err := c.sessions.QueuedTurnByExecutionID(ctx, r.sessionID, executionID)
	if err != nil {
		return err
	}
	if !found {
		return ErrTurnNotFound
	}
	if item.Status != session.QueuedTurnQueued {
		return ErrQueuedTurnStarted
	}

	_, cancelled, err := c.sessions.CancelQueuedTurn(ctx, r.sessionID, executionID, string(TaskCancelled), func(item session.QueuedTurn) json.RawMessage {
		return mustMarshalEvent(Event{
			Kind: EventTaskFinished, SessionID: r.sessionID, QueueID: item.QueueID,
			ExecutionID: item.ExecutionID, Status: TaskCancelled, Err: "cancelled by user",
		})
	})
	if err != nil {
		return err
	}
	if !cancelled {
		return ErrQueuedTurnStarted
	}
	c.queueMutationMu.Unlock()
	locked = false
	c.resolveTurnWaiter(executionID, turnResult{err: context.Canceled})
	c.eventHub(r.sessionID).notifyDurable()
	c.scheduleQueueDrain(r.sessionID)
	return nil
}

// ReplaceQueuedTurnInput replaces a turn that has not started, keeping its
// queue position and cancelling later queued turns atomically. If the target
// has already been claimed, callers should use the active-turn edit path.
func (r *SessionRuntime) ReplaceQueuedTurnInput(ctx context.Context, executionID string, input TaskInput) (TurnSubmission, []string, error) {
	if r == nil || r.core == nil || ctx == nil || executionID == "" {
		return TurnSubmission{}, nil, errors.New("session runtime, core, context, and execution id are required")
	}
	responseInput, err := model.NewResponseInput(input.Raw)
	if err != nil {
		return TurnSubmission{}, nil, err
	}
	if err := model.ValidateInputAttachments(responseInput, r.core.attachmentLimits.Resolved()); err != nil {
		return TurnSubmission{}, nil, err
	}

	c := r.core
	c.queueMutationMu.Lock()
	state, err := r.State(ctx)
	if err != nil {
		c.queueMutationMu.Unlock()
		return TurnSubmission{}, nil, err
	}
	if state.Task.Status == TaskPaused {
		c.queueMutationMu.Unlock()
		return TurnSubmission{}, nil, ErrTaskPaused
	}
	queued, found, err := c.sessions.QueuedTurnByExecutionID(ctx, r.sessionID, executionID)
	if err != nil {
		c.queueMutationMu.Unlock()
		return TurnSubmission{}, nil, err
	}
	if !found {
		c.queueMutationMu.Unlock()
		return TurnSubmission{}, nil, ErrTurnNotFound
	}
	if queued.Status != session.QueuedTurnQueued {
		c.queueMutationMu.Unlock()
		return TurnSubmission{}, nil, ErrQueuedTurnStarted
	}
	var stored storedTurnRequest
	if err := json.Unmarshal(queued.Request, &stored); err != nil {
		c.queueMutationMu.Unlock()
		return TurnSubmission{}, nil, fmt.Errorf("decode queued turn request: %w", err)
	}
	stored.Input = append(json.RawMessage(nil), input.Raw...)
	encoded, err := json.Marshal(stored)
	if err != nil {
		c.queueMutationMu.Unlock()
		return TurnSubmission{}, nil, fmt.Errorf("encode replacement queued turn: %w", err)
	}
	updatedEvent := mustMarshalEvent(Event{
		Kind: EventTaskQueued, SessionID: r.sessionID, QueueID: queued.QueueID,
		ExecutionID: executionID, Status: TaskQueued,
	})
	updated, cancelled, replaced, err := c.sessions.ReplaceQueuedTurnAndCancelAfter(
		ctx, r.sessionID, queued.QueueID, executionID, encoded, updatedEvent, string(TaskCancelled),
		func(item session.QueuedTurn) json.RawMessage {
			return mustMarshalEvent(Event{
				Kind: EventTaskFinished, SessionID: r.sessionID, QueueID: item.QueueID,
				ExecutionID: item.ExecutionID, Status: TaskCancelled, Err: "cancelled because an earlier queued message was edited",
			})
		},
	)
	c.queueMutationMu.Unlock()
	if err != nil {
		return TurnSubmission{}, nil, err
	}
	if !replaced {
		if updated.Status == session.QueuedTurnActive {
			return TurnSubmission{}, nil, ErrQueuedTurnStarted
		}
		return TurnSubmission{}, nil, ErrTurnNotFound
	}
	for _, item := range cancelled {
		c.resolveTurnWaiter(item.ExecutionID, turnResult{err: context.Canceled})
	}
	c.eventHub(r.sessionID).notifyDurable()
	c.scheduleQueueDrain(r.sessionID)
	cancelledIDs := make([]string, 0, len(cancelled))
	for _, item := range cancelled {
		cancelledIDs = append(cancelledIDs, item.ExecutionID)
	}
	return TurnSubmission{Status: SubmissionQueued, QueueID: updated.QueueID, ExecutionID: updated.ExecutionID, Position: updated.Position}, cancelledIDs, nil
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
				Status:       TaskStatus(stored.Status),
				Response:     response,
				ProposedPlan: plan,
				BudgetReason: BudgetReason(stored.BudgetReason),
			}, nil
		}
		_, queued, err := r.core.sessions.QueuedTurnByExecutionID(ctx, r.sessionID, executionID)
		if err != nil {
			return TurnResult{}, err
		}
		if !queued {
			// Completion removes the queue row and creates the result in one
			// transaction, which may commit between the two reads above.
			if _, completed, err := r.core.sessions.TurnResult(ctx, r.sessionID, executionID); err != nil {
				return TurnResult{}, err
			} else if completed {
				continue
			}
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
