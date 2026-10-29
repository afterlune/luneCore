package runtime

import (
	"context"
	"encoding/json"

	"github.com/afterlune/luneCore/internal/stateformat"
)

type UpgradeReport = stateformat.Report

var ErrUpgradeRequired = stateformat.ErrUpgradeRequired

// UpgradeState runs offline after all Core instances using the directory stop.
func UpgradeState(ctx context.Context, stateDir string) (UpgradeReport, error) {
	return stateformat.Upgrade(ctx, stateDir, upgradeSessionPayload)
}

func upgradeSessionPayload(data json.RawMessage) (json.RawMessage, error) {
	var saved Session
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, err
	}
	saved, _, err := migrateSessionData(saved)
	if err != nil {
		return nil, err
	}
	// Rebuild provider state from the canonical conversation in the new engine.
	saved.ModelContext = nil
	saved.ModelContextKey = ""
	saved.AgentContext = nil
	return json.Marshal(saved)
}
