package agentgraph

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/afterlune/luneCore/internal/model"
)

const teamDispatchToolName = "delegate_team"

const askUserToolName = model.UserQuestionToolName

type userQuestionInput struct {
	Questions []model.UserQuestionItem `json:"questions"`
}

type teamDispatchRequest struct {
	Mode             json.RawMessage  `json:"mode,omitempty"`
	Tasks            []TeamAssignment `json:"tasks"`
	MinimumSuccesses *int             `json:"minimum_successes,omitempty"`
}

func askUserDefinition() model.FunctionTool {
	parameters, _ := json.Marshal(map[string]any{
		"type": "object", "properties": map[string]any{
			"questions": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{
				"type": "object", "properties": map[string]any{
					"id":       map[string]any{"type": "string", "description": "Unique stable ID within this batch."},
					"question": map[string]any{"type": "string", "description": "One concise user-owned decision."},
					"options": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
						"label":       map[string]any{"type": "string"},
						"description": map[string]any{"type": "string", "description": "A brief phrase that distinguishes this option; avoid full sentences and repeating the label."},
					}, "required": []string{"label", "description"}, "additionalProperties": false}},
				}, "required": []string{"id", "question", "options"}, "additionalProperties": false}},
		}, "required": []string{"questions"}, "additionalProperties": false,
	})
	strict := true
	return model.FunctionTool{
		Name: askUserToolName,
		Description: "Ask only about a user-owned decision or material ambiguity that blocks safe, correct progress. " +
			"In Plan mode, ask before presenting the plan; inspect available facts first and use defaults for routine details. Group related decisions, use concise questions with unique IDs, and offer answer choices when helpful. Keep each option label short and each description to one brief, distinguishing phrase; avoid redundant explanations. Free text is accepted.",
		Parameters: parameters,
		Strict:     &strict,
	}
}

func parseUserQuestion(arguments string) ([]model.UserQuestionItem, error) {
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	var input userQuestionInput
	if err := decoder.Decode(&input); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("expected one JSON object")
	}
	if len(input.Questions) == 0 {
		return nil, errors.New("questions must not be empty")
	}
	seen := map[string]bool{}
	for i := range input.Questions {
		q := &input.Questions[i]
		q.ID = strings.TrimSpace(q.ID)
		q.Question = strings.TrimSpace(q.Question)
		if q.ID == "" || q.Question == "" || seen[q.ID] {
			return nil, errors.New("questions require unique IDs and nonempty text")
		}
		seen[q.ID] = true
		for j := range q.Options {
			o := &q.Options[j]
			o.Label = strings.TrimSpace(o.Label)
			o.Description = strings.TrimSpace(o.Description)
			if o.Label == "" {
				return nil, errors.New("answer options must not be empty")
			}
		}
	}
	return input.Questions, nil
}

func teamDispatchDefinition(team TeamExecutor, maxChildren int) model.FunctionTool {
	profiles := team.Profiles()
	profileIDs := make([]string, 0, len(profiles))
	for profileID := range profiles {
		profileIDs = append(profileIDs, profileID)
	}
	sort.Strings(profileIDs)
	parameters, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{
				"type": "string", "enum": []string{TeamDispatchTask, TeamDispatchFork},
				"description": "task is isolated. fork inherits context and may reuse cache only for the same provider/model; other profiles receive only their assignment.",
			},
			"tasks": map[string]any{
				"type":     "array",
				"minItems": 1,
				"maxItems": maxChildren,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":    map[string]any{"type": "string", "description": "Short unique name for this team member."},
						"model": map[string]any{"type": "string", "enum": profileIDs},
						"task":  map[string]any{"type": "string", "description": "A bounded, independent task for this member."},
					},
					"required":             []string{"id", "model", "task"},
					"additionalProperties": false,
				},
			},
			"minimum_successes": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "integer", "minimum": 0},
					map[string]any{"type": "null"},
				},
				"description": "Minimum successful members before results can merge. Use null to inherit the host default; zero waits for every member.",
			},
		},
		"required":             []string{"mode", "tasks", "minimum_successes"},
		"additionalProperties": false,
	})
	strict := true
	return model.FunctionTool{
		Name:        teamDispatchToolName,
		Description: "Delegate independent tasks in parallel with distinct deliverables. task gives each member only its assignment. fork inherits parent context and may reuse cache only for the same provider/model; other profiles receive only their assignment and should inspect the workspace. Include needed findings. Members share workspace and permissions. Synthesize results. minimum_successes null uses host default; zero waits for all.",
		Parameters:  parameters,
		Strict:      &strict,
	}
}

func parseTeamAssignments(arguments string, team TeamExecutor, maxChildren int) ([]TeamAssignment, string, *int, error) {
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	var request teamDispatchRequest
	if err := decoder.Decode(&request); err != nil {
		return nil, "", nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, "", nil, errors.New("expected one JSON object")
	}
	mode := TeamDispatchTask
	if len(request.Mode) > 0 {
		var requestedMode string
		if err := json.Unmarshal(request.Mode, &requestedMode); err != nil {
			return nil, "", nil, fmt.Errorf("mode must be a string: %w", err)
		}
		mode = requestedMode
	}
	if mode != TeamDispatchTask && mode != TeamDispatchFork {
		return nil, "", nil, fmt.Errorf("mode must be %q or %q", TeamDispatchTask, TeamDispatchFork)
	}
	if maxChildren <= 0 || len(request.Tasks) == 0 || len(request.Tasks) > maxChildren {
		return nil, "", nil, fmt.Errorf("team task count must be between 1 and %d", maxChildren)
	}
	if request.MinimumSuccesses != nil && (*request.MinimumSuccesses < 0 || *request.MinimumSuccesses > len(request.Tasks)) {
		return nil, "", nil, fmt.Errorf("minimum_successes must be between 0 and %d", len(request.Tasks))
	}
	profiles := team.Profiles()
	seen := make(map[string]struct{}, len(request.Tasks))
	for index := range request.Tasks {
		task := &request.Tasks[index]
		task.ID = strings.TrimSpace(task.ID)
		task.Model = strings.TrimSpace(task.Model)
		task.Task = strings.TrimSpace(task.Task)
		if task.ID == "" || task.Task == "" {
			return nil, "", nil, errors.New("each team task requires a non-empty id and task")
		}
		if _, exists := seen[task.ID]; exists {
			return nil, "", nil, fmt.Errorf("team member id %q is repeated", task.ID)
		}
		seen[task.ID] = struct{}{}
		if _, exists := profiles[task.Model]; !exists {
			return nil, "", nil, fmt.Errorf("unknown team model profile %q", task.Model)
		}
	}
	return request.Tasks, mode, request.MinimumSuccesses, nil
}

func formatTeamResults(results []TeamMemberResult) string {
	var output strings.Builder
	output.WriteString("Team results:")
	for _, result := range results {
		output.WriteString("\n\n## ")
		output.WriteString(result.ID)
		output.WriteString(" (model ")
		output.WriteString(result.Model)
		output.WriteString(")\n")
		if result.ContextMode == TeamDispatchFork {
			output.WriteString("Context: forked\n")
		} else if result.ContextMode == TeamDispatchTask {
			output.WriteString("Context: task-only\n")
		}
		if result.Error != "" {
			output.WriteString("Failed: ")
			output.WriteString(result.Error)
		} else {
			output.WriteString(result.Text)
		}
	}
	return output.String()
}
