package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRespondRunsFunctionCallAndSendsFullHistory(t *testing.T) {
	executor := newTestExecutor()
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.Method != http.MethodPost || r.URL.Path != "/responses" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}

		var request ResponseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Model != DeepSeekFlashModel {
			t.Errorf("model = %q", request.Model)
		}
		if len(request.Tools) != 1 {
			t.Errorf("tools = %d, want 1", len(request.Tools))
		}
		if request.Input == nil || request.Input.Items == nil {
			t.Errorf("expected item-list input, got %#v", request.Input)
		}

		if requestCount == 1 {
			if got := len(request.Input.Items); got != 1 {
				t.Errorf("initial history has %d items, want 1", got)
			}
			_, _ = fmt.Fprint(w, `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"deepseek-flash","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Checking."}]},{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"}]}`)
			return
		}
		if requestCount == 2 {
			if got := len(request.Input.Items); got != 4 {
				t.Errorf("continued history has %d items, want 4", got)
			} else {
				if _, ok := request.Input.Items[1].(InputMessage); !ok {
					t.Errorf("assistant response decoded as %T", request.Input.Items[1])
				}
				if _, ok := request.Input.Items[2].(InputFunctionCall); !ok {
					t.Errorf("function call decoded as %T", request.Input.Items[2])
				}
				if output, ok := request.Input.Items[3].(InputFunctionCallOutput); !ok || output.CallID != "call_1" {
					t.Errorf("tool output decoded as %#v", request.Input.Items[3])
				}
			}
			_, _ = fmt.Fprint(w, `{"id":"resp_2","object":"response","created_at":2,"status":"completed","model":"deepseek-flash","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}]}`)
			return
		}
		t.Errorf("unexpected extra request %d", requestCount)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := newClient("test-key", server.URL+"/responses", server.Client(), executor)
	input, err := NewResponseInput("read a file")
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Respond(context.Background(), input)
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if response.ID != "resp_2" || requestCount != 2 {
		t.Fatalf("response=%#v requests=%d", response, requestCount)
	}
	if len(executor.calls) != 1 || executor.calls[0].CallID != "call_1" {
		t.Fatalf("unexpected tool calls: %#v", executor.calls)
	}
}

