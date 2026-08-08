package manager

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

// --- inspectRootSocket tests ---

func TestInspectRootSocketRejectsSymlinkAndWrongMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "candidate.root.sock")
	target := filepath.Join(dir, "target")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectRootSocket(path, os.Lstat, os.Geteuid()); err == nil {
		t.Fatal("accepted symlink")
	}
}

func TestInspectRootSocketRejectsNonSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "regular.root.sock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectRootSocket(path, os.Lstat, os.Geteuid()); err == nil {
		t.Fatal("accepted regular file as socket")
	}
}

func TestInspectRootSocketRejectsWrongMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mode.root.sock")
	addr := &net.UnixAddr{Name: path, Net: "unix"}
	listener, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	listener.Close()
	// Socket is created with mode 0755 by default — that is already wrong.
	// Set it to 0o644 to make the intent explicit.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectRootSocket(path, os.Lstat, os.Geteuid()); err == nil {
		t.Fatal("accepted wrong mode")
	}
}

func TestInspectRootSocketRejectsForeignUID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "uid.root.sock")
	addr := &net.UnixAddr{Name: path, Net: "unix"}
	listener, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	listener.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	// Inject a fake lstat that reports a different UID.
	fakeLstat := func(p string) (os.FileInfo, error) {
		info, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		// Wrap with modified Stat_t claiming nobody UID.
		return &fakeFileInfo{FileInfo: info, uid: 65534}, nil
	}
	if _, err := inspectRootSocket(path, fakeLstat, os.Geteuid()); err == nil {
		t.Fatal("accepted foreign UID")
	}
}

func TestInspectRootSocketAcceptsValidSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "valid.root.sock")
	addr := &net.UnixAddr{Name: path, Net: "unix"}
	listener, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	listener.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := inspectRootSocket(path, os.Lstat, os.Geteuid())
	if err != nil {
		t.Fatalf("valid socket rejected: %v", err)
	}
	if id.dev == 0 && id.ino == 0 {
		t.Fatal("identity is zero")
	}
}

// fakeFileInfo wraps os.FileInfo with an overridden UID in Sys().
type fakeFileInfo struct {
	os.FileInfo
	uid uint32
}

func (f *fakeFileInfo) Sys() any {
	orig := f.FileInfo.Sys()
	if stat, ok := orig.(*syscall.Stat_t); ok {
		copy := *stat
		copy.Uid = f.uid
		return &copy
	}
	return orig
}

// --- validatePrivateDir tests ---

func TestValidatePrivateDirRejectsWrongMode(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateDir(dir); err == nil {
		t.Fatal("accepted 0755 directory")
	}
}

func TestValidatePrivateDirAcceptsOwned0700Dir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateDir(dir); err != nil {
		t.Fatalf("rejected valid private dir: %v", err)
	}
}

func TestValidatePrivateDirRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// validatePrivateDir uses os.Lstat so it should see the symlink.
	if err := validatePrivateDir(link); err == nil {
		t.Fatal("accepted symlink directory")
	}
}

// --- validateRootTree tests ---

func TestValidateRootTreeBuildsCompleteRoutes(t *testing.T) {
	tree := rootTree("root", childTree("child", "root", childTree("grandchild", "child")))
	normalized, routes, err := validateRootTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 3 || routes["grandchild"] != "root" {
		t.Fatalf("routes = %#v", routes)
	}
	if normalized.Children[0].Children[0].ParentSessionID != "child" {
		t.Fatal("parent changed")
	}
}

func TestValidateRootTreeRejectsWrongRootID(t *testing.T) {
	tree := rootTree("root")
	tree.RootSessionID = "other"
	if _, _, err := validateRootTree(tree); err == nil {
		t.Fatal("accepted wrong root ID")
	}
}

func TestValidateRootTreeRejectsRootKindBelowTop(t *testing.T) {
	child := childTree("child", "root")
	child.Kind = contract.ChildKindRoot
	tree := rootTree("root", child)
	if _, _, err := validateRootTree(tree); err == nil {
		t.Fatal("accepted root kind below top level")
	}
}

func TestValidateRootTreeRejectsDuplicateIDs(t *testing.T) {
	tree := rootTree("root",
		childTree("dup", "root"),
		childTree("dup", "root"),
	)
	if _, _, err := validateRootTree(tree); err == nil {
		t.Fatal("accepted duplicate session IDs")
	}
}

