package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
)

const CurrentSessionDataVersion = 3

func migrateSessionData(saved Session) (Session, bool, error) {
	if saved.DataVersion < 0 {
		return Session{}, false, fmt.Errorf("invalid negative data version %d", saved.DataVersion)
	}
	if saved.DataVersion > CurrentSessionDataVersion {
		return Session{}, false, fmt.Errorf("data version %d is newer than supported version %d", saved.DataVersion, CurrentSessionDataVersion)
	}
	changed := false
	for saved.DataVersion < CurrentSessionDataVersion {
		switch saved.DataVersion {
		case 0:
			saved = migrateSessionDataV0ToV1(saved)
		case 1:
			saved = migrateSessionDataV1ToV2(saved)
		case 2:
			saved = migrateSessionDataV2ToV3(saved)
		default:
			return Session{}, false, fmt.Errorf("no migration exists from data version %d", saved.DataVersion)
		}
		changed = true
	}
	return saved, changed, nil
}

func migrateSessionDataV2ToV3(saved Session) Session {
	// Existing history remains the canonical transcript. A provider context is
	// built from it on the next task using the selected protocol.
	saved.ModelContext = nil
	saved.ModelContextKey = ""
	saved.DataVersion = 3
	return saved
}

func migrateSessionDataV0ToV1(saved Session) Session {
	if len(saved.History) == 0 {
		saved.History = json.RawMessage("null")
	}
	saved.DataVersion = 1
	return saved
}

func migrateSessionDataV1ToV2(saved Session) Session {
	if saved.AgentUsage == nil {
		saved.AgentUsage = make(map[string]UsageTotals)
		for _, record := range saved.UsageRequests {
			totals := saved.AgentUsage[record.AgentID]
			addUsage(&totals, record.Usage, record.Available)
			totals.EstimatedInput += estimatedInputTokensIfMissing(record.Usage.InputTokens, record.EstimatedInputTokens)
			saved.AgentUsage[record.AgentID] = totals
		}
	}
	if saved.SessionUsage.Requests == len(saved.UsageRequests) {
		saved.AgentUsageComplete = true
		if saved.SessionUsage.Requests == 0 && hasSessionHistory(saved.History) {
			saved.AgentUsageComplete = false
		}
	} else {
		saved.AgentUsageComplete = false
	}
	saved.DataVersion = 2
	return saved
}

func hasSessionHistory(history []byte) bool {
	trimmed := strings.TrimSpace(string(history))
	return trimmed != "" && trimmed != "null" && trimmed != "[]"
}
