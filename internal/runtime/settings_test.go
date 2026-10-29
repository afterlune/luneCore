package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/afterlune/luneCore/internal/agentgraph"
	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
	publicmodel "github.com/afterlune/luneCore/model"
)

func writeSettingsConfig(t *testing.T, workspace, endpoint string) {
	t.Helper()
	config := projectconfig.ProjectConfig{
		Models: publicmodel.Config{
			Providers: map[string]publicmodel.ProviderConfig{
				"provider-a": {Protocol: model.ProtocolResponses, BaseURL: endpoint, APIKey: "key-a"},
				"provider-b": {Protocol: model.ProtocolResponses, BaseURL: endpoint, APIKey: "key-b"},
			},
			Profiles: map[string]publicmodel.ModelProfile{
				"primary": {Provider: "provider-a", Model: "primary-wire-model"},
				"review":  {Provider: "provider-b", Model: "review-wire-model"},
			},
			DefaultProfile: "primary",
		},
		Runtime: projectconfig.RuntimeConfig{AutoCompact: true},
	}
	writeProjectSettingsConfig(t, workspace, config)
}

func writeProjectSettingsConfig(t *testing.T, workspace string, config projectconfig.ProjectConfig) {
	t.Helper()
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, projectconfig.FileName), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func projectSettingsForTest(providers map[string]model.ProviderConfig, profiles map[string]model.ModelProfile, defaultProfile string, runtime projectconfig.RuntimeConfig) projectconfig.ProjectConfig {
	publicProviders := make(map[string]publicmodel.ProviderConfig, len(providers))
	for name, provider := range providers {
		publicProviders[name] = publicmodel.ProviderConfig{
			Protocol: provider.Protocol, BaseURL: provider.BaseURL, APIKey: provider.APIKey,
			ServerCompaction: provider.ServerCompaction,
		}
	}
	return projectconfig.ProjectConfig{
		Models:  publicmodel.Config{Providers: publicProviders, Profiles: profiles, DefaultProfile: defaultProfile},
		Runtime: runtime,
	}
}

func openSettingsCore(t *testing.T, workspace, stateDir string) *Core {
	t.Helper()
	client, err := New(Config{Workspace: workspace, StateDir: stateDir, Memory: MemoryConfig{Disabled: true}})
	if err != nil {
		t.Fatalf("New Core: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(context.Background()); err != nil {
			t.Errorf("close Core: %v", err)
		}
	})
	return client
}

func TestCoreDefaultsTeamTreeLimits(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeSettingsConfig(t, workspace, "http://127.0.0.1:1")
	client := openSettingsCore(t, workspace, stateDir)
	if client.executionDefaults.MaxTeamChildrenPerAgent != 0 || client.executionDefaults.MaxTeamDepth != agentgraph.DefaultMaxTeamDepth {
		t.Fatalf("default team execution limits = %#v", client.executionDefaults)
	}
	defaults := client.ExecutionDefaults()
	if defaults != client.executionDefaults {
		t.Fatalf("public execution defaults = %#v, want %#v", defaults, client.executionDefaults)
	}
	defaults.MaxTeamChildrenPerAgent = 99
	if client.ExecutionDefaults().MaxTeamChildrenPerAgent != 0 {
		t.Fatal("changing the returned defaults mutated Core")
	}
}

func TestCoreTeamTreeLimitPrecedence(t *testing.T) {
	workspace := t.TempDir()
	writeSettingsConfig(t, workspace, "http://127.0.0.1:1")
	project, err := projectconfig.Load(filepath.Join(workspace, projectconfig.FileName))
	if err != nil {
		t.Fatalf("load project config: %v", err)
	}
	project.Runtime.MaxTeamChildrenPerAgent = 7
	project.Runtime.MaxTeamDepth = 3
	writeProjectSettingsConfig(t, workspace, project.ProjectConfig)

	projectClient := openSettingsCore(t, workspace, t.TempDir())
	if got := projectClient.executionDefaults; got.MaxTeamChildrenPerAgent != 7 || got.MaxTeamDepth != 3 {
		t.Fatalf("project team limits = %#v", got)
	}

	hostClient, err := New(Config{
		Workspace: workspace, StateDir: t.TempDir(), Memory: MemoryConfig{Disabled: true},
		Execution: ExecutionOptions{MaxTeamChildrenPerAgent: 2, MaxTeamDepth: 1},
	})
	if err != nil {
		t.Fatalf("New with host team limits: %v", err)
	}
	t.Cleanup(func() {
		if err := hostClient.Close(context.Background()); err != nil {
			t.Errorf("close Core: %v", err)
		}
	})
	if got := hostClient.executionDefaults; got.MaxTeamChildrenPerAgent != 2 || got.MaxTeamDepth != 1 {
		t.Fatalf("host limits did not override project limits: %#v", got)
	}
}

