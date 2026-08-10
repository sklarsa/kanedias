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

func TestRunReadTaskSendsExactFreshAndForkPromptAndRequiresAcceptance(t *testing.T) {
	for _, mode := range []contract.ContextMode{contract.ContextFresh, contract.ContextFork} {
		t.Run(string(mode), func(t *testing.T) {
			node, peer := boundReadNodeForContext(t, mode)
			defer func() { _ = peer.Close() }()
			go func() {
				reader := bufio.NewReader(peer)
				prompt := readRPCCommand(t, reader)
				if prompt.Type != "prompt" || prompt.Message != "Bootstrap task exactly" {
					t.Errorf("prompt = %#v", prompt)
				}
				writeRPCResponse(t, peer, prompt.ID, "prompt", false, nil)
			}()
			_, err := node.RunReadTask(context.Background(), "Bootstrap task exactly")
			var typed *contract.Error
			if !errors.As(err, &typed) || typed.Code != contract.ErrorChildFailed {
				t.Fatalf("rejection error = %v", err)
			}
		})
	}
}

func TestReadResultRequiresSettledStopAndNonNullFinalText(t *testing.T) {
	tests := []struct {
		name       string
		events     []string
		finalText  any
		closeEarly bool
		wantCode   contract.ErrorCode
	}{
		{name: "success", events: []string{`{"type":"message_end","message":{"role":"assistant","stopReason":"stop"}}`, `{"type":"agent_settled"}`}, finalText: "answer"},
		{name: "aborted", events: []string{`{"type":"message_end","message":{"role":"assistant","stopReason":"aborted"}}`, `{"type":"agent_settled"}`}, wantCode: contract.ErrorChildAborted},
		{name: "error", events: []string{`{"type":"message_end","message":{"role":"assistant","stopReason":"error"}}`, `{"type":"agent_settled"}`}, wantCode: contract.ErrorChildFailed},
		{name: "length", events: []string{`{"type":"message_end","message":{"role":"assistant","stopReason":"length"}}`, `{"type":"agent_settled"}`}, wantCode: contract.ErrorChildFailed},
		{name: "extension error", events: []string{`{"type":"extension_error","error":"boom"}`}, wantCode: contract.ErrorChildFailed},
		{name: "settled without assistant", events: []string{`{"type":"agent_settled"}`}, wantCode: contract.ErrorChildFailed},
		{name: "null text", events: []string{`{"type":"message_end","message":{"role":"assistant","stopReason":"stop"}}`, `{"type":"agent_settled"}`}, finalText: nil, wantCode: contract.ErrorChildFailed},
		{name: "rpc eof", closeEarly: true, wantCode: contract.ErrorChildFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node, peer := boundReadNode(t)
			defer func() { _ = peer.Close() }()
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				reader := bufio.NewReader(peer)
				prompt := readRPCCommand(t, reader)
				if prompt.Type != "prompt" || prompt.Message != "exact task" {
					t.Errorf("prompt = %#v", prompt)
				}
				writeRPCResponse(t, peer, prompt.ID, "prompt", true, nil)
				if tt.closeEarly {
					_ = peer.Close()
					return
				}
				for _, event := range tt.events {
					_, _ = peer.Write([]byte(event + "\n"))
				}
				if tt.wantCode == "" || tt.finalText != nil || tt.name == "null text" {
					last := readRPCCommand(t, reader)
					writeRPCResponse(t, peer, last.ID, "get_last_assistant_text", true, map[string]any{"text": tt.finalText})
				}
			}()
			result, err := node.RunReadTask(context.Background(), "exact task")
			if tt.wantCode == "" {
				if err != nil {
					t.Fatal(err)
				}
				if result.Output != "answer" || result.SessionID != "child-1" || result.WorkerType != "reviewer" {
					t.Fatalf("result = %#v", result)
				}
			} else {
				var typed *contract.Error
				if !errors.As(err, &typed) || typed.Code != tt.wantCode {
					t.Fatalf("error = %v, want %s", err, tt.wantCode)
				}
			}
			select {
			case <-serverDone:
			case <-time.After(time.Second):
				t.Fatal("fake Pi did not finish")
			}
		})
	}
}

type rpcTestCommand struct{ ID, Type, Message string }

func readRPCCommand(t *testing.T, reader *bufio.Reader) rpcTestCommand {
	t.Helper()
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Error(err)
		return rpcTestCommand{}
	}
	var command rpcTestCommand
	if err := json.Unmarshal(line, &command); err != nil {
		t.Error(err)
	}
	return command
}
func writeRPCResponse(t *testing.T, peer net.Conn, id, command string, success bool, data any) {
	t.Helper()
	value := map[string]any{"id": id, "type": "response", "command": command, "success": success}
	if data != nil || command == "get_last_assistant_text" {
		value["data"] = data
	}
	wire, _ := json.Marshal(value)
	if _, err := peer.Write(append(wire, '\n')); err != nil {
		t.Error(err)
	}
}
func boundReadNode(t *testing.T) (*Node, net.Conn) {
	return boundReadNodeForContext(t, contract.ContextFresh)
}

