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

func rootSupervisorDependencies(options SessionOptions, dependencies supervisor.Dependencies) supervisor.Dependencies {
	dependencies.Workspace = options.Workspace
	return dependencies
}

type productionChildRuntime struct {
	newChildProvisioner     func(context.Context, config.Config) (provision.ChildProvisioner, func(), error)
	newDirectChildRecoverer func(context.Context, config.Config) (provision.DirectChildRecoverer, func(), error)
	dialRPC                 func(context.Context, string) (io.ReadWriteCloser, error)
	spawnChild              func(string) supervisor.ChildSpawner
	descendantClient        supervisor.DescendantClientFactory
	afterReady              func(context.Context, *supervisor.Node) error
}

func defaultProductionChildRuntime() productionChildRuntime {
	return productionChildRuntime{
		newChildProvisioner: func(ctx context.Context, cfg config.Config) (provision.ChildProvisioner, func(), error) {
			configured, err := provision.NewConfiguredChildProvisioner(ctx, cfg)
			if err != nil {
				return nil, nil, err
			}
			return configured, configured.Close, nil
		},
		newDirectChildRecoverer: func(ctx context.Context, cfg config.Config) (provision.DirectChildRecoverer, func(), error) {
			configured, err := provision.NewConfiguredDirectChildRecoverer(ctx, cfg)
			if err != nil {
				return nil, nil, err
			}
			return configured, configured.Close, nil
		},
		dialRPC: dialPiRPC,
		spawnChild: func(configPath string) supervisor.ChildSpawner {
			spawner := process.Spawner{ConfigPath: configPath}
			return func(ctx context.Context, bootstrap process.Bootstrap) (supervisor.ChildProcess, error) {
				return spawner.Spawn(ctx, bootstrap)
			}
		},
		descendantClient: supervisorapi.NewDescendantClient,
	}
}

func runSupervisor(ctx context.Context, cfg config.Config, opts SessionOptions, out io.Writer) error {
	return runSupervisorWithBrokerFactory(ctx, cfg, opts, out, supervisor.NewEventBrokerWithOptions)
}

