package agentgraph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/afterlune/luneCore/acceptance"
	"github.com/afterlune/luneCore/internal/model"
	graph "github.com/afterlune/luneGraph"
)

const (
	submitCriteriaToolName   = "prepare_task_criteria"
	submitAcceptanceToolName = "submit_acceptance"
	requestEvidenceToolName  = "request_acceptance_evidence"
	maxAcceptanceRepairs     = 2
)

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
		"Current user turn text (attachments remain in the conversation input): " + acceptanceTaskPromptText(state.TaskInput),
		"Explicit clarification answers from the user: " + strings.Join(state.AcceptanceClarifications, "\n"),
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
	evidence, _ := json.Marshal(state.AcceptanceEvidence)
	return strings.Join([]string{
		"Review the current candidate against the fixed criteria below. Do not change, remove, weaken, or add criteria. Do not call business tools from this review stage.",
		"For each criterion, return passed, failed, or unknown, cite at least one supplied evidence reference, and give a concise reason. Model text is not evidence by itself. A claim that tests or commands ran requires a reference to their saved tool result; the candidate response alone cannot prove execution. If more evidence is needed, use request_acceptance_evidence; do not guess.",
		"Task kind: " + string(state.AcceptanceKind),
		"Current user turn text (attachments remain in the conversation input): " + acceptanceTaskPromptText(state.TaskInput),
		"Explicit clarification answers from the user: " + strings.Join(state.AcceptanceClarifications, "\n"),
		"Fixed criteria: " + string(criteria),
		"Candidate ID: " + state.AcceptanceCandidateID,
		"Candidate response: " + state.AcceptanceCandidate,
		"Saved evidence: " + string(evidence),
	}, "\n\n")
}

func (a *Agent) retryAcceptanceFormat(state State, response *model.Response, target string, instruction string) (graph.Transition[State], error) {
	if state.AcceptanceFormatRetries < 1 {
		state.AcceptanceFormatRetries++
		state.History.Items = append(state.History.Items, model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("The internal acceptance response was missing its required structured tool call. " + instruction)})
		state.Transcript.Items = append(state.Transcript.Items, model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("The internal acceptance response was missing its required structured tool call. " + instruction)})
		return graph.To(state, target), nil
	}
	state.AcceptancePauseReason = acceptance.PauseInvalidReport
	state.AcceptancePaused = true
	a.observeAcceptance(state, "required", "", string(state.AcceptancePauseReason))
	return graph.Wait(state, AcceptanceContinuation, acceptanceNode), nil
}

func (a *Agent) resolveAcceptanceDecision(state State) (graph.Transition[State], error) {
	decision := state.AcceptanceDecision
	state.AcceptanceDecision = ""
	if decision == acceptance.DecisionConfirm {
		if len(state.AcceptanceCriteria) == 0 || state.AcceptanceCandidateID == "" {
			return graph.Transition[State]{}, errors.New("cannot confirm acceptance before criteria and a candidate exist")
		}
		results := make([]acceptance.CriterionResult, 0, len(state.AcceptanceCriteria))
		for _, criterion := range state.AcceptanceCriteria {
			results = append(results, acceptance.CriterionResult{CriterionID: criterion.ID, Outcome: acceptance.Passed, Evidence: []string{"candidate:" + state.AcceptanceCandidateID}, Reason: "Explicitly confirmed by the host."})
		}
		state.AcceptanceReports = append(state.AcceptanceReports, acceptance.Report{
			CandidateID: state.AcceptanceCandidateID, Attempt: len(state.AcceptanceReports) + 1,
			Source: acceptance.SourceUser, Results: results,
		})
		state.AcceptancePaused, state.AcceptancePauseReason = false, ""
		a.observeAcceptance(state, "resolved", string(acceptance.SourceUser), "confirmed")
		return a.finishAccepted(state), nil
	}
	if decision != acceptance.DecisionContinue {
		return graph.Transition[State]{}, fmt.Errorf("unsupported acceptance decision %q", decision)
	}
	state.AcceptancePaused, state.AcceptancePauseReason = false, ""
	state.AcceptanceFormatRetries = 0
	state.TaskError = ""
	if len(state.AcceptanceCriteria) == 0 {
		state.AcceptancePhase = "criteria"
		state.AcceptanceRepairs = 0
		a.observeAcceptance(state, "resolved", "", "continue")
		return graph.To(state, criteriaNode), nil
	}
	state.AcceptanceRepairs = 0
	state.AcceptancePhase = "execution"
	state.History.Items = append(state.History.Items, model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("The host chose to continue after the acceptance pause. Make a correction against the saved criteria and previous reports; the host grants up to two new correction attempts. Do not change the criteria.")})
	state.Transcript.Items = append(state.Transcript.Items, model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("The host chose to continue after the acceptance pause. Make a correction against the saved criteria and previous reports; the host grants up to two new correction attempts. Do not change the criteria.")})
	a.observeAcceptance(state, "resolved", "", "continue")
	return graph.To(state, modelNode), nil
}

