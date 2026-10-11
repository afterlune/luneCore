package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	DeepSeekFlashModel   = "deepseek-flash"
	requestTimeout       = 2 * time.Minute
	maxResponseBodyBytes = 8 * 1024 * 1024
)

var ErrToolCallLimit = errors.New("maximum tool-call rounds reached")

// RespondOptions configures one call to RespondWithOptions.
type RespondOptions struct {
	// MaxToolRounds limits batches of tool calls. Zero means unlimited.
	MaxToolRounds int
}

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
	ToolPermissionWorkspaceRead  ToolPermissionClass = "workspace-read"
	ToolPermissionWorkspaceWrite ToolPermissionClass = "workspace-write"
	ToolPermissionCommand        ToolPermissionClass = "command"
	ToolPermissionNetwork        ToolPermissionClass = "network"
)

// ToolMetadata keeps discovery, permission, and plan-mode classification next
// to the tool registration that owns it. An empty permission class is treated
// as unclassified and must fail closed where the active mode requires it.
type ToolMetadata struct {
	Category        ToolCategory
	PermissionClass ToolPermissionClass
	ReadOnly        bool
}

// Valid reports whether the class is recognized by the permission system.
func (c ToolPermissionClass) Valid() bool {
	switch c {
	case ToolPermissionWorkspaceRead, ToolPermissionWorkspaceWrite, ToolPermissionCommand, ToolPermissionNetwork:
		return true
	default:
		return false
	}
}

// IsReadOnly is true only for explicitly classified read-only tools.
func (m ToolMetadata) IsReadOnly() bool {
	return m.PermissionClass.Valid() && m.ReadOnly
}

// CategorizedToolExecutor optionally exposes tools one category at a time.
// Executors that only implement ToolExecutor retain the original behavior.
type CategorizedToolExecutor interface {
	ToolExecutor
	Categories() []ToolCategory
	DefinitionsForCategory(string) []ResponseTool
}

const (
	CategorySelectorName = "select_tool_category"
	// UserQuestionToolName is the runtime-owned lead-agent clarification tool.
	UserQuestionToolName = "ask_user"
)

type Client struct {
	mu        sync.Mutex
	provider  Provider
	profileID string
	profile   ModelProfile
	tools     ToolExecutor
	history   []InputItem
}

// NewResponseInput encodes any JSON-serializable Responses input value. Pass a
// string for simple text or an item slice for structured and multimodal input.
func NewResponseInput[T any](value T) (*ResponseInput, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode input: %w", err)
	}
	var input ResponseInput
	if err := json.Unmarshal(data, &input); err != nil {
		return nil, fmt.Errorf("decode input: %w", err)
	}
	if input.Text == nil && input.Items == nil {
		return nil, errors.New("input must be a string or an item list")
	}
	return &input, nil
}

// CreateResponse sends one normalized agent request through the configured
// provider without managing history or executing returned tool calls.
func (c *Client) CreateResponse(ctx context.Context, request ModelRequest) (*ModelResponse, error) {
	if c == nil {
		return nil, errors.New("model client is nil")
	}
	if ctx == nil {
		return nil, errors.New("context is nil")
	}
	if c.provider == nil {
		return nil, errors.New("model provider is required")
	}
	if request.Model == "" {
		request.Model = c.profile.Model
	}
	return c.provider.CreateResponse(ctx, request, c.profile)
}

// StreamResponse streams normalized model events and returns the final
// response. A nil event callback discards incremental events.
func (c *Client) StreamResponse(ctx context.Context, request ModelRequest, emit func(ModelStreamEvent) error) (*ModelResponse, error) {
	if c == nil {
		return nil, errors.New("model client is nil")
	}
	if ctx == nil {
		return nil, errors.New("context is nil")
	}
	if c.provider == nil {
		return nil, errors.New("model provider is required")
	}
	if request.Model == "" {
		request.Model = c.profile.Model
	}
	return c.provider.StreamResponse(ctx, request, c.profile, emit)
}

// Respond sends input through the configured provider and runs returned function calls without
// imposing a tool-call round limit. It keeps conversation history on this
// Client, so use one Client per conversation.
func (c *Client) Respond(ctx context.Context, input *ResponseInput) (*Response, error) {
	return c.RespondWithOptions(ctx, input, RespondOptions{})
}

