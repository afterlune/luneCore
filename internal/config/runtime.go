package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/afterlune/luneCore/internal/model"
	publicmodel "github.com/afterlune/luneCore/model"
)

const hostProviderPlaceholder = "host-provider"

// LoadForRuntime selects either a complete host-supplied configuration or the
// workspace config.json, then overlays any legacy host model settings.
func LoadForRuntime(workspace string, supplied *publicmodel.Config, hostProviders map[string]publicmodel.Provider, projectConfig *ProjectConfig) (Config, error) {
	var loaded Config
	if projectConfig != nil {
		loaded = projectConfig.toConfig()
		loaded.applyDefaults()
	} else {
		validateProjectModelCatalog := supplied == nil && len(hostProviders) == 0
		fileConfig, err := load(filepath.Join(workspace, FileName), validateProjectModelCatalog)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) || supplied == nil {
				return Config{}, err
			}
			loaded = Config{AutoCompact: true, AutoCompactThreshold: DefaultAutoCompactThreshold}
		} else {
			loaded = fileConfig
		}
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
				Provider: profile.Provider, Model: profile.EffectiveModel(), ModelID: profile.EffectiveModel(), DisplayName: profile.DisplayName, ContextWindow: profile.ContextWindow, MaxOutputTokens: profile.MaxOutputTokens,
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
			return Config{}, fmt.Errorf("host model provider %q has an invalid name", name)
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
		loaded.AutoCompactThreshold = DefaultAutoCompactThreshold
	}
	if err := loaded.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate runtime configuration: %w", err)
	}
	return loaded, nil
}
