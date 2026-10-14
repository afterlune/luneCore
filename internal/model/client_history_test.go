package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRespondKeepsHistoryAcrossUserTurns(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		var request ResponseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if requestCount == 1 {
			if len(request.Input.Items) != 1 {
				t.Errorf("first turn input items = %d, want 1", len(request.Input.Items))
			}
			_, _ = fmt.Fprint(w, `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"First answer."}]}]}`)
			return
		}
		if requestCount == 2 {
			if len(request.Input.Items) != 3 {
				t.Errorf("second turn history has %d items, want 3", len(request.Input.Items))
			} else {
				if first, ok := request.Input.Items[0].(InputMessage); !ok || first.Role != RoleUser {
					t.Errorf("first item = %#v, want original user message", request.Input.Items[0])
				}
				if assistant, ok := request.Input.Items[1].(InputMessage); !ok || assistant.Role != RoleAssistant {
					t.Errorf("second item = %#v, want prior assistant message", request.Input.Items[1])
				}
				if second, ok := request.Input.Items[2].(InputMessage); !ok || second.Role != RoleUser {
					t.Errorf("third item = %#v, want new user message", request.Input.Items[2])
				}
			}
			_, _ = fmt.Fprint(w, `{"id":"resp_2","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Second answer."}]}]}`)
			return
		}
		t.Errorf("unexpected request %d", requestCount)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client := newClient("test-key", server.URL+"/responses", server.Client(), newTestExecutor())

	for _, text := range []string{"first question", "follow-up"} {
		input, err := NewResponseInput(text)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Respond(context.Background(), input); err != nil {
			t.Fatalf("Respond(%q): %v", text, err)
		}
	}
	if requestCount != 2 {
		t.Fatalf("requests = %d, want 2", requestCount)
	}
}

func TestRespondPreservesHistoryWhenCanceledDuringRequest(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(requestStarted)
		<-releaseHandler
	}))
	defer server.Close()
	defer close(releaseHandler)
	client := newClient("test-key", server.URL, server.Client(), newTestExecutor())
	input, err := NewResponseInput("cancel this")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.Respond(ctx, input)
		result <- err
	}()
	<-requestStarted
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Respond error = %v, want context.Canceled", err)
	}
	if len(client.history) != 2 {
		t.Fatalf("history items = %d, want user input and interruption marker", len(client.history))
	}
	marker, ok := client.history[1].(InputMessage)
	if !ok || marker.Role != RoleAssistant {
		t.Fatalf("history marker = %#v, want assistant interruption marker", client.history[1])
	}
}

func TestRespondPreservesCompletedAndSkippedToolCallsOnCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"},{"type":"function_call","call_id":"call_2","name":"read_file","arguments":"{}"}]}`)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	executor := &cancelingToolExecutor{testToolExecutor: newTestExecutor(), cancel: cancel}
	client := newClient("test-key", server.URL, server.Client(), executor)
	input, err := NewResponseInput("cancel between tools")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Respond(ctx, input)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Respond error = %v, want context.Canceled", err)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("executed calls = %d, want 1", len(executor.calls))
	}
	var outputs []InputFunctionCallOutput
	for _, item := range client.history {
		if output, ok := item.(InputFunctionCallOutput); ok {
			outputs = append(outputs, output)
		}
	}
	if len(outputs) != 2 || outputs[0].CallID != "call_1" || outputs[1].CallID != "call_2" {
		t.Fatalf("function call outputs = %#v, want outputs for both call IDs", outputs)
	}
	if len(client.history) == 0 {
		t.Fatal("interrupted history was not saved")
	}
	marker, ok := client.history[len(client.history)-1].(InputMessage)
	if !ok || marker.Role != RoleAssistant {
		t.Fatalf("final history item = %#v, want assistant interruption marker", client.history[len(client.history)-1])
	}
}

type cancelingToolExecutor struct {
	*testToolExecutor
	cancel context.CancelFunc
}

func (e *cancelingToolExecutor) ExecuteCall(_ context.Context, call OutputFunctionCall) InputFunctionCallOutput {
	e.calls = append(e.calls, call)
	e.cancel()
	return InputFunctionCallOutput{CallID: call.CallID, Output: NewToolOutputText("completed before interruption")}
}
