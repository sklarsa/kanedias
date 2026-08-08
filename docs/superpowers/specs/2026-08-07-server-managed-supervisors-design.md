# Server-Managed Root Supervisors and Live Astrolabe UI

**Status:** Approved v1 design

**Date:** 2026-08-07

## Purpose

Connect the existing recursive Pi session supervisor to the existing Astrolabe
web UI.

`kanedias server` becomes a local control plane for multiple independent root
supervisors. It may launch roots, discovers roots that are already running,
streams their state to browsers, and routes operator actions through each root's
Unix-socket API.

A root supervisor remains the lifecycle owner of its Pi session, descendants,
Incus resources, event replay, pending questions, and cleanup. The server is an
observer and controller, not a required parent process. Stopping or restarting
the server must not stop an admitted root supervisor.

## Existing Contracts

This design builds on, rather than replaces, the contracts in
`docs/architecture/session-supervisor.md`:

- one supervisor owns one immutable Pi session;
- one root socket exposes its complete live descendant tree;
- descendant commands, question responses, and stops route through that root;
- one root `/v1/events` subscription contains events for the complete subtree;
- the supervisor continuously drains Pi RPC whether or not a socket client is
  connected;
- recent events are retained in a bounded in-memory buffer;
- pending blocking questions are retained separately from event replay; and
- deleting a descendant stops its subtree, while deleting the root stops the
  complete supervision tree.

The manager connects only to roots. It must never adopt or independently manage
a descendant socket.

## Goals

V1 must:

- display multiple unrelated root supervision trees in one fleet UI;
- discover live roots from a private root-socket directory;
- launch a new independent root from the web UI;
- leave admitted roots running across normal server shutdown and ordinary
  server-process failure;
- maintain one continuous event subscription per root while the server runs;
- recover retained replay after reconnect or server restart;
- show live tree state, recent transcript activity, recent tool activity,
  supported Pi metrics, and pending questions;
- route Steer, Interrupt Turn, Stop Session, and Answer Question correctly;
- make supervisor event retention configurable;
- report replay gaps instead of claiming complete history; and
- protect the loopback HTTP control surface with a browser capability and
  same-origin write checks.

## Non-goals

V1 does not provide:

- durable or complete event history;
- guaranteed replay after the supervisor's configured buffer has evicted data;
- complete transcript retrieval for arbitrarily large sessions;
- retention of a descendant after its supervisor has completed and cleaned it
  up;
- independent discovery or adoption of descendant sockets;
- discovery outside the configured root-socket directory;
- blind cleanup of unresponsive or malformed sockets;
- orphaned Incus resource reconciliation after a supervisor or host crash;
- survival across reboot, host shutdown, OOM termination, container shutdown,
  or service-manager cgroup termination;
- remote or non-loopback access;
- a human "Spawn Subagent" action; or
- changes to the supervisor's HTTP protocol.

## Core Invariants

### Supervisors own execution and replay

The supervisor always drains Pi RPC stdout. Its event buffer exists even when no
manager or browser is connected. Subscribing to `/v1/events` copies retained
replay and then follows live events; it does not consume or empty the buffer.

### The manager owns fleet projection, not session truth

The manager maintains browser-facing projections and routing indexes. A root
`/v1/tree` snapshot is authoritative for live structure, lifecycle, model, and
pending questions. Pi RPC responses are authoritative for Pi state and metrics.
The manager must not manufacture unsupported progress or lifecycle values.

### Roots are the only discovery unit

A root socket already aggregates and routes its complete descendant tree.
Connecting to descendant sockets separately would duplicate nodes and events and
would violate their parent-liveness ownership model.

### Gaps are visible

Supervisor and manager state are bounded and volatile. If sequence continuity
cannot be proved, the UI marks the activity tail as incomplete. It never labels
bounded replay as complete session history.

### Server shutdown is non-destructive

Closing the server cancels browser streams and manager subscriptions. It does
not send signals or stop requests to admitted roots. A root stops only through
an explicit Stop Session action or its own supervisor lifecycle.

## Configuration

The server command now loads the selected persistent `--config` file and passes
its clean absolute path to the manager. Spawned roots receive the same path, so
root and descendant worker policy remains consistent.

```toml
[server]
root_socket_dir = ""       # default described below
session_log_dir = ""       # default ~/.local/state/kanedias/sessions
discovery_interval = "5s"
snapshot_interval = "1s"
spawn_timeout = "2m"
session_binary = ""        # default os.Executable()

[supervisor.events]
max_events = 4096
max_bytes = 16777216        # 16 MiB
```

