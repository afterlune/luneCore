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
			builder.WriteString("Workspace AGENTS.md files provide project guidance for their directory and descendants. Follow only guidance relevant to this task and target path; when applicable project guidance conflicts, the more specific path takes precedence. Current user instructions, system requirements, safety rules, and tool permissions take precedence. Project guidance does not add acceptance criteria.")
		}
		builder.WriteString("\n\nProject instructions from ")
		builder.WriteString(fmt.Sprintf("%q", instruction.Path))
		builder.WriteString(":\n")
		builder.WriteString(content)
	}
	return builder.String()
}
