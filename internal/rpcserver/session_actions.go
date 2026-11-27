package rpcserver

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	core "github.com/afterlune/luneCore"
	"github.com/afterlune/luneCore/internal/agentgraph"
)

type transcriptItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func historyDigest(raw json.RawMessage) string { return fmt.Sprintf("%x", sha256.Sum256(raw)) }

func transcript(raw json.RawMessage) []transcriptItem {
	var items []transcriptItem
	if json.Unmarshal(raw, &items) == nil {
		return items
	}
	var wrapped struct {
		Items []transcriptItem `json:"items"`
	}
	_ = json.Unmarshal(raw, &wrapped)
	return wrapped.Items
}

func chatText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range parts {
		if part.Text != "" {
			builder.WriteString(part.Text)
			builder.WriteByte('\n')
		}
	}
	return strings.TrimSpace(builder.String())
}

func containsChatText(session core.Session, term string) bool {
	if strings.Contains(strings.ToLower(session.Title), term) || strings.Contains(strings.ToLower(session.ID), term) {
		return true
	}
	for _, item := range transcript(session.History) {
		if item.Type == "message" && (item.Role == "assistant" || item.Role == "user") &&
			strings.Contains(strings.ToLower(chatText(item.Content)), term) {
			return true
		}
	}
	return false
}

func (c *connection) searchSessions(raw json.RawMessage) (any, *rpcError) {
	var params struct {
		Query           string `json:"query"`
		Offset          int    `json:"offset"`
		Limit           int    `json:"limit"`
		IncludeArchived bool   `json:"includeArchived"`
	}
	if err := c.readParams(raw, &params); err != nil {
		return nil, err
	}
	term := strings.ToLower(strings.TrimSpace(params.Query))
	if term == "" || len(term) > 512 || params.Offset < 0 || params.Limit < 0 || params.Limit > 100 {
		return nil, &rpcError{Code: -32602, Message: "query, non-negative offset, and limit up to 100 are required"}
	}
	if params.Limit == 0 {
		params.Limit = 50
	}
	const pageSize = 100
	matches := make([]sessionSummary, 0, params.Limit)
	skipped := 0
	for offset := 0; ; offset += pageSize {
		page, err := c.server.core.ListSessionPage(c.ctx, offset, pageSize, params.IncludeArchived)
		if err != nil {
			return nil, applicationError(err)
		}
		for _, saved := range page {
			if !containsChatText(saved, term) {
				continue
			}
			if skipped < params.Offset {
				skipped++
				continue
			}
			matches = append(matches, summarizeSession(saved))
			if len(matches) == params.Limit {
				return matches, nil
			}
		}
		if len(page) < pageSize {
			return matches, nil
		}
	}
}