Rules:

- An omitted event limit uses the current default. The config decoder must
  preserve field presence so an omitted value is distinguishable from an
  explicit zero.
- `0` disables that individual limit, but at least one of `max_events` or
  `max_bytes` must be positive.
- Negative limits and non-positive intervals are rejected.
- The event limits apply to every supervisor process and to the manager's
  non-authoritative per-root event mirror. Because descendant events are retained
  by the descendant, forwarded through its ancestors, and mirrored while the
  server is connected, operators must account for memory multiplication across
  deep trees.
- Subscriber mailbox capacity remains an internal safety setting. It controls
  slow-subscriber disconnection, not replay history.
- `root_socket_dir` must be absolute, owned by the effective user, mode `0700`,
  free of symlink traversal at the managed directory, and short enough for Unix
  socket paths. Its default is `$XDG_RUNTIME_DIR/kanedias/roots`; when
  `XDG_RUNTIME_DIR` is unavailable, use a private, EUID-specific directory under
  `/tmp` with the same ownership and mode checks.
- `session_log_dir` must be private to the effective user. Each spawned root gets
  a mode-private stdout/stderr log.
- `session_binary` must resolve to an absolute executable. Existing roots may
  have been launched by another compatible binary.

The only supervisor-package change required by this design is configurable
`EventBroker` construction. Existing default construction remains available for
tests and callers that do not supply options.

## Root Socket Layout and Discovery

Manager-spawned roots use an opaque random launch token:

```text
<root_socket_dir>/<launch-token>.root.sock
```

The filename is not a session identity. A root's Kanedias session ID is generated
inside the root process after launch and is learned from `/v1/tree`.

Child supervisors continue to create `<sessionID>.sock` files beside their
parent. The discovery scan considers only `*.root.sock`, so child sockets are
never candidates.

An externally launched root is discoverable when the operator gives it a socket
with the `.root.sock` suffix inside the configured directory:

```bash
kanedias session \
  --socket "$XDG_RUNTIME_DIR/kanedias/roots/manual.root.sock"
```

### Candidate validation

Before dialing a candidate, discovery verifies with `lstat` that the path:

- is a Unix socket, not a symlink;
- is owned by the effective user; and
- has mode `0600`.

A successful `/v1/tree` probe is admitted only when its top node has:

- a non-empty `SessionID`;
- `RootSessionID == SessionID`;
- an empty `ParentSessionID`;
- `Kind == root` and `Context == root`;
- non-empty Pi session ID and session file; and
- lifecycle `ready` or `running`.

Provisioning and starting roots remain candidates and are retried. Stopping,
stopped, failed, malformed, or identity-conflicting candidates are not admitted.
Duplicate root IDs on different socket identities are reported as conflicts
rather than silently selecting one.

Discovery never unlinks a candidate. A failed probe does not prove process death,
and a recursive tree probe may fail temporarily because a descendant is
unavailable. If an admitted socket still exists but probing fails, the manager
keeps its last good snapshot marked stale and retries. If the socket disappears,
the root is removed from the live fleet. Socket publication and unlinking remain
the owning supervisor's responsibility.

## Spawning and Admission

`POST /ui/sessions` asks the manager to launch a root using the configured
binary, config path, root directory, and log directory.

The root spawner:

1. generates an unpredictable launch token and selects a unique, nonexistent
   `.root.sock` path;
2. opens mode-private log output and `/dev/null` stdin;
3. starts `kanedias --config <absolute-path> session --socket <absolute-path>`
   with `exec.Command`, not a server-lifetime `exec.CommandContext`;
4. creates a new session with `Setsid` so ordinary parent exit and terminal
   process-group signals do not stop the root;
5. immediately starts a waiter so a child that exits while the server is alive
   is reaped;
6. probes until the exact socket identity reports an admissible root snapshot;
   and
7. registers the root and starts monitoring only after admission succeeds.

The request deadline bounds admission, not the admitted root's lifetime.

If admission fails, the manager must not leave an unknown half-started root. If
the socket is responsive, it first requests a graceful root stop. It then sends
`SIGTERM`, waits under a cleanup deadline, escalates the root process group with
`SIGKILL` only if necessary, and reaps the process. Cleanup records the exact
socket device/inode and never removes a replacement path.

After admission, neither request cancellation nor `Manager.Close` signals the
root. `Setsid` guarantees only ordinary process independence; it does not move
the root out of a service manager's cgroup or make it survive host-level events.

## Manager Model

The manager keeps two indexes:

