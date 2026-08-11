package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/provision"
)

var allowAllHandoffs = HandoffVerifierFunc(func(context.Context, []contract.RepositoryHandoff) error { return nil })

func TestRunWriteTaskPromptsExactlyAndLeavesWriterLiveUntilHandoff(t *testing.T) {
	identity := writerIdentity(t)
	local, peer, broker := newTestLocalSession(t, identity)
	bindTestLocal(t, local, peer, "pi-writer", "/tmp/writer.jsonl", false)
	node := &Node{identity: identity, local: local, broker: broker, state: LifecycleReady, deps: Dependencies{HandoffVerifier: allowAllHandoffs}}

	done := make(chan error, 1)
	go func() { done <- node.RunWriteTask(context.Background(), "implement exactly") }()
	line, err := bufio.NewReader(peer).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var prompt struct{ ID, Type, Message string }
	if err := json.Unmarshal(line, &prompt); err != nil {
		t.Fatal(err)
	}
	if prompt.Type != "prompt" || prompt.Message != "implement exactly" {
		t.Fatalf("prompt = %#v", prompt)
	}
	writeLine(t, peer, `{"id":"`+prompt.ID+`","type":"response","command":"prompt","success":true}`)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := local.Snapshot().Lifecycle; got != string(LifecycleRunning) {
		t.Fatalf("lifecycle = %s", got)
	}
	writeLine(t, peer, `{"type":"agent_settled"}`)
	waitFor(t, func() bool { return local.Snapshot().Lifecycle == string(LifecycleAwaitingHandoff) }, "writer to remain awaiting handoff")
	node.reportWrite = func(contract.WriteChildResult) error { return nil }
	_, err = node.Handoff(context.Background(), WriteHandoffRequest{
		Repositories: []contract.RepositoryHandoff{{Repository: "owner/repo", BaseCommit: "base", Branch: "feature", HeadCommit: "head"}},
		Summary:      "done",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := local.Snapshot().Lifecycle; got != string(LifecycleCompleted) {
		t.Fatalf("lifecycle after handoff = %s", got)
	}
}

func TestStopWhileWriterAwaitsHandoffCleansProcessResourcesAndSocket(t *testing.T) {
	identity := writerIdentity(t)
	socket, listener := boundSocket(t)
	clientConn, peer := net.Pipe()
	tracked := &trackedConn{ReadWriteCloser: clientConn}
	resources := &provision.Resources{Instance: "writer-instance", Volume: "writer-volume", RPCAddr: "rpc"}
	provisioner := &fakeRootProvisioner{resources: resources}
	var listenerClosed atomic.Bool
	node, err := NewChild(identity, Dependencies{
		Provisioner: provisioner,
		DialRPC:     func(context.Context, string) (io.ReadWriteCloser, error) { return tracked, nil },
		ModelPolicy: testModelPolicy(), SocketPath: socket,
		CloseListener:   func(context.Context) error { listenerClosed.Store(true); return listener.Close() },
		ReportWrite:     func(contract.WriteChildResult) error { return nil },
		HandoffVerifier: allowAllHandoffs,
	}, NewEventBroker())
	if err != nil {
		t.Fatal(err)
	}
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		reader := bufio.NewReader(peer)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			var command struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			}
			if json.Unmarshal(line, &command) != nil {
				return
			}
			response := map[string]any{"id": command.ID, "type": "response", "command": command.Type, "success": true}
			if command.Type == "get_state" {
				response["data"] = map[string]any{
					"sessionId": "pi-writer", "sessionFile": "/tmp/writer.jsonl", "isStreaming": false,
					"model": map[string]any{"provider": "local-executor", "id": "worker-model"}, "thinkingLevel": "off",
				}
			}
			wire, _ := json.Marshal(response)
			_, _ = peer.Write(append(wire, '\n'))
			if command.Type == "prompt" {
				_, _ = peer.Write([]byte(`{"type":"agent_settled"}` + "\n"))
			}
		}
	}()
	if err := node.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := node.RunWriteTask(context.Background(), "prepare handoff"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return node.Snapshot().Lifecycle == string(LifecycleAwaitingHandoff) }, "writer awaiting handoff")

	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := node.Stop(stopCtx, StopReasonRequested); err != nil {
		t.Fatal(err)
	}
	if !tracked.closed.Load() || !listenerClosed.Load() {
		t.Fatalf("stop awaiting handoff did not close Pi/socket: pi=%v socket=%v", tracked.closed.Load(), listenerClosed.Load())
	}
	provisioner.mu.Lock()
	destroyed := provisioner.destroyed
	provisioner.mu.Unlock()
	if destroyed != 1 {
		t.Fatalf("destroy calls = %d, want 1", destroyed)
	}
	select {
	case <-peerDone:
	case <-time.After(time.Second):
		t.Fatal("Pi peer did not observe stop while awaiting handoff")
	}
}

