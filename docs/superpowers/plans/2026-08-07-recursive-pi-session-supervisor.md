# Recursive Pi Session Supervisor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a foreground, host-side, recursively composable supervisor where every Pi session owns an independent Incus sandbox, exposes a hybrid Unix-socket API, and can synchronously delegate read or write work to COW-cloned child sessions.

**Architecture:** Add a new `internal/supervisor` runtime rather than stretching the one-shot `internal/session` spike. Freeze Go/TypeScript contracts and the persistent Pi RPC pump first, then develop the root runtime/API, Incus COW provisioner, and lightweight Pi extension in parallel branches. Integrate those seams into recursive child processes, then add read/write completion, Git handoff, and a live acceptance harness.

**Tech Stack:** Go 1.26.5, Incus Go client v7.3.0, Pi coding agent 0.83.0, TypeScript, Node.js 22, TypeBox 1.3.7, HTTP/JSON and SSE over Unix sockets, GitHub refs.

## Global Constraints

- `docs/architecture/session-supervisor.md` is the canonical approved design.
- One supervisor is permanently bound to one Pi session; reject `new_session`, `switch_session`, `fork`, and `clone` through routed raw RPC.
- Every child gets a separate COW-cloned Incus instance root and custom workspace volume; never use a shared workspace or Git worktree as a runtime fallback.
- V1 supports the currently deployed same-project, same-pool `btrfs` configuration. Fail child provisioning closed for any unvalidated storage driver or cross-pool copy.
- `kanedias proxy run` must be reachable on the configured Incus gateway port `3128` before any session-owned resource is created.
- `workerType`, `kind: read | write`, `context: fresh | fork`, and non-empty `task` are required for child creation.
- Worker type is resolved from trusted host configuration to provider, model, and optional thinking level; child requests cannot override the model directly.
- The custom Pi extension registers only `delegate_session` and `handoff`; transcript, steering, question, and stop controls remain on the host socket.
- Delegation blocks synchronously in v1. Tool cancellation stops the corresponding child subtree.
- Read children return the requested final answer. Write children return remotely verified Git refs and terminate after acknowledged handoff.
- Recent events are bounded and memory-only. Pending blocking UI questions are retained separately until answered, cancelled, or terminated.
- A root socket exposes its complete live descendant tree and routes Pi RPC, questions, and stop requests recursively.
- Parent shutdown and parent-liveness EOF cascade through unfinished descendants. V1 does not adopt orphaned sessions after a host crash.
- Keep `internal/server` and the existing browser/Datastar server out of the supervisor implementation.
- Keep one writer per implementation branch. Parallel workers may edit only the isolated file ownership lanes listed below.
- Review only at the six PR-sized gates below, never after individual TDD steps. Consolidate each gate into at most one normal fix pass; run a second review only when that pass materially changes behavior or leaves a blocker.

---

## Delivery Graph and Parallel Ownership

```text
PR 1: Contracts, worker configuration, Pi RPC pump, provision interfaces
  ├── PR 2: Runtime state, events, questions ──► PR 3: Root provision + Unix API
  ├── PR 4: Incus COW child provisioner                       ┐ parallel
  └── PR 5: Pi extension + staged image assets                ┘ lanes
                         │
                         ▼
PR 6: Child process bootstrap and parent-liveness
                         │
                         ▼
PR 7: Recursive registry, routing, and child HTTP API
                         │
                         ▼
PR 8: Activate extension; fresh/fork read delegation and CLI cutover
                         │
                         ▼
PR 9: Write completion and verified Git handoff
                         │
                         ▼
PR 10: Live acceptance, leak checks, and spike removal
```

### Merge-conflict ownership

| Files or package | Sole writer until integration |
|---|---|
| `internal/supervisor/contract/**` | PR 1 contract owner |
| `internal/supervisor/pirpc/**` | PR 1 RPC owner |
| `internal/config/config.go`, `config.toml` | PR 1 configuration owner |
| `internal/supervisor/**` excluding `contract`, `pirpc`, `process`, and `provision` | PR 2, then PR 7 integration owner |
| `internal/supervisorapi/**` | PR 3, then PR 7 integration owner |
| `internal/incusclient/{instance,storage}.go` and clone tests | PR 4 Incus owner |
| `internal/supervisor/provision/**` implementations | root files: PR 3; child files: PR 4; shared types stay PR 1-owned |
| `internal/image/pi-extension/**`, extension Node lockfile | PR 5 extension owner |
| `internal/image/{image.go,image_test.go,install.sh}` | PR 5 extension/image owner |
| `internal/supervisor/process/**`, `cmd/session_child.go`, minimal hidden-command registration | PR 6 process owner |
| `cmd/{session.go,root.go,root_test.go}` public session behavior | PR 8 CLI integrator after PR 6 rebases |
| Live supervisor harness | PR 10 acceptance owner |

PRs 2, 4, and 5 branch from PR 1 and develop in parallel without editing frozen contract/provision seams. PR 3 follows PR 2. If a contract change is unavoidable, the PR 1 owner makes one compatibility commit and regenerates both Go and TypeScript fixtures before dependent branches rebase.

### PR-sized review gates

Do not review after every TDD step or small commit. Use six substantial gates:

1. **Foundation gate:** PR 1 contracts, worker policy, provision seams, and Pi RPC concurrency.
2. **Root gate:** combined PRs 2–3 runtime state, root ownership, Unix API, SSE, and questions.
3. **Parallel-lanes gate:** PRs 4–5 reviewed concurrently as two complete diffs: Incus/COW and extension/image.
4. **Recursion gate:** combined PRs 6–7 process bootstrap, liveness, registry, routing, and synchronous child API.
5. **Delegation gate:** PRs 8–9 reviewed together, with parallel read-path and writer-security/order reviewers.
6. **Release gate:** PR 10 live acceptance, leaks, final deletions, and deferred-scope compliance.

At each gate, run focused checks and `go test ./...`, then launch one parallel fresh-context review wave with distinct correctness, tests, and security/operations angles. The parent consolidates blockers and fixes worth doing now into one writer pass. A second review happens only when that pass materially changes behavior or leaves a blocker unresolved.

---

## Target File Map

### New Go packages

```text
internal/supervisor/
├── contract/
│   ├── types.go          # dependency-neutral request/result DTOs and enums
│   ├── errors.go         # typed domain errors and HTTP-safe codes
│   └── testdata/         # canonical Go/TypeScript JSON fixtures
├── events.go             # bounded subtree event broker
├── questions.go          # retained blocking extension UI questions
├── identity.go           # immutable Kanedias and bound Pi identity
├── lifecycle.go          # legal node-state transitions
├── node.go               # one local supervised session
├── children.go           # direct-child registry and synchronous waiters
├── router.go             # recursive target selection and forwarding
├── result.go             # read/write terminal result cells
├── stop.go               # idempotent cascade
├── pirpc/
│   ├── client.go         # permanent JSONL reader and correlated writes
│   ├── protocol.go       # raw Pi envelopes and verified command names
│   └── client_test.go
├── process/
│   ├── protocol.go       # inherited-FD bootstrap/report messages
│   ├── spawn.go          # child supervisor process launch
│   ├── liveness.go       # parent-liveness EOF monitor
│   └── process_test.go
└── provision/
    ├── types.go          # resource plans, ownership ledger, interfaces
    ├── root.go           # base-image root provisioning
    ├── child.go          # stopped COW clone and device replacement
    ├── cleanup.go        # bounded idempotent resource cleanup
    └── provision_test.go

internal/supervisorapi/
├── handler.go            # chi router and strict JSON/error helpers
├── unix.go               # mode-0600 listener lifecycle
├── client.go             # parent-to-child HTTP-over-Unix client
├── events.go             # standard SSE
└── handler_test.go
```

### New Pi extension package

```text
internal/image/pi-extension/
├── package.json
├── package-lock.json
├── tsconfig.json
├── src/
│   ├── index.ts
│   ├── schemas.ts
│   ├── supervisor-client.ts
│   ├── fork.ts
│   ├── git-handoff.ts
│   └── types.ts
├── skills/
│   ├── delegate-session/SKILL.md
│   └── writer-handoff/SKILL.md
└── test/
    ├── tools.test.ts
    ├── supervisor-client.test.ts
    ├── fork.test.ts
    └── git-handoff.test.ts
```

### Existing files changed during integration

```text
cmd/session.go
cmd/root.go
cmd/root_test.go
config.toml
assets/pi-settings.json
internal/config/config.go
internal/config/config_test.go
internal/image/image.go
internal/image/image_test.go
internal/image/install.sh
internal/image/kanedias-pi-rpc
internal/incusclient/instance.go
internal/incusclient/instance_test.go
internal/incusclient/storage.go
internal/incusclient/storage_test.go
```

