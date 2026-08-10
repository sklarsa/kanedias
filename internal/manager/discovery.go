package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/sklarsa/kanedias/internal/supervisor"
)

// errHandleCancelled is returned by commitTree when the caller's handle has
// already been cancelled (removed/displaced). It signals a stale monitor loop to
// exit quietly rather than mark the (already-gone) root stale.
var (
	errHandleCancelled = errors.New("manager: root handle cancelled")
	errManagerQuiesced = errors.New("manager: quiesced")
)

// rootHandle is the manager's in-memory record for one admitted root socket.
//
// Concurrency invariant: every mutable field below (client, identity, rootID,
// name, nameTouched, actionable, tree, stale, streamConnected, mirror,
// monitoring, ctx/cancel) is
// guarded by Manager.mu. The monitor loops (snapshotLoop/eventLoop) MUST read
// handle.client — and any other mutable field they touch — only while holding
// m.mu; they snapshot the pointer under the lock and use the snapshot for the
// network call. A client is never Closed while a loop that may still reference
// it is running: on client-replace or removal we cancel the handle's loops and
// wait for them to exit (via monitorWG bookkeeping / per-handle context) before
// closing the old client.
type rootHandle struct {
	socketPath      string
	rootID          string
	name            string
	nameTouched     bool
	identity        socketIdentity
	tree            supervisor.NodeSnapshot
	mirror          *eventMirror
	stale           bool
	streamConnected bool
	// actionable is false for stopping/failed/stopped/completed snapshots.
	actionable bool
	client     rootClient
	// monitoring is true once monitorRoot has launched this handle's snapshot
	// and event loops. It guards against double-starting the loops when a root's
	// tree is re-committed on a later discovery/snapshot cycle. Guarded by m.mu.
	monitoring bool
	// ctx/cancel scope this handle's monitor loops. cancel is a child of the
	// manager-wide closeCtx, so cancelling closeCtx also cancels every handle.
	// Cancelling a single handle (on removal or client-replace) stops just that
	// handle's loops without touching the rest of the fleet. Guarded by m.mu.
	ctx    context.Context
	cancel context.CancelFunc
	// loopsWG tracks this handle's own snapshot+event goroutines so a
	// client-replace can wait for exactly these loops to exit before closing the
	// replaced client (MGR-A), without waiting on the whole fleet. Each loop
	// calls Done on exit; monitorRootLocked calls Add(2) when it starts them.
	loopsWG sync.WaitGroup
}

// monitorResult is the tri-state outcome of monitorRoot, letting callers
// distinguish "the manager refused because it is closing" (in which case the
// caller must clean up the orphaned handle) from "someone else already monitors
// this handle" (in which case the handle is live and must NOT be removed).
type monitorResult int

const (
	// monitorStarted: this call launched the handle's loops; it is now live.
	monitorStarted monitorResult = iota
	// monitorAlreadyLive: the handle was already being monitored by an earlier
	// call (e.g. a concurrent discovery started it). It is live; do not remove.
	monitorAlreadyLive
	// monitorRefusedClosing: the manager is closing; no loops were started and
	// the handle will never be monitored. The caller should remove it.
	monitorRefusedClosing
)