func TestWriterHandoffRecordsAndForwardsExactlyOnceBeforeAcceptance(t *testing.T) {
	identity, err := NewIdentity(IdentitySpec{SessionID: "writer-1", ParentID: "root-1", RootID: "root-1", Kind: contract.ChildKindWrite, Context: contract.ContextFresh, Worker: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var forwarded []contract.WriteChildResult
	node := &Node{identity: identity, deps: Dependencies{HandoffVerifier: allowAllHandoffs}, reportWrite: func(result contract.WriteChildResult) error {
		mu.Lock()
		defer mu.Unlock()
		forwarded = append(forwarded, result)
		return nil
	}}
	request := WriteHandoffRequest{
		Repositories: []contract.RepositoryHandoff{{Repository: "owner/repo", BaseCommit: "base", Branch: "feature", HeadCommit: "head"}},
		Summary:      "done", Verification: []string{"go test ./..."},
	}
	accepted, err := node.Handoff(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !accepted.Accepted || accepted.SessionID != "writer-1" {
		t.Fatalf("accepted = %#v", accepted)
	}
	mu.Lock()
	if len(forwarded) != 1 {
		t.Fatalf("forwarded = %#v", forwarded)
	}
	got := forwarded[0]
	mu.Unlock()
	if got.SessionID != "writer-1" || got.WorkerType != "worker" || got.Kind != contract.ChildKindWrite {
		t.Fatalf("result identity = %#v", got)
	}
	if got.Repositories[0].Repository != "owner/repo" {
		t.Fatalf("repositories = %#v", got.Repositories)
	}

	_, err = node.Handoff(context.Background(), request)
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorConflict {
		t.Fatalf("duplicate error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(forwarded) != 1 {
		t.Fatalf("duplicate forwarded %d results", len(forwarded))
	}
}

func TestAcceptedHandoffWatchdogForcesCleanupWhenPiDoesNotEOF(t *testing.T) {
	identity := writerIdentity(t)
	socket, listener := boundSocket(t)
	clientConn, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	tracked := &trackedConn{ReadWriteCloser: clientConn}
	resources := &provision.Resources{Instance: "writer-instance", Volume: "writer-volume", RPCAddr: "rpc"}
	provisioner := &fakeRootProvisioner{resources: resources}
	var listenerClosed atomic.Bool
	node, err := NewChild(identity, Dependencies{
		Provisioner: provisioner,
		DialRPC:     func(context.Context, string) (io.ReadWriteCloser, error) { return tracked, nil },
		ModelPolicy: testModelPolicy(), SocketPath: socket,
		CloseListener:          func(context.Context) error { listenerClosed.Store(true); return listener.Close() },
		ReportWrite:            func(contract.WriteChildResult) error { return nil },
		HandoffVerifier:        allowAllHandoffs,
		HandoffShutdownTimeout: 30 * time.Millisecond,
	}, NewEventBroker())
	if err != nil {
		t.Fatal(err)
	}
	peerDone := startPiPeerWithState(t, peer, nil, "pi-1", "/tmp/pi-1.jsonl", workerModel(testModelPolicy(), "worker"))
	if err := node.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	unfinished := newFakeChildProcess()
	client := &fakeDescendantClient{}
	entry := &childEntry{id: "unfinished-descendant", socket: "/tmp/unfinished.sock"}
	entry.init()
	entry.setProcess(unfinished)
	entry.setClient(client)
	if err := node.children.add(entry); err != nil {
		t.Fatal(err)
	}
	parentReturned := make(chan struct{})
	go func() { <-node.Done(); close(parentReturned) }()

	destroyCount := func() int {
		provisioner.mu.Lock()
		defer provisioner.mu.Unlock()
		return provisioner.destroyed
	}
	accepted, err := node.Handoff(context.Background(), WriteHandoffRequest{
		Repositories: []contract.RepositoryHandoff{{Repository: "owner/repo", BaseCommit: "base", Branch: "feature", HeadCommit: "head"}},
		Summary:      "done",
	})
	if err != nil || !accepted.Accepted {
		t.Fatalf("Handoff() = %#v, %v", accepted, err)
	}
	if tracked.closed.Load() || listenerClosed.Load() || destroyCount() != 0 {
		t.Fatal("cleanup began before handoff acceptance returned")
	}
	if err := node.AcknowledgeHandoff(); err != nil {
		t.Fatalf("AcknowledgeHandoff() error = %v", err)
	}

	select {
	case <-node.Done():
	case <-time.After(time.Second):
		t.Fatal("accepted handoff watchdog did not complete node")
	}
	if !tracked.closed.Load() {
		t.Fatal("watchdog did not close Pi RPC")
	}
	if !listenerClosed.Load() {
		t.Fatal("watchdog did not close listener")
	}
	if got := destroyCount(); got != 1 {
		t.Fatalf("destroy calls = %d, want 1", got)
	}
	if unfinished.ackClosed.Load() == 0 || unfinished.liveness.Load() == 0 {
		t.Fatalf("unfinished descendant was not cancelled parent-side: ackClosed=%d liveness=%d",
			unfinished.ackClosed.Load(), unfinished.liveness.Load())
	}
	if node.children.get("unfinished-descendant") != nil {
		t.Fatal("unfinished descendant remains registered after forced cleanup")
	}
	select {
	case <-parentReturned:
	case <-time.After(time.Second):
		t.Fatal("parent waiter did not return after forced cleanup")
	}
	select {
	case <-peerDone:
	case <-time.After(time.Second):
		t.Fatal("Pi peer did not observe forced close")
	}
}

func TestRejectedHandoffLeavesWriterLiveAndRetryable(t *testing.T) {
	identity, err := NewIdentity(IdentitySpec{SessionID: "writer-1", ParentID: "root-1", RootID: "root-1", Kind: contract.ChildKindWrite, Context: contract.ContextFresh, Worker: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	node := &Node{identity: identity, deps: Dependencies{HandoffVerifier: allowAllHandoffs}, reportWrite: func(contract.WriteChildResult) error {
		attempts++
		if attempts == 1 {
			return errors.New("report pipe unavailable")
		}
		return nil
	}}
	request := WriteHandoffRequest{Repositories: []contract.RepositoryHandoff{{Repository: "owner/repo", BaseCommit: "base", Branch: "feature", HeadCommit: "head"}}, Summary: "done"}
	if _, err := node.Handoff(context.Background(), request); err == nil {
		t.Fatal("expected rejected handoff")
	}
	if node.handoffAccepted() {
		t.Fatal("rejected handoff became terminal")
	}
	if _, err := node.Handoff(context.Background(), request); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d", attempts)
	}
}

func TestHandoffRejectsRootReadEmptyAndDuplicateRepositories(t *testing.T) {
	for _, kind := range []contract.ChildKind{contract.ChildKindRoot, contract.ChildKindRead} {
		parent, root := "root-1", "root-1"
		contextMode := contract.ContextRoot
		worker := ""
		if kind == contract.ChildKindRead {
			parent, contextMode, worker = "root-1", contract.ContextFresh, "reviewer"
		}
		identity, err := NewIdentity(IdentitySpec{SessionID: map[bool]string{true: "root-1", false: "read-1"}[kind == contract.ChildKindRoot], ParentID: map[bool]string{true: "", false: parent}[kind == contract.ChildKindRoot], RootID: root, Kind: kind, Context: contextMode, Worker: worker})
		if err != nil {
			t.Fatal(err)
		}
		node := &Node{identity: identity, reportWrite: func(contract.WriteChildResult) error { t.Fatal("must not report"); return nil }}
		_, err = node.Handoff(context.Background(), WriteHandoffRequest{Repositories: []contract.RepositoryHandoff{{Repository: "owner/repo", BaseCommit: "base", Branch: "branch", HeadCommit: "head"}}, Summary: "done"})
		var typed *contract.Error
		if !errors.As(err, &typed) || typed.Code != contract.ErrorConflict {
			t.Fatalf("%s error = %v", kind, err)
		}
	}

	identity, _ := NewIdentity(IdentitySpec{SessionID: "writer-1", ParentID: "root-1", RootID: "root-1", Kind: contract.ChildKindWrite, Context: contract.ContextFresh, Worker: "worker"})
	node := &Node{identity: identity, reportWrite: func(contract.WriteChildResult) error { t.Fatal("invalid request forwarded"); return nil }}
	invalid := []WriteHandoffRequest{
		{Summary: "done"},
		{Repositories: []contract.RepositoryHandoff{{Repository: "owner/repo", BaseCommit: "base", Branch: "one", HeadCommit: "head"}, {Repository: "owner/repo", BaseCommit: "base", Branch: "two", HeadCommit: "head"}}, Summary: "done"},
	}
	for _, request := range invalid {
		if _, err := node.Handoff(context.Background(), request); err == nil {
			t.Fatalf("accepted invalid request %#v", request)
		}
	}
}

func TestWriterHandoffRejectsForgedDirectPostBeforeReport(t *testing.T) {
	identity := writerIdentity(t)
	var reported atomic.Bool
	node := &Node{
		identity: identity,
		deps: Dependencies{HandoffVerifier: HandoffVerifierFunc(func(context.Context, []contract.RepositoryHandoff) error {
			return contract.NewError(contract.ErrorHandoffRefMismatch, "trusted GitHub tip differs")
		})},
		reportWrite: func(contract.WriteChildResult) error { reported.Store(true); return nil },
	}
	_, err := node.Handoff(context.Background(), WriteHandoffRequest{
		Repositories: []contract.RepositoryHandoff{{Repository: "owner/repo", BaseCommit: "base", Branch: "feature", HeadCommit: "forged"}},
		Summary:      "forged direct POST",
	})
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorHandoffRefMismatch {
		t.Fatalf("Handoff() error = %v", err)
	}
	if reported.Load() {
		t.Fatal("forged handoff was reported")
	}
}
