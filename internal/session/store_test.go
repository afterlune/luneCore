package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store, path
}

func TestOpenMigratesUnversionedSessionMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY, workspace TEXT NOT NULL, updated_at INTEGER NOT NULL, data BLOB NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"history":[{"type":"message","role":"user","content":"hello"}],"settings":{"model_profile":"main"}}`)
	if _, err := db.Exec(`INSERT INTO sessions(id,workspace,updated_at,data) VALUES(?,?,?,?)`, "legacy", "workspace", int64(7), legacy); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy database: %v", err)
	}
	var version int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("user_version = %d, want %d", version, currentSchemaVersion)
	}
	snapshot, err := store.Get(context.Background(), "legacy", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(snapshot.Data, &data); err != nil {
		t.Fatal(err)
	}
	if string(data["title"]) != `""` || string(data["archived"]) != "false" {
		t.Fatalf("migrated metadata title=%s archived=%s", data["title"], data["archived"])
	}
	if !strings.Contains(string(data["history"]), "hello") {
		t.Fatalf("migration lost history: %s", data["history"])
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen migrated database: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenMigratesVersionThreeDatabaseToRuntimeTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, workspace TEXT NOT NULL, updated_at INTEGER NOT NULL, data BLOB NOT NULL)`,
		`CREATE TABLE tool_invocations (
			run_id TEXT NOT NULL, invocation_id TEXT NOT NULL, session_id TEXT NOT NULL,
			execution_id TEXT NOT NULL DEFAULT '', agent_id TEXT NOT NULL, call_id TEXT NOT NULL,
			name TEXT NOT NULL, arguments BLOB NOT NULL, status TEXT NOT NULL, output BLOB, updated_at INTEGER NOT NULL,
			PRIMARY KEY (run_id, invocation_id)
		)`,
		`PRAGMA user_version = 3`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO sessions(id, workspace, updated_at, data) VALUES('old', 'workspace', 7, ?)`, []byte(`{"title":"old","history":null}`)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open v3 database: %v", err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 9 {
		t.Fatalf("user_version = %d, want 9", version)
	}
	if _, err := store.Get(context.Background(), "old", "workspace"); err != nil {
		t.Fatalf("old session was not preserved: %v", err)
	}
	queued, cursor, err := store.EnqueueTurn(context.Background(), QueuedTurn{
		SessionID: "old", QueueID: "q1", ExecutionID: "e1", Request: json.RawMessage(`{"input":"hello"}`),
	}, json.RawMessage(`{"kind":"task_queued"}`))
	if err != nil || queued.Position != 1 || cursor == 0 {
		t.Fatalf("enqueue after migration = %#v cursor=%d error=%v", queued, cursor, err)
	}
	events, err := store.RuntimeEventsAfter(context.Background(), "old", 0, 10)
	if err != nil || len(events) != 1 || events[0].Cursor != cursor {
		t.Fatalf("runtime events = %#v, error=%v", events, err)
	}
	deleted, err := store.Delete(context.Background(), "old", "workspace")
	if err != nil || !deleted {
		t.Fatalf("delete migrated session = %v, error=%v", deleted, err)
	}
	queuedRows, err := store.QueuedTurns(context.Background(), "old")
	if err != nil || len(queuedRows) != 0 {
		t.Fatalf("queued rows after session delete = %#v, error=%v", queuedRows, err)
	}
	events, err = store.RuntimeEventsAfter(context.Background(), "old", 0, 10)
	if err != nil || len(events) != 0 {
		t.Fatalf("events after session delete = %#v, error=%v", events, err)
	}
}

func TestOpenMigratesVersionFourDatabaseWithoutBackfillingTurnResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, workspace TEXT NOT NULL, updated_at INTEGER NOT NULL, data BLOB NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions(id, workspace, updated_at, data) VALUES('old', 'workspace', 7, ?)`, []byte(`{"history":null}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 4`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open v4 database: %v", err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 9 {
		t.Fatalf("user_version = %d, want 9", version)
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM turn_results`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("migration backfilled %d turn results, want none", count)
	}
	if _, err := store.Get(context.Background(), "old", "workspace"); err != nil {
		t.Fatalf("old session was not preserved: %v", err)
	}
}

func TestOpenRejectsNewerSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted a database from a newer version")
	}
}

func TestSaveAndHistoryUpdatePreserveSessionMetadata(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	first := Snapshot{ID: "one", Workspace: "workspace", Data: json.RawMessage(`{"history":["first"],"title":"Keep me","archived":true,"settings":{"model_profile":"main"}}`)}
	if err := store.Save(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := Snapshot{ID: "one", Workspace: "workspace", Data: json.RawMessage(`{"history":null,"title":"stale","archived":false,"settings":{"model_profile":"review"}}`)}
	if err := store.Save(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateHistory(ctx, "one", "workspace", json.RawMessage(`["second"]`)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTitle(ctx, "one", "workspace", "Renamed"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetArchived(ctx, "one", "workspace", false); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Get(ctx, "one", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		History  []string `json:"history"`
		Title    string   `json:"title"`
		Archived bool     `json:"archived"`
		Settings struct {
			ModelProfile string `json:"model_profile"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(snapshot.Data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.History) != 1 || got.History[0] != "second" || got.Title != "Renamed" || got.Archived || got.Settings.ModelProfile != "review" {
		t.Fatalf("session after updates = %#v", got)
	}
	if err := store.UpdateTitle(ctx, "missing", "workspace", "title"); err == nil {
		t.Fatal("UpdateTitle succeeded for a missing session")
	}
}

