package config

import (
	"fmt"
	"net/netip"
	"os"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	Network Network `toml:"network"`
}

type Network struct {
	IPv4 string `toml:"ipv4"`
	IPv6 string `toml:"ipv6"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	if _, err := cfg.Network.IPv4Prefix(); err != nil {
		return Config{}, err
	}
	if _, _, err := cfg.Network.IPv6Prefix(); err != nil {
		return Config{}, err
	}
	return cfg, nil
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
