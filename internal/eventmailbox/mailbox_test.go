package eventmailbox

import (
	"errors"
	"runtime"
	"testing"
	"time"
)

func TestMailboxAcceptsBurstAndDeliversFIFO(t *testing.T) {
	mailbox, err := New[int](Limits{MaxEvents: 4, MaxBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer mailbox.Abort()

	for _, value := range []int{1, 2, 3, 4} {
		if err := mailbox.Send(value, 4); err != nil {
			t.Fatalf("Send(%d) error = %v", value, err)
		}
	}
	for _, want := range []int{1, 2, 3, 4} {
		select {
		case got := <-mailbox.Events():
			if got != want {
				t.Fatalf("event = %d, want %d", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %d", want)
		}
	}
}

func TestMailboxRejectsCountAndByteOverflow(t *testing.T) {
	tests := []struct {
		name   string
		limits Limits
		sizes  []int
	}{
		{name: "count", limits: Limits{MaxEvents: 2, MaxBytes: 100}, sizes: []int{1, 1, 1}},
		{name: "bytes", limits: Limits{MaxEvents: 10, MaxBytes: 5}, sizes: []int{3, 3}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mailbox, err := New[int](test.limits)
			if err != nil {
				t.Fatal(err)
			}
			defer mailbox.Abort()
			for index, size := range test.sizes {
				err = mailbox.Send(index, size)
				if index < len(test.sizes)-1 && err != nil {
					t.Fatalf("accepted Send(%d) error = %v", index, err)
				}
			}
			if !errors.Is(err, ErrFull) {
				t.Fatalf("overflow error = %v, want ErrFull", err)
			}
		})
	}
}

func TestMailboxDeliveryReleasesByteCapacity(t *testing.T) {
	mailbox, err := New[int](Limits{MaxEvents: 2, MaxBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer mailbox.Abort()
	if err := mailbox.Send(1, 4); err != nil {
		t.Fatal(err)
	}
	if got := <-mailbox.Events(); got != 1 {
		t.Fatalf("event = %d", got)
	}
	deadline := time.Now().Add(time.Second)
	for {
		err = mailbox.Send(2, 4)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrFull) || time.Now().After(deadline) {
			t.Fatalf("capacity was not released after receive: %v", err)
		}
		runtime.Gosched()
	}
}

func TestMailboxGracefulCloseDrainsAcceptedEvents(t *testing.T) {
	mailbox, err := New[int](Limits{MaxEvents: 3, MaxBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []int{1, 2, 3} {
		if err := mailbox.Send(value, 1); err != nil {
			t.Fatal(err)
		}
	}
	mailbox.Close()
	var got []int
	for value := range mailbox.Events() {
		got = append(got, value)
	}
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("drained events = %v, want [1 2 3]", got)
	}
}

func TestMailboxAbortClosesUnreadConsumerPromptly(t *testing.T) {
	mailbox, err := New[int](Limits{MaxEvents: 3, MaxBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := mailbox.Send(1, 1); err != nil {
		t.Fatal(err)
	}
	returned := make(chan struct{})
	go func() { mailbox.Abort(); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Abort blocked behind unread output")
	}
	if _, open := <-mailbox.Events(); open {
		t.Fatal("aborted mailbox output remains open")
	}
}

func TestMailboxCloseAndAbortRace(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		mailbox, err := New[int](Limits{MaxEvents: 1, MaxBytes: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := mailbox.Send(iteration, 1); err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		closeReturned := make(chan struct{})
		abortReturned := make(chan struct{})
		go func() {
			<-start
			mailbox.Close()
			close(closeReturned)
		}()
		go func() {
			<-start
			mailbox.Abort()
			close(abortReturned)
		}()
		close(start)

		for name, returned := range map[string]<-chan struct{}{
			"Close": closeReturned,
			"Abort": abortReturned,
		} {
			select {
			case <-returned:
			case <-time.After(time.Second):
				t.Fatalf("iteration %d: %s did not return", iteration, name)
			}
		}
		select {
		case <-mailbox.Done():
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: Done did not close", iteration)
		}
		if err := mailbox.Send(iteration, 1); !errors.Is(err, ErrClosed) {
			t.Fatalf("iteration %d: Send error = %v, want ErrClosed", iteration, err)
		}
	}
}