`internal/session/**` remains untouched until PR 8 removes the superseded spike.

The dependency direction is strict: `supervisor` may import `contract`, `pirpc`, `process`, and `provision`; those subpackages must never import the parent `supervisor` package. `process` and `provision` exchange only `contract` types or their own primitive wire/resource types. This prevents Go import cycles while the PR 2, PR 4, and PR 5 lanes develop in parallel and PR 3 follows PR 2.

---

### Task 1: Freeze Contracts, Worker Profiles, and the Persistent Pi RPC Pump

**PR-sized deliverable:** A race-tested internal protocol foundation with no supervisor process or Incus child creation yet.

**Files:**
- Create: `internal/supervisor/contract/types.go`
- Create: `internal/supervisor/contract/types_test.go`
- Create: `internal/supervisor/contract/errors.go`
- Create: `internal/supervisor/contract/testdata/create-child-read.json`
- Create: `internal/supervisor/contract/testdata/create-child-write.json`
- Create: `internal/supervisor/contract/testdata/read-result.json`
- Create: `internal/supervisor/contract/testdata/write-result.json`
- Create: `internal/supervisor/contract/testdata/error.json`
- Create: `internal/supervisor/pirpc/protocol.go`
- Create: `internal/supervisor/pirpc/client.go`
- Create: `internal/supervisor/pirpc/client_test.go`
- Create: `internal/supervisor/provision/types.go`
- Create: `internal/supervisor/provision/ownership.go`
- Create: `internal/supervisor/provision/ownership_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `config.toml`

**Interfaces:**
- Produces: `config.WorkerProfile`, `Config.ResolveWorker`, `contract.CreateChildRequest`, `contract.ReadChildResult`, `contract.WriteChildResult`, complete `contract.ErrorCode`/HTTP mappings, `pirpc.Client.Call`, `pirpc.Client.Send`, `pirpc.Client.Events`, `provision.RootRequest`, `provision.ChildRequest`, `provision.Resources`, and `provision.Ownership`.
- Consumes: Pi RPC 0.83.0 LF-delimited JSONL and exact `get_state`/extension UI envelopes documented in the installed Pi package.

- [ ] **Step 1: Add failing worker-profile configuration tests**

Add table tests that load and validate this exact TOML shape:

```toml
[workers.reviewer]
description = "Review code and designs without modifying files."
provider = "openai-codex"
model = "gpt-5.6-sol"
thinking_level = "xhigh"

[workers.worker]
description = "Implement changes and hand off pushed Git refs."
provider = "openai-codex"
model = "gpt-5.6-sol"
thinking_level = "high"
```

The test must assert deterministic sorted listing, exact resolution by name, rejection of an unknown worker, and rejection of empty description/provider/model or a thinking level outside:

```go
var validThinkingLevels = map[string]struct{}{
    "off": {}, "minimal": {}, "low": {}, "medium": {},
    "high": {}, "xhigh": {}, "max": {},
}
```

- [ ] **Step 2: Run the configuration tests and confirm the new API is absent**

Run:

```bash
go test ./internal/config -run 'TestLoadWorkers|TestResolveWorker|TestValidateSupervisorWorkers' -count=1
```

Expected: compile failure because `WorkerProfile`, `ResolveWorker`, and `ValidateSupervisor` do not exist.

- [ ] **Step 3: Implement worker configuration and fix the malformed repository list**

Add:

```go
type WorkerProfile struct {
    Description   string `toml:"description" json:"description"`
    Provider      string `toml:"provider" json:"provider"`
    Model         string `toml:"model" json:"model"`
    ThinkingLevel string `toml:"thinking_level" json:"thinkingLevel,omitempty"`
}

type Config struct {
    Network   Network                  `toml:"network"`
    BaseImage BaseImage                `toml:"base_image"`
    Workspace Workspace                `toml:"workspace"`
    Workers   map[string]WorkerProfile `toml:"workers"`
    Dir       string                   `toml:"-"`
}

func (cfg Config) ResolveWorker(name string) (WorkerProfile, error)
func (cfg Config) WorkerNames() []string
func (cfg Config) ValidateSupervisor() error
```

`ValidateSupervisor` must call `ValidateLifecycle`, require at least one worker, validate each map key and profile, and leave image/workspace-only commands free to call the existing `ValidateLifecycle`.

Add the missing comma between `"sklarsa/incus-azure-pipelines"` and `"sklarsa/kanedias"` in `config.toml`, then add `researcher`, `reviewer`, and `worker` profiles.

- [ ] **Step 4: Run focused configuration tests**

Run:

```bash
go test ./internal/config -count=1
```

Expected: PASS.

- [ ] **Step 5: Add failing JSON contract round-trip tests**

Define and test these exact public DTOs:

```go
type ChildKind string
const (
    ChildKindRoot  ChildKind = "root"
    ChildKindRead  ChildKind = "read"
    ChildKindWrite ChildKind = "write"
)

type ContextMode string
const (
    ContextRoot  ContextMode = "root"
    ContextFresh ContextMode = "fresh"
    ContextFork  ContextMode = "fork"
)

type ForkSpec struct {
    SessionFile string `json:"sessionFile"`
    PiSessionID string `json:"piSessionId"`
    LeafEntryID string `json:"leafEntryId"`
}

type ModelProfile struct {
    Provider      string `json:"provider"`
    Model         string `json:"model"`
    ThinkingLevel string `json:"thinkingLevel,omitempty"`
}

type WorkerSummary struct {
    WorkerType  string       `json:"workerType"`
    Description string       `json:"description"`
    Profile     ModelProfile `json:"profile"`
}

type CreateChildRequest struct {
    WorkerType string      `json:"workerType"`
    Kind       ChildKind   `json:"kind"`
    Context    ContextMode `json:"context"`
    Task       string      `json:"task"`
    Fork       *ForkSpec   `json:"fork,omitempty"`
}

type RepositoryHandoff struct {
    Repository string `json:"repository"`
    BaseCommit string `json:"baseCommit"`
    Branch     string `json:"branch"`
    HeadCommit string `json:"headCommit"`
}

type ReadChildResult struct {
    Kind       ChildKind `json:"kind"`
    WorkerType string    `json:"workerType"`
    SessionID  string    `json:"sessionId"`
    Output     string    `json:"output"`
}

type WriteChildResult struct {
    Kind         ChildKind          `json:"kind"`
    WorkerType   string             `json:"workerType"`
    SessionID    string             `json:"sessionId"`
    Repositories []RepositoryHandoff `json:"repositories"`
    Summary      string             `json:"summary"`
    Verification []string           `json:"verification"`
}
```

Pure DTO tests must reject invalid enum combinations, require fork data only for `context: fork`, and forbid it for fresh context. Strict unknown-field decoding plus task/summary/output and request-body limits belong to the PR 3 HTTP boundary tests.

- [ ] **Step 6: Run contract tests and confirm failure**

Run:

```bash
go test ./internal/supervisor/contract -run Contract -count=1
```

Expected: compile failure because the contract package does not exist.

- [ ] **Step 7: Implement contracts, typed errors, and canonical fixtures**

Use error codes:

```go
type ErrorCode string
const (
    ErrorInvalidRequest       ErrorCode = "invalid_request"
    ErrorUnknownWorkerType    ErrorCode = "unknown_worker_type"
    ErrorForbiddenRPC         ErrorCode = "forbidden_rpc"
    ErrorProxyUnavailable     ErrorCode = "proxy_unavailable"
    ErrorProvisioningFailed   ErrorCode = "provisioning_failed"
    ErrorChildFailed          ErrorCode = "child_failed"
    ErrorChildAborted         ErrorCode = "child_aborted"
    ErrorHandoffRefMissing    ErrorCode = "handoff_ref_missing"
    ErrorHandoffRefMismatch   ErrorCode = "handoff_ref_mismatch"
    ErrorSessionStopping      ErrorCode = "session_stopping"
    ErrorNotFound             ErrorCode = "not_found"
    ErrorChildUnavailable     ErrorCode = "child_unavailable"
    ErrorConflict             ErrorCode = "conflict"
    ErrorInternal             ErrorCode = "internal"
)
```

Write canonical fixtures with stable field ordering through `json.MarshalIndent`. Child-request validation must reject `kind: root`; that value exists only for local root snapshots. Freeze HTTP mappings in tests: invalid request `400`, forbidden/conflict/stopping `409`, not found `404`, saturation `429`, and child unavailable `502`. Both Go and the TypeScript package in PR 5 must consume these checked-in examples.

- [ ] **Step 8: Freeze dependency-neutral provisioning types and ownership tests**

Define these seams in PR 1 so root and child provisioning can develop independently:

```go
type Resources struct {
    SessionID string
    Pool      string
    Instance  string
    Volume    string
    RPCAddr   string
}

