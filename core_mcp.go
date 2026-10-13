package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/afterlune/luneCore/internal/session"
	publictool "github.com/afterlune/luneCore/tool"
)

func (c *Core) MCPServers() []MCPServerStatus {
	if c == nil || c.mcpManager == nil {
		return nil
	}
	servers := c.mcpManager.List()
	result := make([]MCPServerStatus, 0, len(servers))
	for _, server := range servers {
		result = append(result, MCPServerStatus{
			Name: server.Name, Transport: string(server.Transport), Endpoint: server.Endpoint, Command: server.Command, Enabled: server.Enabled,
			Connected: server.Connected, ToolCount: server.ToolCount, LastError: server.LastError,
		})
	}
	return result
}

func (c *Core) EnableMCPServer(name string) error {
	if c == nil || c.mcpManager == nil {
		return errors.New("MCP manager is unavailable")
	}
	if !c.runtimeConfigMu.TryLock() {
		return errors.New("cannot change MCP servers while a task is running")
	}
	defer c.runtimeConfigMu.Unlock()
	c.mu.RLock()
	closed := c.closed
	running := c.hasRunningTasksLocked()
	c.mu.RUnlock()
	if closed {
		return errors.New("core is closed")
	}
	if running {
		return errors.New("cannot change MCP servers while a task is running")
	}
	return c.mcpManager.Enable(name)
}

func (c *Core) DisableMCPServer(name string) error {
	if c == nil || c.mcpManager == nil {
		return errors.New("MCP manager is unavailable")
	}
	if !c.runtimeConfigMu.TryLock() {
		return errors.New("cannot change MCP servers while a task is running")
	}
	defer c.runtimeConfigMu.Unlock()
	c.mu.RLock()
	closed := c.closed
	running := c.hasRunningTasksLocked()
	c.mu.RUnlock()
	if closed {
		return errors.New("core is closed")
	}
	if running {
		return errors.New("cannot change MCP servers while a task is running")
	}
	return c.mcpManager.Disable(name)
}

func (c *Core) AuthenticateMCPServer(ctx context.Context, name string) error {
	if c == nil || c.mcpManager == nil {
		return errors.New("MCP manager is unavailable")
	}
	if !c.runtimeConfigMu.TryLock() {
		return errors.New("cannot authorize an MCP server while a task is running")
	}
	defer c.runtimeConfigMu.Unlock()
	c.mu.RLock()
	closed := c.closed
	running := c.hasRunningTasksLocked()
	c.mu.RUnlock()
	if closed {
		return errors.New("core is closed")
	}
	if running {
		return errors.New("cannot authorize an MCP server while a task is running")
	}
	return c.mcpManager.Authorize(ctx, name)
}

func (c *Core) OpenMCPURL(serverName, target string) error {
	if c == nil || c.mcpManager == nil {
		return errors.New("MCP manager is unavailable")
	}
	return c.mcpManager.OpenElicitationURL(serverName, target)
}

func (c *Core) requestUserInteraction(ctx context.Context, agentID, serverName string, request json.RawMessage) (json.RawMessage, error) {
	if agentID != "" && agentID != "lead" {
		return nil, errors.New("MCP user interaction from subagents is not supported")
	}
	identity := publictool.CallIdentityFromContext(ctx)
	c.mu.RLock()
	available := c.interactiveSessions[identity.SessionID]
	c.mu.RUnlock()
	if !available {
		return nil, errors.New("this client cannot display MCP user interaction; use the TUI")
	}
	id, err := newID()
	if err != nil {
		return nil, fmt.Errorf("create user interaction ID: %w", err)
	}
	channel := make(chan userInteractionResult, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("core is closed")
	}
	interaction := UserInteractionRequest{ID: id, AgentID: agentID, ServerName: serverName, Request: append(json.RawMessage(nil), request...)}
	c.interactions[id] = pendingInteraction{response: channel, request: interaction, sessionID: identity.SessionID, taskID: identity.TaskID, executionID: identity.ExecutionID}
	c.mu.Unlock()
	c.emit(Event{Kind: EventUserInteractionRequired, SessionID: identity.SessionID, TaskID: identity.TaskID, ExecutionID: identity.ExecutionID, Interaction: &interaction})
	select {
	case result := <-channel:
		return result.Response, result.Err
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.interactions, id)
		c.mu.Unlock()
		c.emit(Event{Kind: EventUserInteractionCancelled, SessionID: identity.SessionID, TaskID: identity.TaskID, ExecutionID: identity.ExecutionID, InteractionID: id})
		return nil, ctx.Err()
	}
}

func (c *Core) resolveUserInteraction(sessionID, id string, response json.RawMessage) error {
	c.mu.Lock()
	pending, ok := c.interactions[id]
	if ok && pending.sessionID == sessionID {
		delete(c.interactions, id)
	} else {
		ok = false
	}
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("user interaction %q is no longer pending in this session", id)
	}
	pending.response <- userInteractionResult{Response: append(json.RawMessage(nil), response...)}
	return nil
}

func (c *Core) resolveUserQuestion(ctx context.Context, sessionID string, answer UserQuestionAnswer) error {
	if c == nil || ctx == nil || sessionID == "" || answer.RunID == "" || answer.InvocationID == "" || answer.CallID == "" {
		return errors.New("session runtime and exact user question wait identity are required")
	}
	question, pending, err := c.runtime.PendingUserQuestion(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("inspect pending user question: %w", err)
	}
	input := session.GraphResumeInput{RunID: answer.RunID, InvocationID: answer.InvocationID, CallID: answer.CallID}
	payload, err := json.Marshal(answer.Answer)
	if err != nil {
		return err
	}
	input.Payload = payload
	inserted, err := c.sessions.PutGraphResumeInput(ctx, sessionID, input)
	if err != nil {
		return err
	}
	matches := pending && question.RunID == answer.RunID && question.InvocationID == answer.InvocationID && question.CallID == answer.CallID
	if !matches {
		if inserted {
			_ = c.sessions.DeleteGraphResumeInput(ctx, sessionID, input)
			return fmt.Errorf("user question %q is stale or no longer pending in this session", answer.CallID)
		}
		return nil
	}
	if state, ok := c.StateForSession(sessionID); ok && state.Status == TaskRunning && state.ExecutionID == question.ExecutionID {
		return nil
	}
	if _, _, err := c.recoverSession(ctx, sessionID, false); err != nil {
		if errors.Is(err, ErrSessionBusy) {
			return nil
		}
		return fmt.Errorf("resume answered user question: %w", err)
	}
	return nil
}
