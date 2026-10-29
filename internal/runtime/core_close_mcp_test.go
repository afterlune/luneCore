package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/mcp"
	publicmodel "github.com/afterlune/luneCore/model"
)

func TestCoreCloseTimeoutKeepsStorageUntilMCPOperationExits(t *testing.T) {
	p := &goalProvider{respond: func(context.Context, int, publicmodel.ModelRequest) (*publicmodel.ModelResponse, error) {
		return hostTextResponse("unused"), nil
	}}
	client, rt := newGoalTestCore(t, p)
	client.queueDrainWG.Wait()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request hostMCPRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.Method == "tools/list" {
			writeHostMCPResult(w, request.ID, map[string]any{"tools": []any{}})
			return
		}
		writeHostMCPResult(w, request.ID, map[string]any{"resultType": "input_required", "requestState": "wait", "inputRequests": map[string]any{"input": map[string]any{"method": "elicitation/create", "params": map[string]any{"mode": "form", "message": "wait", "requestedSchema": map[string]any{"type": "object"}}}}})
	}))
	defer server.Close()
	manager, err := mcp.NewManager(mcp.Options{Workspace: client.workspace, StateDir: t.TempDir(), Registry: client.registry, Servers: map[string]projectconfig.MCPServerConfig{"host": {Transport: projectconfig.MCPTransportStreamableHTTP, URL: server.URL}}, RequestInteraction: func(context.Context, string, string, json.RawMessage) (json.RawMessage, error) {
		close(entered)
		<-release
		return nil, errors.New("interaction released")
	}})
	if err != nil {
		t.Fatal(err)
	}
	if client.mcpManager != nil {
		if err := client.mcpManager.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	client.mcpManager = manager
	if err := manager.Enable("host"); err != nil {
		t.Fatal(err)
	}
	hostDone := make(chan error, 1)
	go func() { _, err := manager.GetPrompt(t.Context(), "host", "test", nil); hostDone <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("host operation not entered")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := client.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close=%v", err)
	}
	if _, err := client.sessions.Get(t.Context(), rt.sessionID, client.workspace); err != nil {
		t.Fatalf("storage closed under live MCP: %v", err)
	}
	if _, err := client.CreateSession(t.Context(), nil); err == nil {
		t.Fatal("admission reopened after shutdown began")
	}
	retryCtx, retryCancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer retryCancel()
	if err := client.Close(retryCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retry did not join cleanup: %v", err)
	}
	unblock()
	finalCtx, finalCancel := context.WithTimeout(t.Context(), time.Second)
	defer finalCancel()
	if err := client.Close(finalCtx); err != nil {
		t.Fatal(err)
	}
	if err := <-hostDone; !errors.Is(err, mcp.ErrServerLifecycleChanged) {
		t.Fatalf("host error=%v", err)
	}
	if _, err := client.sessions.Get(t.Context(), rt.sessionID, client.workspace); err == nil {
		t.Fatal("storage remained open after cleanup")
	}
}
