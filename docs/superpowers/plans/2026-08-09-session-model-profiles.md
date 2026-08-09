# Per-Session Model Profiles Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an allowlisted model chooser for new root sessions, per-session root and worker model/thinking selections, and immutable profile inheritance across every nested clone.

**Architecture:** `config.toml` defines model types and default selections, while `internal/config` resolves them into one credential-free `SessionModelPolicy`. The manager validates browser selections before side effects and transfers the resolved policy to a new root over a strict inherited JSON pipe; the root starts Pi on the selected model and embeds the complete policy in every child bootstrap. The server renders an accessible modal from the manager's read-only launch options, and Pi `get_state` remains the authoritative effective-model display.

**Tech Stack:** Go 1.24, Cobra, Incus Go client, `html/template`, Datastar fleet streams, vanilla browser JavaScript with Node's built-in test runner, Bash launcher tests.

## Global Constraints

- Session choices are ephemeral; they never rewrite global defaults or create a durable per-session configuration file.
- Browser requests contain allowlisted model type IDs and thinking levels only—never raw providers, model IDs, descriptions, or credentials.
- Worker names and descriptions remain administrator-owned global policy.
- A launch request contains every configured worker exactly once; missing, duplicate, and unknown workers fail.
- Manager validation and policy resolution complete before token generation, log creation, pipe creation, socket naming, process start, or any Incus operation.
- Root Pi starts with the selected provider/model/thinking flags; there is no launch-then-switch RPC window.
- Every node owns a cloned, immutable copy of the complete root policy and passes it unchanged to descendants.
- A live tree does not re-resolve model choices from later `config.toml` edits.
- Root, fresh-child, and fork-child binding all compare Pi's effective provider/model/thinking with the expected profile.
- Existing fork session ID/file checks, worker-type delegation API, authenticated-console option, same-origin write boundary, cleanup ordering, and fleet streaming remain intact.
- Do not run destructive Incus tests without the existing explicit authorization environment.
- Baseline on `feat/session-model-profiles` at `e1d18be`: `go mod download` and `go test ./...` pass.

---

## File Structure

### New files

- `internal/config/model_policy.go` — resolved model/worker policy values, structural validation, cloning, and default resolution.
- `internal/manager/launch.go` — browser launch request types, immutable launch catalog, default launch view, and request-to-policy resolution.
- `internal/manager/launch_test.go` — exact worker-set, allowlist, thinking-level, and copy-isolation tests.
- `internal/supervisor/process/root_bootstrap.go` — strict bounded root-policy bootstrap encoder/decoder.
- `internal/supervisor/process/root_bootstrap_test.go` — root bootstrap unknown-field, oversize, and policy-validation tests.
- `internal/server/web/session-modal.html` — server-rendered accessible New Session dialog.
- `internal/server/web/session-modal.js` — modal state, model-aware thinking choices, request construction, and pending/error behavior.
- `internal/server/web/session-modal.test.js` — Node tests for modal request/state decisions.

### Modified files

- `internal/config/config.go`, `internal/config/config_test.go`, `config.toml` — model catalog and default references.
- `internal/supervisor/contract/types.go` — alias API `ModelProfile` to the canonical config profile.
- `internal/manager/types.go`, `internal/manager/manager.go`, `internal/manager/spawn.go`, manager tests — launch configuration, request validation, and root bootstrap FD lifecycle.
- `cmd/session.go`, `cmd/session_runtime.go`, `cmd/session_test.go`, `cmd/session_runtime_test.go` — optional root bootstrap FD, direct-default policy, and policy-backed root/child runtimes.
- `internal/supervisor/identity.go`, `internal/supervisor/node.go`, `internal/supervisor/local.go`, supervisor tests — immutable policy catalog and exact effective-model binding.
- `internal/supervisor/process/protocol.go`, `internal/supervisor/process/process_test.go` — full child policy instead of a duplicated selected worker.
- `internal/supervisor/provision/types.go`, `internal/supervisor/provision/root.go`, provisioning tests — selected root model environment.
- `internal/image/kanedias-pi-rpc`, `internal/image/image_test.go` — root model flags.
- `internal/server/server.go`, `internal/server/handler.go`, `internal/server/actions.go`, `internal/server/signals.go`, `internal/server/view.go`, server tests/fakes — launch options, strict direct JSON POST, modal rendering, and effective model details.
- `internal/server/web/index.html`, `internal/server/web/detail.html`, `internal/server/web/app.css`, `internal/server/web/app.js` — modal shell and effective model display without regressing the markdown/terminal controls from `6acd91e`.
- `internal/supervisor/live_incus_test.go` — use configured default launch selections when exercising `POST /ui/sessions`.

---

### Task 1: Add the canonical model policy and configuration schema

**Files:**
- Create: `internal/config/model_policy.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/config/server_test.go`
- Modify: `internal/supervisor/contract/types.go`
- Modify: `cmd/session_runtime.go`
- Modify: `cmd/session_runtime_test.go`
- Modify: tests that construct `config.Config.Workers`
- Modify: `config.toml`