type RootRequest struct {
    SessionID string
    SocketPath string
}

type ChildRequest struct {
    SessionID     string
    ParentID      string
    RootID        string
    SourceInstance string
    SourceVolume   string
    HostSocketPath string
    Worker         config.WorkerProfile
    Contract       contract.CreateChildRequest
}

type RootProvisioner interface {
    ProvisionRoot(context.Context, RootRequest) (*Resources, error)
    Destroy(context.Context, *Resources) error
}

type ChildProvisioner interface {
    ProvisionChild(context.Context, ChildRequest) (*Resources, error)
    Destroy(context.Context, *Resources) error
}
```

`Ownership` records submitted instance/volume operations independently from confirmed completion so cleanup can probe ambiguous outcomes. It contains no Incus client and imports no parent `supervisor` package.

- [ ] **Step 9: Add failing concurrent Pi RPC transport tests**

Use `net.Pipe` and cover:

```go
func TestClientCorrelatesOutOfOrderResponses(t *testing.T)
func TestClientDrainsEventBeforeFirstCommand(t *testing.T)
func TestClientReplacesCallerID(t *testing.T)
func TestClientSerializesConcurrentWrites(t *testing.T)
func TestClientCancellationRemovesPendingCall(t *testing.T)
func TestClientEOFFailsEveryPendingCall(t *testing.T)
func TestClientRejectsPartialMalformedAndOversizedRecords(t *testing.T)
func TestClientKeepsReadingAfterAgentSettled(t *testing.T)
func TestClientRejectsSessionReplacementWithoutWriting(t *testing.T)
func TestClientSendWritesExtensionUIResponseWithoutWaiting(t *testing.T)
```

The out-of-order test must start two `Call` goroutines, decode their private IDs on the peer, respond in reverse order, and assert each caller receives its own response.

- [ ] **Step 10: Run RPC tests and confirm failure**

Run:

```bash
go test ./internal/supervisor/pirpc -count=1
```

Expected: compile failure because `pirpc.Client` does not exist.

- [ ] **Step 11: Implement the permanent JSONL pump**

Expose:

```go
type Event struct {
    Type string
    Raw  json.RawMessage
}

type Client struct {
    conn    io.ReadWriteCloser
    events  chan Event
    done    chan struct{}
}

const MaxRecordBytes = 4 << 20

func NewClient(conn io.ReadWriteCloser) *Client
func (c *Client) Call(ctx context.Context, command json.RawMessage) (json.RawMessage, error)
func (c *Client) Send(ctx context.Context, command json.RawMessage) error
func (c *Client) Events() <-chan Event
func (c *Client) Done() <-chan struct{}
func (c *Client) Err() error
func (c *Client) Close() error
```

Implementation rules:

- Start exactly one reader goroutine in `NewClient`.
- Parse with `bufio.Reader.ReadSlice('\n')` using a `MaxRecordBytes+1` buffer; terminate immediately on `bufio.ErrBufferFull` so an unterminated oversized record cannot allocate without bound.
- Generate private IDs with an atomic counter plus random process prefix.
- Serialize writes with one mutex.
- Store pending result channels buffered to one.
- Dispatch `type=response` records with matching IDs to pending calls; publish all other records to the internal event channel.
- Keep reading after `agent_settled`.
- Reject the four identity-changing command types before any write.
- Close all pending calls with the terminal stream error on EOF or malformed input.
- Never invoke HTTP/SSE writers from the reader goroutine.

- [ ] **Step 12: Run race-enabled foundation tests**

Run:

```bash
go test -race ./internal/config ./internal/supervisor/... -count=1
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 13: Commit PR 1**

```bash
git add config.toml internal/config internal/supervisor
git commit -m "feat: add supervisor contracts and Pi RPC transport"
```

- [ ] **Step 14: Run foundation review gate 1**

Review the complete PR 1 diff for protocol fidelity, acyclic seams, concurrency, cancellation, error completeness, and test coverage. Do not request review after individual steps 1–13.

---

### Task 2: Build Runtime State, Event Buffering, Questions, and Local Pi Ownership

**PR-sized deliverable:** A resource-independent local session runtime that binds one injected Pi RPC connection, owns immutable identity/lifecycle, buffers events, retains questions, and supports local control. Incus and HTTP remain PR 3.

**Files:**
- Create: `internal/supervisor/identity.go`
- Create: `internal/supervisor/lifecycle.go`
- Create: `internal/supervisor/events.go`
- Create: `internal/supervisor/questions.go`
- Create: `internal/supervisor/local.go`
- Create: `internal/supervisor/result.go`
- Create: `internal/supervisor/*_test.go`

**Interfaces:**
- Consumes: PR 1 contracts, worker resolution, and `pirpc.Client`.
- Produces: `supervisor.LocalSession`, `supervisor.NodeSnapshot`, `supervisor.EventBroker`, and `supervisor.QuestionStore` for PR 3.

- [ ] **Step 1: Add failing identity and lifecycle tests**

Test immutable construction and this legal transition graph:

```text
provisioning -> starting -> ready -> running
running -> ready                    # reusable root settles
running -> awaiting_handoff         # writer settles without handoff
awaiting_handoff -> running         # prompt/follow-up resumes writer
ready|running|awaiting_handoff -> completed|failed|stopping
provisioning|starting -> failed|stopping
completed|failed -> stopping
stopping -> stopped
```

A second Pi binding, changed Pi session ID, empty session file, or any backwards transition must return an invariant error.

- [ ] **Step 2: Implement identity and lifecycle state**

Use private fields and snapshot methods:

```go
type Identity struct {
    sessionID string
    parentID  string
    rootID    string
    kind      ChildKind
    context   ContextMode
    worker    string
}

type PiBinding struct {
    SessionID   string `json:"sessionId"`
    SessionFile string `json:"sessionFile"`
}

type WorkerCatalog interface {
    Resolve(name string) (config.WorkerProfile, error)
    Summaries() []contract.WorkerSummary
}

type NodeSnapshot struct {
    SessionID       string                   `json:"sessionId"`
    PiSessionID     string                   `json:"piSessionId,omitempty"`
    SessionFile     string                   `json:"sessionFile,omitempty"`
    ParentSessionID string                   `json:"parentSessionId,omitempty"`
    RootSessionID   string                   `json:"rootSessionId"`
    Kind            contract.ChildKind       `json:"kind"`
    Context         contract.ContextMode     `json:"context"`
    WorkerType      string                   `json:"workerType,omitempty"`
    Model            contract.ModelProfile   `json:"model"`
    Lifecycle       string                   `json:"lifecycle"`
    Questions       []QuestionSummary        `json:"pendingQuestions"`
    Children        []NodeSnapshot           `json:"children"`
}
```

Bind Pi identity only from a successful `get_state` response. Sort questions and children deterministically before returning a snapshot.

- [ ] **Step 3: Add failing event-broker and slow-subscriber tests**

Use a ring capacity of 4,096 envelopes and subscriber mailbox capacity of 128 in production defaults. Tests use smaller capacities and assert:

- monotonic local `SourceSeq` and subtree `Seq`;
- forwarded events preserve source session and source sequence;
- ring eviction removes only the oldest envelope;
- a new subscriber receives the currently retained ring followed by live events;
- subscriber overflow disconnects only that subscriber;
- publishing 10,000 events completes while a subscriber never reads.

- [ ] **Step 4: Implement the bounded event broker**

Expose:

```go
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
}
```

Publication may lock only long enough to update the ring and copy subscribers. Mailbox sends are nonblocking. `Subscribe()` returns an immutable copy of the retained ring in `Replay` plus a live mailbox; SSE writes `Replay` before reading `Events`. V1 defines no `Last-Event-ID`, caller cursor, or gap protocol.

- [ ] **Step 5: Add failing pending-question tests from real Pi fixtures**

Check in the documented `select`, `confirm`, `input`, and `editor` request examples. Assert they remain pending after event-ring eviction, answer exactly once, restore to pending when `pirpc.Send` fails, and clear on node termination.

- [ ] **Step 6: Implement question parsing and consume-on-success responses**

Store raw payload plus normalized summary. Build exact Pi responses:

```json
{"type":"extension_ui_response","id":"uuid-1","value":"Allow"}
{"type":"extension_ui_response","id":"uuid-2","confirmed":true}
{"type":"extension_ui_response","id":"uuid-3","cancelled":true}
```

Do not retain fire-and-forget methods such as `notify` as pending questions.

- [ ] **Step 7: Add failing local Pi ownership tests**

