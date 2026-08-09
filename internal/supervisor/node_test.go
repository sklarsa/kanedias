package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/provision"
)

type fakeRootProvisioner struct {
	mu                 sync.Mutex
	calls              int
	request            provision.RootRequest
	resources          *provision.Resources
	provisionErr       error
	destroyErr         error
	destroyed          int
	destroyContextErr  error
	destroyHasDeadline bool
	onDestroy          func()
	provisionStarted   chan struct{}
	provisionRelease   chan struct{}
}

func (fake *fakeRootProvisioner) ProvisionRoot(_ context.Context, request provision.RootRequest) (*provision.Resources, error) {
	fake.mu.Lock()
	fake.calls++
	fake.request = request
	started, release := fake.provisionStarted, fake.provisionRelease
	resources, err := fake.resources, fake.provisionErr
	fake.mu.Unlock()
	if started != nil {
		close(started)
	}
	if release != nil {
		<-release
	}
	return resources, err
}
func (fake *fakeRootProvisioner) Destroy(ctx context.Context, _ *provision.Resources) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.destroyed++
	fake.destroyContextErr = ctx.Err()
	_, fake.destroyHasDeadline = ctx.Deadline()
	if fake.onDestroy != nil {
		fake.onDestroy()
	}
	return fake.destroyErr
}

func testModelPolicy() config.SessionModelPolicy {
	return config.SessionModelPolicy{
		Root: config.ModelProfile{Provider: "openai-codex", Model: "gpt-5.6-sol", ThinkingLevel: "high"},
		Workers: map[string]config.WorkerProfile{
			"reviewer": {Description: "Review", Provider: "openai-codex", Model: "gpt-5.6-sol", ThinkingLevel: "high"},
			"worker":   {Description: "Implement", Provider: "openai-codex", Model: "gpt-5.6-sol", ThinkingLevel: "high"},
		},
	}
}

type trackedConn struct {
	io.ReadWriteCloser
	closed atomic.Bool
}

func (conn *trackedConn) Close() error { conn.closed.Store(true); return conn.ReadWriteCloser.Close() }

func testRootIdentity(t *testing.T) Identity {
	t.Helper()
	identity, err := NewIdentity(IdentitySpec{SessionID: "root-1", RootID: "root-1", Kind: contract.ChildKindRoot, Context: contract.ContextRoot})
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func boundSocket(t *testing.T) (string, net.Listener) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "supervisor.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return path, listener
}

func startPiPeer(t *testing.T, peer net.Conn, beforeResponse json.RawMessage) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		reader := bufio.NewReader(peer)
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var command struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &command) != nil || command.Type != "get_state" {
			return
		}
		if beforeResponse != nil {
			_, _ = peer.Write(append(append([]byte(nil), beforeResponse...), '\n'))
		}
		response := map[string]any{"id": command.ID, "type": "response", "command": "get_state", "success": true, "data": map[string]any{
			"sessionId": "pi-1", "sessionFile": "/tmp/pi-1.jsonl", "isStreaming": false,
			"model": map[string]any{"provider": "openai-codex", "id": "gpt-5.6-sol"}, "thinkingLevel": "high",
		}}
		wire, _ := json.Marshal(response)
		_, _ = peer.Write(append(wire, '\n'))
	}()
	return done
}

func TestForkChildBindingMismatchFailsBeforeReadyAndCleansEverything(t *testing.T) {
	socket, listener := boundSocket(t)
	clientConn, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	connection := &trackedConn{ReadWriteCloser: clientConn}
	fake := &fakeRootProvisioner{resources: &provision.Resources{SessionID: "writer", Instance: "instance", Volume: "volume", RPCAddr: "rpc"}}
	listenerClosed := false
	node, err := NewChild(writerIdentity(t), Dependencies{
		Provisioner: fake, SocketPath: socket,
		DialRPC:     func(context.Context, string) (io.ReadWriteCloser, error) { return connection, nil },
		ModelPolicy: testModelPolicy(), ExpectedPiBinding: &PiBinding{SessionID: "admitted-pi", SessionFile: "/tmp/admitted.jsonl"},
		CloseListener: func(context.Context) error { listenerClosed = true; return listener.Close() },
	}, NewEventBroker())
	if err != nil {
		t.Fatal(err)
	}
	peerDone := startPiPeer(t, peer, nil)
	if err := node.Start(context.Background()); !errors.Is(err, ErrInvariant) {
		t.Fatalf("Start() error = %v, want fork binding invariant", err)
	}
	<-peerDone
	if node.Snapshot().Lifecycle != string(LifecycleFailed) {
		t.Fatalf("lifecycle = %s, want failed", node.Snapshot().Lifecycle)
	}
	if !connection.closed.Load() {
		t.Fatal("Pi RPC was not closed")
	}
	if fake.destroyed != 1 {
		t.Fatalf("destroy calls = %d, want 1", fake.destroyed)
	}
	if !listenerClosed {
		t.Fatal("listener was not closed")
	}
}

