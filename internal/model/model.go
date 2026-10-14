package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	DeepSeekFlashModel   = "deepseek-flash"
	requestTimeout       = 2 * time.Minute
	maxResponseBodyBytes = 8 * 1024 * 1024
)

type Client struct {
	mu        sync.Mutex
	provider  Provider
	profileID string
	profile   ModelProfile
	tools     ToolExecutor
	logger    *slog.Logger
	history   []InputItem
}

// NewResponseInput encodes any JSON-serializable Responses input value. Pass a
// string for simple text or an item slice for structured and multimodal input.
func NewResponseInput[T any](value T) (*ResponseInput, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode input: %w", err)
	}
	var input ResponseInput
	if err := json.Unmarshal(data, &input); err != nil {
		return nil, fmt.Errorf("decode input: %w", err)
	}
	if input.Text == nil && input.Items == nil {
		return nil, errors.New("input must be a string or an item list")
	}
	return &input, nil
}

// CreateResponse sends one normalized agent request through the configured
// provider without managing history or executing returned tool calls.
func (c *Client) CreateResponse(ctx context.Context, request ModelRequest) (*ModelResponse, error) {
	if c == nil {
		return nil, errors.New("model client is nil")
	}
	if ctx == nil {
		return nil, errors.New("context is nil")
	}
	if c.provider == nil {
		return nil, errors.New("model provider is required")
	}
	if request.Model == "" {
		request.Model = c.profile.Model
	}
	return c.provider.CreateResponse(ctx, request, c.profile)
}

// StreamResponse streams normalized model events and returns the final
// response. A nil event callback discards incremental events.
func (c *Client) StreamResponse(ctx context.Context, request ModelRequest, emit func(ModelStreamEvent) error) (*ModelResponse, error) {
	if c == nil {
		return nil, errors.New("model client is nil")
	}
	if ctx == nil {
		return nil, errors.New("context is nil")
	}
	if c.provider == nil {
		return nil, errors.New("model provider is required")
	}
	if request.Model == "" {
		request.Model = c.profile.Model
	}
	return c.provider.StreamResponse(ctx, request, c.profile, emit)
}

func newClient(apiKey, endpoint string, httpClient *http.Client, tools ToolExecutor) *Client {
	return &Client{
		provider:  newResponsesProvider(ProviderDeepSeek, apiKey, endpoint, httpClient),
		profileID: DeepSeekFlashModel,
		profile:   ModelProfile{Provider: ProviderDeepSeek, Model: DeepSeekFlashModel},
		tools:     tools,
		logger:    slog.Default(),
	}
}

func readAPIKey(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	var config struct {
		APIKey string `json:"apikey"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return "", fmt.Errorf("decode %s: %w", path, err)
	}
	apiKey := strings.TrimSpace(config.APIKey)
	if apiKey == "" {
		return "", fmt.Errorf("%s must contain a non-empty apikey", path)
	}
	return apiKey, nil
}
