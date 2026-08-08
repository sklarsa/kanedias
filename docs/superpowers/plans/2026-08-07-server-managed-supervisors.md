# Server-Managed Supervisors Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `kanedias server` securely discover, launch, observe, and control multiple independent root supervisors through the live Astrolabe UI without making supervisor lifetime depend on the server.

**Architecture:** Add a root-only `internal/manager` control plane between the existing recursive supervisor API and `internal/server`. Each supervisor remains the execution and replay authority; the manager keeps one root subscription, a bounded mirror, an independently polled tree, and a descendant-to-root routing index. The loopback web server renders manager projections with Datastar and protects every fleet/control route with an in-memory browser capability.

**Tech Stack:** Go 1.26.5, Pi RPC 0.83.0, HTTP/JSON over mode-`0600` Unix sockets, `chi` v5.3.1, `datastar-go` v1.2.2, vendored Datastar browser v1.0.2, `html/template`, Linux process/session APIs, Incus v7.

## Global Constraints

- The canonical design is `docs/superpowers/specs/2026-08-07-server-managed-supervisors-design.md`.
- Manage only sockets ending in `.root.sock`; descendants always route through their owning root.
- A manager scan or failed probe must never unlink an external candidate.
- The root socket filename is an opaque launch token, never a session identity.
- Supervisor replay is bounded and configurable by independent count and byte limits; pending questions remain separate.
- A missing aggregate root sequence marks activity for the complete root tree as incomplete.
- Server shutdown rejects new writes, cancels browser streams, closes manager connections, and never stops an admitted root.
- Use plain `exec.Command` plus `Setsid` for roots; do not reuse the descendant `process.Spawner` or a server-lifetime `exec.CommandContext`. Claim survival only for ordinary parent exit, not reboot, host shutdown, OOM, or service-manager cgroup termination.
- Pi Steer sends `steer` while streaming and `prompt` while idle; `follow_up` is not exposed.
- Pi `abort` interrupts a turn; supervisor `DELETE` stops a session/subtree.
- Treat HTTP 200 with a Pi `success:false` envelope as an operator-visible failure.
- The web listener remains loopback-only. Fleet and control routes require a valid capability cookie; writes also require exact Host/Origin, JSON, and same-origin fetch metadata.
- Keep browser Datastar v1.0.2 and Go SDK v1.2.2 wire behavior covered by tests; do not upgrade either dependency in this feature.
- Full transcript retrieval for arbitrarily large sessions and durable run archives remain out of scope. V1 renders retained recent activity and does not add a `get_entries` hydration path.
- Build every behavior test-first, run focused tests before broad tests, and commit after every task.

---

## File Structure and Locked Interfaces

### Configuration and supervisor replay

- `internal/config/server.go` — raw TOML server/supervisor-event settings, quoted-duration parsing, defaults, and validation.
- `internal/config/server_test.go` — omission versus explicit-zero tests and server duration/path validation.
- `internal/supervisor/events.go` — validated configurable broker construction and independent eviction limits.
- `internal/supervisor/events_test.go` — count-only, byte-only, oversized-event, and replay/live tests.
- `cmd/session_runtime.go` — inject configured brokers into both root and child runtimes before provisioning.
- `cmd/session_runtime_test.go` — prove both runtime paths choose the configured limits before side effects.

Use these exact types:

```go
// internal/config/server.go
const (
    DefaultServerDiscoveryInterval = 5 * time.Second
    DefaultServerSnapshotInterval  = time.Second
    DefaultServerSpawnTimeout      = 2 * time.Minute
    DefaultSupervisorEventMaxEvents = 4_096
    DefaultSupervisorEventMaxBytes  = 16 << 20
)

type Duration struct{ time.Duration }
func (d *Duration) UnmarshalText(text []byte) error

type ServerConfig struct {
    RootSocketDir     string    `toml:"root_socket_dir"`
    SessionLogDir     string    `toml:"session_log_dir"`
    DiscoveryInterval *Duration `toml:"discovery_interval"`
    SnapshotInterval  *Duration `toml:"snapshot_interval"`
    SpawnTimeout      *Duration `toml:"spawn_timeout"`
    SessionBinary     string    `toml:"session_binary"`
}

type SupervisorConfig struct {
    Events SupervisorEventsConfig `toml:"events"`
}

type SupervisorEventsConfig struct {
    MaxEvents *int `toml:"max_events"`
    MaxBytes  *int `toml:"max_bytes"`
}

type EventLimits struct {
    MaxEvents int
    MaxBytes  int
}

type ResolvedServerConfig struct {
    RootSocketDir     string
    SessionLogDir     string
    DiscoveryInterval time.Duration
    SnapshotInterval  time.Duration
    SpawnTimeout      time.Duration
    SessionBinary     string
}

func (c ServerConfig) Resolve() (ResolvedServerConfig, error)
func (c SupervisorEventsConfig) Limits() (EventLimits, error)
```

```go
// internal/supervisor/events.go
type EventBrokerOptions struct {
    MaxEvents int
    MaxBytes  int
}

func NewEventBroker() *EventBroker
func NewEventBrokerWithOptions(EventBrokerOptions) (*EventBroker, error)
```

### Root manager

Create `internal/manager` with focused files:

- `manager.go` — lifecycle, public API, root and route indexes, lock discipline.
- `types.go` — public projections, subscriptions, stats, and options.
- `unix.go` — private-directory and read-only socket identity validation.
- `tree.go` — recursive root-tree validation, sorting, lookup, and route extraction.
- `discovery.go` — deterministic `*.root.sock` scan and reconciliation.
- `mirror.go` — root-sequence-preserving bounded mirror, deduplication, and gap state.
- `projection.go` — allowlisted Pi event projection into recent activity.
- `notify.go` — bounded nonblocking fleet/detail subscribers.
- `monitor.go` — independent snapshot and event/reconnect loops.
- `spawn.go` — detached root launch, admission transaction, cleanup, and cached waiter.
- `pi.go` — typed Pi commands/responses, stats, and high-level controls.
- matching `*_test.go` files plus `testutil_test.go`.

Lock these public interfaces:

```go
package manager

type Options struct {
    ConfigPath        string
    RootSocketDir     string
    SessionLogDir     string
    SessionBinary     string
    DiscoveryInterval time.Duration
    SnapshotInterval  time.Duration
    SpawnTimeout      time.Duration
    EventLimits       supervisor.EventBrokerOptions
    Logger            *slog.Logger
}

type ReplayGap struct {
    ExpectedSeq       uint64
    FirstAvailableSeq uint64
}

type DiscoveryIssue struct {
    SocketName string
    Code       string
    Message    string
}

type RootState struct {
    RootSessionID   string
    Tree            supervisor.NodeSnapshot
    Stale           bool
    StreamConnected bool
    Incomplete      bool
    Gap             *ReplayGap
    Revision        uint64
}

type ActivityItem struct {
    Seq        uint64
    Kind       string
    Label      string
    Text       string
    ToolCallID string
    ToolName   string
    Status     string
    IsError    bool
}

type SessionState struct {
    RootSessionID   string
    Node            supervisor.NodeSnapshot
    RootStale       bool
    StreamConnected bool
    Incomplete      bool
    Gap             *ReplayGap
    RecentActivity  []ActivityItem
    Revision        uint64
}

type FleetSnapshot struct {
    Roots    []RootState
    Issues   []DiscoveryIssue
    Revision uint64
}

type ChangeSubscription struct {
    Updates <-chan uint64
    Close   func()
}

type SessionStats struct {
    UserMessages      int
    AssistantMessages int
    ToolCalls         int
    ToolResults       int
    TotalMessages     int
    Tokens            TokenStats
    Cost              float64
    ContextUsage      *ContextUsage
}

type TokenStats struct {
    Input, Output, CacheRead, CacheWrite, Total int64
}

type ContextUsage struct {
    Tokens        *float64
    ContextWindow float64
    Percent       *float64
}

func New(Options) (*Manager, error)
func (m *Manager) Start(context.Context) error
func (m *Manager) Fleet() FleetSnapshot
func (m *Manager) Session(string) (SessionState, error)
func (m *Manager) SubscribeFleet() ChangeSubscription
func (m *Manager) SubscribeSession(string) (ChangeSubscription, error)
func (m *Manager) SpawnRoot(context.Context) (string, error)
func (m *Manager) Steer(context.Context, string, string) error
func (m *Manager) Interrupt(context.Context, string) error
func (m *Manager) StopSession(context.Context, string) error
func (m *Manager) AnswerQuestion(context.Context, string, string, json.RawMessage) error
func (m *Manager) SessionStats(context.Context, string) (SessionStats, error)
func (m *Manager) Quiesce(context.Context) error
func (m *Manager) Close(context.Context) error
```

Use this manager-private client seam so tests do not widen the supervisor's parent/child interface:

```go
type rootClient interface {
    Snapshot(context.Context) (supervisor.NodeSnapshot, error)
    Subscribe(context.Context) (supervisor.Subscription, error)
    CallRPC(context.Context, string, json.RawMessage) (json.RawMessage, error)
    AnswerQuestion(context.Context, string, string, json.RawMessage) error
    Stop(context.Context, string) error
    Close() error
}

type clientFactory func(string) (rootClient, error)
```

Routine implementation decisions are fixed as follows:

