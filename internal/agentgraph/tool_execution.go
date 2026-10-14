package agentgraph

import (
	"context"

	"github.com/afterlune/luneCore/internal/model"
	runtimepermission "github.com/afterlune/luneCore/internal/tool"
	publictool "github.com/afterlune/luneCore/tool"
)

func (a *Agent) executeBusinessTool(ctx context.Context, state State, call model.OutputFunctionCall, callID string) (model.InputFunctionCallOutput, error) {
	toolContext := runtimepermission.WithPermissionMode(ctx, state.PermissionOverride)
	toolContext = publictool.WithPlanAuthorization(toolContext, state.PlanAuthorization)
	toolContext = publictool.WithNoInteractiveDecisions(toolContext, state.NoInteractiveDecisions)
	toolContext = model.WithAgentID(toolContext, state.AgentID)
	toolContext = model.WithInputAudioSupport(toolContext, state.SupportsInputAudio)
	identity := publictool.CallIdentityFromContext(toolContext)
	identity.RunID = state.RunID
	identity.ExecutionID = state.ExecutionID
	identity.AgentID = state.AgentID
	identity.InvocationID = callID
	toolContext = publictool.WithCallIdentity(toolContext, identity)
	if recoverable, ok := a.tools.(interface {
		ExecuteCallRecoverable(context.Context, model.OutputFunctionCall) (model.InputFunctionCallOutput, error)
	}); ok {
		return recoverable.ExecuteCallRecoverable(toolContext, call)
	}
	return a.tools.ExecuteCall(toolContext, call), nil
}
