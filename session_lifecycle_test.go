package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint"
)

func TestSessionTitleArchiveForkAndLatest(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeSettingsConfig(t, workspace, server.URL)
	client := openSettingsCore(t, workspace, stateDir)
	ctx := context.Background()

	history := json.RawMessage(`[{"type":"message","role":"user","content":"review this"}]`)
	source, err := client.CreateSession(ctx, history)
	if err != nil {
		t.Fatal(err)
	}
	settings := SessionSettings{ModelProfile: "review", ReasoningEffort: model.ReasoningEffortHigh}
	if err := client.SetSessionSettings(ctx, source.ID, settings); err != nil {
		t.Fatal(err)
	}
	if err := client.SetSessionTitle(ctx, source.ID, "  Review thread  "); err != nil {
		t.Fatal(err)
	}
	if err := client.SetSessionArchived(ctx, source.ID, true); err != nil {
		t.Fatal(err)
	}
	active, err := client.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	latest, found, err := client.LatestSession(ctx)
	if err != nil || !found || latest.ID != active.ID {
		t.Fatalf("LatestSession = %#v, found=%v, error=%v", latest, found, err)
	}
	ordinary, err := client.ListSessions(ctx, 30, false)
	if err != nil || len(ordinary) != 1 || ordinary[0].ID != active.ID {
		t.Fatalf("active sessions = %#v, error=%v", ordinary, err)
	}
	all, err := client.ListSessions(ctx, 30, true)
	if err != nil || len(all) != 2 {
		t.Fatalf("all sessions = %#v, error=%v", all, err)
	}
	archived, err := client.GetSession(ctx, source.ID)
	if err != nil || archived.Title != "Review thread" || !archived.Archived {
		t.Fatalf("archived session = %#v, error=%v", archived, err)
	}

	fork, err := client.ForkSession(ctx, source.ID)
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	if fork.ID == source.ID || string(fork.History) != string(history) || fork.Settings != settings || fork.Title != "" || fork.Archived {
		t.Fatalf("fork = %#v", fork)
	}
	if err := client.SetSessionTitle(ctx, fork.ID, " "); err != nil {
		t.Fatalf("clear title: %v", err)
	}
	if err := client.SetSessionTitle(ctx, fork.ID, strings.Repeat("界", 121)); err == nil {
		t.Fatal("title longer than 120 characters should be rejected")
	}
	cleared, err := client.GetSession(ctx, fork.ID)
	if err != nil || cleared.Title != "" {
		t.Fatalf("cleared title = %q, error=%v", cleared.Title, err)
	}
}

