package manager

import (
	"context"
	"errors"
	"math/rand"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor"
)

const (
	backoffMin = 100 * time.Millisecond
	backoffMax = 5 * time.Second
	jitterPct  = 0.20
)

// monitorRoot launches independent snapshot and event loops for handle exactly
// once, serialized against Close so the WaitGroup Add happens-before Wait.
//
// It returns a tri-state monitorResult:
//   - monitorRefusedClosing if the manager is already closing (no loops started;
//     the caller must clean up the orphaned handle);
//   - monitorAlreadyLive if the handle is already being monitored (e.g. a
//     concurrent discovery started it) — the handle is live and MUST be left
//     alone by the caller;
//   - monitorStarted if this call launched the loops.
//
// The whole decision is made under a single m.mu hold so a concurrent caller
// racing to monitor the same handle sees a coherent state and exactly one of
// them wins the start.
func (m *Manager) monitorRoot(handle *rootHandle) monitorResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.monitorRootLocked(handle)
}

// monitorRootLocked is monitorRoot's body; it must be called with m.mu held so
// that the "commit + start monitoring" decision can be folded under a single
// lock hold (spawn.go/discovery.go call it right after commitTree while still
// holding, or via monitorRoot which re-takes, the lock).
func (m *Manager) monitorRootLocked(handle *rootHandle) monitorResult {
	if m.closed || m.quiesced || m.closeCtx.Err() != nil {
		return monitorRefusedClosing
	}
	if handle.monitoring {
		return monitorAlreadyLive
	}
	handle.monitoring = true
	// Give the handle its own cancellation scope, a child of closeCtx so the
	// manager-wide Close still cancels it, but also cancellable individually on
	// removal or client-replace.
	if handle.ctx == nil {
		handle.ctx, handle.cancel = context.WithCancel(m.closeCtx)
	}
	// Add under m.mu so this Add happens-before Close's Wait (which acquires
	// m.mu via closeClients only after the WaitGroup drains). loopsWG mirrors
	// monitorWG but is scoped to this handle so a client-replace can wait for
	// exactly these two loops (MGR-A).
	m.monitorWG.Add(2)
	handle.loopsWG.Add(2)

	go func() { defer m.monitorWG.Done(); defer handle.loopsWG.Done(); m.snapshotLoop(handle) }()
	go func() { defer m.monitorWG.Done(); defer handle.loopsWG.Done(); m.eventLoop(handle) }()
	return monitorStarted
}

// snapshotLoop periodically fetches the root snapshot and updates the tree.
//
// It selects on the handle's own context so a single-root removal or a
// client-replace cancels just this loop (see rootHandle's concurrency
// invariant). The client pointer and context are snapshotted under m.mu at the
// top of each iteration so a concurrent commitTree replacing handle.client
// races neither the read nor a use-after-close: if the handle was cancelled we
// bail before making the call.
func (m *Manager) snapshotLoop(handle *rootHandle) {
	ticker := time.NewTicker(m.opts.SnapshotInterval)
	defer ticker.Stop()
	for {
		hctx := m.handleCtx(handle)
		select {
		case <-hctx.Done():
			return
		case <-m.snapshotCtx.Done():
			return
		case <-ticker.C:
			client, ctx, cancel, ok := m.snapshotClient(handle)
			if !ok {
				return // handle cancelled/replaced or manager quiesced.
			}
			snapshot, err := client.Snapshot(ctx)
			cancel()
			if m.snapshotCtx.Err() != nil {
				return
			}
			if err != nil {
				m.markStale(handle, true)
				continue
			}
			normalized, candidate, err := validateRootTree(snapshot)
			if err != nil {
				m.markStale(handle, true)
				continue
			}
			if retainable(snapshot) {
				m.updateRetainedTree(handle, normalized, false)
				m.markStale(handle, false)
				m.bumpFleetRevision()
				m.bumpSessionRevision()
				continue
			}
			if !admissible(snapshot) {
				// Never make an admitted root actionable from a lifecycle/binding
				// snapshot that would fail initial admission. Retain the last good
				// tree, disable writes through stale, and retry later.
				m.markStale(handle, true)
				continue
			}
			// snapshotLoop always re-commits its OWN handle. If the handle was
			// cancelled (removed/displaced) while this snapshot was in flight,
			// commitTree returns errHandleCancelled — exit quietly instead of
			// marking a root that no longer exists stale (and instead of letting a
			// stale iteration displace whatever replaced us).
			if _, err := m.commitTreeWithActionability(handle, normalized, candidate, true); err != nil {
				if errors.Is(err, errHandleCancelled) || errors.Is(err, errManagerQuiesced) {
					return
				}
				m.markStale(handle, true)
				continue
			}
			m.markStale(handle, false)
			m.bumpFleetRevision()
			m.bumpSessionRevision()
		}
	}
}

