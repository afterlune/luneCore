package agentgraph

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint/memory"
)

func TestAgentCanAskUserDuringTask(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.SetPermissionMode(tool.PermissionModeReadOnly); err != nil {
		t.Fatal(err)
	}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"ask-1","name":"ask_user","arguments":"{\"questions\":[{\"id\":\"q1\",\"question\":\"Which environment?\",\"options\":[{\"label\":\"local\",\"description\":\"Local workspace\"},{\"label\":\"staging\",\"description\":\"Staging environment\"}]}]}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I will use staging."}]}]}`),
	}}
	agent, err := NewWithOptions(requester, registry, AgentOptions{
		ID: "lead", Name: "Lead", Profile: "main", Provider: model.ProviderDeepSeek, Model: "test-model",
		AskUser: true,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	input, err := model.NewResponseInput("prepare the deployment plan")
	if err != nil {
		t.Fatal(err)
	}
	execution, waiting, err := agent.Start(context.Background(), "ask-user-task", nil, input, TaskOptions{PlanMode: true}, store, graph.Options{})
	if err != nil || waiting.Status != graph.StatusWaiting {
		t.Fatalf("Start = status %q, error %v", waiting.Status, err)
	}
	pending := execution.Pending()
	if len(pending) != 1 || pending[0].Continuation != UserQuestionContinuation {
		t.Fatalf("pending user question = %#v", pending)
	}
	checkpoint, err := execution.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	var question State
	for _, invocation := range checkpoint.Invocations {
		if invocation.ID == pending[0].InvocationID {
			question = invocation.State
		}
	}
	if question.UserQuestions[0].Question != "Which environment?" || len(question.UserQuestions[0].Options) != 2 || question.UserQuestions[0].Options[1].Label != "staging" {
		t.Fatalf("persisted user question = %#v", question)
	}
	answer, err := json.Marshal([]model.UserQuestionResponse{{QuestionID: "q1", Answer: "staging"}})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := execution.Advance(context.Background(), []graph.ResumeInput{{
		InvocationID: pending[0].InvocationID, CallID: pending[0].CallID, Payload: answer,
	}}, graph.Options{})
	if err != nil || completed.Status != graph.StatusCompleted || completed.Final == nil {
		t.Fatalf("Advance = status %q, error %v", completed.Status, err)
	}
	var foundAskUser bool
	for _, definition := range requester.requests[0].Tools {
		if function, ok := definition.(model.FunctionTool); ok && function.Name == askUserToolName {
			foundAskUser = true
		}
	}
	if !foundAskUser {
		t.Fatalf("task tools = %#v, missing %q", requester.requests[0].Tools, askUserToolName)
	}
	if !strings.Contains(requestContextText(requester.requests[0]), "You are in Plan mode.") {
		t.Fatalf("Plan mode user-question instructions = %q", requester.requests[0].Instructions)
	}
	encodedInput, err := json.Marshal(requester.requests[1].Input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encodedInput, []byte("staging")) {
		t.Fatalf("user answer was not returned to the model: %s", encodedInput)
	}
}

func TestGreetingDoesNotAskForUserDecision(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.SetPermissionMode(tool.PermissionModeReadOnly); err != nil {
		t.Fatal(err)
	}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hi!"}]}]}`),
	}}
	agent, err := NewWithOptions(requester, registry, AgentOptions{
		ID: "lead", Name: "Lead", Profile: "main", Provider: model.ProviderDeepSeek, Model: "test-model",
		AskUser: true,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	input, err := model.NewResponseInput("hi")
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "greeting-no-ask", nil, input, TaskOptions{}, store, graph.Options{})
	if err != nil || result.Status != graph.StatusCompleted {
		t.Fatalf("Start = status %q, error %v", result.Status, err)
	}
	if len(requester.requests) != 1 {
		t.Fatalf("model requests = %d, want one response", len(requester.requests))
	}
	containsAsk := func(request model.ResponseRequest) bool {
		for _, definition := range request.Tools {
			if function, ok := definition.(model.FunctionTool); ok && function.Name == askUserToolName {
				return true
			}
		}
		return false
	}
	if !containsAsk(requester.requests[0]) {
		t.Fatal("read-only task did not expose ask_user")
	}
	if result.Final == nil || len(result.Final.UserQuestions) != 0 {
		t.Fatalf("hidden ask_user call left a user question in completed state: %#v", result.Final)
	}
}
