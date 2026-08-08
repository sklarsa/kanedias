package manager

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor"
)

// gatedClient is a rootClient that records Snapshot/Subscribe/Close and can be
// driven deterministically: Snapshot/Subscribe optionally block until released
// via channels, and every call after Close() sets afterClose so tests can
// assert a client is never CALLED after it was Closed (use-after-close probe).
type gatedClient struct {
	mu         sync.Mutex
	snapshot   supervisor.NodeSnapshot
	closed     bool
	afterClose atomic.Bool // set true if any method runs after Close returned
	snapCalls  atomic.Int64
	subCalls   atomic.Int64
	closeCalls atomic.Int64

	// Optional gates. If non-nil, Snapshot/Subscribe send on <ready> then block
	// on <-release before returning, letting a test pin the loop mid-call.
	snapReady   chan struct{}
	snapRelease chan struct{}

	// subEvents is the channel returned to eventLoop; closed on Close so the
	// loop's live-event select unblocks.
	subEvents chan supervisor.EventEnvelope
	subClosed bool
}

func newGatedClient(snap supervisor.NodeSnapshot) *gatedClient {
	return &gatedClient{snapshot: snap, subEvents: make(chan supervisor.EventEnvelope)}
}

func (g *gatedClient) markIfClosed() {
	g.mu.Lock()
	closed := g.closed
	g.mu.Unlock()
	if closed {
		g.afterClose.Store(true)
	}
}

func (g *gatedClient) Snapshot(ctx context.Context) (supervisor.NodeSnapshot, error) {
	g.markIfClosed() // caught a call that STARTED after Close.
	g.snapCalls.Add(1)
	if g.snapReady != nil {
		// Deliberately DO NOT honor ctx here: simulate a client blocked on a real
		// network read that only unblocks when the connection is Closed. This is
		// exactly the case that makes "drain (wait for loops) THEN close" load
		// bearing — if the manager closed the client before waiting, this in-flight
		// call would be a use-after-close.
		g.snapReady <- struct{}{}
		<-g.snapRelease
	}
	g.markIfClosed() // caught a call that was IN FLIGHT when Close ran.
	g.mu.Lock()
	snap := g.snapshot
	g.mu.Unlock()
	return snap, nil
}

func (g *gatedClient) Subscribe(ctx context.Context) (supervisor.Subscription, error) {
	g.markIfClosed()
	g.subCalls.Add(1)
	return supervisor.Subscription{
		Replay: []supervisor.EventEnvelope{},
		Events: g.subEvents,
		Close:  func() {},
		Err:    func() error { return nil },
	}, nil
}

func (g *gatedClient) CallRPC(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	g.markIfClosed()
	return nil, nil
}
func (g *gatedClient) AnswerQuestion(context.Context, string, string, json.RawMessage) error {
	g.markIfClosed()
	return nil
}
func (g *gatedClient) Stop(context.Context, string) error { g.markIfClosed(); return nil }
func (g *gatedClient) Close() error {
	g.closeCalls.Add(1)
	g.mu.Lock()
	if !g.subClosed {
		close(g.subEvents)
		g.subClosed = true
	}
	g.closed = true
	g.mu.Unlock()
	return nil
}

