package agentgraph

import (
	"fmt"

	"github.com/afterlune/luneCore/internal/model"
)

func (a *Agent) visibleTools(state State, category, permissionMode string) ([]model.ResponseTool, []string, error) {
	if permissionMode == "" {
		permissionMode = state.PermissionOverride
	}
	if permissionMode == "" && a.options.CurrentPermissionMode != nil {
		permissionMode = a.options.CurrentPermissionMode()
	}
	addRuntimeTools := func(definitions []model.ResponseTool) []model.ResponseTool {
		if a.options.Team != nil && len(a.options.Team.Profiles()) > 0 && a.options.Team.MaxParallelAgents() > 0 {
			definitions = append(definitions, teamDispatchDefinition(a.options.Team))
		}
		if a.options.AskUser && !state.NoInteractiveDecisions && !state.TeamBranch {
			definitions = append(definitions, askUserDefinition())
		}
		return definitions
	}
	if a.categorized == nil {
		definitions := a.visibleDefinitions(a.tools.Definitions(), permissionMode, state.PlanMode)
		definitions = addRuntimeTools(definitions)
		return definitions, toolNames(definitions), nil
	}
	categories := a.visibleCategories(permissionMode, state.PlanMode)
	if len(categories) == 0 {
		definitions := addRuntimeTools(nil)
		return definitions, toolNames(definitions), nil
	}
	selector, err := model.CategorySelectorDefinition(categories)
	if err != nil {
		return nil, nil, fmt.Errorf("build category selector: %w", err)
	}
	definitions := []model.ResponseTool{selector}
	if category != "" {
		definitions = append(definitions, a.visibleDefinitions(a.categorized.DefinitionsForCategory(category), permissionMode, state.PlanMode)...)
	}
	definitions = addRuntimeTools(definitions)
	return definitions, toolNames(definitions), nil
}

func (a *Agent) visibleCategories(permissionMode string, planMode bool) []model.ToolCategory {
	var categories []model.ToolCategory
	for _, category := range a.categories {
		if len(a.visibleDefinitions(a.categorized.DefinitionsForCategory(category.Name), permissionMode, planMode)) > 0 {
			categories = append(categories, category)
		}
	}
	return categories
}

func (a *Agent) visibleDefinitions(definitions []model.ResponseTool, permissionMode string, planMode bool) []model.ResponseTool {
	visible := make([]model.ResponseTool, 0, len(definitions))
	for _, definition := range definitions {
		function, ok := definition.(model.FunctionTool)
		isVisible := !ok
		if ok && a.options.ToolVisibleForMode == nil && a.options.ToolVisible == nil {
			isVisible = true
		}
		if ok && a.options.ToolVisibleForMode != nil {
			isVisible = a.options.ToolVisibleForMode(function.Name, permissionMode)
		} else if ok && a.options.ToolVisible != nil {
			isVisible = a.options.ToolVisible(function.Name)
		}
		if ok && isVisible && planMode {
			isVisible = a.options.ToolReadOnly != nil && a.options.ToolReadOnly(function.Name)
		}
		if isVisible {
			visible = append(visible, definition)
		}
	}
	return visible
}

func (a *Agent) categoryForTool(name string) string {
	if a.categorized == nil {
		return ""
	}
	for _, category := range a.categories {
		for _, definition := range a.categorized.DefinitionsForCategory(category.Name) {
			if function, ok := definition.(model.FunctionTool); ok && function.Name == name {
				return category.Name
			}
		}
	}
	return ""
}

func unavailableToolMessage(name, category string, categorized bool) string {
	if category != "" {
		return fmt.Sprintf("Tool %q belongs to category %q, which is not visible in this request. Call %s with category %q, then retry the tool in the next request.",
			name, category, model.CategorySelectorName, category)
	}
	if categorized {
		return fmt.Sprintf("Tool %q is not registered in a discoverable category. Call %s to review available tools, then use a listed tool.",
			name, model.CategorySelectorName)
	}
	return fmt.Sprintf("Tool %q is not available in this request. Only call tools listed in the current request.", name)
}

func toolNames(definitions []model.ResponseTool) []string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		if function, ok := definition.(model.FunctionTool); ok {
			names = append(names, function.Name)
		}
	}
	return names
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
