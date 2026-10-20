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
		"delegate_team": teamDispatchDefinition(team, 4).Description,
	}
	want := map[string][]string{
		"ask_user": {
			"user-owned decision", "blocks safe, correct progress", "plan mode", "inspect available facts",
			"defaults for routine details", "unique IDs", "answer choices", "free text",
			"each description to one brief, distinguishing phrase", "avoid redundant explanations",
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
	if !strings.Contains(strings.ToLower(description), "brief phrase") || !strings.Contains(strings.ToLower(description), "repeating the label") {
		t.Fatalf("ask_user option description schema should request concise, non-redundant text, got %q", description)
	}
}