// waitFor polls fn until it returns true or the deadline elapses.
func waitFor(t *testing.T, d time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.After(d)
	for {
		if fn() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for %s", what)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// TestMGRA_ClientReplaceNoRaceNoUseAfterClose is the MGR-A regression probe. A
// monitored handle's snapshotLoop is pinned mid-Snapshot on the OLD client, then
// a concurrent commitTree replaces the socket (new inode). The old client must
// be Closed only AFTER its loops exit (no use-after-close), and the handle's
// client field is read/written only under m.mu (no data race — proven by -race).
func TestMGRA_ClientReplaceNoRaceNoUseAfterClose(t *testing.T) {
	m := fakeManager(nil)
	m.opts.SnapshotInterval = 5 * time.Millisecond

	oldClient := newGatedClient(rootTree("root"))
	oldClient.snapReady = make(chan struct{})
	oldClient.snapRelease = make(chan struct{})

	handle := &rootHandle{
		socketPath: "/tmp/replace.root.sock",
		rootID:     "root",
		identity:   socketIdentity{dev: 1, ino: 1},
		actionable: true,
		client:     oldClient,
		mirror:     newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 100}),
		tree:       rootTree("root"),
	}
	m.roots[handle.socketPath] = handle
	m.routes["root"] = "root"

	if m.monitorRoot(handle) != monitorStarted {
		t.Fatal("monitorRoot did not start")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})

	// Pin the snapshot loop inside Snapshot on the OLD client.
	<-oldClient.snapReady

	// Concurrently commit a replacement (different inode) for the same socket.
	newClient := newGatedClient(rootTree("root"))
	replacement := &rootHandle{
		socketPath: handle.socketPath,
		rootID:     "root",
		identity:   socketIdentity{dev: 1, ino: 2}, // new inode
		actionable: true,
		client:     newClient,
	}
	_, _, _ = validateRootTree(rootTree("root"))
	_, candidate, _ := validateRootTree(rootTree("root"))

	res, err := m.commitTree(replacement, rootTree("root"), candidate)
	if err != nil {
		t.Fatalf("commitTree replace: %v", err)
	}
	if res.displaced != handle {
		t.Fatalf("expected old handle displaced, got %v", res.displaced)
	}
	// The old client must NOT be closed yet — its loop is still pinned mid-call.
	if oldClient.closeCalls.Load() != 0 {
		t.Fatal("old client closed before its loops drained (use-after-close risk)")
	}

	// Release the pinned Snapshot so the loop can observe cancellation and exit.
	// Do the drain+close in a goroutine (it Waits on the loops) while we release.
	drained := make(chan struct{})
	go func() { m.drainAndCloseDisplaced(res.displaced); close(drained) }()
	close(oldClient.snapRelease)

	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("drainAndCloseDisplaced did not complete — loops leaked")
	}

	// Old client closed exactly once, and no method ran after Close.
	if got := oldClient.closeCalls.Load(); got != 1 {
		t.Fatalf("old client Close called %d times, want 1", got)
	}
	if oldClient.afterClose.Load() {
		t.Fatal("old client was called AFTER Close — use-after-close")
	}

	// Start monitoring the replacement handle and confirm it is the live one.
	if m.monitorRoot(res.handle) != monitorStarted {
		t.Fatal("replacement handle did not start monitoring")
	}
	m.mu.Lock()
	live := m.roots[handle.socketPath]
	m.mu.Unlock()
	if live != replacement {
		t.Fatal("replacement handle is not the live root")
	}
}

// TestMGRB_RemovedRootStopsMonitorLoops is the MGR-B regression probe: after a
// monitored root is removed, its snapshot/event loops must STOP (bounded call
// counts) rather than spin forever against a closed client.
func TestMGRB_RemovedRootStopsMonitorLoops(t *testing.T) {
	m := fakeManager(nil)
	m.opts.SnapshotInterval = 2 * time.Millisecond

	client := newGatedClient(rootTree("gone"))
	handle := &rootHandle{
		socketPath: "/tmp/gone.root.sock",
		rootID:     "gone",
		identity:   socketIdentity{dev: 1, ino: 1},
		actionable: true,
		client:     client,
		mirror:     newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 100}),
		tree:       rootTree("gone"),
	}
	m.roots[handle.socketPath] = handle
	m.routes["gone"] = "gone"

	if m.monitorRoot(handle) != monitorStarted {
		t.Fatal("monitorRoot did not start")
	}

	// Let the loops run a few iterations so Snapshot/Subscribe have been called.
	waitFor(t, 2*time.Second, "loops to run", func() bool {
		return client.snapCalls.Load() >= 2 && client.subCalls.Load() >= 1
	})

	// Remove the root (simulating socket disappearance) and drain its loops.
	m.mu.Lock()
	removed := m.removeRootLocked(handle.socketPath)
	m.mu.Unlock()
	if removed != handle {
		t.Fatalf("removeRootLocked returned %v, want the handle", removed)
	}
	m.drainAndCloseDisplaced(removed)

	// Both loops have exited (drain waited on loopsWG). Record call counts and
	// assert they DO NOT grow afterward.
	snapAfter := client.snapCalls.Load()
	subAfter := client.subCalls.Load()
	time.Sleep(60 * time.Millisecond) // several snapshot intervals
	if got := client.snapCalls.Load(); got != snapAfter {
		t.Fatalf("snapshot loop kept running after removal: %d -> %d", snapAfter, got)
	}
	if got := client.subCalls.Load(); got != subAfter {
		t.Fatalf("event loop kept reconnecting after removal: %d -> %d", subAfter, got)
	}

	// Manager Close must still balance (no leaked WaitGroup accounting).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close after removal: %v", err)
	}
}

