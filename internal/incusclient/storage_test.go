package incusclient

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	incus "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

type storageAdapterServer struct {
	incus.InstanceServer
	contextSeen   context.Context
	pool          *api.StoragePool
	volume        *api.StorageVolume
	volumeETag    string
	copyOperation incus.RemoteOperation
	updatedPool   string
	updatedType   string
	updatedName   string
	updatedPut    api.StorageVolumePut
	updatedETag   string
}

func (s *storageAdapterServer) WithContext(ctx context.Context) incus.InstanceServer {
	s.contextSeen = ctx
	return s
}

func (s *storageAdapterServer) GetStoragePool(string) (*api.StoragePool, string, error) {
	return s.pool, "pool-etag", nil
}

func (s *storageAdapterServer) GetStoragePoolVolume(string, string, string) (*api.StorageVolume, string, error) {
	return s.volume, s.volumeETag, nil
}

func (s *storageAdapterServer) UpdateStoragePoolVolume(pool, volumeType, name string, put api.StorageVolumePut, etag string) error {
	s.updatedPool = pool
	s.updatedType = volumeType
	s.updatedName = name
	s.updatedPut = put
	s.updatedETag = etag
	return nil
}

func (s *storageAdapterServer) CopyStoragePoolVolume(string, incus.InstanceServer, string, api.StorageVolume, *incus.StoragePoolVolumeCopyArgs) (incus.RemoteOperation, error) {
	return s.copyOperation, nil
}

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

func TestGetStoragePoolUsesRequestContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), clientTestCtxKey{}, "pool-context")
	server := &storageAdapterServer{pool: &api.StoragePool{Name: "default", Driver: "btrfs"}}
	client := &Client{server: server}

	got, err := client.GetStoragePool(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if got != server.pool || server.contextSeen != ctx {
		t.Fatalf("GetStoragePool() = %#v with context %v, want configured pool and supplied context", got, server.contextSeen)
	}
}

