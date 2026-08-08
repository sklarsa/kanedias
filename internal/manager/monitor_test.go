package manager

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor"
)

// subscriptionFromReplay returns a closed Subscription pre-loaded with replay events.
func subscriptionFromReplay(replay []supervisor.EventEnvelope) supervisor.Subscription {
	closed := make(chan supervisor.EventEnvelope)
	close(closed)
	return supervisor.Subscription{
		Replay: replay,
		Events: closed,
		Close:  func() {},
		Err:    func() error { return nil },
	}
}

// subscriptionWithLiveEvents returns a Subscription that delivers events from
// a channel and then closes.
func subscriptionWithLiveEvents(events []supervisor.EventEnvelope, closeErr error) supervisor.Subscription {
	ch := make(chan supervisor.EventEnvelope, len(events)+1)
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return supervisor.Subscription{
		Replay: []supervisor.EventEnvelope{},
		Events: ch,
		Close:  func() {},
		Err:    func() error { return closeErr },
	}
}

// ---- Mirror integration via consumeSubscription ----

func TestConsumeSubscriptionAcceptsReplayBeforeLiveEvents(t *testing.T) {
	m := fakeManager(nil)
	handle := &rootHandle{
		socketPath: "/tmp/test.root.sock",
		rootID:     "root",
		mirror:     newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 100}),
	}

	replayEvent := supervisor.EventEnvelope{
		Seq: 1, SessionID: "root", SourceSeq: 1, Kind: "pi",
		Payload: json.RawMessage(`{"type":"agent_start"}`),
	}
	liveEvent := supervisor.EventEnvelope{
		Seq: 2, SessionID: "root", SourceSeq: 2, Kind: "pi",
		Payload: json.RawMessage(`{"type":"agent_settled"}`),
	}

	ch := make(chan supervisor.EventEnvelope, 1)
	ch <- liveEvent
	close(ch)
	sub := supervisor.Subscription{
		Replay: []supervisor.EventEnvelope{replayEvent},
		Events: ch,
		Close:  func() {},
		Err:    func() error { return nil },
	}
	m.consumeSubscription(handle, sub)

	events := handle.mirror.Events()
	if len(events) != 2 {
		t.Fatalf("expected 2 events in mirror, got %d", len(events))
	}
	if events[0].Seq != 1 || events[1].Seq != 2 {
		t.Fatalf("event order wrong: %v %v", events[0].Seq, events[1].Seq)
	}
}

func TestConsumeSubscriptionDeduplicatesReconnectReplay(t *testing.T) {
	m := fakeManager(nil)
	handle := &rootHandle{
		socketPath: "/tmp/test2.root.sock",
		rootID:     "root2",
		mirror:     newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 100}),
	}
	// Pre-populate mirror with seq 1-2.
	handle.mirror.Accept(envelope(1, "root2"))
	handle.mirror.Accept(envelope(2, "root2"))

	// Reconnect replay overlaps: starts from seq 2.
	sub := subscriptionFromReplay([]supervisor.EventEnvelope{
		envelope(2, "root2"), // duplicate
		envelope(3, "root2"), // new
	})
	m.consumeSubscription(handle, sub)

	events := handle.mirror.Events()
	if len(events) != 3 {
		t.Fatalf("expected 3 events after reconnect, got %d", len(events))
	}
}

func TestConsumeSubscriptionEOFDoesNotLossPreviousEvents(t *testing.T) {
	m := fakeManager(nil)
	handle := &rootHandle{
		socketPath: "/tmp/eof.root.sock",
		rootID:     "eof-root",
		mirror:     newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 100}),
	}
	handle.mirror.Accept(envelope(1, "eof-root"))

	sub := subscriptionWithLiveEvents(nil, errors.New("EOF"))
	m.consumeSubscription(handle, sub)

	if len(handle.mirror.Events()) != 1 {
		t.Fatalf("lost events on EOF")
	}
}

// ---- ManagerStart / ManagerClose tests ----

func TestManagerStartAndClose(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	logDir := t.TempDir()
	if err := os.Chmod(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{
		RootSocketDir:     dir,
		SessionLogDir:     logDir,
		EventLimits:       supervisor.EventBrokerOptions{MaxEvents: 100},
		Logger:            discardLogger(),
		DiscoveryInterval: 100 * time.Millisecond,
		SnapshotInterval:  100 * time.Millisecond,
		SpawnTimeout:      5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := m.Close(closeCtx); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
}

func TestManagerCloseDoesNotStopAdmittedRoots(t *testing.T) {
	client := &fakeClient{snapshot: rootTree("root")}
	m := fakeManager(func(_ string) (rootClient, error) { return client, nil })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	// Stop must never have been called.
	client.mu.Lock()
	defer client.mu.Unlock()
	for _, call := range client.callLog {
		if call == "Stop" {
			t.Fatal("Close() called Stop on admitted root — must not")
		}
	}
}

func TestManagerFleetFanoutPublishesOnDiscovery(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	makeRootSocket(t, dir, "fan.root.sock")
	tree := rootTree("fan")
	client := &fakeClient{snapshot: tree}
	m := fakeManager(func(_ string) (rootClient, error) { return client, nil })
	m.opts.RootSocketDir = dir

	sub := m.SubscribeFleet()
	m.discoverOnce(context.Background())
	m.bumpFleetRevision()

	select {
	case rev := <-sub.Updates:
		if rev == 0 {
			t.Fatal("received zero revision")
		}
	case <-time.After(time.Second):
		t.Fatal("fleet fanout did not publish after discovery")
	}
	sub.Close()
}