Inject `net.Pipe` into `LocalSession` and assert initial events are buffered before readiness, `get_state` binds identity exactly once, root `running -> ready` occurs on settlement, writer `awaiting_handoff -> running` is legal, forbidden RPC stays unwritten, and EOF fails pending calls.

- [ ] **Step 8: Implement `LocalSession` without Incus or HTTP dependencies**

```go
type LocalSession struct {
    identity Identity
    binding  PiBinding
    rpc      *pirpc.Client
    events   *EventBroker
    questions *QuestionStore
}

func NewLocalSession(identity Identity, rpc *pirpc.Client, events *EventBroker) *LocalSession
func (s *LocalSession) Bind(context.Context) error
func (s *LocalSession) CallRPC(context.Context, json.RawMessage) (json.RawMessage, error)
func (s *LocalSession) Snapshot() NodeSnapshot
func (s *LocalSession) StopRPC() error
```

`Bind` issues `get_state`; no caller may mark readiness from a TCP dial alone.

- [ ] **Step 9: Verify and commit PR 2**

```bash
go test -race ./internal/supervisor/... -count=1
go test ./... -count=1
git add internal/supervisor
git commit -m "feat: add supervised Pi runtime state"
```

Do not review this half in isolation; review it with PR 3 at root gate 2.

---

### Task 3: Add Root Provisioning and the Unix Control API

**PR-sized deliverable:** Compose PR 2’s local Pi runtime with root Incus ownership and a mode-0600 Unix API. Do not wire the public `session` command yet.

**Files:**
- Create: `internal/supervisor/node.go`
- Create: `internal/supervisor/stop.go`
- Create: `internal/supervisor/provision/root.go`
- Create: `internal/supervisor/provision/root_test.go`
- Create: `internal/supervisorapi/handler.go`
- Create: `internal/supervisorapi/unix.go`
- Create: `internal/supervisorapi/events.go`
- Create: `internal/supervisorapi/handler_test.go`

**Interfaces:**
- Consumes: PR 1 `provision` seams and PR 2 `LocalSession`.
- Produces: `supervisor.Node`, `supervisorapi.NewHandler`, and `supervisorapi.ServeUnix` for PR 7.

- [ ] **Step 1: Extract root provisioning behind the PR 1 resource owner**

Port the proxy check, network/profile preparation, seed-volume clone, image-based instance creation, start, RPC address discovery, and cleanup from `internal/session/session.go` behind PR 1’s `provision.RootProvisioner`.

Bind the host Unix listener before provisioning, then include this instance-local device in the root `InstancesPost` before the instance starts:

```go
"supervisor": {
    "type": "proxy", "bind": "instance",
    "listen": "unix:/run/kanedias/supervisor.sock",
    "connect": "unix:" + request.SocketPath,
    "uid": "1000", "gid": "1000", "mode": "0600",
},
```

Preserve submitted-operation ambiguity handling, `context.WithoutCancel`, 30-second cleanup, not-found tolerance, and `errors.Join`. Return live resources without a function-scoped defer destroying them on success. Tests must prove the root extension can never start without its own socket proxy and that proxy reachability is checked before the volume or instance is created.

- [ ] **Step 2: Add failing root node tests with fake provisioner and `net.Pipe` Pi**

Cover:

- proxy/provisioning failure before RPC;
- TCP connection alone is not readiness;
- successful `get_state` binds identity and reaches `ready`;
- an initial Pi event before `get_state` is buffered;
- forbidden raw RPC remains local and unwritten;
- Pi EOF fails the node and pending HTTP calls;
- stop is idempotent and closes Pi before destroying resources;
- cleanup errors join the primary failure.

- [ ] **Step 3: Implement the root-only `Node`**

Constructor:

```go
type Dependencies struct {
    Provisioner provision.RootProvisioner
    DialRPC     func(context.Context, string) (io.ReadWriteCloser, error)
    Workers     WorkerCatalog
}

func NewRoot(identity Identity, deps Dependencies, broker *EventBroker) (*Node, error)
func (n *Node) Start(context.Context) error
func (n *Node) CallRPC(context.Context, json.RawMessage) (json.RawMessage, error)
func (n *Node) Snapshot() NodeSnapshot
func (n *Node) Stop(context.Context, StopReason) error
func (n *Node) Done() <-chan struct{}
```

`Start` must issue correlated `get_state`, decode `sessionId` and `sessionFile`, bind once, and only then report readiness.

- [ ] **Step 4: Add failing Unix HTTP handler tests**

Test:

```text
GET    /v1/tree
GET    /v1/workers
GET    /v1/events
POST   /v1/sessions/{self}/rpc
POST   /v1/sessions/{self}/questions/{id}/response
DELETE /v1/sessions/{self}
```

Require strict methods, JSON content type for bodies, 1 MiB request-body cap, no HTML routes, and standard SSE framing. Assert socket mode `0600`, unsafe symlink refusal, stale owned socket cleanup, and unlink on shutdown.

- [ ] **Step 5: Implement `internal/supervisorapi` without importing Datastar**

Expose:

```go
type Service interface {
    Snapshot(context.Context) (supervisor.NodeSnapshot, error)
    Workers(context.Context) []contract.WorkerSummary
    CallRPC(context.Context, string, json.RawMessage) (json.RawMessage, error)
    AnswerQuestion(context.Context, string, string, json.RawMessage) error
    Subscribe(context.Context) (supervisor.Subscription, error)
    Stop(context.Context, string) error
}

func NewHandler(service Service) http.Handler
func ServeUnix(ctx context.Context, path string, handler http.Handler) error
```

Write DELETE’s response before asynchronously initiating self-stop so the server does not tear down its own connection mid-response.

- [ ] **Step 6: Run root-slice verification**

```bash
go test -race ./internal/supervisor/... ./internal/supervisorapi/... -count=1
go test ./internal/server ./cmd -count=1
go test ./... -count=1
```

Expected: PASS; the existing browser server remains unchanged.

- [ ] **Step 7: Commit PR 3**

```bash
git add internal/supervisor internal/supervisorapi
git commit -m "feat: add root Pi supervisor API"
```

- [ ] **Step 8: Run combined root review gate 2**

Review PRs 2–3 together for runtime concurrency, Unix-socket safety, question fidelity, root proxy/resource ownership, HTTP bounds, and tests. Apply one consolidated fix pass across the two branches after integration.

---

### Task 4: Implement Fail-Closed Incus COW Child Provisioning

**PR-sized deliverable:** A stopped child-resource clone primitive with tested device replacement, metadata, COW policy, and cleanup. It is not connected to recursive process spawning yet.

**Files:**
- Modify: `internal/incusclient/instance.go`
- Modify: `internal/incusclient/instance_test.go`
- Modify: `internal/incusclient/storage.go`
- Modify: `internal/incusclient/storage_test.go`
- Create: `internal/incusclient/clone_capability.go`
- Create: `internal/incusclient/clone_capability_test.go`
- Create: `internal/supervisor/provision/child.go`
- Create: `internal/supervisor/provision/child_test.go`
- Create: `internal/supervisor/provision/live_incus_test.go`

**Interfaces:**
- Consumes: PR 1 `provision.ChildRequest`, `Resources`, `ChildProvisioner`, and ownership ledger.
- Produces: an Incus-backed `provision.ChildProvisioner`, used by PR 6.
- Parallel rule: branch from PR 1; do not edit PR 2/3 runtime or root-provisioner files.

- [ ] **Step 1: Add failing Incus adapter tests**

Require these methods:

```go
func (c *Client) GetStoragePool(ctx context.Context, name string) (*api.StoragePool, error)
func (c *Client) CopyInstance(ctx context.Context, source, target string) error
func (c *Client) GetStorageVolumeWithETag(ctx context.Context, pool, name string) (*api.StorageVolume, string, error)
func (c *Client) UpdateStorageVolume(ctx context.Context, pool, name string, request api.StorageVolumePut, etag string) error
```

`CopyInstance` must call Incus v7.3.0 with:

```go
&incus.InstanceCopyArgs{
    Name:         target,
    Mode:         "pull",
    InstanceOnly: true,
    Live:         false,
}
```

- [ ] **Step 2: Run adapter tests and confirm compile failure**

```bash
go test ./internal/incusclient -run 'CopyInstance|StoragePool|StorageVolumeWithETag|UpdateStorageVolume' -count=1
```

- [ ] **Step 3: Implement and verify Incus adapters**

Use `server.WithContext(ctx)` and the existing submitted-operation helpers. Preserve ETags on instance and volume updates.

- [ ] **Step 4: Add failing COW capability tests**

V1 policy is exact and conservative:

