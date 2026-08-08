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
	socketPath string
	rootID     string
	identity   socketIdentity
	tree       supervisor.NodeSnapshot
	// actionable is false for stopping/failed/stopped/completed snapshots.
	actionable bool
	client     rootClient
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
	if err := m.commitTree(handle, normalized, candidate); err != nil {
		_ = client.Close()
		*issues = append(*issues, DiscoveryIssue{
			SocketName: name, Code: "route_conflict", Message: err.Error(),
		})
		return
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
	if err := m.commitTree(handle, normalized, candidate); err != nil {
		_ = client.Close()
		*issues = append(*issues, DiscoveryIssue{
			SocketName: name, Code: "route_conflict", Message: err.Error(),
		})
	}
}

// commitTree atomically commits a validated root tree and its routes, or
// nothing on conflict. It replaces routes from the previous rootID if the
// handle is already known.
func (m *Manager) commitTree(handle *rootHandle, tree supervisor.NodeSnapshot, candidate map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for sessionID, rootID := range candidate {
		if existing, ok := m.routes[sessionID]; ok && existing != rootID {
			return fmt.Errorf("route conflict for session %q (owned by %q, new root %q)", sessionID, existing, rootID)
		}
	}
	// Remove old routes for this root if the root already existed.
	m.removeRoutesLocked(handle.rootID)
	for sessionID, rootID := range candidate {
		m.routes[sessionID] = rootID
	}
	handle.tree = tree
	m.roots[handle.socketPath] = handle
	return nil
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
