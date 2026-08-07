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
	NextMessage(context.Context) (process.ChildMessage, error)
	CloseLiveness() error
	CloseReports() error
	Done() <-chan struct{}
	Wait() error
	Terminate() error
	Kill() error
}

type ChildSpawner func(context.Context, process.Bootstrap) (ChildProcess, error)

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

	cleanupOnce sync.Once
	cleanupDone chan struct{}
	cleanupErr  error
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

func (entry *childEntry) setEventCancel(cancel context.CancelFunc) {
	entry.mu.Lock()
	entry.eventCancel = cancel
	entry.mu.Unlock()
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
	entry.setEventCancel(cancel)
	go node.forwardChildEvents(ctx, cancel, entry)
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
				return
			}
			node.broker.Forward(event)
		}
	}
}

func (node *Node) cleanupChild(ctx context.Context, entry *childEntry, requestStop bool) error {
	entry.cleanupOnce.Do(func() {
		defer close(entry.cleanupDone)
		client, child, cancelEvents := entry.values()
		if child == nil {
			entry.mu.RLock()
			spawnCancel := entry.spawnCancel
			entry.mu.RUnlock()
			if spawnCancel != nil {
				spawnCancel()
			}
			<-entry.spawnDone
			client, child, cancelEvents = entry.values()
		}
		var cleanupErr error
		if child != nil && requestStop {
			if client != nil {
				cleanupErr = errors.Join(cleanupErr, client.Stop(ctx, entry.id))
			} else {
				cleanupErr = errors.Join(cleanupErr, child.CloseLiveness())
			}
		}
		if child != nil {
			select {
			case <-child.Done():
			case <-ctx.Done():
				cleanupErr = errors.Join(cleanupErr, ctx.Err(), child.CloseLiveness())
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
			cleanupErr = errors.Join(cleanupErr, child.Wait(), child.CloseLiveness(), child.CloseReports())
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
			errorsByChild[index] = node.cleanupChild(ctx, entry, true)
		}(index, entry)
	}
	wait.Wait()
	return errors.Join(errorsByChild...)
}
