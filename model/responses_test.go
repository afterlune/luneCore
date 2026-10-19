package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInputReasoningEncryptedContentRoundTrip(t *testing.T) {
	const payload = `[{"type":"reasoning","id":"rs_1","encrypted_content":"opaque-state","content":[{"type":"reasoning_text","text":"thinking"}]}]`
	var input ResponseInput
	if err := json.Unmarshal([]byte(payload), &input); err != nil {
		t.Fatalf("unmarshal reasoning input: %v", err)
	}
	if len(input.Items) != 1 {
		t.Fatalf("input items = %#v, want one reasoning item", input.Items)
	}
	reasoning, ok := input.Items[0].(InputReasoning)
	if !ok || reasoning.EncryptedContent != "opaque-state" {
		t.Fatalf("reasoning item = %#v, want encrypted content preserved", input.Items[0])
	}

	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal reasoning input: %v", err)
	}
	if !strings.Contains(string(encoded), `"encrypted_content":"opaque-state"`) {
		t.Fatalf("encrypted reasoning content missing from JSON: %s", encoded)
	}
}

func TestModelProfileJSONAndEffectiveModel(t *testing.T) {
	// 1. JSON with only modelId and displayName
	const idOnly = `{"provider":"anthropic","modelId":"claude-3-7-sonnet","displayName":"Claude 3.7 Sonnet","contextWindow":200000,"maxOutputTokens":8192}`
	var p1 ModelProfile
	if err := json.Unmarshal([]byte(idOnly), &p1); err != nil {
		t.Fatalf("unmarshal idOnly: %v", err)
	}
	if p1.Model != "claude-3-7-sonnet" || p1.ModelID != "claude-3-7-sonnet" {
		t.Errorf("expected Model and ModelID to be 'claude-3-7-sonnet', got Model=%q, ModelID=%q", p1.Model, p1.ModelID)
	}
	if p1.EffectiveModel() != "claude-3-7-sonnet" {
		t.Errorf("EffectiveModel() = %q, want 'claude-3-7-sonnet'", p1.EffectiveModel())
	}
	if p1.EffectiveDisplayName() != "Claude 3.7 Sonnet" {
		t.Errorf("EffectiveDisplayName() = %q, want 'Claude 3.7 Sonnet'", p1.EffectiveDisplayName())
	}
	if p1.ContextWindow != 200000 {
		t.Errorf("ContextWindow = %d, want 200000", p1.ContextWindow)
	}

	// 2. JSON with legacy model only
	const modelOnly = `{"provider":"deepseek","model":"deepseek-chat"}`
	var p2 ModelProfile
	if err := json.Unmarshal([]byte(modelOnly), &p2); err != nil {
		t.Fatalf("unmarshal modelOnly: %v", err)
	}
	if p2.Model != "deepseek-chat" || p2.ModelID != "deepseek-chat" {
		t.Errorf("expected Model and ModelID to be 'deepseek-chat', got Model=%q, ModelID=%q", p2.Model, p2.ModelID)
	}
	if p2.EffectiveDisplayName() != "deepseek-chat" {
		t.Errorf("EffectiveDisplayName() = %q, want 'deepseek-chat'", p2.EffectiveDisplayName())
	}

	// 3. Config validation with modelId only
	cfg := Config{
		Providers: map[string]ProviderConfig{
			"anthropic": {Protocol: ProtocolAnthropicMessages, BaseURL: "https://api.anthropic.com", APIKey: "key"},
		},
		Models: map[string]ModelProfile{
			"main": p1,
		},
		DefaultModel: "main",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validation failed with modelId profile: %v", err)
	}
	cfg.Models["main"] = ModelProfile{Provider: "anthropic", Model: "claude", ContextWindow: -1}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "contextWindow must not be negative") {
		t.Fatalf("negative model profile contextWindow error = %v", err)
	}
}