func TestGetStorageVolumeWithETagPreservesETag(t *testing.T) {
	server := &storageAdapterServer{volume: &api.StorageVolume{Name: "workspace"}, volumeETag: "volume-etag"}
	client := &Client{server: server}

	volume, etag, err := client.GetStorageVolumeWithETag(context.Background(), "default", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if volume != server.volume || etag != "volume-etag" {
		t.Fatalf("GetStorageVolumeWithETag() = %#v, %q, want configured volume and ETag", volume, etag)
	}
}

func TestUpdateStorageVolumePreservesETag(t *testing.T) {
	server := &storageAdapterServer{}
	client := &Client{server: server}
	put := api.StorageVolumePut{Description: "child workspace", Config: map[string]string{"size": "5GiB"}}

	if err := client.UpdateStorageVolume(context.Background(), "default", "workspace", put, "volume-etag"); err != nil {
		t.Fatal(err)
	}
	if server.updatedPool != "default" || server.updatedType != customVolumeType || server.updatedName != "workspace" || server.updatedETag != "volume-etag" {
		t.Fatalf("UpdateStoragePoolVolume arguments = %q, %q, %q, %q", server.updatedPool, server.updatedType, server.updatedName, server.updatedETag)
	}
	if !reflect.DeepEqual(server.updatedPut, put) {
		t.Fatalf("updated volume = %#v, want %#v", server.updatedPut, put)
	}
}

type fakeRemoteOperation struct {
	waitStarted chan struct{}
	waitRelease chan struct{}
	cancelled   chan struct{}
	waitErr     error
	cancelErr   error
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
	return o.cancelErr
}

func TestRemoteVolumeWaitCancelsTargetButWaitsForTerminalResult(t *testing.T) {
	waitErr := errors.New("remote operation cancelled")
	op := &fakeRemoteOperation{
		waitStarted: make(chan struct{}),
		waitRelease: make(chan struct{}),
		cancelled:   make(chan struct{}),
		waitErr:     waitErr,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- waitRemoteOperation(ctx, op) }()
	<-op.waitStarted
	cancel()

	select {
	case <-op.cancelled:
	case <-time.After(time.Second):
		t.Fatal("CancelTarget was not called")
	}
	select {
	case err := <-done:
		t.Fatalf("waitRemoteOperation returned before the remote operation was terminal: %v", err)
	default:
	}

	close(op.waitRelease)
	if err := <-done; !errors.Is(err, context.Canceled) || !errors.Is(err, waitErr) {
		t.Fatalf("waitRemoteOperation() error = %v, want context.Canceled joined with remote wait error", err)
	}
}

type storageRemoteOperation struct {
	incus.RemoteOperation
	fake *fakeRemoteOperation
}

func (o *storageRemoteOperation) Wait() error         { return o.fake.Wait() }
func (o *storageRemoteOperation) CancelTarget() error { return o.fake.CancelTarget() }

func TestCopyStorageVolumeUntilTerminalUsesAdapterAndHoldsCancellationUntilRemoteTerminal(t *testing.T) {
	waitErr := errors.New("remote operation cancelled")
	remote := &fakeRemoteOperation{
		waitStarted: make(chan struct{}),
		waitRelease: make(chan struct{}),
		cancelled:   make(chan struct{}),
		waitErr:     waitErr,
	}
	server := &storageAdapterServer{
		volume:        &api.StorageVolume{Name: "seed"},
		copyOperation: &storageRemoteOperation{fake: remote},
	}
	client := &Client{server: server}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- client.CopyStorageVolumeUntilTerminal(ctx, "pool1", "seed", "clone")
	}()
	<-remote.waitStarted
	cancel()

	select {
	case <-remote.cancelled:
	case <-time.After(time.Second):
		t.Fatal("CopyStorageVolumeUntilTerminal did not call CancelTarget")
	}
	select {
	case err := <-result:
		t.Fatalf("CopyStorageVolumeUntilTerminal returned before remote terminal state: %v", err)
	default:
	}

	close(remote.waitRelease)
	if err := <-result; !errors.Is(err, context.Canceled) || !errors.Is(err, waitErr) || !OperationWasSubmitted(err) {
		t.Fatalf("CopyStorageVolumeUntilTerminal() error = %v, want submitted cancellation joined with terminal error", err)
	}
}

func TestCopyStorageVolumeCancellationReturnsSubmittedOperationPromptlyAndRemainsAwaitable(t *testing.T) {
	remote := &fakeRemoteOperation{
		waitStarted: make(chan struct{}),
		waitRelease: make(chan struct{}),
		cancelled:   make(chan struct{}),
	}
	server := &storageAdapterServer{
		volume:        &api.StorageVolume{Name: "seed"},
		copyOperation: &storageRemoteOperation{fake: remote},
	}
	client := &Client{server: server}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- client.CopyStorageVolume(ctx, "pool1", "seed", "clone") }()
	<-remote.waitStarted
	cancel()

	var submittedErr error
	select {
	case submittedErr = <-result:
	case <-time.After(time.Second):
		t.Fatal("CopyStorageVolume did not return promptly after cancellation")
	}
	if !OperationWasSubmitted(submittedErr) || !errors.Is(submittedErr, context.Canceled) {
		t.Fatalf("CopyStorageVolume() error = %v, want submitted context cancellation", submittedErr)
	}
	select {
	case <-remote.cancelled:
	default:
		t.Fatal("CopyStorageVolume did not call CancelTarget")
	}

	awaited := make(chan error, 1)
	go func() { awaited <- AwaitSubmittedOperation(context.Background(), submittedErr) }()
	select {
	case err := <-awaited:
		t.Fatalf("AwaitSubmittedOperation returned before remote terminal state: %v", err)
	default:
	}
	close(remote.waitRelease)
	if err := <-awaited; err != nil {
		t.Fatalf("AwaitSubmittedOperation() error = %v, want nil terminal result", err)
	}
}
