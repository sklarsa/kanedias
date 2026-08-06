package workspace

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
	workspaceDevice = "workspace"
	workspacePath   = "/workspace"
	managedUser     = "kanedias"
	managedHome     = "/home/kanedias"
	cleanupTimeout  = 30 * time.Second
	systemdTimeout  = 60 * time.Second
	dnsTimeout      = 60 * time.Second
)

type client interface {
	Disconnect()
	ResolvePool(context.Context, string) (string, error)
	GetStorageVolume(context.Context, string, string) (*api.StorageVolume, error)
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
	Exec(context.Context, string, incusclient.ExecRequest) (string, string, error)
}

type dependencies struct {
	connect               func(context.Context) (client, error)
	initCA                func() error
	ensureNetwork         func(context.Context, client, config.Config) error
	renderProfile         func(io.Writer, string, config.Config) error
	readinessTimeout      time.Duration
	readinessPollInterval time.Duration
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
		ensureNetwork: func(ctx context.Context, client client, cfg config.Config) error {
			return network.EnsureWithClient(ctx, client, cfg)
		},
		renderProfile:         profiles.Render,
		readinessTimeout:      systemdTimeout,
		readinessPollInterval: time.Second,
	}
}

// Sync synchronizes configured GitHub repositories into the persistent seed volume.
func Sync(ctx context.Context, cfg config.Config, stdout, stderr io.Writer) error {
	return syncWithDependencies(ctx, cfg, stdout, stderr, defaultDependencies())
}

func syncWithDependencies(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, deps dependencies) (err error) {
	if err := cfg.ValidateLifecycle(); err != nil {
		return err
	}
	repositories, err := parseRepositories(cfg.Workspace.Repos)
	if err != nil {
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
	volume := cfg.Workspace.Volume
	if volume == "" {
		volume = config.DefaultWorkspaceVolume
	}
	if err := ensureSeedVolume(ctx, incus, pool, volume); err != nil {
		return err
	}
	if len(repositories) == 0 {
		fmt.Fprintln(stderr, "warning: no repositories configured; workspace seed volume is ready")
		return nil
	}

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

	name := fmt.Sprintf("workspace-sync-%d", time.Now().UnixNano())
	request := api.InstancesPost{
		Name: name,
		Source: api.InstanceSource{
			Type:  "image",
			Alias: cfg.BaseImage.Name,
		},
		InstancePut: api.InstancePut{
			Profiles: []string{"default", string(profiles.Sandbox)},
			Devices: api.DevicesMap{
				workspaceDevice: {
					"type":   "disk",
					"pool":   pool,
					"source": volume,
					"path":   workspacePath,
				},
			},
		},
	}
	if err := incus.CreateInstance(ctx, request); err != nil {
		return err
	}

	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		err = errors.Join(err, cleanupInstance(cleanupCtx, incus, name))
	}()

	if err := incus.StartInstance(ctx, name); err != nil {
		return err
	}
	if err := waitForSystemd(ctx, incus, name, deps.readinessTimeout, deps.readinessPollInterval); err != nil {
		return err
	}
	if err := exec(ctx, incus, name, stdout, stderr, []string{"update-ca-certificates"}); err != nil {
		return err
	}
	if err := waitForDNS(ctx, incus, name); err != nil {
		return err
	}
	if err := prepareRepositoryRoot(ctx, incus, name, stdout, stderr); err != nil {
		return err
	}
	if err := syncRepositories(ctx, incus, name, repositories, stdout, stderr); err != nil {
		return err
	}
	return nil
}

func ensureSeedVolume(ctx context.Context, incus client, pool, volume string) error {
	_, err := incus.GetStorageVolume(ctx, pool, volume)
	switch {
	case err == nil:
		return nil
	case incusclient.IsNotFound(err):
		return incus.CreateStorageVolume(ctx, pool, volume)
	default:
		return err
	}
}

func waitForSystemd(ctx context.Context, incus client, name string, timeout, pollInterval time.Duration) error {
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastState string
	var lastStderr string
	var lastErr error
	for {
		stdout, stderr, err := incus.Exec(readyCtx, name, incusclient.ExecRequest{
			Command: []string{"systemctl", "is-system-running", "--wait"},
		})
		state := strings.TrimSpace(stdout)
		if state == "running" || state == "degraded" {
			return nil
		}
		lastState = state
		lastStderr = strings.TrimSpace(stderr)
		lastErr = err

		if readyCtx.Err() != nil {
			return fmt.Errorf("wait for systemd in workspace instance %q: %w (last state %q, stderr %q, exec error %v)", name, readyCtx.Err(), lastState, lastStderr, lastErr)
		}

		timer := time.NewTimer(pollInterval)
		select {
		case <-readyCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return fmt.Errorf("wait for systemd in workspace instance %q: %w (last state %q, stderr %q, exec error %v)", name, readyCtx.Err(), lastState, lastStderr, lastErr)
		case <-timer.C:
		}
	}
}

func waitForDNS(ctx context.Context, incus client, name string) error {
	dnsCtx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()
	for {
		_, _, err := incus.Exec(dnsCtx, name, incusclient.ExecRequest{Command: []string{"getent", "ahosts", "github.com"}})
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if dnsCtx.Err() != nil {
			return fmt.Errorf("timed out waiting for DNS in Incus instance %q", name)
		}

		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-dnsCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return fmt.Errorf("timed out waiting for DNS in Incus instance %q", name)
		case <-timer.C:
		}
	}
}

func cleanupInstance(ctx context.Context, incus client, name string) error {
	var cleanupErrors []error
	if err := incus.StopInstance(ctx, name, true); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}

	instance, etag, err := incus.GetInstance(ctx, name)
	if err != nil {
		cleanupErrors = append(cleanupErrors, err)
	} else {
		request := instance.Writable()
		delete(request.Devices, workspaceDevice)
		if err := incus.UpdateInstance(ctx, name, request, etag); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	if err := incus.DeleteInstance(ctx, name); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	return errors.Join(cleanupErrors...)
}