// discoverOnce scans opts.RootSocketDir for *.root.sock files, probes each
// one, and updates the manager's root set and route index atomically.
func (m *Manager) discoverOnce(ctx context.Context) {
	entries, err := os.ReadDir(m.opts.RootSocketDir)
	if err != nil {
		m.opts.Logger.Error("discovery: read root socket dir", "dir", m.opts.RootSocketDir, "err", err)
		return
	}

	// Build a map of currently known socket paths.
	m.mu.Lock()
	known := make(map[string]*rootHandle, len(m.roots))
	for _, handle := range m.roots {
		known[handle.socketPath] = handle
	}
	m.mu.Unlock()

	var newIssues []DiscoveryIssue
	changed := false

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".root.sock") {
			continue
		}
		socketPath := filepath.Join(m.opts.RootSocketDir, name)
		// Pre-probe identity check.
		identity, err := inspectRootSocket(socketPath, os.Lstat, os.Geteuid())
		if err != nil {
			m.opts.Logger.Warn("discovery: candidate inspection failed", "socket", name, "err", err)
			newIssues = append(newIssues, DiscoveryIssue{
				SocketName: name,
				Code:       "inspect_failed",
				Message:    "candidate validation failed",
			})
			continue
		}

		if handle, ok := known[socketPath]; ok && handle.identity == identity {
			// Socket generation is unchanged. Its independent snapshot loop owns
			// tree refresh, so discovery has nothing to do.
			delete(known, socketPath)
			continue
		}

		// A client dials by pathname, so once the path names a different socket
		// generation the old handle must be retired before probing it. Keeping a
		// cancelled handle would retain an unusable client indefinitely when the
		// replacement cannot be admitted.
		if displaced := m.removeReplacedSocket(socketPath, identity); displaced != nil {
			changed = true
			m.drainAndCloseDisplaced(displaced)
		}
		if m.probeRoot(ctx, socketPath, identity, &newIssues) {
			changed = true
		}
		delete(known, socketPath)
	}

	// Anything left in `known` has disappeared from disk. Cancel and evict each
	// under the lock, then drain their loops and close their clients outside the
	// lock (the loops need m.mu to finish).
	m.mu.Lock()
	var removed []*rootHandle
	for socketPath := range known {
		if h := m.removeRootLocked(socketPath); h != nil {
			removed = append(removed, h)
			changed = true
		}
	}
	issuesChanged := !slices.Equal(m.discoveryIssues, newIssues)
	m.discoveryIssues = newIssues
	m.mu.Unlock()
	for _, h := range removed {
		m.drainAndCloseDisplaced(h)
	}
	if changed || issuesChanged {
		m.bumpFleetRevision()
	}
}

// removeReplacedSocket retires the old handle before a new socket generation
// at the same path is probed. The caller drains and closes the returned handle
// outside m.mu.
func (m *Manager) removeReplacedSocket(socketPath string, identity socketIdentity) *rootHandle {
	m.mu.Lock()
	defer m.mu.Unlock()
	handle, ok := m.roots[socketPath]
	if !ok || handle.identity == identity {
		return nil
	}
	return m.removeRootLocked(socketPath)
}

// probeRoot connects to a candidate root socket, fetches a snapshot, validates
// it, and attempts to commit the tree atomically. It reports whether fleet state
// changed so discovery can publish one coalesced revision.
func (m *Manager) probeRoot(ctx context.Context, socketPath string, identity socketIdentity, issues *[]DiscoveryIssue) bool {
	name := filepath.Base(socketPath)

	client, err := m.factory(socketPath)
	if err != nil {
		m.opts.Logger.Warn("discovery: candidate connection failed", "socket", name, "err", err)
		*issues = append(*issues, DiscoveryIssue{
			SocketName: name, Code: "connect_failed", Message: "candidate connection failed",
		})
		return false
	}

	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		_ = client.Close()
		m.opts.Logger.Warn("discovery: candidate snapshot failed", "socket", name, "err", err)
		*issues = append(*issues, DiscoveryIssue{
			SocketName: name, Code: "probe_failed", Message: "candidate snapshot failed",
		})
		return false
	}

	// Re-check socket identity after the probe to guard against TOCTOU.
	postIdentity, err := inspectRootSocket(socketPath, os.Lstat, os.Geteuid())
	if err != nil || postIdentity != identity {
		_ = client.Close()
		*issues = append(*issues, DiscoveryIssue{
			SocketName: name, Code: "identity_changed", Message: "socket was replaced during probe",
		})
		return false
	}

	lc := supervisor.LifecycleState(snapshot.Lifecycle)
	switch lc {
	case supervisor.LifecycleProvisioning, supervisor.LifecycleStarting:
		// Retry next cycle.
		_ = client.Close()
		return false
	case supervisor.LifecycleReady, supervisor.LifecycleRunning:
		// Attempt full admission.
	case supervisor.LifecycleStopping, supervisor.LifecycleFailed,
		supervisor.LifecycleStopped, supervisor.LifecycleCompleted:
		// Retain as non-actionable.
		return m.commitStoppingRoot(socketPath, identity, snapshot, client, issues)
	default:
		_ = client.Close()
		m.opts.Logger.Warn("discovery: candidate has unrecognised lifecycle", "socket", name, "lifecycle", snapshot.Lifecycle)
		*issues = append(*issues, DiscoveryIssue{
			SocketName: name, Code: "malformed",
			Message: "candidate snapshot has an unrecognised lifecycle",
		})
		return false
	}

	// Validate full tree.
	normalized, candidate, err := validateRootTree(snapshot)
	if err != nil {
		_ = client.Close()
		m.opts.Logger.Warn("discovery: malformed candidate tree", "socket", name, "err", err)
		*issues = append(*issues, DiscoveryIssue{
			SocketName: name, Code: "malformed", Message: "candidate snapshot is malformed",
		})
		return false
	}
	if !admissible(snapshot) {
		_ = client.Close()
		*issues = append(*issues, DiscoveryIssue{
			SocketName: name, Code: "malformed",
			Message: "root is ready/running but lacks Pi binding",
		})
		return false
	}

	handle := &rootHandle{
		socketPath: socketPath,
		rootID:     snapshot.SessionID,
		identity:   identity,
		actionable: true,
		client:     client,
	}
	res, err := m.commitTree(handle, normalized, candidate)
	if err != nil {
		_ = client.Close()
		m.opts.Logger.Warn("discovery: candidate route conflict", "socket", name, "err", err)
		*issues = append(*issues, DiscoveryIssue{
			SocketName: name, Code: "route_conflict", Message: "candidate conflicts with an admitted root",
		})
		return false
	}
	// If a replaced-inode handle was displaced, drain its loops and close its old
	// client outside the lock (MGR-A).
	m.drainAndCloseDisplaced(res.displaced)
	m.admitCommitted(res.handle, handle)
	return true
}

