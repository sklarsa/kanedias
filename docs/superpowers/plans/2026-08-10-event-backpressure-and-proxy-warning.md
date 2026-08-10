# Event Backpressure and Proxy Warning Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prevent ordinary Pi event bursts from disconnecting supervised sessions or creating replay gaps while retaining strict memory bounds and privacy-safe proxy diagnostics.

**Architecture:** Introduce a reusable generic FIFO mailbox whose queued and in-flight events are bounded by count and bytes, then use it at Pi RPC, descendant SSE, and broker subscriber boundaries. Preserve existing public channel APIs and terminal overflow semantics. Classify goproxy diagnostics from constant format strings, suppress expected teardown errors by error identity, and never render diagnostic arguments.

**Tech Stack:** Go 1.24, goroutines, channels, `sync.Cond`, JSON-line Pi RPC, SSE, `log/slog`, table-driven Go tests.

## Global Constraints

- Mailboxes admit at most 4,096 retained events and 16 MiB retained event data in production defaults.
- The byte count includes the event blocked in output delivery; it is released only after the consumer receives that event.
- Producers never wait for event consumers; overflow remains terminal for the affected transport or subscription.
- Graceful close drains accepted events in FIFO order; abort drops unread events and closes promptly.
- Pi RPC responses continue to bypass event buffering and settle pending calls immediately.
- Proxy diagnostics never log hosts, URLs, headers, credentials, raw error text, or formatted goproxy arguments.
- Expected cancellation, EOF, unexpected EOF, closed-connection, reset, and broken-pipe teardown does not emit warning-level proxy logs.
- Every production change follows red-green-refactor; run and observe the specified failing test before implementation.

---

## File Structure

- Create `internal/eventmailbox/mailbox.go`: generic count-and-byte-bounded FIFO with graceful close and abort.
- Create `internal/eventmailbox/mailbox_test.go`: behavioral ordering, bounds, close, abort, and concurrency tests.
- Modify `internal/supervisor/pirpc/client.go`: replace the one-entry event channel with the bounded mailbox.
- Modify `internal/supervisor/pirpc/client_test.go`: reproduce ordinary burst survival and genuine overflow.
- Modify `internal/supervisorapi/client.go`: buffer parsed descendant SSE events with the bounded mailbox.
- Modify `internal/supervisorapi/handler_test.go`: reproduce SSE burst survival, overflow, and close semantics through a real Unix HTTP server.
- Modify `internal/supervisor/events.go`: wrap broker subscriber delivery in the bounded mailbox and use replay-derived production limits.
- Modify `internal/supervisor/events_test.go`: replace private channel-accounting assertions with observable delivery and teardown behavior; add the 128-event replay-gap regression.
- Modify `internal/manager/monitor_test.go`: strengthen retained-overlap reconnection coverage by asserting no false gap.
- Modify `internal/proxy/observability.go`: classify safe proxy diagnostic categories and suppress expected teardown errors.
- Modify `internal/proxy/observability_test.go`: test classifications, suppression, and privacy directly.
- Modify `internal/proxy/observability_integration_test.go`: assert the real MITM failure receives the safe `upstream_read` class.

---

### Task 1: Generic Bounded Event Mailbox

**Files:**
- Create: `internal/eventmailbox/mailbox.go`
- Create: `internal/eventmailbox/mailbox_test.go`

**Interfaces:**
- Produces: `Limits{MaxEvents int, MaxBytes int}`.
- Produces: sentinel errors `ErrClosed` and `ErrFull`.
- Produces: `New[T any](Limits) (*Mailbox[T], error)`.
- Produces: `(*Mailbox[T]).Events() <-chan T`.
- Produces: `(*Mailbox[T]).Send(value T, retainedBytes int) error`.
- Produces: `(*Mailbox[T]).Close()` for graceful draining.
- Produces: `(*Mailbox[T]).Abort()` for prompt discard and close.
- Produces: `(*Mailbox[T]).Done() <-chan struct{}`.
- Depends only on the Go standard library.

- [ ] **Step 1: Write failing ordering and capacity tests**

