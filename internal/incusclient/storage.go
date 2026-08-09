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

func (c *Client) GetStorageVolumeWithETag(ctx context.Context, pool, name string) (*api.StorageVolume, string, error) {
	volume, etag, err := c.server.WithContext(ctx).GetStoragePoolVolume(pool, customVolumeType, name)
	if err != nil {
		return nil, "", fmt.Errorf("get Incus storage volume %q in pool %q: %w", name, pool, err)
	}
	return volume, etag, nil
}

func (c *Client) UpdateStorageVolume(ctx context.Context, pool, name string, request api.StorageVolumePut, etag string) error {
	if err := c.server.WithContext(ctx).UpdateStoragePoolVolume(pool, customVolumeType, name, request, etag); err != nil {
		return fmt.Errorf("update Incus storage volume %q in pool %q: %w", name, pool, err)
	}
	return nil
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

func (c *Client) DeleteStorageVolume(ctx context.Context, pool, name string) error {
	server := c.server.WithContext(ctx)
	if err := server.DeleteStoragePoolVolume(pool, customVolumeType, name); err != nil && !IsNotFound(err) {
		return fmt.Errorf("delete Incus storage volume %q in pool %q: %w", name, pool, err)
	}
	return nil
}

type remoteOperationWaitStrategy func(context.Context, func() (remoteOperationWaiter, error)) error

func (c *Client) storageVolumeCopySubmission(ctx context.Context, pool, source, target string) (func() (remoteOperationWaiter, error), error) {
	server := c.server.WithContext(ctx)
	volume, _, err := server.GetStoragePoolVolume(pool, customVolumeType, source)
	if err != nil {
		return nil, fmt.Errorf("get source Incus storage volume %q in pool %q: %w", source, pool, err)
	}
	return func() (remoteOperationWaiter, error) {
		return server.CopyStoragePoolVolume(pool, server, pool, *volume, &incus.StoragePoolVolumeCopyArgs{
			Name: target,
			Mode: "pull",
		})
	}, nil
}

func (c *Client) copyStorageVolume(ctx context.Context, pool, source, target string, wait remoteOperationWaitStrategy) error {
	submit, err := c.storageVolumeCopySubmission(ctx, pool, source, target)
	if err != nil {
		return err
	}
	if err := wait(ctx, submit); err != nil {
		return fmt.Errorf("copy Incus storage volume %q to %q in pool %q: %w", source, target, pool, err)
	}
	return nil
}

// CopyStorageVolume returns promptly on caller cancellation while retaining the
// submitted operation for detached, bounded cleanup by supervisor callers.
func (c *Client) CopyStorageVolume(ctx context.Context, pool, source, target string) error {
	return c.copyStorageVolume(ctx, pool, source, target, submitAndWaitRemoteOperation)
}
