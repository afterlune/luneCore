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