Create `internal/eventmailbox/mailbox_test.go`. The first tests must use the real mailbox API and literal expectations:

```go
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
```

These tests catch a mailbox that still uses a one-entry channel, delivers out of order, ignores its count bound, or ignores its byte bound.

- [ ] **Step 2: Run the new package test and verify RED**

Run:

```bash
go test ./internal/eventmailbox
```

Expected: compile failure because `New`, `Limits`, `Mailbox`, `ErrClosed`, and `ErrFull` do not exist.

- [ ] **Step 3: Implement the minimal mailbox core**

Create `internal/eventmailbox/mailbox.go` with this shape:

```go
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

func (mailbox *Mailbox[T]) Events() <-chan T { return mailbox.events }
func (mailbox *Mailbox[T]) Done() <-chan struct{} { return mailbox.done }
```

Implement `Send` so it validates `retainedBytes >= 0`, rejects non-open state with `ErrClosed`, checks both enabled limits, appends one `entry`, increments `retainedBytes`, and signals `ready` without waiting for a receiver.

Implement `dispatch` so the head entry remains in `queue` and remains charged while the output send is blocked. After a successful receive, lock again and remove/decharge the head unless an abort won concurrently. When a graceful close has drained the queue, close `events` and `done`. When abort is observed, clear the queue and counters, then close `events` and `done`.

Implement `Close` as an idempotent `stateOpen -> stateDraining` transition plus `ready.Broadcast`. Implement `Abort` as an idempotent transition to `stateAborted`, close the abort signal once, clear retained state, broadcast, unlock, and wait for `done` so callers observe a closed output channel on return.

- [ ] **Step 4: Run the ordering and capacity tests and verify GREEN**

Run:

```bash
go test ./internal/eventmailbox
```

Expected: PASS.

- [ ] **Step 5: Add failing close, release, and concurrency tests**

Add tests with these observable assertions:

```go
func TestMailboxDeliveryReleasesByteCapacity(t *testing.T) {
    mailbox, err := New[int](Limits{MaxEvents: 2, MaxBytes: 4})
    if err != nil { t.Fatal(err) }
    defer mailbox.Abort()
    if err := mailbox.Send(1, 4); err != nil { t.Fatal(err) }
    if got := <-mailbox.Events(); got != 1 { t.Fatalf("event = %d", got) }
    deadline := time.Now().Add(time.Second)
    for {
        err = mailbox.Send(2, 4)
        if err == nil { break }
        if !errors.Is(err, ErrFull) || time.Now().After(deadline) {
            t.Fatalf("capacity was not released after receive: %v", err)
        }
        runtime.Gosched()
    }
}

func TestMailboxGracefulCloseDrainsAcceptedEvents(t *testing.T) {
    mailbox, err := New[int](Limits{MaxEvents: 3, MaxBytes: 3})
    if err != nil { t.Fatal(err) }
    for _, value := range []int{1, 2, 3} {
        if err := mailbox.Send(value, 1); err != nil { t.Fatal(err) }
    }
    mailbox.Close()
    var got []int
    for value := range mailbox.Events() { got = append(got, value) }
    if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
        t.Fatalf("drained events = %v, want [1 2 3]", got)
    }
}

func TestMailboxAbortClosesUnreadConsumerPromptly(t *testing.T) {
    mailbox, err := New[int](Limits{MaxEvents: 3, MaxBytes: 3})
    if err != nil { t.Fatal(err) }
    if err := mailbox.Send(1, 1); err != nil { t.Fatal(err) }
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
```

Add a 100-iteration test racing `Close` and `Abort`, asserting both calls return, `Done` closes, and a later `Send` returns `ErrClosed`. This catches double-close panics, output leaks, and delivery accounting that is released before receive.

- [ ] **Step 6: Run close tests and the race detector**

Run:

```bash
go test ./internal/eventmailbox
go test -race ./internal/eventmailbox
```

Expected: PASS with no race report.

- [ ] **Step 7: Commit Task 1**

```bash
git add internal/eventmailbox/mailbox.go internal/eventmailbox/mailbox_test.go
git commit -m "feat: add byte-bounded event mailbox"
```

