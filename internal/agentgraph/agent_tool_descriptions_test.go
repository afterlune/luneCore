package agentgraph

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRuntimeToolDescriptionsRetainDecisionAndDispatchConstraints(t *testing.T) {
	team := benchmarkTeamExecutor{}
	definitions := map[string]string{
		"ask_user":      askUserDefinition().Description,
		"delegate_team": teamDispatchDefinition(team).Description,
	}
	want := map[string][]string{
		"ask_user": {
			"clarification", "decisions",
		},
		"delegate_team": {
			"independent tasks", "distinct deliverables", "task gives each member only its assignment",
			"same provider/model", "may reuse cache", "other profiles receive only their assignment", "inspect the workspace", "share workspace and permissions",
			"minimum_successes null uses host default", "zero waits for all",
		},
	}
	for name, terms := range want {
		description := strings.ToLower(definitions[name])
		for _, term := range terms {
			if !strings.Contains(description, strings.ToLower(term)) {
				t.Errorf("%s description %q is missing %q", name, description, term)
			}
		}
	}
}

func TestAskUserOptionDescriptionSchemaRequestsBriefText(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(askUserDefinition().Parameters, &schema); err != nil {
		t.Fatalf("decode ask_user schema: %v", err)
	}
	properties := schema["properties"].(map[string]any)
	questions := properties["questions"].(map[string]any)
	questionItem := questions["items"].(map[string]any)
	questionProperties := questionItem["properties"].(map[string]any)
	options := questionProperties["options"].(map[string]any)
	optionItem := options["items"].(map[string]any)
	optionProperties := optionItem["properties"].(map[string]any)
	description := optionProperties["description"].(map[string]any)["description"].(string)
	if !strings.Contains(strings.ToLower(description), "brief phrase") {
		t.Fatalf("ask_user option description schema should request concise text, got %q", description)
	}
	recommended, ok := optionProperties["recommended"].(map[string]any)
	if !ok || recommended["type"] != "boolean" {
		t.Fatalf("ask_user option schema should include a boolean recommendation flag: %#v", optionProperties["recommended"])
	}
	required, ok := optionItem["required"].([]any)
	if !ok || !containsAny(required, "label") || !containsAny(required, "description") {
		t.Fatalf("ask_user option schema should require label and description: %#v", optionItem["required"])
	}
	if containsAny(required, "recommended") {
		t.Fatalf("ask_user option schema should not require recommended: %#v", optionItem["required"])
	}
}

func containsAny(values []any, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
