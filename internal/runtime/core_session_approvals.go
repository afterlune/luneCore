package runtime

import (
	"context"
	"errors"
	"time"

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
		c.usageMu.Lock()
		defer c.usageMu.Unlock()
		saved, err := c.GetSession(ctx, sessionID)
		if err != nil {
			return err
		}
		store := runtimepermission.NewSessionApprovalStore(saved.Approvals)
		store.Add(approval)
		saved.Approvals = store.Snapshot()
		saved.UpdatedAt = time.Now()
		return c.saveSession(ctx, saved)
	})
}
