package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/afterlune/luneCore/internal/agentgraph"
	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
)

func writeSettingsConfig(t *testing.T, workspace, endpoint string) {
	t.Helper()
	config := projectconfig.Config{
		Providers: map[string]model.ProviderConfig{
			"provider-a": {Protocol: model.ProtocolResponses, BaseURL: endpoint, APIKey: "key-a"},
			"provider-b": {Protocol: model.ProtocolResponses, BaseURL: endpoint, APIKey: "key-b"},
		},
		Models: map[string]model.ModelProfile{
			"primary": {Provider: "provider-a", Model: "primary-wire-model"},
			"review":  {Provider: "provider-b", Model: "review-wire-model"},
		},
		DefaultModel: "primary",
	}
	writeProjectSettingsConfig(t, workspace, config)
}

func writeProjectSettingsConfig(t *testing.T, workspace string, config projectconfig.Config) {
	t.Helper()
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, projectconfig.FileName), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func openSettingsCore(t *testing.T, workspace, stateDir string) *Core {
	t.Helper()
	client, err := New(Config{Workspace: workspace, StateDir: stateDir})
	if err != nil {
		t.Fatalf("New Core: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close Core: %v", err)
		}
	})
	return client
}

func TestSessionModelSettingsPersistAndDefault(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeSettingsConfig(t, workspace, server.URL)
	client := openSettingsCore(t, workspace, stateDir)

	session, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if session.Settings.ModelProfile != "primary" || session.Settings.ReasoningEffort != "" {
		t.Fatalf("new session settings = %#v", session.Settings)
	}
	settings := SessionSettings{ModelProfile: "review", ReasoningEffort: model.ReasoningEffortHigh}
	if err := client.SetSessionSettings(context.Background(), session.ID, settings); err != nil {
		t.Fatalf("SetSessionSettings: %v", err)
	}
	if err := client.SetSessionSettings(context.Background(), session.ID, SessionSettings{ModelProfile: "missing", ReasoningEffort: "high"}); err == nil {
		t.Fatal("unknown profile should be rejected")
	}
	if err := client.SetSessionSettings(context.Background(), session.ID, SessionSettings{ModelProfile: "primary", ReasoningEffort: "ultra"}); err == nil {
		t.Fatal("unknown effort should be rejected")
	}
	profiles := client.ListModelProfiles()
	if len(profiles) != 2 || profiles[0].Profile != "primary" || profiles[1].Profile != "review" {
		t.Fatalf("model profiles = %#v, want sorted profiles", profiles)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close Core before reopen: %v", err)
	}

	reopened := openSettingsCore(t, workspace, stateDir)
	restored, err := reopened.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("GetSession after reopen: %v", err)
	}
	if restored.Settings != settings {
		t.Fatalf("restored settings = %#v, want %#v", restored.Settings, settings)
	}
	newSession, err := reopened.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatalf("CreateSession after reopen: %v", err)
	}
	if newSession.Settings.ModelProfile != "primary" || newSession.Settings.ReasoningEffort != "" {
		t.Fatalf("new session defaults = %#v", newSession.Settings)
	}
}

