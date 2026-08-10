package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/pirpc"
	"github.com/sklarsa/kanedias/internal/supervisor/process"
	"github.com/sklarsa/kanedias/internal/supervisor/provision"
)

type Dependencies struct {
	Provisioner            provision.RootProvisioner
	DialRPC                func(context.Context, string) (io.ReadWriteCloser, error)
	ModelPolicy            config.SessionModelPolicy
	Workspace              config.WorkspaceStart
	SocketPath             string
	SpawnChild             ChildSpawner
	DescendantClient       DescendantClientFactory
	DirectChildRecoverer   provision.DirectChildRecoverer
	NewSessionID           func() (string, error)
	ChildStopTimeout       time.Duration
	CloseListener          func(context.Context) error
	ReportWrite            func(contract.WriteChildResult) error
	ExpectedPiBinding      *PiBinding
	HandoffVerifier        HandoffVerifier
	ResourcePublished      func(*provision.Resources) error
	RunAttribution         string
	HandoffShutdownTimeout time.Duration
}

type Node struct {
	identity Identity
	deps     Dependencies
	broker   *EventBroker

	mu              sync.RWMutex
	started         bool
	stopRequested   bool
	startupCancel   context.CancelFunc
	startupDone     chan struct{}
	startupDoneOnce sync.Once
	local           *LocalSession
	resources       *provision.Resources
	state           LifecycleState
	children        *childRegistry

	handoffMu       sync.Mutex
	handoffResult   *contract.WriteChildResult
	handoffComplete bool
	reportWrite     func(contract.WriteChildResult) error

	stopFinalizerOnce   sync.Once
	handoffWatchdogOnce sync.Once
	finishOnce          sync.Once
	finishErr           error
	done                chan struct{}
}

func NewRoot(identity Identity, deps Dependencies, broker *EventBroker) (*Node, error) {
	snapshot := identity.Snapshot()
	if snapshot.Kind != contract.ChildKindRoot || snapshot.SessionID != snapshot.RootID || snapshot.ParentID != "" {
		return nil, invariantf("NewRoot requires a root identity")
	}
	return newNode(identity, deps, broker)
}

func NewChild(identity Identity, deps Dependencies, broker *EventBroker) (*Node, error) {
	snapshot := identity.Snapshot()
	if snapshot.Kind != contract.ChildKindRead && snapshot.Kind != contract.ChildKindWrite {
		return nil, invariantf("NewChild requires a read or write identity")
	}
	if snapshot.ParentID == "" || snapshot.SessionID == snapshot.RootID {
		return nil, invariantf("NewChild requires a valid descendant identity")
	}
	return newNode(identity, deps, broker)
}

func newNode(identity Identity, deps Dependencies, broker *EventBroker) (*Node, error) {
	if deps.Provisioner == nil {
		return nil, invariantf("root provisioner is required")
	}
	if deps.DialRPC == nil {
		return nil, invariantf("Pi RPC dialer is required")
	}
	if err := deps.ModelPolicy.Validate(); err != nil {
		return nil, invariantf("session model policy is invalid: %v", err)
	}
	deps.ModelPolicy = deps.ModelPolicy.Clone()
	identitySnapshot := identity.Snapshot()
	if identitySnapshot.Kind != contract.ChildKindRoot {
		if _, err := deps.ModelPolicy.ResolveWorker(identitySnapshot.Worker); err != nil {
			return nil, invariantf("selected worker model is unavailable: %v", err)
		}
	}
	if deps.CloseListener == nil {
		return nil, invariantf("listener lifecycle hook is required")
	}
	if deps.SocketPath == "" || !filepath.IsAbs(deps.SocketPath) {
		return nil, invariantf("root supervisor socket path must be absolute")
	}
	if broker == nil {
		broker = NewEventBroker()
	}
	return &Node{
		identity:    identity,
		deps:        deps,
		broker:      broker,
		state:       LifecycleProvisioning,
		children:    newChildRegistry(),
		reportWrite: deps.ReportWrite,
		startupDone: make(chan struct{}),
		done:        make(chan struct{}),
	}, nil
}

