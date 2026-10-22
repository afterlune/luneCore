package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
)

type testRPCRequest struct {
	JSONRPC string                     `json:"jsonrpc"`
	ID      json.RawMessage            `json:"id"`
	Method  string                     `json:"method"`
	Params  map[string]json.RawMessage `json:"params"`
}

func testRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.SetPermissionMode(tool.PermissionModeFullAccess); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	return registry
}

func TestHTTPManagerRegistersToolsAndPreservesMCPResults(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %q", request.Method)
		}
		if got := request.Header.Get("MCP-Protocol-Version"); got != protocolVersion {
			t.Errorf("MCP-Protocol-Version = %q", got)
		}
		if got := request.Header.Get("Accept"); !strings.Contains(got, "application/json") || !strings.Contains(got, "text/event-stream") {
			t.Errorf("Accept = %q", got)
		}
		var message testRPCRequest
		if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		calls = append(calls, message.Method)
		if message.JSONRPC != "2.0" {
			t.Errorf("jsonrpc = %q", message.JSONRPC)
		}
		var meta map[string]json.RawMessage
		if err := json.Unmarshal(message.Params["_meta"], &meta); err != nil {
			t.Errorf("request did not include _meta: %v", err)
		} else {
			var caps map[string]json.RawMessage
			_ = json.Unmarshal(meta["io.modelcontextprotocol/clientCapabilities"], &caps)
			if _, exists := caps["elicitation"]; !exists {
				t.Error("client did not declare its elicitation capability")
			}
		}
		if got := request.Header.Get("Mcp-Method"); got != message.Method {
			t.Errorf("Mcp-Method = %q, body method = %q", got, message.Method)
		}
		switch message.Method {
		case "tools/list":
			writeTestRPC(writer, message.ID, map[string]any{
				"resultType": "complete",
				"tools": []any{map[string]any{
					"name": "echo", "description": "echo input",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
						"tenant": map[string]any{"type": "string", "x-mcp-header": "Tenant"},
					}},
					"annotations": map[string]any{"readOnlyHint": true},
				}},
			})
		case "tools/call":
			if got := request.Header.Get("Mcp-Name"); got != "echo" {
				t.Errorf("Mcp-Name = %q", got)
			}
			if got := request.Header.Get("Mcp-Param-Tenant"); got != "=?base64?5Lit5Zu9?=" {
				t.Errorf("Mcp-Param-Tenant = %q", got)
			}
			var params map[string]json.RawMessage
			_ = json.Unmarshal(mustJSON(message.Params), &params)
			writeTestRPC(writer, message.ID, map[string]any{
				"resultType": "complete",
				"content": []any{
					map[string]any{"type": "text", "text": "model-visible text"},
					map[string]any{"type": "text", "text": "user-only note", "annotations": map[string]any{"audience": []string{"user"}}},
					map[string]any{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString([]byte{1, 2, 3})},
					map[string]any{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString([]byte{9, 8, 7}), "annotations": map[string]any{"audience": []string{"user"}}},
					map[string]any{"type": "resource_link", "name": "readme", "uri": "urn:test:readme"},
				},
				"structuredContent": map[string]any{"count": 2},
			})
		case "resources/read":
			if got := request.Header.Get("Mcp-Name"); got != "urn:test:readme" {
				t.Errorf("resource Mcp-Name = %q", got)
			}
			writeTestRPC(writer, message.ID, map[string]any{"contents": []any{map[string]any{
				"uri": "urn:test:readme", "mimeType": "text/plain", "text": "resource text",
			}}})
		default:
			t.Errorf("unexpected MCP method %q", message.Method)
		}
	}))
	defer server.Close()

	registry := testRegistry(t)
	manager, err := NewManager(Options{
		Workspace: t.TempDir(), StateDir: t.TempDir(), Registry: registry,
		RestoreEnabled: true,
		Servers:        map[string]projectconfig.MCPServerConfig{"docs": {Transport: projectconfig.MCPTransportStreamableHTTP, URL: server.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.Enable("docs"); err != nil {
		t.Fatal(err)
	}
	definitions := registry.DefinitionsForCategory("mcp")
	if len(definitions) != 1 {
		t.Fatalf("MCP definitions = %d", len(definitions))
	}
	definition, ok := definitions[0].(model.FunctionTool)
	if !ok || !strings.HasPrefix(definition.Name, "mcp__docs__echo_") {
		t.Fatalf("registered definition = %#v", definitions[0])
	}
	metadata, ok := registry.ToolMetadata(definition.Name)
	if !ok || !metadata.ReadOnly || metadata.PermissionClass != model.ToolPermissionNetwork || metadata.AlwaysVisible {
		t.Fatalf("MCP metadata = %#v, %v; MCP tools should remain deferred by default", metadata, ok)
	}
	output := registry.ExecuteCall(context.Background(), model.OutputFunctionCall{
		CallID: "call-1", Name: definition.Name, Arguments: `{"tenant":"中国"}`,
	})
	if output.CallID != "call-1" || len(output.TranscriptOnly) != 3 {
		t.Fatalf("tool result transcript routing = %#v", output)
	}
	if note, ok := output.TranscriptOnly[0].(model.InputText); !ok || note.Text != "user-only note" {
		t.Fatalf("transcript-only text = %#v", output.TranscriptOnly[0])
	}
	if marker, ok := output.TranscriptOnly[1].(model.InputText); !ok || !strings.Contains(marker.Text, "image/png") {
		t.Fatalf("transcript-only image marker = %#v", output.TranscriptOnly[1])
	}
	if image, ok := output.TranscriptOnly[2].(model.InputImage); !ok || !strings.HasSuffix(image.ImageURL, "CQgH") {
		t.Fatalf("transcript-only image data = %#v", output.TranscriptOnly[2])
	}
	if len(output.Output.Parts) != 4 {
		t.Fatalf("model-visible result parts = %#v", output.Output.Parts)
	}
	if _, ok := output.Output.Parts[0].(model.InputText); !ok {
		t.Fatalf("first model part = %T", output.Output.Parts[0])
	}
	if image, ok := output.Output.Parts[1].(model.InputImage); !ok || !strings.HasPrefix(image.ImageURL, "data:image/png;base64,") {
		t.Fatalf("image part = %#v", output.Output.Parts[1])
	}
	if !strings.Contains(outputText(output.Output), "resource text") || !strings.Contains(outputText(output.Output), "Structured content") {
		t.Fatalf("resource/structured content missing: %s", outputText(output.Output))
	}
	encodedModelOutput, err := json.Marshal(output.Output)
	if err != nil || strings.Contains(string(encodedModelOutput), "CQgH") {
		t.Fatalf("user-only image leaked into model output: %s, error=%v", encodedModelOutput, err)
	}
	if got := manager.List(); len(got) != 1 || !got[0].Connected || !got[0].Enabled || got[0].ToolCount != 1 {
		t.Fatalf("server status = %#v", got)
	}
	statePath := filepath.Join(manager.stateFile)
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("enabled state was not persisted: %v", err)
	}
	if err := manager.Enable("docs"); err != nil {
		t.Fatalf("re-enable MCP server: %v", err)
	}
	if got := manager.List(); len(got) != 1 || !got[0].Connected || !got[0].Enabled || got[0].ToolCount != 1 {
		t.Fatalf("server status after reconnect = %#v", got)
	}
	if definitions := registry.DefinitionsForCategory("mcp"); len(definitions) != 1 {
		t.Fatalf("reconnect registered duplicate definitions: %#v", definitions)
	}
	if strings.Join(calls, ",") != "tools/list,tools/call,resources/read,tools/list" {
		t.Fatalf("MCP calls = %v", calls)
	}
	if err := manager.Disable("docs"); err != nil {
		t.Fatal(err)
	}
	if definitions := registry.DefinitionsForCategory("mcp"); len(definitions) != 0 {
		t.Fatalf("disabled server left definitions: %#v", definitions)
	}
}

func TestSafeEndpointRemovesCredentialsAndQuery(t *testing.T) {
	if got, want := safeEndpoint("https://user:password@example.test/v1/mcp?token=secret#fragment"), "https://example.test/v1/mcp"; got != want {
		t.Fatalf("safe endpoint = %q, want %q", got, want)
	}
	if got := safeEndpoint("not a URL"); got != "" {
		t.Fatalf("invalid endpoint = %q, want empty", got)
	}
}

func TestHTTPManagerRetriesMRTRWithNewIDAndOpaqueRequestState(t *testing.T) {
	var initialID json.RawMessage
	toolCalls := 0
	interactions := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var message testRPCRequest
		if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		switch message.Method {
		case "tools/list":
			writeTestRPC(writer, message.ID, map[string]any{"tools": []any{map[string]any{
				"name": "login", "inputSchema": map[string]any{"type": "object"},
			}}})
		case "tools/call":
			toolCalls++
			if toolCalls == 1 {
				initialID = append(json.RawMessage(nil), message.ID...)
				writeTestRPC(writer, message.ID, map[string]any{
					"resultType": "input_required", "requestState": "opaque:%2F\u0000state",
					"inputRequests": map[string]any{"credentials": map[string]any{
						"method": "elicitation/create", "params": map[string]any{
							"mode": "form", "message": "Enter a one-time code",
							"requestedSchema": map[string]any{"type": "object", "properties": map[string]any{"code": map[string]any{"type": "string"}}},
						},
					}},
				})
				return
			}
			if string(message.ID) == string(initialID) {
				t.Error("MRTR retry reused the original JSON-RPC ID")
			}
			var params map[string]any
			if err := json.Unmarshal(mustJSON(message.Params), &params); err != nil {
				t.Error(err)
			}
			if params["requestState"] != "opaque:%2F\x00state" {
				t.Errorf("requestState = %#v", params["requestState"])
			}
			responses, _ := params["inputResponses"].(map[string]any)
			if responses == nil || responses["credentials"] == nil {
				t.Errorf("inputResponses = %#v", params["inputResponses"])
			}
			writeTestRPC(writer, message.ID, map[string]any{"resultType": "complete", "content": []any{map[string]any{"type": "text", "text": "authenticated"}}})
		default:
			t.Errorf("unexpected method %q", message.Method)
		}
	}))
	defer server.Close()
	registry := testRegistry(t)
	manager, err := NewManager(Options{
		Workspace: t.TempDir(), StateDir: t.TempDir(), Registry: registry,
		Servers: map[string]projectconfig.MCPServerConfig{"login": {Transport: projectconfig.MCPTransportStreamableHTTP, URL: server.URL}},
		RequestInteraction: func(_ context.Context, agentID, serverName string, request json.RawMessage) (json.RawMessage, error) {
			interactions++
			if agentID != "lead" || serverName != "login" || !strings.Contains(string(request), "one-time code") {
				t.Errorf("interaction context: agent=%q server=%q request=%s", agentID, serverName, request)
			}
			return json.RawMessage(`{"action":"accept","content":{"code":"123456"}}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.Enable("login"); err != nil {
		t.Fatal(err)
	}
	definition := registry.DefinitionsForCategory("mcp")[0].(model.FunctionTool)
	output := registry.ExecuteCall(model.WithAgentID(context.Background(), "lead"), model.OutputFunctionCall{
		CallID: "call-login", Name: definition.Name, Arguments: `{}`,
	})
	if output.Output.Text != nil || !strings.Contains(outputText(output.Output), "authenticated") {
		t.Fatalf("MRTR result = %#v", output.Output)
	}
	if interactions != 1 || toolCalls != 2 {
		t.Fatalf("round trips: interactions=%d tool calls=%d", interactions, toolCalls)
	}
}

func TestMCPAudioPreservesOnlySupportedFormats(t *testing.T) {
	remote := &remoteTool{}
	limits := Limits{MaxBinaryBlocks: 5, MaxFileBytes: 10, MaxTotalBytes: 20}
	parts := make([]model.ContentPart, 0)
	transcript := make([]model.ContentPart, 0)
	budget := &binaryBudget{limits: limits}
	ctx := model.WithInputAudioSupport(context.Background(), true)
	if err := remote.addBinary(base64.StdEncoding.EncodeToString([]byte{1, 2, 3}), "audio/mpeg", false, ctx, budget, &parts, &transcript); err != nil {
		t.Fatal(err)
	}
	if audio, ok := parts[0].(model.InputAudio); !ok || audio.Format != "mp3" {
		t.Fatalf("supported audio = %#v", parts[0])
	}
	parts = parts[:0]
	budget = &binaryBudget{limits: limits}
	if err := remote.addBinary(base64.StdEncoding.EncodeToString([]byte{4, 5}), "audio/ogg", false, ctx, budget, &parts, &transcript); err != nil {
		t.Fatal(err)
	}
	if marker, ok := parts[0].(model.InputText); !ok || !strings.Contains(marker.Text, "audio/ogg") {
		t.Fatalf("unsupported audio marker = %#v", parts[0])
	}
}

func writeTestRPC(writer http.ResponseWriter, id json.RawMessage, result any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func outputText(output model.ToolOutput) string {
	if output.Text != nil {
		return *output.Text
	}
	var text strings.Builder
	for _, part := range output.Parts {
		if value, ok := part.(model.InputText); ok {
			text.WriteString(value.Text)
			text.WriteByte('\n')
		}
	}
	return text.String()
}
