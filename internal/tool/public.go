package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/afterlune/luneCore/internal/model"
	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
)

type publicToolAdapter struct{ inner publictool.Tool }

func (t publicToolAdapter) Definition() model.FunctionTool {
	var definition model.FunctionTool
	data, err := json.Marshal(t.inner.Definition())
	if err != nil || json.Unmarshal(data, &definition) != nil {
		return model.FunctionTool{}
	}
	return definition
}

func (t publicToolAdapter) Run(ctx context.Context, arguments json.RawMessage) Result {
	result := t.inner.Run(ctx, arguments)
	out := Result{Output: result.Output, Err: result.Err, Details: result.Details}
	if result.ModelOutput != nil {
		var converted model.ToolOutput
		if err := publicToolJSONRoundTrip(result.ModelOutput, &converted); err != nil {
			return Result{Err: fmt.Errorf("decode host tool output: %w", err)}
		}
		out.ModelOutput = &converted
	}
	if len(result.TranscriptOnly) > 0 {
		data, err := json.Marshal(publicmodel.NewToolOutputParts(result.TranscriptOnly...))
		if err != nil {
			return Result{Err: fmt.Errorf("encode host tool transcript output: %w", err)}
		}
		var converted model.ToolOutput
		if err := json.Unmarshal(data, &converted); err != nil {
			return Result{Err: fmt.Errorf("decode host tool transcript output: %w", err)}
		}
		out.TranscriptOnly = converted.Parts
	}
	return out
}

func publicToolJSONRoundTrip(source, destination any) error {
	data, err := json.Marshal(source)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, destination)
}

func HostToolRegistrations(registrations []publictool.Registration) ([]ToolRegistration, error) {
	converted := make([]ToolRegistration, 0, len(registrations))
	for _, registration := range registrations {
		if registration.Tool == nil {
			return nil, fmt.Errorf("%w: tool is nil", publictool.ErrInvalidTool)
		}
		if !registration.Metadata.Valid() {
			return nil, fmt.Errorf("%w: tool %q must declare a valid permission class", publictool.ErrInvalidTool, registration.Tool.Definition().Name)
		}
		if registration.Metadata.Category.Name == "" {
			return nil, fmt.Errorf("%w: tool %q must declare a category", publictool.ErrInvalidTool, registration.Tool.Definition().Name)
		}
		definition := publicToolAdapter{inner: registration.Tool}.Definition()
		if definition.Name == "" || len(definition.Parameters) == 0 || !json.Valid(definition.Parameters) {
			return nil, fmt.Errorf("%w: tool definition must have a name and valid JSON parameters", publictool.ErrInvalidTool)
		}
		var category model.ToolCategory
		if err := publicToolJSONRoundTrip(registration.Metadata.Category, &category); err != nil {
			return nil, fmt.Errorf("convert category for host tool: %w", err)
		}
		converted = append(converted, ToolRegistration{
			Tool: publicToolAdapter{inner: registration.Tool},
			Metadata: model.ToolMetadata{
				Category: category, PermissionClass: model.ToolPermissionClass(registration.Metadata.PermissionClass),
				ReadOnly: registration.Metadata.ReadOnly,
			},
		})
	}
	return converted, nil
}

type invocationIDContextKey struct{}

func withCallID(ctx context.Context, callID string) context.Context {
	return context.WithValue(ctx, invocationIDContextKey{}, callID)
}

func callIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	callID, _ := ctx.Value(invocationIDContextKey{}).(string)
	return callID
}
