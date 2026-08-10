# Event Backpressure and Proxy Warning Design

**Date:** 2026-08-10

**Status:** Approved for planning

## Summary

Kanedias currently mistakes normal Pi streaming bursts for stalled event
consumers. The image-attachment work increased the maximum Pi RPC record from
4 MiB to 12 MiB and, to retain a byte bound, reduced the Pi RPC and descendant
SSE event channels from 128 entries to one. A producer that emits a second
event before the consumer finishes publishing or forwarding the first event
therefore terminates the transport.

The live failure chain is:

1. Pi emits adjacent streaming events.
2. The one-entry Pi RPC mailbox fills.
3. Pi RPC terminates with `pi RPC event consumer exceeded bounded capacity`.
4. The supervised session transitions to failed and child creation loses its
   terminal settlement.
5. Descendant and root event streams close; reconnecting consumers can start
   after older events have left the replay ring, producing warnings such as
   `replay gap: expected seq 24, first available 95`.
6. Aborted HTTPS streams cause goproxy teardown diagnostics, all of which are
   currently collapsed into the unhelpful warning
   `proxy internal warning error_class=internal`.

The fix introduces count-and-byte-bounded burst mailboxes at event transport
boundaries, aligns broker subscriber buffering with replay retention, and
makes proxy warnings both quiet for expected teardown and useful for
unexpected failures without exposing private request data.

## Goals

- Preserve every event, in order, during ordinary Pi token, tool-call, and
  lifecycle bursts.
- Keep queued event memory bounded by both count and retained bytes.
- Disconnect a genuinely stalled consumer rather than block RPC responses,
  cancellation, or shutdown indefinitely.
- Prevent routine bursts from disconnecting server and descendant event
  streams or creating replay gaps.
- Preserve prompt, race-safe close and abort behavior.
- Suppress warning-level proxy noise for expected connection teardown.
- Classify unexpected proxy warnings without logging hosts, paths, headers,
  credentials, or raw errors.

## Non-goals

- Infinite or disk-backed event retention.
- Exactly-once delivery after a consumer exceeds the complete replay budget.
- Hiding genuine replay gaps.
- Changing Pi's JSON-line protocol or the supervisor SSE wire format.
- Logging request targets or raw goproxy diagnostic arguments.
- Refactoring unrelated supervisor lifecycle behavior.

## Design

### 1. Bounded burst mailbox

Add a small internal FIFO mailbox abstraction for event transport boundaries.
It owns one queue and exposes a receive-only event channel to existing
consumers. Producers enqueue without waiting for a consumer. A dispatcher
forwards queued events in FIFO order.

Each mailbox enforces two independent limits:

- **Maximum events:** 4,096.
- **Maximum retained bytes:** 16 MiB.

An event is accepted only when adding it stays within both limits. The mailbox
tracks the retained size supplied by its caller so that Pi RPC events and
supervisor envelopes can use their existing accounting rules. An event remains
charged while the dispatcher is blocked delivering it and is subtracted only
after the consumer receives it. The limits therefore cover queued and
in-flight delivery memory rather than only the slice waiting behind the
dispatcher.

The mailbox has explicit graceful-close and abort behavior:

- **Graceful close** rejects new events, drains every accepted event in order,
  then closes the output channel.
- **Abort** rejects new events, releases queued payloads, interrupts a blocked
  dispatch, and closes the output channel promptly.
- Both operations are idempotent and safe against concurrent enqueue,
  dispatch, and close calls.

The queue remains nonblocking at producer boundaries. If either bound would be
exceeded, enqueue returns a bounded-capacity error. This distinguishes normal
bursts from a consumer that has fallen behind the complete memory budget.

The 16 MiB limit admits one maximum 12 MiB Pi record with JSON headroom while
preventing multiplication of maximum-size image records. The count limit
bounds the metadata cost of very small token deltas.

### 2. Pi RPC integration

Replace the one-entry `pirpc.Client.events` mailbox with the bounded burst
mailbox. The RPC reader continues to parse and sequence records itself:

- Correlated responses still bypass the event mailbox and settle pending RPC
  calls immediately.
- Non-response records enqueue with their raw JSON byte size.
- A true mailbox overflow terminates the Pi RPC transport with the existing
  normalized bounded-capacity error.
- Client termination aborts the mailbox so `Close` cannot wait behind an unread
  event.
- A normal reader shutdown gracefully drains already accepted events before
  `DrainDone` can complete.

This keeps event backpressure from blocking response dispatch while restoring
burst tolerance.

### 3. Descendant SSE integration

Replace the one-entry channel in `supervisorapi.DescendantClient.Subscribe`
with the same bounded mailbox behavior:

- Parsed `EventEnvelope` values enqueue in wire order.
- Accounting uses the envelope's retained session ID, kind, payload, and fixed
  metadata size rather than channel capacity as a proxy for memory.
- Context cancellation aborts promptly.
- Clean or failed stream completion gracefully drains events already accepted
  from the wire before exposing the stream error.
- Genuine overflow records a typed `child_unavailable` stream error and aborts
  the stream.

The public `supervisor.Subscription` interface remains unchanged.

### 4. Broker subscriber capacity

The root `EventBroker` already accounts subscriber bytes, but every subscriber
has a fixed 128-event count ceiling even when the configured replay ring retains
4,096 events. Configure broker subscriber mailboxes from the broker's resolved
replay count and byte limits instead.

A subscriber can therefore absorb the same bounded event window the broker can
replay. If a subscriber falls behind beyond that complete window, it is still
detached. Reconnection and the existing mirror logic continue to deduplicate
retained replay events and report a real gap when the required sequence has
already been evicted.

Manager change-notification fanout remains at 128 revisions because revisions
are coalescible state-change notifications rather than transcript events.

### 5. Proxy diagnostic classification

`privacySafeProxyLogger.Printf` receives goproxy's constant format string and
its arguments. It must never format or log those arguments wholesale because
they can contain hosts, URLs, or lower-level errors with private data.

Classify diagnostics using only known constant format prefixes:

- `connect_dial`
- `tls_handshake`
- `client_read`
- `upstream_read`
- `client_write`
- `tunnel_copy`
- `websocket`
- `certificate`
- `protocol`
- `internal` for unknown formats

Inspect error arguments only through `errors.Is` and safe syscall identity.
Expected lifecycle failures—`context.Canceled`, `io.EOF`,
`io.ErrUnexpectedEOF`, `net.ErrClosed`, `syscall.EPIPE`, and
`syscall.ECONNRESET`—produce no warning. No raw error text is logged.

Unexpected diagnostics retain the existing message `proxy internal warning`
but carry the classified `error_class`. Unknown formats remain
`error_class=internal`, preserving a safe fallback.

## Error Semantics

- Event order is preserved for every accepted event.
- No accepted event is silently dropped during graceful shutdown.
- Abort intentionally discards unread events because the owning transport is
  no longer usable.
- Overflow remains terminal for the affected Pi RPC client or descendant
  stream; continuing after event loss would make lifecycle and transcript
  state untrustworthy.
- Broker subscriber overflow detaches only that subscriber. Other subscribers
  and publication continue.
- Replay-gap presentation remains unchanged and therefore continues to expose
  true retention loss.

## Testing

### Bounded mailbox

- A burst larger than one event is accepted and delivered in FIFO order.
- Count exhaustion rejects the next event.
- Byte exhaustion rejects the next event.
- Dequeue releases retained byte capacity.
- Graceful close drains accepted events.
- Abort releases queued payloads and closes promptly with an unread consumer.
- Concurrent enqueue and close are race-safe.

### Pi RPC

- More than 128 adjacent ordinary events survive while the consumer is
  temporarily delayed, remain ordered, and leave the transport usable for a
  correlated RPC response.
- A stalled consumer exceeding the byte or count budget still terminates the
  transport with the normalized overflow error.
- `Close` remains prompt under event backpressure.

### Descendant SSE

- An SSE burst larger than the old one-event capacity is delivered completely
  and in order.
- A stream that exceeds the bounded backlog reports a typed consumer-overflow
  error.
- Cancellation remains prompt and accepted events follow the documented close
  mode.

### Event broker and manager replay

- A subscriber survives more than 128 ordinary events when they remain inside
  configured replay limits.
- A subscriber exceeding configured replay limits is detached without
  affecting a fast subscriber.
- A manager reconnect over retained overlap deduplicates events and records no
  gap; a true replay eviction still records the first gap.

### Proxy logging

- Expected canceled, EOF (including unexpected EOF during teardown), closed,
  reset, and broken-pipe diagnostics emit no warning.
- Each known unexpected format emits its safe class.
- Unknown formats emit `internal`.
- Logged records never contain supplied host, URL, credential, or raw-error
  argument text.

### Verification

Run targeted package tests during each red/green cycle, then:

```bash
go test ./...
go test -race ./internal/supervisor/... ./internal/supervisorapi ./internal/manager ./internal/proxy
```

The existing live Incus acceptance suite remains opt-in and is not required for
local unit verification unless its authorization environment is available.

## Operational Outcome

After deployment, normal model streaming bursts no longer fail sessions or
children, and server event consumers no longer disconnect merely because more
than one or 128 events arrive quickly. A consumer that genuinely falls more
than 4,096 events or 16 MiB behind still fails explicitly. Proxy output is
quiet during expected session teardown and retains actionable privacy-safe
categories for unexpected failures.
