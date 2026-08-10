package supervisor

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/sklarsa/kanedias/internal/config"
)

const (
	// DefaultEventRingCapacity and DefaultEventRingByteCapacity alias the
	// config-owned constants so the two default paths cannot drift.
	DefaultEventRingCapacity     = config.DefaultSupervisorEventMaxEvents
	DefaultEventRingByteCapacity = config.DefaultSupervisorEventMaxBytes

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
	Err    func() error
}

type EventBroker struct {
	publishMu   sync.Mutex
	mu          sync.Mutex
	ring        []EventEnvelope
	ringCap     int
	ringBytes   int
	byteCap     int
	mailCap     int
	mailByteCap int
	nextSeq     uint64
	sourceSeq   map[string]uint64
	nextSubID   uint64
	subs        map[uint64]*eventMailbox
	cloneReplay func([]EventEnvelope) []EventEnvelope
	closed      bool
}

type eventMailbox struct {
	mu         sync.Mutex
	events     chan EventEnvelope
	sizes      []int // FIFO of retained byte sizes for events still buffered on events
	totalBytes int
	maxEvents  int
	maxBytes   int
	closed     bool
}

type eventSubscriber struct {
	id      uint64
	mailbox *eventMailbox
}

func NewEventBroker() *EventBroker {
	return newEventBrokerWithByteCapacity(DefaultEventRingCapacity, DefaultSubscriberMailboxCapacity, DefaultEventRingByteCapacity)
}

// EventBrokerOptions configures independent broker eviction limits. A zero
// field disables that limit; at least one limit must be positive.
type EventBrokerOptions struct {
	MaxEvents int
	MaxBytes  int
}

// NewEventBrokerWithOptions constructs a broker with configured, independent
// limits. A zero field disables that limit; a negative field is rejected. At
// least one limit must be positive.
func NewEventBrokerWithOptions(options EventBrokerOptions) (*EventBroker, error) {
	if options.MaxEvents < 0 {
		return nil, fmt.Errorf("MaxEvents must be >= 0")
	}
	if options.MaxBytes < 0 {
		return nil, fmt.Errorf("MaxBytes must be >= 0")
	}
	if options.MaxEvents == 0 && options.MaxBytes == 0 {
		return nil, fmt.Errorf("at least one of MaxEvents or MaxBytes must be positive")
	}
	return newEventBrokerWithByteCapacity(options.MaxEvents, DefaultSubscriberMailboxCapacity, options.MaxBytes), nil
}

func newEventBroker(ringCapacity, mailboxCapacity int) *EventBroker {
	return newEventBrokerWithByteCapacity(ringCapacity, mailboxCapacity, DefaultEventRingByteCapacity)
}

func newEventBrokerWithByteCapacity(ringCapacity, mailboxCapacity, byteCapacity int) *EventBroker {
	if ringCapacity < 0 {
		ringCapacity = 0
	}
	if mailboxCapacity < 0 {
		mailboxCapacity = 0
	}
	if byteCapacity < 0 {
		byteCapacity = 0
	}
	mailboxByteCapacity := byteCapacity
	if mailboxByteCapacity == 0 {
		mailboxByteCapacity = DefaultEventRingByteCapacity
	}
	return &EventBroker{
		ringCap:     ringCapacity,
		byteCap:     byteCapacity,
		mailCap:     mailboxCapacity,
		mailByteCap: mailboxByteCapacity,
		sourceSeq:   make(map[string]uint64),
		subs:        make(map[uint64]*eventMailbox),
		cloneReplay: cloneEnvelopes,
	}
}

func (broker *EventBroker) PublishLocal(sessionID, kind string, payload json.RawMessage) EventEnvelope {
	broker.publishMu.Lock()
	defer broker.publishMu.Unlock()

	event := EventEnvelope{SessionID: sessionID, Kind: kind, Payload: cloneRaw(payload)}
	broker.mu.Lock()
	if broker.closed {
		broker.mu.Unlock()
		return cloneEnvelope(event)
	}
	broker.sourceSeq[sessionID]++
	event.SourceSeq = broker.sourceSeq[sessionID]
	event = broker.retainLocked(event)
	subscribers := broker.subscribersLocked()
	broker.mu.Unlock()

	broker.deliver(event, subscribers)
	return cloneEnvelope(event)
}

