package provision

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
)

type directChildRecoveryClient interface {
	GetInstance(context.Context, string) (*api.Instance, string, error)
	GetStorageVolumeWithETag(context.Context, string, string) (*api.StorageVolume, string, error)
	StopInstance(context.Context, string, bool) error
	DeleteInstance(context.Context, string) error
	DeleteStorageVolume(context.Context, string, string) error
}

type IncusDirectChildRecoverer struct {
	client      directChildRecoveryClient
	trustedPool string
}

type ConfiguredDirectChildRecoverer struct {
	*IncusDirectChildRecoverer
	client *incusclient.Client
}

func NewConfiguredDirectChildRecoverer(ctx context.Context, cfg config.Config) (*ConfiguredDirectChildRecoverer, error) {
	client, err := incusclient.Connect(ctx)
	if err != nil {
		return nil, err
	}
	pool, err := client.ResolvePool(ctx, cfg.Workspace.Pool)
	if err != nil {
		client.Disconnect()
		return nil, err
	}
	return &ConfiguredDirectChildRecoverer{IncusDirectChildRecoverer: &IncusDirectChildRecoverer{client: client, trustedPool: pool}, client: client}, nil
}

func (recoverer *ConfiguredDirectChildRecoverer) Close() {
	if recoverer != nil && recoverer.client != nil {
		recoverer.client.Disconnect()
	}
}

func (recoverer *IncusDirectChildRecoverer) RecoverDirectChild(ctx context.Context, ticket RecoveryTicket) error {
	if recoverer == nil || recoverer.client == nil {
		return fmt.Errorf("direct-child recovery client is unavailable")
	}
	if err := validateRecoveryTicket(ticket, recoverer.trustedPool); err != nil {
		return err
	}

	instance, _, instanceErr := recoverer.client.GetInstance(ctx, ticket.Instance)
	instanceAbsent := incusclient.IsNotFound(instanceErr)
	if instanceErr != nil && !instanceAbsent {
		return fmt.Errorf("probe recovery instance %q: %w", ticket.Instance, instanceErr)
	}
	volume, _, volumeErr := recoverer.client.GetStorageVolumeWithETag(ctx, ticket.Pool, ticket.Volume)
	volumeAbsent := incusclient.IsNotFound(volumeErr)
	if volumeErr != nil && !volumeAbsent {
		return fmt.Errorf("probe recovery volume %q in pool %q: %w", ticket.Volume, ticket.Pool, volumeErr)
	}
	if !instanceAbsent {
		if instance == nil {
			return fmt.Errorf("probe recovery instance %q returned nil", ticket.Instance)
		}
		if err := verifyRecoveryMetadata("instance", instance.Name, instance.Config, ticket); err != nil {
			return err
		}
		rootPool, err := incusclient.EffectiveRootPool(instance)
		if err != nil || rootPool != ticket.Pool {
			return fmt.Errorf("refuse recovery of instance %q: root pool %q does not match ticket pool %q: %w", ticket.Instance, rootPool, ticket.Pool, err)
		}
	}
	if !volumeAbsent {
		if volume == nil {
			return fmt.Errorf("probe recovery volume %q returned nil", ticket.Volume)
		}
		if err := verifyRecoveryMetadata("volume", volume.Name, volume.Config, ticket); err != nil {
			return err
		}
	}
	if err := verifyRecoverySocket(ticket); err != nil {
		return err
	}

	var result error
	if !instanceAbsent {
		if instance.IsActive() {
			if err := recoverer.client.StopInstance(ctx, ticket.Instance, true); err != nil && !incusclient.IsNotFound(err) {
				return fmt.Errorf("stop exact recovery instance %q: %w", ticket.Instance, err)
			}
		}
		if err := recoverer.client.DeleteInstance(ctx, ticket.Instance); err != nil && !incusclient.IsNotFound(err) {
			result = errors.Join(result, fmt.Errorf("delete exact recovery instance %q: %w", ticket.Instance, err))
		}
	}
	if !volumeAbsent {
		if err := recoverer.client.DeleteStorageVolume(ctx, ticket.Pool, ticket.Volume); err != nil && !incusclient.IsNotFound(err) {
			result = errors.Join(result, fmt.Errorf("delete exact recovery volume %q in pool %q: %w", ticket.Volume, ticket.Pool, err))
		}
	}
	result = errors.Join(result, unlinkRecoverySocket(ticket))
	return result
}

func validateRecoveryTicket(ticket RecoveryTicket, trustedPool string) error {
	for name, value := range map[string]string{
		"session": ticket.SessionID, "parent": ticket.ParentID, "root": ticket.RootID,
		"pool": ticket.Pool, "instance": ticket.Instance, "volume": ticket.Volume,
		"socket": ticket.SocketPath, "worker": ticket.WorkerType,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("direct-child recovery ticket %s is required", name)
		}
	}
	if ticket.Pool != trustedPool {
		return fmt.Errorf("refuse direct-child recovery pool %q; configured pool is %q", ticket.Pool, trustedPool)
	}
	if ticket.Instance != "session-"+ticket.SessionID || ticket.Volume != "workspace-"+ticket.SessionID {
		return fmt.Errorf("refuse non-deterministic direct-child recovery names instance=%q volume=%q session=%q", ticket.Instance, ticket.Volume, ticket.SessionID)
	}
	if !filepath.IsAbs(ticket.SocketPath) || filepath.Clean(ticket.SocketPath) != ticket.SocketPath || ticket.Socket.Device == 0 || ticket.Socket.Inode == 0 {
		return fmt.Errorf("direct-child recovery socket identity is invalid for %q", ticket.SocketPath)
	}
	return nil
}

func verifyRecoveryMetadata(resourceKind, name string, metadata map[string]string, ticket RecoveryTicket) error {
	want := map[string]string{
		metaSessionID: ticket.SessionID, metaParentID: ticket.ParentID, metaRootID: ticket.RootID,
		metaKind: string(ticket.Kind), metaContext: string(ticket.Context), metaWorker: ticket.WorkerType,
		metaVolume: ticket.Volume,
	}
	if ticket.RunAttribution != "" {
		want[metaRun] = ticket.RunAttribution
	}
	for key, expected := range want {
		if metadata[key] != expected {
			return fmt.Errorf("refuse recovery of %s %q: metadata %s=%q, expected %q (session=%q parent=%q root=%q pool=%q)", resourceKind, name, key, metadata[key], expected, ticket.SessionID, ticket.ParentID, ticket.RootID, ticket.Pool)
		}
	}
	return nil
}

func verifyRecoverySocket(ticket RecoveryTicket) error {
	info, err := os.Lstat(ticket.SocketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect exact recovery socket %q: %w", ticket.SocketPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refuse unlink of replaced recovery socket %q", ticket.SocketPath)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(stat.Dev) != ticket.Socket.Device || stat.Ino != ticket.Socket.Inode {
		return fmt.Errorf("refuse unlink of replacement recovery socket %q (ticket dev=%d ino=%d)", ticket.SocketPath, ticket.Socket.Device, ticket.Socket.Inode)
	}
	return nil
}

func unlinkRecoverySocket(ticket RecoveryTicket) error {
	if err := verifyRecoverySocket(ticket); err != nil {
		return err
	}
	if err := os.Remove(ticket.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("unlink exact recovery socket %q: %w", ticket.SocketPath, err)
	}
	return nil
}

var _ DirectChildRecoverer = (*IncusDirectChildRecoverer)(nil)
