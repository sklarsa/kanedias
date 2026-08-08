package manager

import (
	"encoding/json"
	"testing"

	"github.com/sklarsa/kanedias/internal/supervisor"
)

func envelope(seq uint64, session string) supervisor.EventEnvelope {
	return supervisor.EventEnvelope{
		Seq: seq, SessionID: session, SourceSeq: seq,
		Kind: "pi", Payload: json.RawMessage(`{"type":"agent_start"}`),
	}
}

func TestMirrorDeduplicatesReplayAndRecordsFirstGap(t *testing.T) {
	mirror := newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 8, MaxBytes: 4096})
	mirror.Accept(envelope(4, "a"))
	mirror.Accept(envelope(4, "a")) // duplicate
	mirror.Accept(envelope(6, "b")) // gap between 4 and 6
	if got := mirror.Events(); len(got) != 2 {
		t.Fatalf("events = %#v", got)
	}
	gap := mirror.Gap()
	if gap == nil || gap.ExpectedSeq != 1 || gap.FirstAvailableSeq != 4 {
		t.Fatalf("gap = %#v", gap)
	}
}

func TestMirrorPreservesUpstreamSeq(t *testing.T) {
	mirror := newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 10})
	mirror.Accept(envelope(100, "a"))
	evts := mirror.Events()
	if len(evts) != 1 || evts[0].Seq != 100 {
		t.Fatalf("expected Seq=100, got %#v", evts)
	}
}

func TestMirrorRejectsZeroSeq(t *testing.T) {
	mirror := newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 10})
	e := envelope(0, "a")
	if mirror.Accept(e) {
		t.Fatal("accepted zero-seq event")
	}
}

func TestMirrorRejectsEmptySessionID(t *testing.T) {
	mirror := newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 10})
	e := envelope(1, "")
	if mirror.Accept(e) {
		t.Fatal("accepted empty session ID")
	}
}

func TestMirrorRejectsInvalidJSON(t *testing.T) {
	mirror := newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 10})
	e := supervisor.EventEnvelope{Seq: 1, SessionID: "a", Kind: "pi", Payload: json.RawMessage(`not-json`)}
	if mirror.Accept(e) {
		t.Fatal("accepted invalid JSON payload")
	}
}

func TestMirrorCountOnlyEviction(t *testing.T) {
	mirror := newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 3})
	for i := uint64(1); i <= 5; i++ {
		mirror.Accept(envelope(i, "a"))
	}
	evts := mirror.Events()
	if len(evts) != 3 {
		t.Fatalf("expected 3 events after count eviction, got %d", len(evts))
	}
	// Oldest should be seq 3.
	if evts[0].Seq != 3 {
		t.Fatalf("expected oldest seq=3, got %d", evts[0].Seq)
	}
}

func TestMirrorByteOnlyEviction(t *testing.T) {
	// Each envelope with session "a", kind "pi", and payload {"type":"agent_start"} (22 bytes):
	// retainedBytes = 1 + 2 + 22 + 24 = 49 bytes.
	// With MaxBytes=100, exactly 2 envelopes fit (49+49=98 <= 100, 3*49=147 > 100).
	mirror := newEventMirror(supervisor.EventBrokerOptions{MaxBytes: 100})
	mirror.Accept(envelope(1, "a"))
	mirror.Accept(envelope(2, "a"))
	mirror.Accept(envelope(3, "a"))
	evts := mirror.Events()
	if len(evts) != 2 {
		t.Fatalf("expected 2 events after byte eviction, got %d: %#v", len(evts), evts)
	}
}

func TestMirrorOnlyRecordsFirstGap(t *testing.T) {
	mirror := newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 100})
	mirror.Accept(envelope(3, "a")) // gap 1-2
	mirror.Accept(envelope(7, "b")) // second gap 4-6 — must not overwrite first
	gap := mirror.Gap()
	if gap == nil || gap.ExpectedSeq != 1 || gap.FirstAvailableSeq != 3 {
		t.Fatalf("gap = %#v", gap)
	}
}

func TestMirrorPayloadIsCloned(t *testing.T) {
	mirror := newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 10})
	original := json.RawMessage(`{"type":"agent_start"}`)
	e := supervisor.EventEnvelope{Seq: 1, SessionID: "a", Kind: "pi", Payload: original}
	mirror.Accept(e)
	original[0] = 'X' // mutate original
	evts := mirror.Events()
	if evts[0].Payload[0] == 'X' {
		t.Fatal("payload was not cloned on Accept")
	}
}

func TestMirrorEventsForFilters(t *testing.T) {
	mirror := newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 20})
	mirror.Accept(envelope(1, "a"))
	mirror.Accept(envelope(2, "b"))
	mirror.Accept(envelope(3, "a"))
	got := mirror.EventsFor("a")
	if len(got) != 2 {
		t.Fatalf("EventsFor(a) = %d, want 2", len(got))
	}
	for _, e := range got {
		if e.SessionID != "a" {
			t.Fatalf("unexpected session %q in filtered events", e.SessionID)
		}
	}
}

func TestMirrorReconnectDeduplicatesRetainedReplay(t *testing.T) {
	mirror := newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 20})
	// First connection: accept seq 1-3.
	mirror.Accept(envelope(1, "a"))
	mirror.Accept(envelope(2, "a"))
	mirror.Accept(envelope(3, "a"))
	// Simulate reconnect: replay comes back from seq 2 (overlap).
	mirror.Accept(envelope(2, "a")) // duplicate — rejected
	mirror.Accept(envelope(3, "a")) // duplicate — rejected
	mirror.Accept(envelope(4, "a")) // new event — accepted
	evts := mirror.Events()
	if len(evts) != 4 {
		t.Fatalf("expected 4 events after reconnect dedup, got %d", len(evts))
	}
}
