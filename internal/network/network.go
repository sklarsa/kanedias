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
		config := api.ConfigMap{
			"ipv4.address": ipv4.String(),
			"ipv4.nat":     "true",
		}
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
		return fmt.Errorf("network %q is not managed by Incus", Name)
	}
	if network.Type != "bridge" {
		return fmt.Errorf("unexpected type %q on Incus network %q; it must be a bridge", network.Type, Name)
	}
	if network.Config["ipv4.nat"] != "true" {
		return fmt.Errorf("ipv4.nat=true required for direct image-build egress on Incus network %q; run: incus network set %s ipv4.nat=true --project default", Name, Name)
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
		return fmt.Errorf("invalid %s %q on Incus network %q: %w", setting, actual, Name, err)
	}
	if actualPrefix != expected {
		return fmt.Errorf("unexpected %s %q on Incus network %q, expected %q", setting, actual, Name, expected.String())
	}
	return nil
}
