package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	projectconfig "github.com/afterlune/luneCore/internal/config"
)

func lifecycleStdio(t *testing.T, env map[string]string) *stdioTransport {
	t.Helper()
	env["LUNE_MCP_STDIO_HELPER"] = "1"
	client, err := startStdio(projectconfig.MCPServerConfig{
		Command: os.Args[0], Args: []string{"-test.run=^TestMCPStdioHelper$"}, Env: env,
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	return client
}

func assertNoPending(t *testing.T, client *stdioTransport) {
	t.Helper()
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.pending) != 0 {
		t.Fatalf("pending requests = %d", len(client.pending))
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-tick.C:
		case <-timer.C:
			t.Fatal("helper did not signal readiness")
		}
	}
}

func TestStdioReleasesCompletedRequests(t *testing.T) {
	client := lifecycleStdio(t, map[string]string{})
	for i := 0; i < 50; i++ {
		if _, err := client.Request(context.Background(), "tools/list", nil, "", nil); err != nil {
			t.Fatal(err)
		}
		assertNoPending(t, client)
	}
	if _, err := client.Request(context.Background(), "test/error", nil, "", nil); err == nil {
		t.Fatal("RPC error accepted")
	}
	assertNoPending(t, client)
	if !client.Alive() {
		t.Fatal("RPC error disconnected the server")
	}
}

func TestStdioFinalResponseSurvivesExit(t *testing.T) {
	client := lifecycleStdio(t, map[string]string{})
	data, err := client.Request(context.Background(), "test/final_exit", nil, "", nil)
	if err != nil || !strings.Contains(string(data), "test/final_exit") {
		t.Fatalf("final response=%s err=%v", data, err)
	}
	if !waitUntil(client.processDone, time.Now().Add(5*time.Second)) {
		t.Fatal("process was not reaped")
	}
	assertNoPending(t, client)
}

func TestStdioExitWithoutResponseCompletesRequest(t *testing.T) {
	client := lifecycleStdio(t, map[string]string{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Request(ctx, "test/no_response", nil, "", nil); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exit error=%v", err)
	}
	assertNoPending(t, client)
}

func TestStdioCancellationIgnoresLateResponse(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "seen")
	client := lifecycleStdio(t, map[string]string{"LUNE_MCP_STDIO_SEEN": seen})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := client.Request(ctx, "test/delay", nil, "", nil); result <- err }()
	waitForFile(t, seen)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not complete request")
	}
	assertNoPending(t, client)
	data, err := client.Request(context.Background(), "test/release", nil, "", nil)
	if err != nil || !strings.Contains(string(data), "test/release") {
		t.Fatalf("next response=%s err=%v", data, err)
	}
	assertNoPending(t, client)
}

func TestStdioConcurrentRequestsReceiveOwnResponses(t *testing.T) {
	client := lifecycleStdio(t, map[string]string{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, err := client.Request(context.Background(), "test/reverse", map[string]any{"token": i}, "", nil)
			var response struct {
				Token int `json:"token"`
			}
			if err != nil || json.Unmarshal(data, &response) != nil || response.Token != i {
				t.Errorf("response=%s err=%v", data, err)
			}
		}()
	}
	wg.Wait()
	assertNoPending(t, client)
}

func TestStdioWriteFailureCompletesRequest(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "seen")
	client := lifecycleStdio(t, map[string]string{"LUNE_MCP_STDIO_SEEN": seen})
	if _, err := client.Request(context.Background(), "test/close_input", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, seen)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.Request(ctx, "test/write", nil, "", nil); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("write error=%v", err)
	}
	assertNoPending(t, client)
	if client.Alive() {
		t.Fatal("broken input pipe remained connected")
	}
}

func TestStdioCloseCompletesWaitingRequest(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "seen")
	client := lifecycleStdio(t, map[string]string{"LUNE_MCP_STDIO_SEEN": seen})
	result := make(chan error, 1)
	go func() { _, err := client.Request(context.Background(), "test/delay", nil, "", nil); result <- err }()
	waitForFile(t, seen)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("closed request succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Close left a pending request")
	}
	assertNoPending(t, client)
}

func TestStdioBlockedWriteCanBeCancelledAndClosed(t *testing.T) {
	starts := filepath.Join(t.TempDir(), "started")
	client := lifecycleStdio(t, map[string]string{"LUNE_MCP_STDIO_MODE": "stubborn", "LUNE_MCP_STDIO_STARTS": starts})
	waitForFile(t, starts)
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_, err := client.Request(ctx, "test/blocked", map[string]any{"body": strings.Repeat("x", 2<<20)}, "", nil)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("blocked request error=%v", err)
		}
	}
	assertNoPending(t, client)
	start := time.Now()
	results := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() { results <- client.Close() }()
	}
	for i := 0; i < 4; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(4 * time.Second):
			t.Fatal("Close blocked")
		}
	}
	if time.Since(start) > stdioCloseTimeout+500*time.Millisecond {
		t.Fatal("Close exceeded its deadline")
	}
	for _, done := range []chan struct{}{client.readerDone, client.writerDone, client.processDone} {
		select {
		case <-done:
		default:
			t.Fatal("transport resource remains active")
		}
	}
}

func TestStdioInheritedOutputDoesNotPreventReaping(t *testing.T) {
	client := lifecycleStdio(t, map[string]string{"LUNE_MCP_STDIO_MODE": "inherited"})
	if !waitUntil(client.processDone, time.Now().Add(5*time.Second)) {
		t.Fatal("parent process was not reaped")
	}
	if !waitUntil(client.readerDone, time.Now().Add(stdioDrainTimeout+time.Second)) {
		t.Fatal("inherited stdout prevented drain timeout")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}