// TestMGRC_RootIDChangeUnderSameSocketPurgesOldRoutes is the MGR-C regression
// probe: replacing a socket (new inode) with a root that has a DIFFERENT rootID
// under the same path must purge the OLD rootID's routes, leaving no ghosts.
func TestMGRC_RootIDChangeUnderSameSocketPurgesOldRoutes(t *testing.T) {
	m := fakeManager(nil)

	oldClient := &fakeClient{}
	oldHandle := &rootHandle{
		socketPath: "/tmp/samepath.root.sock",
		rootID:     "old-root",
		identity:   socketIdentity{dev: 1, ino: 1},
		actionable: true,
		client:     oldClient,
	}
	m.roots[oldHandle.socketPath] = oldHandle
	// Old root owns two routes.
	m.routes["old-root"] = "old-root"
	m.routes["old-child"] = "old-root"

	// New root under the SAME socket path but a new inode and a different rootID.
	newClient := &fakeClient{}
	newHandle := &rootHandle{
		socketPath: oldHandle.socketPath,
		rootID:     "new-root",
		identity:   socketIdentity{dev: 1, ino: 2},
		actionable: true,
		client:     newClient,
	}
	candidate := map[string]string{"new-root": "new-root", "new-child": "new-root"}

	res, err := m.commitTree(newHandle, rootTree("new-root"), candidate)
	if err != nil {
		t.Fatalf("commitTree: %v", err)
	}
	m.drainAndCloseDisplaced(res.displaced)

	m.mu.Lock()
	defer m.mu.Unlock()
	// Old routes must be GONE.
	if _, ok := m.routes["old-root"]; ok {
		t.Error("ghost route 'old-root' survived rootID change")
	}
	if _, ok := m.routes["old-child"]; ok {
		t.Error("ghost route 'old-child' survived rootID change")
	}
	// New routes must be present and point at the new root.
	if m.routes["new-root"] != "new-root" || m.routes["new-child"] != "new-root" {
		t.Errorf("new routes wrong: %#v", m.routes)
	}
	// Exactly the two new routes remain (no leaked entries).
	if len(m.routes) != 2 {
		t.Errorf("expected exactly 2 routes, got %d: %#v", len(m.routes), m.routes)
	}
	// The live handle is the new one.
	if m.roots[oldHandle.socketPath] != newHandle {
		t.Error("new handle is not the live root after rootID change")
	}
}