**Interfaces:**
- Produces:
  - `config.ModelDefinition`
  - `config.SessionDefaults`
  - `config.WorkerDefaults`
  - `config.ModelProfile`
  - `config.WorkerProfile`
  - `config.SessionModelPolicy`
  - `Config.ResolveModel(modelType, thinkingLevel string) (ModelProfile, error)`
  - `Config.ResolveWorker(name string) (WorkerProfile, error)`
  - `Config.DefaultSessionModelPolicy() (SessionModelPolicy, error)`
  - `SessionModelPolicy.Clone() SessionModelPolicy`
  - `SessionModelPolicy.ResolveWorker(name string) (WorkerProfile, error)`
- Consumers: manager launch resolution, supervisor dependencies, root/child process protocols, provisioning, and the existing worker summary endpoint.

- [ ] **Step 1: Write failing configuration and policy tests**

Add table tests that cover slug validation, duplicate provider/model pairs, invalid and duplicate thinking sets, default thinking membership, unknown model references, unsupported default thinking, deterministic worker names, and deep-copy isolation:

```go
func TestDefaultSessionModelPolicyResolvesCatalogReferences(t *testing.T) {
    cfg := modelConfigFixture()
    policy, err := cfg.DefaultSessionModelPolicy()
    if err != nil { t.Fatal(err) }
    if policy.Root != (ModelProfile{Provider: "local-executor", Model: "Qwen3.6-27B-GGUF", ThinkingLevel: "off"}) {
        t.Fatalf("root = %#v", policy.Root)
    }
    reviewer, err := policy.ResolveWorker("reviewer")
    if err != nil { t.Fatal(err) }
    if reviewer.Model != "gpt-5.6-sol" || reviewer.ThinkingLevel != "xhigh" {
        t.Fatalf("reviewer = %#v", reviewer)
    }
}

func TestSessionModelPolicyCloneDoesNotAliasWorkers(t *testing.T) {
    original, err := modelConfigFixture().DefaultSessionModelPolicy()
    if err != nil { t.Fatal(err) }
    cloned := original.Clone()
    worker := cloned.Workers["reviewer"]
    worker.Model = "mutated"
    cloned.Workers["reviewer"] = worker
    if original.Workers["reviewer"].Model == "mutated" {
        t.Fatal("Clone retained the workers map")
    }
}
```

- [ ] **Step 2: Run the focused tests and confirm RED**

Run:

```bash
go test ./internal/config ./internal/supervisor/contract ./cmd -run 'Model|Worker|Supervisor' -count=1
```

Expected: compile failures for missing model catalog/default/policy types and old worker fields.

- [ ] **Step 3: Implement the policy types in `internal/config/model_policy.go`**

Use one canonical runtime representation:

```go
type ModelProfile struct {
    Provider      string `json:"provider"`
    Model         string `json:"model"`
    ThinkingLevel string `json:"thinkingLevel"`
}

type WorkerProfile struct {
    Description   string `json:"description"`
    Provider      string `json:"provider"`
    Model         string `json:"model"`
    ThinkingLevel string `json:"thinkingLevel"`
}

type SessionModelPolicy struct {
    Root    ModelProfile             `json:"root"`
    Workers map[string]WorkerProfile `json:"workers"`
}

func (p SessionModelPolicy) Clone() SessionModelPolicy {
    out := p
    out.Workers = make(map[string]WorkerProfile, len(p.Workers))
    for name, worker := range p.Workers { out.Workers[name] = worker }
    return out
}
```

Add `Validate`, sorted `WorkerNames`, and `ResolveWorker` methods. Require nonempty provider/model, valid global thinking levels, nonempty descriptions, and at least one worker.

- [ ] **Step 4: Replace raw worker provider/model TOML with model type references**

Define:

```go
type ModelDefinition struct {
    Label                string   `toml:"label"`
    Provider             string   `toml:"provider"`
    Model                string   `toml:"model"`
    ThinkingLevels       []string `toml:"thinking_levels"`
    DefaultThinkingLevel string   `toml:"default_thinking_level"`
}

type SessionDefaults struct {
    ModelType    string `toml:"model_type"`
    ThinkingLevel string `toml:"thinking_level"`
}

type WorkerDefaults struct {
    Description   string `toml:"description"`
    ModelType     string `toml:"model_type"`
    ThinkingLevel string `toml:"thinking_level"`
}
```

Update `Config` to hold `Models map[string]ModelDefinition`, `Session SessionDefaults`, and `Workers map[string]WorkerDefaults`. Validate model type IDs with `^[a-z0-9][a-z0-9-]{0,62}$`, require unique provider/model pairs, and make `ValidateSupervisor` call `DefaultSessionModelPolicy` after lifecycle/event validation.

- [ ] **Step 5: Preserve the API model type and migrate callers**

In `internal/supervisor/contract/types.go` use:

```go
type ModelProfile = config.ModelProfile
```

Keep `Config.ResolveWorker` returning the resolved runtime `config.WorkerProfile`. Update `configWorkerCatalog.Summaries` to call `ResolveWorker` instead of reading raw defaults. Mechanically update test fixtures from `map[string]config.WorkerProfile` to `map[string]config.WorkerDefaults` plus matching `Models` and `Session` entries.

