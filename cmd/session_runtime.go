package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/process"
	"github.com/sklarsa/kanedias/internal/supervisor/provision"
	"github.com/sklarsa/kanedias/internal/supervisorapi"
)

const supervisorCleanupTimeout = 30 * time.Second

type configWorkerCatalog struct{ config config.Config }

func (catalog configWorkerCatalog) Resolve(name string) (config.WorkerProfile, error) {
	profile, err := catalog.config.ResolveWorker(name)
	if err != nil {
		return config.WorkerProfile{}, contract.NewError(contract.ErrorUnknownWorkerType, err.Error())
	}
	return profile, nil
}

func (catalog configWorkerCatalog) Summaries() []contract.WorkerSummary {
	names := catalog.config.WorkerNames()
	result := make([]contract.WorkerSummary, 0, len(names))
	for _, name := range names {
		profile, err := catalog.config.ResolveWorker(name)
		if err != nil {
			continue
		}
		result = append(result, contract.WorkerSummary{
			WorkerType: name, Description: profile.Description,
			Profile: contract.ModelProfile{Provider: profile.Provider, Model: profile.Model, ThinkingLevel: profile.ThinkingLevel},
		})
	}
	return result
}

type childRootProvisionAdapter struct {
	provisioner provision.ChildProvisioner
	request     provision.ChildRequest
}

func (adapter childRootProvisionAdapter) ProvisionRoot(ctx context.Context, _ provision.RootRequest) (*provision.Resources, error) {
	return adapter.provisioner.ProvisionChild(ctx, adapter.request)
}
func (adapter childRootProvisionAdapter) Destroy(ctx context.Context, resources *provision.Resources) error {
	return adapter.provisioner.Destroy(ctx, resources)
}

type eventBrokerFactory func(supervisor.EventBrokerOptions) (*supervisor.EventBroker, error)

func runSupervisor(ctx context.Context, cfg config.Config, opts SessionOptions, out io.Writer) error {
	return runSupervisorWithBrokerFactory(ctx, cfg, opts, out, supervisor.NewEventBrokerWithOptions)
}

func runSupervisorWithBrokerFactory(ctx context.Context, cfg config.Config, options SessionOptions, output io.Writer, factory eventBrokerFactory) error {
	if err := cfg.ValidateSupervisor(); err != nil {
		return err
	}
	limits, err := cfg.Supervisor.Events.Limits()
	if err != nil {
		return err
	}
	broker, err := factory(supervisor.EventBrokerOptions{MaxEvents: limits.MaxEvents, MaxBytes: limits.MaxBytes})
	if err != nil {
		return err
	}
	if options.ConfigPath == "" || !filepath.IsAbs(options.ConfigPath) || filepath.Clean(options.ConfigPath) != options.ConfigPath {
		return fmt.Errorf("supervisor config path must be absolute and clean")
	}
	socketPath, err := filepath.Abs(options.SocketPath)
	if err != nil {
		return fmt.Errorf("resolve supervisor socket path %q: %w", options.SocketPath, err)
	}
	sessionID, err := supervisorSessionID()
	if err != nil {
		return err
	}
	identity, err := supervisor.NewIdentity(supervisor.IdentitySpec{
		SessionID: sessionID, RootID: sessionID, Kind: contract.ChildKindRoot, Context: contract.ContextRoot,
	})
	if err != nil {
		return err
	}

	var unixServer *supervisorapi.UnixServer
	listenerReady := make(chan struct{})
	closeListener := func(closeCtx context.Context) error {
		select {
		case <-listenerReady:
			return unixServer.Close(closeCtx)
		case <-closeCtx.Done():
			return closeCtx.Err()
		}
	}
	spawner := process.Spawner{ConfigPath: options.ConfigPath}
	handoffVerifier, err := supervisor.NewGitHubHandoffVerifier(cfg.Workspace.Repos, nil)
	if err != nil {
		return err
	}
	directRecoverer, err := provision.NewConfiguredDirectChildRecoverer(ctx, cfg)
	if err != nil {
		return err
	}
	defer directRecoverer.Close()
	node, err := supervisor.NewRoot(identity, supervisor.Dependencies{
		Provisioner: provision.NewRootProvisioner(cfg),
		DialRPC:     dialPiRPC,
		Workers:     configWorkerCatalog{config: cfg},
		SocketPath:  socketPath,
		SpawnChild: func(ctx context.Context, bootstrap process.Bootstrap) (supervisor.ChildProcess, error) {
			return spawner.Spawn(ctx, bootstrap)
		},
		DescendantClient:     supervisorapi.NewDescendantClient,
		DirectChildRecoverer: directRecoverer,
		CloseListener:        closeListener,
		HandoffVerifier:      handoffVerifier,
		RunAttribution:       os.Getenv("KANEDIAS_E2E_RUN_ID"),
	}, broker)
	if err != nil {
		return err
	}
	router := supervisor.NewRouter(node)
	unixServer, err = supervisorapi.StartUnix(socketPath, supervisorapi.NewHandler(router))
	if err != nil {
		return err
	}
	close(listenerReady)

	runtimeCtx, cancelRuntime := context.WithCancel(ctx)
	defer cancelRuntime()
	go func() { <-unixServer.Done(); cancelRuntime() }()
	if err := node.Start(runtimeCtx); err != nil {
		return errors.Join(err, unixServer.Err())
	}
	if output != nil {
		_ = json.NewEncoder(output).Encode(node.Snapshot())
	}

	select {
	case <-ctx.Done():
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), supervisorCleanupTimeout)
		defer cancel()
		return node.Stop(cleanupCtx, supervisor.StopReasonCancelled)
	case <-node.Done():
		return node.Stop(context.Background(), supervisor.StopReasonRequested)
	case <-unixServer.Done():
		cleanupCtx, cancel := context.WithTimeout(context.Background(), supervisorCleanupTimeout)
		defer cancel()
		return errors.Join(unixServer.Err(), node.Stop(cleanupCtx, supervisor.StopReasonRPCFailure))
	}
}