// admitCommitted starts monitoring the just-committed handle and cleans up only
// when the manager is genuinely closing. It distinguishes "already monitored by
// a concurrent admitter" (leave it alone — it is live, MGR-D) from "refused
// because closing" (remove the orphan). ownHandle is the fresh handle the caller
// constructed; removal is attempted only when the committed handle IS that fresh
// handle (never a reused live one).
func (m *Manager) admitCommitted(committed, ownHandle *rootHandle) {
	if m.monitorRoot(committed) == monitorRefusedClosing && committed == ownHandle {
		// Manager is closing and this is a brand-new handle that will never be
		// monitored — release its client to avoid a leak.
		m.removeRootBySocketPath(ownHandle.socketPath, ownHandle)
	}
}

// commitStoppingRoot records a stopping/failed root as non-actionable.
func (m *Manager) commitStoppingRoot(socketPath string, identity socketIdentity, snapshot supervisor.NodeSnapshot, client rootClient, issues *[]DiscoveryIssue) bool {
	name := filepath.Base(socketPath)
	normalized, candidate, err := validateRootTree(snapshot)
	if err != nil {
		_ = client.Close()
		m.opts.Logger.Warn("discovery: malformed retained candidate tree", "socket", name, "err", err)
		*issues = append(*issues, DiscoveryIssue{
			SocketName: name, Code: "malformed", Message: "candidate snapshot is malformed",
		})
		return false
	}
	handle := &rootHandle{
		socketPath: socketPath,
		rootID:     snapshot.SessionID,
		identity:   identity,
		actionable: false,
		client:     client,
	}
	res, err := m.commitTree(handle, normalized, candidate)
	if err != nil {
		_ = client.Close()
		m.opts.Logger.Warn("discovery: retained candidate route conflict", "socket", name, "err", err)
		*issues = append(*issues, DiscoveryIssue{
			SocketName: name, Code: "route_conflict", Message: "candidate conflicts with an admitted root",
		})
		return false
	}
	m.drainAndCloseDisplaced(res.displaced)
	m.admitCommitted(res.handle, handle)
	return true
}

// commitResult is what commitTree returns to the caller so any needed
// out-of-lock teardown can happen after the lock is released.
type commitResult struct {
	// handle is the handle now recorded in m.roots for this socket path (either
	// the caller's handle, or a reused existing one). Callers start/keep
	// monitoring exactly this instance.
	handle *rootHandle
	// displaced, if non-nil, is a previously-monitored handle for the same
	// socket path whose socket was replaced by a NEW inode. Its monitor loops
	// have been cancelled under the lock; the caller MUST wait for them to exit
	// and then close displaced.client — outside the lock — to avoid a
	// use-after-close on a client a running loop might still be about to call
	// (MGR-A). Its routes have already been removed under the lock (MGR-C).
	displaced *rootHandle
}

