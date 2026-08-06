package sandbox

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
	sandboxProfile      = "sandbox"
	workspaceDevice     = "workspace"
	workspacePath       = "/workspace"
	workspaceNamePrefix = "kanedias-workspace-"
)

type lifecycleClient interface {
	Disconnect()
	ResolvePool(context.Context, string) (string, error)
	GetNetwork(context.Context, string) (*api.Network, error)
	CreateNetwork(context.Context, api.NetworksPost) error
	EnsureProfile(context.Context, string, []byte) error
	GetStorageVolume(context.Context, string, string) (*api.StorageVolume, error)
	CopyStorageVolume(context.Context, string, string, string) error
	DeleteStorageVolume(context.Context, string, string) error
	GetInstance(context.Context, string) (*api.Instance, string, error)
	CreateInstance(context.Context, api.InstancesPost) error
	StartInstance(context.Context, string) error
	StopInstance(context.Context, string, bool) error
	DeleteInstance(context.Context, string) error
	Exec(context.Context, string, incusclient.ExecRequest) (string, string, error)
}

type dependencies struct {
	connect               func(context.Context) (lifecycleClient, error)
	acquireLock           func(string) (io.Closer, error)
	defaultProxyOptions   func() (proxy.Options, error)
	initCA                func(string, string) error
	ensureNetwork         func(context.Context, lifecycleClient, config.Config) error
	readinessTimeout      time.Duration
	readinessPollInterval time.Duration
}

func defaultDependencies() dependencies {
	return dependencies{
		connect: func(ctx context.Context) (lifecycleClient, error) {
			return incusclient.Connect(ctx)
		},
		acquireLock:         acquireLifecycleLock,
		defaultProxyOptions: proxy.DefaultOptions,
		initCA:              proxy.InitCA,
		ensureNetwork: func(ctx context.Context, client lifecycleClient, cfg config.Config) error {
			return network.EnsureWithClient(ctx, client, cfg)
		},
		readinessTimeout:      60 * time.Second,
		readinessPollInterval: time.Second,
	}
}

// Create clones a sandbox workspace and launches an instance from the configured local base image.
func Create(ctx context.Context, cfg config.Config, name string, stdout, stderr io.Writer) error {
	return create(ctx, cfg, name, stdout, stderr, defaultDependencies())
}

func create(ctx context.Context, cfg config.Config, name string, stdout, stderr io.Writer, deps dependencies) (err error) {
	if err := cfg.ValidateLifecycle(); err != nil {
		return err
	}
	if err := validateName(name); err != nil {
		return err
	}

	client, err := deps.connect(ctx)
	if err != nil {
		return err
	}
	defer client.Disconnect()

	pool, err := client.ResolvePool(ctx, cfg.Workspace.Pool)
	if err != nil {
		return err
	}
	lock, err := deps.acquireLock(name)
	if err != nil {
		return err
	}
	defer lock.Close()

	options, err := deps.defaultProxyOptions()
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Initializing proxy CA...")
	if err := deps.initCA(options.CACertPath, options.CAKeyPath); err != nil {
		return fmt.Errorf("initialize proxy CA: %w", err)
	}

	fmt.Fprintln(stdout, "Ensuring sandbox network...")
	if err := deps.ensureNetwork(ctx, client, cfg); err != nil {
		return err
	}

	var profile bytes.Buffer
	if err := profiles.Render(&profile, sandboxProfile, cfg); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Ensuring sandbox profile...")
	if err := client.EnsureProfile(ctx, sandboxProfile, profile.Bytes()); err != nil {
		return err
	}

	seed := seedVolume(cfg)
	if _, err := client.GetStorageVolume(ctx, pool, seed); err != nil {
		return fmt.Errorf("get workspace seed volume: %w", err)
	}

	volume := workspaceVolume(name)
	volumeCreated := false
	instanceCreated := false
	instanceRunning := false
	defer func() {
		if err == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		var cleanupErr error
		if instanceCreated {
			if instanceRunning {
				fmt.Fprintf(stderr, "Stopping failed sandbox %s...\n", name)
				if stopErr := client.StopInstance(cleanupCtx, name, true); stopErr != nil && !incusclient.IsNotFound(stopErr) {
					cleanupErr = errors.Join(cleanupErr, stopErr)
				}
			}
			fmt.Fprintf(stderr, "Deleting failed sandbox %s...\n", name)
			if deleteErr := client.DeleteInstance(cleanupCtx, name); deleteErr != nil && !incusclient.IsNotFound(deleteErr) {
				cleanupErr = errors.Join(cleanupErr, deleteErr)
			}
		}
		if volumeCreated {
			fmt.Fprintf(stderr, "Deleting failed workspace %s...\n", volume)
			if deleteErr := client.DeleteStorageVolume(cleanupCtx, pool, volume); deleteErr != nil && !incusclient.IsNotFound(deleteErr) {
				cleanupErr = errors.Join(cleanupErr, deleteErr)
			}
		}
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("clean up failed sandbox: %w", cleanupErr))
		}
	}()

	fmt.Fprintf(stdout, "Cloning workspace %s to %s...\n", seed, volume)
	if err := client.CopyStorageVolume(ctx, pool, seed, volume); err != nil {
		return err
	}
	volumeCreated = true

	request := api.InstancesPost{
		Name: name,
		InstancePut: api.InstancePut{
			Profiles: []string{"default", sandboxProfile},
			Devices: api.DevicesMap{
				workspaceDevice: {
					"type":   "disk",
					"pool":   pool,
					"source": volume,
					"path":   workspacePath,
				},
			},
		},
		Source: api.InstanceSource{Type: "image", Alias: cfg.BaseImage.Name},
	}
	fmt.Fprintf(stdout, "Creating sandbox %s...\n", name)
	if err := client.CreateInstance(ctx, request); err != nil {
		return err
	}
	instanceCreated = true

	fmt.Fprintf(stdout, "Starting sandbox %s...\n", name)
	if err := client.StartInstance(ctx, name); err != nil {
		return err
	}
	instanceRunning = true

	fmt.Fprintf(stdout, "Waiting for systemd in %s...\n", name)
	if err := waitForSystemd(ctx, client, name, deps.readinessTimeout, deps.readinessPollInterval); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "Updating trusted CA certificates in %s...\n", name)
	if _, _, err := client.Exec(ctx, name, incusclient.ExecRequest{Command: []string{"update-ca-certificates"}}); err != nil {
		return fmt.Errorf("update trusted CA certificates in sandbox %q: %w", name, err)
	}
	fmt.Fprintf(stdout, "Sandbox %s is ready.\n", name)
	return nil
}

