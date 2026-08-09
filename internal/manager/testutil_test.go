package manager

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/config"
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
	mu        sync.Mutex
	snapshot  supervisor.NodeSnapshot
	err       error
	closed    bool
	callLog   []string
	closeChan chan struct{} // closed by Close() to unblock a parked Subscribe
}

func (fc *fakeClient) Snapshot(_ context.Context) (supervisor.NodeSnapshot, error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.callLog = append(fc.callLog, "Snapshot")
	return fc.snapshot, fc.err
}

// callCount returns how many times the named method was invoked.
func (fc *fakeClient) callCount(name string) int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	n := 0
	for _, c := range fc.callLog {
		if c == name {
			n++
		}
	}
	return n
}

func (fc *fakeClient) Subscribe(_ context.Context) (supervisor.Subscription, error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.callLog = append(fc.callLog, "Subscribe")
	if fc.err != nil {
		return supervisor.Subscription{}, fc.err
	}
	if fc.closeChan == nil {
		fc.closeChan = make(chan struct{})
	}
	closeChan := fc.closeChan
	// Events stays open until the client is closed, so eventLoop parks on a
	// select instead of spinning in a tight reconnect loop during tests.
	events := make(chan supervisor.EventEnvelope)
	go func() {
		<-closeChan
		close(events)
	}()
	return supervisor.Subscription{
		Replay: []supervisor.EventEnvelope{},
		Events: events,
		Close:  func() {},
		Err:    func() error { return nil },
	}, nil
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
	if !fc.closed && fc.closeChan != nil {
		close(fc.closeChan)
	}
	fc.closed = true
	return nil
}

// fakeManager builds a Manager with injected factories for testing.
func fakeManager(factory clientFactory) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	snapshotCtx, snapshotCancel := context.WithCancel(ctx)
	return &Manager{
		opts: Options{
			EventLimits:      supervisor.EventBrokerOptions{MaxEvents: 100},
			Logger:           discardLogger(),
			SnapshotInterval: time.Hour, // long: monitoring loops must not fire during unit tests
		},
		launch:                 managerTestLaunch(),
		roots:                  make(map[string]*rootHandle),
		routes:                 make(map[string]string),
		factory:                factory,
		newSpawnToken:          generateToken,
		newBootstrapPipe:       os.Pipe,
		writeRootBootstrap:     writeRootBootstrap,
		waitRootBootstrapWrite: waitRootBootstrapWrite,
		closeCtx:               ctx,
		closeCancel:            cancel,
		snapshotCtx:            snapshotCtx,
		snapshotCancel:         snapshotCancel,
		fleetFanout:            newChangeFanout(4),
		sessionFanout:          newChangeFanout(4),
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// modelConfigFixture builds a valid model/worker/session config used by launch
// and manager tests.
func modelConfigFixture() config.Config {
	return config.Config{
		BaseImage: config.BaseImage{Name: "sandbox", Source: "https://images.linuxcontainers.org", Image: "debian/13"},
		Models: map[string]config.ModelDefinition{
			"local-qwen": {
				Label: "Local Qwen", Provider: "local-executor", Model: "Qwen3.6-27B-GGUF",
				ThinkingLevels: []string{"off"}, DefaultThinkingLevel: "off",
			},
			"gpt-5-6-sol": {
				Label: "GPT-5.6 Solver", Provider: "openai-codex", Model: "gpt-5.6-sol",
				ThinkingLevels:       []string{"minimal", "low", "medium", "high", "xhigh", "max"},
				DefaultThinkingLevel: "high",
			},
		},
		Session: config.SessionDefaults{ModelType: "local-qwen", ThinkingLevel: "off"},
		Workers: map[string]config.WorkerDefaults{
			"reviewer": {Description: "Review code and designs without modifying files.", ModelType: "gpt-5-6-sol", ThinkingLevel: "xhigh"},
			"worker":   {Description: "Implement changes and hand off pushed Git refs.", ModelType: "gpt-5-6-sol", ThinkingLevel: "high"},
		},
	}
}

// managerTestLaunch builds the launch catalog used by manager fakes and tests.
// It must not fail for the valid fixture.
func managerTestLaunch() LaunchConfiguration {
	lc, err := NewLaunchConfiguration(modelConfigFixture())
	if err != nil {
		panic(err)
	}
	return lc
}

// shortTempDirs creates two short-path temporary directories suitable for
// use as RootSocketDir and SessionLogDir in spawn tests. Unix socket paths
// are limited to 107 bytes on Linux, so we use /tmp directly.
func shortTempDirs(t *testing.T) (rootDir, logDir string) {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "kmgr-")
	if err != nil {
		t.Fatalf("shortTempDirs MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	rootDir = filepath.Join(base, "r")
	logDir = filepath.Join(base, "l")
	for _, d := range []string{rootDir, logDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatalf("shortTempDirs Mkdir: %v", err)
		}
	}
	return rootDir, logDir
}
