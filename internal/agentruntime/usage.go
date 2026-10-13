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
}

func (r *UsageTrackingRequester) CreateResponse(ctx context.Context, request model.ModelRequest) (*model.ModelResponse, error) {
	response, err := r.inner.CreateResponse(ctx, request)
	if response != nil && (err == nil || response.Usage != (model.ResponseUsage{})) {
		r.mu.RLock()
		observer := r.observer
		contextObserver := r.contextObserver
		modelName := r.modelName
		r.mu.RUnlock()
		if request.Model != "" {
			modelName = request.Model
		}
		if contextObserver != nil {
			estimatedInputTokens := int64(0)
			if err == nil && response.Usage.InputTokens <= 0 {
				estimatedInputTokens = model.EstimateInputTokens(request)
			}
			contextObserver(ctx, response.Usage, r.agentID, r.profile, r.providerName, modelName, estimatedInputTokens)
		} else if observer != nil {
			estimatedInputTokens := int64(0)
			if err == nil && response.Usage.InputTokens <= 0 {
				estimatedInputTokens = model.EstimateInputTokens(request)
			}
			observer(response.Usage, r.agentID, r.profile, r.providerName, modelName, estimatedInputTokens)
		}
	}
	return response, err
}

func (r *UsageTrackingRequester) StreamResponse(ctx context.Context, request model.ModelRequest, emit func(model.ModelStreamEvent) error) (*model.ModelResponse, error) {
	streaming, ok := r.inner.(agentgraph.StreamingRequester)
	if !ok {
		return r.CreateResponse(ctx, request)
	}
	response, err := streaming.StreamResponse(ctx, request, emit)
	if response != nil && (err == nil || response.Usage != (model.ResponseUsage{})) {
		r.mu.RLock()
		observer := r.observer
		contextObserver := r.contextObserver
		modelName := r.modelName
		r.mu.RUnlock()
		if request.Model != "" {
			modelName = request.Model
		}
		if contextObserver != nil {
			estimatedInputTokens := int64(0)
			if err == nil && response.Usage.InputTokens <= 0 {
				estimatedInputTokens = model.EstimateInputTokens(request)
			}
			contextObserver(ctx, response.Usage, r.agentID, r.profile, r.providerName, modelName, estimatedInputTokens)
		} else if observer != nil {
			estimatedInputTokens := int64(0)
			if err == nil && response.Usage.InputTokens <= 0 {
				estimatedInputTokens = model.EstimateInputTokens(request)
			}
			observer(response.Usage, r.agentID, r.profile, r.providerName, modelName, estimatedInputTokens)
		}
	}
	return response, err
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
