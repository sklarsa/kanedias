package process

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/provision"
)

const defaultProbeInterval = 25 * time.Millisecond

type Spawner struct {
	Executable    string
	ProbeInterval time.Duration
	ConfigPath    string
}

type reportEvent struct {
	message ChildMessage
	err     error
}

type Child struct {
	command           *exec.Cmd
	bootstrap         Bootstrap
	liveness          *os.File
	reports           chan reportEvent
	probeInterval     time.Duration
	waitDone          chan struct{}
	waitErr           error
	closeOnce         sync.Once
	killOnce          sync.Once
	reportReader      *os.File
	terminalAck       *os.File
	terminalAckOnce   sync.Once
	terminalMu        sync.Mutex
	pendingTerminal   *ChildMessage
	terminalAcked     bool
	terminalAckClosed bool
	reportStop        chan struct{}
	reportDone        chan struct{}
	reportCloseOnce   sync.Once
	recoveryMu        sync.RWMutex
	recoveryTicket    provision.RecoveryTicket
	hasRecovery       bool
}

func (spawner Spawner) Spawn(ctx context.Context, bootstrap Bootstrap) (*Child, error) {
	bootstrap.Policy = bootstrap.Policy.Clone()
	if err := bootstrap.Validate(); err != nil {
		return nil, err
	}
	if spawner.ConfigPath != "" && (!filepath.IsAbs(spawner.ConfigPath) || filepath.Clean(spawner.ConfigPath) != spawner.ConfigPath) {
		return nil, fmt.Errorf("child config path must be absolute and clean")
	}
	var encoded bytes.Buffer
	if err := EncodeBootstrap(&encoded, bootstrap); err != nil {
		return nil, err
	}
	executable := spawner.Executable
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolve child executable: %w", err)
		}
	}

	bootstrapRead, bootstrapWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create bootstrap pipe: %w", err)
	}
	livenessRead, livenessWrite, err := os.Pipe()
	if err != nil {
		_ = bootstrapRead.Close()
		_ = bootstrapWrite.Close()
		return nil, fmt.Errorf("create liveness pipe: %w", err)
	}
	reportRead, reportWrite, err := os.Pipe()
	if err != nil {
		_ = bootstrapRead.Close()
		_ = bootstrapWrite.Close()
		_ = livenessRead.Close()
		_ = livenessWrite.Close()
		return nil, fmt.Errorf("create report pipe: %w", err)
	}
	terminalAckRead, terminalAckWrite, err := os.Pipe()
	if err != nil {
		_ = bootstrapRead.Close()
		_ = bootstrapWrite.Close()
		_ = livenessRead.Close()
		_ = livenessWrite.Close()
		_ = reportRead.Close()
		_ = reportWrite.Close()
		return nil, fmt.Errorf("create terminal acknowledgement pipe: %w", err)
	}
	closeAll := func() {
		_ = bootstrapRead.Close()
		_ = bootstrapWrite.Close()
		_ = livenessRead.Close()
		_ = livenessWrite.Close()
		_ = reportRead.Close()
		_ = reportWrite.Close()
		_ = terminalAckRead.Close()
		_ = terminalAckWrite.Close()
	}

	command := exec.CommandContext(ctx, executable,
		"session-child", "--bootstrap-fd", "3", "--liveness-fd", "4", "--report-fd", "5", "--terminal-ack-fd", "6")
	// Recursive diagnostics follow the root process's already-private persistent
	// log sinks instead of disappearing into /dev/null.
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if spawner.ConfigPath != "" {
		command.Env = withConfigPath(os.Environ(), spawner.ConfigPath)
	}
	// A child supervisor owns its complete descendant process tree. Isolating it
	// in a process group lets the direct parent escalate without leaving a
	// grandchild behind.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// ExtraFiles is the complete descriptor allowlist beyond stdin/out/err.
	command.ExtraFiles = []*os.File{bootstrapRead, livenessRead, reportWrite, terminalAckRead}
	if err := command.Start(); err != nil {
		closeAll()
		return nil, fmt.Errorf("start child supervisor: %w", err)
	}
	_ = bootstrapRead.Close()
	_ = livenessRead.Close()
	_ = reportWrite.Close()
	_ = terminalAckRead.Close()

	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := bootstrapWrite.Write(encoded.Bytes())
		closeErr := bootstrapWrite.Close()
		writeDone <- errors.Join(writeErr, closeErr)
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			_ = livenessWrite.Close()
			_ = reportRead.Close()
			_ = terminalAckWrite.Close()
			return nil, fmt.Errorf("send child bootstrap: %w", err)
		}
	case <-ctx.Done():
		_ = bootstrapWrite.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = livenessWrite.Close()
		_ = reportRead.Close()
		_ = terminalAckWrite.Close()
		return nil, ctx.Err()
	}

	interval := spawner.ProbeInterval
	if interval <= 0 {
		interval = defaultProbeInterval
	}
	child := &Child{
		command: command, bootstrap: bootstrap, liveness: livenessWrite,
		reports: make(chan reportEvent, 1), probeInterval: interval, waitDone: make(chan struct{}),
		reportReader: reportRead, terminalAck: terminalAckWrite,
		reportStop: make(chan struct{}), reportDone: make(chan struct{}),
	}
	go child.readReports(reportRead)
	go func() { child.waitErr = command.Wait(); close(child.waitDone) }()
	return child, nil
}

