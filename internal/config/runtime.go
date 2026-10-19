package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	publicmodel "github.com/afterlune/luneCore/model"
)

const hostProviderPlaceholder = "host-provider"

// LoadForRuntime selects the project config.json or a host-supplied project
// config, then overlays explicit host model settings.
func LoadForRuntime(workspace string, supplied *publicmodel.Config, hostProviders map[string]publicmodel.Provider, projectConfig *ProjectConfig, maxParallelAgents int) (Config, error) {
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
			loaded = DefaultProjectConfig().toConfig()
		} else {
			loaded = fileConfig
		}
	}
	if supplied != nil {
		if loaded.Models.Providers == nil {
			loaded.Models.Providers = make(map[string]publicmodel.ProviderConfig, len(supplied.Providers))
		}
		for name, provider := range supplied.Providers {
			loaded.Models.Providers[name] = publicmodel.ProviderConfig{
				Protocol: provider.Protocol, BaseURL: provider.BaseURL, APIKey: provider.APIKey,
				ServerCompaction: provider.ServerCompaction,
			}
		}
		if loaded.Models.Profiles == nil {
			loaded.Models.Profiles = make(map[string]publicmodel.ModelProfile, len(supplied.Profiles))
		}
		for name, profile := range supplied.Profiles {
			loaded.Models.Profiles[name] = profile
		}
		if supplied.DefaultProfile != "" {
			loaded.Models.DefaultProfile = supplied.DefaultProfile
		}
	}
	if maxParallelAgents > 0 {
		loaded.Runtime.MaxParallelAgents = maxParallelAgents
	}
	if loaded.Models.Providers == nil {
		loaded.Models.Providers = make(map[string]publicmodel.ProviderConfig)
	}
	for name, provider := range hostProviders {
		if provider == nil || strings.TrimSpace(name) == "" || provider.Name() != name {
			return Config{}, fmt.Errorf("host model provider %q has an invalid name", name)
		}
		configured, ok := loaded.Models.Providers[name]
		if !ok {
			configured = publicmodel.ProviderConfig{Protocol: publicmodel.ProtocolResponses, BaseURL: "https://host-provider.invalid"}
		}
		if configured.Protocol == "" {
			configured.Protocol = publicmodel.ProtocolResponses
		}
		if configured.BaseURL == "" {
			configured.BaseURL = "https://host-provider.invalid"
		}
		if configured.APIKey == "" {
			configured.APIKey = hostProviderPlaceholder
		}
		loaded.Models.Providers[name] = configured
	}
	loaded.applyDefaults()
	if err := loaded.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate runtime configuration: %w", err)
	}
	return loaded, nil
}
