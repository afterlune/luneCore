package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
)

func TestDiscoveryAndHostContextPersistAcrossCoreRestart(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		hostToolCallResponse("original", "host_echo", `{}`), hostTextResponse("first done"), hostTextResponse("second done"),
	}}
	config := runtimeTestConfig(t.TempDir(), t.TempDir(), provider)
	config.Tools = []publictool.Registration{{Tool: &hostEchoTool{}, Metadata: publictool.Metadata{
		Category: publictool.Category{Name: "host", Description: "Host tools"}, PermissionClass: publictool.PermissionWorkspaceRead, ReadOnly: true,
	}}}
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	ctx := context.Background()
	saved, err := client.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	run := func(input string) {
		rt, err := client.OpenSessionRuntime(ctx, saved.ID)
		if err != nil {
			t.Fatal(err)
		}
		sub, err := rt.SubmitTurn(ctx, TurnRequest{Input: mustSettingsTaskInput(t, input)}, StartIfIdle)
		if err != nil {
			t.Fatal(err)
		}
		result, err := rt.WaitTurn(ctx, sub.ExecutionID)
		if err != nil || result.Status != TaskCompleted {
			t.Fatalf("turn: %s %v", result.Status, err)
		}
	}
	run("first")
	before, _ := client.GetSession(ctx, saved.ID)
	if !strings.Contains(string(before.AgentContext), `"host"`) || strings.Contains(string(before.History), "Lune runtime context") {
		t.Fatal("discovery missing or host snapshot rendered")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	client, err = New(config)
	if err != nil {
		t.Fatal(err)
	}
	run("second")
	provider.mu.Lock()
	requests := append([]publicmodel.ModelRequest(nil), provider.seenRequests...)
	provider.mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("requests=%d", len(requests))
	}
	if requests[0].Instructions != requests[2].Instructions {
		t.Fatal("restart changed instructions")
	}
	found := false
	for _, def := range requests[2].Tools {
		if fn, ok := def.(publicmodel.FunctionTool); ok && fn.Name == "host_echo" {
			found = true
		}
	}
	if !found {
		t.Fatal("restart discarded discovery")
	}
	after, _ := client.GetSession(ctx, saved.ID)
	last := after.UsageRequests[len(after.UsageRequests)-1]
	if last.RequestID == "" || last.RequestAttempt != 1 || len(last.RequestChanges) != 0 {
		t.Fatalf("trace=%#v", last)
	}
	if err := client.SetSessionHistory(ctx, saved.ID, json.RawMessage(`[]`)); err != nil {
		t.Fatal(err)
	}
	after, _ = client.GetSession(ctx, saved.ID)
	if len(after.AgentContext) > 0 && string(after.AgentContext) != "null" {
		t.Fatal("history replacement retained host state")
	}
}
