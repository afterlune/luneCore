package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestOpenMigratesToolInvocationLedgerWithExecutionID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, workspace TEXT NOT NULL, updated_at INTEGER NOT NULL, data BLOB NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE tool_invocations (
			run_id TEXT NOT NULL, invocation_id TEXT NOT NULL, session_id TEXT NOT NULL, agent_id TEXT NOT NULL,
			call_id TEXT NOT NULL, name TEXT NOT NULL, arguments BLOB NOT NULL, status TEXT NOT NULL,
			output BLOB, updated_at INTEGER NOT NULL, PRIMARY KEY(run_id, invocation_id))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open v2 database: %v", err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, currentSchemaVersion)
	}
	var executionColumn bool
	rows, err := store.db.Query(`PRAGMA table_info(tool_invocations)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "execution_id" {
			executionColumn = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !executionColumn {
		t.Fatal("schema migration did not add execution_id")
	}
}

func TestToolInvocationUnknownOutcomeRequiresExplicitResolution(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	initial := ToolInvocation{
		RunID: "run-1", InvocationID: "tool-1", SessionID: "session-1", ExecutionID: "execution-1", AgentID: "lead",
		CallID: "call-1", Name: "command", Arguments: json.RawMessage(`{"command":"touch marker"}`),
	}
	recorded, created, err := store.StartToolInvocation(ctx, initial)
	if err != nil || !created || recorded.Status != ToolInvocationRunning {
		t.Fatalf("StartToolInvocation = %#v, created=%v, err=%v", recorded, created, err)
	}
	if err := store.MarkToolInvocationUnknown(ctx, initial.RunID, initial.InvocationID); err != nil {
		t.Fatalf("MarkToolInvocationUnknown: %v", err)
	}
	if err := store.ClaimToolInvocationRetry(ctx, initial.RunID, initial.InvocationID); err == nil {
		t.Fatal("claimed a retry before the host resolved the unknown outcome")
	}
	if err := store.ResolveToolInvocation(ctx, initial.SessionID, initial.RunID, initial.InvocationID, ToolInvocationRetry, nil); err != nil {
		t.Fatalf("ResolveToolInvocation retry: %v", err)
	}
	if err := store.ClaimToolInvocationRetry(ctx, initial.RunID, initial.InvocationID); err != nil {
		t.Fatalf("ClaimToolInvocationRetry: %v", err)
	}
	output := json.RawMessage(`{"call_id":"call-1","output":"done"}`)
	if err := store.SaveToolInvocationOutput(ctx, initial.RunID, initial.InvocationID, ToolInvocationCompleted, output); err != nil {
		t.Fatalf("SaveToolInvocationOutput: %v", err)
	}
	replayed, created, err := store.StartToolInvocation(ctx, initial)
	if err != nil || created || replayed.Status != ToolInvocationCompleted || string(replayed.Output) != string(output) {
		t.Fatalf("replayed invocation = %#v, created=%v, err=%v", replayed, created, err)
	}
}

func TestToolInvocationCanBeSkippedWithoutExecution(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	initial := ToolInvocation{
		RunID: "run-2", InvocationID: "tool-2", SessionID: "session-2", ExecutionID: "execution-2", AgentID: "lead",
		CallID: "call-2", Name: "mcp__server__write", Arguments: json.RawMessage(`{}`),
	}
	if _, _, err := store.StartToolInvocation(ctx, initial); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkToolInvocationUnknown(ctx, initial.RunID, initial.InvocationID); err != nil {
		t.Fatal(err)
	}
	output := json.RawMessage(`{"call_id":"call-2","output":"skipped"}`)
	if err := store.ResolveToolInvocation(ctx, initial.SessionID, initial.RunID, initial.InvocationID, ToolInvocationSkipped, output); err != nil {
		t.Fatalf("ResolveToolInvocation skip: %v", err)
	}
	stored, created, err := store.StartToolInvocation(ctx, initial)
	if err != nil || created || stored.Status != ToolInvocationSkipped || string(stored.Output) != string(output) {
		t.Fatalf("skipped invocation = %#v, created=%v, err=%v", stored, created, err)
	}
}
