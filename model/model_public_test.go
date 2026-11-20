package model

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestModelProfileEffectiveDisplayName(t *testing.T) {
	if got := (ModelProfile{Model: "m1"}).EffectiveDisplayName(); got != "m1" {
		t.Fatalf("got %q", got)
	}
	if got := (ModelProfile{Model: "m1", DisplayName: "Model One"}).EffectiveDisplayName(); got != "Model One" {
		t.Fatalf("got %q", got)
	}
}

func TestConfigValidate(t *testing.T) {
	valid := Config{
		Providers: map[string]ProviderConfig{
			"deepseek": {Protocol: ProtocolResponses, BaseURL: DeepSeekResponsesBaseURL, ServerCompaction: true},
		},
		Profiles: map[string]ModelProfile{
			"flash": {Provider: "deepseek", Model: "deepseek-chat", SupportsInputAudio: true},
		},
		DefaultProfile: "flash",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config failed: %v", err)
	}

	cases := []struct {
		name string
		cfg  Config
	}{
		{"empty", Config{}},
		{"missing default", Config{Providers: valid.Providers, Profiles: valid.Profiles}},
		{"unknown default", Config{Providers: valid.Providers, Profiles: valid.Profiles, DefaultProfile: "nope"}},
		{"padded provider", Config{Providers: map[string]ProviderConfig{" ds": {Protocol: ProtocolResponses, BaseURL: DeepSeekResponsesBaseURL}}, Profiles: valid.Profiles, DefaultProfile: "flash"}},
		{"bad protocol", Config{Providers: map[string]ProviderConfig{"deepseek": {Protocol: "grpc", BaseURL: DeepSeekResponsesBaseURL}}, Profiles: valid.Profiles, DefaultProfile: "flash"}},
		{"server compaction non-responses", Config{Providers: map[string]ProviderConfig{"deepseek": {Protocol: ProtocolAnthropicMessages, BaseURL: DeepSeekResponsesBaseURL, ServerCompaction: true}}, Profiles: valid.Profiles, DefaultProfile: "flash"}},
		{"bad base url", Config{Providers: map[string]ProviderConfig{"deepseek": {Protocol: ProtocolResponses, BaseURL: "ftp://bad"}}, Profiles: valid.Profiles, DefaultProfile: "flash"}},
		{"padded base url", Config{Providers: map[string]ProviderConfig{"deepseek": {Protocol: ProtocolResponses, BaseURL: " https://api.deepseek.com"}}, Profiles: valid.Profiles, DefaultProfile: "flash"}},
		{"padded profile", Config{Providers: valid.Providers, Profiles: map[string]ModelProfile{" flash": {Provider: "deepseek", Model: "m"}}, DefaultProfile: " flash"}},
		{"empty model id", Config{Providers: valid.Providers, Profiles: map[string]ModelProfile{"flash": {Provider: "deepseek"}}, DefaultProfile: "flash"}},
		{"unknown provider", Config{Providers: valid.Providers, Profiles: map[string]ModelProfile{"flash": {Provider: "other", Model: "m"}}, DefaultProfile: "flash"}},
		{"negative max output", Config{Providers: valid.Providers, Profiles: map[string]ModelProfile{"flash": {Provider: "deepseek", Model: "m", MaxOutputTokens: -1}}, DefaultProfile: "flash"}},
		{"negative context window", Config{Providers: valid.Providers, Profiles: map[string]ModelProfile{"flash": {Provider: "deepseek", Model: "m", ContextWindow: -1}}, DefaultProfile: "flash"}},
		{"audio non-responses", Config{
			Providers:      map[string]ProviderConfig{"anth": {Protocol: ProtocolAnthropicMessages, BaseURL: "https://api.anthropic.com"}},
			Profiles:       map[string]ModelProfile{"claude": {Provider: "anth", Model: "c", MaxOutputTokens: 100, SupportsInputAudio: true}},
			DefaultProfile: "claude",
		}},
		{"anthropic missing maxOutputTokens", Config{
			Providers:      map[string]ProviderConfig{"anth": {Protocol: ProtocolAnthropicMessages, BaseURL: "https://api.anthropic.com"}},
			Profiles:       map[string]ModelProfile{"claude": {Provider: "anth", Model: "c"}},
			DefaultProfile: "claude",
		}},
	}
	for _, tc := range cases {
		if err := tc.cfg.Validate(); err == nil {
			t.Fatalf("%s: expected validation error", tc.name)
		}
	}
}

