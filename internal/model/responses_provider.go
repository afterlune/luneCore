package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type responsesProvider struct {
	providerID string
	apiKey     string
	endpoint   string
	httpClient *http.Client
}

type responsesWireRequest struct {
	ResponsesRequest
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
}

type responsesCompactWireRequest struct {
	Model        string         `json:"model"`
	Input        *ResponseInput `json:"input,omitempty"`
	Instructions string         `json:"instructions,omitempty"`
}

func (p *responsesProvider) wireRequest(request ModelRequest) responsesWireRequest {
	wire := responsesWireRequest{ResponsesRequest: ResponsesRequest(request)}
	if p.providerID == ProviderOpenAI && request.PromptCache != nil {
		wire.PromptCacheKey = request.PromptCache.Key
	}
	return wire
}

func newResponsesProvider(providerID, apiKey, endpoint string, httpClient *http.Client) *responsesProvider {
	return &responsesProvider{providerID: providerID, apiKey: apiKey, endpoint: endpoint, httpClient: httpClient}
}

func (p *responsesProvider) Name() string { return p.providerID }

func (p *responsesProvider) CreateResponse(ctx context.Context, request ModelRequest, profile ModelProfile) (*ModelResponse, error) {
	request.Model = resolveModelID(request.Model, profile)
	if request.MaxOutputTokens == nil && profile.MaxOutputTokens > 0 {
		limit := profile.MaxOutputTokens
		request.MaxOutputTokens = &limit
	}
	request.Stream = false
	wire := p.wireRequest(request)
	response, err := postJSON(ctx, p.httpClient, p.endpoint, map[string]string{
		"Authorization": "Bearer " + p.apiKey,
	}, wire)
	if err != nil {
		return nil, fmt.Errorf("call %s Responses API: %w", p.providerID, err)
	}
	body, err := responseBody(response)
	if err != nil {
		return nil, err
	}
	if err := httpStatusError(response, body, p.providerID+" Responses API"); err != nil {
		return nil, err
	}
	var decoded ModelResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode %s Responses response: %w", p.providerID, err)
	}
	if decoded.Status == "failed" && decoded.Error != nil {
		return &decoded, annotateRequestError(response, ResponseFailure(decoded.Error))
	}
	return &decoded, nil
}

func (p *responsesProvider) CompactResponse(ctx context.Context, request ModelRequest, profile ModelProfile) (*ModelResponse, error) {
	request.Model = resolveModelID(request.Model, profile)
	endpoint := strings.TrimRight(p.endpoint, "/") + "/compact"
	wire := responsesCompactWireRequest{
		Model:        request.Model,
		Input:        request.Input,
		Instructions: request.Instructions,
	}
	response, err := postJSON(ctx, p.httpClient, endpoint, map[string]string{
		"Authorization": "Bearer " + p.apiKey,
	}, wire)
	if err != nil {
		return nil, fmt.Errorf("call %s Responses compact endpoint: %w", p.providerID, err)
	}
	body, err := responseBody(response)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed || response.StatusCode == http.StatusNotImplemented {
		return nil, fmt.Errorf("%w: %s Responses compact endpoint returned HTTP %d", ErrCompactionUnsupported, p.providerID, response.StatusCode)
	}
	if err := httpStatusError(response, body, p.providerID+" Responses compact endpoint"); err != nil {
		return nil, err
	}
	var decoded ModelResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode %s Responses compact response: %w", p.providerID, err)
	}
	if decoded.Status == "failed" && decoded.Error != nil {
		return &decoded, annotateRequestError(response, ResponseFailure(decoded.Error))
	}
	return &decoded, nil
}

func (p *responsesProvider) StreamResponse(ctx context.Context, request ModelRequest, profile ModelProfile, emit func(ModelStreamEvent) error) (*ModelResponse, error) {
	request.Model = resolveModelID(request.Model, profile)
	if request.MaxOutputTokens == nil && profile.MaxOutputTokens > 0 {
		limit := profile.MaxOutputTokens
		request.MaxOutputTokens = &limit
	}
	request.Stream = true
	wire := p.wireRequest(request)
	response, err := postJSON(ctx, p.httpClient, p.endpoint, map[string]string{
		"Authorization": "Bearer " + p.apiKey,
		"Accept":        "text/event-stream",
	}, wire)
	if err != nil {
		return nil, fmt.Errorf("call %s Responses API stream: %w", p.providerID, err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, readErr := responseBody(response)
		if readErr != nil {
			return nil, readErr
		}
		return nil, httpStatusError(response, body, p.providerID+" Responses API")
	}
	defer response.Body.Close()
	var final *Response
	terminal := false
	err = scanSSE(ctx, response.Body, func(_ string, data string) error {
		if data == "[DONE]" {
			return nil
		}
		var event ResponseStreamEvent
		if err := decodeSSEJSON(data, &event); err != nil {
			return err
		}
		if event.Type == EventResponseCompleted || event.Type == EventResponseIncomplete {
			terminal = true
		}
		if event.Type == "error" {
			return providerPayloadError(0, []byte(data))
		}
		if event.Response != nil {
			final = event.Response
		}
		if emit != nil {
			if err := emit(normalizeResponsesStreamEvent(event, data)); err != nil {
				return err
			}
		}
		if event.Type == EventResponseFailed && event.Response != nil && event.Response.Error != nil {
			return ResponseFailure(event.Response.Error)
		}
		return nil
	})
	if err != nil {
		return final, annotateRequestError(response, err)
	}
	if final == nil || !terminal {
		return final, annotateRequestError(response, transportError(io.ErrUnexpectedEOF))
	}
	return final, nil
}

func normalizeResponsesStreamEvent(event ResponseStreamEvent, raw string) ModelStreamEvent {
	normalized := ModelStreamEvent{ItemID: event.ItemID, OutputIndex: event.OutputIndex, Delta: event.Delta, Response: event.Response}
	switch event.Type {
	case EventOutputTextDelta:
		normalized.Type = ModelStreamTextDelta
	case EventReasoningTextDelta:
		normalized.Type = ModelStreamReasoningDelta
	case EventFunctionCallArgumentsDelta, EventCustomToolCallInputDelta:
		normalized.Type = ModelStreamToolDelta
		if event.Type == EventCustomToolCallInputDelta {
			normalized.Delta = event.Input
		}
	case EventOutputItemAdded:
		if call, ok := event.Item.(OutputFunctionCall); ok {
			normalized.Type = ModelStreamToolStarted
			normalized.ToolCall = &call
		} else {
			normalized.Raw = cloneJSON([]byte(raw))
		}
	case EventResponseCompleted, EventResponseIncomplete:
		normalized.Type = ModelStreamCompleted
	case EventResponseFailed:
		normalized.Type = ModelStreamFailed
		if event.Response != nil {
			normalized.Error = event.Response.Error
		}
	default:
		normalized.Raw = cloneJSON([]byte(raw))
	}
	return normalized
}

var _ Provider = (*responsesProvider)(nil)
