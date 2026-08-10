package pirpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/eventmailbox"
)

func TestProtocolDecodesExactGetStateEnvelope(t *testing.T) {
	const record = `{"id":"private-1","type":"response","command":"get_state","success":true,"data":{"model":{"provider":"openai-codex","id":"gpt-5.6-sol"},"thinkingLevel":"xhigh","isStreaming":false,"isCompacting":false,"steeringMode":"all","followUpMode":"one-at-a-time","sessionFile":"/workspace/.pi/sessions/root.jsonl","sessionId":"pi-root","sessionName":"root","autoCompactionEnabled":true,"messageCount":12,"pendingMessageCount":1}}`
	var response GetStateResponse
	if err := json.Unmarshal([]byte(record), &response); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if response.Type != "response" || response.Command != "get_state" || !response.Success {
		t.Fatalf("response envelope = %#v", response)
	}
	if response.Data.SessionID != "pi-root" || response.Data.SessionFile != "/workspace/.pi/sessions/root.jsonl" || response.Data.ThinkingLevel != "xhigh" {
		t.Fatalf("get_state data = %#v", response.Data)
	}
}

func TestProtocolMarshalsExactExtensionUIResponses(t *testing.T) {
	allow := "Allow"
	confirmed := true
	tests := []struct {
		name  string
		value ExtensionUIResponse
		want  string
	}{
		{name: "select", value: ExtensionUIResponse{Type: "extension_ui_response", ID: "uuid-1", Value: &allow}, want: `{"type":"extension_ui_response","id":"uuid-1","value":"Allow"}`},
		{name: "confirm", value: ExtensionUIResponse{Type: "extension_ui_response", ID: "uuid-2", Confirmed: &confirmed}, want: `{"type":"extension_ui_response","id":"uuid-2","confirmed":true}`},
		{name: "cancel", value: ExtensionUIResponse{Type: "extension_ui_response", ID: "uuid-3", Cancelled: true}, want: `{"type":"extension_ui_response","id":"uuid-3","cancelled":true}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.value)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("Marshal() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestClientCorrelatesOutOfOrderResponses(t *testing.T) {
	clientConn, peer := net.Pipe()
	client := NewClient(clientConn)
	defer func() { _ = client.Close() }()
	defer func() { _ = peer.Close() }()

	stateResult := make(chan rpcCallResult, 1)
	messagesResult := make(chan rpcCallResult, 1)
	go func() {
		raw, err := client.Call(context.Background(), json.RawMessage(`{"type":"get_state"}`))
		stateResult <- rpcCallResult{raw: raw, err: err}
	}()
	go func() {
		raw, err := client.Call(context.Background(), json.RawMessage(`{"type":"get_messages"}`))
		messagesResult <- rpcCallResult{raw: raw, err: err}
	}()

	var observed [2]wireCommand
	seenTypes := make(map[string]struct{}, len(observed))
	reader := bufio.NewReader(peer)
	for index := range observed {
		observed[index] = readCommand(t, reader)
		command := observed[index]
		if command.ID == "" {
			t.Fatal("private ID is empty")
		}
		if _, duplicate := seenTypes[command.Type]; duplicate {
			t.Fatalf("duplicate command type %q", command.Type)
		}
		seenTypes[command.Type] = struct{}{}
	}
	if observed[0].ID == observed[1].ID {
		t.Fatalf("private IDs = %q, %q", observed[0].ID, observed[1].ID)
	}

	respond := func(command wireCommand) {
		switch command.Type {
		case "get_state":
			writeJSONLine(t, peer, fmt.Sprintf(`{"id":%q,"type":"response","command":"get_state","success":true,"data":{"marker":"state-response"}}`, command.ID))
		case "get_messages":
			writeJSONLine(t, peer, fmt.Sprintf(`{"id":%q,"type":"response","command":"get_messages","success":true,"data":{"marker":"messages-response"}}`, command.ID))
		default:
			t.Fatalf("unexpected command type %q", command.Type)
		}
	}
	respond(observed[1])
	respond(observed[0])

	assertCallResponse(t, <-stateResult, "get_state", "state-response")
	assertCallResponse(t, <-messagesResult, "get_messages", "messages-response")
}

func TestClientDrainsEventBeforeFirstCommand(t *testing.T) {
	clientConn, peer := net.Pipe()
	client := NewClient(clientConn)
	defer func() { _ = client.Close() }()
	defer func() { _ = peer.Close() }()

	eventWritten := make(chan error, 1)
	go func() {
		_, err := io.WriteString(peer, `{"type":"agent_start"}`+"\n")
		eventWritten <- err
	}()
	select {
	case err := <-eventWritten:
		if err != nil {
			t.Fatalf("write initial event: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("initial event blocked before first command")
	}

	callDone := make(chan error, 1)
	go func() {
		_, err := client.Call(context.Background(), json.RawMessage(`{"type":"get_state"}`))
		callDone <- err
	}()
	command := readCommand(t, bufio.NewReader(peer))
	writeJSONLine(t, peer, fmt.Sprintf(`{"id":%q,"type":"response","command":"get_state","success":true}`, command.ID))
	if err := <-callDone; err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	select {
	case event := <-client.Events():
		if event.Type != "agent_start" || string(event.Raw) != `{"type":"agent_start"}` {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("initial event not published")
	}
}

func TestClientReplacesCallerID(t *testing.T) {
	clientConn, peer := net.Pipe()
	client := NewClient(clientConn)
	defer func() { _ = client.Close() }()
	defer func() { _ = peer.Close() }()

	done := make(chan error, 1)
	go func() {
		_, err := client.Call(context.Background(), json.RawMessage(`{"id":"caller-controlled","type":"get_state"}`))
		done <- err
	}()
	command := readCommand(t, bufio.NewReader(peer))
	if command.ID == "caller-controlled" || command.ID == "" {
		t.Fatalf("written ID = %q, want generated private ID", command.ID)
	}
	writeJSONLine(t, peer, fmt.Sprintf(`{"id":%q,"type":"response","command":"get_state","success":true}`, command.ID))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestClientSerializesConcurrentWrites(t *testing.T) {
	clientConn, peer := net.Pipe()
	probe := &concurrentWriteProbe{ReadWriteCloser: clientConn}
	client := NewClient(probe)
	defer func() { _ = client.Close() }()
	defer func() { _ = peer.Close() }()

	const writes = 32
	readDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(peer)
		for range writes {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				readDone <- err
				return
			}
			if !json.Valid(bytes.TrimSuffix(line, []byte{'\n'})) {
				readDone <- fmt.Errorf("invalid JSON record %q", line)
				return
			}
		}
		readDone <- nil
	}()

	var wg sync.WaitGroup
	for i := range writes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := client.Send(context.Background(), json.RawMessage(fmt.Sprintf(`{"type":"extension_ui_response","id":"ui-%d","cancelled":true}`, i))); err != nil {
				t.Errorf("Send() error = %v", err)
			}
		}(i)
	}
	wg.Wait()
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	if probe.concurrent.Load() {
		t.Fatal("connection Write calls overlapped")
	}
}

func TestClientCancellationRemovesPendingCall(t *testing.T) {
	clientConn, peer := net.Pipe()
	client := NewClient(clientConn)
	defer func() { _ = client.Close() }()
	defer func() { _ = peer.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Call(ctx, json.RawMessage(`{"type":"get_state"}`))
		done <- err
	}()
	command := readCommand(t, bufio.NewReader(peer))
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Call() error = %v, want context.Canceled", err)
	}

	writeJSONLine(t, peer, fmt.Sprintf(`{"id":%q,"type":"response","command":"get_state","success":true}`, command.ID))
	select {
	case event := <-client.Events():
		if event.Type != "response" {
			t.Fatalf("late response event type = %q", event.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("late response was not published after cancellation")
	}
}

func TestClientCancellationWhileWriteQueuedKeepsTransport(t *testing.T) {
	clientConn, peer := net.Pipe()
	probe := &blockingFirstWriteProbe{
		ReadWriteCloser: clientConn,
		started:         make(chan struct{}),
		release:         make(chan struct{}),
	}
	client := NewClient(probe)
	defer func() { _ = client.Close() }()
	defer func() { _ = peer.Close() }()

	firstDone := make(chan error, 1)
	go func() {
		_, err := client.Call(context.Background(), json.RawMessage(`{"type":"get_state"}`))
		firstDone <- err
	}()
	<-probe.started

	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		_, err := client.Call(ctx, json.RawMessage(`{"type":"get_messages"}`))
		secondDone <- err
	}()
	waitForPendingCount(t, client, 2)
	cancel()

	var secondErr error
	cancelledBeforeRelease := false
	select {
	case secondErr = <-secondDone:
		cancelledBeforeRelease = true
		if !errors.Is(secondErr, context.Canceled) {
			t.Errorf("queued Call() error = %v, want context.Canceled", secondErr)
		}
		if got := pendingCount(client); got != 1 {
			t.Errorf("pending calls after queued cancellation = %d, want 1", got)
		}
	case <-time.After(100 * time.Millisecond):
	}

	close(probe.release)
	command := readCommand(t, bufio.NewReader(peer))
	if command.Type != "get_state" {
		t.Errorf("written command = %q, want get_state", command.Type)
	}
	writeJSONLine(t, peer, fmt.Sprintf(`{"id":%q,"type":"response","command":"get_state","success":true}`, command.ID))
	if err := <-firstDone; err != nil {
		t.Fatalf("first Call() error = %v; queued cancellation terminated transport", err)
	}
	if !cancelledBeforeRelease {
		secondErr = <-secondDone
		if !errors.Is(secondErr, context.Canceled) {
			t.Errorf("queued Call() error = %v, want context.Canceled", secondErr)
		}
		t.Error("queued Call() did not return until the blocked first write was released")
	}
}

func TestClientEOFFailsEveryPendingCall(t *testing.T) {
	clientConn, peer := net.Pipe()
	client := NewClient(clientConn)
	defer func() { _ = client.Close() }()

	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := client.Call(context.Background(), json.RawMessage(`{"type":"get_state"}`))
			errs <- err
		}()
	}
	reader := bufio.NewReader(peer)
	_ = readCommand(t, reader)
	_ = readCommand(t, reader)
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-errs; err == nil || !errors.Is(err, io.EOF) {
			t.Fatalf("pending Call() error = %v, want EOF", err)
		}
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("Done() not closed after EOF")
	}
}

func TestClientAcceptsExactMaxRecord(t *testing.T) {
	clientConn, peer := net.Pipe()
	client := NewClient(clientConn)
	defer func() { _ = client.Close() }()
	defer func() { _ = peer.Close() }()

	record := sizedJSONRecord(t, "message_update", MaxRecordBytes)
	writeDone := make(chan error, 1)
	go func() {
		_, err := peer.Write(append(append([]byte(nil), record...), '\n'))
		writeDone <- err
	}()

	select {
	case event := <-client.Events():
		if event.Type != "message_update" || len(event.Raw) != len(record) {
			t.Fatalf("event type = %q, bytes = %d; want message_update with %d bytes", event.Type, len(event.Raw), len(record))
		}
	case <-client.Done():
		t.Fatalf("exact-limit record terminated client: %v", client.Err())
	case <-time.After(3 * time.Second):
		t.Fatal("exact-limit record was not published")
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("write exact-limit record: %v", err)
	}
}

func TestClientCallFinalRecordBoundary(t *testing.T) {
	t.Run("exact limit", func(t *testing.T) {
		clientConn, peer := net.Pipe()
		client := NewClient(clientConn)
		defer func() { _ = client.Close() }()
		defer func() { _ = peer.Close() }()

		nextID := fmt.Sprintf("kanedias-%s-%d", processIDPrefix, requestCounter.Load()+1)
		command := sizedCallCommand(t, nextID, MaxRecordBytes)
		result := make(chan error, 1)
		go func() {
			_, err := client.Call(context.Background(), command)
			result <- err
		}()
		line, err := bufio.NewReader(peer).ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		if got := len(line) - 1; got != MaxRecordBytes {
			t.Fatalf("record bytes = %d, want %d", got, MaxRecordBytes)
		}
		var written wireCommand
		if err := json.Unmarshal(line, &written); err != nil {
			t.Fatal(err)
		}
		writeJSONLine(t, peer, fmt.Sprintf(`{"id":%q,"type":"response","success":true}`, written.ID))
		if err := <-result; err != nil {
			t.Fatalf("Call() error = %v", err)
		}
	})

	t.Run("one byte over", func(t *testing.T) {
		conn := newWriteCountConn()
		client := NewClient(conn)
		defer func() { _ = client.Close() }()

		nextID := fmt.Sprintf("kanedias-%s-%d", processIDPrefix, requestCounter.Load()+1)
		command := sizedCallCommand(t, nextID, MaxRecordBytes+1)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := client.Call(ctx, command)
		want := fmt.Sprintf("record exceeds %d bytes on the Pi RPC transport", MaxRecordBytes)
		if err == nil || err.Error() != want {
			t.Fatalf("Call() error = %v, want %q", err, want)
		}
		if got := conn.bytes.Load(); got != 0 {
			t.Fatalf("oversized Call() emitted %d bytes", got)
		}
		if got := pendingCount(client); got != 0 {
			t.Fatalf("oversized Call() registered %d pending calls", got)
		}
		select {
		case <-client.Done():
			t.Fatalf("oversized Call() terminated healthy transport: %v", client.Err())
		default:
		}
	})
}

func TestClientSendFinalRecordBoundary(t *testing.T) {
	for _, tt := range []struct {
		name      string
		recordLen int
		wantErr   bool
	}{
		{name: "exact limit", recordLen: MaxRecordBytes},
		{name: "one byte over", recordLen: MaxRecordBytes + 1, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conn := newWriteCountConn()
			client := NewClient(conn)
			defer func() { _ = client.Close() }()
			command := sizedJSONRecord(t, "extension_ui_response", tt.recordLen)

			err := client.Send(context.Background(), command)
			if tt.wantErr {
				want := fmt.Sprintf("record exceeds %d bytes on the Pi RPC transport", MaxRecordBytes)
				if err == nil || err.Error() != want {
					t.Fatalf("Send() error = %v, want %q", err, want)
				}
				if got := conn.bytes.Load(); got != 0 {
					t.Fatalf("oversized Send() emitted %d bytes", got)
				}
				select {
				case <-client.Done():
					t.Fatalf("oversized Send() terminated healthy transport: %v", client.Err())
				default:
				}
				return
			}
			if err != nil {
				t.Fatalf("Send() error = %v", err)
			}
			if got := conn.bytes.Load(); got != int64(MaxRecordBytes+1) {
				t.Fatalf("exact-limit Send() wire bytes = %d, want %d including delimiter", got, MaxRecordBytes+1)
			}
		})
	}
}

func TestClientRejectsPartialMalformedAndOversizedRecords(t *testing.T) {
	tests := []struct {
		name   string
		record func(io.Writer) error
		want   string
	}{
		{name: "partial", record: func(w io.Writer) error { _, err := io.WriteString(w, `{"type":"agent_start"}`); return err }, want: "partial record"},
		{name: "malformed", record: func(w io.Writer) error { _, err := io.WriteString(w, "{bad json}\n"); return err }, want: "decode RPC record"},
		{name: "oversized", record: func(w io.Writer) error {
			record := sizedJSONRecord(t, "message_update", MaxRecordBytes+1)
			_, err := w.Write(append(record, '\n'))
			return err
		}, want: "exceeds"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientConn, peer := net.Pipe()
			client := NewClient(clientConn)
			writeDone := make(chan error, 1)
			go func() {
				writeDone <- tt.record(peer)
				_ = peer.Close()
			}()
			select {
			case <-client.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("client did not terminate")
			}
			<-writeDone
			if err := client.Err(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Err() = %v, want containing %q", err, tt.want)
			}
			_ = client.Close()
		})
	}
}

func TestClientKeepsReadingAfterAgentSettled(t *testing.T) {
	clientConn, peer := net.Pipe()
	client := NewClient(clientConn)
	defer func() { _ = client.Close() }()
	defer func() { _ = peer.Close() }()

	writeJSONLineAsync(t, peer, `{"type":"agent_settled"}`)
	select {
	case event := <-client.Events():
		if event.Type != "agent_settled" {
			t.Fatalf("event type = %q", event.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("agent_settled not published")
	}

	done := make(chan error, 1)
	go func() {
		_, err := client.Call(context.Background(), json.RawMessage(`{"type":"get_state"}`))
		done <- err
	}()
	command := readCommand(t, bufio.NewReader(peer))
	writeJSONLine(t, peer, fmt.Sprintf(`{"id":%q,"type":"response","command":"get_state","success":true}`, command.ID))
	if err := <-done; err != nil {
		t.Fatalf("Call() after agent_settled error = %v", err)
	}
}

func TestClientRejectsSessionReplacementWithoutWriting(t *testing.T) {
	for _, commandType := range []string{"new_session", "switch_session", "fork", "clone"} {
		t.Run(commandType, func(t *testing.T) {
			clientConn, peer := net.Pipe()
			client := NewClient(clientConn)
			defer func() { _ = client.Close() }()
			defer func() { _ = peer.Close() }()
			if err := peer.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}

			_, err := client.Call(context.Background(), json.RawMessage(fmt.Sprintf(`{"type":%q}`, commandType)))
			if !errors.Is(err, ErrForbiddenCommand) {
				t.Fatalf("Call() error = %v, want ErrForbiddenCommand", err)
			}
			one := make([]byte, 1)
			if _, err := peer.Read(one); err == nil {
				t.Fatal("forbidden command wrote to connection")
			} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
				t.Fatalf("peer Read() error = %v, want timeout proving no write", err)
			}
		})
	}
}

func TestClientSendRejectsSessionReplacementWithoutWriting(t *testing.T) {
	for _, commandType := range []string{"new_session", "switch_session", "fork", "clone"} {
		t.Run(commandType, func(t *testing.T) {
			clientConn, peer := net.Pipe()
			client := NewClient(clientConn)
			defer func() { _ = client.Close() }()
			defer func() { _ = peer.Close() }()
			if err := peer.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)
			go func() {
				done <- client.Send(context.Background(), json.RawMessage(fmt.Sprintf(`{"type":%q}`, commandType)))
			}()
			one := make([]byte, 1)
			if _, err := peer.Read(one); err == nil {
				t.Fatal("forbidden command wrote to connection")
			} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
				t.Fatalf("peer Read() error = %v, want timeout proving no write", err)
			}
			if err := <-done; !errors.Is(err, ErrForbiddenCommand) {
				t.Fatalf("Send() error = %v, want ErrForbiddenCommand", err)
			}
		})
	}
}

func TestClientCloseClosesUnderlyingConnectionExactlyOnce(t *testing.T) {
	firstCloseErr := errors.New("first close failed")
	secondCloseErr := errors.New("second close failed")
	tests := []struct {
		name      string
		closeErrs []error
		wantErr   error
	}{
		{name: "second close failure is irrelevant", closeErrs: []error{nil, secondCloseErr}, wantErr: nil},
		{name: "first close failure is returned", closeErrs: []error{firstCloseErr}, wantErr: firstCloseErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientConn, peer := net.Pipe()
			probe := &closeResultProbe{ReadWriteCloser: clientConn, results: tt.closeErrs}
			client := NewClient(probe)
			defer func() { _ = peer.Close() }()

			err := client.Close()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Close() error = %v, want %v", err, tt.wantErr)
			}
			if got := probe.calls.Load(); got != 1 {
				t.Fatalf("underlying Close() calls = %d, want 1", got)
			}
		})
	}
}

func TestClientPreservesOrdinaryEventBurstAndRemainsUsable(t *testing.T) {
	clientConn, peer := net.Pipe()
	client := NewClient(clientConn)
	defer func() { _ = client.Close(); _ = peer.Close() }()

	writeDone := make(chan error, 1)
	go func() {
		writer := bufio.NewWriter(peer)
		for seq := 1; seq <= 256; seq++ {
			if _, err := fmt.Fprintf(writer, "{\"type\":\"message_update\",\"seq\":%d}\n", seq); err != nil {
				writeDone <- err
				return
			}
		}
		writeDone <- writer.Flush()
	}()

	for want := uint64(1); want <= 256; want++ {
		select {
		case event, open := <-client.Events():
			if !open || event.Seq != want {
				t.Fatalf("event %d = %#v, open=%t", want, event, open)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out at event %d", want)
		}
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if client.Err() != nil {
		t.Fatalf("client failed during burst: %v", client.Err())
	}
}

func TestClientCapacityOneSurvivesRepeatedReceiveHandoffs(t *testing.T) {
	clientConn, peer := net.Pipe()
	client := newClientWithEventLimits(clientConn, eventmailbox.Limits{MaxEvents: 1})
	defer func() { _ = client.Close(); _ = peer.Close() }()

	for want := uint64(1); want <= 100; want++ {
		writeJSONLineAsync(t, peer, `{"type":"message_update"}`)
		select {
		case event, open := <-client.Events():
			if !open || event.Seq != want {
				t.Fatalf("event %d = %#v, open=%t", want, event, open)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %d", want)
		}
		if err := client.Err(); err != nil {
			t.Fatalf("event %d terminated capacity-one client: %v", want, err)
		}
	}
}

func TestClientDisconnectsStalledEventConsumerAtByteBoundedCapacity(t *testing.T) {
	clientConn, peer := net.Pipe()
	client := newClientWithEventLimits(clientConn, eventmailbox.Limits{MaxEvents: 1, MaxBytes: 1024})
	defer func() { _ = peer.Close() }()

	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(peer, `{"type":"message_update","payload":"one"}`+"\n"+
			`{"type":"message_update","payload":"two"}`+"\n")
		writeDone <- err
	}()
	select {
	case <-client.Done():
		const want = "pi RPC event consumer exceeded bounded capacity"
		if err := client.Err(); err == nil || err.Error() != want {
			t.Fatalf("client error = %v, want %q", err, want)
		}
	case <-time.After(time.Second):
		t.Fatal("stalled event consumer did not terminate transport")
	}
	<-writeDone
}

func TestClientDispatchesCorrelatedResponseBeforeFullEventMailbox(t *testing.T) {
	clientConn, peer := net.Pipe()
	client := newClientWithEventLimits(clientConn, eventmailbox.Limits{MaxEvents: 1, MaxBytes: 1024})
	defer func() { _ = client.Close(); _ = peer.Close() }()

	writeJSONLineAsync(t, peer, `{"type":"message_update"}`)
	callDone := make(chan rpcCallResult, 1)
	go func() {
		raw, err := client.Call(context.Background(), json.RawMessage(`{"type":"get_state"}`))
		callDone <- rpcCallResult{raw: raw, err: err}
	}()
	command := readCommand(t, bufio.NewReader(peer))
	writeJSONLineAsync(t, peer, fmt.Sprintf(`{"id":%q,"type":"response","command":"get_state","success":true}`, command.ID))

	select {
	case result := <-callDone:
		if result.err != nil {
			t.Fatalf("Call() error = %v", result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("correlated response waited for event mailbox draining")
	}
}

func TestClientCloseUnblocksEventBackpressure(t *testing.T) {
	clientConn, peer := net.Pipe()
	client := newClientWithEventLimits(clientConn, eventmailbox.Limits{MaxEvents: 2, MaxBytes: 1024})
	defer func() { _ = peer.Close() }()

	writeDone := make(chan error, 1)
	go func() {
		for range 2 {
			if _, err := io.WriteString(peer, `{"type":"message_update"}`+"\n"); err != nil {
				writeDone <- err
				return
			}
		}
		writeDone <- nil
	}()

	// Let the reader fill its bounded event mailbox while its consumer is idle.
	time.Sleep(20 * time.Millisecond)
	closeDone := make(chan error, 1)
	go func() { closeDone <- client.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close() blocked behind an unread event channel")
	}
	<-writeDone
}

func TestClientSendWritesExtensionUIResponseWithoutWaiting(t *testing.T) {
	clientConn, peer := net.Pipe()
	client := NewClient(clientConn)
	defer func() { _ = client.Close() }()
	defer func() { _ = peer.Close() }()

	const response = `{"type":"extension_ui_response","id":"uuid-1","value":"Allow"}`
	done := make(chan error, 1)
	go func() { done <- client.Send(context.Background(), json.RawMessage(response)) }()
	line, err := bufio.NewReader(peer).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSuffix(line, "\n"); got != response {
		t.Fatalf("wire response = %s, want %s", got, response)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Send() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Send() waited for a correlated response")
	}
}

func sizedJSONRecord(t *testing.T, recordType string, size int) json.RawMessage {
	t.Helper()
	prefix := fmt.Sprintf(`{"type":%q,"padding":"`, recordType)
	const suffix = `"}`
	padding := size - len(prefix) - len(suffix)
	if padding < 0 {
		t.Fatalf("record size %d is too small", size)
	}
	record := json.RawMessage(prefix + strings.Repeat("x", padding) + suffix)
	if len(record) != size || !json.Valid(record) {
		t.Fatalf("sized record bytes = %d, valid = %t; want %d valid bytes", len(record), json.Valid(record), size)
	}
	return record
}

func sizedCallCommand(t *testing.T, id string, finalSize int) json.RawMessage {
	t.Helper()
	base := json.RawMessage(`{"type":"get_state","padding":""}`)
	wire, err := commandWithID(base, id)
	if err != nil {
		t.Fatal(err)
	}
	padding := finalSize - len(wire)
	if padding < 0 {
		t.Fatalf("final record size %d is too small", finalSize)
	}
	command := json.RawMessage(`{"type":"get_state","padding":"` + strings.Repeat("x", padding) + `"}`)
	wire, err = commandWithID(command, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) != finalSize {
		t.Fatalf("final record bytes = %d, want %d", len(wire), finalSize)
	}
	return command
}

type writeCountConn struct {
	closed chan struct{}
	once   sync.Once
	bytes  atomic.Int64
}

func newWriteCountConn() *writeCountConn {
	return &writeCountConn{closed: make(chan struct{})}
}

func (conn *writeCountConn) Read([]byte) (int, error) {
	<-conn.closed
	return 0, io.ErrClosedPipe
}

func (conn *writeCountConn) Write(p []byte) (int, error) {
	conn.bytes.Add(int64(len(p)))
	return len(p), nil
}

func (conn *writeCountConn) Close() error {
	conn.once.Do(func() { close(conn.closed) })
	return nil
}

type wireCommand struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type rpcCallResult struct {
	raw json.RawMessage
	err error
}

func assertCallResponse(t *testing.T, got rpcCallResult, wantCommand, wantMarker string) {
	t.Helper()
	if got.err != nil {
		t.Fatalf("Call() error = %v", got.err)
	}
	var response struct {
		Command string `json:"command"`
		Data    struct {
			Marker string `json:"marker"`
		} `json:"data"`
	}
	if err := json.Unmarshal(got.raw, &response); err != nil {
		t.Fatal(err)
	}
	if response.Command != wantCommand || response.Data.Marker != wantMarker {
		t.Fatalf("Call() response = %#v, want command %q and marker %q", response, wantCommand, wantMarker)
	}
}

func readCommand(t *testing.T, reader *bufio.Reader) wireCommand {
	t.Helper()
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read command: %v", err)
	}
	var command wireCommand
	if err := json.Unmarshal(line, &command); err != nil {
		t.Fatalf("decode command: %v", err)
	}
	return command
}

func writeJSONLine(t *testing.T, writer io.Writer, record string) {
	t.Helper()
	if _, err := io.WriteString(writer, record+"\n"); err != nil {
		t.Fatalf("write JSON line: %v", err)
	}
}

func writeJSONLineAsync(t *testing.T, writer io.Writer, record string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := io.WriteString(writer, record+"\n")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write JSON line: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("write JSON line blocked")
	}
}

func waitForPendingCount(t *testing.T, client *Client, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if pendingCount(client) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending calls = %d, want %d", pendingCount(client), want)
		}
		runtime.Gosched()
	}
}

