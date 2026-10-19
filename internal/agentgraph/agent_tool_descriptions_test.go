package agentgraph

import (
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
		},
		"delegate_team": {
			"independent tasks", "distinct deliverables", "listed model profiles", "task uses isolated context",
			"fork also includes parent history", "matching provider/model prefix", "share the workspace and permissions",
			"minimum_successes null uses the host default", "zero waits for all",
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