- Reject an incoming tree if any session ID collides with another root; preserve the previous route map and expose a sanitized `route_conflict` issue.
- Retain successful `stopping`, `stopped`, or `failed` root snapshots as current but non-actionable until the exact socket disappears.
- Retain the first observed replay gap for the handle and set `Incomplete=true`; do not keep an unbounded gap list.
- A root gap marks every session under that root incomplete because the missing aggregate envelope's source is unknowable.
- Require the final socket/log directories themselves to be non-symlinks, EUID-owned, and mode `0700`; do not reject a trusted ancestor such as `/var/run -> /run`.
- Require the configured binary to resolve to a clean absolute regular executable; allow a symlink whose target satisfies that check.
- Use one 30-second failed-admission cleanup deadline: 10 seconds total for graceful `Stop` plus waiting, 10 seconds after `SIGTERM`, then `SIGKILL` and reap within the remaining time.
- Show malformed/conflicting root candidates as sanitized fleet issues and structured logs.
- Project only `message_update`, `message_end`, `tool_execution_start`, `tool_execution_update`, `tool_execution_end`, `queue_update`, `agent_start`, `agent_settled`, and `extension_error`; unknown events become a generic event label without raw payload text.

### Server and browser

- `internal/server/security.go` — bootstrap/session capabilities and request boundary.
- `internal/server/signals.go` — bounded strict Datastar signal decoding.
- `internal/server/render.go` — template parse/render/patch helpers.
- `internal/server/view.go` — safe template view models.
- `internal/server/handler.go` — authenticated router, read streams, and actions.
- `internal/server/server.go` — manager construction and shutdown order.
- `internal/server/web/index.html` — authenticated shell and stable patch roots.
- `internal/server/web/{fleet,detail,questions,activity,deck-status}.html` — fragments.
- `internal/server/web/app.js` — delegated local behavior; no one-time node arrays.
- existing CSS/assets remain embedded.

The server consumes the manager through:

```go
type fleetManager interface {
    Start(context.Context) error
    Fleet() manager.FleetSnapshot
    Session(string) (manager.SessionState, error)
    SubscribeFleet() manager.ChangeSubscription
    SubscribeSession(string) (manager.ChangeSubscription, error)
    SpawnRoot(context.Context) (string, error)
    Steer(context.Context, string, string) error
    Interrupt(context.Context, string) error
    StopSession(context.Context, string) error
    AnswerQuestion(context.Context, string, string, json.RawMessage) error
    SessionStats(context.Context, string) (manager.SessionStats, error)
    Quiesce(context.Context) error
    Close(context.Context) error
}
```

---

### Task 1: Configurable Supervisor Replay and Server Configuration

**Files:**
- Create: `internal/config/server.go`
- Create: `internal/config/server_test.go`
- Create: `cmd/session_runtime_test.go`
- Modify: `internal/config/config.go:21-90,100-151`
- Modify: `internal/config/config_test.go`
- Modify: `internal/supervisor/events.go:8-84,188-204`
- Modify: `internal/supervisor/events_test.go`
- Modify: `cmd/session_runtime.go:62-150,152-266`

**Interfaces:**
- Produces: `config.ServerConfig.Resolve`, `config.SupervisorEventsConfig.Limits`, `supervisor.NewEventBrokerWithOptions`.
- Preserves: `supervisor.NewEventBroker()` with the existing 4,096-event/16-MiB defaults.

- [ ] **Step 1: Write failing configuration tests**

Add table tests that distinguish nil from explicit zero and parse quoted durations:

```go
func TestSupervisorEventLimitsPreserveOmissionAndZero(t *testing.T) {
    zero := 0
    tests := []struct {
        name string
        cfg  SupervisorEventsConfig
        want EventLimits
        err  string
    }{
        {"omitted", SupervisorEventsConfig{}, EventLimits{4096, 16 << 20}, ""},
        {"disable count", SupervisorEventsConfig{MaxEvents: &zero}, EventLimits{0, 16 << 20}, ""},
        {"disable bytes", SupervisorEventsConfig{MaxBytes: &zero}, EventLimits{4096, 0}, ""},
        {"disable both", SupervisorEventsConfig{MaxEvents: &zero, MaxBytes: &zero}, EventLimits{}, "at least one"},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got, err := tt.cfg.Limits()
            if tt.err != "" {
                if err == nil || !strings.Contains(err.Error(), tt.err) { t.Fatalf("error = %v", err) }
                return
            }
            if err != nil || got != tt.want { t.Fatalf("Limits = %#v, %v", got, err) }
        })
    }
}
```

Also decode a TOML fixture containing `"5s"`, `"1s"`, and `"2m"`, and reject zero/negative resolved durations.

- [ ] **Step 2: Run the configuration tests and confirm failure**

Run:

```bash
go test ./internal/config -run 'Test(SupervisorEvent|ServerConfig)' -count=1
```

Expected: compile failure because the configuration types and resolvers do not exist.

- [ ] **Step 3: Implement raw config presence and defaults**

Add `Server` and `Supervisor` fields to `config.Config`, implement `Duration.UnmarshalText` with `time.ParseDuration`, and have `ValidateSupervisor` call `Events.Limits()`. `ServerConfig.Resolve` supplies duration defaults while leaving empty path/binary fields for manager path resolution.

```go
func (d *Duration) UnmarshalText(text []byte) error {
    parsed, err := time.ParseDuration(string(text))
    if err != nil { return fmt.Errorf("parse duration %q: %w", text, err) }
    d.Duration = parsed
    return nil
}
```

- [ ] **Step 4: Write failing independent-limit broker tests**

Cover count-only, byte-only, both-zero rejection, negative rejection, and an event larger than the byte limit:

```go
func TestEventBrokerCountAndByteLimitsAreIndependent(t *testing.T) {
    countOnly, err := NewEventBrokerWithOptions(EventBrokerOptions{MaxEvents: 2})
    if err != nil { t.Fatal(err) }
    byteOnly, err := NewEventBrokerWithOptions(EventBrokerOptions{MaxBytes: 100})
    if err != nil { t.Fatal(err) }
    for range 3 {
        countOnly.PublishLocal("root", "pi", json.RawMessage(`{"n":1}`))
        byteOnly.PublishLocal("root", "pi", json.RawMessage(`{"payload":"12345678901234567890"}`))
    }
    if got := len(countOnly.Subscribe().Replay); got != 2 { t.Fatalf("count replay = %d", got) }
    if got := len(byteOnly.Subscribe().Replay); got == 0 || got >= 3 { t.Fatalf("byte replay = %d", got) }
}
```

- [ ] **Step 5: Run broker tests and confirm the zero-limit bug**

Run:

```bash
go test ./internal/supervisor -run TestEventBroker -count=1
```

Expected: new count-only and byte-only tests fail because current retention requires both limits to be positive.

- [ ] **Step 6: Implement configurable broker construction and eviction**

Alias the existing supervisor defaults to the config-owned constants so the two default paths cannot drift. Use independent predicates and preserve sequence/live delivery for an oversized event even when it cannot remain in replay:

```go
for (broker.ringCap > 0 && len(broker.ring) > broker.ringCap) ||
    (broker.byteCap > 0 && broker.ringBytes > broker.byteCap) {
    broker.ringBytes -= retainedEventBytes(broker.ring[0])
    broker.ring[0] = EventEnvelope{}
    broker.ring = broker.ring[1:]
}
```

- [ ] **Step 7: Write failing runtime-propagation tests**

Introduce unexported factory-backed variants and assert the configured options are observed before provisioning:

```go
type eventBrokerFactory func(supervisor.EventBrokerOptions) (*supervisor.EventBroker, error)

func validSupervisorConfig() config.Config {
    return config.Config{
        BaseImage: config.BaseImage{Name: "sandbox", Source: "images:", Image: "debian/13"},
        Workers: map[string]config.WorkerProfile{"worker": {
            Description: "work", Provider: "provider", Model: "model",
        }},
    }
}

func TestRunSupervisorSelectsConfiguredEventLimitsBeforeProvisioning(t *testing.T) {
    maxEvents, maxBytes := 7, 1024
    cfg := validSupervisorConfig()
    cfg.Supervisor.Events = config.SupervisorEventsConfig{MaxEvents: &maxEvents, MaxBytes: &maxBytes}
    sentinel := errors.New("broker sentinel")
    err := runSupervisorWithBrokerFactory(context.Background(), cfg, SessionOptions{
        SocketPath: filepath.Join(t.TempDir(), "root.sock"), ConfigPath: "/tmp/config.toml",
    }, io.Discard, func(got supervisor.EventBrokerOptions) (*supervisor.EventBroker, error) {
        if got != (supervisor.EventBrokerOptions{MaxEvents: 7, MaxBytes: 1024}) { t.Fatalf("options = %#v", got) }
        return nil, sentinel
    })
    if !errors.Is(err, sentinel) { t.Fatalf("error = %v", err) }
}
```

For the child-runtime test, write an absolute temporary TOML file containing
`[network]`, `[base_image]`, `[workspace]`, `[workers.worker]`, and:

```toml
[supervisor.events]
max_events = 7
max_bytes = 1024
```

Set `KANEDIAS_CONFIG` to that file and call
`productionChildRunnerWithBrokerFactory` with the same sentinel factory before
any child provisioner is constructed.