func TestSessionDataVersionMigratesLegacyPayloadAndRejectsFutureVersion(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeSettingsConfig(t, workspace, server.URL)
	client := openSettingsCore(t, workspace, stateDir)
	ctx := context.Background()

	legacyData := json.RawMessage(`{"history":[{"type":"message","role":"user","content":"old"}],"settings":{"model_profile":"review","reasoning_effort":"high"},"title":"Legacy thread","archived":true}`)
	legacyUpdatedAt := time.Now().Add(-time.Hour)
	if err := client.sessions.Save(ctx, session.Snapshot{
		ID: "legacy", Workspace: workspace, UpdatedAt: legacyUpdatedAt, Data: legacyData,
	}); err != nil {
		t.Fatalf("save legacy session: %v", err)
	}
	migrated, err := client.GetSession(ctx, "legacy")
	if err != nil {
		t.Fatalf("GetSession legacy payload: %v", err)
	}
	if migrated.DataVersion != CurrentSessionDataVersion || migrated.Title != "Legacy thread" || !migrated.Archived ||
		migrated.Settings.ModelProfile != "review" || migrated.Settings.ReasoningEffort != model.ReasoningEffortHigh || !strings.Contains(string(migrated.History), "old") {
		t.Fatalf("migrated session = %#v", migrated)
	}
	if !migrated.UpdatedAt.Equal(legacyUpdatedAt) {
		t.Fatalf("migration changed updated_at: got %s want %s", migrated.UpdatedAt, legacyUpdatedAt)
	}
	persisted, err := client.sessions.Get(ctx, "legacy", workspace)
	if err != nil {
		t.Fatal(err)
	}
	var persistedData struct {
		DataVersion int `json:"data_version"`
	}
	if err := json.Unmarshal(persisted.Data, &persistedData); err != nil {
		t.Fatal(err)
	}
	if persistedData.DataVersion != CurrentSessionDataVersion {
		t.Fatalf("persisted data_version = %d, want %d", persistedData.DataVersion, CurrentSessionDataVersion)
	}

	created, err := client.CreateSession(ctx, json.RawMessage("null"))
	if err != nil || created.DataVersion != CurrentSessionDataVersion || !created.AgentUsageComplete {
		t.Fatalf("new session data_version = %d, error=%v", created.DataVersion, err)
	}

	versionOne := json.RawMessage(`{"data_version":1,"history":null,"usage_requests":[{"request":204,"available":true,"agent_id":"lead","usage":{"input_tokens":80,"output_tokens":10,"total_tokens":90}},{"request":205,"available":true,"agent_id":"research","usage":{"input_tokens":50,"output_tokens":5,"total_tokens":55}}],"session_usage":{"requests":205,"measured":205,"input":10000}}`)
	if err := client.sessions.Save(ctx, session.Snapshot{ID: "version-one", Workspace: workspace, Data: versionOne}); err != nil {
		t.Fatalf("save version-one session: %v", err)
	}
	migratedV1, err := client.GetSession(ctx, "version-one")
	if err != nil {
		t.Fatalf("GetSession version-one payload: %v", err)
	}
	if migratedV1.DataVersion != CurrentSessionDataVersion || migratedV1.AgentUsageComplete ||
		migratedV1.AgentUsage["lead"].Requests != 1 || migratedV1.AgentUsage["lead"].Input != 80 ||
		migratedV1.AgentUsage["research"].Requests != 1 || migratedV1.AgentUsage["research"].Input != 50 {
		t.Fatalf("version-one usage migration = %#v", migratedV1)
	}

	for _, dataVersion := range []int{-1, CurrentSessionDataVersion + 1} {
		id := "version-" + strconv.Itoa(dataVersion)
		data, _ := json.Marshal(map[string]any{"data_version": dataVersion, "history": json.RawMessage("null")})
		if err := client.sessions.Save(ctx, session.Snapshot{ID: id, Workspace: workspace, Data: data}); err != nil {
			t.Fatal(err)
		}
		if _, err := client.GetSession(ctx, id); err == nil {
			t.Fatalf("GetSession accepted unsupported data_version %d", dataVersion)
		}
	}
}

func TestResumeRejectsCheckpointWithoutTaskSettingsSnapshot(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeSettingsConfig(t, workspace, server.URL)
	client := openSettingsCore(t, workspace, stateDir)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runID := client.runtime.checkpointID(saved.ID)
	if err := client.checkpoints.Create(context.Background(), graph.Checkpoint[agentgraph.State]{
		FormatVersion: graph.CheckpointFormatVersion,
		RunID:         runID,
		MachineID:     "lune-agent-v2",
		Revision:      1,
		NextID:        2,
		Invocations: []graph.Invocation[agentgraph.State]{
			{ID: "old-invocation", Node: "model_request", Status: graph.InvocationReady, State: agentgraph.State{RunID: runID}},
		},
	}); err != nil {
		t.Fatalf("create old checkpoint: %v", err)
	}

	if _, err := client.ResumeTask(context.Background(), saved.ID); err == nil || !strings.Contains(err.Error(), "task settings version 0") {
		t.Fatalf("ResumeTask error = %v, want unsupported settings checkpoint error", err)
	}
	if state := client.State(); state.Status != TaskIdle {
		t.Fatalf("task state after rejected resume = %#v, want idle", state)
	}
	if pending, err := client.hasPending(context.Background(), saved.ID); err != nil || !pending {
		t.Fatalf("checkpoint after rejected resume: pending=%v err=%v; want preserved", pending, err)
	}
	state, found, err := client.runtime.PendingStateFor(context.Background(), saved.ID)
	if err != nil || !found || state.TaskSettingsVersion != 0 {
		t.Fatalf("old checkpoint after rejection: found=%v state=%#v err=%v", found, state, err)
	}
}