---

### Task 2: Pi RPC Burst Tolerance

**Files:**
- Modify: `internal/supervisor/pirpc/client.go`
- Modify: `internal/supervisor/pirpc/client_test.go`

**Interfaces:**
- Consumes: `eventmailbox.New[Event]`, `Send`, `Events`, `Close`, and `Abort` from Task 1.
- Consumes: `config.DefaultSupervisorEventMaxEvents` and `config.DefaultSupervisorEventMaxBytes`.
- Preserves: `NewClient(io.ReadWriteCloser) *Client`, `Events() <-chan Event`, `Done() <-chan struct{}`, and existing overflow error text.
- Produces internally: `newClientWithEventLimits(io.ReadWriteCloser, eventmailbox.Limits) *Client`, used by `NewClient` and package tests.

- [ ] **Step 1: Replace the old one-entry assertion with a failing ordinary-burst regression**

In `internal/supervisor/pirpc/client_test.go`, replace the `cap(client.events) == 1` assertion and two-record overflow fixture in `TestClientDisconnectsStalledEventConsumerAtByteBoundedCapacity`. First add:

```go
func TestClientPreservesOrdinaryEventBurstAndRemainsUsable(t *testing.T) {
    clientConn, peer := net.Pipe()
    client := NewClient(clientConn)
    defer func() { _ = client.Close(); _ = peer.Close() }()

    writeDone := make(chan error, 1)
    go func() {
        writer := bufio.NewWriter(peer)
        for seq := 1; seq <= 256; seq++ {
            if _, err := fmt.Fprintf(writer, "{\"type\":\"message_update\",\"seq\":%d}\n", seq); err != nil {
                writeDone <- err
                return
            }
        }
        writeDone <- writer.Flush()
    }()

    for want := uint64(1); want <= 256; want++ {
        select {
        case event, open := <-client.Events():
            if !open || event.Seq != want {
                t.Fatalf("event %d = %#v, open=%t", want, event, open)
            }
        case <-time.After(time.Second):
            t.Fatalf("timed out at event %d", want)
        }
    }
    if err := <-writeDone; err != nil { t.Fatal(err) }
    if client.Err() != nil { t.Fatalf("client failed during burst: %v", client.Err()) }
}
```

This catches the production regression: the current client terminates on the second adjacent event.

- [ ] **Step 2: Run the Pi RPC burst test and verify RED**

Run:

```bash
go test ./internal/supervisor/pirpc -run TestClientPreservesOrdinaryEventBurstAndRemainsUsable -count=1
```

Expected: FAIL because the event channel closes near sequence 2 and `client.Err()` contains `event consumer exceeded bounded capacity`.

- [ ] **Step 3: Integrate the mailbox into `pirpc.Client`**

Change `Client.events` from `chan Event` to `*eventmailbox.Mailbox[Event]`. Add:

```go
func NewClient(conn io.ReadWriteCloser) *Client {
    return newClientWithEventLimits(conn, eventmailbox.Limits{
        MaxEvents: config.DefaultSupervisorEventMaxEvents,
        MaxBytes:  config.DefaultSupervisorEventMaxBytes,
    })
}
```

The internal constructor must build the mailbox and may panic only if the compile-time production limits are invalid. `Events` returns `client.events.Events()`.

In `readLoop`, remove `defer close(client.events)` and use `defer client.events.Close()`. Replace the nonblocking channel select with:

```go
err = client.events.Send(Event{Seq: sequence, Type: envelope.Type, Raw: raw}, len(raw))
if errors.Is(err, eventmailbox.ErrClosed) {
    return
}
if errors.Is(err, eventmailbox.ErrFull) {
    _ = client.terminate(errors.New("pi RPC event consumer exceeded bounded capacity"))
    return
}
if err != nil {
    _ = client.terminate(fmt.Errorf("queue Pi RPC event: %w", err))
    return
}
```

Keep correlated response dispatch before event enqueue. In `Close`, call `terminate`, abort the event mailbox, wait for `readDone`, and return the first connection-close result exactly as today. This keeps explicit close prompt even if nobody reads `Events`.

