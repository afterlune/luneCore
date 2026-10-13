package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
	"github.com/afterlune/luneGraph/checkpoint"
)

var (
	ErrSessionBusy  = errors.New("session already has active or queued work")
	ErrTaskPaused   = errors.New("session task is paused; resolve its pending request, recover, or discard it")
	ErrTurnNotFound = errors.New("execution id does not exist in this session")
)

// TaskPausedError identifies a resumable task pause and its optional budget cause.
type TaskPausedError struct {
	BudgetReason BudgetReason
}

func (e *TaskPausedError) Error() string { return ErrTaskPaused.Error() }
func (e *TaskPausedError) Unwrap() error { return ErrTaskPaused }

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
	Input         TaskInput
	PlanMode      bool
	Model         string
	MaxToolRounds int
	Execution     ExecutionOptions
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

type TurnResult struct {
	ExecutionID  string
	Status       TaskStatus
	Response     json.RawMessage
	BudgetReason BudgetReason
}

type PendingRequestKind string

const (
	PendingApproval PendingRequestKind = "approval"
	// PendingUserQuestion is a lead-agent clarification waiting for the host.
	PendingUserQuestion    PendingRequestKind = "user_question"
	PendingUserInteraction PendingRequestKind = "user_interaction"
	PendingToolRecovery    PendingRequestKind = "tool_recovery"
)

type PendingRequest struct {
	Kind         PendingRequestKind
	Approval     *ApprovalRequest
	UserQuestion *UserQuestionRequest
	Interaction  *UserInteractionRequest
	ToolRecovery *ToolRecoveryRequest
}

// SessionRuntime is a handle to one session's turns, event log and pending
// requests. Accepted turns outlive the context used to submit them.
type SessionRuntime struct {
	core      *Core
	sessionID string
}

type storedTurnRequest struct {
	Input             json.RawMessage  `json:"input"`
	PlanMode          bool             `json:"plan_mode,omitempty"`
	Model             string           `json:"model,omitempty"`
	MaxToolRounds     int              `json:"max_tool_rounds,omitempty"`
	Execution         ExecutionOptions `json:"execution,omitempty"`
	ExecutionOverride bool             `json:"execution_override,omitempty"`
	Resume            bool             `json:"resume,omitempty"`
}

type turnResult struct {
	response json.RawMessage
	err      error
}

type sessionEventHub struct {
	mu      sync.Mutex
	changed chan struct{}
	live    map[uint64]eventSubscriber
	nextID  uint64
	closed  bool
}

type eventSubscriber struct {
	channel chan Event
}

func newSessionEventHub() *sessionEventHub {
	return &sessionEventHub{changed: make(chan struct{}), live: make(map[uint64]eventSubscriber)}
}

func (h *sessionEventHub) subscribeLive() (<-chan Event, func()) {
	h.mu.Lock()
	h.nextID++
	id := h.nextID
	channel := make(chan Event, 64)
	h.live[id] = eventSubscriber{channel: channel}
	h.mu.Unlock()
	return channel, func() { h.mu.Lock(); delete(h.live, id); h.mu.Unlock() }
}

func (h *sessionEventHub) changeChannel() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.changed
}

func (h *sessionEventHub) notifyDurable() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	close(h.changed)
	h.changed = make(chan struct{})
	h.mu.Unlock()
}

func (h *sessionEventHub) isClosed() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.closed }

func (h *sessionEventHub) close() {
	h.mu.Lock()
	if !h.closed {
		h.closed = true
		close(h.changed)
	}
	h.mu.Unlock()
}

