package process

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

const MaxRecordBytes = 1 << 20

var ErrRecordTooLarge = errors.New("process protocol record exceeds 1 MiB")

type Bootstrap struct {
	SessionID      string                      `json:"sessionId"`
	ParentID       string                      `json:"parentId"`
	RootID         string                      `json:"rootId"`
	SocketPath     string                      `json:"socketPath"`
	SourceInstance string                      `json:"sourceInstance"`
	SourceVolume   string                      `json:"sourceVolume"`
	Worker         config.WorkerProfile        `json:"worker"`
	Request        contract.CreateChildRequest `json:"request"`
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
	Ready     *ReadyMessage              `json:"ready,omitempty"`
	Read      *contract.ReadChildResult  `json:"read,omitempty"`
	Write     *contract.WriteChildResult `json:"write,omitempty"`
	Error     *WireError                 `json:"error,omitempty"`
}

const (
	MessageReady   = "ready"
	MessageRead    = "read"
	MessageWrite   = "write"
	MessageFailure = "failure"
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
	if err := validateWorker(bootstrap.Worker); err != nil {
		return err
	}
	if err := bootstrap.Request.Validate(); err != nil {
		return err
	}
	return nil
}

func validateWorker(worker config.WorkerProfile) error {
	for field, value := range map[string]string{
		"worker description": worker.Description,
		"worker provider":    worker.Provider,
		"worker model":       worker.Model,
	} {
		if strings.TrimSpace(value) == "" {
			return contract.NewError(contract.ErrorInvalidRequest, field+" is required")
		}
	}
	if worker.ThinkingLevel != "" {
		switch worker.ThinkingLevel {
		case "off", "minimal", "low", "medium", "high", "xhigh", "max":
		default:
			return contract.NewError(contract.ErrorInvalidRequest, "worker thinking level is invalid")
		}
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
	for _, present := range []bool{message.Ready != nil, message.Read != nil, message.Write != nil, message.Error != nil} {
		if present {
			payloads++
		}
	}
	if payloads != 1 {
		return fmt.Errorf("child message must contain exactly one payload")
	}
	switch message.Type {
	case MessageReady:
		if message.Ready == nil || message.Read != nil || message.Write != nil || message.Error != nil {
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

type Reporter struct {
	mu        sync.Mutex
	writer    io.Writer
	sessionID string
}

func NewReporter(writer io.Writer, sessionID string) *Reporter {
	return &Reporter{writer: writer, sessionID: sessionID}
}

func (reporter *Reporter) Ready(socketPath string) error {
	return reporter.send(ChildMessage{Type: MessageReady, SessionID: reporter.sessionID, Ready: &ReadyMessage{SocketPath: socketPath}})
}

func (reporter *Reporter) Read(result contract.ReadChildResult) error {
	return reporter.send(ChildMessage{Type: MessageRead, SessionID: reporter.sessionID, Read: &result})
}

func (reporter *Reporter) Write(result contract.WriteChildResult) error {
	return reporter.send(ChildMessage{Type: MessageWrite, SessionID: reporter.sessionID, Write: &result})
}

func (reporter *Reporter) Failure(code contract.ErrorCode, message string) error {
	return reporter.send(ChildMessage{Type: MessageFailure, SessionID: reporter.sessionID, Error: &WireError{Code: code, Message: message}})
}

func (reporter *Reporter) send(message ChildMessage) error {
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