```go
func ValidateCOWPool(pool *api.StoragePool) error {
    if pool.Status != api.StoragePoolStatusCreated {
        return fmt.Errorf("storage pool %q is not ready: %s", pool.Name, pool.Status)
    }
    if pool.Driver != "btrfs" {
        return fmt.Errorf("storage pool %q uses unsupported non-attested driver %q", pool.Name, pool.Driver)
    }
    return nil
}
```

Test `btrfs/Created` success and rejection of `dir`, `zfs`, `lvm`, unknown, and non-created pools. Resolve the parent instance’s effective root pool from `Instance.ExpandedDevices["root"]["pool"]`; require it to equal the resolved custom-volume pool before either copy. Test a Btrfs workspace pool paired with a different or unsupported root pool. Supporting another driver requires its own later live attestation and policy change.

- [ ] **Step 5: Add failing child-provisioning order and cleanup tests**

Use a recording fake and require this sequence:

```text
check configured proxy listener
resolve workspace pool
resolve parent effective root pool
require the same created Btrfs pool for root and volume
verify parent instance and volume
copy child workspace volume
copy stopped child instance
replace workspace device
replace supervisor proxy device
write child instance metadata
write child volume metadata
verify local devices
start child instance
wait for RPC address
```

Inject failure after every line and assert owned resources are removed in instance-then-volume order. The proxy failure case must prove neither copy was submitted. Submitted-but-unconfirmed copy operations count as possibly owned and trigger name/metadata probes during cleanup.

- [ ] **Step 6: Implement child identity metadata and device replacement**

Use centralized keys:

```go
const (
    metaSessionID = "user.kanedias.session_id"
    metaParentID  = "user.kanedias.parent_session_id"
    metaRootID    = "user.kanedias.root_session_id"
    metaKind      = "user.kanedias.kind"
    metaContext   = "user.kanedias.context"
    metaWorker    = "user.kanedias.worker_type"
    metaVolume    = "user.kanedias.workspace_volume"
)
```

Replace, rather than merge, these inherited local devices:

```go
put.Devices["workspace"] = map[string]string{
    "type": "disk", "pool": pool,
    "source": childVolume, "path": "/workspace",
}
put.Devices["supervisor"] = map[string]string{
    "type": "proxy", "bind": "instance",
    "listen": "unix:/run/kanedias/supervisor.sock",
    "connect": "unix:" + childHostSocket,
    "uid": "1000", "gid": "1000", "mode": "0600",
}
```

Also override child-specific `environment.KANEDIAS_*` launch values before start. Never start the copied instance while it still references the parent socket or volume.

- [ ] **Step 7: Run unit and race tests**

```bash
go test -race ./internal/incusclient ./internal/supervisor/provision -count=1
go test ./... -count=1
```

- [ ] **Step 8: Add opt-in live Btrfs clone validation**

Gate with `//go:build incus` and `KANEDIAS_LIVE_SUPERVISOR=1`. Against the current `btrfs` pool, prove:

- child instance copy is initially stopped;
- child volume and parent volume diverge under writes;
- child cannot reach the parent supervisor socket after replacement;
- deleting child resources leaves parent resources intact;
- cancellation at each remote-operation wait leaks no named resources;
- stopping the proxy before child creation yields `proxy_unavailable` with zero new instance/volume resources.

Run when the prerequisite image and proxy are available:

```bash
KANEDIAS_LIVE_SUPERVISOR=1 go test -tags=incus ./internal/supervisor/provision -run TestLiveBtrfsChildClone -v -count=1
```

- [ ] **Step 9: Commit PR 4**

```bash
git add internal/incusclient internal/supervisor/provision
git commit -m "feat: add Incus COW child provisioning"
```

PR 4 is reviewed concurrently with PR 5 at parallel-lanes gate 3 rather than triggering a separate back-and-forth cycle.

---

### Task 5: Build the Lightweight Pi Extension and Stage It in the Image

**PR-sized deliverable:** A tested TypeScript extension package and skills, copied into the image but not activated by the production launcher until PR 8.

**Files:**
- Create: `internal/image/pi-extension/package.json`
- Create: `internal/image/pi-extension/package-lock.json`
- Create: `internal/image/pi-extension/tsconfig.json`
- Create: `internal/image/pi-extension/src/{index,schemas,supervisor-client,fork,git-handoff,types}.ts`
- Create: `internal/image/pi-extension/test/*.test.ts`
- Create: `internal/image/pi-extension/skills/delegate-session/SKILL.md`
- Create: `internal/image/pi-extension/skills/writer-handoff/SKILL.md`
- Modify: `internal/image/image.go`
- Modify: `internal/image/image_test.go`
- Modify: `internal/image/install.sh`

**Interfaces:**
- Consumes: PR 1 JSON fixtures and API contract; uses a fake Unix-socket supervisor in this PR.
- Produces: `/opt/kanedias/pi-extension` image content for PR 8 activation.
- Parallel rule: do not edit Go contract DTOs, supervisor handlers, CLI files, or child provisioner files.
- Required implementation sub-skill: use `superpowers:writing-skills` for both `SKILL.md` files.

- [ ] **Step 1: Create the pinned extension package and failing schema tests**

Use:

```json
{
  "name": "@kanedias/pi-extension",
  "version": "0.1.0",
  "private": true,
  "type": "module",
  "scripts": {
    "test": "node --import tsx --test test/*.test.ts",
    "typecheck": "tsc --noEmit"
  },
  "dependencies": {
    "@earendil-works/pi-coding-agent": "0.83.0",
    "typebox": "1.3.7"
  },
  "devDependencies": {
    "@types/node": "^24.0.0",
    "tsx": "^4.20.0",
    "typescript": "^5.9.0"
  }
}
```

Tests must validate all required `delegate_session` fields and reject unknown fields, invalid enums, or empty task. `handoff` input additionally accepts an internal checkout path for verification:

```ts
interface RepositoryHandoffInput {
  path: string;
  repository: string;
  baseCommit: string;
  branch: string;
  headCommit: string;
}
```

The extension strips `path` before returning the durable parent result.

- [ ] **Step 2: Run TypeScript tests and confirm failure**

```bash
cd internal/image/pi-extension
npm ci
npm test
npm run typecheck
```

Expected: tests fail because schemas and tools are not implemented.

- [ ] **Step 3: Implement the bounded Unix-socket HTTP client**

Use Node’s native API only:

```ts
http.request({
  socketPath: "/run/kanedias/supervisor.sock",
  method,
  path,
  headers: { "content-type": "application/json", "accept": "application/json" },
  signal,
});
```

Requirements tested with a temporary Unix server:

- no TCP fallback and no redirects;
- 1 MiB maximum response body;
- JSON content-type required;
- bounded discovery/handoff timeout;
- no overall timeout on a healthy blocking delegation;
- tool abort destroys the request and surfaces cancellation.

- [ ] **Step 4: Implement fork preparation and signed-thinking sanitization tests**

Use `SessionManager.open(sessionFile).createBranchedSession(leafID)` before child creation. Fixtures must prove:

- parent session file is byte-for-byte unchanged;
- branch has a new session ID and parent path;
- branch ends at the selected leaf;
- provider-signed thinking blocks are preserved only for a compatible target profile;
- incompatible signed blocks are removed as complete blocks;
- normal text and tool history remain;
- malformed or unpersisted source fails before any HTTP request.

Worker discovery may expose non-secret provider/model/thinking metadata because the tree already exposes resolved model identity. It must never expose credentials.

- [ ] **Step 5: Register only `delegate_session` and `handoff`**

`delegate_session` flow:

```text
validate args
load worker catalog
prepare fork when requested
POST /v1/sessions/{ownSessionID}/children
wait for read/write result
return bounded tool content and full typed details
```

`handoff` flow in this PR uses the fake server contract:

```text
validate write-session environment
validate fields
POST /v1/handoff
on accepted response return terminate:true
call ctx.shutdown only after acceptance
```

Require the skill to tell writers to call `handoff` alone in the final assistant tool batch because `terminate: true` is batch-wide only when all sibling results terminate.

- [ ] **Step 6: Write and test the two skills**

The delegation skill must explain worker type, read/write kind, fresh/fork context, synchronous waiting, and when not to delegate. The writer skill must explain commit/push discipline, no automatic cleanliness enforcement, exact refs, verification evidence, and terminal standalone `handoff`.

Run the writing-skills verification workflow and extension unit tests after each skill is complete.

- [ ] **Step 7: Stage extension files in the image without activation**

Embed/upload the extension tree and install it root-owned under:

```text
/opt/kanedias/pi-extension
```

Create `/usr/lib/tmpfiles.d/kanedias.conf` so `/run/kanedias` is owned by the managed Pi user, but do not add `-e` to `kanedias-pi-rpc` yet. This keeps PR 5 independently mergeable before the supervisor socket exists.

After copying the package and lockfile, run a locked production install inside `/opt/kanedias/pi-extension`:

```bash
npm ci --omit=dev --ignore-scripts
```

Update `image_test.go` to assert exact upload/install order, production `node_modules/typebox` presence, file modes, and absence of credentials inside the extension tree. PR 8’s real-Pi activation test must import the staged TypeScript entry point through Pi’s loader.

- [ ] **Step 8: Run extension and image verification**

```bash
cd internal/image/pi-extension
npm ci
npm test
npm run typecheck
cd ../../..
go test ./internal/image -count=1
go test ./... -count=1
```

- [ ] **Step 9: Commit PR 5**

```bash
git add internal/image
git commit -m "feat: add Kanedias Pi delegation extension"
```

- [ ] **Step 10: Run parallel-lanes review gate 3**

Review complete PRs 4 and 5 concurrently: one reviewer pair focuses on Incus/COW ownership and one on extension/session/image behavior. Consolidate both reports once; do not run separate review cycles for the two parallel lanes.

---

### Task 6: Add Child Process Bootstrap and Parent-Liveness

**PR-sized deliverable:** The same executable can start one child supervisor from inherited descriptors, report readiness/terminal state, and shut down on parent-liveness EOF. Recursive registry and HTTP routing remain PR 7.

**Files:**
- Create: `internal/supervisor/process/protocol.go`
- Create: `internal/supervisor/process/spawn.go`
- Create: `internal/supervisor/process/liveness.go`
- Create: `internal/supervisor/process/process_test.go`
- Create: `cmd/session_child.go`
- Modify: `cmd/root.go`
- Modify: `cmd/root_test.go`

**Interfaces:**
- Consumes: PR 1 contracts/provision types and PRs 3–4 root/child resource implementations.
- Produces: strict `process.Bootstrap`, `process.ChildMessage`, `process.Spawner`, liveness monitor, and hidden child command for PR 7.
- Dependency rule: `process` imports `contract`, `config`, and `provision`, never the parent `supervisor` package.

- [ ] **Step 1: Add failing inherited-FD process protocol tests**

Use these fixed descriptors in the child command:

```text
fd 3: bootstrap JSON, parent writes then closes
fd 4: parent-liveness read end, parent retains only write end
fd 5: child readiness/result JSONL, child retains only write end
```

Launch the same executable with a hidden command:

```text
kanedias session-child --bootstrap-fd 3 --liveness-fd 4 --report-fd 5
```

No task text, model, credentials, or repository data may appear in argv or process listings.

- [ ] **Step 2: Implement bootstrap, readiness, terminal reports, and liveness EOF**

Freeze the complete bootstrap before implementation:

```go
type Bootstrap struct {
    SessionID      string                      `json:"sessionId"`
    ParentID       string                      `json:"parentId"`
    RootID         string                      `json:"rootId"`
    SocketPath     string                      `json:"socketPath"`
    SourceInstance string                      `json:"sourceInstance"`
    SourceVolume   string                      `json:"sourceVolume"`
    Worker         config.WorkerProfile        `json:"worker"`
    Request        contract.CreateChildRequest `json:"request"`
}

type ChildMessage struct {
    Type      string                     `json:"type"`
    SessionID string                     `json:"sessionId"`
    Ready     *ReadyMessage              `json:"ready,omitempty"`
    Read      *contract.ReadChildResult  `json:"read,omitempty"`
    Write     *contract.WriteChildResult `json:"write,omitempty"`
    Error     *WireError                 `json:"error,omitempty"`
}
```

Decode at most 1 MiB with unknown fields rejected. Validate identity, source resources, socket path, worker profile, fork combination, and task before provisioning. The parent accepts `ready` only when the session ID matches and the child socket answers `GET /v1/tree`. Parent death closes fd 4 and triggers the same idempotent stop path as an explicit parent shutdown.

- [ ] **Step 3: Test real helper-process parent death without Incus**

Start a helper child and grandchild, close the parent write end without sending a stop request, and assert both descendants observe EOF and exit. Ensure no descendant inherited an ancestor liveness write end.

- [ ] **Step 4: Verify and commit PR 6**

```bash
go test -race ./internal/supervisor/process ./cmd -count=1
go test ./... -count=1
git add cmd internal/supervisor/process
git commit -m "feat: add child supervisor process bootstrap"
```

Review PR 6 with PR 7 at recursion gate 4 rather than reviewing this protocol half alone.

---

### Task 7: Add Recursive Registry, Routing, and the Child HTTP API

**PR-sized deliverable:** Parent nodes own direct-child processes, aggregate descendant trees/events, route controls without import cycles, and synchronously serve `POST /children`.

**Files:**
- Create: `internal/supervisor/children.go`
- Create: `internal/supervisor/router.go`
- Modify: `internal/supervisor/node.go`
- Modify: `internal/supervisor/stop.go`
- Create: `internal/supervisorapi/client.go`
- Modify: `internal/supervisorapi/handler.go`
- Modify: `internal/supervisorapi/handler_test.go`

**Interfaces:**
- Consumes: PR 6 process bootstrap and PRs 3–4 node/provision behavior.
- Produces: recursive `/tree`, `/events`, `/rpc`, questions, delete, and blocking `/children` for PR 8.
- Import-cycle rule: `supervisor` declares a `DescendantClient` interface and injected factory; `supervisorapi` implements it. `supervisor` never imports `supervisorapi`.

- [ ] **Step 1: Add failing direct-child registry and three-level routing tests**

Create fake root → child → grandchild clients and assert:

- deterministic recursive tree output;
- sibling startup can run concurrently;
- local, child, and grandchild RPC reach the correct target;
- question answer and delete follow the same target path;
- locks are not held across child HTTP calls;
- child socket failure returns typed child/gateway error;
- terminal children disappear after their blocked caller receives the result.

- [ ] **Step 2: Implement the acyclic descendant-client seam and router**

Declare in `supervisor`:

```go
type DescendantClient interface {
    Snapshot(context.Context) (NodeSnapshot, error)
    Subscribe(context.Context) (Subscription, error)
    CallRPC(context.Context, string, json.RawMessage) (json.RawMessage, error)
    AnswerQuestion(context.Context, string, string, json.RawMessage) error
    Stop(context.Context, string) error
}

type DescendantClientFactory func(socketPath string) (DescendantClient, error)
```

Implement the interface in `supervisorapi/client.go` and inject the factory when constructing a `Node`. The parent-to-child client supports tree snapshot, SSE subscription, routed RPC, answer, stop, and readiness probe. Each direct child gets one event-forwarding goroutine. The parent preserves descendant `SessionID`/`SourceSeq` and assigns its own subtree `Seq`.

- [ ] **Step 3: Add `POST /v1/sessions/{id}/children` with synchronous waiting**

Handler behavior:

```text
strictly decode request
resolve worker before side effects
start direct child
wait for terminal report
wait for child process exit and resource cleanup
on request cancellation stop child subtree and await bounded cleanup
return result or typed failure only after cleanup
```

At this stage tests may drive child completion through the report pipe. A failure report is provisional until `ChildProcess.Wait` confirms cleanup/exit; join cleanup errors before responding. Write handoff verification remains PR 9.

- [ ] **Step 4: Implement idempotent concurrent cascade**

Stop order:

1. transition once to `stopping` and reject new children;
2. stop direct children concurrently;
3. wait through graceful deadline, then signal/kill child process groups;
4. close local Pi RPC;
5. destroy local resources;
6. close broker and listener;
7. unlink own socket;
8. close report and liveness descriptors;
9. finish `stopped`.

- [ ] **Step 5: Run recursive-core verification**

```bash
go test -race ./internal/supervisor/... ./internal/supervisorapi/... ./cmd -count=1
go test ./... -count=1
```

- [ ] **Step 6: Commit PR 7**

```bash
git add internal/supervisor internal/supervisorapi
git commit -m "feat: add recursive session routing"
```

- [ ] **Step 7: Run combined recursion review gate 4**

Review PRs 6–7 together for descriptor inheritance, parent death, import direction, routed locking, event aggregation, failure-after-cleanup ordering, synchronous cancellation, and cascade escalation.

---

### Task 8: Activate the Extension and Deliver Fresh/Fork Read Delegation

**PR-sized deliverable:** `kanedias session --socket PATH` runs the first complete vertical slice: root prompt → lightweight extension → fresh or forked read child → answer → cleanup.

**Files:**
- Modify: `internal/image/kanedias-pi-rpc`
- Modify: `internal/image/install.sh`
- Modify: `internal/image/image_test.go`
- Modify: `assets/pi-settings.json`
- Modify: `cmd/session.go`
- Modify: `cmd/root.go`
- Modify: `cmd/root_test.go`
- Create: `cmd/session_test.go`
- Modify: `internal/supervisor/node.go`
- Modify: `internal/supervisor/result.go`
- Create: `internal/supervisor/read_result_test.go`
- Modify: `internal/supervisor/provision/root.go`
- Modify: `internal/supervisor/provision/child.go`

