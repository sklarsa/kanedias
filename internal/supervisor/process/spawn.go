package process

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

const defaultProbeInterval = 25 * time.Millisecond

type Spawner struct {
	Executable    string
	ProbeInterval time.Duration
}

type reportEvent struct {
	message ChildMessage
	err     error
}

type Child struct {
	command       *exec.Cmd
	bootstrap     Bootstrap
	liveness      *os.File
	reports       chan reportEvent
	probeInterval time.Duration
	waitDone      chan struct{}
	waitErr       error
	closeOnce     sync.Once
	killOnce      sync.Once
}

func (spawner Spawner) Spawn(ctx context.Context, bootstrap Bootstrap) (*Child, error) {
	if err := bootstrap.Validate(); err != nil {
		return nil, err
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
		bootstrapRead.Close()
		bootstrapWrite.Close()
		return nil, fmt.Errorf("create liveness pipe: %w", err)
	}
	reportRead, reportWrite, err := os.Pipe()
	if err != nil {
		bootstrapRead.Close()
		bootstrapWrite.Close()
		livenessRead.Close()
		livenessWrite.Close()
		return nil, fmt.Errorf("create report pipe: %w", err)
	}
	closeAll := func() {
		bootstrapRead.Close()
		bootstrapWrite.Close()
		livenessRead.Close()
		livenessWrite.Close()
		reportRead.Close()
		reportWrite.Close()
	}

	command := exec.CommandContext(ctx, executable,
		"session-child", "--bootstrap-fd", "3", "--liveness-fd", "4", "--report-fd", "5")
	// ExtraFiles is the complete descriptor allowlist beyond stdin/out/err.
	command.ExtraFiles = []*os.File{bootstrapRead, livenessRead, reportWrite}
	if err := command.Start(); err != nil {
		closeAll()
		return nil, fmt.Errorf("start child supervisor: %w", err)
	}
	_ = bootstrapRead.Close()
	_ = livenessRead.Close()
	_ = reportWrite.Close()

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
			return nil, fmt.Errorf("send child bootstrap: %w", err)
		}
	case <-ctx.Done():
		_ = bootstrapWrite.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = livenessWrite.Close()
		_ = reportRead.Close()
		return nil, ctx.Err()
	}

	interval := spawner.ProbeInterval
	if interval <= 0 {
		interval = defaultProbeInterval
	}
	child := &Child{
		command: command, bootstrap: bootstrap, liveness: livenessWrite,
		reports: make(chan reportEvent, 1), probeInterval: interval, waitDone: make(chan struct{}),
	}
	go child.readReports(reportRead)
	go func() { child.waitErr = command.Wait(); close(child.waitDone) }()
	return child, nil
}

func (child *Child) readReports(reader *os.File) {
	defer reader.Close()
	defer close(child.reports)
	buffered := bufio.NewReaderSize(reader, MaxRecordBytes+1)
	for {
		record, err := buffered.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			child.reports <- reportEvent{err: ErrRecordTooLarge}
			return
		}
		if errors.Is(err, io.EOF) {
			if len(record) != 0 {
				child.reports <- reportEvent{err: fmt.Errorf("child report ended with a partial JSONL record")}
			} else {
				child.reports <- reportEvent{err: io.EOF}
			}
			return
		}
		if err != nil {
			child.reports <- reportEvent{err: fmt.Errorf("read child report: %w", err)}
			return
		}
		if len(record) > MaxRecordBytes {
			child.reports <- reportEvent{err: ErrRecordTooLarge}
			return
		}
		message, decodeErr := DecodeChildMessage(bytes.NewReader(record[:len(record)-1]))
		if decodeErr != nil {
			child.reports <- reportEvent{err: decodeErr}
			return
		}
		child.reports <- reportEvent{message: message}
	}
}

func (child *Child) WaitReady(ctx context.Context) error {
	for {
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
		return probeTree(ctx, message.Ready.SocketPath, child.probeInterval)
	}
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
		return event.message, nil
	case <-ctx.Done():
		return ChildMessage{}, ctx.Err()
	}
}

func (child *Child) CloseLiveness() error {
	var err error
	child.closeOnce.Do(func() { err = child.liveness.Close() })
	return err
}

func (child *Child) Wait() error {
	<-child.waitDone
	return child.waitErr
}

func (child *Child) Kill() error {
	_ = child.CloseLiveness()
	var err error
	child.killOnce.Do(func() {
		select {
		case <-child.waitDone:
		default:
			err = child.command.Process.Kill()
		}
	})
	<-child.waitDone
	return err
}

func probeTree(ctx context.Context, socketPath string, interval time.Duration) error {
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
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, MaxRecordBytes+1))
			closeErr := response.Body.Close()
			if response.StatusCode == http.StatusOK && closeErr == nil {
				return nil
			}
			lastErr = fmt.Errorf("GET /v1/tree returned %s", response.Status)
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
