package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
)

func TestCoreUsesProtocolSpecificCompactionAndRebuildsContextOnProviderSwitch(t *testing.T) {
	var responsesRequests, anthropicRequests int
	responsesServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responsesRequests++
		var request model.ModelRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode Responses request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if !request.Stream {
			t.Error("Core did not stream the Responses request")
		}
		if len(request.ContextManagement) != 1 || request.ContextManagement[0].Type != "compaction" || request.ContextManagement[0].CompactThreshold != 47923 {
			t.Errorf("Responses context_management = %#v", request.ContextManagement)
		}
		if err := writeCompletedSSE(w, &model.ModelResponse{
			ID: "resp-native-compact", Object: "response", Status: "completed", Model: request.Model,
			Output: []model.ResponseOutputItem{
				model.OutputCompaction{ID: "cmp_1", EncryptedContent: "opaque-openai-state"},
				model.OutputReasoning{EncryptedContent: "encrypted-openai-reasoning", Signature: "openai-reasoning-signature"},
				model.OutputMessage{Role: model.RoleAssistant, Content: []model.OutputContentPart{model.OutputText{Text: "OpenAI response"}}},
			},
			Usage: model.ResponseUsage{InputTokens: 50000, OutputTokens: 2, TotalTokens: 50002},
		}); err != nil {
			t.Errorf("write Responses completion: %v", err)
		}
	}))
	defer responsesServer.Close()

	var sawAnthropicRequest bool
	var anthropicHadOpaqueState bool
	anthropicServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicRequests++
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode Anthropic request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if _, leaked := request["context_management"]; leaked {
			t.Error("Responses context_management leaked into Anthropic Messages request")
		}
		encoded, _ := json.Marshal(request)
		anthropicHadOpaqueState = strings.Contains(string(encoded), "opaque-openai-state") ||
			strings.Contains(string(encoded), "encrypted-openai-reasoning") ||
			strings.Contains(string(encoded), "openai-reasoning-signature")
		sawAnthropicRequest = true
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`event: message_start` + "\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\",\"model\":\"shared-model\",\"usage\":{\"input_tokens\":20}}}\n\n",
			`event: content_block_start` + "\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			`event: content_block_delta` + "\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Anthropic response\"}}\n\n",
			`event: content_block_stop` + "\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			`event: message_delta` + "\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n",
			`event: message_stop` + "\ndata: {\"type\":\"message_stop\"}\n\n",
		} {
			_, _ = fmt.Fprint(w, event)
		}
	}))
	defer anthropicServer.Close()

	workspace, stateDir := t.TempDir(), t.TempDir()
	writeProjectSettingsConfig(t, workspace, projectconfig.Config{
		Providers: map[string]model.ProviderConfig{
			"responses": {Protocol: model.ProtocolResponses, BaseURL: responsesServer.URL, APIKey: "responses-key", ServerCompaction: true},
			"messages":  {Protocol: model.ProtocolAnthropicMessages, BaseURL: anthropicServer.URL, APIKey: "anthropic-key"},
		},
		Models: map[string]model.ModelProfile{
			"responses": {Provider: "responses", Model: "shared-model"},
			"messages":  {Provider: "messages", Model: "shared-model", MaxOutputTokens: 1024},
		},
		DefaultModel: "responses", ContextWindow: 64000, ReserveOutputTokens: 4096,
		AutoCompact: true, AutoCompactThreshold: 0.8,
	})
	client := openSettingsCore(t, workspace, stateDir)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	eventStream := subscribeTestEvents(t, client, saved.ID)
	if _, err := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: mustSettingsTaskInput(t, "start long task")}); err != nil {
		t.Fatalf("Responses RunTask: %v", err)
	}
	var events []Event
	for {
		event := <-eventStream
		events = append(events, event)
		if event.Kind == EventTaskFinished {
			break
		}
	}
	responsesSession, err := client.GetSession(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(responsesSession.ModelContext), "opaque-openai-state") ||
		!strings.Contains(string(responsesSession.ModelContext), "encrypted-openai-reasoning") ||
		strings.Contains(string(responsesSession.History), "opaque-openai-state") ||
		strings.Contains(string(responsesSession.History), "openai-reasoning-signature") {
		t.Fatalf("native context was not separated from transcript: context=%s history=%s", responsesSession.ModelContext, responsesSession.History)
	}
	var sawStart, sawCompletion bool
	for _, event := range events {
		if event.Kind != EventContextCompaction || event.SessionID != saved.ID || event.Compaction == nil {
			continue
		}
		sawStart = sawStart || event.Compaction.Stage == "started"
		sawCompletion = sawCompletion || event.Compaction.Stage == "completed"
	}
	if !sawStart || !sawCompletion {
		t.Fatalf("compaction lifecycle events = %#v", events)
	}
	if err := client.SetSessionSettings(context.Background(), saved.ID, SessionSettings{ModelProfile: "messages"}); err != nil {
		t.Fatalf("SetSessionSettings: %v", err)
	}
	if _, err := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: mustSettingsTaskInput(t, "continue through Anthropic Messages")}); err != nil {
		t.Fatalf("Anthropic RunTask: %v", err)
	}
	if responsesRequests != 1 || anthropicRequests != 1 || !sawAnthropicRequest || anthropicHadOpaqueState {
		t.Fatalf("requests responses=%d anthropic=%d sawAnthropic=%v opaqueLeaked=%v", responsesRequests, anthropicRequests, sawAnthropicRequest, anthropicHadOpaqueState)
	}
	updated, err := client.GetSession(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated.History), "OpenAI response") || !strings.Contains(string(updated.History), "Anthropic response") {
		t.Fatalf("canonical transcript lost cross-provider turns: %s", updated.History)
	}
	if updated.ModelContextKey == responsesSession.ModelContextKey {
		t.Fatal("provider switch kept the previous provider's model context key")
	}
}
