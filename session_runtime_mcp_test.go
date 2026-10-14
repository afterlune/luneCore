package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/mcp"
	publicmodel "github.com/afterlune/luneCore/model"
)

type hostMCPRequest struct {
	ID     json.RawMessage            `json:"id"`
	Method string                     `json:"method"`
	Params map[string]json.RawMessage `json:"params"`
}

func attachHostMCPServer(t *testing.T, client *Core, name, endpoint string, limits ...mcp.Limits) {
	t.Helper()
	if client.mcpManager != nil {
		if err := client.mcpManager.Close(); err != nil {
			t.Fatal(err)
		}
	}
	options := mcp.Options{
		Workspace: client.workspace, StateDir: t.TempDir(), Registry: client.registry,
		Servers: map[string]projectconfig.MCPServerConfig{
			name: {Transport: projectconfig.MCPTransportStreamableHTTP, URL: endpoint},
		},
		RequestInteraction: client.requestUserInteraction,
	}
	if len(limits) > 0 {
		options.Limits = limits[0]
	}
	manager, err := mcp.NewManager(options)
	if err != nil {
		t.Fatal(err)
	}
	client.mcpManager = manager
	if err := manager.Enable(name); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRuntimeMCPHostAPIs(t *testing.T) {
	imageData := base64.StdEncoding.EncodeToString([]byte{1, 2, 3})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var message hostMCPRequest
		if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
			t.Errorf("decode MCP request: %v", err)
			return
		}
		var result any
		switch message.Method {
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "search", "inputSchema": map[string]any{"type": "object"}}}}
		case "prompts/list":
			var cursor string
			_ = json.Unmarshal(message.Params["cursor"], &cursor)
			if cursor == "" {
				result = map[string]any{"prompts": []any{map[string]any{
					"name": "summarize", "title": "Summarize", "description": "Summarize a document",
					"arguments": []any{map[string]any{"name": "style", "description": "Tone", "required": true}},
				}}, "nextCursor": "prompt-page-2"}
			} else if cursor == "prompt-page-2" {
				result = map[string]any{"prompts": []any{map[string]any{"name": "translate"}}}
			} else {
				t.Errorf("unexpected prompt cursor %q", cursor)
			}
		case "resources/list":
			var cursor string
			_ = json.Unmarshal(message.Params["cursor"], &cursor)
			if cursor == "" {
				result = map[string]any{"resources": []any{map[string]any{
					"uri": "urn:manual", "name": "manual", "title": "Manual", "mimeType": "text/plain", "size": 6,
				}}, "nextCursor": "resource-page-2"}
			} else {
				result = map[string]any{"resources": []any{map[string]any{"uri": "urn:other", "name": "other"}}}
			}
		case "resources/templates/list":
			var cursor string
			_ = json.Unmarshal(message.Params["cursor"], &cursor)
			if cursor == "" {
				result = map[string]any{"resourceTemplates": []any{map[string]any{
					"uriTemplate": "urn:page:{id}", "name": "page", "mimeType": "text/markdown",
				}}, "nextCursor": "template-page-2"}
			} else {
				result = map[string]any{"resourceTemplates": []any{map[string]any{"uriTemplate": "urn:other:{id}", "name": "other"}}}
			}
		case "prompts/get":
			var name string
			_ = json.Unmarshal(message.Params["name"], &name)
			if name != "summarize" {
				t.Errorf("prompt name = %q", name)
			}
			var args map[string]string
			_ = json.Unmarshal(message.Params["arguments"], &args)
			if args["style"] != "brief" {
				t.Errorf("prompt arguments = %#v", args)
			}
			result = map[string]any{"description": "Prepared prompt", "messages": []any{
				map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": "Summarize this"}},
				map[string]any{"role": "assistant", "content": []any{
					map[string]any{"type": "text", "text": "I will summarize it."},
					map[string]any{"type": "image", "mimeType": "image/png", "data": imageData},
				}},
			}}
		case "resources/read":
			result = map[string]any{"contents": []any{
				map[string]any{"uri": "urn:manual", "mimeType": "text/plain", "text": "Manual"},
				map[string]any{"uri": "urn:manual", "mimeType": "image/png", "blob": imageData},
			}}
		default:
			t.Errorf("unexpected MCP method %q", message.Method)
			return
		}
		writeHostMCPResult(writer, message.ID, result)
	}))
	defer server.Close()

	client := newProgrammaticCore(t, &scriptedHostProvider{})
	attachHostMCPServer(t, client, "docs", server.URL)
	session, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}

	prompts, err := runtime.ListMCPPrompts(context.Background(), "docs")
	if err != nil || len(prompts) != 2 || prompts[0].Name != "summarize" || len(prompts[0].Arguments) != 1 || !prompts[0].Arguments[0].Required {
		t.Fatalf("listed prompts = %#v, error = %v", prompts, err)
	}
	prompt, err := runtime.GetMCPPrompt(context.Background(), "docs", "summarize", map[string]string{"style": "brief"})
	if err != nil {
		t.Fatal(err)
	}
	if prompt.Description != "Prepared prompt" || prompt.Input == nil || len(prompt.Input.Items) != 2 {
		t.Fatalf("prompt result = %#v", prompt)
	}
	first, ok := prompt.Input.Items[0].(publicmodel.InputMessage)
	if !ok || first.Role != publicmodel.RoleUser {
		t.Fatalf("prompt user message = %#v", prompt.Input.Items[0])
	}
	second, ok := prompt.Input.Items[1].(publicmodel.InputMessage)
	if !ok || second.Role != publicmodel.RoleAssistant || len(second.Content.Parts) != 2 {
		t.Fatalf("prompt assistant message = %#v", prompt.Input.Items[1])
	}
	if text, ok := second.Content.Parts[0].(publicmodel.OutputText); !ok || text.Text != "I will summarize it." {
		t.Fatalf("normalized assistant text = %#v", second.Content.Parts[0])
	}
	if image, ok := second.Content.Parts[1].(publicmodel.InputImage); !ok || !strings.HasSuffix(image.ImageURL, imageData) {
		t.Fatalf("normalized prompt image = %#v", second.Content.Parts[1])
	}
	if _, err := NewTaskInput(prompt.Input); err != nil {
		t.Fatalf("prompt input cannot be wrapped as task input: %v", err)
	}

	resources, err := runtime.ListMCPResources(context.Background(), "docs")
	if err != nil || len(resources) != 2 || resources[0].URI != "urn:manual" || resources[0].Size != 6 || resources[1].URI != "urn:other" {
		t.Fatalf("listed resources = %#v, error = %v", resources, err)
	}
	templates, err := runtime.ListMCPResourceTemplates(context.Background(), "docs")
	if err != nil || len(templates) != 2 || templates[0].URITemplate != "urn:page:{id}" || templates[1].URITemplate != "urn:other:{id}" {
		t.Fatalf("listed resource templates = %#v, error = %v", templates, err)
	}
	read, err := runtime.ReadMCPResource(context.Background(), "docs", "urn:manual")
	if err != nil || read == nil || len(read.Items) != 1 {
		t.Fatalf("read resource input = %#v, error = %v", read, err)
	}
	readMessage, ok := read.Items[0].(publicmodel.InputMessage)
	if !ok || readMessage.Role != publicmodel.RoleUser || len(readMessage.Content.Parts) != 2 {
		t.Fatalf("read resource message = %#v", read.Items[0])
	}
	if text, ok := readMessage.Content.Parts[0].(publicmodel.InputText); !ok || text.Text != "Manual" {
		t.Fatalf("resource text = %#v", readMessage.Content.Parts[0])
	}
	if image, ok := readMessage.Content.Parts[1].(publicmodel.InputImage); !ok || !strings.HasSuffix(image.ImageURL, imageData) {
		t.Fatalf("resource image = %#v", readMessage.Content.Parts[1])
	}
	if definitions := client.registry.DefinitionsForCategory("mcp"); len(definitions) != 1 {
		t.Fatalf("host prompt/resource APIs changed the registered MCP tools: %#v", definitions)
	}
	if _, err := runtime.ListMCPPrompts(context.Background(), "missing"); err == nil {
		t.Fatal("unknown MCP server should return an error")
	}
	if err := client.DisableMCPServer("docs"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ListMCPResources(context.Background(), "docs"); err == nil {
		t.Fatal("disabled MCP server should return an error")
	}
}