func (c *connection) manageSession(method string, raw json.RawMessage) (any, *rpcError) {
	var params struct {
		SessionID   string `json:"sessionId"`
		ExecutionID string `json:"executionId"`
		Turns       int    `json:"turns"`
		Digest      string `json:"digest"`
	}
	if err := c.readParams(raw, &params); err != nil {
		return nil, err
	}
	switch method {
	case "session.fork":
		saved, err := c.server.core.ForkSession(c.ctx, params.SessionID)
		if err != nil {
			return nil, applicationError(err)
		}
		return summarizeSession(saved), nil
	case "session.compact":
		result, err := c.server.core.Compact(c.ctx, params.SessionID)
		return result, applicationError(err)
	case "session.rewindPreview":
		if params.Turns <= 0 {
			return nil, &rpcError{Code: -32602, Message: "turns must be positive"}
		}
		saved, err := c.server.core.GetSession(c.ctx, params.SessionID)
		if err != nil {
			return nil, applicationError(err)
		}
		var turns []string
		for _, item := range transcript(saved.History) {
			if item.Type != "message" || item.Role != "user" {
				continue
			}
			text := chatText(item.Content)
			if agentgraph.IsHostContextText(item.Role, strings.TrimSpace(text)) {
				continue
			}
			runes := []rune(text)
			if len(runes) > 120 {
				text = string(runes[:120]) + "…"
			}
			if text == "" {
				text = "(attachment)"
			}
			turns = append(turns, text)
		}
		count := min(len(turns), params.Turns)
		excerpts := turns[len(turns)-min(count, 10):]
		return map[string]any{"removed": count, "digest": historyDigest(saved.History), "excerpts": excerpts}, nil
	case "session.rewind":
		if params.Turns <= 0 || len(params.Digest) != 64 {
			return nil, &rpcError{Code: -32602, Message: "turns and a preview digest are required"}
		}
		removed, err := c.server.core.RewindSessionIfCurrent(c.ctx, params.SessionID, params.Turns, params.Digest)
		return map[string]int{"removed": removed}, applicationError(err)
	case "turn.cancelQueued":
		return c.withRuntime(c.ctx, params.SessionID, func(runtime *core.SessionRuntime) (any, error) {
			err := runtime.CancelQueuedTurn(c.ctx, params.ExecutionID)
			return map[string]bool{"cancelled": err == nil}, err
		})
	case "turn.cancelPending":
		return c.withRuntime(c.ctx, params.SessionID, func(runtime *core.SessionRuntime) (any, error) {
			cancelled, err := runtime.CancelPendingTurns(c.ctx)
			if cancelled == nil {
				cancelled = []string{}
			}
			return map[string]any{"cancelledExecutionIds": cancelled}, err
		})
	case "turn.replaceQueued":
		var replaceParams struct {
			SessionID   string          `json:"sessionId"`
			ExecutionID string          `json:"executionId"`
			Input       json.RawMessage `json:"input"`
			Attachments []attachment    `json:"attachments,omitempty"`
		}
		if err := c.readParams(raw, &replaceParams); err != nil {
			return nil, err
		}
		if strings.TrimSpace(replaceParams.ExecutionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "executionId is required"}
		}
		if len(replaceParams.Input) == 0 || !json.Valid(replaceParams.Input) {
			return nil, &rpcError{Code: -32602, Message: "input must be valid JSON"}
		}
		input := replaceParams.Input
		if len(replaceParams.Attachments) > 0 {
			var err error
			input, err = attachmentInput(submitParams{Input: replaceParams.Input, Attachments: replaceParams.Attachments})
			if err != nil {
				return nil, &rpcError{Code: -32602, Message: err.Error()}
			}
		}
		return c.withRuntime(c.ctx, replaceParams.SessionID, func(runtime *core.SessionRuntime) (any, error) {
			submission, cancelled, err := runtime.ReplaceQueuedTurnInput(c.ctx, replaceParams.ExecutionID, core.TaskInput{Raw: input})
			if cancelled == nil {
				cancelled = []string{}
			}
			return map[string]any{"submission": submission, "cancelledExecutionIds": cancelled}, err
		})
	case "turn.implementPlan":
		var planParams struct {
			SessionID         string                      `json:"sessionId"`
			SourceExecutionID string                      `json:"sourceExecutionId"`
			Mode              core.PlanImplementationMode `json:"mode,omitempty"`
		}
		if err := c.readParams(raw, &planParams); err != nil {
			return nil, err
		}
		if strings.TrimSpace(planParams.SourceExecutionID) == "" {
			return nil, &rpcError{Code: -32602, Message: "sourceExecutionId is required"}
		}
		if planParams.Mode == "" {
			planParams.Mode = core.PlanImplementSameContext
		}
		if planParams.Mode != core.PlanImplementSameContext && planParams.Mode != core.PlanImplementNewContext {
			return nil, &rpcError{Code: -32602, Message: "mode must be same_context or new_context"}
		}
		return c.withRuntime(c.ctx, planParams.SessionID, func(runtime *core.SessionRuntime) (any, error) {
			return runtime.ImplementPlan(c.ctx, planParams.SourceExecutionID, core.PlanImplementationOptions{Mode: planParams.Mode})
		})
	}
	return nil, &rpcError{Code: -32601, Message: "method not found"}
}

