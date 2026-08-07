package incusworkspace

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/sklarsa/kanedias/internal/config"
)

type recordingVolumeClient struct {
	volume    *api.StorageVolume
	getErr    error
	copyErr   error
	deleteErr error
	calls     []string
	onCopy    func(context.Context)
}

func (client *recordingVolumeClient) GetStorageVolume(_ context.Context, pool, volume string) (*api.StorageVolume, error) {
	client.calls = append(client.calls, "get "+pool+" "+volume)
	return client.volume, client.getErr
}

func (client *recordingVolumeClient) CopyStorageVolumeUntilTerminal(ctx context.Context, pool, source, target string) error {
	client.calls = append(client.calls, "copy-terminal "+pool+" "+source+" "+target)
	if client.onCopy != nil {
		client.onCopy(ctx)
	}
	return client.copyErr
}

func (client *recordingVolumeClient) DeleteStorageVolume(_ context.Context, pool, volume string) error {
	client.calls = append(client.calls, "delete "+pool+" "+volume)
	return client.deleteErr
}

func TestStateNamesAndDevice(t *testing.T) {
	cfg := config.Config{Workspace: config.Workspace{Incus: config.IncusWorkspace{Volume: "seed"}}}
	if got := SeedVolume(cfg); got != "seed" {
		t.Fatalf("SeedVolume() = %q", got)
	}
	if got := SeedVolume(config.Config{}); got != config.DefaultIncusWorkspaceVolume {
		t.Fatalf("SeedVolume(empty config) = %q, want %q", got, config.DefaultIncusWorkspaceVolume)
	}
	if got := SandboxVolume("demo"); got != "kanedias-incus-demo" {
		t.Fatalf("SandboxVolume() = %q", got)
	}
	if DeviceName != "incus-state" {
		t.Fatalf("DeviceName = %q, want %q", DeviceName, "incus-state")
	}
	want := map[string]string{
		"type": "disk", "pool": "pool1", "source": "kanedias-incus-demo", "path": "/var/lib/incus",
	}
	if got := Device("pool1", "kanedias-incus-demo"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Device() = %#v, want %#v", got, want)
	}
}

func TestCloneReturnsWrappedSeedLookupErrorWithoutCopy(t *testing.T) {
	getErr := errors.New("seed missing")
	client := &recordingVolumeClient{getErr: getErr}

	_, err := Clone(context.Background(), client, "pool1", "seed", "demo")
	if !errors.Is(err, getErr) {
		t.Fatalf("Clone() error = %v, want wrapped %v", err, getErr)
	}
	if err == getErr {
		t.Fatal("Clone() returned seed lookup error without context")
	}
	if strings.Contains(strings.Join(client.calls, "\n"), "copy-terminal ") {
		t.Fatalf("calls = %v, want no copy", client.calls)
	}
}

func TestCloneRejectsAttachedSeedWithoutCopy(t *testing.T) {
	client := &recordingVolumeClient{volume: &api.StorageVolume{UsedBy: []string{"/1.0/instances/demo"}}}

	_, err := Clone(context.Background(), client, "pool1", "seed", "demo")
	want := `nested Incus seed "seed" is attached and cannot be cloned`
	if err == nil || err.Error() != want {
		t.Fatalf("Clone() error = %v, want %q", err, want)
	}
	if strings.Contains(strings.Join(client.calls, "\n"), "copy-terminal ") {
		t.Fatalf("calls = %v, want no copy", client.calls)
	}
}

func TestCloneSuccessfulCopyReturnsCreated(t *testing.T) {
	client := &recordingVolumeClient{volume: &api.StorageVolume{}}

	result, err := Clone(context.Background(), client, "pool1", "seed", "demo")
	if err != nil {
		t.Fatal(err)
	}
	want := CloneResult{Name: "kanedias-incus-demo", Created: true}
	if result != want {
		t.Fatalf("Clone() = %#v, want %#v", result, want)
	}
}

func TestClonePreSubmissionErrorReturnsNotCreated(t *testing.T) {
	copyErr := errors.New("copy rejected")
	client := &recordingVolumeClient{volume: &api.StorageVolume{}, copyErr: copyErr}

	result, err := Clone(context.Background(), client, "pool1", "seed", "demo")
	if err != copyErr {
		t.Fatalf("Clone() error = %v, want unchanged %v", err, copyErr)
	}
	want := CloneResult{Name: "kanedias-incus-demo", Created: false}
	if result != want {
		t.Fatalf("Clone() = %#v, want %#v", result, want)
	}
}

