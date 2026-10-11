package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/afterlune/luneCore/internal/model"
)

// Registry stores tools by their function name.
type Registry struct {
	mu            sync.RWMutex
	tools         map[string]Tool
	toolMetadata  map[string]model.ToolMetadata
	toolOrder     []string
	categories    map[string]model.ToolCategory
	categoryOrder []string
	uncategorized int
	eventObserver ToolEventObserver
	permissions   *permissionSettings
	workspaceRoot *workspaceIdentity
}

// ToolEvent describes a tool call. Arguments are transient so clients can build
// a presentation appropriate to their interface; do not log or persist them.
type ToolEvent struct {
	CallID    string
	AgentID   string
	Name      string
	Arguments json.RawMessage `json:"-"`
	Started   bool
	Duration  time.Duration
	Failed    bool
	Canceled  bool
}

// ToolEventObserver receives tool lifecycle events. Implementations should
// return promptly because tool execution calls the observer synchronously.
type ToolEventObserver func(ToolEvent)

var _ model.ToolExecutor = (*Registry)(nil)
var _ model.CategorizedToolExecutor = (*Registry)(nil)

// ToolRegistration associates a tool with its discovery and safety metadata.
type ToolRegistration struct {
	Tool     Tool
	Metadata model.ToolMetadata
}

func NewRegistry(registrations ...ToolRegistration) (*Registry, error) {
	registry := &Registry{
		tools:        make(map[string]Tool, len(registrations)),
		toolMetadata: make(map[string]model.ToolMetadata, len(registrations)),
		permissions:  newPermissionSettings(PermissionModeReadOnly),
	}
	for _, registration := range registrations {
		if err := registry.Register(registration); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

// SetToolEventObserver installs an observer for tool start and finish events.
func (r *Registry) SetToolEventObserver(observer ToolEventObserver) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.eventObserver = observer
	r.mu.Unlock()
}

// Close releases resources retained by the registry. Registries returned by
// NewDefaultRegistry keep the workspace directory pinned until Close.
func (r *Registry) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	workspaceRoot := r.workspaceRoot
	r.workspaceRoot = nil
	r.mu.Unlock()
	return workspaceRoot.closeRoot()
}

func (r *Registry) PermissionMode() PermissionMode {
	if r == nil {
		return PermissionModeFullAccess
	}
	r.mu.RLock()
	permissions := r.permissions
	r.mu.RUnlock()
	return permissions.modeValue()
}

func (r *Registry) SetPermissionMode(mode PermissionMode) error {
	if r == nil {
		return errors.New("tool registry is nil")
	}
	r.mu.RLock()
	permissions := r.permissions
	r.mu.RUnlock()
	if permissions == nil {
		return errors.New("permission settings are unavailable")
	}
	return permissions.setMode(mode)
}

func (r *Registry) SetPermissionApprovalHandler(handler PermissionApprovalHandler) {
	if r == nil {
		return
	}
	r.mu.RLock()
	permissions := r.permissions
	r.mu.RUnlock()
	permissions.setApprovalHandler(handler)
}

