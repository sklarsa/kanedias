package incusclient

import (
	"context"
	"errors"
	"testing"
)

type fakeRemoteOperation struct {
	waitStarted chan struct{}
	waitRelease chan struct{}
	cancelled   chan struct{}
}

func (o *fakeRemoteOperation) Wait() error {
	close(o.waitStarted)
	<-o.waitRelease
	return nil
}

func (o *fakeRemoteOperation) CancelTarget() error {
	select {
	case <-o.cancelled:
	default:
		close(o.cancelled)
	}
	return nil
}

func TestRemoteVolumeWaitCancelsTargetWithContext(t *testing.T) {
	op := &fakeRemoteOperation{
		waitStarted: make(chan struct{}),
		waitRelease: make(chan struct{}),
		cancelled:   make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- waitRemoteOperation(ctx, op) }()
	<-op.waitStarted
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("waitRemoteOperation() error = %v, want context.Canceled", err)
	}
	select {
	case <-op.cancelled:
	default:
		t.Fatal("CancelTarget was not called")
	}
	close(op.waitRelease)
}
