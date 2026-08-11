package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/process"
	"github.com/sklarsa/kanedias/internal/supervisor/provision"
)

// DescendantClient is the acyclic parent-to-child control seam. Implementations
// live outside this package (supervisorapi is the production implementation).
type DescendantClient interface {
	Snapshot(context.Context) (NodeSnapshot, error)
	Subscribe(context.Context) (Subscription, error)
	CallRPC(context.Context, string, json.RawMessage) (json.RawMessage, error)
	AnswerQuestion(context.Context, string, string, json.RawMessage) error
	Stop(context.Context, string) error
}

type DescendantClientFactory func(socketPath string) (DescendantClient, error)

type descendantCloser interface{ Close() error }

type ChildProcess interface {
	WaitReady(context.Context) error
	RecoveryTicket() (provision.RecoveryTicket, bool)
	NextMessage(context.Context) (process.ChildMessage, error)
	AcknowledgeTerminal(process.ChildMessage) error
	CloseTerminalAck() error
	CloseLiveness() error
	CloseReports() error
	Done() <-chan struct{}
	Wait() error
	Terminate() error
	Kill() error
}

type ChildSpawner func(context.Context, process.Bootstrap) (ChildProcess, error)

// childDisposition is the single linearization point between terminal acceptance
// and an external direct-child cancellation. Exactly one side wins; later claims
// from the losing side are rejected, so a race can never both acknowledge a
// terminal report and cancel a child.
type childDisposition uint8

const (
	childUndecided childDisposition = iota
	childAccepted                   // terminal acceptance linearized first
	childCancelled                  // external cancellation linearized first
)

// childCleanupMode selects how a direct child's cleanup signals its stop.
type childCleanupMode uint8

const (
	childCleanupComplete  childCleanupMode = iota // terminal acceptance: no stop signaled
	childCleanupForced                            // generic startup/runtime failure: route descendant stop
	childCleanupCancelled                         // external cancellation: ack + liveness, no HTTP stop
)

type childEntry struct {
	id       string
	socket   string
	fallback NodeSnapshot

	mu          sync.RWMutex
	client      DescendantClient
	process     ChildProcess
	spawnCancel context.CancelFunc
	spawnDone   chan struct{}
	spawnOnce   sync.Once
	eventCancel context.CancelFunc
	eventDone   chan struct{}
	recovery    *provision.RecoveryTicket
	disposition childDisposition

	cleanupOnce        sync.Once
	cleanupDone        chan struct{}
	cleanupErr         error
	streamErr          error
	eventCloseExpected bool
}

func (entry *childEntry) init() {
	if entry.cleanupDone == nil {
		entry.cleanupDone = make(chan struct{})
	}
	if entry.spawnDone == nil {
		entry.spawnDone = make(chan struct{})
	}
}

func (entry *childEntry) setProcess(child ChildProcess) {
	entry.mu.Lock()
	entry.process = child
	entry.mu.Unlock()
	entry.markSpawnDone()
}

func (entry *childEntry) markSpawnDone() {
	entry.spawnOnce.Do(func() { close(entry.spawnDone) })
}

func (entry *childEntry) setRecovery(ticket provision.RecoveryTicket) {
	entry.mu.Lock()
	entry.recovery = &ticket
	entry.mu.Unlock()
}

func (entry *childEntry) setClient(client DescendantClient) {
	entry.mu.Lock()
	entry.client = client
	entry.mu.Unlock()
}

func (entry *childEntry) values() (DescendantClient, ChildProcess, context.CancelFunc) {
	entry.mu.RLock()
	defer entry.mu.RUnlock()
	return entry.client, entry.process, entry.eventCancel
}

func (entry *childEntry) setEventForwarder(cancel context.CancelFunc, done chan struct{}) {
	entry.mu.Lock()
	entry.eventCancel = cancel
	entry.eventDone = done
	entry.mu.Unlock()
}

func (entry *childEntry) eventForwarder() (context.CancelFunc, <-chan struct{}) {
	entry.mu.RLock()
	defer entry.mu.RUnlock()
	return entry.eventCancel, entry.eventDone
}

func (entry *childEntry) setStreamError(err error) {
	entry.mu.Lock()
	entry.streamErr = err
	entry.mu.Unlock()
}

// claimAccepted claims the terminal-acceptance disposition, marking descendant SSE
// closure expected is deferred to the caller only after a successful claim. It
// returns true only for the first (winning) claim; once cancellation has already
// won, a terminal report must never be acknowledged.
func (entry *childEntry) claimAccepted() bool {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.disposition != childUndecided {
		return false
	}
	entry.disposition = childAccepted
	return true
}

// claimCancelled claims the external-cancellation disposition. It returns true
// only for the first (winning) claim; after terminal acceptance has already won,
// an external cancellation is benign and still bounds its own cleanup.
func (entry *childEntry) claimCancelled() bool {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.disposition != childUndecided {
		return false
	}
	entry.disposition = childCancelled
	return true
}