- [ ] **Step 6: Migrate committed defaults**

Add `local-qwen` and `gpt-5-6-sol` under `[models.*]`, add `[session]`, and change each `[workers.*]` block to `model_type = "gpt-5-6-sol"`. Use `off` as the local model's sole thinking level and retain current worker thinking defaults.

- [ ] **Step 7: Run focused and package-wide tests**

Run:

```bash
go test ./internal/config ./internal/supervisor/contract ./cmd -count=1
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/config internal/supervisor/contract/types.go cmd/session_runtime.go cmd/session_runtime_test.go config.toml
git add $(git diff --name-only -- '*_test.go')
git commit -m "feat: add model policy configuration"
```

---

### Task 2: Resolve allowlisted launch requests in the manager

**Files:**
- Create: `internal/manager/launch.go`
- Create: `internal/manager/launch_test.go`
- Modify: `internal/manager/types.go`
- Modify: `internal/manager/manager.go`
- Modify: `internal/manager/testutil_test.go`
- Modify: `internal/server/server.go`
- Modify: `internal/server/server_test.go`

**Interfaces:**
- Consumes: Task 1's `config.ModelDefinition`, defaults, and `SessionModelPolicy`.
- Produces:
  - `manager.ModelSelection`
  - `manager.WorkerModelSelection`
  - `manager.SessionLaunchRequest`
  - `manager.ModelLaunchOption`
  - `manager.WorkerLaunchOption`
  - `manager.SessionLaunchOptions`
  - `manager.NewLaunchConfiguration(config.Config) (LaunchConfiguration, error)`
  - `(*Manager).LaunchOptions() SessionLaunchOptions`
  - private `LaunchConfiguration.Resolve(SessionLaunchRequest) (config.SessionModelPolicy, error)`

- [ ] **Step 1: Write failing launch resolution tests**

Cover a valid custom request, exact worker-set enforcement, unknown models, unsupported thinking, deterministic option ordering, and returned-value copy isolation:

```go
func TestLaunchConfigurationRequiresEveryWorkerExactlyOnce(t *testing.T) {
    launch := mustLaunchConfiguration(t, modelConfigFixture())
    request := launch.DefaultRequest()
    request.Workers = append(request.Workers, request.Workers[0])
    _, err := launch.Resolve(request)
    var typed *contract.Error
    if !errors.As(err, &typed) || typed.Code != contract.ErrorInvalidRequest {
        t.Fatalf("error = %v", err)
    }
}

func TestLaunchConfigurationResolveReturnsIndependentPolicy(t *testing.T) {
    launch := mustLaunchConfiguration(t, modelConfigFixture())
    first, err := launch.Resolve(launch.DefaultRequest())
    if err != nil { t.Fatal(err) }
    first.Workers["reviewer"] = config.WorkerProfile{Description: "changed", Provider: "x", Model: "y", ThinkingLevel: "off"}
    second, err := launch.Resolve(launch.DefaultRequest())
    if err != nil { t.Fatal(err) }
    if second.Workers["reviewer"].Provider == "x" { t.Fatal("launch policy aliased prior result") }
}
```

- [ ] **Step 2: Run focused tests and confirm RED**

```bash
go test ./internal/manager -run 'Launch|Policy' -count=1
```

Expected: compile failure for missing launch types.

- [ ] **Step 3: Implement the wire request and read-only view**

Use a worker slice on the wire:

```go
type ModelSelection struct {
    ModelType    string `json:"modelType"`
    ThinkingLevel string `json:"thinkingLevel"`
}

type WorkerModelSelection struct {
    WorkerType   string `json:"workerType"`
    ModelType    string `json:"modelType"`
    ThinkingLevel string `json:"thinkingLevel"`
}

type SessionLaunchRequest struct {
    Root    ModelSelection         `json:"root"`
    Workers []WorkerModelSelection `json:"workers"`
}
```

`SessionLaunchOptions` contains sorted model options, the default root selection, and sorted worker rows with descriptions and defaults. Return copied slices from `LaunchOptions` and `DefaultRequest`.

- [ ] **Step 4: Implement exact validation and resolution**

Build private maps once in `NewLaunchConfiguration`. In `Resolve`, reject empty/unknown model types, unsupported thinking levels, missing/duplicate/unknown workers, and any role-set mismatch with `contract.ErrorInvalidRequest`. Resolve only administrator-owned provider/model/description values into `config.SessionModelPolicy` and call `policy.Validate()` before returning `policy.Clone()`.

- [ ] **Step 5: Inject the launch configuration into the manager**

Add `Launch LaunchConfiguration` to `manager.Options`, validate it in `manager.New`, store it on `Manager`, and expose `LaunchOptions()`. In `server.runApplication`, construct it before `manager.New`:

```go
launch, err := manager.NewLaunchConfiguration(cfg)
if err != nil { return fmt.Errorf("run server: model launch configuration: %w", err) }
// manager.Options{Launch: launch, ...}
```

Update manager and server fakes with a valid fixture; do not permit zero launch configuration in production construction.

