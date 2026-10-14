package agentruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
)

type tokenBudgetProbeRequester struct {
	response *model.ModelResponse
	err      error
	request  model.ModelRequest
}

func (r *tokenBudgetProbeRequester) CreateResponse(_ context.Context, request model.ModelRequest) (*model.ModelResponse, error) {
	r.request = request
	return r.response, r.err
}

type tokenBudgetProbe struct {
	reservation TokenBudgetReservation
	allowed     bool
	reserveErr  error
	reservedIn  int64
	reservedOut int64
	settledID   int64
	settled     int64
	settleCalls int
}

func (b *tokenBudgetProbe) Reserve(_ context.Context, input, output int64) (TokenBudgetReservation, bool, error) {
	b.reservedIn, b.reservedOut = input, output
	return b.reservation, b.allowed, b.reserveErr
}

func (b *tokenBudgetProbe) Settle(_ context.Context, id, actual int64) error {
	b.settledID, b.settled, b.settleCalls = id, actual, b.settleCalls+1
	return nil
}

func TestUsageTrackingRequesterCapsOutputAndPausesOnBudgetIncomplete(t *testing.T) {
	requester := &tokenBudgetProbeRequester{response: &model.ModelResponse{
		Status: "incomplete", IncompleteDetails: &model.IncompleteDetails{Reason: "max_output_tokens"},
		Usage: model.ResponseUsage{InputTokens: 40, OutputTokens: 10, TotalTokens: 50},
	}}
	budget := &tokenBudgetProbe{reservation: TokenBudgetReservation{ID: 7, OutputLimit: 20, OutputWasLimited: true}, allowed: true}
	tracked := NewUsageTrackingRequester(requester, "lead", "profile", "provider", "model")
	tracked.SetMaxOutputTokens(1000)
	request := model.ModelRequest{Input: model.NewResponseInputText("this is a request")}
	response, err := tracked.CreateResponse(WithTokenBudget(context.Background(), budget), request)
	if !errors.Is(err, model.ErrTokenBudgetExceeded) || response == nil {
		t.Fatalf("CreateResponse = (%#v, %v), want incomplete response and token budget pause", response, err)
	}
	if requester.request.MaxOutputTokens == nil || *requester.request.MaxOutputTokens != 20 {
		t.Fatalf("provider output limit = %v, want 20", requester.request.MaxOutputTokens)
	}
	if budget.reservedIn != model.EstimateInputTokens(request) || budget.reservedOut != 1000 {
		t.Fatalf("reservation request = input %d/output %d", budget.reservedIn, budget.reservedOut)
	}
	if budget.settleCalls != 1 || budget.settledID != 7 || budget.settled != 50 {
		t.Fatalf("settled reservation = id %d, usage %d, calls %d", budget.settledID, budget.settled, budget.settleCalls)
	}
}

func TestUsageTrackingRequesterKeepsReservationWithoutCertainUsage(t *testing.T) {
	for _, test := range []struct {
		name     string
		response *model.ModelResponse
		err      error
	}{
		{name: "missing usage", response: &model.ModelResponse{Status: "completed"}},
		{name: "ambiguous outcome", response: &model.ModelResponse{Usage: model.ResponseUsage{TotalTokens: 3}}, err: errors.New("connection lost")},
	} {
		t.Run(test.name, func(t *testing.T) {
			budget := &tokenBudgetProbe{reservation: TokenBudgetReservation{ID: 9, OutputLimit: 10}, allowed: true}
			tracked := NewUsageTrackingRequester(&tokenBudgetProbeRequester{response: test.response, err: test.err}, "lead", "profile", "provider", "model")
			_, _ = tracked.CreateResponse(WithTokenBudget(context.Background(), budget), model.ModelRequest{Input: model.NewResponseInputText("call")})
			if budget.settleCalls != 0 {
				t.Fatalf("settled uncertain reservation %d times", budget.settleCalls)
			}
		})
	}
}

var _ agentgraph.Requester = (*tokenBudgetProbeRequester)(nil)
