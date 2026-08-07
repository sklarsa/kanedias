package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/pirpc"
)

func TestLocalSessionDrainsInitialEventsBeforeReadinessAndBindsOnce(t *testing.T) {
	local, peer, broker := newTestLocalSession(t, rootIdentity(t))

	writeLine(t, peer, `{"type":"message_start","message":{"role":"assistant"}}`)
	waitFor(t, func() bool { return brokerReplayCount(broker) == 1 }, "initial Pi event to reach broker")

	bindTestLocal(t, local, peer, "pi-root", "/sessions/pi-root.jsonl", false)
	snapshot := local.Snapshot()
	if snapshot.PiSessionID != "pi-root" || snapshot.SessionFile != "/sessions/pi-root.jsonl" {
		t.Fatalf("binding snapshot = (%q, %q), want pi-root and session file", snapshot.PiSessionID, snapshot.SessionFile)
	}
	if snapshot.Lifecycle != string(LifecycleReady) {
		t.Fatalf("Lifecycle = %q, want ready", snapshot.Lifecycle)
	}
	if snapshot.Model != (contract.ModelProfile{Provider: "openai-codex", Model: "gpt-5.6-sol", ThinkingLevel: "high"}) {
		t.Fatalf("Model = %#v, want bound get_state model", snapshot.Model)
	}

	if err := local.Bind(context.Background()); !errors.Is(err, ErrInvariant) {
		t.Fatalf("second Bind() error = %v, want ErrInvariant", err)
	}
	assertNoPeerWrite(t, peer)
}

func TestLocalSessionRejectsEmptyOrChangedPiBinding(t *testing.T) {
	for _, tt := range []struct {
		name        string
		sessionID   string
		sessionFile string
	}{
		{name: "empty session ID", sessionFile: "/sessions/pi-root.jsonl"},
		{name: "empty session file", sessionID: "pi-root"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			local, peer, _ := newTestLocalSession(t, rootIdentity(t))
			errCh := make(chan error, 1)
			go func() { errCh <- local.Bind(context.Background()) }()
			request := readObject(t, peer)
			writeGetStateResponse(t, peer, request.ID, tt.sessionID, tt.sessionFile, false)
			if err := <-errCh; !errors.Is(err, ErrInvariant) {
				t.Fatalf("Bind() error = %v, want ErrInvariant", err)
			}
		})
	}

	t.Run("changed session id", func(t *testing.T) {
		local, peer, _ := newTestLocalSession(t, rootIdentity(t))
		bindTestLocal(t, local, peer, "pi-root", "/sessions/pi-root.jsonl", false)

		result := make(chan error, 1)
		go func() {
			_, err := local.CallRPC(context.Background(), json.RawMessage(`{"type":"get_state"}`))
			result <- err
		}()
		request := readObject(t, peer)
		writeGetStateResponse(t, peer, request.ID, "pi-other", "/sessions/pi-other.jsonl", false)
		if err := <-result; !errors.Is(err, ErrInvariant) {
			t.Fatalf("CallRPC(get_state) error = %v, want ErrInvariant", err)
		}
		if got := local.Snapshot().PiSessionID; got != "pi-root" {
			t.Fatalf("PiSessionID after changed response = %q, want pi-root", got)
		}
	})
}

func TestLocalSessionForkBindingMustMatchAdmittedSessionBeforeReadiness(t *testing.T) {
	for _, test := range []struct {
		name, sessionID, sessionFile string
	}{
		{name: "session ID mismatch", sessionID: "wrong-pi", sessionFile: "/sessions/fork.jsonl"},
		{name: "session file mismatch", sessionID: "expected-pi", sessionFile: "/sessions/wrong.jsonl"},
	} {
		t.Run(test.name, func(t *testing.T) {
			local, peer, _ := newTestLocalSession(t, writerIdentity(t))
			result := make(chan error, 1)
			go func() {
				result <- local.BindExpected(context.Background(), &PiBinding{SessionID: "expected-pi", SessionFile: "/sessions/fork.jsonl"})
			}()
			request := readObject(t, peer)
			writeGetStateResponse(t, peer, request.ID, test.sessionID, test.sessionFile, false)
			if err := <-result; !errors.Is(err, ErrInvariant) {
				t.Fatalf("BindExpected() error = %v, want ErrInvariant", err)
			}
			if got := local.Snapshot().Lifecycle; got == string(LifecycleReady) {
				t.Fatalf("mismatched fork binding reached ready")
			}
		})
	}
}

