package eventmailbox

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrClosed = errors.New("event mailbox is closed")
	ErrFull   = errors.New("event mailbox exceeded bounded capacity")
)

const (
	initialDeliveryProbeDelay = 100 * time.Microsecond
	maxDeliveryProbeDelay     = 10 * time.Millisecond
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
	limits        Limits
	state         state
	queue         []entry[T]
	retainedBytes int
	events        chan T
	wake          chan struct{}
	abort         chan struct{}
	done          chan struct{}
	afterHandoff  func()
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
		wake:   make(chan struct{}, 1),
		abort:  make(chan struct{}),
		done:   make(chan struct{}),
	}
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
	if mailbox.state != stateOpen {
		mailbox.mu.Unlock()
		return ErrClosed
	}
	if mailbox.limits.MaxEvents > 0 && len(mailbox.queue) >= mailbox.limits.MaxEvents {
		mailbox.mu.Unlock()
		return ErrFull
	}
	if mailbox.limits.MaxBytes > 0 && retainedBytes > mailbox.limits.MaxBytes-mailbox.retainedBytes {
		mailbox.mu.Unlock()
		return ErrFull
	}

	mailbox.queue = append(mailbox.queue, entry[T]{value: value, bytes: retainedBytes})
	mailbox.retainedBytes += retainedBytes
	mailbox.mu.Unlock()
	mailbox.signal()
	return nil
}

func (mailbox *Mailbox[T]) Close() {
	mailbox.mu.Lock()
	if mailbox.state == stateOpen {
		mailbox.state = stateDraining
	}
	mailbox.mu.Unlock()
	mailbox.signal()
}

func (mailbox *Mailbox[T]) Abort() {
	mailbox.mu.Lock()
	if mailbox.state != stateAborted {
		mailbox.state = stateAborted
		close(mailbox.abort)
		mailbox.clearQueue()
	}
	mailbox.mu.Unlock()
	mailbox.signal()

	<-mailbox.done
}

func (mailbox *Mailbox[T]) dispatch() {
	retryDelay := initialDeliveryProbeDelay
	for {
		mailbox.mu.Lock()
		if mailbox.state == stateAborted {
			mailbox.clearQueue()
			mailbox.mu.Unlock()
			mailbox.finish()
			return
		}
		if len(mailbox.queue) == 0 {
			if mailbox.state == stateDraining {
				mailbox.mu.Unlock()
				mailbox.finish()
				return
			}
			mailbox.mu.Unlock()
			retryDelay = initialDeliveryProbeDelay
			select {
			case <-mailbox.wake:
			case <-mailbox.abort:
			}
			continue
		}

		head := mailbox.queue[0]
		delivered := false
		select {
		case mailbox.events <- head.value:
			if mailbox.afterHandoff != nil {
				mailbox.afterHandoff()
			}
			var zero entry[T]
			mailbox.queue[0] = zero
			mailbox.queue = mailbox.queue[1:]
			mailbox.retainedBytes -= head.bytes
			delivered = true
		default:
		}
		mailbox.mu.Unlock()

		if delivered {
			retryDelay = initialDeliveryProbeDelay
			continue
		}
		mailbox.waitForProbe(retryDelay)
		if retryDelay < maxDeliveryProbeDelay {
			retryDelay *= 2
			if retryDelay > maxDeliveryProbeDelay {
				retryDelay = maxDeliveryProbeDelay
			}
		}
	}
}

func (mailbox *Mailbox[T]) signal() {
	select {
	case mailbox.wake <- struct{}{}:
	default:
	}
}

func (mailbox *Mailbox[T]) waitForProbe(delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-mailbox.abort:
	case <-mailbox.wake:
	case <-timer.C:
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
