package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
)

// Run executes a named tool with its JSON arguments.
func (r *Registry) Run(ctx context.Context, name string, arguments json.RawMessage) Result {
	ctx = r.contextWithPermissionMode(ctx, name)
	registeredTool, ctx, err := r.authorizedTool(ctx, name, arguments)
	if err != nil {
		return Result{Err: err}
	}
	return registeredTool.Run(ctx, arguments)
}

func (r *Registry) authorizedTool(ctx context.Context, name string, arguments json.RawMessage) (Tool, context.Context, error) {
	if r == nil {
		return nil, ctx, errors.New("tool registry is nil")
	}
	r.mu.RLock()
	registeredTool, exists := r.tools[name]
	metadata := r.toolMetadata[name]
	permissions := r.permissions
	r.mu.RUnlock()
	if !exists {
		return nil, ctx, errors.New("unknown tool: " + name)
	}
	if name == "command" {
		var commandRequest struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(arguments, &commandRequest) == nil {
			if reason := extremeDangerCommandReason(commandRequest.Command); reason != "" {
				return nil, ctx, &publictool.ExecutionBlockerError{Tool: name, Reason: "command is permanently blocked as extremely dangerous: " + reason}
			}
		}
	}
	if control, ok := registeredTool.(CommandControlTool); ok {
		if err := control.authorizeCommand(ctx, arguments); err != nil {
			return nil, ctx, err
		}
		return registeredTool, ctx, nil
	}
	mode := permissionModeFromRegistry(ctx, permissions, metadata)
	if permissionPolicy(mode, metadata.PermissionClass) == permissionDenied {
		request := PermissionRequest{Tool: name, AgentID: model.AgentIDFromContext(ctx), Arguments: append(json.RawMessage(nil), arguments...), Metadata: metadata}
		if err := permissions.authorize(ctx, request); err != nil {
			return nil, ctx, err
		}
	}
	var pathGrant *publictool.PathGrant
	if pathTool, ok := registeredTool.(PathAccessTool); ok {
		grant, err := pathTool.PathAccessRequest(ctx, arguments)
		if err != nil {
			return nil, ctx, err
		}
		pathGrant = grant
	}
	var fingerprint string
	if r.workspaceRoot != nil && r.workspaceRoot.root != nil {
		ws := r.workspaceRoot.root.Name()
		var toolchains []string
		if cmdTool, ok := registeredTool.(CommandTool); ok {
			toolchains = cmdTool.CommandToolchainPaths
		}
		fingerprint = ComputeExecutionFingerprint(ws, string(mode), "powershell", toolchains, true)
	}
	permissionRequest := PermissionRequest{
		Tool: name, AgentID: model.AgentIDFromContext(ctx), Arguments: append(json.RawMessage(nil), arguments...), Metadata: metadata,
		PathGrant: pathGrant,
		Fingerprint: fingerprint,
	}
	if err := permissions.authorize(ctx, permissionRequest); err != nil {
		return nil, ctx, err
	}
	if pathGrant != nil {
		ctx = WithApprovedPathGrant(ctx, *pathGrant)
	}
	if name == "command" && publictool.NoInteractiveDecisions(ctx) {
		var request struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(arguments, &request) == nil {
			if highRiskCommandReason(request.Command) != "" && !commandApproved(request.Command, publictool.PlanAuthorizationFromContext(ctx).ApprovedHighRiskCommands) {
				return nil, ctx, &publictool.ExecutionBlockerError{Tool: name, Reason: "high-risk command is not individually approved in the plan"}
			}
		}
	}
	return registeredTool, ctx, nil
}

func (r *Registry) contextWithPermissionMode(ctx context.Context, name string) context.Context {
	if ctx == nil {
		return nil
	}
	if _, ok := permissionModeFromContext(ctx); ok {
		return ctx
	}
	metadata, ok := r.ToolMetadata(name)
	if !ok {
		return ctx
	}
	mode := permissionModeFromRegistry(ctx, r.permissions, metadata)
	return WithPermissionMode(ctx, string(mode))
}