func TestSessionRuntimeMCPHostInteractionUsesPendingRequests(t *testing.T) {
	calls := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var message hostMCPRequest
		if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
			t.Errorf("decode MCP request: %v", err)
			return
		}
		switch message.Method {
		case "tools/list":
			writeHostMCPResult(writer, message.ID, map[string]any{"tools": []any{}})
		case "prompts/get", "resources/read":
			calls[message.Method]++
			requestState := "opaque-" + message.Method
			if calls[message.Method] == 1 {
				writeHostMCPResult(writer, message.ID, map[string]any{
					"resultType": "input_required", "requestState": requestState,
					"inputRequests": map[string]any{"approval": map[string]any{
						"method": "elicitation/create", "params": map[string]any{"mode": "form", "message": "Confirm " + message.Method, "requestedSchema": map[string]any{"type": "object"}},
					}},
				})
				return
			}
			var params map[string]any
			_ = json.Unmarshal(mustHostJSON(message.Params), &params)
			if params["requestState"] != requestState || params["inputResponses"] == nil {
				t.Errorf("MRTR retry parameters = %#v", params)
			}
			if message.Method == "prompts/get" {
				writeHostMCPResult(writer, message.ID, map[string]any{"messages": []any{
					map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": "done"}},
				}})
			} else {
				writeHostMCPResult(writer, message.ID, map[string]any{"contents": []any{map[string]any{"uri": "urn:read", "text": "done"}}})
			}
		default:
			t.Errorf("unexpected MCP method %q", message.Method)
		}
	}))
	defer server.Close()

	client := newProgrammaticCore(t, &scriptedHostProvider{})
	attachHostMCPServer(t, client, "prompts", server.URL)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	runtime.SetUserInteractionAvailable(true)
	result := make(chan struct {
		prompt MCPPromptResult
		err    error
	}, 1)
	go func() {
		prompt, err := runtime.GetMCPPrompt(context.Background(), "prompts", "login", nil)
		result <- struct {
			prompt MCPPromptResult
			err    error
		}{prompt: prompt, err: err}
	}()

	deadline := time.Now().Add(3 * time.Second)
	var pending []PendingRequest
	for time.Now().Before(deadline) {
		pending, err = runtime.PendingRequests(context.Background())
		if err == nil && len(pending) == 1 && pending[0].Interaction != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || len(pending) != 1 || pending[0].Interaction == nil {
		t.Fatalf("pending host interaction = %#v, error = %v", pending, err)
	}
	interaction := pending[0].Interaction
	if interaction.AgentID != "host" || interaction.ServerName != "prompts" || !strings.Contains(string(interaction.Request), "Confirm prompts/get") {
		t.Fatalf("pending host interaction = %#v", interaction)
	}
	if err := runtime.ResolveUserInteraction(interaction.ID, json.RawMessage(`{"action":"accept","content":{}}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case resolved := <-result:
		if resolved.err != nil || resolved.prompt.Input == nil || calls["prompts/get"] != 2 {
			t.Fatalf("retrieved prompt after interaction = %#v, calls = %#v", resolved, calls)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("host MCP request did not resume after interaction resolution")
	}

	readResult := make(chan error, 1)
	go func() {
		input, err := runtime.ReadMCPResource(context.Background(), "prompts", "urn:read")
		if err == nil && input == nil {
			err = errors.New("resource read returned nil input")
		}
		readResult <- err
	}()
	deadline = time.Now().Add(3 * time.Second)
	pending = nil
	for time.Now().Before(deadline) {
		pending, err = runtime.PendingRequests(context.Background())
		if err == nil && len(pending) == 1 && pending[0].Interaction != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || len(pending) != 1 || pending[0].Interaction == nil || !strings.Contains(string(pending[0].Interaction.Request), "Confirm resources/read") {
		t.Fatalf("pending resource interaction = %#v, error = %v", pending, err)
	}
	if err := runtime.ResolveUserInteraction(pending[0].Interaction.ID, json.RawMessage(`{"action":"accept","content":{}}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readResult:
		if err != nil || calls["resources/read"] != 2 {
			t.Fatalf("resource read after interaction error = %v, calls = %#v", err, calls)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("host MCP resource read did not resume after interaction resolution")
	}
}

func TestSessionRuntimeMCPInteractionIsCancelledByServerLifecycle(t *testing.T) {
	for _, action := range []string{"disable", "reconnect", "close"} {
		t.Run(action, func(t *testing.T) {
			var promptCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var message hostMCPRequest
				if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
					t.Errorf("decode MCP request: %v", err)
					return
				}
				switch message.Method {
				case "tools/list":
					writeHostMCPResult(writer, message.ID, map[string]any{"tools": []any{}})
				case "prompts/get":
					promptCalls.Add(1)
					writeHostMCPResult(writer, message.ID, map[string]any{
						"resultType": "input_required", "requestState": "wait",
						"inputRequests": map[string]any{"input": map[string]any{
							"method": "elicitation/create", "params": map[string]any{"mode": "form", "message": "Waiting for host", "requestedSchema": map[string]any{"type": "object"}},
						}},
					})
				default:
					t.Errorf("unexpected MCP method %q", message.Method)
				}
			}))
			defer server.Close()

			client := newProgrammaticCore(t, &scriptedHostProvider{})
			attachHostMCPServer(t, client, "host", server.URL)
			saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
			if err != nil {
				t.Fatal(err)
			}
			runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
			if err != nil {
				t.Fatal(err)
			}
			runtime.SetUserInteractionAvailable(true)
			requestDone := make(chan error, 1)
			go func() {
				_, err := runtime.GetMCPPrompt(context.Background(), "host", "login", nil)
				requestDone <- err
			}()

			deadline := time.Now().Add(3 * time.Second)
			var pending []PendingRequest
			for time.Now().Before(deadline) {
				pending, err = runtime.PendingRequests(context.Background())
				if err == nil && len(pending) == 1 && pending[0].Interaction != nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err != nil || len(pending) != 1 || pending[0].Interaction == nil {
				t.Fatalf("pending interaction = %#v, error = %v", pending, err)
			}
			interactionID := pending[0].Interaction.ID

			lifecycleDone := make(chan error, 1)
			go func() {
				switch action {
				case "disable":
					lifecycleDone <- client.DisableMCPServer("host")
				case "reconnect":
					lifecycleDone <- client.EnableMCPServer("host")
				default:
					lifecycleDone <- client.Close()
				}
			}()
			select {
			case err := <-requestDone:
				if !errors.Is(err, ErrMCPServerLifecycleChanged) || errors.Is(err, context.Canceled) {
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
				t.Fatalf("%s did not wait for host interaction to drain", action)
			}
			if err := runtime.ResolveUserInteraction(interactionID, json.RawMessage(`{"action":"accept","content":{}}`)); err == nil {
				t.Fatal("lifecycle cancellation left the old interaction resolvable")
			}
			if got := promptCalls.Load(); got != 1 {
				t.Fatalf("prompt/get called %d times after cancellation", got)
			}
			if action != "close" {
				pending, err = runtime.PendingRequests(context.Background())
				if err != nil || len(pending) != 0 {
					t.Fatalf("pending requests after cancellation = %#v, error = %v", pending, err)
				}
			}
		})
	}
}

