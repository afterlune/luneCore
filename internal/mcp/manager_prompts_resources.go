package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/afterlune/luneCore/internal/model"
)

type PromptArgument struct {
	Name        string
	Description string
	Required    bool
}

type Prompt struct {
	Name        string
	Title       string
	Description string
	Arguments   []PromptArgument
}

type PromptResult struct {
	Description string
	Input       *model.ResponseInput
}

type Resource struct {
	URI         string
	Name        string
	Title       string
	Description string
	MIMEType    string
	Size        int64
}

type ResourceTemplate struct {
	URITemplate string
	Name        string
	Title       string
	Description string
	MIMEType    string
}

func (manager *Manager) ListPrompts(ctx context.Context, serverName string) ([]Prompt, error) {
	instance, err := manager.connectedInstance(serverName)
	if err != nil {
		return nil, err
	}
	return listMCPItems[Prompt](ctx, serverName, instance, "prompts/list", "prompts")
}

func (manager *Manager) GetPrompt(ctx context.Context, serverName, promptName string, arguments map[string]string) (PromptResult, error) {
	if strings.TrimSpace(promptName) == "" {
		return PromptResult{}, errors.New("MCP prompt name is required")
	}
	instance, err := manager.connectedInstance(serverName)
	if err != nil {
		return PromptResult{}, err
	}
	params := map[string]any{"name": promptName}
	if arguments != nil {
		params["arguments"] = arguments
	}
	response, err := instance.client.Request(ctx, "prompts/get", params, promptName, nil)
	if err != nil {
		return PromptResult{}, fmt.Errorf("get prompt %q from MCP server %q: %w", promptName, serverName, err)
	}
	response, err = withMRTR(ctx, instance, serverName, "prompts/get", params, promptName, nil, response)
	if err != nil {
		return PromptResult{}, fmt.Errorf("get prompt %q from MCP server %q: %w", promptName, serverName, err)
	}
	var result struct {
		Description string `json:"description"`
		Messages    []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(response, &result); err != nil {
		return PromptResult{}, fmt.Errorf("decode MCP prompt %q: %w", promptName, err)
	}
	if len(result.Messages) == 0 {
		return PromptResult{}, fmt.Errorf("MCP prompt %q returned no messages", promptName)
	}
	items := make([]model.InputItem, 0, len(result.Messages))
	remote := &remoteTool{server: instance, serverName: serverName}
	budget := binaryBudget{limits: instance.managerLimits()}
	for _, message := range result.Messages {
		var role model.Role
		switch message.Role {
		case "user":
			role = model.RoleUser
		case "assistant":
			role = model.RoleAssistant
		default:
			return PromptResult{}, fmt.Errorf("MCP prompt %q returned unsupported message role %q", promptName, message.Role)
		}
		blocks, err := decodeContentBlocks(message.Content)
		if err != nil || len(blocks) == 0 {
			return PromptResult{}, fmt.Errorf("MCP prompt %q returned invalid message content", promptName)
		}
		parts := make([]model.ContentPart, 0, len(blocks))
		for _, block := range blocks {
			var visible, transcript []model.ContentPart
			if err := remote.convertContent(ctx, []json.RawMessage{block}, 0, &budget, &visible, &transcript); err != nil {
				return PromptResult{}, fmt.Errorf("convert MCP prompt %q content: %w", promptName, err)
			}
			for _, part := range append(visible, transcript...) {
				if role == model.RoleAssistant {
					if text, ok := part.(model.InputText); ok {
						part = model.OutputText{Text: text.Text}
					}
				}
				parts = append(parts, part)
			}
		}
		if len(parts) == 0 {
			return PromptResult{}, fmt.Errorf("MCP prompt %q returned an empty message", promptName)
		}
		items = append(items, model.InputMessage{Role: role, Content: model.NewMessageContentParts(parts...)})
	}
	return PromptResult{Description: result.Description, Input: model.NewResponseInputItems(items...)}, nil
}

func (manager *Manager) ListResources(ctx context.Context, serverName string) ([]Resource, error) {
	instance, err := manager.connectedInstance(serverName)
	if err != nil {
		return nil, err
	}
	return listMCPItems[Resource](ctx, serverName, instance, "resources/list", "resources")
}

func (manager *Manager) ListResourceTemplates(ctx context.Context, serverName string) ([]ResourceTemplate, error) {
	instance, err := manager.connectedInstance(serverName)
	if err != nil {
		return nil, err
	}
	return listMCPItems[ResourceTemplate](ctx, serverName, instance, "resources/templates/list", "resourceTemplates")
}

func (manager *Manager) ReadResource(ctx context.Context, serverName, uri string) (*model.ResponseInput, error) {
	if strings.TrimSpace(uri) == "" {
		return nil, errors.New("MCP resource URI is required")
	}
	instance, err := manager.connectedInstance(serverName)
	if err != nil {
		return nil, err
	}
	params := map[string]any{"uri": uri}
	response, err := instance.client.Request(ctx, "resources/read", params, uri, nil)
	if err != nil {
		return nil, fmt.Errorf("read resource %q from MCP server %q: %w", uri, serverName, err)
	}
	response, err = withMRTR(ctx, instance, serverName, "resources/read", params, uri, nil, response)
	if err != nil {
		return nil, fmt.Errorf("read resource %q from MCP server %q: %w", uri, serverName, err)
	}
	var result struct {
		Contents []json.RawMessage `json:"contents"`
	}
	if err := json.Unmarshal(response, &result); err != nil {
		return nil, fmt.Errorf("decode MCP resource %q: %w", uri, err)
	}
	parts := make([]model.ContentPart, 0, len(result.Contents))
	var transcript []model.ContentPart
	budget := binaryBudget{limits: instance.managerLimits()}
	remote := &remoteTool{server: instance, serverName: serverName}
	if err := remote.convertReadContents(ctx, result.Contents, false, &budget, &parts, &transcript); err != nil {
		return nil, fmt.Errorf("convert MCP resource %q: %w", uri, err)
	}
	parts = append(parts, transcript...)
	if len(parts) == 0 {
		return model.NewResponseInputText("MCP resource returned no content."), nil
	}
	item := model.InputMessage{Role: model.RoleUser, Content: model.NewMessageContentParts(parts...)}
	return model.NewResponseInputItems(item), nil
}

func (manager *Manager) connectedInstance(serverName string) (*serverInstance, error) {
	if manager == nil {
		return nil, errors.New("MCP manager is unavailable")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	instance := manager.instances[serverName]
	if manager.closed {
		return nil, errors.New("MCP manager is closed")
	}
	if !manager.enabled[serverName] || instance == nil {
		if _, exists := manager.servers[serverName]; !exists {
			return nil, fmt.Errorf("unknown MCP server %q", serverName)
		}
		return nil, fmt.Errorf("MCP server %q is not enabled and connected", serverName)
	}
	if stdio, ok := instance.client.(*stdioTransport); ok && !stdio.Alive() {
		return nil, fmt.Errorf("MCP server %q is not connected", serverName)
	}
	return instance, nil
}

func listMCPItems[T any](ctx context.Context, serverName string, instance *serverInstance, method, field string) ([]T, error) {
	items := make([]T, 0)
	seen := make(map[string]struct{})
	cursor := ""
	for page := 0; page < 100; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		data, err := instance.client.Request(ctx, method, params, "", nil)
		if err != nil {
			return nil, fmt.Errorf("%s on MCP server %q: %w", method, serverName, err)
		}
		var response map[string]json.RawMessage
		if err := json.Unmarshal(data, &response); err != nil || response == nil {
			return nil, fmt.Errorf("decode %s response from MCP server %q", method, serverName)
		}
		rawItems, exists := response[field]
		if !exists {
			return nil, fmt.Errorf("decode %s response from MCP server %q: missing %s", method, serverName, field)
		}
		var pageItems []T
		if err := json.Unmarshal(rawItems, &pageItems); err != nil {
			return nil, fmt.Errorf("decode %s response from MCP server %q: %w", method, serverName, err)
		}
		items = append(items, pageItems...)
		var rawCursor json.RawMessage
		if raw, ok := response["nextCursor"]; ok {
			rawCursor = raw
		}
		if len(rawCursor) == 0 || string(rawCursor) == "null" {
			return items, nil
		}
		var nextCursor string
		if err := json.Unmarshal(rawCursor, &nextCursor); err != nil {
			return nil, fmt.Errorf("MCP server %q returned an invalid %s cursor", serverName, method)
		}
		if nextCursor == "" {
			return items, nil
		}
		if strings.TrimSpace(nextCursor) != nextCursor {
			return nil, fmt.Errorf("MCP server %q returned an invalid %s cursor", serverName, method)
		}
		if _, duplicate := seen[nextCursor]; duplicate {
			return nil, fmt.Errorf("MCP server %q returned a repeated %s cursor", serverName, method)
		}
		seen[nextCursor] = struct{}{}
		if page == 99 {
			return nil, fmt.Errorf("MCP server %q returned more than 100 %s pages", serverName, method)
		}
		cursor = nextCursor
	}
	return items, nil
}

func decodeContentBlocks(raw json.RawMessage) ([]json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, errors.New("empty content")
	}
	if trimmed[0] == '[' {
		var blocks []json.RawMessage
		err := json.Unmarshal(raw, &blocks)
		return blocks, err
	}
	if !json.Valid(raw) || trimmed[0] != '{' {
		return nil, errors.New("content must be an object or array")
	}
	return []json.RawMessage{append(json.RawMessage(nil), raw...)}, nil
}
