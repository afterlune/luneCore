package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	projectconfig "github.com/afterlune/luneCore/internal/config"
)

func TestManagerLifecycleChangeCancelsHostOperations(t *testing.T) {
	for _, action := range []string{"disable", "reconnect", "close"} {
		t.Run(action, func(t *testing.T) {
			entered := make(chan struct{})
			var enteredOnce sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var message testRPCRequest
				if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
					t.Errorf("decode MCP request: %v", err)
					return
				}
				if message.Method == "tools/list" {
					writeTestRPC(writer, message.ID, map[string]any{"tools": []any{}})
					return
				}
				enteredOnce.Do(func() { close(entered) })
				<-request.Context().Done()
			}))
			defer server.Close()

			registry := testRegistry(t)
			manager, err := NewManager(Options{
				Workspace: t.TempDir(), StateDir: t.TempDir(), Registry: registry,
				Servers: map[string]projectconfig.MCPServerConfig{
					"host": {Transport: projectconfig.MCPTransportStreamableHTTP, URL: server.URL},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Enable("host"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = manager.Close(context.Background()) })

			requestDone := make(chan error, 1)
			go func() {
				_, err := manager.ListPrompts(context.Background(), "host")
				requestDone <- err
			}()
			if !waitUntil(entered, time.Now().Add(3*time.Second)) {
				t.Fatal("host request did not reach MCP server")
			}

			lifecycleDone := make(chan error, 1)
			go func() {
				switch action {
				case "disable":
					lifecycleDone <- manager.Disable("host")
				case "reconnect":
					lifecycleDone <- manager.Enable("host")
				default:
					lifecycleDone <- manager.Close(context.Background())
				}
			}()

			select {
			case err := <-requestDone:
				if !errors.Is(err, ErrServerLifecycleChanged) || errors.Is(err, context.Canceled) {
					t.Fatalf("host request error = %v, want lifecycle sentinel only", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("host request did not stop after lifecycle change")
			}
			select {
			case err := <-lifecycleDone:
				if err != nil {
					t.Fatalf("%s error = %v", action, err)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("%s did not wait for the canceled host request to drain", action)
			}
		})
	}
}

func TestManagerHostOperationPreservesCallerCancellation(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var message testRPCRequest
		if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
			t.Errorf("decode MCP request: %v", err)
			return
		}
		if message.Method == "tools/list" {
			writeTestRPC(writer, message.ID, map[string]any{"tools": []any{}})
			return
		}
		close(entered)
		<-request.Context().Done()
	}))
	defer server.Close()

	manager, err := NewManager(Options{
		Workspace: t.TempDir(), StateDir: t.TempDir(), Registry: testRegistry(t),
		Servers: map[string]projectconfig.MCPServerConfig{
			"host": {Transport: projectconfig.MCPTransportStreamableHTTP, URL: server.URL},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	if err := manager.Enable("host"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	requestDone := make(chan error, 1)
	go func() {
		_, err := manager.ListPrompts(ctx, "host")
		requestDone <- err
	}()
	if !waitUntil(entered, time.Now().Add(3*time.Second)) {
		t.Fatal("host request did not reach MCP server")
	}
	cancel()
	select {
	case err := <-requestDone:
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrServerLifecycleChanged) {
			t.Fatalf("caller cancellation error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("host request did not honor caller cancellation")
	}
}

func TestManagerDisableDrainsStdioHostOperationBeforeClosing(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "host-request-seen")
	manager, err := NewManager(Options{
		Workspace: t.TempDir(), StateDir: t.TempDir(), Registry: testRegistry(t),
		Servers: map[string]projectconfig.MCPServerConfig{
			"stdio": {
				Transport: projectconfig.MCPTransportStdio, Command: os.Args[0], Args: []string{"-test.run=^TestMCPStdioHelper$"},
				Env: map[string]string{
					"LUNE_MCP_STDIO_HELPER":       "1",
					"LUNE_MCP_STDIO_PROMPT_DELAY": "1",
					"LUNE_MCP_STDIO_SEEN":         seen,
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	if err := manager.Enable("stdio"); err != nil {
		t.Fatal(err)
	}

	requestDone := make(chan error, 1)
	go func() {
		_, err := manager.ListPrompts(context.Background(), "stdio")
		requestDone <- err
	}()
	waitForFile(t, seen)
	disableDone := make(chan error, 1)
	go func() { disableDone <- manager.Disable("stdio") }()
	select {
	case err := <-requestDone:
		if !errors.Is(err, ErrServerLifecycleChanged) || errors.Is(err, context.Canceled) {
			t.Fatalf("stdio host request error = %v, want lifecycle sentinel only", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stdio host request did not stop after disable")
	}
	select {
	case err := <-disableDone:
		if err != nil {
			t.Fatalf("disable stdio server: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("disable did not finish closing the drained stdio transport")
	}
}

func TestManagerDisableCancelsEveryHostOperationForOnlyThatServer(t *testing.T) {
	entered := map[string]chan struct{}{"shared": make(chan struct{}, 2), "other": make(chan struct{}, 1)}
	releaseOther := make(chan struct{})
	servers := make(map[string]*httptest.Server)
	configs := make(map[string]projectconfig.MCPServerConfig)
	for _, name := range []string{"shared", "other"} {
		name := name
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			var message testRPCRequest
			if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
				t.Errorf("decode MCP request: %v", err)
				return
			}
			if message.Method == "tools/list" {
				writeTestRPC(writer, message.ID, map[string]any{"tools": []any{}})
				return
			}
			entered[name] <- struct{}{}
			if name == "other" {
				select {
				case <-releaseOther:
					writeTestRPC(writer, message.ID, map[string]any{"prompts": []any{}})
				case <-request.Context().Done():
				}
				return
			}
			<-request.Context().Done()
		}))
		servers[name] = server
		configs[name] = projectconfig.MCPServerConfig{Transport: projectconfig.MCPTransportStreamableHTTP, URL: server.URL}
	}
	defer servers["shared"].Close()
	defer servers["other"].Close()

	manager, err := NewManager(Options{Workspace: t.TempDir(), StateDir: t.TempDir(), Registry: testRegistry(t), Servers: configs})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background())
	for _, name := range []string{"shared", "other"} {
		if err := manager.Enable(name); err != nil {
			t.Fatal(err)
		}
	}

	results := make(chan error, 3)
	for _, name := range []string{"shared", "shared", "other"} {
		name := name
		go func() {
			_, err := manager.ListPrompts(context.Background(), name)
			results <- err
		}()
	}
	for _, name := range []string{"shared", "shared", "other"} {
		select {
		case <-entered[name]:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s request did not reach MCP server", name)
		}
	}
	if err := manager.Disable("shared"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if !errors.Is(err, ErrServerLifecycleChanged) {
				t.Fatalf("shared request error = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("disable did not cancel every request to the server")
		}
	}
	select {
	case err := <-results:
		t.Fatalf("request to unrelated server was canceled: %v", err)
	default:
	}
	close(releaseOther)
	select {
	case err := <-results:
		if err != nil {
			t.Fatalf("unrelated server request error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("unrelated server request did not finish")
	}
}