func TestUpdateConversationAtomicallySavesTranscriptAndProviderContext(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	if err := store.Save(ctx, Snapshot{ID: "session", Workspace: "workspace", Data: json.RawMessage(`{"history":[],"model_context":["old"],"model_context_key":"old-key","title":"keep"}`)}); err != nil {
		t.Fatal(err)
	}
	transcript := json.RawMessage(`[{"type":"message","role":"user","content":"visible"}]`)
	modelContext := json.RawMessage(`[{"type":"compaction","encrypted_content":"opaque"}]`)
	if err := store.UpdateConversation(ctx, "session", "workspace", transcript, modelContext, "responses:openai:gpt-test"); err != nil {
		t.Fatalf("UpdateConversation: %v", err)
	}
	snapshot, err := store.Get(ctx, "session", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		History         json.RawMessage `json:"history"`
		ModelContext    json.RawMessage `json:"model_context"`
		ModelContextKey string          `json:"model_context_key"`
		Title           string          `json:"title"`
	}
	if err := json.Unmarshal(snapshot.Data, &saved); err != nil {
		t.Fatal(err)
	}
	if string(saved.History) != string(transcript) || string(saved.ModelContext) != string(modelContext) || saved.ModelContextKey != "responses:openai:gpt-test" || saved.Title != "keep" {
		t.Fatalf("saved conversation = %#v", saved)
	}
	if err := store.UpdateHistory(ctx, "session", "workspace", json.RawMessage(`[]`)); err != nil {
		t.Fatalf("UpdateHistory: %v", err)
	}
	snapshot, err = store.Get(ctx, "session", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(snapshot.Data), "opaque") || strings.Contains(string(snapshot.Data), "responses:openai") {
		t.Fatalf("replacing canonical history retained stale provider context: %s", snapshot.Data)
	}
}