// commitTree atomically commits a validated root tree and its routes, or
// nothing on conflict. It replaces routes from the previous rootID if the
// handle is already known.
//
// If a live handle for the same socket path already exists (e.g. one just
// created by a concurrent SpawnRoot, or the handle currently being monitored),
// commitTree behaves in one of two ways:
//
//   - Same socket identity (same inode): the existing handle and its running
//     monitor loops are kept; the caller's redundant client is discarded. The
//     existing handle is returned.
//
//   - Replaced socket identity (new inode under the same name): the existing
//     handle's loops are cancelled and it is evicted from m.roots (its routes
//     removed via the OLD rootID — MGR-C), and the caller's fresh handle is
//     installed in its place. The evicted handle is returned as
//     commitResult.displaced so the caller can drain-then-close its old client
//     outside the lock (MGR-A). This never Closes a client a running loop may
//     still call under the lock.
func (m *Manager) commitTree(handle *rootHandle, tree supervisor.NodeSnapshot, candidate map[string]string) (commitResult, error) {
	return m.commitTreeWithAdmission(handle, tree, candidate, handle.actionable, nil)
}

func (m *Manager) commitTreeWithActionability(handle *rootHandle, tree supervisor.NodeSnapshot, candidate map[string]string, actionable bool) (commitResult, error) {
	return m.commitTreeWithAdmission(handle, tree, candidate, actionable, nil)
}

// commitSpawnTree folds a pending launch name into the same manager lock hold
// that admits the root handle and its routes. A same-socket handle keeps a name
// that a user has already explicitly renamed or cleared.
func (m *Manager) commitSpawnTree(handle *rootHandle, tree supervisor.NodeSnapshot, candidate map[string]string, name string) (commitResult, error) {
	return m.commitTreeWithAdmission(handle, tree, candidate, handle.actionable, &name)
}

func (m *Manager) commitTreeWithAdmission(handle *rootHandle, tree supervisor.NodeSnapshot, candidate map[string]string, actionable bool, launchName *string) (commitResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Quiesce is an admission barrier: probes or spawns already in flight must
	// not register a root after shutdown has begun.
	if m.quiesced || m.closed {
		return commitResult{}, errManagerQuiesced
	}

	// Guard against a stale monitor loop: if the caller's handle has already been
	// cancelled (removed or displaced), its in-flight snapshot iteration must NOT
	// resurrect it or displace whatever replaced it. Reject the commit; the loop
	// will exit on its next cancellation check.
	if handle.ctx != nil && handle.ctx.Err() != nil {
		return commitResult{}, errHandleCancelled
	}

	// A root identity belongs to exactly one socket. Without this check, two
	// handles with the same rootID coexist and route lookup picks a client by
	// nondeterministic map iteration.
	for socketPath, existing := range m.roots {
		if existing != handle && socketPath != handle.socketPath && existing.rootID == handle.rootID {
			return commitResult{}, fmt.Errorf("duplicate root ID %q on sockets %q and %q", handle.rootID, existing.socketPath, handle.socketPath)
		}
	}

	// Determine the rootID that currently owns this socket path (if any). Routes
	// belonging to that existing root are NOT conflicts — they will be removed by
	// removeRoutesLocked before the new routes are installed.
	var reuseRootID string
	if existing, ok := m.roots[handle.socketPath]; ok && existing != handle {
		if existing.identity == handle.identity && existing.rootID != handle.rootID {
			return commitResult{}, fmt.Errorf("root ID changed from %q to %q without a socket replacement", existing.rootID, handle.rootID)
		}
		reuseRootID = existing.rootID
	}

	// Conflict check FIRST, before any mutation. A conflict is a candidate
	// sessionID already routed to a different rootID — excluding the reused root's
	// own prior routes, which removeRoutesLocked will clear momentarily.
	for sessionID, rootID := range candidate {
		if owned, ok := m.routes[sessionID]; ok && owned != rootID && owned != reuseRootID {
			return commitResult{}, fmt.Errorf("route conflict for session %q (owned by %q, new root %q)", sessionID, owned, rootID)
		}
	}

	// Conflict check passed — now it is safe to mutate.

	target := handle
	var displaced *rootHandle
	if existing, ok := m.roots[handle.socketPath]; ok && existing != handle {
		if existing.identity == handle.identity {
			// Same socket (e.g. concurrent SpawnRoot already admitted it):
			// discard the caller's redundant client, keep the existing instance
			// and its already-running monitor loops.
			if handle.client != nil && handle.client != existing.client {
				_ = handle.client.Close()
			}
			// Identity equality above also guarantees rootID equality. Keep rootID
			// immutable once monitoring starts so event-loop reads cannot race a
			// redundant same-value write from concurrent discovery.
			existing.actionable = actionable
			target = existing
		} else {
			// Socket was replaced (new inode) under the same name. We must NOT
			// swap the client under the existing handle: a running loop may have
			// snapshotted the old client and be mid-call, so closing it here
			// would be a use-after-close (MGR-A). Instead, cancel the existing
			// handle's loops under the lock and evict it; the caller's fresh
			// handle takes over. Remove the OLD root's routes via its OWN rootID
			// (MGR-C) before installing the new routes below.
			m.removeRoutesLocked(existing.rootID)
			if existing.cancel != nil {
				existing.cancel()
			}
			delete(m.roots, existing.socketPath)
			displaced = existing
			// target stays as the caller's fresh handle.
		}
	}

	// Remove any pre-existing routes for the target's rootID (idempotent for a
	// fresh handle) before installing the candidate routes.
	m.removeRoutesLocked(target.rootID)
	for sessionID, rootID := range candidate {
		m.routes[sessionID] = rootID
	}
	target.tree = tree
	target.actionable = actionable
	if launchName != nil && !target.nameTouched {
		target.name = *launchName
	}
	if target.mirror == nil {
		target.mirror = newEventMirror(m.opts.EventLimits)
	}
	m.roots[target.socketPath] = target
	return commitResult{handle: target, displaced: displaced}, nil
}

