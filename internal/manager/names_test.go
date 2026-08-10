package manager

import (
	"errors"
	"strings"
	"testing"

	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

func admitNamedRoot(t *testing.T, m *Manager, socketPath, rootID, name string, children ...string) *rootHandle {
	t.Helper()
	treeChildren := make([]supervisor.NodeSnapshot, 0, len(children))
	for _, childID := range children {
		treeChildren = append(treeChildren, childTree(childID, rootID))
	}
	tree := rootTree(rootID, treeChildren...)
	normalized, routes, err := validateRootTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	handle := &rootHandle{
		socketPath: socketPath,
		rootID:     rootID,
		name:       name,
		identity:   socketIdentity{ino: uint64(len(m.roots) + 1)},
		actionable: true,
	}
	if _, err := m.commitTree(handle, normalized, routes); err != nil {
		t.Fatalf("commitTree: %v", err)
	}
	return handle
}

func TestRootNameProjectsFleetAndChildSession(t *testing.T) {
	m := fakeManager(nil)
	admitNamedRoot(t, m, "/tmp/named.root.sock", "root", "Triage", "child")

	fleet := m.Fleet()
	if len(fleet.Roots) != 1 || fleet.Roots[0].Name != "Triage" {
		t.Fatalf("Fleet root = %#v, want custom name Triage", fleet.Roots)
	}
	child, err := m.Session("child")
	if err != nil {
		t.Fatal(err)
	}
	if child.RootName != "Triage" {
		t.Fatalf("child RootName = %q, want Triage", child.RootName)
	}
	if child.RootSessionID != "root" || child.Node.SessionID != "child" {
		t.Fatalf("name projection changed IDs: %#v", child)
	}
}

func TestRootNameEmptyProjectsEmptyAndPreservesIDFallback(t *testing.T) {
	m := fakeManager(nil)
	admitNamedRoot(t, m, "/tmp/unnamed.root.sock", "immutable-root-id", "")

	fleet := m.Fleet()
	if len(fleet.Roots) != 1 {
		t.Fatalf("Fleet roots = %d, want 1", len(fleet.Roots))
	}
	if fleet.Roots[0].Name != "" || fleet.Roots[0].RootSessionID != "immutable-root-id" {
		t.Fatalf("unnamed root projection = %#v", fleet.Roots[0])
	}
	state, err := m.Session("immutable-root-id")
	if err != nil {
		t.Fatal(err)
	}
	if state.RootName != "" || state.RootSessionID != "immutable-root-id" {
		t.Fatalf("unnamed session projection = %#v", state)
	}
}

func TestRenameRootTrimsAndChangesFleetAndSessionRevisions(t *testing.T) {
	m := fakeManager(nil)
	admitNamedRoot(t, m, "/tmp/rename.root.sock", "root", "Before", "child")
	m.fleetRevision = 10
	m.sessionRevision = 20
	fleetSub := m.SubscribeFleet()
	defer fleetSub.Close()
	sessionSub, err := m.SubscribeSession("child")
	if err != nil {
		t.Fatal(err)
	}
	defer sessionSub.Close()

	if err := m.RenameRoot("root", "  After  "); err != nil {
		t.Fatalf("RenameRoot: %v", err)
	}
	fleet := m.Fleet()
	if fleet.Revision != 11 || fleet.Roots[0].Revision != 11 || fleet.Roots[0].Name != "After" {
		t.Fatalf("fleet after rename = %#v", fleet)
	}
	state, err := m.Session("child")
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 21 || state.RootName != "After" {
		t.Fatalf("session after rename = %#v", state)
	}
	if got := <-fleetSub.Updates; got != 11 {
		t.Fatalf("fleet notification = %d, want 11", got)
	}
	if got := <-sessionSub.Updates; got != 21 {
		t.Fatalf("session notification = %d, want 21", got)
	}
}

func TestRenameRootClearsNameAndSameValueIsNoOp(t *testing.T) {
	m := fakeManager(nil)
	admitNamedRoot(t, m, "/tmp/clear.root.sock", "root", "Before")

	if err := m.RenameRoot("root", "   "); err != nil {
		t.Fatalf("clear RenameRoot: %v", err)
	}
	if got := m.Fleet().Roots[0].Name; got != "" {
		t.Fatalf("cleared name = %q, want empty", got)
	}
	fleetRevision, sessionRevision := m.fleetRevision, m.sessionRevision
	if err := m.RenameRoot("root", ""); err != nil {
		t.Fatalf("same-value RenameRoot: %v", err)
	}
	if m.fleetRevision != fleetRevision || m.sessionRevision != sessionRevision {
		t.Fatalf("same-value rename changed revisions from (%d,%d) to (%d,%d)", fleetRevision, sessionRevision, m.fleetRevision, m.sessionRevision)
	}
}

func TestRenameRootAllowsDuplicateNamesAndKeepsIDsImmutable(t *testing.T) {
	m := fakeManager(nil)
	admitNamedRoot(t, m, "/tmp/one.root.sock", "one", "First")
	admitNamedRoot(t, m, "/tmp/two.root.sock", "two", "Second")

	if err := m.RenameRoot("one", "Shared"); err != nil {
		t.Fatal(err)
	}
	if err := m.RenameRoot("two", "Shared"); err != nil {
		t.Fatal(err)
	}
	fleet := m.Fleet()
	if len(fleet.Roots) != 2 || fleet.Roots[0].RootSessionID != "one" || fleet.Roots[1].RootSessionID != "two" {
		t.Fatalf("root IDs changed after rename: %#v", fleet.Roots)
	}
	if fleet.Roots[0].Name != "Shared" || fleet.Roots[1].Name != "Shared" {
		t.Fatalf("duplicate names not projected: %#v", fleet.Roots)
	}
}

func TestRenameRootRejectsDescendantAndMissingTarget(t *testing.T) {
	m := fakeManager(nil)
	admitNamedRoot(t, m, "/tmp/target.root.sock", "root", "Before", "child")

	err := m.RenameRoot("child", "Nope")
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorInvalidRequest {
		t.Fatalf("descendant error = %v, want typed invalid request", err)
	}
	err = m.RenameRoot("missing", "Nope")
	if !errors.Is(err, errNotFound) || !errors.As(err, &typed) || typed.Code != contract.ErrorNotFound {
		t.Fatalf("missing error = %v, want typed not found", err)
	}
	if got := m.Fleet().Roots[0].Name; got != "Before" {
		t.Fatalf("failed rename changed name to %q", got)
	}
}

func TestRenameRootReusesLaunchNameNormalization(t *testing.T) {
	m := fakeManager(nil)
	admitNamedRoot(t, m, "/tmp/normalize.root.sock", "root", "Before")

	for _, invalid := range []string{"bad\nname", strings.Repeat("界", 81)} {
		beforeFleet, beforeSession := m.fleetRevision, m.sessionRevision
		err := m.RenameRoot("root", invalid)
		var typed *contract.Error
		if !errors.As(err, &typed) || typed.Code != contract.ErrorInvalidRequest {
			t.Fatalf("RenameRoot(%q) error = %v, want typed invalid request", invalid, err)
		}
		if m.fleetRevision != beforeFleet || m.sessionRevision != beforeSession {
			t.Fatalf("invalid rename changed revisions")
		}
	}
}
