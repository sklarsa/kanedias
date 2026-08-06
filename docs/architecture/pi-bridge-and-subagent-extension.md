# Kanedias Pi Bridge and Subagent Extension Notes

## Purpose

Kanedias will eventually need to observe and control multiple Pi sessions and their nested subagents from the local Go/Datastar server. This note records a practical bridge architecture and the gaps in `pi-subagents` v0.42.0 that a companion extension, upstream contributions, or a narrow custom fork may need to close.

This is not part of the static Circle of the Fleet mockup. It is a future implementation guide, not a committed wire protocol.

## Verified Upstream Capabilities

`pi-subagents` v0.42.0 exposes an in-process Pi event-bus RPC to other extensions in the same Pi process:

- discovery: `subagents:rpc:v1:ready`;
- requests: `subagents:rpc:v1:request`;
- replies: `subagents:rpc:v1:reply:<requestId>`;
- methods: `ping`, `status`, `spawn`, `steer`, `interrupt`, `resume`, and `stop`;
- live completion event advertised by `ping`: `subagent:async-complete`.

The package also writes useful lifecycle artifacts for async runs:

- `status.json` — current lifecycle, activity, topology, usage, paths, and results;
- `events.jsonl` — lifecycle, tool, control, steering, and nested-run events;
- `output-<index>.log` — human-readable live output;
- session and transcript paths where available;
- `process-terminal.json` — public terminal-state proof.

Richer artifact records may contain nested children, current tools and paths, recent tools/output, model and thinking information, input/output/total tokens, tool and turn counts, elapsed timestamps, attention state, budgets, errors, and output/session paths.

The direct RPC is not a network protocol. A standalone Go process cannot call `pi.events`; it only works between extensions loaded in the same Pi process.

Primary references:

- <https://pi.dev/packages/pi-subagents>
- <https://github.com/nicobailon/pi-subagents/blob/v0.42.0/docs/extension-api.md>
- <https://github.com/nicobailon/pi-subagents/blob/v0.42.0/docs/observability.md>
- <https://github.com/nicobailon/pi-subagents/blob/v0.42.0/docs/tool-reference.md>
- <https://github.com/nicobailon/pi-subagents/blob/v0.42.0/docs/workflows.md>

## Recommended Architecture

### 1. Trusted Pi Bridge Extension

Create a small Kanedias extension loaded in each Pi process that Kanedias should supervise. It should:

1. listen for `subagents:rpc:v1:ready`;
2. call `ping` and record the negotiated protocol version, methods, capabilities, Pi session ID, session file, and working directory;
3. issue official RPC calls for status, spawn, steer, interrupt, resume, and stop;
4. capture live start, completion, terminal-proof, attention, and supervisor-question signals available to the owning Pi session;
5. normalize the information into a small Kanedias bridge protocol;
6. expose that protocol only over a same-user Unix socket, or authenticated loopback transport where a socket is impractical.

The bridge must remain an adapter. It should not duplicate orchestration, invent child state, write package control files, or bypass pi-subagents’ ownership checks.

### 2. Go Lifecycle Reconciler

The Go server should reconcile durable and live information:

1. receive authoritative run IDs and `asyncDir` paths from the bridge;
2. read `status.json` as a replaceable snapshot;
3. tail `events.jsonl` from a stored byte offset while tolerating a partial final line;
4. tail only allowlisted transcript/output paths discovered from trusted lifecycle records;
5. rebuild state after server or bridge reconnect instead of assuming process events replay;
6. maintain a redacted Kanedias-owned run index because the upstream completion-result JSON is transient and upstream retention is not a durable product history contract.

The reconciler should deduplicate records by stable identities such as run ID, result index, nested child ID, supervisor request ID, and event identity where present. It must preserve `omitted` counts and treat unknown fields/events as forward-compatible input.

### 3. Kanedias View Model

The browser should never receive raw upstream files or arbitrary filesystem paths. The Go server should publish a versioned, redacted model resembling:

```text
PiSession
  id
  cwdLabel
  bridgeState
  runs[]
  unansweredQuestions

Run
  id
  parentId
  childIndex
  agent
  role
  goal
  lifecycle
  activity
  usage
  capabilities
  children[]
  selectedTranscriptTail
  pendingQuestions[]

PendingQuestion
  id
  runId
  childId
  reason
  prompt
  interview
  createdAt
  expiresAt
```

This sketch intentionally omits raw prompts, environment data, absolute paths, credentials, and unrestricted tool arguments.

### 4. Control Path

All controls must return through the bridge to the owning Pi process:

```text
Browser → Datastar/Go → authenticated bridge → pi-subagents RPC or supervisor reply
```

The Go server must not forge control-inbox files or capability tokens. The UI should feature-detect controls per run:

- `steer` means Pi accepted the guidance, not that the model complied;
- `interrupt` is a soft pause and may target explicit nested children;
- `resume` starts a new child process from a persisted session and cannot revive stopped runs;
- `stop` applies to current-session top-level async runs, not arbitrary nested children;
- external CLI agents have reduced steering, resume, and tool-event support.

Destructive stop actions should require explicit confirmation. Steering and answers should show delivery acknowledgement and subsequent child activity as separate states.

## Upstream Shortcomings Relevant to Kanedias

### Compact Fleet Status Is Display-Only

The versioned fleet DTO is active-only, capped at 16 entries, and deliberately omits control IDs, run IDs, topology, current tools, and detailed lifecycle state. It is useful for a small status widget, not the Circle of the Fleet.

**Kanedias workaround:** reconcile rich artifacts and generic status details behind the bridge.

**Potential upstream fix:** add an optional versioned detailed fleet/status projection with stable target IDs, parent IDs, child indexes, lifecycle, activity, current tool, usage, and per-run capability flags. Keep it bounded and preserve explicit `omitted` counts.

### Transcript Tailing Is Not an RPC Method

The human tool supports transcript-tail views, but RPC status discards `view` and `lines`. A consumer must read output/session/transcript artifacts itself.

**Kanedias workaround:** the Go reconciler tails trusted artifact paths and publishes redacted transcript events.

**Potential upstream fix:** add a versioned `transcript` method using run/child identity, a bounded line or byte limit, and an opaque cursor. It should return redacted structured messages where possible rather than arbitrary file contents.

### Full Supervisor Questions Are Not Exposed by RPC

`need_decision` and `interview_request` are visible to the exact owning Pi session and use temporary supervisor-channel request files. Lifecycle state can indicate `needs_attention`, but it does not reliably preserve the full question or interview structure. There is no documented supervisor pending/reply RPC method.

**Kanedias workaround:** the same-process bridge captures owning-session supervisor requests and forwards only validated question fields. Replies return through the owning session’s supervisor API.

**Potential upstream fix:** add versioned `supervisor.pending` and `supervisor.reply` methods plus a replayable supervisor-request event. Requests need stable IDs, run/child targets, expiry, reply expectation, reason, text, and optional structured interview data.

### Live Events Have No Replay Cursor

Ready, start, completion, terminal, and supervisor signals are process-local live events. A reconnecting bridge cannot ask the event bus to replay what it missed.

**Kanedias workaround:** rescan lifecycle directories and resume JSONL tails from stored offsets.

**Potential upstream fix:** add monotonically increasing per-session event sequence numbers and a bounded replay/journal method. The result should define retention and explicitly report when a requested cursor is too old.

### Completion Shape and Retention Are Not Stable Product History

The full completion payload is not independently versioned, and the temporary completion-result JSON is deleted after accepted delivery. Lifecycle directory retention is not a durable external history contract.

**Kanedias workaround:** consume completion as a hint, then persist a small redacted index owned by Kanedias.

**Potential upstream fix:** publish a versioned completion DTO and documented retention policy, or provide an explicit durable run-history query.

