package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/pirpc"
	"github.com/sklarsa/kanedias/internal/supervisor/provision"
)

type Dependencies struct {
	Provisioner provision.RootProvisioner
	DialRPC     func(context.Context, string) (io.ReadWriteCloser, error)
	Workers     WorkerCatalog
	SocketPath  string
}

type Node struct {
	identity Identity
	deps     Dependencies
	broker   *EventBroker

	mu        sync.RWMutex
	started   bool
	local     *LocalSession
	resources *provision.Resources
	state     LifecycleState

	finishOnce sync.Once
	finishErr  error
	done       chan struct{}
}

func NewRoot(identity Identity, deps Dependencies, broker *EventBroker) (*Node, error) {
	snapshot := identity.Snapshot()
	if snapshot.Kind != contract.ChildKindRoot || snapshot.SessionID != snapshot.RootID || snapshot.ParentID != "" {
		return nil, invariantf("NewRoot requires a root identity")
	}
	if deps.Provisioner == nil {
		return nil, invariantf("root provisioner is required")
	}
	if deps.DialRPC == nil {
		return nil, invariantf("Pi RPC dialer is required")
	}
	if deps.Workers == nil {
		return nil, invariantf("worker catalog is required")
	}
	if deps.SocketPath == "" || !filepath.IsAbs(deps.SocketPath) {
		return nil, invariantf("root supervisor socket path must be absolute")
	}
	if broker == nil {
		broker = NewEventBroker()
	}
	return &Node{identity: identity, deps: deps, broker: broker, state: LifecycleProvisioning, done: make(chan struct{})}, nil
}

func (node *Node) Start(ctx context.Context) error {
	node.mu.Lock()
	if node.started {
		node.mu.Unlock()
		return invariantf("root node has already been started")
	}
	node.started = true
	node.mu.Unlock()

	if err := validateBoundSupervisorSocket(node.deps.SocketPath); err != nil {
		node.failStart(ctx, err)
		return node.finishedError()
	}

	identity := node.identity.Snapshot()
	resources, err := node.deps.Provisioner.ProvisionRoot(ctx, provision.RootRequest{
		SessionID:  identity.SessionID,
		SocketPath: node.deps.SocketPath,
	})
	if err != nil {
		node.failStart(ctx, err)
		return node.finishedError()
	}
	if resources == nil {
		node.failStart(ctx, invariantf("root provisioner returned nil resources"))
		return node.finishedError()
	}

	node.mu.Lock()
	node.resources = resources
	node.state = LifecycleStarting
	node.mu.Unlock()

	connection, err := node.deps.DialRPC(ctx, resources.RPCAddr)
	if err != nil {
		node.failStart(ctx, fmt.Errorf("dial Pi RPC at %q: %w", resources.RPCAddr, err))
		return node.finishedError()
	}
	if connection == nil {
		node.failStart(ctx, invariantf("Pi RPC dialer returned a nil connection"))
		return node.finishedError()
	}

	rpc := pirpc.NewClient(connection)
	local := NewLocalSession(node.identity, rpc, node.broker)
	node.mu.Lock()
	node.local = local
	node.mu.Unlock()

	// A successful transport dial is not readiness. Bind performs the correlated
	// get_state handshake and moves the local session to ready only on success.
	if err := local.Bind(ctx); err != nil {
		node.failStart(ctx, fmt.Errorf("bind root Pi session: %w", err))
		return node.finishedError()
	}

	go node.watchRPC(rpc)
	return nil
}

func (node *Node) CallRPC(ctx context.Context, command json.RawMessage) (json.RawMessage, error) {
	node.mu.RLock()
	local := node.local
	state := node.state
	node.mu.RUnlock()
	if local == nil {
		if state == LifecycleFailed || state == LifecycleStopping || state == LifecycleStopped {
			return nil, contract.NewError(contract.ErrorSessionStopping, "root session is not available")
		}
		return nil, contract.NewError(contract.ErrorChildUnavailable, "root Pi RPC is not ready")
	}
	return local.CallRPC(ctx, command)
}

func (node *Node) Snapshot() NodeSnapshot {
	node.mu.RLock()
	local := node.local
	state := node.state
	node.mu.RUnlock()
	if local != nil {
		snapshot := local.Snapshot()
		if state == LifecycleFailed || state == LifecycleStopped {
			snapshot.Lifecycle = string(state)
		}
		return snapshot
	}
	identity := node.identity.Snapshot()
	return NodeSnapshot{
		SessionID: identity.SessionID, ParentSessionID: identity.ParentID, RootSessionID: identity.RootID,
		Kind: identity.Kind, Context: identity.Context, WorkerType: identity.Worker,
		Lifecycle: string(state), Questions: []QuestionSummary{}, Children: []NodeSnapshot{},
	}
}

func (node *Node) Done() <-chan struct{} { return node.done }

func (node *Node) AnswerQuestion(ctx context.Context, id string, answer json.RawMessage) error {
	node.mu.RLock()
	local := node.local
	node.mu.RUnlock()
	if local == nil {
		return contract.NewError(contract.ErrorChildUnavailable, "root Pi RPC is not ready")
	}
	return local.questions.Answer(ctx, id, answer)
}

func (node *Node) WorkerSummaries() []contract.WorkerSummary {
	return node.deps.Workers.Summaries()
}

func (node *Node) watchRPC(rpc *pirpc.Client) {
	<-rpc.Done()
	node.mu.RLock()
	local := node.local
	node.mu.RUnlock()
	if local != nil {
		local.questions.Clear()
	}
	err := rpc.Err()
	if err == nil {
		err = io.EOF
	}
	node.finish(context.Background(), err, LifecycleFailed, false)
}

func (node *Node) failStart(ctx context.Context, primary error) {
	node.finish(ctx, primary, LifecycleFailed, true)
}

func (node *Node) finishedError() error {
	<-node.done
	node.mu.RLock()
	defer node.mu.RUnlock()
	return node.finishErr
}

func validateBoundSupervisorSocket(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect bound supervisor socket %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("bound supervisor path %q is not a Unix socket", path)
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("bound supervisor socket %q has mode %04o, require 0600", path, info.Mode().Perm())
	}
	return nil
}

func (node *Node) finish(ctx context.Context, primary error, terminal LifecycleState, closeRPC bool) {
	node.finishOnce.Do(func() {
		node.mu.RLock()
		local := node.local
		resources := node.resources
		node.mu.RUnlock()

		var cleanupErr error
		if local != nil {
			if closeRPC {
				cleanupErr = errors.Join(cleanupErr, local.StopRPC())
			} else {
				local.questions.Clear()
			}
		}
		if resources != nil {
			cleanupErr = errors.Join(cleanupErr, node.deps.Provisioner.Destroy(ctx, resources))
		}

		node.mu.Lock()
		node.state = terminal
		node.finishErr = errors.Join(primary, cleanupErr)
		node.mu.Unlock()
		close(node.done)
	})
}