// RespondWithOptions sends input through the configured provider with per-call execution options.
// A positive MaxToolRounds limits batches of tool calls; zero means unlimited.
func (c *Client) RespondWithOptions(ctx context.Context, input *ResponseInput, options RespondOptions) (*Response, error) {
	if c == nil {
		return nil, errors.New("model client is nil")
	}
	if input == nil {
		return nil, errors.New("input is nil")
	}
	if c.tools == nil {
		return nil, errors.New("tool executor is required")
	}
	if options.MaxToolRounds < 0 {
		return nil, errors.New("MaxToolRounds must not be negative")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	newItems, err := initialInputHistory(input)
	if err != nil {
		return nil, err
	}
	history := append(append([]InputItem(nil), c.history...), newItems...)

	categorized, isCategorized := c.tools.(CategorizedToolExecutor)
	var categories []ToolCategory
	if isCategorized {
		categories = categorized.Categories()
	}
	if len(categories) == 0 {
		isCategorized = false
	}
	selectedCategory := ""
	providerName := c.provider.Name()
	modelName := c.profile.Model

	toolRounds := 0
	requestNumber := 0
	for {
		requestNumber++
		requestStarted := time.Now()
		slog.Info("model request started",
			"provider", providerName,
			"model", modelName,
			"request", requestNumber,
			"tool_rounds_completed", toolRounds,
		)
		var visibleTools []ResponseTool
		if isCategorized {
			selector, err := CategorySelectorDefinition(categories)
			if err != nil {
				return nil, fmt.Errorf("build category selector: %w", err)
			}
			visibleTools = []ResponseTool{selector}
			if selectedCategory != "" {
				visibleTools = append(visibleTools, categorized.DefinitionsForCategory(selectedCategory)...)
			}
		} else {
			visibleTools = c.tools.Definitions()
		}
		request := ResponseRequest{
			Model: modelName,
			Input: NewResponseInputItems(history...),
			Tools: visibleTools,
		}
		response, err := c.CreateResponse(ctx, request)
		if err != nil {
			slog.Warn("model request failed",
				"provider", providerName,
				"model", modelName,
				"request", requestNumber,
				"duration", time.Since(requestStarted),
				"canceled", ctx.Err() != nil,
				"error_type", fmt.Sprintf("%T", err),
			)
			if ctx.Err() != nil {
				c.saveInterruptedHistory(history, "The task was interrupted while waiting for the model response.")
				return nil, ctx.Err()
			}
			return nil, err
		}
		calls := functionCalls(response.Output)
		slog.Info("model response received",
			"provider", providerName,
			"model", modelName,
			"request", requestNumber,
			"duration", time.Since(requestStarted),
			"status", response.Status,
			"tool_calls", len(calls),
		)
		if response.Status == "failed" {
			slog.Warn("model response failed", "provider", providerName, "model", modelName, "request", requestNumber)
			if response.Error != nil {
				return response, fmt.Errorf("%s response failed (%s): %s", providerName, response.Error.Code, response.Error.Message)
			}
			return response, fmt.Errorf("%s response failed", providerName)
		}

		if len(calls) == 0 {
			history, err = appendResponseHistory(history, response.Output)
			if err != nil {
				return response, err
			}
			c.history = history
			return response, nil
		}

		// Model APIs are called statelessly, so append the whole assistant response
		// and tool outputs to input and resend that history on the next request.
		history, err = appendResponseHistory(history, response.Output)
		if err != nil {
			return response, err
		}
		if options.MaxToolRounds > 0 && toolRounds >= options.MaxToolRounds {
			slog.Warn("tool round limit reached",
				"limit", options.MaxToolRounds,
				"tool_rounds_completed", toolRounds,
				"skipped_tool_calls", len(calls),
			)
			appendSkippedToolCalls(&history, calls, "Skipped because the tool-call round limit was reached.")
			c.saveInterruptedHistory(history, "The task stopped after reaching its tool-call round limit.")
			return response, fmt.Errorf("%w: %d", ErrToolCallLimit, options.MaxToolRounds)
		}

		visibleToolNames := responseToolNames(visibleTools)
		for i, call := range calls {
			if ctx.Err() != nil {
				slog.Warn("tool batch interrupted",
					"tool_round", toolRounds+1,
					"completed_tool_calls", i,
					"skipped_tool_calls", len(calls)-i,
				)
				appendSkippedToolCalls(&history, calls[i:], "Skipped because the task was interrupted before execution.")
				c.saveInterruptedHistory(history, "The task was interrupted while running tools. Completed tool calls and their results are recorded above.")
				return response, ctx.Err()
			}
			if isCategorized && call.Name == CategorySelectorName {
				category, err := ParseCategorySelection(call.Arguments, categories)
				output := ""
				if err != nil {
					output = "Category was not changed: " + err.Error()
				} else {
					selectedCategory = category
					output = "Category selected. Its tools will be available in the next request."
				}
				history = append(history, InputFunctionCallOutput{
					CallID: call.CallID,
					Output: NewToolOutputText(output),
				})
			} else if _, visible := visibleToolNames[call.Name]; !visible {
				expectedCategory := categoryForTool(categorized, categories, call.Name)
				slog.Warn("tool call is not in the visible tool set",
					"tool", call.Name,
					"selected_category", selectedCategory,
					"visible_tools", responseToolNameList(visibleTools),
					"expected_category", expectedCategory,
				)
				history = append(history, InputFunctionCallOutput{
					CallID: call.CallID,
					Output: NewToolOutputText(unavailableToolMessage(call.Name, expectedCategory, isCategorized)),
				})
			} else {
				history = append(history, c.tools.ExecuteCall(ctx, call))
			}
			if ctx.Err() != nil {
				slog.Warn("tool batch interrupted",
					"tool_round", toolRounds+1,
					"completed_tool_calls", i+1,
					"skipped_tool_calls", len(calls)-i-1,
				)
				appendSkippedToolCalls(&history, calls[i+1:], "Skipped because the task was interrupted before execution.")
				c.saveInterruptedHistory(history, "The task was interrupted while running tools. Completed tool calls and their results are recorded above.")
				return response, ctx.Err()
			}
		}
		toolRounds++
	}
}

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

func appendSkippedToolCalls(history *[]InputItem, calls []OutputFunctionCall, reason string) {
	for _, call := range calls {
		*history = append(*history, InputFunctionCallOutput{
			CallID: call.CallID,
			Output: NewToolOutputText(reason),
		})
	}
}

func (c *Client) saveInterruptedHistory(history []InputItem, message string) {
	history = append(history, InputMessage{
		Role:    RoleAssistant,
		Content: NewMessageText(message),
	})
	c.history = history
}

func newClient(apiKey, endpoint string, httpClient *http.Client, tools ToolExecutor) *Client {
	return &Client{
		provider:  newResponsesProvider(ProviderDeepSeek, apiKey, endpoint, httpClient),
		profileID: DeepSeekFlashModel,
		profile:   ModelProfile{Provider: ProviderDeepSeek, Model: DeepSeekFlashModel},
		tools:     tools,
	}
}

func readAPIKey(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	var config struct {
		APIKey string `json:"apikey"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return "", fmt.Errorf("decode %s: %w", path, err)
	}
	apiKey := strings.TrimSpace(config.APIKey)
	if apiKey == "" {
		return "", fmt.Errorf("%s must contain a non-empty apikey", path)
	}
	return apiKey, nil
}

func initialInputHistory(input *ResponseInput) ([]InputItem, error) {
	if input.Text != nil && input.Items != nil {
		return nil, errors.New("responses input cannot contain both text and items")
	}
	if input.Text != nil {
		return []InputItem{InputMessage{Role: RoleUser, Content: NewMessageText(*input.Text)}}, nil
	}
	if len(input.Items) == 0 {
		return nil, errors.New("input item list is empty")
	}
	return append([]InputItem(nil), input.Items...), nil
}

func appendResponseHistory(history []InputItem, output []ResponseOutputItem) ([]InputItem, error) {
	for _, item := range output {
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("encode response history: %w", err)
		}
		history = append(history, UnknownInputItem{Raw: encoded})
	}
	return history, nil
}

func functionCalls(items []ResponseOutputItem) []OutputFunctionCall {
	calls := make([]OutputFunctionCall, 0)
	for _, item := range items {
		if call, ok := item.(OutputFunctionCall); ok {
			calls = append(calls, call)
		}
	}
	return calls
}