func (entry *childEntry) isCancelled() bool {
	entry.mu.RLock()
	defer entry.mu.RUnlock()
	return entry.disposition == childCancelled
}

func (entry *childEntry) isAccepted() bool {
	entry.mu.RLock()
	defer entry.mu.RUnlock()
	return entry.disposition == childAccepted
}

func (entry *childEntry) markEventStreamCloseExpected() {
	entry.mu.Lock()
	entry.eventCloseExpected = true
	entry.mu.Unlock()
}

func (entry *childEntry) cancelExpectedEventStream() {
	entry.mu.Lock()
	entry.eventCloseExpected = true
	cancel := entry.eventCancel
	entry.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (entry *childEntry) eventStreamCloseIsExpected() bool {
	entry.mu.RLock()
	defer entry.mu.RUnlock()
	return entry.eventCloseExpected
}

type childRegistry struct {
	mu      sync.RWMutex
	entries map[string]*childEntry
}

func newChildRegistry() *childRegistry { return &childRegistry{entries: make(map[string]*childEntry)} }

func (registry *childRegistry) add(entry *childEntry) error {
	entry.init()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.entries[entry.id]; exists {
		return invariantf("duplicate direct child %q", entry.id)
	}
	registry.entries[entry.id] = entry
	return nil
}

func (registry *childRegistry) remove(id string, expected *childEntry) {
	registry.mu.Lock()
	if registry.entries[id] == expected {
		delete(registry.entries, id)
	}
	registry.mu.Unlock()
}

func (registry *childRegistry) get(id string) *childEntry {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	return registry.entries[id]
}

func (registry *childRegistry) snapshot() []*childEntry {
	registry.mu.RLock()
	entries := make([]*childEntry, 0, len(registry.entries))
	for _, entry := range registry.entries {
		entries = append(entries, entry)
	}
	registry.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })
	return entries
}

func childUnavailable(id string, err error) error {
	message := fmt.Sprintf("child session %q is unavailable", id)
	if err != nil {
		message += ": " + err.Error()
	}
	return errors.Join(contract.NewError(contract.ErrorChildUnavailable, message), err)
}

func (node *Node) startChildEventForwarder(entry *childEntry) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	entry.setEventForwarder(cancel, done)
	go func() {
		defer close(done)
		node.forwardChildEvents(ctx, cancel, entry)
	}()
}

func (node *Node) forwardChildEvents(ctx context.Context, cancel context.CancelFunc, entry *childEntry) {
	client, _, _ := entry.values()
	if client == nil {
		cancel()
		return
	}
	subscription, err := client.Subscribe(ctx)
	if err != nil {
		cancel()
		return
	}
	defer cancel()
	if subscription.Close != nil {
		defer subscription.Close()
	}
	for _, event := range subscription.Replay {
		node.broker.Forward(event)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-subscription.Events:
			if !ok {
				if entry.eventStreamCloseIsExpected() || ctx.Err() != nil {
					return
				}
				var streamErr error
				if subscription.Err != nil {
					streamErr = subscription.Err()
				}
				if streamErr != nil && !entry.eventStreamCloseIsExpected() && ctx.Err() == nil {
					entry.setStreamError(streamErr)
					_, child, _ := entry.values()
					if child != nil {
						_ = child.CloseLiveness()
					}
				}
				return
			}
			node.broker.Forward(event)
		}
	}
}

func (node *Node) cleanupChild(ctx context.Context, entry *childEntry, requestStop bool) error {
	if requestStop {
		return node.cleanupChildMode(ctx, entry, childCleanupForced)
	}
	return node.cleanupChildMode(ctx, entry, childCleanupComplete)
}

// cancelChildExternal is the parent-owned cancellation path for a directly-owned
// child. It linearizes external cancellation ahead of any terminal acceptance,
// closes the terminal-ack endpoint without acknowledging and the inherited
// parent-liveness endpoint (the canonical cancellation signal that recursively
// stops the child subtree), then bounds process wait/escalation/recovery/removal.
// It never routes a descendant HTTP stop before the liveness EOF and never turns
// the expected closed child server into a cleanup failure.
func (node *Node) cancelChildExternal(ctx context.Context, entry *childEntry) error {
	entry.claimCancelled()
	return node.cleanupChildMode(ctx, entry, childCleanupCancelled)
}

// awaitChildCancellation waits for the one bounded cleanup already running for an
// externally cancelled child and reports exact child_aborted to the blocked
// CreateChild caller. It never acknowledges a terminal report.
func (node *Node) awaitChildCancellation(ctx context.Context, entry *childEntry) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), node.childStopTimeout())
	defer cancel()
	select {
	case <-entry.cleanupDone:
	case <-cleanupCtx.Done():
	}
	return contract.NewError(contract.ErrorChildAborted, "child was stopped")
}

