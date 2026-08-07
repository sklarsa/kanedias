package incusclient

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lxc/incus/v7/shared/api"
)

type fakeStoragePoolGetter struct {
	name string
	pool *api.StoragePool
	err  error
}

func (f *fakeStoragePoolGetter) GetStoragePool(name string) (*api.StoragePool, string, error) {
	f.name = name
	return f.pool, "etag", f.err
}

func TestGetStoragePool(t *testing.T) {
	fake := &fakeStoragePoolGetter{pool: &api.StoragePool{Name: "pool1", Driver: "btrfs"}}
	pool, err := getStoragePool(fake, "pool1")
	if err != nil {
		t.Fatal(err)
	}
	if fake.name != "pool1" || pool.Driver != "btrfs" {
		t.Fatalf("GetStoragePool() = %#v after name %q", pool, fake.name)
	}
}

func TestGetStoragePoolWrapsError(t *testing.T) {
	sentinel := errors.New("storage unavailable")
	_, err := getStoragePool(&fakeStoragePoolGetter{err: sentinel}, "pool1")
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), `storage pool "pool1"`) {
		t.Fatalf("getStoragePool() error = %v", err)
	}
}

type fakeRemoteOperation struct {
	waitStarted chan struct{}
	waitRelease chan struct{}
	cancelled   chan struct{}
	waitErr     error
}

func (o *fakeRemoteOperation) Wait() error {
	close(o.waitStarted)
	<-o.waitRelease
	return o.waitErr
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
