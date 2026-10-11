package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type responsesProvider struct {
	providerID string
	apiKey     string
	endpoint   string
	httpClient *http.Client
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
	wire := ResponsesRequest(request)
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
	return &decoded, nil
}

func (p *responsesProvider) StreamResponse(ctx context.Context, request ModelRequest, profile ModelProfile, emit func(ModelStreamEvent) error) (*ModelResponse, error) {
	request.Model = resolveModelID(request.Model, profile)
	if request.MaxOutputTokens == nil && profile.MaxOutputTokens > 0 {
		limit := profile.MaxOutputTokens
		request.MaxOutputTokens = &limit
	}
	request.Stream = true
	wire := ResponsesRequest(request)
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
	err = scanSSE(ctx, response.Body, func(_ string, data string) error {
		if data == "[DONE]" {
			return nil
		}
		var event ResponseStreamEvent
		if err := decodeSSEJSON(data, &event); err != nil {
			return err
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
			return fmt.Errorf("%s response failed (%s): %s", p.providerID, event.Response.Error.Code, event.Response.Error.Message)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if final == nil {
		return nil, fmt.Errorf("%s stream ended without a final response", p.providerID)
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
