package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/process"
	"github.com/sklarsa/kanedias/internal/supervisor/provision"
)

type childTestWorkers struct{}

func (childTestWorkers) Resolve(name string) (config.WorkerProfile, error) {
	if name != "reviewer" {
		return config.WorkerProfile{}, contract.NewError(contract.ErrorUnknownWorkerType, "unknown worker")
	}
	return config.WorkerProfile{Description: "review", Provider: "provider", Model: "model"}, nil
}
func (childTestWorkers) Summaries() []contract.WorkerSummary { return nil }

type fakeChildProcess struct {
	readyEntered chan struct{}
	readyRelease <-chan struct{}
	messages     chan process.ChildMessage
	done         chan struct{}
	waitErr      error
	autoFinish   bool
	closeOnce    sync.Once
	liveness     atomic.Int32
	terminated   atomic.Int32
	killed       atomic.Int32
}

func newFakeChildProcess() *fakeChildProcess {
	return &fakeChildProcess{messages: make(chan process.ChildMessage, 1), done: make(chan struct{})}
}
func (child *fakeChildProcess) finish() { child.closeOnce.Do(func() { close(child.done) }) }
func (child *fakeChildProcess) WaitReady(ctx context.Context) error {
	if child.readyEntered != nil {
		child.readyEntered <- struct{}{}
	}
	if child.readyRelease != nil {
		select {
		case <-child.readyRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (child *fakeChildProcess) NextMessage(ctx context.Context) (process.ChildMessage, error) {
	select {
	case message := <-child.messages:
		if child.autoFinish {
			child.finish()
		}
		return message, nil
	case <-child.done:
		return process.ChildMessage{}, io.EOF
	case <-ctx.Done():
		return process.ChildMessage{}, ctx.Err()
	}
}
func (child *fakeChildProcess) CloseLiveness() error {
	child.liveness.Add(1)
	child.finish()
	return nil
}
func (child *fakeChildProcess) CloseReports() error   { return nil }
func (child *fakeChildProcess) Done() <-chan struct{} { return child.done }
func (child *fakeChildProcess) Wait() error           { <-child.done; return child.waitErr }
func (child *fakeChildProcess) Terminate() error      { child.terminated.Add(1); child.finish(); return nil }
func (child *fakeChildProcess) Kill() error           { child.killed.Add(1); child.finish(); return nil }

type stoppingDescendant struct {
	fakeDescendantClient
	process *fakeChildProcess
	started chan struct{}
	release <-chan struct{}
	active  *atomic.Int32
	maximum *atomic.Int32
}

func (client *stoppingDescendant) Stop(_ context.Context, target string) error {
	client.mu.Lock()
	client.stops = append(client.stops, target)
	client.mu.Unlock()
	if client.active != nil {
		current := client.active.Add(1)
		for {
			old := client.maximum.Load()
			if current <= old || client.maximum.CompareAndSwap(old, current) {
				break
			}
		}
		defer client.active.Add(-1)
	}
	if client.started != nil {
		client.started <- struct{}{}
	}
	if client.release != nil {
		<-client.release
	}
	client.process.finish()
	return nil
}

func childCreationNode(t *testing.T, spawn ChildSpawner, factory DescendantClientFactory) *Node {
	t.Helper()
	node := &Node{
		identity: testRootIdentity(t), broker: NewEventBroker(), state: LifecycleReady, started: true,
		resources: &provision.Resources{SessionID: "root-1", Instance: "instance", Volume: "volume"},
		children:  newChildRegistry(), startupDone: make(chan struct{}), done: make(chan struct{}),
		deps: Dependencies{Workers: childTestWorkers{}, SocketPath: "/tmp/root.sock", SpawnChild: spawn, DescendantClient: factory, ChildStopTimeout: 100 * time.Millisecond, CloseListener: func(context.Context) error { return nil }},
	}
	close(node.startupDone)
	return node
}

func readRequest() contract.CreateChildRequest {
	return contract.CreateChildRequest{WorkerType: "reviewer", Kind: contract.ChildKindRead, Context: contract.ContextFresh, Task: "review"}
}

func directChildSnapshot(id string) NodeSnapshot {
	return NodeSnapshot{SessionID: id, ParentSessionID: "root-1", RootSessionID: "root-1", Kind: contract.ChildKindRead, Context: contract.ContextFresh, WorkerType: "reviewer", Lifecycle: string(LifecycleReady), Questions: []QuestionSummary{}, Children: []NodeSnapshot{}}
}

func TestCreateChildResolvesWorkerBeforeAnyProcessSideEffect(t *testing.T) {
	var spawned atomic.Bool
	node := childCreationNode(t, func(context.Context, process.Bootstrap) (ChildProcess, error) {
		spawned.Store(true)
		return nil, errors.New("unexpected spawn")
	}, func(string) (DescendantClient, error) { return nil, errors.New("unexpected client") })
	request := readRequest()
	request.WorkerType = "unknown"
	_, err := node.CreateChild(context.Background(), "root-1", request)
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorUnknownWorkerType {
		t.Fatalf("error = %v", err)
	}
	if spawned.Load() || len(node.children.snapshot()) != 0 {
		t.Fatal("unknown worker caused child side effects")
	}
}

func TestStopDuringSpawnCancelsAdmissionAndWaitsForSpawnCleanup(t *testing.T) {
	spawnEntered := make(chan struct{})
	node := childCreationNode(t, func(ctx context.Context, _ process.Bootstrap) (ChildProcess, error) {
		close(spawnEntered)
		<-ctx.Done()
		return nil, ctx.Err()
	}, func(string) (DescendantClient, error) { return nil, nil })
	node.deps.NewSessionID = func() (string, error) { return "child-spawning", nil }
	created := make(chan error, 1)
	go func() { _, err := node.CreateChild(context.Background(), "root-1", readRequest()); created <- err }()
	<-spawnEntered
	node.mu.Lock()
	node.resources = nil
	node.mu.Unlock()
	stopped := make(chan error, 1)
	go func() { stopped <- node.Stop(context.Background(), StopReasonRequested) }()
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if err := <-created; err == nil {
		t.Fatal("CreateChild succeeded while parent stopped")
	}
	if len(node.children.snapshot()) != 0 {
		t.Fatal("spawning child remains after stop")
	}
}

func TestCreateChildFailureWaitsForProcessExitBeforeReturningAndRemoval(t *testing.T) {
	child := newFakeChildProcess()
	child.messages <- process.ChildMessage{Type: process.MessageFailure, SessionID: "child-1", Error: &process.WireError{Code: contract.ErrorChildFailed, Message: "failed"}}
	client := &stoppingDescendant{fakeDescendantClient: fakeDescendantClient{snapshot: directChildSnapshot("child-1")}, process: child}
	node := childCreationNode(t,
		func(context.Context, process.Bootstrap) (ChildProcess, error) { return child, nil },
		func(string) (DescendantClient, error) { return client, nil },
	)
	node.deps.NewSessionID = func() (string, error) { return "child-1", nil }
	result := make(chan error, 1)
	go func() { _, err := node.CreateChild(context.Background(), "root-1", readRequest()); result <- err }()
	select {
	case err := <-result:
		t.Fatalf("CreateChild returned before process exit: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	if node.children.get("child-1") == nil {
		t.Fatal("terminal child disappeared before process exit")
	}
	child.finish()
	err := <-result
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorChildFailed {
		t.Fatalf("error = %v", err)
	}
	if node.children.get("child-1") != nil {
		t.Fatal("child remains after blocked caller received terminal failure")
	}
}

func TestCreateChildCancellationStopsAndSynchronouslyCleansChild(t *testing.T) {
	child := newFakeChildProcess()
	client := &stoppingDescendant{fakeDescendantClient: fakeDescendantClient{snapshot: directChildSnapshot("child-cancel")}, process: child}
	node := childCreationNode(t,
		func(context.Context, process.Bootstrap) (ChildProcess, error) { return child, nil },
		func(string) (DescendantClient, error) { return client, nil },
	)
	node.deps.NewSessionID = func() (string, error) { return "child-cancel", nil }
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := node.CreateChild(ctx, "root-1", readRequest()); result <- err }()
	for node.children.get("child-cancel") == nil {
		time.Sleep(time.Millisecond)
	}
	cancel()
	err := <-result
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	client.mu.Lock()
	stops := append([]string(nil), client.stops...)
	client.mu.Unlock()
	if len(stops) != 1 || stops[0] != "child-cancel" {
		t.Fatalf("stops = %v", stops)
	}
	if node.children.get("child-cancel") != nil {
		t.Fatal("cancelled child remains registered")
	}
	select {
	case <-child.Done():
	default:
		t.Fatal("cancellation returned before process exit")
	}
}

func TestSiblingChildStartupRunsConcurrently(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var sequence atomic.Int32
	processes := sync.Map{}
	node := childCreationNode(t,
		func(_ context.Context, bootstrap process.Bootstrap) (ChildProcess, error) {
			child := newFakeChildProcess()
			child.readyEntered, child.readyRelease, child.autoFinish = entered, release, true
			child.messages <- process.ChildMessage{Type: process.MessageRead, SessionID: bootstrap.SessionID, Read: &contract.ReadChildResult{Kind: contract.ChildKindRead, WorkerType: "reviewer", SessionID: bootstrap.SessionID, Output: "done"}}
			processes.Store(bootstrap.SessionID, child)
			return child, nil
		},
		func(socket string) (DescendantClient, error) {
			id := filepath.Base(socket)
			id = id[:len(id)-len(filepath.Ext(id))]
			return &fakeDescendantClient{snapshot: directChildSnapshot(id)}, nil
		},
	)
	node.deps.NewSessionID = func() (string, error) { return "child-" + string(rune('0'+sequence.Add(1))), nil }
	done := make(chan error, 2)
	for range 2 {
		go func() { _, err := node.CreateChild(context.Background(), "root-1", readRequest()); done <- err }()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("sibling startup serialized")
		}
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestStopCascadesToDirectChildrenConcurrentlyAndIdempotently(t *testing.T) {
	active, maximum := &atomic.Int32{}, &atomic.Int32{}
	started, release := make(chan struct{}, 2), make(chan struct{})
	node := childCreationNode(t, nil, nil)
	node.resources = nil
	for _, id := range []string{"child-a", "child-b"} {
		child := newFakeChildProcess()
		client := &stoppingDescendant{process: child, started: started, release: release, active: active, maximum: maximum}
		if err := node.children.add(&childEntry{id: id, process: child, client: client}); err != nil {
			t.Fatal(err)
		}
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- node.Stop(context.Background(), StopReasonRequested) }()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("cascade was not concurrent")
		}
	}
	go func() { second <- node.Stop(context.Background(), StopReasonRequested) }()
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrent stops = %d", maximum.Load())
	}
	if len(node.children.snapshot()) != 0 {
		t.Fatal("children remain after cascade")
	}
}

func TestCreateChildSuccessWaitsForProcessExitAndRejectsTerminalContractMismatch(t *testing.T) {
	tests := []struct {
		name    string
		message process.ChildMessage
	}{
		{name: "kind", message: process.ChildMessage{Type: process.MessageWrite, SessionID: "child-mismatch", Write: &contract.WriteChildResult{Kind: contract.ChildKindWrite, WorkerType: "reviewer", SessionID: "child-mismatch"}}},
		{name: "worker", message: process.ChildMessage{Type: process.MessageRead, SessionID: "child-mismatch", Read: &contract.ReadChildResult{Kind: contract.ChildKindRead, WorkerType: "other", SessionID: "child-mismatch", Output: "wrong worker"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			child := newFakeChildProcess()
			child.messages <- test.message
			client := &fakeDescendantClient{snapshot: directChildSnapshot("child-mismatch")}
			node := childCreationNode(t,
				func(context.Context, process.Bootstrap) (ChildProcess, error) { return child, nil },
				func(string) (DescendantClient, error) { return client, nil },
			)
			node.deps.NewSessionID = func() (string, error) { return "child-mismatch", nil }

			returned := make(chan struct {
				result TerminalResult
				err    error
			}, 1)
			go func() {
				result, err := node.CreateChild(context.Background(), "root-1", readRequest())
				returned <- struct {
					result TerminalResult
					err    error
				}{result, err}
			}()
			select {
			case <-returned:
				t.Fatal("CreateChild returned before mismatched child process exited")
			case <-time.After(25 * time.Millisecond):
			}
			child.finish()
			got := <-returned
			var typed *contract.Error
			if !errors.As(got.err, &typed) || typed.Code != contract.ErrorChildFailed {
				t.Fatalf("error = %v, want typed child_failed", got.err)
			}
			if got.result.Read != nil || got.result.Write != nil {
				t.Fatalf("mismatched terminal result leaked as success: %#v", got.result)
			}
			if node.children.get("child-mismatch") != nil {
				t.Fatal("mismatched child remains registered after return")
			}
		})
	}
}

func TestCreateChildValidatesInstalledDescendantIdentityBeforeReadiness(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*NodeSnapshot)
	}{
		{name: "stale session", edit: func(snapshot *NodeSnapshot) { snapshot.SessionID = "old-child" }},
		{name: "wrong parent", edit: func(snapshot *NodeSnapshot) { snapshot.ParentSessionID = "other-parent" }},
		{name: "wrong root", edit: func(snapshot *NodeSnapshot) { snapshot.RootSessionID = "other-root" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			child := newFakeChildProcess()
			snapshot := directChildSnapshot("child-identity")
			test.edit(&snapshot)
			client := &stoppingDescendant{fakeDescendantClient: fakeDescendantClient{snapshot: snapshot}, process: child}
			node := childCreationNode(t,
				func(context.Context, process.Bootstrap) (ChildProcess, error) { return child, nil },
				func(string) (DescendantClient, error) { return client, nil },
			)
			node.deps.NewSessionID = func() (string, error) { return "child-identity", nil }
			_, err := node.CreateChild(context.Background(), "root-1", readRequest())
			var typed *contract.Error
			if !errors.As(err, &typed) || typed.Code != contract.ErrorChildFailed {
				t.Fatalf("error = %v, want typed child_failed", err)
			}
			if node.children.get("child-identity") != nil {
				t.Fatal("identity-mismatched descendant was installed")
			}
		})
	}
}