// TestMGRC_NewRootAfterChangeIsNotRejectedByGhostRoute proves the practical
// consequence of the MGR-C fix: after the rootID change, a subsequent legit
// commit that reuses the old rootID's session IDs is NOT wrongly rejected with a
// route_conflict caused by a leaked ghost route.
func TestMGRC_NewRootAfterChangeIsNotRejectedByGhostRoute(t *testing.T) {
	m := fakeManager(nil)
	old := &rootHandle{
		socketPath: "/tmp/p.root.sock",
		rootID:     "r1",
		identity:   socketIdentity{dev: 1, ino: 1},
		client:     &fakeClient{},
	}
	m.roots[old.socketPath] = old
	m.routes["r1"] = "r1"
	m.routes["shared"] = "r1"

	// Replace with a different root that no longer claims "shared".
	repl := &rootHandle{
		socketPath: old.socketPath, rootID: "r2",
		identity: socketIdentity{dev: 1, ino: 2}, client: &fakeClient{},
	}
	res, err := m.commitTree(repl, rootTree("r2"), map[string]string{"r2": "r2"})
	if err != nil {
		t.Fatalf("commitTree replace: %v", err)
	}
	m.drainAndCloseDisplaced(res.displaced)

	// A brand-new, unrelated root elsewhere now legitimately claims "shared".
	other := &rootHandle{
		socketPath: "/tmp/other.root.sock", rootID: "r3",
		identity: socketIdentity{dev: 2, ino: 1}, client: &fakeClient{},
	}
	if _, err := m.commitTree(other, rootTree("r3"), map[string]string{"r3": "r3", "shared": "r3"}); err != nil {
		t.Fatalf("legit new root wrongly rejected by ghost route: %v", err)
	}
}

