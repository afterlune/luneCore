package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/afterlune/luneCore/internal/model"
)

const maxMRTRRounds = 5

// OpenElicitationURL opens a server-provided URL only after the front end has
// obtained explicit confirmation. The URL is used as an exec argument, never
// interpolated into a shell command.
func (manager *Manager) OpenElicitationURL(serverName, target string) error {
	manager.mu.Lock()
	instance := manager.instances[serverName]
	enabled := manager.enabled[serverName]
	manager.mu.Unlock()
	if !enabled || instance == nil {
		return fmt.Errorf("MCP server %q is not enabled", serverName)
	}
	if strings.TrimSpace(target) != target || strings.ContainsAny(target, "\r\n\t ") {
		return errors.New("MCP elicitation URL contains whitespace")
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.Host == "" || parsed.User != nil || !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return errors.New("MCP elicitation URL must be an absolute http or https URL without userinfo")
	}
	return openOAuthBrowser(target)
}

func (remote *remoteTool) withMRTR(ctx context.Context, method string, originalParams map[string]any, name string, bindings []headerBinding, first json.RawMessage) (json.RawMessage, error) {
	return withMRTR(ctx, remote.server, remote.serverName, method, originalParams, name, bindings, first)
}

func withMRTR(ctx context.Context, server *serverInstance, serverName, method string, originalParams map[string]any, name string, bindings []headerBinding, first json.RawMessage) (json.RawMessage, error) {
	response := first
	params := cloneMap(originalParams)
	for round := 0; round <= maxMRTRRounds; round++ {
		var result struct {
			ResultType    string                     `json:"resultType"`
			InputRequests map[string]mcpInputRequest `json:"inputRequests"`
			RequestState  *string                    `json:"requestState"`
		}
		if err := json.Unmarshal(response, &result); err != nil {
			return nil, err
		}
		if result.ResultType != "input_required" {
			return response, nil
		}
		if round == maxMRTRRounds {
			return nil, fmt.Errorf("MCP server %q exceeded the %d-round input limit", serverName, maxMRTRRounds)
		}
		if len(result.InputRequests) == 0 {
			return nil, errors.New("MCP server requested input without any input requests")
		}
		responses := make(map[string]json.RawMessage, len(result.InputRequests))
		keys := make([]string, 0, len(result.InputRequests))
		for key := range result.InputRequests {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			request := result.InputRequests[key]
			if request.Method != "elicitation/create" {
				return nil, fmt.Errorf("MCP server %q requested unsupported interaction %q", serverName, request.Method)
			}
			if serverName == "" || server == nil {
				return nil, errors.New("MCP server interaction is unavailable")
			}
			payload, err := json.Marshal(request)
			if err != nil {
				return nil, err
			}
			answer, err := withInteraction(ctx, server, serverName, payload)
			if err != nil {
				return nil, err
			}
			if err := validateElicitationResponse(answer, request.Params); err != nil {
				return nil, err
			}
			responses[key] = answer
		}
		params["inputResponses"] = responses
		if result.RequestState != nil {
			params["requestState"] = *result.RequestState
		}
		var err error
		response, err = server.client.Request(ctx, method, cloneMap(params), name, bindings)
		if err != nil {
			return nil, err
		}
	}
	return nil, errors.New("MCP input retry limit reached")
}

type mcpInputRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func validateElicitationResponse(raw, requestParams json.RawMessage) error {
	var response struct {
		Action  string          `json:"action"`
		Content json.RawMessage `json:"content"`
	}
	var request struct {
		Mode string `json:"mode"`
	}
	if len(raw) == 0 || !json.Valid(raw) {
		return errors.New("MCP elicitation response is invalid JSON")
	}
	if len(requestParams) == 0 || json.Unmarshal(requestParams, &request) != nil || request.Mode != "form" && request.Mode != "url" {
		return errors.New("MCP elicitation request has an unsupported mode")
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return errors.New("MCP elicitation response is invalid JSON")
	}
	switch response.Action {
	case "accept":
		if request.Mode == "form" {
			var content map[string]json.RawMessage
			if len(response.Content) == 0 || json.Unmarshal(response.Content, &content) != nil || content == nil {
				return errors.New("accepted MCP form response requires a JSON object")
			}
		} else if len(response.Content) > 0 {
			var content map[string]json.RawMessage
			if json.Unmarshal(response.Content, &content) != nil || content == nil {
				return errors.New("accepted MCP URL response content must be a JSON object")
			}
		}
	case "decline", "cancel":
	default:
		return errors.New("MCP elicitation response action must be accept, decline, or cancel")
	}
	return nil
}

func cloneMap(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func (remote *remoteTool) withInteraction(ctx context.Context, request json.RawMessage) (json.RawMessage, error) {
	return withInteraction(ctx, remote.server, remote.serverName, request)
}

func withInteraction(ctx context.Context, server *serverInstance, serverName string, request json.RawMessage) (json.RawMessage, error) {
	if server == nil || server.owner == nil || server.owner.interact == nil {
		return nil, errors.New("MCP user interaction is unavailable")
	}
	return server.owner.interact(ctx, model.AgentIDFromContext(ctx), serverName, request)
}
