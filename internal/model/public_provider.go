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
	internalProviders := make(map[string]Provider, len(overrides))
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
		internalProviders[name] = hostProvider{inner: provider}
	}
	return NewProviderCatalogWithProviders(config, internalProviders)
}

type hostProvider struct{ inner publicmodel.Provider }

func (p hostProvider) Name() string { return p.inner.Name() }

func (p hostProvider) CreateResponse(ctx context.Context, request ModelRequest, profile ModelProfile) (*ModelResponse, error) {
	var publicRequest publicmodel.ModelRequest
	if err := jsonRoundTrip(request, &publicRequest); err != nil {
		return nil, fmt.Errorf("encode request for host provider: %w", err)
	}
	response, err := p.inner.CreateResponse(ctx, publicRequest, toPublicProfile(profile))
	if err != nil || response == nil {
		return nil, err
	}
	var internalResponse ModelResponse
	if err := jsonRoundTrip(response, &internalResponse); err != nil {
		return nil, fmt.Errorf("decode host provider response: %w", err)
	}
	return &internalResponse, nil
}

func (p hostProvider) StreamResponse(ctx context.Context, request ModelRequest, profile ModelProfile, emit func(ModelStreamEvent) error) (*ModelResponse, error) {
	var publicRequest publicmodel.ModelRequest
	if err := jsonRoundTrip(request, &publicRequest); err != nil {
		return nil, fmt.Errorf("encode request for host provider: %w", err)
	}
	response, err := p.inner.StreamResponse(ctx, publicRequest, toPublicProfile(profile), func(event publicmodel.ModelStreamEvent) error {
		converted, err := internalEventFromHost(event)
		if err != nil {
			return err
		}
		return emit(converted)
	})
	if err != nil || response == nil {
		return nil, err
	}
	var internalResponse ModelResponse
	if err := jsonRoundTrip(response, &internalResponse); err != nil {
		return nil, fmt.Errorf("decode host provider response: %w", err)
	}
	return &internalResponse, nil
}

func internalEventFromHost(event publicmodel.ModelStreamEvent) (ModelStreamEvent, error) {
	converted := ModelStreamEvent{
		Type: ModelStreamEventType(event.Type), ItemID: event.ItemID, OutputIndex: event.OutputIndex,
		Delta: event.Delta, Raw: append(json.RawMessage(nil), event.Raw...),
	}
	if event.ToolCall != nil {
		var call OutputFunctionCall
		if err := jsonRoundTrip(event.ToolCall, &call); err != nil {
			return ModelStreamEvent{}, fmt.Errorf("decode host provider tool stream event: %w", err)
		}
		converted.ToolCall = &call
	}
	if event.Response != nil {
		var response ModelResponse
		if err := jsonRoundTrip(event.Response, &response); err != nil {
			return ModelStreamEvent{}, fmt.Errorf("decode host provider response stream event: %w", err)
		}
		converted.Response = &response
	}
	if event.Error != nil {
		var responseError ResponseError
		if err := jsonRoundTrip(event.Error, &responseError); err != nil {
			return ModelStreamEvent{}, fmt.Errorf("decode host provider error stream event: %w", err)
		}
		converted.Error = &responseError
	}
	return converted, nil
}

func toPublicProfile(profile ModelProfile) publicmodel.ModelProfile {
	return publicmodel.ModelProfile{
		Provider: profile.Provider, Model: profile.Model, MaxOutputTokens: profile.MaxOutputTokens,
		SupportsInputAudio: profile.SupportsInputAudio,
	}
}

func jsonRoundTrip(source, destination any) error {
	data, err := json.Marshal(source)
	if err != nil {
		return err
	}
	if len(data) == 0 || string(data) == "null" {
		return errors.New("empty model payload")
	}
	if err := json.Unmarshal(data, destination); err != nil {
		return err
	}
	return nil
}