```text
roots[rootSessionID]     -> root handle
routes[anySessionID]     -> owning rootSessionID
```

A root handle owns:

- the socket path and recorded device/inode;
- one `supervisorapi.DescendantClient`;
- the last good recursive tree snapshot and stale status;
- one root-level event sequence cursor;
- one event-drain/reconnect loop;
- a bounded per-root event mirror using the configured supervisor limits;
- per-session UI projections derived from that mirror;
- fleet/detail change notifications; and
- an optional process waiter for roots spawned by this server instance.

The routes index is rebuilt atomically from every accepted tree snapshot.
Descendant actions always use the owning root client and pass the selected
session ID to the existing routed endpoint.

### Tree refresh

Tree polling runs independently of event delivery. Events alone are insufficient
because structural child removal does not emit a dedicated manager event and a
question event may be published immediately before the pending-question store is
updated.

Each root is polled at `snapshot_interval`. Event activity may request a
coalesced early refresh, but it must not cause one `/v1/tree` call per streaming
text delta. The fleet is notified only when rendered tree state changes or stale
status changes.

### Event drain and reconnect

The manager opens one `/v1/events` subscription per root. It handles the stream
as follows:

1. accept supervisor replay and then live envelopes;
2. track the root broker's aggregate `EventEnvelope.Seq`;
3. ignore replayed envelopes at or below the last accepted sequence;
4. if the first new sequence is greater than the expected next sequence, record
   a replay gap;
5. demultiplex accepted envelopes by `EventEnvelope.SessionID`;
6. update the envelope's session projection—even when that session is not
   currently selected—and notify subscribers without blocking the root drain;
   and
7. reconnect stream failures with bounded exponential backoff and jitter.

An event-stream EOF is not proof that the supervisor died. Stream health and
root reachability are tracked separately. Manager-to-browser subscribers use
bounded mailboxes and are disconnected when slow; they can reconnect and receive
a fresh rendered projection.

The manager's mirror and projections are caches, not a second authoritative
event archive. Their purpose is browser fanout and rendering. On manager restart,
supervisor replay rehydrates them. If the first retained root sequence is greater
than one, the manager marks the earlier activity as unavailable.

## History Semantics

Three different data classes must not be conflated:

1. **Recent activity:** bounded supervisor events, used for live transcript and
   tool tails.
2. **Pending questions:** retained separately in `/v1/tree` until answered,
   cancelled, timed out, or terminated.
3. **Persisted Pi entries:** available through Pi `get_entries` while the session
   remains live and routable.

Pi `get_entries` has stable entry IDs and can reconcile message-level history,
but the existing routed client limits unary responses to 1 MiB and Pi provides
no paginated "last N entries" command. V1 may use `get_entries` when the response
fits, but it does not promise complete retrieval for a large session. When it
cannot hydrate persisted entries, the UI shows the retained activity tail and an
explicit limitation message.

Completed descendants disappear after their normal cleanup and are no longer
routable. V1 does not archive their transcript outside the supervisor lifetime.

## Pi RPC Command Semantics

The server uses typed command constructors and validates every raw Pi response.
An HTTP-successful routed call may still contain `success:false`; that is rendered
as an inline operator error, not an acknowledgment.

### Steer

The command deck's Steer action first calls `get_state`:

- when `isStreaming` is true, send
  `{"type":"steer","message":"..."}`;
- when `isStreaming` is false, send
  `{"type":"prompt","message":"..."}`.

Pi `follow_up` is not steering. It means "run after the current generation and
queued steering complete" and is not exposed as a separate v1 control.

### Interrupt Turn

Interrupt sends `{"type":"abort"}`. It aborts the current Pi operation but
leaves the supervisor and session available. The UI labels this action
"Interrupt Turn" to distinguish it from stopping a session.

### Stop Session

Stop calls `DELETE /v1/sessions/{sessionID}` through the owning root client.
Stopping a descendant stops that subtree. Stopping a root stops the complete tree
and causes the root to unlink its own socket. The accepted response means stopping
has begun; disappearance is observed through normal manager reconciliation.

### Answer Question

The answer body must contain exactly one response matching the question method:

- `select`, `input`, or `editor`: `{"value":"..."}`;
- `confirm`: `{"confirmed":true|false}`; or
- any dialog cancellation: `{"cancelled":true}`.

Repeated, mismatched, expired, or stale answers render an inline error.

## Browser Security Boundary

Mode `0600` protects a Unix socket, but it does not protect the loopback TCP
server that proxies requests to that socket. Other local users and browser-based
request forgery must not gain control merely because the listener is local.

At each server start:

1. generate a 256-bit random bootstrap capability;
2. print a loopback bootstrap URL containing that capability;
3. accept it only at `/bootstrap` using constant-time comparison;
4. issue a separate random browser-session capability in an `HttpOnly`,
   `SameSite=Strict`, `Path=/` session cookie;
5. return `Cache-Control: no-store` and `Referrer-Policy: no-referrer`;
6. redirect to `/` so the bootstrap capability leaves the address bar; and
7. keep browser-session capabilities only in memory, making them invalid after
   server restart.

The bootstrap capability may authorize another browser session for the lifetime
of that server process. It appears only in the intentional startup URL written to
the operator's stderr; request logs must omit query strings, cookies, and
capabilities.

All fleet data and control routes require a valid browser-session cookie.
`/healthz`, `/bootstrap`, and immutable static assets are the only unauthenticated
routes.

Every non-GET action additionally:

- requires `Content-Type: application/json`;
- verifies `Origin` matches the server's effective loopback origin;
- rejects an incompatible `Sec-Fetch-Site` value when that header is present;
- validates the expected `Host`; and
- decodes Datastar signals before constructing the response SSE stream.

This protects the browser bridge from other local UIDs and ordinary cross-site
requests. It does not defend against a process already running with the operator's
UID and access to that operator's browser profile or terminal.

## UI and Datastar Integration

The current Astrolabe HTML is a static demonstration. V1 splits it into
server-rendered templates with stable patch targets:

- `#fleet-panel` — all root and descendant rows plus stale/gap indicators;
- `#detail-panel` — selected identity, state, model, and supported metrics;
- `#question-panel` — the selected node's pending questions;
- `#activity-panel` — recent transcript and tool activity; and
- `#deck-status` — command acknowledgments and errors.

The initial page renders the authenticated shell and starts one fleet stream.
Selecting a node starts one detail stream and explicitly cancels the previous
detail stream; at most one detail stream is active per page. Browser streams are
views over manager state and never open supervisor connections.

Patched content must remain interactive. Row selection and actions use Datastar
attributes or document-level delegated handlers that inspect the current DOM.
The existing one-time arrays of rows, tabs, and panes cannot be retained because
they would not include later patches. Search, tabs, expand/collapse, and the
mobile drawer may remain local browser behavior.

Action requests use JSON Datastar signals, for example
`{"message":"focus on the failing test"}`. Each action response is an SSE
`PatchElements` update to `#deck-status`; resulting session changes arrive through
the fleet/detail streams.

### Supported displays

- Tree, lifecycle, model, worker type, and questions come from `/v1/tree`.
- Transcript and tool tabs show retained recent activity and disclose gaps.
- Metrics use Pi `get_session_stats`, including supported token, cost, message,
  tool-call, and context-usage fields.
- The Astrolabe dial may display `contextUsage.percent` and must be labelled
  "context", not invented completion progress.
- Unsupported mock values such as completion percentage, tool success rate, and
  synthetic elapsed time are removed.
- The per-node Spawn Subagent button is removed.
- New Session is a fleet-level action.

## HTTP Routes

```text
GET  /bootstrap
GET  /
GET  /healthz
GET  /ui/fleet
GET  /ui/session
POST /ui/sessions
POST /ui/sessions/{sessionID}/steer
POST /ui/sessions/{sessionID}/interrupt
POST /ui/sessions/{sessionID}/stop
POST /ui/sessions/{sessionID}/questions/{questionID}
```

`GET /ui/session` reads the selected session ID from validated Datastar signals.
The browser owns cancellation of the previous detail request before starting a
new one.

## Error Handling

### Root unavailable

If the socket still exists but a tree probe or event stream fails, retain the
last good tree marked stale and retry. Disable write controls while routing
ownership cannot be established from a current snapshot. If the socket is gone,
remove the root and its routes from the live fleet.

### Replay gap

Record the expected and first available root sequence, continue from the
available tail, and mark affected activity views incomplete. A later successful
Pi-entry reconciliation may restore message-level history but does not recreate
lost streaming or tool-progress events.

### Spawn failure

Render the typed admission error. Gracefully stop and then escalate/reap the
not-yet-admitted process as described above. Never register a partial handle or
unlink an unverified replacement socket.

### Pi command failure

Distinguish:

- supervisor `contract.Error` responses;
- transport timeouts or root unavailability; and
- Pi RPC envelopes with `success:false`.

All are rendered inline with stable operator-facing language. Raw internal paths,
credentials, panic values, and bootstrap capabilities are not returned.

### Slow browser

