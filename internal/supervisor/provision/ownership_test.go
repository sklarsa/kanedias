package provision

import (
	"reflect"
	"sync"
	"testing"
)

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
