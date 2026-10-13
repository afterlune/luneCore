package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/agentruntime"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
)

// teamExecutor supplies profile-specific requesters to branches in the agent
// graph. Branch state, scheduling, joins and recovery belong to luneGraph.
type teamExecutor struct {
	core   *Core
	models *model.ProviderCatalog
	tools  *tool.Registry
}

func (t *teamExecutor) Profiles() map[string]model.ModelProfile {
	return t.models.Profiles()
}

func (t *teamExecutor) MaxParallelAgents() int {
	return t.models.ParallelLimit()
}

func (t *teamExecutor) PrepareBranch(_ context.Context, state agentgraph.State, assignment agentgraph.TeamAssignment, _ agentgraph.TaskOptions) (agentgraph.State, error) {
	profile, ok := t.models.Profile(assignment.Model)
	if !ok {
		return agentgraph.State{}, fmt.Errorf("team model profile %q is no longer configured", assignment.Model)
	}
	provider, ok := t.models.ProviderConfig(profile.Provider)
	if !ok {
		return agentgraph.State{}, fmt.Errorf("team provider %q is no longer configured", profile.Provider)
	}
	state.ModelProfile = assignment.Model
	state.Provider, state.Protocol = profile.Provider, provider.Protocol
	state.ServerCompaction, state.SupportsInputAudio = provider.ServerCompaction, profile.SupportsInputAudio
	state.Model, state.TeamModel, state.TeamProvider = profile.Model, profile.Model, profile.Provider
	return state, nil
}

func (t *teamExecutor) Requester(_ context.Context, state agentgraph.State) (agentgraph.Requester, error) {
	profile, ok := t.models.Profile(state.ModelProfile)
	if !ok {
		return nil, fmt.Errorf("team model profile %q is no longer configured", state.ModelProfile)
	}
	provider, ok := t.models.ProviderConfig(profile.Provider)
	if !ok {
		return nil, fmt.Errorf("team provider %q is no longer configured", profile.Provider)
	}
	if state.Provider != profile.Provider || state.Model != profile.Model || state.Protocol != provider.Protocol || state.ServerCompaction != provider.ServerCompaction || state.SupportsInputAudio != profile.SupportsInputAudio {
		return nil, errors.New("team model configuration changed; the paused task remains pinned to its original profile")
	}
	client, err := t.models.NewClient(state.ModelProfile, t.tools, t.core.logger)
	if err != nil {
		return nil, err
	}
	requester := agentruntime.NewUsageTrackingRequester(client, state.AgentID, state.ModelProfile, state.Provider, state.Model)
	requester.SetContextAgentUsageObserver(t.core.onAgentUsageContext)
	return requester, nil
}
