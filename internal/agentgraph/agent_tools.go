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
	Question string                     `json:"question"`
	Options  []model.UserQuestionOption `json:"options,omitempty"`
}

type teamDispatchRequest struct {
	Tasks            []TeamAssignment `json:"tasks"`
	MinimumSuccesses *int             `json:"minimum_successes,omitempty"`
}

func askUserDefinition() model.FunctionTool {
	parameters, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question": map[string]any{"type": "string", "description": "A concise clarification question for the user."},
			"options": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "object", "properties": map[string]any{"label": map[string]any{"type": "string"}, "description": map[string]any{"type": "string"}}, "required": []string{"label", "description"}, "additionalProperties": false},
				"description": "Use an empty array when no choices are needed. The user may also provide a free-text answer.",
			},
		},
		"required":             []string{"question", "options"},
		"additionalProperties": false,
	})
	strict := true
	return model.FunctionTool{
		Name: askUserToolName,
		Description: "Ask the user a concise question only when missing information blocks useful progress. " +
			"Optionally provide answer choices; the user may choose one or reply with free text.",
		Parameters: parameters,
		Strict:     &strict,
	}
}

func parseUserQuestion(arguments string) (string, []model.UserQuestionOption, error) {
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	var input userQuestionInput
	if err := decoder.Decode(&input); err != nil {
		return "", nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", nil, errors.New("expected one JSON object")
	}
	input.Question = strings.TrimSpace(input.Question)
	if input.Question == "" {
		return "", nil, errors.New("question must not be empty")
	}
	for index := range input.Options {
		input.Options[index].Label = strings.TrimSpace(input.Options[index].Label)
		input.Options[index].Description = strings.TrimSpace(input.Options[index].Description)
		if input.Options[index].Label == "" {
			return "", nil, errors.New("answer options must not be empty")
		}
	}
	return input.Question, input.Options, nil
}

func teamDispatchDefinition(team TeamExecutor) model.FunctionTool {
	profiles := team.Profiles()
	profileIDs := make([]string, 0, len(profiles))
	for profileID := range profiles {
		profileIDs = append(profileIDs, profileID)
	}
	sort.Strings(profileIDs)
	parameters, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tasks": map[string]any{
				"type":     "array",
				"minItems": 1,
				"maxItems": team.MaxParallelAgents(),
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
		"required":             []string{"tasks", "minimum_successes"},
		"additionalProperties": false,
	})
	strict := true
	return model.FunctionTool{
		Name: teamDispatchToolName,
		Description: "Delegate independent work to multiple team members at once. Choose each member's model profile from the listed catalog. " +
			"Members have isolated context, share the workspace and current permissions, and return results for you to synthesize. Use this only when tasks can proceed independently. Set minimum_successes to null to use the host default or zero to wait for every member.",
		Parameters: parameters,
		Strict:     &strict,
	}
}

func parseTeamAssignments(arguments string, team TeamExecutor) ([]TeamAssignment, *int, error) {
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	var request teamDispatchRequest
	if err := decoder.Decode(&request); err != nil {
		return nil, nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, nil, errors.New("expected one JSON object")
	}
	limit := team.MaxParallelAgents()
	if len(request.Tasks) == 0 || len(request.Tasks) > limit {
		return nil, nil, fmt.Errorf("team task count must be between 1 and %d", limit)
	}
	if request.MinimumSuccesses != nil && (*request.MinimumSuccesses < 0 || *request.MinimumSuccesses > len(request.Tasks)) {
		return nil, nil, fmt.Errorf("minimum_successes must be between 0 and %d", len(request.Tasks))
	}
	profiles := team.Profiles()
	seen := make(map[string]struct{}, len(request.Tasks))
	for index := range request.Tasks {
		task := &request.Tasks[index]
		task.ID = strings.TrimSpace(task.ID)
		task.Model = strings.TrimSpace(task.Model)
		task.Task = strings.TrimSpace(task.Task)
		if task.ID == "" || task.Task == "" {
			return nil, nil, errors.New("each team task requires a non-empty id and task")
		}
		if _, exists := seen[task.ID]; exists {
			return nil, nil, fmt.Errorf("team member id %q is repeated", task.ID)
		}
		seen[task.ID] = struct{}{}
		if _, exists := profiles[task.Model]; !exists {
			return nil, nil, fmt.Errorf("unknown team model profile %q", task.Model)
		}
	}
	return request.Tasks, request.MinimumSuccesses, nil
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
		if result.Error != "" {
			output.WriteString("Failed: ")
			output.WriteString(result.Error)
		} else {
			output.WriteString(result.Text)
		}
	}
	return output.String()
}