func TestValidateRootTreeRejectsUnknownLifecycle(t *testing.T) {
	tree := rootTree("root")
	tree.Lifecycle = "invented"
	if _, _, err := validateRootTree(tree); err == nil {
		t.Fatal("accepted unknown lifecycle")
	}
}

func TestValidateRootTreeRejectsWrongParent(t *testing.T) {
	child := childTree("child", "wrong-parent")
	tree := rootTree("root", child)
	if _, _, err := validateRootTree(tree); err == nil {
		t.Fatal("accepted wrong parent ID")
	}
}

func TestValidateRootTreeSortsChildrenStably(t *testing.T) {
	tree := rootTree("root",
		childTree("z-child", "root"),
		childTree("a-child", "root"),
		childTree("m-child", "root"),
	)
	normalized, _, err := validateRootTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{
		normalized.Children[0].SessionID,
		normalized.Children[1].SessionID,
		normalized.Children[2].SessionID,
	}
	if ids[0] != "a-child" || ids[1] != "m-child" || ids[2] != "z-child" {
		t.Fatalf("children not sorted: %v", ids)
	}
}

// --- discovery reconciliation tests ---

func makeRootSocket(t *testing.T, dir string, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	addr := &net.UnixAddr{Name: path, Net: "unix"}
	listener, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatalf("create root socket %q: %v", path, err)
	}
	listener.SetUnlinkOnClose(false)
	listener.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod root socket %q: %v", path, err)
	}
	return path
}

func TestDiscoverOnceInspectsOnlyRootSockEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Create a *.root.sock and a plain *.sock (child socket).
	rootPath := makeRootSocket(t, dir, "abc.root.sock")
	_ = makeRootSocket(t, dir, "child.sock")

	probed := map[string]int{}
	factory := func(socketPath string) (rootClient, error) {
		probed[socketPath]++
		tree := rootTree("abc")
		tree.SessionFile = "/s/abc.jsonl"
		return &fakeClient{snapshot: tree}, nil
	}
	m := fakeManager(factory)
	m.opts.RootSocketDir = dir
	m.discoverOnce(context.Background())

	if probed[rootPath] != 1 {
		t.Fatalf("root socket probe count = %d, want 1", probed[rootPath])
	}
	childPath := filepath.Join(dir, "child.sock")
	if probed[childPath] != 0 {
		t.Fatalf("child socket was probed, must not be")
	}
}

func TestDiscoverOnceAdmitsReadyRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	makeRootSocket(t, dir, "ready.root.sock")
	tree := rootTree("ready")
	factory := func(_ string) (rootClient, error) {
		return &fakeClient{snapshot: tree}, nil
	}
	m := fakeManager(factory)
	m.opts.RootSocketDir = dir
	m.discoverOnce(context.Background())

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.roots) != 1 {
		t.Fatalf("roots = %d, want 1", len(m.roots))
	}
	if m.routes["ready"] != "ready" {
		t.Fatalf("routes[ready] = %q, want ready", m.routes["ready"])
	}
}

func TestDiscoverOnceSkipsProvisioningAndStarting(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	makeRootSocket(t, dir, "prov.root.sock")
	tree := rootTree("prov")
	tree.Lifecycle = string(supervisor.LifecycleProvisioning)
	factory := func(_ string) (rootClient, error) {
		return &fakeClient{snapshot: tree}, nil
	}
	m := fakeManager(factory)
	m.opts.RootSocketDir = dir
	m.discoverOnce(context.Background())

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.roots) != 0 {
		t.Fatalf("provisioning root should not be admitted, got %d roots", len(m.roots))
	}
}

