package agentgraph

import (
	"fmt"
	"strings"
)

// ProjectInstruction is one workspace-relative AGENTS.md snapshot. Empty
// content records that the path was checked and no file was present.
type ProjectInstruction struct {
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
}

func cloneProjectInstructions(instructions []ProjectInstruction) []ProjectInstruction {
	return append([]ProjectInstruction(nil), instructions...)
}

func personalInstructionContext(instructions string) string {
	instructions = strings.TrimSpace(instructions)
	if instructions == "" {
		return ""
	}
	return "Host-supplied personal instructions are user-level preferences. Follow when relevant; they do not override the current request, system or safety rules, permissions, or workspace AGENTS.md and add no acceptance criteria.\n\nPersonal instructions:\n" + instructions
}

func mergeProjectInstructions(existing, additions []ProjectInstruction) ([]ProjectInstruction, bool) {
	merged := cloneProjectInstructions(existing)
	known := make(map[string]struct{}, len(existing))
	for _, instruction := range existing {
		known[instruction.Path] = struct{}{}
	}
	changed := false
	for _, instruction := range additions {
		if instruction.Path == "" {
			continue
		}
		if _, found := known[instruction.Path]; found {
			continue
		}
		known[instruction.Path] = struct{}{}
		merged = append(merged, instruction)
		changed = changed || strings.TrimSpace(instruction.Content) != ""
	}
	return merged, changed
}

func projectInstructionContext(instructions []ProjectInstruction) string {
	var builder strings.Builder
	for _, instruction := range instructions {
		content := strings.TrimSpace(instruction.Content)
		if content == "" {
			continue
		}
		if builder.Len() == 0 {
			builder.WriteString("Follow only workspace AGENTS.md guidance relevant to the task and target path. User, system, safety, and permission rules take precedence; more specific project files override less specific ones. Project guidance adds no acceptance criteria.")
		}
		builder.WriteString("\n\nProject instructions from ")
		builder.WriteString(fmt.Sprintf("%q", instruction.Path))
		builder.WriteString(":\n")
		builder.WriteString(content)
	}
	return builder.String()
}