func productionChildRunner(ctx context.Context, bootstrap process.Bootstrap, reporter *process.Reporter) error {
	return productionChildRunnerWithBrokerFactory(ctx, bootstrap, reporter, supervisor.NewEventBrokerWithOptions)
}

func productionChildRunnerWithBrokerFactory(ctx context.Context, bootstrap process.Bootstrap, reporter *process.Reporter, factory eventBrokerFactory) (resultErr error) {
	configPath := os.Getenv("KANEDIAS_CONFIG")
	if configPath == "" || !filepath.IsAbs(configPath) || filepath.Clean(configPath) != configPath {
		return contract.NewError(contract.ErrorInvalidRequest, "KANEDIAS_CONFIG must name an absolute clean path")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if err := cfg.ValidateSupervisor(); err != nil {
		return err
	}
	limits, err := cfg.Supervisor.Events.Limits()
	if err != nil {
		return err
	}
	broker, err := factory(supervisor.EventBrokerOptions{MaxEvents: limits.MaxEvents, MaxBytes: limits.MaxBytes})
	if err != nil {
		return err
	}
	resolvedWorker, err := cfg.ResolveWorker(bootstrap.Request.WorkerType)
	if err != nil {
		return contract.NewError(contract.ErrorUnknownWorkerType, err.Error())
	}
	if resolvedWorker != bootstrap.Worker {
		return contract.NewError(contract.ErrorConflict, "child worker profile does not match configured policy")
	}
	childProvisioner, err := provision.NewConfiguredChildProvisioner(ctx, cfg)
	if err != nil {
		return err
	}
	defer childProvisioner.Close()
	directRecoverer, err := provision.NewConfiguredDirectChildRecoverer(ctx, cfg)
	if err != nil {
		return err
	}
	defer directRecoverer.Close()
	identity, err := supervisor.NewIdentity(supervisor.IdentitySpec{
		SessionID: bootstrap.SessionID, ParentID: bootstrap.ParentID, RootID: bootstrap.RootID,
		Kind: bootstrap.Request.Kind, Context: bootstrap.Request.Context, Worker: bootstrap.Request.WorkerType,
	})
	if err != nil {
		return err
	}
	adapter := childRootProvisionAdapter{provisioner: childProvisioner, request: provision.ChildRequest{
		SessionID: bootstrap.SessionID, ParentID: bootstrap.ParentID, RootID: bootstrap.RootID,
		SourceInstance: bootstrap.SourceInstance, SourceVolume: bootstrap.SourceVolume,
		HostSocketPath: bootstrap.SocketPath, Worker: bootstrap.Worker, Contract: bootstrap.Request,
		RunAttribution: bootstrap.RunAttribution,
	}}

	var unixServer *supervisorapi.UnixServer
	listenerReady := make(chan struct{})
	closeListener := func(closeCtx context.Context) error {
		select {
		case <-listenerReady:
			return unixServer.Close(closeCtx)
		case <-closeCtx.Done():
			return closeCtx.Err()
		}
	}
	spawner := process.Spawner{ConfigPath: configPath}
	var expectedPiBinding *supervisor.PiBinding
	if bootstrap.Request.Context == contract.ContextFork && bootstrap.Request.Fork != nil {
		expectedPiBinding = &supervisor.PiBinding{
			SessionID: bootstrap.Request.Fork.PiSessionID, SessionFile: bootstrap.Request.Fork.SessionFile,
		}
	}
	handoffVerifier, err := supervisor.NewGitHubHandoffVerifier(cfg.Workspace.Repos, nil)
	if err != nil {
		return err
	}
	node, err := supervisor.NewChild(identity, supervisor.Dependencies{
		Provisioner: adapter, DialRPC: dialPiRPC, Workers: configWorkerCatalog{config: cfg},
		SocketPath: bootstrap.SocketPath,
		SpawnChild: func(ctx context.Context, nested process.Bootstrap) (supervisor.ChildProcess, error) {
			return spawner.Spawn(ctx, nested)
		},
		DescendantClient:     supervisorapi.NewDescendantClient,
		DirectChildRecoverer: directRecoverer,
		CloseListener:        closeListener,
		ReportWrite:          reporter.Write,
		ExpectedPiBinding:    expectedPiBinding,
		HandoffVerifier:      handoffVerifier,
		RunAttribution:       bootstrap.RunAttribution,
		ResourcePublished: func(resources *provision.Resources) error {
			socket, err := recoverySocketIdentity(bootstrap.SocketPath)
			if err != nil {
				return err
			}
			return reporter.Ownership(provision.RecoveryTicket{
				SessionID: bootstrap.SessionID, ParentID: bootstrap.ParentID, RootID: bootstrap.RootID,
				Pool: resources.Pool, Instance: resources.Instance, Volume: resources.Volume,
				SocketPath: bootstrap.SocketPath, Socket: socket,
				Kind: bootstrap.Request.Kind, Context: bootstrap.Request.Context, WorkerType: bootstrap.Request.WorkerType,
				RunAttribution: bootstrap.RunAttribution,
			})
		},
	}, broker)
	if err != nil {
		return err
	}
	unixServer, err = supervisorapi.StartUnix(bootstrap.SocketPath, supervisorapi.NewHandler(supervisor.NewRouter(node)))
	if err != nil {
		return err
	}
	close(listenerReady)
	started := false
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), supervisorCleanupTimeout)
		defer cancel()
		if started {
			resultErr = errors.Join(resultErr, node.Stop(cleanupCtx, supervisor.StopReasonRequested))
		} else {
			resultErr = errors.Join(resultErr, unixServer.Close(cleanupCtx))
		}
	}()

	if err := node.Start(ctx); err != nil {
		return err
	}
	started = true
	if err := reporter.Ready(bootstrap.SocketPath); err != nil {
		return err
	}
	switch bootstrap.Request.Kind {
	case contract.ChildKindRead:
		readResult, err := node.RunReadTask(ctx, bootstrap.Request.Task)
		if err != nil {
			return err
		}
		return reporter.Read(readResult)
	case contract.ChildKindWrite:
		if err := node.RunWriteTask(ctx, bootstrap.Request.Task); err != nil {
			return err
		}
		select {
		case <-node.Done():
			return node.Stop(context.Background(), supervisor.StopReasonRequested)
		case <-ctx.Done():
			return ctx.Err()
		}
	default:
		return contract.NewError(contract.ErrorConflict, "unsupported child kind")
	}
}

func recoverySocketIdentity(path string) (provision.SocketIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return provision.SocketIdentity{}, fmt.Errorf("inspect child recovery socket %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return provision.SocketIdentity{}, fmt.Errorf("child recovery path %q is not a Unix socket", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return provision.SocketIdentity{}, fmt.Errorf("inspect child recovery socket %q identity", path)
	}
	return provision.SocketIdentity{Device: uint64(stat.Dev), Inode: stat.Ino}, nil
}

func dialPiRPC(ctx context.Context, address string) (io.ReadWriteCloser, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", address)
}

func supervisorSessionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate supervisor session ID: %w", err)
	}
	return "session-" + hex.EncodeToString(value[:]), nil
}