func boundReadNodeForContext(t *testing.T, mode contract.ContextMode) (*Node, net.Conn) {
	t.Helper()
	identity, err := NewIdentity(IdentitySpec{SessionID: "child-1", ParentID: "root-1", RootID: "root-1", Kind: contract.ChildKindRead, Context: mode, Worker: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	host, peer := net.Pipe()
	rpc := pirpc.NewClient(host)
	broker := NewEventBroker()
	local := NewLocalSession(identity, rpc, broker)
	go func() {
		reader := bufio.NewReader(peer)
		command := readRPCCommand(t, reader)
		writeRPCResponse(t, peer, command.ID, "get_state", true, map[string]any{"sessionId": "pi-child", "sessionFile": "/tmp/child.jsonl", "isStreaming": false})
	}()
	if err := local.Bind(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &Node{identity: identity, broker: broker, local: local, state: LifecycleReady}, peer
}

// TestRunReadTaskPrefersCancelledContextOverRPCStreamEnd proves that when the
// inherited read context is already cancelled, RunReadTask returns that context
// error rather than classifying a racing Pi RPC stream EOF as child_failed.
// The select between the already-cancelled context and the RPC EOF is random, so
// the assertion (always context.Canceled) is repeated across many trials so that
// any single rpc.Done selection that incorrectly returns child_failed fails RED.
func TestRunReadTaskPrefersCancelledContextOverRPCStreamEnd(t *testing.T) {
	for trial := 0; trial < 40; trial++ {
		identity, err := NewIdentity(IdentitySpec{SessionID: "child-1", ParentID: "root-1", RootID: "root-1", Kind: contract.ChildKindRead, Context: contract.ContextFresh, Worker: "reviewer"})
		if err != nil {
			t.Fatal(err)
		}
		host, peer := net.Pipe()
		defer func() { _ = peer.Close() }()
		rpc := pirpc.NewClient(host)
		broker := NewEventBroker()
		local := NewLocalSession(identity, rpc, broker)
		serverDone := make(chan struct{})
		promptAcked := make(chan struct{})
		release := make(chan struct{})
		go func() {
			defer close(serverDone)
			reader := bufio.NewReader(peer)
			getState := readRPCCommand(t, reader)
			writeRPCResponse(t, peer, getState.ID, "get_state", true, map[string]any{"sessionId": "pi-child", "sessionFile": "/tmp/child.jsonl", "isStreaming": false})
			prompt := readRPCCommand(t, reader)
			writeRPCResponse(t, peer, prompt.ID, "prompt", true, nil)
			close(promptAcked)
			// Await the release so the prompt is submitted under a live context before
			// the transport EOF races the cancellation inside RunReadTask.
			<-release
			_ = peer.Close()
		}()
		if err := local.Bind(context.Background()); err != nil {
			t.Fatal(err)
		}
		node := &Node{identity: identity, broker: broker, local: local, state: LifecycleReady}

		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, err := node.RunReadTask(ctx, "exact task")
			result <- err
		}()
		<-promptAcked
		cancel()
		close(release)
		err = <-result
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("trial %d: error = %v, want inherited context.Canceled (not child_failed)", trial, err)
		}
		select {
		case <-serverDone:
		case <-time.After(time.Second):
			t.Fatalf("trial %d: fake Pi did not finish", trial)
		}
	}
}

func TestRunReadTaskIgnoresSuccessfulRetainedGenerationBeforeExactPrompt(t *testing.T) {
	node, peer := boundReadNode(t)
	defer func() { _ = peer.Close() }()
	_, _ = peer.Write([]byte(`{"type":"message_end","message":{"role":"assistant","stopReason":"stop"}}` + "\n" + `{"type":"agent_settled"}` + "\n"))
	waitFor(t, func() bool { return node.broker.SourceBoundary("child-1") >= 2 }, "old generation replay")
	go func() {
		reader := bufio.NewReader(peer)
		prompt := readRPCCommand(t, reader)
		if prompt.Type != "prompt" || prompt.Message != "new exact task" {
			t.Errorf("prompt = %#v", prompt)
		}
		writeRPCResponse(t, peer, prompt.ID, "prompt", true, nil)
		_, _ = peer.Write([]byte(`{"type":"message_end","message":{"role":"assistant","stopReason":"stop"}}` + "\n" + `{"type":"agent_settled"}` + "\n"))
		last := readRPCCommand(t, reader)
		writeRPCResponse(t, peer, last.ID, "get_last_assistant_text", true, map[string]any{"text": "new answer"})
	}()
	result, err := node.RunReadTask(context.Background(), "new exact task")
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "new answer" {
		t.Fatalf("result = %#v", result)
	}
}
