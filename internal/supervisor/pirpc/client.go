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

const MaxRecordBytes = 4 << 20

var ErrForbiddenCommand = errors.New("Pi RPC command would replace the bound session")

var (
	processIDPrefix = newProcessIDPrefix()
	requestCounter  atomic.Uint64
)

type Event struct {
	Type string
	Raw  json.RawMessage
}

type callResult struct {
	raw json.RawMessage
	err error
}

type Client struct {
	conn   io.ReadWriteCloser
	events chan Event
	done   chan struct{}

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan callResult
	err     error
	closed  bool
}

func NewClient(conn io.ReadWriteCloser) *Client {
	client := &Client{
		conn:    conn,
		events:  make(chan Event, 128),
		done:    make(chan struct{}),
		pending: make(map[string]chan callResult),
	}
	go client.readLoop()
	return client
}

func (client *Client) Call(ctx context.Context, command json.RawMessage) (json.RawMessage, error) {
	commandType, err := parseCommandType(command)
	if err != nil {
		return nil, err
	}
	if isForbidden(commandType) {
		return nil, fmt.Errorf("%w: %s", ErrForbiddenCommand, commandType)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	id := fmt.Sprintf("kanedias-%s-%d", processIDPrefix, requestCounter.Add(1))
	wire, err := commandWithID(command, id)
	if err != nil {
		return nil, err
	}
	result := make(chan callResult, 1)

	client.mu.Lock()
	if client.closed {
		err := client.err
		client.mu.Unlock()
		if err == nil {
			err = io.ErrClosedPipe
		}
		return nil, err
	}
	client.pending[id] = result
	client.mu.Unlock()

	if err := client.write(ctx, wire); err != nil {
		client.removePending(id)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		client.terminate(fmt.Errorf("write Pi RPC command: %w", err))
		return nil, err
	}

	select {
	case response := <-result:
		return response.raw, response.err
	case <-ctx.Done():
		client.removePending(id)
		return nil, ctx.Err()
	}
}

func (client *Client) Send(ctx context.Context, command json.RawMessage) error {
	commandType, err := parseCommandType(command)
	if err != nil {
		return err
	}
	if isForbidden(commandType) {
		return fmt.Errorf("%w: %s", ErrForbiddenCommand, commandType)
	}
	if err := ctx.Err(); err != nil {
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
		client.terminate(fmt.Errorf("write Pi RPC command: %w", err))
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
	client.terminate(io.ErrClosedPipe)
	return client.conn.Close()
}

func (client *Client) readLoop() {
	reader := bufio.NewReaderSize(client.conn, MaxRecordBytes+1)
	for {
		record, err := reader.ReadSlice('\n')
		if err != nil {
			switch {
			case errors.Is(err, bufio.ErrBufferFull):
				client.terminate(fmt.Errorf("Pi RPC record exceeds %d bytes", MaxRecordBytes))
			case errors.Is(err, io.EOF) && len(record) != 0:
				client.terminate(fmt.Errorf("read Pi RPC record: partial record before EOF"))
			default:
				client.terminate(err)
			}
			return
		}
		record = record[:len(record)-1]
		if len(record) > MaxRecordBytes {
			client.terminate(fmt.Errorf("Pi RPC record exceeds %d bytes", MaxRecordBytes))
			return
		}

		var envelope struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(record, &envelope); err != nil {
			client.terminate(fmt.Errorf("decode RPC record: %w", err))
			return
		}
		raw := append(json.RawMessage(nil), record...)
		if envelope.Type == "response" && client.dispatchResponse(envelope.ID, raw) {
			continue
		}
		select {
		case client.events <- Event{Type: envelope.Type, Raw: raw}:
		case <-client.done:
			return
		}
	}
}

func (client *Client) dispatchResponse(id string, raw json.RawMessage) bool {
	client.mu.Lock()
	result, ok := client.pending[id]
	if ok {
		delete(client.pending, id)
	}
	client.mu.Unlock()
	if ok {
		result <- callResult{raw: raw}
	}
	return ok
}

func (client *Client) removePending(id string) {
	client.mu.Lock()
	delete(client.pending, id)
	client.mu.Unlock()
}

func (client *Client) write(ctx context.Context, record []byte) error {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	wire := make([]byte, 0, len(record)+1)
	wire = append(wire, record...)
	wire = append(wire, '\n')
	written, err := client.conn.Write(wire)
	if err != nil {
		return err
	}
	if written != len(wire) {
		return io.ErrShortWrite
	}
	return nil
}

func (client *Client) terminate(err error) {
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		return
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
	_ = client.conn.Close()
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
