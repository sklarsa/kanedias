package config

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	Network    Network                    `toml:"network"`
	BaseImage  BaseImage                  `toml:"base_image"`
	Workspace  Workspace                  `toml:"workspace"`
	Models     map[string]ModelDefinition `toml:"models"`
	Session    SessionDefaults            `toml:"session"`
	Workers    map[string]WorkerDefaults  `toml:"workers"`
	Server     ServerConfig               `toml:"server"`
	Supervisor SupervisorConfig           `toml:"supervisor"`
	Dir        string                     `toml:"-"`
}

var validThinkingLevels = map[string]struct{}{
	"off": {}, "minimal": {}, "low": {}, "medium": {},
	"high": {}, "xhigh": {}, "max": {},
}

type BaseImage struct {
	Name            string   `toml:"name"`
	Source          string   `toml:"source"`
	Image           string   `toml:"image"`
	AuthorizedHosts []string `toml:"authorized_hosts"`
	BuildScriptsDir string   `toml:"build_scripts_dir"`
}

type Workspace struct {
	Pool   string   `toml:"pool"`
	Volume string   `toml:"volume"`
	Repos  []string `toml:"repos"`
}

const (
	DefaultWorkspaceVolume = "kanedias-workspace-seed"
)

type Network struct {
	IPv4 string `toml:"ipv4"`
	IPv6 string `toml:"ipv6"`
}

func Load(path string) (Config, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return Config{}, fmt.Errorf("resolve config path %q: %w", path, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	cfg.Dir = filepath.Dir(absPath)
	if cfg.Workspace.Volume == "" {
		cfg.Workspace.Volume = DefaultWorkspaceVolume
	}
	if _, err := cfg.Network.IPv4Prefix(); err != nil {
		return Config{}, err
	}
	if _, _, err := cfg.Network.IPv6Prefix(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (cfg Config) ResolveWorker(name string) (WorkerProfile, error) {
	defaults, ok := cfg.Workers[name]
	if !ok {
		return WorkerProfile{}, fmt.Errorf("unknown worker type %q", name)
	}
	profile, err := cfg.ResolveModel(defaults.ModelType, defaults.ThinkingLevel)
	if err != nil {
		return WorkerProfile{}, fmt.Errorf("worker %q: %w", name, err)
	}
	return WorkerProfile{
		Description:   defaults.Description,
		Provider:      profile.Provider,
		Model:         profile.Model,
		ThinkingLevel: profile.ThinkingLevel,
	}, nil
}

func (cfg Config) WorkerNames() []string {
	names := make([]string, 0, len(cfg.Workers))
	for name := range cfg.Workers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ValidateChildRuntime validates only configuration that remains authoritative
// for an already-admitted descendant. Model catalog, session, and worker
// defaults are deliberately excluded because descendants inherit their complete
// resolved model policy from the parent bootstrap.
func (cfg Config) ValidateChildRuntime() error {
	if err := cfg.ValidateLifecycle(); err != nil {
		return err
	}
	if _, err := cfg.Supervisor.Events.Limits(); err != nil {
		return err
	}
	return nil
}

func (cfg Config) ValidateSupervisor() error {
	if err := cfg.ValidateChildRuntime(); err != nil {
		return err
	}
	if err := cfg.validateModelCatalog(); err != nil {
		return err
	}
	if _, err := cfg.DefaultSessionModelPolicy(); err != nil {
		return err
	}
	return nil
}

// validateModelCatalog checks the structural invariants of the model catalog:
// slug-shaped model type IDs, nonempty provider/model, unique provider/model
// pairs, a nonempty set of valid, non-duplicate thinking levels, and a default
// thinking level that belongs to the supported set.
func (cfg Config) validateModelCatalog() error {
	if len(cfg.Models) == 0 {
		return fmt.Errorf("at least one model is required")
	}
	seenPairs := make(map[string]string, len(cfg.Models))
	modelTypes := make([]string, 0, len(cfg.Models))
	for modelType := range cfg.Models {
		modelTypes = append(modelTypes, modelType)
	}
	sort.Strings(modelTypes)
	for _, modelType := range modelTypes {
		def := cfg.Models[modelType]
		if !modelTypeSlugPattern.MatchString(modelType) {
			return fmt.Errorf("model type %q has invalid name (want ^[a-z0-9][a-z0-9-]{0,62}$)", modelType)
		}
		if strings.TrimSpace(def.Provider) == "" {
			return fmt.Errorf("model %q provider is required", modelType)
		}
		if strings.TrimSpace(def.Model) == "" {
			return fmt.Errorf("model %q model is required", modelType)
		}
		pair := def.Provider + "/" + def.Model
		if previous, dup := seenPairs[pair]; dup {
			return fmt.Errorf("duplicate provider/model %q across model types %q and %q", pair, previous, modelType)
		}
		seenPairs[pair] = modelType
		if len(def.ThinkingLevels) == 0 {
			return fmt.Errorf("model %q requires at least one thinking level", modelType)
		}
		seenLevels := make(map[string]struct{}, len(def.ThinkingLevels))
		for _, level := range def.ThinkingLevels {
			if !validThinkingLevel(level) {
				return fmt.Errorf("model %q has invalid thinking level %q", modelType, level)
			}
			if _, dup := seenLevels[level]; dup {
				return fmt.Errorf("model %q has duplicate thinking level %q", modelType, level)
			}
			seenLevels[level] = struct{}{}
		}
		if def.DefaultThinkingLevel == "" {
			return fmt.Errorf("model %q default_thinking_level is required", modelType)
		}
		if !contains(def.ThinkingLevels, def.DefaultThinkingLevel) {
			return fmt.Errorf("model %q default thinking level %q is not in thinking_levels", modelType, def.DefaultThinkingLevel)
		}
	}
	return nil
}

func (cfg Config) ValidateLifecycle() error {
	if cfg.BaseImage.Name == "" {
		return fmt.Errorf("base_image.name is required")
	}
	if cfg.BaseImage.Source == "" {
		return fmt.Errorf("base_image.source is required")
	}
	if cfg.BaseImage.Image == "" {
		return fmt.Errorf("base_image.image is required")
	}
	return nil
}

func (cfg Config) AssetPath(name string) string {
	return filepath.Join(cfg.Dir, "assets", name)
}

func (cfg Config) BuildScriptsPath() string {
	if cfg.BaseImage.BuildScriptsDir == "" {
		return ""
	}
	if filepath.IsAbs(cfg.BaseImage.BuildScriptsDir) {
		return filepath.Clean(cfg.BaseImage.BuildScriptsDir)
	}
	return filepath.Join(cfg.Dir, cfg.BaseImage.BuildScriptsDir)
}

func (network Network) IPv4Prefix() (netip.Prefix, error) {
	if network.IPv4 == "" {
		return netip.Prefix{}, fmt.Errorf("network.ipv4 is required")
	}

	prefix, err := netip.ParsePrefix(network.IPv4)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("network.ipv4: %w", err)
	}
	if !prefix.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("network.ipv4 must be IPv4")
	}
	return prefix, nil
}

func (network Network) IPv6Prefix() (netip.Prefix, bool, error) {
	if network.IPv6 == "" {
		return netip.Prefix{}, false, nil
	}

	prefix, err := netip.ParsePrefix(network.IPv6)
	if err != nil {
		return netip.Prefix{}, false, fmt.Errorf("network.ipv6: %w", err)
	}
	if !prefix.Addr().Is6() {
		return netip.Prefix{}, false, fmt.Errorf("network.ipv6 must be IPv6")
	}
	return prefix, true, nil
}
