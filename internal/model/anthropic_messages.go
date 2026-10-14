package model

import (
	"context"
	"fmt"
	"net/http"
)

const (
	anthropicAPIVersion = "2023-06-01"
)

type anthropicMessagesProvider struct {
	providerID string
	apiKey     string
	endpoint   string
	httpClient *http.Client
}

func newAnthropicMessagesProvider(providerID, apiKey, endpoint string, httpClient *http.Client) *anthropicMessagesProvider {
	return &anthropicMessagesProvider{providerID: providerID, apiKey: apiKey, endpoint: endpoint, httpClient: httpClient}
}

func (p *anthropicMessagesProvider) Name() string { return p.providerID }

func (p *anthropicMessagesProvider) CreateResponse(ctx context.Context, request ModelRequest, profile ModelProfile) (*ModelResponse, error) {
	wire, err := toAnthropicRequest(request, profile, false)
	if err != nil {
		return nil, err
	}
	response, err := postJSON(ctx, p.httpClient, p.endpoint, anthropicHeaders(p.apiKey, "application/json"), wire)
	if err != nil {
		return nil, fmt.Errorf("call %s Messages API: %w", p.providerID, err)
	}
	body, err := responseBody(response)
	if err != nil {
		return nil, err
	}
	if err := httpStatusError(response, body, p.providerID+" Messages API"); err != nil {
		return nil, err
	}
	result, err := decodeAnthropicResponse(body)
	if err != nil {
		return nil, fmt.Errorf("decode %s Messages response: %w", p.providerID, err)
	}
	return result, nil
}

func (p *anthropicMessagesProvider) StreamResponse(ctx context.Context, request ModelRequest, profile ModelProfile, emit func(ModelStreamEvent) error) (*ModelResponse, error) {
	wire, err := toAnthropicRequest(request, profile, true)
	if err != nil {
		return nil, err
	}
	response, err := postJSON(ctx, p.httpClient, p.endpoint, anthropicHeaders(p.apiKey, "text/event-stream"), wire)
	if err != nil {
		return nil, fmt.Errorf("call %s Messages stream: %w", p.providerID, err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, readErr := responseBody(response)
		if readErr != nil {
			return nil, readErr
		}
		return nil, httpStatusError(response, body, p.providerID+" Messages API")
	}
	defer response.Body.Close()
	result, streamErr := readAnthropicStream(ctx, response.Body, emit, wire.Model, p.providerID)
	return result, annotateRequestError(response, streamErr)
}

func anthropicHeaders(apiKey, accept string) map[string]string {
	return map[string]string{
		"x-api-key":         apiKey,
		"anthropic-version": anthropicAPIVersion,
		"Accept":            accept,
	}
}

var _ Provider = (*anthropicMessagesProvider)(nil)
