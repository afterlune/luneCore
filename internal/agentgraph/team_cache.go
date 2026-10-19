package agentgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/afterlune/luneCore/internal/model"
)

func teamForkCacheHint(state State) *model.PromptCacheHint {
	if !state.TeamForked || state.TeamForkPrefixItems <= 0 || state.TeamRunID == "" {
		return nil
	}
	identity := strings.Join([]string{state.TeamRunID, state.Provider, state.Model}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	return &model.PromptCacheHint{
		Key:         "lune-" + hex.EncodeToString(digest[:16]),
		PrefixItems: state.TeamForkPrefixItems,
	}
}