**Interfaces:**
- Consumes: PRs 1–7 and staged PR 5 extension.
- Produces: public foreground root command, persisted Pi sessions, model-profile launch, initial child-task prompting, fresh/fork child startup, and read result classification.

- [ ] **Step 1: Add failing launcher tests for fresh and forked sessions**

The launcher must build an argument array without `eval`:

```text
fresh root/child: pi --mode rpc -e /opt/kanedias/pi-extension/src/index.ts
fork child:       pi --mode rpc --session <prepared-file> -e /opt/kanedias/pi-extension/src/index.ts
```

For child workers append validated:

```text
--provider <profile provider> --model <profile model> --thinking <profile level>
```

Fresh sessions intentionally omit both `--no-session` and `--session`, causing Pi 0.83.0 to create a persisted session. `get_state` binds the generated Pi identity.

- [ ] **Step 2: Activate the extension and remove `pi-subagents`**

Remove `npm:pi-subagents` from `assets/pi-settings.json` and its installer invocation. Install the two Kanedias skills with the staged extension. Keep unrelated packages unless they conflict with the two Kanedias tool names.

- [ ] **Step 3: Add failing CLI tests and replace the one-shot command**

New contract:

```text
kanedias session --socket ./session.sock
```

`--socket` is required, stdin is not required, positional args remain forbidden, and the command runs until context cancellation or self-delete. Change the service seam to:

```go
type SessionOptions struct {
    SocketPath string
}

runSupervisor func(context.Context, config.Config, SessionOptions, io.Writer) error
```

Do not add daemonization, attach, adoption, or global listing.

- [ ] **Step 4: Implement root and child launch environment**

Set instance-specific configuration before start:

```text
environment.KANEDIAS_SESSION_ID
environment.KANEDIAS_SESSION_KIND
environment.KANEDIAS_WORKER_TYPE
environment.KANEDIAS_PI_PROVIDER
environment.KANEDIAS_PI_MODEL
environment.KANEDIAS_PI_THINKING
environment.KANEDIAS_PI_SESSION_FILE
environment.KANEDIAS_SUPERVISOR_SOCKET=/run/kanedias/supervisor.sock
```

Root worker/model fields may be empty and use image defaults. Fork children require the cloned branch file path.

- [ ] **Step 5: Submit the delegated task after child readiness**

After provisioning, socket serving, Pi launch, and successful `get_state` binding:

1. write the child `ready` report;
2. send one correlated Pi `prompt` whose `message` is exactly `Bootstrap.Request.Task`;
3. require a successful prompt-acceptance response;
4. transition `ready -> running`;
5. then wait for settlement/handoff.

Add fresh and fork tests that decode the prompt text exactly. Prompt rejection must report typed child failure, close Pi, clean resources, exit the child process, and only then unblock the parent HTTP request.

- [ ] **Step 6: Add failing read-result classification tests**

Success requires all of:

```text
agent_settled observed
no explicit abort
no terminal extension error
healthy RPC stream
final assistant stopReason == stop
successful get_last_assistant_text with non-null text
```

Treat `error`, `aborted`, `length`, Pi EOF, and null final text as typed child failure rather than normal output.

- [ ] **Step 7: Implement read completion and terminal report ordering**

When a read child succeeds:

1. send `ReadChildResult` through fd 5;
2. wait for the report write to complete;
3. gracefully close Pi RPC;
4. delete child resources and socket;
5. exit child supervisor;
6. parent returns the answer to `delegate_session`.

- [ ] **Step 8: Add fake-supervisor extension integration tests**

Run Pi 0.83.0 RPC with the real extension and a temporary Unix server. Assert:

- exactly `delegate_session` and `handoff` are registered by Kanedias;
- worker catalog populates the delegation description;
- fresh request has no fork block;
- fork request references a new persisted branch and leaves parent unchanged;
- cancellation closes the HTTP request;
- read result becomes tool content/details.

- [ ] **Step 9: Run vertical-slice unit verification**

```bash
cd internal/image/pi-extension
npm ci
npm test
npm run typecheck
cd ../../..
go test -race ./internal/supervisor/... ./internal/supervisorapi/... ./cmd -count=1
go test ./... -count=1
```

- [ ] **Step 10: Run a live fresh-read smoke test with an owned proxy subprocess**

The test helper builds the current checkout’s `kanedias` binary, starts `proxy run` as a subprocess, polls gateway port `3128`, runs the smoke test, and terminates the proxy in cleanup. Invoke:

```bash
KANEDIAS_LIVE_SUPERVISOR=1 go test -tags=incus ./internal/supervisor -run TestLiveFreshReadDelegation -v -count=1
```

Assert selected reviewer model, distinct root/child IDs and resources, visible child events, normal answer, and child cleanup.

- [ ] **Step 11: Commit PR 8**

```bash
git add assets cmd internal/image internal/supervisor internal/supervisorapi
git commit -m "feat: deliver supervised read delegation"
```

- [ ] **Step 12: Record PR 8 evidence for the combined delegation gate**

Save Pi launch/session, extension activation, fresh/fork, read classification, CLI, model-dispatch, and live-smoke evidence. Do not start a separate review cycle; PR 9 adds the writer half before gate 5.

---

### Task 9: Add Write Completion and Remotely Verified Git Handoff

**PR-sized deliverable:** Write children remain live after settlement, accept only exact pushed refs, return durable handoff results, and then disappear.

**Files:**
- Modify: `internal/image/pi-extension/src/git-handoff.ts`
- Modify: `internal/image/pi-extension/src/index.ts`
- Modify: `internal/image/pi-extension/src/schemas.ts`
- Modify: `internal/image/pi-extension/test/git-handoff.test.ts`
- Modify: `internal/image/pi-extension/test/tools.test.ts`
- Modify: `internal/image/pi-extension/skills/writer-handoff/SKILL.md`
- Modify: `internal/supervisor/node.go`
- Modify: `internal/supervisor/result.go`
- Modify: `internal/supervisorapi/handler.go`
- Create: `internal/supervisor/handoff_test.go`
- Create: `internal/supervisorapi/handoff_test.go`

**Interfaces:**
- Consumes: `WriteChildResult`, synchronous parent waiter, extension `handoff` tool, and host API.
- Produces: `POST /v1/handoff`, `awaiting_handoff`, exact remote-ref verification, and terminal write result.

- [ ] **Step 1: Add failing writer-state tests**

Assert:

- writer `agent_settled` transitions `running -> awaiting_handoff`;
- parent `POST /children` remains blocked;
- transcript, `follow_up`, prompt, abort, and stop remain routable;
- a second prompt can return to running and settle again;
- handoff is rejected for root/read sessions;
- duplicate handoff returns conflict.

- [ ] **Step 2: Implement `awaiting_handoff` without automatic prompting**

Do not manufacture commits, enforce a clean tree, or auto-send instructions. External clients may inspect and send a prompt/follow-up asking the writer to finish.

- [ ] **Step 3: Add failing local Git verification tests in TypeScript**

Create temporary repositories and bare remotes. Verify this exact sequence for each input checkout:

```bash
git -C <path> rev-parse --show-toplevel
git -C <path> rev-parse HEAD
git -C <path> check-ref-format --branch <branch>
git -C <path> remote get-url origin
git -C <path> ls-remote --exit-code origin refs/heads/<branch>
```

Reject symlinked/escaping paths outside `/workspace/repos`, repository slug mismatch, local `HEAD != headCommit`, absent branch, or remote branch tip mismatch. Do not run `git status` or reject dirtiness.

- [ ] **Step 4: Implement extension-side ref verification before POST**

Use `pi.exec` with argument arrays, never shell interpolation. Strip local `path` from the durable request sent to `/v1/handoff`.

- [ ] **Step 5: Add failing supervisor handoff-order tests**

Require:

```text
validate write kind
validate non-empty unique repositories
receive extension-verified refs
record terminal result
write terminal result to parent report pipe
acknowledge /v1/handoff
extension requests graceful shutdown
stop descendants
close Pi
remove instance and volume
```

A rejected request leaves the writer live. Successful handoff cancels unfinished descendants.

- [ ] **Step 6: Implement `POST /v1/handoff` and exactly-once completion**

The socket identifies the calling session; reject caller-supplied session IDs. Return:

```json
{"accepted":true,"sessionId":"<own-session-id>"}
```

Only acknowledge after the terminal result has been recorded and forwarded upward.

- [ ] **Step 7: Test multi-repository handoff**

