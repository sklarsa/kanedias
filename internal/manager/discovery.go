package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sklarsa/kanedias/internal/supervisor"
)

// rootHandle is the manager's in-memory record for one admitted root socket.
type rootHandle struct {
	socketPath      string
	rootID          string
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
}

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

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".root.sock") {
			continue
		}
		socketPath := filepath.Join(m.opts.RootSocketDir, name)
		// Pre-probe identity check.
		identity, err := inspectRootSocket(socketPath, os.Lstat, os.Geteuid())
		if err != nil {
			newIssues = append(newIssues, DiscoveryIssue{
				SocketName: name,
				Code:       "inspect_failed",
				Message:    err.Error(),
			})
			continue
		}

		// If we already know this exact socket (same device+inode), check if
		// it still exists.
		if handle, ok := known[socketPath]; ok && handle.identity == identity {
			// Socket is unchanged. Nothing to do for existing admitted roots.
			delete(known, socketPath)
			continue
		}
		// New or replaced socket — probe it.
		m.probeRoot(ctx, socketPath, identity, &newIssues)
		delete(known, socketPath)
	}

	// Anything left in `known` has disappeared from disk.
	m.mu.Lock()
	for socketPath := range known {
		m.removeRootLocked(socketPath)
	}
	// Replace discovery issues list.
	m.discoveryIssues = newIssues
	m.mu.Unlock()
}

// probeRoot connects to a candidate root socket, fetches a snapshot, validates
// it, and attempts to commit the tree atomically.
func (m *Manager) probeRoot(ctx context.Context, socketPath string, identity socketIdentity, issues *[]DiscoveryIssue) {
	name := filepath.Base(socketPath)

	client, err := m.factory(socketPath)
	if err != nil {
		*issues = append(*issues, DiscoveryIssue{
			SocketName: name, Code: "connect_failed", Message: err.Error(),
		})
		return
	}

	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		_ = client.Close()
		// Retain existing root on transient failure.
		m.mu.Lock()
		if _, ok := m.roots[socketPath]; ok {
			m.roots[socketPath].actionable = admissible(m.roots[socketPath].tree)
		}
		m.mu.Unlock()
		*issues = append(*issues, DiscoveryIssue{
			SocketName: name, Code: "probe_failed", Message: err.Error(),
		})
		return
	}

	// Re-check socket identity after the probe to guard against TOCTOU.
	postIdentity, err := inspectRootSocket(socketPath, os.Lstat, os.Geteuid())
	if err != nil || postIdentity != identity {
		_ = client.Close()
		*issues = append(*issues, DiscoveryIssue{
			SocketName: name, Code: "identity_changed", Message: "socket was replaced during probe",
		})
		return
	}

	lc := supervisor.LifecycleState(snapshot.Lifecycle)
	switch lc {
	case supervisor.LifecycleProvisioning, supervisor.LifecycleStarting:
		// Retry next cycle.
		_ = client.Close()
		return
	case supervisor.LifecycleReady, supervisor.LifecycleRunning:
		// Attempt full admission.
	case supervisor.LifecycleStopping, supervisor.LifecycleFailed,
		supervisor.LifecycleStopped, supervisor.LifecycleCompleted:
		// Retain as non-actionable.
		m.commitStoppingRoot(socketPath, identity, snapshot, client, issues)
		return
	default:
		_ = client.Close()
		*issues = append(*issues, DiscoveryIssue{
			SocketName: name, Code: "malformed",
			Message: fmt.Sprintf("unrecognised root lifecycle %q", snapshot.Lifecycle),
		})
		return
	}

	// Validate full tree.
	normalized, candidate, err := validateRootTree(snapshot)
	if err != nil {
		_ = client.Close()
		*issues = append(*issues, DiscoveryIssue{
			SocketName: name, Code: "malformed", Message: "tree validation: " + err.Error(),
		})
		return
	}
	if !admissible(snapshot) {
		_ = client.Close()
		*issues = append(*issues, DiscoveryIssue{
			SocketName: name, Code: "malformed",
			Message: "root is ready/running but lacks Pi binding",
		})
		return
	}

	handle := &rootHandle{
		socketPath: socketPath,
		rootID:     snapshot.SessionID,
		identity:   identity,
		actionable: true,
		client:     client,
	}
	committed, err := m.commitTree(handle, normalized, candidate)
	if err != nil {
		_ = client.Close()
		*issues = append(*issues, DiscoveryIssue{
			SocketName: name, Code: "route_conflict", Message: err.Error(),
		})
		return
	}
	// Start monitoring newly-admitted roots exactly once. Roots re-committed on a
	// later scan already have monitoring running; monitorRoot is a no-op for them.
	if !m.monitorRoot(committed) && committed == handle {
		// Manager is closing and this is a brand-new handle that will never be
		// monitored — release its client to avoid a leak.
		m.removeRootBySocketPath(handle.socketPath, handle)
	}
}

