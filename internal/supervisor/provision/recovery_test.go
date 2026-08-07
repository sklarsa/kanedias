package provision

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/lxc/incus/v7/shared/api"
)

type recoveryClientFake struct {
	instance *api.Instance
	volume   *api.StorageVolume
	deleted  []string
}

func (fake *recoveryClientFake) GetInstance(context.Context, string) (*api.Instance, string, error) {
	return fake.instance, "", nil
}
func (fake *recoveryClientFake) GetStorageVolumeWithETag(context.Context, string, string) (*api.StorageVolume, string, error) {
	return fake.volume, "", nil
}
func (fake *recoveryClientFake) StopInstance(context.Context, string, bool) error {
	fake.deleted = append(fake.deleted, "stop")
	return nil
}
func (fake *recoveryClientFake) DeleteInstance(_ context.Context, name string) error {
	fake.deleted = append(fake.deleted, "instance:"+name)
	return nil
}
func (fake *recoveryClientFake) DeleteStorageVolume(_ context.Context, pool, name string) error {
	fake.deleted = append(fake.deleted, "volume:"+pool+"/"+name)
	return nil
}

func recoveryFixture(t *testing.T) (RecoveryTicket, *recoveryClientFake, net.Listener) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "child.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	ticket := RecoveryTicket{
		SessionID: "child-1", ParentID: "parent-1", RootID: "root-1", Pool: "pool",
		Instance: "session-child-1", Volume: "workspace-child-1", SocketPath: path,
		Socket: SocketIdentity{Device: uint64(stat.Dev), Inode: stat.Ino},
		Kind:   "read", Context: "fresh", WorkerType: "reviewer",
	}
	metadata := map[string]string{
		metaSessionID: ticket.SessionID, metaParentID: ticket.ParentID, metaRootID: ticket.RootID,
		metaKind: string(ticket.Kind), metaContext: string(ticket.Context), metaWorker: ticket.WorkerType,
		metaVolume: ticket.Volume,
	}
	fake := &recoveryClientFake{
		instance: &api.Instance{Name: ticket.Instance, Status: "Stopped", StatusCode: api.Stopped, InstancePut: api.InstancePut{Config: copyConfig(metadata)}, ExpandedDevices: api.DevicesMap{"root": {"type": "disk", "pool": ticket.Pool, "path": "/"}}},
		volume:   &api.StorageVolume{Name: ticket.Volume, StorageVolumePut: api.StorageVolumePut{Config: copyConfig(metadata)}},
	}
	return ticket, fake, listener
}

func TestDirectParentRecoveryDeletesOnlyExactTicketAfterProcessExit(t *testing.T) {
	ticket, fake, listener := recoveryFixture(t)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	recoverer := &IncusDirectChildRecoverer{client: fake, trustedPool: "pool"}
	if err := recoverer.RecoverDirectChild(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	want := "instance:session-child-1,volume:pool/workspace-child-1"
	if got := strings.Join(fake.deleted, ","); got != want {
		t.Fatalf("deletions = %s, want %s", got, want)
	}
	if _, err := os.Lstat(ticket.SocketPath); !os.IsNotExist(err) {
		t.Fatalf("recovery socket remains: %v", err)
	}
}

func TestDirectParentRecoveryRefusesWrongMetadataAndReplacementSocket(t *testing.T) {
	ticket, fake, listener := recoveryFixture(t)
	fake.instance.Config[metaParentID] = "wrong-parent"
	recoverer := &IncusDirectChildRecoverer{client: fake, trustedPool: "pool"}
	if err := recoverer.RecoverDirectChild(context.Background(), ticket); err == nil || !strings.Contains(err.Error(), "wrong-parent") {
		t.Fatalf("metadata mismatch error = %v", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("mismatched resources deleted: %#v", fake.deleted)
	}
	_ = listener.Close()
	_ = os.Remove(ticket.SocketPath)
	replacement, err := net.Listen("unix", ticket.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	fake.instance.Config[metaParentID] = ticket.ParentID
	if err := recoverer.RecoverDirectChild(context.Background(), ticket); err == nil || !strings.Contains(err.Error(), "replacement") {
		t.Fatalf("replacement socket error = %v", err)
	}
	if _, err := os.Lstat(ticket.SocketPath); err != nil {
		t.Fatalf("replacement socket was removed: %v", err)
	}
}
