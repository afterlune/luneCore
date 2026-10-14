package model

import "errors"

// ErrTokenBudgetExceeded marks a model request that could not be safely
// reserved within its execution's remaining token allowance.
var ErrTokenBudgetExceeded = errors.New("execution token budget exhausted")
