package manager

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
type contextBlockingClient struct {
	*fakeClient
	entered chan struct{}
	exited  chan struct{}
}

func (client *contextBlockingClient) Snapshot(ctx context.Context) (supervisor.NodeSnapshot, error) {
	close(client.entered)
	defer close(client.exited)
	<-ctx.Done()
	return supervisor.NodeSnapshot{}, ctx.Err()
}

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
	if gap := handle.mirror.Gap(); gap != nil {
		t.Fatalf("retained overlap recorded false replay gap: %#v", gap)
	}
}

func TestConsumeSubscriptionRecordsReconnectReplayGap(t *testing.T) {
	m := fakeManager(nil)
	handle := &rootHandle{
		socketPath: "/tmp/test-gap.root.sock",
		rootID:     "root-gap",
		mirror:     newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 100}),
	}
	handle.mirror.Accept(envelope(1, "root-gap"))
	handle.mirror.Accept(envelope(2, "root-gap"))

	sub := subscriptionFromReplay([]supervisor.EventEnvelope{
		envelope(4, "root-gap"),
		envelope(5, "root-gap"),
	})
	m.consumeSubscription(handle, sub)

	gap := handle.mirror.Gap()
	if gap == nil || gap.ExpectedSeq != 3 || gap.FirstAvailableSeq != 4 {
		t.Fatalf("reconnect replay gap = %#v, want ExpectedSeq 3 and FirstAvailableSeq 4", gap)
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

// TestConsumeSubscriptionNotifiesAfterReplayDrain is the M3 regression test:
// after draining a non-empty replay batch, session subscribers get one notify so
// a reconnect refreshes the activity tail without waiting for a live event.
func TestConsumeSubscriptionNotifiesAfterReplayDrain(t *testing.T) {
	m := fakeManager(nil)
	handle := &rootHandle{
		socketPath: "/tmp/replay-notify.root.sock",
		rootID:     "replay-root",
		mirror:     newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 100}),
	}

	subscription := m.sessionFanout.Subscribe()
	defer subscription.Close()

	replay := subscriptionFromReplay([]supervisor.EventEnvelope{
		envelope(1, "replay-root"),
		envelope(2, "replay-root"),
	})
	m.consumeSubscription(handle, replay)

	select {
	case rev := <-subscription.Updates:
		if rev == 0 {
			t.Fatal("received zero session revision after replay drain")
		}
	case <-time.After(time.Second):
		t.Fatal("no session notify after replay drain")
	}
}

// TestConsumeSubscriptionNoNotifyOnEmptyReplay ensures the M3 notify is only
// issued when the replay actually delivered new events.
func TestConsumeSubscriptionNoNotifyOnEmptyReplay(t *testing.T) {
	m := fakeManager(nil)
	handle := &rootHandle{
		socketPath: "/tmp/replay-empty.root.sock",
		rootID:     "empty-root",
		mirror:     newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 100}),
	}
	subscription := m.sessionFanout.Subscribe()
	defer subscription.Close()

	m.consumeSubscription(handle, subscriptionFromReplay(nil))

	select {
	case <-subscription.Updates:
		t.Fatal("unexpected session notify on empty replay drain")
	case <-time.After(100 * time.Millisecond):
		// expected: no notify
	}
}

