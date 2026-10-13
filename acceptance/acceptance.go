// Package acceptance defines the small public contract used to review task
// results. It is independent of the agent and graph runtimes.
package acceptance

import (
	"context"
	"encoding/json"
)

type TaskKind string

const (
	TaskAnswer    TaskKind = "answer"
	TaskPlan      TaskKind = "plan"
	TaskExecution TaskKind = "execution"
)

type Criterion struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	// Basis quotes the current user turn or an explicit clarification answer.
	Basis string `json:"basis"`
}

type Outcome string

const (
	Passed  Outcome = "passed"
	Failed  Outcome = "failed"
	Unknown Outcome = "unknown"
)

type Source string

const (
	SourceModel Source = "model"
	SourceHost  Source = "host"
	SourceUser  Source = "user"
)

type CriterionResult struct {
	CriterionID string   `json:"criterion_id"`
	Outcome     Outcome  `json:"outcome"`
	Evidence    []string `json:"evidence,omitempty"`
	Reason      string   `json:"reason,omitempty"`
}

// Report records one candidate's per-criterion acceptance judgment.
type Report struct {
	CandidateID string            `json:"candidate_id"`
	Attempt     int               `json:"attempt"`
	Source      Source            `json:"source"`
	Results     []CriterionResult `json:"results"`
}

type Evidence struct {
	Reference string `json:"reference"`
	Content   string `json:"content"`
}

type VerificationInput struct {
	TaskInput            json.RawMessage `json:"task_input,omitempty"`
	Goal                 string          `json:"goal,omitempty"`
	Kind                 TaskKind        `json:"kind"`
	ClarificationAnswers []string        `json:"clarification_answers,omitempty"`
	Criteria             []Criterion     `json:"criteria"`
	Candidate            string          `json:"candidate"`
	Evidence             []Evidence      `json:"evidence,omitempty"`
}

// Verifier is called after the model submits its report. It may be called
// concurrently for separate sessions. When configured, its report is
// authoritative; missing criterion results are treated as unknown.
type Verifier interface {
	VerifyAcceptance(context.Context, VerificationInput) (Report, error)
}

type PauseReason string

const (
	PauseUnknown       PauseReason = "unknown"
	PauseRepairLimit   PauseReason = "repair_limit"
	PauseInvalidReport PauseReason = "invalid_report"
)

type Decision string

const (
	DecisionContinue Decision = "continue"
	DecisionConfirm  Decision = "confirm"
)