func withConfigPath(environment []string, path string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, variable := range environment {
		if !strings.HasPrefix(variable, "KANEDIAS_CONFIG=") {
			result = append(result, variable)
		}
	}
	return append(result, "KANEDIAS_CONFIG="+path)
}

func (child *Child) readReports(reader *os.File) {
	defer func() { _ = reader.Close() }()
	defer close(child.reports)
	defer close(child.reportDone)
	buffered := bufio.NewReaderSize(reader, MaxRecordBytes+1)
	for {
		record, err := buffered.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			child.sendReport(reportEvent{err: ErrRecordTooLarge})
			return
		}
		if errors.Is(err, io.EOF) {
			if len(record) != 0 {
				child.sendReport(reportEvent{err: fmt.Errorf("child report ended with a partial JSONL record")})
			} else {
				child.sendReport(reportEvent{err: io.EOF})
			}
			return
		}
		if err != nil {
			child.sendReport(reportEvent{err: fmt.Errorf("read child report: %w", err)})
			return
		}
		if len(record) > MaxRecordBytes {
			child.sendReport(reportEvent{err: ErrRecordTooLarge})
			return
		}
		message, decodeErr := DecodeChildMessage(bytes.NewReader(record[:len(record)-1]))
		if decodeErr != nil {
			child.sendReport(reportEvent{err: decodeErr})
			return
		}
		if !child.sendReport(reportEvent{message: message}) {
			return
		}
	}
}

func (child *Child) sendReport(event reportEvent) bool {
	select {
	case child.reports <- event:
		return true
	case <-child.reportStop:
		return false
	}
}

