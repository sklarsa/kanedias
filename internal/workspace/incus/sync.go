package incusworkspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
	"github.com/sklarsa/kanedias/internal/network"
	"github.com/sklarsa/kanedias/internal/profiles"
	"github.com/sklarsa/kanedias/internal/proxy"
)

const (
	maintenanceDevice = DeviceName
	cleanupTimeout    = 30 * time.Second
	systemdTimeout    = 60 * time.Second
	dnsTimeout        = 60 * time.Second
)

type client interface {
	VolumeClient
	Disconnect()
	ResolvePool(context.Context, string) (string, error)
	GetStoragePool(context.Context, string) (*api.StoragePool, error)
	CreateStorageVolume(context.Context, string, string) error
	GetNetwork(context.Context, string) (*api.Network, error)
	CreateNetwork(context.Context, api.NetworksPost) error
	EnsureProfile(context.Context, string, []byte) error
	CreateInstance(context.Context, api.InstancesPost) error
	StartInstance(context.Context, string) error
	StopInstance(context.Context, string, bool) error
	GetInstance(context.Context, string) (*api.Instance, string, error)
	UpdateInstance(context.Context, string, api.InstancePut, string) error
	DeleteInstance(context.Context, string) error
	Executor
}

type dependencies struct {
	connect               func(context.Context) (client, error)
	initCA                func() error
	ensureNetwork         func(context.Context, client, config.Config) error
	renderProfile         func(io.Writer, string, config.Config) error
	newInstanceName       func() string
	operationWasSubmitted func(error) bool
}

func defaultDependencies() dependencies {
	return dependencies{
		connect: func(ctx context.Context) (client, error) {
			return incusclient.Connect(ctx)
		},
		initCA: func() error {
			options, err := proxy.DefaultOptions()
			if err != nil {
				return err
			}
			return proxy.InitCA(options.CACertPath, options.CAKeyPath)
		},
		ensureNetwork: func(ctx context.Context, incus client, cfg config.Config) error {
			return network.EnsureWithClient(ctx, incus, cfg)
		},
		renderProfile: profiles.Render,
		newInstanceName: func() string {
			return fmt.Sprintf("workspace-incus-sync-%d", time.Now().UnixNano())
		},
		operationWasSubmitted: incusclient.OperationWasSubmitted,
	}
}

// Sync refreshes the cold storage-volume seed used by native nested Incus workspaces.
func Sync(ctx context.Context, cfg config.Config, stdout, stderr io.Writer) error {
	return syncWithDependencies(ctx, cfg, stdout, stderr, defaultDependencies())
}

func syncWithDependencies(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, deps dependencies) (err error) {
	if err := cfg.ValidateLifecycle(); err != nil {
		return err
	}

	incus, err := deps.connect(ctx)
	if err != nil {
		return err
	}
	defer incus.Disconnect()

	pool, err := incus.ResolvePool(ctx, cfg.Workspace.Pool)
	if err != nil {
		return err
	}
	storagePool, err := incus.GetStoragePool(ctx, pool)
	if err != nil {
		return err
	}
	if storagePool.Driver != "btrfs" {
		return fmt.Errorf("outer Incus storage pool %q uses %q, want btrfs", pool, storagePool.Driver)
	}

	seed := SeedVolume(cfg)
	lock, err := acquireSeedLock(pool, seed, true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()

	seedCreated := false
	volume, err := incus.GetStorageVolume(ctx, pool, seed)
	switch {
	case err == nil:
		if len(volume.UsedBy) != 0 {
			return fmt.Errorf("nested Incus seed %q is attached and cannot be refreshed", seed)
		}
	case incusclient.IsNotFound(err):
		if err := incus.CreateStorageVolume(ctx, pool, seed); err != nil {
			return err
		}
		seedCreated = true
	default:
		return err
	}

	name := ""
	instanceCreated := false
	instanceRunning := false
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()

		cleanupErr := cleanupMaintenanceInstance(cleanupCtx, incus, name, instanceCreated, instanceRunning)
		result := errors.Join(err, cleanupErr)
		if result != nil && seedCreated {
			result = errors.Join(result, incus.DeleteStorageVolume(cleanupCtx, pool, seed))
		}
		err = result
	}()

	if err := deps.initCA(); err != nil {
		return fmt.Errorf("initialize proxy CA: %w", err)
	}
	if err := deps.ensureNetwork(ctx, incus, cfg); err != nil {
		return err
	}
	var profile bytes.Buffer
	if err := deps.renderProfile(&profile, string(profiles.Sandbox), cfg); err != nil {
		return err
	}
	if err := incus.EnsureProfile(ctx, string(profiles.Sandbox), profile.Bytes()); err != nil {
		return err
	}

	name = deps.newInstanceName()
	request := api.InstancesPost{
		Name: name,
		Source: api.InstanceSource{
			Type:  "image",
			Alias: cfg.BaseImage.Name,
		},
		InstancePut: api.InstancePut{
			Profiles: []string{"default", string(profiles.Sandbox)},
			Devices: api.DevicesMap{
				"root": {
					"type": "disk",
					"pool": pool,
					"path": "/",
				},
				maintenanceDevice: Device(pool, seed),
			},
		},
	}
	if createErr := incus.CreateInstance(ctx, request); createErr != nil {
		instanceCreated = deps.operationWasSubmitted(createErr)
		return createErr
	}
	instanceCreated = true

	if startErr := incus.StartInstance(ctx, name); startErr != nil {
		instanceRunning = deps.operationWasSubmitted(startErr)
		return startErr
	}
	instanceRunning = true

	if err := waitForSystemd(ctx, incus, name); err != nil {
		return err
	}
	if err := execAndWrite(ctx, incus, name, stdout, stderr, []string{"update-ca-certificates"}); err != nil {
		return err
	}
	if err := waitForDNS(ctx, incus, name, dnsTimeout, time.Second); err != nil {
		return err
	}
	if err := initialize(ctx, incus, name, seedCreated, systemdTimeout); err != nil {
		return err
	}
	if err := syncImages(ctx, incus, name, cfg.Workspace.Incus.Images); err != nil {
		return err
	}
	if err := quiesce(ctx, incus, name); err != nil {
		return err
	}
	if err := incus.StopInstance(ctx, name, false); err != nil {
		return err
	}
	instanceRunning = false

	if err := detachAndDeleteInstance(ctx, incus, name); err != nil {
		return err
	}
	instanceCreated = false
	return nil
}