func (node *Node) Start(ctx context.Context) error {
	node.mu.Lock()
	if node.started {
		node.mu.Unlock()
		return invariantf("supervisor node has already been started")
	}
	if node.stopRequested {
		node.mu.Unlock()
		return contract.NewError(contract.ErrorSessionStopping, "supervisor node was stopped before startup")
	}
	node.started = true
	startupCtx, cancel := context.WithCancel(ctx)
	node.startupCancel = cancel
	node.mu.Unlock()
	defer func() {
		cancel()
		node.markStartupDone()
	}()

	if err := validateBoundSupervisorSocket(node.deps.SocketPath); err != nil {
		node.failStart(ctx, err)
		return node.finishedError()
	}

	identity := node.identity.Snapshot()
	selectedModel := node.deps.ModelPolicy.Root
	if identity.Kind != contract.ChildKindRoot {
		worker, resolveErr := node.deps.ModelPolicy.ResolveWorker(identity.Worker)
		if resolveErr != nil {
			node.failStart(ctx, invariantf("resolve selected worker model: %v", resolveErr))
			return node.finishedError()
		}
		selectedModel = config.ModelProfile{Provider: worker.Provider, Model: worker.Model, ThinkingLevel: worker.ThinkingLevel}
	}
	resources, err := node.deps.Provisioner.ProvisionRoot(startupCtx, provision.RootRequest{
		SessionID:      identity.SessionID,
		SocketPath:     node.deps.SocketPath,
		Model:          node.deps.ModelPolicy.Root,
		Workspace:      node.deps.Workspace,
		RunAttribution: node.deps.RunAttribution,
	})
	node.mu.Lock()
	if resources != nil {
		node.resources = resources
	}
	stopRequested := node.stopRequested
	if err == nil && resources != nil && !stopRequested {
		node.state = LifecycleStarting
	}
	node.mu.Unlock()
	if stopRequested {
		return errors.Join(contract.NewError(contract.ErrorSessionStopping, "root startup was cancelled by stop"), err, startupCtx.Err())
	}
	if err != nil {
		node.failStart(ctx, err)
		return node.finishedError()
	}
	if resources == nil {
		node.failStart(ctx, invariantf("root provisioner returned nil resources"))
		return node.finishedError()
	}
	if node.deps.ResourcePublished != nil {
		if err := node.deps.ResourcePublished(resources); err != nil {
			node.failStart(ctx, fmt.Errorf("publish direct-child resource ownership: %w", err))
			return node.finishedError()
		}
	}

	connection, err := node.deps.DialRPC(startupCtx, resources.RPCAddr)
	if err != nil {
		if node.startupWasStopped() {
			return errors.Join(contract.NewError(contract.ErrorSessionStopping, "root startup was cancelled while dialing Pi"), err)
		}
		node.failStart(ctx, fmt.Errorf("dial Pi RPC at %q: %w", resources.RPCAddr, err))
		return node.finishedError()
	}
	node.mu.RLock()
	stopRequested = node.stopRequested
	node.mu.RUnlock()
	if stopRequested {
		if connection != nil {
			_ = connection.Close()
		}
		return contract.NewError(contract.ErrorSessionStopping, "root startup was cancelled before Pi binding")
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
	if err := local.BindExpected(startupCtx, PiExpectation{Binding: node.deps.ExpectedPiBinding, Model: selectedModel}); err != nil {
		if node.startupWasStopped() {
			return errors.Join(contract.NewError(contract.ErrorSessionStopping, "root startup was cancelled while binding Pi"), err)
		}
		node.failStart(ctx, fmt.Errorf("bind Pi session: %w", err))
		return node.finishedError()
	}

	node.mu.RLock()
	stopRequested = node.stopRequested
	node.mu.RUnlock()
	if stopRequested {
		return contract.NewError(contract.ErrorSessionStopping, "root startup was cancelled after Pi binding")
	}
	go node.watchRPC(rpc, local)
	return nil
}

func (node *Node) handoffShutdownTimeout() time.Duration {
	if node.deps.HandoffShutdownTimeout > 0 {
		return node.deps.HandoffShutdownTimeout
	}
	return 5 * time.Second
}

func (node *Node) armHandoffShutdownWatchdog() {
	node.handoffWatchdogOnce.Do(func() {
		go func() {
			timer := time.NewTimer(node.handoffShutdownTimeout())
			defer timer.Stop()
			select {
			case <-node.done:
				return
			case <-timer.C:
				node.finish(context.Background(), nil, LifecycleCompleted, true)
			}
		}()
	})
}

func (node *Node) childStopTimeout() time.Duration {
	if node.deps.ChildStopTimeout > 0 {
		return node.deps.ChildStopTimeout
	}
	return stopCleanupTimeout
}

func (node *Node) childEscalationGrace() time.Duration {
	grace := node.childStopTimeout() / 10
	if grace < 10*time.Millisecond {
		return 10 * time.Millisecond
	}
	if grace > 250*time.Millisecond {
		return 250 * time.Millisecond
	}
	return grace
}

func randomSessionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate child session ID: %w", err)
	}
	return "session-" + hex.EncodeToString(value[:]), nil
}

