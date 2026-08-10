package process

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/provision"
)

const MaxRecordBytes = 1 << 20

var (
	ErrRecordTooLarge    = errors.New("process protocol record exceeds 1 MiB")
	ErrTerminalAckClosed = errors.New("terminal acknowledgement endpoint closed without acknowledgement")
)

type Bootstrap struct {
	SessionID      string                      `json:"sessionId"`
	ParentID       string                      `json:"parentId"`
	RootID         string                      `json:"rootId"`
	SocketPath     string                      `json:"socketPath"`
	SourceInstance string                      `json:"sourceInstance"`
	SourceVolume   string                      `json:"sourceVolume"`
	Policy         config.SessionModelPolicy   `json:"policy"`
	Workspace      config.WorkspaceStart       `json:"workspace"`
	Request        contract.CreateChildRequest `json:"request"`
	RunAttribution string                      `json:"runAttribution,omitempty"`
}

type ReadyMessage struct {
	SocketPath string `json:"socketPath"`
}

type WireError struct {
	Code    contract.ErrorCode `json:"code"`
	Message string             `json:"message"`
}

type ChildMessage struct {
	Type      string                     `json:"type"`
	SessionID string                     `json:"sessionId"`
	Ownership *provision.RecoveryTicket  `json:"ownership,omitempty"`
	Ready     *ReadyMessage              `json:"ready,omitempty"`
	Read      *contract.ReadChildResult  `json:"read,omitempty"`
	Write     *contract.WriteChildResult `json:"write,omitempty"`
	Error     *WireError                 `json:"error,omitempty"`
}

const (
	MessageOwnership = "ownership"
	MessageReady     = "ready"
	MessageRead      = "read"
	MessageWrite     = "write"
	MessageFailure   = "failure"
)

func EncodeBootstrap(writer io.Writer, bootstrap Bootstrap) error {
	if err := bootstrap.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(bootstrap)
	if err != nil {
		return fmt.Errorf("encode child bootstrap: %w", err)
	}
	if len(data) > MaxRecordBytes {
		return ErrRecordTooLarge
	}
	if _, err := writer.Write(data); err != nil {
		return fmt.Errorf("write child bootstrap: %w", err)
	}
	return nil
}

func DecodeBootstrap(reader io.Reader) (Bootstrap, error) {
	var bootstrap Bootstrap
	if err := strictDecode(reader, &bootstrap); err != nil {
		return Bootstrap{}, fmt.Errorf("decode child bootstrap: %w", err)
	}
	if err := bootstrap.Validate(); err != nil {
		return Bootstrap{}, fmt.Errorf("validate child bootstrap: %w", err)
	}
	return bootstrap, nil
}

func (bootstrap Bootstrap) Validate() error {
	for field, value := range map[string]string{
		"session ID":      bootstrap.SessionID,
		"parent ID":       bootstrap.ParentID,
		"root ID":         bootstrap.RootID,
		"source instance": bootstrap.SourceInstance,
		"source volume":   bootstrap.SourceVolume,
	} {
		if strings.TrimSpace(value) == "" {
			return contract.NewError(contract.ErrorInvalidRequest, field+" is required")
		}
	}
	if bootstrap.SessionID == bootstrap.ParentID {
		return contract.NewError(contract.ErrorInvalidRequest, "child session ID must differ from parent ID")
	}
	if bootstrap.SessionID == bootstrap.RootID {
		return contract.NewError(contract.ErrorInvalidRequest, "child session ID must differ from root ID")
	}
	if !filepath.IsAbs(bootstrap.SocketPath) || filepath.Clean(bootstrap.SocketPath) != bootstrap.SocketPath {
		return contract.NewError(contract.ErrorInvalidRequest, "child socket path must be an absolute clean path")
	}
	if err := bootstrap.Policy.Validate(); err != nil {
		return contract.NewError(contract.ErrorInvalidRequest, "session model policy is invalid: "+err.Error())
	}
	if err := bootstrap.Workspace.Validate(); err != nil {
		return contract.NewError(contract.ErrorInvalidRequest, "workspace start is invalid: "+err.Error())
	}
	if err := bootstrap.Request.Validate(); err != nil {
		return err
	}
	if _, err := bootstrap.Policy.ResolveWorker(bootstrap.Request.WorkerType); err != nil {
		return contract.NewError(contract.ErrorUnknownWorkerType, err.Error())
	}
	return nil
}

func DecodeChildMessage(reader io.Reader) (ChildMessage, error) {
	var message ChildMessage
	if err := strictDecode(reader, &message); err != nil {
		return ChildMessage{}, fmt.Errorf("decode child message: %w", err)
	}
	if err := message.Validate(); err != nil {
		return ChildMessage{}, fmt.Errorf("validate child message: %w", err)
	}
	return message, nil
}

