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

type WorkerProfile struct {
	Description   string `toml:"description" json:"description"`
	Provider      string `toml:"provider" json:"provider"`
	Model         string `toml:"model" json:"model"`
	ThinkingLevel string `toml:"thinking_level" json:"thinkingLevel,omitempty"`
}

type Config struct {
	Network   Network                  `toml:"network"`
	BaseImage BaseImage                `toml:"base_image"`
	Workspace Workspace                `toml:"workspace"`
	Workers   map[string]WorkerProfile `toml:"workers"`
	Dir       string                   `toml:"-"`
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
}

type Workspace struct {
	Pool   string         `toml:"pool"`
	Volume string         `toml:"volume"`
	Repos  []string       `toml:"repos"`
	Incus  IncusWorkspace `toml:"incus"`
}

type IncusWorkspace struct {
	Volume string   `toml:"volume"`
	Images []string `toml:"images"`
}

const (
	DefaultWorkspaceVolume      = "kanedias-workspace-seed"
	DefaultIncusWorkspaceVolume = "kanedias-incus-seed"
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
	if cfg.Workspace.Incus.Volume == "" {
		cfg.Workspace.Incus.Volume = DefaultIncusWorkspaceVolume
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
	profile, ok := cfg.Workers[name]
	if !ok {
		return WorkerProfile{}, fmt.Errorf("unknown worker type %q", name)
	}
	return profile, nil
}

func (cfg Config) WorkerNames() []string {
	names := make([]string, 0, len(cfg.Workers))
	for name := range cfg.Workers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (cfg Config) ValidateSupervisor() error {
	if err := cfg.ValidateLifecycle(); err != nil {
		return err
	}
	if len(cfg.Workers) == 0 {
		return fmt.Errorf("at least one worker is required")
	}
	for _, name := range cfg.WorkerNames() {
		profile := cfg.Workers[name]
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("worker name is required")
		}
		if strings.TrimSpace(profile.Description) == "" {
			return fmt.Errorf("worker %q description is required", name)
		}
		if strings.TrimSpace(profile.Provider) == "" {
			return fmt.Errorf("worker %q provider is required", name)
		}
		if strings.TrimSpace(profile.Model) == "" {
			return fmt.Errorf("worker %q model is required", name)
		}
		if profile.ThinkingLevel != "" {
			if _, ok := validThinkingLevels[profile.ThinkingLevel]; !ok {
				return fmt.Errorf("worker %q has invalid thinking_level %q", name, profile.ThinkingLevel)
			}
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
	workspaceSeed := cfg.Workspace.Volume
	if workspaceSeed == "" {
		workspaceSeed = DefaultWorkspaceVolume
	}
	incusSeed := cfg.Workspace.Incus.Volume
	if incusSeed == "" {
		incusSeed = DefaultIncusWorkspaceVolume
	}
	if workspaceSeed == incusSeed {
		return fmt.Errorf("workspace.volume and workspace.incus.volume must be different")
	}
	return nil
}

func (cfg Config) AssetPath(name string) string {
	return filepath.Join(cfg.Dir, "assets", name)
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
