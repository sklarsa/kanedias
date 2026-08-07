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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/provision"
)

type fakeRootProvisioner struct {
	mu           sync.Mutex
	calls        int
	request      provision.RootRequest
	resources    *provision.Resources
	provisionErr error
	destroyErr   error
	destroyed    int
	onDestroy    func()
}

func (fake *fakeRootProvisioner) ProvisionRoot(_ context.Context, request provision.RootRequest) (*provision.Resources, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	fake.request = request
	return fake.resources, fake.provisionErr
}
func (fake *fakeRootProvisioner) Destroy(context.Context, *provision.Resources) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.destroyed++
	if fake.onDestroy != nil {
		fake.onDestroy()
	}
	return fake.destroyErr
}

type fakeWorkers struct{}

func (fakeWorkers) Resolve(string) (config.WorkerProfile, error) { return config.WorkerProfile{}, nil }
func (fakeWorkers) Summaries() []contract.WorkerSummary          { return nil }

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
		listener.Close()
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
		response := map[string]any{"id": command.ID, "type": "response", "command": "get_state", "success": true, "data": map[string]any{"sessionId": "pi-1", "sessionFile": "/tmp/pi-1.jsonl", "isStreaming": false}}
		wire, _ := json.Marshal(response)
		_, _ = peer.Write(append(wire, '\n'))
	}()
	return done
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
			node, err := NewRoot(testRootIdentity(t), Dependencies{Provisioner: fake, SocketPath: test.prepare(t), DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) { t.Fatal("DialRPC called"); return nil, nil }, Workers: fakeWorkers{}}, NewEventBroker())
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
		Workers: fakeWorkers{},
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

func TestRootNodeTCPConnectionAloneIsNotReady(t *testing.T) {
	path, _ := boundSocket(t)
	host, peer := net.Pipe()
	fake := &fakeRootProvisioner{resources: &provision.Resources{SessionID: "root-1", RPCAddr: "rpc"}}
	node, err := NewRoot(testRootIdentity(t), Dependencies{Provisioner: fake, SocketPath: path, DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) { return host, nil }, Workers: fakeWorkers{}}, nil)
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
	defer peer.Close()
	fake := &fakeRootProvisioner{resources: &provision.Resources{SessionID: "root-1", Pool: "btrfs", Instance: "i", Volume: "v", RPCAddr: "rpc"}}
	broker := NewEventBroker()
	node, err := NewRoot(testRootIdentity(t), Dependencies{Provisioner: fake, SocketPath: path, DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) { return host, nil }, Workers: fakeWorkers{}}, broker)
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
}

func TestRootNodeForbiddenRPCIsNotWritten(t *testing.T) {
	path, _ := boundSocket(t)
	host, peer := net.Pipe()
	defer peer.Close()
	fake := &fakeRootProvisioner{resources: &provision.Resources{SessionID: "root-1", RPCAddr: "rpc"}}
	node, err := NewRoot(testRootIdentity(t), Dependencies{Provisioner: fake, SocketPath: path, DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) { return host, nil }, Workers: fakeWorkers{}}, nil)
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
	node, err := NewRoot(testRootIdentity(t), Dependencies{Provisioner: fake, SocketPath: path, DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) { return host, nil }, Workers: fakeWorkers{}}, nil)
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

func TestRootNodeStopIsIdempotentAndClosesPiBeforeDestroy(t *testing.T) {
	path, _ := boundSocket(t)
	host, peer := net.Pipe()
	defer peer.Close()
	tracked := &trackedConn{ReadWriteCloser: host}
	fake := &fakeRootProvisioner{resources: &provision.Resources{SessionID: "root-1", RPCAddr: "rpc"}}
	fake.onDestroy = func() {
		if !tracked.closed.Load() {
			t.Error("Destroy called before Pi connection closed")
		}
	}
	node, err := NewRoot(testRootIdentity(t), Dependencies{Provisioner: fake, SocketPath: path, DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) { return tracked, nil }, Workers: fakeWorkers{}}, nil)
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
}

func TestRootNodeStartJoinsBindingAndCleanupErrors(t *testing.T) {
	path, _ := boundSocket(t)
	host, peer := net.Pipe()
	cleanupErr := errors.New("cleanup failed")
	fake := &fakeRootProvisioner{resources: &provision.Resources{SessionID: "root-1", RPCAddr: "rpc"}, destroyErr: cleanupErr}
	node, err := NewRoot(testRootIdentity(t), Dependencies{Provisioner: fake, SocketPath: path, DialRPC: func(context.Context, string) (io.ReadWriteCloser, error) { return host, nil }, Workers: fakeWorkers{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = bufio.NewReader(peer).ReadBytes('\n'); _ = peer.Close() }()
	err = node.Start(context.Background())
	if err == nil || !errors.Is(err, cleanupErr) {
		t.Fatalf("Start() error = %v, want joined cleanup error", err)
	}
	if fake.destroyed != 1 {
		t.Fatalf("Destroy called %d times", fake.destroyed)
	}
}
