package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenPreservesVersionFifteenState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Create(ctx, Snapshot{
		ID: "existing-session", Workspace: "workspace", UpdatedAt: time.Unix(15, 0),
		Data: json.RawMessage(`{"title":"existing","archived":false,"history":null}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnqueueTurn(ctx, QueuedTurn{
		SessionID: "existing-session", QueueID: "queued-before-upgrade", ExecutionID: "execution-before-upgrade",
		Request: json.RawMessage(`{"input":"resume after upgrade"}`),
	}, json.RawMessage(`{"kind":"task_queued"}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE turn_submission_dedup`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 15`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("open version 15 state: %v", err)
	}
	defer upgraded.Close()
	var version int
	if err := upgraded.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("database version = %d, want %d", version, currentSchemaVersion)
	}
	if _, err := upgraded.Get(ctx, "existing-session", "workspace"); err != nil {
		t.Fatalf("existing session after upgrade: %v", err)
	}
	queued, err := upgraded.QueuedTurns(ctx, "existing-session")
	if err != nil || len(queued) != 1 || queued[0].QueueID != "queued-before-upgrade" {
		t.Fatalf("queued work after upgrade = %#v, error=%v", queued, err)
	}
}