// commitStoppingRoot records a stopping/failed root as non-actionable.
func (m *Manager) commitStoppingRoot(socketPath string, identity socketIdentity, snapshot supervisor.NodeSnapshot, client rootClient, issues *[]DiscoveryIssue) {
	name := filepath.Base(socketPath)
	normalized, candidate, err := validateRootTree(snapshot)
	if err != nil {
		_ = client.Close()
		*issues = append(*issues, DiscoveryIssue{
			SocketName: name, Code: "malformed", Message: "stopping tree validation: " + err.Error(),
		})
		return
	}
	handle := &rootHandle{
		socketPath: socketPath,
		rootID:     snapshot.SessionID,
		identity:   identity,
		actionable: false,
		client:     client,
	}
	committed, err := m.commitTree(handle, normalized, candidate)
	if err != nil {
		_ = client.Close()
		*issues = append(*issues, DiscoveryIssue{
			SocketName: name, Code: "route_conflict", Message: err.Error(),
		})
		return
	}
	if !m.monitorRoot(committed) && committed == handle {
		m.removeRootBySocketPath(handle.socketPath, handle)
	}
}

// commitTree atomically commits a validated root tree and its routes, or
// nothing on conflict. It replaces routes from the previous rootID if the
// handle is already known.
//
// If a live handle for the same socket path already exists (e.g. one just
// created by a concurrent SpawnRoot, or the handle currently being monitored),
// commitTree updates that existing handle in place rather than overwriting it
// with a second handle. This avoids leaking the prior handle's client and
// orphaning its monitor goroutines. commitTree returns the committed handle
// (the one now recorded in m.roots) so callers can start monitoring exactly the
// admitted instance.
func (m *Manager) commitTree(handle *rootHandle, tree supervisor.NodeSnapshot, candidate map[string]string) (*rootHandle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Determine the rootID that currently owns this socket path (if any). Routes
	// belonging to that existing root are NOT conflicts — they will be removed by
	// removeRoutesLocked before the new routes are installed.
	var reuseRootID string
	if existing, ok := m.roots[handle.socketPath]; ok && existing != handle {
		reuseRootID = existing.rootID
	}

	// Conflict check FIRST, before any mutation. A conflict is a candidate
	// sessionID already routed to a different rootID — excluding the reused root's
	// own prior routes, which removeRoutesLocked will clear momentarily.
	for sessionID, rootID := range candidate {
		if owned, ok := m.routes[sessionID]; ok && owned != rootID && owned != reuseRootID {
			return nil, fmt.Errorf("route conflict for session %q (owned by %q, new root %q)", sessionID, owned, rootID)
		}
	}

	// Conflict check passed — now it is safe to mutate.

	// Reuse a live handle for the same socket path if one already exists and it
	// is a different instance than the caller's (Q1 orphan guard). Reusing the
	// existing instance keeps its already-running monitor goroutines valid (they
	// look themselves up by pointer/socket path) and avoids leaking clients.
	target := handle
	if existing, ok := m.roots[handle.socketPath]; ok && existing != handle {
		if existing.identity == handle.identity {
			// Same socket (e.g. concurrent SpawnRoot already admitted it):
			// discard the caller's redundant client, keep the existing one.
			if handle.client != nil && handle.client != existing.client {
				_ = handle.client.Close()
			}
		} else {
			// Socket was replaced (new inode) under the same name: swap the fresh
			// client into the existing (monitored) handle and close the stale one.
			if existing.client != nil && existing.client != handle.client {
				_ = existing.client.Close()
			}
			existing.client = handle.client
			existing.identity = handle.identity
		}
		existing.rootID = handle.rootID
		existing.actionable = handle.actionable
		target = existing
	}

	// Remove old routes for this root if the root already existed.
	m.removeRoutesLocked(target.rootID)
	for sessionID, rootID := range candidate {
		m.routes[sessionID] = rootID
	}
	target.tree = tree
	if target.mirror == nil {
		target.mirror = newEventMirror(m.opts.EventLimits)
	}
	m.roots[target.socketPath] = target
	return target, nil
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
// index. Must be called with m.mu held.
func (m *Manager) removeRootLocked(socketPath string) {
	handle, ok := m.roots[socketPath]
	if !ok {
		return
	}
	m.removeRoutesLocked(handle.rootID)
	delete(m.roots, socketPath)
	_ = handle.client.Close()
}

// removeRoutesLocked removes all routes for the given rootID. Must be called
// with m.mu held.
func (m *Manager) removeRoutesLocked(rootID string) {
	for sessionID, rid := range m.routes {
		if rid == rootID {
			delete(m.routes, sessionID)
		}
	}
}
