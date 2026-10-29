package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

func TestListPageReturnsNewestNonArchivedSessions(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	records := []Snapshot{
		{ID: "old", Workspace: "workspace", UpdatedAt: time.Unix(1, 0), Data: json.RawMessage(`{"title":"old","archived":false}`)},
		{ID: "middle", Workspace: "workspace", UpdatedAt: time.Unix(2, 0), Data: json.RawMessage(`{"title":"middle","archived":false}`)},
		{ID: "archived", Workspace: "workspace", UpdatedAt: time.Unix(3, 0), Data: json.RawMessage(`{"title":"archived","archived":true}`)},
		{ID: "new", Workspace: "workspace", UpdatedAt: time.Unix(4, 0), Data: json.RawMessage(`{"title":"new","archived":false}`)},
	}
	for _, record := range records {
		if err := store.Create(ctx, record); err != nil {
			t.Fatalf("Save(%s): %v", record.ID, err)
		}
	}

	first, err := store.ListPage(ctx, "workspace", 0, 2, false)
	if err != nil {
		t.Fatalf("ListPage first page: %v", err)
	}
	second, err := store.ListPage(ctx, "workspace", 2, 2, false)
	if err != nil {
		t.Fatalf("ListPage second page: %v", err)
	}
	got := []string{first[0].ID, first[1].ID, second[0].ID}
	want := []string{"new", "middle", "old"}
	if len(first) != 2 || len(second) != 1 || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ListPage results = %v + %v, want %v", first, second, want)
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
	if version != currentSchemaVersion {
		t.Fatalf("user_version = %d, want %d", version, currentSchemaVersion)
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
	if version != currentSchemaVersion {
		t.Fatalf("user_version = %d, want %d", version, currentSchemaVersion)
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

func TestFieldUpdatesPreserveSessionMetadata(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	first := Snapshot{ID: "one", Workspace: "workspace", Data: json.RawMessage(`{"history":["first"],"title":"Keep me","archived":true,"settings":{"model_profile":"main"}}`)}
	if err := store.Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSettings(ctx, "one", "workspace", json.RawMessage(`{"model_profile":"review"}`)); err != nil {
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
	if err := store.Create(ctx, Snapshot{ID: "session", Workspace: "workspace", Data: json.RawMessage(`{"history":[],"model_context":["old"],"model_context_key":"old-key","title":"keep"}`)}); err != nil {
		t.Fatal(err)
	}
	transcript := json.RawMessage(`[{"type":"message","role":"user","content":"visible"}]`)
	modelContext := json.RawMessage(`[{"type":"compaction","encrypted_content":"opaque"}]`)
	if err := store.UpdateConversation(ctx, "session", "workspace", transcript, modelContext, "responses:openai:gpt-test", json.RawMessage(`{"categories":["files"],"host_context":"private-context"}`)); err != nil {
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
		AgentContext    json.RawMessage `json:"agent_context"`
		Title           string          `json:"title"`
	}
	if err := json.Unmarshal(snapshot.Data, &saved); err != nil {
		t.Fatal(err)
	}
	if string(saved.History) != string(transcript) || string(saved.ModelContext) != string(modelContext) || saved.ModelContextKey != "responses:openai:gpt-test" || saved.Title != "keep" || !strings.Contains(string(saved.AgentContext), "private-context") {
		t.Fatalf("saved conversation = %#v", saved)
	}
	if err := store.UpdateHistory(ctx, "session", "workspace", json.RawMessage(`[]`)); err != nil {
		t.Fatalf("UpdateHistory: %v", err)
	}
	snapshot, err = store.Get(ctx, "session", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(snapshot.Data), "opaque") || strings.Contains(string(snapshot.Data), "responses:openai") || strings.Contains(string(snapshot.Data), "private-context") {
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
	if err := store.Create(ctx, Snapshot{ID: "session", Workspace: "workspace", Data: json.RawMessage(`{"history":null}`)}); err != nil {
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
	event := json.RawMessage(`{"kind":"task_finished","status":"completed"}`)
	if _, err := store.CompleteQueuedTurn(ctx, "session", "queue", "execution", "completed", response, history, history, "context-key", event, json.RawMessage(`{"categories":["files"]}`)); err == nil {
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
	if _, err := store.CompleteQueuedTurn(ctx, "session", "queue", "execution", "completed", response, history, history, "context-key", event, json.RawMessage(`{"categories":["files"]}`)); err != nil {
		t.Fatalf("CompleteQueuedTurn: %v", err)
	}
	stored, found, err := store.TurnResult(ctx, "session", "execution")
	if err != nil || !found || stored.Status != "completed" || string(stored.Response) != string(response) {
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
	if err != nil || !found || string(stored.Response) != string(response) {
		t.Fatalf("reopened turn result = %#v found=%t error=%v", stored, found, err)
	}
	// A repeated worker completion must not overwrite the committed result or
	// conversation even though the result upsert precedes queue removal in SQL.
	if _, err := reopened.CompleteQueuedTurn(ctx, "session", "queue", "execution", "failed", nil, json.RawMessage(`["wrong"]`), nil, "", event, nil); err == nil {
		t.Fatal("duplicate completion accepted without queue ownership")
	}
	stable, found, err := reopened.TurnResult(ctx, "session", "execution")
	if err != nil || !found || stable.Status != "completed" || string(stable.Response) != string(response) {
		t.Fatalf("duplicate completion changed durable result: %+v,%v", stable, err)
	}
	snapshot, err = reopened.Get(ctx, "session", "workspace")
	if err != nil || !strings.Contains(string(snapshot.Data), `"text":"done"`) {
		t.Fatalf("duplicate completion changed history: %+v,%v", snapshot, err)
	}
	if deleted, err := reopened.Delete(ctx, "session", "workspace"); err != nil || !deleted {
		t.Fatalf("delete session = %t, error=%v", deleted, err)
	}
	if _, found, err := reopened.TurnResult(ctx, "session", "execution"); err != nil || found {
		t.Fatalf("result after session deletion found=%t, error=%v", found, err)
	}
}

func TestV7TurnResultsMigrate(t *testing.T) {
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
	if err != nil || !found || result.Status != "completed" || string(result.Response) != `{"id":"old-response"}` {
		t.Fatalf("migrated old result = %#v, found=%v, error=%v", result, found, err)
	}
}

func TestReplaceDataIfUnchangedPreservesTimestampAndRejectsStalePayload(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	updatedAt := time.Unix(123, 456)
	oldData := json.RawMessage(`{"history":["old"],"title":"thread","archived":false}`)
	newData := json.RawMessage(`{"data_version":1,"history":["old"],"title":"thread","archived":false}`)
	if err := store.Create(ctx, Snapshot{ID: "one", Workspace: "workspace", UpdatedAt: updatedAt, Data: oldData}); err != nil {
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
		if err := store.Create(ctx, snapshot); err != nil {
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
	if err := store.Create(ctx, Snapshot{ID: "one", Workspace: "workspace", Data: json.RawMessage(`{}`)}); err != nil {
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

func TestUsageMutationPreservesCurrentConversation(t *testing.T) {
	for _, clearContext := range []bool{false, true} {
		t.Run(fmt.Sprintf("clear_context_%t", clearContext), func(t *testing.T) {
			store, _ := openTestStore(t)
			stale := Snapshot{ID: "session", Workspace: "workspace", Data: json.RawMessage(`{"history":["old"],"model_context":["old context"],"model_context_key":"old-key","agent_context":{"categories":["old"]},"settings":{"model_profile":"main"},"session_usage":{"requests":1}}`)}
			if err := store.Create(t.Context(), stale); err != nil {
				t.Fatal(err)
			}
			history := json.RawMessage(`["new history"]`)
			modelContext := json.RawMessage(`[{"type":"compaction","encrypted_content":"new opaque state"}]`)
			agentContext := json.RawMessage(`{"categories":["new"],"host_context":"new host state"}`)
			key := "new-key"
			if clearContext {
				modelContext = json.RawMessage(`null`)
				agentContext = json.RawMessage(`null`)
				key = ""
			}
			if err := store.UpdateConversation(t.Context(), stale.ID, stale.Workspace, history, modelContext, key, agentContext); err != nil {
				t.Fatal(err)
			}
			var update map[string]any
			if err := json.Unmarshal(stale.Data, &update); err != nil {
				t.Fatal(err)
			}
			update["settings"] = map[string]any{"model_profile": "review"}
			update["session_usage"] = map[string]any{"requests": 2}
			stale.Data, _ = json.Marshal(update)
			if err := store.ModifyUsage(t.Context(), stale.ID, stale.Workspace, func(json.RawMessage) (json.RawMessage, error) { return stale.Data, nil }); err != nil {
				t.Fatal(err)
			}
			if err := store.UpdateSettings(t.Context(), stale.ID, stale.Workspace, json.RawMessage(`{"model_profile":"review"}`)); err != nil {
				t.Fatal(err)
			}
			latest, err := store.Get(t.Context(), stale.ID, stale.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(latest.Data, &got); err != nil {
				t.Fatal(err)
			}
			for field, want := range map[string]json.RawMessage{"history": history, "model_context": modelContext, "agent_context": agentContext, "model_context_key": mustJSON(t, key)} {
				if string(got[field]) != string(want) {
					t.Errorf("%s=%s, want %s", field, got[field], want)
				}
			}
			if !strings.Contains(string(got["settings"]), "review") || !strings.Contains(string(got["session_usage"]), "2") {
				t.Fatalf("snapshot fields were not updated: %s", latest.Data)
			}
		})
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestFieldMutationPreservesJSONScalarConversationFields(t *testing.T) {
	for _, value := range []json.RawMessage{json.RawMessage(`"text"`), json.RawMessage(`true`), json.RawMessage(`7`), json.RawMessage(`null`)} {
		t.Run(string(value), func(t *testing.T) {
			store, _ := openTestStore(t)
			payload := mustJSON(t, map[string]json.RawMessage{"history": value, "model_context": value, "agent_context": value})
			snapshot := Snapshot{ID: "session", Workspace: "workspace", Data: payload}
			if err := store.Create(t.Context(), snapshot); err != nil {
				t.Fatal(err)
			}
			snapshot.Data = json.RawMessage(`{"history":[],"session_usage":{"requests":2}}`)
			if err := store.ModifyUsage(t.Context(), snapshot.ID, snapshot.Workspace, func(json.RawMessage) (json.RawMessage, error) { return snapshot.Data, nil }); err != nil {
				t.Fatal(err)
			}
			latest, err := store.Get(t.Context(), snapshot.ID, snapshot.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(latest.Data, &got); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"history", "model_context", "agent_context"} {
				if string(got[field]) != string(value) {
					t.Errorf("%s changed from %s to %s", field, value, got[field])
				}
			}
		})
	}
}