func runSupervisorWithBrokerFactory(ctx context.Context, cfg config.Config, options SessionOptions, output io.Writer, factory eventBrokerFactory) (resultErr error) {
	rootStatus := options.RootStatus
	defer func() {
		if rootStatus != nil {
			resultErr = errors.Join(resultErr, rootStatus.Close())
		}
	}()

	policy := options.Policy.Clone()
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("validate session model policy: %w", err)
	}
	options.Policy = policy
	if err := options.Workspace.Validate(); err != nil {
		return fmt.Errorf("validate workspace start: %w", err)
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
	node, err := supervisor.NewRoot(identity, rootSupervisorDependencies(options, supervisor.Dependencies{
		Provisioner: provision.NewRootProvisioner(cfg),
		DialRPC:     dialPiRPC,
		ModelPolicy: policy,
		SocketPath:  socketPath,
		SpawnChild: func(ctx context.Context, bootstrap process.Bootstrap) (supervisor.ChildProcess, error) {
			return spawner.Spawn(ctx, bootstrap)
		},
		DescendantClient:     supervisorapi.NewDescendantClient,
		DirectChildRecoverer: directRecoverer,
		CloseListener:        closeListener,
		HandoffVerifier:      handoffVerifier,
		RunAttribution:       os.Getenv("KANEDIAS_E2E_RUN_ID"),
	}), broker)
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
	startErr := node.Start(runtimeCtx)
	if rootStatus != nil {
		reportErr := reportRootStartup(rootStatus, startErr)
		rootStatus = nil
		if reportErr != nil {
			if startErr == nil {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), supervisorCleanupTimeout)
				defer cancel()
				return errors.Join(reportErr, node.Stop(cleanupCtx, supervisor.StopReasonRPCFailure))
			}
			return errors.Join(startErr, unixServer.Err(), reportErr)
		}
	}
	if startErr != nil {
		return errors.Join(startErr, unixServer.Err())
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

func reportRootStartup(writer io.WriteCloser, startErr error) error {
	status := process.RootStartupStatus{Status: process.RootStartupReady}
	if startErr != nil {
		code := contract.ErrorInternal
		var typed *contract.Error
		if errors.As(startErr, &typed) {
			code = typed.Code
		}
		status = process.RootStartupStatus{Status: process.RootStartupFailure, Code: code}
	}
	return errors.Join(process.EncodeRootStartupStatus(writer, status), writer.Close())
}

func productionChildRunner(ctx context.Context, bootstrap process.Bootstrap, reporter *process.Reporter) error {
	return productionChildRunnerWithBrokerFactory(ctx, bootstrap, reporter, supervisor.NewEventBrokerWithOptions)
}

func inheritedChildPolicy(bootstrap process.Bootstrap) (config.SessionModelPolicy, config.WorkerProfile, error) {
	policy := bootstrap.Policy.Clone()
	if err := policy.Validate(); err != nil {
		return config.SessionModelPolicy{}, config.WorkerProfile{}, contract.NewError(contract.ErrorInvalidRequest, "inherited session model policy is invalid: "+err.Error())
	}
	worker, err := policy.ResolveWorker(bootstrap.Request.WorkerType)
	if err != nil {
		return config.SessionModelPolicy{}, config.WorkerProfile{}, contract.NewError(contract.ErrorUnknownWorkerType, err.Error())
	}
	return policy, worker, nil
}

func productionChildRunnerWithBrokerFactory(ctx context.Context, bootstrap process.Bootstrap, reporter *process.Reporter, factory eventBrokerFactory) error {
	return productionChildRunnerWithRuntime(ctx, bootstrap, reporter, factory, defaultProductionChildRuntime())
}

func productionChildRunnerWithRuntime(ctx context.Context, bootstrap process.Bootstrap, reporter *process.Reporter, factory eventBrokerFactory, runtime productionChildRuntime) (resultErr error) {
	if err := bootstrap.Workspace.Validate(); err != nil {
		return contract.NewError(contract.ErrorInvalidRequest, "inherited workspace start is invalid: "+err.Error())
	}
	configPath := os.Getenv("KANEDIAS_CONFIG")
	if configPath == "" || !filepath.IsAbs(configPath) || filepath.Clean(configPath) != configPath {
		return contract.NewError(contract.ErrorInvalidRequest, "KANEDIAS_CONFIG must name an absolute clean path")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if err := cfg.ValidateChildRuntime(); err != nil {
		return err
	}
	policy, resolvedWorker, err := inheritedChildPolicy(bootstrap)
	if err != nil {
		return err
	}
	handoffVerifier, err := supervisor.NewGitHubHandoffVerifier(cfg.Workspace.Repos, nil)
	if err != nil {
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
	childProvisioner, closeChildProvisioner, err := runtime.newChildProvisioner(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeChildProvisioner()
	directRecoverer, closeDirectRecoverer, err := runtime.newDirectChildRecoverer(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeDirectRecoverer()
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
		HostSocketPath: bootstrap.SocketPath, Worker: resolvedWorker, Workspace: bootstrap.Workspace, Contract: bootstrap.Request,
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
	spawnChild := runtime.spawnChild(configPath)
	var expectedPiBinding *supervisor.PiBinding
	if bootstrap.Request.Context == contract.ContextFork && bootstrap.Request.Fork != nil {
		expectedPiBinding = &supervisor.PiBinding{
			SessionID: bootstrap.Request.Fork.PiSessionID, SessionFile: bootstrap.Request.Fork.SessionFile,
		}
	}
	node, err := supervisor.NewChild(identity, supervisor.Dependencies{
		Provisioner: adapter, DialRPC: runtime.dialRPC, ModelPolicy: policy,
		Workspace:            bootstrap.Workspace,
		SocketPath:           bootstrap.SocketPath,
		SpawnChild:           spawnChild,
		DescendantClient:     runtime.descendantClient,
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
	if runtime.afterReady != nil {
		if err := runtime.afterReady(ctx, node); err != nil {
			return err
		}
	}
	switch bootstrap.Request.Kind {
	case contract.ChildKindRead:
		readResult, err := node.RunReadTask(ctx, bootstrap.Request.Task)
		if err != nil {
			return publishReadFailureAfterDrain(ctx, node, reporter, err)
		}
		// A natural read result still quiesces already-admitted routed RPC
		// handlers before the terminal success is acknowledged. When the
		// inherited context already won, publish nothing (Task 6G parent-owned
		// cancellation path).
		if ctx.Err() != nil {
			return ctx.Err()
		}
		drainCtx, cancelDrain := context.WithTimeout(context.WithoutCancel(ctx), supervisorCleanupTimeout)
		drainErr := node.QuiesceRPC(drainCtx)
		cancelDrain()
		if drainErr != nil {
			// A quiescence failure must never publish success; report one fixed
			// privacy-safe internal failure and join the drain diagnostics.
			return errors.Join(drainErr, publishReadFailure(context.WithoutCancel(ctx), reporter,
				contract.NewError(contract.ErrorInternal, "read child RPC did not drain")))
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

// publishReadFailureAfterDrain quiesces every already-admitted Node.CallRPC
// handler before publishing a read-child terminal failure, so the admitted
// handler can return its exact response to the direct parent before the terminal
// report is published and the child teardown starts. Inherited-context
// cancellation skips terminal publication entirely (Task 6G parent-owned
// cancellation path). An existing typed failure keeps its exact type after the
// drain; a quiescence error is retained only in the joined local diagnostics and
// never changes the published code/message.
func publishReadFailureAfterDrain(ctx context.Context, node *supervisor.Node, reporter *process.Reporter, runErr error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), supervisorCleanupTimeout)
	defer cancel()
	drainErr := node.QuiesceRPC(drainCtx)
	reportErr := publishReadFailure(ctx, reporter, runErr)
	return errors.Join(drainErr, reportErr)
}

// publishReadFailure publishes a typed privacy-safe terminal failure over the
// inherited report/ack channels for a failed read child while its supervisor
// transport is still live, so the runtime's deferred node teardown cannot close
// Unix/SSE before the direct parent ingests and acknowledges the report. When the
// inherited context is already cancelled, it publishes nothing and returns the
// context error: parent-liveness cancellation is already the canonical signal and
// must not produce a terminal report. reporter.Failure blocks for the exact
// parent acknowledgement; parent-liveness cancellation still unblocks that wait.
func publishReadFailure(ctx context.Context, reporter *process.Reporter, runErr error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	code := contract.ErrorInternal
	message := "internal supervisor error"
	var typed *contract.Error
	if errors.As(runErr, &typed) {
		code = typed.Code
		message = typed.Message
	}
	reportErr := reporter.Failure(code, message)
	return errors.Join(runErr, reportErr)
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
