package network

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
)

const Name = "kanedias"

type Client interface {
	GetNetwork(context.Context, string) (*api.Network, error)
	CreateNetwork(context.Context, api.NetworksPost) error
}

func Ensure(ctx context.Context, cfg config.Config) error {
	client, err := incusclient.Connect(ctx)
	if err != nil {
		return err
	}
	defer client.Disconnect()
	return EnsureWithClient(ctx, client, cfg)
}

func EnsureWithClient(ctx context.Context, client Client, cfg config.Config) error {
	ipv4, err := cfg.Network.IPv4Prefix()
	if err != nil {
		return err
	}
	ipv6, ipv6Present, err := cfg.Network.IPv6Prefix()
	if err != nil {
		return err
	}

	network, err := client.GetNetwork(ctx, Name)
	if incusclient.IsNotFound(err) {
		config := api.ConfigMap{"ipv4.address": ipv4.String()}
		if ipv6Present {
			config["ipv6.address"] = ipv6.String()
		}
		if err := client.CreateNetwork(ctx, api.NetworksPost{
			Name: Name,
			Type: "bridge",
			NetworkPut: api.NetworkPut{
				Config: config,
			},
		}); err != nil {
			return fmt.Errorf("create Incus network %q: %w", Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get Incus network %q: %w", Name, err)
	}

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
