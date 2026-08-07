package supervisor

import (
	"encoding/json"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestEventBrokerAssignsLocalAndSubtreeSequences(t *testing.T) {
	broker := newEventBroker(8, 4)

	first := broker.PublishLocal("root", "pi", json.RawMessage(`{"n":1}`))
	second := broker.PublishLocal("root", "pi", json.RawMessage(`{"n":2}`))
	forwarded := broker.Forward(EventEnvelope{
		Seq:       99,
		SessionID: "child",
		SourceSeq: 7,
		Kind:      "pi",
		Payload:   json.RawMessage(`{"n":3}`),
	})

	if first.Seq != 1 || first.SourceSeq != 1 {
		t.Fatalf("first sequences = (%d, %d), want (1, 1)", first.Seq, first.SourceSeq)
	}
	if second.Seq != 2 || second.SourceSeq != 2 {
		t.Fatalf("second sequences = (%d, %d), want (2, 2)", second.Seq, second.SourceSeq)
	}
	if forwarded.Seq != 3 {
		t.Fatalf("forwarded Seq = %d, want 3", forwarded.Seq)
	}
	if forwarded.SessionID != "child" || forwarded.SourceSeq != 7 {
		t.Fatalf("forwarded source = (%q, %d), want (child, 7)", forwarded.SessionID, forwarded.SourceSeq)
	}
}

func TestEventBrokerEvictsOnlyOldestEnvelope(t *testing.T) {
	broker := newEventBroker(3, 4)
	for n := 1; n <= 4; n++ {
		broker.PublishLocal("root", "pi", json.RawMessage(`{}`))
	}

	subscription := broker.Subscribe()
	defer subscription.Close()
	if len(subscription.Replay) != 3 {
		t.Fatalf("Replay length = %d, want 3", len(subscription.Replay))
	}
	for index, wantSeq := range []uint64{2, 3, 4} {
		if got := subscription.Replay[index].Seq; got != wantSeq {
			t.Fatalf("Replay[%d].Seq = %d, want %d", index, got, wantSeq)
		}
	}
}

func TestEventBrokerSubscriptionReplaysThenReceivesLiveEvents(t *testing.T) {
	broker := newEventBroker(4, 2)
	broker.PublishLocal("root", "pi", json.RawMessage(`{"phase":"replay"}`))

	subscription := broker.Subscribe()
	defer subscription.Close()
	if len(subscription.Replay) != 1 || subscription.Replay[0].Seq != 1 {
		t.Fatalf("Replay = %#v, want retained event 1", subscription.Replay)
	}

	broker.PublishLocal("root", "pi", json.RawMessage(`{"phase":"live"}`))
	select {
	case event := <-subscription.Events:
		if event.Seq != 2 || string(event.Payload) != `{"phase":"live"}` {
			t.Fatalf("live event = %#v, want seq 2 live payload", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live event")
	}
}

func TestEventBrokerReplayIsImmutableCopy(t *testing.T) {
	broker := newEventBroker(2, 1)
	payload := json.RawMessage(`{"safe":true}`)
	broker.PublishLocal("root", "pi", payload)
	payload[2] = 'X'

	subscription := broker.Subscribe()
	defer subscription.Close()
	subscription.Replay[0].Payload[2] = 'Y'

	second := broker.Subscribe()
	defer second.Close()
	if got := string(second.Replay[0].Payload); got != `{"safe":true}` {
		t.Fatalf("retained payload = %q, want immutable original", got)
	}
}

func TestEventBrokerOverflowDisconnectsOnlySlowSubscriber(t *testing.T) {
	broker := newEventBroker(4, 1)
	slow := broker.Subscribe()
	fast := broker.Subscribe()
	defer slow.Close()
	defer fast.Close()

	broker.PublishLocal("root", "pi", json.RawMessage(`{"n":1}`))
	select {
	case <-fast.Events:
	case <-time.After(time.Second):
		t.Fatal("fast subscriber missed first event")
	}

	broker.PublishLocal("root", "pi", json.RawMessage(`{"n":2}`))
	select {
	case event := <-fast.Events:
		if event.Seq != 2 {
			t.Fatalf("fast event Seq = %d, want 2", event.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("fast subscriber was disconnected by slow subscriber overflow")
	}
	broker.PublishLocal("root", "pi", json.RawMessage(`{"n":3}`))

	select {
	case _, ok := <-slow.Events:
		if ok {
			select {
			case _, ok = <-slow.Events:
				if ok {
					t.Fatal("slow subscriber channel remains open after overflow")
				}
			case <-time.After(time.Second):
				t.Fatal("slow subscriber channel remains open after draining mailbox")
			}
		}
	case <-time.After(time.Second):
		t.Fatal("slow subscriber was not disconnected")
	}
}

func TestEventBrokerConcurrentPublishersDeliverMonotonicSequence(t *testing.T) {
	const publishers = 256
	broker := newEventBroker(publishers, publishers)
	subscription := broker.Subscribe()
	defer subscription.Close()

	start := make(chan struct{})
	var group sync.WaitGroup
	group.Add(publishers)
	for n := 0; n < publishers; n++ {
		go func() {
			defer group.Done()
			<-start
			broker.PublishLocal("root", "pi", json.RawMessage(`{}`))
		}()
	}
	close(start)
	group.Wait()

	for want := uint64(1); want <= publishers; want++ {
		select {
		case event, ok := <-subscription.Events:
			if !ok {
				t.Fatalf("subscriber disconnected at sequence %d", want)
			}
			if event.Seq != want {
				t.Fatalf("delivered Seq = %d, want monotonic %d", event.Seq, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for sequence %d", want)
		}
	}
}

func TestEventBrokerStateMutexIsReleasedBeforeMailboxDelivery(t *testing.T) {
	broker := newEventBroker(1, 1)
	subscription := broker.Subscribe()
	defer subscription.Close()

	broker.mu.Lock()
	var mailbox *eventMailbox
	for _, candidate := range broker.subs {
		mailbox = candidate
	}
	broker.mu.Unlock()
	if mailbox == nil {
		t.Fatal("subscriber mailbox was not registered")
	}

	mailbox.mu.Lock()
	published := make(chan struct{})
	go func() {
		broker.PublishLocal("root", "pi", json.RawMessage(`{"large":"payload"}`))
		close(published)
	}()

	deadline := time.Now().Add(200 * time.Millisecond)
	stateVisible := false
	for time.Now().Before(deadline) {
		if broker.mu.TryLock() {
			stateVisible = broker.nextSeq == 1
			broker.mu.Unlock()
			if stateVisible {
				break
			}
		}
		runtime.Gosched()
	}
	mailbox.mu.Unlock()
	<-published
	if !stateVisible {
		t.Fatal("EventBroker state mutex remained held while delivery waited on a mailbox")
	}
}

func TestEventBrokerConcurrentSubscribeHasSingleReplayLiveCut(t *testing.T) {
	const eventCount = 256
	broker := newEventBroker(eventCount, eventCount)
	start := make(chan struct{})
	published := make(chan struct{})
	go func() {
		defer close(published)
		<-start
		for n := 0; n < eventCount; n++ {
			broker.PublishLocal("root", "pi", json.RawMessage(`{}`))
		}
	}()

	close(start)
	subscription := broker.Subscribe()
	defer subscription.Close()
	<-published

	combined := append([]EventEnvelope(nil), subscription.Replay...)
	for len(combined) < eventCount {
		select {
		case event, ok := <-subscription.Events:
			if !ok {
				t.Fatalf("subscription closed after %d of %d events", len(combined), eventCount)
			}
			combined = append(combined, event)
		case <-time.After(time.Second):
			t.Fatalf("timed out after %d of %d events", len(combined), eventCount)
		}
	}
	for index, event := range combined {
		want := uint64(index + 1)
		if event.Seq != want {
			t.Fatalf("combined[%d].Seq = %d, want exactly-once monotonic %d", index, event.Seq, want)
		}
	}
}

func TestEventBrokerConcurrentCloseAndSubscribe(t *testing.T) {
	const iterations = 100
	broker := newEventBroker(iterations, iterations)
	var group sync.WaitGroup
	start := make(chan struct{})
	for n := 0; n < iterations; n++ {
		subscription := broker.Subscribe()
		group.Add(4)
		go func() {
			defer group.Done()
			<-start
			broker.PublishLocal("root", "pi", json.RawMessage(`{}`))
		}()
		go func() {
			defer group.Done()
			<-start
			subscription.Close()
		}()
		go func() {
			defer group.Done()
			<-start
			subscription.Close()
		}()
		go func() {
			defer group.Done()
			<-start
			concurrent := broker.Subscribe()
			concurrent.Close()
		}()
	}
	close(start)
	group.Wait()

	final := broker.Subscribe()
	final.Close()
	select {
	case _, ok := <-final.Events:
		if ok {
			t.Fatal("closed subscription produced an event")
		}
	case <-time.After(time.Second):
		t.Fatal("closed subscription channel remained open")
	}
}

func TestEventBrokerNeverBlocksPublisherOnUnreadSubscriber(t *testing.T) {
	broker := newEventBroker(8, 1)
	slow := broker.Subscribe()
	defer slow.Close()

	done := make(chan struct{})
	go func() {
		for n := 0; n < 10_000; n++ {
			broker.PublishLocal("root", "pi", json.RawMessage(`{}`))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publishing 10,000 events blocked on unread subscriber")
	}
}

func TestEventBrokerCloseIsSafeWithConcurrentPublishAndSubscribe(t *testing.T) {
	broker := newEventBroker(128, 128)
	start := make(chan struct{})
	var group sync.WaitGroup
	for range 128 {
		group.Add(2)
		go func() {
			defer group.Done()
			<-start
			broker.PublishLocal("root", "pi", json.RawMessage(`{}`))
		}()
		go func() {
			defer group.Done()
			<-start
			subscription := broker.Subscribe()
			subscription.Close()
		}()
	}
	group.Add(2)
	go func() { defer group.Done(); <-start; broker.Close() }()
	go func() { defer group.Done(); <-start; broker.Close() }()
	close(start)
	group.Wait()

	rejected := broker.Subscribe()
	select {
	case _, open := <-rejected.Events:
		if open {
			t.Fatal("concurrent broker close admitted a final subscriber")
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent broker close left final subscriber open")
	}
}

func TestEventBrokerCloseIsIdempotentClosesSubscribersAndRejectsNewOnes(t *testing.T) {
	broker := newEventBroker(8, 2)
	existing := broker.Subscribe()

	broker.Close()
	broker.Close()

	select {
	case _, open := <-existing.Events:
		if open {
			t.Fatal("existing subscription remained open after broker close")
		}
	case <-time.After(time.Second):
		t.Fatal("broker close did not close existing subscriber mailbox")
	}

	rejected := broker.Subscribe()
	if len(rejected.Replay) != 0 {
		t.Fatalf("closed broker replay = %#v, want none", rejected.Replay)
	}
	select {
	case _, open := <-rejected.Events:
		if open {
			t.Fatal("closed broker admitted a new subscription")
		}
	case <-time.After(time.Second):
		t.Fatal("subscription to closed broker was not rejected promptly")
	}
	existing.Close()
	rejected.Close()
}

func TestEventBrokerEvictsOldestUntilCountAndByteBudgetsHold(t *testing.T) {
	broker := newEventBrokerWithByteCapacity(10, 2, 100)
	for index := 0; index < 4; index++ {
		broker.PublishLocal("root", "pi", json.RawMessage(`{"payload":"12345678901234567890"}`))
	}
	sub := broker.Subscribe()
	defer sub.Close()
	if len(sub.Replay) == 0 || len(sub.Replay) >= 4 {
		t.Fatalf("byte-bounded replay length = %d, want partial retained tail", len(sub.Replay))
	}
	for index := 1; index < len(sub.Replay); index++ {
		if sub.Replay[index].Seq != sub.Replay[index-1].Seq+1 {
			t.Fatalf("replay is not the monotonic newest tail: %#v", sub.Replay)
		}
	}
}

func TestEventBrokerSubscribeDoesNotHoldPublicationGateWhileCloningReplay(t *testing.T) {
	broker := newEventBroker(8, 8)
	broker.PublishLocal("root", "pi", json.RawMessage(`{"old":true}`))
	cloneEntered := make(chan struct{})
	cloneRelease := make(chan struct{})
	broker.cloneReplay = func(events []EventEnvelope) []EventEnvelope {
		close(cloneEntered)
		<-cloneRelease
		return cloneEnvelopes(events)
	}
	subscribed := make(chan Subscription, 1)
	go func() { subscribed <- broker.Subscribe() }()
	<-cloneEntered
	published := make(chan EventEnvelope, 1)
	go func() { published <- broker.PublishLocal("root", "pi", json.RawMessage(`{"live":true}`)) }()
	select {
	case event := <-published:
		if event.Seq != 2 {
			t.Fatalf("published event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("publication blocked behind replay deep-copy")
	}
	close(cloneRelease)
	sub := <-subscribed
	defer sub.Close()
	if len(sub.Replay) != 1 || sub.Replay[0].Seq != 1 {
		t.Fatalf("replay = %#v", sub.Replay)
	}
	select {
	case event := <-sub.Events:
		if event.Seq != 2 {
			t.Fatalf("live event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("live event missing after replay cut")
	}
}
