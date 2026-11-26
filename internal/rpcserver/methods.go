package rpcserver

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"time"

	core "github.com/afterlune/luneCore"
	publictool "github.com/afterlune/luneCore/tool"
)

type helloParams struct {
	ClientName    string `json:"clientName,omitempty"`
	ClientVersion string `json:"clientVersion,omitempty"`
}

type sessionSummary struct {
	ID                 string                      `json:"id"`
	UpdatedAt          time.Time                   `json:"updatedAt"`
	DataVersion        int                         `json:"dataVersion"`
	Title              string                      `json:"title"`
	Archived           bool                        `json:"archived"`
	Settings           sessionSettings             `json:"settings"`
	Analysis           *core.AnalysisMetadata      `json:"analysis,omitempty"`
	UsageRequests      []core.UsageRecord          `json:"usageRequests,omitempty"`
	AgentUsage         map[string]core.UsageTotals `json:"agentUsage,omitempty"`
	AgentUsageComplete bool                        `json:"agentUsageComplete"`
	TaskUsage          core.UsageTotals            `json:"taskUsage"`
	SessionUsage       core.UsageTotals            `json:"sessionUsage"`
}

type sessionSettings struct {
	ModelProfile         string   `json:"modelProfile"`
	ReasoningEffort      string   `json:"reasoningEffort,omitempty"`
	ContextWindow        *int64   `json:"contextWindow,omitempty"`
	ReserveOutputTokens  *int64   `json:"reserveOutputTokens,omitempty"`
	AutoCompact          *bool    `json:"autoCompact,omitempty"`
	AutoCompactThreshold *float64 `json:"autoCompactThreshold,omitempty"`
}

func (s sessionSettings) coreSettings() core.SessionSettings {
	return core.SessionSettings{
		ModelProfile: s.ModelProfile, ReasoningEffort: s.ReasoningEffort,
		ContextWindow: s.ContextWindow, ReserveOutputTokens: s.ReserveOutputTokens,
		AutoCompact: s.AutoCompact, AutoCompactThreshold: s.AutoCompactThreshold,
	}
}

func summarizeSession(session core.Session) sessionSummary {
	return sessionSummary{
		ID: session.ID, UpdatedAt: session.UpdatedAt, DataVersion: session.DataVersion,
		Title: session.Title, Archived: session.Archived,
		Analysis: session.Analysis,
		Settings: sessionSettings{
			ModelProfile: session.Settings.ModelProfile, ReasoningEffort: session.Settings.ReasoningEffort,
			ContextWindow: session.Settings.ContextWindow, ReserveOutputTokens: session.Settings.ReserveOutputTokens,
			AutoCompact: session.Settings.AutoCompact, AutoCompactThreshold: session.Settings.AutoCompactThreshold,
		},
		UsageRequests: session.UsageRequests, AgentUsage: session.AgentUsage,
		AgentUsageComplete: session.AgentUsageComplete, TaskUsage: session.TaskUsage,
		SessionUsage: session.SessionUsage,
	}
}