- [ ] **Step 6: Run tests**

```bash
go test ./internal/manager ./internal/server -run 'Launch|RunApplication|NewManager' -count=1
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/manager/launch.go internal/manager/launch_test.go internal/manager/types.go internal/manager/manager.go internal/manager/testutil_test.go internal/server/server.go internal/server/server_test.go
git commit -m "feat: resolve session launch profiles"
```

---

### Task 3: Transfer the policy to root supervisors through an inherited FD

**Files:**
- Create: `internal/supervisor/process/root_bootstrap.go`
- Create: `internal/supervisor/process/root_bootstrap_test.go`
- Modify: `internal/manager/spawn.go`
- Modify: `internal/manager/spawn_test.go`
- Modify: `cmd/session.go`
- Modify: `cmd/session_test.go`
- Modify: `cmd/session_runtime.go`
- Modify: `cmd/session_runtime_test.go`

**Interfaces:**
- Consumes: Task 2's launch request resolver.
- Produces:
  - `process.RootBootstrap{Policy config.SessionModelPolicy}`
  - `process.EncodeRootBootstrap(io.Writer, RootBootstrap) error`
  - `process.DecodeRootBootstrap(io.Reader) (RootBootstrap, error)`
  - `Manager.SpawnRoot(context.Context) (string, error)` as the configured-default convenience path.
  - `Manager.SpawnRootWithRequest(context.Context, SessionLaunchRequest) (string, error)` as the validated custom path.
  - `SessionOptions.Policy config.SessionModelPolicy`
  - hidden root `session --bootstrap-fd <n>` support.

- [ ] **Step 1: Write failing strict root-bootstrap tests**

```go
func TestRootBootstrapStrictBoundedPolicy(t *testing.T) {
    bootstrap := RootBootstrap{Policy: validPolicy(t)}
    var wire bytes.Buffer
    if err := EncodeRootBootstrap(&wire, bootstrap); err != nil { t.Fatal(err) }
    got, err := DecodeRootBootstrap(&wire)
    if err != nil { t.Fatal(err) }
    if !reflect.DeepEqual(got.Policy, bootstrap.Policy) { t.Fatalf("got %#v", got) }

    raw, _ := json.Marshal(bootstrap)
    raw = append(raw[:len(raw)-1], []byte(`,"unknown":true}`)...)
    if _, err := DecodeRootBootstrap(bytes.NewReader(raw)); err == nil || !strings.Contains(err.Error(), "unknown field") {
        t.Fatalf("unknown field error = %v", err)
    }
}
```

Also test oversize input and structurally invalid policies.

- [ ] **Step 2: Write failing manager ordering/FD tests**

Extend `fakeStarter` so its `Start` method records and reads `spawnSpec.ExtraFiles[0]`. Assert:

- invalid requests do not call `generateToken`, create a log, open a pipe, or call `Start`;
- valid argv ends with `session --socket <path> --bootstrap-fd 3`;
- argv and environment do not contain provider/model/policy JSON;
- the inherited record decodes to the resolved policy;
- parent pipe endpoints close on start failure and bootstrap-write failure.

Add `newSpawnToken func() (string, error)` and `newBootstrapPipe func() (*os.File, *os.File, error)` dependencies to `Manager`, initialize them to `generateToken` and `os.Pipe` in `New`, and set them explicitly in `fakeManager`. Tests replace both with counters so zero-side-effect ordering is deterministic rather than inferred from the filesystem.

- [ ] **Step 3: Run focused tests and confirm RED**

```bash
go test ./internal/supervisor/process ./internal/manager ./cmd -run 'RootBootstrap|RootSpawner|SpawnRoot|Session' -count=1
```

Expected: missing root bootstrap, `ExtraFiles`, request parameter, and CLI flag failures.

- [ ] **Step 4: Implement the root bootstrap protocol**

Reuse the existing `MaxRecordBytes` and strict decoder behavior. `EncodeRootBootstrap` validates a cloned policy, marshals exactly one JSON value, and never writes a trailing second value. `DecodeRootBootstrap` rejects unknown fields, trailing values, oversize records, and invalid policies.

- [ ] **Step 5: Validate before manager side effects, then transfer through fd 3**

Change `spawnSpec` and the OS starter:

```go
type spawnSpec struct {
    // existing fields...
    ExtraFiles []*os.File
}
// cmd.ExtraFiles = spec.ExtraFiles
```

Implement `SpawnRootWithRequest` so its first operation is `m.launch.Resolve(request)`, before token generation. Create a pipe only after successful resolution, pass its read endpoint as `ExtraFiles[0]`, write the bounded bootstrap after process start, and close both parent endpoints on every path. Mirror the child `process.Spawner` write-goroutine and cancellation discipline; do not let failed admission own the already-consumed bootstrap FD.

Keep `SpawnRoot(ctx)` as a thin default convenience method:

```go
func (m *Manager) SpawnRoot(ctx context.Context) (string, error) {
    return m.SpawnRootWithRequest(ctx, m.launch.DefaultRequest())
}
```

This preserves a working default New Session action until Task 6 atomically replaces the browser route and modal.