func (a *Agent) observeAcceptance(state State, status, source, reason string) {
	if a.options.AcceptanceObserver != nil {
		a.options.AcceptanceObserver(state, status, source, reason)
	}
}

func parseJSONStrict[T any](data string, value *T) error {
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}

func validateCriteria(state State, submission criteriaSubmission) error {
	if submission.Kind != acceptance.TaskAnswer && submission.Kind != acceptance.TaskPlan && submission.Kind != acceptance.TaskExecution {
		return errors.New("task kind must be answer, plan, or execution")
	}
	if len(submission.Criteria) == 0 || len(submission.Criteria) > 24 {
		return errors.New("provide between 1 and 24 acceptance criteria")
	}
	seen := make(map[string]struct{}, len(submission.Criteria))
	request := acceptanceTaskText(state.TaskInput) + "\n" + strings.Join(state.AcceptanceClarifications, "\n")
	for index := range submission.Criteria {
		criterion := &submission.Criteria[index]
		criterion.ID = strings.TrimSpace(criterion.ID)
		criterion.Description = strings.TrimSpace(criterion.Description)
		criterion.Basis = strings.TrimSpace(criterion.Basis)
		if !validCriterionID(criterion.ID) {
			return errors.New("criterion IDs must be short lowercase identifiers using letters, numbers, underscores, or hyphens")
		}
		if _, exists := seen[criterion.ID]; exists {
			return fmt.Errorf("criterion ID %q is duplicated", criterion.ID)
		}
		seen[criterion.ID] = struct{}{}
		if strings.TrimSpace(criterion.Description) == "" || strings.TrimSpace(criterion.Basis) == "" {
			return fmt.Errorf("criterion %q needs a description and authorization basis", criterion.ID)
		}
		if request != "" && !strings.Contains(strings.ToLower(request), strings.ToLower(strings.TrimSpace(criterion.Basis))) {
			return fmt.Errorf("criterion %q basis must quote text from the current user turn", criterion.ID)
		}
	}
	return nil
}

func validateModelReport(state State, submission acceptanceSubmission) (acceptance.Report, error) {
	if submission.CandidateID != state.AcceptanceCandidateID {
		return acceptance.Report{}, errors.New("report candidate_id does not match the current candidate")
	}
	if len(submission.Results) != len(state.AcceptanceCriteria) {
		return acceptance.Report{}, errors.New("report must include exactly one result for every criterion")
	}
	criteria := make(map[string]bool, len(state.AcceptanceCriteria))
	for _, criterion := range state.AcceptanceCriteria {
		criteria[criterion.ID] = true
	}
	evidence := make(map[string]bool, len(state.AcceptanceEvidence))
	for _, item := range state.AcceptanceEvidence {
		evidence[item.Reference] = true
	}
	seen := make(map[string]bool, len(submission.Results))
	for _, result := range submission.Results {
		if !criteria[result.CriterionID] || seen[result.CriterionID] {
			return acceptance.Report{}, fmt.Errorf("criterion ID %q is unknown or repeated", result.CriterionID)
		}
		seen[result.CriterionID] = true
		if result.Outcome != acceptance.Passed && result.Outcome != acceptance.Failed && result.Outcome != acceptance.Unknown {
			return acceptance.Report{}, fmt.Errorf("criterion %q has an invalid outcome", result.CriterionID)
		}
		if strings.TrimSpace(result.Reason) == "" {
			return acceptance.Report{}, fmt.Errorf("criterion %q needs a reason", result.CriterionID)
		}
		if len(result.Evidence) == 0 {
			return acceptance.Report{}, fmt.Errorf("criterion %q must cite saved evidence", result.CriterionID)
		}
		for _, reference := range result.Evidence {
			if !evidence[reference] {
				return acceptance.Report{}, fmt.Errorf("evidence reference %q does not exist", reference)
			}
		}
	}
	return acceptance.Report{
		CandidateID: state.AcceptanceCandidateID, Attempt: len(state.AcceptanceReports) + 1,
		Source: acceptance.SourceModel, Results: submission.Results,
	}, nil
}

