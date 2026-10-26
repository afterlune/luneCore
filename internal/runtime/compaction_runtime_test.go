package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
)

func compactionTestConfig(serverURL string) projectconfig.ProjectConfig {
	return projectSettingsForTest(
		map[string]model.ProviderConfig{"vendor": {Protocol: model.ProtocolResponses, BaseURL: serverURL, APIKey: "key"}},
		map[string]model.ModelProfile{"main": {Provider: "vendor", Model: "test-model"}},
		"main", projectconfig.RuntimeConfig{ContextWindow: 100000, AutoCompact: true, AutoCompactThreshold: 0.8},
	)
}

func TestAutomaticCompactionUsesStoredMeasuredInput(t *testing.T) {
	var requests int
	var sawSummary bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request model.ModelRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		encoded, _ := json.Marshal(request.Input)
		if strings.Contains(string(encoded), "handoff summary") {
			sawSummary = true
		}
		response := &model.ModelResponse{
			ID: "resp", Object: "response", Status: "completed", Model: request.Model,
			Output: []model.ResponseOutputItem{model.OutputMessage{Role: model.RoleAssistant,
				Content: []model.OutputContentPart{model.OutputText{Text: "done"}}}},
			Usage: model.ResponseUsage{InputTokens: 90000, OutputTokens: 1, TotalTokens: 90001},
		}
		if !request.Stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		if err := writeCompletedSSE(w, response); err != nil {
			t.Errorf("write SSE: %v", err)
		}
	}))
	defer server.Close()

	workspace, stateDir := t.TempDir(), t.TempDir()
	writeProjectSettingsConfig(t, workspace, compactionTestConfig(server.URL))
	client := openSettingsCore(t, workspace, stateDir)
	defer client.Close()
	session, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	events := subscribeTestEvents(t, client, session.ID)
	for _, input := range []string{"first turn", "second turn"} {
		if _, err := client.RunTask(context.Background(), TaskRequest{SessionID: session.ID, Input: mustSettingsTaskInput(t, input)}); err != nil {
			t.Fatalf("RunTask %q: %v", input, err)
		}
		for {
			if event := <-events; event.Kind == EventTaskFinished {
				break
			}
		}
	}
	if requests < 3 || !sawSummary {
		t.Fatalf("measured pressure did not compact: requests=%d summary=%v", requests, sawSummary)
	}
}

func TestCoreCompactReturnsResultAndEmitsEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request model.ModelRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if request.Stream {
			_ = writeCompletedSSE(w, &model.ModelResponse{Status: "completed", Model: request.Model,
				Output: []model.ResponseOutputItem{model.OutputMessage{Role: model.RoleAssistant,
					Content: []model.OutputContentPart{model.OutputText{Text: "summary of older work"}}}},
				Usage: model.ResponseUsage{InputTokens: 40, OutputTokens: 4, TotalTokens: 44}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&model.ModelResponse{Status: "completed", Model: request.Model,
			Output: []model.ResponseOutputItem{model.OutputMessage{Role: model.RoleAssistant,
				Content: []model.OutputContentPart{model.OutputText{Text: "summary of older work"}}}},
			Usage: model.ResponseUsage{InputTokens: 40, OutputTokens: 4, TotalTokens: 44}})
	}))
	defer server.Close()

	workspace, stateDir := t.TempDir(), t.TempDir()
	writeProjectSettingsConfig(t, workspace, compactionTestConfig(server.URL))
	client := openSettingsCore(t, workspace, stateDir)
	defer client.Close()

	history := model.NewResponseInputItems(
		model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("old request")},
		model.InputMessage{Role: model.RoleAssistant, Content: model.NewMessageText(strings.Repeat("detail.", 600))},
		model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("recent request")},
		model.InputMessage{Role: model.RoleAssistant, Content: model.NewMessageText("recent answer")},
	)
	encoded, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.CreateSession(context.Background(), encoded)
	if err != nil {
		t.Fatal(err)
	}
	events := subscribeTestEvents(t, client, session.ID)
	result, err := client.Compact(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !result.Compacted || result.Turns == 0 || result.Before <= 0 || result.After <= 0 {
		t.Fatalf("compaction result = %#v", result)
	}
	var started, completed bool
	for {
		select {
		case event := <-events:
			if event.Kind == EventContextCompaction && event.Compaction != nil && event.Compaction.Reason == "manual" {
				started = started || event.Compaction.Stage == "started"
				completed = completed || event.Compaction.Stage == "completed"
			}
			if started && completed {
				goto done
			}
		case <-time.After(2 * time.Second):
			goto done
		}
	}
done:
	if !started || !completed {
		t.Fatalf("manual compaction events = started:%v completed:%v", started, completed)
	}
}