func (node *Node) CreateChild(ctx context.Context, parent string, request contract.CreateChildRequest) (TerminalResult, error) {
	identity := node.identity.Snapshot()
	if parent != identity.SessionID {
		return TerminalResult{}, contract.NewError(contract.ErrorNotFound, "children may only be created by their direct parent")
	}
	if err := request.Validate(); err != nil {
		return TerminalResult{}, err
	}
	// Resolve trusted worker policy before allocating an ID, socket, registry
	// entry, or process.
	if _, err := node.deps.ModelPolicy.ResolveWorker(request.WorkerType); err != nil {
		return TerminalResult{}, contract.NewError(contract.ErrorUnknownWorkerType, err.Error())
	}
	if node.deps.SpawnChild == nil || node.deps.DescendantClient == nil {
		return TerminalResult{}, contract.NewError(contract.ErrorInternal, "child runtime dependencies are unavailable")
	}
	newID := node.deps.NewSessionID
	if newID == nil {
		newID = randomSessionID
	}
	childID, idErr := newID()
	if idErr != nil {
		return TerminalResult{}, idErr
	}

	node.mu.Lock()
	if node.stopRequested || node.state == LifecycleStopping || node.state == LifecycleStopped || node.state == LifecycleFailed {
		node.mu.Unlock()
		return TerminalResult{}, contract.NewError(contract.ErrorSessionStopping, "session is not accepting new children")
	}
	resources := node.resources
	if resources == nil {
		node.mu.Unlock()
		return TerminalResult{}, contract.NewError(contract.ErrorChildUnavailable, "parent resources are not ready")
	}
	childSocket := filepath.Join(filepath.Dir(node.deps.SocketPath), childID+".sock")
	spawnCtx, spawnCancel := context.WithCancel(context.WithoutCancel(ctx))
	entry := &childEntry{
		id: childID, socket: childSocket, spawnCancel: spawnCancel,
		fallback: NodeSnapshot{
			SessionID: childID, ParentSessionID: identity.SessionID, RootSessionID: identity.RootID,
			Kind: request.Kind, Context: request.Context, WorkerType: request.WorkerType,
			Lifecycle: string(LifecycleStarting), Questions: []QuestionSummary{}, Children: []NodeSnapshot{},
		},
	}
	if err := node.children.add(entry); err != nil {
		node.mu.Unlock()
		return TerminalResult{}, err
	}
	node.mu.Unlock()

	bootstrap := process.Bootstrap{
		SessionID: childID, ParentID: identity.SessionID, RootID: identity.RootID,
		SocketPath: childSocket, SourceInstance: resources.Instance, SourceVolume: resources.Volume,
		Policy: node.deps.ModelPolicy.Clone(), Workspace: node.deps.Workspace,
		Request: request, RunAttribution: node.deps.RunAttribution,
	}
	child, err := node.deps.SpawnChild(spawnCtx, bootstrap)
	if err != nil {
		entry.markSpawnDone()
		return TerminalResult{}, errors.Join(contract.NewError(contract.ErrorChildFailed, "start child supervisor failed"), node.failChildCreation(ctx, entry, err))
	}
	entry.setProcess(child)
	readyErr := child.WaitReady(ctx)
	ticket, ok := child.RecoveryTicket()
	if ok {
		entry.setRecovery(ticket)
	}
	if readyErr != nil {
		return TerminalResult{}, node.failChildCreation(ctx, entry, readyErr)
	}
	if !ok {
		return TerminalResult{}, node.failChildCreation(ctx, entry, fmt.Errorf("child did not publish a direct-parent recovery ticket"))
	}
	client, err := node.deps.DescendantClient(childSocket)
	if err != nil {
		return TerminalResult{}, node.failChildCreation(ctx, entry, childUnavailable(childID, err))
	}
	snapshot, err := client.Snapshot(ctx)
	if err == nil {
		err = validateDirectChildSnapshot(snapshot, childID, identity.SessionID, identity.RootID)
	}
	if err != nil {
		var closeErr error
		if closer, ok := client.(descendantCloser); ok {
			closeErr = closer.Close()
		}
		identityErr := errors.Join(contract.NewError(contract.ErrorChildFailed, "child supervisor socket identity does not match admission"), err, closeErr)
		return TerminalResult{}, node.failChildCreation(ctx, entry, identityErr)
	}
	entry.setClient(client)
	node.startChildEventForwarder(entry)

	message, err := child.NextMessage(ctx)
	if err != nil {
		// External cancellation linearized before a terminal report; this expected
		// stream end is a cancellation, never a child_failed. Wait for the one
		// bounded cleanup already running and report exact child_aborted.
		if entry.isCancelled() {
			return TerminalResult{}, node.awaitChildCancellation(ctx, entry)
		}
		return TerminalResult{}, node.failChildCreation(ctx, entry, err)
	}
	if message.SessionID != childID {
		return TerminalResult{}, node.failChildCreation(ctx, entry, fmt.Errorf("terminal report session ID %q does not match %q", message.SessionID, childID))
	}
	var result TerminalResult
	var terminalErr error
	switch message.Type {
	case process.MessageRead:
		if request.Kind != contract.ChildKindRead {
			terminalErr = contract.NewError(contract.ErrorChildFailed, "child terminal report kind does not match admitted request")
		} else if message.Read.WorkerType != request.WorkerType {
			terminalErr = contract.NewError(contract.ErrorChildFailed, "child terminal report worker does not match admitted worker")
		} else {
			result.Read = message.Read
		}
	case process.MessageWrite:
		if request.Kind != contract.ChildKindWrite {
			terminalErr = contract.NewError(contract.ErrorChildFailed, "child terminal report kind does not match admitted request")
		} else if message.Write.WorkerType != request.WorkerType {
			terminalErr = contract.NewError(contract.ErrorChildFailed, "child terminal report worker does not match admitted worker")
		} else {
			result.Write = message.Write
		}
	case process.MessageFailure:
		terminalErr = contract.NewError(message.Error.Code, message.Error.Message)
	default:
		return TerminalResult{}, node.failChildCreation(ctx, entry, contract.NewError(contract.ErrorChildFailed, "child returned a non-terminal report"))
	}

	// The terminal report has now been ingested and checked against the exact
	// admission. If an external cancellation already won the disposition, never
	// acknowledge a terminal; await the running cancellation and report
	// child_aborted instead.
	if !entry.claimAccepted() {
		return TerminalResult{}, node.awaitChildCancellation(ctx, entry)
	}
	// Mark SSE closure expected before acknowledging that same report. The child
	// cannot return from Reporter.Read/Write/Failure and begin teardown until this
	// inherited protocol write succeeds.
	entry.expectEventStreamClose()
	if err := child.AcknowledgeTerminal(message); err != nil {
		// A cancellation that won during the acknowledgement race is still an
		// external cancellation, not a child_failed acknowledgement fault.
		if entry.isCancelled() {
			return TerminalResult{}, node.awaitChildCancellation(ctx, entry)
		}
		return TerminalResult{}, node.failChildCreation(ctx, entry, errors.Join(contract.NewError(contract.ErrorChildFailed, "acknowledge child terminal report failed"), err))
	}

	cleanupCtx, cancel := context.WithTimeout(ctx, node.childStopTimeout())
	cleanupErr := node.cleanupChild(cleanupCtx, entry, false)
	cancel()
	if cleanupErr != nil && terminalErr == nil {
		terminalErr = contract.NewError(contract.ErrorChildFailed, "child process or resource cleanup failed")
	}
	return result, errors.Join(terminalErr, cleanupErr)
}