func (node *Node) cleanupChildMode(ctx context.Context, entry *childEntry, mode childCleanupMode) error {
	entry.cleanupOnce.Do(func() {
		defer close(entry.cleanupDone)
		client, child, cancelEvents := entry.values()
		_, forwarderDone := entry.eventForwarder()
		if child == nil {
			entry.mu.RLock()
			spawnCancel := entry.spawnCancel
			entry.mu.RUnlock()
			if spawnCancel != nil {
				spawnCancel()
			}
			<-entry.spawnDone
			client, child, cancelEvents = entry.values()
			_, forwarderDone = entry.eventForwarder()
		}
		var cleanupErr error
		if child != nil {
			switch mode {
			case childCleanupForced:
				// A forced/cancelled stop is not a terminal acceptance. Closing the
				// private ack endpoint unblocks any terminal reporter without granting
				// acknowledgement, then normal stop cascades through the child subtree.
				cleanupErr = errors.Join(cleanupErr, child.CloseTerminalAck())
				// Mark and cancel event ownership before any intentional child stop.
				// Its server may close SSE as part of Stop before the process/report
				// paths have otherwise had a chance to classify that EOF.
				entry.cancelExpectedEventStream()
				if client != nil {
					cleanupErr = errors.Join(cleanupErr, client.Stop(ctx, entry.id))
				} else {
					cleanupErr = errors.Join(cleanupErr, child.CloseLiveness())
				}
			case childCleanupCancelled:
				// External cancellation already claimed the disposition in
				// cancelChildExternal. Close the ack endpoint without acknowledging
				// and close the inherited parent-liveness endpoint as the canonical
				// cancellation signal. Do not wait for a descendant HTTP stop before
				// the liveness EOF.
				cleanupErr = errors.Join(cleanupErr, child.CloseTerminalAck())
				entry.cancelExpectedEventStream()
				cleanupErr = errors.Join(cleanupErr, child.CloseLiveness())
			}
		}
		if child != nil {
			select {
			case <-child.Done():
			case <-ctx.Done():
				cleanupErr = errors.Join(cleanupErr, ctx.Err(), child.CloseTerminalAck(), child.CloseLiveness())
				grace := time.NewTimer(node.childEscalationGrace())
				select {
				case <-child.Done():
					grace.Stop()
				case <-grace.C:
					cleanupErr = errors.Join(cleanupErr, child.Terminate())
					killTimer := time.NewTimer(node.childEscalationGrace())
					select {
					case <-child.Done():
						killTimer.Stop()
					case <-killTimer.C:
						cleanupErr = errors.Join(cleanupErr, child.Kill())
					}
				}
			}
			cleanupErr = errors.Join(cleanupErr, child.Wait(), child.CloseTerminalAck(), child.CloseLiveness(), child.CloseReports())
			if mode == childCleanupComplete && forwarderDone != nil {
				select {
				case <-forwarderDone:
				default:
					select {
					case <-forwarderDone:
					case <-ctx.Done():
						cleanupErr = errors.Join(cleanupErr, ctx.Err())
						if cancelEvents != nil {
							cancelEvents()
						}
					}
				}
			}
			entry.mu.RLock()
			ticket := entry.recovery
			entry.mu.RUnlock()
			if ticket != nil && node.deps.DirectChildRecoverer != nil {
				recoveryCtx, cancelRecovery := context.WithTimeout(context.Background(), node.childStopTimeout())
				cleanupErr = errors.Join(cleanupErr, node.deps.DirectChildRecoverer.RecoverDirectChild(recoveryCtx, *ticket))
				cancelRecovery()
			}
		}
		entry.mu.RLock()
		spawnCancel := entry.spawnCancel
		entry.mu.RUnlock()
		if spawnCancel != nil {
			spawnCancel()
		}
		if cancelEvents != nil {
			cancelEvents()
		}
		if closer, ok := client.(descendantCloser); ok {
			cleanupErr = errors.Join(cleanupErr, closer.Close())
		}
		entry.mu.RLock()
		streamErr := entry.streamErr
		entry.mu.RUnlock()
		cleanupErr = errors.Join(cleanupErr, streamErr)
		node.children.remove(entry.id, entry)
		entry.cleanupErr = cleanupErr
	})
	select {
	case <-entry.cleanupDone:
		return entry.cleanupErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (node *Node) stopChildren(ctx context.Context) error {
	entries := node.children.snapshot()
	var wait sync.WaitGroup
	errorsByChild := make([]error, len(entries))
	for index, entry := range entries {
		wait.Add(1)
		go func(index int, entry *childEntry) {
			defer wait.Done()
			errorsByChild[index] = node.cancelChildExternal(ctx, entry)
		}(index, entry)
	}
	wait.Wait()
	return errors.Join(errorsByChild...)
}
