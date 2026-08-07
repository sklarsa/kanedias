package supervisor

import (
	"encoding/json"
	"sync"
)

const (
	DefaultEventRingCapacity         = 4_096
	DefaultSubscriberMailboxCapacity = 128
)

type EventEnvelope struct {
	Seq       uint64          `json:"seq"`
	SessionID string          `json:"sessionId"`
	SourceSeq uint64          `json:"sourceSeq"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
}

type Subscription struct {
	Replay []EventEnvelope
	Events <-chan EventEnvelope
	Close  func()
}

type EventBroker struct {
	mu        sync.Mutex
	ring      []EventEnvelope
	ringCap   int
	mailCap   int
	nextSeq   uint64
	sourceSeq map[string]uint64
	nextSubID uint64
	subs      map[uint64]*eventMailbox
}

type eventMailbox struct {
	mu     sync.Mutex
	events chan EventEnvelope
	closed bool
}

func NewEventBroker() *EventBroker {
	return newEventBroker(DefaultEventRingCapacity, DefaultSubscriberMailboxCapacity)
}

func newEventBroker(ringCapacity, mailboxCapacity int) *EventBroker {
	if ringCapacity < 0 {
		ringCapacity = 0
	}
	if mailboxCapacity < 0 {
		mailboxCapacity = 0
	}
	return &EventBroker{
		ringCap:   ringCapacity,
		mailCap:   mailboxCapacity,
		sourceSeq: make(map[string]uint64),
		subs:      make(map[uint64]*eventMailbox),
	}
}

func (broker *EventBroker) PublishLocal(sessionID, kind string, payload json.RawMessage) EventEnvelope {
	broker.mu.Lock()
	broker.sourceSeq[sessionID]++
	event := EventEnvelope{
		SessionID: sessionID,
		SourceSeq: broker.sourceSeq[sessionID],
		Kind:      kind,
		Payload:   cloneRaw(payload),
	}
	event, subscribers := broker.retainLocked(event)
	broker.mu.Unlock()

	broker.deliver(event, subscribers)
	return cloneEnvelope(event)
}

func (broker *EventBroker) Forward(source EventEnvelope) EventEnvelope {
	broker.mu.Lock()
	event := EventEnvelope{
		SessionID: source.SessionID,
		SourceSeq: source.SourceSeq,
		Kind:      source.Kind,
		Payload:   cloneRaw(source.Payload),
	}
	event, subscribers := broker.retainLocked(event)
	broker.mu.Unlock()

	broker.deliver(event, subscribers)
	return cloneEnvelope(event)
}

func (broker *EventBroker) Subscribe() Subscription {
	broker.mu.Lock()
	replay := cloneEnvelopes(broker.ring)
	broker.nextSubID++
	id := broker.nextSubID
	mailbox := &eventMailbox{events: make(chan EventEnvelope, broker.mailCap)}
	broker.subs[id] = mailbox
	broker.mu.Unlock()

	var once sync.Once
	return Subscription{
		Replay: replay,
		Events: mailbox.events,
		Close: func() {
			once.Do(func() {
				broker.removeSubscriber(id, mailbox)
			})
		},
	}
}

func (broker *EventBroker) retainLocked(event EventEnvelope) (EventEnvelope, map[uint64]*eventMailbox) {
	broker.nextSeq++
	event.Seq = broker.nextSeq
	if broker.ringCap > 0 {
		if len(broker.ring) == broker.ringCap {
			copy(broker.ring, broker.ring[1:])
			broker.ring[len(broker.ring)-1] = event
		} else {
			broker.ring = append(broker.ring, event)
		}
	}
	subscribers := make(map[uint64]*eventMailbox, len(broker.subs))
	for id, mailbox := range broker.subs {
		subscribers[id] = mailbox
	}
	return event, subscribers
}

func (broker *EventBroker) deliver(event EventEnvelope, subscribers map[uint64]*eventMailbox) {
	for id, mailbox := range subscribers {
		if mailbox.send(cloneEnvelope(event)) {
			continue
		}
		broker.removeSubscriber(id, mailbox)
	}
}

func (broker *EventBroker) removeSubscriber(id uint64, mailbox *eventMailbox) {
	broker.mu.Lock()
	if broker.subs[id] == mailbox {
		delete(broker.subs, id)
	}
	broker.mu.Unlock()
	mailbox.close()
}

func (mailbox *eventMailbox) send(event EventEnvelope) bool {
	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	if mailbox.closed {
		return false
	}
	select {
	case mailbox.events <- event:
		return true
	default:
		return false
	}
}

func (mailbox *eventMailbox) close() {
	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	if mailbox.closed {
		return
	}
	mailbox.closed = true
	close(mailbox.events)
}

func cloneEnvelopes(events []EventEnvelope) []EventEnvelope {
	cloned := make([]EventEnvelope, len(events))
	for index, event := range events {
		cloned[index] = cloneEnvelope(event)
	}
	return cloned
}

func cloneEnvelope(event EventEnvelope) EventEnvelope {
	event.Payload = cloneRaw(event.Payload)
	return event
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}
