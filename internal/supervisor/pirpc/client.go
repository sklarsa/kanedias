package pirpc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// 8 MiB raw images expand to about 10.7 MiB; 12 MiB leaves JSON/text headroom
// while remaining below the default 16 MiB event-byte budget.
const MaxRecordBytes = 12 << 20

var ErrForbiddenCommand = errors.New("command would replace the bound Pi RPC session")

var (
	processIDPrefix = newProcessIDPrefix()
	requestCounter  atomic.Uint64
)

type Event struct {
	// Seq is the monotonic position of this record on the Pi transport.
	Seq  uint64
	Type string
	Raw  json.RawMessage
}

type callResult struct {
	raw json.RawMessage
	seq uint64
	err error
}

type Client struct {
	conn     io.ReadWriteCloser
	events   chan Event
	done     chan struct{}
	readDone chan struct{}

	writeGate chan struct{}
	mu        sync.Mutex
	pending   map[string]chan callResult
	err       error
	closed    bool
}

func NewClient(conn io.ReadWriteCloser) *Client {
	client := &Client{
		conn:      conn,
		events:    make(chan Event, 128),
		done:      make(chan struct{}),
		readDone:  make(chan struct{}),
		writeGate: make(chan struct{}, 1),
		pending:   make(map[string]chan callResult),
	}
	go client.readLoop()
	return client
}

func (client *Client) Call(ctx context.Context, command json.RawMessage) (json.RawMessage, error) {
	raw, _, err := client.CallWithSequence(ctx, command)
	return raw, err
}

// guardCommand enforces the forbidden-command policy and honors context
// cancellation before a command is written to the Pi transport.
func guardCommand(ctx context.Context, command json.RawMessage) error {
	commandType, err := parseCommandType(command)
	if err != nil {
		return err
	}
	if isForbidden(commandType) {
		return fmt.Errorf("%w: %s", ErrForbiddenCommand, commandType)
	}
	return ctx.Err()
}

// CallWithSequence returns the response and its monotonic position on the Pi
// transport so owners can reconcile responses with asynchronously drained events.
func (client *Client) CallWithSequence(ctx context.Context, command json.RawMessage) (json.RawMessage, uint64, error) {
	if err := guardCommand(ctx, command); err != nil {
		return nil, 0, err
	}

	id := fmt.Sprintf("kanedias-%s-%d", processIDPrefix, requestCounter.Add(1))
	wire, err := commandWithID(command, id)
	if err != nil {
		return nil, 0, err
	}
	result := make(chan callResult, 1)

	client.mu.Lock()
	if client.closed {
		err := client.err
		client.mu.Unlock()
		if err == nil {
			err = io.ErrClosedPipe
		}
		return nil, 0, err
	}
	client.pending[id] = result
	client.mu.Unlock()

	if err := client.write(ctx, wire); err != nil {
		client.removePending(id)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, 0, ctxErr
		}
		_ = client.terminate(fmt.Errorf("write Pi RPC command: %w", err))
		return nil, 0, err
	}

	select {
	case response := <-result:
		return response.raw, response.seq, response.err
	case <-ctx.Done():
		client.removePending(id)
		return nil, 0, ctx.Err()
	}
}

func (client *Client) Send(ctx context.Context, command json.RawMessage) error {
	if err := guardCommand(ctx, command); err != nil {
		return err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, command); err != nil {
		return fmt.Errorf("encode Pi RPC command: %w", err)
	}
	if err := client.write(ctx, compact.Bytes()); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		_ = client.terminate(fmt.Errorf("write Pi RPC command: %w", err))
		return err
	}
	return nil
}

func (client *Client) Events() <-chan Event { return client.events }

func (client *Client) Done() <-chan struct{} { return client.done }

func (client *Client) Err() error {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.err
}

func (client *Client) Close() error {
	err := client.terminate(io.ErrClosedPipe)
	<-client.readDone
	return err
}

