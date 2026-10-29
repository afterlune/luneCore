package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
)

func blockedDiscoveryManager(t *testing.T) (*Manager, <-chan struct{}, func()) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request testRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"echo","inputSchema":{"type":"object"}}]}}`, request.ID)
	}))
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() { unblock(); server.Close() })
	manager, err := NewManager(Options{
		Workspace: t.TempDir(), StateDir: t.TempDir(), Registry: testRegistry(t),
		Servers: map[string]projectconfig.MCPServerConfig{"local": {Transport: projectconfig.MCPTransportStreamableHTTP, URL: server.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	return manager, entered, unblock
}

func TestManagerInvalidatesDiscoveryOnDisableOrClose(t *testing.T) {
	for _, action := range []string{"disable", "close"} {
		t.Run(action, func(t *testing.T) {
			manager, entered, release := blockedDiscoveryManager(t)
			result := make(chan error, 1)
			go func() { result <- manager.Enable("local") }()
			if !waitUntil(entered, time.Now().Add(5*time.Second)) {
				t.Fatal("discovery did not start")
			}
			var err error
			if action == "disable" {
				err = manager.Disable("local")
			} else {
				err = manager.Close(context.Background())
			}
			if err != nil {
				t.Fatal(err)
			}
			release()
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("invalidated discovery succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("discovery cancellation blocked")
			}
			if len(manager.registry.DefinitionsForCategory("mcp")) != 0 {
				t.Fatal("invalidated discovery registered tools")
			}
			manager.mu.Lock()
			instance, enabled := manager.instances["local"], manager.enabled["local"]
			manager.mu.Unlock()
			if instance != nil || enabled {
				t.Fatal("invalidated connection was published")
			}
		})
	}
}

func TestManagerConnectionGateHonorsCancellation(t *testing.T) {
	manager, entered, release := blockedDiscoveryManager(t)
	result := make(chan error, 1)
	go func() { result <- manager.Enable("local") }()
	if !waitUntil(entered, time.Now().Add(5*time.Second)) {
		t.Fatal("discovery did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := manager.connectServer(ctx, "local", false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued connection error=%v", err)
	}
	release()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("discovery did not finish")
	}
}

func TestManagerConcurrentReconnectDoesNotReplayTool(t *testing.T) {
	dir := t.TempDir()
	marker, starts, calls := filepath.Join(dir, "exit"), filepath.Join(dir, "starts"), filepath.Join(dir, "calls")
	if err := os.WriteFile(marker, []byte("initial"), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Options{
		Workspace: dir, StateDir: t.TempDir(), Registry: testRegistry(t),
		Servers: map[string]projectconfig.MCPServerConfig{"local": {
			Transport: projectconfig.MCPTransportStdio, Command: os.Args[0], Args: []string{"-test.run=^TestMCPStdioHelper$"},
			Env: map[string]string{"LUNE_MCP_STDIO_HELPER": "1", "LUNE_MCP_STDIO_EXIT_MARKER": marker, "LUNE_MCP_STDIO_STARTS": starts, "LUNE_MCP_STDIO_CALLS": calls},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	if err := manager.Enable("local"); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	old := manager.instances["local"]
	manager.mu.Unlock()
	stdio := old.client.(*stdioTransport)
	if !waitUntil(stdio.closed, time.Now().Add(5*time.Second)) || !waitUntil(stdio.processDone, time.Now().Add(5*time.Second)) {
		t.Fatal("original server did not exit")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := old.tools[0].Run(context.Background(), json.RawMessage(`{}`))
			if result.Err == nil || !strings.Contains(result.Err.Error(), "restarted") {
				t.Errorf("reconnect result=%v", result.Err)
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(starts)
	if err != nil || strings.Count(string(data), "start\n") != 2 {
		t.Fatalf("process starts=%q err=%v", data, err)
	}
	if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("disconnected calls were replayed")
	}
	definition := manager.registry.DefinitionsForCategory("mcp")[0].(model.FunctionTool)
	output := manager.registry.ExecuteCall(context.Background(), model.OutputFunctionCall{Name: definition.Name, CallID: "next", Arguments: `{}`})
	if !strings.Contains(outputText(output.Output), "call completed") {
		t.Fatalf("next call=%s", outputText(output.Output))
	}
	data, err = os.ReadFile(calls)
	if err != nil || string(data) != "call\n" {
		t.Fatalf("tool calls=%q err=%v", data, err)
	}
}
