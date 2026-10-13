package model

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	publicmodel "github.com/afterlune/luneCore/model"
)

func TestProviderRequestErrorClassification(t *testing.T) {
	for _, protocol := range []string{"responses", "anthropic"} {
		for _, test := range []struct {
			status   int
			code     string
			category publicmodel.ErrorCategory
		}{
			{429, "rate_limit_error", publicmodel.ErrorRateLimit}, {429, "insufficient_quota", publicmodel.ErrorQuota},
			{429, "credit_balance_exhausted", publicmodel.ErrorQuota}, {429, "project_spend_limit_exceeded", publicmodel.ErrorQuota},
			{503, "overloaded_error", publicmodel.ErrorUnavailable}, {500, "api_error", publicmodel.ErrorUnavailable},
			{401, "authentication_error", publicmodel.ErrorAuthentication}, {400, "invalid_request_error", publicmodel.ErrorInvalidRequest},
		} {
			t.Run(protocol+"/"+test.code, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("request-id", "req_test")
					w.Header().Set("Retry-After", "7")
					w.WriteHeader(test.status)
					_, _ = io.WriteString(w, `{"error":{"code":"`+test.code+`","type":"`+test.code+`","message":"sensitive provider payload"}}`)
				}))
				defer server.Close()
				var provider Provider = newResponsesProvider("test", "key", server.URL, server.Client())
				if protocol == "anthropic" {
					provider = newAnthropicMessagesProvider("test", "key", server.URL, server.Client())
				}
				input, _ := NewResponseInput("hello")
				for _, streaming := range []bool{false, true} {
					var err error
					if streaming {
						_, err = provider.StreamResponse(context.Background(), ModelRequest{Input: input}, ModelProfile{Model: "test", MaxOutputTokens: 10}, nil)
					} else {
						_, err = provider.CreateResponse(context.Background(), ModelRequest{Input: input}, ModelProfile{Model: "test", MaxOutputTokens: 10})
					}
					var requestErr *publicmodel.RequestError
					if !errors.As(err, &requestErr) || requestErr.Category != test.category || requestErr.StatusCode != test.status || requestErr.RequestID != "req_test" || requestErr.RetryAfter != 7*time.Second || strings.Contains(err.Error(), "sensitive") {
						t.Fatalf("error=%#v", err)
					}
				}
			})
		}
	}
}

func TestRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for value, want := range map[string]time.Duration{"2": 2 * time.Second, now.Add(10 * time.Second).Format(http.TimeFormat): 10 * time.Second, "-1": 0, "invalid": 0, now.Add(-time.Hour).Format(http.TimeFormat): 0} {
		if got := retryAfter(value, now); got != want {
			t.Fatalf("%q: %v, want %v", value, got, want)
		}
	}
}

func TestTransportRequestErrors(t *testing.T) {
	for _, cause := range []error{io.ErrUnexpectedEOF, &net.OpError{Op: "read", Net: "tcp", Err: errors.New("reset")}, context.DeadlineExceeded} {
		var requestErr *publicmodel.RequestError
		err := transportError(cause)
		if !errors.As(err, &requestErr) || !requestErr.Retryable() || !errors.Is(err, cause) {
			t.Fatalf("error=%v", err)
		}
	}
	if err := transportError(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestIncompleteStreamsReturnFailureAndPreserveUsage(t *testing.T) {
	response, err := readAnthropicStream(context.Background(), strings.NewReader("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg\",\"usage\":{\"input_tokens\":12}}}\n\n"), nil, "test", "test")
	var requestErr *publicmodel.RequestError
	if !errors.As(err, &requestErr) || response == nil || response.Usage.InputTokens != 12 {
		t.Fatalf("response=%v error=%v", response, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\",\"usage\":{\"input_tokens\":15}}}\n\n")
	}))
	defer server.Close()
	response, err = newResponsesProvider("test", "key", server.URL, server.Client()).StreamResponse(context.Background(), ModelRequest{}, ModelProfile{}, nil)
	if !errors.As(err, &requestErr) || response == nil || response.Usage.InputTokens != 15 {
		t.Fatalf("response=%v error=%v", response, err)
	}
}