func validateDirectChildSnapshot(snapshot NodeSnapshot, childID, parentID, rootID string) error {
	if snapshot.SessionID != childID {
		return fmt.Errorf("child tree session ID %q does not match admitted child %q", snapshot.SessionID, childID)
	}
	if snapshot.ParentSessionID != parentID {
		return fmt.Errorf("child tree parent ID %q does not match direct parent %q", snapshot.ParentSessionID, parentID)
	}
	if snapshot.RootSessionID != rootID {
		return fmt.Errorf("child tree root ID %q does not match admitted root %q", snapshot.RootSessionID, rootID)
	}
	return nil
}

func (node *Node) failChildCreation(requestCtx context.Context, entry *childEntry, primary error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(requestCtx), node.childStopTimeout())
	defer cancel()
	cleanupErr := node.cleanupChild(cleanupCtx, entry, true)
	var typed *contract.Error
	if !errors.As(primary, &typed) {
		primary = errors.Join(contract.NewError(contract.ErrorChildFailed, "child supervisor failed"), primary)
	}
	return errors.Join(primary, cleanupErr)
}

func (node *Node) CallRPC(ctx context.Context, command json.RawMessage) (json.RawMessage, error) {
	node.mu.RLock()
	local := node.local
	state := node.state
	node.mu.RUnlock()
	if state == LifecycleFailed || state == LifecycleStopping || state == LifecycleStopped {
		return nil, contract.NewError(contract.ErrorSessionStopping, "root session is not available")
	}
	if local == nil {
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
		if state == LifecycleFailed || state == LifecycleStopping || state == LifecycleStopped {
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
	names := node.deps.ModelPolicy.WorkerNames()
	result := make([]contract.WorkerSummary, 0, len(names))
	for _, name := range names {
		worker := node.deps.ModelPolicy.Workers[name]
		result = append(result, contract.WorkerSummary{
			WorkerType: name, Description: worker.Description,
			Profile: config.ModelProfile{Provider: worker.Provider, Model: worker.Model, ThinkingLevel: worker.ThinkingLevel},
		})
	}
	return result
}

func (node *Node) watchRPC(rpc *pirpc.Client, local *LocalSession) {
	<-rpc.Done()
	<-local.DrainDone()
	if node.startupWasStopped() {
		return
	}
	if node.handoffAccepted() {
		node.finish(context.Background(), nil, LifecycleCompleted, false)
		return
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

func (node *Node) startupWasStopped() bool {
	node.mu.RLock()
	defer node.mu.RUnlock()
	return node.stopRequested
}

func (node *Node) requestStop(ctx context.Context) {
	node.mu.Lock()
	firstRequest := !node.stopRequested
	node.stopRequested = true
	if node.state != LifecycleStopped && node.state != LifecycleFailed {
		node.state = LifecycleStopping
	}
	if node.startupCancel != nil {
		node.startupCancel()
	}
	if !node.started {
		node.markStartupDone()
	}
	node.mu.Unlock()

	if firstRequest {
		detached := context.WithoutCancel(ctx)
		node.stopFinalizerOnce.Do(func() {
			go node.finalizeStop(detached)
		})
	}
}

func (node *Node) markStartupDone() {
	node.startupDoneOnce.Do(func() { close(node.startupDone) })
}

func (node *Node) finish(ctx context.Context, primary error, terminal LifecycleState, closeRPC bool) {
	node.finishOnce.Do(func() {
		// Closing admission precedes the child snapshot. The registry itself is
		// never held while child HTTP, process waits, or cleanup runs.
		node.mu.Lock()
		node.stopRequested = true
		local := node.local
		resources := node.resources
		node.mu.Unlock()

		var cleanupErr error
		if node.children != nil {
			// Descendant stop and process escalation may consume their entire
			// deadline. Keep that phase from starving this node's own teardown.
			childrenCtx, cancelChildren := context.WithTimeout(context.WithoutCancel(ctx), node.childStopTimeout())
			cleanupErr = errors.Join(cleanupErr, node.stopChildren(childrenCtx))
			cancelChildren()
		}
		if local != nil {
			if closeRPC {
				cleanupErr = errors.Join(cleanupErr, local.StopRPC())
			} else {
				<-local.DrainDone()
				local.questions.Clear()
			}
		}
		if resources != nil {
			resourcesCtx, cancelResources := context.WithTimeout(context.WithoutCancel(ctx), node.childStopTimeout())
			cleanupErr = errors.Join(cleanupErr, node.deps.Provisioner.Destroy(resourcesCtx, resources))
			cancelResources()
		}
		if node.broker != nil {
			node.broker.Close()
		}
		if node.deps.CloseListener != nil {
			listenerCtx, cancelListener := context.WithTimeout(context.WithoutCancel(ctx), node.childStopTimeout())
			cleanupErr = errors.Join(cleanupErr, node.deps.CloseListener(listenerCtx))
			cancelListener()
		}

		node.mu.Lock()
		node.state = terminal
		node.finishErr = errors.Join(primary, cleanupErr)
		node.mu.Unlock()
		close(node.done)
	})
}
