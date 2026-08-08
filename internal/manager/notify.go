package manager

import "sync"

// changeFanout distributes monotonically increasing revision numbers to
// registered subscribers. A subscriber whose mailbox is full is disconnected
// immediately. Close is idempotent.
type changeFanout struct {
	mu          sync.Mutex
	mailboxCap  int
	nextID      uint64
	subscribers map[uint64]*changeMailbox
}

type changeMailbox struct {
	mu      sync.Mutex
	updates chan uint64
	closed  bool
}

func newChangeFanout(mailboxCap int) *changeFanout {
	if mailboxCap < 1 {
		mailboxCap = 1
	}
	return &changeFanout{
		mailboxCap:  mailboxCap,
		subscribers: make(map[uint64]*changeMailbox),
	}
}

// Subscribe creates a new subscriber. The returned ChangeSubscription.Updates
// channel is closed when the subscriber is disconnected.
func (f *changeFanout) Subscribe() ChangeSubscription {
	f.mu.Lock()
	f.nextID++
	id := f.nextID
	mailbox := &changeMailbox{updates: make(chan uint64, f.mailboxCap)}
	f.subscribers[id] = mailbox
	f.mu.Unlock()

	var once sync.Once
	closeFunc := func() {
		once.Do(func() {
			f.mu.Lock()
			if f.subscribers[id] == mailbox {
				delete(f.subscribers, id)
			}
			f.mu.Unlock()
			mailbox.close()
		})
	}
	return ChangeSubscription{
		Updates: mailbox.updates,
		Close:   closeFunc,
	}
}

// Publish sends revision to all subscribers. Slow subscribers (full mailbox)
// are disconnected and their channel is closed.
func (f *changeFanout) Publish(revision uint64) {
	f.mu.Lock()
	var overflow []*changeMailbox
	for id, mailbox := range f.subscribers {
		select {
		case mailbox.updates <- revision:
		default:
			delete(f.subscribers, id)
			overflow = append(overflow, mailbox)
		}
	}
	f.mu.Unlock()

	for _, mailbox := range overflow {
		mailbox.close()
	}
}

// Close disconnects all subscribers.
func (f *changeFanout) Close() {
	f.mu.Lock()
	mailboxes := make([]*changeMailbox, 0, len(f.subscribers))
	for _, mailbox := range f.subscribers {
		mailboxes = append(mailboxes, mailbox)
	}
	clear(f.subscribers)
	f.mu.Unlock()

	for _, mailbox := range mailboxes {
		mailbox.close()
	}
}

// close closes the mailbox channel exactly once.
func (m *changeMailbox) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	close(m.updates)
}
