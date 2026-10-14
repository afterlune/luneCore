package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
)

func TestMigrateSessionDataV2ToV3KeepsTranscriptAndDropsUnkeyedContext(t *testing.T) {
	history := json.RawMessage(`[{"type":"message","role":"user","content":"continue"}]`)
	saved, changed, err := migrateSessionData(Session{
		ID: "session", UpdatedAt: time.Unix(10, 0), DataVersion: 2,
		History: history, ModelContext: json.RawMessage(`[{"type":"compaction","encrypted_content":"stale"}]`),
		ModelContextKey: "old-key",
	})
	if err != nil {
		t.Fatalf("migrateSessionData: %v", err)
	}
	if !changed || saved.DataVersion != 3 || string(saved.History) != string(history) {
		t.Fatalf("migrated session = %#v, changed=%v", saved, changed)
	}
	if len(saved.ModelContext) != 0 || saved.ModelContextKey != "" {
		t.Fatalf("unkeyed model context survived migration: %#v", saved)
	}
}

func TestModelContextKeyUsesSelectedProtocolAndEndpointCapability(t *testing.T) {
	responses := modelContextKey("vendor", "same-model", model.ProviderConfig{
		Protocol: model.ProtocolResponses, BaseURL: "https://gateway.test", ServerCompaction: true,
	})
	anthropic := modelContextKey("vendor", "same-model", model.ProviderConfig{
		Protocol: model.ProtocolAnthropicMessages, BaseURL: "https://gateway.test",
	})
	localResponses := modelContextKey("vendor", "same-model", model.ProviderConfig{
		Protocol: model.ProtocolResponses, BaseURL: "https://gateway.test",
	})
	if responses == anthropic || responses == localResponses || anthropic == localResponses {
		t.Fatalf("protocol/capability did not isolate contexts: %q %q %q", responses, anthropic, localResponses)
	}
}

func TestSessionConversationPersistsProviderContextAndClearsItOnHistoryEdit(t *testing.T) {
	store, err := session.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	client := &Core{workspace: "workspace", sessions: store}
	saved, err := client.createSession(context.Background(), json.RawMessage(`[{"type":"message","role":"user","content":"hello"}]`), SessionSettings{ModelProfile: "main"})
	if err != nil {
		t.Fatal(err)
	}
	transcript := model.NewResponseInputItems(model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("hello")})
	modelContext := model.NewResponseInputItems(model.UnknownInputItem{Raw: json.RawMessage(`{"type":"compaction","id":"cmp-1","encrypted_content":"opaque"}`)})
	if err := client.setSessionConversation(context.Background(), saved.ID, transcript, modelContext, "responses:openai:gpt-test"); err != nil {
		t.Fatalf("setSessionConversation: %v", err)
	}
	loaded, err := client.GetSession(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSessionHistory(loaded.ModelContext) || loaded.ModelContextKey != "responses:openai:gpt-test" {
		t.Fatalf("provider context = %s key=%q", loaded.ModelContext, loaded.ModelContextKey)
	}
	if err := client.SetSessionHistory(context.Background(), saved.ID, json.RawMessage(`[]`)); err != nil {
		t.Fatalf("SetSessionHistory: %v", err)
	}
	loaded, err = client.GetSession(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hasSessionHistory(loaded.ModelContext) || loaded.ModelContextKey != "" {
		t.Fatalf("history edit retained stale provider context: %s key=%q", loaded.ModelContext, loaded.ModelContextKey)
	}
}

func TestSessionConversationDoesNotDuplicateUncompactedContext(t *testing.T) {
	store, err := session.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	client := &Core{workspace: "workspace", sessions: store}
	saved, err := client.createSession(context.Background(), json.RawMessage(`null`), SessionSettings{ModelProfile: "main"})
	if err != nil {
		t.Fatal(err)
	}
	conversation := model.NewResponseInputItems(
		model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("look at this image")},
		model.InputMessage{Role: model.RoleUser, Content: model.NewMessageContentParts(
			model.InputImage{ImageURL: "data:image/png;base64," + strings.Repeat("a", 64)},
		)},
	)
	if err := client.setSessionConversation(context.Background(), saved.ID, conversation, conversation, "same-context-key"); err != nil {
		t.Fatalf("setSessionConversation: %v", err)
	}
	loaded, err := client.GetSession(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hasSessionHistory(loaded.ModelContext) || loaded.ModelContextKey != "" {
		t.Fatalf("identical context was stored twice: model_context=%s key=%q", loaded.ModelContext, loaded.ModelContextKey)
	}
	if !strings.Contains(string(loaded.History), "data:image/png;base64,") {
		t.Fatalf("canonical history lost image data: %s", loaded.History)
	}
}
