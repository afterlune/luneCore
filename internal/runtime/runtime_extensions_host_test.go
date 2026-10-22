package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
)

func TestCoreAcceptsHostProviderAndRegisteredTool(t *testing.T) {
	hostTool := &hostEchoTool{}
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		hostToolCallResponse("select-host", "select_tool_category", `{"category":"host"}`),
		hostToolCallResponse("host-call", "host_echo", `{}`),
		hostTextResponse("finished"),
	}}
	client := newProgrammaticCore(t, provider, publictool.Registration{
		Tool: hostTool,
		Metadata: publictool.Metadata{
			Category:        publictool.Category{Name: "host", Description: "Host supplied tools."},
			PermissionClass: publictool.PermissionWorkspaceRead, ReadOnly: true,
		},
	})
	session, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	eventStream := subscribeTestEvents(t, client, session.ID)
	input, err := NewTaskInput("use the host tool")
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.RunTask(context.Background(), TaskRequest{SessionID: session.ID, Input: input})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	var events []Event
	for {
		event := <-eventStream
		events = append(events, event)
		if event.Kind == EventTaskFinished {
			break
		}
	}
	var result publicmodel.ModelResponse
	if err := json.Unmarshal(response.Response, &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(result.Output) != 1 {
		t.Fatalf("response output = %#v", result.Output)
	}
	hostTool.mu.Lock()
	calls, invocationID := hostTool.calls, hostTool.invocationID
	hostTool.mu.Unlock()
	if calls != 1 || invocationID == "" {
		t.Fatalf("host tool calls=%d invocationID=%q", calls, invocationID)
	}
	state, ok := client.StateForSession(session.ID)
	if !ok || state.Status != TaskCompleted {
		t.Fatalf("session task state = %#v, found=%v", state, ok)
	}
	var started, finished bool
	for _, event := range events {
		if event.SessionID != session.ID {
			continue
		}
		if event.Kind == EventToolStarted && event.Tool.ID == "host-call" && event.Tool.InvocationID == invocationID {
			started = true
		}
		if event.Kind == EventToolFinished && event.Tool.ID == "host-call" && event.Tool.InvocationID == invocationID {
			finished = true
		}
	}
	if !started || !finished {
		t.Fatalf("host tool lifecycle events started=%v finished=%v", started, finished)
	}
}

func TestProgrammaticModelConfigOverridesFileAndStateDirOwnsMCPState(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	project := projectSettingsForTest(
		map[string]model.ProviderConfig{
			"file-provider": {Protocol: model.ProtocolResponses, BaseURL: "https://file-provider.invalid", APIKey: "file-key"},
			"host":          {Protocol: model.ProtocolResponses, BaseURL: "https://old-host.invalid", APIKey: "file-key"},
		},
		map[string]model.ModelProfile{
			"file-profile": {Provider: "file-provider", Model: "file-model"},
			"host-profile": {Provider: "host", Model: "old-model"},
		},
		"file-profile", projectconfig.RuntimeConfig{ContextWindow: 24000, AutoCompact: true},
	)
	writeProjectSettingsConfig(t, workspace, project)
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{hostTextResponse("unused")}}
	client, err := New(Config{
		Workspace: workspace, StateDir: stateDir,
		RestoreEnabledMCP: true,
		Memory:            MemoryConfig{Disabled: true},
		ModelConfig: &publicmodel.Config{
			Providers:      map[string]publicmodel.ProviderConfig{"host": {Protocol: publicmodel.ProtocolResponses}},
			Profiles:       map[string]publicmodel.ModelProfile{"host-profile": {Provider: "host", Model: "new-model"}},
			DefaultProfile: "host-profile",
		},
		ModelProviders: map[string]publicmodel.Provider{"host": provider},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	profiles := client.ListModelProfiles()
	if len(profiles) != 2 || profiles[0].Profile != "file-profile" || profiles[1].Model != "new-model" {
		t.Fatalf("merged profiles = %#v", profiles)
	}
	if defaults := client.ProjectContextDefaults(); defaults.ContextWindow != 24000 {
		t.Fatalf("file project defaults = %#v", defaults)
	}
	if err := client.DisableMCPServer("not-configured"); err != nil {
		t.Fatalf("DisableMCPServer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "mcp-enabled.json")); err != nil {
		t.Fatalf("MCP state in custom StateDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".luneagent", "mcp-enabled.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected MCP state in workspace default directory, stat err=%v", err)
	}
}

func TestCoreIgnoresMCPEnabledStateByDefault(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeSettingsConfig(t, workspace, "http://127.0.0.1:1")
	statePath := filepath.Join(stateDir, "mcp-enabled.json")
	if err := os.WriteFile(statePath, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}

	client, err := New(Config{
		Workspace: workspace,
		StateDir:  stateDir,
		Memory:    MemoryConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("New with default MCP restore policy: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if data, err := os.ReadFile(statePath); err != nil || string(data) != "not json" {
		t.Fatalf("Core changed MCP state file: data=%q err=%v", data, err)
	}
}

func TestCoreReadsMCPEnabledStateWhenRestoreOptedIn(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeSettingsConfig(t, workspace, "http://127.0.0.1:1")
	statePath := filepath.Join(stateDir, "mcp-enabled.json")
	if err := os.WriteFile(statePath, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}

	client, err := New(Config{
		Workspace:         workspace,
		StateDir:          stateDir,
		Memory:            MemoryConfig{Disabled: true},
		RestoreEnabledMCP: true,
	})
	if err == nil {
		_ = client.Close()
		t.Fatal("New with opted-in MCP restore accepted malformed state")
	}
}