func (c *connection) dispatch(method string, raw json.RawMessage) (any, *rpcError) {
	switch method {
	case "workspace.info", "workspace.listDirectory", "workspace.readFile":
		return c.workspaceFiles(method, raw)
	case "review.start":
		return c.startWorkspaceReview(raw)
	case "review.get":
		return c.getWorkspaceReview(raw)
	case "diagnosis.start":
		return c.startTaskDiagnosis(raw)
	case "diagnosis.get":
		return c.getTaskDiagnosis(raw)
	case "session.historyPage":
		return c.historyPage(raw)
	case "session.search":
		return c.searchSessions(raw)
	case "session.fork", "session.rewindPreview", "session.rewind", "session.compact", "turn.cancelQueued", "turn.cancelPending", "turn.replaceQueued", "turn.implementPlan":
		return c.manageSession(method, raw)
	case "goal.get", "goal.create", "goal.pause", "goal.resume", "goal.edit", "goal.setBudget", "goal.clear":
		return c.manageGoal(method, raw)
	case "question.saveDraft":
		return c.saveQuestionDraft(raw)
	case "command.list", "command.read", "command.stop":
		return c.manageCommand(method, raw)
	case "mcp.servers", "mcp.enable", "mcp.disable", "mcp.listPrompts", "mcp.getPrompt", "mcp.listResources", "mcp.listResourceTemplates", "mcp.readResource":
		return c.manageMCP(method, raw)
	case "permission.get":
		return map[string]any{
			"mode":    string(c.server.core.PermissionMode()),
			"modes":   allowedPermissionModes(c.server.maxPermissionMode),
			"maxMode": string(c.server.maxPermissionMode),
		}, nil
	case "permission.set":
		var params struct {
			Mode string `json:"mode"`
		}
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		mode := core.PermissionMode(strings.TrimSpace(strings.ToLower(params.Mode)))
		rank := permissionRank(mode)
		if rank < 0 {
			return nil, &rpcError{Code: -32602, Message: "unknown permission mode"}
		}
		if c.server.maxPermissionMode != "" {
			maxRank := permissionRank(c.server.maxPermissionMode)
			if maxRank >= 0 && rank > maxRank {
				return nil, &rpcError{Code: -32001, Message: "permission mode exceeds server maximum"}
			}
		}
		if err := c.server.core.SetPermissionMode(mode); err != nil {
			return nil, applicationError(err)
		}
		return map[string]any{"mode": string(c.server.core.PermissionMode())}, nil

	case "runtime.hello":
		var params helloParams
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		return map[string]any{
			"protocolVersion": "1",
			"client":          map[string]string{"name": params.ClientName, "version": params.ClientVersion},
			"transports":      []string{"stdio", "wss"},
			"maxMessageBytes": MaxMessageBytes,
			"permissionMode":  string(c.server.core.PermissionMode()),
			"capabilities": []string{
				"session.search", "session.fork", "session.rewind", "session.compact",
				"turn.cancelQueued", "turn.cancelPending", "turn.replaceQueued", "turn.replaceHistory",
				"turn.plan", "turn.implementPlan", "turn.attachments", "session.historyPage",
				"goal.manage", "question.draft", "command.control", "mcp.inspect", "workspace.files",
				"permission.manage",
				"review.workspace",
				"task.diagnose",
			},
		}, nil
	case "session.create":
		var params struct {
			History json.RawMessage `json:"history"`
		}
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		session, err := c.server.core.CreateSession(c.ctx, params.History)
		if err != nil {
			return nil, applicationError(err)
		}
		return summarizeSession(session), nil
	case "session.list":
		var params struct {
			Offset          int  `json:"offset"`
			Limit           int  `json:"limit"`
			IncludeArchived bool `json:"includeArchived"`
		}
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		if params.Offset < 0 || params.Limit < 0 || params.Limit > 500 {
			return nil, &rpcError{Code: -32602, Message: "offset must be non-negative and limit must be between 0 and 500"}
		}
		if params.Limit == 0 {
			params.Limit = 50
		}
		sessions, err := c.server.core.ListSessionPage(c.ctx, params.Offset, params.Limit, params.IncludeArchived)
		if err != nil {
			return nil, applicationError(err)
		}
		result := make([]sessionSummary, 0, len(sessions))
		for _, session := range sessions {
			result = append(result, summarizeSession(session))
		}
		return result, nil
	case "session.get":
		var params struct {
			SessionID string `json:"sessionId"`
		}
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		session, err := c.server.core.GetSession(c.ctx, params.SessionID)
		if err != nil {
			return nil, applicationError(err)
		}
		return summarizeSession(session), nil
	case "session.history":
		var params struct {
			SessionID string `json:"sessionId"`
		}
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		session, err := c.server.core.GetSession(c.ctx, params.SessionID)
		if err != nil {
			return nil, applicationError(err)
		}
		return session.History, nil
	case "session.setTitle":
		var params struct {
			SessionID string `json:"sessionId"`
			Title     string `json:"title"`
		}
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		if err := c.server.core.SetSessionTitle(c.ctx, params.SessionID, params.Title); err != nil {
			return nil, applicationError(err)
		}
		return map[string]bool{"updated": true}, nil
	case "session.setSettings":
		var params struct {
			SessionID string          `json:"sessionId"`
			Settings  sessionSettings `json:"settings"`
		}
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		if err := c.server.core.SetSessionSettings(c.ctx, params.SessionID, params.Settings.coreSettings()); err != nil {
			return nil, applicationError(err)
		}
		return map[string]bool{"updated": true}, nil
	case "session.setArchived":
		var params struct {
			SessionID string `json:"sessionId"`
			Archived  bool   `json:"archived"`
		}
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		if err := c.server.core.SetSessionArchived(c.ctx, params.SessionID, params.Archived); err != nil {
			return nil, applicationError(err)
		}
		return map[string]bool{"updated": true}, nil
	case "session.delete":
		var params struct {
			SessionID string `json:"sessionId"`
		}
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		if err := c.server.core.DeleteSession(c.ctx, params.SessionID); err != nil {
			return nil, applicationError(err)
		}
		return map[string]bool{"deleted": true}, nil
	case "model.list":
		return c.server.core.ListModelProfiles(), nil
	case "runtime.state":
		var params struct {
			SessionID string `json:"sessionId"`
		}
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		return c.withRuntime(c.ctx, params.SessionID, func(runtime *core.SessionRuntime) (any, error) {
			return runtime.State(c.ctx)
		})
	case "runtime.pending":
		var params struct {
			SessionID string `json:"sessionId"`
		}
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		return c.withRuntime(c.ctx, params.SessionID, func(runtime *core.SessionRuntime) (any, error) {
			return runtime.PendingRequests(c.ctx)
		})
	case "turn.submit":
		return c.submitTurn(raw)
	case "turn.replaceHistory":
		return c.replaceHistoryAndSubmitTurn(raw)
	case "turn.wait":
		var params struct {
			SessionID   string `json:"sessionId"`
			ExecutionID string `json:"executionId"`
		}
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		return c.withRuntime(c.ctx, params.SessionID, func(runtime *core.SessionRuntime) (any, error) {
			return runtime.WaitTurn(c.ctx, params.ExecutionID)
		})
	case "turn.interrupt":
		var params struct {
			SessionID   string `json:"sessionId"`
			ExecutionID string `json:"executionId"`
		}
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		return c.withRuntime(c.ctx, params.SessionID, func(runtime *core.SessionRuntime) (any, error) {
			interrupted, err := runtime.Interrupt(c.ctx, params.ExecutionID)
			return map[string]bool{"interrupted": interrupted}, err
		})
	case "turn.recover":
		var params struct {
			SessionID string                `json:"sessionId"`
			Execution core.ExecutionOptions `json:"execution"`
		}
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		return c.withRuntime(c.ctx, params.SessionID, func(runtime *core.SessionRuntime) (any, error) {
			return runtime.Recover(c.ctx, params.Execution)
		})
	case "turn.discardPaused":
		var params struct {
			SessionID string `json:"sessionId"`
		}
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		return c.withRuntime(c.ctx, params.SessionID, func(runtime *core.SessionRuntime) (any, error) {
			discarded, err := runtime.DiscardPaused(c.ctx)
			return map[string]bool{"discarded": discarded}, err
		})
	case "approval.resolve":
		var params struct {
			SessionID string                      `json:"sessionId"`
			ID        string                      `json:"id"`
			Decision  publictool.ApprovalDecision `json:"decision"`
		}
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		return c.withRuntime(c.ctx, params.SessionID, func(runtime *core.SessionRuntime) (any, error) {
			return map[string]bool{"resolved": true}, runtime.ResolveApprovalDecision(c.ctx, params.ID, params.Decision)
		})
	case "question.resolve":
		var params struct {
			SessionID string                  `json:"sessionId"`
			Answer    core.UserQuestionAnswer `json:"answer"`
		}
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		return c.withRuntime(c.ctx, params.SessionID, func(runtime *core.SessionRuntime) (any, error) {
			return map[string]bool{"resolved": true}, runtime.ResolveUserQuestion(c.ctx, params.Answer)
		})
	case "interaction.resolve":
		var params struct {
			SessionID string          `json:"sessionId"`
			ID        string          `json:"id"`
			Response  json.RawMessage `json:"response"`
		}
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		return c.withRuntime(c.ctx, params.SessionID, func(runtime *core.SessionRuntime) (any, error) {
			return map[string]bool{"resolved": true}, runtime.ResolveUserInteraction(params.ID, params.Response)
		})
	case "toolRecovery.resolve":
		var params struct {
			SessionID    string                    `json:"sessionId"`
			InvocationID string                    `json:"invocationId"`
			Decision     core.ToolRecoveryDecision `json:"decision"`
		}
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		return c.withRuntime(c.ctx, params.SessionID, func(runtime *core.SessionRuntime) (any, error) {
			return map[string]bool{"resolved": true}, runtime.ResolveToolRecovery(c.ctx, params.InvocationID, params.Decision)
		})
	case "runtime.subscribe":
		return c.subscribe(raw)
	case "runtime.unsubscribe":
		var params struct {
			SubscriptionID string `json:"subscriptionId"`
		}
		if err := c.readParams(raw, &params); err != nil {
			return nil, err
		}
		return map[string]bool{"unsubscribed": c.unsubscribe(params.SubscriptionID)}, nil
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found", Data: method}
	}
}

