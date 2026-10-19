package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	publicmodel "github.com/afterlune/luneCore/model"
)

// NewProviderCatalogWithHostProviders creates a catalog that can mix the
// built-in protocol adapters with host-provided normalized providers.
func NewProviderCatalogWithHostProviders(config Config, overrides map[string]publicmodel.Provider) (*ProviderCatalog, error) {
	providers := make(map[string]Provider, len(overrides))
	if config.Providers == nil {
		config.Providers = make(map[string]ProviderConfig)
	}
	for name, provider := range overrides {
		if provider == nil || provider.Name() != name {
			return nil, fmt.Errorf("provider override for %q has an invalid name", name)
		}
		providerConfig, ok := config.Providers[name]
		if !ok {
			providerConfig = ProviderConfig{Protocol: ProtocolResponses, BaseURL: "https://host-provider.invalid", APIKey: "host-provider"}
		} else {
			if providerConfig.Protocol == "" {
				providerConfig.Protocol = ProtocolResponses
			}
			if providerConfig.BaseURL == "" {
				providerConfig.BaseURL = "https://host-provider.invalid"
			}
			if providerConfig.APIKey == "" {
				providerConfig.APIKey = "host-provider"
			}
		}
		config.Providers[name] = providerConfig
		providers[name] = hostProvider{inner: provider}
	}
	return NewProviderCatalogWithProviders(config, providers)
}

// hostProvider keeps mutable runtime values isolated from host implementations.
// The shared DTOs need no conversion, but JSON copying also omits TranscriptOnly.
type hostProvider struct{ inner publicmodel.Provider }

func (p hostProvider) Name() string { return p.inner.Name() }

func (p hostProvider) CreateResponse(ctx context.Context, request ModelRequest, profile ModelProfile) (*ModelResponse, error) {
	hostRequest, err := cloneModelRequestForHost(request)
	if err != nil {
		return nil, fmt.Errorf("copy request for host provider: %w", err)
	}
	response, err := p.inner.CreateResponse(ctx, hostRequest, profile)
	if response == nil {
		return nil, err
	}
	requestErr := err
	responseCopy, err := cloneForHost(response)
	if err != nil {
		return nil, fmt.Errorf("copy response from host provider: %w", err)
	}
	return responseCopy, requestErr
}

func (p hostProvider) StreamResponse(ctx context.Context, request ModelRequest, profile ModelProfile, emit func(ModelStreamEvent) error) (*ModelResponse, error) {
	hostRequest, err := cloneModelRequestForHost(request)
	if err != nil {
		return nil, fmt.Errorf("copy request for host provider: %w", err)
	}
	response, err := p.inner.StreamResponse(ctx, hostRequest, profile, func(event publicmodel.ModelStreamEvent) error {
		if emit == nil {
			return nil
		}
		copiedEvent, err := cloneForHost(event)
		if err != nil {
			return fmt.Errorf("copy event from host provider: %w", err)
		}
		return emit(copiedEvent)
	})
	if response == nil {
		return nil, err
	}
	requestErr := err
	responseCopy, err := cloneForHost(response)
	if err != nil {
		return nil, fmt.Errorf("copy response from host provider: %w", err)
	}
	return responseCopy, requestErr
}

func cloneModelRequestForHost(request ModelRequest) (publicmodel.ModelRequest, error) {
	hostRequest, err := cloneForHost(request)
	if err != nil {
		return hostRequest, err
	}
	if request.PromptCache != nil {
		hint := *request.PromptCache
		hostRequest.PromptCache = &hint
	}
	return hostRequest, nil
}

func cloneForHost[T any](value T) (T, error) {
	var copy T
	data, err := json.Marshal(value)
	if err != nil {
		return copy, err
	}
	if len(data) == 0 || string(data) == "null" {
		return copy, errors.New("empty model payload")
	}
	if err := json.Unmarshal(data, &copy); err != nil {
		return copy, err
	}
	return copy, nil
}
