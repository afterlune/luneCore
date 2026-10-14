package core

import (
	"context"
	"errors"

	"github.com/afterlune/luneCore/internal/mcp"
	"github.com/afterlune/luneCore/internal/model"
	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
)

// ErrMCPServerLifecycleChanged identifies a host MCP request canceled because
// its server was disabled, reconnected, or the Core was closed.
var ErrMCPServerLifecycleChanged = mcp.ErrServerLifecycleChanged

// MCPPromptArgument describes one argument accepted by an MCP prompt.
type MCPPromptArgument struct {
	Name        string
	Description string
	Required    bool
}

// MCPPrompt is metadata for a prompt exposed by an MCP server.
type MCPPrompt struct {
	Name        string
	Title       string
	Description string
	Arguments   []MCPPromptArgument
}

// MCPPromptResult contains the provider-neutral messages returned by a prompt.
type MCPPromptResult struct {
	Description string
	Input       *publicmodel.ResponseInput
}

// MCPResource is metadata for a readable MCP resource.
type MCPResource struct {
	URI         string
	Name        string
	Title       string
	Description string
	MIMEType    string
	Size        int64
}

// MCPResourceTemplate describes a URI pattern exposed by an MCP server.
type MCPResourceTemplate struct {
	URITemplate string
	Name        string
	Title       string
	Description string
	MIMEType    string
}

// ListMCPPrompts fetches all prompts from an enabled MCP server. The host
// chooses whether and when to pass a retrieved prompt to a model.
func (r *SessionRuntime) ListMCPPrompts(ctx context.Context, serverName string) ([]MCPPrompt, error) {
	manager, ctx, err := r.mcpHostContext(ctx)
	if err != nil {
		return nil, err
	}
	prompts, err := manager.ListPrompts(ctx, serverName)
	if err != nil {
		return nil, err
	}
	result := make([]MCPPrompt, 0, len(prompts))
	for _, prompt := range prompts {
		item := MCPPrompt{Name: prompt.Name, Title: prompt.Title, Description: prompt.Description}
		for _, argument := range prompt.Arguments {
			item.Arguments = append(item.Arguments, MCPPromptArgument{Name: argument.Name, Description: argument.Description, Required: argument.Required})
		}
		result = append(result, item)
	}
	return result, nil
}

// GetMCPPrompt retrieves a prompt as provider-neutral conversation input.
// MCP prompts are host-controlled and are never added to a task automatically.
func (r *SessionRuntime) GetMCPPrompt(ctx context.Context, serverName, promptName string, arguments map[string]string) (MCPPromptResult, error) {
	manager, ctx, err := r.mcpHostContext(ctx)
	if err != nil {
		return MCPPromptResult{}, err
	}
	result, err := manager.GetPrompt(ctx, serverName, promptName, arguments)
	if err != nil {
		return MCPPromptResult{}, err
	}
	return MCPPromptResult{Description: result.Description, Input: result.Input}, nil
}

// ListMCPResources fetches all resource metadata from an enabled MCP server.
func (r *SessionRuntime) ListMCPResources(ctx context.Context, serverName string) ([]MCPResource, error) {
	manager, ctx, err := r.mcpHostContext(ctx)
	if err != nil {
		return nil, err
	}
	resources, err := manager.ListResources(ctx, serverName)
	if err != nil {
		return nil, err
	}
	result := make([]MCPResource, 0, len(resources))
	for _, resource := range resources {
		result = append(result, MCPResource{
			URI: resource.URI, Name: resource.Name, Title: resource.Title, Description: resource.Description,
			MIMEType: resource.MIMEType, Size: resource.Size,
		})
	}
	return result, nil
}

// ListMCPResourceTemplates fetches all resource template metadata from an
// enabled MCP server.
func (r *SessionRuntime) ListMCPResourceTemplates(ctx context.Context, serverName string) ([]MCPResourceTemplate, error) {
	manager, ctx, err := r.mcpHostContext(ctx)
	if err != nil {
		return nil, err
	}
	templates, err := manager.ListResourceTemplates(ctx, serverName)
	if err != nil {
		return nil, err
	}
	result := make([]MCPResourceTemplate, 0, len(templates))
	for _, template := range templates {
		result = append(result, MCPResourceTemplate{
			URITemplate: template.URITemplate, Name: template.Name, Title: template.Title,
			Description: template.Description, MIMEType: template.MIMEType,
		})
	}
	return result, nil
}

// ReadMCPResource returns a resource as provider-neutral conversation input.
// The host decides whether to submit it to a task.
func (r *SessionRuntime) ReadMCPResource(ctx context.Context, serverName, uri string) (*publicmodel.ResponseInput, error) {
	manager, ctx, err := r.mcpHostContext(ctx)
	if err != nil {
		return nil, err
	}
	return manager.ReadResource(ctx, serverName, uri)
}

func (r *SessionRuntime) mcpHostContext(ctx context.Context) (*mcp.Manager, context.Context, error) {
	if r == nil || r.core == nil || ctx == nil || r.sessionID == "" {
		return nil, nil, errors.New("session runtime and context are required")
	}
	session, err := r.core.GetSession(ctx, r.sessionID)
	if err != nil {
		return nil, nil, err
	}
	r.core.mu.RLock()
	closed := r.core.closed
	r.core.mu.RUnlock()
	if closed {
		return nil, nil, errors.New("core is closed")
	}
	if r.core.mcpManager == nil {
		return nil, nil, errors.New("MCP manager is unavailable")
	}
	ctx = publictool.WithCallIdentity(ctx, publictool.CallIdentity{SessionID: r.sessionID, AgentID: "host"})
	ctx = model.WithAgentID(ctx, "host")
	profile, configured := r.core.models.Profile(session.Settings.ModelProfile)
	ctx = model.WithInputAudioSupport(ctx, configured && profile.SupportsInputAudio)
	return r.core.mcpManager, ctx, nil
}