func (message ChildMessage) Validate() error {
	if strings.TrimSpace(message.SessionID) == "" {
		return fmt.Errorf("sessionId is required")
	}
	payloads := 0
	for _, present := range []bool{message.Ownership != nil, message.Ready != nil, message.Read != nil, message.Write != nil, message.Error != nil} {
		if present {
			payloads++
		}
	}
	if payloads != 1 {
		return fmt.Errorf("child message must contain exactly one payload")
	}
	switch message.Type {
	case MessageOwnership:
		if message.Ownership == nil || message.Ready != nil || message.Read != nil || message.Write != nil || message.Error != nil {
			return fmt.Errorf("ownership message has the wrong payload")
		}
		if message.Ownership.SessionID != message.SessionID {
			return fmt.Errorf("ownership session ID does not match envelope")
		}
	case MessageReady:
		if message.Ready == nil || message.Ownership != nil || message.Read != nil || message.Write != nil || message.Error != nil {
			return fmt.Errorf("ready message has the wrong payload")
		}
		if !filepath.IsAbs(message.Ready.SocketPath) || filepath.Clean(message.Ready.SocketPath) != message.Ready.SocketPath {
			return fmt.Errorf("ready socket path must be absolute and clean")
		}
	case MessageRead:
		if message.Read == nil || message.Ready != nil || message.Write != nil || message.Error != nil {
			return fmt.Errorf("read message has the wrong payload")
		}
		if message.Read.SessionID != message.SessionID {
			return fmt.Errorf("read result session ID does not match envelope")
		}
		if err := message.Read.Validate(); err != nil {
			return err
		}
	case MessageWrite:
		if message.Write == nil || message.Ready != nil || message.Read != nil || message.Error != nil {
			return fmt.Errorf("write message has the wrong payload")
		}
		if message.Write.SessionID != message.SessionID {
			return fmt.Errorf("write result session ID does not match envelope")
		}
		if err := message.Write.Validate(); err != nil {
			return err
		}
	case MessageFailure:
		if message.Error == nil || message.Ready != nil || message.Read != nil || message.Write != nil {
			return fmt.Errorf("failure message has the wrong payload")
		}
		if message.Error.Code == "" || strings.TrimSpace(message.Error.Message) == "" {
			return fmt.Errorf("failure code and message are required")
		}
	default:
		return fmt.Errorf("unknown child message type %q", message.Type)
	}
	return nil
}

func strictDecode(reader io.Reader, target any) error {
	data, err := io.ReadAll(io.LimitReader(reader, MaxRecordBytes+1))
	if err != nil {
		return err
	}
	if len(data) > MaxRecordBytes {
		return ErrRecordTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are forbidden")
		}
		return err
	}
	return nil
}

const TerminalAckByte byte = 0x06

type Reporter struct {
	mu           sync.Mutex
	writer       io.Writer
	sessionID    string
	ctx          context.Context
	terminalAck  io.ReadCloser
	terminalMu   sync.Mutex
	terminalSent bool
}

// NewAcknowledgedReporter binds terminal reporting to the private inherited
// parent acknowledgement endpoint. A terminal method does not return until the
// direct parent writes exactly one acknowledgement byte and closes the pipe.
func NewAcknowledgedReporter(ctx context.Context, writer io.Writer, terminalAck io.ReadCloser, sessionID string) *Reporter {
	if ctx == nil {
		ctx = context.Background()
	}
	return &Reporter{writer: writer, sessionID: sessionID, ctx: ctx, terminalAck: terminalAck}
}

func (reporter *Reporter) Ownership(ticket provision.RecoveryTicket) error {
	return reporter.sendNonterminal(ChildMessage{Type: MessageOwnership, SessionID: reporter.sessionID, Ownership: &ticket})
}

func (reporter *Reporter) Ready(socketPath string) error {
	return reporter.sendNonterminal(ChildMessage{Type: MessageReady, SessionID: reporter.sessionID, Ready: &ReadyMessage{SocketPath: socketPath}})
}

func (reporter *Reporter) Read(result contract.ReadChildResult) error {
	return reporter.sendTerminal(ChildMessage{Type: MessageRead, SessionID: reporter.sessionID, Read: &result})
}

func (reporter *Reporter) Write(result contract.WriteChildResult) error {
	return reporter.sendTerminal(ChildMessage{Type: MessageWrite, SessionID: reporter.sessionID, Write: &result})
}

func (reporter *Reporter) Failure(code contract.ErrorCode, message string) error {
	return reporter.sendTerminal(ChildMessage{Type: MessageFailure, SessionID: reporter.sessionID, Error: &WireError{Code: code, Message: message}})
}

func (reporter *Reporter) TerminalSent() bool {
	reporter.terminalMu.Lock()
	defer reporter.terminalMu.Unlock()
	return reporter.terminalSent
}

func (reporter *Reporter) sendTerminal(message ChildMessage) error {
	reporter.terminalMu.Lock()
	if reporter.terminalSent {
		reporter.terminalMu.Unlock()
		return fmt.Errorf("child terminal report already sent")
	}
	reporter.terminalSent = true
	reporter.terminalMu.Unlock()
	if err := reporter.sendWire(message); err != nil {
		return err
	}
	if reporter.terminalAck == nil {
		return fmt.Errorf("terminal acknowledgement reader is required")
	}
	return reporter.waitForTerminalAck()
}

func (reporter *Reporter) waitForTerminalAck() error {
	result := make(chan error, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(reporter.terminalAck, 2))
		closeErr := reporter.terminalAck.Close()
		if err == nil && closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			err = closeErr
		}
		if err != nil {
			result <- fmt.Errorf("read terminal acknowledgement: %w", err)
			return
		}
		if len(data) == 0 {
			result <- ErrTerminalAckClosed
			return
		}
		if len(data) != 1 || data[0] != TerminalAckByte {
			result <- fmt.Errorf("terminal acknowledgement must contain exactly byte 0x%02x", TerminalAckByte)
			return
		}
		result <- nil
	}()
	select {
	case err := <-result:
		return err
	case <-reporter.ctx.Done():
		_ = reporter.terminalAck.Close()
		<-result
		return reporter.ctx.Err()
	}
}

func (reporter *Reporter) sendNonterminal(message ChildMessage) error {
	reporter.terminalMu.Lock()
	defer reporter.terminalMu.Unlock()
	if reporter.terminalSent {
		return fmt.Errorf("child terminal report already sent")
	}
	return reporter.sendWire(message)
}

func (reporter *Reporter) sendWire(message ChildMessage) error {
	if err := message.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(data)+1 > MaxRecordBytes {
		return ErrRecordTooLarge
	}
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if _, err := reporter.writer.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write child report: %w", err)
	}
	return nil
}
