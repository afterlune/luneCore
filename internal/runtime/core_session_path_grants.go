package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	runtimepermission "github.com/afterlune/luneCore/internal/tool"
	publictool "github.com/afterlune/luneCore/tool"
)

func (c *Core) addSessionPathGrant(ctx context.Context, sessionID string, grant publictool.PathGrant) error {
	if c == nil || ctx == nil || sessionID == "" {
		return errors.New("core, context, and session id are required")
	}
	if grant.Access != publictool.PathRead && grant.Access != publictool.PathReadWrite {
		return fmt.Errorf("invalid path access %q", grant.Access)
	}
	if !filepath.IsAbs(grant.Path) {
		return errors.New("granted path must be absolute")
	}
	canonical, err := filepath.EvalSymlinks(filepath.Clean(grant.Path))
	if err != nil {
		return fmt.Errorf("resolve granted directory: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return fmt.Errorf("inspect granted directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("granted path is not a directory")
	}
	grant.Path = canonical

	return c.withSessionLock(sessionID, func() error {
		c.usageMu.Lock()
		defer c.usageMu.Unlock()
		saved, err := c.GetSession(ctx, sessionID)
		if err != nil {
			return err
		}
		store := runtimepermission.NewSessionPathGrantStore(saved.PathGrants)
		store.Add(grant)
		saved.PathGrants = store.Snapshot()
		saved.UpdatedAt = time.Now()
		return c.saveSession(ctx, saved)
	})
}
