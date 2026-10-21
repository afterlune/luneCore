package agentgraph

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/afterlune/luneCore/internal/model"
	publicmodel "github.com/afterlune/luneCore/model"
	graph "github.com/afterlune/luneGraph"
)

type retryTiming struct {
	now    func() time.Time
	wait   func(context.Context, time.Duration) error
	jitter func() time.Duration
}

func defaultRetryTiming() *retryTiming {
	return &retryTiming{now: time.Now, jitter: func() time.Duration { return time.Duration(rand.Int64N(int64(250*time.Millisecond) + 1)) }, wait: func(ctx context.Context, delay time.Duration) error {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}}
}

type attemptFailure struct {
	failure publicmodel.Failure
	cause   error
}

func (e *attemptFailure) Error() string { return e.cause.Error() }
func (e *attemptFailure) Unwrap() error { return e.cause }

// Every attempt passes through the usage-tracking requester. The request and
// backoff share one deadline; an unsuccessful response never enters history.
func (a *Agent) sendModel(ctx context.Context, state State, requester Requester, request model.ModelRequest, emit func(model.ModelStreamEvent) error) (*model.ModelResponse, error) {
	timing := a.options.retry
	if timing == nil {
		timing = defaultRetryTiming()
	}
	maxAttempts := state.Execution.ModelMaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 3
	}
	trace := model.RequestTraceFromContext(ctx)
	if trace.ID == "" {
		trace = traceRequest(state, request, state.ExecutionID+":aux:"+cryptorand.Text(), a.permissionMode(state))
	}
	streamStarted := false
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		trace.Attempt = attempt
		attemptCtx := model.WithRequestTrace(ctx, trace)
		a.options.Logger.InfoContext(ctx, "model request attempt", "session_run_id", state.RunID, "execution_id", state.ExecutionID, "agent_id", state.AgentID, "request_id", trace.ID, "attempt", attempt, "instructions_hash", trace.Instructions, "tools_hash", trace.Tools, "history_hash", trace.History, "local_changes", trace.Changes)
		var response *model.ModelResponse
		var err error
		if streaming, ok := requester.(StreamingRequester); ok && emit != nil {
			response, err = streaming.StreamResponse(attemptCtx, request, func(event model.ModelStreamEvent) error { streamStarted = true; return emit(event) })
		} else {
			response, err = requester.CreateResponse(attemptCtx, request)
		}
		if err == nil && response != nil && response.Status == "failed" {
			if response.Error != nil {
				err = model.ResponseFailure(response.Error)
			} else {
				err = &publicmodel.RequestError{Category: publicmodel.ErrorUnknown}
			}
		}
		if err == nil {
			return response, nil
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, ctx.Err()
		}
		var requestErr *publicmodel.RequestError
		if !errors.As(err, &requestErr) {
			if errors.Is(err, context.DeadlineExceeded) {
				requestErr = &publicmodel.RequestError{Category: publicmodel.ErrorTimeout, Cause: err}
			} else if streamStarted {
				requestErr = &publicmodel.RequestError{Category: publicmodel.ErrorProtocol, Cause: err}
			} else {
				return nil, err
			}
		}
		if !requestErr.Retryable() && !streamStarted {
			return nil, err
		}
		copy := *requestErr
		copy.Code = failureIdentifier(copy.Code)
		copy.RequestID = failureIdentifier(copy.RequestID)
		requestErr = &copy
		delay := time.Second
		for n := 1; n < attempt && delay < 8*time.Second; n++ {
			delay *= 2
		}
		delay += timing.jitter()
		if requestErr.RetryAfter > delay {
			delay = requestErr.RetryAfter
		}
		failure := publicmodel.Failure{Category: requestErr.Category, StatusCode: requestErr.StatusCode, Code: requestErr.Code, RequestID: requestErr.RequestID, Attempts: attempt, StreamStarted: streamStarted, RetryNotBefore: timing.now().Add(delay)}
		stop := attempt >= maxAttempts || streamStarted || delay > 30*time.Second
		if deadline, ok := ctx.Deadline(); ok && !timing.now().Add(delay).Before(deadline) {
			stop = true
		}
		if stop {
			return nil, &attemptFailure{failure: failure, cause: requestErr}
		}
		if a.options.ModelObserver != nil {
			a.options.ModelObserver(state, "retry", failure)
		}
		a.options.Logger.InfoContext(ctx, "model request retry", "category", failure.Category, "status", failure.StatusCode, "attempt", attempt)
		if err := timing.wait(ctx, delay); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil, err
			}
			return nil, &attemptFailure{failure: failure, cause: requestErr}
		}
	}
}

func (a *Agent) failOrPauseModel(ctx context.Context, state State, err error) (graph.Transition[State], error) {
	var failure *attemptFailure
	if errors.As(err, &failure) && !state.TeamBranch {
		state.ModelFailure = &failure.failure
		if a.options.ModelObserver != nil {
			a.options.ModelObserver(state, "required", failure.failure)
		}
		return graph.Wait(state, ModelRecoveryContinuation, modelNode), nil
	}
	return a.failTask(state, err)
}

// RetryResponse applies the same policy to manual compaction, without graph waits.
func (a *Agent) RetryResponse(ctx context.Context, requester Requester, request model.ModelRequest) (*model.ModelResponse, error) {
	if a.options.Execution.ModelTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.options.Execution.ModelTimeout)
		defer cancel()
	}
	return a.sendModel(ctx, State{Execution: a.options.Execution}, requester, request, nil)
}

func streamFailure(err error) bool {
	var failure *attemptFailure
	return errors.As(err, &failure) && failure.failure.StreamStarted
}

func failureIdentifier(value string) string {
	if len(value) > 256 {
		return ""
	}
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == ':' {
			continue
		}
		return ""
	}
	return value
}