// TestStoppingTransitionNotifiesFleet is the M1 regression test: when a monitored
// root transitions to a retained (stopping/failed) lifecycle, snapshotLoop must
// bump the fleet revision so the UI refreshes the non-actionable transition.
func TestStoppingTransitionNotifiesFleet(t *testing.T) {
	stopping := rootTree("stop")
	stopping.Lifecycle = string(supervisor.LifecycleStopping)
	client := &fakeClient{snapshot: stopping}

	m := fakeManager(func(_ string) (rootClient, error) { return client, nil })
	m.opts.SnapshotInterval = 10 * time.Millisecond

	handle := &rootHandle{
		socketPath: "/tmp/stopping.root.sock",
		rootID:     "stop",
		actionable: true,
		client:     client,
		mirror:     newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 100}),
		tree:       rootTree("stop"),
	}
	m.roots[handle.socketPath] = handle
	m.routes["stop"] = "stop"

	fleetSub := m.SubscribeFleet()
	defer fleetSub.Close()
	sessionSub, err := m.SubscribeSession("stop")
	if err != nil {
		t.Fatal(err)
	}
	defer sessionSub.Close()

	if m.monitorRoot(handle) != monitorStarted {
		t.Fatal("monitorRoot refused to start")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})

	select {
	case rev := <-fleetSub.Updates:
		if rev == 0 {
			t.Fatal("received zero fleet revision on stopping transition")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no fleet notify after retained-tree transition")
	}
	select {
	case <-sessionSub.Updates:
	case <-time.After(2 * time.Second):
		t.Fatal("no session notify after retained-tree transition")
	}

	// The handle must be marked non-actionable after the retained update.
	deadline := time.After(2 * time.Second)
	for {
		m.mu.Lock()
		actionable := m.roots[handle.socketPath].actionable
		m.mu.Unlock()
		if !actionable {
			break
		}
		select {
		case <-deadline:
			t.Fatal("retained root still marked actionable")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// A later admissible snapshot must restore actionability; before the
	// explicit actionability commit this transition stayed disabled forever.
	client.mu.Lock()
	client.snapshot = rootTree("stop")
	client.mu.Unlock()
	deadline = time.After(2 * time.Second)
	for {
		m.mu.Lock()
		actionable := m.roots[handle.socketPath].actionable
		m.mu.Unlock()
		if actionable {
			break
		}
		select {
		case <-deadline:
			t.Fatal("recovered retained root did not become actionable")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := m.actionableClient("stop"); err != nil {
		t.Fatalf("recovered retained root still rejects actions: %v", err)
	}
}

func TestSnapshotLoopKeepsInadmissibleRefreshNonActionableAndRecovers(t *testing.T) {
	starting := rootTree("root")
	starting.Lifecycle = string(supervisor.LifecycleStarting)
	starting.PiSessionID = ""
	starting.SessionFile = ""
	client := &fakeClient{snapshot: starting}
	m := fakeManager(nil)
	m.opts.SnapshotInterval = 5 * time.Millisecond
	handle := &rootHandle{
		socketPath: "/tmp/refresh.root.sock", rootID: "root", actionable: true,
		client: client, tree: rootTree("root"), mirror: newEventMirror(m.opts.EventLimits),
	}
	m.roots[handle.socketPath] = handle
	m.routes["root"] = "root"
	if m.monitorRoot(handle) != monitorStarted {
		t.Fatal("monitorRoot did not start")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})

	deadline := time.Now().Add(time.Second)
	for !m.Fleet().Roots[0].Stale && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	state := m.Fleet().Roots[0]
	if !state.Stale {
		t.Fatal("inadmissible refresh did not mark root stale")
	}
	if state.Tree.Lifecycle != string(supervisor.LifecycleReady) {
		t.Fatalf("inadmissible refresh replaced last good tree with %q", state.Tree.Lifecycle)
	}
	if err := m.Interrupt(context.Background(), "root"); err == nil {
		t.Fatal("control reached root after inadmissible refresh")
	}

	client.mu.Lock()
	client.snapshot = rootTree("root")
	client.mu.Unlock()
	deadline = time.Now().Add(time.Second)
	for m.Fleet().Roots[0].Stale && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.Fleet().Roots[0].Stale {
		t.Fatal("admissible refresh did not recover stale root")
	}
}

// ---- ManagerStart / ManagerClose tests ----

func TestReplacementHandleIgnoresOldMonitorMutations(t *testing.T) {
	m := fakeManager(nil)
	path := "/tmp/replaced.root.sock"
	old := &rootHandle{socketPath: path, rootID: "old"}
	replacementTree := rootTree("replacement")
	replacement := &rootHandle{
		socketPath: path, rootID: "replacement", tree: replacementTree,
		actionable: true, streamConnected: true,
	}
	m.roots[path] = replacement

	m.markStale(old, true)
	m.setStreamConnected(old, false)
	terminal := rootTree("old")
	terminal.Lifecycle = string(supervisor.LifecycleStopped)
	m.updateRetainedTree(old, terminal, false)

	if replacement.stale || !replacement.streamConnected || !replacement.actionable {
		t.Fatalf("old monitor mutated replacement state: %+v", replacement)
	}
	if replacement.tree.SessionID != "replacement" {
		t.Fatalf("old monitor replaced tree with %q", replacement.tree.SessionID)
	}
}

func TestEventLoopBacksOffAfterImmediateEOF(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	client := &fakeClient{snapshot: rootTree("eof"), closeChan: closed, closed: true}
	m := fakeManager(nil)
	handle := &rootHandle{
		socketPath: "/tmp/eof-loop.root.sock", rootID: "eof", client: client,
		actionable: true, tree: rootTree("eof"),
		mirror: newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 100}),
	}
	m.roots[handle.socketPath] = handle
	m.routes["eof"] = "eof"
	if m.monitorRoot(handle) != monitorStarted {
		t.Fatal("monitorRoot did not start")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})

	time.Sleep(250 * time.Millisecond)
	if got := client.callCount("Subscribe"); got < 2 || got > 4 {
		t.Fatalf("immediate EOF reconnect count = %d, want bounded 2..4", got)
	}
}

