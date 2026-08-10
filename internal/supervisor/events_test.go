package supervisor

import (
	"encoding/json"
	"strings"
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

func TestDefaultEventBrokerSubscriberSurvivesRetainedBurstBeyondLegacyMailbox(t *testing.T) {
	broker := NewEventBroker()
	subscription := broker.Subscribe()
	defer subscription.Close()

	for seq := 1; seq <= 256; seq++ {
		broker.PublishLocal("root", "pi", json.RawMessage(`{"type":"message_update"}`))
	}
	for want := uint64(1); want <= 256; want++ {
		select {
		case event, open := <-subscription.Events:
			if !open || event.Seq != want {
				t.Fatalf("event %d = %#v, open=%t", want, event, open)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %d", want)
		}
	}
}

func TestEventBrokerMailboxDisconnectsBeforeByteBudgetOverflow(t *testing.T) {
	broker := newEventBrokerWithByteCapacity(8, 128, 120)
	slow := broker.Subscribe()
	defer slow.Close()

	first := broker.PublishLocal("root", "pi", json.RawMessage(`{"payload":"`+strings.Repeat("x", 48)+`"}`))
	if RetainedEventBytes(first) > 120 {
		t.Fatal("test event unexpectedly exceeds mailbox budget")
	}
	broker.PublishLocal("root", "pi", json.RawMessage(`{"payload":"`+strings.Repeat("y", 48)+`"}`))

	select {
	case _, open := <-slow.Events:
		if open {
			t.Fatal("byte-overflowed subscription delivered unread queued data")
		}
	case <-time.After(time.Second):
		t.Fatal("slow subscriber did not close after byte overflow")
	}
}

func TestEventBrokerSubscriptionCloseDiscardsUnreadQueuedEvent(t *testing.T) {
	broker := newEventBroker(4, 1)
	subscription := broker.Subscribe()

	broker.PublishLocal("root", "pi", json.RawMessage(`{"n":1}`))
	subscription.Close()

	select {
	case _, open := <-subscription.Events:
		if open {
			t.Fatal("subscription close delivered an event that was unread at close")
		}
	case <-time.After(time.Second):
		t.Fatal("subscription close did not terminate the mailbox promptly")
	}
}

func TestEventBrokerOverflowDiscardsUnreadQueuedEvent(t *testing.T) {
	broker := newEventBroker(4, 1)
	subscription := broker.Subscribe()
	defer subscription.Close()

	broker.PublishLocal("root", "pi", json.RawMessage(`{"n":1}`))
	broker.PublishLocal("root", "pi", json.RawMessage(`{"n":2}`))

	select {
	case _, open := <-subscription.Events:
		if open {
			t.Fatal("overflow delivered an event that was unread at detachment")
		}
	case <-time.After(time.Second):
		t.Fatal("overflow detachment did not terminate the mailbox promptly")
	}
}

func TestEventBrokerGracefulCloseKeepsBufferedEventsReadable(t *testing.T) {
	broker := newEventBroker(4, 2)
	subscription := broker.Subscribe()
	defer subscription.Close()

	broker.PublishLocal("root", "pi", json.RawMessage(`{"n":1}`))
	broker.PublishLocal("root", "pi", json.RawMessage(`{"n":2}`))
	broker.Close()

	for want := uint64(1); want <= 2; want++ {
		select {
		case event, open := <-subscription.Events:
			if !open || event.Seq != want {
				t.Fatalf("drained event %d = %#v, open=%t", want, event, open)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out draining accepted event %d", want)
		}
	}
	select {
	case _, open := <-subscription.Events:
		if open {
			t.Fatal("subscription events channel still open after graceful close drain")
		}
	case <-time.After(time.Second):
		t.Fatal("subscription events channel did not close after graceful drain")
	}
}

func TestEventBrokerSubscriptionCloseAfterGracefulCloseDiscardsUnreadEvents(t *testing.T) {
	broker := newEventBroker(4, 2)
	subscription := broker.Subscribe()

	broker.PublishLocal("root", "pi", json.RawMessage(`{"n":1}`))
	broker.Close()
	subscription.Close()

	select {
	case _, open := <-subscription.Events:
		if open {
			t.Fatal("subscription close delivered an unread event after graceful close")
		}
	case <-time.After(time.Second):
		t.Fatal("subscription output did not close after graceful close was aborted")
	}
}

func TestEventBrokerSubscriptionCloseRacingGracefulCloseReleasesMailbox(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		broker := newEventBroker(4, 2)
		subscription := broker.Subscribe()
		broker.PublishLocal("root", "pi", json.RawMessage(`{"n":1}`))

		start := make(chan struct{})
		var group sync.WaitGroup
		group.Add(2)
		go func() {
			defer group.Done()
			<-start
			broker.Close()
		}()
		go func() {
			defer group.Done()
			<-start
			subscription.Close()
		}()
		close(start)
		completed := make(chan struct{})
		go func() {
			group.Wait()
			close(completed)
		}()
		select {
		case <-completed:
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: racing closes did not complete", iteration)
		}

		select {
		case _, open := <-subscription.Events:
			if open {
				t.Fatalf("iteration %d: racing close retained an unread event", iteration)
			}
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: racing close left output open", iteration)
		}
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

func TestEventBrokerCountAndByteBoundedFastSubscriberSurvivesRepeatedHandoffs(t *testing.T) {
	tests := []struct {
		name      string
		newBroker func() *EventBroker
	}{
		{
			name: "count",
			newBroker: func() *EventBroker {
				return newEventBroker(4, 1)
			},
		},
		{
			name: "bytes",
			newBroker: func() *EventBroker {
				return newEventBrokerWithByteCapacity(4, 4, 32)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for iteration := 0; iteration < 100; iteration++ {
				broker := test.newBroker()
				slow := broker.Subscribe()
				fast := broker.Subscribe()

				first := broker.PublishLocal("root", "pi", json.RawMessage(`{}`))
				if test.name == "bytes" && RetainedEventBytes(first) != 32 {
					t.Fatalf("iteration %d: test event bytes = %d, want 32", iteration, RetainedEventBytes(first))
				}
				select {
				case event, open := <-fast.Events:
					if !open || event.Seq != 1 {
						t.Fatalf("iteration %d: first fast event = %#v, open=%t", iteration, event, open)
					}
				case <-time.After(time.Second):
					t.Fatalf("iteration %d: timed out waiting for first fast event", iteration)
				}

				broker.PublishLocal("root", "pi", json.RawMessage(`{}`))
				select {
				case event, open := <-fast.Events:
					if !open || event.Seq != 2 {
						t.Fatalf("iteration %d: second fast event = %#v, open=%t", iteration, event, open)
					}
				case <-time.After(time.Second):
					t.Fatalf("iteration %d: fast subscriber detached at handoff boundary", iteration)
				}
				select {
				case _, open := <-slow.Events:
					if open {
						t.Fatalf("iteration %d: slow subscriber delivered unread data after genuine overflow", iteration)
					}
				case <-time.After(time.Second):
					t.Fatalf("iteration %d: genuinely full slow subscriber was not detached", iteration)
				}

				fast.Close()
				slow.Close()
				broker.Close()
			}
		})
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

func TestEventBrokerCountAndByteLimitsAreIndependent(t *testing.T) {
	countOnly, err := NewEventBrokerWithOptions(EventBrokerOptions{MaxEvents: 2})
	if err != nil {
		t.Fatal(err)
	}
	byteOnly, err := NewEventBrokerWithOptions(EventBrokerOptions{MaxBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		countOnly.PublishLocal("root", "pi", json.RawMessage(`{"n":1}`))
		byteOnly.PublishLocal("root", "pi", json.RawMessage(`{"payload":"12345678901234567890"}`))
	}
	if got := len(countOnly.Subscribe().Replay); got != 2 {
		t.Fatalf("count replay = %d", got)
	}
	if got := len(byteOnly.Subscribe().Replay); got == 0 || got >= 3 {
		t.Fatalf("byte replay = %d", got)
	}
}

func TestEventBrokerByteOnlySubscriberQueuesUntilByteWindowExceeded(t *testing.T) {
	broker, err := NewEventBrokerWithOptions(EventBrokerOptions{MaxBytes: 512})
	if err != nil {
		t.Fatal(err)
	}
	subscription := broker.Subscribe()
	defer subscription.Close()

	broker.PublishLocal("root", "pi", json.RawMessage(`{"n":1}`))
	broker.PublishLocal("root", "pi", json.RawMessage(`{"n":2}`))
	select {
	case event, open := <-subscription.Events:
		if !open || event.Seq != 1 {
			t.Fatalf("first queued event = %#v, open=%t; byte-only subscriber detached early", event, open)
		}
	case <-time.After(time.Second):
		t.Fatal("byte-only subscriber did not deliver its first queued event")
	}

	for sequence := 3; sequence <= 100; sequence++ {
		broker.PublishLocal("root", "pi", json.RawMessage(`{"n":3}`))
	}
	select {
	case _, open := <-subscription.Events:
		if open {
			t.Fatal("byte-only subscriber delivered unread data after exceeding its byte window")
		}
	case <-time.After(time.Second):
		t.Fatal("byte-only subscriber did not detach after exceeding its byte window")
	}
}

func TestEventBrokerWithOptionsRejectsBothZero(t *testing.T) {
	_, err := NewEventBrokerWithOptions(EventBrokerOptions{})
	if err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("error = %v, want at-least-one error", err)
	}
}

func TestEventBrokerWithOptionsRejectsNegative(t *testing.T) {
	_, err := NewEventBrokerWithOptions(EventBrokerOptions{MaxEvents: -1, MaxBytes: 100})
	if err == nil {
		t.Fatalf("error = nil, want rejection of negative MaxEvents")
	}
	_, err = NewEventBrokerWithOptions(EventBrokerOptions{MaxEvents: 100, MaxBytes: -1})
	if err == nil {
		t.Fatalf("error = nil, want rejection of negative MaxBytes")
	}
}

func TestEventBrokerOversizedEventIsNotRetainedAndDisconnectsStalledSubscriber(t *testing.T) {
	broker, err := NewEventBrokerWithOptions(EventBrokerOptions{MaxBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	sub := broker.Subscribe()
	defer sub.Close()

	envelope := broker.PublishLocal("root", "pi", json.RawMessage(`{"large":"payload_that_exceeds_cap"}`))
	if envelope.Seq == 0 {
		t.Fatalf("oversized event has no sequence number")
	}
	select {
	case _, open := <-sub.Events:
		if open {
			t.Fatal("oversized event was queued beyond the subscriber byte budget")
		}
	case <-time.After(time.Second):
		t.Fatal("stalled subscriber was not disconnected")
	}

	replay := broker.Subscribe()
	defer replay.Close()
	if len(replay.Replay) != 0 {
		t.Fatalf("oversized event retained in replay ring, want empty")
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