func (c *connection) manageGoal(method string, raw json.RawMessage) (any, *rpcError) {
	var params struct {
		SessionID              string `json:"sessionId"`
		Objective              string `json:"objective,omitempty"`
		TokenBudget            *int64 `json:"tokenBudget,omitempty"`
		NoInteractiveDecisions *bool  `json:"noInteractiveDecisions,omitempty"`
	}
	if err := c.readParams(raw, &params); err != nil {
		return nil, err
	}
	return c.withRuntime(c.ctx, params.SessionID, func(runtime *core.SessionRuntime) (any, error) {
		switch method {
		case "goal.get":
			goal, err := runtime.GetGoal(c.ctx)
			return map[string]any{"goal": goal}, err
		case "goal.create":
			noInteractive := false
			if params.NoInteractiveDecisions != nil {
				noInteractive = *params.NoInteractiveDecisions
			}
			return runtime.CreateGoal(c.ctx, core.GoalCreateOptions{
				Objective:              params.Objective,
				TokenBudget:            params.TokenBudget,
				NoInteractiveDecisions: noInteractive,
			})
		case "goal.pause":
			return runtime.PauseGoal(c.ctx)
		case "goal.resume":
			if params.NoInteractiveDecisions != nil {
				return runtime.ResumeGoal(c.ctx, core.GoalResumeOptions{NoInteractiveDecisions: *params.NoInteractiveDecisions})
			}
			return runtime.ResumeGoal(c.ctx)
		case "goal.edit":
			return runtime.EditGoal(c.ctx, params.Objective)
		case "goal.setBudget":
			return runtime.SetGoalBudget(c.ctx, params.TokenBudget)
		case "goal.clear":
			err := runtime.ClearGoal(c.ctx)
			return map[string]bool{"cleared": err == nil}, err
		default:
			return nil, fmt.Errorf("unknown goal method %q", method)
		}
	})
}

func (c *connection) saveQuestionDraft(raw json.RawMessage) (any, *rpcError) {
	var params struct {
		SessionID string                  `json:"sessionId"`
		Wait      core.UserQuestionAnswer `json:"wait"`
		Draft     *core.UserQuestionDraft `json:"draft,omitempty"`
		Payload   json.RawMessage         `json:"payload,omitempty"`
	}
	if err := c.readParams(raw, &params); err != nil {
		return nil, err
	}
	if params.Draft != nil && len(params.Payload) > 0 {
		return nil, &rpcError{Code: -32602, Message: "specify either draft or payload, not both"}
	}
	if params.Draft == nil && (len(params.Payload) == 0 || !json.Valid(params.Payload)) {
		return nil, &rpcError{Code: -32602, Message: "draft or valid JSON payload is required"}
	}
	return c.withRuntime(c.ctx, params.SessionID, func(runtime *core.SessionRuntime) (any, error) {
		if params.Draft != nil {
			err := runtime.SaveUserQuestionDraft(c.ctx, params.Wait, *params.Draft)
			return map[string]bool{"saved": err == nil}, err
		}
		err := runtime.SaveUserQuestionDraftPayload(c.ctx, params.Wait, params.Payload)
		return map[string]bool{"saved": err == nil}, err
	})
}