- [ ] **Step 4: Run the burst test and verify GREEN**

Run:

```bash
go test ./internal/supervisor/pirpc -run TestClientPreservesOrdinaryEventBurstAndRemainsUsable -count=1
```

Expected: PASS.

- [ ] **Step 5: Restore genuine-overflow and response-bypass coverage with small test limits**

Use `newClientWithEventLimits` in the stalled-consumer test with `eventmailbox.Limits{MaxEvents: 1, MaxBytes: 1024}` and write two small event records. Assert `Done` closes and the exact normalized overflow error remains.

Add a correlated-response test that fills the event mailbox to its test count limit, issues `Call` concurrently, reads the command from the peer, writes its matching response ID, and asserts the call settles before event draining. This catches an implementation that queues responses behind events.

Retain `TestClientCloseUnblocksEventBackpressure`, but construct the client with a small test mailbox and assert `Close` returns inside one second.

- [ ] **Step 6: Run Pi RPC tests and race tests**

Run:

```bash
go test ./internal/supervisor/pirpc
go test -race ./internal/supervisor/pirpc
```

Expected: PASS with no race report.

- [ ] **Step 7: Commit Task 2**

```bash
git add internal/supervisor/pirpc/client.go internal/supervisor/pirpc/client_test.go
git commit -m "fix: tolerate bounded Pi event bursts"
```

---

### Task 3: Descendant SSE Burst Tolerance

**Files:**
- Modify: `internal/supervisorapi/client.go`
- Modify: `internal/supervisorapi/handler_test.go`
- Modify: `internal/supervisor/events.go`

**Interfaces:**
- Consumes: Task 1 mailbox API.
- Consumes: `supervisor.DefaultEventRingCapacity` and `supervisor.DefaultEventRingByteCapacity`.
- Produces: `supervisor.RetainedEventBytes(EventEnvelope) int`, preserving the existing retained-size formula for SSE and broker accounting.
- Preserves: `DescendantClient.Subscribe(context.Context) (supervisor.Subscription, error)` and the public `Subscription` shape.
- Adds to `DescendantClient`: unexported `eventLimits eventmailbox.Limits`, initialized by `NewClient`, so tests can force overflow without allocating production-sized fixtures.

- [ ] **Step 1: Add a failing real-SSE burst regression**

Add `TestDescendantSSEPreservesOrdinaryBurst` to `internal/supervisorapi/handler_test.go`. Start a real `ServeUnix` server whose handler flushes 256 sequential `EventEnvelope` SSE records after a release signal. Subscribe before releasing the server, delay consumption until all records have been written, then read and assert literal sequences 1 through 256. Assert `sub.Err()` is nil until the server is intentionally canceled.

This test catches the current second-event consumer-overflow failure.

- [ ] **Step 2: Run the SSE burst test and verify RED**

Run:

```bash
go test ./internal/supervisorapi -run TestDescendantSSEPreservesOrdinaryBurst -count=1
```

Expected: FAIL because `sub.Events` closes after its one-entry mailbox overflows.

- [ ] **Step 3: Integrate the bounded mailbox into `DescendantClient.Subscribe`**

Initialize the client limits in `NewClient`:

```go
eventLimits: eventmailbox.Limits{
    MaxEvents: supervisor.DefaultEventRingCapacity,
    MaxBytes:  supervisor.DefaultEventRingByteCapacity,
},
```

In `internal/supervisor/events.go`, rename the existing private `retainedEventBytes` helper to exported `RetainedEventBytes` without changing its formula, and update its current broker call sites.

In `Subscribe`, construct `eventmailbox.New[supervisor.EventEnvelope](client.eventLimits)` instead of `make(chan ..., 1)`. Return `mailbox.Events()` as `Subscription.Events` and enqueue using `supervisor.RetainedEventBytes(event)`.

Separate network closure from mailbox closure:

- `Subscription.Close` cancels the stream, closes the response body, and calls `mailbox.Abort()` exactly once.
- Scanner EOF or parse failure sets `streamErr`, closes the response body/cancel scope, and calls `mailbox.Close()` so accepted records drain.
- Mailbox `ErrFull` sets the existing typed `child event consumer exceeded bounded capacity` error, closes the network stream, and calls `mailbox.Abort()`.
- Context cancellation caused by the owner aborts without manufacturing a stream error.

