package agentgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/afterlune/luneCore/internal/model"
)

func requestPromptCacheHint(state State, request model.ModelRequest, visibleTools []string, forkPrefixValid bool) *model.PromptCacheHint {
	if request.Input == nil || len(request.ContextManagement) > 0 {
		return nil
	}
	prefixItems := responseInputItemCount(request.Input)
	if prefixItems == 0 {
		return nil
	}
	if state.TeamForked && forkPrefixValid && state.TeamForkPrefixItems > 0 && state.TeamForkPrefixItems <= prefixItems {
		if hint := teamForkCacheHint(state, request); hint != nil {
			return hint
		}
	}
	return agentPromptCacheHint(state, request, prefixItems)
}

func teamForkCacheHint(state State, request model.ModelRequest) *model.PromptCacheHint {
	if !state.TeamForked || state.TeamForkPrefixItems <= 0 || state.TeamRunID == "" {
		return nil
	}
	// Core scopes the hint by provider/model lineage; adapters and providers
	// decide whether the fully rendered prompt prefix is actually reusable.
	if source := state.TeamForkSourceCache; source.Key != "" &&
		state.Provider == source.Provider && state.Protocol == source.Protocol && request.Model == source.Model {
		return &model.PromptCacheHint{Key: source.Key, PrefixItems: state.TeamForkPrefixItems}
	}
	return &model.PromptCacheHint{
		Key:         cacheKey("fork", state.TeamRunID, state.Provider, request.Model),
		PrefixItems: state.TeamForkPrefixItems,
	}
}

func agentPromptCacheHint(state State, request model.ModelRequest, prefixItems int) *model.PromptCacheHint {
	if state.RunID == "" || state.AgentID == "" || state.Provider == "" || request.Model == "" {
		return nil
	}
	return &model.PromptCacheHint{
		Key:         cacheKey("agent", state.RunID, state.AgentID, state.Provider, state.Protocol, request.Model),
		PrefixItems: prefixItems,
	}
}

func responseInputItemCount(input *model.ResponseInput) int {
	if input == nil {
		return 0
	}
	if len(input.Items) > 0 {
		return len(input.Items)
	}
	if input.Text != nil {
		return 1
	}
	return 0
}

func cacheKey(parts ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "lune-" + hex.EncodeToString(digest[:16])
}

func clearTeamForkBase(state *State) {
	state.TeamForkBaseInput = nil
	state.TeamForkBaseCache = teamCacheIdentity{}
}
