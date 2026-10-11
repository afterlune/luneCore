package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
	publicmodel "github.com/afterlune/luneCore/model"
)

const hostProviderPlaceholder = "host-provider"

func loadProjectConfig(workspace string, supplied *publicmodel.Config, hostProviders map[string]publicmodel.Provider) (projectconfig.Config, error) {
	loaded, err := projectconfig.Load(filepath.Join(workspace, projectconfig.FileName))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) || supplied == nil {
			return projectconfig.Config{}, err
		}
		loaded = projectconfig.Config{AutoCompact: true, AutoCompactThreshold: projectconfig.DefaultAutoCompactThreshold}
	}
	if supplied != nil {
		if loaded.Providers == nil {
			loaded.Providers = make(map[string]model.ProviderConfig, len(supplied.Providers))
		}
		for name, provider := range supplied.Providers {
			loaded.Providers[name] = model.ProviderConfig{
				Protocol: provider.Protocol, BaseURL: provider.BaseURL, APIKey: provider.APIKey,
				ServerCompaction: provider.ServerCompaction,
			}
		}
		if loaded.Models == nil {
			loaded.Models = make(map[string]model.ModelProfile, len(supplied.Models))
		}
		for name, profile := range supplied.Models {
			loaded.Models[name] = model.ModelProfile{
				Provider: profile.Provider, Model: profile.Model, MaxOutputTokens: profile.MaxOutputTokens,
				SupportsInputAudio: profile.SupportsInputAudio,
			}
		}
		if supplied.DefaultModel != "" {
			loaded.DefaultModel = supplied.DefaultModel
		}
		if supplied.MaxParallelAgents != 0 {
			loaded.MaxParallelAgents = supplied.MaxParallelAgents
		}
	}
	if loaded.Providers == nil {
		loaded.Providers = make(map[string]model.ProviderConfig)
	}
	for name, provider := range hostProviders {
		if provider == nil || strings.TrimSpace(name) == "" || provider.Name() != name {
			return projectconfig.Config{}, fmt.Errorf("host model provider %q has an invalid name", name)
		}
		configured, ok := loaded.Providers[name]
		if !ok {
			configured = model.ProviderConfig{Protocol: model.ProtocolResponses, BaseURL: "https://host-provider.invalid"}
		}
		if configured.Protocol == "" {
			configured.Protocol = model.ProtocolResponses
		}
		if configured.BaseURL == "" {
			configured.BaseURL = "https://host-provider.invalid"
		}
		if configured.APIKey == "" {
			configured.APIKey = hostProviderPlaceholder
		}
		loaded.Providers[name] = configured
	}
	if loaded.AutoCompact && loaded.AutoCompactThreshold == 0 {
		loaded.AutoCompactThreshold = projectconfig.DefaultAutoCompactThreshold
	}
	if err := loaded.Validate(); err != nil {
		return projectconfig.Config{}, fmt.Errorf("validate runtime configuration: %w", err)
	}
	return loaded, nil
}