`RetainedEventBytes` must remain exactly `len(SessionID) + len(Kind) + len(Payload) + 24`.

- [ ] **Step 4: Run the SSE burst test and verify GREEN**

Run:

```bash
go test ./internal/supervisorapi -run TestDescendantSSEPreservesOrdinaryBurst -count=1
```

Expected: PASS.

- [ ] **Step 5: Rewrite the stalled-consumer test to exceed an explicit small bound**

In `TestDescendantSSEDisconnectsStalledConsumerAtBoundedCapacity`, construct the concrete client with `NewClient`, set `client.eventLimits = eventmailbox.Limits{MaxEvents: 2, MaxBytes: 1024}`, and make the server emit three events while the consumer is stalled. Assert:

- `sub.Err()` is a `child_unavailable` error containing `event consumer`.
- `sub.Events` closes promptly after overflow.
- No assumption is made that an unread accepted event survives abort.

Add a clean-EOF drain test where the server emits three events and closes; assert all three remain readable in order before the output channel closes and `sub.Err()` reports the owned clean EOF.

- [ ] **Step 6: Run supervisor API tests and race tests**

Run:

```bash
go test ./internal/supervisorapi
go test -race ./internal/supervisorapi
```

Expected: PASS with no race report.

- [ ] **Step 7: Commit Task 3**

```bash
git add internal/supervisorapi/client.go internal/supervisorapi/handler_test.go internal/supervisor/events.go
git commit -m "fix: buffer descendant event bursts safely"
```

---

### Task 4: Broker Subscribers Match Replay Capacity

**Files:**
- Modify: `internal/supervisor/events.go`
- Modify: `internal/supervisor/events_test.go`
- Modify: `internal/manager/monitor_test.go`
- Modify: `internal/supervisorapi/client.go`

**Interfaces:**
- Consumes: Task 1 mailbox API.
- Consumes: `supervisor.RetainedEventBytes(EventEnvelope) int` from Task 3 for consistent descendant and broker accounting.
- Preserves: `EventBroker`, `EventBrokerOptions`, `Subscription`, publication methods, replay/live cut, source sequencing, and clone behavior.
- Preserves test-only constructor semantics: `newEventBroker(ringCapacity, mailboxCapacity int)` still permits a deliberately smaller subscriber count limit than replay count.

- [ ] **Step 1: Add the failing default-broker 128-event regression**

Add to `internal/supervisor/events_test.go`:

```go
func TestDefaultEventBrokerSubscriberSurvivesRetainedBurstBeyondLegacyMailbox(t *testing.T) {
    broker := NewEventBroker()
    subscription := broker.Subscribe()
    defer subscription.Close()

    for seq := 1; seq <= 256; seq++ {
        broker.PublishLocal("root", "pi", json.RawMessage(`{"type":"message_update"}`))
    }
    for want := uint64(1); want <= 256; want++ {
        select {
        case event, open := <-subscription.Events:
            if !open || event.Seq != want {
                t.Fatalf("event %d = %#v, open=%t", want, event, open)
            }
        case <-time.After(time.Second):
            t.Fatalf("timed out waiting for event %d", want)
        }
    }
}
```

This catches the fixed 128-event subscriber capacity that can detach the manager before replay retention is exhausted.

- [ ] **Step 2: Run the broker regression and verify RED**

Run:

```bash
go test ./internal/supervisor -run TestDefaultEventBrokerSubscriberSurvivesRetainedBurstBeyondLegacyMailbox -count=1
```

Expected: FAIL because the subscription closes after event 128.

- [ ] **Step 3: Replace broker subscriber channel storage with the bounded mailbox**

Use `RetainedEventBytes` from Task 3 for replay-ring and subscriber accounting.

Refactor the private supervisor `eventMailbox` into a thin wrapper over `*eventmailbox.Mailbox[EventEnvelope]`. Its constructor receives independent count and byte limits. `send` maps successful `Send` to true and `ErrFull`/`ErrClosed` to false. `close` calls graceful `Close`; `abort` calls `Abort`.

