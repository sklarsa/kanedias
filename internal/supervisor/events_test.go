package supervisor

import (
	"encoding/json"
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
