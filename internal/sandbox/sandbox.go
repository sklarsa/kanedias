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
	incusworkspace "github.com/sklarsa/kanedias/internal/workspace/incus"
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
	CopyStorageVolumeUntilTerminal(context.Context, string, string, string) error
	DeleteStorageVolume(context.Context, string, string) error
	GetInstance(context.Context, string) (*api.Instance, string, error)
	CreateInstance(context.Context, api.InstancesPost) error
	StartInstance(context.Context, string) error
	StopInstance(context.Context, string, bool) error
	DeleteInstance(context.Context, string) error
	Exec(context.Context, string, incusclient.ExecRequest) (string, string, error)
}

type dependencies struct {
	connect                 func(context.Context) (lifecycleClient, error)
	acquireLock             func(string) (io.Closer, error)
	defaultProxyOptions     func() (proxy.Options, error)
	initCA                  func(string, string) error
	ensureNetwork           func(context.Context, lifecycleClient, config.Config) error
	cloneIncusState         func(context.Context, incusworkspace.VolumeClient, string, string, string) (incusworkspace.CloneResult, error)
	waitNestedIncus         func(context.Context, incusworkspace.Executor, string, time.Duration) error
	verifyNestedIncus       func(context.Context, incusworkspace.Executor, string) error
	operationWasSubmitted   func(error) bool
	awaitSubmittedOperation func(context.Context, error) error
	readinessTimeout        time.Duration
	readinessPollInterval   time.Duration
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
		cloneIncusState:         incusworkspace.Clone,
		waitNestedIncus:         incusworkspace.WaitReady,
		verifyNestedIncus:       incusworkspace.VerifyNativeBtrfs,
		operationWasSubmitted:   incusclient.OperationWasSubmitted,
		awaitSubmittedOperation: incusclient.AwaitSubmittedOperation,
		readinessTimeout:        60 * time.Second,
		readinessPollInterval:   time.Second,
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
	incusSeed := incusworkspace.SeedVolume(cfg)
	incusVolume := incusworkspace.SandboxVolume(name)
	volumeCreated := false
	incusVolumeCreated := false
	instanceCreated := false
	instanceRunning := false
	var submittedErrors []error
	defer func() {
		if err == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		var cleanupErr error
		for _, submittedErr := range submittedErrors {
			cleanupErr = errors.Join(cleanupErr, deps.awaitSubmittedOperation(cleanupCtx, submittedErr))
		}
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
		if incusVolumeCreated {
			fmt.Fprintf(stderr, "Deleting failed nested Incus state %s...\n", incusVolume)
			if deleteErr := incusworkspace.Delete(cleanupCtx, client, pool, incusSeed, incusVolume); deleteErr != nil && !incusclient.IsNotFound(deleteErr) {
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
	if copyErr := client.CopyStorageVolume(ctx, pool, seed, volume); copyErr != nil {
		volumeCreated = deps.operationWasSubmitted(copyErr)
		if volumeCreated {
			submittedErrors = append(submittedErrors, copyErr)
		}
		return copyErr
	}
	volumeCreated = true

	fmt.Fprintf(stdout, "Cloning nested Incus state %s to %s...\n", incusSeed, incusVolume)
	clone, cloneErr := deps.cloneIncusState(ctx, client, pool, incusSeed, name)
	incusVolumeCreated = clone.Created
	if cloneErr != nil {
		if deps.operationWasSubmitted(cloneErr) {
			submittedErrors = append(submittedErrors, cloneErr)
		}
		return cloneErr
	}

	request := api.InstancesPost{
		Name: name,
		InstancePut: api.InstancePut{
			Profiles: []string{"default", sandboxProfile},
			Devices: api.DevicesMap{
				"root": {
					"type": "disk",
					"pool": pool,
					"path": "/",
				},
				workspaceDevice: {
					"type":   "disk",
					"pool":   pool,
					"source": volume,
					"path":   workspacePath,
				},
				incusworkspace.DeviceName: incusworkspace.Device(pool, clone.Name),
			},
		},
		Source: api.InstanceSource{Type: "image", Alias: cfg.BaseImage.Name},
	}
	fmt.Fprintf(stdout, "Creating sandbox %s...\n", name)
	if err := client.CreateInstance(ctx, request); err != nil {
		if deps.operationWasSubmitted(err) {
			instanceCreated = true
			submittedErrors = append(submittedErrors, err)
		}
		return err
	}
	instanceCreated = true

	fmt.Fprintf(stdout, "Starting sandbox %s...\n", name)
	if err := client.StartInstance(ctx, name); err != nil {
		if deps.operationWasSubmitted(err) {
			instanceRunning = true
			submittedErrors = append(submittedErrors, err)
		}
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
	fmt.Fprintf(stdout, "Waiting for nested Incus in %s...\n", name)
	if err := deps.waitNestedIncus(ctx, client, name, deps.readinessTimeout); err != nil {
		return err
	}
	if err := deps.verifyNestedIncus(ctx, client, name); err != nil {
		return err
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
	seed := seedVolume(cfg)
	incusVolume := incusworkspace.SandboxVolume(name)
	incusSeed := incusworkspace.SeedVolume(cfg)
	for _, clone := range []string{volume, incusVolume} {
		for _, protectedSeed := range []string{seed, incusSeed} {
			if clone == protectedSeed {
				return fmt.Errorf("refusing to remove protected seed volume %q", clone)
			}
		}
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
		if !ok {
			return fmt.Errorf("refusing to remove %q: local workspace device is missing", name)
		}
		if err := verifyOwnedDevice(workspace, pool, volume, workspacePath); err != nil {
			return fmt.Errorf("refusing to remove %q: workspace device: %w", name, err)
		}
		incusState, ok := instance.Devices[incusworkspace.DeviceName]
		if !ok {
			return fmt.Errorf("refusing to remove %q: local nested Incus state device is missing", name)
		}
		if err := verifyOwnedDevice(incusState, pool, incusVolume, incusworkspace.MountPath); err != nil {
			return fmt.Errorf("refusing to remove %q: nested Incus state device: %w", name, err)
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

	deleteVolume := func(volume, seed, description string, delete func(context.Context, incusworkspace.VolumeClient, string, string, string) error) error {
		_, err := client.GetStorageVolume(ctx, pool, volume)
		if incusclient.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Deleting %s %s...\n", description, volume)
		if err := delete(ctx, client, pool, seed, volume); err != nil && !incusclient.IsNotFound(err) {
			return err
		}
		return nil
	}
	deleteIncusState := func(ctx context.Context, client incusworkspace.VolumeClient, pool, seed, volume string) error {
		return incusworkspace.Delete(ctx, client, pool, seed, volume)
	}
	deleteWorkspace := func(ctx context.Context, client incusworkspace.VolumeClient, pool, _, volume string) error {
		return client.DeleteStorageVolume(ctx, pool, volume)
	}
	incusErr := deleteVolume(incusVolume, incusSeed, "nested Incus state", deleteIncusState)
	workspaceErr := deleteVolume(volume, seed, "workspace", deleteWorkspace)
	if !instanceExists && incusErr == nil && workspaceErr == nil {
		fmt.Fprintf(stdout, "Sandbox %s and its volumes are absent.\n", name)
	}
	return errors.Join(incusErr, workspaceErr)
}

func verifyOwnedDevice(device map[string]string, pool, source, path string) error {
	want := map[string]string{"type": "disk", "pool": pool, "source": source, "path": path}
	for _, key := range []string{"type", "pool", "source", "path"} {
		if device[key] != want[key] {
			return fmt.Errorf("%s is %q, expected %q", key, device[key], want[key])
		}
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