func TestSessionTaskSettingsInheritProjectDefaultsAndPersistOverrides(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	workspace, stateDir := t.TempDir(), t.TempDir()
	project := projectconfig.Config{
		Providers: map[string]model.ProviderConfig{
			"provider-a": {Protocol: model.ProtocolResponses, BaseURL: server.URL, APIKey: "key-a"},
			"provider-b": {Protocol: model.ProtocolResponses, BaseURL: server.URL, APIKey: "key-b"},
		},
		Models: map[string]model.ModelProfile{
			"primary": {Provider: "provider-a", Model: "primary-wire-model"},
			"review":  {Provider: "provider-b", Model: "review-wire-model"},
		},
		DefaultModel: "primary", ContextWindow: 64000, ReserveOutputTokens: 4096,
		AutoCompact: true, AutoCompactThreshold: 0.75,
	}
	writeProjectSettingsConfig(t, workspace, project)
	client := openSettingsCore(t, workspace, stateDir)
	if defaults := client.ProjectContextDefaults(); defaults != (ContextDefaults{
		ContextWindow: 64000, ReserveOutputTokens: 4096, AutoCompact: true, AutoCompactThreshold: 0.75,
	}) {
		t.Fatalf("project context defaults = %#v", defaults)
	}
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}

	effective, err := client.resolveTaskSettings(saved.Settings)
	if err != nil {
		t.Fatalf("resolve project defaults: %v", err)
	}
	wantDefaults := taskSettings{
		ContextWindow: 64000, ReserveOutputTokens: 4096,
		AutoCompact: true, AutoCompactThreshold: 0.75,
	}
	if effective != wantDefaults {
		t.Fatalf("effective defaults = %#v, want %#v", effective, wantDefaults)
	}

	settings := SessionSettings{
		ModelProfile: "review", Goal: "  inspect the provider boundary  ",
		ContextWindow: int64Pointer(0), ReserveOutputTokens: int64Pointer(0),
		AutoCompact: boolPointer(false), AutoCompactThreshold: float64Pointer(0.6),
	}
	if err := client.SetSessionSettings(context.Background(), saved.ID, settings); err != nil {
		t.Fatalf("SetSessionSettings: %v", err)
	}
	settings.Goal = "inspect the provider boundary"
	restored, err := client.GetSession(context.Background(), saved.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !reflect.DeepEqual(restored.Settings, settings) {
		t.Fatalf("stored settings = %#v, want %#v", restored.Settings, settings)
	}
	effective, err = client.resolveTaskSettings(restored.Settings)
	if err != nil {
		t.Fatalf("resolve overrides: %v", err)
	}
	wantOverrides := taskSettings{Goal: "inspect the provider boundary", ContextWindow: 0, ReserveOutputTokens: 0, AutoCompact: false, AutoCompactThreshold: 0.6}
	if effective != wantOverrides {
		t.Fatalf("effective overrides = %#v, want %#v", effective, wantOverrides)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close before reopen: %v", err)
	}

	reopened := openSettingsCore(t, workspace, stateDir)
	afterReopen, err := reopened.GetSession(context.Background(), saved.ID)
	if err != nil {
		t.Fatalf("GetSession after reopen: %v", err)
	}
	if !reflect.DeepEqual(afterReopen.Settings, settings) {
		t.Fatalf("settings after reopen = %#v, want %#v", afterReopen.Settings, settings)
	}
}

func TestSessionTaskSettingsRejectInvalidOverrides(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeSettingsConfig(t, workspace, server.URL)
	client := openSettingsCore(t, workspace, stateDir)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	for _, settings := range []SessionSettings{
		{ContextWindow: int64Pointer(-1)},
		{ReserveOutputTokens: int64Pointer(-1)},
		{ContextWindow: int64Pointer(1000), ReserveOutputTokens: int64Pointer(1000)},
		{AutoCompactThreshold: float64Pointer(0)},
		{AutoCompactThreshold: float64Pointer(1.1)},
		{AutoCompactThreshold: float64Pointer(math.NaN())},
		{AutoCompactThreshold: float64Pointer(math.Inf(1))},
	} {
		if err := client.SetSessionSettings(context.Background(), saved.ID, settings); err == nil {
			t.Errorf("SetSessionSettings accepted invalid override %#v", settings)
		}
	}
}