func (h *sessionEventHub) publishLive(event Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	for _, subscriber := range h.live {
		select {
		case subscriber.channel <- event:
		default: // Model deltas are best effort and must not stall execution.
		}
	}
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
			return TurnResult{
				ExecutionID:  stored.ExecutionID,
				Status:       TaskStatus(stored.Status),
				Response:     append(json.RawMessage(nil), stored.Response...),
				BudgetReason: BudgetReason(stored.BudgetReason),
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
		requests = append(requests, PendingRequest{Kind: PendingUserQuestion, UserQuestion: &question})
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

func (c *Core) eventHub(sessionID string) *sessionEventHub {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.eventHubs == nil {
		c.eventHubs = make(map[string]*sessionEventHub)
	}
	hub := c.eventHubs[sessionID]
	if hub == nil {
		hub = newSessionEventHub()
		c.eventHubs[sessionID] = hub
	}
	return hub
}

func (c *Core) emit(event Event) {
	if event.SessionID == "" {
		return
	}
	if event.Kind == EventModelStream {
		c.eventHub(event.SessionID).publishLive(event)
		return
	}
	encoded, err := json.Marshal(safeEventProjection(event))
	if err != nil {
		slog.Warn("encode runtime event", "error", err)
		return
	}
	if _, err := c.sessions.AppendRuntimeEvent(context.Background(), event.SessionID, encoded); err != nil {
		slog.Warn("persist runtime event", "error", err, "kind", event.Kind)
		return
	}
	c.eventHub(event.SessionID).notifyDurable()
}

func safeEventProjection(event Event) Event {
	event.Cursor, event.OccurredAt, event.Err = 0, time.Time{}, ""
	event.Tool.Arguments = nil
	if event.Approval != nil {
		copy := cloneApproval(*event.Approval)
		copy.Arguments = nil
		event.Approval = &copy
	}
	if event.Interaction != nil {
		copy := cloneInteraction(*event.Interaction)
		copy.Request = nil
		event.Interaction = &copy
	}
	if event.UserQuestion != nil {
		copy := cloneUserQuestion(*event.UserQuestion)
		copy.Question = ""
		copy.Options = nil
		event.UserQuestion = &copy
	}
	if event.Compaction != nil {
		copy := *event.Compaction
		copy.Err = ""
		event.Compaction = &copy
	}
	if event.ToolRecovery != nil {
		copy := *event.ToolRecovery
		copy.Arguments = nil
		event.ToolRecovery = &copy
	}
	event.Stream = nil
	return event
}

func (c *Core) persistQueuedEvent(ctx context.Context, event Event, operation func(json.RawMessage) (int64, error)) error {
	encoded, err := json.Marshal(safeEventProjection(event))
	if err != nil {
		return err
	}
	if _, err := operation(encoded); err != nil {
		return err
	}
	c.eventHub(event.SessionID).notifyDurable()
	return nil
}

func mustMarshalEvent(event Event) json.RawMessage {
	encoded, _ := json.Marshal(safeEventProjection(event))
	return encoded
}

func cloneApproval(request ApprovalRequest) ApprovalRequest {
	request.Arguments = append(json.RawMessage(nil), request.Arguments...)
	return request
}
func cloneInteraction(request UserInteractionRequest) UserInteractionRequest {
	request.Request = append(json.RawMessage(nil), request.Request...)
	return request
}

func cloneUserQuestion(request UserQuestionRequest) UserQuestionRequest {
	request.Options = append([]string(nil), request.Options...)
	return request
}

func (c *Core) submitTurn(ctx context.Context, sessionID string, request TurnRequest, mode SubmissionMode, wait bool) (TurnSubmission, <-chan turnResult, error) {
	if c == nil || ctx == nil || sessionID == "" {
		return TurnSubmission{}, nil, errors.New("core, context, and session id are required")
	}
	if mode != StartIfIdle && mode != QueueInput {
		return TurnSubmission{}, nil, fmt.Errorf("unsupported submission mode %q", mode)
	}
	if len(request.Input.Raw) == 0 || !json.Valid(request.Input.Raw) {
		return TurnSubmission{}, nil, errors.New("task input must be valid JSON")
	}
	input, err := model.NewResponseInput(request.Input.Raw)
	if err != nil {
		return TurnSubmission{}, nil, err
	}
	if err := validateTaskInputAttachments(input, c.attachmentLimits); err != nil {
		return TurnSubmission{}, nil, err
	}
	if request.MaxToolRounds < 0 {
		return TurnSubmission{}, nil, errors.New("maximum tool rounds must not be negative")
	}
	if err := validateExecutionOptions(request.Execution); err != nil {
		return TurnSubmission{}, nil, err
	}
	if _, err := c.GetSession(ctx, sessionID); err != nil {
		return TurnSubmission{}, nil, err
	}
	executionID, err := newID()
	if err != nil {
		return TurnSubmission{}, nil, err
	}
	queueID, err := newID()
	if err != nil {
		return TurnSubmission{}, nil, err
	}
	storedRequest, err := json.Marshal(storedTurnRequest{
		Input: append(json.RawMessage(nil), request.Input.Raw...), PlanMode: request.PlanMode,
		Model: request.Model, MaxToolRounds: request.MaxToolRounds, Execution: request.Execution,
	})
	if err != nil {
		return TurnSubmission{}, nil, fmt.Errorf("encode queued turn: %w", err)
	}
	status := SubmissionQueued
	if mode == StartIfIdle {
		status = SubmissionAccepted
	}
	var waiter chan turnResult
	if wait {
		waiter = make(chan turnResult, 1)
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return TurnSubmission{}, nil, errors.New("core is closed")
	}
	if _, err := c.sessions.Get(ctx, sessionID, c.workspace); err != nil {
		c.mu.Unlock()
		return TurnSubmission{}, nil, err
	}
	if mode == StartIfIdle {
		if c.sessionIsRunningLocked(sessionID) {
			c.mu.Unlock()
			return TurnSubmission{}, nil, ErrSessionBusy
		}
		pending, pendingErr := c.runtime.HasPendingFor(sessionID, ctx)
		if pendingErr != nil {
			c.mu.Unlock()
			return TurnSubmission{}, nil, pendingErr
		}
		if pending {
			c.mu.Unlock()
			return TurnSubmission{}, nil, ErrTaskPaused
		}
		queued, queueErr := c.sessions.QueuedTurns(ctx, sessionID)
		if queueErr != nil {
			c.mu.Unlock()
			return TurnSubmission{}, nil, queueErr
		}
		if len(queued) > 0 {
			c.mu.Unlock()
			for _, item := range queued {
				if item.Status == session.QueuedTurnPaused {
					return TurnSubmission{}, nil, ErrTaskPaused
				}
			}
			return TurnSubmission{}, nil, ErrSessionBusy
		}
	}
	queued, _, err := c.sessions.EnqueueTurn(ctx, session.QueuedTurn{
		SessionID: sessionID, QueueID: queueID, ExecutionID: executionID, Request: storedRequest, Status: session.QueuedTurnQueued,
	}, mustMarshalEvent(Event{Kind: EventTaskQueued, SessionID: sessionID, ExecutionID: executionID, QueueID: queueID, Status: TaskQueued}))
	if err != nil {
		c.mu.Unlock()
		return TurnSubmission{}, nil, err
	}
	if waiter != nil {
		c.turnWaiters[executionID] = waiter
	}
	c.mu.Unlock()
	c.eventHub(sessionID).notifyDurable()
	c.scheduleQueueDrain(sessionID)
	return TurnSubmission{Status: status, QueueID: queued.QueueID, ExecutionID: executionID, Position: queued.Position}, waiter, nil
}

func (c *Core) recoverSession(ctx context.Context, sessionID string, wait bool, executionOverride ...ExecutionOptions) (TurnSubmission, <-chan turnResult, error) {
	if c == nil || ctx == nil || sessionID == "" {
		return TurnSubmission{}, nil, errors.New("core, context, and session id are required")
	}
	if len(executionOverride) > 1 {
		return TurnSubmission{}, nil, errors.New("at most one execution options override may be supplied")
	}
	if len(executionOverride) == 1 {
		if err := validateExecutionOptions(executionOverride[0]); err != nil {
			return TurnSubmission{}, nil, err
		}
	}
	if _, err := c.GetSession(ctx, sessionID); err != nil {
		return TurnSubmission{}, nil, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return TurnSubmission{}, nil, errors.New("core is closed")
	}
	if _, err := c.sessions.Get(ctx, sessionID, c.workspace); err != nil {
		c.mu.Unlock()
		return TurnSubmission{}, nil, err
	}
	if c.sessionIsRunningLocked(sessionID) {
		c.mu.Unlock()
		return TurnSubmission{}, nil, ErrSessionBusy
	}
	pending, found, err := c.pendingState(ctx, sessionID)
	if err != nil {
		c.mu.Unlock()
		return TurnSubmission{}, nil, err
	}
	if !found {
		c.mu.Unlock()
		return TurnSubmission{}, nil, errors.New("no paused task exists in this session")
	}
	if pending.ExecutionID == "" {
		if pending.TaskSettingsVersion != agentgraph.CurrentTaskSettingsVersion {
			c.mu.Unlock()
			return TurnSubmission{}, nil, checkpointSettingsVersionError(pending.TaskSettingsVersion)
		}
		c.mu.Unlock()
		return TurnSubmission{}, nil, errors.New("paused task checkpoint is missing its execution identity; discard it and start a new task")
	}
	if pending.TaskSettingsVersion != agentgraph.CurrentTaskSettingsVersion {
		c.mu.Unlock()
		return TurnSubmission{}, nil, checkpointSettingsVersionError(pending.TaskSettingsVersion)
	}
	queued, err := c.sessions.QueuedTurns(ctx, sessionID)
	if err != nil {
		c.mu.Unlock()
		return TurnSubmission{}, nil, err
	}
	var item *session.QueuedTurn
	for i := range queued {
		if queued[i].ExecutionID == pending.ExecutionID {
			item = &queued[i]
			break
		}
	}
	if item == nil {
		queueID, idErr := newID()
		if idErr != nil {
			c.mu.Unlock()
			return TurnSubmission{}, nil, idErr
		}
		resumeRequest := storedTurnRequest{Input: json.RawMessage("null"), Resume: true}
		if len(executionOverride) == 1 {
			resumeRequest.Execution, resumeRequest.ExecutionOverride = executionOverride[0], true
		}
		request, _ := json.Marshal(resumeRequest)
		created, _, enqueueErr := c.sessions.EnqueueTurn(ctx, session.QueuedTurn{SessionID: sessionID, QueueID: queueID, Position: -1, ExecutionID: pending.ExecutionID, Request: request, Status: session.QueuedTurnQueued},
			mustMarshalEvent(Event{Kind: EventTaskQueued, SessionID: sessionID, ExecutionID: pending.ExecutionID, QueueID: queueID, Status: TaskQueued}))
		if enqueueErr != nil {
			c.mu.Unlock()
			return TurnSubmission{}, nil, enqueueErr
		}
		item = &created
		if hub := c.eventHubs[sessionID]; hub != nil {
			hub.notifyDurable()
		}
	} else {
		var request storedTurnRequest
		if err := json.Unmarshal(item.Request, &request); err != nil {
			c.mu.Unlock()
			return TurnSubmission{}, nil, fmt.Errorf("decode paused turn request: %w", err)
		}
		request.Resume = true
		if len(executionOverride) == 1 {
			request.Execution = executionOverride[0]
			request.ExecutionOverride = true
		}
		encoded, err := json.Marshal(request)
		if err != nil {
			c.mu.Unlock()
			return TurnSubmission{}, nil, err
		}
		event := Event{Kind: EventTaskQueued, SessionID: sessionID, QueueID: item.QueueID, ExecutionID: item.ExecutionID, Status: TaskQueued}
		if _, err := c.sessions.SetQueuedTurnWithEvent(ctx, sessionID, item.QueueID, session.QueuedTurnQueued, encoded, mustMarshalEvent(event)); err != nil {
			c.mu.Unlock()
			return TurnSubmission{}, nil, err
		}
		if hub := c.eventHubs[sessionID]; hub != nil {
			hub.notifyDurable()
		}
		item.Status, item.Request = session.QueuedTurnQueued, encoded
	}
	var waiter chan turnResult
	if wait {
		waiter = make(chan turnResult, 1)
		c.turnWaiters[pending.ExecutionID] = waiter
	}
	c.mu.Unlock()
	c.scheduleQueueDrain(sessionID)
	return TurnSubmission{Status: SubmissionAccepted, QueueID: item.QueueID, ExecutionID: item.ExecutionID, Position: item.Position}, waiter, nil
}

func (c *Core) reconcileQueuedTurns(ctx context.Context, sessionID string) error {
	queued, err := c.sessions.QueuedTurns(ctx, sessionID)
	if err != nil {
		return err
	}
	pending, found, err := c.pendingState(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, item := range queued {
		if item.Status != session.QueuedTurnActive {
			continue
		}
		completed, _, finalizeErr := c.completeQueuedTurnFromCheckpoint(ctx, item)
		if finalizeErr != nil {
			return finalizeErr
		}
		if completed {
			continue
		}
		status := session.QueuedTurnQueued
		event := Event{Kind: EventTaskQueued, SessionID: sessionID, QueueID: item.QueueID, ExecutionID: item.ExecutionID, Status: TaskQueued}
		if found && pending.ExecutionID == item.ExecutionID {
			status = session.QueuedTurnPaused
			event.Kind, event.Status = EventTaskFinished, TaskPaused
		}
		var updateErr error
		if status == session.QueuedTurnPaused {
			_, updateErr = c.sessions.PauseQueuedTurn(ctx, sessionID, item.QueueID, item.ExecutionID, string(TaskPaused), mustMarshalEvent(event))
		} else {
			_, updateErr = c.sessions.SetQueuedTurnStatusWithEvent(ctx, sessionID, item.QueueID, status, mustMarshalEvent(event))
		}
		if updateErr != nil {
			return updateErr
		}
		if hub := c.eventHubs[sessionID]; hub != nil {
			hub.notifyDurable()
		}
	}
	// A committed result may survive a crash that happened before checkpoint
	// deletion. It cannot be resumed and should not block the next turn.
	if completedCheckpoint, err := c.runtime.CheckpointForSession(ctx, sessionID); err == nil && completedCheckpoint.Completed {
		if err := c.runtime.DeleteCompletedRunForSession(sessionID); err != nil {
			return err
		}
	} else if err != nil && !errors.Is(err, checkpoint.ErrNotFound) {
		return err
	}
	return nil
}

func (c *Core) completeQueuedTurnFromCheckpoint(ctx context.Context, queued session.QueuedTurn) (bool, json.RawMessage, error) {
	cp, err := c.runtime.CheckpointForSession(ctx, queued.SessionID)
	if errors.Is(err, checkpoint.ErrNotFound) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	if !cp.Completed {
		return false, nil, nil
	}
	state := cp.Final
	if state != nil && state.ExecutionID != "" && state.ExecutionID != queued.ExecutionID {
		return false, nil, nil
	}
	status := TaskFailed
	var response, history, modelContext json.RawMessage
	contextKey := ""
	if state != nil {
		transcript := state.Transcript
		if transcript.Items == nil && transcript.Text == nil {
			transcript = state.History
		}
		history, err = json.Marshal(&transcript)
		if err != nil {
			return true, nil, fmt.Errorf("encode completed transcript: %w", err)
		}
		modelContext, err = json.Marshal(&state.History)
		if err != nil {
			return true, nil, fmt.Errorf("encode completed model context: %w", err)
		}
		if state.FinalResponse != nil && state.TaskError == "" && cp.Failure == nil {
			response, err = json.Marshal(state.FinalResponse)
			if err != nil {
				return true, nil, fmt.Errorf("encode completed turn response: %w", err)
			}
			status = TaskCompleted
		}
	}
	saved, err := c.GetSession(ctx, queued.SessionID)
	if err != nil {
		return true, nil, err
	}
	contextKey = saved.ModelContextKey
	if state != nil && state.Provider != "" {
		if providerConfig, ok := c.models.ProviderConfig(state.Provider); ok {
			effectiveModel := state.Model
			if state.ModelOverride != "" {
				effectiveModel = state.ModelOverride
			}
			contextKey = modelContextKey(state.Provider, effectiveModel, providerConfig)
		}
	}
	if state != nil && state.TaskError != "" {
		response = nil
	}
	if status != TaskCompleted {
		response = nil
	}
	if err := c.commitQueuedTurn(ctx, queued, status, response, history, modelContext, contextKey); err != nil {
		return true, nil, err
	}
	return true, response, nil
}

func (c *Core) completeQueuedTurnFromRuntime(ctx context.Context, queued session.QueuedTurn, status TaskStatus, response json.RawMessage, runtime *graphResponder) error {
	var history, modelContext json.RawMessage
	contextKey := ""
	var err error
	if runtime != nil {
		transcript := runtime.SessionHistory()
		if transcript == nil {
			transcript = model.NewResponseInputItems()
		}
		history, err = json.Marshal(transcript)
		if err != nil {
			return fmt.Errorf("encode completed transcript: %w", err)
		}
		context := runtime.ModelContextHistory()
		if context != nil {
			modelContext, err = json.Marshal(context)
			if err != nil {
				return fmt.Errorf("encode completed model context: %w", err)
			}
		}
		contextKey = runtime.ContextKey()
	}
	if status != TaskCompleted {
		response = nil
	}
	return c.commitQueuedTurn(ctx, queued, status, response, history, modelContext, contextKey)
}

func (c *Core) commitQueuedTurn(ctx context.Context, queued session.QueuedTurn, status TaskStatus, response, history, modelContext json.RawMessage, contextKey string) error {
	taskState, ok := c.StateForSession(queued.SessionID)
	var taskID string
	if ok && taskState.ExecutionID == queued.ExecutionID {
		taskID = taskState.TaskID
	}
	event := Event{Kind: EventTaskFinished, SessionID: queued.SessionID, TaskID: taskID, QueueID: queued.QueueID, ExecutionID: queued.ExecutionID, Status: status}
	if _, err := c.sessions.CompleteQueuedTurn(ctx, queued.SessionID, queued.QueueID, queued.ExecutionID, string(status), response, history, modelContext, contextKey, mustMarshalEvent(event)); err != nil {
		return err
	}
	c.rememberQueuedTaskStatus(queued.SessionID, queued.ExecutionID, queued.QueueID, status)
	if hub := c.eventHubs[queued.SessionID]; hub != nil {
		hub.notifyDurable()
	}
	if err := c.runtime.DeleteCompletedRunForSession(queued.SessionID); err != nil {
		slog.Warn("delete completed graph checkpoint", "error", err)
	}
	if err := c.sessions.DeleteToolInvocationsForSession(context.Background(), queued.SessionID); err != nil {
		slog.Warn("delete completed tool invocation records", "error", err)
	}
	return nil
}

func (c *Core) rememberQueuedTaskStatus(sessionID, executionID, queueID string, status TaskStatus) {
	c.mu.Lock()
	state := c.activeTasks[sessionID]
	if state.ExecutionID != executionID {
		state = TaskState{SessionID: sessionID, ExecutionID: executionID, QueueID: queueID}
	}
	state.Status = status
	c.activeTasks[sessionID] = state
	c.active = state
	c.mu.Unlock()
}

func (c *Core) scheduleQueueDrain(sessionID string) {
	c.mu.Lock()
	if c.closed || c.queueDraining[sessionID] {
		c.mu.Unlock()
		return
	}
	c.queueDraining[sessionID] = true
	c.queueDrainWG.Add(1)
	c.mu.Unlock()
	go c.drainQueue(sessionID)
}

func (c *Core) drainQueue(sessionID string) {
	defer c.queueDrainWG.Done()
	defer func() {
		c.mu.Lock()
		delete(c.queueDraining, sessionID)
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return
		}
		queued, err := c.sessions.QueuedTurns(context.Background(), sessionID)
		if err != nil {
			return
		}
		for _, item := range queued {
			if item.Status == session.QueuedTurnPaused || item.Status == session.QueuedTurnActive {
				return
			}
			if item.Status == session.QueuedTurnQueued {
				c.scheduleQueueDrain(sessionID)
				return
			}
		}
	}()
	ctx := context.Background()
	if err := c.reconcileQueuedTurns(ctx, sessionID); err != nil {
		slog.Warn("reconcile queued turns before draining", "error", err)
		return
	}
	for {
		c.mu.RLock()
		closed := c.closed
		c.mu.RUnlock()
		if closed {
			return
		}
		if state, ok := c.StateForSession(sessionID); ok && state.Status == TaskRunning {
			return
		}
		pending, found, err := c.pendingState(ctx, sessionID)
		if err != nil {
			slog.Warn("inspect checkpoint before queued turn", "error", err)
			return
		}
		if found {
			queued, listErr := c.sessions.QueuedTurns(ctx, sessionID)
			if listErr != nil || len(queued) == 0 || queued[0].ExecutionID != pending.ExecutionID {
				return
			}
			var request storedTurnRequest
			if json.Unmarshal(queued[0].Request, &request) != nil || !request.Resume {
				return
			}
		}
		queued, claimed, err := c.sessions.ClaimNextQueuedTurn(ctx, sessionID)
		if err != nil {
			slog.Warn("claim queued turn", "error", err)
			return
		}
		if !claimed {
			return
		}
		c.mu.RLock()
		closing := c.closed
		c.mu.RUnlock()
		if closing {
			if err := c.sessions.SetQueuedTurnStatus(ctx, sessionID, queued.QueueID, session.QueuedTurnQueued); err != nil {
				slog.Warn("return claimed turn to queue during close", "error", err)
			}
			c.resolveTurnWaiter(queued.ExecutionID, turnResult{err: errors.New("core is closing")})
			return
		}
		var request storedTurnRequest
		if err := json.Unmarshal(queued.Request, &request); err != nil {
			c.failQueuedTurn(ctx, queued, err)
			continue
		}
		input, err := model.NewResponseInput(request.Input)
		if err != nil {
			c.failQueuedTurn(ctx, queued, err)
			continue
		}
		runDone := make(chan struct{})
		c.mu.Lock()
		c.executionRuns[queued.ExecutionID] = runDone
		c.mu.Unlock()
		response, runErr := c.run(ctx, sessionID, request.PlanMode, request.MaxToolRounds, request.Model, request.Execution, request.ExecutionOverride, request.Resume, input, queued.QueueID, queued.ExecutionID)
		c.mu.Lock()
		if c.executionRuns[queued.ExecutionID] == runDone {
			delete(c.executionRuns, queued.ExecutionID)
		}
		close(runDone)
		c.mu.Unlock()
		c.eventHub(sessionID).notifyDurable()
		c.mu.RLock()
		closing = c.closed
		c.mu.RUnlock()
		if closing {
			if _, exists, lookupErr := c.sessions.QueuedTurnByExecutionID(ctx, sessionID, queued.ExecutionID); lookupErr == nil && exists {
				if err := c.sessions.SetQueuedTurnStatus(ctx, sessionID, queued.QueueID, session.QueuedTurnQueued); err != nil {
					slog.Warn("preserve queued turn during close", "error", err)
				}
			}
			c.resolveTurnWaiter(queued.ExecutionID, turnResult{err: errors.New("core is closing")})
			return
		}
		resumeQueued := false
		if current, exists, lookupErr := c.sessions.QueuedTurnByExecutionID(ctx, sessionID, queued.ExecutionID); lookupErr == nil && exists {
			state, ok := c.StateForSession(sessionID)
			if ok && state.Status == TaskPaused {
				if current.Status != session.QueuedTurnPaused && current.Status != session.QueuedTurnQueued {
					event := Event{Kind: EventTaskFinished, SessionID: sessionID, TaskID: state.TaskID, QueueID: queued.QueueID, ExecutionID: queued.ExecutionID, Status: TaskPaused, BudgetReason: budgetReasonFromError(runErr), Err: errorString(runErr)}
					if persistErr := c.persistQueuedEvent(ctx, event, func(payload json.RawMessage) (int64, error) {
						return c.sessions.PauseQueuedTurnWithReason(ctx, sessionID, queued.QueueID, queued.ExecutionID, string(TaskPaused), string(event.BudgetReason), payload)
					}); persistErr != nil {
						slog.Warn("restore paused queued turn", "error", persistErr)
					}
				}
				if current.Status == session.QueuedTurnQueued {
					var request storedTurnRequest
					resumeQueued = json.Unmarshal(current.Request, &request) == nil && request.Resume
				} else {
					// The host may answer as soon as the wait event is emitted, while
					// this drain still owns the run. Queue that answer once the wait
					// checkpoint has committed, then continue this drain in place.
					inputs, inputErr := c.graphResumeInputs(ctx, sessionID, c.runtime.checkpointID(sessionID))
					if inputErr != nil {
						slog.Warn("inspect answered graph wait", "error", inputErr)
					} else if len(inputs) > 0 {
						if _, _, recoverErr := c.recoverSession(ctx, sessionID, false); recoverErr == nil {
							resumeQueued = true
						} else if !errors.Is(recoverErr, ErrSessionBusy) {
							slog.Warn("queue answer received while graph was pausing", "error", recoverErr)
						}
					}
				}
			} else {
				completed, finalResponse, finalizeErr := c.completeQueuedTurnFromCheckpoint(ctx, current)
				if finalizeErr != nil {
					slog.Warn("finalize completed queued turn", "error", finalizeErr)
					c.resolveTurnWaiter(queued.ExecutionID, turnResult{response: response, err: finalizeErr})
					return
				}
				if completed {
					if finalResponse != nil {
						response = finalResponse
					}
				} else {
					c.failQueuedTurn(ctx, queued, runErr)
				}
			}
		}
		c.resolveTurnWaiter(queued.ExecutionID, turnResult{response: response, err: runErr})
		if state, ok := c.StateForSession(sessionID); ok && state.Status == TaskPaused {
			if resumeQueued {
				continue
			}
			return
		}
	}
}

func (c *Core) failQueuedTurn(ctx context.Context, queued session.QueuedTurn, err error) {
	event := Event{Kind: EventTaskFinished, SessionID: queued.SessionID, QueueID: queued.QueueID, ExecutionID: queued.ExecutionID, Status: TaskFailed, Err: errorString(err)}
	if _, persistErr := c.sessions.CompleteQueuedTurn(ctx, queued.SessionID, queued.QueueID, queued.ExecutionID, string(TaskFailed), nil, nil, nil, "", mustMarshalEvent(event)); persistErr != nil {
		slog.Warn("finish rejected queued turn", "error", persistErr)
	} else if hub := c.eventHubs[queued.SessionID]; hub != nil {
		hub.notifyDurable()
	}
	c.resolveTurnWaiter(queued.ExecutionID, turnResult{err: err})
}

func (c *Core) resolveTurnWaiter(executionID string, result turnResult) {
	c.mu.Lock()
	waiter := c.turnWaiters[executionID]
	delete(c.turnWaiters, executionID)
	c.mu.Unlock()
	if waiter != nil {
		waiter <- result
	}
}

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
	if c.closed {
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
	parentID := c.runtime.checkpointID(sessionID)
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
			if hub := c.eventHubs[sessionID]; hub != nil {
				hub.notifyDurable()
			}
		}
	} else if deletedCheckpoint {
		event := Event{Kind: EventTaskDiscarded, SessionID: sessionID, Status: TaskIdle}
		if _, err := c.sessions.AppendRuntimeEvent(ctx, sessionID, mustMarshalEvent(event)); err != nil {
			return false, err
		}
		if hub := c.eventHubs[sessionID]; hub != nil {
			hub.notifyDurable()
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

func (c *Core) waitForTurn(ctx context.Context, runtime *SessionRuntime, executionID string, waiter <-chan turnResult) (json.RawMessage, error) {
	if waiter == nil {
		return nil, errors.New("turn completion channel is unavailable")
	}
	select {
	case result := <-waiter:
		return result.response, result.err
	case <-ctx.Done():
		runtime.Interrupt(executionID)
		result := <-waiter
		if result.err != nil {
			return result.response, result.err
		}
		return result.response, ctx.Err()
	}
}

func (c *Core) runSynchronous(ctx context.Context, request TaskRequest) (json.RawMessage, error) {
	if len(request.Input.Raw) == 0 || !json.Valid(request.Input.Raw) {
		return nil, errors.New("task input must be valid JSON")
	}
	input, err := model.NewResponseInput(request.Input.Raw)
	if err != nil {
		return nil, err
	}
	if err := validateTaskInputAttachments(input, c.attachmentLimits); err != nil {
		return nil, err
	}
	runtime, err := c.OpenSessionRuntime(ctx, request.SessionID)
	if err != nil {
		return nil, err
	}
	cursor, err := c.sessions.LatestRuntimeEventCursor(ctx, request.SessionID)
	if err != nil {
		return nil, err
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	events, err := runtime.subscribeEvents(streamCtx, cursor, false)
	if err != nil {
		return nil, err
	}
	go func() {
		for range events {
		}
	}()
	submission, waiter, err := c.submitTurn(ctx, request.SessionID, TurnRequest{
		Input: request.Input, PlanMode: request.PlanMode, Model: request.Model, MaxToolRounds: request.MaxToolRounds,
		Execution: request.Execution,
	}, StartIfIdle, true)
	if err != nil {
		return nil, err
	}
	c.scheduleQueueDrain(request.SessionID)
	return c.waitForTurn(ctx, runtime, submission.ExecutionID, waiter)
}
