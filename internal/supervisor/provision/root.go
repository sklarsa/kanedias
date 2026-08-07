package provision

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/incusclient"
	"github.com/sklarsa/kanedias/internal/network"
	"github.com/sklarsa/kanedias/internal/profiles"
	"github.com/sklarsa/kanedias/internal/proxy"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

const (
	rootRPCPort             = "7777"
	rootSandboxProfile      = "sandbox"
	rootWorkspaceNamePrefix = "kanedias-workspace-"
	rootCleanupTimeout      = 30 * time.Second
)

type rootClient interface {
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

type rootDependencies struct {
	connect                 func(context.Context) (rootClient, error)
	ensureNetwork           func(context.Context, rootClient, config.Config) error
	renderProfile           func(io.Writer, string, config.Config) error
	initProxyCA             func() error
	checkProxy              func(context.Context, config.Config) error
	operationWasSubmitted   func(error) bool
	awaitSubmittedOperation func(context.Context, error) error
	newName                 func() (string, error)
	readinessTimeout        time.Duration
	retryInterval           time.Duration
}

// IncusRootProvisioner creates and owns one image-based root session.
type IncusRootProvisioner struct {
	config config.Config
	deps   rootDependencies
}

// NewRootProvisioner returns the production root provisioner.
func NewRootProvisioner(cfg config.Config) *IncusRootProvisioner {
	return newRootProvisioner(cfg, defaultRootDependencies())
}

func newRootProvisioner(cfg config.Config, deps rootDependencies) *IncusRootProvisioner {
	return &IncusRootProvisioner{config: cfg, deps: deps}
}

func defaultRootDependencies() rootDependencies {
	return rootDependencies{
		connect: func(ctx context.Context) (rootClient, error) { return incusclient.Connect(ctx) },
		ensureNetwork: func(ctx context.Context, client rootClient, cfg config.Config) error {
			return network.EnsureWithClient(ctx, client, cfg)
		},
		renderProfile: profiles.Render,
		initProxyCA: func() error {
			options, err := proxy.DefaultOptions()
			if err != nil {
				return err
			}
			return proxy.InitCA(options.CACertPath, options.CAKeyPath)
		},
		checkProxy:              checkRootProxy,
		operationWasSubmitted:   incusclient.OperationWasSubmitted,
		awaitSubmittedOperation: incusclient.AwaitSubmittedOperation,
		newName:                 newRootName,
		readinessTimeout:        60 * time.Second,
		retryInterval:           500 * time.Millisecond,
	}
}

func (provisioner *IncusRootProvisioner) ProvisionRoot(ctx context.Context, request RootRequest) (_ *Resources, err error) {
	if err := provisioner.config.ValidateLifecycle(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.SessionID) == "" {
		return nil, contract.NewError(contract.ErrorInvalidRequest, "root session ID is required")
	}
	if request.SocketPath == "" || !filepath.IsAbs(request.SocketPath) {
		return nil, contract.NewError(contract.ErrorInvalidRequest, "root supervisor socket path must be absolute")
	}

	client, err := provisioner.deps.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Disconnect()

	pool, err := client.ResolvePool(ctx, provisioner.config.Workspace.Pool)
	if err != nil {
		return nil, err
	}
	if err := provisioner.deps.initProxyCA(); err != nil {
		return nil, fmt.Errorf("initialize proxy CA: %w", err)
	}
	if err := provisioner.deps.ensureNetwork(ctx, client, provisioner.config); err != nil {
		return nil, err
	}
	var profile bytes.Buffer
	if err := provisioner.deps.renderProfile(&profile, rootSandboxProfile, provisioner.config); err != nil {
		return nil, err
	}
	if err := client.EnsureProfile(ctx, rootSandboxProfile, profile.Bytes()); err != nil {
		return nil, err
	}

	// The proxy check is deliberately before the first session-owned create/copy.
	if err := provisioner.deps.checkProxy(ctx, provisioner.config); err != nil {
		return nil, fmt.Errorf("%w: %w", contract.NewError(contract.ErrorProxyUnavailable, "configured proxy listener is unavailable"), err)
	}
	if _, err := client.GetImageAlias(ctx, provisioner.config.BaseImage.Name); err != nil {
		return nil, err
	}
	seed := provisioner.config.Workspace.Volume
	if seed == "" {
		seed = config.DefaultWorkspaceVolume
	}
	if _, err := client.GetStorageVolume(ctx, pool, seed); err != nil {
		return nil, fmt.Errorf("get workspace seed volume: %w", err)
	}

	name, err := provisioner.deps.newName()
	if err != nil {
		return nil, fmt.Errorf("generate root session name: %w", err)
	}
	volume := rootWorkspaceNamePrefix + name
	owned := rootOwned{instance: name, volume: volume, pool: pool}
	var submittedErrors []error
	defer func() {
		if err == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rootCleanupTimeout)
		defer cancel()
		for _, submittedErr := range submittedErrors {
			if awaitErr := provisioner.deps.awaitSubmittedOperation(cleanupCtx, submittedErr); awaitErr != nil {
				err = errors.Join(err, awaitErr)
			}
		}
		if cleanupErr := cleanupRoot(cleanupCtx, client, owned); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("clean up root session %q: %w", name, cleanupErr))
		}
	}()

	if err = client.CopyStorageVolume(ctx, pool, seed, volume); err != nil {
		owned.volumeOwned = provisioner.deps.operationWasSubmitted(err)
		owned.volumeAmbiguous = owned.volumeOwned
		if owned.volumeOwned {
			submittedErrors = append(submittedErrors, err)
		}
		return nil, err
	}
	owned.volumeOwned = true

	instanceRequest := api.InstancesPost{
		Name: name,
		InstancePut: api.InstancePut{
			Profiles: []string{"default", rootSandboxProfile},
			Config: map[string]string{
				"user.kanedias.kind":             "root",
				"user.kanedias.session_id":       request.SessionID,
				"user.kanedias.rpc.port":         rootRPCPort,
				"user.kanedias.workspace_volume": volume,
			},
			Devices: api.DevicesMap{
				"root":      {"type": "disk", "pool": pool, "path": "/"},
				"workspace": {"type": "disk", "pool": pool, "source": volume, "path": "/workspace"},
				"supervisor": {
					"type": "proxy", "bind": "instance",
					"listen":  "unix:/run/kanedias/supervisor.sock",
					"connect": "unix:" + request.SocketPath,
					"uid":     "1000", "gid": "1000", "mode": "0600",
				},
			},
		},
		Source: api.InstanceSource{Type: "image", Alias: provisioner.config.BaseImage.Name},
	}
	if err = client.CreateInstance(ctx, instanceRequest); err != nil {
		owned.instanceOwned = provisioner.deps.operationWasSubmitted(err)
		owned.instanceAmbiguous = owned.instanceOwned
		if owned.instanceOwned {
			submittedErrors = append(submittedErrors, err)
		}
		return nil, err
	}
	owned.instanceOwned = true

	if err = client.StartInstance(ctx, name); err != nil {
		owned.startAmbiguous = provisioner.deps.operationWasSubmitted(err)
		if owned.startAmbiguous {
			submittedErrors = append(submittedErrors, err)
		}
		return nil, err
	}
	owned.running = true

	address, waitErr := waitForRootRPCAddress(ctx, client, name, provisioner.deps.readinessTimeout, provisioner.deps.retryInterval)
	if waitErr != nil {
		err = waitErr
		return nil, err
	}
	return &Resources{SessionID: request.SessionID, Pool: pool, Instance: name, Volume: volume, RPCAddr: address}, nil
}