- [ ] **Step 8: Wire both runtime paths through the factory**

Production wrappers pass `supervisor.NewEventBrokerWithOptions`; construct the broker immediately after config validation and pass it to `NewRoot`/`NewChild` instead of calling `NewEventBroker()` inline.

```go
func runSupervisor(ctx context.Context, cfg config.Config, opts SessionOptions, out io.Writer) error {
    return runSupervisorWithBrokerFactory(ctx, cfg, opts, out, supervisor.NewEventBrokerWithOptions)
}

func productionChildRunner(ctx context.Context, bootstrap process.Bootstrap, reporter *process.Reporter) error {
    return productionChildRunnerWithBrokerFactory(ctx, bootstrap, reporter, supervisor.NewEventBrokerWithOptions)
}
```

- [ ] **Step 9: Run focused and race tests**

```bash
go test ./internal/config ./internal/supervisor ./cmd -count=1
go test -race ./internal/supervisor ./cmd -count=1
```

Expected: PASS.

- [ ] **Step 10: Commit the replay/configuration foundation**

```bash
git add internal/config internal/supervisor/events.go internal/supervisor/events_test.go cmd/session_runtime.go cmd/session_runtime_test.go
git commit -m "feat: configure supervisor event replay"
```

---

### Task 2: Root-Only Discovery, Tree Validation, and Routing

**Files:**
- Create: `internal/manager/types.go`
- Create: `internal/manager/manager.go`
- Create: `internal/manager/unix.go`
- Create: `internal/manager/tree.go`
- Create: `internal/manager/discovery.go`
- Create: `internal/manager/discovery_test.go`
- Create: `internal/manager/testutil_test.go`
- Modify: `internal/supervisorapi/client.go:34-52,119-122`
- Modify: `internal/supervisorapi/handler_test.go`

**Interfaces:**
- Consumes: `supervisor.EventBrokerOptions`, `supervisor.NodeSnapshot`, existing routed client methods.
- Produces: `manager.New`, `Manager.Fleet`, `Manager.Session`, and an atomic `routes[sessionID]rootID` index.

- [ ] **Step 1: Add a concrete manager-client constructor and strict unary decoding without widening supervisor interfaces**

Write a failing constructor test, then split production construction:

```go
func NewClient(socketPath string) (*DescendantClient, error)
func NewDescendantClient(socketPath string) (supervisor.DescendantClient, error) {
    return NewClient(socketPath)
}
```

Add a response-boundary test containing a valid snapshot followed by a second JSON value. Replace the one-value `Decoder.Decode` in `readDescendantJSON` with whole-body unmarshalling so trailing data is rejected:

```go
if err := json.Unmarshal(body, target); err != nil {
    return contract.NewError(contract.ErrorChildUnavailable, "decode child response: "+err.Error())
}
```

Run:

```bash
go test ./internal/supervisorapi -run TestDescendantClient -count=1
```

Expected after implementation: PASS with all existing parent/child call sites unchanged.

- [ ] **Step 2: Write failing socket and directory validation tests**

Test final-directory mode/owner/symlink policy and candidate socket type/mode/owner/device/inode. Inject `lstat` for a foreign UID because tests cannot chown safely.

```go
func TestInspectRootSocketRejectsSymlinkAndWrongMode(t *testing.T) {
    dir := t.TempDir()
    path := filepath.Join(dir, "candidate.root.sock")
    target := filepath.Join(dir, "target")
    if err := os.Symlink(target, path); err != nil { t.Fatal(err) }
    if _, err := inspectRootSocket(path, os.Lstat, os.Geteuid()); err == nil { t.Fatal("accepted symlink") }
}
```

- [ ] **Step 3: Implement secure option/path normalization**

`New` resolves defaults, creates missing final directories, then validates final `lstat`, EUID, exact `0700`, socket path length, clean absolute config, positive intervals, executable target, and logger. Use `$XDG_RUNTIME_DIR/kanedias/roots` or `/tmp/kanedias-<euid>/roots`; use `$XDG_STATE_HOME/kanedias/sessions` or `~/.local/state/kanedias/sessions` for logs.

```go
func validatePrivateDir(path string) error {
    info, err := os.Lstat(path)
    if err != nil { return err }
    stat, ok := info.Sys().(*syscall.Stat_t)
    if !ok || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() ||
        info.Mode().Perm() != 0o700 || int(stat.Uid) != os.Geteuid() {
        return fmt.Errorf("private directory %q must be an EUID-owned non-symlink mode-0700 directory", path)
    }
    return nil
}
```

- [ ] **Step 4: Write failing recursive-tree validation tests**

Cover a valid depth-three tree and reject wrong root ID, wrong parent, root kind below top, duplicate IDs, unknown lifecycle, and cross-root route collisions. Add these test builders to `testutil_test.go`:

```go
func rootTree(id string, children ...supervisor.NodeSnapshot) supervisor.NodeSnapshot {
    return supervisor.NodeSnapshot{
        SessionID: id, RootSessionID: id, PiSessionID: "pi-" + id,
        SessionFile: "/sessions/" + id + ".jsonl",
        Kind: contract.ChildKindRoot, Context: contract.ContextRoot,
        Lifecycle: string(supervisor.LifecycleReady), Children: children,
        Questions: []supervisor.QuestionSummary{},
    }
}

func childTree(id, parent string, children ...supervisor.NodeSnapshot) supervisor.NodeSnapshot {
    return supervisor.NodeSnapshot{
        SessionID: id, ParentSessionID: parent, RootSessionID: "root",
        PiSessionID: "pi-" + id, SessionFile: "/sessions/" + id + ".jsonl",
        Kind: contract.ChildKindRead, Context: contract.ContextFresh,
        WorkerType: "reviewer", Lifecycle: string(supervisor.LifecycleReady),
        Children: children, Questions: []supervisor.QuestionSummary{},
    }
}

func TestValidateRootTreeBuildsCompleteRoutes(t *testing.T) {
    tree := rootTree("root", childTree("child", "root", childTree("grandchild", "child")))
    normalized, routes, err := validateRootTree(tree)
    if err != nil { t.Fatal(err) }
    if len(routes) != 3 || routes["grandchild"] != "root" { t.Fatalf("routes = %#v", routes) }
    if normalized.Children[0].Children[0].ParentSessionID != "child" { t.Fatal("parent changed") }
}
```

- [ ] **Step 5: Implement iterative validation and stable sorting**

Validate every node and sort children by session ID before comparing/committing snapshots. Allow starting descendants without Pi binding; require bound Pi identity only for the admitted top root.

```go
type treeWork struct {
    node     *supervisor.NodeSnapshot
    parentID string
    rootID   string
}

func validateRootTree(root supervisor.NodeSnapshot) (supervisor.NodeSnapshot, map[string]string, error) {
    routes := map[string]string{}
    work := []treeWork{{node: &root, rootID: root.SessionID}}
    // Pop, validate identity/lifecycle, reject duplicates, sort children,
    // and append children with parentID set to the current node.
    return root, routes, nil
}
```

- [ ] **Step 6: Write failing discovery reconciliation tests**

Use fake clients keyed by socket path. Prove scans:

- inspect only direct `*.root.sock` entries;
- ignore `<sessionID>.sock` children;
- validate socket identity before and after the probe;
- admit `ready`/`running` roots;
- retry `provisioning`/`starting` roots;
- expose sanitized malformed/conflict issues;
- retain stale existing roots on probe failure;
- remove a root only when its exact path disappears or is replaced; and
- never call `os.Remove`.

- [ ] **Step 7: Implement `discoverOnce` and atomic route commits**

Under the manager lock, either commit the complete normalized tree plus routes or commit nothing. A successful `stopping`/`failed` refresh replaces the displayed tree, marks it non-actionable, and remains until path disappearance.

```go
func (m *Manager) commitTree(handle *rootHandle, tree supervisor.NodeSnapshot, candidate map[string]string) error {
    m.mu.Lock()
    defer m.mu.Unlock()
    for sessionID, rootID := range candidate {
        if existing, ok := m.routes[sessionID]; ok && existing != rootID {
            return fmt.Errorf("route conflict for session %q", sessionID)
        }
    }
    m.removeRoutesLocked(handle.rootID)
    for sessionID, rootID := range candidate { m.routes[sessionID] = rootID }
    handle.tree = tree
    return nil
}
```

- [ ] **Step 8: Run manager discovery tests**

```bash
go test ./internal/manager -run 'Test(Inspect|ValidateRootTree|Discover)' -count=1
go test -race ./internal/manager -run 'Test(Discover|Route)' -count=1
```

Expected: PASS.

- [ ] **Step 9: Commit root discovery**

```bash
git add internal/manager internal/supervisorapi/client.go internal/supervisorapi/handler_test.go
git commit -m "feat: discover root supervisor trees"
```

---

### Task 3: Continuous Monitoring, Replay Mirrors, and Browser Fanout

**Files:**
- Create: `internal/manager/mirror.go`
- Create: `internal/manager/mirror_test.go`
- Create: `internal/manager/projection.go`
- Create: `internal/manager/projection_test.go`
- Create: `internal/manager/notify.go`
- Create: `internal/manager/notify_test.go`
- Create: `internal/manager/monitor.go`
- Create: `internal/manager/monitor_test.go`
- Modify: `internal/manager/manager.go`
- Modify: `internal/manager/types.go`

