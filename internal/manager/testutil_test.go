package manager

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

// rootTree builds a minimal root NodeSnapshot for use in tests.
func rootTree(id string, children ...supervisor.NodeSnapshot) supervisor.NodeSnapshot {
	return supervisor.NodeSnapshot{
		SessionID: id, RootSessionID: id, PiSessionID: "pi-" + id,
		SessionFile: "/sessions/" + id + ".jsonl",
		Kind:        contract.ChildKindRoot, Context: contract.ContextRoot,
		Lifecycle: string(supervisor.LifecycleReady), Children: children,
		Questions: []supervisor.QuestionSummary{},
	}
}

// childTree builds a minimal child NodeSnapshot whose RootSessionID is always
// "root" — matches the childTree helper in the brief.
func childTree(id, parent string, children ...supervisor.NodeSnapshot) supervisor.NodeSnapshot {
	return supervisor.NodeSnapshot{
		SessionID: id, ParentSessionID: parent, RootSessionID: "root",
		PiSessionID: "pi-" + id, SessionFile: "/sessions/" + id + ".jsonl",
		Kind: contract.ChildKindRead, Context: contract.ContextFresh,
		WorkerType: "reviewer", Lifecycle: string(supervisor.LifecycleReady),
		Children:  children,
		Questions: []supervisor.QuestionSummary{},
	}
}

// fakeClient is an injected rootClient for testing discovery without real sockets.
type fakeClient struct {
	mu       sync.Mutex
	snapshot supervisor.NodeSnapshot
	err      error
	closed   bool
	callLog  []string
}

func (fc *fakeClient) Snapshot(_ context.Context) (supervisor.NodeSnapshot, error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.callLog = append(fc.callLog, "Snapshot")
	return fc.snapshot, fc.err
}

func (fc *fakeClient) Subscribe(_ context.Context) (supervisor.Subscription, error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.callLog = append(fc.callLog, "Subscribe")
	closed := make(chan supervisor.EventEnvelope)
	close(closed)
	return supervisor.Subscription{
		Replay: []supervisor.EventEnvelope{},
		Events: closed,
		Close:  func() {},
	}, fc.err
}

func (fc *fakeClient) CallRPC(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.callLog = append(fc.callLog, "CallRPC")
	return nil, fc.err
}

func (fc *fakeClient) AnswerQuestion(_ context.Context, _, _ string, _ json.RawMessage) error {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.callLog = append(fc.callLog, "AnswerQuestion")
	return fc.err
}

func (fc *fakeClient) Stop(_ context.Context, _ string) error {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.callLog = append(fc.callLog, "Stop")
	return fc.err
}

func (fc *fakeClient) Close() error {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.callLog = append(fc.callLog, "Close")
	fc.closed = true
	return nil
}

// fakeManager builds a Manager with injected factories for testing.
func fakeManager(factory clientFactory) *Manager {
	return &Manager{
		opts: Options{
			EventLimits: supervisor.EventBrokerOptions{MaxEvents: 100},
			Logger:      discardLogger(),
		},
		roots:   make(map[string]*rootHandle),
		routes:  make(map[string]string),
		factory: factory,
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(nil, &slog.HandlerOptions{Level: slog.LevelError + 100}))
}
