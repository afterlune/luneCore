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

func (c *Core) saveSession(ctx context.Context, saved Session) error {
	var err error
	saved, _, err = migrateSessionData(saved)
	if err != nil {
		return err
	}
	if saved.UpdatedAt.IsZero() {
		saved.UpdatedAt = time.Now()
	}
	data, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	return c.sessions.Save(ctx, session.Snapshot{ID: saved.ID, Workspace: c.workspace, UpdatedAt: saved.UpdatedAt, Data: data})
}

func (c *Core) decodeSession(ctx context.Context, snapshot session.Snapshot) (Session, error) {
	for attempt := 0; attempt < 3; attempt++ {
		var saved Session
		if err := json.Unmarshal(snapshot.Data, &saved); err != nil {
			return Session{}, fmt.Errorf("decode session %q: %w", snapshot.ID, err)
		}
		saved.ID, saved.Workspace, saved.UpdatedAt = snapshot.ID, snapshot.Workspace, snapshot.UpdatedAt
		migrated, changed, err := migrateSessionData(saved)
		if err != nil {
			return Session{}, fmt.Errorf("migrate session %q: %w", snapshot.ID, err)
		}
		saved = migrated
		if len(saved.History) == 0 {
			saved.History = json.RawMessage("null")
		}
		if saved.Settings.ModelProfile == "" {
			saved.Settings.ModelProfile = c.models.DefaultModel()
		}
		if !changed {
			return saved, nil
		}
		encoded, err := json.Marshal(saved)
		if err != nil {
			return Session{}, fmt.Errorf("encode migrated session %q: %w", snapshot.ID, err)
		}
		updated, err := c.sessions.ReplaceDataIfUnchanged(ctx, snapshot.ID, snapshot.Workspace, snapshot.Data, encoded)
		if err != nil {
			return Session{}, fmt.Errorf("persist session %q migration: %w", snapshot.ID, err)
		}
		if updated {
			return saved, nil
		}
		latest, err := c.sessions.Get(ctx, snapshot.ID, snapshot.Workspace)
		if err != nil {
			return Session{}, fmt.Errorf("reload session %q during migration: %w", snapshot.ID, err)
		}
		snapshot = *latest
	}
	return Session{}, fmt.Errorf("session %q changed repeatedly during data migration", snapshot.ID)
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