func TestCoreUsesV7CheckpointFileAndLeavesLegacyFilesUntouched(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeSettingsConfig(t, workspace, server.URL)
	legacyCheckpoint := []byte("legacy-v6-checkpoint-file")
	legacyTeamRuns := []byte("legacy-team-run-file")
	if err := os.WriteFile(filepath.Join(stateDir, "checkpoints.db"), legacyCheckpoint, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "team_runs.db"), legacyTeamRuns, 0600); err != nil {
		t.Fatal(err)
	}
	client := openSettingsCore(t, workspace, stateDir)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{"checkpoints.db": legacyCheckpoint, "team_runs.db": legacyTeamRuns} {
		got, err := os.ReadFile(filepath.Join(stateDir, name))
		if err != nil || string(got) != string(want) {
			t.Fatalf("legacy file %q changed: %q, error=%v", name, got, err)
		}
	}
	if info, err := os.Stat(filepath.Join(stateDir, "checkpoints-v7.db")); err != nil || info.IsDir() {
		t.Fatalf("v7 checkpoint file missing: info=%v error=%v", info, err)
	}
}

func TestRewindSessionPreservesWholeTurnsAndRejectsPausedTasks(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeSettingsConfig(t, workspace, server.URL)
	client := openSettingsCore(t, workspace, stateDir)
	ctx := context.Background()
	history := json.RawMessage(`[
		{"type":"message","role":"user","content":"one"},
		{"type":"message","role":"assistant","content":"answer one"},
		{"type":"message","role":"user","content":"two"},
		{"type":"function_call","call_id":"call-1","name":"read_file","arguments":"{}"},
		{"type":"function_call_output","call_id":"call-1","output":"result"},
		{"type":"message","role":"assistant","content":"answer two"},
		{"type":"message","role":"user","content":"three"},
		{"type":"message","role":"assistant","content":"answer three"}
	]`)
	saved, err := client.CreateSession(ctx, history)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := client.RewindSession(ctx, saved.ID, 1)
	if err != nil || removed != 1 {
		t.Fatalf("RewindSession removed=%d, error=%v", removed, err)
	}
	updated, err := client.GetSession(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	var items []map[string]any
	if err := json.Unmarshal(updated.History, &items); err != nil {
		t.Fatalf("decode rewound history %s: %v", updated.History, err)
	}
	if len(items) != 6 || items[3]["type"] != "function_call" || items[4]["type"] != "function_call_output" {
		t.Fatalf("rewound history split a user turn or tool pair: %s", updated.History)
	}
	removed, err = client.RewindSession(ctx, saved.ID, 10)
	if err != nil || removed != 2 {
		t.Fatalf("oversized rewind removed=%d, error=%v; want all two remaining turns", removed, err)
	}
	updated, err = client.GetSession(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(updated.History)) != "[]" {
		t.Fatalf("rewind to start left history %s", updated.History)
	}
	if _, err := client.RewindSession(ctx, saved.ID, 0); err == nil {
		t.Fatal("zero rewind should be rejected")
	}

	checkpointID := client.runtime.checkpointID(saved.ID)
	createBareTestCheckpoint(t, client, checkpointID)
	if _, err := client.RewindSession(ctx, saved.ID, 1); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("rewind with pending task error = %v", err)
	}
}

func TestDeleteSessionDeletesItsGraphCheckpoint(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeSettingsConfig(t, workspace, server.URL)
	client := openSettingsCore(t, workspace, stateDir)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	parentID := client.runtime.checkpointID(saved.ID)
	createBareTestCheckpoint(t, client, parentID)
	client.mu.Lock()
	client.active = TaskState{Status: TaskRunning, SessionID: saved.ID}
	client.mu.Unlock()
	if err := client.DeleteSession(context.Background(), saved.ID); err == nil {
		t.Fatal("DeleteSession deleted a currently running session")
	}
	client.mu.Lock()
	client.active = TaskState{Status: TaskIdle}
	client.mu.Unlock()

	if err := client.DeleteSession(context.Background(), saved.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := client.GetSession(context.Background(), saved.ID); err == nil {
		t.Fatal("deleted session still exists")
	}
	if _, err := client.checkpoints.Load(context.Background(), parentID); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("checkpoint %q after delete: %v", parentID, err)
	}
	if err := client.DeleteSession(context.Background(), saved.ID); err == nil {
		t.Fatal("repeat DeleteSession should report the missing session")
	}
}

func createBareTestCheckpoint(t *testing.T, client *Core, runID string) {
	t.Helper()
	if err := client.checkpoints.Create(context.Background(), graph.Checkpoint[agentgraph.State]{
		FormatVersion: graph.CheckpointFormatVersion,
		RunID:         runID,
		MachineID:     "lune-agent-v2",
		Revision:      1,
	}); err != nil {
		t.Fatalf("create checkpoint %q: %v", runID, err)
	}
}
