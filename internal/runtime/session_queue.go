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
	return c.submitTurnWithID(ctx, sessionID, request, mode, wait, "", nil)
}

func (c *Core) submitTurnAdmitted(ctx context.Context, sessionID string, request TurnRequest, mode SubmissionMode, wait bool) (TurnSubmission, <-chan turnResult, error) {
	return c.submitTurnAdmittedWithID(ctx, sessionID, request, mode, wait, "", nil)
}

func (c *Core) submitTurnWithID(ctx context.Context, sessionID string, request TurnRequest, mode SubmissionMode, wait bool, submissionID string, digest []byte) (TurnSubmission, <-chan turnResult, error) {
	return c.submitTurnWithIDAndAnalysis(ctx, sessionID, request, mode, wait, submissionID, digest, nil)
}

func (c *Core) submitTurnWithIDAndAnalysis(ctx context.Context, sessionID string, request TurnRequest, mode SubmissionMode, wait bool, submissionID string, digest []byte, analysis *AnalysisMetadata) (TurnSubmission, <-chan turnResult, error) {
	if c == nil || ctx == nil {
		return TurnSubmission{}, nil, errors.New("core and context are required")
	}
	if err := lockContext(ctx, &c.queueMutationMu); err != nil {
		return TurnSubmission{}, nil, err
	}
	defer c.queueMutationMu.Unlock()
	return c.submitTurnAdmittedWithIDAndAnalysis(ctx, sessionID, request, mode, wait, submissionID, digest, analysis)
}

func (c *Core) submitTurnAdmittedWithID(ctx context.Context, sessionID string, request TurnRequest, mode SubmissionMode, wait bool, submissionID string, digest []byte) (TurnSubmission, <-chan turnResult, error) {
	return c.submitTurnAdmittedWithIDAndAnalysis(ctx, sessionID, request, mode, wait, submissionID, digest, nil)
}

func (c *Core) submitTurnAdmittedWithIDAndAnalysis(ctx context.Context, sessionID string, request TurnRequest, mode SubmissionMode, wait bool, submissionID string, digest []byte, analysis *AnalysisMetadata) (TurnSubmission, <-chan turnResult, error) {
	return c.submitTurnAdmittedWithIDAndHistoryAndAnalysis(ctx, sessionID, request, mode, wait, submissionID, digest, nil, "", 0, analysis)
}

func (c *Core) submitTurnAdmittedWithIDAndHistory(ctx context.Context, sessionID string, request TurnRequest, mode SubmissionMode, wait bool, submissionID string, digest []byte, replacementHistory json.RawMessage, expectedHistoryDigest string, replacedUserTurns int) (TurnSubmission, <-chan turnResult, error) {
	return c.submitTurnAdmittedWithIDAndHistoryAndAnalysis(ctx, sessionID, request, mode, wait, submissionID, digest, replacementHistory, expectedHistoryDigest, replacedUserTurns, nil)
}

