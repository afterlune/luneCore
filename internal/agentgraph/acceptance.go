package agentgraph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/afterlune/luneCore/acceptance"
	"github.com/afterlune/luneCore/internal/model"
	graph "github.com/afterlune/luneGraph"
)

const maxAcceptanceRepairs = 2

func (a *Agent) retryAcceptanceFormat(state State, response *model.Response, target string, instruction string) (graph.Transition[State], error) {
	if state.AcceptanceFormatRetries < 1 {
		state.AcceptanceFormatRetries++
		message := InternalAcceptancePromptPrefix + "The internal acceptance response was missing its required structured tool call. " + instruction
		state.History.Items = append(state.History.Items, model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText(message)})
		state.Transcript.Items = append(state.Transcript.Items, model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText(message)})
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
	message := InternalAcceptancePromptPrefix + "The host chose to continue after the acceptance pause. Make a correction against the saved criteria and previous reports; the host grants up to two new correction attempts. Do not change the criteria."
	state.History.Items = append(state.History.Items, model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText(message)})
	state.Transcript.Items = append(state.Transcript.Items, model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText(message)})
	a.observeAcceptance(state, "resolved", "", "continue")
	return graph.To(state, modelNode), nil
}

func (a *Agent) observeAcceptance(state State, status, source, reason string) {
	if a.options.AcceptanceObserver != nil {
		a.options.AcceptanceObserver(state, status, source, reason)
	}
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
		message := InternalAcceptancePromptPrefix + acceptanceRepairInstructions(state, report)
		state.History.Items = append(state.History.Items, model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText(message)})
		state.Transcript.Items = append(state.Transcript.Items, model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText(message)})
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

func (a *Agent) pauseAcceptanceFormat(state State, reason string) graph.Transition[State] {
	state.AcceptancePauseReason = acceptance.PauseInvalidReport
	state.AcceptancePaused = true
	if reason != "" {
		state.TaskError = reason
	}
	a.observeAcceptance(state, "required", "", string(state.AcceptancePauseReason))
	return graph.Wait(state, AcceptanceContinuation, acceptanceNode)
}