func TestCompleteQueuedTurnAtomicallyPersistsConversationResultAndEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Save(ctx, Snapshot{ID: "session", Workspace: "workspace", Data: json.RawMessage(`{"history":null}`)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnqueueTurn(ctx, QueuedTurn{
		SessionID: "session", QueueID: "queue", ExecutionID: "execution", Request: json.RawMessage(`{"input":"hello"}`),
	}, json.RawMessage(`{"kind":"task_queued"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_terminal_event BEFORE INSERT ON runtime_events
		WHEN json_extract(CAST(NEW.payload AS TEXT), '$.kind') = 'task_finished'
		BEGIN SELECT RAISE(ABORT, 'reject terminal event'); END`); err != nil {
		t.Fatal(err)
	}
	history := json.RawMessage(`{"items":[{"role":"assistant","text":"done"}]}`)
	response := json.RawMessage(`{"id":"response-1","output":[]}`)
	reports := json.RawMessage(`[{"candidate_id":"candidate-1","attempt":1,"source":"model","results":[{"criterion_id":"answer","outcome":"passed","evidence":["candidate:candidate-1"],"reason":"complete"}]}]`)
	criteria := json.RawMessage(`[{"id":"answer","description":"Answer the question.","basis":"hello"}]`)
	evidence := json.RawMessage(`[{"reference":"candidate:candidate-1","content":"hello world"}]`)
	event := json.RawMessage(`{"kind":"task_finished","status":"completed"}`)
	if _, err := store.CompleteQueuedTurnWithAcceptance(ctx, "session", "queue", "execution", "completed", response, history, history, "context-key", reports, criteria, evidence, event); err == nil {
		t.Fatal("CompleteQueuedTurn succeeded despite a rejected terminal event")
	}
	snapshot, err := store.Get(ctx, "session", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if string(snapshot.Data) != `{"history":null}` {
		t.Fatalf("session changed despite rolled back completion: %s", snapshot.Data)
	}
	queued, err := store.QueuedTurns(ctx, "session")
	if err != nil || len(queued) != 1 {
		t.Fatalf("queue after rollback = %#v, error=%v", queued, err)
	}
	if _, found, err := store.TurnResult(ctx, "session", "execution"); err != nil || found {
		t.Fatalf("result after rollback found=%t, error=%v", found, err)
	}
	if _, err := store.db.Exec(`DROP TRIGGER reject_terminal_event`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteQueuedTurnWithAcceptance(ctx, "session", "queue", "execution", "completed", response, history, history, "context-key", reports, criteria, evidence, event); err != nil {
		t.Fatalf("CompleteQueuedTurn: %v", err)
	}
	stored, found, err := store.TurnResult(ctx, "session", "execution")
	if err != nil || !found || stored.Status != "completed" || string(stored.Response) != string(response) || string(stored.AcceptanceReports) != string(reports) || string(stored.AcceptanceCriteria) != string(criteria) || string(stored.AcceptanceEvidence) != string(evidence) {
		t.Fatalf("turn result = %#v found=%t error=%v", stored, found, err)
	}
	queued, err = store.QueuedTurns(ctx, "session")
	if err != nil || len(queued) != 0 {
		t.Fatalf("queue after completion = %#v, error=%v", queued, err)
	}
	snapshot, err = store.Get(ctx, "session", "workspace")
	if err != nil || !strings.Contains(string(snapshot.Data), `"text":"done"`) {
		t.Fatalf("completed conversation = %s, error=%v", snapshot.Data, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	stored, found, err = reopened.TurnResult(ctx, "session", "execution")
	if err != nil || !found || string(stored.Response) != string(response) || string(stored.AcceptanceReports) != string(reports) || string(stored.AcceptanceCriteria) != string(criteria) || string(stored.AcceptanceEvidence) != string(evidence) {
		t.Fatalf("reopened turn result = %#v found=%t error=%v", stored, found, err)
	}
	if deleted, err := reopened.Delete(ctx, "session", "workspace"); err != nil || !deleted {
		t.Fatalf("delete session = %t, error=%v", deleted, err)
	}
	if _, found, err := reopened.TurnResult(ctx, "session", "execution"); err != nil || found {
		t.Fatalf("result after session deletion found=%t, error=%v", found, err)
	}
}

func TestV7TurnResultsMigrateWithEmptyAcceptanceReports(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE turn_results (
		session_id TEXT NOT NULL, execution_id TEXT NOT NULL, status TEXT NOT NULL, response BLOB,
		budget_reason TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL, PRIMARY KEY(session_id, execution_id)
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO turn_results(session_id, execution_id, status, response, updated_at)
		VALUES('old-session', 'old-execution', 'completed', ?, 7)`, []byte(`{"id":"old-response"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 7`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open v7 database: %v", err)
	}
	defer store.Close()
	result, found, err := store.TurnResult(context.Background(), "old-session", "old-execution")
	if err != nil || !found || result.Status != "completed" || len(result.AcceptanceReports) != 0 || len(result.AcceptanceCriteria) != 0 || len(result.AcceptanceEvidence) != 0 {
		t.Fatalf("migrated old result = %#v, found=%v, error=%v", result, found, err)
	}
}

func TestReplaceDataIfUnchangedPreservesTimestampAndRejectsStalePayload(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	updatedAt := time.Unix(123, 456)
	oldData := json.RawMessage(`{"history":["old"],"title":"thread","archived":false}`)
	newData := json.RawMessage(`{"data_version":1,"history":["old"],"title":"thread","archived":false}`)
	if err := store.Save(ctx, Snapshot{ID: "one", Workspace: "workspace", UpdatedAt: updatedAt, Data: oldData}); err != nil {
		t.Fatal(err)
	}
	updated, err := store.ReplaceDataIfUnchanged(ctx, "one", "workspace", json.RawMessage(`{"history":[]}`), newData)
	if err != nil || updated {
		t.Fatalf("stale replacement updated=%v, error=%v; want unchanged", updated, err)
	}
	updated, err = store.ReplaceDataIfUnchanged(ctx, "one", "workspace", oldData, newData)
	if err != nil || !updated {
		t.Fatalf("matching replacement updated=%v, error=%v; want updated", updated, err)
	}
	snapshot, err := store.Get(ctx, "one", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if string(snapshot.Data) != string(newData) || !snapshot.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("snapshot after replacement = %#v, want data version update without timestamp change", snapshot)
	}
}

func TestLatestAndListFilterArchivedBeforeLimit(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	for _, snapshot := range []Snapshot{
		{ID: "active-old", Workspace: "workspace", UpdatedAt: time.Unix(1, 0), Data: json.RawMessage(`{"title":"old","archived":false}`)},
		{ID: "archived-new", Workspace: "workspace", UpdatedAt: time.Unix(3, 0), Data: json.RawMessage(`{"title":"hidden","archived":true}`)},
		{ID: "active-new", Workspace: "workspace", UpdatedAt: time.Unix(2, 0), Data: json.RawMessage(`{"title":"new","archived":false}`)},
		{ID: "other-workspace", Workspace: "other", UpdatedAt: time.Unix(4, 0), Data: json.RawMessage(`{"archived":false}`)},
	} {
		if err := store.Save(ctx, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	latest, err := store.Latest(ctx, "workspace")
	if err != nil || latest == nil || latest.ID != "active-new" {
		t.Fatalf("Latest = %#v, %v; want active-new", latest, err)
	}
	active, err := store.List(ctx, "workspace", 1, false)
	if err != nil || len(active) != 1 || active[0].ID != "active-new" {
		t.Fatalf("List active = %#v, %v; want active-new despite limit 1", active, err)
	}
	all, err := store.List(ctx, "workspace", 10, true)
	if err != nil || len(all) != 3 || all[0].ID != "archived-new" {
		t.Fatalf("List including archived = %#v, %v", all, err)
	}
}

func TestDeleteIsWorkspaceScopedAndIdempotent(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	if err := store.Save(ctx, Snapshot{ID: "one", Workspace: "workspace", Data: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.Delete(ctx, "one", "other")
	if err != nil || deleted {
		t.Fatalf("cross-workspace Delete = %v, %v", deleted, err)
	}
	deleted, err = store.Delete(ctx, "one", "workspace")
	if err != nil || !deleted {
		t.Fatalf("Delete = %v, %v", deleted, err)
	}
	deleted, err = store.Delete(ctx, "one", "workspace")
	if err != nil || deleted {
		t.Fatalf("repeat Delete = %v, %v", deleted, err)
	}
}