**Interfaces:**
- Consumes: admitted root handles and configured replay limits from Tasks 1–2.
- Produces: `Manager.Start`, fleet/session subscriptions, stale/stream/gap projections, nonblocking browser update signals.

- [ ] **Step 1: Write failing mirror tests**

Preserve upstream root sequence numbers and test initial gaps, reconnect duplicates, later gaps, count-only/byte-only limits, payload cloning, and per-session filtering.

```go
func TestMirrorDeduplicatesReplayAndRecordsFirstGap(t *testing.T) {
    mirror := newEventMirror(supervisor.EventBrokerOptions{MaxEvents: 8, MaxBytes: 4096})
    envelope := func(seq uint64, session string) supervisor.EventEnvelope {
        return supervisor.EventEnvelope{
            Seq: seq, SessionID: session, SourceSeq: seq,
            Kind: "pi", Payload: json.RawMessage(`{"type":"agent_start"}`),
        }
    }
    mirror.Accept(envelope(4, "a"))
    mirror.Accept(envelope(4, "a"))
    mirror.Accept(envelope(6, "b"))
    if got := mirror.Events(); len(got) != 2 { t.Fatalf("events = %#v", got) }
    gap := mirror.Gap()
    if gap == nil || gap.ExpectedSeq != 1 || gap.FirstAvailableSeq != 4 { t.Fatalf("gap = %#v", gap) }
}
```

- [ ] **Step 2: Implement the bounded root mirror**

Use the same retained-byte accounting as the supervisor but never assign a second sequence. Reject invalid zero sequences, empty session/kind, or invalid JSON before advancing the cursor.

```go
func (m *eventMirror) Accept(event supervisor.EventEnvelope) bool {
    if event.Seq <= m.lastSeq { return false }
    if event.Seq > m.lastSeq+1 && m.gap == nil {
        m.gap = &ReplayGap{ExpectedSeq: m.lastSeq + 1, FirstAvailableSeq: event.Seq}
    }
    m.lastSeq = event.Seq
    m.events = append(m.events, cloneEnvelope(event))
    m.evict()
    return true
}
```

- [ ] **Step 3: Write failing activity-projection tests**

Feed allowlisted Pi payloads and assert safe output. For `message_update`, accumulate only `assistantMessageEvent.type == "text_delta"`; for tools, track by `toolCallId`; for unknown events, return `ActivityItem{Kind:"event", Label:"Pi event: <type>"}` without raw payload.

- [ ] **Step 4: Implement projection from the retained mirror**

`Manager.Session` filters the root mirror by `SessionID` and projects at read time, so sessions not currently selected still retain recent activity.

```go
func projectActivity(events []supervisor.EventEnvelope, sessionID string) []ActivityItem {
    projector := newActivityProjector()
    for _, event := range events {
        if event.SessionID == sessionID { projector.Apply(event) }
    }
    return projector.Items()
}
```

- [ ] **Step 5: Write failing subscriber-backpressure tests**

```go
func TestSlowChangeSubscriberIsDisconnected(t *testing.T) {
    fanout := newChangeFanout(1)
    slow := fanout.Subscribe()
    fanout.Publish(1)
    fanout.Publish(2)
    <-slow.Updates
    if _, open := <-slow.Updates; open { t.Fatal("slow subscriber remains open") }
}
```

- [ ] **Step 6: Implement fleet/detail fanout**

Use bounded mailboxes, close/remove full subscribers without blocking, clone public projections, and make `Close` idempotent.

```go
select {
case subscriber.updates <- revision:
default:
    delete(fanout.subscribers, subscriber.id)
    close(subscriber.updates)
}
```

- [ ] **Step 7: Write failing monitor tests**

Use fake subscriptions that place replay either in `Subscription.Replay` or on `Events`, then close with an error. Assert:

- replay is consumed before live events;
- EOF changes only `StreamConnected`;
- reconnect deduplicates retained replay;
- snapshot failure marks stale but retains the last tree;
- a later snapshot clears stale and rebuilds routes;
- structural removal and delayed question retention are observed by polling; and
- stopping/failed snapshots remain visible but reject routes.

- [ ] **Step 8: Implement independent snapshot and event loops**

Use one non-overlapping snapshot loop and one reconnecting event loop per root. Backoff starts at 100 ms, caps at 5 seconds, and adds ±20% jitter from injected randomness. Reset only after an accepted event or at least one healthy snapshot interval.

```go
func (m *Manager) monitorRoot(handle *rootHandle) {
    m.monitorWG.Add(2)
    go func() { defer m.monitorWG.Done(); m.snapshotLoop(handle) }()
    go func() { defer m.monitorWG.Done(); m.eventLoop(handle) }()
}
```

- [ ] **Step 9: Implement `Start`, `Quiesce`, and monitor-only `Close`**

`Start` performs one discovery pass before launching periodic discovery. `Quiesce` rejects writes/spawns and stops discovery/snapshot polling while event drains remain until `Close`. `Close` cancels subscriptions, waits only for manager monitor goroutines, closes clients, and never calls root `Stop`.

```go
func (m *Manager) Close(ctx context.Context) error {
    _ = m.Quiesce(ctx)
    m.closeCancel()
    done := make(chan struct{})
    go func() { m.monitorWG.Wait(); close(done) }()
    select {
    case <-done:
        return m.closeClients()
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

- [ ] **Step 10: Run focused race tests**

```bash
go test ./internal/manager -run 'Test(Mirror|Project|SlowChange|Monitor|ManagerStart|ManagerClose)' -count=1
go test -race ./internal/manager -count=1
```

Expected: PASS.

- [ ] **Step 11: Commit monitoring**

```bash
git add internal/manager
git commit -m "feat: monitor supervisor replay and snapshots"
```

---

### Task 4: Detached Root Spawn and Admission Transaction

**Files:**
- Create: `internal/manager/spawn.go`
- Create: `internal/manager/spawn_test.go`
- Modify: `internal/manager/manager.go`
- Modify: `internal/manager/types.go`
- Modify: `internal/manager/testutil_test.go`

**Interfaces:**
- Consumes: secure paths, root admission, client factory, monitor registration.
- Produces: `Manager.SpawnRoot` and a cached waiter excluded from manager shutdown waits.

- [ ] **Step 1: Define injected process seams and write failing argv tests**

```go
type spawnSpec struct {
    Path        string
    Args        []string
    Env         []string
    Stdin       *os.File
    Output      *os.File
    SysProcAttr *syscall.SysProcAttr
}

type spawnedProcess interface {
    PID() int
    Done() <-chan struct{}
    WaitErr() error
    SignalGroup(syscall.Signal) error
}