func TestCoreManualCompactUsesResponsesCompactEndpoint(t *testing.T) {
	var compactRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/responses/compact" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		compactRequests++
		var request model.ModelRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode compact request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if !strings.Contains(request.Instructions, "context checkpoint") || !strings.Contains(string(mustJSON(t, request.Input)), "latest request") {
			t.Errorf("compact request = %#v", request)
		}
		_, _ = fmt.Fprint(w, `{"id":"compact-response","object":"response.compaction","status":"completed","model":"test-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"retained compacted content"}]},{"type":"compaction","id":"cmp-1","encrypted_content":"opaque-server-state","created_by":"responses.compact"}],"usage":{"input_tokens":200,"output_tokens":12,"total_tokens":212}}`)
	}))
	defer server.Close()

	workspace, stateDir := t.TempDir(), t.TempDir()
	config := projectSettingsForTest(
		map[string]model.ProviderConfig{"vendor": {Protocol: model.ProtocolResponses, BaseURL: server.URL, APIKey: "key", ServerCompaction: true}},
		map[string]model.ModelProfile{"main": {Provider: "vendor", Model: "test-model"}},
		"main", projectconfig.RuntimeConfig{ContextWindow: 100000, AutoCompact: true, AutoCompactThreshold: 0.8},
	)
	writeProjectSettingsConfig(t, workspace, config)
	client := openSettingsCore(t, workspace, stateDir)
	defer client.Close()
	history, err := json.Marshal(model.NewResponseInputItems(
		model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("first request")},
		model.InputMessage{Role: model.RoleAssistant, Content: model.NewMessageText("first answer")},
		model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("latest request")},
	))
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.CreateSession(context.Background(), history)
	if err != nil {
		t.Fatal(err)
	}
	events := subscribeTestEvents(t, client, session.ID)
	result, err := client.Compact(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if compactRequests != 1 || result.Backend != "server" || !result.Compacted {
		t.Fatalf("compact requests=%d, result=%#v", compactRequests, result)
	}
	saved, err := client.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved.ModelContext), "opaque-server-state") || strings.Contains(string(saved.History), "opaque-server-state") {
		t.Fatalf("server compaction state was not isolated: context=%s history=%s", saved.ModelContext, saved.History)
	}
	var started, completed bool
	for {
		select {
		case event := <-events:
			if event.Kind == EventContextCompaction && event.Compaction != nil {
				started = started || event.Compaction.Stage == "started" && event.Compaction.Mode == "server"
				completed = completed || event.Compaction.Stage == "completed" && event.Compaction.Mode == "server"
			}
			if started && completed {
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("server compaction events missing: started=%v completed=%v", started, completed)
		}
	}
}

func TestCoreManualCompactFallsBackWhenStandaloneEndpointIsUnsupported(t *testing.T) {
	var compactRequests, summaryRequests int
	oldDetails := strings.Repeat("earlier verified project requirement ", 5000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/responses/compact":
			compactRequests++
			http.NotFound(w, r)
		case "/responses":
			summaryRequests++
			var request model.ModelRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode local summary request: %v", err)
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			if !strings.Contains(string(mustJSON(t, request.Input)), "handoff summary") || !strings.Contains(string(mustJSON(t, request.Input)), oldDetails) {
				t.Error("local fallback did not summarize the original context")
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(&model.ModelResponse{
				ID: "local-summary", Object: "response", Status: "completed", Model: request.Model,
				Output: []model.ResponseOutputItem{model.OutputMessage{Role: model.RoleAssistant,
					Content: []model.OutputContentPart{model.OutputText{Text: "Earlier requirements remain in force."}}}},
				Usage: model.ResponseUsage{InputTokens: 18000, OutputTokens: 10, TotalTokens: 18010},
			})
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	workspace, stateDir := t.TempDir(), t.TempDir()
	config := projectSettingsForTest(
		map[string]model.ProviderConfig{"vendor": {Protocol: model.ProtocolResponses, BaseURL: server.URL, APIKey: "key", ServerCompaction: true}},
		map[string]model.ModelProfile{"main": {Provider: "vendor", Model: "test-model"}},
		"main", projectconfig.RuntimeConfig{ContextWindow: 100000, AutoCompact: true, AutoCompactThreshold: 0.8},
	)
	writeProjectSettingsConfig(t, workspace, config)
	client := openSettingsCore(t, workspace, stateDir)
	defer client.Close()
	history, err := json.Marshal(model.NewResponseInputItems(
		model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("first request")},
		model.InputMessage{Role: model.RoleAssistant, Content: model.NewMessageText(oldDetails)},
		model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("latest request")},
	))
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.CreateSession(context.Background(), history)
	if err != nil {
		t.Fatal(err)
	}
	events := subscribeTestEvents(t, client, session.ID)
	result, err := client.Compact(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if compactRequests != 1 || summaryRequests != 1 || result.Backend != "local" || !result.Fallback || !result.Compacted {
		t.Fatalf("compact requests=%d summary requests=%d result=%#v", compactRequests, summaryRequests, result)
	}
	var fallback, completed bool
	for {
		select {
		case event := <-events:
			if event.Kind == EventContextCompaction && event.Compaction != nil {
				fallback = fallback || event.Compaction.Stage == "fallback" && event.Compaction.Mode == "server"
				completed = completed || event.Compaction.Stage == "completed" && event.Compaction.Mode == "local"
			}
			if fallback && completed {
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("fallback compaction events missing: fallback=%v completed=%v", fallback, completed)
		}
	}
}
