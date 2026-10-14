package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/afterlune/luneCore/internal/agentruntime"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
	"github.com/afterlune/luneCore/internal/tool"
	publictool "github.com/afterlune/luneCore/tool"
)

var ErrToolInvocationPersistence = agentruntime.ErrToolInvocationPersistence

type storedToolCallOutput struct {
	CallID         string           `json:"call_id"`
	Output         model.ToolOutput `json:"output"`
	TranscriptOnly model.ToolOutput `json:"transcript_only"`
}

func encodeToolCallOutput(output model.InputFunctionCallOutput) (json.RawMessage, error) {
	transcript := model.NewToolOutputParts(output.TranscriptOnly...)
	stored := storedToolCallOutput{CallID: output.CallID, Output: output.Output, TranscriptOnly: transcript}
	data, err := json.Marshal(stored)
	return json.RawMessage(data), err
}

func decodeToolCallOutput(data json.RawMessage) (model.InputFunctionCallOutput, error) {
	var stored storedToolCallOutput
	if err := json.Unmarshal(data, &stored); err != nil {
		return model.InputFunctionCallOutput{}, err
	}
	return model.InputFunctionCallOutput{CallID: stored.CallID, Output: stored.Output, TranscriptOnly: stored.TranscriptOnly.Parts}, nil
}

func (c *Core) executeToolCall(ctx context.Context, call model.OutputFunctionCall, execute tool.ToolCallExecution) (model.InputFunctionCallOutput, error) {
	identity := publictool.CallIdentityFromContext(ctx)
	if identity.SessionID == "" || identity.RunID == "" || identity.InvocationID == "" {
		return execute(), nil
	}
	arguments := json.RawMessage(call.Arguments)
	if !json.Valid(arguments) {
		arguments = json.RawMessage("null")
	}
	record, created, err := c.sessions.StartToolInvocation(ctx, session.ToolInvocation{
		RunID: identity.RunID, InvocationID: identity.InvocationID, SessionID: identity.SessionID,
		ExecutionID: identity.ExecutionID, AgentID: identity.AgentID, CallID: call.CallID, Name: call.Name, Arguments: arguments,
	})
	if err != nil {
		return model.InputFunctionCallOutput{}, fmt.Errorf("%w: %w", ErrToolInvocationPersistence, err)
	}
	if created {
		return c.runAndSaveToolCall(ctx, record, execute)
	}
	switch record.Status {
	case session.ToolInvocationCompleted, session.ToolInvocationSkipped:
		output, err := decodeToolCallOutput(record.Output)
		if err != nil {
			return model.InputFunctionCallOutput{}, fmt.Errorf("decode saved tool invocation result: %w", err)
		}
		c.emit(Event{Kind: EventToolFinished, SessionID: identity.SessionID, TaskID: identity.TaskID, ExecutionID: identity.ExecutionID, Tool: ToolCall{
			ID: call.CallID, InvocationID: identity.InvocationID, AgentID: identity.AgentID,
			Name: call.Name, Arguments: append(json.RawMessage(nil), arguments...), Recovered: true,
		}})
		return output, nil
	case session.ToolInvocationRetry:
		if err := c.sessions.ClaimToolInvocationRetry(ctx, record.RunID, record.InvocationID); err != nil {
			return model.InputFunctionCallOutput{}, err
		}
		return c.runAndSaveToolCall(ctx, record, execute)
	case session.ToolInvocationRunning:
		if err := c.sessions.MarkToolInvocationUnknown(ctx, record.RunID, record.InvocationID); err != nil {
			return model.InputFunctionCallOutput{}, err
		}
		record.Status = session.ToolInvocationUnknown
		c.emitToolRecoveryRequired(identity, record)
		return model.InputFunctionCallOutput{}, &ToolRecoveryRequiredError{InvocationID: record.InvocationID}
	case session.ToolInvocationUnknown:
		c.emitToolRecoveryRequired(identity, record)
		return model.InputFunctionCallOutput{}, &ToolRecoveryRequiredError{InvocationID: record.InvocationID}
	default:
		return model.InputFunctionCallOutput{}, fmt.Errorf("tool invocation %q has unsupported state %q", record.InvocationID, record.Status)
	}
}

func (c *Core) runAndSaveToolCall(ctx context.Context, record session.ToolInvocation, execute tool.ToolCallExecution) (model.InputFunctionCallOutput, error) {
	output := execute()
	encoded, err := encodeToolCallOutput(output)
	if err != nil {
		return model.InputFunctionCallOutput{}, fmt.Errorf("%w: encode result: %w", ErrToolInvocationPersistence, err)
	}
	if err := c.sessions.SaveToolInvocationOutput(ctx, record.RunID, record.InvocationID, session.ToolInvocationCompleted, encoded); err != nil {
		return model.InputFunctionCallOutput{}, fmt.Errorf("%w: %w", ErrToolInvocationPersistence, err)
	}
	return output, nil
}