type submitParams struct {
	SessionID     string                `json:"sessionId"`
	SubmissionID  string                `json:"submissionId"`
	Input         json.RawMessage       `json:"input"`
	Mode          core.SubmissionMode   `json:"mode"`
	PlanMode      bool                  `json:"planMode,omitempty"`
	Attachments   []attachment          `json:"attachments,omitempty"`
	Model         string                `json:"model,omitempty"`
	MaxToolRounds int                   `json:"maxToolRounds,omitempty"`
	Execution     core.ExecutionOptions `json:"execution,omitempty"`
}

func (c *connection) submitTurn(raw json.RawMessage) (any, *rpcError) {
	var params submitParams
	if err := c.readParams(raw, &params); err != nil {
		return nil, err
	}
	if strings.TrimSpace(params.SubmissionID) == "" || len(params.SubmissionID) > 128 {
		return nil, &rpcError{Code: -32602, Message: "submissionId is required and must be at most 128 characters"}
	}
	if len(params.Input) == 0 || !json.Valid(params.Input) {
		return nil, &rpcError{Code: -32602, Message: "input must be valid JSON"}
	}
	if params.PlanMode && params.Mode == core.SteerInput {
		return nil, &rpcError{Code: -32602, Message: "a Plan turn must run separately from the current task"}
	}
	if params.Mode == "" {
		params.Mode = core.StartIfIdle
	}
	digestInput, err := json.Marshal(params)
	if err != nil {
		return nil, applicationError(err)
	}
	digest := sha256.Sum256(digestInput)
	input := params.Input
	if len(params.Attachments) > 0 {
		input, err = attachmentInput(params)
		if err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
	}
	request := core.TurnRequest{
		Input: core.TaskInput{Raw: input}, Model: params.Model, PlanMode: params.PlanMode, RequireImageProfile: len(params.Attachments) > 0,
		MaxToolRounds: params.MaxToolRounds, Execution: params.Execution,
	}
	return c.withRuntime(c.ctx, params.SessionID, func(runtime *core.SessionRuntime) (any, error) {
		return runtime.SubmitTurnWithID(c.ctx, request, params.Mode, params.SubmissionID, digest[:])
	})
}

