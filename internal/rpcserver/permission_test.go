package rpcserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"testing"

	core "github.com/afterlune/luneCore"
	"github.com/afterlune/luneCore/model"
	"github.com/coder/websocket"
)

func TestPermissionRPCAndMaxMode(t *testing.T) {
	runtimeCore, err := core.New(core.Config{
		Workspace: t.TempDir(),
		StateDir:  t.TempDir(),
		Memory:    core.MemoryConfig{Disabled: true},
		ModelConfig: &model.Config{
			Providers: map[string]model.ProviderConfig{"fixture": {Protocol: model.ProtocolResponses}},
			Profiles:  map[string]model.ModelProfile{"fixture": {Provider: "fixture", Model: "fixture-model"}},
			DefaultProfile: "fixture",
		},
		ModelProviders: map[string]model.Provider{"fixture": &rpcBarrierProvider{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtimeCore.Close(context.Background()) })
	if err := runtimeCore.SetPermissionMode(core.PermissionWorkspace); err != nil {
		t.Fatal(err)
	}

	service, err := NewWithOptions(runtimeCore, "perm-token", ServerOptions{
		MaxPermissionMode: core.PermissionAuto,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(service)
	t.Cleanup(server.Close)
	rpc := dialTestRPC(t, server.URL, "perm-token", server.Certificate())
	t.Cleanup(rpc.close)

	var hello struct {
		PermissionMode string   `json:"permissionMode"`
		Capabilities   []string `json:"capabilities"`
	}
	decodeResult(t, rpc.call(t, "runtime.hello", map[string]any{"clientName": "Test", "clientVersion": "0.1"}), &hello)
	if hello.PermissionMode != string(core.PermissionWorkspace) {
		t.Fatalf("hello permissionMode = %q, want workspace", hello.PermissionMode)
	}
	if !slices.Contains(hello.Capabilities, "permission.manage") {
		t.Fatalf("missing permission.manage capability: %v", hello.Capabilities)
	}

	var current struct {
		Mode    string   `json:"mode"`
		Modes   []string `json:"modes"`
		MaxMode string   `json:"maxMode"`
	}
	decodeResult(t, rpc.call(t, "permission.get", map[string]any{}), &current)
	if current.Mode != "workspace" || current.MaxMode != "auto" || len(current.Modes) != 3 {
		t.Fatalf("permission.get = %+v", current)
	}

	var updated struct {
		Mode string `json:"mode"`
	}
	decodeResult(t, rpc.call(t, "permission.set", map[string]any{"mode": "auto"}), &updated)
	if updated.Mode != "auto" || runtimeCore.PermissionMode() != core.PermissionAuto {
		t.Fatalf("permission.set auto = %+v, core=%s", updated, runtimeCore.PermissionMode())
	}

	if reply := rpc.call(t, "permission.set", map[string]any{"mode": "full-access"}); reply.Error == nil {
		t.Fatal("expected full-access to be rejected when maxMode is auto")
	}
	if reply := rpc.call(t, "permission.set", map[string]any{"mode": "bogus"}); reply.Error == nil {
		t.Fatal("expected bogus permission mode to be rejected")
	}

	// Verify burst pipelined RPCs are not rejected with request limit reached.
	const burst = 24
	for i := 1; i <= burst; i++ {
		frame, marshalErr := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1000 + i, "method": "permission.get", "params": map[string]any{}})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if writeErr := rpc.conn.Write(rpc.ctx, websocket.MessageText, frame); writeErr != nil {
			t.Fatalf("pipelined write %d: %v", i, writeErr)
		}
	}
	for i := 1; i <= burst; i++ {
		_, data, readErr := rpc.conn.Read(rpc.ctx)
		if readErr != nil {
			t.Fatalf("pipelined read %d: %v", i, readErr)
		}
		var reply testRPCReply
		if decErr := json.Unmarshal(data, &reply); decErr != nil {
			t.Fatalf("pipelined decode %d: %v", i, decErr)
		}
		if reply.Error != nil {
			t.Fatalf("pipelined RPC %d failed: %+v", i, reply.Error)
		}
	}
}
