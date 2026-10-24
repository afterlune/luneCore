package tool

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
)

// Registry stores tools by their function name.
type Registry struct {
	mu              sync.RWMutex
	logger          *slog.Logger
	tools           map[string]Tool
	toolMetadata    map[string]model.ToolMetadata
	definitions     map[string]model.FunctionTool
	definitionOrder []string
	categoryTools   map[string][]string
	categories      map[string]model.ToolCategory
	categoryOrder   []string
	uncategorized   int
	eventObserver   ToolEventObserver
	callController  ToolCallController
	permissions     *permissionSettings
	workspaceRoot   *workspaceIdentity
}

// ToolEvent describes a tool call. Arguments are transient so clients can build
// a presentation appropriate to their interface; do not log or persist them.
type ToolEvent struct {
	Details      *publictool.ExecutionDetails `json:"-"`
	CallID       string
	InvocationID string
	SessionID    string
	TaskID       string
	RunID        string
	ExecutionID  string
	AgentID      string
	Name         string
	Arguments    json.RawMessage `json:"-"`
	Started      bool
	Duration     time.Duration
	Failed       bool
	Canceled     bool
	Recovered    bool
}

// ToolEventObserver receives tool lifecycle events. Implementations should
// return promptly because tool execution calls the observer synchronously.
type ToolEventObserver func(ToolEvent)

type ToolCallExecution func() model.InputFunctionCallOutput

type ToolCallController func(context.Context, model.OutputFunctionCall, ToolCallExecution) (model.InputFunctionCallOutput, error)

var _ model.ToolExecutor = (*Registry)(nil)
var _ model.CategorizedToolExecutor = (*Registry)(nil)

// ToolRegistration associates a tool with its discovery and safety metadata.
type ToolRegistration struct {
	Tool     Tool
	Metadata model.ToolMetadata
}

// PathAccessTool declares the directory scope a filesystem operation will
// access. The registry authorizes it before Run and passes the granted root in
// the call context.
type PathAccessTool interface {
	PathAccessRequest(context.Context, json.RawMessage) (*publictool.PathGrant, error)
}

// RegistryOptions supplies host extensions and command execution policy to the
// default workspace registry.
type RegistryOptions struct {
	Tools                 []ToolRegistration
	CommandExecutor       publictool.CommandExecutor
	SandboxHelperPath     string
	CommandToolchainPaths []string
	AllowLocalCommands    bool
	ImageMaxBytes         int64
	Logger                *slog.Logger
}

func NewRegistry(registrations ...ToolRegistration) (*Registry, error) {
	return newRegistry(nil, registrations...)
}