func (child *Child) WaitReady(ctx context.Context) error {
	ownership, err := child.NextMessage(ctx)
	if err != nil {
		return err
	}
	if ownership.SessionID != child.bootstrap.SessionID {
		return fmt.Errorf("child report session ID %q does not match bootstrap %q", ownership.SessionID, child.bootstrap.SessionID)
	}
	if ownership.Type == MessageFailure && ownership.Error != nil {
		return contractError(ownership.Error)
	}
	if ownership.Type != MessageOwnership || ownership.Ownership == nil {
		return fmt.Errorf("child reported %q before ownership", ownership.Type)
	}
	if err := validateRecoveryTicket(child.bootstrap, *ownership.Ownership); err != nil {
		return err
	}
	child.recoveryMu.Lock()
	child.recoveryTicket = *ownership.Ownership
	child.hasRecovery = true
	child.recoveryMu.Unlock()

	message, err := child.NextMessage(ctx)
	if err != nil {
		return err
	}
	if message.SessionID != child.bootstrap.SessionID {
		return fmt.Errorf("child report session ID %q does not match bootstrap %q", message.SessionID, child.bootstrap.SessionID)
	}
	if message.Type != MessageReady {
		if message.Type == MessageFailure && message.Error != nil {
			return contractError(message.Error)
		}
		return fmt.Errorf("child reported %q before readiness", message.Type)
	}
	if message.Ready.SocketPath != child.bootstrap.SocketPath {
		return fmt.Errorf("child ready socket %q does not match bootstrap %q", message.Ready.SocketPath, child.bootstrap.SocketPath)
	}
	return probeTree(ctx, message.Ready.SocketPath, child.bootstrap, child.probeInterval)
}

func validateRecoveryTicket(bootstrap Bootstrap, ticket provision.RecoveryTicket) error {
	if ticket.SessionID != bootstrap.SessionID || ticket.ParentID != bootstrap.ParentID || ticket.RootID != bootstrap.RootID ||
		ticket.Instance != "session-"+bootstrap.SessionID || ticket.Volume != "workspace-"+bootstrap.SessionID ||
		ticket.SocketPath != bootstrap.SocketPath || ticket.Kind != bootstrap.Request.Kind || ticket.Context != bootstrap.Request.Context ||
		ticket.WorkerType != bootstrap.Request.WorkerType || ticket.RunAttribution != bootstrap.RunAttribution || ticket.Pool == "" || ticket.Socket.Device == 0 || ticket.Socket.Inode == 0 {
		return fmt.Errorf("child ownership ticket does not exactly match admitted bootstrap %q", bootstrap.SessionID)
	}
	return nil
}

func (child *Child) RecoveryTicket() (provision.RecoveryTicket, bool) {
	child.recoveryMu.RLock()
	defer child.recoveryMu.RUnlock()
	return child.recoveryTicket, child.hasRecovery
}

func (child *Child) NextMessage(ctx context.Context) (ChildMessage, error) {
	select {
	case event, ok := <-child.reports:
		if !ok {
			return ChildMessage{}, io.EOF
		}
		if event.err != nil {
			return ChildMessage{}, event.err
		}
		if isTerminalMessage(event.message) {
			child.terminalMu.Lock()
			if child.pendingTerminal != nil || child.terminalAcked {
				child.terminalMu.Unlock()
				return ChildMessage{}, fmt.Errorf("child published more than one terminal report")
			}
			message := event.message
			child.pendingTerminal = &message
			child.terminalMu.Unlock()
		}
		return event.message, nil
	case <-ctx.Done():
		return ChildMessage{}, ctx.Err()
	}
}

func isTerminalMessage(message ChildMessage) bool {
	return message.Type == MessageRead || message.Type == MessageWrite || message.Type == MessageFailure
}

func (child *Child) AcknowledgeTerminal(message ChildMessage) error {
	if !isTerminalMessage(message) {
		return fmt.Errorf("cannot acknowledge non-terminal child report %q", message.Type)
	}
	child.terminalMu.Lock()
	defer child.terminalMu.Unlock()
	if child.pendingTerminal == nil {
		return fmt.Errorf("no terminal child report is pending acknowledgement")
	}
	pending, pendingErr := json.Marshal(child.pendingTerminal)
	acknowledged, acknowledgedErr := json.Marshal(message)
	if pendingErr != nil || acknowledgedErr != nil || !bytes.Equal(pending, acknowledged) {
		return fmt.Errorf("terminal acknowledgement does not match the exact ingested report")
	}
	if child.terminalAcked {
		return fmt.Errorf("terminal child report was already acknowledged")
	}
	if child.terminalAckClosed {
		return fmt.Errorf("terminal acknowledgement endpoint is closed")
	}
	child.terminalAcked = true
	var result error
	child.terminalAckOnce.Do(func() {
		_, writeErr := child.terminalAck.Write([]byte{TerminalAckByte})
		closeErr := child.terminalAck.Close()
		result = errors.Join(writeErr, closeErr)
	})
	if result != nil {
		return fmt.Errorf("acknowledge terminal child report: %w", result)
	}
	return nil
}