### Generic Detailed Status Is Not a Dedicated Stable DTO

RPC status can return broad package details, but only the compact fleet projection has a narrow versioned schema.

**Kanedias workaround:** capability-negotiate, feature-detect, ignore unknown fields, and prefer documented lifecycle records for detail.

**Potential upstream fix:** version detailed lifecycle/status records independently of human tool output.

### No Supported Cross-Process Control Protocol

Package internals coordinate processes through protected files and tokens, but those are not a public protocol for arbitrary clients.

**Kanedias workaround:** keep control in the same-process bridge.

**Potential upstream fix:** none is required if the extension RPC remains the supported authority boundary. A separately packaged official bridge could be useful, but exposing raw control files would weaken ownership and security.

### Nested Stop and Capability Reporting Are Limited

Explicit nested children can be inspected, steered, interrupted, or resumed under documented conditions, while stop remains a top-level async-run action. Capability differences are scattered across runner and lifecycle metadata.

**Kanedias workaround:** render controls from per-run capabilities and explain when only top-level stop is possible.

**Potential upstream fix:** expose one versioned per-target capability object. Nested stop should only be added if pi-subagents can preserve parent workflow invariants and report the resulting partial state safely.

## Companion First, Fork Last

The preferred sequence is:

1. build the Kanedias bridge as a separate companion extension against the public v1 RPC and artifact contracts;
2. keep all Kanedias-specific transport, authentication, redaction, indexing, and UI DTOs outside pi-subagents;
3. propose generally useful missing RPC projections upstream with tests and backwards-compatible capability negotiation;
4. carry a narrow patch set or fork only when a required owning-session signal cannot be consumed or added upstream in time.

A fork should not rename existing channels or silently change v1 payloads. New behavior should be advertised through `ping.capabilities`, use new method names or explicit schema versions, and allow Kanedias to fall back to artifact reconciliation when absent.

## Security Requirements

The bridge and reconciler operate over highly sensitive data. Session files and transcripts may contain source code, prompts, absolute paths, environment details, tool arguments, secrets, or model output.

Required boundaries:

- Unix socket permissions restricted to the current user; if loopback HTTP is used, require an ephemeral high-entropy bearer secret;
- bind only to loopback and validate browser origin/CSRF for state changes;
- scope every bridge to its exact Pi session ID and reject cross-session control;
- accept artifact paths only when supplied by the trusted bridge/lifecycle record and contained beneath an allowlisted run directory;
- never expose raw filesystem paths as browser-controlled inputs;
- redact secrets and unsafe tool arguments before persistence or rendering;
- escape all transcript content as text, never trusted HTML;
- bound transcript tails, event rates, stored history, and payload sizes;
- distinguish observed terminal proof from guessed process death;
- log control requests without logging their sensitive full message bodies.

## First Implementation Slice

When the live feature is approved, the smallest useful slice should be read-only:

1. bridge discovery and `ping` capability negotiation;
2. async start/complete capture;
3. artifact-based top-level and nested run snapshots;
4. redacted recent transcript and current-tool display;
5. full supervisor-question capture if the Pi extension API provides a stable observation hook;
6. reconnect and replay from lifecycle files;
7. no spawn, answer, steer, interrupt, resume, stop, or shell yet.

A second slice can add question replies and acknowledged steering. Spawn, interruption/resume, stop, and shell access should follow only after session ownership, authentication, audit logging, and reconnect behavior are tested end to end.

## Open Questions for a Future Spec

- Which Pi extension hook reliably observes owning-session custom supervisor messages without scraping the visible transcript?
- Should one bridge process expose one Pi session or multiplex several sessions with separate credentials?
- What redaction rules are required for tool arguments and transcript content?
- How long should Kanedias retain completed-run summaries and transcript tails?
- Should Kanedias manage only runs it spawned, or all current-session runs observed from pi-subagents?
- What is the user-confirmation policy for stop, interrupt, and shell commands?
- Which missing methods should be proposed upstream before considering a fork?
