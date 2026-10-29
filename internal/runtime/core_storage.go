package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/afterlune/luneCore/internal/session"
)

func (c *Core) createSnapshot(ctx context.Context, saved Session) error {
	if saved.DataVersion != CurrentSessionDataVersion {
		return fmt.Errorf("session data version %d is unsupported", saved.DataVersion)
	}
	if saved.UpdatedAt.IsZero() {
		saved.UpdatedAt = time.Now()
	}
	data, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	return c.sessions.Create(ctx, session.Snapshot{ID: saved.ID, Workspace: c.workspace, UpdatedAt: saved.UpdatedAt, Data: data})
}

func (c *Core) decodeSession(ctx context.Context, snapshot session.Snapshot) (Session, error) {
	var saved Session
	if err := json.Unmarshal(snapshot.Data, &saved); err != nil {
		return Session{}, fmt.Errorf("decode session %q: %w", snapshot.ID, err)
	}
	if saved.DataVersion != CurrentSessionDataVersion {
		return Session{}, fmt.Errorf("session %q has unsupported data version %d; run lune migrate --state-dir %s", snapshot.ID, saved.DataVersion, c.stateDir)
	}
	saved.ID, saved.Workspace, saved.UpdatedAt = snapshot.ID, snapshot.Workspace, snapshot.UpdatedAt
	if len(saved.History) == 0 {
		saved.History = json.RawMessage(`null`)
	}
	if saved.Settings.ModelProfile == "" {
		saved.Settings.ModelProfile = c.models.DefaultModel()
	}
	return saved, nil
}

func newID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("create id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
