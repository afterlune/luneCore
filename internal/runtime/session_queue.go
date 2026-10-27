package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
)

func (c *Core) submitTurn(ctx context.Context, sessionID string, request TurnRequest, mode SubmissionMode, wait bool) (TurnSubmission, <-chan turnResult, error) {

	if c == nil || ctx == nil || sessionID == "" {
		return TurnSubmission{}, nil, errors.New("core, context, and session id are required")
	}
	if mode != StartIfIdle && mode != QueueInput && mode != SteerInput {
		return TurnSubmission{}, nil, fmt.Errorf("unsupported submission mode %q", mode)
	}
	if len(request.Input.Raw) == 0 || !json.Valid(request.Input.Raw) {
		return TurnSubmission{}, nil, errors.New("task input must be valid JSON")
	}
	input, err := model.NewResponseInput(request.Input.Raw)
	if err != nil {
		return TurnSubmission{}, nil, err
	}
	if err := model.ValidateInputAttachments(input, c.attachmentLimits.Resolved()); err != nil {
		return TurnSubmission{}, nil, err
	}
	if request.MaxToolRounds < 0 {
		return TurnSubmission{}, nil, errors.New("maximum tool rounds must not be negative")
	}
	if err := validateExecutionOptions(request.Execution); err != nil {
		return TurnSubmission{}, nil, err
	}
	permissionOverride := string(request.PermissionMode)
	if err := validatePermissionOverride(permissionOverride); err != nil {
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
	goal, err := c.sessions.Goal(ctx, sessionID)
	if err != nil {
		return TurnSubmission{}, nil, err
	}
	goalID := ""
	var goalRevision int64
	if goal != nil && goal.Status == GoalActive && !request.PlanMode {
		goalID, goalRevision = goal.ID, goal.Revision
	}
	storedRequest, err := json.Marshal(storedTurnRequest{
		GoalID: goalID, GoalRevision: goalRevision, AutomaticGoal: request.automaticGoal,
		Input: append(json.RawMessage(nil), request.Input.Raw...),
		Model: request.Model, MaxToolRounds: request.MaxToolRounds, Execution: request.Execution,
		PermissionOverride: permissionOverride, PlanMode: request.PlanMode, PlanAuthorization: request.PlanAuthorization,
		NoInteractiveDecisions: request.NoInteractiveDecisions, SteerInput: mode == SteerInput,
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
	steeringLocked := mode == SteerInput
	if steeringLocked {
		c.queueMutationMu.Lock()
	}
	c.mu.Lock()
	unlock := func() {
		c.mu.Unlock()
		if steeringLocked {
			c.queueMutationMu.Unlock()
		}
	}
	if c.closed || c.closing {
		unlock()
		return TurnSubmission{}, nil, errors.New("core is closed")
	}
	if _, err := c.sessions.Get(ctx, sessionID, c.workspace); err != nil {
		unlock()
		return TurnSubmission{}, nil, err
	}
	steeringParentID := ""
	if mode == SteerInput {
		steeringParentID = c.steeringExecutions[sessionID]
		if steeringParentID != "" {
			var marshalErr error
			storedRequest, marshalErr = json.Marshal(storedTurnRequest{
				GoalID: goalID, GoalRevision: goalRevision, AutomaticGoal: request.automaticGoal,
				Input: append(json.RawMessage(nil), request.Input.Raw...),
				Model: request.Model, MaxToolRounds: request.MaxToolRounds, Execution: request.Execution,
				PermissionOverride: permissionOverride, PlanMode: request.PlanMode, PlanAuthorization: request.PlanAuthorization,
				NoInteractiveDecisions: request.NoInteractiveDecisions, SteerInput: true,
				SteerParentExecutionID: steeringParentID,
			})
			if marshalErr != nil {
				unlock()
				return TurnSubmission{}, nil, fmt.Errorf("encode steered turn: %w", marshalErr)
			}
		}
	}
	if mode == StartIfIdle {
		if c.sessionIsRunningLocked(sessionID) {
			unlock()
			return TurnSubmission{}, nil, ErrSessionBusy
		}
		pending, pendingErr := c.runtime.HasPendingFor(sessionID, ctx)
		if pendingErr != nil {
			unlock()
			return TurnSubmission{}, nil, pendingErr
		}
		if pending {
			unlock()
			return TurnSubmission{}, nil, ErrTaskPaused
		}
		queued, queueErr := c.sessions.QueuedTurns(ctx, sessionID)
		if queueErr != nil {
			unlock()
			return TurnSubmission{}, nil, queueErr
		}
		if len(queued) > 0 {
			unlock()
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
	}, mustMarshalEvent(Event{Kind: EventTaskQueued, SessionID: sessionID, ExecutionID: executionID, QueueID: queueID, ParentExecutionID: steeringParentID, Status: TaskQueued, GoalID: goalID, GoalRevision: goalRevision, AutomaticGoal: request.automaticGoal}))
	if err != nil {
		unlock()
		return TurnSubmission{}, nil, err
	}
	if waiter != nil {
		c.turnWaiters[executionID] = waiter
	}
	unlock()
	c.eventHub(sessionID).notifyDurable()
	c.scheduleQueueDrain(sessionID)
	return TurnSubmission{Status: status, QueueID: queued.QueueID, ExecutionID: executionID, ParentExecutionID: steeringParentID, Position: queued.Position}, waiter, nil
}
