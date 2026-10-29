package runtime

import (
	"context"
	"encoding/json"
	"errors"

	runtimepermission "github.com/afterlune/luneCore/internal/tool"
	publictool "github.com/afterlune/luneCore/tool"
)

func (c *Core) addSessionApproval(ctx context.Context, sessionID string, approval publictool.SessionApproval) error {
	if c == nil || ctx == nil || sessionID == "" {
		return errors.New("core, context, and session id are required")
	}
	if approval.Tool == "" {
		return errors.New("approval tool is required")
	}
	return c.withSessionLock(sessionID, func() error {
		return c.sessions.ModifyApprovals(ctx, sessionID, c.workspace, func(previous json.RawMessage) (json.RawMessage, error) {
			var saved Session
			if err := json.Unmarshal(previous, &saved); err != nil {
				return nil, err
			}
			store := runtimepermission.NewSessionApprovalStore(saved.Approvals)
			store.Add(approval)
			saved.Approvals = store.Snapshot()
			return json.Marshal(saved)
		})
	})
}