func TestRespondRevealsCategoriesAndValidatesVisibleTools(t *testing.T) {
	executor := newCategorizedTestExecutor()
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
			if len(request.Tools) != 1 {
				t.Errorf("initial tools = %d, want only category selector", len(request.Tools))
			} else if selector, ok := request.Tools[0].(FunctionTool); !ok || selector.Name != CategorySelectorName {
				t.Errorf("initial tool = %#v, want category selector", request.Tools[0])
			} else if !strings.Contains(string(selector.Parameters), `"enum":["files","command","web"]`) {
				t.Errorf("selector schema does not enumerate categories: %s", selector.Parameters)
			}
			_, _ = fmt.Fprint(w, `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"function_call","call_id":"select_1","name":"select_tool_category","arguments":"{\"category\":\"files\"}"}]}`)
			return
		}
		if requestCount == 2 {
			if len(request.Tools) != 2 {
				t.Errorf("files category tools = %d, want selector and read_file", len(request.Tools))
			} else if definition, ok := request.Tools[1].(FunctionTool); !ok || definition.Name != "read_file" {
				t.Errorf("revealed tool = %#v, want read_file", request.Tools[1])
			}
			_, _ = fmt.Fprint(w, `{"id":"resp_2","object":"response","status":"completed","output":[{"type":"function_call","call_id":"select_2","name":"select_tool_category","arguments":"{\"category\":\"web\"}"},{"type":"function_call","call_id":"read_1","name":"read_file","arguments":"{}"},{"type":"function_call","call_id":"hidden_1","name":"command","arguments":"{}"}]}`)
			return
		}
		if requestCount == 3 {
			if len(request.Tools) != 2 {
				t.Errorf("web category tools = %d, want selector and web_search", len(request.Tools))
			} else if definition, ok := request.Tools[1].(FunctionTool); !ok || definition.Name != "web_search" {
				t.Errorf("switched tool = %#v, want web_search", request.Tools[1])
			}
			if len(request.Input.Items) != 9 {
				t.Errorf("continued history items = %d, want 9", len(request.Input.Items))
			}
			for _, item := range request.Input.Items {
				if output, ok := item.(InputFunctionCallOutput); ok && output.CallID == "hidden_1" {
					encoded, _ := json.Marshal(output.Output)
					for _, expected := range []string{"belongs to category", "select_tool_category", "next request"} {
						if !strings.Contains(string(encoded), expected) {
							t.Errorf("hidden tool output = %s, want actionable category hint containing %q", encoded, expected)
						}
					}
				}
			}
			_, _ = fmt.Fprint(w, `{"id":"resp_3","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
			return
		}
		if requestCount == 4 {
			if len(request.Tools) != 1 {
				t.Errorf("new task tools = %d, want category selector only", len(request.Tools))
			}
			_, _ = fmt.Fprint(w, `{"id":"resp_4","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second task done"}]}]}`)
			return
		}
		t.Errorf("unexpected request %d", requestCount)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client := newClient("test-key", server.URL, server.Client(), executor)

	for _, prompt := range []string{"first task", "second task"} {
		input, err := NewResponseInput(prompt)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Respond(context.Background(), input); err != nil {
			t.Fatalf("Respond(%q): %v", prompt, err)
		}
	}
	if got, want := len(executor.calls), 1; got != want || executor.calls[0].Name != "read_file" {
		t.Fatalf("executed calls = %#v, want only read_file", executor.calls)
	}
}

func TestRespondMakesHiddenCategoryToolActionableAndLogsMismatch(t *testing.T) {
	logs := captureLogs(t)
	executor := newCategorizedTestExecutor()
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		var request ResponseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotTools := strings.Join(responseToolNameList(request.Tools), ",")
		wantTools := []string{
			CategorySelectorName,
			CategorySelectorName + ",read_file",
			CategorySelectorName + ",web_search",
			CategorySelectorName + ",web_search",
		}
		if requestCount > len(wantTools) {
			t.Errorf("unexpected request %d", requestCount)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if gotTools != wantTools[requestCount-1] {
			t.Errorf("request %d tools = %q, want %q", requestCount, gotTools, wantTools[requestCount-1])
		}
		switch requestCount {
		case 1:
			_, _ = fmt.Fprint(w, `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"function_call","call_id":"select_files","name":"select_tool_category","arguments":"{\"category\":\"files\"}"},{"type":"function_call","call_id":"hidden_web","name":"web_search","arguments":"{\"query\":\"PRIVATE-ARGUMENTS\"}"}]}`)
		case 2:
			_, _ = fmt.Fprint(w, `{"id":"resp_2","object":"response","status":"completed","output":[{"type":"function_call","call_id":"select_web","name":"select_tool_category","arguments":"{\"category\":\"web\"}"}]}`)
		case 3:
			_, _ = fmt.Fprint(w, `{"id":"resp_3","object":"response","status":"completed","output":[{"type":"function_call","call_id":"web_call","name":"web_search","arguments":"{}"}]}`)
		default:
			_, _ = fmt.Fprint(w, `{"id":"resp_4","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
		}
	}))
	defer server.Close()

	client := newClient("test-key", server.URL, server.Client(), executor)
	input, err := NewResponseInput("search the web")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Respond(context.Background(), input); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if requestCount != 4 {
		t.Fatalf("requests = %d, want 4", requestCount)
	}
	if len(executor.calls) != 1 || executor.calls[0].Name != "web_search" {
		t.Fatalf("executed calls = %#v, want only the later visible web_search call", executor.calls)
	}

	var hiddenOutput string
	for _, item := range client.history {
		if output, ok := item.(InputFunctionCallOutput); ok && output.CallID == "hidden_web" && output.Output.Text != nil {
			hiddenOutput = *output.Output.Text
		}
	}
	for _, expected := range []string{"web_search", `category "web"`, CategorySelectorName, "next request"} {
		if !strings.Contains(hiddenOutput, expected) {
			t.Errorf("hidden tool output %q missing %q", hiddenOutput, expected)
		}
	}
	for _, expected := range []string{"tool call is not in the visible tool set", "tool=web_search", "selected_category=files", "visible_tools", "expected_category=web"} {
		if !strings.Contains(logs.String(), expected) {
			t.Errorf("logs %q missing %q", logs.String(), expected)
		}
	}
	if strings.Contains(logs.String(), "PRIVATE-ARGUMENTS") {
		t.Fatalf("logs exposed tool arguments: %s", logs.String())
	}
}

