package eventmailbox

import (
	"errors"
	"fmt"
	"sync"
)

var (
	ErrClosed = errors.New("event mailbox is closed")
	ErrFull   = errors.New("event mailbox exceeded bounded capacity")
)

type Limits struct {
	MaxEvents int
	MaxBytes  int
}

type state uint8

const (
	stateOpen state = iota
	stateDraining
	stateAborted
)

type entry[T any] struct {
	value T
	bytes int
}

type Mailbox[T any] struct {
	mu            sync.Mutex
	ready         *sync.Cond
	limits        Limits
	state         state
	queue         []entry[T]
	retainedBytes int
	events        chan T
	abort         chan struct{}
	done          chan struct{}
}

func New[T any](limits Limits) (*Mailbox[T], error) {
	if limits.MaxEvents < 0 || limits.MaxBytes < 0 {
		return nil, fmt.Errorf("event mailbox limits must be nonnegative")
	}
	if limits.MaxEvents == 0 && limits.MaxBytes == 0 {
		return nil, fmt.Errorf("event mailbox requires at least one positive limit")
	}
	mailbox := &Mailbox[T]{
		limits: limits,
		events: make(chan T),
		abort:  make(chan struct{}),
		done:   make(chan struct{}),
	}
	mailbox.ready = sync.NewCond(&mailbox.mu)
	go mailbox.dispatch()
	return mailbox, nil
}

func (mailbox *Mailbox[T]) Events() <-chan T      { return mailbox.events }
func (mailbox *Mailbox[T]) Done() <-chan struct{} { return mailbox.done }

func (mailbox *Mailbox[T]) Send(value T, retainedBytes int) error {
	if retainedBytes < 0 {
		return fmt.Errorf("event mailbox retained bytes must be nonnegative")
	}

	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()

	if mailbox.state != stateOpen {
		return ErrClosed
	}
	if mailbox.limits.MaxEvents > 0 && len(mailbox.queue) >= mailbox.limits.MaxEvents {
		return ErrFull
	}
	if mailbox.limits.MaxBytes > 0 && retainedBytes > mailbox.limits.MaxBytes-mailbox.retainedBytes {
		return ErrFull
	}

	mailbox.queue = append(mailbox.queue, entry[T]{value: value, bytes: retainedBytes})
	mailbox.retainedBytes += retainedBytes
	mailbox.ready.Signal()
	return nil
}

func (mailbox *Mailbox[T]) Close() {
	mailbox.mu.Lock()
	if mailbox.state == stateOpen {
		mailbox.state = stateDraining
		mailbox.ready.Broadcast()
	}
	mailbox.mu.Unlock()
}

func (mailbox *Mailbox[T]) Abort() {
	mailbox.mu.Lock()
	if mailbox.state != stateAborted {
		mailbox.state = stateAborted
		close(mailbox.abort)
		mailbox.clearQueue()
		mailbox.ready.Broadcast()
	}
	mailbox.mu.Unlock()

	<-mailbox.done
}

func (mailbox *Mailbox[T]) dispatch() {
	for {
		mailbox.mu.Lock()
		for mailbox.state == stateOpen && len(mailbox.queue) == 0 {
			mailbox.ready.Wait()
		}

		if mailbox.state == stateAborted {
			mailbox.clearQueue()
			mailbox.mu.Unlock()
			mailbox.finish()
			return
		}
		if len(mailbox.queue) == 0 {
			mailbox.mu.Unlock()
			mailbox.finish()
			return
		}

		head := mailbox.queue[0]
		mailbox.mu.Unlock()

		select {
		case mailbox.events <- head.value:
			mailbox.mu.Lock()
			if mailbox.state != stateAborted {
				var zero entry[T]
				mailbox.queue[0] = zero
				mailbox.queue = mailbox.queue[1:]
				mailbox.retainedBytes -= head.bytes
			}
			mailbox.mu.Unlock()
		case <-mailbox.abort:
		}
	}
}

func (mailbox *Mailbox[T]) clearQueue() {
	var zero entry[T]
	for index := range mailbox.queue {
		mailbox.queue[index] = zero
	}
	mailbox.queue = nil
	mailbox.retainedBytes = 0
}

func (mailbox *Mailbox[T]) finish() {
	close(mailbox.events)
	close(mailbox.done)
}