func (c *Core) emitToolRecoveryRequired(identity publictool.CallIdentity, record session.ToolInvocation) {
	c.emit(Event{
		Kind: EventToolRecoveryRequired, SessionID: identity.SessionID, TaskID: identity.TaskID, ExecutionID: identity.ExecutionID,
		AgentID: identity.AgentID, ToolRecovery: &ToolRecoveryRequest{
			InvocationID: record.InvocationID, CallID: record.CallID, AgentID: record.AgentID,
			Tool: record.Name, Arguments: append(json.RawMessage(nil), record.Arguments...),
		},
	})
}

// resolveToolRecovery records an explicit retry or skip choice for a tool call
// whose process may have stopped after causing a side effect.
func (c *Core) resolveToolRecovery(ctx context.Context, sessionID, invocationID string, decision ToolRecoveryDecision) error {
	if c == nil || ctx == nil || sessionID == "" || invocationID == "" {
		return errors.New("core, context, session id, and invocation id are required")
	}
	record, err := c.sessions.GetToolInvocationForSession(ctx, sessionID, invocationID)
	if err != nil {
		return err
	}
	if record.SessionID != sessionID || record.Status != session.ToolInvocationUnknown {
		return errors.New("tool invocation is not awaiting recovery in this session")
	}
	resolution := session.ToolInvocationRetry
	var output json.RawMessage
	if decision == ToolRecoverySkip {
		resolution = session.ToolInvocationSkipped
		result, encodeErr := encodeToolCallOutput(model.InputFunctionCallOutput{
			CallID: record.CallID,
			Output: model.NewToolOutputText("The prior tool outcome is unknown; the host chose to skip retrying this invocation."),
		})
		if encodeErr != nil {
			return encodeErr
		}
		output = result
	} else if decision != ToolRecoveryRetry {
		return fmt.Errorf("unknown tool recovery decision %q", decision)
	} else {
		resolution = session.ToolInvocationRetry
	}
	return c.sessions.ResolveToolInvocation(ctx, sessionID, record.RunID, invocationID, resolution, output)
}

func (c *Core) pendingToolRecoveries(ctx context.Context, sessionID string) ([]ToolRecoveryRequest, error) {
	if state, ok := c.StateForSession(sessionID); ok && state.Status == TaskRunning {
		return nil, nil
	}
	pendingState, found, err := c.pendingState(ctx, sessionID)
	if err != nil || !found || pendingState.ExecutionID == "" {
		return nil, err
	}
	invocations, err := c.sessions.PendingToolInvocations(ctx, sessionID, pendingState.ExecutionID)
	if err != nil {
		return nil, err
	}
	requests := make([]ToolRecoveryRequest, 0, len(invocations))
	for _, record := range invocations {
		if record.Status == session.ToolInvocationRunning {
			if err := c.sessions.MarkToolInvocationUnknown(ctx, record.RunID, record.InvocationID); err != nil {
				return nil, err
			}
		}
		requests = append(requests, ToolRecoveryRequest{
			InvocationID: record.InvocationID, CallID: record.CallID, AgentID: record.AgentID,
			Tool: record.Name, Arguments: append(json.RawMessage(nil), record.Arguments...),
		})
	}
	return requests, nil
}

func (c *Core) checkToolRecovery(ctx context.Context, sessionID string, taskState TaskState) error {
	invocations, err := c.sessions.PendingToolInvocations(ctx, sessionID, taskState.ExecutionID)
	if err != nil {
		return err
	}
	var first *ToolRecoveryRequiredError
	for _, record := range invocations {
		if record.Status == session.ToolInvocationRunning {
			if err := c.sessions.MarkToolInvocationUnknown(ctx, record.RunID, record.InvocationID); err != nil {
				return err
			}
			record.Status = session.ToolInvocationUnknown
		}
		identity := publictool.CallIdentity{
			SessionID: sessionID, TaskID: taskState.TaskID, ExecutionID: taskState.ExecutionID,
			RunID: record.RunID, AgentID: record.AgentID,
		}
		c.emitToolRecoveryRequired(identity, record)
		if first == nil {
			first = &ToolRecoveryRequiredError{InvocationID: record.InvocationID}
		}
	}
	if first != nil {
		return first
	}
	return nil
}