func TestLocalSessionRootSettlementReturnsRunningSessionToReady(t *testing.T) {
	local, peer, _ := newTestLocalSession(t, rootIdentity(t))
	bindTestLocal(t, local, peer, "pi-root", "/sessions/pi-root.jsonl", false)

	call := make(chan error, 1)
	go func() {
		_, err := local.CallRPC(context.Background(), json.RawMessage(`{"type":"prompt","message":"work"}`))
		call <- err
	}()
	request := readObject(t, peer)
	writeLine(t, peer, `{"id":"`+request.ID+`","type":"response","command":"prompt","success":true}`)
	if err := <-call; err != nil {
		t.Fatalf("CallRPC(prompt) error = %v", err)
	}
	if got := local.Snapshot().Lifecycle; got != string(LifecycleRunning) {
		t.Fatalf("Lifecycle after prompt = %q, want running", got)
	}

	writeLine(t, peer, `{"type":"agent_settled"}`)
	waitFor(t, func() bool { return local.Snapshot().Lifecycle == string(LifecycleReady) }, "root settlement to reach ready")
}

func TestLocalSessionWriterSettlementAwaitsHandoffAndFollowUpResumes(t *testing.T) {
	identity, err := NewIdentity(IdentitySpec{SessionID: "writer", ParentID: "root", RootID: "root", Kind: contract.ChildKindWrite, Context: contract.ContextFresh, Worker: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	local, peer, _ := newTestLocalSession(t, identity)
	bindTestLocal(t, local, peer, "pi-writer", "/sessions/pi-writer.jsonl", false)

	callRPCSuccess(t, local, peer, `{"type":"prompt","message":"implement"}`, "prompt")
	writeLine(t, peer, `{"type":"agent_settled"}`)
	waitFor(t, func() bool { return local.Snapshot().Lifecycle == string(LifecycleAwaitingHandoff) }, "writer settlement to await handoff")

	for _, command := range []struct {
		raw, name string
	}{
		{`{"type":"get_messages"}`, "get_messages"},
		{`{"type":"abort"}`, "abort"},
	} {
		callRPCSuccess(t, local, peer, command.raw, command.name)
		if got := local.Snapshot().Lifecycle; got != string(LifecycleAwaitingHandoff) {
			t.Fatalf("Lifecycle after %s = %q, want awaiting_handoff", command.name, got)
		}
	}

	callRPCSuccess(t, local, peer, `{"type":"follow_up","message":"finish handoff"}`, "follow_up")
	if got := local.Snapshot().Lifecycle; got != string(LifecycleRunning) {
		t.Fatalf("Lifecycle after follow_up = %q, want running", got)
	}
	writeLine(t, peer, `{"type":"agent_settled"}`)
	waitFor(t, func() bool { return local.Snapshot().Lifecycle == string(LifecycleAwaitingHandoff) }, "second writer settlement to await handoff")

	callRPCSuccess(t, local, peer, `{"type":"prompt","message":"one more change"}`, "prompt")
	if got := local.Snapshot().Lifecycle; got != string(LifecycleRunning) {
		t.Fatalf("Lifecycle after second prompt = %q, want running", got)
	}
}

func TestLocalSessionDelayedPromptResponseCannotOverwriteLaterSettlement(t *testing.T) {
	for _, test := range []struct {
		name     string
		identity func(*testing.T) Identity
		want     LifecycleState
	}{
		{name: "root", identity: rootIdentity, want: LifecycleReady},
		{name: "writer", identity: writerIdentity, want: LifecycleAwaitingHandoff},
	} {
		t.Run(test.name, func(t *testing.T) {
			local, peer, broker := newTestLocalSession(t, test.identity(t))
			bindTestLocal(t, local, peer, "pi-"+test.name, "/sessions/pi.jsonl", false)
			result := make(chan error, 1)
			go func() {
				_, err := local.CallRPC(context.Background(), json.RawMessage(`{"type":"prompt","message":"work"}`))
				result <- err
			}()
			request := readObject(t, peer)
			writeBatch(t, peer,
				`{"id":"`+request.ID+`","type":"response","command":"prompt","success":true}`,
				`{"type":"agent_start"}`,
				`{"type":"agent_settled"}`,
			)
			waitFor(t, func() bool { return brokerReplayCount(broker) == 2 }, "events after accepted prompt")
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { return local.Snapshot().Lifecycle == string(test.want) }, "settlement to remain terminal for generation")
		})
	}
}

func TestLocalSessionGetStateReconcilesSettlementAfterResponse(t *testing.T) {
	for _, test := range []struct {
		name     string
		identity func(*testing.T) Identity
		want     LifecycleState
	}{
		{name: "root", identity: rootIdentity, want: LifecycleReady},
		{name: "writer", identity: writerIdentity, want: LifecycleAwaitingHandoff},
	} {
		t.Run(test.name, func(t *testing.T) {
			local, peer, broker := newTestLocalSession(t, test.identity(t))
			result := make(chan error, 1)
			go func() { result <- local.Bind(context.Background()) }()
			request := readObject(t, peer)
			response, err := json.Marshal(map[string]any{
				"id": request.ID, "type": "response", "command": "get_state", "success": true,
				"data": map[string]any{"sessionId": "pi-" + test.name, "sessionFile": "/sessions/pi.jsonl", "isStreaming": true},
			})
			if err != nil {
				t.Fatal(err)
			}
			writeBatch(t, peer, string(response), `{"type":"agent_settled"}`)
			waitFor(t, func() bool { return brokerReplayCount(broker) == 1 }, "settlement after get_state response")
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			if got := local.Snapshot().Lifecycle; got != string(test.want) {
				t.Fatalf("lifecycle = %q, want %q", got, test.want)
			}
		})
	}
}

func TestLocalSessionForbiddenRPCStaysUnwritten(t *testing.T) {
	local, peer, _ := newTestLocalSession(t, rootIdentity(t))
	bindTestLocal(t, local, peer, "pi-root", "/sessions/pi-root.jsonl", false)

	_, err := local.CallRPC(context.Background(), json.RawMessage(`{"type":"new_session"}`))
	var contractErr *contract.Error
	if !errors.As(err, &contractErr) || contractErr.Code != contract.ErrorForbiddenRPC {
		t.Fatalf("CallRPC(new_session) error = %v, want forbidden_rpc", err)
	}
	assertNoPeerWrite(t, peer)
}

func TestLocalSessionEOFFailsPendingCallAndClearsQuestions(t *testing.T) {
	local, peer, _ := newTestLocalSession(t, rootIdentity(t))
	bindTestLocal(t, local, peer, "pi-root", "/sessions/pi-root.jsonl", false)
	writeLine(t, peer, `{"type":"extension_ui_request","id":"question-1","method":"confirm","title":"Continue?","message":"Choose"}`)
	waitFor(t, func() bool { return len(local.Snapshot().Questions) == 1 }, "question to become pending")

	result := make(chan error, 1)
	go func() {
		_, err := local.CallRPC(context.Background(), json.RawMessage(`{"type":"get_messages"}`))
		result <- err
	}()
	_ = readObject(t, peer)
	peer.Close()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("pending CallRPC() error = nil after EOF")
		}
	case <-time.After(time.Second):
		t.Fatal("pending CallRPC() did not fail after EOF")
	}
	waitFor(t, func() bool { return local.Snapshot().Lifecycle == string(LifecycleFailed) }, "EOF to fail local session")
	if got := local.Snapshot().Questions; len(got) != 0 {
		t.Fatalf("questions after EOF = %#v, want empty", got)
	}
}

