package manager

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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
	_ = listener.Close()
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
	_ = listener.Close()
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
	_ = listener.Close()
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

func TestValidateRootTreeRejectsRootParentAndContext(t *testing.T) {
	tests := []struct {
		name string
		edit func(*supervisor.NodeSnapshot)
	}{
		{name: "parent", edit: func(tree *supervisor.NodeSnapshot) { tree.ParentSessionID = "parent" }},
		{name: "context", edit: func(tree *supervisor.NodeSnapshot) { tree.Context = contract.ContextFresh }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tree := rootTree("root")
			tc.edit(&tree)
			if _, _, err := validateRootTree(tree); err == nil {
				t.Fatalf("accepted root with invalid %s", tc.name)
			}
		})
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

func TestValidateRootTreeRejectsUnsafeSessionIDs(t *testing.T) {
	for _, id := range []string{".", "..", "../sessions/victim", "with/slash", `with\backslash`, "percent%2Fescape", "query?value", "fragment#value", "white space", "snowman-☃", "", strings.Repeat("a", 129)} {
		t.Run(id, func(t *testing.T) {
			tree := rootTree("root", childTree(id, "root"))
			if _, _, err := validateRootTree(tree); err == nil {
				t.Fatalf("accepted unsafe session ID %q", id)
			}
		})
	}
}

func TestValidateRootTreeAcceptsSafeSessionIDCharacters(t *testing.T) {
	tree := rootTree("root", childTree("child_1.example:fork-2", "root"), childTree(strings.Repeat("a", 128), "root"))
	if _, _, err := validateRootTree(tree); err != nil {
		t.Fatalf("rejected safe session ID: %v", err)
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
	_ = listener.Close()
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

func TestDiscoverOnceFailedReplacementProbeRetiresOldRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := makeRootSocket(t, dir, "replaced.root.sock")
	identity, err := inspectRootSocket(socketPath, os.Lstat, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}

	m := fakeManager(func(string) (rootClient, error) { return nil, errors.New("replacement unavailable") })
	m.opts.RootSocketDir = dir
	oldClient := &fakeClient{}
	handle := &rootHandle{
		socketPath: socketPath, rootID: "old-root", identity: identity,
		tree: rootTree("old-root"), actionable: true, client: oldClient,
	}
	handle.ctx, handle.cancel = context.WithCancel(m.closeCtx)
	m.roots[socketPath] = handle
	m.routes["old-root"] = "old-root"

	if err := os.Remove(socketPath); err != nil {
		t.Fatal(err)
	}
	makeRootSocket(t, dir, "replaced.root.sock")
	m.discoverOnce(context.Background())

	if _, ok := m.roots[socketPath]; ok {
		t.Fatal("failed replacement probe retained the old root handle")
	}
	if _, ok := m.routes["old-root"]; ok {
		t.Fatal("failed replacement probe retained the old route")
	}
	if handle.ctx.Err() == nil {
		t.Fatal("old monitor context was not cancelled after socket replacement")
	}
	if !oldClient.closed {
		t.Fatal("old client was not closed after socket replacement")
	}
	if len(m.discoveryIssues) != 1 || m.discoveryIssues[0].Code != "connect_failed" {
		t.Fatalf("discovery issues = %+v, want connect_failed", m.discoveryIssues)
	}
}

func TestDiscoverOnceAdmitsSuccessfulSocketReplacement(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := makeRootSocket(t, dir, "replace-success.root.sock")
	identity, err := inspectRootSocket(socketPath, os.Lstat, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}

	oldClient := &fakeClient{}
	newClient := &fakeClient{snapshot: rootTree("new-root")}
	m := fakeManager(func(string) (rootClient, error) { return newClient, nil })
	m.opts.RootSocketDir = dir
	oldHandle := &rootHandle{
		socketPath: socketPath, rootID: "old-root", identity: identity,
		tree: rootTree("old-root"), actionable: true, client: oldClient,
	}
	m.roots[socketPath] = oldHandle
	m.routes["old-root"] = "old-root"

	if err := os.Remove(socketPath); err != nil {
		t.Fatal(err)
	}
	makeRootSocket(t, dir, "replace-success.root.sock")
	m.discoverOnce(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})

	if got := m.roots[socketPath]; got == nil || got.rootID != "new-root" {
		t.Fatalf("replacement root = %+v, want new-root", got)
	}
	if _, ok := m.routes["old-root"]; ok {
		t.Fatal("successful replacement retained old route")
	}
	if m.routes["new-root"] != "new-root" {
		t.Fatalf("new route = %q, want new-root", m.routes["new-root"])
	}
	if !oldClient.closed {
		t.Fatal("successful replacement did not close old client")
	}
	if len(m.discoveryIssues) != 0 {
		t.Fatalf("successful replacement issues = %+v", m.discoveryIssues)
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

func TestDiscoveryIssuesDoNotExposeAbsoluteSocketPaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(dir, "invalid.root.sock")
	if err := os.WriteFile(candidate, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := fakeManager(nil)
	m.opts.RootSocketDir = dir
	m.discoverOnce(context.Background())
	if len(m.discoveryIssues) != 1 {
		t.Fatalf("issues = %#v", m.discoveryIssues)
	}
	if strings.Contains(m.discoveryIssues[0].Message, dir) || strings.Contains(m.discoveryIssues[0].Message, candidate) {
		t.Fatalf("public discovery issue exposed absolute path: %#v", m.discoveryIssues[0])
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
	if _, err := m.commitTree(handle, supervisor.NodeSnapshot{}, candidate); err == nil {
		t.Fatal("expected route conflict error")
	}
}

func TestCommitTreeRejectsAdmissionAfterQuiesce(t *testing.T) {
	m := fakeManager(nil)
	if err := m.Quiesce(context.Background()); err != nil {
		t.Fatal(err)
	}
	handle := &rootHandle{socketPath: "/tmp/late.root.sock", rootID: "late", actionable: true}
	if _, err := m.commitTree(handle, rootTree("late"), map[string]string{"late": "late"}); !errors.Is(err, errManagerQuiesced) {
		t.Fatalf("commitTree error = %v, want errManagerQuiesced", err)
	}
	if len(m.roots) != 0 || len(m.routes) != 0 {
		t.Fatalf("quiesced commit mutated manager: roots=%d routes=%d", len(m.roots), len(m.routes))
	}
}

func TestCommitTreeRejectsDuplicateRootIDOnAnotherSocket(t *testing.T) {
	m := fakeManager(nil)
	tree := rootTree("duplicate")
	first := &rootHandle{socketPath: "/tmp/first.root.sock", rootID: "duplicate", identity: socketIdentity{dev: 1, ino: 1}}
	if _, err := m.commitTree(first, tree, map[string]string{"duplicate": "duplicate"}); err != nil {
		t.Fatalf("commit first root: %v", err)
	}
	second := &rootHandle{socketPath: "/tmp/second.root.sock", rootID: "duplicate", identity: socketIdentity{dev: 1, ino: 2}}
	if _, err := m.commitTree(second, tree, map[string]string{"duplicate": "duplicate"}); err == nil {
		t.Fatal("accepted duplicate root ID on another socket")
	}
	if len(m.roots) != 1 || m.roots[first.socketPath] != first {
		t.Fatalf("duplicate commit mutated roots: %#v", m.roots)
	}
}

// TestCommitTreeConflictIsAtomic verifies that commitTree does not mutate a
// live, monitored handle when a route conflict is detected. Before the fix,
// commitTree swapped the client and reassigned fields BEFORE the conflict
// check, leaving the existing handle pointing at a closed client on error.
func TestCommitTreeConflictIsAtomic(t *testing.T) {
	m := fakeManager(nil)

	// Set up a live monitored handle for socket "a.root.sock" owning route "owns".
	existingClient := &fakeClient{}
	existingHandle := &rootHandle{
		socketPath: "/tmp/a.root.sock",
		rootID:     "root-a",
		identity:   socketIdentity{dev: 1, ino: 1},
		actionable: true,
		client:     existingClient,
	}
	m.roots["/tmp/a.root.sock"] = existingHandle
	m.routes["root-a"] = "root-a"
	m.routes["owns"] = "root-a"

	// Pre-seed "shared" -> "root-b" so the incoming commit will conflict.
	m.routes["shared"] = "root-b"

	// Caller's fresh client (different identity — simulates replaced inode).
	freshClient := &fakeClient{}
	incomingHandle := &rootHandle{
		socketPath: "/tmp/a.root.sock",
		rootID:     "root-a-new",
		identity:   socketIdentity{dev: 1, ino: 2}, // different inode
		actionable: true,
		client:     freshClient,
	}
	// Candidate claims "shared" which is already owned by "root-b" — conflict.
	candidate := map[string]string{"root-a-new": "root-a-new", "shared": "root-a-new"}

	_, err := m.commitTree(incomingHandle, supervisor.NodeSnapshot{}, candidate)
	if err == nil {
		t.Fatal("expected route conflict error, got nil")
	}

	// The existing handle's client must NOT have been closed.
	if existingClient.closed {
		t.Error("commitTree closed the existing handle's client on conflict — not atomic")
	}

	// The existing handle's fields must be unchanged.
	if existingHandle.rootID != "root-a" {
		t.Errorf("existingHandle.rootID mutated to %q, want root-a", existingHandle.rootID)
	}
	if existingHandle.identity != (socketIdentity{dev: 1, ino: 1}) {
		t.Errorf("existingHandle.identity mutated: %+v", existingHandle.identity)
	}
	if existingHandle.client != existingClient {
		t.Error("existingHandle.client was replaced on conflict — not atomic")
	}

	// Routes must be unchanged.
	if m.routes["owns"] != "root-a" {
		t.Errorf("route 'owns' changed to %q, want root-a", m.routes["owns"])
	}
	if m.routes["shared"] != "root-b" {
		t.Errorf("route 'shared' changed to %q, want root-b", m.routes["shared"])
	}

	// The caller's fresh client must NOT have been closed by commitTree (the
	// caller, probeRoot, will close it on error — no double-close).
	if freshClient.closed {
		t.Error("commitTree closed the caller's fresh client on conflict — caller would double-close")
	}
}

func TestRootNameDiscoveryCreatedHandleIsEmpty(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "discovered.root.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := inspectRootSocket(socketPath, os.Lstat, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{snapshot: rootTree("discovered")}
	m := fakeManager(func(string) (rootClient, error) { return client, nil })
	var issues []DiscoveryIssue
	if changed := m.probeRoot(context.Background(), socketPath, identity, &issues); !changed {
		t.Fatalf("probeRoot did not admit discovery handle; issues=%#v", issues)
	}
	fleet := m.Fleet()
	if len(fleet.Roots) != 1 || fleet.Roots[0].Name != "" {
		t.Fatalf("discovery root projection = %#v, want empty custom name", fleet.Roots)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
