package provision

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

func TestProvisionContractsCarryIndependentRootAndChildInputs(t *testing.T) {
	root := RootRequest{SessionID: "root-1", SocketPath: "/run/kanedias/root.sock"}
	if root.SessionID != "root-1" || root.SocketPath != "/run/kanedias/root.sock" {
		t.Fatalf("RootRequest = %#v", root)
	}

	child := ChildRequest{
		SessionID: "child-1", ParentID: "parent-1", RootID: "root-1",
		SourceInstance: "instance-parent", SourceVolume: "volume-parent",
		HostSocketPath: "/run/kanedias/child.sock",
		Worker:         config.WorkerProfile{Description: "Review.", Provider: "provider", Model: "model", ThinkingLevel: "high"},
		Contract:       contract.CreateChildRequest{WorkerType: "reviewer", Kind: contract.ChildKindRead, Context: contract.ContextFresh, Task: "Review."},
	}
	if got, want := child.Contract.Kind, contract.ChildKindRead; got != want {
		t.Fatalf("ChildRequest.Contract.Kind = %q, want %q", got, want)
	}

	resources := Resources{SessionID: "child-1", Pool: "default", Instance: "instance-child", Volume: "volume-child", RPCAddr: "10.0.0.2:4444"}
	if resources.Instance != "instance-child" || resources.Volume != "volume-child" {
		t.Fatalf("Resources = %#v", resources)
	}

	var _ RootProvisioner = rootProvisionerStub{}
	var _ ChildProvisioner = childProvisionerStub{}
}

func TestOwnershipRecordsSubmissionBeforeConfirmation(t *testing.T) {
	var ownership Ownership
	if got := ownership.Snapshot(); !reflect.DeepEqual(got, OwnershipSnapshot{}) {
		t.Fatalf("initial Snapshot() = %#v", got)
	}

	ownership.RecordVolumeSubmitted("volume-child")
	if got, want := ownership.Snapshot().Volume, (OwnedResource{Name: "volume-child", Submitted: true}); got != want {
		t.Fatalf("volume after submission = %#v, want %#v", got, want)
	}
	ownership.RecordVolumeConfirmed()
	if got, want := ownership.Snapshot().Volume, (OwnedResource{Name: "volume-child", Submitted: true, Confirmed: true}); got != want {
		t.Fatalf("volume after confirmation = %#v, want %#v", got, want)
	}

	ownership.RecordInstanceSubmitted("instance-child")
	if got, want := ownership.Snapshot().Instance, (OwnedResource{Name: "instance-child", Submitted: true}); got != want {
		t.Fatalf("instance after submission = %#v, want %#v", got, want)
	}
	ownership.RecordInstanceConfirmed()
	if got, want := ownership.Snapshot().Instance, (OwnedResource{Name: "instance-child", Submitted: true, Confirmed: true}); got != want {
		t.Fatalf("instance after confirmation = %#v, want %#v", got, want)
	}
}

func TestOwnershipSnapshotIsRaceSafeAndIndependent(t *testing.T) {
	var ownership Ownership
	ownership.RecordVolumeSubmitted("volume-child")
	ownership.RecordInstanceSubmitted("instance-child")

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(3)
		go func() { defer wg.Done(); ownership.RecordVolumeConfirmed() }()
		go func() { defer wg.Done(); ownership.RecordInstanceConfirmed() }()
		go func() { defer wg.Done(); _ = ownership.Snapshot() }()
	}
	wg.Wait()

	got := ownership.Snapshot()
	if !got.Volume.Confirmed || !got.Instance.Confirmed {
		t.Fatalf("Snapshot() = %#v, want both resources confirmed", got)
	}
	if got.Volume.Name == got.Instance.Name {
		t.Fatalf("resource operation names were conflated: %#v", got)
	}
}

type rootProvisionerStub struct{}

func (rootProvisionerStub) ProvisionRoot(context.Context, RootRequest) (*Resources, error) {
	return nil, nil
}
func (rootProvisionerStub) Destroy(context.Context, *Resources) error { return nil }

type childProvisionerStub struct{}

func (childProvisionerStub) ProvisionChild(context.Context, ChildRequest) (*Resources, error) {
	return nil, nil
}
func (childProvisionerStub) Destroy(context.Context, *Resources) error { return nil }
