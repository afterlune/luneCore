package core

import "github.com/afterlune/luneCore/acceptance"

type AcceptanceTaskKind = acceptance.TaskKind
type AcceptanceCriterion = acceptance.Criterion
type AcceptanceOutcome = acceptance.Outcome
type AcceptanceSource = acceptance.Source
type AcceptanceCriterionResult = acceptance.CriterionResult
type AcceptanceReport = acceptance.Report
type AcceptanceEvidence = acceptance.Evidence
type AcceptanceVerificationInput = acceptance.VerificationInput
type AcceptanceVerifier = acceptance.Verifier
type AcceptancePauseReason = acceptance.PauseReason
type AcceptanceDecision = acceptance.Decision

const (
	AcceptanceTaskAnswer    = acceptance.TaskAnswer
	AcceptanceTaskPlan      = acceptance.TaskPlan
	AcceptanceTaskExecution = acceptance.TaskExecution

	AcceptancePassed  = acceptance.Passed
	AcceptanceFailed  = acceptance.Failed
	AcceptanceUnknown = acceptance.Unknown

	AcceptanceSourceModel = acceptance.SourceModel
	AcceptanceSourceHost  = acceptance.SourceHost
	AcceptanceSourceUser  = acceptance.SourceUser

	AcceptancePauseUnknown       = acceptance.PauseUnknown
	AcceptancePauseRepairLimit   = acceptance.PauseRepairLimit
	AcceptancePauseInvalidReport = acceptance.PauseInvalidReport

	AcceptanceContinue = acceptance.DecisionContinue
	AcceptanceConfirm  = acceptance.DecisionConfirm
)

// PendingAcceptance identifies a durable acceptance wait. Detailed criteria,
// candidate and reports are returned here rather than copied into durable events.
type PendingAcceptance struct {
	RunID                string
	InvocationID         string
	CallID               string
	ExecutionID          string
	CandidateID          string
	Candidate            string
	Kind                 AcceptanceTaskKind
	Criteria             []AcceptanceCriterion
	ClarificationAnswers []string
	Evidence             []AcceptanceEvidence
	Reports              []AcceptanceReport
	Reason               AcceptancePauseReason
	Repairs              int
}

type AcceptanceResolution struct {
	RunID        string
	InvocationID string
	CallID       string
	Decision     AcceptanceDecision
}
