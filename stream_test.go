package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
)

func TestCoreEmitsLeadTextDeltasWithTaskIdentity(t *testing.T) {
	const taskModel = "task-stream-override"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request model.ResponseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode streamed request: %v", err)
		} else if !request.Stream {
			t.Error("Core request did not enable provider streaming")
		} else if request.Model != taskModel {
			t.Errorf("stream request model = %q, want %q", request.Model, taskModel)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"item_id\":\"msg_1\",\"output_index\":0,\"delta\":\"hello\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_stream\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"deepseek-test\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	defer server.Close()

	workspace, stateDir := t.TempDir(), t.TempDir()
	writeProjectSettingsConfig(t, workspace, projectconfig.Config{
		Providers: map[string]model.ProviderConfig{
			model.ProviderDeepSeek: {Protocol: model.ProtocolResponses, BaseURL: server.URL, APIKey: "test-key"},
		},
		Models:       map[string]model.ModelProfile{"primary": {Provider: model.ProviderDeepSeek, Model: "deepseek-test"}},
		DefaultModel: "primary", ContextWindow: 64000,
	})
	client := openSettingsCore(t, workspace, stateDir)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	var events []Event
	client.SetEventHandler(func(event Event) { events = append(events, event) })
	if _, err := client.RunTask(context.Background(), TaskRequest{
		SessionID: saved.ID, Input: mustSettingsTaskInput(t, "say hello"), Model: taskModel,
	}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	var taskID string
	var sawTextDelta, sawCompleted, sawTaskWindow, sawUsageWindow bool
	var sawTaskModel, sawStreamModel, sawUsageModel bool
	for _, event := range events {
		switch event.Kind {
		case EventTaskStarted:
			taskID = event.TaskID
			sawTaskWindow = event.ContextWindow == 64000
			sawTaskModel = event.Model == taskModel
		case EventModelStream:
			if event.AgentID != "lead" || event.TaskID != taskID || event.SessionID != saved.ID || event.Stream == nil {
				t.Fatalf("stream event lost task/session/agent identity: %#v", event)
			}
			sawStreamModel = sawStreamModel || event.Model == taskModel
			switch event.Stream.Type {
			case "text_delta":
				sawTextDelta = sawTextDelta || event.Stream.Delta == "hello"
			case "completed":
				sawCompleted = true
			}
		case EventTaskFinished:
			if event.TaskID != taskID || event.Status != TaskCompleted {
				t.Fatalf("task finished event = %#v", event)
			}
		case EventUsage:
			sawUsageWindow = event.AgentID == "lead" && event.ContextWindow == 64000
			sawUsageModel = event.Model == taskModel
		}
	}
	if !sawTextDelta || !sawCompleted || !sawTaskWindow || !sawUsageWindow || !sawTaskModel || !sawStreamModel || !sawUsageModel {
		t.Fatalf("stream events = %#v, want delta, completion, effective context window, and task model", events)
	}
	updated, err := client.GetSession(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.UsageRequests) != 1 || updated.UsageRequests[0].ContextWindow != 64000 || updated.UsageRequests[0].Model != taskModel {
		t.Fatalf("persisted request usage = %#v; want context window and task model", updated.UsageRequests)
	}
}

func TestCoreStreamingCancellationPausesAndResumeStreamsAgain(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request model.ResponseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode streamed request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if !request.Stream {
			t.Error("Core request did not enable provider streaming")
		}
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"item_id\":\"partial_1\",\"output_index\":0,\"delta\":\"partial before cancellation\"}\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			} else {
				t.Error("test server response does not support flushing")
			}
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"item_id\":\"resumed_1\",\"output_index\":0,\"delta\":\"resumed result\"}\n\n")
		if err := writeCompletedSSE(w, &model.ModelResponse{
			ID: "resumed-response", Object: "response", Status: "completed", Model: request.Model,
			Output: []model.ResponseOutputItem{model.OutputMessage{
				Role:    model.RoleAssistant,
				Content: []model.OutputContentPart{model.OutputText{Text: "resumed result"}},
			}},
		}); err != nil {
			t.Errorf("write resumed response: %v", err)
		}
	}))
	defer server.Close()

	workspace, stateDir := t.TempDir(), t.TempDir()
	writeProjectSettingsConfig(t, workspace, projectconfig.Config{
		Providers: map[string]model.ProviderConfig{
			model.ProviderDeepSeek: {Protocol: model.ProtocolResponses, BaseURL: server.URL, APIKey: "test-key"},
		},
		Models:       map[string]model.ModelProfile{"primary": {Provider: model.ProviderDeepSeek, Model: "deepseek-test"}},
		DefaultModel: "primary",
	})
	client := openSettingsCore(t, workspace, stateDir)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan Event, 32)
	client.SetEventHandler(func(event Event) { events <- event })
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		_, runErr := client.RunTask(ctx, TaskRequest{SessionID: saved.ID, Input: mustSettingsTaskInput(t, "continue safely")})
		runDone <- runErr
	}()

	firstTaskID := ""
	gotPartial := false
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for !gotPartial {
		select {
		case event := <-events:
			if event.Kind == EventTaskStarted {
				firstTaskID = event.TaskID
			}
			if event.Kind == EventModelStream && event.Stream != nil && event.Stream.Type == "text_delta" {
				if event.TaskID != firstTaskID || event.SessionID != saved.ID || event.Stream.Delta != "partial before cancellation" {
					t.Fatalf("first partial event = %#v, task started as %q", event, firstTaskID)
				}
				gotPartial = true
			}
		case <-timeout.C:
			cancel()
			t.Fatal("Core did not emit the first streamed delta")
		}
	}
	cancel()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted RunTask error = %v, want context canceled", err)
	}
	timeout.Reset(5 * time.Second)
	paused := false
	for !paused {
		select {
		case event := <-events:
			if event.Kind == EventTaskFinished && event.TaskID == firstTaskID {
				if event.Status != TaskPaused {
					t.Fatalf("interrupted task status = %q, want paused", event.Status)
				}
				paused = true
			}
		case <-timeout.C:
			t.Fatal("Core did not emit the paused task event")
		}
	}
	if pending, err := client.HasPending(context.Background(), saved.ID); err != nil || !pending {
		t.Fatalf("pending checkpoint after cancellation = %v, error=%v; want paused task", pending, err)
	}

	response, err := client.ResumeTask(context.Background(), saved.ID)
	if err != nil {
		t.Fatalf("ResumeTask after stream cancellation: %v", err)
	}
	if len(response) == 0 {
		t.Fatal("resumed stream returned an empty response")
	}
	timeout.Reset(5 * time.Second)
	var resumedTaskID string
	gotResumedDelta, completed := false, false
	for !completed {
		select {
		case event := <-events:
			switch event.Kind {
			case EventTaskStarted:
				resumedTaskID = event.TaskID
			case EventModelStream:
				if event.Stream == nil {
					continue
				}
				if event.TaskID != resumedTaskID || event.SessionID != saved.ID {
					t.Fatalf("resumed stream event lost task/session identity: %#v", event)
				}
				if event.Stream.Type == "text_delta" && event.Stream.Delta == "resumed result" {
					gotResumedDelta = true
				}
			case EventTaskFinished:
				if event.TaskID == resumedTaskID {
					if event.Status != TaskCompleted {
						t.Fatalf("resumed task status = %q, want completed", event.Status)
					}
					completed = true
				}
			}
		case <-timeout.C:
			t.Fatal("Core did not finish the resumed stream")
		}
	}
	if resumedTaskID == "" || resumedTaskID == firstTaskID || !gotResumedDelta || requests.Load() != 2 {
		t.Fatalf("resume task=%q first task=%q delta=%v provider requests=%d", resumedTaskID, firstTaskID, gotResumedDelta, requests.Load())
	}
}