- [ ] **Step 6: Add root CLI bootstrap handling and direct-default behavior**

Add hidden `--bootstrap-fd` defaulting to `-1`. When present, wrap the exact descriptor, mark it close-on-exec, decode once, close it, and pass the policy via `SessionOptions`. When absent, call `cfg.DefaultSessionModelPolicy()`. Keep stdin untouched in both paths.

```go
type SessionOptions struct {
    SocketPath string
    ConfigPath string
    Policy     config.SessionModelPolicy
}
```

Validate and clone the policy at the start of `runSupervisorWithBrokerFactory`.

- [ ] **Step 7: Run focused and full tests**

```bash
go test ./internal/supervisor/process ./internal/manager ./cmd -count=1
go test ./... -count=1
```

Expected: PASS, including the existing default New Session backend path and spawn cleanup tests.

- [ ] **Step 8: Commit**

```bash
git add internal/supervisor/process/root_bootstrap.go internal/supervisor/process/root_bootstrap_test.go internal/manager/spawn.go internal/manager/spawn_test.go cmd/session.go cmd/session_test.go cmd/session_runtime.go cmd/session_runtime_test.go
git commit -m "feat: bootstrap root model policies"
```

---

### Task 4: Start and bind root Pi on the selected model

**Files:**
- Modify: `internal/supervisor/identity.go`
- Modify: `internal/supervisor/node.go`
- Modify: `internal/supervisor/node_test.go`
- Modify: `internal/supervisor/local.go`
- Modify: `internal/supervisor/local_test.go`
- Modify: `internal/supervisor/read_result_test.go`
- Modify: `internal/supervisor/provision/types.go`
- Modify: `internal/supervisor/provision/root.go`
- Modify: `internal/supervisor/provision/root_test.go`
- Modify: `internal/image/kanedias-pi-rpc`
- Modify: `internal/image/image_test.go`
- Modify: `cmd/session_runtime.go`

**Interfaces:**
- Consumes: root `SessionOptions.Policy`.
- Produces:
  - `Dependencies.ModelPolicy config.SessionModelPolicy`
  - existing `Dependencies.ExpectedPiBinding *PiBinding` retained for fork identity/file checks.
  - `RootRequest.Model config.ModelProfile`
  - `PiExpectation{Binding *PiBinding, Model config.ModelProfile}` built by `Node.Start` from the node identity and private policy.
  - exact model verification in `LocalSession.BindExpected`.

- [ ] **Step 1: Write failing root provisioning and launcher tests**

Change the expected root environment to nonempty values:

```go
request := RootRequest{
    SessionID: "root-1",
    SocketPath: "/tmp/root.sock",
    Model: config.ModelProfile{Provider: "openai-codex", Model: "gpt-5.6-sol", ThinkingLevel: "high"},
}
```

Assert exact `KANEDIAS_PI_*` values. In `image_test.go`, add root cases expecting:

```text
--mode rpc -e /opt/kanedias/pi-extension/src/index.ts --provider openai-codex --model gpt-5.6-sol --thinking high
```

and failures for missing root provider/model and invalid thinking.

- [ ] **Step 2: Write failing exact model-binding tests**

Feed `get_state` model/thinking values and table-test provider, model, and thinking mismatches:

```go
expectation := PiExpectation{Model: config.ModelProfile{
    Provider: "openai-codex", Model: "gpt-5.6-sol", ThinkingLevel: "high",
}}
if err := local.BindExpected(context.Background(), expectation); !errors.Is(err, ErrInvariant) {
    t.Fatalf("mismatch error = %v", err)
}
```

Retain a fork case with both `Binding` and `Model` so session ID/file mismatch behavior remains covered.

- [ ] **Step 3: Run focused tests and confirm RED**

```bash
go test ./internal/supervisor/provision ./internal/image ./internal/supervisor -run 'Root|Bind|Model|Fork' -count=1
```

- [ ] **Step 4: Make the immutable policy the node's worker catalog**

Replace `Dependencies.Workers WorkerCatalog` with `Dependencies.ModelPolicy config.SessionModelPolicy`. Validate and clone it in `newNode`. Resolve workers and produce sorted `WorkerSummary` values directly from the node's private policy. This removes the risk of retaining a mutable external worker map.

- [ ] **Step 5: Propagate and validate the root model**

`Node.Start` passes `node.deps.ModelPolicy.Root` in `provision.RootRequest`. The root provisioner rejects empty/invalid model profiles before connecting to Incus and writes provider/model/thinking into instance environment.

Update the launcher so both root and child paths require provider/model, append `--provider`/`--model`, validate optional thinking through the existing case list, and append `--thinking` when present. Root still rejects a session file; child still loads fork session files.

- [ ] **Step 6: Verify Pi's effective model during binding**

Introduce:

```go
type PiExpectation struct {
    Binding *PiBinding
    Model   config.ModelProfile
}
```

`BindExpected` validates the model profile, performs current optional fork binding checks, derives `modelFromGetState`, and compares all three fields before storing the binding/model or transitioning to ready. `Node.Start` always supplies the root or selected worker expectation.

