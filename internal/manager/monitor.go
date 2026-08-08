package manager

import (
	"math/rand"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor"
)

const (
	backoffMin = 100 * time.Millisecond
	backoffMax = 5 * time.Second
	jitterPct  = 0.20
)

// monitorRoot launches independent snapshot and event loops for handle.
// Both goroutines are tracked in m.monitorWG.
func (m *Manager) monitorRoot(handle *rootHandle) {
	m.monitorWG.Add(2)
	go func() { defer m.monitorWG.Done(); m.snapshotLoop(handle) }()
	go func() { defer m.monitorWG.Done(); m.eventLoop(handle) }()
}

// snapshotLoop periodically fetches the root snapshot and updates the tree.
func (m *Manager) snapshotLoop(handle *rootHandle) {
	ticker := time.NewTicker(m.opts.SnapshotInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.closeCtx.Done():
			return
		case <-ticker.C:
			snapshot, err := handle.client.Snapshot(m.closeCtx)
			if err != nil {
				m.markStale(handle, true)
				continue
			}
			normalized, candidate, err := validateRootTree(snapshot)
			if err != nil {
				m.markStale(handle, true)
				continue
			}
			if retainable(snapshot) && !admissible(snapshot) {
				m.updateRetainedTree(handle, normalized, false)
				continue
			}
			if err := m.commitTree(handle, normalized, candidate); err != nil {
				m.markStale(handle, true)
				continue
			}
			m.markStale(handle, false)
			m.bumpFleetRevision()
		}
	}
}

// eventLoop subscribes to the root event stream and feeds events into the
// root's mirror. It reconnects with exponential backoff and jitter on failure.
func (m *Manager) eventLoop(handle *rootHandle) {
	backoff := backoffMin
	for {
		if m.closeCtx.Err() != nil {
			return
		}
		sub, err := handle.client.Subscribe(m.closeCtx)
		if err != nil {
			m.setStreamConnected(handle, false)
			m.sleep(backoff)
			backoff = nextBackoff(backoff)
			continue
		}
		m.setStreamConnected(handle, true)
		accepted := m.consumeSubscription(handle, sub)
		if accepted {
			backoff = backoffMin
		} else {
			backoff = nextBackoff(backoff)
		}
		m.setStreamConnected(handle, false)
	}
}

// consumeSubscription drains replay then live events from sub into the mirror.
// Returns true if at least one event was accepted (to reset backoff).
func (m *Manager) consumeSubscription(handle *rootHandle, sub supervisor.Subscription) bool {
	defer sub.Close()
	accepted := false

	// Drain replay first.
	m.mu.Lock()
	for _, event := range sub.Replay {
		if handle.mirror.Accept(event) {
			accepted = true
		}
	}
	m.mu.Unlock()

	// Then live events.
	for {
		select {
		case event, ok := <-sub.Events:
			if !ok {
				return accepted
			}
			m.mu.Lock()
			if handle.mirror.Accept(event) {
				accepted = true
			}
			m.mu.Unlock()
			m.bumpSessionRevision(handle.rootID)
		case <-m.closeCtx.Done():
			return accepted
		}
	}
}

// markStale sets or clears the stale flag for a root handle and notifies.
func (m *Manager) markStale(handle *rootHandle, stale bool) {
	m.mu.Lock()
	if h, ok := m.roots[handle.socketPath]; ok {
		h.stale = stale
	}
	m.mu.Unlock()
	m.bumpFleetRevision()
}

// setStreamConnected updates the StreamConnected field for a root.
func (m *Manager) setStreamConnected(handle *rootHandle, connected bool) {
	m.mu.Lock()
	if h, ok := m.roots[handle.socketPath]; ok {
		h.streamConnected = connected
	}
	m.mu.Unlock()
}

// updateRetainedTree replaces the tree for a stopping/failed root without
// updating routes (since routes have been removed).
func (m *Manager) updateRetainedTree(handle *rootHandle, tree supervisor.NodeSnapshot, actionable bool) {
	m.mu.Lock()
	if h, ok := m.roots[handle.socketPath]; ok {
		h.tree = tree
		h.actionable = actionable
	}
	m.mu.Unlock()
}

// bumpFleetRevision increments the global fleet revision and notifies.
func (m *Manager) bumpFleetRevision() {
	m.mu.Lock()
	m.fleetRevision++
	rev := m.fleetRevision
	m.mu.Unlock()
	m.fleetFanout.Publish(rev)
}

// bumpSessionRevision notifies session subscribers for all sessions in rootID.
func (m *Manager) bumpSessionRevision(rootID string) {
	m.mu.Lock()
	m.sessionRevision++
	rev := m.sessionRevision
	m.mu.Unlock()
	m.sessionFanout.Publish(rev)
	_ = rootID // future: per-session fanout
}

// sleep waits for duration or until closeCtx is cancelled.
func (m *Manager) sleep(d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-m.closeCtx.Done():
	}
}

func nextBackoff(current time.Duration) time.Duration {
	next := current * 2
	if next > backoffMax {
		next = backoffMax
	}
	// Add ±20% jitter.
	jitter := float64(next) * jitterPct * (rand.Float64()*2 - 1)
	next += time.Duration(jitter)
	if next < backoffMin {
		next = backoffMin
	}
	if next > backoffMax {
		next = backoffMax
	}
	return next
}
