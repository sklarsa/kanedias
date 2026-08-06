package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
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
	rpcPort             = "7777"
	sandboxProfile      = "sandbox"
	workspaceDevice     = "workspace"
	workspacePath       = "/workspace"
	workspaceNamePrefix = "kanedias-workspace-"
	cleanupTimeout      = 30 * time.Second
)

type sessionClient interface {
	Disconnect()
	ResolvePool(context.Context, string) (string, error)
	GetNetwork(context.Context, string) (*api.Network, error)
	CreateNetwork(context.Context, api.NetworksPost) error
	EnsureProfile(context.Context, string, []byte) error
	GetImageAlias(context.Context, string) (*api.ImageAliasesEntry, error)
	GetStorageVolume(context.Context, string, string) (*api.StorageVolume, error)
	CopyStorageVolume(context.Context, string, string, string) error
	DeleteStorageVolume(context.Context, string, string) error
	CreateInstance(context.Context, api.InstancesPost) error
	StartInstance(context.Context, string) error
	StopInstance(context.Context, string, bool) error
	DeleteInstance(context.Context, string) error
	GetInstanceState(context.Context, string) (*api.InstanceState, error)
}

type dependencies struct {
	connect               func(context.Context) (sessionClient, error)
	ensureNetwork         func(context.Context, sessionClient, config.Config) error
	renderProfile         func(io.Writer, string, config.Config) error
	defaultProxyOpts      func() (proxy.Options, error)
	initCA                func(string, string) error
	checkProxy            func(context.Context, config.Config) error
	dialRPC               func(context.Context, string) (net.Conn, error)
	operationWasSubmitted func(error) bool
	newName               func() (string, error)
	readinessTimeout      time.Duration
	retryInterval         time.Duration
}

func defaultDependencies() dependencies {
	return dependencies{
		connect: func(ctx context.Context) (sessionClient, error) {
			return incusclient.Connect(ctx)
		},
		ensureNetwork: func(ctx context.Context, client sessionClient, cfg config.Config) error {
			return network.EnsureWithClient(ctx, client, cfg)
		},
		renderProfile:    profiles.Render,
		defaultProxyOpts: proxy.DefaultOptions,
		initCA:           proxy.InitCA,
		checkProxy:       checkProxy,
		dialRPC: func(ctx context.Context, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", address)
		},
		operationWasSubmitted: incusclient.OperationWasSubmitted,
		newName:               newSessionName,
		readinessTimeout:      60 * time.Second,
		retryInterval:         500 * time.Millisecond,
	}
}

// Run executes one prompt in a newly-created ephemeral Incus instance.
func Run(ctx context.Context, cfg config.Config, prompt string, stdout, stderr io.Writer) error {
	return run(ctx, cfg, prompt, stdout, stderr, defaultDependencies())
}