func (a *Agent) acceptModelReport(ctx context.Context, state State, report acceptance.Report) (graph.Transition[State], error) {
	if a.options.AcceptanceVerifier != nil {
		input := acceptance.VerificationInput{
			TaskInput: append(json.RawMessage(nil), state.TaskInput...), Goal: state.Goal,
			Kind: state.AcceptanceKind, Criteria: append([]acceptance.Criterion(nil), state.AcceptanceCriteria...),
			ClarificationAnswers: append([]string(nil), state.AcceptanceClarifications...),
			Candidate:            state.AcceptanceCandidate, Evidence: append([]acceptance.Evidence(nil), state.AcceptanceEvidence...),
		}
		verifyCtx := ctx
		cancel := func() {}
		if timeout := a.options.Execution.ToolTimeout; timeout > 0 {
			verifyCtx, cancel = context.WithTimeout(ctx, timeout*9/10)
		}
		defer cancel()
		hostReport, err := a.options.AcceptanceVerifier.VerifyAcceptance(verifyCtx, input)
		report = normalizeHostReport(state, hostReport, err)
	}
	state.AcceptanceReports = append(state.AcceptanceReports, report)
	state.AcceptanceFormatRetries = 0
	a.observeAcceptance(state, "reported", string(report.Source), "")
	failed, unknown := false, false
	for _, result := range report.Results {
		failed = failed || result.Outcome == acceptance.Failed
		unknown = unknown || result.Outcome == acceptance.Unknown
	}
	if !failed && !unknown {
		state.AcceptancePaused, state.AcceptancePauseReason = false, ""
		return a.finishAccepted(state), nil
	}
	if failed && state.AcceptanceRepairs < maxAcceptanceRepairs {
		state.AcceptanceRepairs++
		state.AcceptancePhase = "execution"
		state.TaskError = ""
		state.History.Items = append(state.History.Items, model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText(acceptanceRepairInstructions(state, report))})
		state.Transcript.Items = append(state.Transcript.Items, model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText(acceptanceRepairInstructions(state, report))})
		return graph.To(state, modelNode), nil
	}
	state.AcceptancePauseReason = acceptance.PauseUnknown
	if failed {
		state.AcceptancePauseReason = acceptance.PauseRepairLimit
	}
	state.AcceptancePaused = true
	a.observeAcceptance(state, "required", string(report.Source), string(state.AcceptancePauseReason))
	return graph.Wait(state, AcceptanceContinuation, acceptanceNode), nil
}

func (a *Agent) failOrPauseAcceptance(ctx context.Context, state State, err error) (graph.Transition[State], error) {
	if state.AcceptancePhase == "acceptance" && !state.TeamBranch && !errors.Is(err, context.Canceled) {
		if a.options.AcceptanceVerifier != nil {
			return a.acceptModelReport(ctx, state, acceptanceUnknownReport(state, err))
		}
		return a.pauseAcceptanceUnknown(state, err), nil
	}
	return a.failTask(state, err)
}

func (a *Agent) pauseAcceptanceUnknown(state State, err error) graph.Transition[State] {
	state.AcceptanceReports = append(state.AcceptanceReports, acceptanceUnknownReport(state, err))
	state.AcceptancePauseReason = acceptance.PauseUnknown
	state.AcceptancePaused = true
	a.observeAcceptance(state, "reported", string(acceptance.SourceModel), "")
	a.observeAcceptance(state, "required", string(acceptance.SourceModel), string(state.AcceptancePauseReason))
	return graph.Wait(state, AcceptanceContinuation, acceptanceNode)
}