- [ ] **Step 7: Run tests**

```bash
go test ./internal/supervisor/provision ./internal/image ./internal/supervisor ./cmd -count=1
go test ./... -count=1
```

- [ ] **Step 8: Commit**

```bash
git add internal/supervisor/identity.go internal/supervisor/node.go internal/supervisor/node_test.go internal/supervisor/local.go internal/supervisor/local_test.go internal/supervisor/read_result_test.go internal/supervisor/provision internal/image/kanedias-pi-rpc internal/image/image_test.go cmd/session_runtime.go
git commit -m "feat: bind roots to selected models"
```

---

### Task 5: Inherit the complete immutable policy through every clone

**Files:**
- Modify: `internal/supervisor/process/protocol.go`
- Modify: `internal/supervisor/process/process_test.go`
- Modify: `internal/supervisor/process/spawn.go`
- Modify: `internal/supervisor/node.go`
- Modify: `internal/supervisor/children_test.go`
- Modify: `internal/supervisor/ordering_integration_test.go`
- Modify: `cmd/session_runtime.go`
- Modify: `cmd/session_runtime_test.go`
- Modify: `internal/supervisor/provision/child_test.go`

**Interfaces:**
- Consumes: node-private `SessionModelPolicy` and existing child bootstrap transport.
- Produces: `process.Bootstrap.Policy config.SessionModelPolicy`; removes `process.Bootstrap.Worker`.

- [ ] **Step 1: Write failing child protocol and inheritance tests**

Update `validBootstrap` to include the complete policy. Assert strict decode keeps all workers and rejects invalid policy structure. Add a child creation test that captures two generations:

```go
var childBootstrap process.Bootstrap
node := childCreationNodeWithPolicy(t, policy, func(_ context.Context, got process.Bootstrap) (ChildProcess, error) {
    childBootstrap = got
    return terminalChild(t), nil
})
// create reviewer child
if !reflect.DeepEqual(childBootstrap.Policy, policy) { t.Fatalf("policy changed: %#v", childBootstrap.Policy) }
childBootstrap.Policy.Workers["reviewer"] = mutatedWorker
if reflect.DeepEqual(node.deps.ModelPolicy, childBootstrap.Policy) { t.Fatal("bootstrap aliases node policy") }
```

Add a nested-node test whose infrastructure config defaults are changed after root admission; creating a grandchild must still emit the original policy.

- [ ] **Step 2: Write a failing child-runner policy authority test**

Create a valid current `config.toml` whose worker defaults point at a different model than `bootstrap.Policy`. Inject the broker sentinel after policy resolution and assert the child accepts the inherited policy and selects `bootstrap.Policy.Workers[request.WorkerType]`, not `cfg.ResolveWorker`.

- [ ] **Step 3: Run focused tests and confirm RED**

```bash
go test ./internal/supervisor/process ./internal/supervisor ./cmd -run 'Bootstrap|Policy|Child|Grandchild' -count=1
```

- [ ] **Step 4: Replace the duplicated worker field with the complete policy**

Change `process.Bootstrap`:

```go
type Bootstrap struct {
    // identity/source fields...
    Policy         config.SessionModelPolicy  `json:"policy"`
    Request        contract.CreateChildRequest `json:"request"`
    RunAttribution string                       `json:"runAttribution,omitempty"`
}
```

`Bootstrap.Validate` validates the complete policy and requires `Policy.ResolveWorker(Request.WorkerType)` to succeed. Remove `validateWorker` and the independently mutable `Worker` field.

- [ ] **Step 5: Clone policy at every process boundary**

`Node.CreateChild` resolves the selected worker for fallback display, then sets `Policy: node.deps.ModelPolicy.Clone()`. `productionChildRunner` validates/clones `bootstrap.Policy`, resolves the selected worker from it, uses it in `provision.ChildRequest`, and constructs the nested node with that same cloned policy. Do not compare against current global worker defaults.

Keep loading and validating global configuration for infrastructure, event limits, repositories, proxy, and Incus settings.

- [ ] **Step 6: Run recursive tests and regression suite**

```bash
go test ./internal/supervisor/process ./internal/supervisor ./internal/supervisor/provision ./cmd -count=1
go test ./... -count=1
```

- [ ] **Step 7: Commit**

```bash
git add internal/supervisor/process internal/supervisor/node.go internal/supervisor/children_test.go internal/supervisor/ordering_integration_test.go internal/supervisor/provision/child_test.go cmd/session_runtime.go cmd/session_runtime_test.go
git commit -m "feat: inherit model policy through clones"
```

---

### Task 6: Render the launch modal and effective model details

**Files:**
- Create: `internal/server/web/session-modal.html`
- Modify: `internal/server/server.go`
- Modify: `internal/server/handler.go`
- Modify: `internal/server/actions.go`
- Modify: `internal/server/signals.go`
- Modify: `internal/server/view.go`
- Modify: `internal/server/web/index.html`
- Modify: `internal/server/web/detail.html`
- Modify: `internal/server/handler_test.go`
- Modify: `internal/server/actions_test.go`
- Modify: `internal/server/questions_render_test.go`
- Modify: `internal/server/server_test.go`
- Modify: `internal/server/manager_integration_test.go`