func TestSessionRuntimeMCPResourceReadRespectsLimits(t *testing.T) {
	imageData := base64.StdEncoding.EncodeToString([]byte{1, 2, 3})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var message hostMCPRequest
		if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
			t.Errorf("decode MCP request: %v", err)
			return
		}
		if message.Method == "tools/list" {
			writeHostMCPResult(writer, message.ID, map[string]any{"tools": []any{}})
			return
		}
		writeHostMCPResult(writer, message.ID, map[string]any{"contents": []any{map[string]any{
			"uri": "urn:large", "mimeType": "image/png", "blob": imageData,
		}}})
	}))
	defer server.Close()

	client := newProgrammaticCore(t, &scriptedHostProvider{})
	attachHostMCPServer(t, client, "limited", server.URL, mcp.Limits{MaxBinaryBlocks: 1, MaxFileBytes: 2, MaxTotalBytes: 2})
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ReadMCPResource(context.Background(), "limited", "urn:large"); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("oversized MCP resource error = %v", err)
	}
}

func TestSessionRuntimeMCPListPropagatesRPCErrorAndCancellation(t *testing.T) {
	var block atomic.Bool
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var message hostMCPRequest
		if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
			t.Errorf("decode MCP request: %v", err)
			return
		}
		if message.Method == "tools/list" {
			writeHostMCPResult(writer, message.ID, map[string]any{"tools": []any{}})
			return
		}
		if block.Load() {
			started <- struct{}{}
			<-request.Context().Done()
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": message.ID, "error": map[string]any{"code": -32601, "message": "Method not found"}})
	}))
	defer server.Close()

	client := newProgrammaticCore(t, &scriptedHostProvider{})
	attachHostMCPServer(t, client, "errors", server.URL)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ListMCPPrompts(context.Background(), "errors"); err == nil || !strings.Contains(err.Error(), "Method not found") {
		t.Fatalf("unsupported method error = %v", err)
	}

	block.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := runtime.ListMCPPrompts(ctx, "errors")
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("MCP list request did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled MCP request error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled MCP list request did not return")
	}
}

func writeHostMCPResult(writer http.ResponseWriter, id json.RawMessage, result any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func mustHostJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}
