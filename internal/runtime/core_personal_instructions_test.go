package runtime

import (
	"encoding/json"
	"testing"

	"github.com/afterlune/luneCore/internal/agentgraph"
)

func TestPersonalInstructionsRestoreFromCheckpointSnapshot(t *testing.T) {
	encoded, err := json.Marshal(agentgraph.State{PersonalInstructions: "original user preferences"})
	if err != nil {
		t.Fatal(err)
	}
	var resumed agentgraph.State
	if err := json.Unmarshal(encoded, &resumed); err != nil {
		t.Fatal(err)
	}
	if options := taskOptionsFromState(resumed); options.PersonalInstructions != "original user preferences" {
		t.Fatalf("restored personal instructions = %q", options.PersonalInstructions)
	}

	var legacy agentgraph.State
	if err := json.Unmarshal([]byte(`{"task_settings_version":7}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if options := taskOptionsFromState(legacy); options.PersonalInstructions != "" {
		t.Fatalf("legacy checkpoint personal instructions = %q, want empty", options.PersonalInstructions)
	}
}
