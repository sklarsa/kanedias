package incusclient

import (
	"context"
	"errors"
	"testing"
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
