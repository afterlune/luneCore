package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/mcp"
	publicmodel "github.com/afterlune/luneCore/model"
)

func TestCoreReloadProjectConfigUpdatesWithoutDroppingSubscription(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeSettingsConfig(t, workspace, "http://127.0.0.1:1")
	client := openSettingsCore(t, workspace, stateDir)

	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sr, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatalf("OpenSessionRuntime: %v", err)
	}
	t.Cleanup(func() { _ = sr.Close(context.Background()) })

	subCtx, cancelSub := context.WithCancel(context.Background())
	t.Cleanup(cancelSub)
	events, err := sr.SubscribeEventsWithReplayComplete(subCtx, 0)
	if err != nil {
		t.Fatalf("SubscribeEventsWithReplayComplete: %v", err)
	}
	select {
	case ev, ok := <-events:
		if !ok || ev.Kind != EventRuntimeReplayComplete {
			t.Fatalf("first event = (%#v, %v), want replay complete", ev, ok)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for replay complete")
	}

	loaded, err := projectconfig.Load(filepath.Join(workspace, projectconfig.FileName))
	if err != nil {
		t.Fatalf("load project config: %v", err)
	}
	next := loaded.ProjectConfig
	next.Models.Profiles["beta"] = publicmodel.ModelProfile{Provider: "provider-a", Model: "beta-model", DisplayName: "Beta"}
	next.Runtime.MaxParallelAgents = 6
	next.Runtime.MaxTeamChildrenPerAgent = 3
	next.Runtime.MaxTeamDepth = 2
	next.Attachments.MaxFiles = 9

	res, err := client.ReloadProjectConfig(context.Background(), next, "custom instructions")
	if err != nil {
		t.Fatalf("ReloadProjectConfig: %v", err)
	}
	if !res.Reloaded || res.MCPPending {
		t.Fatalf("ReloadProjectConfig result = %#v, want Reloaded=true MCPPending=false", res)
	}

	// Verify live event arrives and subscription stays open.
	select {
	case ev, ok := <-events:
		if !ok || ev.Kind != EventConfigReloaded {
			t.Fatalf("reload event = (%#v, %v), want %q", ev, ok, EventConfigReloaded)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for EventConfigReloaded")
	}

	if got := client.ExecutionDefaults(); got.MaxTeamChildrenPerAgent != 3 || got.MaxTeamDepth != 2 {
		t.Fatalf("ExecutionDefaults after reload = %#v", got)
	}
	if got := client.MaxParallelAgents(); got != 6 {
		t.Fatalf("MaxParallelAgents after reload = %d, want 6", got)
	}
	if got := client.ProjectAttachmentLimits(); got.MaxFiles != 9 {
		t.Fatalf("ProjectAttachmentLimits after reload = %#v, want MaxFiles=9", got)
	}
	if got := client.ListModelProfiles(); len(got) != 3 {
		t.Fatalf("ListModelProfiles count = %d, want 3 (%#v)", len(got), got)
	}

	// Simulate an active task: non-MCP updates apply immediately, MCP change is marked pending.
	client.mu.Lock()
	client.activeTasks[saved.ID] = TaskState{SessionID: saved.ID, TaskID: "busy-task", Status: TaskRunning}
	client.publishTaskSnapshotLocked()
	client.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = client.ListModelProfiles()
			_ = client.ExecutionDefaults()
			_ = client.MaxParallelAgents()
			_ = client.ProjectAttachmentLimits()
		}()
	}

	nextWithMCP := next
	nextWithMCP.Runtime.MaxTeamChildrenPerAgent = 4
	nextWithMCP.MCPServers = map[string]projectconfig.MCPServerConfig{
		"docs": {Transport: projectconfig.MCPTransportStreamableHTTP, URL: "https://example.invalid/mcp"},
	}
	busyRes, err := client.ReloadProjectConfig(context.Background(), nextWithMCP, "")
	if err != nil {
		t.Fatalf("ReloadProjectConfig while busy: %v", err)
	}
	wg.Wait()
	if !busyRes.Reloaded || !busyRes.MCPPending {
		t.Fatalf("busy ReloadProjectConfig = %#v, want Reloaded=true MCPPending=true", busyRes)
	}
	if got := client.ExecutionDefaults(); got.MaxTeamChildrenPerAgent != 4 {
		t.Fatalf("non-MCP runtime limit did not apply while busy: %#v", got)
	}
	select {
	case ev, ok := <-events:
		if !ok || ev.Kind != EventConfigReloaded {
			t.Fatalf("initial busy reload event = (%#v, %v), want %q", ev, ok, EventConfigReloaded)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for EventConfigReloaded on busy non-MCP swap")
	}

	// Repeating the same reload while still busy must stay MCPPending without spamming EventConfigReloaded.
	repeatBusyRes, err := client.ReloadProjectConfig(context.Background(), nextWithMCP, "")
	if err != nil {
		t.Fatalf("repeat busy ReloadProjectConfig: %v", err)
	}
	if !repeatBusyRes.Reloaded || !repeatBusyRes.MCPPending {
		t.Fatalf("repeat busy ReloadProjectConfig = %#v, want Reloaded=true MCPPending=true", repeatBusyRes)
	}
	select {
	case ev := <-events:
		t.Fatalf("unexpected event during repeated no-op busy reload: %#v", ev)
	case <-time.After(100 * time.Millisecond):
	}

	// Changing attachment limits while busy applies immediately without triggering MCPPending.
	nextAttachments := nextWithMCP
	nextAttachments.Attachments.MaxFiles = 12
	attachRes, err := client.ReloadProjectConfig(context.Background(), nextAttachments, "")
	if err != nil {
		t.Fatalf("attachment ReloadProjectConfig while busy: %v", err)
	}
	if !attachRes.Reloaded || !attachRes.MCPPending {
		t.Fatalf("attachment ReloadProjectConfig with unresolved MCP = %#v", attachRes)
	}
	if got := client.ProjectAttachmentLimits(); got.MaxFiles != 12 {
		t.Fatalf("attachment limits did not update while busy: %#v", got)
	}
	select {
	case ev, ok := <-events:
		if !ok || ev.Kind != EventConfigReloaded {
			t.Fatalf("attachment reload event = (%#v, %v), want %q", ev, ok, EventConfigReloaded)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for EventConfigReloaded on attachment swap")
	}

	client.mu.Lock()
	delete(client.activeTasks, saved.ID)
	client.publishTaskSnapshotLocked()
	client.mu.Unlock()

	idleRes, err := client.ReloadProjectConfig(context.Background(), nextAttachments, "")
	if err != nil {
		t.Fatalf("ReloadProjectConfig once idle: %v", err)
	}
	if !idleRes.Reloaded || idleRes.MCPPending {
		t.Fatalf("idle ReloadProjectConfig = %#v, want Reloaded=true MCPPending=false", idleRes)
	}
	select {
	case ev, ok := <-events:
		if !ok || ev.Kind != EventConfigReloaded {
			t.Fatalf("idle MCP reload event = (%#v, %v), want %q", ev, ok, EventConfigReloaded)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for EventConfigReloaded once idle")
	}

	// Updating only attachment limits when idle and MCP is already up-to-date must NOT set MCPPending even if busy later.
	client.mu.Lock()
	client.activeTasks[saved.ID] = TaskState{SessionID: saved.ID, TaskID: "busy-attach", Status: TaskRunning}
	client.publishTaskSnapshotLocked()
	client.mu.Unlock()
	onlyAttach := nextAttachments
	onlyAttach.Attachments.MaxFiles = 15
	onlyAttachRes, err := client.ReloadProjectConfig(context.Background(), onlyAttach, "")
	if err != nil {
		t.Fatalf("only attachment ReloadProjectConfig while busy: %v", err)
	}
	if !onlyAttachRes.Reloaded || onlyAttachRes.MCPPending {
		t.Fatalf("only attachment ReloadProjectConfig while busy = %#v, want Reloaded=true MCPPending=false", onlyAttachRes)
	}
	client.mu.Lock()
	delete(client.activeTasks, saved.ID)
	client.publishTaskSnapshotLocked()
	client.mu.Unlock()

	servers := client.MCPServers()
	if len(servers) != 1 || servers[0].Name != "docs" {
		t.Fatalf("MCPServers after UpdateServers = %#v, want [docs]", servers)
	}
	_ = mcp.Limits{}
}