func waitForSystemd(ctx context.Context, incus client, name string) error {
	readyCtx, cancel := context.WithTimeout(ctx, systemdTimeout)
	defer cancel()

	stdout, stderr, err := incus.Exec(readyCtx, name, incusclient.ExecRequest{
		Command: []string{"systemctl", "is-system-running", "--wait"},
	})
	state := strings.TrimSpace(stdout)
	if err != nil {
		return fmt.Errorf("wait for systemd in maintenance instance %q (state %q, stderr %q): %w", name, state, strings.TrimSpace(stderr), err)
	}
	if state != "running" && state != "degraded" {
		return fmt.Errorf("wait for systemd in maintenance instance %q: state is %q", name, state)
	}
	return nil
}

func waitForDNS(ctx context.Context, incus client, name string, timeout, pollInterval time.Duration) error {
	dnsCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastStderr string
	var lastErr error
	for {
		if err := dnsCtx.Err(); err != nil {
			return fmt.Errorf("wait for DNS in maintenance instance %q (last stderr %q, last error %v): %w", name, lastStderr, lastErr, err)
		}
		_, stderr, err := incus.Exec(dnsCtx, name, incusclient.ExecRequest{
			Command: []string{"getent", "ahosts", "images.linuxcontainers.org"},
		})
		if err == nil {
			return nil
		}
		lastStderr = strings.TrimSpace(stderr)
		lastErr = err

		timer := time.NewTimer(pollInterval)
		select {
		case <-dnsCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return fmt.Errorf("wait for DNS in maintenance instance %q (last stderr %q, last error %v): %w", name, lastStderr, lastErr, dnsCtx.Err())
		case <-timer.C:
		}
	}
}

func execAndWrite(ctx context.Context, incus client, name string, stdout, stderr io.Writer, command []string) error {
	commandStdout, commandStderr, err := incus.Exec(ctx, name, incusclient.ExecRequest{Command: command})
	if commandStdout != "" {
		_, _ = io.WriteString(stdout, commandStdout)
	}
	if commandStderr != "" {
		_, _ = io.WriteString(stderr, commandStderr)
	}
	if err != nil {
		return fmt.Errorf("run %q in maintenance instance %q: %w", strings.Join(command, " "), name, err)
	}
	return nil
}

func cleanupMaintenanceInstance(ctx context.Context, incus client, name string, created, running bool) error {
	if !created {
		return nil
	}

	var cleanupErrors []error
	if running {
		if err := quiesce(ctx, incus, name); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	if err := incus.StopInstance(ctx, name, true); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if err := detachAndDeleteInstance(ctx, incus, name); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	return errors.Join(cleanupErrors...)
}

func detachAndDeleteInstance(ctx context.Context, incus client, name string) error {
	var cleanupErrors []error
	instance, etag, err := incus.GetInstance(ctx, name)
	if err != nil {
		cleanupErrors = append(cleanupErrors, err)
	} else {
		request := instance.Writable()
		delete(request.Devices, maintenanceDevice)
		if err := incus.UpdateInstance(ctx, name, request, etag); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	if err := incus.DeleteInstance(ctx, name); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	return errors.Join(cleanupErrors...)
}