func TestRequestError(t *testing.T) {
	cause := errors.New("boom")
	e := &RequestError{Category: ErrorRateLimit, StatusCode: 429, Code: "rate_limit", RetryAfter: time.Second, Cause: cause}
	if e.Error() == "" || !errors.Is(e, cause) || !e.Retryable() {
		t.Fatalf("unexpected RequestError behavior: %#v", e)
	}
	for _, cat := range []ErrorCategory{ErrorUnavailable, ErrorNetwork, ErrorTimeout} {
		if !(&RequestError{Category: cat}).Retryable() {
			t.Fatalf("expected %s to be retryable", cat)
		}
	}
	if (&RequestError{Category: ErrorAuthentication}).Retryable() {
		t.Fatal("authentication error should not be retryable")
	}
}

func TestResponseInputAndToolRoundTrips(t *testing.T) {
	txt := NewResponseInputText("hello")
	raw, err := json.Marshal(txt)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ResponseInput
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Text == nil || *decoded.Text != "hello" {
		t.Fatalf("unexpected decoded text input: %#v, err=%v", decoded, err)
	}

	items := NewResponseInputItems(
		InputMessage{Role: RoleUser, Content: NewMessageText("hi")},
		InputFunctionCall{CallID: "c1", Name: "read_file", Arguments: `{}`},
		InputFunctionCallOutput{CallID: "c1", Output: NewToolOutputText("ok")},
	)
	rawItems, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	var decodedItems ResponseInput
	if err := json.Unmarshal(rawItems, &decodedItems); err != nil || len(decodedItems.Items) != 3 {
		t.Fatalf("unexpected decoded items: %#v, err=%v", decodedItems, err)
	}
	if _, err := (ResponseInput{Text: txt.Text, Items: items.Items}).MarshalJSON(); err == nil {
		t.Fatal("expected error when both text and items are set")
	}
	var empty ResponseInput
	if err := json.Unmarshal([]byte("null"), &empty); err != nil || empty.Text != nil || empty.Items != nil {
		t.Fatalf("expected null input to decode empty: %#v, err=%v", empty, err)
	}
	if err := json.Unmarshal([]byte(""), &empty); err == nil {
		t.Fatal("expected error on empty input bytes")
	}

	for _, tool := range []ResponseTool{
		FunctionTool{Name: "f1", Description: "desc", Parameters: json.RawMessage(`{"type":"object"}`)},
		CustomTool{Name: "c1", Description: "custom"},
		UnknownResponseTool{Raw: json.RawMessage(`{"type":"future","x":1}`)},
	} {
		data, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeResponseTool(data)
		if err != nil || got == nil {
			t.Fatalf("decodeResponseTool(%s): %v", data, err)
		}
	}
	if _, err := decodeResponseTool([]byte("{bad")); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestAllInputAndOutputItemsRoundTrip(t *testing.T) {
	items := NewResponseInputItems(
		InputMessage{Role: RoleDeveloper, Content: NewMessageContentParts(
			InputText{Text: "sys"},
			InputImage{ImageURL: "https://example.com/i.png", Detail: "high"},
			InputAudio{Data: "AQID", Format: "wav"},
			UnknownContentPart{Raw: json.RawMessage(`{"type":"custom_part"}`)},
		)},
		InputCustomToolCall{CallID: "ct-1", Name: "custom_tool", Input: "raw"},
		InputCustomToolCallOutput{CallID: "ct-1", Output: NewToolOutputParts(InputText{Text: "out"})},
		InputReasoning{ID: "rs-1"},
		UnknownInputItem{Raw: json.RawMessage(`{"type":"future_input","k":1}`)},
	)
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ResponseInput
	if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded.Items) != 5 {
		t.Fatalf("decoded items = %#v, err=%v", decoded, err)
	}

	resp := ModelResponse{
		ID:     "resp-1",
		Status: "completed",
		Output: []ResponseOutputItem{
			OutputMessage{ID: "m1", Role: "assistant", Content: []OutputContentPart{
				OutputText{Text: "answer"},
				UnknownOutputContentPart{Raw: json.RawMessage(`{"type":"refusal"}`)},
			}},
			OutputReasoning{ID: "r1", Summary: []ReasoningSummaryText{{Text: "step"}}},
			OutputFunctionCall{ID: "fc1", CallID: "c1", Name: "read_file", Arguments: `{}`},
			OutputCustomToolCall{ID: "ct1", CallID: "c2", Name: "custom", Input: "in"},
			OutputCompaction{ID: "cmp1", EncryptedContent: "enc", CreatedBy: "server"},
			UnknownOutputItem{Raw: json.RawMessage(`{"type":"future_out"}`)},
		},
		Usage: ResponseUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	}
	respBytes, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var decodedResp ModelResponse
	if err := json.Unmarshal(respBytes, &decodedResp); err != nil || len(decodedResp.Output) != 6 {
		t.Fatalf("decodedResp = %#v, err=%v", decodedResp, err)
	}
}
