package model

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	publicmodel "github.com/afterlune/luneCore/model"
)

// Provider is the network boundary for one model API. Request/response values
// are the agent's shared conversation contract; implementations translate
// them to the provider's native wire format.
type Provider = publicmodel.Provider

// ResponsesRequest is the wire request shared by Responses-compatible APIs.
type ResponsesRequest ModelRequest

func resolveModelID(requestModel string, profile ModelProfile) string {
	if requestModel != "" {
		return requestModel
	}
	return profile.Model
}

func providerEndpoint(protocol, baseURL string) (string, error) {
	if err := validateBaseURL(baseURL); err != nil {
		return "", err
	}
	baseURL = strings.TrimRight(baseURL, "/")
	switch protocol {
	case ProtocolResponses:
		if strings.HasSuffix(baseURL, "/responses") {
			return baseURL, nil
		}
		return baseURL + "/responses", nil
	case ProtocolAnthropicMessages:
		if strings.HasSuffix(baseURL, "/v1/messages") {
			return baseURL, nil
		}
		return baseURL + "/v1/messages", nil
	default:
		return "", fmt.Errorf("unsupported model API protocol %q", protocol)
	}
}

type ModelStreamEventType = publicmodel.ModelStreamEventType

const (
	ModelStreamTextDelta      = publicmodel.ModelStreamTextDelta
	ModelStreamReasoningDelta = publicmodel.ModelStreamReasoningDelta
	ModelStreamToolStarted    = publicmodel.ModelStreamToolStarted
	ModelStreamToolDelta      = publicmodel.ModelStreamToolDelta
	ModelStreamCompleted      = publicmodel.ModelStreamCompleted
	ModelStreamFailed         = publicmodel.ModelStreamFailed
)

// ModelStreamEvent is the provider-neutral streaming surface consumed by
// Agent and Core.
type ModelStreamEvent = publicmodel.ModelStreamEvent
type UserQuestionOption = publicmodel.UserQuestionOption
type UserQuestionItem = publicmodel.UserQuestionItem
type UserQuestionResponse = publicmodel.UserQuestionResponse

type ProviderCatalog struct {
	config    Config
	providers map[string]Provider
}

type agentIDContextKey struct{}

func WithAgentID(ctx context.Context, agentID string) context.Context {
	if agentID == "" {
		return ctx
	}
	return context.WithValue(ctx, agentIDContextKey{}, agentID)
}

func AgentIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(agentIDContextKey{}).(string)
	return value
}

func NewProviderCatalog(config Config) (*ProviderCatalog, error) {
	return NewProviderCatalogWithProviders(config, nil)
}

// NewProviderCatalogWithProviders allows the Core to inject provider
// implementations and keeps catalog orchestration testable without network
// access.
func NewProviderCatalogWithProviders(config Config, overrides map[string]Provider) (*ProviderCatalog, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	httpClient := &http.Client{Timeout: requestTimeout}
	catalog := &ProviderCatalog{config: config, providers: make(map[string]Provider)}
	for name, providerConfig := range config.Providers {
		if provider, ok := overrides[name]; ok {
			if provider == nil || provider.Name() != name {
				return nil, fmt.Errorf("provider override for %q has an invalid name", name)
			}
			catalog.providers[name] = provider
			continue
		}
		if strings.TrimSpace(providerConfig.APIKey) == "" {
			continue
		}
		apiKey := strings.TrimSpace(providerConfig.APIKey)
		endpoint, err := providerEndpoint(providerConfig.Protocol, providerConfig.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("provider %q endpoint: %w", name, err)
		}
		switch providerConfig.Protocol {
		case ProtocolResponses:
			catalog.providers[name] = newResponsesProvider(name, apiKey, endpoint, httpClient)
		case ProtocolAnthropicMessages:
			catalog.providers[name] = newAnthropicMessagesProvider(name, apiKey, endpoint, httpClient)
		}
	}
	return catalog, nil
}

func (c *ProviderCatalog) DefaultModel() string {
	if c == nil {
		return ""
	}
	return c.config.DefaultModel
}

func (c *ProviderCatalog) ParallelLimit() int {
	if c == nil {
		return DefaultMaxParallelAgents
	}
	return c.config.ParallelLimit()
}

func (c *ProviderCatalog) Profile(name string) (ModelProfile, bool) {
	if c == nil {
		return ModelProfile{}, false
	}
	profile, ok := c.config.Models[name]
	return profile, ok
}

// ProviderConfig returns the API protocol and endpoint capabilities selected
// for one configured provider.
func (c *ProviderCatalog) ProviderConfig(name string) (ProviderConfig, bool) {
	if c == nil {
		return ProviderConfig{}, false
	}
	provider, ok := c.config.Providers[name]
	return provider, ok
}

func (c *ProviderCatalog) Profiles() map[string]ModelProfile {
	profiles := make(map[string]ModelProfile)
	if c == nil {
		return profiles
	}
	for name, profile := range c.config.Models {
		profiles[name] = profile
	}
	return profiles
}

func (c *ProviderCatalog) NewClient(profileID string, tools ToolExecutor, logger *slog.Logger) (*Client, error) {
	if c == nil {
		return nil, errors.New("model provider catalog is nil")
	}
	if tools == nil {
		return nil, errors.New("tool executor is required")
	}
	profile, ok := c.config.Models[profileID]
	if !ok {
		return nil, fmt.Errorf("unknown model profile %q", profileID)
	}
	provider, ok := c.providers[profile.Provider]
	if !ok {
		return nil, fmt.Errorf("provider %q is unavailable for model profile %q", profile.Provider, profileID)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{provider: provider, profileID: profileID, profile: profile, tools: tools, logger: logger}, nil
}

func (c *Client) ProfileID() string {
	if c == nil {
		return ""
	}
	return c.profileID
}

func (c *Client) ProviderName() string {
	if c == nil || c.provider == nil {
		return ""
	}
	return c.provider.Name()
}

func (c *Client) ModelName() string {
	if c == nil {
		return ""
	}
	return c.profile.Model
}