Configure production constructors as follows:

```go
func NewEventBroker() *EventBroker {
    return newEventBrokerWithByteCapacity(
        DefaultEventRingCapacity,
        DefaultEventRingCapacity,
        DefaultEventRingByteCapacity,
    )
}

func NewEventBrokerWithOptions(options EventBrokerOptions) (*EventBroker, error) {
    // existing validation
    return newEventBrokerWithByteCapacity(options.MaxEvents, options.MaxEvents, options.MaxBytes), nil
}
```

A zero `MaxEvents` remains truly disabled because the generic mailbox can enforce bytes without preallocating a giant channel. Keep `newEventBroker(ringCapacity, mailboxCapacity)` for focused overflow tests.

Change `Subscribe` to expose the wrapper mailbox's `Events()` channel. Preserve the existing replay/publication cut and clone replay outside locks.

- [ ] **Step 4: Run the default-broker regression and verify GREEN**

Run:

```bash
go test ./internal/supervisor -run TestDefaultEventBrokerSubscriberSurvivesRetainedBurstBeyondLegacyMailbox -count=1
```

Expected: PASS.

- [ ] **Step 5: Convert private-structure broker tests to observable contracts**

Update tests that read `mailbox.mu`, `mailbox.events`, `mailbox.sizes`, or `mailbox.totalBytes`:

- Byte overflow: publish within-budget then over-budget events and assert detachment/output closure.
- Subscription close: publish unread events, call `Close`, and assert the public output closes without delivering unread data.
- Graceful broker close: publish accepted events, call `broker.Close`, and assert every accepted event drains in order before channel close.
- Close/abort race: race `broker.Close` and `subscription.Close` for 100 iterations and assert completion/no open output.
- State-lock behavior: use the existing public nonblocking-publisher test instead of taking a private mailbox lock; remove the duplicate lock-coupled test.

Keep the slow-versus-fast subscriber test with a deliberately small test mailbox. Keep concurrent sequencing, replay/live cut, immutable cloning, and close idempotence tests.

Add a byte-only options subscriber test: `NewEventBrokerWithOptions(EventBrokerOptions{MaxBytes: 512})`, subscribe without consuming, publish small events until the byte window is exceeded, and assert the subscriber survives more than one event but eventually detaches. This catches accidental unbuffered behavior when count is disabled.

- [ ] **Step 6: Strengthen manager reconnect gap coverage and share accounting**

In `TestConsumeSubscriptionDeduplicatesReconnectReplay`, add:

```go
if gap := handle.mirror.Gap(); gap != nil {
    t.Fatalf("retained overlap recorded false replay gap: %#v", gap)
}
```

Add a neighboring true-gap assertion using replay sequences 4 and 5 after a mirror ending at sequence 2; assert exactly `ExpectedSeq: 3` and `FirstAvailableSeq: 4`.

Confirm `internal/supervisorapi/client.go` still calls `supervisor.RetainedEventBytes(event)` after the broker mailbox refactor.

- [ ] **Step 7: Run broker, manager, and supervisor API tests**

Run:

```bash
go test ./internal/supervisor ./internal/manager ./internal/supervisorapi
go test -race ./internal/supervisor ./internal/manager ./internal/supervisorapi
```

Expected: PASS with no race report.

- [ ] **Step 8: Commit Task 4**

```bash
git add internal/supervisor/events.go internal/supervisor/events_test.go internal/manager/monitor_test.go internal/supervisorapi/client.go
git commit -m "fix: align event subscribers with replay bounds"
```

---

### Task 5: Privacy-Safe Proxy Diagnostic Classes

**Files:**
- Modify: `internal/proxy/observability.go`
- Modify: `internal/proxy/observability_test.go`
- Modify: `internal/proxy/observability_integration_test.go`

**Interfaces:**
- Produces internally: `proxyDiagnosticClass(format string) string`.
- Produces internally: `isExpectedProxyTeardown(args []any) bool`.
- Preserves: `privacySafeProxyLogger.Printf(string, ...any)` and the log message `proxy internal warning`.
- Preserves: safe fallback `error_class=internal` for unknown formats.

