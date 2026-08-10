# Recursive Pi Session Supervisor Design

**Status:** Approved v1 design

**Date:** 2026-08-07

## Purpose

Kanedias needs a small host-side primitive that owns one complete Pi session and makes that session observable and controllable. The same primitive must support nested subagents without putting multiple Pi sessions in one supervisor or sharing a workspace between agents.

The result is a recursive process tree: every Pi session has exactly one supervisor, and a supervisor may start child supervisor processes. Each child receives an independent Incus sandbox and workspace created with copy-on-write storage operations.

This design replaces the earlier proposal to adapt `pi-subagents` and reconcile its lifecycle artifacts. Kanedias will instead provide its own lightweight Pi extension and make every subagent a first-class supervised Pi RPC session.

## Goals

The v1 design must:

- run one foreground host process for one immutable Pi session;
- own the session's Incus instance, workspace volume, Pi process, RPC connection, Unix socket, and cleanup;
- continuously drain and expose Pi RPC events;
- buffer recent events and retain unanswered extension UI questions;
- accept Pi RPC commands over a host Unix socket;
- allow a supervisor to start child supervisors recursively;
- give every child an independent COW-cloned container and workspace volume;
- support either fresh or forked conversational context;
- require a worker type and resolve it to an appropriate model profile;
- support read and write child completion contracts;
- hand writer results back as pushed Git refs;
- expose a complete descendant tree through the root supervisor socket;
- route transcript queries, steering, follow-ups, aborts, and question responses to live descendants;
- terminate unfinished descendants when an ancestor shuts down.

## Non-goals

V1 does not provide:

- a global daemon or registry;
- built-in daemonization, `attach`, or a machine-wide `list` command;
- adoption of supervisors or sandboxes after a crash;
- Git worktrees or shared writable workspaces;
- a replacement for GitHub as the initial writer handoff mechanism;
- durable event journals or transcript archives owned by Kanedias;
- detached/background delegation handles in the Pi extension;
- advanced scheduling, quotas, budgets, retries, or retention policies;
- a custom browser UI;
- compatibility with the `pi-subagents` artifact or control protocols.

The caller remains responsible for putting a root supervisor in the background or under another process manager when desired.

## Runtime Prerequisite: Kanedias Proxy

A Kanedias credential proxy must already be running before any root or child supervisor creates session-owned resources:

```text
kanedias proxy run
```

The proxy listens on the configured Incus network gateway on port `3128`. The public proxy CA is baked into the base image and the Debian trust bundle by `kanedias image create`; the sandbox profile supplies the proxy and CA environment variables (`HTTP_PROXY`, `HTTPS_PROXY`, their lowercase equivalents, `NODE_EXTRA_CA_CERTS`, and `SSL_CERT_FILE`) for processes inside each container but no longer mounts the host certificate. Model-provider, GitHub, package-manager, and other outbound requests therefore depend on this service.

Every supervisor performs a fail-fast TCP reachability check against the configured proxy listener before creating or starting session-owned resources. If the listener is unavailable, session creation returns a clear prerequisite error instead of allowing Pi or Git operations to fail later with indirect connectivity errors.

The proxy is shared host infrastructure, not part of a session's ownership tree. Supervisors neither launch nor stop it.

## Core Invariants

### One supervisor, one Pi session

A supervisor is permanently bound to one Pi session. It must not expose operations that replace that session in place.

The following Pi RPC commands are rejected by the supervisor's raw RPC endpoint:

- `new_session`;
- `switch_session`;
- `fork`;
- `clone`.

Creating or forking a session always creates another supervisor. The Kanedias extension should also reject Pi session-replacement lifecycle operations while running under supervision. If the Pi session identity nevertheless changes, the supervisor treats that as a terminal invariant violation.

### One sandbox per session

Every supervisor owns exactly one Incus instance and one writable workspace volume. A child never executes in its parent's container or volume.

### Recursive supervision, not recursive ownership of Pi

A parent supervisor owns the lifecycle of child supervisor processes. Each child supervisor still exclusively owns its own Pi session and Incus resources.

### Git refs are writer results

A writer's durable result is a pushed Git ref and exact commit identity. The child's filesystem and Pi transcript are ephemeral and can be destroyed after handoff.