// Register adds a tool and its discovery and safety metadata.
func (r *Registry) Register(registration ToolRegistration) error {
	category := registration.Metadata.Category
	registeredTool := registration.Tool
	if category.Name == "" && category.Description != "" {
		return errors.New("tool category description requires a category name")
	}
	if category.Name != "" && category.Name != strings.TrimSpace(category.Name) {
		return errors.New("tool category name must not be padded with whitespace")
	}
	if category.Name != "" && strings.TrimSpace(category.Description) == "" {
		return errors.New("tool category description must not be empty")
	}
	if registeredTool == nil {
		return errors.New("cannot register a nil tool")
	}
	definition := registeredTool.Definition()
	if definition.Name == "" {
		return errors.New("tool name is empty")
	}
	if category.Name != "" && definition.Name == model.CategorySelectorName {
		return errors.New("tool name is reserved: " + model.CategorySelectorName)
	}
	if len(definition.Parameters) == 0 {
		return errors.New("tool parameters schema is empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if category.Name == "" && len(r.categories) > 0 {
		return errors.New("cannot add an uncategorized tool to a categorized registry")
	}
	if category.Name != "" && r.uncategorized > 0 {
		return errors.New("cannot categorize tools in a registry with uncategorized tools")
	}
	if category.Name != "" {
		if existing, exists := r.categories[category.Name]; exists && existing.Description != category.Description {
			return errors.New("tool category description does not match existing category: " + category.Name)
		}
	}
	if r.tools == nil {
		r.tools = make(map[string]Tool)
	}
	if r.toolMetadata == nil {
		r.toolMetadata = make(map[string]model.ToolMetadata)
	}
	if _, exists := r.tools[definition.Name]; exists {
		return errors.New("tool is already registered: " + definition.Name)
	}
	r.tools[definition.Name] = registeredTool
	r.toolMetadata[definition.Name] = registration.Metadata
	r.toolOrder = append(r.toolOrder, definition.Name)
	if category.Name != "" {
		if r.categories == nil {
			r.categories = make(map[string]model.ToolCategory)
		}
		if _, exists := r.categories[category.Name]; !exists {
			r.categories[category.Name] = category
			r.categoryOrder = append(r.categoryOrder, category.Name)
		}
	} else {
		r.uncategorized++
	}
	return nil
}

// Unregister removes a dynamically managed tool and drops its category when
// the category no longer contains any registered tools.
func (r *Registry) Unregister(name string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[name]; !exists {
		return false
	}
	categoryName := r.toolMetadata[name].Category.Name
	if categoryName == "" && r.uncategorized > 0 {
		r.uncategorized--
	}
	delete(r.tools, name)
	delete(r.toolMetadata, name)
	for index, existing := range r.toolOrder {
		if existing == name {
			r.toolOrder = append(r.toolOrder[:index], r.toolOrder[index+1:]...)
			break
		}
	}
	if categoryName != "" {
		for _, metadata := range r.toolMetadata {
			if metadata.Category.Name == categoryName {
				return true
			}
		}
		delete(r.categories, categoryName)
		for index, existing := range r.categoryOrder {
			if existing == categoryName {
				r.categoryOrder = append(r.categoryOrder[:index], r.categoryOrder[index+1:]...)
				break
			}
		}
	}
	return true
}

// ToolMetadata returns the registration metadata for a tool.
func (r *Registry) ToolMetadata(name string) (model.ToolMetadata, bool) {
	if r == nil {
		return model.ToolMetadata{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	metadata, ok := r.toolMetadata[name]
	return metadata, ok
}

// Definitions returns function-tool definitions suitable for ResponseRequest.Tools.
func (r *Registry) Definitions() []model.ResponseTool {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	registeredTools := make(map[string]Tool, len(r.tools))
	for name, registeredTool := range r.tools {
		registeredTools[name] = registeredTool
	}
	r.mu.RUnlock()

	names := make([]string, 0, len(registeredTools))
	for name := range registeredTools {
		names = append(names, name)
	}
	sort.Strings(names)
	definitions := make([]model.ResponseTool, 0, len(names))
	for _, name := range names {
		definitions = append(definitions, registeredTools[name].Definition())
	}
	return definitions
}

// Categories returns categories in registration order.
func (r *Registry) Categories() []model.ToolCategory {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	categories := make([]model.ToolCategory, 0, len(r.categoryOrder))
	for _, name := range r.categoryOrder {
		categories = append(categories, r.categories[name])
	}
	return categories
}

// DefinitionsForCategory returns function definitions in registration order.
func (r *Registry) DefinitionsForCategory(category string) []model.ResponseTool {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	registeredTools := make([]Tool, 0)
	for _, name := range r.toolOrder {
		if r.toolMetadata[name].Category.Name == category {
			registeredTools = append(registeredTools, r.tools[name])
		}
	}
	r.mu.RUnlock()
	definitions := make([]model.ResponseTool, 0, len(registeredTools))
	for _, registeredTool := range registeredTools {
		definitions = append(definitions, registeredTool.Definition())
	}
	return definitions
}

// Run executes a named tool with its JSON arguments.
func (r *Registry) Run(ctx context.Context, name string, arguments json.RawMessage) Result {
	registeredTool, err := r.authorizedTool(ctx, name, arguments)
	if err != nil {
		return Result{Err: err}
	}
	return registeredTool.Run(ctx, arguments)
}

func (r *Registry) authorizedTool(ctx context.Context, name string, arguments json.RawMessage) (Tool, error) {
	if r == nil {
		return nil, errors.New("tool registry is nil")
	}
	r.mu.RLock()
	registeredTool, exists := r.tools[name]
	metadata := r.toolMetadata[name]
	permissions := r.permissions
	r.mu.RUnlock()
	if !exists {
		return nil, errors.New("unknown tool: " + name)
	}
	if err := permissions.authorize(ctx, PermissionRequest{
		Tool: name, AgentID: model.AgentIDFromContext(ctx), Arguments: append(json.RawMessage(nil), arguments...), Metadata: metadata,
	}); err != nil {
		return nil, err
	}
	return registeredTool, nil
}

// ExecuteCall runs a function call from a Responses API result and returns the
// matching function_call_output input item for the next request.
func (r *Registry) ExecuteCall(ctx context.Context, call model.OutputFunctionCall) model.InputFunctionCallOutput {
	started := time.Now()
	arguments := json.RawMessage(call.Arguments)
	registeredTool, permissionErr := r.authorizedTool(ctx, call.Name, arguments)
	var result Result
	if permissionErr != nil {
		result = Result{Err: permissionErr}
	} else {
		slog.Info("tool execution started", "tool", call.Name, "call_id", call.CallID)
		r.observeToolEvent(ToolEvent{CallID: call.CallID, AgentID: model.AgentIDFromContext(ctx), Name: call.Name, Arguments: append(json.RawMessage(nil), arguments...), Started: true})
		result = registeredTool.Run(ctx, arguments)
	}
	duration := time.Since(started)
	attributes := []any{
		"tool", call.Name,
		"call_id", call.CallID,
		"duration", duration,
		"failed", result.Err != nil,
		"canceled", errors.Is(result.Err, context.Canceled) || errors.Is(result.Err, context.DeadlineExceeded),
	}
	if result.Err != nil {
		slog.Warn("tool execution finished", attributes...)
	} else {
		slog.Info("tool execution finished", attributes...)
	}
	r.observeToolEvent(ToolEvent{
		CallID: call.CallID, AgentID: model.AgentIDFromContext(ctx), Name: call.Name, Arguments: append(json.RawMessage(nil), arguments...), Duration: duration,
		Failed:   result.Err != nil,
		Canceled: errors.Is(result.Err, context.Canceled) || errors.Is(result.Err, context.DeadlineExceeded),
	})
	toolOutput := model.NewToolOutputText(result.Text())
	if result.ModelOutput != nil {
		toolOutput = *result.ModelOutput
		if result.Err != nil {
			if toolOutput.Text != nil {
				toolOutput = model.NewToolOutputParts(model.InputText{Text: *toolOutput.Text}, model.InputText{Text: "Error: " + result.Err.Error()})
			} else {
				toolOutput.Parts = append(toolOutput.Parts, model.InputText{Text: "Error: " + result.Err.Error()})
			}
		}
	}
	return model.InputFunctionCallOutput{
		CallID:         call.CallID,
		Output:         toolOutput,
		TranscriptOnly: append([]model.ContentPart(nil), result.TranscriptOnly...),
	}
}

func (r *Registry) observeToolEvent(event ToolEvent) {
	if r == nil {
		return
	}
	r.mu.RLock()
	observer := r.eventObserver
	r.mu.RUnlock()
	if observer == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Warn("tool event observer panicked", "panic_type", fmt.Sprintf("%T", recovered))
		}
	}()
	observer(event)
}

// NewDefaultRegistry creates the built-in workspace and web tools. Call Close
// when the registry is no longer in use to release its pinned workspace root.
func NewDefaultRegistry(workspace string) (*Registry, error) {
	rootHandle, err := openWorkspaceRoot(workspace)
	if err != nil {
		return nil, err
	}
	workspacePath := rootHandle.Name()
	workspaceID := &workspaceIdentity{root: rootHandle}
	filesCategory := model.ToolCategory{Name: "files", Description: "Explore, read, write, and edit workspace files."}
	commandCategory := model.ToolCategory{Name: "command", Description: "Run a command in the workspace."}
	webCategory := model.ToolCategory{Name: "web", Description: "Search the web and fetch web pages."}
	workspaceRead := model.ToolMetadata{Category: filesCategory, PermissionClass: model.ToolPermissionWorkspaceRead, ReadOnly: true}
	workspaceWrite := model.ToolMetadata{Category: filesCategory, PermissionClass: model.ToolPermissionWorkspaceWrite}
	command := model.ToolMetadata{Category: commandCategory, PermissionClass: model.ToolPermissionCommand}
	network := model.ToolMetadata{Category: webCategory, PermissionClass: model.ToolPermissionNetwork, ReadOnly: true}
	registry, err := NewRegistry(
		ToolRegistration{Tool: ListDirTool{Workspace: workspacePath, workspace: workspaceID}, Metadata: workspaceRead},
		ToolRegistration{Tool: GlobTool{Workspace: workspacePath, workspace: workspaceID}, Metadata: workspaceRead},
		ToolRegistration{Tool: GrepTool{Workspace: workspacePath, workspace: workspaceID}, Metadata: workspaceRead},
		ToolRegistration{Tool: ReadFileTool{Workspace: workspacePath, workspace: workspaceID}, Metadata: workspaceRead},
		ToolRegistration{Tool: WriteFileTool{Workspace: workspacePath, workspace: workspaceID}, Metadata: workspaceWrite},
		ToolRegistration{Tool: EditFileTool{Workspace: workspacePath, workspace: workspaceID}, Metadata: workspaceWrite},
		ToolRegistration{Tool: CommandTool{Workspace: workspacePath}, Metadata: command},
		ToolRegistration{Tool: WebSearchTool{httpClient: defaultWebHTTPClient}, Metadata: network},
		ToolRegistration{Tool: WebFetchTool{httpClient: defaultWebHTTPClient}, Metadata: network},
	)
	if err != nil {
		_ = workspaceID.closeRoot()
		return nil, err
	}
	registry.workspaceRoot = workspaceID
	if err := registry.SetPermissionMode(PermissionModeWorkspace); err != nil {
		_ = registry.Close()
		return nil, err
	}
	return registry, nil
}