func TestNewRootRejectsSocketThatIsNotReadyBeforeProvisioning(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(t *testing.T) string
	}{
		{"empty", func(*testing.T) string { return "" }},
		{"relative", func(*testing.T) string { return "supervisor.sock" }},
		{"absent", func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing.sock") }},
		{"regular", func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"wrong-mode", func(t *testing.T) string {
			p, _ := boundSocket(t)
			if err := os.Chmod(p, 0o660); err != nil {
				t.Fatal(err)
			}
			return p
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeRootProvisioner{resources: &provision.Resources{SessionID: "root-1", RPCAddr: "rpc"}}
			node, err := NewRoot(testRootIdentity(t), Dependencies{Provisioner: fake, SocketPath: test.prepare(t), DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) { t.Fatal("DialRPC called"); return nil, nil }, ModelPolicy: testModelPolicy(), CloseListener: func(context.Context) error { return nil }}, NewEventBroker())
			if test.name == "empty" || test.name == "relative" {
				if err == nil || node != nil {
					t.Fatalf("NewRoot() = (%v, %v), want validation error", node, err)
				}
			} else {
				if err != nil {
					t.Fatalf("NewRoot() error = %v", err)
				}
				if err := node.Start(context.Background()); err == nil {
					t.Fatal("Start() succeeded with unsafe socket")
				}
			}
			if fake.calls != 0 {
				t.Fatalf("ProvisionRoot called %d times", fake.calls)
			}
		})
	}
}