### Pi stdout is always drained

RPC responses and events share Pi's stdout and can arrive in either order. The supervisor must continuously drain the stream independently of connected clients. A slow client must never stall Pi execution.

## Process Topology

```text
root supervisor A
├── owns sandbox A, volume A, and Pi session A
├── starts child supervisor B
│   ├── owns COW sandbox B, COW volume B, and Pi session B
│   └── starts child supervisor D
│       └── owns COW sandbox D, COW volume D, and Pi session D
└── starts child supervisor C
    └── owns COW sandbox C, COW volume C, and Pi session C
```

Each node is the same executable and implements the same session API. A node is either:

- a **root**, launched directly by a user or process manager; or
- a **child**, launched by another supervisor with parent and clone-source information.

No separate global coordination daemon is required for a supervision tree. A future product-level observer may connect to multiple independent roots without changing this model.

## Supervisor Responsibilities

A supervisor owns:

- one immutable Kanedias session identity and its bound Pi session identity;
- Incus instance creation, startup, stop, and deletion;
- workspace volume creation and deletion;
- Pi RPC startup, readiness handshake, command correlation, and shutdown;
- the host Unix control socket;
- a small in-memory event buffer;
- pending extension UI questions;
- child supervisor processes and their control sockets;
- recursive event aggregation and command routing;
- terminal result delivery to its parent, when it has one;
- cascading cleanup.

A supervisor does not own:

- a global list of unrelated sessions;
- durable result storage beyond the lifetime of its process;
- branch merging or conflict resolution;
- agent policy such as whether a Git checkout should be clean;
- more than one Pi session directly.

## Session Identity

Kanedias assigns an immutable session ID before provisioning resources. The supervisor records the Pi session ID obtained from the successful RPC readiness handshake. They are one-to-one for the supervisor's lifetime, but remain separate fields because they are created by different systems.

Every tree node records:

- Kanedias session ID;
- Pi session ID and session file;
- direct parent session ID, if any;
- root session ID;
- child kind (`read` or `write`);
- context mode (`fresh` or `fork`);
- worker type and resolved model profile;
- Incus instance and volume names;
- lifecycle state.

Every session-owned instance and custom workspace volume records exact session, parent/root, kind, context, worker, and workspace metadata. Authorized live acceptance resources additionally carry a unique run-attribution value. These keys support exact leak accounting and direct-parent recovery after confirmed child-process death; they never enable discovery, adoption, or a global registry.

## Resource Provisioning and COW Clones

### Root session

A root supervisor clones the configured seed workspace volume and creates its instance directly from the configured base image.

### Child session

The parent starts a new supervisor process with:

- a new session ID;
- parent and root IDs;
- source instance and workspace-volume identifiers;
- child kind;
- context mode;
- worker type and its resolved model profile;
- delegated task;
- fork metadata when required.

The child supervisor performs its own Incus operations so that creation and cleanup have one owner. It:

1. creates a COW copy of the parent's workspace volume;
2. creates a COW copy of the parent's instance root filesystem;
3. replaces the inherited workspace device with the new child volume;
4. replaces any inherited supervisor-socket proxy device with the child's socket;
5. writes child-specific Incus metadata;
6. starts the child instance and Pi RPC session;
7. reports readiness to its parent.

The configured storage pool must provide snapshot-backed, copy-on-write clones for both instance root disks and custom workspace volumes. The supervisor validates that capability before provisioning and fails the child request if either clone would degrade to a full copy. Kanedias does not fall back to Git worktrees or a shared volume.

The delegation tool call is a natural clone boundary: the parent agent is waiting in the extension while the child snapshot is prepared. Stronger filesystem quiescing and atomic multi-volume snapshot semantics can be added after v1 if required.

## Conversational Context

Every child is a new Pi session and supervisor, regardless of context mode.

### Fresh context

`context: "fresh"` starts a new empty Pi transcript in the cloned filesystem. The child receives the delegated task, the Kanedias extension, and the appropriate delegation/handoff skill instructions.

### Forked context

`context: "fork"` creates a branched Pi session from the parent's current session file and leaf entry. The branch receives a new Pi session ID and records its parent relationship. The parent remains on its original session.