func (c *connection) manageCommand(method string, raw json.RawMessage) (any, *rpcError) {
	var params struct {
		SessionID string `json:"sessionId"`
		CommandID string `json:"commandId,omitempty"`
		Cursor    int64  `json:"cursor,omitempty"`
		WaitMs    int64  `json:"waitMs,omitempty"`
	}
	if err := c.readParams(raw, &params); err != nil {
		return nil, err
	}
	if method != "command.list" && strings.TrimSpace(params.CommandID) == "" {
		return nil, &rpcError{Code: -32602, Message: "commandId is required"}
	}
	if params.Cursor < 0 || params.WaitMs < 0 || params.WaitMs > 5000 {
		return nil, &rpcError{Code: -32602, Message: "cursor must be non-negative and waitMs must be between 0 and 5000"}
	}
	return c.withRuntime(c.ctx, params.SessionID, func(runtime *core.SessionRuntime) (any, error) {
		switch method {
		case "command.list":
			commands, err := runtime.ListCommands(c.ctx)
			if commands == nil {
				commands = []core.CommandInfo{}
			}
			return commands, err
		case "command.read":
			return runtime.ReadCommand(c.ctx, params.CommandID, core.CommandReadOptions{
				Cursor: params.Cursor,
				Wait:   time.Duration(params.WaitMs) * time.Millisecond,
			})
		case "command.stop":
			return runtime.StopCommand(c.ctx, params.CommandID)
		default:
			return nil, fmt.Errorf("unknown command method %q", method)
		}
	})
}

func (c *connection) manageMCP(method string, raw json.RawMessage) (any, *rpcError) {
	var params struct {
		SessionID  string            `json:"sessionId,omitempty"`
		ServerName string            `json:"serverName,omitempty"`
		PromptName string            `json:"promptName,omitempty"`
		Arguments  map[string]string `json:"arguments,omitempty"`
		URI        string            `json:"uri,omitempty"`
	}
	if err := c.readParams(raw, &params); err != nil {
		return nil, err
	}
	switch method {
	case "mcp.servers":
		servers := c.server.core.MCPServers()
		if servers == nil {
			servers = []core.MCPServerStatus{}
		}
		return servers, nil
	case "mcp.enable":
		if strings.TrimSpace(params.ServerName) == "" {
			return nil, &rpcError{Code: -32602, Message: "serverName is required"}
		}
		if err := c.server.core.EnableMCPServer(params.ServerName); err != nil {
			return nil, applicationError(err)
		}
		return map[string]bool{"enabled": true}, nil
	case "mcp.disable":
		if strings.TrimSpace(params.ServerName) == "" {
			return nil, &rpcError{Code: -32602, Message: "serverName is required"}
		}
		if err := c.server.core.DisableMCPServer(params.ServerName); err != nil {
			return nil, applicationError(err)
		}
		return map[string]bool{"enabled": false}, nil
	}
	if strings.TrimSpace(params.ServerName) == "" {
		return nil, &rpcError{Code: -32602, Message: "serverName is required"}
	}
	if method == "mcp.getPrompt" && strings.TrimSpace(params.PromptName) == "" {
		return nil, &rpcError{Code: -32602, Message: "promptName is required"}
	}
	if method == "mcp.readResource" && strings.TrimSpace(params.URI) == "" {
		return nil, &rpcError{Code: -32602, Message: "uri is required"}
	}
	return c.withRuntime(c.ctx, params.SessionID, func(runtime *core.SessionRuntime) (any, error) {
		switch method {
		case "mcp.listPrompts":
			prompts, err := runtime.ListMCPPrompts(c.ctx, params.ServerName)
			if prompts == nil {
				prompts = []core.MCPPrompt{}
			}
			return prompts, err
		case "mcp.getPrompt":
			return runtime.GetMCPPrompt(c.ctx, params.ServerName, params.PromptName, params.Arguments)
		case "mcp.listResources":
			resources, err := runtime.ListMCPResources(c.ctx, params.ServerName)
			if resources == nil {
				resources = []core.MCPResource{}
			}
			return resources, err
		case "mcp.listResourceTemplates":
			templates, err := runtime.ListMCPResourceTemplates(c.ctx, params.ServerName)
			if templates == nil {
				templates = []core.MCPResourceTemplate{}
			}
			return templates, err
		case "mcp.readResource":
			return runtime.ReadMCPResource(c.ctx, params.ServerName, params.URI)
		default:
			return nil, fmt.Errorf("unknown MCP method %q", method)
		}
	})
}