func TestCleanupChildNeverUnlinksChildOwnedReplacementSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "child.sock")
	replacement, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	child := newFakeChildProcess()
	child.finish()
	node := childCreationNode(t, nil, nil)
	entry := &childEntry{id: "child-replaced", socket: path, process: child}
	if err := node.children.add(entry); err != nil {
		t.Fatal(err)
	}
	if err := node.cleanupChild(context.Background(), entry, false); err != nil {
		t.Fatalf("cleanupChild() error = %v", err)
	}
	connection, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("replacement socket was parent-unlinked: %v", err)
	}
	_ = connection.Close()
}

type deadlineDescendant struct{ deadlineSeen atomic.Bool }

func (client *deadlineDescendant) Snapshot(context.Context) (NodeSnapshot, error) {
	return NodeSnapshot{}, nil
}
func (client *deadlineDescendant) Subscribe(context.Context) (Subscription, error) {
	return Subscription{}, nil
}
func (client *deadlineDescendant) CallRPC(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}
func (client *deadlineDescendant) AnswerQuestion(context.Context, string, string, json.RawMessage) error {
	return nil
}
func (client *deadlineDescendant) Stop(ctx context.Context, _ string) error {
	_, hasDeadline := ctx.Deadline()
	client.deadlineSeen.Store(hasDeadline)
	<-ctx.Done()
	return ctx.Err()
}