func TestLegacySessionWithoutTaskOverridesInheritsProjectDefaults(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	workspace, stateDir := t.TempDir(), t.TempDir()
	project := projectconfig.Config{
		Providers: map[string]model.ProviderConfig{
			"provider-a": {Protocol: model.ProtocolResponses, BaseURL: server.URL, APIKey: "key-a"},
		},
		Models: map[string]model.ModelProfile{
			"primary": {Provider: "provider-a", Model: "primary-wire-model"},
		},
		DefaultModel: "primary", ContextWindow: 32000, ReserveOutputTokens: 2048, AutoCompact: true,
	}
	writeProjectSettingsConfig(t, workspace, project)
	store, err := session.Open(filepath.Join(stateDir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), session.Snapshot{
		ID: "legacy-settings", Workspace: workspace,
		Data: json.RawMessage(`{"history":null,"settings":{"model_profile":"primary"}}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	client := openSettingsCore(t, workspace, stateDir)
	saved, err := client.GetSession(context.Background(), "legacy-settings")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Settings.ContextWindow != nil || saved.Settings.ReserveOutputTokens != nil || saved.Settings.AutoCompact != nil || saved.Settings.AutoCompactThreshold != nil {
		t.Fatalf("legacy session should have no explicit task overrides: %#v", saved.Settings)
	}
	effective, err := client.resolveTaskSettings(saved.Settings)
	if err != nil {
		t.Fatalf("resolve legacy session settings: %v", err)
	}
	want := taskSettings{ContextWindow: 32000, ReserveOutputTokens: 2048, AutoCompact: true, AutoCompactThreshold: 0.8}
	if effective != want {
		t.Fatalf("legacy session effective settings = %#v, want %#v", effective, want)
	}
}

func int64Pointer(value int64) *int64       { return &value }
func boolPointer(value bool) *bool          { return &value }
func float64Pointer(value float64) *float64 { return &value }

func TestLegacySessionWithoutSettingsGetsDefaultProfile(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeSettingsConfig(t, workspace, server.URL)
	store, err := session.Open(filepath.Join(stateDir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), session.Snapshot{
		ID: "legacy-session", Workspace: workspace, Data: json.RawMessage(`{"history":null}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	client := openSettingsCore(t, workspace, stateDir)
	restored, err := client.GetSession(context.Background(), "legacy-session")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if restored.Settings.ModelProfile != "primary" || restored.Settings.ReasoningEffort != "" {
		t.Fatalf("legacy session settings = %#v", restored.Settings)
	}
}

func TestPausedTaskResumeUsesCheckpointModelAndEffort(t *testing.T) {
	observed := make(chan model.ModelRequest, 2)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request model.ModelRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		select {
		case observed <- request:
		default:
		}
		callNumber := calls.Add(1)
		if callNumber == 1 {
			<-r.Context().Done()
			return
		}
		stage := acceptanceRequestStage(request)
		if stage == "criteria" || stage == "review" {
			response := scriptedCriteriaResponse(request)
			if stage == "review" {
				response = scriptedAcceptanceResponse(request)
			}
			if err := writeCompletedSSE(w, response); err != nil {
				t.Errorf("write acceptance response: %v", err)
			}
			return
		}
		writeCompletedResponseSSE(w, request.Model)
	}))
	defer server.Close()
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeSettingsConfig(t, workspace, server.URL)
	client := openSettingsCore(t, workspace, stateDir)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	initial := SessionSettings{
		ModelProfile: "primary", ReasoningEffort: model.ReasoningEffortLow,
		Goal: "preserve this goal", ContextWindow: int64Pointer(32000), ReserveOutputTokens: int64Pointer(2048),
		AutoCompact: boolPointer(true), AutoCompactThreshold: float64Pointer(0.7),
	}
	if err := client.SetSessionSettings(context.Background(), saved.ID, initial); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	input := mustSettingsTaskInput(t, "pause me")
	const taskModel = "task-specific-model"
	go func() {
		_, runErr := client.RunTask(ctx, TaskRequest{SessionID: saved.ID, Input: input, Model: taskModel})
		firstDone <- runErr
	}()
	select {
	case first := <-observed:
		if first.Model != taskModel || first.Reasoning == nil || first.Reasoning.Effort != model.ReasoningEffortLow {
			cancel()
			t.Fatalf("initial request = %#v", first)
		}
		if active := client.State(); active.Model != taskModel {
			cancel()
			t.Fatalf("active task model = %q, want %q", active.Model, taskModel)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("initial request was not observed")
	}
	cancel()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted task error = %v, want context canceled", err)
	}
	if pending, err := client.hasPending(context.Background(), saved.ID); err != nil || !pending {
		t.Fatalf("pending after interruption = %v, error = %v", pending, err)
	}
	pausedState, ok, err := client.runtime.PendingStateFor(context.Background(), saved.ID)
	if err != nil || !ok {
		t.Fatalf("PendingStateFor after interruption: found=%v error=%v", ok, err)
	}
	if pausedState.ModelOverride != taskModel {
		t.Fatalf("checkpoint model override = %q, want %q", pausedState.ModelOverride, taskModel)
	}
	if pausedState.ExecutionID == "" {
		t.Fatal("paused checkpoint has no stable task execution ID")
	}
	assertTaskSettingsSnapshot(t, pausedState, taskSettings{
		Goal: "preserve this goal", ContextWindow: 32000, ReserveOutputTokens: 2048,
		AutoCompact: true, AutoCompactThreshold: 0.7,
	})
	if err := client.SetSessionSettings(context.Background(), saved.ID, SessionSettings{
		ModelProfile: "review", ReasoningEffort: model.ReasoningEffortHigh,
		Goal: "changed after pause", ContextWindow: int64Pointer(16000), ReserveOutputTokens: int64Pointer(1024),
		AutoCompact: boolPointer(false), AutoCompactThreshold: float64Pointer(0.9),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ResumeTask(context.Background(), saved.ID); err != nil {
		t.Fatalf("ResumeTask: %v", err)
	}
	select {
	case resumed := <-observed:
		if resumed.Model != taskModel || resumed.Reasoning == nil || resumed.Reasoning.Effort != model.ReasoningEffortLow {
			t.Fatalf("resumed request = %#v; want pinned original settings", resumed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resumed request was not observed")
	}
	resumedState, found := client.StateForSession(saved.ID)
	if !found {
		t.Fatal("resumed task state was not retained")
	}
	assertTaskOptionsSnapshot(t, agentgraph.TaskOptions{
		Goal: resumedState.Goal, ContextWindow: resumedState.ContextWindow, ReserveOutputTokens: resumedState.ReserveOutputTokens,
		AutoCompact: resumedState.AutoCompact, AutoCompactThreshold: resumedState.AutoCompactThreshold,
	}, taskSettings{
		Goal: "preserve this goal", ContextWindow: 32000, ReserveOutputTokens: 2048,
		AutoCompact: true, AutoCompactThreshold: 0.7,
	})
	if resumedState.ModelOverride != taskModel {
		t.Fatalf("resumed task model override = %q, want %q", resumedState.ModelOverride, taskModel)
	}
	if resumedState.ExecutionID != pausedState.ExecutionID {
		t.Fatalf("resumed execution ID = %q, want stable ID %q", resumedState.ExecutionID, pausedState.ExecutionID)
	}
	current, err := client.GetSession(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Settings.ModelProfile != "review" || current.Settings.ReasoningEffort != model.ReasoningEffortHigh {
		t.Fatalf("session settings after resume = %#v", current.Settings)
	}
	if len(current.UsageRequests) != 3 {
		t.Fatalf("usage requests = %#v; want criteria, execution, and acceptance requests", current.UsageRequests)
	}
	for _, request := range current.UsageRequests {
		if request.Model != taskModel {
			t.Fatalf("usage request model = %q, want pinned task model %q", request.Model, taskModel)
		}
	}
}

func assertTaskSettingsSnapshot(t *testing.T, state agentgraph.State, want taskSettings) {
	t.Helper()
	if state.TaskSettingsVersion != agentgraph.CurrentTaskSettingsVersion {
		t.Fatalf("task settings version = %d, want %d", state.TaskSettingsVersion, agentgraph.CurrentTaskSettingsVersion)
	}
	got := taskSettings{
		Goal: state.Goal, ContextWindow: state.ContextWindow, ReserveOutputTokens: state.ReserveOutputTokens,
		AutoCompact: state.AutoCompact, AutoCompactThreshold: state.AutoCompactThreshold,
	}
	if got != want {
		t.Fatalf("checkpoint task settings = %#v, want %#v", got, want)
	}
}

func assertTaskOptionsSnapshot(t *testing.T, options agentgraph.TaskOptions, want taskSettings) {
	t.Helper()
	got := taskSettings{
		Goal: options.Goal, ContextWindow: options.ContextWindow, ReserveOutputTokens: options.ReserveOutputTokens,
		AutoCompact: options.AutoCompact, AutoCompactThreshold: options.AutoCompactThreshold,
	}
	if got != want {
		t.Fatalf("task options = %#v, want %#v", got, want)
	}
}

func TestDefaultReasoningEffortIsOmittedFromModelRequest(t *testing.T) {
	observed := make(chan model.ModelRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request model.ModelRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		stage := acceptanceRequestStage(request)
		if stage == "criteria" || stage == "review" {
			response := scriptedCriteriaResponse(request)
			if stage == "review" {
				response = scriptedAcceptanceResponse(request)
			}
			if err := writeCompletedSSE(w, response); err != nil {
				t.Errorf("write acceptance response: %v", err)
			}
			return
		}
		select {
		case observed <- request:
		default:
		}
		writeCompletedResponseSSE(w, request.Model)
	}))
	defer server.Close()
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeSettingsConfig(t, workspace, server.URL)
	client := openSettingsCore(t, workspace, stateDir)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	input := mustSettingsTaskInput(t, "use the provider default")
	if _, err := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: input}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	request := <-observed
	if request.Model != "primary-wire-model" || request.Reasoning != nil {
		t.Fatalf("default request = %#v, want configured model and no reasoning override", request)
	}
}

func mustSettingsTaskInput(t *testing.T, text string) TaskInput {
	t.Helper()
	input, err := NewTaskInput(text)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func writeCompletedResponseSSE(w http.ResponseWriter, modelName string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"done\",\"object\":\"response\",\"status\":\"completed\",\"model\":%q,\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}]}}\n\n", modelName)
}