// eventLoop subscribes to the root event stream and feeds events into the
// root's mirror. It reconnects with exponential backoff and jitter on failure.
//
// Like snapshotLoop it selects on the handle's own context and snapshots the
// client pointer under m.mu, so a removed/replaced root's loop exits promptly
// instead of spinning forever against a closed client (MGR-B).
func (m *Manager) eventLoop(handle *rootHandle) {
	backoff := backoffMin
	for {
		client, ctx, ok := m.currentClient(handle)
		if !ok {
			return // handle cancelled (removed/replaced); stop the loop.
		}
		sub, err := client.Subscribe(ctx)
		if err != nil {
			m.setStreamConnected(handle, false)
			if !m.sleep(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		m.setStreamConnected(handle, true)
		accepted := m.consumeSubscription(handle, sub)
		m.setStreamConnected(handle, false)

		// A successfully established stream can still close immediately. Apply a
		// bounded delay on every reconnect path so repeated EOFs cannot form a hot
		// accept/close loop.
		delay := backoff
		if accepted {
			backoff = backoffMin
			delay = backoffMin
		} else {
			backoff = nextBackoff(backoff)
		}
		if !m.sleep(ctx, delay) {
			return
		}
	}
}

// currentClient snapshots the handle's client pointer and monitor context under
// m.mu. It returns ok=false if the handle's loops have been cancelled (removed
// or client-replaced), signalling the loop to exit. Reading handle.client only
// here — under the lock — is what makes the concurrent commitTree replace safe
// (MGR-A: no data race, no use-after-close, because a replaced client is closed
// only after the loop has been cancelled and observed the cancellation).
func (m *Manager) currentClient(handle *rootHandle) (rootClient, context.Context, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx := handle.ctx
	if ctx == nil {
		ctx = m.closeCtx
	}
	if ctx.Err() != nil {
		return nil, nil, false
	}
	return handle.client, ctx, true
}

// snapshotClient returns a client with a call context canceled by either the
// per-handle lifetime or Quiesce. Event subscriptions intentionally continue to
// use only the handle lifetime until Close.
func (m *Manager) snapshotClient(handle *rootHandle) (rootClient, context.Context, context.CancelFunc, bool) {
	client, handleCtx, ok := m.currentClient(handle)
	if !ok || m.snapshotCtx.Err() != nil {
		return nil, nil, nil, false
	}
	ctx, cancel := context.WithCancel(handleCtx)
	stop := context.AfterFunc(m.snapshotCtx, cancel)
	return client, ctx, func() {
		stop()
		cancel()
	}, true
}

// handleCtx returns the handle's monitor context (or closeCtx as a fallback),
// under m.mu.
func (m *Manager) handleCtx(handle *rootHandle) context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if handle.ctx != nil {
		return handle.ctx
	}
	return m.closeCtx
}

// consumeSubscription drains replay then live events from sub into the mirror.
// Returns true if at least one event was accepted (to reset backoff).
func (m *Manager) consumeSubscription(handle *rootHandle, sub supervisor.Subscription) bool {
	defer sub.Close()
	accepted := false
	// The live-event select below waits on the handle's own context so a
	// per-root removal/replace exits promptly, not just a manager-wide Close.
	hctx := m.handleCtx(handle)

	// Drain replay first.
	m.mu.Lock()
	replayAccepted := false
	for _, event := range sub.Replay {
		if handle.mirror.Accept(event) {
			accepted = true
			replayAccepted = true
		}
	}
	m.mu.Unlock()
	// Notify session subscribers once after the replay drain so a reconnect
	// refreshes the activity tail without waiting for the next live event.
	if replayAccepted {
		m.bumpSessionRevision()
	}

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
			m.bumpSessionRevision()
		case <-hctx.Done():
			return accepted
		}
	}
}

// markStale sets or clears the stale flag for a root handle and notifies.
func (m *Manager) markStale(handle *rootHandle, stale bool) {
	m.mu.Lock()
	changed := false
	if h, ok := m.roots[handle.socketPath]; ok && h == handle && h.stale != stale {
		h.stale = stale
		changed = true
	}
	m.mu.Unlock()
	if changed {
		m.bumpFleetRevision()
		m.bumpSessionRevision()
	}
}

// setStreamConnected updates the StreamConnected field for a root.
func (m *Manager) setStreamConnected(handle *rootHandle, connected bool) {
	m.mu.Lock()
	changed := false
	if h, ok := m.roots[handle.socketPath]; ok && h == handle && h.streamConnected != connected {
		h.streamConnected = connected
		changed = true
	}
	m.mu.Unlock()
	if changed {
		m.bumpFleetRevision()
	}
}

// updateRetainedTree replaces the tree for a stopping/failed root without
// updating routes (since routes have been removed).
func (m *Manager) updateRetainedTree(handle *rootHandle, tree supervisor.NodeSnapshot, actionable bool) {
	m.mu.Lock()
	if h, ok := m.roots[handle.socketPath]; ok && h == handle {
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

// bumpSessionRevision notifies the global v1 session fanout. Per-root fanout is
// a scalability follow-up; callers must not read mutable handle fields merely to
// publish this global revision.
func (m *Manager) bumpSessionRevision() {
	m.mu.Lock()
	m.sessionRevision++
	rev := m.sessionRevision
	m.mu.Unlock()
	m.sessionFanout.Publish(rev)
}

// sleep waits for duration or until ctx is cancelled. It returns true if the
// full duration elapsed and false if ctx was cancelled first, so callers can
// abort their loop promptly on removal/replace/close.
func (m *Manager) sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
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
