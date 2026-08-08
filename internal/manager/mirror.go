package manager

import (
	"encoding/json"

	"github.com/sklarsa/kanedias/internal/supervisor"
)

// eventMirror is a bounded in-memory ring of EventEnvelopes that mirrors the
// upstream root's replay stream. Unlike the supervisor's EventBroker it never
// assigns a new sequence — it preserves upstream Seq values as the stable
// identity. Concurrent access must be externally serialised.
type eventMirror struct {
	maxEvents int
	maxBytes  int
	events    []supervisor.EventEnvelope
	bytes     int
	lastSeq   uint64
	gap       *ReplayGap
}

func newEventMirror(opts supervisor.EventBrokerOptions) *eventMirror {
	return &eventMirror{
		maxEvents: opts.MaxEvents,
		maxBytes:  opts.MaxBytes,
	}
}

// Accept attempts to add event to the mirror. It returns false and is a no-op
// for:
//   - sequences at or below the last accepted sequence (deduplication / replay);
//   - zero Seq, empty SessionID, empty Kind, or invalid JSON payload.
//
// When a gap is detected (skipped sequences) the first gap is recorded; later
// gaps are ignored.
func (m *eventMirror) Accept(event supervisor.EventEnvelope) bool {
	if event.Seq == 0 || event.SessionID == "" || event.Kind == "" {
		return false
	}
	if len(event.Payload) > 0 {
		if !json.Valid(event.Payload) {
			return false
		}
	}
	if event.Seq <= m.lastSeq {
		return false
	}
	if event.Seq > m.lastSeq+1 && m.gap == nil {
		m.gap = &ReplayGap{ExpectedSeq: m.lastSeq + 1, FirstAvailableSeq: event.Seq}
	}
	m.lastSeq = event.Seq
	m.events = append(m.events, cloneEnvelope(event))
	m.bytes += retainedBytes(event)
	m.evict()
	return true
}

// Events returns a snapshot of the retained events. The returned slice is
// owned by the caller; payloads are cloned.
func (m *eventMirror) Events() []supervisor.EventEnvelope {
	cloned := make([]supervisor.EventEnvelope, len(m.events))
	for i, e := range m.events {
		cloned[i] = cloneEnvelope(e)
	}
	return cloned
}

// EventsFor returns retained events whose SessionID matches sessionID.
func (m *eventMirror) EventsFor(sessionID string) []supervisor.EventEnvelope {
	var out []supervisor.EventEnvelope
	for _, e := range m.events {
		if e.SessionID == sessionID {
			out = append(out, cloneEnvelope(e))
		}
	}
	return out
}

// Gap returns the first observed replay gap, or nil if the stream was
// contiguous.
func (m *eventMirror) Gap() *ReplayGap {
	if m.gap == nil {
		return nil
	}
	copy := *m.gap
	return &copy
}

// LastSeq returns the highest sequence number accepted so far.
func (m *eventMirror) LastSeq() uint64 {
	return m.lastSeq
}

func (m *eventMirror) evict() {
	for len(m.events) > 0 {
		overCount := m.maxEvents > 0 && len(m.events) > m.maxEvents
		overBytes := m.maxBytes > 0 && m.bytes > m.maxBytes
		if !overCount && !overBytes {
			break
		}
		m.bytes -= retainedBytes(m.events[0])
		m.events[0] = supervisor.EventEnvelope{}
		m.events = m.events[1:]
	}
}

func retainedBytes(e supervisor.EventEnvelope) int {
	return len(e.SessionID) + len(e.Kind) + len(e.Payload) + 24
}

func cloneEnvelope(e supervisor.EventEnvelope) supervisor.EventEnvelope {
	e.Payload = append(json.RawMessage(nil), e.Payload...)
	return e
}
