package manager

import (
	"testing"
	"time"
)

func TestSlowChangeSubscriberIsDisconnected(t *testing.T) {
	fanout := newChangeFanout(1)
	slow := fanout.Subscribe()
	fanout.Publish(1)
	fanout.Publish(2) // mailbox is full — slow subscriber gets disconnected
	<-slow.Updates
	select {
	case _, open := <-slow.Updates:
		if open {
			t.Fatal("slow subscriber remains open after overflow")
		}
	case <-time.After(time.Second):
		t.Fatal("slow subscriber channel was not closed")
	}
}

func TestChangeFanoutDeliverToMultipleSubscribers(t *testing.T) {
	fanout := newChangeFanout(4)
	s1 := fanout.Subscribe()
	s2 := fanout.Subscribe()
	fanout.Publish(42)
	select {
	case rev := <-s1.Updates:
		if rev != 42 {
			t.Fatalf("s1 got %d, want 42", rev)
		}
	case <-time.After(time.Second):
		t.Fatal("s1 did not receive revision")
	}
	select {
	case rev := <-s2.Updates:
		if rev != 42 {
			t.Fatalf("s2 got %d, want 42", rev)
		}
	case <-time.After(time.Second):
		t.Fatal("s2 did not receive revision")
	}
}

func TestChangeFanoutCloseIdempotent(t *testing.T) {
	fanout := newChangeFanout(1)
	sub := fanout.Subscribe()
	sub.Close()
	sub.Close() // must not panic
	if _, open := <-sub.Updates; open {
		t.Fatal("channel still open after Close")
	}
}

func TestChangeFanoutCloseDisconnectsAll(t *testing.T) {
	fanout := newChangeFanout(4)
	s1 := fanout.Subscribe()
	s2 := fanout.Subscribe()
	fanout.Close()
	for _, sub := range []ChangeSubscription{s1, s2} {
		select {
		case _, open := <-sub.Updates:
			if open {
				t.Fatal("subscriber channel open after fanout Close")
			}
		case <-time.After(time.Second):
			t.Fatal("subscriber channel was not closed by fanout Close")
		}
	}
}
