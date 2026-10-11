package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveModelIDUsesOverrideAndProfileFallback(t *testing.T) {
	profile := ModelProfile{Model: "configured-model"}
	if got := resolveModelID("task-model", profile); got != "task-model" {
		t.Fatalf("override model = %q, want task-model", got)
	}
	if got := resolveModelID("", profile); got != "configured-model" {
		t.Fatalf("fallback model = %q, want configured-model", got)
	}
}

func TestServerCompactionRequiresResponsesProtocol(t *testing.T) {
	config := Config{
		Providers: map[string]ProviderConfig{
			"claude": {Protocol: ProtocolAnthropicMessages, BaseURL: "https://example.test", APIKey: "key", ServerCompaction: true},
		},
		Models:       map[string]ModelProfile{"main": {Provider: "claude", Model: "claude-test", MaxOutputTokens: 512}},
		DefaultModel: "main",
	}
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "does not use the Responses protocol") {
		t.Fatalf("Validate error = %v, want server compaction protocol error", err)
	}
}

func TestInputAudioCapabilityRequiresResponsesProtocol(t *testing.T) {
	config := Config{
		Providers: map[string]ProviderConfig{
			"claude": {Protocol: ProtocolAnthropicMessages, BaseURL: "https://example.test", APIKey: "key"},
		},
		Models:       map[string]ModelProfile{"main": {Provider: "claude", Model: "claude-test", MaxOutputTokens: 512, SupportsInputAudio: true}},
		DefaultModel: "main",
	}
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "supportsInputAudio") {
		t.Fatalf("Validate error = %v, want input audio protocol error", err)
	}
}

func TestDeepSeekProviderCreateResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Method != http.MethodPost {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer ds-key" {
			t.Errorf("authorization = %q", got)
		}
		var request ResponseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if request.Model != "deepseek-override" || request.Stream {
			t.Errorf("request model/stream = %q/%v", request.Model, request.Stream)
		}
		_, _ = fmt.Fprint(w, `{"id":"resp-1","object":"response","status":"completed","model":"deepseek-test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}`)
	}))
	defer server.Close()
	provider := newResponsesProvider(ProviderDeepSeek, "ds-key", server.URL+"/responses", server.Client())
	profile := ModelProfile{Provider: ProviderDeepSeek, Model: "deepseek-test"}
	response, err := provider.CreateResponse(context.Background(), ResponseRequest{Model: "deepseek-override", Input: NewResponseInputText("hi")}, profile)
	if err != nil {
		t.Fatalf("CreateResponse: %v", err)
	}
	if response.ID != "resp-1" || response.Usage.TotalTokens != 6 {
		t.Fatalf("response = %#v", response)
	}
}

func TestResponsesProviderSendsServerCompactionConfiguration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		var management []ContextManagementItem
		if err := json.Unmarshal(request["context_management"], &management); err != nil {
			t.Fatalf("decode context_management: %v", err)
		}
		if len(management) != 1 || management[0] != (ContextManagementItem{Type: "compaction", CompactThreshold: 51200}) {
			t.Fatalf("context management = %#v", management)
		}
		_, _ = fmt.Fprint(w, `{"id":"resp-compact","object":"response","status":"completed","model":"openai-test","output":[{"type":"compaction","id":"cmp_1","encrypted_content":"opaque-state","created_by":"responses.compact"}],"usage":{"input_tokens":50000,"output_tokens":2,"total_tokens":50002}}`)
	}))
	defer server.Close()
	provider := newResponsesProvider("openai", "test-key", server.URL, server.Client())
	response, err := provider.CreateResponse(context.Background(), ModelRequest{
		Model: "openai-test", Input: NewResponseInputText("continue"),
		ContextManagement: []ContextManagementItem{{Type: "compaction", CompactThreshold: 51200}},
	}, ModelProfile{Provider: "openai", Model: "openai-test"})
	if err != nil {
		t.Fatalf("CreateResponse: %v", err)
	}
	if len(response.Output) != 1 {
		t.Fatalf("output = %#v", response.Output)
	}
	compaction, ok := response.Output[0].(OutputCompaction)
	if !ok || compaction.ID != "cmp_1" || compaction.EncryptedContent != "opaque-state" || compaction.CreatedBy != "responses.compact" {
		t.Fatalf("compaction output = %#v", response.Output[0])
	}
}