type processStarter interface { Start(spawnSpec) (spawnedProcess, error) }
```

Assert exact arguments:

```text
<binary> --config <clean-absolute-config> session --socket <absolute-token>.root.sock
```

and `SysProcAttr{Setsid:true}` in the production starter.

- [ ] **Step 2: Run the spawn tests and confirm failure**

```bash
go test ./internal/manager -run TestRootSpawner -count=1
```

Expected: compile failure because `spawn.go` does not exist.

- [ ] **Step 3: Implement token, log, and process startup**

Generate 32 random bytes as lowercase hex; verify Unix path length; create `<token>.log` with `O_CREATE|O_EXCL|O_WRONLY` and `0600`; open `/dev/null`; start with `exec.Command` and `Setsid`. Close parent log/stdin descriptors after `Start`; the child keeps inherited descriptors.

```go
cmd := exec.Command(spec.Path, spec.Args[1:]...)
cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Env, spec.Stdin, spec.Output, spec.Output
cmd.SysProcAttr = spec.SysProcAttr
if err := cmd.Start(); err != nil { return nil, err }
started := &startedProcess{cmd: cmd, done: make(chan struct{})}
go started.waitExactlyOnce()
return started, nil
```

The cached waiter owns the only `Wait` call:

```go
func (process *startedProcess) waitExactlyOnce() {
    process.waitErr = process.cmd.Wait()
    close(process.done)
}
```

- [ ] **Step 4: Add a self-reexec helper test for OS behavior**

The helper reports its SID, stdin EOF, argv, stdout, and stderr. Assert `unix.Getsid(0) == os.Getpid()` using `golang.org/x/sys/unix`, and assert that the cached waiter calls `cmd.Wait` exactly once.

- [ ] **Step 5: Write failing admission-state tests**

Fake snapshots transition `provisioning -> starting -> ready`. Assert only `ready`/`running` with nonempty Pi fields commits. Assert process exit, request timeout, socket replacement, malformed tree, and duplicate root ID abort before registration.

- [ ] **Step 6: Implement admission polling and atomic commit**

The earlier of request cancellation and configured `spawn_timeout` bounds admission. Recheck socket device/inode after each successful snapshot. Atomic registration is the lifetime boundary: once committed, do not recheck request cancellation or tie the root to it.

```go
select {
case <-process.Done():
    return "", fmt.Errorf("root exited before admission: %w", process.WaitErr())
case <-admissionCtx.Done():
    return "", admissionCtx.Err()
case <-probe.C:
    snapshot, identity, err := pending.probe(admissionCtx)
    if err == nil && admissible(snapshot) {
        return m.commitSpawn(pending, snapshot, identity)
    }
}
```

- [ ] **Step 7: Write failing cleanup escalation tests**

Use a fake process and client to assert the exact order:

```text
root Stop (when root ID is known) -> wait -> SIGTERM group -> wait -> SIGKILL group -> reap
```

Assert cleanup never removes a changed inode and that the overall path returns within 30 seconds under an injected clock.

- [ ] **Step 8: Implement failed-admission cleanup**

Use `context.WithTimeout(context.Background(), 30*time.Second)`, never the canceled request context. Unlink only the exact manager-created candidate identity; discovery paths remain read-only.

```go
waitUntil := func(ctx context.Context, done <-chan struct{}, duration time.Duration) bool {
    timer := time.NewTimer(duration)
    defer timer.Stop()
    select { case <-done: return true; case <-timer.C: return false; case <-ctx.Done(): return false }
}
cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
stopCtx, stopCancel := context.WithTimeout(cleanupCtx, 5*time.Second)
_ = pending.stopIfResponsive(stopCtx)
stopCancel()
// SignalGroup checks process.Done immediately before syscall.Kill to bound PID-reuse risk.
if !waitUntil(cleanupCtx, pending.process.Done(), 5*time.Second) { _ = pending.process.SignalGroup(syscall.SIGTERM) }
if !waitUntil(cleanupCtx, pending.process.Done(), 10*time.Second) { _ = pending.process.SignalGroup(syscall.SIGKILL) }
select {
case <-pending.process.Done():
case <-cleanupCtx.Done(): return cleanupCtx.Err()
}
return errors.Join(pending.process.WaitErr(), pending.safeUnlinkOwnedSocket())
```

- [ ] **Step 9: Prove admitted roots survive cancellation and manager close**

After `SpawnRoot` returns, cancel its request and call `Manager.Close`; assert no `Stop`, `SIGTERM`, `SIGKILL`, or waiter wait appears in the fake process log.

- [ ] **Step 10: Run process and race tests**

```bash
go test ./internal/manager -run 'Test(RootSpawner|SpawnRoot|AdmissionCleanup)' -count=1
go test -race ./internal/manager -count=1
```

Expected: PASS.

- [ ] **Step 11: Commit spawning**

```bash
git add internal/manager/spawn.go internal/manager/spawn_test.go internal/manager/manager.go internal/manager/types.go internal/manager/testutil_test.go
git commit -m "feat: launch independent root supervisors"
```

---

### Task 5: Typed Pi Controls and Supported Metrics

**Files:**
- Create: `internal/manager/pi.go`
- Create: `internal/manager/pi_test.go`
- Modify: `internal/manager/manager.go`
- Modify: `internal/manager/types.go`

**Interfaces:**
- Consumes: current actionable routes and `rootClient.CallRPC/AnswerQuestion/Stop`.
- Produces: `Steer`, `Interrupt`, `StopSession`, `AnswerQuestion`, and `SessionStats`.

- [ ] **Step 1: Write failing generic Pi response tests**

Define exact envelope validation:

```go
type piResponse[T any] struct {
    Type    string `json:"type"`
    Command string `json:"command"`
    Success bool   `json:"success"`
    Data    T      `json:"data"`
    Error   string `json:"error,omitempty"`
}
```

Reject invalid JSON, wrong type, wrong command, trailing data, and `success:false` even when transport returned no Go error.

- [ ] **Step 2: Implement typed command constructors and response decoding**

Use `json.Marshal` for messages; never interpolate operator text into JSON. Keep commands private and exact:

```go
map[string]any{"type":"get_state"}
map[string]any{"type":"steer", "message":message}
map[string]any{"type":"prompt", "message":message}
map[string]any{"type":"abort"}
map[string]any{"type":"get_session_stats"}
```

- [ ] **Step 3: Write failing streaming/idle Steer tests**

Record both calls through a fake root client. `isStreaming:true` must emit `get_state` then `steer`; false must emit `get_state` then `prompt`. A race-produced second-call `success:false` returns an inline-safe error.

- [ ] **Step 4: Implement high-level control methods**

Resolve routes under the manager lock, copy the client, release the lock before network I/O, and reject stale/non-actionable roots. `Interrupt` sends `abort`; `StopSession` calls the supervisor stop method; `AnswerQuestion` forwards the selected session/question and exact raw answer.

```go
func (m *Manager) Interrupt(ctx context.Context, sessionID string) error {
    client, err := m.actionableClient(sessionID)
    if err != nil { return err }
    raw, err := client.CallRPC(ctx, sessionID, mustJSON(map[string]any{"type":"abort"}))
    if err != nil { return err }
    _, err = decodePiResponse[struct{}](raw, "abort")
    return err
}
```

- [ ] **Step 5: Write failing stats tests including nullable compaction state**

Use JSON with absent `contextUsage`, then `tokens:null` and `percent:null`, and verify pointer fields remain nil instead of becoming zero. Confirm tokens/cost/message/tool fields map exactly.

- [ ] **Step 6: Implement `SessionStats`**

Decode the installed Pi 0.83.0 shape and validate returned Pi session ID matches the selected node's `PiSessionID` when both are present.

```go
func (m *Manager) SessionStats(ctx context.Context, sessionID string) (SessionStats, error) {
    client, node, err := m.actionableClientAndNode(sessionID)
    if err != nil { return SessionStats{}, err }
    raw, err := client.CallRPC(ctx, sessionID, mustJSON(map[string]any{"type":"get_session_stats"}))
    if err != nil { return SessionStats{}, err }
    data, err := decodePiResponse[piSessionStats](raw, "get_session_stats")
    if err != nil { return SessionStats{}, err }
    if data.SessionID != "" && node.PiSessionID != "" && data.SessionID != node.PiSessionID {
        return SessionStats{}, fmt.Errorf("Pi stats identity mismatch")
    }
    return projectStats(data), nil
}
```

- [ ] **Step 7: Run focused tests**

```bash
go test ./internal/manager -run 'Test(PiResponse|Steer|Interrupt|StopSession|AnswerQuestion|SessionStats)' -count=1
go test -race ./internal/manager -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit controls**

```bash
git add internal/manager/pi.go internal/manager/pi_test.go internal/manager/manager.go internal/manager/types.go
git commit -m "feat: route typed pi supervisor controls"
```

---

### Task 6: Server CLI Configuration and Non-Destructive Application Lifecycle

**Files:**
- Modify: `cmd/root.go:27-111`
- Modify: `cmd/server.go`
- Modify: `cmd/root_test.go:270-376,700-799`
- Modify: `internal/server/server.go`
- Modify: `internal/server/server_test.go`
- Modify: `internal/server/handler.go` constructor signature only

**Interfaces:**
- Consumes: resolved config and production `manager.New`.
- Produces: `server.Run(context.Context, config.Config, server.Options)` and shutdown ordering hooks.

```go
type Options struct {
    ListenAddress   string
    Logger          *slog.Logger
    BootstrapOutput io.Writer
    ConfigPath      string
}

type managerFactory func(manager.Options) (fleetManager, error)

func Run(context.Context, config.Config, Options) error
func runApplication(context.Context, config.Config, Options, managerFactory, listenFunc) error
```

- [ ] **Step 1: Write failing CLI config-forwarding tests**

Change `services.runServer` to:

```go
runServer func(context.Context, config.Config, server.Options) error
```

Assert a valid server invocation loads the selected file once, resolves a clean absolute path, forwards command stderr as `BootstrapOutput`, and still rejects unsafe listeners before loading config.

- [ ] **Step 2: Implement the command seam**

Change `newServerCommand(service, getConfigPath)`. Validate listen first, then resolve/load config and call:

```go
server.Options{
    ListenAddress:   listenAddress,
    Logger:          logger,
    BootstrapOutput: command.ErrOrStderr(),
    ConfigPath:      absoluteConfig,
}
```

- [ ] **Step 3: Write failing server lifecycle-order tests**

Use a fake managed fleet and blocking SSE-like handler. Assert cancellation order:

```text
manager.Quiesce -> stream context canceled -> HTTP Shutdown -> manager.Close
```

and assert neither shutdown method invokes `StopSession`.

- [ ] **Step 4: Refactor listener/handler construction around the effective address**

Introduce an internal handler factory:

```go
type handlerFactory func(effectiveAddress string, streamContext context.Context) (http.Handler, error)
```

Call it after `net.Listen`, before `http.Server.Serve`, so security can lock exact Host/Origin in Task 7.

- [ ] **Step 5: Construct and start the production manager**

Resolve `cfg.Server`, event limits, and manager path defaults; call `manager.New`, then `Start`. Initial discovery failure should fail server startup; zero discovered roots is valid.

```go
resolved, err := cfg.Server.Resolve()
if err != nil { return err }
limits, err := cfg.Supervisor.Events.Limits()
if err != nil { return err }
fleet, err := manager.New(manager.Options{
    ConfigPath: options.ConfigPath, RootSocketDir: resolved.RootSocketDir,
    SessionLogDir: resolved.SessionLogDir, SessionBinary: resolved.SessionBinary,
    DiscoveryInterval: resolved.DiscoveryInterval, SnapshotInterval: resolved.SnapshotInterval,
    SpawnTimeout: resolved.SpawnTimeout,
    EventLimits: supervisor.EventBrokerOptions{MaxEvents: limits.MaxEvents, MaxBytes: limits.MaxBytes},
    Logger: options.Logger,
})
if err != nil { return err }
if err := fleet.Start(ctx); err != nil { return errors.Join(err, fleet.Close(context.Background())) }
```

