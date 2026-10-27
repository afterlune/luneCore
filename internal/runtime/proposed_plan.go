package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
)

var reMarkdownCode = regexp.MustCompile("(?s)```.*?```|`[^`\n]+`")

var (
	// ErrPlanUnavailable means the source execution did not complete with a valid plan.
	ErrPlanUnavailable = errors.New("completed execution has no proposed plan")
	// ErrPlanOutdated means a newer turn has replaced the plan in its source session.
	ErrPlanOutdated = errors.New("proposed plan is no longer the latest session result")
)

const (
	proposedPlanOpenTag  = "<proposed_plan>"
	proposedPlanCloseTag = "</proposed_plan>"
	// Some providers emit this self-closing alias instead of the canonical close tag.
	proposedPlanCompatCloseTag = "<proposed_plan_end/>"
)

// ProposedPlan is a complete plan emitted by the model.
type ProposedPlan struct {
	Text string `json:"text"`
}

// PlanAuthorization is the host-confirmed permission snapshot for one plan run.
type PlanAuthorization = publictool.PlanAuthorization

// PlanImplementationMode selects the conversation context used to execute a plan.
type PlanImplementationMode string

const (
	// PlanImplementSameContext continues the session that produced the plan.
	PlanImplementSameContext PlanImplementationMode = "same_context"
	// PlanImplementNewContext creates a sibling session containing only the plan.
	PlanImplementNewContext PlanImplementationMode = "new_context"
)

// PlanImplementationOptions contains the host's context and execution authorization.
type PlanImplementationOptions struct {
	Mode           PlanImplementationMode
	PermissionMode PermissionMode
	Authorization  PlanAuthorization
}

// PlanImplementation identifies the session and queued turn that will execute
// an approved plan. A host can render “stay read-only” by dismissing the plan
// without calling ImplementPlan.
type PlanImplementation struct {
	SessionID  string
	Submission TurnSubmission
}

// extractProposedPlan recognizes one complete plan block in a model response.
// Inline mentions or code blocks like `<proposed_plan>` are protected so that
// discussing plan tags in code spans does not invalidate the actual plan.
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
	content := text.String()
	if !strings.Contains(strings.ToLower(content), "proposed_plan") {
		return nil
	}

	var codeTokens []string
	protected := reMarkdownCode.ReplaceAllStringFunc(content, func(m string) string {
		idx := len(codeTokens)
		codeTokens = append(codeTokens, m)
		return "\x00CODE_" + strconv.Itoa(idx) + "\x00"
	})

	start := strings.Index(protected, proposedPlanOpenTag)
	if start < 0 || strings.Contains(protected[start+len(proposedPlanOpenTag):], proposedPlanOpenTag) {
		return nil
	}
	start += len(proposedPlanOpenTag)
	end, closeLength := proposedPlanCloseIndex(protected[start:])
	if end < 0 {
		return nil
	}
	end += start
	if _, extraCloseLength := proposedPlanCloseIndex(protected[end+closeLength:]); extraCloseLength > 0 {
		return nil
	}
	planRaw := protected[start:end]
	for idx, token := range codeTokens {
		planRaw = strings.ReplaceAll(planRaw, "\x00CODE_"+strconv.Itoa(idx)+"\x00", token)
	}
	plan := strings.TrimSpace(planRaw)
	if plan == "" {
		return nil
	}
	return &ProposedPlan{Text: plan}
}

func proposedPlanCloseIndex(text string) (int, int) {
	canonical := strings.Index(text, proposedPlanCloseTag)
	compat := strings.Index(text, proposedPlanCompatCloseTag)
	switch {
	case canonical < 0 && compat < 0:
		return -1, 0
	case canonical < 0:
		return compat, len(proposedPlanCompatCloseTag)
	case compat < 0:
		return canonical, len(proposedPlanCloseTag)
	case canonical < compat:
		return canonical, len(proposedPlanCloseTag)
	default:
		return compat, len(proposedPlanCompatCloseTag)
	}
}

// ImplementPlan starts one approved plan using the selected permission mode.
// The Core's global permission mode is left unchanged.
func (r *SessionRuntime) ImplementPlan(ctx context.Context, sourceExecutionID string, options PlanImplementationOptions) (PlanImplementation, error) {
	if r == nil || r.core == nil || ctx == nil || sourceExecutionID == "" {
		return PlanImplementation{}, errors.New("session runtime, context, and source execution id are required")
	}
	mode := options.Mode
	if mode != PlanImplementSameContext && mode != PlanImplementNewContext {
		return PlanImplementation{}, fmt.Errorf("unsupported plan implementation mode %q", mode)
	}
	plan, source, err := r.core.validatePlanSource(ctx, r.sessionID, sourceExecutionID)
	if err != nil {
		return PlanImplementation{}, err
	}
	authorization, err := validatePlanAuthorization(options.Authorization)
	if err != nil {
		return PlanImplementation{}, err
	}
	executionPermission := options.PermissionMode
	if executionPermission == "" {
		executionPermission = r.core.PermissionMode()
	}
	if err := validatePermissionOverride(string(executionPermission)); err != nil {
		return PlanImplementation{}, err
	}

	targetSessionID := r.sessionID
	inputText := "Implement the plan under the active permission mode. Use the normal approval flow whenever an action requires permission."
	if mode == PlanImplementNewContext {
		inputText = "Implement the following plan under the active permission mode. Treat it as the complete task context; do not assume access to the previous conversation. Use the normal approval flow whenever an action requires permission.\n\n" + plan.Text
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
	submission, _, err := r.core.submitTurn(ctx, targetSessionID, TurnRequest{
		Input: input, PermissionMode: executionPermission, PlanAuthorization: authorization,
	}, StartIfIdle, false)
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

func validatePlanAuthorization(authorization PlanAuthorization) (PlanAuthorization, error) {
	validated := authorization
	validated.Paths = append([]publictool.PathGrant(nil), authorization.Paths...)
	validated.AllowedTools = append([]string(nil), authorization.AllowedTools...)
	validated.ApprovedHighRiskCommands = append([]string(nil), authorization.ApprovedHighRiskCommands...)
	for index, grant := range validated.Paths {
		if grant.Access != publictool.PathRead && grant.Access != publictool.PathReadWrite {
			return PlanAuthorization{}, fmt.Errorf("path grant %d has unsupported access %q", index, grant.Access)
		}
		if !filepath.IsAbs(grant.Path) {
			return PlanAuthorization{}, fmt.Errorf("path grant %d must use an absolute path", index)
		}
		resolved, err := filepath.EvalSymlinks(grant.Path)
		if err != nil {
			return PlanAuthorization{}, fmt.Errorf("resolve approved path %q: %w", grant.Path, err)
		}
		if _, err := os.Stat(resolved); err != nil {
			return PlanAuthorization{}, fmt.Errorf("inspect approved path %q: %w", grant.Path, err)
		}
		validated.Paths[index].Path = resolved
	}
	for index, command := range validated.ApprovedHighRiskCommands {
		command = strings.TrimSpace(command)
		if command == "" {
			return PlanAuthorization{}, fmt.Errorf("approved high-risk command %d must not be empty", index)
		}
		validated.ApprovedHighRiskCommands[index] = command
	}
	for index, name := range validated.AllowedTools {
		name = strings.TrimSpace(name)
		if name == "" {
			return PlanAuthorization{}, fmt.Errorf("approved tool %d must not be empty", index)
		}
		validated.AllowedTools[index] = name
	}
	return validated, nil
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
	closed := c.closed || c.closing
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
