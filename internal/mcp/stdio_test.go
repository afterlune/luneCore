package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
)

func TestMCPStdioHelper(t *testing.T) {
	if os.Getenv("LUNE_MCP_STDIO_HELPER") != "1" {
		return
	}
	mode := os.Getenv("LUNE_MCP_STDIO_MODE")
	if mode == "hold_stdout" {
		time.Sleep(2 * time.Second)
		return
	}
	if mode == "inherited" {
		child := exec.Command(os.Args[0], "-test.run=^TestMCPStdioHelper$")
		child.Env = mergeEnvironment(os.Environ(), map[string]string{"LUNE_MCP_STDIO_MODE": "hold_stdout"})
		child.Stdout = os.Stdout
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if path := os.Getenv("LUNE_MCP_STDIO_STARTS"); path != "" {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.WriteString("start\n")
		_ = file.Close()
	}
	if mode == "stubborn" {
		time.Sleep(30 * time.Second)
		return
	}
	var deferred []byte
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  map[string]any  `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		if request.Method == "test/no_response" {
			return
		}
		result := map[string]any{
			"method":   request.Method,
			"token":    request.Params["token"],
			"parent":   os.Getenv("LUNE_MCP_STDIO_PARENT"),
			"override": os.Getenv("LUNE_MCP_STDIO_OVERRIDE"),
		}
		marker := os.Getenv("LUNE_MCP_STDIO_EXIT_MARKER")
		exitAfterResponse := false
		if request.Method == "tools/list" {
			result["tools"] = []any{map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}
			if marker != "" {
				if data, err := os.ReadFile(marker); err == nil && string(data) == "initial" {
					_ = os.WriteFile(marker, []byte("exited"), 0600)
					exitAfterResponse = true
				}
			}
		}
		if request.Method == "tools/call" {
			if path := os.Getenv("LUNE_MCP_STDIO_CALLS"); path != "" {
				file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = file.WriteString("call\n")
				_ = file.Close()
			}
			result["resultType"] = "complete"
			result["content"] = []any{map[string]any{"type": "text", "text": "call completed"}}
		}
		response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		if request.Method == "test/error" {
			response, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -1, "message": "expected error"}})
		}
		if request.Method == "test/delay" || request.Method == "test/reverse" && deferred == nil {
			deferred = response
			if path := os.Getenv("LUNE_MCP_STDIO_SEEN"); path != "" {
				_ = os.WriteFile(path, []byte("seen"), 0600)
			}
			continue
		}
		if request.Method == "test/release" && deferred != nil {
			_, _ = os.Stdout.Write(append(deferred, '\n'))
			deferred = nil
		}
		_, _ = os.Stdout.Write(append(response, '\n'))
		if request.Method == "test/close_input" {
			_ = os.Stdin.Close()
			if path := os.Getenv("LUNE_MCP_STDIO_SEEN"); path != "" {
				_ = os.WriteFile(path, []byte("seen"), 0600)
			}
			time.Sleep(30 * time.Second)
			return
		}
		if request.Method == "test/reverse" && deferred != nil {
			_, _ = os.Stdout.Write(append(deferred, '\n'))
			deferred = nil
		}
		if exitAfterResponse || request.Method == "test/final_exit" {
			return
		}
	}
}

func TestManagerRestartsExitedStdioForFutureCallsOnly(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	marker := workspace + string(os.PathSeparator) + "stdio-process-state"
	if err := os.WriteFile(marker, []byte("initial"), 0600); err != nil {
		t.Fatal(err)
	}
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.SetPermissionMode(tool.PermissionModeFullAccess); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Options{
		Workspace: workspace, StateDir: stateDir, Registry: registry,
		Servers: map[string]projectconfig.MCPServerConfig{"local": {
			Transport: projectconfig.MCPTransportStdio, Command: os.Args[0],
			Args: []string{"-test.run=^TestMCPStdioHelper$"},
			Env:  map[string]string{"LUNE_MCP_STDIO_HELPER": "1", "LUNE_MCP_STDIO_EXIT_MARKER": marker},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.Enable("local"); err != nil {
		t.Fatal(err)
	}
	definitions := registry.DefinitionsForCategory("mcp")
	if len(definitions) != 1 {
		t.Fatalf("initial tools = %#v", definitions)
	}
	manager.mu.Lock()
	original := manager.instances["local"].client.(*stdioTransport)
	manager.mu.Unlock()
	if !waitUntil(original.closed, time.Now().Add(5*time.Second)) || !waitUntil(original.processDone, time.Now().Add(5*time.Second)) {
		t.Fatal("original MCP process did not exit")
	}
	first := definitions[0].(model.FunctionTool)
	firstResult := registry.ExecuteCall(context.Background(), model.OutputFunctionCall{CallID: "first", Name: first.Name, Arguments: `{}`})
	if firstResult.Output.Text == nil || !strings.Contains(*firstResult.Output.Text, "restarted") {
		t.Fatalf("call after process exit = %#v", firstResult.Output)
	}
	if status := manager.List()[0]; !status.Connected {
		t.Fatalf("server not restarted for a later call: %#v", status)
	}
	definitions = registry.DefinitionsForCategory("mcp")
	second := definitions[0].(model.FunctionTool)
	secondResult := registry.ExecuteCall(context.Background(), model.OutputFunctionCall{CallID: "second", Name: second.Name, Arguments: `{}`})
	if secondResult.Output.Text != nil || !strings.Contains(outputText(secondResult.Output), "call completed") {
		t.Fatalf("future call output = %#v", secondResult.Output)
	}
}

func TestStdioTransportUsesJSONLinesAndMergedEnvironment(t *testing.T) {
	t.Setenv("LUNE_MCP_STDIO_PARENT", "parent")
	transport, err := startStdio(projectconfig.MCPServerConfig{
		Transport: projectconfig.MCPTransportStdio,
		Command:   os.Args[0],
		Args:      []string{"-test.run=^TestMCPStdioHelper$"},
		Env:       map[string]string{"LUNE_MCP_STDIO_HELPER": "1", "LUNE_MCP_STDIO_OVERRIDE": "config"},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := transport.Request(context.Background(), "tools/list", map[string]any{}, "", nil)
	if err != nil {
		t.Fatalf("stdio request: %v", err)
	}
	var response struct {
		Method   string `json:"method"`
		Parent   string `json:"parent"`
		Override string `json:"override"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatal(err)
	}
	if response.Method != "tools/list" || response.Parent != "parent" || response.Override != "config" {
		t.Fatalf("stdio response = %#v", response)
	}
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEncodeHeaderValueProtectsUnsafeValues(t *testing.T) {
	if got := encodeHeaderValue("plain"); got != "plain" {
		t.Fatalf("plain header = %q", got)
	}
	if got := encodeHeaderValue("line\nfeed"); !strings.HasPrefix(got, "=?base64?") || !strings.HasSuffix(got, "?=") {
		t.Fatalf("unsafe header value = %q", got)
	}
	if got := encodeHeaderValue("=?base64?literal?="); got == "=?base64?literal?=" {
		t.Fatal("sentinel-looking literal was not encoded")
	}
}
