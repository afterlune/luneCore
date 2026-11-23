package rpcserver

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	core "github.com/afterlune/luneCore"
	publicmodel "github.com/afterlune/luneCore/model"
)

func TestWorkspaceFileRPC(t *testing.T) {
	root := t.TempDir()
	config := workspaceFixtureConfig(&rpcBarrierProvider{})
	config.Workspace = root
	client, err := core.New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	if err := os.Mkdir(filepath.Join(root, "docs"), 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"docs/overview.md": []byte("# Example"),
		"binary":           {0, 255},
		"large":            make([]byte, workspacePreviewLimit+1),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for i := range workspacePageSize {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("entry-%03d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "outside")
	linkAvailable := os.Symlink(t.TempDir(), link) == nil

	service, err := New(client, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(service)
	t.Cleanup(server.Close)
	rpc := dialTestRPC(t, server.URL, "test-token", server.Certificate())
	t.Cleanup(rpc.close)
	var hello struct{ Capabilities []string }
	decodeResult(t, rpc.call(t, "runtime.hello", map[string]any{}), &hello)
	if !slices.Contains(hello.Capabilities, "workspace.files") {
		t.Fatal("workspace.files capability is missing")
	}
	var info struct{ Path string }
	decodeResult(t, rpc.call(t, "workspace.info", map[string]any{}), &info)
	if info.Path != root {
		t.Fatalf("workspace path = %q, want %q", info.Path, root)
	}
	var page struct {
		Items      []workspaceEntry
		More       bool
		NextOffset int
	}
	decodeResult(t, rpc.call(t, "workspace.listDirectory", map[string]any{}), &page)
	if len(page.Items) != workspacePageSize || !page.More || page.Items[0].Name != "docs" {
		t.Fatalf("unexpected first page: %d items, more %v", len(page.Items), page.More)
	}
	decodeResult(t, rpc.call(t, "workspace.listDirectory", map[string]any{"offset": page.NextOffset}), &page)
	if page.More || len(page.Items) != 3 {
		t.Fatalf("unexpected last page: %d items, more %v", len(page.Items), page.More)
	}
	var preview struct{ Text string }
	decodeResult(t, rpc.call(t, "workspace.readFile", map[string]string{"path": "docs/overview.md"}), &preview)
	if preview.Text != "# Example" {
		t.Fatalf("preview = %q", preview.Text)
	}
	for _, path := range []string{"binary", "large", "../overview.md", ".lune", ".lune/execution-format", "docs", filepath.Join(root, "docs", "overview.md")} {
		if reply := rpc.call(t, "workspace.readFile", map[string]string{"path": path}); reply.Error == nil {
			t.Errorf("unexpected access to %q", path)
		}
	}
	if linkAvailable {
		if reply := rpc.call(t, "workspace.listDirectory", map[string]string{"path": "outside"}); reply.Error == nil {
			t.Fatal("directory link was followed")
		}
	}
}

func TestWorkspaceManagerKeepsIndependentQueuesRunning(t *testing.T) {
	ctx := t.Context()
	provider := &rpcBarrierProvider{entered: make(chan struct{}, 4), release: make(chan struct{})}
	configure := func(string) (core.Config, error) {
		return workspaceFixtureConfig(provider), nil
	}
	firstPath, secondPath := t.TempDir(), t.TempDir()
	var manager core.WorkspaceManager
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	firstWorkspace, first, err := manager.OpenWorkspace(firstPath, configure)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, same, err := manager.OpenWorkspace(firstPath, nil)
	if err != nil || duplicate != firstWorkspace || same != first {
		t.Fatalf("workspace was not reused: %v", err)
	}
	secondWorkspace, second, err := manager.OpenWorkspace(secondPath, configure)
	if err != nil || firstWorkspace.ID == secondWorkspace.ID {
		t.Fatalf("second workspace = %v, %v", secondWorkspace, err)
	}
	firstSession, err := first.CreateSession(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.GetSession(ctx, firstSession.ID); err == nil {
		t.Fatal("second workspace could read first session")
	}
	submit := func(client *core.WorkspaceRuntime, id string) func() {
		t.Helper()
		if id == "" {
			session, err := client.CreateSession(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			id = session.ID
		}
		runtime, err := client.OpenSessionRuntime(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = runtime.Close(context.Background()) })
		submission, err := runtime.SubmitTurn(ctx, core.TurnRequest{Input: core.TaskInput{Raw: []byte(`"fixture"`)}}, core.StartIfIdle)
		if err != nil {
			t.Fatal(err)
		}
		return func() {
			if _, err := runtime.WaitTurn(ctx, submission.ExecutionID); err != nil {
				t.Fatal(err)
			}
		}
	}
	wait := []func(){submit(first, firstSession.ID), submit(first, ""), submit(second, "")}
	awaitProviderCalls(t, provider.entered, 3)
	close(provider.release)
	for _, finish := range wait {
		finish()
	}
}

func workspaceFixtureConfig(provider publicmodel.Provider) core.Config {
	return core.Config{
		Memory: core.MemoryConfig{Disabled: true},
		ModelConfig: &publicmodel.Config{
			Providers:      map[string]publicmodel.ProviderConfig{"fixture": {Protocol: "responses"}},
			Profiles:       map[string]publicmodel.ModelProfile{"fixture": {Provider: "fixture", Model: "fixture-model"}},
			DefaultProfile: "fixture",
		},
		ModelProviders: map[string]publicmodel.Provider{"fixture": provider},
	}
}