// TestMGRD_SpawnAndDiscoveryAdmitSameSocket is the MGR-D regression probe. It
// drives the REAL commitSpawn path. Via afterCommitSpawnHook it injects a
// concurrent discovery that reuses the SAME committed handle and starts its
// monitor loops in the exact window between spawn's commitTree and spawn's
// monitorRoot. commitSpawn must NOT destroy the live admitted root nor return a
// bogus "closing" error: it must observe monitorAlreadyLive and succeed.
func TestMGRD_SpawnAndDiscoveryAdmitSameSocket(t *testing.T) {
	m := fakeManager(nil)
	m.opts.SnapshotInterval = time.Hour // don't let loops fire during the test

	client := newGatedClient(rootTree("spawned"))
	pending := &pendingRoot{
		socketPath: "/tmp/spawned.root.sock",
		identity:   socketIdentity{dev: 3, ino: 3},
		client:     client,
		rootID:     "spawned",
	}

	// Inject the concurrent discovery in the commitTree->monitorRoot window: it
	// simulates discovery reusing the just-committed handle and starting its
	// loops FIRST (so spawn's later monitorRoot sees AlreadyLive).
	var hookRan bool
	m.afterCommitSpawnHook = func(committed *rootHandle) {
		hookRan = true
		// Discovery commits its own fresh handle for the same path+identity;
		// commitTree reuses the live one and returns it.
		discHandle := &rootHandle{
			socketPath: committed.socketPath,
			rootID:     "spawned",
			identity:   committed.identity, // SAME inode
			actionable: true,
			client:     newGatedClient(rootTree("spawned")),
		}
		dres, err := m.commitTree(discHandle, rootTree("spawned"), map[string]string{"spawned": "spawned"})
		if err != nil {
			t.Errorf("discovery commitTree in hook: %v", err)
			return
		}
		if dres.handle != committed {
			t.Errorf("discovery should have reused spawn's live handle")
		}
		if got := m.monitorRoot(dres.handle); got != monitorStarted {
			t.Errorf("discovery monitorRoot = %v, want monitorStarted", got)
		}
	}

	rootID, err := m.commitSpawn(pending, rootTree("spawned"))
	if err != nil {
		t.Fatalf("commitSpawn returned a bogus error (MGR-D): %v", err)
	}
	if rootID != "spawned" {
		t.Fatalf("commitSpawn rootID = %q, want spawned", rootID)
	}
	if !hookRan {
		t.Fatal("test hook did not run — interleaving not exercised")
	}

	// The admitted root must remain live and monitored, its client NOT closed.
	m.mu.Lock()
	live, ok := m.roots[pending.socketPath]
	m.mu.Unlock()
	if !ok {
		t.Fatal("admitted root was destroyed by the spawn/discovery race (MGR-D)")
	}
	if live.rootID != "spawned" {
		t.Fatalf("live root has wrong id %q", live.rootID)
	}
	if client.closeCalls.Load() != 0 {
		t.Fatal("live admitted root's client was closed out from under its loops (MGR-D)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestSpawnTokenLength asserts the generated socket filename is short enough to
// leave ample room in the 107-byte unix path budget, and that the New() probe
// still rejects genuinely-too-long dirs while accepting reasonable ones.
func TestSpawnTokenLength(t *testing.T) {
	tok, err := generateToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != spawnTokenHexLen {
		t.Fatalf("token length = %d, want %d", len(tok), spawnTokenHexLen)
	}
	if spawnTokenHexLen != 32 {
		t.Fatalf("spawnTokenHexLen = %d, want 32 (16 bytes hex)", spawnTokenHexLen)
	}
	// Lowercase hex only.
	if strings.ToLower(tok) != tok {
		t.Fatalf("token not lowercase hex: %q", tok)
	}
	for _, c := range tok {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("token has non-hex char %q in %q", c, tok)
		}
	}
	// The full socket filename ("<token>.root.sock") length.
	fname := tok + ".root.sock"
	if len(fname) != spawnTokenHexLen+len(".root.sock") {
		t.Fatalf("filename length = %d", len(fname))
	}
	// 32 + 10 = 42 bytes for the filename, leaving 107-1-42 = 64 bytes for the dir
	// (plus separator). The old 64-hex token consumed 74 bytes for the filename.
	if len(fname) != 42 {
		t.Fatalf("socket filename length = %d, want 42", len(fname))
	}
}

// TestNewProbeAcceptsModeratelyNestedDir proves a moderately-nested socket dir
// that would have FAILED with the old 64-hex token now passes with the 32-hex
// token, while a genuinely-too-long dir is still rejected.
func TestNewProbeAcceptsModeratelyNestedDir(t *testing.T) {
	// Directory that leaves room for a 32-char token filename but NOT a 64-char
	// one. Budget: 107. filename(32-hex) = 42 incl leading '/'. So a dir up to
	// 107-43 = 64 bytes passes with the new token.
	base, err := mkTempDirLen(t, 55) // dir path ~55 bytes: passes 32-hex, fails 64-hex
	if err != nil {
		t.Fatal(err)
	}

	// New-style (32-hex) probe: must PASS.
	newPath := base + "/" + strings.Repeat("a", spawnTokenHexLen) + ".root.sock"
	if err := validateUnixPathLength(newPath); err != nil {
		t.Fatalf("32-hex token rejected for a moderately-nested dir: %v (len=%d)", err, len(newPath))
	}
	// Old-style (64-hex) probe on the same dir: must FAIL (documents the win).
	oldPath := base + "/" + strings.Repeat("a", 64) + ".root.sock"
	if err := validateUnixPathLength(oldPath); err == nil {
		t.Fatalf("64-hex token would have PASSED (len=%d) — test dir not tight enough", len(oldPath))
	}

	// A genuinely-too-long dir must still be rejected even with the short token.
	tooLong := "/" + strings.Repeat("b", 90) + "/" + strings.Repeat("a", spawnTokenHexLen) + ".root.sock"
	if err := validateUnixPathLength(tooLong); err == nil {
		t.Fatalf("probe accepted a genuinely-too-long path (len=%d)", len(tooLong))
	}
}

// mkTempDirLen returns a directory path whose byte length is approximately
// target (>= target), so tests can pin the path-length budget precisely.
func mkTempDirLen(t *testing.T, target int) (string, error) {
	t.Helper()
	base := t.TempDir() // e.g. /tmp/TestXxx/001
	for len(base) < target {
		base = base + "/" + strings.Repeat("d", 8)
	}
	return base, nil
}