func (c *connection) replaceHistoryAndSubmitTurn(raw json.RawMessage) (any, *rpcError) {
	var params struct {
		SessionID     string                `json:"sessionId"`
		SubmissionID  string                `json:"submissionId"`
		Turns         int                   `json:"turns"`
		HistoryDigest string                `json:"historyDigest"`
		Input         json.RawMessage       `json:"input"`
		Mode          core.SubmissionMode   `json:"mode,omitempty"`
		PlanMode      bool                  `json:"planMode,omitempty"`
		Attachments   []attachment          `json:"attachments,omitempty"`
		Model         string                `json:"model,omitempty"`
		MaxToolRounds int                   `json:"maxToolRounds,omitempty"`
		Execution     core.ExecutionOptions `json:"execution,omitempty"`
	}
	if err := c.readParams(raw, &params); err != nil {
		return nil, err
	}
	if strings.TrimSpace(params.SubmissionID) == "" || len(params.SubmissionID) > 128 || params.Turns <= 0 || len(params.HistoryDigest) != 64 {
		return nil, &rpcError{Code: -32602, Message: "submissionId, positive turns, and a history preview digest are required"}
	}
	if len(params.Input) == 0 || !json.Valid(params.Input) {
		return nil, &rpcError{Code: -32602, Message: "input must be valid JSON"}
	}
	if params.Mode == "" {
		params.Mode = core.StartIfIdle
	}
	if params.Mode != core.StartIfIdle {
		return nil, &rpcError{Code: -32602, Message: "history replacement must start only when the session is idle"}
	}
	digestInput, err := json.Marshal(params)
	if err != nil {
		return nil, applicationError(err)
	}
	digest := sha256.Sum256(digestInput)
	input := params.Input
	if len(params.Attachments) > 0 {
		input, err = attachmentInput(submitParams{Input: params.Input, Attachments: params.Attachments})
		if err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
	}
	request := core.TurnRequest{
		Input: core.TaskInput{Raw: input}, Model: params.Model, PlanMode: params.PlanMode, RequireImageProfile: len(params.Attachments) > 0,
		MaxToolRounds: params.MaxToolRounds, Execution: params.Execution,
	}
	return c.withRuntime(c.ctx, params.SessionID, func(runtime *core.SessionRuntime) (any, error) {
		return runtime.ReplaceHistoryAndSubmitTurnWithID(c.ctx, params.Turns, params.HistoryDigest, request, params.SubmissionID, digest[:])
	})
}

