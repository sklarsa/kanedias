package incusclient

import (
	"context"
	"errors"
	"testing"

	"github.com/lxc/incus/v7/shared/api"
)

func TestSubmittedLocalOperationWaitFailuresAreMarked(t *testing.T) {
	for _, operation := range []string{"create instance", "start instance"} {
		t.Run(operation, func(t *testing.T) {
			waitErr := errors.New("wait failed")
			err := submitAndWaitOperation(context.Background(), func() (operationWaiter, error) {
				return &fakeOperation{waitErr: waitErr}, nil
			})
			if !errors.Is(err, waitErr) {
				t.Fatalf("error = %v, want wait failure", err)
			}
			if !OperationWasSubmitted(err) {
				t.Fatalf("OperationWasSubmitted(%v) = false, want true", err)
			}
		})
	}
}

func TestSubmittedLocalOperationCanBeAwaitedAfterRequestCancellation(t *testing.T) {
	operation := &rewaitLocalOperation{terminal: make(chan struct{})}
	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()
	submittedErr := submitAndWaitOperation(requestCtx, func() (operationWaiter, error) { return operation, nil })
	if !errors.Is(submittedErr, context.Canceled) || !OperationWasSubmitted(submittedErr) {
		t.Fatalf("submitted error = %v", submittedErr)
	}
	awaitDone := make(chan error, 1)
	go func() { awaitDone <- AwaitSubmittedOperation(context.Background(), submittedErr) }()
	select {
	case err := <-awaitDone:
		t.Fatalf("await returned before local operation terminal state: %v", err)
	default:
	}
	close(operation.terminal)
	if err := <-awaitDone; err != nil {
		t.Fatalf("AwaitSubmittedOperation() error = %v", err)
	}
}

type rewaitLocalOperation struct {
	terminal chan struct{}
	calls    int
}

func (operation *rewaitLocalOperation) WaitContext(ctx context.Context) error {
	operation.calls++
	if operation.calls == 1 {
		return ctx.Err()
	}
	select {
	case <-operation.terminal:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (*rewaitLocalOperation) Get() api.Operation { return api.Operation{} }

func TestSubmittedVolumeCopyWaitFailureIsMarked(t *testing.T) {
	waitErr := errors.New("copy wait failed")
	err := submitAndWaitRemoteOperation(context.Background(), func() (remoteOperationWaiter, error) {
		return &fakeRemoteOperation{
			waitStarted: make(chan struct{}),
			waitRelease: closedChannel(),
			cancelled:   make(chan struct{}),
			waitErr:     waitErr,
		}, nil
	})
	if !errors.Is(err, waitErr) {
		t.Fatalf("error = %v, want wait failure", err)
	}
	if !OperationWasSubmitted(err) {
		t.Fatalf("OperationWasSubmitted(%v) = false, want true", err)
	}
}

func TestSubmittedRemoteOperationCanBeAwaitedAfterCancellation(t *testing.T) {
	cancelErr := errors.New("target cancellation unsupported")
	operation := &fakeRemoteOperation{
		waitStarted: make(chan struct{}),
		waitRelease: make(chan struct{}),
		cancelled:   make(chan struct{}),
		cancelErr:   cancelErr,
	}
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	copyDone := make(chan error, 1)
	go func() {
		copyDone <- submitAndWaitRemoteOperation(requestCtx, func() (remoteOperationWaiter, error) {
			return operation, nil
		})
	}()
	<-operation.waitStarted
	cancelRequest()

	copyErr := <-copyDone
	if !errors.Is(copyErr, context.Canceled) || !OperationWasSubmitted(copyErr) {
		t.Fatalf("copy error = %v, want submitted context cancellation", copyErr)
	}
	if !errors.Is(copyErr, cancelErr) {
		t.Fatalf("copy error = %v, want surfaced CancelTarget failure", copyErr)
	}

	awaitDone := make(chan error, 1)
	go func() {
		awaitDone <- AwaitSubmittedRemoteOperation(context.Background(), copyErr)
	}()
	select {
	case err := <-awaitDone:
		t.Fatalf("await returned before operation became terminal: %v", err)
	default:
	}
	close(operation.waitRelease)
	if err := <-awaitDone; err != nil {
		t.Fatalf("AwaitSubmittedRemoteOperation() error = %v, want terminal success", err)
	}
}

func TestRemoteOperationWaitHookSynchronizesCancellationAfterSubmission(t *testing.T) {
	operation := &fakeRemoteOperation{
		waitStarted: make(chan struct{}),
		waitRelease: make(chan struct{}),
		cancelled:   make(chan struct{}),
	}
	hookStarted := make(chan struct{})
	releaseHook := make(chan struct{})
	ctx, cancel := context.WithCancel(WithRemoteOperationWaitHook(context.Background(), func() {
		close(hookStarted)
		<-releaseHook
	}))
	copyDone := make(chan error, 1)
	go func() {
		copyDone <- submitAndWaitRemoteOperation(ctx, func() (remoteOperationWaiter, error) {
			return operation, nil
		})
	}()
	<-operation.waitStarted
	<-hookStarted
	cancel()
	close(releaseHook)

	if err := <-copyDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("copy error = %v, want cancellation while remote wait was in flight", err)
	}
	select {
	case <-operation.cancelled:
	default:
		t.Fatal("in-flight cancellation did not call CancelTarget")
	}
	close(operation.waitRelease)
}

func TestImmediateSubmissionFailuresAreNotMarked(t *testing.T) {
	submitErr := errors.New("submission rejected")
	for _, test := range []struct {
		name string
		run  func() error
	}{
		{name: "create or start", run: func() error {
			return submitAndWaitOperation(context.Background(), func() (operationWaiter, error) {
				return nil, submitErr
			})
		}},
		{name: "volume copy", run: func() error {
			return submitAndWaitRemoteOperation(context.Background(), func() (remoteOperationWaiter, error) {
				return nil, submitErr
			})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			if !errors.Is(err, submitErr) {
				t.Fatalf("error = %v, want submission failure", err)
			}
			if OperationWasSubmitted(err) {
				t.Fatalf("OperationWasSubmitted(%v) = true, want false", err)
			}
		})
	}
}

func closedChannel() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