type escalatingChildProcess struct {
	done       chan struct{}
	closeOnce  sync.Once
	terminated atomic.Int32
	killed     atomic.Int32
}

func newEscalatingChildProcess() *escalatingChildProcess {
	return &escalatingChildProcess{done: make(chan struct{})}
}
func (*escalatingChildProcess) WaitReady(context.Context) error { return nil }
func (*escalatingChildProcess) NextMessage(context.Context) (process.ChildMessage, error) {
	return process.ChildMessage{}, io.EOF
}
func (*escalatingChildProcess) CloseLiveness() error        { return nil }
func (*escalatingChildProcess) CloseReports() error         { return nil }
func (child *escalatingChildProcess) Done() <-chan struct{} { return child.done }
func (child *escalatingChildProcess) Wait() error           { <-child.done; return nil }
func (child *escalatingChildProcess) Terminate() error      { child.terminated.Add(1); return nil }
func (child *escalatingChildProcess) Kill() error {
	child.killed.Add(1)
	child.closeOnce.Do(func() { close(child.done) })
	return nil
}

type phaseRootProvisioner struct {
	destroyStarted chan context.Context
	destroyRelease <-chan struct{}
	destroyDone    chan struct{}
	waitForTimeout bool
}

func (*phaseRootProvisioner) ProvisionRoot(context.Context, provision.RootRequest) (*provision.Resources, error) {
	return nil, errors.New("unused")
}