func (provisioner *IncusRootProvisioner) Destroy(ctx context.Context, resources *Resources) error {
	if resources == nil {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rootCleanupTimeout)
	defer cancel()
	client, err := provisioner.deps.connect(cleanupCtx)
	if err != nil {
		return err
	}
	defer client.Disconnect()
	return cleanupRoot(cleanupCtx, client, rootOwned{
		instance: resources.Instance, volume: resources.Volume, pool: resources.Pool,
		instanceOwned: resources.Instance != "", volumeOwned: resources.Volume != "", startAmbiguous: resources.Instance != "",
	})
}

type rootOwned struct {
	instance, volume, pool             string
	instanceOwned, volumeOwned         bool
	instanceAmbiguous, volumeAmbiguous bool
	running, startAmbiguous            bool
}

func cleanupRoot(ctx context.Context, client rootClient, owned rootOwned) error {
	var cleanupErr error
	shouldStop := owned.running
	if owned.instanceOwned && (owned.instanceAmbiguous || owned.startAmbiguous) {
		state, err := client.GetInstanceState(ctx, owned.instance)
		switch {
		case err == nil && state != nil:
			shouldStop = state.StatusCode != api.Stopped
		case err == nil:
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("probe possibly owned root instance %q: returned no state", owned.instance))
		case incusclient.IsNotFound(err):
			owned.instanceOwned = false
		default:
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("probe possibly owned root instance %q: %w", owned.instance, err))
		}
	}
	if shouldStop {
		if err := client.StopInstance(ctx, owned.instance, true); err != nil && !incusclient.IsNotFound(err) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if owned.instanceOwned {
		if err := client.DeleteInstance(ctx, owned.instance); err != nil && !incusclient.IsNotFound(err) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if owned.volumeOwned && owned.volumeAmbiguous {
		volume, err := client.GetStorageVolume(ctx, owned.pool, owned.volume)
		switch {
		case err == nil && volume != nil:
		case err == nil:
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("probe possibly owned root volume %q: returned no volume", owned.volume))
		case incusclient.IsNotFound(err):
			owned.volumeOwned = false
		default:
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("probe possibly owned root volume %q: %w", owned.volume, err))
		}
	}
	if owned.volumeOwned {
		if err := client.DeleteStorageVolume(ctx, owned.pool, owned.volume); err != nil && !incusclient.IsNotFound(err) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	return cleanupErr
}

func waitForRootRPCAddress(ctx context.Context, client rootClient, instance string, timeout, interval time.Duration) (string, error) {
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		state, err := client.GetInstanceState(readyCtx, instance)
		if err != nil {
			lastErr = err
		} else if address := rootGlobalIPv4(state); address != "" {
			return net.JoinHostPort(address, rootRPCPort), nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-readyCtx.Done():
			timer.Stop()
			return "", fmt.Errorf("wait for Pi RPC address in root session %q: %w", instance, errors.Join(readyCtx.Err(), lastErr))
		case <-timer.C:
		}
	}
}

func rootGlobalIPv4(state *api.InstanceState) string {
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

func checkRootProxy(ctx context.Context, cfg config.Config) error {
	prefix, err := cfg.Network.IPv4Prefix()
	if err != nil {
		return err
	}
	address := net.JoinHostPort(prefix.Addr().String(), "3128")
	connection, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("check proxy listener at %s: %w", address, err)
	}
	if err := connection.Close(); err != nil {
		return fmt.Errorf("close proxy listener check at %s: %w", address, err)
	}
	return nil
}

func newRootName() (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return "session-" + hex.EncodeToString(suffix[:]), nil
}