func TestRespondExplainsToolOutsideEveryCategory(t *testing.T) {
	logs := captureLogs(t)
	executor := newCategorizedTestExecutor()
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
			_, _ = fmt.Fprint(w, `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"function_call","call_id":"unknown_tool","name":"imaginary_tool","arguments":"{\"secret\":\"PRIVATE-ARGUMENTS\"}"}]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"resp_2","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
	}))
	defer server.Close()
	client := newClient("test-key", server.URL, server.Client(), executor)
	input, err := NewResponseInput("use a tool")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Respond(context.Background(), input); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if len(executor.calls) != 0 {
		t.Fatalf("executed calls = %#v, want no unknown tool execution", executor.calls)
	}
	var unknownOutput string
	for _, item := range client.history {
		if output, ok := item.(InputFunctionCallOutput); ok && output.CallID == "unknown_tool" && output.Output.Text != nil {
			unknownOutput = *output.Output.Text
		}
	}
	for _, expected := range []string{"imaginary_tool", "not registered in a discoverable category", CategorySelectorName, "listed tool"} {
		if !strings.Contains(unknownOutput, expected) {
			t.Errorf("unknown tool output %q missing %q", unknownOutput, expected)
		}
	}
	if strings.Contains(logs.String(), "PRIVATE-ARGUMENTS") {
		t.Fatalf("logs exposed tool arguments: %s", logs.String())
	}
}

func TestRespondCountsCategorySelectionTowardToolRoundLimit(t *testing.T) {
	executor := newCategorizedTestExecutor()
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount++
		if requestCount == 1 {
			_, _ = fmt.Fprint(w, `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"function_call","call_id":"select_1","name":"select_tool_category","arguments":"{\"category\":\"files\"}"}]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"resp_2","object":"response","status":"completed","output":[{"type":"function_call","call_id":"read_1","name":"read_file","arguments":"{}"}]}`)
	}))
	defer server.Close()
	client := newClient("test-key", server.URL, server.Client(), executor)
	input, err := NewResponseInput("read a file")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.RespondWithOptions(context.Background(), input, RespondOptions{MaxToolRounds: 1})
	if !errors.Is(err, ErrToolCallLimit) {
		t.Fatalf("RespondWithOptions error = %v, want ErrToolCallLimit", err)
	}
	if len(executor.calls) != 0 {
		t.Fatalf("executed calls = %#v, want no calls after selection reached the limit", executor.calls)
	}
}

func TestRespondRejectsUnknownCategorySelection(t *testing.T) {
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
			_, _ = fmt.Fprint(w, `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"function_call","call_id":"select_1","name":"select_tool_category","arguments":"{\"category\":\"hidden\"}"}]}`)
			return
		}
		if len(request.Tools) != 1 {
			t.Errorf("tools after invalid category = %d, want selector only", len(request.Tools))
		}
		_, _ = fmt.Fprint(w, `{"id":"resp_2","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
	}))
	defer server.Close()
	client := newClient("test-key", server.URL, server.Client(), newCategorizedTestExecutor())
	input, err := NewResponseInput("select a category")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Respond(context.Background(), input); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if requestCount != 2 {
		t.Fatalf("requests = %d, want 2", requestCount)
	}
	foundErrorOutput := false
	for _, item := range client.history {
		if output, ok := item.(InputFunctionCallOutput); ok && output.CallID == "select_1" {
			encoded, _ := json.Marshal(output.Output)
			foundErrorOutput = strings.Contains(string(encoded), "unknown category")
		}
	}
	if !foundErrorOutput {
		t.Fatal("history is missing an error output for the unknown category")
	}
}