func newRegistry(logger *slog.Logger, registrations ...ToolRegistration) (*Registry, error) {
	if logger == nil {
		logger = slog.Default()
	}
	registry := &Registry{
		logger:        logger,
		tools:         make(map[string]Tool, len(registrations)),
		toolMetadata:  make(map[string]model.ToolMetadata, len(registrations)),
		definitions:   make(map[string]model.FunctionTool, len(registrations)),
		categoryTools: make(map[string][]string),
		permissions:   newPermissionSettings(PermissionModeReadOnly),
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

func (r *Registry) SetToolCallController(controller ToolCallController) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.callController = controller
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

func (r *Registry) SetPermissionApprovalDecisionHandler(handler PermissionApprovalDecisionHandler) {
	if r == nil {
		return
	}
	r.mu.RLock()
	permissions := r.permissions
	r.mu.RUnlock()
	permissions.setApprovalDecisionHandler(handler)
}

// SetPermissionAutoReviewHandler configures the evaluator for auto mode review.
func (r *Registry) SetPermissionAutoReviewHandler(handler PermissionAutoReviewHandler) {
	if r == nil {
		return
	}
	r.mu.RLock()
	permissions := r.permissions
	r.mu.RUnlock()
	permissions.setAutoReviewHandler(handler)
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
	definition := cloneFunctionTool(registeredTool.Definition())
	if definition.Name == "" {
		return errors.New("tool name is empty")
	}
	if definition.Name == model.UserQuestionToolName || (category.Name != "" && definition.Name == model.CategorySelectorName) {
		return errors.New("tool name is reserved: " + definition.Name)
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
	if r.definitions == nil {
		r.definitions = make(map[string]model.FunctionTool)
	}
	if r.categoryTools == nil {
		r.categoryTools = make(map[string][]string)
	}
	if _, exists := r.tools[definition.Name]; exists {
		return errors.New("tool is already registered: " + definition.Name)
	}
	r.tools[definition.Name] = registeredTool
	r.toolMetadata[definition.Name] = registration.Metadata
	r.definitions[definition.Name] = definition
	index := sort.SearchStrings(r.definitionOrder, definition.Name)
	r.definitionOrder = append(r.definitionOrder, "")
	copy(r.definitionOrder[index+1:], r.definitionOrder[index:])
	r.definitionOrder[index] = definition.Name
	if category.Name != "" {
		if r.categories == nil {
			r.categories = make(map[string]model.ToolCategory)
		}
		if _, exists := r.categories[category.Name]; !exists {
			r.categories[category.Name] = category
			r.categoryOrder = append(r.categoryOrder, category.Name)
		}
		r.categoryTools[category.Name] = append(r.categoryTools[category.Name], definition.Name)
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
	delete(r.definitions, name)
	for index, existing := range r.definitionOrder {
		if existing == name {
			r.definitionOrder = append(r.definitionOrder[:index], r.definitionOrder[index+1:]...)
			break
		}
	}
	if categoryName != "" {
		for index, existing := range r.categoryTools[categoryName] {
			if existing == name {
				r.categoryTools[categoryName] = append(r.categoryTools[categoryName][:index], r.categoryTools[categoryName][index+1:]...)
				break
			}
		}
		for _, metadata := range r.toolMetadata {
			if metadata.Category.Name == categoryName {
				return true
			}
		}
		delete(r.categories, categoryName)
		delete(r.categoryTools, categoryName)
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

// ToolVisible reports whether the active permission mode allows the tool
// directly or permits it after approval.
func (r *Registry) ToolVisible(name string) bool {
	return r.ToolVisibleForMode(name, "")
}

// ToolVisibleForMode reports whether a tool is available under the supplied
// task-scoped mode. An empty mode uses the registry's active mode.
func (r *Registry) ToolVisibleForMode(name, mode string) bool {
	metadata, ok := r.ToolMetadata(name)
	if !ok {
		return false
	}
	r.mu.RLock()
	permissions := r.permissions
	r.mu.RUnlock()
	var disposition permissionDisposition
	if mode == "" {
		_, _, _, _, disposition = permissions.policy(metadata)
	} else {
		disposition = permissionPolicy(PermissionMode(mode), metadata.PermissionClass)
	}
	return disposition != permissionDenied
}

// Definitions returns function-tool definitions suitable for ResponseRequest.Tools.
func (r *Registry) Definitions() []model.ResponseTool {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	definitions := make([]model.ResponseTool, 0, len(r.definitionOrder))
	for _, name := range r.definitionOrder {
		definitions = append(definitions, cloneFunctionTool(r.definitions[name]))
	}
	r.mu.RUnlock()
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
	names := r.categoryTools[category]
	definitions := make([]model.ResponseTool, 0, len(names))
	for _, name := range names {
		definitions = append(definitions, cloneFunctionTool(r.definitions[name]))
	}
	r.mu.RUnlock()
	return definitions
}

func cloneFunctionTool(definition model.FunctionTool) model.FunctionTool {
	definition.Parameters = append(json.RawMessage(nil), definition.Parameters...)
	if definition.Strict != nil {
		strict := *definition.Strict
		definition.Strict = &strict
	}
	return definition
}
