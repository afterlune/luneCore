package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/afterlune/luneCore/internal/session"
	"github.com/afterlune/luneCore/internal/stateformat"
)

func TestOfflineUpgradeKeepsHistoryAndInvalidatesOldWork(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.db")
	store, err := session.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	original := json.RawMessage(`{"data_version":5,"history":[{"type":"message","role":"user","content":"keep history"}],"settings":{"model_profile":"main"},"session_usage":{"requests":42,"input":1000},"title":"Keep title","model_context":["old opaque context"],"model_context_key":"old"}`)
	if err := store.Create(t.Context(), session.Snapshot{ID: "saved", Workspace: "workspace", Data: original}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnqueueTurn(t.Context(), session.QueuedTurn{SessionID: "saved", QueueID: "q", ExecutionID: "e", Request: json.RawMessage(`{"input":"do not replay"}`)}, json.RawMessage(`{"Kind":"task_queued"}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	legacy := []byte("legacy checkpoint must remain untouched")
	if err := os.WriteFile(filepath.Join(dir, "checkpoints-v10.db"), legacy, 0600); err != nil {
		t.Fatal(err)
	}
	if lock, err := stateformat.Open(dir); !errors.Is(err, ErrUpgradeRequired) {
		if lock != nil {
			_ = lock.Close()
		}
		t.Fatalf("old format opened: %v", err)
	}
	report, err := UpgradeState(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if report.DiscardedTasks != 1 || report.BackupDirectory == "" {
		t.Fatalf("report=%+v", report)
	}
	backup, err := session.Open(filepath.Join(report.BackupDirectory, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := backup.Get(t.Context(), "saved", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if string(before.Data) != string(original) {
		t.Fatalf("backup lost original: %s", before.Data)
	}
	_ = backup.Close()
	store, err = session.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	saved, err := store.Get(t.Context(), "saved", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	var decoded Session
	if err := json.Unmarshal(saved.Data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SessionUsage.Requests != 42 || decoded.SessionUsage.Input != 1000 || decoded.Title != "Keep title" || decoded.Settings.ModelProfile != "main" || len(decoded.History) == 0 || len(decoded.ModelContext) > 0 {
		t.Fatalf("upgraded data=%s", saved.Data)
	}
	result, found, err := store.TurnResult(t.Context(), "saved", "e")
	if err != nil || !found || result.Status != string(TaskDiscarded) {
		t.Fatalf("result=%+v found=%v err=%v", result, found, err)
	}
	if queue, err := store.QueuedTurns(t.Context(), "saved"); err != nil || len(queue) != 0 {
		t.Fatalf("queue=%+v err=%v", queue, err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "checkpoints-v10.db")); err != nil || string(data) != string(legacy) {
		t.Fatal("legacy file changed")
	}
	if again, err := UpgradeState(t.Context(), dir); err != nil || !again.AlreadyCurrent {
		t.Fatalf("repeat upgrade=%+v,%v", again, err)
	}
	live, err := stateformat.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpgradeState(t.Context(), dir); !errors.Is(err, session.ErrFileLockInUse) {
		t.Fatalf("upgrade acquired live directory: %v", err)
	}
	_ = live.Close()
}

func TestOfflineUpgradeRollbackAndRetry(t *testing.T) {
	dir := t.TempDir()
	store, err := session.Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"history":[],"data_version":0}`)
	if err := store.Create(t.Context(), session.Snapshot{ID: "one", Workspace: "workspace", Data: raw}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	failure := errors.New("injected transformation failure")
	report, err := stateformat.Upgrade(t.Context(), dir, func(json.RawMessage) (json.RawMessage, error) { return nil, failure })
	if !errors.Is(err, failure) || report.BackupDirectory == "" {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "execution-format")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed upgrade installed current marker")
	}
	store, err = session.Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(t.Context(), "one", "workspace")
	if err != nil || string(got.Data) != string(raw) {
		t.Fatalf("failed upgrade changed data: %+v,%v", got, err)
	}
	_ = store.Close()
	if _, err := UpgradeState(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
}