func TestRespondIsUnlimitedByDefault(t *testing.T) {
	executor := newTestExecutor()
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount++
		if requestCount <= 9 {
			_, _ = fmt.Fprintf(w, `{"id":"resp_%d","object":"response","created_at":1,"status":"completed","model":"deepseek-flash","output":[{"type":"function_call","call_id":"call_%d","name":"read_file","arguments":"{}"}]}`, requestCount, requestCount)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"resp_10","object":"response","created_at":1,"status":"completed","model":"deepseek-flash","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
	}))
	defer server.Close()

	client := newClient("test-key", server.URL+"/responses", server.Client(), executor)
	input, err := NewResponseInput("keep going")
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Respond(context.Background(), input)
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if response == nil || requestCount != 10 || len(executor.calls) != 9 {
		t.Fatalf("response=%#v requests=%d tool calls=%d", response, requestCount, len(executor.calls))
	}
}

func TestRespondWithOptionsStopsAtConfiguredToolRoundLimit(t *testing.T) {
	executor := newTestExecutor()
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount++
		_, _ = fmt.Fprintf(w, `{"id":"resp_%d","object":"response","created_at":1,"status":"completed","model":"deepseek-flash","output":[{"type":"function_call","call_id":"call_%d","name":"read_file","arguments":"{}"}]}`, requestCount, requestCount)
	}))
	defer server.Close()

	client := newClient("test-key", server.URL+"/responses", server.Client(), executor)
	input, err := NewResponseInput("stop after two rounds")
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.RespondWithOptions(context.Background(), input, RespondOptions{MaxToolRounds: 2})
	if !errors.Is(err, ErrToolCallLimit) {
		t.Fatalf("error = %v, want ErrToolCallLimit", err)
	}
	if response == nil || requestCount != 3 || len(executor.calls) != 2 {
		t.Fatalf("response=%#v requests=%d tool calls=%d", response, requestCount, len(executor.calls))
	}
	lastOutput, ok := client.history[len(client.history)-2].(InputFunctionCallOutput)
	if !ok || lastOutput.CallID != "call_3" {
		t.Fatalf("history before interruption marker = %#v, want skipped output for call_3", client.history[len(client.history)-2])
	}
}

func TestRespondWithOptionsAllowsFinalAnswerAtLimitBoundary(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount++
		if requestCount == 1 {
			_, _ = fmt.Fprint(w, `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"}]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"resp_2","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
	}))
	defer server.Close()
	client := newClient("test-key", server.URL, server.Client(), newTestExecutor())
	input, err := NewResponseInput("one tool call")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RespondWithOptions(context.Background(), input, RespondOptions{MaxToolRounds: 1}); err != nil {
		t.Fatalf("final answer at the limit returned an error: %v", err)
	}
	if requestCount != 2 {
		t.Fatalf("requests = %d, want 2", requestCount)
	}
}

func TestRespondWithOptionsRejectsNegativeLimit(t *testing.T) {
	client := newClient("test-key", "", nil, newTestExecutor())
	input, err := NewResponseInput("hello")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RespondWithOptions(context.Background(), input, RespondOptions{MaxToolRounds: -1}); err == nil {
		t.Fatal("expected a negative tool-round limit to fail")
	}
}

type categorizedTestExecutor struct {
	*testToolExecutor
	categories []ToolCategory
	byCategory map[string][]ResponseTool
}

func (e *categorizedTestExecutor) Categories() []ToolCategory {
	return append([]ToolCategory(nil), e.categories...)
}

func (e *categorizedTestExecutor) DefinitionsForCategory(category string) []ResponseTool {
	return append([]ResponseTool(nil), e.byCategory[category]...)
}

func newCategorizedTestExecutor() *categorizedTestExecutor {
	strict := true
	readFile := FunctionTool{
		Name:       "read_file",
		Parameters: json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`),
		Strict:     &strict,
	}
	command := FunctionTool{
		Name:       "command",
		Parameters: json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`),
		Strict:     &strict,
	}
	webSearch := FunctionTool{
		Name:       "web_search",
		Parameters: json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`),
		Strict:     &strict,
	}
	return &categorizedTestExecutor{
		testToolExecutor: &testToolExecutor{definitions: []ResponseTool{readFile, command, webSearch}},
		categories: []ToolCategory{
			{Name: "files", Description: "Workspace files."},
			{Name: "command", Description: "Run commands."},
			{Name: "web", Description: "Search the web."},
		},
		byCategory: map[string][]ResponseTool{
			"files":   {readFile},
			"command": {command},
			"web":     {webSearch},
		},
	}
}