func TestManagerStartAndClose(t *testing.T) {
	// Use short dirs: the path-length probe uses a 32-char token placeholder.
	dir, logDir := shortTempDirs(t)
	m, err := New(Options{
		RootSocketDir:     dir,
		SessionLogDir:     logDir,
		EventLimits:       supervisor.EventBrokerOptions{MaxEvents: 100},
		Logger:            discardLogger(),
		Launch:            managerTestLaunch(),
		DiscoveryInterval: 100 * time.Millisecond,
		SnapshotInterval:  100 * time.Millisecond,
		SpawnTimeout:      5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.opts.SessionBinary == "" || !filepath.IsAbs(m.opts.SessionBinary) {
		t.Fatalf("default SessionBinary = %q, want absolute current executable", m.opts.SessionBinary)
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

func TestManagerQuiesceRejectsActionsAndStopsSnapshotPolling(t *testing.T) {
	client := &fakeClient{snapshot: rootTree("root")}
	m := fakeManager(nil)
	m.opts.SnapshotInterval = 5 * time.Millisecond
	handle := &rootHandle{
		socketPath: "/tmp/quiesce.root.sock", rootID: "root", actionable: true,
		client: client, tree: rootTree("root"), mirror: newEventMirror(m.opts.EventLimits),
	}
	m.roots[handle.socketPath] = handle
	m.routes["root"] = "root"
	if m.monitorRoot(handle) != monitorStarted {
		t.Fatal("monitorRoot did not start")
	}

	deadline := time.Now().Add(time.Second)
	for client.callCount("Snapshot") == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := m.Quiesce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Interrupt(context.Background(), "root"); err == nil {
		t.Fatal("action succeeded after Quiesce")
	}

	// Allow an already-completing call to settle, then prove no new polls start.
	time.Sleep(20 * time.Millisecond)
	calls := client.callCount("Snapshot")
	time.Sleep(30 * time.Millisecond)
	if got := client.callCount("Snapshot"); got != calls {
		t.Fatalf("snapshot polling continued after Quiesce: %d -> %d", calls, got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestManagerQuiesceCancelsInFlightSnapshot(t *testing.T) {
	client := &contextBlockingClient{
		fakeClient: &fakeClient{snapshot: rootTree("root")},
		entered:    make(chan struct{}),
		exited:     make(chan struct{}),
	}
	m := fakeManager(nil)
	m.opts.SnapshotInterval = time.Millisecond
	handle := &rootHandle{
		socketPath: "/tmp/blocked.root.sock", rootID: "root", actionable: true,
		client: client, tree: rootTree("root"), mirror: newEventMirror(m.opts.EventLimits),
	}
	m.roots[handle.socketPath] = handle
	m.routes["root"] = "root"
	if m.monitorRoot(handle) != monitorStarted {
		t.Fatal("monitorRoot did not start")
	}
	select {
	case <-client.entered:
	case <-time.After(time.Second):
		t.Fatal("snapshot did not enter blocked call")
	}
	if err := m.Quiesce(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.exited:
	case <-time.After(time.Second):
		t.Fatal("Quiesce did not cancel in-flight snapshot")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestPeriodicallyDiscoveredRootIsMonitored is the C1 regression test: a root
// that first appears AFTER Start (via the periodic discovery loop) must have its
// monitoring loops launched, not just be added as a dead fleet entry.
func TestPeriodicallyDiscoveredRootIsMonitored(t *testing.T) {
	rootDir, logDir := shortTempDirs(t)

	client := &fakeClient{snapshot: rootTree("late")}
	m, err := New(Options{
		RootSocketDir:     rootDir,
		SessionLogDir:     logDir,
		EventLimits:       supervisor.EventBrokerOptions{MaxEvents: 100},
		Logger:            discardLogger(),
		Launch:            managerTestLaunch(),
		DiscoveryInterval: 20 * time.Millisecond,
		SnapshotInterval:  20 * time.Millisecond,
		SpawnTimeout:      5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Inject the fake client for whatever socket appears.
	m.factory = func(_ string) (rootClient, error) { return client, nil }

	ctx := context.Background()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Close(closeCtx)
	})

	// Nothing should be admitted yet (empty dir at Start).
	if got := len(m.Fleet().Roots); got != 0 {
		t.Fatalf("expected empty fleet at Start, got %d roots", got)
	}

	// Now create the root socket; the periodic loop must admit AND monitor it.
	makeRootSocket(t, rootDir, "late.root.sock")

	deadline := time.After(5 * time.Second)
	for {
		fleet := m.Fleet()
		if len(fleet.Roots) == 1 && fleet.Roots[0].StreamConnected &&
			client.callCount("Subscribe") >= 1 && client.callCount("Snapshot") >= 2 {
			// >=2 Snapshot calls: one at admission probe, at least one from the
			// running snapshotLoop (proving the tree keeps refreshing).
			break
		}
		select {
		case <-deadline:
			t.Fatalf("root discovered via periodic loop was not monitored: roots=%d subscribe=%d snapshot=%d streamConnected=%v",
				len(fleet.Roots), client.callCount("Subscribe"), client.callCount("Snapshot"),
				len(fleet.Roots) == 1 && fleet.Roots[0].StreamConnected)
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Re-committing on later scans must NOT launch duplicate loops. Give the
	// discovery loop a few cycles and assert exactly one monitoring start by
	// checking the handle's monitoring flag stays a single pair of goroutines
	// (indirectly: Subscribe should not explode). Subscribe count should stay
	// bounded because eventLoop parks on the open Events channel.
	subAfter := client.callCount("Subscribe")
	time.Sleep(120 * time.Millisecond)
	if got := client.callCount("Subscribe"); got > subAfter+1 {
		t.Fatalf("duplicate event loops detected: Subscribe went %d -> %d over several discovery cycles", subAfter, got)
	}
}

// TestMonitorRootRefusesAfterClose is the I1 guard: once the manager is closed,
// monitorRoot must not start loops (which Close would never wait on) and must
// report failure so the caller can clean up the orphaned handle.
func TestMonitorRootRefusesAfterClose(t *testing.T) {
	m := fakeManager(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	handle := &rootHandle{
		socketPath: "/tmp/afterclose.root.sock",
		rootID:     "afterclose",
		client:     &fakeClient{snapshot: rootTree("afterclose")},
	}
	if m.monitorRoot(handle) != monitorRefusedClosing {
		t.Fatal("monitorRoot must refuse to start loops after Close")
	}
	if handle.monitoring {
		t.Fatal("handle marked monitoring despite refusal")
	}
}

func TestManagerFleetFanoutPublishesOnDiscovery(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := makeRootSocket(t, dir, "fan.root.sock")
	tree := rootTree("fan")
	client := &fakeClient{snapshot: tree}
	m := fakeManager(func(_ string) (rootClient, error) { return client, nil })
	m.opts.RootSocketDir = dir
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})

	sub := m.SubscribeFleet()
	defer sub.Close()
	m.discoverOnce(context.Background())

	select {
	case rev := <-sub.Updates:
		if rev == 0 {
			t.Fatal("received zero revision")
		}
	case <-time.After(time.Second):
		t.Fatal("fleet fanout did not publish after admission")
	}

	deadline := time.Now().Add(time.Second)
	for !m.Fleet().Roots[0].StreamConnected {
		if time.Now().After(deadline) {
			t.Fatal("event stream did not connect")
		}
		time.Sleep(time.Millisecond)
	}

	// Drain connection-state notifications, then prove an unchanged scan is
	// silent before testing the disappearance notification.
drain:
	for {
		select {
		case <-sub.Updates:
		default:
			break drain
		}
	}
	m.discoverOnce(context.Background())
	select {
	case rev := <-sub.Updates:
		t.Fatalf("unchanged discovery published revision %d", rev)
	case <-time.After(50 * time.Millisecond):
	}

	beforeRemoval := m.Fleet().Revision
	if err := os.Remove(socketPath); err != nil {
		t.Fatal(err)
	}
	m.discoverOnce(context.Background())
	select {
	case rev := <-sub.Updates:
		if rev <= beforeRemoval {
			t.Fatalf("removal revision = %d, want > %d", rev, beforeRemoval)
		}
	case <-time.After(time.Second):
		t.Fatal("fleet fanout did not publish after removal")
	}
}