func (broker *EventBroker) Forward(source EventEnvelope) EventEnvelope {
	broker.publishMu.Lock()
	defer broker.publishMu.Unlock()

	event := EventEnvelope{
		SessionID: source.SessionID,
		SourceSeq: source.SourceSeq,
		Kind:      source.Kind,
		Payload:   cloneRaw(source.Payload),
	}
	broker.mu.Lock()
	if broker.closed {
		broker.mu.Unlock()
		return cloneEnvelope(event)
	}
	event = broker.retainLocked(event)
	subscribers := broker.subscribersLocked()
	broker.mu.Unlock()

	broker.deliver(event, subscribers)
	return cloneEnvelope(event)
}

func (broker *EventBroker) Subscribe() Subscription {
	// Serialize the replay/live cut with publication without extending the state
	// lock across payload cloning or mailbox delivery.
	broker.publishMu.Lock()
	broker.mu.Lock()
	if broker.closed {
		broker.mu.Unlock()
		broker.publishMu.Unlock()
		events := make(chan EventEnvelope)
		close(events)
		return Subscription{Replay: []EventEnvelope{}, Events: events, Close: func() {}}
	}
	replaySnapshot := append([]EventEnvelope(nil), broker.ring...)
	broker.nextSubID++
	id := broker.nextSubID
	mailbox := newEventMailbox(broker.mailCap, broker.mailByteCap)
	broker.subs[id] = mailbox
	broker.mu.Unlock()
	broker.publishMu.Unlock()
	// Ring payloads are immutable after retention, so the expensive deep copy
	// is safe after releasing both publication/state locks.
	replay := broker.cloneReplay(replaySnapshot)

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

// Close terminates every active subscription and permanently rejects new
// subscriptions. It is safe to call concurrently and more than once.
func (broker *EventBroker) Close() {
	broker.publishMu.Lock()
	broker.mu.Lock()
	if broker.closed {
		broker.mu.Unlock()
		broker.publishMu.Unlock()
		return
	}
	broker.closed = true
	mailboxes := make([]*eventMailbox, 0, len(broker.subs))
	for _, mailbox := range broker.subs {
		mailboxes = append(mailboxes, mailbox)
	}
	clear(broker.subs)
	broker.mu.Unlock()
	broker.publishMu.Unlock()

	for _, mailbox := range mailboxes {
		mailbox.close()
	}
}

func (broker *EventBroker) retainLocked(event EventEnvelope) EventEnvelope {
	broker.nextSeq++
	event.Seq = broker.nextSeq
	if broker.ringCap > 0 || broker.byteCap > 0 {
		broker.ring = append(broker.ring, event)
		broker.ringBytes += retainedEventBytes(event)
		for (broker.ringCap > 0 && len(broker.ring) > broker.ringCap) ||
			(broker.byteCap > 0 && broker.ringBytes > broker.byteCap) {
			broker.ringBytes -= retainedEventBytes(broker.ring[0])
			broker.ring[0] = EventEnvelope{}
			broker.ring = broker.ring[1:]
		}
	}
	return event
}

func retainedEventBytes(event EventEnvelope) int {
	return len(event.SessionID) + len(event.Kind) + len(event.Payload) + 24
}

// SourceBoundary returns the last published source sequence at a cut ordered
// against publication. Consumers can ignore retained/pre-generation records.
func (broker *EventBroker) SourceBoundary(sessionID string) uint64 {
	broker.publishMu.Lock()
	broker.mu.Lock()
	sequence := broker.sourceSeq[sessionID]
	broker.mu.Unlock()
	broker.publishMu.Unlock()
	return sequence
}

func (broker *EventBroker) subscribersLocked() []eventSubscriber {
	subscribers := make([]eventSubscriber, 0, len(broker.subs))
	for id, mailbox := range broker.subs {
		subscribers = append(subscribers, eventSubscriber{id: id, mailbox: mailbox})
	}
	return subscribers
}

func (broker *EventBroker) deliver(event EventEnvelope, subscribers []eventSubscriber) {
	var overflowed []eventSubscriber
	for _, subscriber := range subscribers {
		if subscriber.mailbox.send(cloneEnvelope(event)) {
			continue
		}
		overflowed = append(overflowed, subscriber)
	}
	if len(overflowed) == 0 {
		return
	}

	broker.mu.Lock()
	for _, subscriber := range overflowed {
		if broker.subs[subscriber.id] == subscriber.mailbox {
			delete(broker.subs, subscriber.id)
		}
	}
	broker.mu.Unlock()
	for _, subscriber := range overflowed {
		subscriber.mailbox.abort()
	}
}

func (broker *EventBroker) removeSubscriber(id uint64, mailbox *eventMailbox) {
	broker.mu.Lock()
	if broker.subs[id] == mailbox {
		delete(broker.subs, id)
	}
	broker.mu.Unlock()
	mailbox.abort()
}

func newEventMailbox(maxEvents, maxBytes int) *eventMailbox {
	return &eventMailbox{
		// events is the single bounded store: channel capacity maxEvents bounds
		// total retained count, and sizes/totalBytes bound retained bytes. There
		// is no worker goroutine; sends are nonblocking while holding mu.
		events:    make(chan EventEnvelope, maxEvents),
		sizes:     make([]int, 0, maxEvents),
		maxEvents: maxEvents,
		maxBytes:  maxBytes,
	}
}

func (mailbox *eventMailbox) send(event EventEnvelope) bool {
	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	// Reconcile entries the consumer has already read off the buffered channel.
	// Events are only removed from the channel by the consumer, so sizes can only
	// outrun len(events); dropping the leading consumed sizes keeps accounting
	// conservative and never negative.
	if consumed := len(mailbox.sizes) - len(mailbox.events); consumed > 0 {
		for _, size := range mailbox.sizes[:consumed] {
			mailbox.totalBytes -= size
		}
		mailbox.sizes = mailbox.sizes[consumed:]
	}
	if mailbox.closed {
		return false
	}
	if mailbox.maxEvents > 0 && len(mailbox.sizes) >= mailbox.maxEvents {
		return false
	}
	eventBytes := retainedEventBytes(event)
	if mailbox.maxBytes > 0 && mailbox.totalBytes+eventBytes > mailbox.maxBytes {
		return false
	}
	select {
	case mailbox.events <- event:
		mailbox.sizes = append(mailbox.sizes, eventBytes)
		mailbox.totalBytes += eventBytes
		return true
	default:
		return false
	}
}

// close gracefully terminates the mailbox for broker shutdown: it stops
// accepting new events and closes the channel WITHOUT draining, so buffered
// events already accepted remain readable by the consumer. This preserves the
// final buffered-event delivery contract required of broker.Close.
func (mailbox *eventMailbox) close() {
	mailbox.mu.Lock()
	if !mailbox.closed {
		mailbox.closed = true
		close(mailbox.events)
	}
	mailbox.mu.Unlock()
}

// abort terminates a mailbox for an individual subscription close or overflow
// detachment: it drains any buffered unread events (releasing their payload and
// byte count) and closes the channel promptly without requiring a consumer read.
func (mailbox *eventMailbox) abort() {
	mailbox.mu.Lock()
	wasClosed := mailbox.closed
	mailbox.closed = true
	for {
		select {
		case _, open := <-mailbox.events:
			if open {
				continue
			}
			// A graceful broker close won the race. Its closed channel can still
			// contain buffered events, all of which have now been discarded.
			mailbox.totalBytes = 0
			mailbox.sizes = nil
			mailbox.mu.Unlock()
			return
		default:
			mailbox.totalBytes = 0
			mailbox.sizes = nil
			if !wasClosed {
				close(mailbox.events)
			}
			mailbox.mu.Unlock()
			return
		}
	}
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
