package core

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	publicmodel "github.com/afterlune/luneCore/model"
)

func TestDeepSeekMCPInputProtocolsSmoke(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if apiKey == "" {
		t.Skip("set DEEPSEEK_API_KEY to run the live DeepSeek protocol smoke test")
	}

	modelID := strings.TrimSpace(os.Getenv("DEEPSEEK_MODEL"))
	if modelID == "" {
		modelID = "deepseek-flash"
	}

	server, expectedImageURL := newMCPPromptResourceFixture(t)

	modelConfig := &publicmodel.Config{
		Providers: map[string]publicmodel.ProviderConfig{
			"deepseek-responses": {
				Protocol: publicmodel.ProtocolResponses, BaseURL: "https://api.deepseek.com", APIKey: apiKey,
			},
			"deepseek-anthropic": {
				Protocol: publicmodel.ProtocolAnthropicMessages, BaseURL: "https://api.deepseek.com/anthropic", APIKey: apiKey,
			},
		},
		Models: map[string]publicmodel.ModelProfile{
			"deepseek-responses": {Provider: "deepseek-responses", Model: modelID, MaxOutputTokens: 1024},
			"deepseek-anthropic": {Provider: "deepseek-anthropic", Model: modelID, MaxOutputTokens: 1024},
		},
		DefaultModel: "deepseek-responses",
	}
	client, err := New(Config{
		Workspace: t.TempDir(), StateDir: t.TempDir(), ModelConfig: modelConfig,
		Memory: MemoryConfig{Disabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close Core: %v", err)
		}
	})
	attachHostMCPServer(t, client, "fixture", server.URL)
	session, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}

	prompt, err := runtime.GetMCPPrompt(context.Background(), "fixture", mcpFixturePromptName, nil)
	if err != nil {
		t.Fatalf("get MCP prompt: %v", err)
	}
	resourceInput, err := runtime.ReadMCPResource(context.Background(), "fixture", mcpFixtureResourceURI)
	if err != nil {
		t.Fatalf("read MCP resource: %v", err)
	}
	if prompt.Input == nil || resourceInput == nil || prompt.Input.Text != nil || resourceInput.Text != nil {
		t.Fatalf("MCP prompt/resource did not return item inputs: prompt=%#v resource=%#v", prompt.Input, resourceInput)
	}
	items := append([]publicmodel.InputItem{}, prompt.Input.Items...)
	items = append(items, resourceInput.Items...)
	combined := publicmodel.NewResponseInputItems(items...)
	if len(combined.Items) != 3 {
		t.Fatalf("combined MCP input has %d messages, want 3", len(combined.Items))
	}
	assertMCPPromptResourceInput(t, combined, expectedImageURL)
	taskInput, err := NewTaskInput(combined)
	if err != nil {
		t.Fatalf("wrap MCP input as task input: %v", err)
	}
	var roundTrip publicmodel.ResponseInput
	if err := json.Unmarshal(taskInput.Raw, &roundTrip); err != nil {
		t.Fatalf("round-trip public model input: %v", err)
	}
	if len(roundTrip.Items) != 3 {
		t.Fatalf("round-tripped input has %d items, want 3", len(roundTrip.Items))
	}
	assertMCPPromptResourceInput(t, &roundTrip, expectedImageURL)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for _, profileID := range []string{"deepseek-responses", "deepseek-anthropic"} {
		provider, err := client.models.NewClient(profileID, client.registry, client.logger)
		if err != nil {
			t.Fatalf("create %s client: %v", profileID, err)
		}
		response, err := provider.CreateResponse(ctx, publicmodel.ModelRequest{Model: modelID, Input: &roundTrip})
		if err != nil {
			t.Fatalf("%s request with model %q failed: %v", profileID, modelID, err)
		}
		if response == nil {
			t.Fatalf("%s returned a nil response", profileID)
		}
		if response.Status != "completed" {
			reason := ""
			if response.IncompleteDetails != nil {
				reason = response.IncompleteDetails.Reason
			}
			t.Fatalf("%s returned status %q (incomplete reason %q)", profileID, response.Status, reason)
		}
		if len(response.Output) == 0 {
			t.Fatalf("%s returned no output items", profileID)
		}
	}
}