// ExecuteCall runs a function call from a Responses API result and returns the
// matching function_call_output input item for the next request.
func (r *Registry) ExecuteCall(ctx context.Context, call model.OutputFunctionCall) model.InputFunctionCallOutput {
	output, err := r.ExecuteCallRecoverable(ctx, call)
	if err != nil {
		return model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText("Error: " + err.Error())}
	}
	return output
}

// ExecuteCallRecoverable allows Core to persist the tool result before the
// graph advances to the next checkpoint.
func (r *Registry) ExecuteCallRecoverable(ctx context.Context, call model.OutputFunctionCall) (model.InputFunctionCallOutput, error) {
	ctx = withCallID(ctx, call.CallID)
	ctx = r.contextWithPermissionMode(ctx, call.Name)
	identity := publictool.CallIdentityFromContext(ctx)
	identity.AgentID = model.AgentIDFromContext(ctx)
	if identity.InvocationID == "" {
		identity.InvocationID = call.CallID
	}
	ctx = publictool.WithInvocationID(ctx, identity.InvocationID)
	ctx = publictool.WithCallIdentity(ctx, identity)
	started := time.Now()
	arguments := json.RawMessage(call.Arguments)
	registeredTool, ctx, permissionErr := r.authorizedTool(ctx, call.Name, arguments)
	if permissionErr != nil {
		var blocker *publictool.ExecutionBlockerError
		if errors.As(permissionErr, &blocker) {
			if publictool.NoInteractiveDecisions(ctx) {
				return model.InputFunctionCallOutput{}, blocker
			}
			return toolCallOutput(call.CallID, Result{Err: blocker}), nil
		}
		return toolCallOutput(call.CallID, Result{Err: permissionErr}), nil
	}
	execute := func() model.InputFunctionCallOutput {
		r.logger.Info("tool execution started", "tool", call.Name, "call_id", call.CallID)
		r.observeToolEvent(ToolEvent{CallID: call.CallID, InvocationID: identity.InvocationID, SessionID: identity.SessionID, TaskID: identity.TaskID, RunID: identity.RunID, ExecutionID: identity.ExecutionID, AgentID: identity.AgentID, Name: call.Name, Arguments: append(json.RawMessage(nil), arguments...), Started: true})
		result := registeredTool.Run(ctx, arguments)
		duration := time.Since(started)
		if result.Details == nil {
			output := result.Text()
			if len(output) > 128*1024 {
				output = strings.ToValidUTF8(output[:128*1024], "") + "\n[Output truncated]"
			}
			result.Details = &publictool.ExecutionDetails{Output: output}
		}
		attributes := []any{
			"tool", call.Name, "call_id", call.CallID, "duration", duration,
			"failed", result.Err != nil,
			"canceled", errors.Is(result.Err, context.Canceled) || errors.Is(result.Err, context.DeadlineExceeded),
		}
		if result.Err != nil {
			r.logger.Warn("tool execution finished", attributes...)
		} else {
			r.logger.Info("tool execution finished", attributes...)
		}
		r.observeToolEvent(ToolEvent{
			CallID: call.CallID, InvocationID: identity.InvocationID, SessionID: identity.SessionID, TaskID: identity.TaskID, RunID: identity.RunID, ExecutionID: identity.ExecutionID, AgentID: identity.AgentID,
			Name: call.Name, Arguments: append(json.RawMessage(nil), arguments...), Duration: duration, Details: result.Details,
			Failed: result.Err != nil, Canceled: errors.Is(result.Err, context.Canceled) || errors.Is(result.Err, context.DeadlineExceeded),
		})
		return toolCallOutput(call.CallID, result)
	}
	r.mu.RLock()
	controller := r.callController
	r.mu.RUnlock()
	if controller != nil {
		output, err := controller(ctx, call, execute)
		if err != nil {
			return model.InputFunctionCallOutput{}, err
		}
		return output, nil
	}
	return execute(), nil
}

func toolCallOutput(callID string, result Result) model.InputFunctionCallOutput {
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
		CallID: callID, Output: toolOutput, TranscriptOnly: append([]model.ContentPart(nil), result.TranscriptOnly...),
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
			r.logger.Warn("tool event observer panicked", "panic_type", fmt.Sprintf("%T", recovered))
		}
	}()
	observer(event)
}
