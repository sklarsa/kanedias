package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

// IdempotentStop makes explicit shutdown and parent-liveness shutdown share one
// cleanup call. Every caller observes the result of the first call.
func IdempotentStop(stop func(context.Context) error) func(context.Context) error {
	var once sync.Once
	var result error
	return func(ctx context.Context) error {
		once.Do(func() { result = stop(ctx) })
		return result
	}
}

// Fixed child descriptors form the inherited process protocol.
const (
	BootstrapFD   = 3
	LivenessFD    = 4
	ReportFD      = 5
	TerminalAckFD = 6
)

type ChildRunner func(context.Context, Bootstrap, *Reporter) error

// RunInheritedChild owns the hidden command's fixed inherited descriptors. It
// marks every inherited descriptor close-on-exec before invoking runtime code,
// so grandchildren cannot retain any ancestor protocol endpoint.
func RunInheritedChild(ctx context.Context, bootstrapFD, livenessFD, reportFD, terminalAckFD int, runner ChildRunner) error {
	if bootstrapFD != BootstrapFD || livenessFD != LivenessFD || reportFD != ReportFD || terminalAckFD != TerminalAckFD {
		return fmt.Errorf("session-child requires fixed descriptors 3, 4, 5, and 6")
	}
	if runner == nil {
		return fmt.Errorf("child session runner is required")
	}
	for _, descriptor := range []int{bootstrapFD, livenessFD, reportFD, terminalAckFD} {
		syscall.CloseOnExec(descriptor)
	}
	bootstrapFile := os.NewFile(uintptr(bootstrapFD), "child-bootstrap")
	livenessFile := os.NewFile(uintptr(livenessFD), "parent-liveness")
	reportFile := os.NewFile(uintptr(reportFD), "child-report")
	terminalAckFile := os.NewFile(uintptr(terminalAckFD), "parent-terminal-ack")
	if bootstrapFile == nil || livenessFile == nil || reportFile == nil || terminalAckFile == nil {
		return fmt.Errorf("open inherited child descriptors")
	}
	defer livenessFile.Close()
	defer reportFile.Close()
	defer terminalAckFile.Close()

	bootstrap, err := DecodeBootstrap(bootstrapFile)
	closeErr := bootstrapFile.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	childCtx, cancel := context.WithCancel(ctx)
	cancelAckClose := context.AfterFunc(childCtx, func() { _ = terminalAckFile.Close() })
	defer cancelAckClose()
	reporter := NewAcknowledgedReporter(childCtx, reportFile, terminalAckFile, bootstrap.SessionID)
	defer cancel()
	stop := IdempotentStop(func(context.Context) error { cancel(); return nil })

	monitorDone := make(chan error, 1)
	go func() { monitorDone <- MonitorParentLiveness(childCtx, livenessFile, stop) }()
	runErr := runner(childCtx, bootstrap, reporter)
	var reportErr error
	if runErr != nil && !reporter.TerminalSent() && !(errors.Is(runErr, context.Canceled) && childCtx.Err() != nil) {
		code := contract.ErrorChildFailed
		var contractErr *contract.Error
		if errors.As(runErr, &contractErr) {
			code = contractErr.Code
		}
		// The child context remains live while the terminal failure is reported;
		// the direct parent's acknowledgement is the teardown boundary.
		reportErr = reporter.Failure(code, runErr.Error())
	}
	_ = stop(context.WithoutCancel(ctx))
	monitorErr := <-monitorDone
	if errors.Is(monitorErr, context.Canceled) {
		monitorErr = nil
	}
	if childCtx.Err() != nil && (errors.Is(runErr, ErrTerminalAckClosed) || errors.Is(runErr, context.Canceled)) {
		runErr = nil
	}
	return errors.Join(runErr, monitorErr, reportErr)
}

// MonitorParentLiveness waits for EOF on the inherited liveness descriptor.
// The descriptor is signal-only: receiving data is a protocol violation.
func MonitorParentLiveness(ctx context.Context, parent io.ReadCloser, stop func(context.Context) error) error {
	if parent == nil {
		return fmt.Errorf("parent liveness reader is required")
	}
	if stop == nil {
		return fmt.Errorf("parent liveness stop function is required")
	}
	defer parent.Close()

	readResult := make(chan error, 1)
	go func() {
		var data [1]byte
		n, err := parent.Read(data[:])
		switch {
		case n != 0:
			readResult <- fmt.Errorf("parent liveness descriptor carried unexpected data")
		case errors.Is(err, io.EOF):
			readResult <- nil
		case err != nil:
			readResult <- fmt.Errorf("read parent liveness descriptor: %w", err)
		default:
			readResult <- fmt.Errorf("parent liveness read made no progress")
		}
	}()

	select {
	case err := <-readResult:
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		return stop(context.WithoutCancel(ctx))
	case <-ctx.Done():
		_ = parent.Close()
		<-readResult
		return ctx.Err()
	}
}