// drainAndCloseDisplaced waits for a displaced handle's monitor loops to fully
// exit, THEN closes its client. It must be called WITHOUT m.mu held (the loops
// acquire m.mu in currentClient/markStale/etc. before they can finish). This is
// the out-of-lock half of the safe client-replace sequence (MGR-A):
//
//	commitTree (under lock): cancel displaced.ctx + evict from m.roots
//	here      (no lock)    : wait loops out, then Close the old client
//
// Waiting before Close guarantees no loop is mid-call on displaced.client when
// it is closed, so there is no use-after-close. displaced.client is closed
// exactly once here and nowhere else, so there is no double-close.
func (m *Manager) drainAndCloseDisplaced(displaced *rootHandle) {
	if displaced == nil {
		return
	}
	displaced.loopsWG.Wait()
	if displaced.client != nil {
		_ = displaced.client.Close()
	}
}

// removeRootBySocketPath removes a just-committed handle that will never be
// monitored (manager closing) so its client is not leaked. It only removes the
// entry if it is still the exact handle we committed.
func (m *Manager) removeRootBySocketPath(socketPath string, handle *rootHandle) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.roots[socketPath]; ok && h == handle {
		m.removeRoutesLocked(handle.rootID)
		delete(m.roots, socketPath)
		if handle.client != nil {
			_ = handle.client.Close()
		}
	}
}

// removeRootLocked removes the root at socketPath and all its routes from the
// index, and cancels its monitor loops so they stop running (MGR-B: no more
// leaked snapshot/event goroutines spinning against a removed root). Must be
// called with m.mu held.
//
// It does NOT close the handle's client — the loops may still be mid-call, so
// closing here would risk a use-after-close (MGR-A). It returns the removed
// handle; the caller must call drainAndCloseDisplaced on it OUTSIDE the lock to
// wait the loops out and then close the client. Returns nil if nothing removed.
func (m *Manager) removeRootLocked(socketPath string) *rootHandle {
	handle, ok := m.roots[socketPath]
	if !ok {
		return nil
	}
	m.removeRoutesLocked(handle.rootID)
	delete(m.roots, socketPath)
	if handle.cancel != nil {
		handle.cancel()
	}
	return handle
}

// removeRoutesLocked removes all routes for the given rootID and publishes one
// session invalidation when any route was removed. Must be called with m.mu
// held. changeFanout.Publish is non-blocking, so publishing here keeps every
// route-removal path centralized without delaying manager mutations.
func (m *Manager) removeRoutesLocked(rootID string) {
	removed := false
	for sessionID, rid := range m.routes {
		if rid == rootID {
			delete(m.routes, sessionID)
			removed = true
		}
	}
	if removed {
		m.sessionRevision++
		m.sessionFanout.Publish(m.sessionRevision)
	}
}