**Interfaces:**
- Consumes: manager `LaunchOptions` and `SpawnRootWithRequest` from Tasks 2–3.
- Produces: strict direct-JSON launch POST; `indexView`, `sessionModalView`, `modelOptionView`, `workerOptionView`; stable DOM IDs `new-session-modal`, `new-session-form`, `new-session-status`, `new-session-launch`, and `new-session-cancel`.

- [ ] **Step 1: Write failing server-rendering tests**

Construct launch options with two models and three workers. Assert the authenticated index contains:

- a closed `<dialog id="new-session-modal">`;
- root selectors with configured selections;
- each worker exactly once and HTML-escaped descriptions;
- model type/label/supported thinking metadata but no provider or raw model ID;
- expandable Subagent model profiles section;
- accessible title, labels, Cancel, close, Launch, and `aria-live` status;
- the New Session button opens the modal rather than posting an empty request.

Add detail rendering tests asserting `state.Node.Model` produces provider/model/thinking metrics.

- [ ] **Step 2: Run focused tests and confirm RED**

```bash
go test ./internal/server -run 'InitialPage|SessionModal|NewSession|Detail.*Model' -count=1
```

- [ ] **Step 3: Replace the default-only browser action with a strict direct-JSON launch route**

Change `fleetManager` to expose `LaunchOptions()` and `SpawnRootWithRequest(context.Context, manager.SessionLaunchRequest)`. Add `decodeJSON[T]` beside `decodeSignals[T]`, using the same 64 KiB body limit, `DisallowUnknownFields`, and trailing-value rejection. `makeNewSessionHandler` decodes `manager.SessionLaunchRequest`, calls `SpawnRootWithRequest`, logs the real error, and returns sanitized JSON:

```json
{"sessionId":"session-..."}
```

with `201 Created`, or:

```json
{"error":"The session configuration was not valid."}
```

with `400` for decode/invalid request and `503` for admission failure. Keep the same-origin middleware and authentication route grouping unchanged.

- [ ] **Step 4: Add focused view types and a separate modal template**

`newIndexView(fleet.LaunchOptions())` precomputes sorted rows and selected attributes. Each model option exposes only:

```go
type modelOptionView struct {
    ID                    string
    Label                 string
    ThinkingLevelsCSV     string
    DefaultThinkingLevel  string
    Selected              bool
}
```

Each worker row contains role, escaped description, model options, and thinking options. Do not place providers/model IDs in HTML or data attributes.

- [ ] **Step 5: Render the index with launch data**

Change `serveIndex` from `ExecuteTemplate(..., nil)` to `newIndexView(fleet.LaunchOptions())`, with an empty disabled launch view only for the nil-fleet test handler. Include `session-modal.html` in `parseTemplates`, and invoke it from `index.html` below the app shell.

- [ ] **Step 6: Add effective model metrics**

Extend `detailView` with `Provider`, `Model`, and `ThinkingLevel` copied from `state.Node.Model`. Render `—` only for pre-binding/empty values. These values must come from the snapshot populated by `get_state`, not launch options.

- [ ] **Step 7: Update backend action tests and integration fakes**

Have test fleets record the exact `SessionLaunchRequest`. Test valid `201` JSON, malformed/unknown-field `400`, invalid-request sanitized copy, spawn `503`, server-side real-error logging, authentication, and same-origin rejection. Preserve fleet/detail SSE tests and all six upstream markdown/terminal asset assertions.

- [ ] **Step 8: Run tests**

```bash
go test ./internal/server -count=1
go test ./... -count=1
```

- [ ] **Step 9: Commit**

```bash
git add internal/server/server.go internal/server/handler.go internal/server/actions.go internal/server/signals.go internal/server/view.go internal/server/web/index.html internal/server/web/detail.html internal/server/web/session-modal.html internal/server/*_test.go
git commit -m "feat: render session model chooser"
```

---

### Task 7: Add accessible modal behavior and complete verification

**Files:**
- Create: `internal/server/web/session-modal.js`
- Create: `internal/server/web/session-modal.test.js`
- Modify: `internal/server/web/index.html`
- Modify: `internal/server/web/app.js`
- Modify: `internal/server/web/app.css`
- Modify: `internal/server/handler.go`
- Modify: `internal/server/handler_test.go`
- Modify: `internal/supervisor/live_incus_test.go`

**Interfaces:**
- Consumes: stable modal DOM and direct JSON endpoint from Task 6.
- Produces: `window.KanediasSessionModal` UMD module with exported pure decisions plus `bind(document, fetch)`.

- [ ] **Step 1: Write failing Node tests for modal decisions**

Test:

- opening resets configured defaults and focuses the root model;
- model changes replace thinking choices and clamp unsupported values to `data-default-thinking`;
- a one-level model disables but displays its thinking selector;
- `buildRequest` returns root plus every worker exactly once;
- Launch disables controls while pending;
- failed HTTP keeps the modal open, restores controls, and displays returned sanitized text;
- successful `201` closes and resets the modal;
- Cancel, close, backdrop policy, and Escape close without fetch;
- Escape is consumed while the dialog is open so the terminal interrupt shortcut does not fire.