func TestRootNodeProvisioningFailureNeverDialsRPC(t *testing.T) {
	path, _ := boundSocket(t)
	primary := errors.New("proxy unavailable")
	fake := &fakeRootProvisioner{provisionErr: primary}
	node, err := NewRoot(testRootIdentity(t), Dependencies{
		Provisioner: fake,
		SocketPath:  path,
		DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) {
			t.Fatal("DialRPC called after provisioning failure")
			return nil, nil
		},
		ModelPolicy: testModelPolicy(), CloseListener: func(context.Context) error { return nil },
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(context.Background()); !errors.Is(err, primary) {
		t.Fatalf("Start() error = %v, want provisioning error", err)
	}
	if got := node.Snapshot().Lifecycle; got != string(LifecycleFailed) {
		t.Fatalf("lifecycle = %q, want failed", got)
	}
}

func TestNodeClonesPolicyAndReturnsSortedWorkerSummaries(t *testing.T) {
	path, _ := boundSocket(t)
	policy := testModelPolicy()
	policy.Workers["alpha"] = config.WorkerProfile{Description: "First", Provider: "test", Model: "alpha", ThinkingLevel: "off"}
	node, err := NewRoot(testRootIdentity(t), Dependencies{
		Provisioner: &fakeRootProvisioner{}, SocketPath: path,
		DialRPC:     func(context.Context, string) (io.ReadWriteCloser, error) { return nil, errors.New("unused") },
		ModelPolicy: policy, CloseListener: func(context.Context) error { return nil },
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mutated := policy.Workers["alpha"]
	mutated.Description = "mutated after construction"
	policy.Workers["alpha"] = mutated
	delete(policy.Workers, "reviewer")

	summaries := node.WorkerSummaries()
	gotNames := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		gotNames = append(gotNames, summary.WorkerType)
	}
	if got, want := strings.Join(gotNames, ","), "alpha,reviewer,worker"; got != want {
		t.Fatalf("worker order = %q, want %q", got, want)
	}
	if summaries[0].Description != "First" {
		t.Fatalf("cloned worker description = %q, want First", summaries[0].Description)
	}
}

func TestRootNodeTCPConnectionAloneIsNotReady(t *testing.T) {
	path, _ := boundSocket(t)
	host, peer := net.Pipe()
	fake := &fakeRootProvisioner{resources: &provision.Resources{SessionID: "root-1", RPCAddr: "rpc"}}
	node, err := NewRoot(testRootIdentity(t), Dependencies{Provisioner: fake, SocketPath: path, DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) { return host, nil }, ModelPolicy: testModelPolicy(), CloseListener: func(context.Context) error { return nil }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- node.Start(context.Background()) }()
	if _, err := bufio.NewReader(peer).ReadBytes('\n'); err != nil {
		t.Fatal(err)
	}
	if snapshot := node.Snapshot(); snapshot.Lifecycle == string(LifecycleReady) || snapshot.PiSessionID != "" {
		t.Fatalf("TCP dial incorrectly reported readiness: %#v", snapshot)
	}
	_ = peer.Close()
	if err := <-result; err == nil {
		t.Fatal("Start() succeeded without get_state response")
	}
}

func TestRootNodeBindsOnlyAfterGetStateAndBuffersInitialEvent(t *testing.T) {
	path, _ := boundSocket(t)
	host, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	fake := &fakeRootProvisioner{resources: &provision.Resources{SessionID: "root-1", Pool: "btrfs", Instance: "i", Volume: "v", RPCAddr: "rpc"}}
	broker := NewEventBroker()
	node, err := NewRoot(testRootIdentity(t), Dependencies{Provisioner: fake, SocketPath: path, DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) { return host, nil }, ModelPolicy: testModelPolicy(), CloseListener: func(context.Context) error { return nil }}, broker)
	if err != nil {
		t.Fatal(err)
	}
	initial := json.RawMessage(`{"type":"agent_start"}`)
	peerDone := startPiPeer(t, peer, initial)

	if err := node.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	<-peerDone
	snapshot := node.Snapshot()
	if snapshot.PiSessionID != "pi-1" || snapshot.SessionFile != "/tmp/pi-1.jsonl" || snapshot.Lifecycle != string(LifecycleReady) {
		t.Fatalf("Snapshot() = %#v", snapshot)
	}
	sub := broker.Subscribe()
	defer sub.Close()
	var buffered EventEnvelope
	if len(sub.Replay) != 0 {
		buffered = sub.Replay[0]
	} else {
		select {
		case buffered = <-sub.Events:
		case <-time.After(time.Second):
			t.Fatal("initial event was not buffered")
		}
	}
	if string(buffered.Payload) != string(initial) {
		t.Fatalf("buffered event = %#v", buffered)
	}
	if fake.request.SocketPath != path {
		t.Fatalf("provision socket = %q, want %q", fake.request.SocketPath, path)
	}
	if fake.request.Model != testModelPolicy().Root {
		t.Fatalf("provision model = %#v, want %#v", fake.request.Model, testModelPolicy().Root)
	}
}

func TestRootNodeForbiddenRPCIsNotWritten(t *testing.T) {
	path, _ := boundSocket(t)
	host, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	fake := &fakeRootProvisioner{resources: &provision.Resources{SessionID: "root-1", RPCAddr: "rpc"}}
	node, err := NewRoot(testRootIdentity(t), Dependencies{Provisioner: fake, SocketPath: path, DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) { return host, nil }, ModelPolicy: testModelPolicy(), CloseListener: func(context.Context) error { return nil }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	startPiPeer(t, peer, nil)
	if err := node.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := node.CallRPC(context.Background(), json.RawMessage(`{"type":"new_session"}`)); !errors.Is(err, contract.NewError(contract.ErrorForbiddenRPC, "")) {
		var typed *contract.Error
		if !errors.As(err, &typed) || typed.Code != contract.ErrorForbiddenRPC {
			t.Fatalf("CallRPC error = %v", err)
		}
	}
	_ = peer.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := bufio.NewReader(peer).ReadByte(); err == nil {
		t.Fatal("forbidden RPC was written")
	}
}

func TestRootNodeEOFEndsPendingCallAndFailsNode(t *testing.T) {
	path, _ := boundSocket(t)
	host, peer := net.Pipe()
	fake := &fakeRootProvisioner{resources: &provision.Resources{SessionID: "root-1", RPCAddr: "rpc"}}
	node, err := NewRoot(testRootIdentity(t), Dependencies{Provisioner: fake, SocketPath: path, DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) { return host, nil }, ModelPolicy: testModelPolicy(), CloseListener: func(context.Context) error { return nil }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	startPiPeer(t, peer, nil)
	if err := node.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := node.CallRPC(context.Background(), json.RawMessage(`{"type":"get_messages"}`))
		result <- err
	}()
	reader := bufio.NewReader(peer)
	if _, err := reader.ReadBytes('\n'); err != nil {
		t.Fatal(err)
	}
	_ = peer.Close()
	if err := <-result; err == nil {
		t.Fatal("pending CallRPC returned nil after EOF")
	}
	select {
	case <-node.Done():
	case <-time.After(time.Second):
		t.Fatal("node did not finish after Pi EOF")
	}
	if got := node.Snapshot().Lifecycle; got != string(LifecycleFailed) {
		t.Fatalf("lifecycle = %q, want failed", got)
	}
}

func TestRootNodeWaitsForBufferedPiEventsBeforeDoneAndCleanup(t *testing.T) {
	path, _ := boundSocket(t)
	host, peer := net.Pipe()
	fake := &fakeRootProvisioner{resources: &provision.Resources{SessionID: "root-1", Instance: "i", Volume: "v", RPCAddr: "rpc"}}
	broker := NewEventBroker()
	node, err := NewRoot(testRootIdentity(t), Dependencies{Provisioner: fake, SocketPath: path, DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) { return host, nil }, ModelPolicy: testModelPolicy(), CloseListener: func(context.Context) error { return nil }}, broker)
	if err != nil {
		t.Fatal(err)
	}
	startPiPeer(t, peer, nil)
	if err := node.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	sub := broker.Subscribe()
	defer sub.Close()
	broker.mu.Lock()
	writeDone := make(chan struct{})
	go func() {
		_, _ = peer.Write([]byte("{\"type\":\"agent_settled\",\"final\":true}\n"))
		_ = peer.Close()
		close(writeDone)
	}()
	<-writeDone
	select {
	case <-node.Done():
		broker.mu.Unlock()
		t.Fatal("Node.Done closed while a buffered Pi event was still blocked in publication")
	case <-time.After(20 * time.Millisecond):
	}
	broker.mu.Unlock()
	select {
	case <-node.Done():
	case <-time.After(time.Second):
		t.Fatal("Node.Done did not close after event drain")
	}
	select {
	case event := <-sub.Events:
		if !strings.Contains(string(event.Payload), `"final":true`) {
			t.Fatalf("final event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("final buffered event was not delivered before broker close")
	}
	if fake.destroyed != 1 {
		t.Fatalf("Destroy called %d times", fake.destroyed)
	}
}

func TestRootNodeStopIsIdempotentAndClosesPiBeforeDestroy(t *testing.T) {
	path, _ := boundSocket(t)
	host, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	tracked := &trackedConn{ReadWriteCloser: host}
	fake := &fakeRootProvisioner{resources: &provision.Resources{SessionID: "root-1", RPCAddr: "rpc"}}
	var orderMu sync.Mutex
	var order []string
	fake.onDestroy = func() {
		if !tracked.closed.Load() {
			t.Error("Destroy called before Pi connection closed")
		}
		orderMu.Lock()
		order = append(order, "destroy")
		orderMu.Unlock()
	}
	listenerCalls := 0
	node, err := NewRoot(testRootIdentity(t), Dependencies{Provisioner: fake, SocketPath: path, DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) { return tracked, nil }, ModelPolicy: testModelPolicy(), CloseListener: func(context.Context) error {
		orderMu.Lock()
		defer orderMu.Unlock()
		listenerCalls++
		order = append(order, "listener")
		return nil
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	startPiPeer(t, peer, nil)
	if err := node.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := node.Stop(context.Background(), StopReasonRequested); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := node.Stop(context.Background(), StopReasonRequested); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
	if fake.destroyed != 1 {
		t.Fatalf("Destroy called %d times", fake.destroyed)
	}
	orderMu.Lock()
	defer orderMu.Unlock()
	if listenerCalls != 1 || strings.Join(order, ",") != "destroy,listener" {
		t.Fatalf("shutdown order=%v listener calls=%d", order, listenerCalls)
	}
}

func TestRootNodeClosesBrokerBeforeListenerCancellationAndWait(t *testing.T) {
	path, _ := boundSocket(t)
	fake := &fakeRootProvisioner{}
	broker := NewEventBroker()
	subscription := broker.Subscribe()
	listenerCalled := atomic.Bool{}
	node, err := NewRoot(testRootIdentity(t), Dependencies{
		Provisioner: fake,
		SocketPath:  path,
		DialRPC:     func(context.Context, string) (io.ReadWriteCloser, error) { return nil, errors.New("unused") },
		ModelPolicy: testModelPolicy(),
		CloseListener: func(context.Context) error {
			listenerCalled.Store(true)
			select {
			case _, open := <-subscription.Events:
				if open {
					return errors.New("broker subscriber remained open")
				}
				return nil
			case <-time.After(100 * time.Millisecond):
				return errors.New("listener waited for active broker subscriber")
			}
		},
	}, broker)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Stop(context.Background(), StopReasonRequested); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if !listenerCalled.Load() {
		t.Fatal("listener hook was not called")
	}
	subscription.Close()
}

func TestNewRootRequiresListenerLifecycleHook(t *testing.T) {
	path, _ := boundSocket(t)
	fake := &fakeRootProvisioner{}
	node, err := NewRoot(testRootIdentity(t), Dependencies{Provisioner: fake, SocketPath: path, DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) { return nil, nil }, ModelPolicy: testModelPolicy()}, nil)
	if err == nil || node != nil {
		t.Fatalf("NewRoot without CloseListener = (%v, %v)", node, err)
	}
}

func TestRootNodeStopBeforeStartPreventsProvisioning(t *testing.T) {
	path, _ := boundSocket(t)
	fake := &fakeRootProvisioner{resources: &provision.Resources{SessionID: "root-1", RPCAddr: "rpc"}}
	node, err := NewRoot(testRootIdentity(t), Dependencies{
		Provisioner: fake, SocketPath: path,
		DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) {
			t.Fatal("DialRPC called after stop-before-start")
			return nil, nil
		}, ModelPolicy: testModelPolicy(), CloseListener: func(context.Context) error { return nil },
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Stop(context.Background(), StopReasonRequested); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := node.Start(context.Background()); err == nil {
		t.Fatal("Start() succeeded after Done closed")
	}
	if fake.calls != 0 || fake.destroyed != 0 {
		t.Fatalf("provision calls=%d destroys=%d, want zero", fake.calls, fake.destroyed)
	}
}

func TestRootNodeStopWhileProvisioningDestroysLateResourcesWithoutDialing(t *testing.T) {
	path, _ := boundSocket(t)
	started := make(chan struct{})
	release := make(chan struct{})
	cleanupErr := errors.New("late cleanup failed")
	fake := &fakeRootProvisioner{
		resources:        &provision.Resources{SessionID: "root-1", Instance: "late-instance", Volume: "late-volume", RPCAddr: "rpc"},
		destroyErr:       cleanupErr,
		provisionStarted: started, provisionRelease: release,
	}
	node, err := NewRoot(testRootIdentity(t), Dependencies{
		Provisioner: fake, SocketPath: path,
		DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) {
			t.Fatal("DialRPC called after stop won provisioning race")
			return nil, nil
		}, ModelPolicy: testModelPolicy(), CloseListener: func(context.Context) error { return nil },
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	startDone := make(chan error, 1)
	go func() { startDone <- node.Start(context.Background()) }()
	<-started
	stopDone := make(chan error, 1)
	go func() { stopDone <- node.Stop(context.Background(), StopReasonRequested) }()
	select {
	case <-node.Done():
		t.Fatal("Done closed before in-flight provisioning returned ownership")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-stopDone; !errors.Is(err, cleanupErr) {
		t.Fatalf("Stop() error = %v, want joined cleanup error", err)
	}
	if err := <-startDone; err == nil {
		t.Fatal("Start() succeeded after stop won")
	}
	if fake.destroyed != 1 {
		t.Fatalf("Destroy called %d times, want exactly once", fake.destroyed)
	}
	select {
	case <-node.Done():
	default:
		t.Fatal("Done remains open after joined cleanup")
	}
}

func TestRootNodeExpiredStopStillFinalizesLateProvisioning(t *testing.T) {
	path, _ := boundSocket(t)
	started := make(chan struct{})
	release := make(chan struct{})
	fake := &fakeRootProvisioner{
		resources:        &provision.Resources{SessionID: "root-1", Instance: "late-instance", Volume: "late-volume", RPCAddr: "rpc"},
		provisionStarted: started,
		provisionRelease: release,
	}
	var dialed atomic.Bool
	node, err := NewRoot(testRootIdentity(t), Dependencies{
		Provisioner: fake, SocketPath: path,
		DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) {
			dialed.Store(true)
			return nil, errors.New("unexpected Pi dial")
		}, ModelPolicy: testModelPolicy(), CloseListener: func(context.Context) error { return nil },
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	startDone := make(chan error, 1)
	go func() { startDone <- node.Start(context.Background()) }()
	<-started

	stopCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := node.Stop(stopCtx, StopReasonRequested); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop() error = %v, want context.Canceled", err)
	}
	select {
	case <-node.Done():
		t.Fatal("Done closed before late provisioning returned ownership")
	default:
	}

	close(release)
	if err := <-startDone; err == nil {
		t.Fatal("Start() succeeded after durable stop intent")
	}
	select {
	case <-node.Done():
	case <-time.After(time.Second):
		t.Fatal("Done remained open without a second Stop call")
	}

	fake.mu.Lock()
	destroyed := fake.destroyed
	destroyContextErr := fake.destroyContextErr
	destroyHasDeadline := fake.destroyHasDeadline
	fake.mu.Unlock()
	if destroyed != 1 {
		t.Fatalf("Destroy called %d times, want exactly once", destroyed)
	}
	if destroyContextErr != nil || !destroyHasDeadline {
		t.Fatalf("Destroy context: err=%v deadline=%v, want detached bounded context", destroyContextErr, destroyHasDeadline)
	}
	if dialed.Load() {
		t.Fatal("Pi was dialed after Stop intent")
	}
}

func TestRootNodeChangedPiIdentityIsTerminalAndCleansResources(t *testing.T) {
	path, _ := boundSocket(t)
	host, peer := net.Pipe()
	fake := &fakeRootProvisioner{resources: &provision.Resources{SessionID: "root-1", Instance: "i", Volume: "v", RPCAddr: "rpc"}}
	node, err := NewRoot(testRootIdentity(t), Dependencies{Provisioner: fake, SocketPath: path, DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) { return host, nil }, ModelPolicy: testModelPolicy(), CloseListener: func(context.Context) error { return nil }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	startPiPeer(t, peer, nil)
	if err := node.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	pending := make(chan error, 1)
	go func() {
		_, err := node.CallRPC(context.Background(), json.RawMessage(`{"type":"get_messages"}`))
		pending <- err
	}()
	_ = readObject(t, peer)
	changed := make(chan error, 1)
	go func() {
		_, err := node.CallRPC(context.Background(), json.RawMessage(`{"type":"get_state"}`))
		changed <- err
	}()
	request := readObject(t, peer)
	writeGetStateResponse(t, peer, request.ID, "pi-other", "/sessions/pi-other.jsonl", false)
	if err := <-changed; !errors.Is(err, ErrInvariant) {
		t.Fatalf("changed get_state error = %v, want ErrInvariant", err)
	}
	if err := <-pending; err == nil {
		t.Fatal("pending call was not failed by identity invariant")
	}
	select {
	case <-node.Done():
	case <-time.After(time.Second):
		t.Fatal("node did not finish after identity invariant")
	}
	if got := node.Snapshot().Lifecycle; got != string(LifecycleFailed) {
		t.Fatalf("lifecycle = %q, want failed", got)
	}
	if fake.destroyed != 1 {
		t.Fatalf("Destroy called %d times, want once", fake.destroyed)
	}
}

func TestRootNodeStartJoinsBindingAndCleanupErrors(t *testing.T) {
	path, _ := boundSocket(t)
	host, peer := net.Pipe()
	cleanupErr := errors.New("cleanup failed")
	listenerErr := errors.New("listener cleanup failed")
	fake := &fakeRootProvisioner{resources: &provision.Resources{SessionID: "root-1", RPCAddr: "rpc"}, destroyErr: cleanupErr}
	node, err := NewRoot(testRootIdentity(t), Dependencies{Provisioner: fake, SocketPath: path, DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) { return host, nil }, ModelPolicy: testModelPolicy(), CloseListener: func(context.Context) error { return listenerErr }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = bufio.NewReader(peer).ReadBytes('\n'); _ = peer.Close() }()
	err = node.Start(context.Background())
	if err == nil || !errors.Is(err, cleanupErr) || !errors.Is(err, listenerErr) {
		t.Fatalf("Start() error = %v, want joined resource and listener cleanup errors", err)
	}
	if fake.destroyed != 1 {
		t.Fatalf("Destroy called %d times", fake.destroyed)
	}
}
