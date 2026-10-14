package agentruntime

import (
	"errors"
	"fmt"
)

var (
	ErrTaskPaused                = errors.New("session task is paused; resolve its pending request, recover, or discard it")
	ErrToolInvocationPersistence = errors.New("tool invocation could not be durably recorded")
)

type BudgetReason string

const (
	BudgetMaxSteps       BudgetReason = "max_steps"
	BudgetMaxInvocations BudgetReason = "max_invocations"
	BudgetMaxTokens      BudgetReason = "max_tokens"
)

type TaskPausedError struct {
	BudgetReason BudgetReason
}

func (e *TaskPausedError) Error() string { return ErrTaskPaused.Error() }

func (e *TaskPausedError) Unwrap() error { return ErrTaskPaused }

type ToolRecoveryRequiredError struct{ InvocationID string }

func (e *ToolRecoveryRequiredError) Error() string {
	return fmt.Sprintf("tool invocation %q has an unknown outcome; resolve it as retry or skip before resuming", e.InvocationID)
}
