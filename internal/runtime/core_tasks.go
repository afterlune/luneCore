package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
	publictool "github.com/afterlune/luneCore/tool"
)

// State returns the state last changed by any session. Use StateForSession or
// ActiveTasks when a Core is running multiple sessions concurrently.
func (c *Core) State() TaskState {
	if c == nil {
		return TaskState{Status: TaskFailed}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.active
}

// StateForSession returns the most recent task state for one session.
func (c *Core) StateForSession(sessionID string) (TaskState, bool) {
	if c == nil || sessionID == "" {
		return TaskState{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	state, ok := c.activeTasks[sessionID]
	return state, ok
}

// ActiveTasks returns currently running tasks in stable session ID order.
func (c *Core) ActiveTasks() []TaskState {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	ids := make([]string, 0, len(c.activeTasks))
	for id, state := range c.activeTasks {
		if state.Status == TaskRunning {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	states := make([]TaskState, 0, len(ids))
	for _, id := range ids {
		states = append(states, c.activeTasks[id])
	}
	return states
}

func (c *Core) hasRunningTasksLocked() bool {
	for _, state := range c.activeTasks {
		if state.Status == TaskRunning {
			return true
		}
	}
	return false
}

func (c *Core) sessionIsRunningLocked(sessionID string) bool {
	if c.activeTasks[sessionID].Status == TaskRunning {
		return true
	}
	return c.active.Status == TaskRunning && c.active.SessionID == sessionID
}

func (c *Core) resolveApprovalDecision(sessionID, id string, decision publictool.ApprovalDecision) error {
	c.mu.Lock()
	pending, ok := c.approvals[id]
	if ok && pending.sessionID == sessionID {
		delete(c.approvals, id)
	} else {
		ok = false
	}
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("approval request %q is no longer pending in this session", id)
	}
	if decision == publictool.ApprovalAllowSession && pending.request.PathGrant != nil {
		if err := c.addSessionPathGrant(context.Background(), sessionID, *pending.request.PathGrant); err != nil {
			pending.response <- publictool.ApprovalDeny
			return fmt.Errorf("save session path grant: %w", err)
		}
		if pending.pathGrants != nil {
			pending.pathGrants.Add(*pending.request.PathGrant)
		}
	}
	if decision == publictool.ApprovalAllowSession && pending.request.Tool != "" {
		item := publictool.SessionApproval{
			Tool:        pending.request.Tool,
			Arguments:   tool.NormalizeArguments(pending.request.Arguments),
			Fingerprint: pending.request.Fingerprint,
		}
		if err := c.addSessionApproval(context.Background(), sessionID, item); err != nil {
			pending.response <- publictool.ApprovalDeny
			return fmt.Errorf("save session approval: %w", err)
		}
		if pending.sessionApprovals != nil {
			pending.sessionApprovals.Add(item)
		}
	}
	pending.response <- decision
	return nil
}

func (c *Core) resolveApproval(sessionID, id string, allowed bool) error {
	if allowed {
		return c.resolveApprovalDecision(sessionID, id, publictool.ApprovalAllowSession)
	}
	return c.resolveApprovalDecision(sessionID, id, publictool.ApprovalDeny)
}

func (c *Core) beginTask(ctx context.Context, sessionID, queueID string, settings SessionSettings, profile model.ModelProfile, modelName string, taskOptions agentgraph.TaskOptions, expectedUpdatedAt time.Time, cancel context.CancelFunc) (string, error) {
	if ctx == nil {
		return "", errors.New("context is nil")
	}
	c.mu.Lock()
	if c.closed || c.closing {
		c.mu.Unlock()
		return "", errors.New("core is closed")
	}
	if c.sessionIsRunningLocked(sessionID) {
		c.mu.Unlock()
		return "", errors.New("a task is already running in this session")
	}
	if c.interruptRequests[taskOptions.ExecutionID] {
		delete(c.interruptRequests, taskOptions.ExecutionID)
		c.mu.Unlock()
		return "", errors.Join(context.Canceled, errExecutionInterrupted)
	}
	if queueID == "" {
		queued, err := c.sessions.QueuedTurns(ctx, sessionID)
		if err != nil {
			c.mu.Unlock()
			return "", err
		}
		if len(queued) > 0 {
			c.mu.Unlock()
			return "", ErrSessionBusy
		}
	}
	snapshot, err := c.sessions.Get(ctx, sessionID, c.workspace)
	if err != nil {
		c.mu.Unlock()
		return "", err
	}
	if !snapshot.UpdatedAt.Equal(expectedUpdatedAt) {
		c.mu.Unlock()
		return "", errors.New("session changed while preparing the task; retry with the latest session state")
	}
	id, err := newID()
	if err != nil {
		c.mu.Unlock()
		return "", err
	}
	state := TaskState{
		GoalID: taskOptions.GoalID, GoalRevision: taskOptions.GoalRevision, AutomaticGoal: taskOptions.AutomaticGoal,
		Status: TaskRunning, SessionID: sessionID, TaskID: id, ExecutionID: taskOptions.ExecutionID, QueueID: queueID, ContextWindow: taskOptions.ContextWindow,
		ReserveOutputTokens: taskOptions.ReserveOutputTokens, AutoCompact: taskOptions.AutoCompact,
		AutoCompactThreshold: taskOptions.AutoCompactThreshold, Goal: taskOptions.Goal, MaxToolRounds: taskOptions.MaxToolRounds,
		ModelOverride: taskOptions.Model,
		ModelProfile:  settings.ModelProfile, Provider: profile.Provider, Model: modelName,
		ReasoningEffort: settings.ReasoningEffort, Execution: coreExecutionOptions(taskOptions.Execution),
	}
	if c.activeTasks == nil {
		c.activeTasks = make(map[string]TaskState)
	}
	c.activeTasks[sessionID] = state
	if c.steeringExecutions == nil {
		c.steeringExecutions = make(map[string]string)
	}
	c.steeringExecutions[sessionID] = taskOptions.ExecutionID
	if cancel != nil {
		c.taskCancels[sessionID] = cancel
	} else {
		delete(c.taskCancels, sessionID)
	}
	c.active = state
	c.mu.Unlock()
	c.emit(Event{Kind: EventTaskStarted, TaskID: id, ExecutionID: taskOptions.ExecutionID, QueueID: queueID, SessionID: sessionID, Status: TaskRunning, ContextWindow: taskOptions.ContextWindow,
		GoalID: taskOptions.GoalID, GoalRevision: taskOptions.GoalRevision, AutomaticGoal: taskOptions.AutomaticGoal,
		ModelProfile: settings.ModelProfile, Provider: profile.Provider, Model: modelName})
	return id, nil
}

func (c *Core) endTask(sessionID, taskID string, cancel context.CancelFunc) {
	c.mu.Lock()
	state, ok := c.activeTasks[sessionID]
	if ok && state.TaskID == taskID {
		// A repeated interrupt can race the paused queue commit. It belongs to
		// this attempt, not a future recovery under the same ExecutionID.
		delete(c.interruptRequests, state.ExecutionID)
		if c.steeringExecutions[sessionID] == state.ExecutionID {
			delete(c.steeringExecutions, sessionID)
		}
		delete(c.taskCancels, sessionID)
		if state.Status == TaskRunning {
			state.Status = TaskIdle
			c.activeTasks[sessionID] = state
			c.active = state
		}
	}
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *Core) finishTaskState(sessionID string, status TaskStatus, err error) {
	c.mu.Lock()
	state := c.activeTasks[sessionID]
	state.Status = status
	c.activeTasks[sessionID] = state
	c.active = state
	c.mu.Unlock()
	event := Event{Kind: EventTaskFinished, TaskID: state.TaskID, ExecutionID: state.ExecutionID, QueueID: state.QueueID, SessionID: sessionID, Status: status,
		BudgetReason:  budgetReasonFromError(err),
		ContextWindow: state.ContextWindow, Err: errorString(err), ModelProfile: state.ModelProfile, Provider: state.Provider, Model: state.Model}
	if state.QueueID != "" {
		if status != TaskPaused {
			// The queue worker commits terminal state, result, conversation, and
			// event together after the graph has produced a durable checkpoint.
			return
		}
		persistErr := c.persistQueuedEvent(context.Background(), event, func(payload json.RawMessage) (int64, error) {
			modelFailure, failureErr := c.modelFailureArtifact(context.Background(), sessionID)
			if failureErr != nil {
				return 0, failureErr
			}
			return c.sessions.PauseQueuedTurnWithReason(context.Background(), sessionID, state.QueueID, state.ExecutionID, string(TaskPaused), string(event.BudgetReason), payload, modelFailure)
		})
		if persistErr != nil {
			c.logger.Warn("persist paused queued task", "error", persistErr)
		}
		return
	}
	c.emit(event)
}

func (c *Core) onToolEvent(event tool.ToolEvent) {
	if !event.Started && !event.Failed && !event.Canceled {
		metadata, _ := c.registry.ToolMetadata(event.Name)
		if metadata.PermissionClass != model.ToolPermissionSessionControl {
			c.goalMu.Lock()
			if ref, ok := c.goalRuns[event.ExecutionID]; ok {
				ref.ToolProgress = true
				c.goalRuns[event.ExecutionID] = ref
			}
			c.goalMu.Unlock()
		}
	}
	kind := EventToolFinished
	if event.Started {
		kind = EventToolStarted
	}
	c.emit(Event{Kind: kind, SessionID: event.SessionID, TaskID: event.TaskID, ExecutionID: event.ExecutionID, Tool: ToolCall{
		ExecutionID: event.ExecutionID, ID: event.CallID, InvocationID: event.InvocationID, AgentID: event.AgentID, Name: event.Name, Arguments: event.Arguments, Started: event.Started,
		Details: event.Details, Duration: event.Duration, Recovered: event.Recovered, Failed: event.Failed, Canceled: event.Canceled,
	}})
}

func (c *Core) onModelStream(agentID, agentName, profile, provider, modelID string, event model.ModelStreamEvent) {
	c.onModelStreamForTask(c.State(), agentID, agentName, profile, provider, modelID, event)
}

func (c *Core) onModelStreamForTask(state TaskState, agentID, agentName, profile, provider, modelID string, event model.ModelStreamEvent) {
	stream := &ModelStreamEvent{
		RequestID: event.RequestID, Purpose: event.Purpose,
		Type: string(event.Type), ItemID: event.ItemID, OutputIndex: event.OutputIndex,
		Delta: event.Delta,
	}
	if event.ToolCall != nil {
		stream.ToolCallID, stream.ToolName, stream.ToolArguments = event.ToolCall.CallID, event.ToolCall.Name, event.ToolCall.Arguments
	}
	c.emit(Event{
		Kind: EventModelStream, SessionID: state.SessionID, TaskID: state.TaskID, ExecutionID: state.ExecutionID,
		AgentID: agentID, AgentName: agentName, ModelProfile: profile, Provider: provider, Model: modelID,
		Stream: stream,
	})
}

func (c *Core) onUsage(usage model.ResponseUsage) {
	c.onAgentUsage(usage, "", "", "", "", 0)
}

func (c *Core) onAgentUsage(usage model.ResponseUsage, agentID, profile, provider, modelID string, estimatedInputTokens int64) {
	c.onAgentUsageForTask(c.State(), usage, agentID, profile, provider, modelID, estimatedInputTokens, "", model.RequestTrace{})
}

func (c *Core) onAgentUsageContext(ctx context.Context, usage model.ResponseUsage, agentID, profile, provider, modelID string, estimatedInputTokens int64) {
	identity := publictool.CallIdentityFromContext(ctx)
	state, ok := c.StateForSession(identity.SessionID)
	if !ok {
		state = TaskState{SessionID: identity.SessionID, TaskID: identity.TaskID}
	}
	c.onAgentUsageForTask(state, usage, agentID, profile, provider, modelID, estimatedInputTokens, model.UsagePurposeFromContext(ctx), model.RequestTraceFromContext(ctx))
}

func (c *Core) onAgentUsageForTask(state TaskState, usage model.ResponseUsage, agentID, profile, provider, modelID string, estimatedInputTokens int64, purpose UsagePurpose, trace model.RequestTrace) {
	totalTokens := usage.TotalTokens
	if totalTokens <= 0 {
		totalTokens = usage.InputTokens + usage.OutputTokens
	}
	value := Usage{InputTokens: usage.InputTokens, InputTokensDetails: TokenInputDetails{
		CachedTokens: usage.InputTokensDetails.CachedTokens, CacheCreationTokens: usage.InputTokensDetails.CacheCreationTokens,
	},
		OutputTokens: usage.OutputTokens, OutputTokensDetails: TokenOutputDetails{ReasoningTokens: usage.OutputTokensDetails.ReasoningTokens}, TotalTokens: totalTokens}
	c.accountGoalUsage(state, value, agentID, estimatedInputTokens, trace)
	contextWindow := state.ContextWindow
	if agentID != "" && agentID != "lead" {
		if workerProfile, ok := c.models.Profile(profile); ok {
			if workerProfile.ContextWindow > 0 {
				contextWindow = workerProfile.ContextWindow
			}
		}
	}
	if state.SessionID != "" {
		if err := c.recordUsageForAgent(model.WithRequestTrace(context.Background(), trace), state.SessionID, value, agentID, profile, provider, modelID, contextWindow, estimatedInputTokens, purpose); err != nil {
			c.logger.Warn("persist token usage", "error", err)
		}
	}
	c.emit(Event{RequestID: trace.ID, RequestAttempt: trace.Attempt, RequestChanges: trace.Changes, Kind: EventUsage, SessionID: state.SessionID, TaskID: state.TaskID, ExecutionID: state.ExecutionID, Usage: value,
		AgentID: agentID, UsagePurpose: purpose, ModelProfile: profile, Provider: provider, Model: modelID, ContextWindow: contextWindow,
		EstimatedInputTokens: estimatedInputTokensIfMissing(value.InputTokens, estimatedInputTokens)})
}

func (c *Core) requestApprovalDecision(ctx context.Context, request tool.PermissionRequest) publictool.ApprovalDecision {
	if ctx.Err() != nil {
		return publictool.ApprovalDeny
	}
	id, err := newID()
	if err != nil {
		return publictool.ApprovalDeny
	}
	response := make(chan publictool.ApprovalDecision, 1)
	identity := publictool.CallIdentityFromContext(ctx)
	c.mu.Lock()
	if c.closed || c.closing {
		c.mu.Unlock()
		return publictool.ApprovalDeny
	}
	var pathGrant *publictool.PathGrant
	if request.PathGrant != nil {
		grant := *request.PathGrant
		pathGrant = &grant
	}
	approval := ApprovalRequest{
		ID:               id,
		AgentID:          request.AgentID,
		Tool:             request.Tool,
		Arguments:        append(json.RawMessage(nil), request.Arguments...),
		PathGrant:        pathGrant,
		AutoReviewReason: request.AutoReviewReason,
		Fingerprint:      request.Fingerprint,
	}
	c.approvals[id] = pendingApproval{
		response:         response,
		request:          approval,
		pathGrants:       tool.SessionPathGrantStoreFromContext(ctx),
		sessionApprovals: tool.SessionApprovalStoreFromContext(ctx),
		sessionID:        identity.SessionID,
		taskID:           identity.TaskID,
		executionID:      identity.ExecutionID,
	}
	c.mu.Unlock()
	c.emit(Event{Kind: EventApprovalRequired, SessionID: identity.SessionID, TaskID: identity.TaskID, ExecutionID: identity.ExecutionID,
		AgentID: request.AgentID, Approval: &approval})
	select {
	case decision := <-response:
		return decision
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.approvals, id)
		c.mu.Unlock()
		c.emit(Event{Kind: EventApprovalCancelled, SessionID: identity.SessionID, TaskID: identity.TaskID, ExecutionID: identity.ExecutionID, ApprovalID: id})
		return publictool.ApprovalDeny
	}
}

func (c *Core) requestApproval(ctx context.Context, request tool.PermissionRequest) bool {
	return c.requestApprovalDecision(ctx, request) != publictool.ApprovalDeny
}

func (c *Core) autoReviewProfileID(sessionID string) string {
	if sessionID != "" {
		if state, ok := c.StateForSession(sessionID); ok && state.ModelProfile != "" {
			return state.ModelProfile
		}
	}
	if c.models != nil {
		return c.models.DefaultModel()
	}
	return ""
}

type autoReviewModelOutput struct {
	Decision string `json:"decision"`
	Safe     *bool  `json:"safe,omitempty"`
	Reason   string `json:"reason"`
}

func (c *Core) autoReviewToolRequest(ctx context.Context, request tool.PermissionRequest) (tool.AutoReviewResult, error) {
	if c == nil || ctx == nil || c.models == nil {
		return tool.AutoReviewResult{Decision: tool.AutoReviewAskUser, Reason: "auto-review model client is unavailable"}, nil
	}
	identity := publictool.CallIdentityFromContext(ctx)
	profileID := c.autoReviewProfileID(identity.SessionID)
	if profileID == "" {
		return tool.AutoReviewResult{Decision: tool.AutoReviewAskUser, Reason: "model profile for auto-review is unconfigured"}, nil
	}
	client, err := c.models.NewClient(profileID, c.registry, c.logger)
	if err != nil {
		c.logger.Warn("auto-review client creation failed", "error", err)
		return tool.AutoReviewResult{Decision: tool.AutoReviewAskUser, Reason: fmt.Sprintf("create auto-review client: %v", err)}, nil
	}

	reviewCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var sb strings.Builder
	sb.WriteString("Tool: ")
	sb.WriteString(request.Tool)
	sb.WriteString("\nWorkspace: ")
	sb.WriteString(c.workspace)
	sb.WriteString("\n")
	if len(request.Arguments) > 0 {
		sb.WriteString("Arguments: ")
		sb.Write(request.Arguments)
		sb.WriteString("\n")
	}
	if request.PathGrant != nil {
		sb.WriteString(fmt.Sprintf("Path Grant Request: Path=%q, Access=%q\n", request.PathGrant.Path, request.PathGrant.Access))
	}

	modelReq := model.ModelRequest{
		Instructions: "You are a security auditor reviewing agent tool invocations in a software development environment. " +
			"Evaluate whether the requested tool operation is standard, benign, and safe to execute automatically (such as standard test runs, builds, file inspections within workspace), or if it carries risk (destructive actions outside workspace, dangerous command execution like rm -rf, git push --force, system alterations, privilege escalation, credential access, or unverified external access). " +
			"If information is insufficient or you are uncertain, you MUST decide \"ask_user\". " +
			"Respond ONLY with a JSON object: {\"decision\": \"approve\"|\"ask_user\", \"reason\": \"...\"}.",
		Input: model.NewResponseInputText(sb.String()),
	}
	reviewCtx = model.WithUsagePurpose(reviewCtx, model.UsagePurposeValidation)
	resp, err := client.CreateResponse(reviewCtx, modelReq)
	if err != nil {
		c.logger.Warn("auto-review model request failed, falling back to interactive approval", "error", err)
		return tool.AutoReviewResult{Decision: tool.AutoReviewAskUser, Reason: "auto-review request failed: " + err.Error()}, nil
	}
	var outputText string
	if resp != nil {
		for _, item := range resp.Output {
			if msg, ok := item.(model.OutputMessage); ok {
				for _, part := range msg.Content {
					if text, ok := part.(model.OutputText); ok {
						outputText += text.Text
					}
				}
			}
		}
	}
	trimmed := strings.TrimSpace(outputText)
	if strings.HasPrefix(trimmed, "```") {
		lines := strings.Split(trimmed, "\n")
		if len(lines) >= 2 {
			if strings.HasPrefix(lines[0], "```") {
				lines = lines[1:]
			}
			if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "```") {
				lines = lines[:len(lines)-1]
			}
			trimmed = strings.TrimSpace(strings.Join(lines, "\n"))
		}
	}
	var res autoReviewModelOutput
	if err := json.Unmarshal([]byte(trimmed), &res); err != nil {
		c.logger.Warn("auto-review unmarshal failed, falling back to interactive approval", "error", err, "raw", outputText)
		return tool.AutoReviewResult{Decision: tool.AutoReviewAskUser, Reason: "review model response was invalid JSON"}, nil
	}

	isApproved := res.Decision == "approve" || (res.Decision == "" && res.Safe != nil && *res.Safe)
	if isApproved {
		c.logger.Info("auto-review approved tool invocation", "tool", request.Tool, "reason", res.Reason)
		// Model auto-review authorizes this single invocation only; do not persist session approvals.
		return tool.AutoReviewResult{Decision: tool.AutoReviewApprove, Reason: res.Reason}, nil
	}

	reason := res.Reason
	if reason == "" {
		reason = "model flagged potential risk or requested interactive confirmation"
	}
	c.logger.Info("auto-review flagged tool invocation for user decision", "tool", request.Tool, "reason", reason)
	return tool.AutoReviewResult{Decision: tool.AutoReviewAskUser, Reason: reason}, nil
}
