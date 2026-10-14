package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
)

var toolNameRunes = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func (manager *Manager) listTools(ctx context.Context, serverName string, instance *serverInstance) ([]*remoteTool, error) {
	var cursor any
	seen := make(map[string]bool)
	tools := make([]*remoteTool, 0)
	for page := 0; page < 100; page++ {
		params := map[string]any{}
		if cursor != nil {
			params["cursor"] = cursor
		}
		data, err := instance.client.Request(ctx, "tools/list", params, "", nil)
		if err != nil {
			return nil, fmt.Errorf("list tools from MCP server %q: %w", serverName, err)
		}
		var response struct {
			Tools []struct {
				Name        string          `json:"name"`
				Title       string          `json:"title"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
				Annotations struct {
					ReadOnlyHint bool `json:"readOnlyHint"`
				} `json:"annotations"`
			} `json:"tools"`
			NextCursor any `json:"nextCursor"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return nil, fmt.Errorf("decode tools from MCP server %q: %w", serverName, err)
		}
		for _, definition := range response.Tools {
			if strings.TrimSpace(definition.Name) == "" || seen[definition.Name] {
				manager.logger.Warn("ignoring MCP tool with an empty or duplicate name", "server", serverName)
				continue
			}
			seen[definition.Name] = true
			schema := definition.InputSchema
			if len(schema) == 0 || !json.Valid(schema) || !isObjectSchema(schema) {
				manager.logger.Warn("ignoring MCP tool with an invalid input schema", "server", serverName, "tool", definition.Name)
				continue
			}
			var bindings []headerBinding
			if instance.config.Transport == projectconfig.MCPTransportStreamableHTTP {
				var valid bool
				bindings, valid = parseHeaderBindings(schema)
				if !valid {
					manager.logger.Warn("ignoring MCP HTTP tool with invalid x-mcp-header schema annotation", "server", serverName, "tool", definition.Name)
					continue
				}
			}
			publicName := toolName(serverName, definition.Name)
			description := definition.Description
			if description == "" {
				description = definition.Title
			}
			description = fmt.Sprintf("MCP server %q tool %q. %s", serverName, definition.Name, description)
			tools = append(tools, &remoteTool{
				serverName: serverName, remoteName: definition.Name,
				definition: model.FunctionTool{Name: publicName, Description: description, Parameters: append(json.RawMessage(nil), schema...)},
				readOnly:   definition.Annotations.ReadOnlyHint, bindings: bindings,
				registeredID: publicName,
			})
		}
		if response.NextCursor == nil || response.NextCursor == "" {
			return tools, nil
		}
		cursorText, ok := response.NextCursor.(string)
		if !ok || cursorText == "" || cursorText == cursor {
			return nil, fmt.Errorf("MCP server %q returned an invalid pagination cursor", serverName)
		}
		cursor = cursorText
	}
	return nil, fmt.Errorf("MCP server %q returned more than 100 tools/list pages", serverName)
}

func isObjectSchema(raw json.RawMessage) bool {
	var schema map[string]json.RawMessage
	if json.Unmarshal(raw, &schema) != nil || schema == nil {
		return false
	}
	if rawType, exists := schema["type"]; exists {
		var schemaType string
		if json.Unmarshal(rawType, &schemaType) != nil || schemaType != "object" {
			return false
		}
	}
	return true
}

func toolName(serverName, remoteName string) string {
	serverSlug := toolNameRunes.ReplaceAllString(serverName, "_")
	toolSlug := toolNameRunes.ReplaceAllString(remoteName, "_")
	serverSlug = strings.Trim(serverSlug, "_")
	toolSlug = strings.Trim(toolSlug, "_")
	if serverSlug == "" {
		serverSlug = "server"
	}
	if toolSlug == "" {
		toolSlug = "tool"
	}
	hash := sha256.Sum256([]byte(serverName + "\x00" + remoteName))
	suffix := hex.EncodeToString(hash[:4])
	room := 64 - len("mcp__") - 2 - len(suffix)
	serverLength := min(12, len(serverSlug), room-1)
	serverSlug = serverSlug[:serverLength]
	toolLength := min(32, len(toolSlug), room-serverLength-1)
	toolSlug = toolSlug[:toolLength]
	return "mcp__" + serverSlug + "__" + toolSlug + "_" + suffix
}

func (remote *remoteTool) Definition() model.FunctionTool { return remote.definition }

func (remote *remoteTool) Run(ctx context.Context, arguments json.RawMessage) tool.Result {
	if stdio, ok := instanceClient(remote.server).(*stdioTransport); ok && !stdio.Alive() {
		if remote.server.owner == nil {
			return tool.Result{Err: errors.New("MCP stdio server exited and cannot be restarted")}
		}
		if err := remote.server.owner.connectServer(ctx, remote.serverName, false); err != nil {
			remote.server.owner.mu.Lock()
			remote.server.owner.errors[remote.serverName] = err.Error()
			remote.server.owner.mu.Unlock()
			return tool.Result{Err: fmt.Errorf("MCP stdio server exited and restart failed: %w", err)}
		}
		return tool.Result{Err: errors.New("MCP stdio server restarted; retry with the refreshed tool definition")}
	}
	var values map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(arguments)))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil || values == nil {
		return tool.Result{Err: errors.New("MCP tool arguments must be a JSON object")}
	}
	params := map[string]any{"name": remote.remoteName, "arguments": values}
	data, err := remote.server.client.Request(ctx, "tools/call", params, remote.remoteName, remote.bindings)
	if err != nil {
		return tool.Result{Err: err}
	}
	data, err = remote.withMRTR(ctx, "tools/call", params, remote.remoteName, remote.bindings, data)
	if err != nil {
		return tool.Result{Err: err}
	}
	output, transcriptOnly, isError, err := remote.convertToolResult(ctx, data)
	if err != nil {
		return tool.Result{Err: err}
	}
	result := tool.Result{ModelOutput: &output, TranscriptOnly: transcriptOnly}
	if isError {
		result.Err = errors.New("MCP server reported that the tool call failed")
	}
	return result
}
