package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadAPIKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"apikey":" test-key "}`), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := readAPIKey(path)
	if err != nil {
		t.Fatalf("readAPIKey: %v", err)
	}
	if key != "test-key" {
		t.Fatalf("key = %q, want trimmed key", key)
	}

	if err := os.WriteFile(path, []byte(`{"apikey":" "}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readAPIKey(path); err == nil {
		t.Fatal("expected an empty API key to fail")
	}
}

func TestNewResponseInputAcceptsGenericInputAndPreservesUnknownParts(t *testing.T) {
	input, err := NewResponseInput("hello")
	if err != nil {
		t.Fatalf("NewResponseInput string: %v", err)
	}
	if input.Text == nil || *input.Text != "hello" {
		t.Fatalf("unexpected string input: %#v", input)
	}

	payload := []map[string]any{{
		"type": "message",
		"role": "user",
		"content": []map[string]any{
			{"type": "input_text", "text": "inspect this"},
			{"type": "input_audio", "payload": map[string]any{"codec": "raw", "data": "opaque"}},
		},
	}}
	input, err = NewResponseInput(payload)
	if err != nil {
		t.Fatalf("NewResponseInput structured payload: %v", err)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal generic input: %v", err)
	}
	if !strings.Contains(string(encoded), `"type":"input_audio"`) || !strings.Contains(string(encoded), `"codec":"raw"`) {
		t.Fatalf("generic content was not preserved: %s", encoded)
	}

	if _, err := NewResponseInput(map[string]string{"type": "message"}); err == nil {
		t.Fatal("expected an object that is not a Responses input item list to fail")
	}
}

func TestCreateResponseSendsOneRequestWithoutManagingToolsOrHistory(t *testing.T) {
	executor := newTestExecutor()
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("authorization = %q", got)
		}
		var request ResponseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Model != DeepSeekFlashModel || request.Input == nil || len(request.Tools) != 1 {
			t.Errorf("unexpected request: %#v", request)
		}
		_, _ = fmt.Fprint(w, `{"status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"}]}`)
	}))
	defer server.Close()
	client := newClient("test-key", server.URL, server.Client(), executor)
	input, err := NewResponseInput("one request")
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.CreateResponse(context.Background(), ResponseRequest{
		Input: input,
		Tools: executor.Definitions(),
	})
	if err != nil {
		t.Fatalf("CreateResponse: %v", err)
	}
	if len(response.Output) != 1 || requestCount != 1 || len(executor.calls) != 0 || len(client.history) != 0 {
		t.Fatalf("response=%#v requests=%d tool_calls=%d history=%#v", response, requestCount, len(executor.calls), client.history)
	}
}

func TestRespondLogsRequestMetadataWithoutPayloads(t *testing.T) {
	logs := captureLogs(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"private-response-content"}]}]}`)
	}))
	defer server.Close()
	client := newClient("private-api-key", server.URL, server.Client(), newTestExecutor())
	input, err := NewResponseInput("private-user-input")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Respond(context.Background(), input); err != nil {
		t.Fatalf("Respond: %v", err)
	}

	output := logs.String()
	for _, expected := range []string{"model request started", "model response received", "model=deepseek-flash", "request=1", "status=completed", "tool_calls=0"} {
		if !strings.Contains(output, expected) {
			t.Errorf("logs %q do not contain %q", output, expected)
		}
	}
	for _, private := range []string{"private-user-input", "private-response-content", "private-api-key"} {
		if strings.Contains(output, private) {
			t.Errorf("logs contain private payload %q: %s", private, output)
		}
	}
}

func TestRespondReturnsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":"invalid key"}`)
	}))
	defer server.Close()

	client := newClient("test-key", server.URL+"/responses", server.Client(), newTestExecutor())
	input, err := NewResponseInput("hello")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Respond(context.Background(), input); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v, want HTTP 401", err)
	}
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	previous := slog.Default()
	buffer := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(buffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buffer
}