Use small fake select/dialog/fetch objects; do not require jsdom.

- [ ] **Step 2: Run the JS test and confirm RED**

```bash
node --test internal/server/web/session-modal.test.js
```

Expected: module-not-found failure.

- [ ] **Step 3: Implement the UMD modal controller**

Follow the existing `terminal-ui.js` module shape so Node can `require` it and the browser receives `window.KanediasSessionModal`. Keep data decisions pure:

```js
function levelsFor(modelSelect) {
  var option = modelSelect.options[modelSelect.selectedIndex];
  return (option.getAttribute("data-thinking-levels") || "").split(",").filter(Boolean);
}

function buildRequest(dialog) {
  return {
    root: selection(dialog.querySelector("[data-root-model]"), dialog.querySelector("[data-root-thinking]")),
    workers: Array.from(dialog.querySelectorAll("[data-worker-row]")).map(workerSelection)
  };
}
```

`bind` uses same-origin `fetch` with `Content-Type: application/json`, parses only bounded JSON responses, and never inserts response text with `innerHTML`.

- [ ] **Step 4: Integrate without regressing terminal controls**

Load `/assets/session-modal.js` before `app.js`, serve it as an embedded asset, and initialize it from `app.js`. The New Session button calls the controller instead of Datastar POST. Use the dialog's `cancel` event and a capture-phase Escape guard while open; outside the modal, the existing terminal `Escape` interrupt behavior remains unchanged.

- [ ] **Step 5: Add Astrolabe modal styling**

Use native `<dialog>` and `::backdrop`; preserve responsive behavior at `max-width: 820px`. Keep visible focus rings, minimum 44px mobile controls, scrollable worker rows, explicit pending state, and the existing brass/cyan/amber color-plus-text semantics. Do not reuse the mobile sidebar `.scrim` as the dialog backdrop.

- [ ] **Step 6: Update static asset and interaction tests**

Update the script count/order assertion to include `session-modal.js`; assert every script is local. Add handler asset tests and prove `terminal-ui.test.js`, `markdown-renderer.test.js`, and the new modal suite all run under the existing unchanged `node --test internal/server/web/*.test.js` Makefile command.

- [ ] **Step 7: Update the opt-in live acceptance request without running it**

Where `internal/supervisor/live_incus_test.go` posts empty New Session payloads, build the complete default request from the loaded config/launch configuration and post JSON. Preserve all existing authorization gates and do not weaken skips.

- [ ] **Step 8: Run focused UI and full hermetic verification**

```bash
node --test internal/server/web/session-modal.test.js internal/server/web/terminal-ui.test.js internal/server/web/markdown-renderer.test.js
go test ./internal/server ./internal/manager ./internal/supervisor/... ./cmd -count=1
make test
go test -race ./internal/config ./internal/manager ./internal/server ./internal/supervisor/...
go vet ./...
git diff --check
```

Expected: all commands exit 0. Do not run `make test-live` without explicit authorization.

- [ ] **Step 9: Manually inspect the rendered user flow**

Start the local control server on a dedicated loopback port:

```bash
go run . --config config.toml server --listen 127.0.0.1:18080
```

Open `http://127.0.0.1:18080/` and confirm:

1. New Session opens the focused modal.
2. Defaults are preselected.
3. Advanced worker profiles expand.
4. Changing a model updates allowed thinking levels.
5. Cancel/Escape closes without a request.
6. The modal fits desktop and narrow/mobile widths without obscuring its actions.
7. Existing transcript markdown, tool cards, and terminal controls remain visually intact.

Do not press Launch during this visual-only check, because a valid launch enters the existing Incus lifecycle. Use the automated invalid-response and effective-model rendering tests for those states. Record the browser-observed results in the implementation handoff.

- [ ] **Step 10: Commit**

```bash
git add internal/server/web/session-modal.js internal/server/web/session-modal.test.js internal/server/web/index.html internal/server/web/app.js internal/server/web/app.css internal/server/handler.go internal/server/handler_test.go internal/supervisor/live_incus_test.go
git commit -m "feat: complete session model chooser"
```

---

## Final Review Contract

Before integration, request independent fresh-context review with distinct angles:

1. **Correctness and inheritance:** launch validation ordering, root FD ownership, immutable policy copies, child/grandchild behavior, and config-change stability.
2. **Security and lifecycle:** browser trust boundary, no provider/credential leakage, strict bounded decoders, descriptor closure, cleanup ordering, and no new persistence.
3. **UI and regression:** accessible dialog behavior, thinking-level clamping, error/pending states, effective model display, and preservation of markdown/terminal controls.
4. **Tests and simplicity:** acceptance coverage, deterministic tests, unnecessary abstractions, and broad fixture churn.

After accepted fixes, rerun affected focused tests followed by the complete Task 7 verification ladder. The parent performs the final diff review and reports changed files, commands/results, browser-versus-rendered UI evidence, and residual live-provider/Incus risk.