func pendingCount(client *Client) int {
	client.mu.Lock()
	defer client.mu.Unlock()
	return len(client.pending)
}

type blockingFirstWriteProbe struct {
	io.ReadWriteCloser
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (probe *blockingFirstWriteProbe) Write(p []byte) (int, error) {
	probe.once.Do(func() {
		close(probe.started)
		<-probe.release
	})
	return probe.ReadWriteCloser.Write(p)
}

type closeResultProbe struct {
	io.ReadWriteCloser
	results []error
	calls   atomic.Int32
}

func (probe *closeResultProbe) Close() error {
	call := int(probe.calls.Add(1))
	baseErr := probe.ReadWriteCloser.Close()
	if call <= len(probe.results) && probe.results[call-1] != nil {
		return probe.results[call-1]
	}
	return baseErr
}

type concurrentWriteProbe struct {
	io.ReadWriteCloser
	writing    atomic.Int32
	concurrent atomic.Bool
}

func (probe *concurrentWriteProbe) Write(p []byte) (int, error) {
	if probe.writing.Add(1) != 1 {
		probe.concurrent.Store(true)
	}
	defer probe.writing.Add(-1)
	time.Sleep(time.Millisecond)
	return probe.ReadWriteCloser.Write(p)
}

type blockedWriteConn struct {
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func newBlockedWriteConn() *blockedWriteConn {
	return &blockedWriteConn{entered: make(chan struct{}), closed: make(chan struct{})}
}
func (conn *blockedWriteConn) Read([]byte) (int, error) { <-conn.closed; return 0, io.ErrClosedPipe }
func (conn *blockedWriteConn) Write([]byte) (int, error) {
	conn.once.Do(func() { close(conn.entered) })
	<-conn.closed
	return 0, io.ErrClosedPipe
}
func (conn *blockedWriteConn) Close() error {
	select {
	case <-conn.closed:
	default:
		close(conn.closed)
	}
	return nil
}

func TestClientCancellationClosesTransportDuringActiveBlockedWrite(t *testing.T) {
	conn := newBlockedWriteConn()
	client := NewClient(conn)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := client.Call(ctx, json.RawMessage(`{"type":"get_state"}`)); result <- err }()
	select {
	case <-conn.entered:
	case <-time.After(time.Second):
		t.Fatal("write did not become active")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Call() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active write cancellation did not return promptly")
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("ambiguous connection was not terminated")
	}
}
