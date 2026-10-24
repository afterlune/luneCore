package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ToolExecutor is implemented by the tool registry. Keeping this interface in
// model avoids importing the tool package and creating an import cycle.
type ToolExecutor interface {
	Definitions() []ResponseTool
	ExecuteCall(context.Context, OutputFunctionCall) InputFunctionCallOutput
	ToolMetadata(string) (ToolMetadata, bool)
}

// ToolCategory describes a group of tools that can be revealed on demand.
type ToolCategory struct {
	Name        string
	Description string
}

// ToolPermissionClass describes the effect boundary used by permission modes.
type ToolPermissionClass string

const (
	ToolPermissionSessionControl ToolPermissionClass = "session-control"
	ToolPermissionWorkspaceRead  ToolPermissionClass = "workspace-read"
	ToolPermissionWorkspaceWrite ToolPermissionClass = "workspace-write"
	ToolPermissionCommand        ToolPermissionClass = "command"
	ToolPermissionNetwork        ToolPermissionClass = "network"
)

// ToolMetadata keeps discovery, side-effect, and permission hints next to the
// tool registration that owns them. AlwaysVisible controls initial exposure,
// not authorization. An empty permission class is treated as unclassified and
// must fail closed where the active mode requires it.
type ToolMetadata struct {
	Category        ToolCategory
	PermissionClass ToolPermissionClass
	ReadOnly        bool // Side-effect hint; permission decisions use PermissionClass.
	AlwaysVisible   bool
}

// ToolAlwaysVisible reports whether a registered function is exposed before
// category discovery. Permission checks remain the executor's responsibility.
func ToolAlwaysVisible(executor ToolExecutor, definition ResponseTool) bool {
	function, ok := definition.(FunctionTool)
	if !ok || executor == nil {
		return false
	}
	metadata, exists := executor.ToolMetadata(function.Name)
	return exists && metadata.AlwaysVisible
}

// AlwaysVisibleDefinitions returns registered tools that bypass category
// discovery while preserving their registered order.
func AlwaysVisibleDefinitions(executor ToolExecutor) []ResponseTool {
	if executor == nil {
		return nil
	}
	definitions := executor.Definitions()
	visible := make([]ResponseTool, 0, len(definitions))
	for _, definition := range definitions {
		if ToolAlwaysVisible(executor, definition) {
			visible = append(visible, definition)
		}
	}
	return visible
}

// DeferredDefinitions filters out tools exposed before category discovery.
func DeferredDefinitions(executor ToolExecutor, definitions []ResponseTool) []ResponseTool {
	deferred := make([]ResponseTool, 0, len(definitions))
	for _, definition := range definitions {
		if !ToolAlwaysVisible(executor, definition) {
			deferred = append(deferred, definition)
		}
	}
	return deferred
}

// Valid reports whether the class is recognized by the permission system.
func (c ToolPermissionClass) Valid() bool {
	switch c {
	case ToolPermissionSessionControl, ToolPermissionWorkspaceRead, ToolPermissionWorkspaceWrite, ToolPermissionCommand, ToolPermissionNetwork:
		return true
	default:
		return false
	}
}

// CategorizedToolExecutor optionally exposes tools one category at a time.
// Executors that only implement ToolExecutor retain the original behavior.
type CategorizedToolExecutor interface {
	ToolExecutor
	Categories() []ToolCategory
	DefinitionsForCategory(string) []ResponseTool
}

// ToolVisibilityExecutor optionally lets an executor filter model-visible
// definitions using its active permission mode.
type ToolVisibilityExecutor interface {
	ToolVisible(string) bool
}

// PermissionVisibleDefinitions applies an executor's active permission-mode
// visibility policy when it provides one.
func PermissionVisibleDefinitions(executor ToolExecutor, definitions []ResponseTool) []ResponseTool {
	visibility, ok := executor.(ToolVisibilityExecutor)
	if !ok {
		return definitions
	}
	visible := make([]ResponseTool, 0, len(definitions))
	for _, definition := range definitions {
		function, ok := definition.(FunctionTool)
		if !ok || visibility.ToolVisible(function.Name) {
			visible = append(visible, definition)
		}
	}
	return visible
}

const (
	CategorySelectorName = "select_tool_category"
	// UserQuestionToolName is the runtime-owned lead-agent clarification tool.
	UserQuestionToolName = "ask_user"
)

// CategorySelectorDefinition builds the virtual tool used to reveal one
// category at a time.
func CategorySelectorDefinition(categories []ToolCategory) (FunctionTool, error) {
	names := make([]string, 0, len(categories))
	descriptions := make([]string, 0, len(categories))
	for _, category := range categories {
		names = append(names, category.Name)
		descriptions = append(descriptions, category.Name+": "+category.Description)
	}
	parameters, err := json.Marshal(struct {
		Type                 string   `json:"type"`
		Properties           any      `json:"properties"`
		Required             []string `json:"required"`
		AdditionalProperties bool     `json:"additionalProperties"`
	}{
		Type: "object",
		Properties: map[string]any{
			"category": map[string]any{"type": "string", "enum": names},
		},
		Required:             []string{"category"},
		AdditionalProperties: false,
	})
	if err != nil {
		return FunctionTool{}, err
	}
	strict := true
	return FunctionTool{
		Name:        CategorySelectorName,
		Description: "Choose a tool category to reveal its tools. Available categories: " + strings.Join(descriptions, "; "),
		Parameters:  parameters,
		Strict:      &strict,
	}, nil
}

// ParseCategorySelection validates the arguments returned for the virtual
// category selector.
func ParseCategorySelection(arguments string, categories []ToolCategory) (string, error) {
	var input struct {
		Category string `json:"category"`
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return "", fmt.Errorf("invalid category selection: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", errors.New("invalid category selection: expected one JSON object")
	}
	for _, category := range categories {
		if input.Category == category.Name {
			return input.Category, nil
		}
	}
	return "", fmt.Errorf("unknown category %q", input.Category)
}

func responseToolNames(tools []ResponseTool) map[string]struct{} {
	names := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if function, ok := tool.(FunctionTool); ok {
			names[function.Name] = struct{}{}
		}
	}
	return names
}

func responseToolNameList(tools []ResponseTool) []string {
	names := make([]string, 0, len(tools))
	for _, definition := range tools {
		if function, ok := definition.(FunctionTool); ok {
			names = append(names, function.Name)
		}
	}
	return names
}

func categoryForTool(categorized CategorizedToolExecutor, categories []ToolCategory, name string) string {
	if categorized == nil {
		return ""
	}
	for _, category := range categories {
		for _, definition := range categorized.DefinitionsForCategory(category.Name) {
			if function, ok := definition.(FunctionTool); ok && function.Name == name {
				return category.Name
			}
		}
	}
	return ""
}

func unavailableToolMessage(name, category string, categorized bool) string {
	if category != "" {
		return fmt.Sprintf("Tool %q belongs to category %q, which is not visible in this request. Call %s with category %q, then retry the tool in the next request.",
			name, category, CategorySelectorName, category)
	}
	if categorized {
		return fmt.Sprintf("Tool %q is not registered in a discoverable category. Call %s to review available tools, then use a listed tool.",
			name, CategorySelectorName)
	}
	return fmt.Sprintf("Tool %q is not available in this request. Only call tools listed in the current request.", name)
}
