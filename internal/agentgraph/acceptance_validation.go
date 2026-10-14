package agentgraph

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/afterlune/luneCore/acceptance"
	"github.com/afterlune/luneCore/internal/model"
)

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