func TestModelProfileContextWindowIsUsedAfterSessionAndProjectSettings(t *testing.T) {
	for _, test := range []struct {
		name          string
		projectWindow int64
		sessionWindow int64
		wantWindow    int64
	}{
		{name: "profile fallback", wantWindow: 96000},
		{name: "project override", projectWindow: 32000, wantWindow: 32000},
		{name: "session override", projectWindow: 32000, sessionWindow: 16000, wantWindow: 16000},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			config := projectconfig.ProjectConfig{
				Models: publicmodel.Config{
					Providers: map[string]publicmodel.ProviderConfig{
						"provider-a": {Protocol: model.ProtocolResponses, BaseURL: "http://127.0.0.1:1", APIKey: "key-a"},
					},
					Profiles: map[string]publicmodel.ModelProfile{
						"primary": {Provider: "provider-a", Model: "primary-wire-model", ContextWindow: 96000},
					},
					DefaultProfile: "primary",
				},
				Runtime: projectconfig.RuntimeConfig{ContextWindow: test.projectWindow, AutoCompact: true},
			}
			writeProjectSettingsConfig(t, workspace, config)
			client := openSettingsCore(t, workspace, t.TempDir())
			saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			if test.sessionWindow > 0 {
				window := test.sessionWindow
				if err := client.SetSessionSettings(context.Background(), saved.ID, SessionSettings{
					ModelProfile: "primary", ContextWindow: &window,
				}); err != nil {
					t.Fatalf("SetSessionSettings: %v", err)
				}
			}
			setup, err := client.prepareTaskRun(context.Background(), taskRunConfig{sessionID: saved.ID, maxRounds: 10})
			if err != nil {
				t.Fatalf("prepareTaskRun: %v", err)
			}
			if setup.taskOptions.ContextWindow != test.wantWindow {
				t.Fatalf("task context window = %d, want %d", setup.taskOptions.ContextWindow, test.wantWindow)
			}
		})
	}
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
	if err := client.SetSessionSettings(context.Background(), session.ID, SessionSettings{ModelProfile: "primary", ReasoningEffort: "default"}); err == nil {
		t.Fatal("default alias should be rejected; an unset effort uses the provider default")
	}
	profiles := client.ListModelProfiles()
	if len(profiles) != 2 || profiles[0].Profile != "primary" || profiles[1].Profile != "review" {
		t.Fatalf("model profiles = %#v, want sorted profiles", profiles)
	}
	if err := client.Close(context.Background()); err != nil {
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
	project := projectconfig.ProjectConfig{
		Models: publicmodel.Config{
			Providers: map[string]publicmodel.ProviderConfig{
				"provider-a": {Protocol: model.ProtocolResponses, BaseURL: server.URL, APIKey: "key-a"},
				"provider-b": {Protocol: model.ProtocolResponses, BaseURL: server.URL, APIKey: "key-b"},
			},
			Profiles: map[string]publicmodel.ModelProfile{
				"primary": {Provider: "provider-a", Model: "primary-wire-model"},
				"review":  {Provider: "provider-b", Model: "review-wire-model"},
			},
			DefaultProfile: "primary",
		},
		Runtime: projectconfig.RuntimeConfig{ContextWindow: 64000, ReserveOutputTokens: 4096, AutoCompact: true, AutoCompactThreshold: 0.75},
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
		ModelProfile:  "review",
		ContextWindow: int64Pointer(0), ReserveOutputTokens: int64Pointer(0),
		AutoCompact: boolPointer(false), AutoCompactThreshold: float64Pointer(0.6),
	}
	if err := client.SetSessionSettings(context.Background(), saved.ID, settings); err != nil {
		t.Fatalf("SetSessionSettings: %v", err)
	}
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
	wantOverrides := taskSettings{ContextWindow: 0, ReserveOutputTokens: 0, AutoCompact: false, AutoCompactThreshold: 0.6}
	if effective != wantOverrides {
		t.Fatalf("effective overrides = %#v, want %#v", effective, wantOverrides)
	}
	if err := client.Close(context.Background()); err != nil {
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
	project := projectconfig.ProjectConfig{
		Models: publicmodel.Config{
			Providers: map[string]publicmodel.ProviderConfig{
				"provider-a": {Protocol: model.ProtocolResponses, BaseURL: server.URL, APIKey: "key-a"},
			},
			Profiles: map[string]publicmodel.ModelProfile{
				"primary": {Provider: "provider-a", Model: "primary-wire-model"},
			},
			DefaultProfile: "primary",
		},
		Runtime: projectconfig.RuntimeConfig{ContextWindow: 32000, ReserveOutputTokens: 2048, AutoCompact: true},
	}
	writeProjectSettingsConfig(t, workspace, project)
	store, err := session.Open(filepath.Join(stateDir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), session.Snapshot{
		ID: "legacy-settings", Workspace: workspace,
		Data: json.RawMessage(`{"history":null,"settings":{"model_profile":"primary"}}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := UpgradeState(t.Context(), stateDir); err != nil {
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
	if err := store.Create(context.Background(), session.Snapshot{
		ID: "legacy-session", Workspace: workspace, Data: json.RawMessage(`{"history":null}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := UpgradeState(t.Context(), stateDir); err != nil {
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
	workspace, stateDir := t.TempDir(), t.TempDir()
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		askUserToolCallResponse("Which environment?", []string{"staging"}),
		hostTextResponse("Deployment ready."),
	}}
	config := runtimeTestConfig(workspace, stateDir, provider)
	config.ModelConfig = &publicmodel.Config{
		Providers: map[string]publicmodel.ProviderConfig{"host": {Protocol: publicmodel.ProtocolResponses}},
		Profiles: map[string]publicmodel.ModelProfile{
			"primary": {Provider: "host", Model: "primary-wire-model"},
			"review":  {Provider: "host", Model: "review-wire-model"},
		},
		DefaultProfile: "primary",
	}
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(context.Background()); err != nil {
			t.Errorf("close Core: %v", err)
		}
	})
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	initial := SessionSettings{
		ModelProfile: "primary", ReasoningEffort: model.ReasoningEffortLow,
		ContextWindow: int64Pointer(32000), ReserveOutputTokens: int64Pointer(2048),
		AutoCompact: boolPointer(true), AutoCompactThreshold: float64Pointer(0.7),
	}
	if err := client.SetSessionSettings(context.Background(), saved.ID, initial); err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	input := mustSettingsTaskInput(t, "pause for clarification")
	const taskModel = "task-specific-model"
	submission, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: input, Model: taskModel}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	pausedWait := waitTurnAsync(runtime, submission.ExecutionID)
	question := waitPendingUserQuestion(t, runtime, submission.ExecutionID)
	paused := waitTurnResult(t, pausedWait)
	if paused.err != nil || paused.result.Status != TaskPaused {
		t.Fatalf("task did not pause for clarification: %#v, error=%v", paused.result, paused.err)
	}
	if active := client.State(); active.Model != taskModel {
		t.Fatalf("active task model = %q, want %q", active.Model, taskModel)
	}
	pausedState, ok, err := client.runtime.PendingStateFor(context.Background(), saved.ID)
	if err != nil || !ok {
		t.Fatalf("PendingStateFor after question: found=%v error=%v", ok, err)
	}
	if pausedState.ModelOverride != taskModel {
		t.Fatalf("checkpoint model override = %q, want %q", pausedState.ModelOverride, taskModel)
	}
	if pausedState.ExecutionID == "" {
		t.Fatal("paused checkpoint has no stable task execution ID")
	}
	assertTaskSettingsSnapshot(t, pausedState, taskSettings{
		ContextWindow: 32000, ReserveOutputTokens: 2048,
		AutoCompact: true, AutoCompactThreshold: 0.7,
	})
	if err := client.SetSessionSettings(context.Background(), saved.ID, SessionSettings{
		ModelProfile: "review", ReasoningEffort: model.ReasoningEffortHigh,
		ContextWindow: int64Pointer(16000), ReserveOutputTokens: int64Pointer(1024),
		AutoCompact: boolPointer(false), AutoCompactThreshold: float64Pointer(0.9),
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.ResolveUserQuestion(context.Background(), UserQuestionAnswer{
		ExecutionID: question.ExecutionID, RunID: question.RunID, InvocationID: question.InvocationID, CallID: question.CallID,
		Answers: []UserQuestionResponse{{QuestionID: "q1", Answer: "staging"}},
	}); err != nil {
		t.Fatal(err)
	}
	resumed := waitTurnResult(t, waitTurnAsync(runtime, submission.ExecutionID))
	if resumed.err != nil || resumed.result.Status != TaskCompleted {
		t.Fatalf("resumed task = %#v, error=%v", resumed.result, resumed.err)
	}
	provider.mu.Lock()
	seenRequests := append([]publicmodel.ModelRequest(nil), provider.seenRequests...)
	provider.mu.Unlock()
	if len(seenRequests) != 2 {
		t.Fatalf("model requests = %d, want initial and resumed requests", len(seenRequests))
	}
	for _, request := range seenRequests {
		if request.Model != taskModel || request.Reasoning == nil || request.Reasoning.Effort != model.ReasoningEffortLow {
			t.Fatalf("request after resume = %#v; want pinned model and effort", request)
		}
	}
	resumedState, found := client.StateForSession(saved.ID)
	if !found {
		t.Fatal("resumed task state was not retained")
	}
	assertTaskOptionsSnapshot(t, agentgraph.TaskOptions{
		Goal: resumedState.Goal, ContextWindow: resumedState.ContextWindow, ReserveOutputTokens: resumedState.ReserveOutputTokens,
		AutoCompact: resumedState.AutoCompact, AutoCompactThreshold: resumedState.AutoCompactThreshold,
	}, taskSettings{
		ContextWindow: 32000, ReserveOutputTokens: 2048,
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
