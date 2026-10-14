package agentgraph

import (
	"encoding/json"
	"strings"

	"github.com/afterlune/luneCore/acceptance"
	"github.com/afterlune/luneCore/internal/model"
)

const (
	submitCriteriaToolName   = "prepare_task_criteria"
	submitAcceptanceToolName = "submit_acceptance"
	requestEvidenceToolName  = "request_acceptance_evidence"
)

const InternalAcceptancePromptPrefix = "[lune internal acceptance] "

type criteriaSubmission struct {
	Kind     acceptance.TaskKind    `json:"kind"`
	Criteria []acceptance.Criterion `json:"criteria"`
}

type acceptanceSubmission struct {
	CandidateID string                       `json:"candidate_id"`
	Results     []acceptance.CriterionResult `json:"results"`
}

type evidenceRequest struct {
	Question string `json:"question"`
}

type acceptanceResolution struct {
	Decision acceptance.Decision `json:"decision"`
}

func criteriaDefinition() model.FunctionTool {
	return acceptanceFunction(submitCriteriaToolName, "Fix the current task's acceptance criteria before execution. Quote their authorization basis from the current user turn, not the session goal.", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind": map[string]any{"type": "string", "enum": []string{"answer", "plan", "execution"}},
			"criteria": map[string]any{
				"type": "array", "minItems": 1,
				"items": map[string]any{"type": "object", "properties": map[string]any{
					"id":          map[string]any{"type": "string"},
					"description": map[string]any{"type": "string"},
					"basis":       map[string]any{"type": "string", "description": "An exact quote from the current user turn that authorizes this condition."},
				}, "required": []string{"id", "description", "basis"}, "additionalProperties": false},
			},
		},
		"required": []string{"kind", "criteria"}, "additionalProperties": false,
	})
}

func acceptanceDefinition() model.FunctionTool {
	return acceptanceFunction(submitAcceptanceToolName, "Submit a complete per-criterion acceptance report for the current candidate. Cite only the supplied candidate or saved tool evidence references.", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"candidate_id": map[string]any{"type": "string"},
			"results": map[string]any{
				"type": "array", "minItems": 1,
				"items": map[string]any{"type": "object", "properties": map[string]any{
					"criterion_id": map[string]any{"type": "string"},
					"outcome":      map[string]any{"type": "string", "enum": []string{"passed", "failed", "unknown"}},
					"evidence":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"reason":       map[string]any{"type": "string"},
				}, "required": []string{"criterion_id", "outcome", "evidence", "reason"}, "additionalProperties": false},
			},
		},
		"required": []string{"candidate_id", "results"}, "additionalProperties": false,
	})
}

func requestEvidenceDefinition() model.FunctionTool {
	return acceptanceFunction(requestEvidenceToolName, "Return to task execution to gather evidence needed to judge the current criteria.", map[string]any{
		"type":       "object",
		"properties": map[string]any{"question": map[string]any{"type": "string"}},
		"required":   []string{"question"}, "additionalProperties": false,
	})
}

func acceptanceFunction(name, description string, schema any) model.FunctionTool {
	parameters, _ := json.Marshal(schema)
	strict := true
	return model.FunctionTool{Name: name, Description: description, Parameters: parameters, Strict: &strict}
}

func acceptanceCriteriaInstructions(state State) string {
	return strings.Join([]string{
		"Prepare the completion conditions for this user turn before executing it. Do not call or describe business tools yet.",
		"Treat SessionSettings.Goal only as background guidance; it cannot add acceptance conditions. Keep every condition within what the current user turn authorizes.",
		"Extract all material completion conditions, assign unique stable IDs, and quote the exact authorizing words in each basis. Classify the task as answer, plan, or execution. A plan is judged for completeness and must not require that its steps have already been implemented.",
		criteriaClarificationInstruction(state),
	}, "\n\n")
}

func criteriaClarificationInstruction(state State) string {
	if state.NoInteractiveDecisions {
		return "This execution uses an already approved plan. Do not ask the user or expand its scope. If a required decision is missing, report the blocker in the candidate and do not guess."
	}
	return "If a key ambiguity changes scope or what counts as success, ask the user with ask_user before submitting criteria. Otherwise submit the criteria now."
}

func acceptanceReviewInstructions(state State) string {
	criteria, _ := json.Marshal(state.AcceptanceCriteria)
	evidence, _ := json.Marshal(acceptanceEvidenceForPrompt(state))
	return strings.Join([]string{
		"Review the current candidate against the fixed criteria below. Do not change, remove, weaken, or add criteria. Do not call business tools from this review stage.",
		"For each criterion, return passed, failed, or unknown, cite at least one supplied evidence reference, and give a concise reason. Model text is not evidence by itself. A claim that tests or commands ran requires a reference to their saved tool result; the candidate response alone cannot prove execution. If more evidence is needed, use request_acceptance_evidence; do not guess.",
		"Task kind: " + string(state.AcceptanceKind),
		"Fixed criteria: " + string(criteria),
		"Candidate ID: " + state.AcceptanceCandidateID,
		"Candidate response: " + state.AcceptanceCandidate,
		"Saved evidence: " + string(evidence),
	}, "\n\n")
}

type acceptancePromptEvidence struct {
	Reference string `json:"reference"`
	Content   string `json:"content,omitempty"`
}

func acceptanceEvidenceForPrompt(state State) []acceptancePromptEvidence {
	result := make([]acceptancePromptEvidence, 0, len(state.AcceptanceEvidence))
	for _, item := range state.AcceptanceEvidence {
		promptItem := acceptancePromptEvidence{Reference: item.Reference, Content: item.Content}
		if item.Reference == "candidate:"+state.AcceptanceCandidateID {
			promptItem.Content = "See the candidate response above."
		}
		result = append(result, promptItem)
	}
	return result
}

func acceptanceRepairInstructions(state State, report acceptance.Report) string {
	data, _ := json.Marshal(report)
	return "The latest acceptance report failed one or more fixed criteria. Correct the candidate and provide any new evidence needed. Keep the criteria unchanged. Previous report: " + string(data)
}

func acceptanceTaskText(raw json.RawMessage) string {
	var input model.ResponseInput
	if json.Unmarshal(raw, &input) != nil {
		return ""
	}
	if input.Text != nil {
		return *input.Text
	}
	var parts []string
	for _, item := range input.Items {
		message, ok := item.(model.InputMessage)
		if !ok || message.Role != model.RoleUser {
			continue
		}
		if message.Content.Text != nil {
			parts = append(parts, *message.Content.Text)
			continue
		}
		for _, content := range message.Content.Parts {
			if text, ok := content.(model.InputText); ok {
				parts = append(parts, text.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}