func TestDeepSeekProviderStreamsNormalizedEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q", got)
		}
		var request ResponseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !request.Stream || request.Model != "deepseek-stream-override" {
			t.Errorf("stream request = %#v, error = %v", request, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"delta\":\"hi\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp-stream\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"deepseek-test\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
	}))
	defer server.Close()
	provider := newResponsesProvider(ProviderDeepSeek, "ds-key", server.URL, server.Client())
	var events []ModelStreamEvent
	response, err := provider.StreamResponse(context.Background(), ResponseRequest{Model: "deepseek-stream-override", Input: NewResponseInputText("hi")}, ModelProfile{Model: "deepseek-test"}, func(event ModelStreamEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	if response.ID != "resp-stream" || len(events) != 2 || events[0].Delta != "hi" || events[0].Type != ModelStreamTextDelta {
		t.Fatalf("response=%#v events=%#v", response, events)
	}
}

func TestAnthropicMessagesMapsToolsImagesUsageAndErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("request path = %q", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "anthropic-key" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != anthropicAPIVersion {
			t.Errorf("anthropic-version = %q", got)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if _, leaked := request["context_management"]; leaked {
			t.Errorf("Responses-only context management leaked into Anthropic request: %#v", request["context_management"])
		}
		encoded, _ := json.Marshal(request)
		for _, expected := range []string{`"model":"claude-override"`, `"max_tokens":2048`, `"type":"tool_use"`, `"type":"tool_result"`, `"type":"image"`, `"input_schema"`} {
			if !strings.Contains(string(encoded), expected) {
				t.Errorf("request missing %s: %s", expected, encoded)
			}
		}
		_, _ = fmt.Fprint(w, `{"id":"msg-1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"reading"},{"type":"tool_use","id":"toolu-1","name":"read_file","input":{"path":"a.go"}}],"stop_reason":"tool_use","usage":{"input_tokens":11,"output_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}`)
	}))
	defer server.Close()
	provider := newAnthropicMessagesProvider(ProviderAnthropic, "anthropic-key", server.URL+"/v1/messages", server.Client())
	profile := ModelProfile{Provider: ProviderAnthropic, Model: "claude-test", MaxOutputTokens: 2048}
	request := ResponseRequest{
		Model:        "claude-override",
		Instructions: "Inspect safely.",
		Input: NewResponseInputItems(
			InputMessage{Role: RoleUser, Content: NewMessageContentParts(
				InputText{Text: "inspect this"}, InputImage{ImageURL: "data:image/png;base64,YWJj"},
			)},
			UnknownInputItem{Raw: json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I will inspect it."}]}`)},
			UnknownInputItem{Raw: json.RawMessage(`{"type":"function_call","call_id":"toolu-previous","name":"read_file","arguments":"{\"path\":\"a.go\"}"}`)},
			InputFunctionCallOutput{CallID: "toolu-previous", Output: NewToolOutputText("package main")},
		),
		Tools:             []ResponseTool{FunctionTool{Name: "read_file", Parameters: json.RawMessage(`{"type":"object"}`)}},
		ContextManagement: []ContextManagementItem{{Type: "compaction", CompactThreshold: 51200}},
	}
	response, err := provider.CreateResponse(context.Background(), request, profile)
	if err != nil {
		t.Fatalf("CreateResponse: %v", err)
	}
	if response.ID != "msg-1" || len(functionCalls(response.Output)) != 1 {
		t.Fatalf("normalized response = %#v", response)
	}
	if response.Usage.InputTokensDetails.CachedTokens != 3 || response.Usage.InputTokensDetails.CacheCreationTokens != 2 {
		t.Fatalf("usage = %#v", response.Usage)
	}
}

func TestAnthropicMessagesRejectsUnsupportedModalitiesExplicitly(t *testing.T) {
	request := ResponseRequest{Input: NewResponseInputItems(InputMessage{
		Role:    RoleUser,
		Content: NewMessageContentParts(UnknownContentPart{Raw: json.RawMessage(`{"type":"input_audio","data":"opaque"}`)}),
	})}
	_, err := toAnthropicRequest(request, ModelProfile{Model: "claude-test", MaxOutputTokens: 100}, false)
	if err == nil || !strings.Contains(err.Error(), "cannot map content part") {
		t.Fatalf("unsupported modality error = %v", err)
	}
}

func TestAnthropicImageDecodesCaseInsensitiveEscapedDataURL(t *testing.T) {
	image, err := anthropicImage(InputImage{ImageURL: "DATA:IMAGE/PNG;BASE64,%2B%2F8%3D"})
	if err != nil {
		t.Fatalf("anthropicImage: %v", err)
	}
	source, ok := image["source"].(map[string]any)
	if !ok {
		t.Fatalf("image source = %#v, want object", image["source"])
	}
	if source["media_type"] != "image/png" || source["data"] != "+/8=" {
		t.Fatalf("image source = %#v, want normalized image/png base64 payload", source)
	}
}

func TestAnthropicMessagesStreamsTextAndToolDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request["stream"] != true {
			t.Errorf("stream = %v", request["stream"])
		}
		if request["model"] != "claude-stream-override" {
			t.Errorf("model = %v", request["model"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`event: message_start` + "\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-stream\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":5}}}\n\n",
			`event: content_block_start` + "\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			`event: content_block_delta` + "\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n",
			`event: content_block_stop` + "\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			`event: message_delta` + "\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n",
			`event: message_stop` + "\ndata: {\"type\":\"message_stop\"}\n\n",
		} {
			_, _ = fmt.Fprint(w, event)
		}
	}))
	defer server.Close()
	provider := newAnthropicMessagesProvider(ProviderAnthropic, "anthropic-key", server.URL, server.Client())
	var deltas []string
	response, err := provider.StreamResponse(context.Background(), ResponseRequest{Model: "claude-stream-override", Input: NewResponseInputText("hi")}, ModelProfile{Model: "claude-test", MaxOutputTokens: 128}, func(event ModelStreamEvent) error {
		if event.Type == ModelStreamTextDelta {
			deltas = append(deltas, event.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	if response.ID != "msg-stream" || response.Status != "completed" || len(deltas) != 1 || deltas[0] != "hello" {
		t.Fatalf("response=%#v deltas=%#v", response, deltas)
	}
}