type testRPCObject struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

func rootIdentity(t *testing.T) Identity {
	t.Helper()
	identity, err := NewIdentity(IdentitySpec{SessionID: "root", RootID: "root", Kind: contract.ChildKindRoot, Context: contract.ContextRoot})
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func writerIdentity(t *testing.T) Identity {
	t.Helper()
	identity, err := NewIdentity(IdentitySpec{SessionID: "writer", ParentID: "root", RootID: "root", Kind: contract.ChildKindWrite, Context: contract.ContextFresh, Worker: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func newTestLocalSession(t *testing.T, identity Identity) (*LocalSession, net.Conn, *EventBroker) {
	t.Helper()
	clientConn, peerConn := net.Pipe()
	rpc := pirpc.NewClient(clientConn)
	broker := newEventBroker(8, 8)
	local := NewLocalSession(identity, rpc, broker)
	t.Cleanup(func() {
		local.StopRPC()
		peerConn.Close()
	})
	return local, peerConn, broker
}

func bindTestLocal(t *testing.T, local *LocalSession, peer net.Conn, sessionID, sessionFile string, streaming bool) {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- local.Bind(context.Background()) }()
	request := readObject(t, peer)
	if request.Type != "get_state" {
		t.Fatalf("Bind() command type = %q, want get_state", request.Type)
	}
	writeGetStateResponse(t, peer, request.ID, sessionID, sessionFile, streaming)
	if err := <-result; err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
}

func callRPCSuccess(t *testing.T, local *LocalSession, peer net.Conn, raw, command string) {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		_, err := local.CallRPC(context.Background(), json.RawMessage(raw))
		result <- err
	}()
	request := readObject(t, peer)
	writeLine(t, peer, `{"id":"`+request.ID+`","type":"response","command":"`+command+`","success":true}`)
	if err := <-result; err != nil {
		t.Fatalf("CallRPC(%s) error = %v", command, err)
	}
}

func writeGetStateResponse(t *testing.T, peer net.Conn, id, sessionID, sessionFile string, streaming bool) {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"id": id, "type": "response", "command": "get_state", "success": true,
		"data": map[string]any{
			"model":         map[string]any{"provider": "openai-codex", "id": "gpt-5.6-sol"},
			"thinkingLevel": "high", "isStreaming": streaming,
			"sessionId": sessionID, "sessionFile": sessionFile,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeLine(t, peer, string(encoded))
}

func readObject(t *testing.T, peer net.Conn) testRPCObject {
	t.Helper()
	peer.SetReadDeadline(time.Now().Add(time.Second))
	line, err := bufio.NewReader(peer).ReadBytes('\n')
	peer.SetReadDeadline(time.Time{})
	if err != nil {
		t.Fatalf("read Pi RPC command: %v", err)
	}
	var object testRPCObject
	if err := json.Unmarshal(line, &object); err != nil {
		t.Fatalf("decode Pi RPC command: %v", err)
	}
	return object
}

func writeLine(t *testing.T, peer net.Conn, line string) {
	t.Helper()
	writeBatch(t, peer, line)
}

func writeBatch(t *testing.T, peer net.Conn, lines ...string) {
	t.Helper()
	var wire []byte
	for _, line := range lines {
		wire = append(wire, line...)
		wire = append(wire, '\n')
	}
	peer.SetWriteDeadline(time.Now().Add(time.Second))
	_, err := peer.Write(wire)
	peer.SetWriteDeadline(time.Time{})
	if err != nil {
		t.Fatalf("write Pi RPC lines: %v", err)
	}
}

func assertNoPeerWrite(t *testing.T, peer net.Conn) {
	t.Helper()
	peer.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	defer peer.SetReadDeadline(time.Time{})
	buffer := make([]byte, 1)
	if _, err := peer.Read(buffer); err == nil {
		t.Fatal("unexpected Pi RPC bytes were written")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
			t.Fatalf("peer read error = %v, want timeout", err)
		}
	}
}

func brokerReplayCount(broker *EventBroker) int {
	subscription := broker.Subscribe()
	defer subscription.Close()
	return len(subscription.Replay)
}

func waitFor(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}
