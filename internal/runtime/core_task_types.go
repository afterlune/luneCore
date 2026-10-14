package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/afterlune/luneCore/internal/agentruntime"
	publictool "github.com/afterlune/luneCore/tool"
)

type TaskStatus string

// BudgetReason identifies the execution limit that paused a task.
type BudgetReason = agentruntime.BudgetReason

const (
	BudgetMaxSteps       = agentruntime.BudgetMaxSteps
	BudgetMaxInvocations = agentruntime.BudgetMaxInvocations
	BudgetMaxTokens      = agentruntime.BudgetMaxTokens
)

const (
	TaskIdle      TaskStatus = "idle"
	TaskQueued    TaskStatus = "queued"
	TaskRunning   TaskStatus = "running"
	TaskPaused    TaskStatus = "paused"
	TaskCompleted TaskStatus = "completed"
	TaskFailed    TaskStatus = "failed"
	TaskCancelled TaskStatus = "cancelled"
	TaskDiscarded TaskStatus = "discarded"
)

type TaskInput struct{ Raw json.RawMessage }

func NewTaskInput[T any](value T) (TaskInput, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return TaskInput{}, fmt.Errorf("encode task input: %w", err)
	}
	return TaskInput{Raw: raw}, nil
}

func ParseTaskInput(line string) (TaskInput, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return TaskInput{}, errors.New("input is empty")
	}
	if json.Valid([]byte(line)) {
		var value any
		if err := json.Unmarshal([]byte(line), &value); err == nil {
			switch value.(type) {
			case string, []any:
				return TaskInput{Raw: json.RawMessage(line)}, nil
			}
		}
	}
	return NewTaskInput(line)
}

type TaskRequest struct {
	SessionID string
	Input     TaskInput
	// Model optionally overrides the selected profile's API model identifier for this task.
	Model                  string
	MaxToolRounds          int
	Execution              ExecutionOptions
	PlanAuthorization      publictool.PlanAuthorization
	NoInteractiveDecisions bool
}

type TaskState struct {
	Status    TaskStatus
	SessionID string
	TaskID    string
	// ExecutionID identifies the logical run across pause and resume attempts.
	ExecutionID          string
	QueueID              string
	ContextWindow        int64
	ReserveOutputTokens  int64
	AutoCompact          bool
	AutoCompactThreshold float64
	Goal                 string
	MaxToolRounds        int
	ModelOverride        string
	ModelProfile         string
	Provider             string
	Model                string
	ReasoningEffort      string
	Execution            ExecutionOptions
}

// ExecutionOptions configures limits and timeouts for one Agent run. Config
// values become host defaults; task request values override only non-zero fields.
// Recovery options replace the saved set, so zero restores runtime defaults.
type ExecutionOptions struct {
	// ModelMaxAttempts includes the first request; zero defaults to three, one disables retry.
	ModelMaxAttempts int           `json:"model_max_attempts,omitempty"`
	MaxSteps         int           `json:"max_steps,omitempty"`
	MaxInvocations   int           `json:"max_invocations,omitempty"`
	MaxConcurrency   int           `json:"max_concurrency,omitempty"`
	ModelTimeout     time.Duration `json:"model_timeout,omitempty"`
	ToolTimeout      time.Duration `json:"tool_timeout,omitempty"`
	JoinTimeout      time.Duration `json:"join_timeout,omitempty"`
	TeamEntryTimeout time.Duration `json:"team_entry_timeout,omitempty"`
	TeamQuorum       int           `json:"team_quorum,omitempty"`
	// TokenBudget caps total input and output tokens across the whole execution.
	// Zero means unlimited when effective; task overrides inherit the host
	// default at zero. Recovery may raise or replace this absolute total cap.
	TokenBudget int64 `json:"token_budget,omitempty"`
}