func TestCoreProviderFailureAfterTextDeltaFailsTaskAndEmitsStreamFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request model.ResponseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode streamed request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"item_id\":\"partial_1\",\"output_index\":0,\"delta\":\"visible partial output\"}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		data, err := json.Marshal(model.ResponseStreamEvent{
			Type: model.EventResponseFailed, SequenceNumber: 2,
			Response: &model.ModelResponse{
				ID: "failed-response", Object: "response", Status: "failed", Model: request.Model,
				Error: &model.ResponseError{Code: "injected_failure", Message: "injected stream failure"},
			},
		})
		if err != nil {
			t.Errorf("encode failed response: %v", err)
			return
		}
		_, _ = fmt.Fprintf(w, "event: response.failed\ndata: %s\n\n", data)
	}))
	defer server.Close()

	workspace, stateDir := t.TempDir(), t.TempDir()
	writeProjectSettingsConfig(t, workspace, projectconfig.Config{
		Providers: map[string]model.ProviderConfig{
			model.ProviderDeepSeek: {Protocol: model.ProtocolResponses, BaseURL: server.URL, APIKey: "test-key"},
		},
		Models:       map[string]model.ModelProfile{"primary": {Provider: model.ProviderDeepSeek, Model: "deepseek-test"}},
		DefaultModel: "primary",
	})
	client := openSettingsCore(t, workspace, stateDir)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	client.SetEventHandler(func(event Event) { events = append(events, event) })
	_, runErr := client.RunTask(context.Background(), TaskRequest{
		SessionID: saved.ID, Input: mustSettingsTaskInput(t, "stream and fail"),
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "injected stream failure") {
		t.Fatalf("RunTask error = %v, want injected provider failure", runErr)
	}
	var taskID string
	var deltaIndex, failureIndex, finishIndex = -1, -1, -1
	for index, event := range events {
		switch event.Kind {
		case EventTaskStarted:
			taskID = event.TaskID
		case EventModelStream:
			if event.TaskID != taskID || event.SessionID != saved.ID || event.Stream == nil {
				t.Fatalf("stream event lost task/session identity: %#v", event)
			}
			switch event.Stream.Type {
			case "text_delta":
				if event.Stream.Delta == "visible partial output" {
					deltaIndex = index
				}
			case "failed":
				failureIndex = index
			}
		case EventTaskFinished:
			if event.TaskID == taskID {
				if event.Status != TaskFailed || !strings.Contains(event.Err, "injected stream failure") {
					t.Fatalf("failed task event = %#v", event)
				}
				finishIndex = index
			}
		}
	}
	if deltaIndex < 0 || failureIndex <= deltaIndex || finishIndex <= failureIndex {
		t.Fatalf("stream failure sequence delta=%d failure=%d task finish=%d events=%#v", deltaIndex, failureIndex, finishIndex, events)
	}
	if pending, err := client.HasPending(context.Background(), saved.ID); err != nil || pending {
		t.Fatalf("failed provider stream pending=%v error=%v; want failed task without resumable checkpoint", pending, err)
	}
}

func writeCompletedSSE(w http.ResponseWriter, response *model.ModelResponse) error {
	data, err := json.Marshal(model.ResponseStreamEvent{
		Type: model.EventResponseCompleted, SequenceNumber: 2, Response: response,
	})
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, err = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", data)
	return err
}