func run(
	ctx context.Context,
	cfg config.Config,
	prompt string,
	stdout io.Writer,
	stderr io.Writer,
	deps dependencies,
) (err error) {
	if err := cfg.ValidateLifecycle(); err != nil {
		return err
	}
	if strings.TrimSpace(prompt) == "" {
		return errors.New("session prompt is empty")
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

	proxyOptions, err := deps.defaultProxyOpts()
	if err != nil {
		return err
	}
	if err := deps.initCA(proxyOptions.CACertPath, proxyOptions.CAKeyPath); err != nil {
		return fmt.Errorf("initialize proxy CA: %w", err)
	}
	if err := deps.ensureNetwork(ctx, client, cfg); err != nil {
		return err
	}

	var profile bytes.Buffer
	if err := deps.renderProfile(&profile, sandboxProfile, cfg); err != nil {
		return err
	}
	if err := client.EnsureProfile(ctx, sandboxProfile, profile.Bytes()); err != nil {
		return err
	}
	if err := deps.checkProxy(ctx, cfg); err != nil {
		return err
	}
	if _, err := client.GetImageAlias(ctx, cfg.BaseImage.Name); err != nil {
		return err
	}

	seed := cfg.Workspace.Volume
	if seed == "" {
		seed = config.DefaultWorkspaceVolume
	}
	if _, err := client.GetStorageVolume(ctx, pool, seed); err != nil {
		return fmt.Errorf("get workspace seed volume: %w", err)
	}

	name, err := deps.newName()
	if err != nil {
		return fmt.Errorf("generate session name: %w", err)
	}
	volume := workspaceNamePrefix + name
	volumeCreated := false
	instanceCreated := false
	instanceRunning := false
	instanceStartAmbiguous := false
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()

		var cleanupErr error
		shouldStop := instanceRunning
		if instanceStartAmbiguous {
			state, stateErr := client.GetInstanceState(cleanupCtx, name)
			switch {
			case stateErr == nil:
				shouldStop = state != nil && state.StatusCode != api.Stopped
			case incusclient.IsNotFound(stateErr):
				// The submitted create/start is already absent.
			default:
				cleanupErr = errors.Join(cleanupErr, stateErr)
			}
		}
		if shouldStop {
			fmt.Fprintf(stderr, "Stopping session %s...\n", name)
			if stopErr := client.StopInstance(cleanupCtx, name, true); stopErr != nil && !incusclient.IsNotFound(stopErr) {
				cleanupErr = errors.Join(cleanupErr, stopErr)
			}
		}
		if instanceCreated {
			fmt.Fprintf(stderr, "Deleting session %s...\n", name)
			if deleteErr := client.DeleteInstance(cleanupCtx, name); deleteErr != nil && !incusclient.IsNotFound(deleteErr) {
				cleanupErr = errors.Join(cleanupErr, deleteErr)
			}
		}
		if volumeCreated {
			fmt.Fprintf(stderr, "Deleting workspace %s...\n", volume)
			if deleteErr := client.DeleteStorageVolume(cleanupCtx, pool, volume); deleteErr != nil && !incusclient.IsNotFound(deleteErr) {
				cleanupErr = errors.Join(cleanupErr, deleteErr)
			}
		}
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("clean up session %q: %w", name, cleanupErr))
		}
	}()

	fmt.Fprintf(stderr, "Cloning workspace %s to %s...\n", seed, volume)
	if err := client.CopyStorageVolume(ctx, pool, seed, volume); err != nil {
		if deps.operationWasSubmitted(err) {
			volumeCreated = true
		}
		return err
	}
	volumeCreated = true

	request := api.InstancesPost{
		Name: name,
		InstancePut: api.InstancePut{
			Profiles: []string{"default", sandboxProfile},
			Config: map[string]string{
				"user.kanedias.kind":     "session",
				"user.kanedias.rpc.port": rpcPort,
			},
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
			},
		},
		Source: api.InstanceSource{Type: "image", Alias: cfg.BaseImage.Name},
	}
	fmt.Fprintf(stderr, "Creating session %s...\n", name)
	if err := client.CreateInstance(ctx, request); err != nil {
		if deps.operationWasSubmitted(err) {
			instanceCreated = true
		}
		return err
	}
	instanceCreated = true

	fmt.Fprintf(stderr, "Starting session %s...\n", name)
	if err := client.StartInstance(ctx, name); err != nil {
		if deps.operationWasSubmitted(err) {
			instanceStartAmbiguous = true
		}
		return err
	}
	instanceRunning = true

	fmt.Fprintf(stderr, "Waiting for Pi RPC in %s...\n", name)
	conn, err := waitForRPC(ctx, client, name, deps)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := runRPC(ctx, conn, prompt, stdout); err != nil {
		return err
	}
	return nil
}

func waitForRPC(ctx context.Context, client sessionClient, name string, deps dependencies) (net.Conn, error) {
	readyCtx, cancel := context.WithTimeout(ctx, deps.readinessTimeout)
	defer cancel()

	var lastErr error
	for {
		state, err := client.GetInstanceState(readyCtx, name)
		if err != nil {
			lastErr = err
		} else if address := globalIPv4(state); address != "" {
			conn, dialErr := deps.dialRPC(readyCtx, net.JoinHostPort(address, rpcPort))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}

		timer := time.NewTimer(deps.retryInterval)
		select {
		case <-readyCtx.Done():
			timer.Stop()
			if lastErr != nil {
				return nil, fmt.Errorf("wait for Pi RPC in session %q: %w", name, errors.Join(readyCtx.Err(), lastErr))
			}
			return nil, fmt.Errorf("wait for Pi RPC in session %q: %w", name, readyCtx.Err())
		case <-timer.C:
		}
	}
}

func globalIPv4(state *api.InstanceState) string {
	if state == nil {
		return ""
	}
	for _, address := range state.Network["eth0"].Addresses {
		if address.Family == "inet" && address.Scope == "global" {
			return address.Address
		}
	}
	return ""
}

func checkProxy(ctx context.Context, cfg config.Config) error {
	prefix, err := cfg.Network.IPv4Prefix()
	if err != nil {
		return err
	}
	address := net.JoinHostPort(prefix.Addr().String(), "3128")
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("check proxy listener at %s: %w", address, err)
	}
	if err := conn.Close(); err != nil {
		return fmt.Errorf("close proxy listener check at %s: %w", address, err)
	}
	return nil
}

func newSessionName() (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return "session-" + hex.EncodeToString(suffix[:]), nil
}