- [ ] **Step 6: Implement phased shutdown**

Use separate manager/persistent-stream contexts. Cancel streams before `http.Server.Shutdown`; call manager `Close` afterward. Preserve existing forced-close/error joining and structured logging.

```go
_ = fleet.Quiesce(shutdownCtx)
streamCancel()
shutdownErr := httpServer.Shutdown(shutdownCtx)
managerErr := fleet.Close(shutdownCtx)
return errors.Join(shutdownErr, managerErr, serveErr)
```

- [ ] **Step 7: Run command/server tests**

```bash
go test ./cmd -run TestServerCommand -count=1
go test ./internal/server -run 'Test(Run|CoordinateServer|ManagerLifecycle)' -count=1
go test -race ./cmd ./internal/server ./internal/manager -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit server lifecycle wiring**

```bash
git add cmd/root.go cmd/server.go cmd/root_test.go internal/server/server.go internal/server/server_test.go internal/server/handler.go
git commit -m "feat: wire supervisor manager into server lifecycle"
```

---

### Task 7: Browser Capability and Same-Origin Write Boundary

**Files:**
- Create: `internal/server/security.go`
- Create: `internal/server/security_test.go`
- Create: `internal/server/signals.go`
- Create: `internal/server/signals_test.go`
- Modify: `internal/server/handler.go`
- Modify: `internal/server/handler_test.go`
- Modify: `internal/server/server.go`

**Interfaces:**
- Produces: bootstrap query `capability`, cookie `kanedias_session`, authenticated route middleware, and strict `decodeSignals[T]`.

- [ ] **Step 1: Write failing capability-store tests**

Use a deterministic reader and assert:

- bootstrap tokens and browser tokens contain 32 random bytes in base64url form;
- only SHA-256 digests are retained;
- invalid bootstrap returns 403;
- valid bootstrap returns 303 `/` and required headers;
- cookie is `HttpOnly`, `SameSite=Strict`, `Path=/`;
- the bootstrap token can issue a second browser session during one process; and
- a new store rejects cookies from the previous store.

- [ ] **Step 2: Implement capability storage and bootstrap handler**

Use `subtle.ConstantTimeCompare` on fixed-size SHA-256 digests. The startup token is printed once to `BootstrapOutput`; request logs continue to record only `URL.Path`.

```go
provided := sha256.Sum256([]byte(r.URL.Query().Get(bootstrapQueryName)))
if subtle.ConstantTimeCompare(provided[:], store.bootstrapDigest[:]) != 1 {
    http.Error(w, "Forbidden", http.StatusForbidden)
    return
}
browserToken, browserDigest, err := newCapability(store.random)
if err != nil { http.Error(w, "Internal Server Error", http.StatusInternalServerError); return }
store.addSession(browserDigest)
http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: browserToken, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
http.Redirect(w, r, "/", http.StatusSeeOther)
```

- [ ] **Step 3: Write failing boundary-middleware tests**

For effective address `127.0.0.1:43127`, assert exact Host and Origin, absent or `same-origin` `Sec-Fetch-Site`, and parsed media type `application/json`. Verify statuses 401/403/415.

- [ ] **Step 4: Implement the request boundary**

```go
type requestBoundary struct {
    Host   string
    Origin string
}
```

Derive it only from `listener.Addr().String()`. Do not trust request Host to construct the expected origin.

- [ ] **Step 5: Write failing strict-signal tests**

Cap action bodies at 64 KiB, reject unknown fields/trailing JSON/empty bodies, and test exact types:

```go
type steerSignals struct { Message string `json:"message"` }
type selectedSessionSignals struct { SelectedSessionID string `json:"selectedSessionId"` }
type answerSignals struct {
    Value *string `json:"value,omitempty"`
    Confirmed *bool `json:"confirmed,omitempty"`
    Cancelled bool `json:"cancelled,omitempty"`
}
```

- [ ] **Step 6: Implement `decodeSignals[T]` before SSE creation**

Wrap non-GET bodies with `http.MaxBytesReader`, call `datastar.ReadSignals` into `json.RawMessage`, then decode with `DisallowUnknownFields` and a second decode requiring `io.EOF`.

```go
func decodeSignals[T any](w http.ResponseWriter, r *http.Request) (T, error) {
    var zero T
    if r.Method != http.MethodGet { r.Body = http.MaxBytesReader(w, r.Body, 64<<10) }
    var raw json.RawMessage
    if err := datastar.ReadSignals(r, &raw); err != nil { return zero, err }
    decoder := json.NewDecoder(bytes.NewReader(raw))
    decoder.DisallowUnknownFields()
    var value T
    if err := decoder.Decode(&value); err != nil { return zero, err }
    var extra any
    if err := decoder.Decode(&extra); err != io.EOF { return zero, fmt.Errorf("signals contain multiple JSON values") }
    return value, nil
}
```

- [ ] **Step 7: Wire authenticated route groups**

Leave only `/healthz`, `/bootstrap`, and immutable `/assets/*` unauthenticated. Require a session cookie for `/` and all `/ui/*`; apply the write boundary to all action POSTs.

```go
router.Get("/healthz", serveHealth)
router.Get("/bootstrap", auth.serveBootstrap)
router.Group(func(protected chi.Router) {
    protected.Use(auth.requireSession)
    protected.Get("/", serveIndex)
    protected.Route("/ui", func(ui chi.Router) {
        ui.Get("/fleet", serveFleet)
        ui.Get("/session", serveSession)
    })
})
```

- [ ] **Step 8: Prove secrets stay out of request logs**

Extend request logging tests with a bootstrap query and Cookie header. Assert the structured output contains path/status but neither token nor cookie.

- [ ] **Step 9: Run security tests**

```bash
go test ./internal/server -run 'Test(Capability|Bootstrap|SessionCookie|RequestBoundary|DecodeSignals|RequestLogging)' -count=1
go test -race ./internal/server -count=1
```

Expected: PASS.

- [ ] **Step 10: Commit browser security**

```bash
git add internal/server/security.go internal/server/security_test.go internal/server/signals.go internal/server/signals_test.go internal/server/handler.go internal/server/handler_test.go internal/server/server.go
git commit -m "feat: protect local supervisor controls"
```

---

### Task 8: Dynamic Astrolabe Templates and Read Streams

**Files:**
- Create: `internal/server/render.go`
- Create: `internal/server/render_test.go`
- Create: `internal/server/view.go`
- Create: `internal/server/web/fleet.html`
- Create: `internal/server/web/detail.html`
- Create: `internal/server/web/questions.html`
- Create: `internal/server/web/activity.html`
- Create: `internal/server/web/deck-status.html`
- Create: `internal/server/web/app.js`
- Modify: `internal/server/web/index.html`
- Modify: `internal/server/web/app.css`
- Modify: `internal/server/handler.go`
- Rewrite relevant static assertions in: `internal/server/handler_test.go`

**Interfaces:**
- Consumes: manager fleet/session projections and change subscriptions.
- Produces: authenticated `GET /`, `GET /ui/fleet`, and `GET /ui/session` with stable Datastar patch targets.

- [ ] **Step 1: Write failing template-structure tests**

Parse every fragment and assert exactly these stable roots exist:

```text
#fleet-panel
#detail-panel
#question-panel
#activity-panel
#deck-status
```

Assert rendered output excludes mock agent names/questions, Spawn Subagent, completion percentage, synthetic elapsed time, tool success, and invented cache-hit progress.

- [ ] **Step 2: Split the shell and fragments**

The shell owns sibling stable roots and initial signals:

```html
<body data-signals='{"selectedSessionId":"","commandMessage":""}' data-init="@get('/ui/fleet')">
  <div id="fleet-panel"></div>
  <div id="detail-panel"></div>
  <div id="question-panel"></div>
  <div id="activity-panel"></div>
  <div id="deck-status" role="status"></div>
</body>
```

Preserve the Astrolabe visual classes but render only supported model/state fields.

- [ ] **Step 3: Implement safe view models and rendering helpers**

Use `html/template`; never build row/question HTML with string concatenation. `patchTemplate` renders to a buffer before calling `NewSSE`/`PatchElements` so template errors do not partially open a stream.

```go
func renderTemplate(templates *template.Template, name string, data any) (string, error) {
    var buffer bytes.Buffer
    if err := templates.ExecuteTemplate(&buffer, name, data); err != nil { return "", err }
    return buffer.String(), nil
}

func patchTemplate(sse *datastar.ServerSentEventGenerator, templates *template.Template, name, id string, data any) error {
    rendered, err := renderTemplate(templates, name, data)
    if err != nil { return err }
    return sse.PatchElements(rendered, datastar.WithSelectorID(id), datastar.WithModeOuter())
}
```

- [ ] **Step 4: Write failing fleet-stream tests**

With a fake manager subscription, assert:

- initial `#fleet-panel` patch includes two unrelated roots once each;
- descendants are nested under the correct root;
- stale/conflict/gap indicators render;
- later revisions patch the same target; and
- request/server-stream cancellation closes the manager subscription promptly.

- [ ] **Step 5: Implement the fleet stream**

Subscribe before reading the initial snapshot to avoid a state-change race. After rendering succeeds, create SSE with a context canceled by request or server stream shutdown. On each revision, fetch and patch a fresh projection.

```go
subscription := fleet.SubscribeFleet()
defer subscription.Close()
initial, err := renderTemplate(templates, templateFleet, newFleetView(fleet.Fleet()))
if err != nil { return }
streamCtx, cancel := mergeStreamContext(r.Context(), serverStreams)
defer cancel()
sse := datastar.NewSSE(w, r, datastar.WithContext(streamCtx))
if err := sse.PatchElements(initial, datastar.WithSelectorID("fleet-panel"), datastar.WithModeOuter()); err != nil { return }
for {
    select {
    case <-streamCtx.Done(): return
    case _, open := <-subscription.Updates:
        if !open { return }
        if err := patchTemplate(sse, templates, templateFleet, "fleet-panel", newFleetView(fleet.Fleet())); err != nil { return }
    }
}
```

- [ ] **Step 6: Write failing selected-detail tests**

Decode `selectedSessionId` from GET Datastar signals; establish the selected subscription; fetch stats only for actionable non-stale nodes. Assert detail/question/activity targets patch independently, nullable context metrics render `—`, and a burst of 100 activity revisions does not trigger 100 `get_session_stats` calls.

- [ ] **Step 7: Implement selected-detail streaming**

Use the Astrolabe dial for `ContextUsage.Percent` and label it `context`. Render recent activity as explicitly bounded and show root-wide incomplete/gap text. Coalesce activity patches to at most one per 50 ms and refresh `get_session_stats` at most once per second, reusing the last successful stats between refreshes.

```go
lastStatsAt := time.Time{}
var cached manager.SessionStats
render := func(now time.Time) error {
    state, err := fleet.Session(signals.SelectedSessionID)
    if err != nil { return err }
    if !state.RootStale && now.Sub(lastStatsAt) >= time.Second {
        if stats, statsErr := fleet.SessionStats(streamCtx, signals.SelectedSessionID); statsErr == nil {
            cached, lastStatsAt = stats, now
        }
    }
    return patchSessionTargets(sse, templates, state, cached)
}
```

- [ ] **Step 8: Replace one-time browser arrays with delegated behavior**

Move inline JS to `/assets/app.js`. Tabs/search/mobile interactions use `document.addEventListener` and query the current DOM per event. Selection uses a stable detail URL and automatic cancellation:

```html
data-on:click="$selectedSessionId = el.dataset.sessionId; @get('/ui/session', {payload:{selectedSessionId:el.dataset.sessionId}, requestCancellation:'auto'})"
```

- [ ] **Step 9: Update asset/template tests**

Add `/assets/app.js`, preserve local-only/provenance assertions, and replace exact-two-script/`selectRow` checks with delegated/Datastar wiring checks.

```go
for _, path := range []string{
    "/assets/terminal.css", "/assets/app.css", "/assets/datastar.js", "/assets/app.js",
} {
    response := serveAuthenticatedRequest(t, handler, http.MethodGet, path, nil)
    if response.Code != http.StatusOK { t.Fatalf("GET %s = %d", path, response.Code) }
}
```

- [ ] **Step 10: Run read/UI tests**

```bash
go test ./internal/server -run 'Test(Templates|FleetStream|SessionStream|InitialPage|Astrolabe|Assets)' -count=1
go test -race ./internal/server -count=1
```

Expected: PASS.

- [ ] **Step 11: Commit dynamic read views**

```bash
git add internal/server/render.go internal/server/render_test.go internal/server/view.go internal/server/handler.go internal/server/handler_test.go internal/server/web
git commit -m "feat: stream supervisor fleet into astrolabe"
```

---

### Task 9: Datastar Actions and Live Command Deck

**Files:**
- Modify: `internal/server/handler.go`
- Modify: `internal/server/handler_test.go`
- Modify: `internal/server/view.go`
- Modify: `internal/server/web/index.html`
- Modify: `internal/server/web/fleet.html`
- Modify: `internal/server/web/questions.html`
- Modify: `internal/server/web/deck-status.html`
- Modify: `internal/server/web/app.js`

**Interfaces:**
- Consumes: authenticated strict signals and high-level manager controls.
- Produces: New Session, Steer, Interrupt Turn, Stop Session, and Answer Question routes that patch only `#deck-status`.

- [ ] **Step 1: Write failing action-route tests**

Table-drive exact routes/bodies and verify manager calls:

```text
POST /ui/sessions                                      {}
POST /ui/sessions/{id}/steer                           {"message":"focus"}
POST /ui/sessions/{id}/interrupt                       {}
POST /ui/sessions/{id}/stop                            {}
POST /ui/sessions/{id}/questions/{qid}                 method-correct answer
```

Every success and business error must emit exactly one Datastar patch for `#deck-status`.

- [ ] **Step 2: Implement action handlers with pre-SSE validation**

Validate cookie/boundary/signals/session IDs before `datastar.NewSSE`. Call only high-level manager methods; handlers never construct raw Pi commands.

```go
func serveSteer(fleet fleetManager, templates *template.Template) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        signals, err := decodeSignals[steerSignals](w, r)
        if err != nil { writeBoundaryError(w, err); return }
        sessionID := chi.URLParam(r, "sessionID")
        operationErr := fleet.Steer(r.Context(), sessionID, signals.Message)
        patchDeckStatus(w, r, templates, operationErr)
    }
}
```

- [ ] **Step 3: Write failing Pi `success:false` handler tests**

Make fake `Steer` return a sanitized Pi rejection and assert an error fragment—not success copy or HTTP 500—is patched.

- [ ] **Step 4: Render manager errors safely**

Map typed contract errors, unavailable/stale routes, and Pi failures to stable operator copy. Log full internal errors server-side; never render socket paths, session files, capabilities, or panic values.

```go
func operatorMessage(err error) string {
    var typed *contract.Error
    if errors.As(err, &typed) {
        switch typed.Code {
        case contract.ErrorNotFound: return "Session is no longer available."
        case contract.ErrorSessionStopping: return "Session is stopping."
        case contract.ErrorConflict: return "Session state changed; refresh and retry."
        }
    }
    return "The supervisor command could not be completed."
}
```

- [ ] **Step 5: Write failing question-shape tests**

For each question method, assert the handler produces exactly one of:

```json
{"value":"selected-or-entered-text"}
{"confirmed":true}
{"confirmed":false}
{"cancelled":true}
```

Reject mismatched or multiple answer fields before manager invocation.

- [ ] **Step 6: Wire server-rendered Datastar action attributes**

New Session is fleet-level. Steer uses `$commandMessage`; Interrupt/Stop use `{}` payloads. Question buttons render concrete selected session/question routes and method-correct payloads. Remove the per-node Spawn Subagent control.

```html
<button data-on:click="@post('/ui/sessions', {payload:{}})">New Session</button>
<button data-on:click="@post('/ui/sessions/'+$selectedSessionId+'/steer', {payload:{message:$commandMessage}})">Steer</button>
<button data-on:click="@post('/ui/sessions/'+$selectedSessionId+'/interrupt', {payload:{}})">Interrupt Turn</button>
<button data-on:click="@post('/ui/sessions/'+$selectedSessionId+'/stop', {payload:{}})">Stop Session</button>
```

- [ ] **Step 7: Test persistent streams receive resulting state separately**

The action response contains only deck status. Publish a fake manager revision after the action and assert fleet/detail streams—not the action response—render lifecycle/activity changes.

- [ ] **Step 8: Run handler and browser-structure tests**

```bash
go test ./internal/server -run 'Test(Action|Steer|Interrupt|Stop|AnswerQuestion|DeckStatus|Datastar)' -count=1
go test -race ./internal/server ./internal/manager -count=1
```

Expected: PASS.

- [ ] **Step 9: Commit actions**

```bash
git add internal/server
git commit -m "feat: control supervisors from astrolabe"
```

---

### Task 10: Hermetic Restart and Rediscovery Integration

**Files:**
- Create: `internal/server/manager_integration_test.go`
- Modify: `internal/server/server_test.go`
- Modify: `internal/manager/testutil_test.go` only if a reusable fake root helper belongs in manager tests instead

**Interfaces:**
- Exercises: real Unix supervisor HTTP API, real manager discovery/monitoring, authenticated server handlers, and server restart without Incus.

- [ ] **Step 1: Build a persistent fake root service**

Use `supervisorapi.ServeUnix` with a fake `Service` backed by `supervisor.EventBroker`. Return a valid bound root tree, accept routed Pi calls, record question/stop actions, and keep serving while manager/server instances are closed. Implement every `supervisorapi.Service` method on the fixture; unsupported child/handoff operations return typed conflict errors rather than panicking.

```go
type persistentRootService struct {
    mu       sync.Mutex
    tree     supervisor.NodeSnapshot
    broker   *supervisor.EventBroker
    commands []json.RawMessage
    stopped  chan string
}

func (service *persistentRootService) Snapshot(context.Context) (supervisor.NodeSnapshot, error) {
    service.mu.Lock(); defer service.mu.Unlock()
    return cloneSnapshot(service.tree), nil
}
func (service *persistentRootService) Subscribe(context.Context) (supervisor.Subscription, error) {
    return service.broker.Subscribe(), nil
}
```

- [ ] **Step 2: Write the failing vertical test**

```go
func TestServerRestartRediscoversRunningRoot(t *testing.T) {
    // Start fake root at <temp>/existing.root.sock.
    // Start manager/server instance 1, bootstrap a cookie, and observe the root.
    // Publish events and verify fleet/detail patches.
    // Close server/manager instance 1 without stopping the fake root.
    // Start instance 2 against the same directory and bootstrap a new cookie.
    // Verify replay, route control, question answer, and explicit final stop.
}
```

Also verify child `.sock` entries never appear as independent roots and an initial replay beginning above sequence one displays incomplete history.

- [ ] **Step 3: Run the test and confirm the missing integration**

```bash
go test ./internal/server -run TestServerRestartRediscoversRunningRoot -count=1 -v
```

Expected before implementation adjustments: FAIL at the first unimplemented wiring or assertion.

- [ ] **Step 4: Complete the integration fixture through production seams**

Start each server instance with the production manager factory, a real TCP listener, and the production handler factory; only the root service is fake. Keep the root context separate from each server context so restart behavior is real.

```go
func startTestServer(t *testing.T, cfg config.Config, configPath string) *runningTestServer {
    t.Helper()
    ctx, cancel := context.WithCancel(context.Background())
    started := make(chan string, 1)
    result := make(chan error, 1)
    logger, _ := testLogger()
    output := newBootstrapCapture(started) // io.Writer that extracts the one printed bootstrap URL.
    go func() {
        result <- runApplication(ctx, cfg, Options{
            ListenAddress: "127.0.0.1:0", Logger: logger,
            BootstrapOutput: output, ConfigPath: configPath,
        }, productionManagerFactory, net.Listen)
    }()
    return &runningTestServer{cancel: cancel, result: result, bootstrapURL: <-started}
}
```

- [ ] **Step 5: Run the hermetic suite and race detector**

```bash
go test ./internal/server -run TestServerRestartRediscoversRunningRoot -count=5
go test -race ./internal/manager ./internal/server ./cmd -count=1
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the hermetic vertical slice**

```bash
git add internal/server/manager_integration_test.go internal/server/server_test.go internal/manager/testutil_test.go
git commit -m "test: cover server supervisor rediscovery"
```

---

### Task 11: Live Incus Acceptance and Final Verification

**Files:**
- Modify: `internal/supervisor/live_incus_test.go`
- Modify: `.env.example` only if the new live gate needs a documented variable not already present
- Modify: `Makefile` only if a distinct target is required to select the new live test

**Interfaces:**
- Exercises the approved real path: server spawn, supervisor buffering, server restart, rediscovery, Pi control, descendant stop, root stop, and Incus cleanup.

- [ ] **Step 1: Add an explicitly gated server-managed live test**

Reuse the existing authorization gates and live harness setup. Add:

```go
func TestLiveServerManagedSupervisorAcceptance(t *testing.T) {
    requireLiveSupervisorAuthorization(t)
    harness := newLiveAcceptance(t)
    defer harness.close()
    harness.runServerManaged()
}
```

Factor the existing prerequisite check into `requireLiveSupervisorAuthorization` so both live tests retain identical no-side-effect skip behavior.

- [ ] **Step 2: Create a run-local server config and start the built binary**

Copy the authorized config into the run directory and append explicit `[server]` and `[supervisor.events]` values pointing to private run-local socket/log directories. Start `kanedias server --listen 127.0.0.1:0`, parse the intentional bootstrap URL from its private log, and bootstrap an HTTP client with a cookie jar. Add concrete harness helpers `waitForBootstrapURL`, `bootstrapManagedServer`, and `postDatastar`; `postDatastar` sets the exact server Origin, `Sec-Fetch-Site: same-origin`, and `Content-Type: application/json`.

```go
server := h.startProcess("managed-server", h.binary, "--config", managedConfig, "server", "--listen", "127.0.0.1:0")
bootstrapURL := h.waitForBootstrapURL(filepath.Join(h.runDir, "managed-server.log"))
jar, err := cookiejar.New(nil)
if err != nil { h.t.Fatal(err) }
client := &http.Client{Jar: jar, Timeout: 30 * time.Second}
response, err := client.Get(bootstrapURL)
if err != nil { h.t.Fatal(err) }
_ = response.Body.Close()
```

- [ ] **Step 3: Exercise two server-spawned roots**

POST New Session twice, discover both root IDs from fleet output/tree sockets, prompt each through the Steer action while idle, and assert root event buffers advance even after closing the fleet/detail browser streams. Represent each observed root with `managedRoot{SessionID string, SocketPath string, PID int}` and implement `waitForManagedRoots` by scanning only `*.root.sock`, probing `/v1/tree`, and resolving the owning process from the server process tree before server shutdown.

```go
for range 2 {
    h.postDatastar(client, serverOrigin+"/ui/sessions", map[string]any{})
}
roots := h.waitForManagedRoots(2)
for _, root := range roots {
    h.postDatastar(client, serverOrigin+"/ui/sessions/"+url.PathEscape(root.SessionID)+"/steer", map[string]any{"message":"Reply with exactly MANAGED_ROOT_OK."})
}
```

- [ ] **Step 4: Prove non-destructive restart**

Send `SIGTERM` to the server, wait for server exit, and verify both root PIDs, sockets, snapshots, and Incus resources remain. Start a second server on the same config, perform the new bootstrap exchange, and assert both roots appear exactly once with retained replay or an honest gap marker.

```go
h.stopProcess(server, syscall.SIGTERM, 30*time.Second)
for _, root := range roots {
    h.assertProcessAlive(root.PID)
    h.snapshotRoot(root.SocketPath)
}
restarted := h.startProcess("managed-server-restart", h.binary, "--config", managedConfig, "server", "--listen", "127.0.0.1:0")
restartedClient := h.bootstrapManagedServer(restarted)
h.assertFleetContainsExactly(restartedClient, roots)
```

- [ ] **Step 5: Exercise real controls and cleanup**

Have one root create a controlled descendant. Add harness helpers `waitForManagedDescendant`, `actionURL`, `answerManagedQuestion`, `assertProcessAlive`, and `assertFleetContainsExactly`, each implemented with bounded polling and existing HTTP/Incus assertions. Then:

- Steer the running descendant with Pi `steer`;
- interrupt a turn with `abort`;
- answer the controlled blocking question;
- stop the descendant without stopping the root; and
- explicitly stop both roots through the UI.

Require all sockets/processes/owned instances/volumes to return to the exact baseline.

```go
descendant := h.waitForManagedDescendant(roots[0].SessionID)
h.postDatastar(restartedClient, actionURL(descendant.SessionID, "steer"), map[string]any{"message":"Focus on the acceptance marker."})
h.postDatastar(restartedClient, actionURL(descendant.SessionID, "interrupt"), map[string]any{})
h.answerManagedQuestion(restartedClient, descendant.SessionID, "deterministic-answer")
h.postDatastar(restartedClient, actionURL(descendant.SessionID, "stop"), map[string]any{})
for _, root := range roots { h.postDatastar(restartedClient, actionURL(root.SessionID, "stop"), map[string]any{}) }
h.assertBaseline("after-server-managed-roots")
```

- [ ] **Step 6: Run the live test twice**

```bash
KANEDIAS_LIVE_SUPERVISOR=1 \
KANEDIAS_CONFIG=./config.toml \
KANEDIAS_E2E_PROVIDER_READY=1 \
KANEDIAS_E2E_DISPOSABLE_GITHUB=1 \
KANEDIAS_E2E_GITHUB_REPOSITORY=owner/disposable-repository \
KANEDIAS_E2E_GITHUB_REMOTE=https://github.com/owner/disposable-repository.git \
go test -tags=incus ./internal/supervisor \
  -run TestLiveServerManagedSupervisorAcceptance -v -count=2
```

Expected: PASS twice with exact baseline restoration. Without every authorization variable, the test must skip before build, bind, Incus, provider, or GitHub side effects.

- [ ] **Step 7: Run final static, race, and full hermetic validation**

```bash
files=$(gofmt -l .); test -z "$files" || { printf '%s\n' "$files"; exit 1; }
golangci-lint run ./...
go test -race ./internal/config ./internal/supervisor ./internal/supervisorapi ./internal/manager ./internal/server ./cmd -count=1
go test ./... -count=1
go build ./...
```

Expected: all commands exit 0.

- [ ] **Step 8: Review the final diff against every acceptance criterion**

Confirm in the diff and test evidence:

```text
root-only discovery
configured independent replay limits
one manager stream per root
sequence deduplication and visible gaps
independent tree polling
spawn admission and reaping
non-destructive server shutdown
exact Pi steer/prompt/abort semantics
explicit subtree/root stop
method-correct question answers
capability + same-origin boundary
dynamic supported-only Astrolabe values
server restart and rediscovery
explicit final resource cleanup
```

- [ ] **Step 9: Commit live acceptance and final adjustments**

```bash
git add internal/supervisor/live_incus_test.go .env.example Makefile
git commit -m "test: prove server managed supervisor lifecycle"
```

Only include `.env.example` or `Makefile` in the commit when their contents actually changed.