func (p *phaseRootProvisioner) Destroy(ctx context.Context, _ *provision.Resources) error {
	p.destroyStarted <- ctx
	defer close(p.destroyDone)
	if p.waitForTimeout {
		<-ctx.Done()
		return ctx.Err()
	}
	select {
	case <-p.destroyRelease:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func assertLiveBoundedContext(t *testing.T, phase string, ctx context.Context) time.Time {
	t.Helper()
	if err := ctx.Err(); err != nil {
		t.Errorf("%s context already expired: %v", phase, err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Errorf("%s context has no deadline", phase)
	}
	return deadline
}

func TestUnexpectedFinishUsesIndependentDeadlinesForDescendantsResourcesAndListener(t *testing.T) {
	path, listener := boundSocket(t)
	original, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	resourceRelease := make(chan struct{})
	resources := &phaseRootProvisioner{
		destroyStarted: make(chan context.Context, 1),
		destroyRelease: resourceRelease,
		destroyDone:    make(chan struct{}),
	}
	listenerStarted := make(chan context.Context, 1)
	listenerRelease := make(chan struct{})
	listenerDone := make(chan struct{})
	identityChecked := atomic.Bool{}

	node := childCreationNode(t, nil, nil)
	node.deps.Provisioner = resources
	node.deps.ChildStopTimeout = 100 * time.Millisecond
	node.deps.CloseListener = func(ctx context.Context) error {
		listenerStarted <- ctx
		defer close(listenerDone)
		select {
		case <-listenerRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
		current, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !os.SameFile(original, current) {
			return errors.New("listener socket identity changed")
		}
		identityChecked.Store(true)
		if err := listener.Close(); err != nil {
			return err
		}
		return os.Remove(path)
	}
	client := &deadlineDescendant{}
	child := newEscalatingChildProcess()
	if err := node.children.add(&childEntry{id: "child-hanging", client: client, process: child}); err != nil {
		t.Fatal(err)
	}

	finishReturned := make(chan struct{})
	started := time.Now()
	go func() {
		node.finish(context.Background(), io.EOF, LifecycleFailed, false)
		close(finishReturned)
	}()

	resourceCtx := <-resources.destroyStarted
	resourceDeadline := assertLiveBoundedContext(t, "resource", resourceCtx)
	select {
	case <-resources.destroyDone:
		t.Error("resource destruction returned before its release")
	default:
	}
	select {
	case <-node.Done():
		t.Error("Node.Done closed before resource destruction completed")
	default:
	}
	close(resourceRelease)
	<-resources.destroyDone

	listenerCtx := <-listenerStarted
	listenerDeadline := assertLiveBoundedContext(t, "listener", listenerCtx)
	if !listenerDeadline.After(resourceDeadline) {
		t.Errorf("listener deadline %v is not fresh after resource deadline %v", listenerDeadline, resourceDeadline)
	}
	select {
	case <-listenerDone:
		t.Error("listener cleanup returned before identity-checked cleanup was released")
	default:
	}
	select {
	case <-node.Done():
		t.Error("Node.Done closed before listener cleanup completed")
	default:
	}
	close(listenerRelease)
	<-listenerDone
	<-finishReturned

	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("unexpected finish took %s, want bounded cleanup", elapsed)
	}
	if !client.deadlineSeen.Load() {
		t.Error("unexpected descendant Stop did not receive a bounded cleanup context")
	}
	if child.terminated.Load() != 1 || child.killed.Load() != 1 {
		t.Errorf("escalation TERM=%d KILL=%d, want 1 each", child.terminated.Load(), child.killed.Load())
	}
	if !identityChecked.Load() {
		t.Error("listener did not complete identity-checked socket cleanup")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("listener socket remains after cleanup: %v", err)
	}
	select {
	case <-node.Done():
	default:
		t.Error("Node.Done remains open after all finalization phases")
	}
}

func TestResourcePhaseTimeoutStillGivesListenerFreshDeadline(t *testing.T) {
	resources := &phaseRootProvisioner{
		destroyStarted: make(chan context.Context, 1),
		destroyDone:    make(chan struct{}),
		waitForTimeout: true,
	}
	listenerStarted := make(chan context.Context, 1)
	listenerRelease := make(chan struct{})
	listenerDone := make(chan struct{})

	node := childCreationNode(t, nil, nil)
	node.deps.Provisioner = resources
	node.deps.ChildStopTimeout = 30 * time.Millisecond
	node.deps.CloseListener = func(ctx context.Context) error {
		listenerStarted <- ctx
		defer close(listenerDone)
		select {
		case <-listenerRelease:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	finishReturned := make(chan struct{})
	go func() {
		node.finish(context.Background(), nil, LifecycleStopped, false)
		close(finishReturned)
	}()

	resourceCtx := <-resources.destroyStarted
	resourceDeadline := assertLiveBoundedContext(t, "resource", resourceCtx)
	<-resources.destroyDone
	listenerCtx := <-listenerStarted
	listenerDeadline := assertLiveBoundedContext(t, "listener after resource timeout", listenerCtx)
	if !listenerDeadline.After(resourceDeadline) {
		t.Errorf("listener deadline %v is not fresh after timed-out resource deadline %v", listenerDeadline, resourceDeadline)
	}
	select {
	case <-listenerDone:
		t.Error("listener returned before release after resource timeout")
	default:
	}
	select {
	case <-node.Done():
		t.Error("Node.Done closed before listener completed after resource timeout")
	default:
	}
	close(listenerRelease)
	<-listenerDone
	<-finishReturned
	if err := node.finishedError(); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("finish error = %v, want resource deadline exceeded", err)
	}
}

func TestForwardedGrandchildEventPreservesSourceAndGetsLocalSubtreeSequence(t *testing.T) {
	events := make(chan EventEnvelope, 1)
	client := &fakeDescendantClient{events: Subscription{Replay: []EventEnvelope{{Seq: 99, SessionID: "grandchild", SourceSeq: 7, Kind: "pi", Payload: json.RawMessage(`{"type":"agent_start"}`)}}, Events: events, Close: func() {}}}
	node := routingNode(t, map[string]*fakeDescendantClient{})
	entry := &childEntry{id: "child", client: client}
	node.startChildEventForwarder(entry)
	deadline := time.Now().Add(time.Second)
	for {
		sub := node.broker.Subscribe()
		if len(sub.Replay) > 0 {
			event := sub.Replay[0]
			sub.Close()
			if event.Seq != 1 || event.SessionID != "grandchild" || event.SourceSeq != 7 {
				t.Fatalf("event = %#v", event)
			}
			break
		}
		sub.Close()
		if time.Now().After(deadline) {
			t.Fatal("forwarded event missing")
		}
		time.Sleep(time.Millisecond)
	}
	close(events)
}
