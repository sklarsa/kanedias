package supervisor

import (
	"errors"
	"testing"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

func TestNewIdentityPreservesImmutableSessionOwnership(t *testing.T) {
	identity, err := NewIdentity(IdentitySpec{
		SessionID: "sess-child",
		ParentID:  "sess-parent",
		RootID:    "sess-root",
		Kind:      contract.ChildKindWrite,
		Context:   contract.ContextFork,
		Worker:    "worker",
	})
	if err != nil {
		t.Fatalf("NewIdentity() error = %v", err)
	}

	snapshot := identity.Snapshot()
	want := IdentitySnapshot{
		SessionID: "sess-child",
		ParentID:  "sess-parent",
		RootID:    "sess-root",
		Kind:      contract.ChildKindWrite,
		Context:   contract.ContextFork,
		Worker:    "worker",
	}
	if snapshot != want {
		t.Fatalf("Snapshot() = %#v, want %#v", snapshot, want)
	}

	copy := identity
	copy.sessionID = "changed"
	if got := identity.Snapshot().SessionID; got != "sess-child" {
		t.Fatalf("original identity SessionID = %q after copy mutation, want sess-child", got)
	}
}

func TestNewIdentityRejectsInvalidOwnership(t *testing.T) {
	tests := []struct {
		name string
		spec IdentitySpec
	}{
		{name: "empty session", spec: IdentitySpec{RootID: "root", Kind: contract.ChildKindRead, Context: contract.ContextFresh, ParentID: "parent", Worker: "reviewer"}},
		{name: "root does not own itself", spec: IdentitySpec{SessionID: "root", RootID: "other", Kind: contract.ChildKindRoot, Context: contract.ContextRoot}},
		{name: "root has parent", spec: IdentitySpec{SessionID: "root", ParentID: "parent", RootID: "root", Kind: contract.ChildKindRoot, Context: contract.ContextRoot}},
		{name: "root has worker", spec: IdentitySpec{SessionID: "root", RootID: "root", Kind: contract.ChildKindRoot, Context: contract.ContextRoot, Worker: "worker"}},
		{name: "child has no parent", spec: IdentitySpec{SessionID: "child", RootID: "root", Kind: contract.ChildKindRead, Context: contract.ContextFresh, Worker: "reviewer"}},
		{name: "child has root context", spec: IdentitySpec{SessionID: "child", ParentID: "parent", RootID: "root", Kind: contract.ChildKindRead, Context: contract.ContextRoot, Worker: "reviewer"}},
		{name: "child has no worker", spec: IdentitySpec{SessionID: "child", ParentID: "parent", RootID: "root", Kind: contract.ChildKindRead, Context: contract.ContextFresh}},
		{name: "unknown kind", spec: IdentitySpec{SessionID: "child", ParentID: "parent", RootID: "root", Kind: "mystery", Context: contract.ContextFresh, Worker: "reviewer"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewIdentity(tt.spec)
			if !errors.Is(err, ErrInvariant) {
				t.Fatalf("NewIdentity() error = %v, want ErrInvariant", err)
			}
		})
	}
}
