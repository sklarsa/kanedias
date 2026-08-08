package manager

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/sklarsa/kanedias/internal/supervisor"
)

// errNotImplemented marks interface-locked stubs replaced by the manager
// implementation lane; it never ships once the lane merges.
var errNotImplemented = errors.New("manager: not implemented")

// Manager is the root-only control plane between the recursive supervisor
// API and the web server. This skeleton locks the public interface; the
// manager lane replaces the stubbed behavior.
type Manager struct {
	mu     sync.Mutex
	opts   Options
	closed bool
}

// New normalizes and validates options. The skeleton accepts any options;
// the manager lane adds path, identity, and binary validation.
func New(opts Options) (*Manager, error) {
	if opts.EventLimits.MaxEvents <= 0 && opts.EventLimits.MaxBytes <= 0 {
		return nil, errors.New("manager: event limits require at least one positive bound")
	}
	return &Manager{opts: opts}, nil
}

// Start performs one discovery pass and launches monitor loops.
func (m *Manager) Start(context.Context) error { return nil }

// Fleet returns the current fleet projection.
func (m *Manager) Fleet() FleetSnapshot { return FleetSnapshot{} }

// Session returns the projection for one session in an admitted root tree.
func (m *Manager) Session(sessionID string) (SessionState, error) {
	return SessionState{}, errNotImplemented
}

// SubscribeFleet registers a bounded fleet change subscriber.
func (m *Manager) SubscribeFleet() ChangeSubscription {
	updates := make(chan uint64)
	close(updates)
	return ChangeSubscription{Updates: updates, Close: func() {}}
}

// SubscribeSession registers a bounded subscriber for one session.
func (m *Manager) SubscribeSession(sessionID string) (ChangeSubscription, error) {
	return ChangeSubscription{}, errNotImplemented
}

// SpawnRoot launches a detached root supervisor and admits it.
func (m *Manager) SpawnRoot(ctx context.Context) (string, error) {
	return "", errNotImplemented
}

// Steer sends a streaming steer or an idle prompt to a session.
func (m *Manager) Steer(ctx context.Context, sessionID string, message string) error {
	return errNotImplemented
}

// Interrupt aborts the current turn of a session.
func (m *Manager) Interrupt(ctx context.Context, sessionID string) error {
	return errNotImplemented
}

// StopSession stops one session subtree through its owning root.
func (m *Manager) StopSession(ctx context.Context, sessionID string) error {
	return errNotImplemented
}

// AnswerQuestion forwards a raw answer to one pending question.
func (m *Manager) AnswerQuestion(ctx context.Context, sessionID string, questionID string, answer json.RawMessage) error {
	return errNotImplemented
}

// SessionStats returns typed Pi metrics for one actionable session.
func (m *Manager) SessionStats(ctx context.Context, sessionID string) (SessionStats, error) {
	return SessionStats{}, errNotImplemented
}

// Quiesce rejects new writes and stops discovery/polling while event drains
// continue until Close.
func (m *Manager) Quiesce(context.Context) error { return nil }

// Close cancels subscriptions, waits for manager goroutines, and closes
// clients without stopping admitted roots.
func (m *Manager) Close(context.Context) error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	return nil
}

// ensure the skeleton keeps referenced imports alive across lane merges.
var (
	_ = supervisor.NewEventBroker
	_ clientFactory
)