func (c *connection) subscribe(raw json.RawMessage) (any, *rpcError) {
	var params struct {
		SessionID      string `json:"sessionId"`
		SubscriptionID string `json:"subscriptionId"`
		AfterCursor    int64  `json:"afterCursor"`
	}
	if err := c.readParams(raw, &params); err != nil {
		return nil, err
	}
	if params.SubscriptionID == "" || len(params.SubscriptionID) > 128 || params.AfterCursor < 0 {
		return nil, &rpcError{Code: -32602, Message: "subscriptionId (up to 128 characters) and a non-negative afterCursor are required"}
	}
	c.mu.Lock()
	if _, exists := c.subscriptions[params.SubscriptionID]; exists || len(c.subscriptions) >= maxSubscriptionsPerConnection {
		c.mu.Unlock()
		return nil, &rpcError{Code: -32001, Message: "subscription id is already in use or the connection subscription limit was reached"}
	}
	c.mu.Unlock()
	runtime, err := c.server.core.OpenSessionRuntime(c.ctx, params.SessionID)
	if err != nil {
		return nil, applicationError(err)
	}
	ctx, cancel := context.WithCancel(c.ctx)
	events, err := runtime.SubscribeEventsWithReplayComplete(ctx, params.AfterCursor)
	if err != nil {
		cancel()
		_ = runtime.Close(context.Background())
		return nil, applicationError(err)
	}
	sub := &subscription{cancel: cancel, runtime: runtime, ready: make(chan struct{}), done: make(chan struct{})}
	c.mu.Lock()
	if _, exists := c.subscriptions[params.SubscriptionID]; exists || len(c.subscriptions) >= maxSubscriptionsPerConnection {
		c.mu.Unlock()
		cancel()
		_ = runtime.Close(context.Background())
		return nil, &rpcError{Code: -32001, Message: "subscription id is already in use or the connection subscription limit was reached"}
	}
	c.subscriptions[params.SubscriptionID] = sub
	c.mu.Unlock()
	c.server.retainInteractiveSession(params.SessionID, runtime)
	c.eventWG.Add(1)
	go func() {
		defer c.eventWG.Done()
		defer close(sub.done)
		defer func() {
			c.server.releaseInteractiveSession(params.SessionID, runtime)
			_ = runtime.Close(context.Background())
		}()
		select {
		case <-sub.ready:
		case <-ctx.Done():
			return
		}
		for event := range events {
			if err := c.notify("runtime.event", map[string]any{"subscriptionId": params.SubscriptionID, "event": event}); err != nil {
				cancel()
				break
			}
		}
		c.mu.Lock()
		if c.subscriptions[params.SubscriptionID] == sub {
			delete(c.subscriptions, params.SubscriptionID)
		}
		c.mu.Unlock()
	}()
	return subscriptionReady{
		result: map[string]string{"subscriptionId": params.SubscriptionID},
		ready:  sub.ready,
	}, nil
}

func (c *connection) unsubscribe(id string) bool {
	c.mu.Lock()
	sub := c.subscriptions[id]
	if sub != nil {
		delete(c.subscriptions, id)
		sub.cancel()
	}
	c.mu.Unlock()
	if sub == nil {
		return false
	}
	<-sub.done
	return true
}

func permissionRank(mode core.PermissionMode) int {
	switch mode {
	case core.PermissionReadOnly:
		return 0
	case core.PermissionWorkspace:
		return 1
	case core.PermissionAuto:
		return 2
	case core.PermissionFullAccess:
		return 3
	default:
		return -1
	}
}

func allowedPermissionModes(maxMode core.PermissionMode) []string {
	ordered := []core.PermissionMode{
		core.PermissionReadOnly,
		core.PermissionWorkspace,
		core.PermissionAuto,
		core.PermissionFullAccess,
	}
	maxRank := permissionRank(maxMode)
	modes := make([]string, 0, len(ordered))
	for _, m := range ordered {
		if maxMode != "" && maxRank >= 0 && permissionRank(m) > maxRank {
			break
		}
		modes = append(modes, string(m))
	}
	return modes
}