Before requesting the COW clone, the custom extension uses Pi's `SessionManager` to open the persisted parent session and call `createBranchedSession()` at the current leaf. This creates a child-specific session file without switching the parent runtime. The extension passes that path and new Pi session identity to the supervisor. The subsequent root-filesystem clone carries the prepared branch into the child at the same path, and the child launches Pi RPC against it.

Fork preparation removes provider-specific signed thinking blocks when they are unsafe for the model selected by the worker profile. Failure to persist or sanitize the branch fails delegation before resource provisioning.

## Lightweight Kanedias Pi Extension

Kanedias supplies its own trusted TypeScript extension and loads it into every supervised Pi session. It does not depend on `pi-subagents`.

V1 registers only two model-facing tools:

### `delegate_session`

Starts a child session and blocks until it reaches its terminal completion contract.

Required arguments:

```json
{
  "workerType": "reviewer",
  "kind": "read",
  "context": "fresh",
  "task": "Review the authentication implementation"
}
```

`workerType` selects a configured worker profile. `kind` is `read` or `write`. `context` is `fresh` or `fork`.

Pi may execute sibling tool calls concurrently, so multiple `delegate_session` calls in one assistant response naturally create parallel children without a separate parallel-workflow DSL.

### Worker profiles

Worker type is required so delegation can select an appropriate model rather than inheriting one accidentally. The host configuration maps names such as `researcher`, `reviewer`, and `worker` to a model and optional thinking level. The supervisor resolves and validates the profile before creating COW resources, passes the selected model settings to the child Pi launch, and records the resolution in the tree.

The extension obtains the available profile names from its supervisor when registering `delegate_session`. An unknown worker type fails before provisioning. V1 does not expose arbitrary model overrides in the tool request; model selection remains host policy.

### `handoff`

Completes a write session by submitting pushed Git refs, summary, and verification evidence. The tool is valid only for a session created as `kind: "write"`.

After the supervisor acknowledges the handoff, the extension returns a terminating tool result and requests graceful Pi shutdown. The supervisor then completes cleanup.

### Skill guidance

The extension contributes small skills that tell agents:

- how to choose the worker type whose model fits the task;
- when to choose read versus write children;
- when fresh or forked context is appropriate;
- that writer work must be committed and pushed before `handoff`;
- how to report repository, base commit, branch, head commit, summary, and verification;
- that Git cleanliness is a handoff discipline, not a supervisor-enforced gate.

The extension does not implement transcript inspection, steering, fleet status, scheduling, branch merging, or durable run state.

## Extension-to-Supervisor Transport

The extension talks directly to its own host supervisor over HTTP on a Unix socket. An Incus `proxy` device exposes the host socket inside the container at a fixed path:

```text
/run/kanedias/supervisor.sock
```

The TypeScript extension uses Node's Unix-socket HTTP support. It does not tunnel coordination messages through Pi extension UI requests and does not require a TCP listener.

The proxy device is session-specific. A cloned instance must have the inherited device replaced before startup so that it cannot connect to its parent's supervisor socket.

The extension sees only its own supervisor and descendant subtree. The host-facing socket remains mode `0600`. V1 may use the same HTTP handlers for host and proxied connections; a later version can split the guest capability surface if needed.

## Child Completion Contracts

The child kind is required at creation because it determines successful completion.

### Read child

A read child's initial delegated run reaches a completion boundary when it emits `agent_settled`. Settlement alone is not success: the supervisor also classifies the tracked final assistant stop reason, explicit abort state, extension errors, and Pi process health.

For a successful run, the child supervisor obtains the final assistant response, returns it to the waiting parent extension call, shuts down Pi, removes the sandbox and volume, and exits. An aborted or failed run returns a typed error instead of presenting partial assistant text as a normal answer.

Representative result:

```json
{
  "kind": "read",
  "workerType": "reviewer",
  "sessionId": "sess_child",
  "output": "The review found ..."
}
```

The result should preserve whatever response the task requested rather than forcing it into a Git handoff schema.

### Write child

A write child completes only after the extension calls `handoff` successfully.

Representative result:

```json
{
  "kind": "write",
  "workerType": "worker",
  "sessionId": "sess_child",
  "repositories": [
    {
      "repository": "owner/repository",
      "baseCommit": "0123456789abcdef",
      "branch": "kanedias/sess_child/authentication",
      "headCommit": "fedcba9876543210"
    }
  ],
  "summary": "Implemented authentication changes.",
  "verification": ["go test ./..."]
}
```

There is one branch and exact head commit per modified repository. Read-only children do not need to create branches.

If a writer agent settles without handoff, the session enters an `awaiting_handoff` state. It remains live so a client can inspect its transcript, send a new prompt or follow-up asking it to finish the handoff, or cancel it. V1 does not enforce repository cleanliness or manufacture commits.

Before submitting handoff, the extension checks the guest checkout origin and reported refs as a defense-in-depth preflight. Guest checks do not establish durability or authority: the guest and its same-UID socket caller are untrusted. Before accepting handoff, the host supervisor derives the canonical GitHub remote only from its configured repository allowlist and runs bounded `git ls-remote` verification for each exact reported branch and head. That host-configured canonical check is authoritative. A missing, ambiguous, or mismatched remote ref leaves the writer live and returns a tool error without imposing a clean-working-tree policy.

A successful handoff is terminal. Any unfinished descendants of that writer are cancelled as part of subtree teardown.

## GitHub Handoff

COW storage and GitHub have separate purposes:

- COW cloning transfers the starting filesystem state into an independent sandbox quickly.
- GitHub refs transfer completed writer changes back to the parent after the child disappears.

The supervisor does not require a clean Git state before spawning a writer. The handoff skill instructs the agent to establish an appropriate commit boundary and push its work. Extension-side Git checks are preflight only. The host supervisor checks remote reachability and exact head identity against its configured canonical GitHub remote, after which the parent receives immutable commit identities and decides whether to inspect, merge, or cherry-pick them.

Branch integration is deliberately outside the supervisor. Normal Git conflicts are handled as normal Git conflicts by the parent session or user.

## Foreground Lifecycle

A supervisor remains a foreground process. A typical root invocation will eventually resemble:

```text
kanedias session --socket ./session.sock
```

Startup is successful only after:

1. the control socket is listening;
2. Incus resources are created and the instance is running;
3. the supervisor connects to Pi RPC;
4. a `get_state` command succeeds;
5. the returned Pi session identity is bound to the supervisor.

Pi provides no normal shutdown RPC command. The supervisor requests graceful shutdown by closing Pi stdin or signaling the process, waits briefly, then escalates if necessary. It subsequently stops and deletes the Incus instance, deletes its workspace volume, and unlinks its socket.

An expected child exit reports its terminal result before the parent releases the blocked delegation call.

## Recursive Failure and Shutdown

Parent ownership is strict in v1:

- stopping a supervisor cancels all unfinished direct children;
- each child recursively cancels its descendants;
- every child monitors an inherited parent-liveness pipe and begins cascading shutdown when that pipe reaches EOF;
- terminal `read`, `write`, and `failure` reports use a separate inherited acknowledgement pipe: the child blocks after writing exactly one terminal report until its direct parent ingests that exact report, marks descendant SSE closure expected, writes one acknowledgement byte, and closes the pipe; cancellation closes the acknowledgement endpoint without acknowledging;
- bootstrap, liveness, report, and terminal-ack endpoints are fixed descriptors `3`–`6`, marked close-on-exec before runtime code so grandchildren cannot retain them;
- inherited child liveness descriptors are made nonblocking before `os.NewFile`, so context cancellation interrupts pending reads;
- graceful shutdown is attempted before forced termination;
- each supervisor normally removes only the resources it created;
- after exact terminal acknowledgement, the parent bounds real-process settlement and escalates through the admitted process group before exact-ticket recovery and registry removal;
- after an admitted direct child process has definitely exited, its direct parent has one narrow recovery exception: it may delete only the deterministic instance, volume, and socket named in that child's exact pre-published ownership ticket after pool, complete identity metadata, workspace name, run attribution, and socket device/inode all match;
- recovery never scans for resources, discovers unrelated sessions, adopts a child, or acts before the exact child process exits;
- no child is detached or adopted;
- a successfully delivered Git handoff remains valid even after the child sandbox is deleted.