type submittedCopyError struct{}

func (submittedCopyError) Error() string { return "copy wait failed" }
func (submittedCopyError) As(any) bool   { return true }

func TestCloneSubmittedErrorReturnsCreated(t *testing.T) {
	copyErr := submittedCopyError{}
	client := &recordingVolumeClient{volume: &api.StorageVolume{}, copyErr: copyErr}

	result, err := Clone(context.Background(), client, "pool1", "seed", "demo")
	if err != copyErr {
		t.Fatalf("Clone() error = %v, want unchanged %v", err, copyErr)
	}
	want := CloneResult{Name: "kanedias-incus-demo", Created: true}
	if result != want {
		t.Fatalf("Clone() = %#v, want %#v", result, want)
	}
}

func TestCloneGetsThenCopiesWhileHoldingSharedLock(t *testing.T) {
	pool := "pool-" + strings.ReplaceAll(t.Name(), "/", "-")
	seed := "seed"
	lockHeld := false
	client := &recordingVolumeClient{
		volume: &api.StorageVolume{},
		onCopy: func(context.Context) {
			writer, err := acquireSeedLock(pool, seed, true)
			if err != nil {
				lockHeld = true
				return
			}
			_ = writer.Close()
		},
	}

	if _, err := Clone(context.Background(), client, pool, seed, "demo"); err != nil {
		t.Fatal(err)
	}
	want := []string{"get " + pool + " seed", "copy-terminal " + pool + " seed kanedias-incus-demo"}
	if !reflect.DeepEqual(client.calls, want) {
		t.Fatalf("calls = %v, want %v", client.calls, want)
	}
	if !lockHeld {
		t.Fatal("copy ran without shared seed lock")
	}
	writer, err := acquireSeedLock(pool, seed, true)
	if err != nil {
		t.Fatalf("shared seed lock was not released: %v", err)
	}
	_ = writer.Close()
}

func TestCloneCancellationKeepsSharedLockUntilCopyIsTerminal(t *testing.T) {
	pool := "pool-" + strings.ReplaceAll(t.Name(), "/", "-")
	seed := "seed"
	copyStarted := make(chan struct{})
	copyTerminal := make(chan struct{})
	client := &recordingVolumeClient{
		volume: &api.StorageVolume{},
		onCopy: func(ctx context.Context) {
			close(copyStarted)
			<-ctx.Done()
			<-copyTerminal
		},
		copyErr: context.Canceled,
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := Clone(ctx, client, pool, seed, "demo")
		result <- err
	}()
	<-copyStarted
	cancel()

	if writer, err := acquireSeedLock(pool, seed, true); err == nil {
		_ = writer.Close()
		t.Fatal("exclusive seed lock succeeded before the copy became terminal")
	}
	select {
	case err := <-result:
		t.Fatalf("Clone returned before copy became terminal: %v", err)
	default:
	}

	close(copyTerminal)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Clone error = %v, want context.Canceled", err)
	}
	writer, err := acquireSeedLock(pool, seed, true)
	if err != nil {
		t.Fatalf("exclusive seed lock remained held after Clone returned: %v", err)
	}
	_ = writer.Close()
}

func TestStateDeleteRefusesSeedAndDeletesSandbox(t *testing.T) {
	client := &recordingVolumeClient{}
	if err := Delete(context.Background(), client, "pool1", "seed", "seed"); err == nil {
		t.Fatal("Delete() succeeded for seed volume")
	}
	if len(client.calls) != 0 {
		t.Fatalf("calls = %v, want no seed deletion", client.calls)
	}

	deleteErr := errors.New("delete failed")
	client.deleteErr = deleteErr
	if err := Delete(context.Background(), client, "pool1", "seed", "kanedias-incus-demo"); err != deleteErr {
		t.Fatalf("Delete() error = %v, want unchanged %v", err, deleteErr)
	}
	want := []string{"delete pool1 kanedias-incus-demo"}
	if !reflect.DeepEqual(client.calls, want) {
		t.Fatalf("calls = %v, want %v", client.calls, want)
	}
}
