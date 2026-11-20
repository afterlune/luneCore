package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	publicmodel "github.com/afterlune/luneCore/model"
)

func TestCloneAndContextHelpers(t *testing.T) {
	in := NewResponseInputItems(
		InputMessage{Role: RoleUser, Content: NewMessageContentParts(
			InputText{Text: "hello"},
			InputImage{ImageURL: "https://example.com/a.png"},
		)},
		InputFunctionCall{CallID: "c1", Name: "read_file", Arguments: `{}`},
		InputFunctionCallOutput{CallID: "c1", Output: NewToolOutputParts(InputText{Text: "ok"})},
		UnknownInputItem{Raw: json.RawMessage(`{"type":"future"}`)},
	)
	clonedIn, err := CloneResponseInput(*in)
	if err != nil || len(clonedIn.Items) != 4 {
		t.Fatalf("CloneResponseInput = %#v, %v", clonedIn, err)
	}
	if _, err := CloneResponseInput(ResponseInput{Text: new("hi"), Items: in.Items}); err == nil {
		t.Fatal("expected error for invalid ResponseInput with both text and items")
	}

	resp := &ModelResponse{
		ID:     "r1",
		Status: "completed",
		Error:  &ResponseError{Code: "none", Message: "ok"},
		Output: []ResponseOutputItem{
			OutputMessage{ID: "m1", Role: "assistant", Content: []OutputContentPart{OutputText{Text: "hi"}}},
			OutputFunctionCall{ID: "fc1", CallID: "c1", Name: "glob", Arguments: `{}`},
			OutputCompaction{ID: "cmp1", EncryptedContent: "enc"},
			UnknownOutputItem{Raw: json.RawMessage(`{"type":"x"}`)},
		},
	}
	clonedResp, err := CloneModelResponse(resp)
	if err != nil || clonedResp == nil || len(clonedResp.Output) != 4 {
		t.Fatalf("CloneModelResponse = %#v, %v", clonedResp, err)
	}

	ctx := WithAgentID(context.Background(), "agent-1")
	if AgentIDFromContext(ctx) != "agent-1" {
		t.Fatal("AgentIDFromContext mismatch")
	}
	ctx = WithRequestTrace(ctx, RequestTrace{ID: "req-1"})
	if tr := RequestTraceFromContext(ctx); tr.ID != "req-1" {
		t.Fatalf("RequestTraceFromContext = %#v", tr)
	}
	ctx = WithInputAudioSupport(ctx, true)
	if !InputAudioSupported(ctx) {
		t.Fatal("InputAudioSupported should be true")
	}
	ctx = WithUsagePurpose(ctx, UsagePurposeTeam)
	if UsagePurposeFromContext(ctx) != UsagePurposeTeam {
		t.Fatal("UsagePurposeFromContext mismatch")
	}

	if !ToolPermissionWorkspaceRead.Valid() || ToolPermissionClass("bad").Valid() {
		t.Fatal("unexpected ToolPermissionClass.Valid")
	}
}

func TestProviderCatalogAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r1","status":"completed","output":[]}`))
	}))
	defer srv.Close()

	cfg := Config{
		Providers: map[string]ProviderConfig{
			"deepseek": {Protocol: ProtocolResponses, BaseURL: srv.URL, APIKey: "k1"},
		},
		Models: map[string]ModelProfile{
			"flash": {Provider: "deepseek", Model: "deepseek-chat"},
		},
		DefaultModel: "flash",
	}
	cat, err := NewProviderCatalog(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cat.DefaultModel() != "flash" || cat.ParallelLimit() <= 0 {
		t.Fatalf("unexpected catalog defaults: %q %d", cat.DefaultModel(), cat.ParallelLimit())
	}
	if _, ok := cat.Profile("flash"); !ok {
		t.Fatal("expected flash profile")
	}
	if _, ok := cat.ProviderConfig("deepseek"); !ok {
		t.Fatal("expected deepseek provider config")
	}
	if len(cat.Profiles()) != 1 {
		t.Fatal("expected 1 profile")
	}
	client, err := cat.NewClient("flash", newTestExecutor(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if client.ProfileID() != "flash" || client.ProviderName() != "deepseek" || client.ModelName() != "deepseek-chat" {
		t.Fatalf("unexpected client metadata: %q %q %q", client.ProfileID(), client.ProviderName(), client.ModelName())
	}

	if !IsContextWindowError(&publicmodel.RequestError{Category: publicmodel.ErrorContextWindow}) {
		t.Fatal("expected IsContextWindowError=true")
	}
	if IsContextWindowError(nil) {
		t.Fatal("expected IsContextWindowError(nil)=false")
	}
	if ResponseFailure(&ResponseError{Code: "bad", Message: "oops"}) == nil {
		t.Fatal("expected ResponseFailure error")
	}
}