func (child *Child) CloseTerminalAck() error {
	child.terminalMu.Lock()
	defer child.terminalMu.Unlock()
	if child.terminalAckClosed || child.terminalAcked {
		return nil
	}
	child.terminalAckClosed = true
	var err error
	child.terminalAckOnce.Do(func() { err = child.terminalAck.Close() })
	if errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}

func (child *Child) CloseLiveness() error {
	var err error
	child.closeOnce.Do(func() { err = child.liveness.Close() })
	return err
}

func (child *Child) CloseReports() error {
	var err error
	child.reportCloseOnce.Do(func() {
		close(child.reportStop)
		err = child.reportReader.Close()
	})
	<-child.reportDone
	if errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}

func (child *Child) Done() <-chan struct{} { return child.waitDone }

func (child *Child) Wait() error {
	<-child.waitDone
	return child.waitErr
}

func (child *Child) Terminate() error {
	_ = child.CloseTerminalAck()
	_ = child.CloseLiveness()
	select {
	case <-child.waitDone:
		return nil
	default:
	}
	if child.command.Process == nil {
		return nil
	}
	if err := syscall.Kill(-child.command.Process.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (child *Child) Kill() error {
	_ = child.CloseTerminalAck()
	_ = child.CloseLiveness()
	var err error
	child.killOnce.Do(func() {
		select {
		case <-child.waitDone:
		default:
			if child.command.Process != nil {
				err = syscall.Kill(-child.command.Process.Pid, syscall.SIGKILL)
				if errors.Is(err, syscall.ESRCH) {
					err = nil
				}
			}
		}
	})
	<-child.waitDone
	return errors.Join(err, child.CloseReports())
}

func probeTree(ctx context.Context, socketPath string, expected Bootstrap, interval time.Duration) error {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	var lastErr error
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/v1/tree", nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, MaxRecordBytes+1))
			closeErr := response.Body.Close()
			switch {
			case response.StatusCode != http.StatusOK:
				lastErr = fmt.Errorf("GET /v1/tree returned %s", response.Status)
			case readErr != nil || closeErr != nil:
				lastErr = fmt.Errorf("read GET /v1/tree response: %w", errors.Join(readErr, closeErr))
			case len(body) > MaxRecordBytes:
				lastErr = ErrRecordTooLarge
			default:
				var identity struct {
					SessionID       string `json:"sessionId"`
					ParentSessionID string `json:"parentSessionId"`
					RootSessionID   string `json:"rootSessionId"`
				}
				if decodeErr := json.Unmarshal(body, &identity); decodeErr != nil {
					lastErr = fmt.Errorf("decode GET /v1/tree response: %w", decodeErr)
				} else if identity.SessionID != expected.SessionID || identity.ParentSessionID != expected.ParentID || identity.RootSessionID != expected.RootID {
					lastErr = fmt.Errorf("GET /v1/tree identity (%q, %q, %q) does not match child bootstrap (%q, %q, %q)", identity.SessionID, identity.ParentSessionID, identity.RootSessionID, expected.SessionID, expected.ParentID, expected.RootID)
				} else {
					return nil
				}
			}
		} else {
			lastErr = err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("probe child supervisor socket %q: %w", socketPath, errors.Join(ctx.Err(), lastErr))
		case <-timer.C:
		}
	}
}

func contractError(wire *WireError) error {
	return contract.NewError(wire.Code, wire.Message)
}