func (client *Client) readLoop() {
	defer close(client.events)
	defer close(client.readDone)
	reader := bufio.NewReaderSize(client.conn, MaxRecordBytes+1)
	var sequence uint64
	for {
		record, err := reader.ReadSlice('\n')
		if err != nil {
			switch {
			case errors.Is(err, bufio.ErrBufferFull):
				_ = client.terminate(fmt.Errorf("record exceeds %d bytes on the Pi RPC transport", MaxRecordBytes))
			case errors.Is(err, io.EOF) && len(record) != 0:
				_ = client.terminate(fmt.Errorf("read Pi RPC record: partial record before EOF"))
			default:
				_ = client.terminate(err)
			}
			return
		}
		record = record[:len(record)-1]
		if len(record) > MaxRecordBytes {
			_ = client.terminate(fmt.Errorf("record exceeds %d bytes on the Pi RPC transport", MaxRecordBytes))
			return
		}

		var envelope struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(record, &envelope); err != nil {
			_ = client.terminate(fmt.Errorf("decode RPC record: %w", err))
			return
		}
		sequence++
		raw := append(json.RawMessage(nil), record...)
		if envelope.Type == "response" && client.dispatchResponse(envelope.ID, sequence, raw) {
			continue
		}
		select {
		case client.events <- Event{Seq: sequence, Type: envelope.Type, Raw: raw}:
		case <-client.done:
			return
		}
	}
}

func (client *Client) dispatchResponse(id string, sequence uint64, raw json.RawMessage) bool {
	client.mu.Lock()
	result, ok := client.pending[id]
	if ok {
		delete(client.pending, id)
	}
	client.mu.Unlock()
	if ok {
		result <- callResult{raw: raw, seq: sequence}
	}
	return ok
}

func (client *Client) removePending(id string) {
	client.mu.Lock()
	delete(client.pending, id)
	client.mu.Unlock()
}

func (client *Client) write(ctx context.Context, record []byte) error {
	select {
	case client.writeGate <- struct{}{}:
		defer func() { <-client.writeGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	wire := make([]byte, 0, len(record)+1)
	wire = append(wire, record...)
	wire = append(wire, '\n')
	writeDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// Give a concurrently returning Write one scheduler turn to publish
			// completion. If it remains active, the record outcome is ambiguous.
			grace := time.NewTimer(5 * time.Millisecond)
			select {
			case <-writeDone:
				grace.Stop()
				return
			case <-grace.C:
			}
			// Once a record write has started its completion is ambiguous. Closing
			// the entire RPC transport is the only safe cancellation boundary.
			_ = client.terminate(ctx.Err())
		case <-writeDone:
		}
	}()
	written, err := client.conn.Write(wire)
	close(writeDone)
	if err != nil {
		return err
	}
	if written != len(wire) {
		return io.ErrShortWrite
	}
	return nil
}

func (client *Client) terminate(err error) error {
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		return nil
	}
	client.closed = true
	client.err = err
	pending := client.pending
	client.pending = make(map[string]chan callResult)
	close(client.done)
	client.mu.Unlock()

	for _, result := range pending {
		result <- callResult{err: err}
	}
	return client.conn.Close()
}

func parseCommandType(command json.RawMessage) (string, error) {
	var envelope commandEnvelope
	if err := json.Unmarshal(command, &envelope); err != nil {
		return "", fmt.Errorf("decode Pi RPC command: %w", err)
	}
	if envelope.Type == "" {
		return "", fmt.Errorf("decode Pi RPC command: type is required")
	}
	return envelope.Type, nil
}

func commandWithID(command json.RawMessage, id string) ([]byte, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(command, &object); err != nil {
		return nil, fmt.Errorf("decode Pi RPC command: %w", err)
	}
	encodedID, _ := json.Marshal(id)
	object["id"] = encodedID
	wire, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("encode Pi RPC command: %w", err)
	}
	return wire, nil
}

func isForbidden(commandType string) bool {
	_, forbidden := forbiddenCommandTypes[commandType]
	return forbidden
}

func newProcessIDPrefix() string {
	var random [8]byte
	if _, err := rand.Read(random[:]); err == nil {
		return hex.EncodeToString(random[:])
	}
	return fmt.Sprintf("%x", time.Now().UnixNano())
}