func acceptanceUnknownReport(state State, err error) acceptance.Report {
	results := make([]acceptance.CriterionResult, 0, len(state.AcceptanceCriteria))
	for _, criterion := range state.AcceptanceCriteria {
		var evidence []string
		if state.AcceptanceCandidateID != "" {
			evidence = []string{"candidate:" + state.AcceptanceCandidateID}
		}
		results = append(results, acceptance.CriterionResult{
			CriterionID: criterion.ID, Outcome: acceptance.Unknown, Evidence: evidence,
			Reason: "Acceptance model could not evaluate this candidate: " + err.Error(),
		})
	}
	report := acceptance.Report{
		CandidateID: state.AcceptanceCandidateID, Attempt: len(state.AcceptanceReports) + 1,
		Source: acceptance.SourceModel, Results: results,
	}
	return report
}

func normalizeHostReport(state State, report acceptance.Report, verifyErr error) acceptance.Report {
	results := make(map[string]acceptance.CriterionResult, len(report.Results))
	duplicates := make(map[string]bool)
	validIDs := make(map[string]bool, len(state.AcceptanceCriteria))
	for _, criterion := range state.AcceptanceCriteria {
		validIDs[criterion.ID] = true
	}
	for _, result := range report.Results {
		if !validIDs[result.CriterionID] || results[result.CriterionID].CriterionID != "" {
			duplicates[result.CriterionID] = true
			continue
		}
		results[result.CriterionID] = result
	}
	evidence := make(map[string]bool, len(state.AcceptanceEvidence))
	for _, item := range state.AcceptanceEvidence {
		evidence[item.Reference] = true
	}
	final := make([]acceptance.CriterionResult, 0, len(state.AcceptanceCriteria))
	for _, criterion := range state.AcceptanceCriteria {
		result, ok := results[criterion.ID]
		if verifyErr != nil {
			result = acceptance.CriterionResult{CriterionID: criterion.ID, Outcome: acceptance.Unknown, Reason: "Host verifier could not confirm the result: " + verifyErr.Error()}
		} else if !ok || duplicates[criterion.ID] ||
			(result.Outcome != acceptance.Passed && result.Outcome != acceptance.Failed && result.Outcome != acceptance.Unknown) ||
			strings.TrimSpace(result.Reason) == "" || len(result.Evidence) == 0 {
			result = acceptance.CriterionResult{CriterionID: criterion.ID, Outcome: acceptance.Unknown, Reason: "Host verifier omitted or returned an invalid result for this criterion."}
		} else {
			for _, reference := range result.Evidence {
				if !evidence[reference] {
					result = acceptance.CriterionResult{CriterionID: criterion.ID, Outcome: acceptance.Unknown, Reason: "Host verifier cited evidence that was not saved by the runtime."}
					break
				}
			}
		}
		result.CriterionID = criterion.ID
		final = append(final, result)
	}
	return acceptance.Report{CandidateID: state.AcceptanceCandidateID, Attempt: len(state.AcceptanceReports) + 1, Source: acceptance.SourceHost, Results: final}
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

func acceptanceTaskPromptText(raw json.RawMessage) string {
	if text := acceptanceTaskText(raw); text != "" {
		return text
	}
	return "(no text; the current turn includes non-text input or attachments)"
}

func validCriterionID(value string) bool {
	if value == "" || len(value) > 80 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range value[1:] {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func modelEvidence(reference, content string) acceptance.Evidence {
	return acceptance.Evidence{Reference: reference, Content: content}
}

func saveToolEvidence(state State, callID string, output model.InputFunctionCallOutput) State {
	if callID == "" {
		return state
	}
	content, err := json.Marshal(output.Output)
	if err == nil {
		reference := "tool:" + callID
		for index := range state.AcceptanceEvidence {
			if state.AcceptanceEvidence[index].Reference == reference {
				state.AcceptanceEvidence[index].Content = string(content)
				return state
			}
		}
		state.AcceptanceEvidence = append(state.AcceptanceEvidence, modelEvidence(reference, string(content)))
	}
	return state
}

func (a *Agent) pauseAcceptanceFormat(state State, reason string) graph.Transition[State] {
	state.AcceptancePauseReason = acceptance.PauseInvalidReport
	state.AcceptancePaused = true
	if reason != "" {
		state.TaskError = reason
	}
	a.observeAcceptance(state, "required", "", string(state.AcceptancePauseReason))
	return graph.Wait(state, AcceptanceContinuation, acceptanceNode)
}