func (c *Core) submitTurnAdmittedWithIDAndHistoryAndAnalysis(ctx context.Context, sessionID string, request TurnRequest, mode SubmissionMode, wait bool, submissionID string, digest []byte, replacementHistory json.RawMessage, expectedHistoryDigest string, replacedUserTurns int, analysis *AnalysisMetadata) (TurnSubmission, <-chan turnResult, error) {

	if c == nil || ctx == nil || sessionID == "" {
		return TurnSubmission{}, nil, errors.New("core, context, and session id are required")
	}
	if submissionID != "" {
		saved, found, err := c.sessions.TurnSubmission(ctx, sessionID, submissionID, digest)
		if err != nil {
			return TurnSubmission{}, nil, err
		}
		if found {
			var submission TurnSubmission
			if err := json.Unmarshal(saved, &submission); err != nil {
				return TurnSubmission{}, nil, fmt.Errorf("decode saved turn submission: %w", err)
			}
			return submission, nil, nil
		}
	}
	if mode != StartIfIdle && mode != QueueInput && mode != SteerInput {
		return TurnSubmission{}, nil, fmt.Errorf("unsupported submission mode %q", mode)
	}
	if len(replacementHistory) > 0 && (mode != StartIfIdle || expectedHistoryDigest == "" || replacedUserTurns <= 0) {
		return TurnSubmission{}, nil, errors.New("history replacement requires an idle start and a current history preview")
	}
	if len(request.Input.Raw) == 0 || !json.Valid(request.Input.Raw) {
		return TurnSubmission{}, nil, errors.New("task input must be valid JSON")
	}
	input, err := model.NewResponseInput(request.Input.Raw)
	if err != nil {
		return TurnSubmission{}, nil, err
	}
	snap := c.currentConfig()
	if snap == nil || snap.models == nil {
		return TurnSubmission{}, nil, errors.New("core configuration snapshot is unavailable")
	}
	if err := model.ValidateInputAttachments(input, snap.attachmentLimits.Resolved()); err != nil {
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
	saved, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return TurnSubmission{}, nil, err
	}
	if request.RequireImageProfile && len(model.TaskInputImages(input)) != 0 {
		profile, ok := snap.models.Profile(saved.Settings.ModelProfile)
		if !ok || !profile.SupportsInputImages || (request.Model != "" && request.Model != profile.Model) {
			return TurnSubmission{}, nil, fmt.Errorf("model profile %q is not configured for this image input", saved.Settings.ModelProfile)
		}
	}
	executionID, err := newID()
	if err != nil {
		return TurnSubmission{}, nil, err
	}
	var analysisJSON json.RawMessage
	if analysis != nil {
		if (analysis.Kind != "review" && analysis.Kind != "diagnosis") || analysis.SourceSessionID == "" || len(analysis.EvidenceDigest) != 64 {
			return TurnSubmission{}, nil, errors.New("analysis kind, source session, and evidence digest are required")
		}
		persistedAnalysis := *analysis
		persistedAnalysis.ExecutionID = executionID
		analysisJSON, err = json.Marshal(persistedAnalysis)
		if err != nil {
			return TurnSubmission{}, nil, fmt.Errorf("encode analysis metadata: %w", err)
		}
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
	c.mu.Lock()
	unlock := c.mu.Unlock
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
	queuedTurn := session.QueuedTurn{
		SessionID: sessionID, QueueID: queueID, ExecutionID: executionID, Request: storedRequest, Status: session.QueuedTurnQueued,
	}
	event := mustMarshalEvent(Event{Kind: EventTaskQueued, SessionID: sessionID, ExecutionID: executionID, QueueID: queueID, ParentExecutionID: steeringParentID, Status: TaskQueued, GoalID: goalID, GoalRevision: goalRevision, AutomaticGoal: request.automaticGoal})
	var queued session.QueuedTurn
	var duplicateResult []byte
	var duplicate bool
	if len(replacementHistory) > 0 {
		queued, _, duplicateResult, duplicate, err = c.sessions.ReplaceHistoryAndEnqueueTurn(ctx, queuedTurn, c.workspace, event, submissionID, digest, expectedHistoryDigest, replacementHistory, func(saved session.QueuedTurn) ([]byte, error) {
			return json.Marshal(TurnSubmission{Status: status, QueueID: saved.QueueID, ExecutionID: executionID, ParentExecutionID: steeringParentID, Position: saved.Position, ReplacedUserTurns: replacedUserTurns})
		})
	} else if submissionID == "" {
		queued, _, err = c.sessions.EnqueueTurn(ctx, queuedTurn, event)
	} else if len(analysisJSON) > 0 {
		queued, _, duplicateResult, duplicate, err = c.sessions.EnqueueTurnIdempotentWithAnalysis(ctx, queuedTurn, event, submissionID, digest, func(saved session.QueuedTurn) ([]byte, error) {
			return json.Marshal(TurnSubmission{Status: status, QueueID: saved.QueueID, ExecutionID: executionID, ParentExecutionID: steeringParentID, Position: saved.Position})
		}, analysisJSON)
	} else {
		queued, _, duplicateResult, duplicate, err = c.sessions.EnqueueTurnIdempotent(ctx, queuedTurn, event, submissionID, digest, func(saved session.QueuedTurn) ([]byte, error) {
			return json.Marshal(TurnSubmission{Status: status, QueueID: saved.QueueID, ExecutionID: executionID, ParentExecutionID: steeringParentID, Position: saved.Position})
		})
	}
	if err != nil {
		unlock()
		return TurnSubmission{}, nil, err
	}
	if duplicate {
		var submission TurnSubmission
		if err := json.Unmarshal(duplicateResult, &submission); err != nil {
			unlock()
			return TurnSubmission{}, nil, fmt.Errorf("decode saved turn submission: %w", err)
		}
		unlock()
		return submission, nil, nil
	}
	if waiter != nil {
		c.turnWaiters[executionID] = waiter
	}
	unlock()
	c.eventHub(sessionID).notifyDurable()
	c.scheduleQueueDrain(sessionID)
	return TurnSubmission{Status: status, QueueID: queued.QueueID, ExecutionID: executionID, ParentExecutionID: steeringParentID, Position: queued.Position, ReplacedUserTurns: replacedUserTurns}, waiter, nil
}