Use two temporary repositories/remotes and assert the result contains one branch and exact head per modified repository, with no local paths.

- [ ] **Step 8: Run full write-path verification**

```bash
cd internal/image/pi-extension
npm test
npm run typecheck
cd ../../..
go test -race ./internal/supervisor/... ./internal/supervisorapi/... -count=1
go test ./... -count=1
```

- [ ] **Step 9: Run a disposable live GitHub handoff smoke test**

Use a configured disposable repository and unique branch namespace. Confirm `git ls-remote` independently after the child disappears.

- [ ] **Step 10: Commit PR 9**

```bash
git add internal/image/pi-extension internal/supervisor internal/supervisorapi
git commit -m "feat: add terminal Git handoff for writer sessions"
```

- [ ] **Step 11: Run combined delegation review gate 5**

Review PRs 8–9 together. Parallel reviewers cover read/session behavior and writer path containment, command injection resistance, remote durability, exactly-once ordering, awaiting-handoff control, and cleanup. Consolidate both into one fix pass.

---

### Task 10: Build the End-to-End Acceptance Harness and Remove the Spike

**PR-sized deliverable:** Repeatable live evidence for the approved 12-step acceptance scenario, leak diagnostics, operational documentation, and removal of the superseded one-shot session implementation.

**Files:**
- Create: `internal/supervisor/live_incus_test.go`
- Create: `internal/supervisor/testdata/read-task.md`
- Create: `internal/supervisor/testdata/write-task.md`
- Modify: `docs/architecture/session-supervisor.md`
- Modify: `docs/superpowers/plans/2026-08-07-recursive-pi-session-supervisor.md`
- Delete: `internal/session/session.go`
- Delete: `internal/session/session_test.go`
- Delete: `internal/session/rpc.go`
- Delete: `internal/session/rpc_test.go`
- Modify: `cmd/root.go`

**Interfaces:**
- Consumes: complete root/child/extension/handoff runtime.
- Produces: gated live acceptance command, failure artifacts, leak assertions, and final public session path.

- [ ] **Step 1: Build the reviewed checkout and create a persistent artifact directory**

The harness runs `go build -o <artifact-dir>/kanedias-under-test .` and uses that absolute binary for proxy, root, and child entrypoints. Create artifacts beneath `KANEDIAS_E2E_ARTIFACT_DIR` when set, otherwise beneath a user-cache `kanedias/e2e` directory. Remove the run directory only on success; retain and print it on failure.

Before starting, record all project instances and custom volumes. Every test-created resource must carry `user.kanedias.session_id` metadata and a unique test prefix.

- [ ] **Step 2: Start and poll the prerequisite proxy**

By default the harness starts `<artifact-dir>/kanedias-under-test proxy run` as an owned child process, polls the configured gateway port `3128`, and terminates/waits for it during cleanup. An explicit `KANEDIAS_E2E_EXTERNAL_PROXY=1` mode may reuse an operator-owned listener but must still poll it. Never use a fixed sleep.

- [ ] **Step 3: Exercise root RPC and stalled-client backpressure**

Start:

```bash
"$KANEDIAS_BINARY_UNDER_TEST" session --socket "$KANEDIAS_E2E_RUN_DIR/root.sock"
```

Assert mode `0600`, connect one consuming SSE client and one stalled SSE client, run a root prompt, and prove the stalled client cannot prevent `agent_settled` or later RPC.

- [ ] **Step 4: Exercise a fresh read child and routed control**

Assert:

- configured reviewer model/thinking;
- distinct instance, volume, Kanedias session, Pi session, and socket;
- child visible through root `/tree` and `/events`;
- `get_messages`, `get_entries`, and `steer` route from root;
- normal answer returns to `delegate_session`;
- child process/socket/instance/volume disappear.

- [ ] **Step 5: Exercise and answer a deterministic pending question**

Use a test extension command or controlled fixture that emits one blocking input request. Assert pending retention, correct response, duplicate rejection, and disappearance after answer.

- [ ] **Step 6: Exercise a forked write child and Git handoff**

Assert parent session file is unchanged, child has a new Pi session with parent metadata, configured worker model is selected, the pushed branch resolves to returned head, result arrives before deletion, and child resources disappear.

- [ ] **Step 7: Exercise recursive cancellation and parent death**

Create child and grandchild sessions, then test both graceful root DELETE and `SIGKILL` of the root process. Assert liveness EOF removes descendant processes, sockets, instances, and volumes. The killed root cannot clean its own resources under the approved v1 model; record that expected limitation, then have test teardown remove only the root resources identified by its exact metadata so the harness itself leaves the Incus baseline clean. Host-wide crash reconciliation remains out of scope.

- [ ] **Step 8: Exercise missing proxy fail-fast behavior**

Stop the proxy, request a new root, assert a clear `proxy_unavailable` error, and compare Incus resource sets to prove no new session-owned resource was created.

- [ ] **Step 9: Run the complete harness twice**

```bash
KANEDIAS_LIVE_SUPERVISOR=1 \
KANEDIAS_CONFIG=./config.toml \
go test -tags=incus ./internal/supervisor -run TestLiveRecursiveSupervisorAcceptance -v -count=2
```

On failure, save tree snapshots, SSE envelopes, process stderr, Incus metadata, resource lists, and reported Git refs under the persistent run directory from Step 1 and print that path. Do not use `t.TempDir()` for evidence that must survive the test process.

- [ ] **Step 10: Remove the old one-shot session package**

Delete `internal/session/**`, remove its import from `cmd/root.go`, and ensure no tests or docs refer to one-prompt stdout forwarding.

- [ ] **Step 11: Run final verification**

```bash
gofmt -w $(find cmd internal -type f -name '*.go')

go test -race ./internal/supervisor/... ./internal/supervisorapi/... ./cmd -count=1
go test ./... -count=1

cd internal/image/pi-extension
npm ci
npm test
npm run typecheck
cd ../../..

git diff --check
```

Then run the live acceptance command once more with the proxy prerequisite satisfied.

- [ ] **Step 12: Commit PR 10**

```bash
git add cmd docs internal
git commit -m "test: verify recursive Pi session supervision"
```

- [ ] **Step 13: Run release review gate 6**

Use parallel fresh reviewers for:

- recursive correctness/resource leaks;
- Pi protocol/session/fork behavior;
- Unix API/security/backpressure;
- TypeScript extension/Git handoff;
- acceptance evidence and deferred-scope compliance.

Consolidate blockers into one fix pass, rerun affected unit/race/TypeScript/live checks, and perform a second focused review only if that pass materially changes behavior.

---

## Final Acceptance Checklist

- [ ] One foreground root supervisor owns exactly one persisted Pi session.
- [ ] Root control socket is mode `0600` and removed on shutdown.
- [ ] Pi stdout drains continuously during concurrent RPC and stalled SSE clients.
- [ ] `get_state` binds immutable Pi session ID/file before readiness.
- [ ] Session-replacement RPC commands are rejected without reaching Pi.
- [ ] Blocking UI questions survive event-ring eviction and answer exactly once.
- [ ] Worker type selects configured provider/model/thinking and cannot be overridden by child input.
- [ ] Every child has independent Btrfs COW root and workspace resources.
- [ ] Every child sees only its own proxied supervisor socket.
- [ ] Fresh and forked contexts both create distinct Pi sessions.
- [ ] Root socket shows and controls all live descendants.
- [ ] Read completion returns only classified successful final output.
- [ ] Writer settlement waits for handoff and remains controllable.
- [ ] Writer handoff verifies exact remote branch heads without enforcing cleanliness.
- [ ] Successful handoff is delivered before child teardown.
- [ ] Request cancellation, graceful stop, and parent-liveness EOF cascade through descendants.
- [ ] Missing proxy fails before session-owned resources are created.
- [ ] No daemon, registry, adoption, detached delegation, worktree runtime, or browser UI was added.
- [ ] Unit, race, TypeScript, image, and live acceptance checks pass with recorded evidence.

## Explicitly Deferred

- Global daemon/registry, built-in daemonization, attach, or machine-wide list.
- Session/sandbox adoption and automatic host-crash reconciliation.
- Detached/background delegation handles.
- Model-facing transcript, steering, question, or fleet tools beyond the two extension tools.
- Durable event journals, transcript archives, and reconnect cursors.
- Sophisticated replay-gap and slow-consumer policies beyond bounded nonblocking v1 behavior.
- Scheduling, quotas, depth/concurrency limits, budgets, retries, and retention policy.
- Storage drivers other than the live-attested Btrfs v1 path.
- Stronger atomic multi-volume quiescing.
- Automatic branch integration, cherry-picking, merge conflict resolution, or PR creation.
- Guest-specific reduced-capability API split.
- Custom browser UI and observation across unrelated root supervisors.
