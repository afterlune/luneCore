package agentruntime

import (
	"context"
	"sync"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
)

type UsageTrackingRequester struct {
	inner           agentgraph.Requester
	mu              sync.RWMutex
	observer        func(model.ResponseUsage, string, string, string, string, int64)
	contextObserver func(context.Context, model.ResponseUsage, string, string, string, string, int64)
	agentID         string
	profile         string
	providerName    string
	modelName       string
	maxOutputTokens int64
}

func (r *UsageTrackingRequester) CreateResponse(ctx context.Context, request model.ModelRequest) (*model.ModelResponse, error) {
	request, reservation, budgetLimited, err := r.reserveTokens(ctx, request)
	if err != nil {
		return nil, err
	}
	response, err := r.inner.CreateResponse(ctx, request)
	r.settleTokens(ctx, reservation, response, err)
	if err == nil && budgetLimited && responseIncompleteForOutputLimit(response) {
		err = model.ErrTokenBudgetExceeded
	}
	r.observeUsage(ctx, request, response, err)
	return response, err
}

func (r *UsageTrackingRequester) CompactResponse(ctx context.Context, request model.ModelRequest) (*model.ModelResponse, error) {
	compactor, ok := r.inner.(agentgraph.CompactionRequester)
	if !ok {
		return nil, model.ErrCompactionUnsupported
	}
	request, reservation, _, err := r.reserveTokens(ctx, request)
	if err != nil {
		return nil, err
	}
	response, err := compactor.CompactResponse(ctx, request)
	r.settleTokens(ctx, reservation, response, err)
	r.observeUsage(ctx, request, response, err)
	return response, err
}

func (r *UsageTrackingRequester) StreamResponse(ctx context.Context, request model.ModelRequest, emit func(model.ModelStreamEvent) error) (*model.ModelResponse, error) {
	streaming, ok := r.inner.(agentgraph.StreamingRequester)
	if !ok {
		return r.CreateResponse(ctx, request)
	}
	request, reservation, budgetLimited, err := r.reserveTokens(ctx, request)
	if err != nil {
		return nil, err
	}
	response, err := streaming.StreamResponse(ctx, request, emit)
	r.settleTokens(ctx, reservation, response, err)
	if err == nil && budgetLimited && responseIncompleteForOutputLimit(response) {
		err = model.ErrTokenBudgetExceeded
	}
	r.observeUsage(ctx, request, response, err)
	return response, err
}

func (r *UsageTrackingRequester) observeUsage(ctx context.Context, request model.ModelRequest, response *model.ModelResponse, err error) {
	if response == nil || err != nil && response.Usage == (model.ResponseUsage{}) {
		return
	}
	r.mu.RLock()
	observer := r.observer
	contextObserver := r.contextObserver
	modelName := r.modelName
	r.mu.RUnlock()
	if request.Model != "" {
		modelName = request.Model
	}
	estimatedInputTokens := int64(0)
	if err == nil && response.Usage.InputTokens <= 0 {
		estimatedInputTokens = model.EstimateInputTokens(request)
	}
	if contextObserver != nil {
		contextObserver(ctx, response.Usage, r.agentID, r.profile, r.providerName, modelName, estimatedInputTokens)
	} else if observer != nil {
		observer(response.Usage, r.agentID, r.profile, r.providerName, modelName, estimatedInputTokens)
	}
}

type TokenBudgetReservation struct {
	ID               int64
	OutputLimit      int64
	OutputWasLimited bool
}

// TokenBudget reserves requests atomically across all agents in one execution.
// A reservation not settled with measured usage remains charged in full.
type TokenBudget interface {
	Reserve(context.Context, int64, int64) (TokenBudgetReservation, bool, error)
	Settle(context.Context, int64, int64) error
}

type tokenBudgetContextKey struct{}

func WithTokenBudget(ctx context.Context, budget TokenBudget) context.Context {
	return context.WithValue(ctx, tokenBudgetContextKey{}, budget)
}

func tokenBudgetFromContext(ctx context.Context) TokenBudget {
	budget, _ := ctx.Value(tokenBudgetContextKey{}).(TokenBudget)
	return budget
}

func (r *UsageTrackingRequester) reserveTokens(ctx context.Context, request model.ModelRequest) (model.ModelRequest, TokenBudgetReservation, bool, error) {
	budget := tokenBudgetFromContext(ctx)
	if budget == nil {
		return request, TokenBudgetReservation{}, false, nil
	}
	r.mu.RLock()
	maxOutputTokens := r.maxOutputTokens
	r.mu.RUnlock()
	requestedOutput := maxOutputTokens
	if request.MaxOutputTokens != nil {
		requestedOutput = *request.MaxOutputTokens
	}
	reservation, allowed, err := budget.Reserve(ctx, model.EstimateInputTokens(request), requestedOutput)
	if err != nil {
		return request, TokenBudgetReservation{}, false, err
	}
	if !allowed {
		return request, TokenBudgetReservation{}, false, model.ErrTokenBudgetExceeded
	}
	if reservation.OutputLimit > 0 && (request.MaxOutputTokens == nil || reservation.OutputLimit < *request.MaxOutputTokens) {
		limit := reservation.OutputLimit
		request.MaxOutputTokens = &limit
	}
	return request, reservation, reservation.OutputWasLimited, nil
}

func (r *UsageTrackingRequester) settleTokens(ctx context.Context, reservation TokenBudgetReservation, response *model.ModelResponse, err error) {
	if reservation.ID == 0 || err != nil || response == nil {
		return
	}
	usage := response.Usage
	actual := usage.TotalTokens
	if actual <= 0 {
		actual = usage.InputTokens + usage.OutputTokens
	}
	if actual <= 0 {
		return
	}
	if budget := tokenBudgetFromContext(ctx); budget != nil {
		// A failed settlement leaves the durable reservation charged in full.
		_ = budget.Settle(ctx, reservation.ID, actual)
	}
}

func responseIncompleteForOutputLimit(response *model.ModelResponse) bool {
	return response != nil && response.Status == "incomplete" && response.IncompleteDetails != nil && response.IncompleteDetails.Reason == "max_output_tokens"
}

func (r *UsageTrackingRequester) SetUsageObserver(observer func(model.ResponseUsage)) {
	if observer == nil {
		r.SetAgentUsageObserver(nil)
		return
	}
	r.SetAgentUsageObserver(func(usage model.ResponseUsage, _ string, _ string, _ string, _ string, _ int64) {
		observer(usage)
	})
}

func (r *UsageTrackingRequester) SetAgentUsageObserver(observer func(model.ResponseUsage, string, string, string, string, int64)) {
	r.mu.Lock()
	r.observer = observer
	r.contextObserver = nil
	r.mu.Unlock()
}

func (r *UsageTrackingRequester) SetContextAgentUsageObserver(observer func(context.Context, model.ResponseUsage, string, string, string, string, int64)) {
	r.mu.Lock()
	r.contextObserver = observer
	r.observer = nil
	r.mu.Unlock()
}

func (r *Responder) SetUsageObserver(observer func(model.ResponseUsage)) {
	if r.requester != nil {
		r.requester.SetUsageObserver(observer)
	}
}

func NewUsageTrackingRequester(inner agentgraph.Requester, agentID, profile, providerName, modelName string) *UsageTrackingRequester {
	return &UsageTrackingRequester{inner: inner, agentID: agentID, profile: profile, providerName: providerName, modelName: modelName}
}

func (r *UsageTrackingRequester) SetMaxOutputTokens(maxOutputTokens int64) {
	r.mu.Lock()
	r.maxOutputTokens = maxOutputTokens
	r.mu.Unlock()
}
