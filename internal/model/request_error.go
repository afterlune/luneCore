package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	publicmodel "github.com/afterlune/luneCore/model"
)

// RequestFailure also handles errors delivered inside a successful HTTP response.
func RequestFailure(status int, code string) *publicmodel.RequestError {
	category := publicmodel.ErrorUnknown
	switch strings.ToLower(code) {
	case "insufficient_quota", "credit_balance_exhausted", "organization_spend_limit_exceeded", "project_spend_limit_exceeded", "organization_usage_limit_exceeded", "billing_error", "quota_exceeded":
		category = publicmodel.ErrorQuota
	case "authentication_error", "invalid_api_key", "permission_error":
		category = publicmodel.ErrorAuthentication
	case "invalid_request_error", "invalid_request", "context_length_exceeded":
		category = publicmodel.ErrorInvalidRequest
	case "rate_limit_error", "rate_limit_exceeded", "rate_limit", "slow_down":
		category = publicmodel.ErrorRateLimit
	case "overloaded_error", "server_error", "api_error", "service_unavailable":
		category = publicmodel.ErrorUnavailable
	default:
		switch {
		case status == 429:
			category = publicmodel.ErrorRateLimit
		case status == 408 || status == 504:
			category = publicmodel.ErrorTimeout
		case status >= 500:
			category = publicmodel.ErrorUnavailable
		case status == 401 || status == 403:
			category = publicmodel.ErrorAuthentication
		case status >= 400:
			category = publicmodel.ErrorInvalidRequest
		}
	}
	return &publicmodel.RequestError{Category: category, StatusCode: status, Code: code}
}

func providerPayloadError(status int, body []byte) *publicmodel.RequestError {
	var payload struct {
		Code  string `json:"code"`
		Type  string `json:"type"`
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Param   string `json:"param"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &payload)
	code := payload.Error.Code
	if code == "" && payload.Error.Type == "" {
		code = payload.Code
	}
	if code == "" {
		code = payload.Error.Type
	}
	err := RequestFailure(status, code)
	if typed := RequestFailure(0, payload.Error.Type); typed.Category == publicmodel.ErrorQuota || typed.Category == publicmodel.ErrorAuthentication {
		err.Category = typed.Category
	}
	// Preserve capability negotiation without storing arbitrary provider messages.
	if payload.Error.Param == "context_management" || strings.Contains(payload.Error.Message, "context_management") || strings.Contains(payload.Error.Message, "compact_threshold") {
		err.Cause = errors.New("unsupported context_management")
	}
	return err
}

func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 32); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date.Sub(now)
	}
	return 0
}

func annotateRequestError(response *http.Response, err error) error {
	var requestErr *publicmodel.RequestError
	if !errors.As(err, &requestErr) {
		return err
	}
	copy := *requestErr
	if copy.RequestID == "" {
		copy.RequestID = response.Header.Get("x-request-id")
		if copy.RequestID == "" {
			copy.RequestID = response.Header.Get("request-id")
		}
	}
	if copy.RetryAfter == 0 {
		copy.RetryAfter = retryAfter(response.Header.Get("Retry-After"), time.Now())
	}
	return &copy
}

func transportError(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	category := publicmodel.ErrorProtocol
	var network net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		category = publicmodel.ErrorTimeout
	case errors.As(err, &network):
		if network.Timeout() {
			category = publicmodel.ErrorTimeout
		} else {
			// Certificate and malformed URL errors are permanent.
			var op *net.OpError
			if errors.As(err, &op) {
				category = publicmodel.ErrorNetwork
			}
		}
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		category = publicmodel.ErrorNetwork
	}
	return &publicmodel.RequestError{Category: category, Cause: err}
}

func ResponseFailure(responseError *ResponseError) *publicmodel.RequestError {
	failure := RequestFailure(0, responseError.Code)
	if strings.Contains(responseError.Message, "context_management") || strings.Contains(responseError.Message, "compact_threshold") {
		failure.Cause = errors.New("unsupported context_management")
	}
	return failure
}
