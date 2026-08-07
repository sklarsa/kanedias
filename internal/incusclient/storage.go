package incusclient

import (
	"context"
	"fmt"

	incus "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

const customVolumeType = "custom"

type storagePoolGetter interface {
	GetStoragePool(string) (*api.StoragePool, string, error)
}

func getStoragePool(server storagePoolGetter, name string) (*api.StoragePool, error) {
	pool, _, err := server.GetStoragePool(name)
	if err != nil {
		return nil, fmt.Errorf("get Incus storage pool %q: %w", name, err)
	}
	return pool, nil
}

func (c *Client) GetStoragePool(ctx context.Context, name string) (*api.StoragePool, error) {
	return getStoragePool(c.server.WithContext(ctx), name)
}

func (c *Client) GetStorageVolume(ctx context.Context, pool, name string) (*api.StorageVolume, error) {
	server := c.server.WithContext(ctx)
	volume, _, err := server.GetStoragePoolVolume(pool, customVolumeType, name)
	if err != nil {
		return nil, fmt.Errorf("get Incus storage volume %q in pool %q: %w", name, pool, err)
	}
	return volume, nil
}

func (c *Client) CreateStorageVolume(ctx context.Context, pool, name string) error {
	server := c.server.WithContext(ctx)
	err := server.CreateStoragePoolVolume(pool, api.StorageVolumesPost{
		Name: name,
		Type: customVolumeType,
	})
	if err != nil {
		return fmt.Errorf("create Incus storage volume %q in pool %q: %w", name, pool, err)
	}
	return nil
}

func (c *Client) CopyStorageVolume(ctx context.Context, pool, source, target string) error {
	server := c.server.WithContext(ctx)
	volume, _, err := server.GetStoragePoolVolume(pool, customVolumeType, source)
	if err != nil {
		return fmt.Errorf("get source Incus storage volume %q in pool %q: %w", source, pool, err)
	}

	if err := submitAndWaitRemoteOperation(ctx, func() (remoteOperationWaiter, error) {
		return server.CopyStoragePoolVolume(pool, server, pool, *volume, &incus.StoragePoolVolumeCopyArgs{
			Name: target,
			Mode: "pull",
		})
	}); err != nil {
		return fmt.Errorf("copy Incus storage volume %q to %q: %w", source, target, err)
	}
	return nil
}

func (c *Client) DeleteStorageVolume(ctx context.Context, pool, name string) error {
	server := c.server.WithContext(ctx)
	if err := server.DeleteStoragePoolVolume(pool, customVolumeType, name); err != nil {
		return fmt.Errorf("delete Incus storage volume %q in pool %q: %w", name, pool, err)
	}
	return nil
}