- [ ] **Step 1: Add failing classification and suppression tests**

Add direct logger tests to `internal/proxy/observability_test.go` using a JSON `slog` handler and supplied secrets that must never appear:

```go
func TestPrivacySafeProxyLoggerClassifiesWithoutRenderingArguments(t *testing.T) {
    tests := []struct {
        name   string
        format string
        want   string
    }{
        {name: "connect", format: "[%03d] WARN: Error dialing to %s: %s", want: "connect_dial"},
        {name: "TLS", format: "[%03d] WARN: Cannot handshake client %v %v", want: "tls_handshake"},
        {name: "client read", format: "[%03d] WARN: Cannot read request from mitm'd client %v %v", want: "client_read"},
        {name: "upstream read", format: "[%03d] WARN: Cannot read response from mitm'd server %v", want: "upstream_read"},
        {name: "client write", format: "[%03d] WARN: Cannot write response from mitm'd client: %v", want: "client_write"},
        {name: "copy", format: "[%03d] WARN: Error copying to client: %s", want: "tunnel_copy"},
        {name: "websocket", format: "[%03d] WARN: Unable to use Websocket connection", want: "websocket"},
        {name: "certificate", format: "[%03d] WARN: Cannot sign host certificate with provided CA: %s", want: "certificate"},
        {name: "protocol", format: "[%03d] WARN: HTTP2 connection failed: %v", want: "protocol"},
        {name: "unknown", format: "unknown %s", want: "internal"},
    }
    for _, test := range tests {
        t.Run(test.name, func(t *testing.T) {
            var logs bytes.Buffer
            logger := privacySafeProxyLogger{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
            logger.Printf(test.format, 7, "private.example", errors.New("secret-error-token"))
            output := logs.String()
            if !strings.Contains(output, `"error_class":"`+test.want+`"`) {
                t.Fatalf("classified log = %s, want %s", output, test.want)
            }
            for _, secret := range []string{"private.example", "secret-error-token"} {
                if strings.Contains(output, secret) {
                    t.Fatalf("secret %q leaked in %s", secret, output)
                }
            }
        })
    }
}
```

Add a table test passing wrapped `context.Canceled`, `io.EOF`, `io.ErrUnexpectedEOF`, `net.ErrClosed`, `syscall.EPIPE`, and `syscall.ECONNRESET`; assert the log buffer stays empty for every case.

These tests catch the current behavior because all classes are `internal` and expected teardown emits warnings.

- [ ] **Step 2: Run proxy unit tests and verify RED**

Run:

```bash
go test ./internal/proxy -run 'TestPrivacySafeProxyLogger(Classifies|Suppresses)' -count=1
```

Expected: FAIL on the first known class or expected-teardown case.

- [ ] **Step 3: Implement safe format classification and error-identity suppression**

In `internal/proxy/observability.go`, add standard-library imports for `errors`, `io`, and `syscall`.

Implement `proxyDiagnosticClass` with ordered `strings.Contains` checks over the format string only. Check specific read/write phrases before broad protocol fallback. Return only the ten approved bounded strings.

Implement `isExpectedProxyTeardown` by iterating arguments, selecting only values implementing `error`, and testing:

```go
for _, expected := range []error{
    context.Canceled,
    io.EOF,
    io.ErrUnexpectedEOF,
    net.ErrClosed,
    syscall.EPIPE,
    syscall.ECONNRESET,
} {
    if errors.Is(err, expected) {
        return true
    }
}
```

Do not inspect or compare arbitrary error strings. Change `Printf` to return immediately for expected teardown; otherwise emit `proxy internal warning` with `slog.String("error_class", proxyDiagnosticClass(format))`. Never call `fmt.Sprintf` on `format` and `args`.

- [ ] **Step 4: Run proxy unit tests and verify GREEN**

Run:

```bash
go test ./internal/proxy -run 'TestPrivacySafeProxyLogger(Classifies|Suppresses)' -count=1
```

Expected: PASS.

