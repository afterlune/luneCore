package model

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrToolCallLimit = errors.New("maximum tool-call rounds reached")

// RespondOptions configures one call to RespondWithOptions.
type RespondOptions struct {
	// MaxToolRounds limits batches of tool calls. Zero means unlimited.
	MaxToolRounds int
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
	var alwaysVisibleTools []ResponseTool
	if isCategorized {
		categories = categorized.Categories()
		if len(categories) == 0 {
			isCategorized = false
		} else {
			alwaysVisibleTools = PermissionVisibleDefinitions(c.tools, AlwaysVisibleDefinitions(c.tools))
			categories = deferredCategories(c.tools, categorized, categories)
		}
	}
	selectedCategory := ""
	providerName := c.provider.Name()
	modelName := c.profile.Model

	toolRounds := 0
	requestNumber := 0
	for {
		requestNumber++
		requestStarted := time.Now()
		c.logger.Info("model request started",
			"provider", providerName,
			"model", modelName,
			"request", requestNumber,
			"tool_rounds_completed", toolRounds,
		)
		var visibleTools []ResponseTool
		if isCategorized {
			visibleTools = append(visibleTools, alwaysVisibleTools...)
			if len(categories) > 0 {
				selector, err := CategorySelectorDefinition(categories)
				if err != nil {
					return nil, fmt.Errorf("build category selector: %w", err)
				}
				visibleTools = append(visibleTools, selector)
			}
			if selectedCategory != "" {
				deferred := DeferredDefinitions(c.tools, categorized.DefinitionsForCategory(selectedCategory))
				visibleTools = append(visibleTools, PermissionVisibleDefinitions(c.tools, deferred)...)
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
			c.logger.Warn("model request failed",
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
		c.logger.Info("model response received",
			"provider", providerName,
			"model", modelName,
			"request", requestNumber,
			"duration", time.Since(requestStarted),
			"status", response.Status,
			"tool_calls", len(calls),
		)
		if response.Status == "failed" {
			c.logger.Warn("model response failed", "provider", providerName, "model", modelName, "request", requestNumber)
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
			c.logger.Warn("tool round limit reached",
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
				c.logger.Warn("tool batch interrupted",
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
				c.logger.Warn("tool call is not in the visible tool set",
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
				c.logger.Warn("tool batch interrupted",
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

func deferredCategories(executor ToolExecutor, categorized CategorizedToolExecutor, categories []ToolCategory) []ToolCategory {
	deferred := make([]ToolCategory, 0, len(categories))
	for _, category := range categories {
		definitions := DeferredDefinitions(executor, categorized.DefinitionsForCategory(category.Name))
		if len(PermissionVisibleDefinitions(executor, definitions)) > 0 {
			deferred = append(deferred, category)
		}
	}
	return deferred
}

func appendSkippedToolCalls(history *[]InputItem, calls []OutputFunctionCall, reason string) {
	for _, call := range calls {
		*history = append(*history, InputFunctionCallOutput{
			CallID: call.CallID,
			Output: NewToolOutputText(reason),
		})
	}
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
