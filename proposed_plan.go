package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/afterlune/luneCore/internal/model"
)

var (
	// ErrPlanUnavailable means the source execution did not complete with a valid plan.
	ErrPlanUnavailable = errors.New("completed execution has no proposed plan")
	// ErrPlanOutdated means a newer turn has replaced the plan in its source session.
	ErrPlanOutdated = errors.New("proposed plan is no longer the latest session result")
)

// ProposedPlan is a complete plan emitted by the model while the active
// permission mode is read-only.
type ProposedPlan struct {
	Text string `json:"text"`
}

// PlanImplementationMode selects the conversation context used to execute a plan.
type PlanImplementationMode string

const (
	// PlanImplementSameContext continues the session that produced the plan.
	PlanImplementSameContext PlanImplementationMode = "same_context"
	// PlanImplementNewContext creates a sibling session containing only the plan.
	PlanImplementNewContext PlanImplementationMode = "new_context"
)

// PlanImplementation identifies the session and queued turn that will execute
// an approved plan. A host can render “stay read-only” by dismissing the plan
// without calling ImplementPlan.
type PlanImplementation struct {
	SessionID  string
	Submission TurnSubmission
}

// extractProposedPlan recognizes one complete plan block in a model response.
func extractProposedPlan(raw json.RawMessage) *ProposedPlan {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil
	}
	var response model.ModelResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil
	}
	var text strings.Builder
	for _, item := range response.Output {
		message, ok := item.(model.OutputMessage)
		if !ok || message.Role != "" && message.Role != model.RoleAssistant {
			continue
		}
		for _, part := range message.Content {
			if output, ok := part.(model.OutputText); ok {
				text.WriteString(output.Text)
			}
		}
	}
	const open, close = "<proposed_plan>", "</proposed_plan>"
	content := text.String()
	start := strings.Index(content, open)
	if start < 0 || strings.Contains(content[start+len(open):], open) {
		return nil
	}
	start += len(open)
	end := strings.Index(content[start:], close)
	if end < 0 {
		return nil
	}
	end += start
	if strings.Contains(content[end+len(close):], close) {
		return nil
	}
	plan := strings.TrimSpace(content[start:end])
	if plan == "" {
		return nil
	}
	return &ProposedPlan{Text: plan}
}

// ImplementPlan starts one approved plan with workspace permission scoped to
// that turn. The Core's global permission mode is left unchanged.
func (r *SessionRuntime) ImplementPlan(ctx context.Context, sourceExecutionID string, mode PlanImplementationMode) (PlanImplementation, error) {
	if r == nil || r.core == nil || ctx == nil || sourceExecutionID == "" {
		return PlanImplementation{}, errors.New("session runtime, context, and source execution id are required")
	}
	if mode != PlanImplementSameContext && mode != PlanImplementNewContext {
		return PlanImplementation{}, fmt.Errorf("unsupported plan implementation mode %q", mode)
	}
	plan, source, err := r.core.validatePlanSource(ctx, r.sessionID, sourceExecutionID)
	if err != nil {
		return PlanImplementation{}, err
	}

	targetSessionID := r.sessionID
	inputText := "Implement the plan."
	if mode == PlanImplementNewContext {
		inputText = "Implement the following plan. Treat this plan as the complete task context; do not assume access to the previous conversation.\n\n" + plan.Text
	}
	input, err := NewTaskInput(inputText)
	if err != nil {
		return PlanImplementation{}, err
	}
	if mode == PlanImplementNewContext {
		target, err := r.core.createSession(ctx, json.RawMessage("null"), source.Settings)
		if err != nil {
			return PlanImplementation{}, err
		}
		targetSessionID = target.ID
	}
	submission, _, err := r.core.submitTurnWithPermission(ctx, targetSessionID, TurnRequest{Input: input}, StartIfIdle, false, string(PermissionWorkspace))
	if err != nil {
		if targetSessionID != r.sessionID {
			if _, deleteErr := r.core.sessions.Delete(context.Background(), targetSessionID, r.core.workspace); deleteErr != nil {
				err = errors.Join(err, fmt.Errorf("remove unused plan session: %w", deleteErr))
			}
		}
		return PlanImplementation{}, err
	}
	return PlanImplementation{SessionID: targetSessionID, Submission: submission}, nil
}

func (c *Core) validatePlanSource(ctx context.Context, sessionID, executionID string) (*ProposedPlan, Session, error) {
	stored, found, err := c.sessions.TurnResult(ctx, sessionID, executionID)
	if err != nil {
		return nil, Session{}, err
	}
	if !found || TaskStatus(stored.Status) != TaskCompleted {
		return nil, Session{}, ErrPlanUnavailable
	}
	plan := extractProposedPlan(stored.Response)
	if plan == nil {
		return nil, Session{}, ErrPlanUnavailable
	}
	source, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return nil, Session{}, err
	}
	c.mu.RLock()
	closed := c.closed
	running := c.sessionIsRunningLocked(sessionID)
	c.mu.RUnlock()
	if closed {
		return nil, Session{}, errors.New("core is closed")
	}
	queued, err := c.sessions.QueuedTurns(ctx, sessionID)
	if err != nil {
		return nil, Session{}, err
	}
	if running || len(queued) > 0 {
		return nil, Session{}, ErrSessionBusy
	}

	var latest string
	var cursor int64
	for {
		events, err := c.sessions.RuntimeEventsAfter(ctx, sessionID, cursor, 200)
		if err != nil {
			return nil, Session{}, err
		}
		for _, event := range events {
			cursor = event.Cursor
			var decoded Event
			if err := json.Unmarshal(event.Payload, &decoded); err != nil {
				return nil, Session{}, fmt.Errorf("decode session event: %w", err)
			}
			if decoded.Kind == EventTaskFinished || decoded.Kind == EventTaskDiscarded {
				latest = decoded.ExecutionID
			}
		}
		if len(events) < 200 {
			break
		}
	}
	if latest != executionID {
		return nil, Session{}, ErrPlanOutdated
	}
	return plan, source, nil
}