func waitForSystemd(ctx context.Context, client lifecycleClient, name string, timeout, pollInterval time.Duration) error {
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastState string
	var lastStderr string
	var lastErr error
	for {
		stdout, stderr, err := client.Exec(readyCtx, name, incusclient.ExecRequest{
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
			return fmt.Errorf("wait for systemd in sandbox %q: %w (last state %q, stderr %q, exec error %v)", name, readyCtx.Err(), lastState, lastStderr, lastErr)
		}

		timer := time.NewTimer(pollInterval)
		select {
		case <-readyCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return fmt.Errorf("wait for systemd in sandbox %q: %w (last state %q, stderr %q, exec error %v)", name, readyCtx.Err(), lastState, lastStderr, lastErr)
		case <-timer.C:
		}
	}
}

// Destroy removes a sandbox instance and its verified, sandbox-owned workspace volume.
func Destroy(ctx context.Context, cfg config.Config, name string, stdout, stderr io.Writer) error {
	return destroy(ctx, cfg, name, stdout, stderr, defaultDependencies())
}

func destroy(ctx context.Context, cfg config.Config, name string, stdout, _ io.Writer, deps dependencies) error {
	if err := cfg.ValidateLifecycle(); err != nil {
		return err
	}
	if err := validateName(name); err != nil {
		return err
	}
	volume := workspaceVolume(name)
	if volume == seedVolume(cfg) {
		return fmt.Errorf("refusing to remove protected workspace volume %q", volume)
	}

	client, err := deps.connect(ctx)
	if err != nil {
		return err
	}
	defer client.Disconnect()

	pool, err := client.ResolvePool(ctx, cfg.Workspace.Pool)
	if err != nil {
		return err
	}
	lock, err := deps.acquireLock(name)
	if err != nil {
		return err
	}
	defer lock.Close()

	instance, _, err := client.GetInstance(ctx, name)
	instanceExists := err == nil
	if err != nil && !incusclient.IsNotFound(err) {
		return err
	}
	if instanceExists {
		workspace, ok := instance.Devices[workspaceDevice]
		if !ok || workspace["source"] == "" {
			return fmt.Errorf("refusing to remove %q: local workspace device is missing", name)
		}
		if workspace["source"] != volume {
			return fmt.Errorf("refusing to remove %q: workspace source is %q, expected %q", name, workspace["source"], volume)
		}
		if instance.StatusCode == api.Running {
			fmt.Fprintf(stdout, "Stopping sandbox %s...\n", name)
			if err := client.StopInstance(ctx, name, true); err != nil {
				return err
			}
		}
		fmt.Fprintf(stdout, "Deleting sandbox %s...\n", name)
		if err := client.DeleteInstance(ctx, name); err != nil {
			return err
		}
	}

	_, err = client.GetStorageVolume(ctx, pool, volume)
	if incusclient.IsNotFound(err) {
		if !instanceExists {
			fmt.Fprintf(stdout, "Sandbox %s and workspace %s are already absent.\n", name, volume)
		}
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Deleting workspace %s...\n", volume)
	if err := client.DeleteStorageVolume(ctx, pool, volume); err != nil {
		return err
	}
	return nil
}

func seedVolume(cfg config.Config) string {
	if cfg.Workspace.Volume == "" {
		return config.DefaultWorkspaceVolume
	}
	return cfg.Workspace.Volume
}

func workspaceVolume(name string) string {
	return workspaceNamePrefix + name
}
