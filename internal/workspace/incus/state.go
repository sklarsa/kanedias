package incusworkspace

import (
	"context"
	"fmt"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
)

const (
	DeviceName   = "incus-state"
	MountPath    = "/var/lib/incus"
	volumePrefix = "kanedias-incus-"
)

type VolumeClient interface {
	GetStorageVolume(context.Context, string, string) (*api.StorageVolume, error)
	CopyStorageVolumeUntilTerminal(context.Context, string, string, string) error
	DeleteStorageVolume(context.Context, string, string) error
}

type CloneResult struct {
	Name    string
	Created bool
}

func SeedVolume(cfg config.Config) string {
	if cfg.Workspace.Incus.Volume == "" {
		return config.DefaultIncusWorkspaceVolume
	}
	return cfg.Workspace.Incus.Volume
}

func SandboxVolume(name string) string { return volumePrefix + name }

func Device(pool, volume string) map[string]string {
	return map[string]string{
		"type": "disk", "pool": pool, "source": volume, "path": MountPath,
	}
}

func Clone(ctx context.Context, client VolumeClient, pool, seed, sandbox string) (CloneResult, error) {
	result := CloneResult{Name: SandboxVolume(sandbox)}

	lock, err := acquireSeedLock(pool, seed, false)
	if err != nil {
		return result, err
	}
	defer func() { _ = lock.Close() }()

	seedVolume, err := client.GetStorageVolume(ctx, pool, seed)
	if err != nil {
		return result, fmt.Errorf("get nested Incus seed volume %q: %w", seed, err)
	}
	if len(seedVolume.UsedBy) != 0 {
		return result, fmt.Errorf("nested Incus seed %q is attached and cannot be cloned", seed)
	}

	err = client.CopyStorageVolumeUntilTerminal(ctx, pool, seed, result.Name)
	if err == nil || incusclient.OperationWasSubmitted(err) {
		result.Created = true
	}
	return result, err
}

func Delete(ctx context.Context, client VolumeClient, pool, seed, volume string) error {
	if volume == seed {
		return fmt.Errorf("refuse to delete nested Incus seed volume %q", seed)
	}
	return client.DeleteStorageVolume(ctx, pool, volume)
}