A browser cannot block a root event drain. Slow browser subscriptions are closed
and may reconnect to the current projection.

### Server shutdown

Shutdown order is:

1. reject new spawns and actions;
2. stop discovery and snapshot polling;
3. cancel fleet/detail browser SSE contexts;
4. shut down the HTTP server;
5. close manager event subscriptions and wait for drain goroutines;
6. close root clients and parent-owned log descriptors without signaling
   admitted roots.

A waiter for an admitted root remains blocked until that root exits; manager
shutdown does not wait for it or turn it into a stop signal.

Canceling browser streams before `http.Server.Shutdown` prevents permanent SSE
handlers from consuming the full shutdown deadline.

## Testing Strategy

### Supervisor event configuration

- omitted options preserve 4,096 events and 16 MiB;
- count and byte eviction each retain the newest events in order;
- zero disables one limit, both zero is rejected;
- replay/live subscription remains gap-free at its cut;
- pending questions survive event eviction; and
- root and child runtimes both use configured broker options.

### Manager

Use temporary mode-private directories and fake Unix-socket supervisors:

- scan admits valid `*.root.sock` roots only;
- descendant-style and malformed snapshots are rejected;
- child `<sessionID>.sock` files are ignored;
- owner, mode, symlink, duplicate-ID, and replacement-inode cases are rejected;
- failed probes never unlink candidates;
- one root tree builds routes for all descendants;
- routed actions use the owning root client;
- replay is deduplicated after reconnect;
- missing root sequences produce a visible gap;
- event EOF reconnects without declaring the root dead;
- independent polling observes child removal and retained questions;
- stale snapshots disable actions and recover after a successful probe;
- manager close leaves admitted fake roots running; and
- browser subscriber backpressure never blocks the root drain.

### Spawner

Use a helper process rather than live Incus:

- command, config path, socket path, environment, setsid, stdin, and log modes
  are correct;
- a responsive provisioning snapshot is not admitted early;
- ready/running plus Pi binding is admitted;
- request cancellation after admission does not stop the process;
- pre-admission timeout performs graceful stop, escalation when needed, and
  reaping; and
- server shutdown leaves an admitted helper alive for rediscovery.

### Server and UI

With `httptest`, a fake manager, and rendered-template assertions:

- unauthenticated fleet and action routes are rejected;
- bootstrap exchange sets the required cookie and redirects without logging the
  token;
- invalid cookies, Host, Origin, fetch-site, content type, and signals fail;
- fleet and selected-detail streams patch only stable targets;
- replacing fleet rows preserves selection, actions, tabs, search, and mobile
  behavior;
- only one selected-detail stream remains active;
- Steer maps to `steer` while streaming and `prompt` while idle;
- Interrupt maps to `abort`;
- Stop routes to the selected session ID;
- Pi `success:false` renders an error rather than success;
- question answer shapes are method-correct;
- real `get_session_stats` fields replace mock metrics;
- replay gaps and stale roots are unmistakable; and
- shutdown cancels SSE handlers before HTTP shutdown.

### Live acceptance

The opt-in Incus harness proves:

1. start the prerequisite proxy;
2. start the server and complete browser bootstrap;
3. create two root sessions from the UI;
4. observe both recursive trees and live activity;
5. stop the server without stopping either root;
6. restart the server, complete the new bootstrap exchange, and rediscover both
   roots;
7. receive retained replay and show any detected gap honestly;
8. steer a running session and prompt an idle session;
9. interrupt a turn and answer a blocking question;
10. stop one descendant without stopping its root; and
11. explicitly stop both roots and verify their owned resources and sockets are
    cleaned up.

The harness records exact process IDs, socket identities, tree snapshots, event
sequences, HTTP action results, and Incus resource baselines. It never infers
permission to run from ambient credentials.

## Acceptance Criteria

The v1 server is complete when:

1. multiple independent roots appear exactly once and descendants are nested
   under their owning root;
2. New Session admits a root only after supervisor and Pi readiness;
3. supervisor buffers retain events with configured count/byte limits when no
   manager is connected;
4. manager reconnect receives retained replay, deduplicates it, and reports any
   sequence gap;
5. stopping or restarting the server leaves admitted roots alive;
6. a restarted server rediscovers and controls those roots;
7. Steer, Interrupt Turn, Stop Session, and Answer Question use the exact Pi and
   supervisor semantics in this design;
8. browser control requires a valid capability and same-origin JSON action;
9. the UI renders only supported state and labels bounded history honestly; and
10. an explicit root stop cleans up the root's complete supervision tree and
    owned resources.