- [ ] **Step 5: Update and run the MITM integration regression**

In `TestObservedProxySanitizesMITMInternalErrors`, change the expected class from `internal` to `upstream_read`; retain the assertion that `secret-error-token` is absent.

Run:

```bash
go test ./internal/proxy -run TestObservedProxySanitizesMITMInternalErrors -count=1
```

Expected: PASS. Mentally mutate the classifier to render arguments or return `internal`; the unit or integration test must fail.

- [ ] **Step 6: Run the complete proxy package and race detector**

Run:

```bash
go test ./internal/proxy
go test -race ./internal/proxy
```

Expected: PASS with no race report.

- [ ] **Step 7: Commit Task 5**

```bash
git add internal/proxy/observability.go internal/proxy/observability_test.go internal/proxy/observability_integration_test.go
git commit -m "fix: classify expected proxy teardown safely"
```

---

### Task 6: Full Verification and Review

**Files:**
- Verify all changed files from Tasks 1-5.
- Update the plan checkboxes as work completes; no production behavior is added in this task.

**Interfaces:**
- Consumes the completed event mailbox, Pi RPC, SSE, broker, manager, and proxy changes.
- Produces verification evidence and an independently reviewed diff.

- [ ] **Step 1: Format and inspect the complete diff**

Run:

```bash
gofmt -w internal/eventmailbox/mailbox.go internal/eventmailbox/mailbox_test.go \
  internal/supervisor/pirpc/client.go internal/supervisor/pirpc/client_test.go \
  internal/supervisorapi/client.go internal/supervisorapi/handler_test.go \
  internal/supervisor/events.go internal/supervisor/events_test.go \
  internal/manager/monitor_test.go internal/proxy/observability.go \
  internal/proxy/observability_test.go internal/proxy/observability_integration_test.go
git diff --check
git status --short
git diff --stat origin/main...HEAD
git diff origin/main...HEAD -- internal/eventmailbox internal/supervisor/pirpc \
  internal/supervisorapi internal/supervisor/events.go internal/supervisor/events_test.go \
  internal/manager/monitor_test.go internal/proxy
```

Expected: no formatting or whitespace errors; only planned files plus approved design/plan documents are changed.

- [ ] **Step 2: Run all unit tests**

Run:

```bash
go test ./...
```

Expected: every package passes.

- [ ] **Step 3: Run focused race tests**

Run:

```bash
go test -race ./internal/eventmailbox ./internal/supervisor/... \
  ./internal/supervisorapi ./internal/manager ./internal/proxy
```

Expected: every package passes with no race report.

- [ ] **Step 4: Run repeated burst/close stress tests**

Run:

```bash
go test ./internal/eventmailbox ./internal/supervisor/pirpc \
  ./internal/supervisorapi ./internal/supervisor \
  -run 'Burst|Overflow|Close|Concurrent' -count=20
```

Expected: all 20 iterations pass without timeout or intermittent channel closure.

- [ ] **Step 5: Request independent code review**

Use the `requesting-code-review` skill. Ask the reviewer to verify:

- no send-on-closed-channel or dispatcher leak,
- in-flight events remain byte/count charged until receive,
- abort and graceful close semantics match every owner,
- RPC responses bypass event backpressure,
- broker replay/live cut remains exact,
- proxy classification cannot leak arguments,
- tests would fail if one-entry behavior or generic `internal` warnings returned.

Apply only findings verified against the approved spec, rerunning the focused tests after each correction.

- [ ] **Step 6: Re-run final verification after review fixes**

Run:

```bash
go test ./...
go test -race ./internal/eventmailbox ./internal/supervisor/... \
  ./internal/supervisorapi ./internal/manager ./internal/proxy
git diff --check
git status --short --branch
```

Expected: all tests pass, no race report, no whitespace errors, and the branch contains only intentional changes.

- [ ] **Step 7: Commit any review-only corrections**

If review produced code changes:

```bash
git add internal/eventmailbox internal/supervisor internal/supervisorapi internal/manager internal/proxy
git commit -m "fix: address event backpressure review"
```

If review produced no code changes, do not create an empty commit.