func TestDiscoverOnceExposesConflictIssueAndPreservesExisting(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Pre-admit root A with session "shared".
	sharedSocket := makeRootSocket(t, dir, "a.root.sock")
	treeA := rootTree("a", childTree("shared", "a"))
	treeA.Children[0].RootSessionID = "a"
	clientA := &fakeClient{snapshot: treeA}

	// Build manager with root A already admitted.
	m := fakeManager(nil)
	m.opts.RootSocketDir = dir
	handleA := &rootHandle{
		socketPath: sharedSocket,
		rootID:     "a",
		actionable: true,
		client:     clientA,
	}
	identA, err := inspectRootSocket(sharedSocket, os.Lstat, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	handleA.identity = identA
	m.roots[sharedSocket] = handleA
	m.routes["a"] = "a"
	m.routes["shared"] = "a"
	handleA.tree = treeA

	// Now create root B that also claims session "shared".
	bSocket := makeRootSocket(t, dir, "b.root.sock")
	treeB := rootTree("b", childTree("shared", "b"))
	treeB.Children[0].RootSessionID = "b"
	clientB := &fakeClient{snapshot: treeB}

	m.factory = func(socketPath string) (rootClient, error) {
		if socketPath == bSocket {
			return clientB, nil
		}
		return clientA, nil
	}

	m.discoverOnce(context.Background())

	m.mu.Lock()
	defer m.mu.Unlock()
	// Root A should still be present.
	if _, ok := m.roots[sharedSocket]; !ok {
		t.Fatal("root A was removed")
	}
	// At least one issue for the conflict.
	hasConflict := false
	for _, issue := range m.discoveryIssues {
		if issue.Code == "route_conflict" {
			hasConflict = true
		}
	}
	if !hasConflict {
		t.Fatalf("no route_conflict issue, got: %+v", m.discoveryIssues)
	}
}

func TestDiscoverOnceRemovesRootWhenSocketDisappears(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sockPath := makeRootSocket(t, dir, "gone.root.sock")
	tree := rootTree("gone")
	clientGone := &fakeClient{snapshot: tree}
	identGone, err := inspectRootSocket(sockPath, os.Lstat, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	m := fakeManager(func(_ string) (rootClient, error) { return &fakeClient{snapshot: tree}, nil })
	m.opts.RootSocketDir = dir
	m.roots[sockPath] = &rootHandle{
		socketPath: sockPath, rootID: "gone", identity: identGone,
		actionable: true, client: clientGone, tree: tree,
	}
	m.routes["gone"] = "gone"

	// Remove the socket from disk.
	if err := os.Remove(sockPath); err != nil {
		t.Fatal(err)
	}
	m.discoverOnce(context.Background())

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.roots) != 0 {
		t.Fatalf("root not removed after socket disappearance, roots=%d", len(m.roots))
	}
	if m.routes["gone"] != "" {
		t.Fatalf("route not cleared: %q", m.routes["gone"])
	}
}

func TestDiscoverOnceMalformedTreeExposesIssue(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	makeRootSocket(t, dir, "bad.root.sock")
	// Tree with wrong kind at root.
	bad := rootTree("bad")
	bad.Kind = contract.ChildKindRead
	factory := func(_ string) (rootClient, error) {
		return &fakeClient{snapshot: bad}, nil
	}
	m := fakeManager(factory)
	m.opts.RootSocketDir = dir
	m.discoverOnce(context.Background())

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.roots) != 0 {
		t.Fatalf("malformed root was admitted")
	}
	if len(m.discoveryIssues) == 0 {
		t.Fatal("no discovery issue for malformed tree")
	}
}

func TestDiscoverOnceNeverCallsOsRemove(t *testing.T) {
	// This test verifies os.Remove is never called by checking that a symlink
	// candidate is simply ignored, not deleted.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.Symlink(target, filepath.Join(dir, "sym.root.sock")); err != nil {
		t.Fatal(err)
	}
	m := fakeManager(func(_ string) (rootClient, error) {
		return nil, errors.New("should not be called")
	})
	m.opts.RootSocketDir = dir
	m.discoverOnce(context.Background())

	// Symlink must still exist.
	if _, err := os.Lstat(filepath.Join(dir, "sym.root.sock")); err != nil {
		t.Fatalf("symlink was removed: %v", err)
	}
}

func TestCommitTreeRouteConflictReturnsError(t *testing.T) {
	m := fakeManager(nil)
	// Pre-seed route "shared" -> "root-a"
	m.routes["shared"] = "root-a"

	handle := &rootHandle{socketPath: "/tmp/b.root.sock", rootID: "root-b"}
	candidate := map[string]string{"shared": "root-b"}
	if err := m.commitTree(handle, supervisor.NodeSnapshot{}, candidate); err == nil {
		t.Fatal("expected route conflict error")
	}
}
