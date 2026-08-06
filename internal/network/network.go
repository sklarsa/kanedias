package network

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"

	"github.com/sklarsa/kanedias/internal/config"
)

const Name = "kanedias"

type runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

type incusRunner struct{}

func (incusRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, "incus", args...).CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("incus %s: %w (output: %q)", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

type incusNetwork struct {
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	Managed bool              `json:"managed"`
	Config  map[string]string `json:"config"`
}

func Ensure(ctx context.Context, cfg config.Config) error {
	return ensure(ctx, incusRunner{}, cfg)
}

func ensure(ctx context.Context, commandRunner runner, cfg config.Config) error {
	ipv4, err := cfg.Network.IPv4Prefix()
	if err != nil {
		return err
	}
	ipv6, ipv6Present, err := cfg.Network.IPv6Prefix()
	if err != nil {
		return err
	}

	output, err := commandRunner.Run(ctx, "network", "list", "name="+Name, "--format=json")
	if err != nil {
		return fmt.Errorf("list Incus network %q: %w", Name, err)
	}

	var listed []incusNetwork
	if err := json.Unmarshal(output, &listed); err != nil {
		return fmt.Errorf("decode Incus network list: %w", err)
	}

	matches := make([]incusNetwork, 0, 1)
	for _, network := range listed {
		if network.Name == Name {
			matches = append(matches, network)
		}
	}

	switch len(matches) {
	case 0:
		args := []string{
			"network", "create", Name, "--type=bridge",
			"ipv4.address=" + ipv4.String(),
		}
		if ipv6Present {
			args = append(args, "ipv6.address="+ipv6.String())
		}
		if _, err := commandRunner.Run(ctx, args...); err != nil {
			return fmt.Errorf("create Incus network %q: %w", Name, err)
		}
		return nil
	case 1:
		// Reconcile the single existing network below.
	default:
		return fmt.Errorf("Incus returned multiple networks named %q", Name)
	}

	network := matches[0]
	if !network.Managed {
		return fmt.Errorf("Incus network %q is not managed", Name)
	}
	if network.Type != "bridge" {
		return fmt.Errorf("Incus network %q has type %q; it must be a bridge", Name, network.Type)
	}
	if err := requirePrefix("ipv4.address", network.Config["ipv4.address"], ipv4); err != nil {
		return err
	}
	if ipv6Present {
		if err := requirePrefix("ipv6.address", network.Config["ipv6.address"], ipv6); err != nil {
			return err
		}
	}
	return nil
}

func requirePrefix(setting, actual string, expected netip.Prefix) error {
	actualPrefix, err := netip.ParsePrefix(actual)
	if err != nil {
		return fmt.Errorf("Incus network %q has invalid %s %q: %w", Name, setting, actual, err)
	}
	if actualPrefix != expected {
		return fmt.Errorf("Incus network %q has %s %q, expected %q", Name, setting, actual, expected.String())
	}
	return nil
}
