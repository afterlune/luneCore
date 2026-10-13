package model

import (
	"fmt"
	"time"
)

// ErrorCategory classifies a request failure without retaining provider payloads.
type ErrorCategory string

const (
	ErrorRateLimit      ErrorCategory = "rate_limit"
	ErrorUnavailable    ErrorCategory = "unavailable"
	ErrorNetwork        ErrorCategory = "network"
	ErrorTimeout        ErrorCategory = "timeout"
	ErrorAuthentication ErrorCategory = "authentication"
	ErrorInvalidRequest ErrorCategory = "invalid_request"
	ErrorQuota          ErrorCategory = "quota"
	ErrorProtocol       ErrorCategory = "protocol"
	ErrorUnknown        ErrorCategory = "unknown"
)

// RequestError lets providers describe failures to the runtime. Providers should
// disable their own retries: the runtime owns attempts and their shared deadline.
type RequestError struct {
	Category   ErrorCategory
	StatusCode int
	Code       string
	RequestID  string
	RetryAfter time.Duration
	Cause      error
}

func (e *RequestError) Error() string {
	return fmt.Sprintf("model request failed: %s (status=%d code=%s)", e.Category, e.StatusCode, e.Code)
}

func (e *RequestError) Unwrap() error { return e.Cause }

func (e *RequestError) Retryable() bool {
	switch e.Category {
	case ErrorRateLimit, ErrorUnavailable, ErrorNetwork, ErrorTimeout:
		return true
	default:
		return false
	}
}

// Failure is safe to persist and excludes the underlying error and response body.
type Failure struct {
	Category       ErrorCategory `json:"category"`
	StatusCode     int           `json:"status_code,omitempty"`
	Code           string        `json:"code,omitempty"`
	RequestID      string        `json:"request_id,omitempty"`
	Attempts       int           `json:"attempts"`
	StreamStarted  bool          `json:"stream_started,omitempty"`
	RetryNotBefore time.Time     `json:"retry_not_before,omitempty"`
}