Unexpected child failure is returned to the waiting `delegate_session` tool as an error after cleanup. Parent process death closes the liveness pipes so descendants can clean themselves up. A host crash can still leave Incus resources behind; metadata makes them identifiable, but automatic crash reconciliation is deferred.

## Unix Socket API

The supervisor serves HTTP/JSON over a mode-`0600` Unix socket. The API is hybrid: it keeps Pi's command vocabulary where useful and adds explicit session-tree lifecycle operations.

Representative v1 routes:

```text
GET    /v1/tree
GET    /v1/events
GET    /v1/workers
POST   /v1/sessions/{sessionId}/rpc
POST   /v1/sessions/{sessionId}/children
POST   /v1/sessions/{sessionId}/questions/{questionId}/response
POST   /v1/handoff
DELETE /v1/sessions/{sessionId}
```

### Tree

`GET /v1/tree` returns the socket owner's session and every live descendant. Each node includes identity, parent, worker type, resolved model, kind, context mode, lifecycle, current activity, and pending-question summaries.

`GET /v1/workers` returns the configured worker profile names and descriptions needed by the extension to register `delegate_session`. It need not expose model credentials.

There is no endpoint for unrelated root supervisors.

### Routed Pi RPC

`POST /v1/sessions/{sessionId}/rpc` accepts a Pi RPC command for any live node in the subtree. The owning supervisor routes the request down through child supervisors until it reaches the target.

The target supervisor replaces any caller-supplied Pi RPC ID with a private unique ID, correlates the interleaved response, and returns it to the HTTP caller. Concurrent requests are allowed.

This endpoint provides Pi-native operations including:

- `get_state`;
- `get_messages`;
- `get_entries`;
- `get_last_assistant_text`;
- `steer`;
- `follow_up`;
- `abort`;
- model and thinking controls.

Prompt acceptance remains asynchronous exactly as in Pi RPC: the HTTP response confirms command acceptance, while `agent_settled` indicates completion through the event stream.

### Child creation

`POST /v1/sessions/{sessionId}/children` validates the requested worker type, resolves its model profile, starts a direct child, and, for the v1 extension path, waits for its terminal read result or write handoff. The live child appears in `/v1/tree` and `/v1/events` while the request is blocked.

Cancelling the extension tool cancels the corresponding child subtree.

### Handoff

`POST /v1/handoff` is called by the extension in a writer child after guest-side remote preflight. The host then performs the authoritative bounded `ls-remote` check using the configured canonical GitHub remote, records and forwards the verified terminal result upward, and only then writes and flushes acknowledgement to the extension.

### Stop

`DELETE /v1/sessions/{sessionId}` stops the target and its complete descendant subtree. Deleting the socket owner's session shuts down the serving supervisor itself.

## Events and Questions

### Event stream

`GET /v1/events` is a server-sent event stream for the socket owner's complete subtree.

A local Pi event receives a monotonically increasing source sequence. When a child event is forwarded into a parent, the parent assigns a subtree sequence while preserving the source session ID and source sequence.

Representative envelope:

```json
{
  "seq": 184,
  "sessionId": "sess_child",
  "sourceSeq": 42,
  "kind": "pi",
  "payload": {}
}
```

V1 keeps a small bounded in-memory ring for recent replay. Exact eviction, replay-gap, and slow-consumer policies are follow-on hardening work. The implementation invariant is only that client delivery cannot block Pi stdout draining.

### Pending questions

Blocking Pi extension UI requests such as `select`, `confirm`, `input`, and `editor` are retained in a pending-question map rather than relying on the event ring. They remain visible in `/v1/tree` until answered, cancelled, or their session terminates.

The question-response endpoint sends the matching Pi `extension_ui_response`. Repeated or stale answers fail rather than being applied to another question.

Questions from descendants are visible and answerable through the root socket. V1 does not add transcript, steering, or question tools to the Pi extension; those controls remain host-socket operations.

## Event and RPC Semantics Inherited from Pi

The implementation must preserve these verified Pi RPC behaviors:

- framing is LF-delimited JSONL;
- commands may have IDs and responses echo them;
- commands execute concurrently and responses may arrive out of order;
- responses and events interleave on stdout;
- extension UI requests may arrive before the first client command;
- `prompt` success means accepted, not settled;
- `agent_settled` is the session-level completion signal;
- successful `get_state` is the practical readiness handshake;
- extension dialogs can block Pi until answered;
- most Pi events lack durable IDs, so supervisors assign sequence numbers;
- stdout backpressure can stall the agent;
- shutdown uses EOF or process signals rather than an RPC shutdown command.

## V1 State Model

A minimal node lifecycle is sufficient:

```text
provisioning
starting
ready
running
awaiting_handoff
completed
failed
stopping
stopped
```

The parent keeps only live child nodes and the terminal result needed to finish a blocked delegation request. It does not build a permanent run database.

## Deferred Work

After the vertical slice works end to end, follow-on work may add:

- detached delegation and result handles;
- model-facing child status and steering tools;
- durable event journals and reconnect cursors;
- explicit slow-consumer and replay-gap behavior;
- child startup retries and failed-sandbox retention;
- quotas, concurrency policies, depth limits, and usage budgets;
- stronger snapshot quiescing;
- guest-specific capability-restricted API handlers;
- configurable child transcript retention;
- global discovery and observation across independent roots;
- crash reconciliation or operator-assisted cleanup.

None of these should be prerequisites for the v1 happy path.

## V1 Acceptance Scenario

The design is proven when the following path works:

1. start a root supervisor in the foreground;
2. connect to its mode-`0600` Unix socket;
3. send a prompt to the root Pi session;
4. have the root call `delegate_session` with a read-oriented worker type;
5. verify that the configured worker model is selected and observe the COW child and its events through the root socket;
6. query and steer that child through routed Pi RPC;
7. receive the child's normal answer and observe automatic cleanup;
8. have the root call `delegate_session` with a write-oriented worker type using either fresh or forked context;
9. have the writer commit, push, verify, and call `handoff` with exact Git refs;
10. receive those refs in the parent tool result;
11. observe the writer supervisor, Pi session, container, and volume disappear;
12. terminate the root and verify cascading cleanup and socket removal.

## Operational Acceptance Evidence

The reviewed checkout is exercised by the opt-in Incus-tagged harness:

```bash
KANEDIAS_LIVE_SUPERVISOR=1 \
KANEDIAS_CONFIG=./config.toml \
KANEDIAS_E2E_PROVIDER_READY=1 \
KANEDIAS_E2E_DISPOSABLE_GITHUB=1 \
KANEDIAS_E2E_GITHUB_REPOSITORY=owner/disposable-repository \
KANEDIAS_E2E_GITHUB_REMOTE=https://github.com/owner/disposable-repository.git \
go test -tags=incus ./internal/supervisor \
  -run TestLiveRecursiveSupervisorAcceptance -v -count=2
```

The provider and disposable-GitHub flags are separate, explicit authorizations. The test skips before building, starting a proxy, or touching Incus/GitHub when any required authorization or value is absent. It never infers permission from credentials already present on the machine.

Each run builds the current checkout into a mode-private persistent run directory and uses that absolute binary for the owned proxy, root, and recursively spawned children. The proxy is owned and polled by default. `KANEDIAS_E2E_EXTERNAL_PROXY=1` opts into polling and preserving an operator-owned listener; because the harness cannot stop infrastructure it does not own, that mode omits the missing-proxy phase. Release evidence uses the default owned mode. No readiness path uses a fixed sleep.

Before the first session, the harness records the exact project instance and custom-volume baseline. It records tree snapshots, consuming SSE output, process logs, Incus metadata/resource lists, and verified Git refs during the run. Every observed session is tracked by exact `user.kanedias.session_id` metadata. Successful runs remove their run directory; failures retain it below `KANEDIAS_E2E_ARTIFACT_DIR`, or the user cache under `kanedias/e2e`, and print the path.

The live path covers a mode-`0600` root socket, consuming and stalled SSE clients, later RPC progress, fresh read delegation and routed control, a controlled blocking-question fixture, forked writer handoff with an independently resolved remote ref, graceful descendant cascade, parent-liveness cleanup after root `SIGKILL`, and missing-proxy zero-resource failure. A killed root cannot clean its own Incus resources in v1; test teardown removes only the root instance and volume whose metadata exactly matches that root session, then requires the complete baseline to be restored. Host-wide reconciliation remains deferred.
